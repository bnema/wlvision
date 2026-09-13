/*
 * wlvision-shell-probe.c - the smallest purpose-built Weston shell that
 * proves the "Option A" claim from docs/weston-baseline.md: when wlvision
 * ships the shell, the module that creates the weston_desktop instance can
 * enumerate, activate and resize an application toplevel using only the
 * public libweston desktop API.
 *
 * It is deliberately narrow:
 *
 *   - every added toplevel gets a stable opaque handle "app-N" (N only ever
 *     increases, a handle is never reused),
 *   - title, app id and geometry are read through the public accessors,
 *   - a view is created for the surface and moved into a single layer that
 *     is created once,
 *   - each new toplevel is activated once, when it first has content, and
 *     given keyboard focus through the seat (the previously focused one is
 *     deactivated),
 *   - a size request (320x240 first, then 640x480) is issued through
 *     weston_desktop_surface_set_size() and the committed buffer is compared
 *     against it,
 *   - one machine-readable "wlvision-shell:" line is written to stdout per
 *     event; the lines are the report run.sh asserts on.
 *
 * The module is loaded as a shell: Weston appends "-shell.so" to the
 * [core] shell= name and looks up wet_shell_init(). It is also loadable as a
 * plain module (wet_module_init), which is the entry point name the probe
 * plan records. Both spellings run the same init.
 *
 * Copyright (C) 2026 wlvision
 * SPDX-License-Identifier: MIT
 */

#define _GNU_SOURCE

#include <stdarg.h>
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include <libweston/libweston.h>
#include <libweston/desktop.h>
#include <libweston/shell-utils.h>

#define REPORT_PREFIX "wlvision-shell: "

#define probe_container_of(ptr, type, member) \
	((type *)(void *)((char *)(ptr) - offsetof(type, member)))

/* Sizes requested through weston_desktop_surface_set_size().  The first one
 * is the size the toplevel is mapped at, the second one is the resize that
 * has to be acknowledged by a matching client buffer. */
#define INITIAL_WIDTH  320
#define INITIAL_HEIGHT 240
#define RESIZE_WIDTH   640
#define RESIZE_HEIGHT  480

struct probe_toplevel {
	struct wl_list link;		/* probe_shell::toplevels */
	struct weston_desktop_surface *surface;
	struct weston_view *view;
	char handle[32];
	unsigned int index;
	uint32_t request_id;		/* id of the last size request */
	int32_t requested_width;
	int32_t requested_height;
	unsigned int commit_count;
	bool mapped;
	bool ever_activated;
};

struct probe_shell {
	struct weston_compositor *compositor;
	struct weston_desktop *desktop;
	struct weston_layer background_layer;
	struct weston_layer layer;
	struct weston_curtain *background;
	struct wl_listener destroy_listener;
	struct wl_listener output_created_listener;
	struct wl_list toplevels;	/* probe_toplevel::link */
	unsigned int next_index;	/* monotonic, handles are never reused */
	uint32_t next_request_id;
	unsigned int live_toplevels;
	struct probe_toplevel *active;	/* currently focused toplevel */
};

static void
report(const char *fmt, ...)
{
	va_list ap;

	fputs(REPORT_PREFIX, stdout);
	va_start(ap, fmt);
	vfprintf(stdout, fmt, ap);
	va_end(ap);
	fputc('\n', stdout);
	fflush(stdout);
}

static const char *
safe_string(const char *string)
{
	return string != NULL ? string : "<none>";
}

static struct weston_seat *
probe_first_seat(struct probe_shell *shell)
{
	struct weston_seat *seat;

	wl_list_for_each(seat, &shell->compositor->seat_list, link)
		return seat;

	return NULL;
}

static void
probe_request_size(struct probe_shell *shell, struct probe_toplevel *toplevel,
		   int32_t width, int32_t height)
{
	toplevel->request_id = ++shell->next_request_id;
	toplevel->requested_width = width;
	toplevel->requested_height = height;

	weston_desktop_surface_set_size(toplevel->surface, width, height);

	report("configured handle=%s request_id=%u requested=%dx%d",
	       toplevel->handle, toplevel->request_id, width, height);
}

/*
 * Bookkeeping: the handle generator is monotonic and the live list is
 * scanned before a handle is handed out, so a handle can never be reused
 * even if a client disappears and a new one connects.
 */
