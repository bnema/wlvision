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
| Capture authorization | `weston_compositor_add_screenshot_authority`, `struct weston_output_capture_attempt`, `struct weston_output_capture_client` | `include/libweston/libweston.h` | public, exported |
| Peer credentials | `wl_client_get_credentials`, `wl_resource_get_client` | `wayland-server-core.h` | public (libwayland) |
| Headless output and pixman renderer | configuration only: the module uses neither header | image configuration | not needed |

The probe measured this rather than assuming it. `nm -D --defined-only` on
the built `libweston-16.so.0` reports all six desktop-surface symbols, eight
input-injection symbols, both seat accessors and
`weston_compositor_add_screenshot_authority` as exported; `wl_client_get_credentials`
and `wl_resource_get_client` are exported by `libwayland-server.so.0`. Two
findings change earlier assumptions: the capture authority is **public**, not a
private interface — `libweston/output-capture.h` is not installed and does not
declare it, so the module needs no source-tree include path — and there is **no
separate `libweston-desktop` library**; desktop is compiled into
`libweston-16.so`.

## Repaint and capture

`compositor->output_capture.weston_capture_v1` holds the capture global, and
`struct weston_output` carries `repaint_only_on_capture`, which is what lets a
capture request force a repaint when an application has produced no damage. The
pixman renderer emits `output->frame_signal` at the end of a repaint
(`libweston/pixman-renderer.c:641`), which is the hook the module uses to assign
monotonic frame sequences and to correlate the capture request that forced the
repaint.

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

### Resolution

Option A was built and measured before committing to it: `test/weston-shell/`
contains the smallest shell that proves the design, run against the pinned
revision with no source patch. Its observed sequence for one client is:

```text
added app-1 title=wlvision-probe-title app_id=wlvision.probe geometry=0,0,0x0
configured request_id=1 requested=320x240
committed buffer=320x240 request_id=1 commit=1 matching=yes
activated seat=default keyboard=yes keyboard_focus=yes
configured request_id=2 requested=640x480
committed buffer=640x480 request_id=2 commit=2 matching=yes
removed app-1 live_toplevels=0
```

Two concurrent clients coexist as `app-1` and `app-2` with request ids
continuing across runs, and a later run reuses neither handle. Enumeration,
title, app id, geometry, view mapping, layer placement, activation, keyboard
focus, xdg activated-state propagation and configure-to-commit resize therefore
all work through installed public headers and exported symbols only.

Three practical notes, none an API gap: a shell must paint something, because
repainting an empty scene graph asserts, and `weston_shell_utils_curtain_create`
is the public answer; keyboard focus needs a seat with a keyboard, which the
headless backend only has with `--fake-seat`; and `weston_seat_set_keyboard_focus`
is private while `weston_view_activate_input` is the public path. Weston's shell
loader looks for `NAME-shell.so` and `wet_shell_init`, so a module named
`wlvision-shell.so` is installed next to a `wlvision-shell-shell.so` symlink.

**Option A is therefore the design.** wlvision ships the shell; the pinned
revision stays unpatched, and the module can own window enumeration, activation,
move, resize, close, input injection, capture authorization and frame
sequencing. The module is `../weston-module/wlvision-shell.c`, and
`../test/module-runtime/` is the gate that proves it at runtime: a real
controller enumerates and activates the application's window, completes a
configure-to-commit resize, is refused a stale revision, injects input, is
granted capture while a foreign UID is refused the protocol, and receives the
frame that carried the capture.

### The options that were weighed

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

**B. Patch Weston in the image** — smaller wlvision code and it keeps
desktop-shell, but it forks the compositor, breaks the "pinned revision, no
source patch" property, and every future Weston bump must rebase the patch.
This is not taken.

## Probe

`test/weston-probe/` builds the pinned revision in a container and compiles
`probe.c` against it, so every interface above is proven to exist and link at
this commit rather than assumed:

```bash
docker build -t wlvision-weston-inputs:local -f test/weston-probe/Containerfile.inputs test/weston-probe
docker build --network=none -t wlvision-weston-probe:local -f test/weston-probe/Containerfile test/weston-probe
docker run --rm wlvision-weston-probe:local sh -c 'ls -l /probe.so && ldd /probe.so'
```

The input stage fetches the lock's archive URL and fails unless its digest
matches, then verifies every `file_sha256/*` entry against the unpacked tree;
the verification stage runs with `--network=none` and uses only the packages
that stage cached. The build produces `/probe.so` as a module linked against
`libweston-16.so.0`, with `wet_module_init` present as a dynamic symbol.

`cairo` and `libpng` are required by Weston's own meson unconditionally, so they
are build dependencies of the image; no meson option had to be relaxed beyond
disabling DRM, GL, X11, the Wayland backend, VNC, RDP, PipeWire, Xwayland, tests,
docs and tools. Exact commands, base-image digests and the offline build evidence
are in `test/weston-probe/README.md`.
