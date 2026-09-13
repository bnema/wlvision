/*
 * wlvision-shell.c - wlvision's compositor shell and privileged control surface.
 *
 * Weston runs this module as its shell: it creates the weston_desktop instance,
 * owns the window list, and serves the private wlvision_control_v1 protocol that
 * the resident controller binds. docs/weston-baseline.md records why the shell
 * is ours: a module loaded beside the normal desktop shell cannot enumerate or
 * control application toplevels, because desktop surfaces belong to a per-shell
 * weston_desktop instance and no reverse lookup exists.
 *
 * What this module owns:
 *
 *   - opaque window handles ("app-N", monotonically assigned, never reused),
 *   - the session revision, which increases whenever an observable window or
 *     layout change happens; window operations carry the revision the caller
 *     saw and are refused when it moved,
 *   - window enumeration, activation, movement, configure-to-commit resize and
 *     close,
 *   - pointer and keyboard injection into the compositor's seat,
 *   - capture authorization: only the client that was granted capture through
 *     the control protocol may use Weston's capture protocol,
 *   - frame sequencing: every repaint is numbered, and the frame event carries
 *     the capture authorization that was outstanding.
 *
 * It never encodes pixels, writes files, parses JSON or manages containers.
 *
 * The control global is advertised to every client in the session, so access is
 * gated on peer credentials: a bind from any UID other than WLVISION_CONTROL_UID
 * is refused with a protocol error. When that variable is unset the module
 * refuses every bind rather than guessing.
 *
 * Copyright (C) 2026 wlvision
 * SPDX-License-Identifier: MIT
 */

#define _GNU_SOURCE

#include <errno.h>
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/types.h>
#include <unistd.h>

#include <wayland-server-core.h>
#include <wayland-server-protocol.h>

#include <libweston/libweston.h>
#include <libweston/desktop.h>
#include <libweston/shell-utils.h>

#include "generated/wlvision-control-server.h"

#define WLVISION_REPORT_PREFIX "wlvision-shell: "

/* Protocol error codes, matching protocol/wlvision-control.xml. */
#define WLVISION_ERROR_STALE_REVISION      0
#define WLVISION_ERROR_WINDOW_NOT_FOUND    1
#define WLVISION_ERROR_NOT_AUTHORIZED      2
#define WLVISION_ERROR_CAPTURE_UNAVAILABLE 3
#define WLVISION_ERROR_INVALID_ARGUMENT    4

#define WLVISION_HANDLE_MAX 32

/* Size a toplevel is configured with when no output is ready yet. */
#define WLVISION_DEFAULT_WIDTH  320
#define WLVISION_DEFAULT_HEIGHT 240

#define wlvision_container_of(ptr, type, member) \
	((type *)(void *)((char *)(ptr) - offsetof(type, member)))

struct wlvision_shell;
struct wlvision_toplevel;

/* One request the controller is still waiting for an answer to. */
struct wlvision_request {
	struct wl_list link;		/* wlvision_shell::requests */
	uint32_t id;
	struct wlvision_toplevel *toplevel;
	/* Size the application was actually configured with. The completion rule
	 * is expressed against this, not against the size the caller typed, so a
	 * module that clamps a configure keeps completing correctly. */
	int32_t configured_width;
	int32_t configured_height;
};

struct wlvision_toplevel {
	struct wl_list link;		/* wlvision_shell::toplevels */
	struct weston_desktop_surface *surface;
	struct weston_view *view;
	char handle[WLVISION_HANDLE_MAX];
	unsigned int index;
	uint32_t state;
	bool mapped;
	bool activated;
};

/* A per-output listener that numbers frames. */
struct wlvision_frame_watch {
	struct wl_list link;		/* wlvision_shell::frame_watches */
	struct wl_listener listener;
	struct wlvision_shell *shell;
	struct weston_output *output;
};

struct wlvision_shell {
	struct weston_compositor *compositor;
	struct weston_desktop *desktop;

	struct weston_layer background_layer;
	struct weston_layer layer;
	struct weston_curtain *background;

	struct wl_listener destroy_listener;
	struct wl_listener output_created_listener;
	struct wl_listener output_destroyed_listener;
	struct wl_listener capture_authority_listener;

	struct wl_list toplevels;	/* wlvision_toplevel::link */
	struct wl_list requests;	/* wlvision_request::link */
	struct wl_list frame_watches;	/* wlvision_frame_watch::link */

	unsigned int next_index;	/* monotonic: handles are never reused */
	uint64_t revision;
	uint64_t frame_sequence;

	struct wl_global *control_global;
	struct wl_resource *manager_resource;
	struct wl_resource *controller_resource;
	struct wl_client *controller_client;
	struct wl_client *capture_client;

	uint32_t capture_request_id;
	bool capture_request_pending;

	uid_t control_uid;
	bool control_uid_known;
};

static void
report(const char *fmt, ...)
{
	va_list ap;

	fputs(WLVISION_REPORT_PREFIX, stdout);
	va_start(ap, fmt);
	vfprintf(stdout, fmt, ap);
	va_end(ap);
	fputc('\n', stdout);
	fflush(stdout);
}

static const char *
safe_string(const char *string)
{
	return string != NULL ? string : "";
}

/*
 * Revision and time helpers.
 */

static uint32_t
revision_hi(uint64_t revision)
{
	return (uint32_t)(revision >> 32);
}

static uint32_t
revision_lo(uint64_t revision)
{
	return (uint32_t)revision;
}