static bool
probe_handle_in_use(struct probe_shell *shell, const char *handle)
{
	struct probe_toplevel *toplevel;

	wl_list_for_each(toplevel, &shell->toplevels, link) {
		if (strcmp(toplevel->handle, handle) == 0)
			return true;
	}

	return false;
}

static void
probe_assign_handle(struct probe_shell *shell, struct probe_toplevel *toplevel)
{
	unsigned int candidate = shell->next_index;

	do {
		candidate++;
		snprintf(toplevel->handle, sizeof toplevel->handle,
			 "app-%u", candidate);
	} while (probe_handle_in_use(shell, toplevel->handle));

	shell->next_index = candidate;
	toplevel->index = candidate;
}

static void
probe_activate(struct probe_shell *shell, struct probe_toplevel *toplevel)
{
	struct weston_seat *seat = probe_first_seat(shell);
	struct weston_keyboard *keyboard;
	struct weston_surface *surface;
	bool focused;

	if (seat == NULL) {
		report("activation-failed handle=%s reason=no-seat",
		       toplevel->handle);
		return;
	}

	/* Keyboard focus through the seat. weston_view_activate_input() is the
	 * public entry point: it calls weston_seat_set_keyboard_focus() and
	 * emits the compositor activate signal, which is what window managers
	 * normally react to. */
	weston_view_activate_input(toplevel->view, seat, 0);

	/* Tell xdg-shell the toplevel is activated, so the next configure
	 * carries XDG_TOPLEVEL_STATE_ACTIVATED. */
	weston_desktop_surface_set_activated(toplevel->surface, true);

	keyboard = weston_seat_get_keyboard(seat);
	surface = weston_desktop_surface_get_surface(toplevel->surface);
	focused = keyboard != NULL && keyboard->focus == surface;

	report("activated handle=%s seat=%s keyboard=%s keyboard_focus=%s",
	       toplevel->handle, safe_string(seat->seat_name),
	       keyboard != NULL ? "yes" : "no", focused ? "yes" : "no");

	toplevel->ever_activated = true;
}

static void
probe_observed_view_mapping(struct probe_shell *shell,
			    struct probe_toplevel *toplevel)
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

static void
probe_surface_added(struct weston_desktop_surface *surface, void *data)
{
	struct probe_shell *shell = data;
	struct probe_toplevel *toplevel;
	struct weston_geometry geometry;

	toplevel = calloc(1, sizeof *toplevel);
	if (toplevel == NULL) {
		report("error handle=none reason=out-of-memory");
		return;
	}

	toplevel->surface = surface;
	probe_assign_handle(shell, toplevel);

	toplevel->view = weston_desktop_surface_create_view(surface);
	if (toplevel->view == NULL) {
		report("error handle=%s reason=no-view", toplevel->handle);
		free(toplevel);
		return;
	}

	weston_desktop_surface_set_user_data(surface, toplevel);
	wl_list_insert(shell->toplevels.prev, &toplevel->link);
	shell->live_toplevels++;

	geometry = weston_desktop_surface_get_geometry(surface);
	report("added handle=%s title=%s app_id=%s geometry=%d,%d,%dx%d",
	       toplevel->handle,
	       safe_string(weston_desktop_surface_get_title(surface)),
	       safe_string(weston_desktop_surface_get_app_id(surface)),
	       geometry.x, geometry.y, geometry.width, geometry.height);

	/* Ask for the size the toplevel should be mapped at. */
	probe_request_size(shell, toplevel, INITIAL_WIDTH, INITIAL_HEIGHT);
}

static void
probe_surface_removed(struct weston_desktop_surface *surface, void *data)
{
	struct probe_shell *shell = data;
	struct probe_toplevel *toplevel =
		weston_desktop_surface_get_user_data(surface);

	if (toplevel == NULL)
		return;

	report("removed handle=%s title=%s app_id=%s live_toplevels=%u",
	       toplevel->handle,
	       safe_string(weston_desktop_surface_get_title(surface)),
	       safe_string(weston_desktop_surface_get_app_id(surface)),
	       shell->live_toplevels - 1);

	if (toplevel->view != NULL) {
		weston_desktop_surface_unlink_view(toplevel->view);
		weston_view_destroy(toplevel->view);
		toplevel->view = NULL;
	}

	weston_desktop_surface_set_user_data(surface, NULL);
	wl_list_remove(&toplevel->link);
	shell->live_toplevels--;
	if (shell->active == toplevel)
		shell->active = NULL;
	free(toplevel);
}

