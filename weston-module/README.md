# weston-module

`wlvision-shell.c` is wlvision's Weston shell and privileged control surface.

Weston loads it as its shell (`shell=wlvision-shell` in `weston.ini`, which the
loader resolves to `wlvision-shell-shell.so` and `wet_shell_init`). It creates
the `weston_desktop` instance, owns the window list, and serves the private
`wlvision_control_v1` protocol defined in `../protocol/wlvision-control.xml`.
`../docs/weston-baseline.md` records why the shell is ours rather than a module
loaded beside the normal desktop shell.

## What it owns

- Opaque handles (`app-N`), assigned monotonically and never reused.
- The session revision, raised on every observable window or layout change.
  Window requests carry the revision the caller saw and are refused with
  `stale_revision` when it moved.
- Enumeration, activation, movement, configure-to-commit resize and close.
  A resize answers with `resize_configured` carrying the size the application
  was actually configured with, then completes with `resize_done` only once
  the application commits a buffer whose content size matches that configure;
  `resize_done` carries the configured, committed and visible sizes.
- Pointer and keyboard injection into the compositor's seat.
- Capture authorization: only the client that was granted capture through the
  control protocol may use Weston's capture protocol; every other attempt is
  denied.
- Frame sequencing: each repaint is numbered, and the frame event carries the
  capture authorization that was outstanding when it arrived.

It never encodes pixels, writes files, parses JSON or manages containers.

## Access control

The control global is advertised to every client in the session, so access is
gated on peer credentials: the supervisor sets `WLVISION_CONTROL_UID`, and a
bind from any other UID is refused with a protocol error before a manager
resource exists. When the variable is unset the module refuses every bind
rather than guessing.

## Building

The module is compiled against the pinned Weston revision, using the same
`pkg-config` metadata the pinned build installs:

```bash
gcc -shared -fPIC -Wall -Wextra -Wno-unused-parameter \
  -o wlvision-shell.so \
  wlvision-shell.c generated/wlvision-control-protocol.c \
  $(pkg-config --cflags --libs weston)
```

`generated/` comes from `../scripts/generate-protocols.sh`; the C server
bindings are produced by `wayland-scanner` and are never edited by hand. The
image installs the result next to a `wlvision-shell-shell.so` symlink, which is
the name Weston's shell loader looks for.

The build is currently a direct compiler invocation because the image compiles
the module against the installed pinned libweston rather than inside Weston's
own build tree. A `meson.build` becomes worthwhile only if we ever build it as
part of that tree.

## State

The module compiles cleanly with `-Wall -Wextra` against the pinned revision and
exports both entry points. Its runtime behaviour is verified by
`../test/module-runtime/`: a real controller binds the protocol, enumerates and
activates windows, resizes one, injects pointer and keyboard events, captures
the framebuffer after authorization, and is refused the protocol when it runs
under a different UID.

## Known follow-ups

Two review findings are recorded here rather than fixed, because both touch
object lifetime in ways that need their own verification against a running
compositor:

- `shell_destroyed` frees the window list and the `weston_desktop` before any
  surface teardown. The pinned Weston never tears those surfaces down after
  `compositor->destroy_signal`, so no path to the resulting use-after-free was
  found, but the ordering is not safe by construction. Upstream `kiosk-shell`
  destroys its surfaces first.
- The module installs no metadata listener, so a title or application-id change
  that is not followed by another observable change is not reported until the
  next event. Weston 16 exposes
  `weston_desktop_surface_add_metadata_listener`, which is the intended fix; it
  needs a listener whose removal on surface destruction is verified first.