static void
shell_bump_revision(struct wlvision_shell *shell)
{
	shell->revision++;
}

static void
input_event_init(struct weston_input_event *base, struct weston_seat *seat)
{
	struct timespec now;

	weston_compositor_get_time(&now);
	base->ts = now;
	base->seat = seat;
	base->flow = (struct weston_trace_flow){ 0 };
}

/*
 * Window registry.
 */

static struct wlvision_toplevel *
find_toplevel(struct wlvision_shell *shell, const char *handle)
{
	struct wlvision_toplevel *toplevel;

	if (handle == NULL)
		return NULL;

	wl_list_for_each(toplevel, &shell->toplevels, link) {
		if (strcmp(toplevel->handle, handle) == 0)
			return toplevel;
	}

	return NULL;
}

static bool
handle_in_use(struct wlvision_shell *shell, const char *handle)
{
	return find_toplevel(shell, handle) != NULL;
}

static void
assign_handle(struct wlvision_shell *shell, struct wlvision_toplevel *toplevel)
{
	unsigned int candidate = shell->next_index;

	do {
		candidate++;
		snprintf(toplevel->handle, sizeof toplevel->handle,
			 "app-%u", candidate);
	} while (handle_in_use(shell, toplevel->handle));

	shell->next_index = candidate;
	toplevel->index = candidate;
}

static uint32_t
toplevel_state(struct wlvision_toplevel *toplevel)
{
	uint32_t state = 0;

	if (toplevel->activated)
		state |= 1u << 0;
	if (toplevel->mapped)
		state |= 1u << 1;

	return state;
}

/*
 * The geometry the module reports for a toplevel: the same size
 * emit_toplevel_changed publishes and resize_done carries as visible_*.
 */
static void
toplevel_visible_size(struct wlvision_toplevel *toplevel,
		      uint32_t *width, uint32_t *height)
{
	struct weston_geometry geometry =
		weston_desktop_surface_get_geometry(toplevel->surface);

	*width = (uint32_t)(geometry.width > 0 ? geometry.width : 0);
	*height = (uint32_t)(geometry.height > 0 ? geometry.height : 0);
}

static void
emit_toplevel_changed(struct wlvision_shell *shell,
		      struct wlvision_toplevel *toplevel)
{
	struct weston_geometry geometry;
	uint32_t width = 0;
	uint32_t height = 0;

	if (shell->controller_resource == NULL)
		return;

	geometry = weston_desktop_surface_get_geometry(toplevel->surface);
	toplevel_visible_size(toplevel, &width, &height);

	wlvision_controller_v1_send_toplevel_changed(shell->controller_resource,
		toplevel->handle,
		safe_string(weston_desktop_surface_get_title(toplevel->surface)),
		safe_string(weston_desktop_surface_get_app_id(toplevel->surface)),
		geometry.x, geometry.y, width, height,
		toplevel_state(toplevel),
		revision_hi(shell->revision), revision_lo(shell->revision));
}

static void
answer_done(struct wlvision_shell *shell, uint32_t request_id)
{
	if (shell->controller_resource == NULL)
		return;

	wlvision_controller_v1_send_request_done(shell->controller_resource,
		request_id, revision_hi(shell->revision),
		revision_lo(shell->revision));
}

/*
 * The module's first answer to a resize: the size the application was actually
 * configured with, emitted immediately after weston_desktop_surface_set_size.
 */
static void
answer_resize_configured(struct wlvision_shell *shell, uint32_t request_id,
			 int32_t width, int32_t height)
{
	if (shell->controller_resource == NULL)
		return;

	wlvision_controller_v1_send_resize_configured(shell->controller_resource,
		request_id, width, height);
}

/*
 * The terminal answer for a resize request, replacing request_done for that
 * operation only. committed_* is the content size the application committed,
 * visible_* the geometry the module reports (the same size
 * emit_toplevel_changed publishes), and the revision the session revision
 * after the resize.
 */
static void
answer_resize_done(struct wlvision_shell *shell,
		   struct wlvision_request *request,
		   int32_t committed_width, int32_t committed_height)
{
	uint32_t visible_width = 0;
	uint32_t visible_height = 0;

	if (shell->controller_resource == NULL)
		return;

	toplevel_visible_size(request->toplevel, &visible_width, &visible_height);

	wlvision_controller_v1_send_resize_done(shell->controller_resource,
		request->id,
		request->configured_width, request->configured_height,
		committed_width, committed_height,
		(int32_t)visible_width, (int32_t)visible_height,
		revision_hi(shell->revision), revision_lo(shell->revision));
}

static void
answer_failed(struct wlvision_shell *shell, uint32_t request_id, uint32_t code,
	      const char *message)
{
	if (shell->controller_resource == NULL)
		return;

	wlvision_controller_v1_send_request_failed(shell->controller_resource,
		request_id, code, message);
}

/*
 * Pending requests.
 */

static struct wlvision_request *
request_new(struct wlvision_shell *shell, uint32_t id)
{
	struct wlvision_request *request = calloc(1, sizeof *request);

	if (request == NULL)
		return NULL;

	request->id = id;
	wl_list_insert(shell->requests.prev, &request->link);
	return request;
}

static void
request_free(struct wlvision_request *request)
{
	wl_list_remove(&request->link);
	free(request);
}

static struct wlvision_request *
request_for_toplevel(struct wlvision_shell *shell,
		     struct wlvision_toplevel *toplevel)
{
	struct wlvision_request *request;