static void
probe_surface_committed(struct weston_desktop_surface *surface,
			struct weston_coord_surface buf_offset, void *data)
{
	struct probe_shell *shell = data;
	struct probe_toplevel *toplevel =
		weston_desktop_surface_get_user_data(surface);
	struct weston_surface *wsurface;
	struct weston_geometry geometry;
	const char *title;
	const char *app_id;
	int buffer_width = 0;
	int buffer_height = 0;
	bool matching;

	(void) buf_offset;

	if (toplevel == NULL)
		return;

	wsurface = weston_desktop_surface_get_surface(surface);
	probe_observed_view_mapping(shell, toplevel);

	geometry = weston_desktop_surface_get_geometry(surface);
	title = weston_desktop_surface_get_title(surface);
	app_id = weston_desktop_surface_get_app_id(surface);
	weston_surface_get_content_size(wsurface, &buffer_width, &buffer_height);

	matching = buffer_width == toplevel->requested_width &&
		   buffer_height == toplevel->requested_height;

	toplevel->commit_count++;
	report("committed handle=%s title=%s app_id=%s geometry=%d,%d,%dx%d "
	       "buffer=%dx%d request_id=%u commit=%u matching=%s",
	       toplevel->handle, safe_string(title), safe_string(app_id),
	       geometry.x, geometry.y, geometry.width, geometry.height,
	       buffer_width, buffer_height, toplevel->request_id,
	       toplevel->commit_count, matching ? "yes" : "no");

	/* Activate a toplevel once, when it first has content, deactivating the
	 * previously focused one, then ask for a larger size: the follow-up
	 * commit with a matching buffer is the resize acknowledgement this
	 * probe exists to observe. */
	if (!toplevel->ever_activated) {
		if (shell->active != NULL && shell->active != toplevel)
			weston_desktop_surface_set_activated(
				shell->active->surface, false);

		probe_activate(shell, toplevel);
		shell->active = toplevel;
		probe_request_size(shell, toplevel,
				   RESIZE_WIDTH, RESIZE_HEIGHT);
	}
}

static void
probe_surface_move(struct weston_desktop_surface *surface,
		   struct weston_seat *seat, uint32_t serial, void *data)
{
	(void) surface;
	(void) seat;
	(void) serial;
	(void) data;
}

static void
probe_surface_resize(struct weston_desktop_surface *surface,
		     struct weston_seat *seat, uint32_t serial,
		     enum weston_desktop_surface_edge edges, void *data)
{
	(void) surface;
	(void) seat;
	(void) serial;
	(void) edges;
	(void) data;
}

static void
probe_surface_fullscreen_requested(struct weston_desktop_surface *surface,
				   bool fullscreen, struct weston_output *output,
				   void *data)
{
	(void) surface;
	(void) fullscreen;
	(void) output;
	(void) data;
}

static void
probe_surface_maximized_requested(struct weston_desktop_surface *surface,
				  bool maximized, void *data)
{
	(void) surface;
	(void) maximized;
	(void) data;
}

static void
probe_surface_minimized_requested(struct weston_desktop_surface *surface,
				  void *data)
{
	(void) surface;
	(void) data;
}

static void
probe_ping_timeout(struct weston_desktop_client *client, void *data)
{
	(void) client;
	(void) data;
}

static void
probe_pong(struct weston_desktop_client *client, void *data)
{
	(void) client;
	(void) data;
}

static const struct weston_desktop_api probe_desktop_api = {
	.struct_size = sizeof(struct weston_desktop_api),
	.surface_added = probe_surface_added,
	.surface_removed = probe_surface_removed,
	.committed = probe_surface_committed,
	.move = probe_surface_move,
	.resize = probe_surface_resize,
	.fullscreen_requested = probe_surface_fullscreen_requested,
	.maximized_requested = probe_surface_maximized_requested,
	.minimized_requested = probe_surface_minimized_requested,
	.ping_timeout = probe_ping_timeout,
	.pong = probe_pong,
};

