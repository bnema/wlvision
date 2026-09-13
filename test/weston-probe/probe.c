/*
 * probe.c - pinned-Weston interface probe for wlvision's control module.
 *
 * This file exists to prove, at compile and link time, that every Weston
 * interface the wlvision control module is designed to use exists in the
 * pinned Weston revision (images/arch/weston.lock):
 *
 *   weston 16.0.0, commit d1882b0a544ae2197b597a6e39478e719bc54302
 *
 * It is built as a shared *module* - wet_module_init() is the symbol the
 * Weston frontend looks up with dlsym() (see frontend/main.c) - and it is
 * deliberately never run: several helpers below dereference compositor state
 * that only exists when the equivalent code runs inside Weston.  The
 * acceptance criterion is that every referenced symbol is declared by a
 * header that ships with the pin and is exported by the built libweston-16.so
 * or by libwayland-server.so.
 *
 * Where each interface comes from is called out inline:
 *
 *   [public]          headers under include/libweston/ - installed by
 *                     `ninja install` under include/libweston-16/
 *   [private]         headers under libweston/ - source tree only, never
 *                     installed
 *   [wayland-server]  <wayland-server-core.h> - libwayland
 *
 * The [private] tag does not appear below: everything the control module
 * needs turned out to be public, which is one of the probe's findings.  See
 * test/weston-probe/README.md.
 */

#include <stdbool.h>
#include <stdint.h>
#include <sys/types.h> /* pid_t, uid_t, gid_t */
#include <time.h>

/* [public] libweston.h: compositor, seats, pointer/keyboard, outputs,
 * capture authority. */
#include <libweston/libweston.h>
/* [public] desktop.h: desktop surface listeners and accessors. */
#include <libweston/desktop.h>
/* [wayland-server] wl_client_get_credentials(), wl_resource_get_client(),
 * wl_listener, wl_signal_add(), wl_resource_for_each(). */
#include <wayland-server-core.h>

/* libweston's public headers do not pull in shared/helpers.h, which is where
 * WL_EXPORT is normally defined; make sure the module entry point is exported
 * even if it is not. */
#ifndef WL_EXPORT
#define WL_EXPORT __attribute__ ((visibility ("default")))
#endif

struct probe_state {
	struct weston_compositor *compositor;

	/* Capture authority registration (libweston.h). */
	struct wl_listener capture_authority;

	/* Post-repaint listener, attached to an output's frame_signal. */
	struct wl_listener frame_listener;
	unsigned int frames;
};

/*
 * Desktop shell listeners.
 *
 * [public] desktop.h: struct weston_desktop_api with surface_added and
 * surface_removed, plus the title/app_id/geometry accessors and the
 * set_size/close requests the control module issues.
 */

static void
probe_surface_added(struct weston_desktop_surface *surface, void *user_data)
{
	struct probe_state *state = user_data;

	/* [public] desktop.h */
	const char *title = weston_desktop_surface_get_title(surface);
	const char *app_id = weston_desktop_surface_get_app_id(surface);
	struct weston_geometry geometry =
		weston_desktop_surface_get_geometry(surface);

	/* [public] desktop.h */
	weston_desktop_surface_set_size(surface, geometry.width, geometry.height);
	weston_desktop_surface_close(surface);

	(void)state;
	(void)title;
	(void)app_id;
}

static void
probe_surface_removed(struct weston_desktop_surface *surface, void *user_data)
{
	(void)user_data;

	if (weston_desktop_surface_get_app_id(surface) != NULL)
		weston_desktop_surface_close(surface);
}

static const struct weston_desktop_api probe_desktop_api = {
	.struct_size = sizeof(probe_desktop_api),
	.surface_added = probe_surface_added,
	.surface_removed = probe_surface_removed,
};

static void
probe_desktop_listeners(struct weston_compositor *compositor)
{
	/* [public] desktop.h: installs the listener table above. */
	(void)weston_desktop_create(compositor, &probe_desktop_api, NULL);
}

/*
 * Input injection.
 *
 * [public] libweston.h: weston_pointer_send_motion/button/axis/frame and
 * weston_keyboard_send_key/modifiers/keymap.  The event structs are public
 * too, so they are filled in directly instead of through the
 * *_event_init() helpers.
 */