	wl_list_for_each(request, &shell->requests, link) {
		if (request->toplevel == toplevel)
			return request;
	}

	return NULL;
}

/*
 * Seat and input injection.
 */

static struct weston_seat *
first_seat(struct wlvision_shell *shell)
{
	struct weston_seat *seat;

	wl_list_for_each(seat, &shell->compositor->seat_list, link)
		return seat;

	return NULL;
}

static struct wlvision_toplevel *
focused_toplevel(struct wlvision_shell *shell)
{
	struct weston_seat *seat = first_seat(shell);
	struct weston_keyboard *keyboard;
	struct wlvision_toplevel *toplevel;
	struct weston_surface *focused;

	if (seat == NULL)
		return NULL;

	keyboard = weston_seat_get_keyboard(seat);
	if (keyboard == NULL)
		return NULL;

	focused = keyboard->focus;

	wl_list_for_each(toplevel, &shell->toplevels, link) {
		if (weston_desktop_surface_get_surface(toplevel->surface) == focused)
			return toplevel;
	}

	return NULL;
}

static void
activate_toplevel(struct wlvision_shell *shell,
		  struct wlvision_toplevel *toplevel)
{
	struct weston_seat *seat = first_seat(shell);
	struct wlvision_toplevel *current;

	if (seat == NULL || toplevel->view == NULL)
		return;

	current = focused_toplevel(shell);
	if (current != NULL && current != toplevel) {
		current->activated = false;
		weston_desktop_surface_set_activated(current->surface, false);
	}

	weston_view_activate_input(toplevel->view, seat, 0);
	weston_desktop_surface_set_activated(toplevel->surface, true);
	toplevel->activated = true;
}

static void
inject_pointer_motion(struct wlvision_shell *shell, double x, double y)
{
	struct weston_seat *seat = first_seat(shell);
	struct weston_pointer *pointer;
	struct weston_pointer_motion_event event = { 0 };

	if (seat == NULL)
		return;

	pointer = weston_seat_get_pointer(seat);
	if (pointer == NULL)
		return;

	input_event_init(&event.base, seat);
	event.mask = WESTON_POINTER_MOTION_ABS;
	event.abs.c = weston_coord(x, y);

	weston_pointer_send_motion(pointer, &event);
	weston_pointer_send_frame(pointer);
}

static void
inject_pointer_button(struct wlvision_shell *shell, uint32_t button,
		      bool pressed)
{
	struct weston_seat *seat = first_seat(shell);
	struct weston_pointer *pointer;
	struct weston_pointer_button_event event = { 0 };

	if (seat == NULL)
		return;

	pointer = weston_seat_get_pointer(seat);
	if (pointer == NULL)
		return;

	input_event_init(&event.base, seat);
	event.button = button;
	event.button_state = pressed ? WL_POINTER_BUTTON_STATE_PRESSED :
				       WL_POINTER_BUTTON_STATE_RELEASED;

	weston_pointer_send_button(pointer, &event);
	weston_pointer_send_frame(pointer);
}

static void
inject_pointer_axis(struct wlvision_shell *shell, uint32_t axis, double value)
{
	struct weston_seat *seat = first_seat(shell);
	struct weston_pointer *pointer;
	struct weston_pointer_axis_event event = { 0 };

	if (seat == NULL)
		return;

	pointer = weston_seat_get_pointer(seat);
	if (pointer == NULL)
		return;

	input_event_init(&event.base, seat);
	event.axis = axis;
	event.value = value;

	weston_pointer_send_axis(pointer, &event);
	weston_pointer_send_frame(pointer);
}

static void
inject_key(struct wlvision_shell *shell, uint32_t key, bool pressed)
{
	struct weston_seat *seat = first_seat(shell);
	struct weston_keyboard *keyboard;
	struct weston_key_event event = { 0 };

	if (seat == NULL)
		return;

	keyboard = weston_seat_get_keyboard(seat);
	if (keyboard == NULL)
		return;

	input_event_init(&event.base, seat);
	event.key = key;
	event.key_state = pressed ? WL_KEYBOARD_KEY_STATE_PRESSED :
				    WL_KEYBOARD_KEY_STATE_RELEASED;
	event.key_update_state = STATE_UPDATE_AUTOMATIC;

	weston_keyboard_send_key(keyboard, &event);
}

/*
 * Control protocol: requests.
 */

static bool
revision_matches(struct wlvision_shell *shell, uint32_t hi, uint32_t lo)
{
	uint64_t revision = ((uint64_t)hi << 32) | (uint64_t)lo;

	return revision == shell->revision;
}

static void
handle_snapshot(struct wl_client *client, struct wl_resource *resource,
		uint32_t request_id)
{
	struct wlvision_shell *shell = wl_resource_get_user_data(resource);
	struct wlvision_toplevel *toplevel;

	(void)client;

	if (shell->controller_resource == NULL)
		return;

	wlvision_controller_v1_send_snapshot(shell->controller_resource,
		revision_hi(shell->revision), revision_lo(shell->revision));

	wl_list_for_each(toplevel, &shell->toplevels, link)
		emit_toplevel_changed(shell, toplevel);

	answer_done(shell, request_id);
}