static void
probe_ensure_background(struct probe_shell *shell, struct weston_output *output)
{
	struct weston_curtain_params params;

	if (shell->background != NULL || output == NULL)
		return;

	/* A repaint with an empty scene graph trips an assertion in
	 * weston_output_repaint(), so a shell must put at least one surface on
	 * the output; upstream shells all ship a background. */
	params = (struct weston_curtain_params) {
		.r = 0.10, .g = 0.12, .b = 0.16, .a = 1.0,
		.pos = output->pos,
		.width = output->width,
		.height = output->height,
		.capture_input = false,
		.surface_committed = NULL,
		.surface_private = NULL,
		.label = strdup("wlvision-shell-probe background"),
	};

	shell->background =
		weston_shell_utils_curtain_create(shell->compositor, &params);
	if (shell->background == NULL) {
		report("error handle=none reason=background-create-failed");
		return;
	}

	weston_view_move_to_layer(shell->background->view,
				  &shell->background_layer.view_list);
}

static void
probe_output_created(struct wl_listener *listener, void *data)
{
	struct probe_shell *shell =
		probe_container_of(listener, struct probe_shell,
				   output_created_listener);
	struct weston_output *output = data;

	probe_ensure_background(shell, output);
	weston_output_set_ready(output);
}

static void
probe_shell_destroy(struct wl_listener *listener, void *data)
{
	struct probe_shell *shell =
		probe_container_of(listener, struct probe_shell,
				  destroy_listener);

	(void) data;

	wl_list_remove(&shell->output_created_listener.link);
	weston_desktop_destroy(shell->desktop);
	if (shell->background != NULL)
		weston_shell_utils_curtain_destroy(shell->background);
	weston_layer_fini(&shell->layer);
	weston_layer_fini(&shell->background_layer);
	free(shell);
}

static int
wlvision_shell_init(struct weston_compositor *compositor)
{
	struct probe_shell *shell;
	struct weston_output *output;

	shell = calloc(1, sizeof *shell);
	if (shell == NULL)
		return -1;

	/* A shell module loaded twice must not create two desktops. */
	if (!weston_compositor_add_destroy_listener_once(
		    compositor, &shell->destroy_listener, probe_shell_destroy)) {
		free(shell);
		return 0;
	}

	shell->compositor = compositor;
	wl_list_init(&shell->toplevels);
	shell->next_index = 0;
	shell->next_request_id = 0;

	weston_layer_init(&shell->background_layer, compositor);
	weston_layer_init(&shell->layer, compositor);
	weston_layer_set_position(&shell->background_layer,
				  WESTON_LAYER_POSITION_BACKGROUND);
	weston_layer_set_position(&shell->layer, WESTON_LAYER_POSITION_NORMAL);

	shell->output_created_listener.notify = probe_output_created;
	wl_signal_add(&compositor->output_created_signal,
		      &shell->output_created_listener);
	wl_list_for_each(output, &compositor->output_list, link) {
		probe_ensure_background(shell, output);
		weston_output_set_ready(output);
	}

	shell->desktop = weston_desktop_create(compositor, &probe_desktop_api,
					       shell);
	if (shell->desktop == NULL) {
		report("error handle=app-%u reason=desktop-create-failed",
		       shell->next_index);
		wl_list_remove(&shell->output_created_listener.link);
		wl_list_remove(&shell->destroy_listener.link);
		weston_layer_fini(&shell->layer);
		weston_layer_fini(&shell->background_layer);
		free(shell);
		return -1;
	}

	setvbuf(stdout, NULL, _IOLBF, 0);
	report("ready compositor=%p desktop=%p", (void *) compositor,
	       (void *) shell->desktop);

	return 0;
}

WL_EXPORT int
wet_shell_init(struct weston_compositor *compositor, int *argc, char *argv[])
{
	(void) argc;
	(void) argv;

	return wlvision_shell_init(compositor);
}

/*
 * Weston's module loader (wet_load_module(), used by [core] modules= and by
 * -M) looks this symbol up. Both entry points run the same init.
 */
WL_EXPORT int
wet_module_init(struct weston_compositor *compositor, int *argc, char *argv[])
{
	(void) argc;
	(void) argv;

	return wlvision_shell_init(compositor);
}
