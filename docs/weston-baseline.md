# Weston baseline

## Pin

| Item | Value |
| --- | --- |
| Version | 16.0.0 (latest stable release) |
| Commit | `d1882b0a544ae2197b597a6e39478e719bc54302` |
| Source archive | `https://gitlab.freedesktop.org/wayland/weston/-/archive/16.0.0/weston-16.0.0.tar.gz` |
| Source archive SHA-256 | `6a81af51045ccb2813f3a1d63ff8a66c121743c1a5ff3ce78388660bf650c55c` |
| libweston ABI major | 16 |
| Lock file | `images/arch/weston.lock` |
| Checker | `scripts/check-weston-baseline.sh` |

The lock records the digest of every Weston file wlvision depends on. The
checker verifies the lock's shape and fails when the vendored
`protocol/weston-output-capture.xml` stops matching the pinned revision, so a
silent edit or a version drift cannot pass unnoticed.

## Interfaces the control module needs

Classification: **public** means the header is installed by `meson install`;
**source-tree** means it exists only inside the Weston build tree, which is why
the module is compiled against that exact revision inside the image.

| Need | Interface | Header | Kind |
| --- | --- | --- | --- |
| Toplevel metadata and control | `weston_desktop_surface_get_title`, `_get_app_id`, `_get_geometry`, `_get_client`, `_get_surface`, `weston_desktop_surface_set_size`, `_set_activated`, `_close`, `weston_desktop_surface_add_metadata_listener`, `struct weston_desktop_api` with `surface_added` / `surface_removed` | `include/libweston/desktop.h` | public (libweston-desktop) |
| Desktop instance | `weston_desktop_create`, `weston_desktop_destroy` | `include/libweston/desktop.h` | public |
| Pointer injection | `weston_pointer_send_motion`, `_send_button`, `_send_axis`, `_send_axis_source`, `_send_frame` | `include/libweston/libweston.h` | public |
| Keyboard injection | `weston_keyboard_send_key`, `_send_modifiers`, `_send_keymap` | `include/libweston/libweston.h` | public |
| Seat and output enumeration | `compositor->seat_list`, `compositor->output_list`, `struct weston_seat` keyboard/pointer, `struct weston_output` geometry and `frame_signal` | `include/libweston/libweston.h` | public |
| Post-repaint observation | `weston_output.frame_signal`, emitted by the pixman renderer after a repaint | `include/libweston/libweston.h`, `libweston/pixman-renderer.c` | public field, emitted by `libweston/pixman-renderer.h` |
| Capture authorization | `weston_compositor_add_screenshot_authority`, `struct weston_output_capture_attempt`, `struct weston_output_capture_client` | `include/libweston/libweston.h` | public |
| Capture internals | `weston_output_capture_info_repaint_done` and the source enum | `libweston/output-capture.h` | source-tree |
| Peer credentials | `wl_client_get_credentials`, `wl_resource_get_client` | `wayland-server-core.h` | public (libwayland) |
| Headless output and pixman renderer | `libweston/backend-headless.h`, `libweston/pixman-renderer.h` | source-tree | source-tree |

`compositor->output_capture.weston_capture_v1` holds the capture global;
`repaint_only_on_capture` exists on `struct weston_output`, which is what lets a
capture request force a repaint when an application has produced no damage.

## Blocking finding: a module cannot observe toplevels from beside the shell

The design assumed a module loaded next to the normal desktop shell could
enumerate and control application toplevels. At Weston 16.0.0 that is not
possible through any declared interface:

- `weston_desktop_surface` objects belong to a `weston_desktop` instance, and
  each instance creates its own `xdg_wm_base` global
  (`libweston/desktop/xdg-shell.c:2168`). A second instance would race the
  shell's global instead of observing it.
- There is no reverse lookup from `struct weston_surface` to its
  `weston_desktop_surface`: `include/libweston/libweston.h` has no such field and
  no accessor. `weston_surface_get_role` returns a role *name* string only.
- The role object the xdg shell installs, `weston_desktop_xdg_toplevel_role`, is
  not declared in any header.
- `weston_desktop_client_for_each_surface` and the `from_grab_link` /
  `from_client_link` helpers all require the desktop instance that owns the
  surfaces.

So a module can watch `compositor->create_surface_signal` and list
`compositor->surface_list`, but it can neither tell which surface is a toplevel
nor obtain the handle needed to activate, resize or close it. Weston's own test
suite reaches the same conclusion: it runs a purpose-built shell
(`tests/weston-test-desktop-shell.c`) whenever a test must observe surfaces.

### Options

**A. wlvision ships the shell.** Weston runs with
`shell=wlvision-control.so`; that module creates the `weston_desktop` instance,
implements `weston_desktop_api`, owns the control protocol and injects input.
Compatible with the pin, no Weston patch, and closest to what the protocol needs
(handle, title, app id, geometry, activate, move, resize, close). Cost: the
module also owns the shell responsibilities it observes — mapping surfaces into
a layer, view creation and commits, activation and keyboard focus. Weston's
`kiosk-shell` is a working 1547-line reference, and wlvision needs a subset: it
has one output, no decorations, no panels and no session management.
This contradicts the approved spec sentence "Weston runs with ... the normal
desktop shell", so it needs an explicit decision.

**B. Patch Weston in the image** to export the surface-to-desktop link or to
broadcast desktop-surface lifecycle events. Smaller wlvision code and it keeps
desktop-shell, but it forks the compositor, breaks the "pinned revision, no
source patch" property, and every future Weston bump must rebase the patch.

Recommendation: **A**. It keeps the pinned revision unpatched, uses only headers
Weston itself declares, and gives the control protocol the exact operations it
promises. The shell subset it must implement is small and testable, and a
purpose-built shell is exactly how upstream tests observe surfaces.

## Probe

`test/weston-probe/` builds the pinned revision with meson (headless backend,
pixman renderer, libweston and libweston-desktop) in a container and compiles
`probe.c` against it, so every interface above is proven to exist and link at
this commit rather than assumed. Build inputs are fetched and digest-verified in
a separate stage; the verification build runs with `--network=none`.

Result of that build is recorded in `test/weston-probe/README.md`.