static void
handle_activate(struct wl_client *client, struct wl_resource *resource,
		uint32_t request_id, const char *handle, uint32_t revision_hi_word,
		uint32_t revision_lo_word)
{
	struct wlvision_shell *shell = wl_resource_get_user_data(resource);
	struct wlvision_toplevel *toplevel;

	(void)client;

	if (!revision_matches(shell, revision_hi_word, revision_lo_word)) {
		answer_failed(shell, request_id, WLVISION_ERROR_STALE_REVISION,
			      "layout changed since the caller looked");
		return;
	}

	toplevel = find_toplevel(shell, handle);
	if (toplevel == NULL) {
		answer_failed(shell, request_id, WLVISION_ERROR_WINDOW_NOT_FOUND,
			      "no live window has that handle");
		return;
	}

	activate_toplevel(shell, toplevel);
	shell_bump_revision(shell);
	emit_toplevel_changed(shell, toplevel);
	answer_done(shell, request_id);
}

static void
handle_move(struct wl_client *client, struct wl_resource *resource,
	    uint32_t request_id, const char *handle, uint32_t revision_hi_word,
	    uint32_t revision_lo_word, int32_t x, int32_t y)
{
	struct wlvision_shell *shell = wl_resource_get_user_data(resource);
	struct wlvision_toplevel *toplevel;
	struct weston_coord_global pos;

	(void)client;

	if (!revision_matches(shell, revision_hi_word, revision_lo_word)) {
		answer_failed(shell, request_id, WLVISION_ERROR_STALE_REVISION,
			      "layout changed since the caller looked");
		return;
	}

	toplevel = find_toplevel(shell, handle);
	if (toplevel == NULL) {
		answer_failed(shell, request_id, WLVISION_ERROR_WINDOW_NOT_FOUND,
			      "no live window has that handle");
		return;
	}

	pos = (struct weston_coord_global){ .c = weston_coord(x, y) };
	weston_view_set_position(toplevel->view, pos);

	shell_bump_revision(shell);
	emit_toplevel_changed(shell, toplevel);
	answer_done(shell, request_id);
}

static void
handle_resize(struct wl_client *client, struct wl_resource *resource,
	      uint32_t request_id, const char *handle, uint32_t revision_hi_word,
	      uint32_t revision_lo_word, uint32_t width, uint32_t height)
{
	struct wlvision_shell *shell = wl_resource_get_user_data(resource);
	struct wlvision_toplevel *toplevel;
	struct wlvision_request *request;

	(void)client;

	if (!revision_matches(shell, revision_hi_word, revision_lo_word)) {
		answer_failed(shell, request_id, WLVISION_ERROR_STALE_REVISION,
			      "layout changed since the caller looked");
		return;
	}

	if (width == 0 || height == 0 || width > INT32_MAX || height > INT32_MAX) {
		answer_failed(shell, request_id, WLVISION_ERROR_INVALID_ARGUMENT,
			      "width and height must be positive and fit in 32 bits");
		return;
	}

	toplevel = find_toplevel(shell, handle);
	if (toplevel == NULL) {
		answer_failed(shell, request_id, WLVISION_ERROR_WINDOW_NOT_FOUND,
			      "no live window has that handle");
		return;
	}

	/* One outstanding resize per window: a second request replaces the first,
	 * which the superseded caller learns about by its own timeout. */
	request = request_for_toplevel(shell, toplevel);
	if (request != NULL)
		request_free(request);

	request = request_new(shell, request_id);
	if (request == NULL) {
		answer_failed(shell, request_id, WLVISION_ERROR_CAPTURE_UNAVAILABLE,
			      "out of memory");
		return;
	}

	request->toplevel = toplevel;

	/*
	 * Configure the application, then record the size it was configured
	 * with before answering: the completion rule compares against this, not
	 * against the number the caller typed.
	 */
	weston_desktop_surface_set_size(toplevel->surface, (int32_t)width,
					(int32_t)height);

	request->configured_width = (int32_t)width;
	request->configured_height = (int32_t)height;
	answer_resize_configured(shell, request_id, request->configured_width,
				 request->configured_height);
}

static void
handle_close_window(struct wl_client *client, struct wl_resource *resource,
		    uint32_t request_id, const char *handle,
		    uint32_t revision_hi_word, uint32_t revision_lo_word)
{
	struct wlvision_shell *shell = wl_resource_get_user_data(resource);
	struct wlvision_toplevel *toplevel;

	(void)client;

	if (!revision_matches(shell, revision_hi_word, revision_lo_word)) {
		answer_failed(shell, request_id, WLVISION_ERROR_STALE_REVISION,
			      "layout changed since the caller looked");
		return;
	}

	toplevel = find_toplevel(shell, handle);
	if (toplevel == NULL) {
		answer_failed(shell, request_id, WLVISION_ERROR_WINDOW_NOT_FOUND,
			      "no live window has that handle");
		return;
	}

	weston_desktop_surface_close(toplevel->surface);
	answer_done(shell, request_id);
}

static void
handle_pointer_motion(struct wl_client *client, struct wl_resource *resource,
		      uint32_t request_id, wl_fixed_t x, wl_fixed_t y)
{
	struct wlvision_shell *shell = wl_resource_get_user_data(resource);

	(void)client;

	inject_pointer_motion(shell, wl_fixed_to_double(x),
			      wl_fixed_to_double(y));
	answer_done(shell, request_id);
}