static void
probe_input_injection(struct weston_compositor *compositor)
{
	struct weston_seat *seat;
	struct timespec ts = { .tv_sec = 0, .tv_nsec = 0 };

	/* [public] libweston.h: struct weston_compositor::seat_list */
	wl_list_for_each(seat, &compositor->seat_list, link) {
		/* [public] libweston.h */
		struct weston_pointer *pointer = weston_seat_get_pointer(seat);
		struct weston_keyboard *keyboard = weston_seat_get_keyboard(seat);

		if (pointer != NULL) {
			struct weston_pointer_motion_event motion = {
				.base = { .ts = ts, .seat = seat },
				.mask = 0,
			};
			struct weston_pointer_button_event button = {
				.base = { .ts = ts, .seat = seat },
				.button = 0x110, /* BTN_LEFT */
				.button_state = WL_POINTER_BUTTON_STATE_PRESSED,
			};
			struct weston_pointer_axis_event axis = {
				.base = { .ts = ts, .seat = seat },
				.axis = WL_POINTER_AXIS_VERTICAL_SCROLL,
				.value = 0.0,
				.has_discrete = false,
				.discrete = 0,
			};

			/* [public] libweston.h: input injection */
			weston_pointer_send_motion(pointer, &motion);
			weston_pointer_send_button(pointer, &button);
			weston_pointer_send_axis(pointer, &axis);
			weston_pointer_send_frame(pointer);
		}

		if (keyboard != NULL) {
			struct weston_key_event key = {
				.base = { .ts = ts, .seat = seat },
				.key = 0, /* KEY_RESERVED */
				.key_state = WL_KEYBOARD_KEY_STATE_PRESSED,
				.key_update_state = STATE_UPDATE_AUTOMATIC,
			};
			struct wl_resource *resource;

			/* [public] libweston.h: input injection */
			weston_keyboard_send_key(keyboard, &key);
			weston_keyboard_send_modifiers(keyboard, 0, 0, 0, 0, 0);
			wl_resource_for_each(resource, &keyboard->resource_list)
				weston_keyboard_send_keymap(keyboard, resource);
		}
	}
}

/*
 * Peer credentials.
 *
 * [wayland-server] wl_resource_get_client() and wl_client_get_credentials().
 * In the control module these run on the wl_resource owned by the client
 * behind a desktop surface; the desktop API route from a surface to its
 * wl_client (weston_desktop_surface_get_client() and
 * weston_desktop_client_get_client(), both [public] desktop.h) is shown here
 * as well because that is how the resource is reached in practice.
 */
static void
probe_peer_credentials(struct weston_desktop_surface *surface,
		       struct wl_resource *resource)
{
	/* [public] desktop.h: surface -> desktop client -> wl_client */
	struct weston_desktop_client *dclient =
		weston_desktop_surface_get_client(surface);
	struct wl_client *surface_client = weston_desktop_client_get_client(dclient);

	/* [wayland-server] resource -> wl_client */
	struct wl_client *resource_client = wl_resource_get_client(resource);

	pid_t pid = 0;
	uid_t uid = 0;
	gid_t gid = 0;

	/* [wayland-server] peer credentials of the owning wl_client */
	wl_client_get_credentials(surface_client, &pid, &uid, &gid);
	wl_client_get_credentials(resource_client, &pid, &uid, &gid);

	(void)pid;
	(void)uid;
	(void)gid;
}

/*
 * Capture authority.
 *
 * [public] libweston.h: struct weston_output_capture_attempt, struct
 * weston_output_capture_client and
 * weston_compositor_add_screenshot_authority().
 *
 * Note this is *not* a private interface: both the attempt struct and the
 * authority function are declared in the installed libweston.h.  The private
 * libweston/output-capture.h (never installed) declares the renderer-side
 * capture helpers and weston_compositor_install_capture_protocol() instead,
 * and does not declare either of these two names.
 */
static void
probe_capture_authority_cb(struct wl_listener *listener,
			   struct weston_output_capture_attempt *attempt)
{
	struct probe_state *state =
		wl_container_of(listener, state, capture_authority);

	/* [public] libweston.h: who is asking for the capture */
	const struct weston_output_capture_client *const who = attempt->who;

	if (who != NULL && who->client != NULL) {
		attempt->authorized = true;
	} else {
		attempt->denied = true;
	}

	(void)state;
}

static void
probe_capture_authority(struct probe_state *state)
{
	/* [public] libweston.h: register the authority callback.  The function
	 * stores it into state->capture_authority.notify() itself (with a cast
	 * to wl_notify_func_t), so no .notify assignment is needed here. */
	weston_compositor_add_screenshot_authority(state->compositor,
						   &state->capture_authority,
						   probe_capture_authority_cb);
}

/*
 * Post-repaint hook.
 *
 * [public] libweston.h: struct weston_compositor::output_list and
 * struct weston_output::frame_signal, which is emitted after every repaint of
 * that output.
 */
static void
probe_frame_cb(struct wl_listener *listener, void *data)
{
	struct probe_state *state = wl_container_of(listener, state, frame_listener);

	(void)data;
	state->frames++;
}

static void
probe_post_repaint(struct probe_state *state)
{
	struct weston_output *output;

	wl_list_for_each(output, &state->compositor->output_list, link) {
		state->frame_listener.notify = probe_frame_cb;
		/* [wayland-server] wl_signal_add(), target frame_signal */
		wl_signal_add(&output->frame_signal, &state->frame_listener);
		break; /* probe_state holds a single listener */
	}
}

/*
 * Module entry point.
 *
 * The Weston frontend loads modules with
 * weston_load_module(name, "wet_module_init", MODULEDIR) (frontend/main.c),
 * so this symbol is what makes /probe.so a module rather than a plain
 * library.  It is never executed by the probe build: its only job is to keep
 * the helpers above referenced so the compiler emits them and the linker has
 * to resolve every symbol they use.
 */
WL_EXPORT int
wet_module_init(struct weston_compositor *compositor, int *argc, char *argv[])
{
	static struct probe_state state;

	(void)argc;
	(void)argv;

	state.compositor = compositor;

	(void)state;
	(void)probe_desktop_listeners;
	(void)probe_input_injection;
	(void)probe_peer_credentials;
	(void)probe_capture_authority;
	(void)probe_post_repaint;

	return 0;
}
