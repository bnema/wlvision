# Module runtime verification

Proves the Weston module (`../../weston-module/`) against a running compositor
rather than against a fake one.

`run.sh` builds an image from `Containerfile` (the pinned Weston plus
`wlvision-shell`), cross-compiles the Go probe in `../controlprobe`, and runs one
session inside the container:

1. Weston starts headless with `--shell=wlvision-shell --fake-seat`, as the
   control user, with `WLVISION_CONTROL_UID` set to that user.
2. The fixture application (`../weston-shell/wlvision-shell-client.c`) maps a
   toplevel and commits a buffer.
3. The probe drives the control protocol as the control user.
4. The probe runs again as a foreign user and must be refused.

The runtime directory is world-accessible on purpose: the refusal has to come
from the module's peer credential check, not from filesystem permissions.

## What the probe proves

- the controller connects and the module creates it;
- snapshot enumerates the application's window with its handle, title, app id
  and geometry;
- activate succeeds with the current revision, is refused as `stale_revision`
  with an outdated one, and as `window_not_found` for an unknown handle;
- resize completes only after the application commits a buffer matching the
  configure it was sent, and the result reports the requested, configured,
  committed and visible sizes separately. The fixture always commits the size
  it was configured with, so this gate cannot produce a resize deadline; that
  path is covered deterministically by the control client's tests;
- pointer motion, a button press and a key press are injected;
- capture is refused before the module authorizes the connection, and completes
  afterwards with real pixels and a PNG;
- the frame the capture produced carries the capture authorization it belongs
  to;
- a foreign UID cannot bind the control protocol at all.

## Running it

```bash
bash test/module-runtime/run.sh
```

The image is built from the repository root because the module and its generated
protocol sources live there.