static void
handle_pointer_button(struct wl_client *client, struct wl_resource *resource,
		      uint32_t request_id, uint32_t button, uint32_t state)
{
	struct wlvision_shell *shell = wl_resource_get_user_data(resource);

	(void)client;

	if (state > WL_POINTER_BUTTON_STATE_PRESSED) {
		answer_failed(shell, request_id, WLVISION_ERROR_INVALID_ARGUMENT,
			      "button state must be 0 or 1");
		return;
	}

	inject_pointer_button(shell, button,
			      state == WL_POINTER_BUTTON_STATE_PRESSED);
	answer_done(shell, request_id);
}

static void
handle_pointer_axis(struct wl_client *client, struct wl_resource *resource,
		    uint32_t request_id, uint32_t axis, wl_fixed_t value)
{
	struct wlvision_shell *shell = wl_resource_get_user_data(resource);

	(void)client;

	inject_pointer_axis(shell, axis, wl_fixed_to_double(value));
	answer_done(shell, request_id);
}

static void
handle_key(struct wl_client *client, struct wl_resource *resource,
	   uint32_t request_id, uint32_t key, uint32_t state)
{
	struct wlvision_shell *shell = wl_resource_get_user_data(resource);

	(void)client;

	if (state > WL_KEYBOARD_KEY_STATE_PRESSED) {
		answer_failed(shell, request_id, WLVISION_ERROR_INVALID_ARGUMENT,
			      "key state must be 0 or 1");
		return;
	}

	inject_key(shell, key, state == WL_KEYBOARD_KEY_STATE_PRESSED);
	answer_done(shell, request_id);
}

static void
handle_authorize_capture(struct wl_client *client, struct wl_resource *resource,
			 uint32_t request_id)
{
	struct wlvision_shell *shell = wl_resource_get_user_data(resource);

	if (!shell->control_uid_known) {
		answer_failed(shell, request_id, WLVISION_ERROR_NOT_AUTHORIZED,
			      "the session has no control identity");
		return;
	}

	shell->capture_client = client;
	shell->capture_request_id = request_id;
	shell->capture_request_pending = true;

	if (shell->controller_resource != NULL) {
		wlvision_controller_v1_send_capture_authorized(
			shell->controller_resource, request_id);
	}

	answer_done(shell, request_id);
}

static void handle_controller_destroy(struct wl_client *client,
				      struct wl_resource *resource);

static const struct wlvision_controller_v1_interface controller_implementation = {
	.snapshot = handle_snapshot,
	.activate = handle_activate,
	.move = handle_move,
	.resize = handle_resize,
	.close = handle_close_window,
	.pointer_motion = handle_pointer_motion,
	.pointer_button = handle_pointer_button,
	.pointer_axis = handle_pointer_axis,
	.key = handle_key,
	.authorize_capture = handle_authorize_capture,
	.destroy = handle_controller_destroy,
};

static void
controller_resource_destroyed(struct wl_resource *resource)
{
	struct wlvision_shell *shell = wl_resource_get_user_data(resource);
	struct wlvision_request *request, *tmp;

	if (shell == NULL)
		return;

	wl_list_for_each_safe(request, tmp, &shell->requests, link)
		request_free(request);

	if (shell->controller_resource == resource) {
		shell->controller_resource = NULL;
		shell->controller_client = NULL;
		shell->capture_client = NULL;
		shell->capture_request_pending = false;
	}
}

static void
handle_controller_destroy(struct wl_client *client, struct wl_resource *resource)
{
	(void)client;

	wl_resource_destroy(resource);
}

static void
handle_create_controller(struct wl_client *client, struct wl_resource *manager,
			 uint32_t id)
{
	struct wlvision_shell *shell = wl_resource_get_user_data(manager);
	struct wl_resource *resource;

	if (shell->controller_resource != NULL) {
		wl_resource_post_error(manager, WL_DISPLAY_ERROR_INVALID_METHOD,
				       "a controller already exists in this session");
		return;
	}

	resource = wl_resource_create(client, &wlvision_controller_v1_interface,
				      1, id);
	if (resource == NULL) {
		wl_client_post_no_memory(client);
		return;
	}

	wl_resource_set_implementation(resource, &controller_implementation, shell,
				       controller_resource_destroyed);

	shell->controller_resource = resource;
	shell->controller_client = client;

	report("controller-created uid=%u", (unsigned)getuid());
}

static void
handle_manager_destroy(struct wl_client *client, struct wl_resource *resource)
{
	(void)client;

	wl_resource_destroy(resource);
}

static const struct wlvision_control_v1_interface manager_implementation = {
	.create_controller = handle_create_controller,
	.destroy = handle_manager_destroy,
};

static bool
client_may_control(struct wlvision_shell *shell, struct wl_client *client)
{
	pid_t pid;
	uid_t uid;
	gid_t gid;

	if (!shell->control_uid_known || client == NULL)
		return false;

	wl_client_get_credentials(client, &pid, &uid, &gid);

	return uid == shell->control_uid;
}

static void
control_global_bind(struct wl_client *client, void *data, uint32_t version,
		    uint32_t id)
{
	struct wlvision_shell *shell = data;
	struct wl_resource *resource;

	resource = wl_resource_create(client, &wlvision_control_v1_interface,
				      version < 1 ? version : 1, id);
	if (resource == NULL) {
		wl_client_post_no_memory(client);
		return;
	}

	/* Group access to the display is not control access: the module checks the
	 * peer's UID, not the socket's permissions. */
	if (!client_may_control(shell, client)) {
		pid_t pid;
		uid_t uid;
		gid_t gid;

		wl_client_get_credentials(client, &pid, &uid, &gid);
		report("control-bind-refused uid=%u", (unsigned)uid);
		wl_resource_post_error(resource, WL_DISPLAY_ERROR_INVALID_METHOD,
				       "wlvision control is not available to this user");
		return;
	}

	wl_resource_set_implementation(resource, &manager_implementation, shell,
				       NULL);
	shell->manager_resource = resource;
}

/*
 * Desktop API: the window list.
 */

static void
observed_view_mapping(struct wlvision_shell *shell,
		      struct wlvision_toplevel *toplevel)
{
	struct weston_surface *surface =
		weston_desktop_surface_get_surface(toplevel->surface);
	struct weston_coord_global pos = { .c = weston_coord(0, 0) };

	if (weston_surface_is_mapped(surface))
		return;

	weston_surface_map(surface);
	weston_view_set_position(toplevel->view, pos);
	weston_view_move_to_layer(toplevel->view, &shell->layer.view_list);
	toplevel->mapped = true;
}

/*
 * A client waits for its first configure before it maps a window, so a shell
 * that never configures anything leaves every application unmapped. The initial
 * size is the output's, or a small default when no output is ready yet.
 */
static void
configure_initial_size(struct wlvision_shell *shell,
		       struct wlvision_toplevel *toplevel)
{
	struct weston_output *output;
	int32_t width = WLVISION_DEFAULT_WIDTH;
	int32_t height = WLVISION_DEFAULT_HEIGHT;

	wl_list_for_each(output, &shell->compositor->output_list, link) {
		width = output->width;
		height = output->height;
		break;
	}

	if (width <= 0 || height <= 0) {
		width = WLVISION_DEFAULT_WIDTH;
		height = WLVISION_DEFAULT_HEIGHT;
	}

	weston_desktop_surface_set_size(toplevel->surface, width, height);
}

static void
desktop_surface_added(struct weston_desktop_surface *surface, void *data)
{
	struct wlvision_shell *shell = data;
	struct wlvision_toplevel *toplevel;

	toplevel = calloc(1, sizeof *toplevel);
	if (toplevel == NULL)
		return;

	toplevel->surface = surface;
	assign_handle(shell, toplevel);

	toplevel->view = weston_desktop_surface_create_view(surface);
	if (toplevel->view == NULL) {
		free(toplevel);
		return;
	}

	weston_desktop_surface_set_user_data(surface, toplevel);
	wl_list_insert(shell->toplevels.prev, &toplevel->link);

	configure_initial_size(shell, toplevel);

	shell_bump_revision(shell);
	emit_toplevel_changed(shell, toplevel);

	report("added handle=%s app_id=%s revision=%llu", toplevel->handle,
	       safe_string(weston_desktop_surface_get_app_id(surface)),
	       (unsigned long long)shell->revision);
}

static void
desktop_surface_removed(struct weston_desktop_surface *surface, void *data)
{
	struct wlvision_shell *shell = data;
	struct wlvision_toplevel *toplevel =
		weston_desktop_surface_get_user_data(surface);
	struct wlvision_request *request;

	if (toplevel == NULL)
		return;

	request = request_for_toplevel(shell, toplevel);
	if (request != NULL) {
		answer_failed(shell, request->id, WLVISION_ERROR_WINDOW_NOT_FOUND,
			      "the window was destroyed while the request was pending");
		request_free(request);
	}

	if (shell->controller_resource != NULL) {
		wlvision_controller_v1_send_toplevel_removed(
			shell->controller_resource, toplevel->handle);
	}

	if (toplevel->view != NULL) {
		weston_desktop_surface_unlink_view(toplevel->view);
		weston_view_destroy(toplevel->view);
		toplevel->view = NULL;
	}

	weston_desktop_surface_set_user_data(surface, NULL);
	wl_list_remove(&toplevel->link);

	shell_bump_revision(shell);

	report("removed handle=%s revision=%llu", toplevel->handle,
	       (unsigned long long)shell->revision);

	free(toplevel);
}

static void
desktop_surface_committed(struct weston_desktop_surface *surface,
			  struct weston_coord_surface buf_offset, void *data)
{
	struct wlvision_shell *shell = data;
	struct wlvision_toplevel *toplevel =
		weston_desktop_surface_get_user_data(surface);
	struct wlvision_request *request;
	struct weston_surface *weston_surface;
	int buffer_width = 0;
	int buffer_height = 0;

	(void)buf_offset;

	if (toplevel == NULL)
		return;

	observed_view_mapping(shell, toplevel);

	request = request_for_toplevel(shell, toplevel);
	if (request == NULL)
		return;

	weston_surface = weston_desktop_surface_get_surface(surface);
	weston_surface_get_content_size(weston_surface, &buffer_width,
					&buffer_height);

	/* A resize is only complete once the application commits a buffer that
	 * matches the size it was configured with: sending a configure proves
	 * nothing. The comparison is against the configure the module sent, not
	 * against the number the caller typed. */
	if (buffer_width != request->configured_width ||
	    buffer_height != request->configured_height)
		return;

	shell_bump_revision(shell);
	emit_toplevel_changed(shell, toplevel);
	answer_resize_done(shell, request, buffer_width, buffer_height);

	report("resize-complete handle=%s configured=%dx%d committed=%dx%d revision=%llu",
	       toplevel->handle, request->configured_width,
	       request->configured_height, buffer_width, buffer_height,
	       (unsigned long long)shell->revision);

	request_free(request);
}

static void
desktop_surface_move(struct weston_desktop_surface *surface,
		     struct weston_seat *seat, uint32_t serial, void *data)
{
	(void)surface;
	(void)seat;
	(void)serial;
	(void)data;
}

static void
desktop_surface_resize(struct weston_desktop_surface *surface,
		       struct weston_seat *seat, uint32_t serial,
		       enum weston_desktop_surface_edge edges, void *data)
{
	(void)surface;
	(void)seat;
	(void)serial;
	(void)edges;
	(void)data;
}

static void
desktop_surface_fullscreen_requested(struct weston_desktop_surface *surface,
				     bool fullscreen,
				     struct weston_output *output, void *data)
{
	struct wlvision_shell *shell = data;
	struct wlvision_toplevel *toplevel =
		weston_desktop_surface_get_user_data(surface);

	(void)fullscreen;
	(void)output;

	if (toplevel == NULL)
		return;

	shell_bump_revision(shell);
	emit_toplevel_changed(shell, toplevel);
}

static void
desktop_surface_maximized_requested(struct weston_desktop_surface *surface,
				    bool maximized, void *data)
{
	struct wlvision_shell *shell = data;
	struct wlvision_toplevel *toplevel =
		weston_desktop_surface_get_user_data(surface);

	(void)maximized;

	if (toplevel == NULL)
		return;

	shell_bump_revision(shell);
	emit_toplevel_changed(shell, toplevel);
}

static void
desktop_surface_minimized_requested(struct weston_desktop_surface *surface,
				    void *data)
{
	(void)surface;
	(void)data;
}

static void
desktop_ping_timeout(struct weston_desktop_client *client, void *data)
{
	(void)client;
	(void)data;
}

static void
desktop_pong(struct weston_desktop_client *client, void *data)
{
	(void)client;
	(void)data;
}

static const struct weston_desktop_api desktop_api = {
	.struct_size = sizeof(struct weston_desktop_api),
	.surface_added = desktop_surface_added,
	.surface_removed = desktop_surface_removed,
	.committed = desktop_surface_committed,
	.move = desktop_surface_move,
	.resize = desktop_surface_resize,
	.fullscreen_requested = desktop_surface_fullscreen_requested,
	.maximized_requested = desktop_surface_maximized_requested,
	.minimized_requested = desktop_surface_minimized_requested,
	.ping_timeout = desktop_ping_timeout,
	.pong = desktop_pong,
};

/*
 * Capture authority and frame sequencing.
 */

static void capture_authority(struct wl_listener *listener,
			      struct weston_output_capture_attempt *attempt);

/* The listener's notify field is a plain wl_notify_func_t, so the typed
 * callback gets an adapter. */
static void
capture_authority_notify(struct wl_listener *listener, void *data)
{
	capture_authority(listener, data);
}

static void
capture_authority(struct wl_listener *listener,
		  struct weston_output_capture_attempt *attempt)
{
	struct wlvision_shell *shell =
		wlvision_container_of(listener, struct wlvision_shell,
				      capture_authority_listener);

	if (attempt == NULL || attempt->who == NULL)
		return;

	if (shell->capture_client != NULL &&
	    attempt->who->client == shell->capture_client) {
		attempt->authorized = true;
		return;
	}

	attempt->denied = true;
}

static void
frame_signal_notify(struct wl_listener *listener, void *data)
{
	struct wlvision_frame_watch *watch =
		wlvision_container_of(listener, struct wlvision_frame_watch,
				      listener);
	struct wlvision_shell *shell = watch->shell;
	uint32_t capture_request_id = 0;

	(void)data;

	shell->frame_sequence++;

	/*
	 * The outstanding authorization is reported on every frame until a new one
	 * replaces it, rather than only on the first frame after it. A repaint can
	 * happen between the authorization and the capture request, and the frame
	 * that actually carries the capture is the one whose id the caller needs.
	 */
	if (shell->capture_request_pending)
		capture_request_id = shell->capture_request_id;

	if (shell->controller_resource != NULL) {
		wlvision_controller_v1_send_frame(shell->controller_resource,
						  capture_request_id,
						  (uint32_t)shell->frame_sequence);
	}
}

static void
watch_output(struct wlvision_shell *shell, struct weston_output *output)
{
	struct wlvision_frame_watch *watch = calloc(1, sizeof *watch);

	if (watch == NULL)
		return;

	watch->shell = shell;
	watch->output = output;
	watch->listener.notify = frame_signal_notify;
	wl_signal_add(&output->frame_signal, &watch->listener);
	wl_list_insert(shell->frame_watches.prev, &watch->link);
}

static void
output_created(struct wl_listener *listener, void *data)
{
	struct wlvision_shell *shell =
		wlvision_container_of(listener, struct wlvision_shell,
				      output_created_listener);
	struct weston_output *output = data;

	watch_output(shell, output);
	weston_output_set_ready(output);
}

static void
output_destroyed(struct wl_listener *listener, void *data)
{
	struct wlvision_shell *shell =
		wlvision_container_of(listener, struct wlvision_shell,
				      output_destroyed_listener);
	struct weston_output *output = data;
	struct wlvision_frame_watch *watch, *tmp;

	(void)shell;

	wl_list_for_each_safe(watch, tmp, &shell->frame_watches, link) {
		if (watch->output != output)
			continue;

		wl_list_remove(&watch->listener.link);
		wl_list_remove(&watch->link);
		free(watch);
	}
}

static void
ensure_background(struct wlvision_shell *shell, struct weston_output *output)
{
	struct weston_curtain_params params;

	if (shell->background != NULL || output == NULL)
		return;

	/* Repainting an empty scene graph asserts, so a shell must paint
	 * something; upstream shells all ship a background. */
	params = (struct weston_curtain_params) {
		.r = 0.05, .g = 0.06, .b = 0.08, .a = 1.0,
		.pos = output->pos,
		.width = output->width,
		.height = output->height,
		.capture_input = false,
		.label = strdup("wlvision background"),
	};

	shell->background =
		weston_shell_utils_curtain_create(shell->compositor, &params);
	if (shell->background == NULL)
		return;

	weston_view_move_to_layer(shell->background->view,
				  &shell->background_layer.view_list);
}

/*
 * Module lifetime.
 */

static void
shell_destroyed(struct wl_listener *listener, void *data)
{
	struct wlvision_shell *shell =
		wlvision_container_of(listener, struct wlvision_shell,
				      destroy_listener);
	struct wlvision_toplevel *toplevel, *tmp;
	struct wlvision_frame_watch *watch, *frame_tmp;
	struct wlvision_request *request, *request_tmp;

	(void)data;

	wl_list_for_each_safe(request, request_tmp, &shell->requests, link)
		request_free(request);

	wl_list_for_each_safe(toplevel, tmp, &shell->toplevels, link) {
		wl_list_remove(&toplevel->link);
		free(toplevel);
	}

	wl_list_for_each_safe(watch, frame_tmp, &shell->frame_watches, link) {
		wl_list_remove(&watch->listener.link);
		wl_list_remove(&watch->link);
		free(watch);
	}

	wl_list_remove(&shell->output_created_listener.link);
	wl_list_remove(&shell->output_destroyed_listener.link);
	wl_list_remove(&shell->capture_authority_listener.link);

	if (shell->control_global != NULL)
		wl_global_destroy(shell->control_global);

	if (shell->background != NULL)
		weston_shell_utils_curtain_destroy(shell->background);

	weston_desktop_destroy(shell->desktop);
	weston_layer_fini(&shell->layer);
	weston_layer_fini(&shell->background_layer);
	free(shell);
}

static void
read_control_uid(struct wlvision_shell *shell)
{
	const char *value = getenv("WLVISION_CONTROL_UID");
	char *end = NULL;
	long uid;

	if (value == NULL || *value == '\0')
		return;

	errno = 0;
	uid = strtol(value, &end, 10);
	/* Reject anything that does not fit a uid_t: a value such as 2^32 would
	 * otherwise truncate to 0 and turn the deny-by-default gate into "root may
	 * control". */
	if (errno != 0 || end == value || *end != '\0' || uid < 0 ||
	    (unsigned long)uid > (unsigned long)UINT32_MAX) {
		report("error reason=invalid-control-uid value=%s", value);
		return;
	}

	shell->control_uid = (uid_t)uid;
	shell->control_uid_known = true;
}

static int
wlvision_shell_init(struct weston_compositor *compositor)
{
	struct wlvision_shell *shell;
	struct weston_output *output;

	shell = calloc(1, sizeof *shell);
	if (shell == NULL)
		return -1;

	if (!weston_compositor_add_destroy_listener_once(
		    compositor, &shell->destroy_listener, shell_destroyed)) {
		free(shell);
		return 0;
	}

	shell->compositor = compositor;
	wl_list_init(&shell->toplevels);
	wl_list_init(&shell->requests);
	wl_list_init(&shell->frame_watches);

	read_control_uid(shell);

	weston_layer_init(&shell->background_layer, compositor);
	weston_layer_init(&shell->layer, compositor);
	weston_layer_set_position(&shell->background_layer,
				  WESTON_LAYER_POSITION_BACKGROUND);
	weston_layer_set_position(&shell->layer, WESTON_LAYER_POSITION_NORMAL);

	shell->output_created_listener.notify = output_created;
	wl_signal_add(&compositor->output_created_signal,
		      &shell->output_created_listener);
	shell->output_destroyed_listener.notify = output_destroyed;
	wl_signal_add(&compositor->output_destroyed_signal,
		      &shell->output_destroyed_listener);

	wl_list_for_each(output, &compositor->output_list, link) {
		ensure_background(shell, output);
		watch_output(shell, output);
		weston_output_set_ready(output);
	}

	shell->capture_authority_listener.notify = capture_authority_notify;
	weston_compositor_add_screenshot_authority(compositor,
						   &shell->capture_authority_listener,
						   capture_authority);

	shell->desktop = weston_desktop_create(compositor, &desktop_api, shell);
	if (shell->desktop == NULL)
		return -1;

	shell->control_global = wl_global_create(compositor->wl_display,
						 &wlvision_control_v1_interface,
						 1, shell, control_global_bind);
	if (shell->control_global == NULL)
		return -1;

	setvbuf(stdout, NULL, _IOLBF, 0);
	report("ready control_uid=%s",
	       shell->control_uid_known ? "configured" : "unset");

	return 0;
}

WL_EXPORT int
wet_shell_init(struct weston_compositor *compositor, int *argc, char *argv[])
{
	(void)argc;
	(void)argv;

	return wlvision_shell_init(compositor);
}

WL_EXPORT int
wet_module_init(struct weston_compositor *compositor, int *argc, char *argv[])
{
	(void)argc;
	(void)argv;

	return wlvision_shell_init(compositor);
}
