# Session runtime

One wlvision session is one container: the pinned Weston with the wlvision
shell, a PID 1 supervisor, a resident controller, and whatever application the
caller injects. This document records the layout and the identity model the
container enforces.

## Identities

| Identity | UID | Runs |
| --- | --- | --- |
| control | 1000 | Weston, the supervisor, the resident controller, `wlvision-call`, payload reception |
| application | 1001 | injected payloads and the commands `wlvision run` launches |

The two identities are the isolation boundary. The control global is
advertised to every client in the session, so the module checks the peer's
credentials and refuses a bind from any UID other than the control one, with
deny-by-default when `WLVISION_CONTROL_UID` is unset. The control socket has
the same gate: the controller only answers its own UID. File modes are a second
line, not the boundary — `test/module-runtime/` proves the refusal with a
world-accessible runtime directory.

## Layout

Everything the session writes lives under `/run/wlvision`, which the engine
mounts as a bounded, memory-backed, sticky world-writable directory. The
supervisor creates the rest and owns it:

| Path | Mode | Purpose |
| --- | --- | --- |
| `/run/wlvision/control` | 0700 | control socket, status file, readiness marker, pid files |
| `/run/wlvision/wayland` | 0770 | the compositor's display socket |
| `/run/wlvision/payload` | 0755 | injected binaries and bundles |
| `/run/wlvision/export` | 0700 | captures the controller stores |

The display socket is readable by both identities because applications must
connect to it; the control directory is not, and that is the asymmetry that
matters. The mounts are world-writable with the sticky bit because the container
engine creates a tmpfs owned by root, and a session running as the control UID
has to be able to create its own directories inside them.

`/tmp` and `/home/agent` are the same kind of mount. The image itself is
read-only, the network is `none`, and no host path is mounted into a session.

## Readiness

Readiness is a report, not a timer. The controller writes
`control/agent.ready` only after it has bound the control global, the capture
protocol, and the output, and after the capture source has announced its size
and format. `wlvision-supervisor status` then combines that marker with the
liveness of the compositor and the controller, so a probe run through an exec
reports a session whose processes died instead of the supervisor's last
optimistic observation.

## Verification

`test/integration/run.sh` builds the session image, cross-compiles the
binaries, and runs the lifecycle gate against a real rootless engine. The gate
creates a session with the CLI and checks, in order: readiness, that the
application identity cannot read the control directory nor reach the
controller, that the image is read-only, that the session has no network, that
no host path is visible inside it, that an injected payload is executable by
the application identity, that the CLI can run an application, that an
application's window is observable and the session capturable, that the logs
carry the session's own progress, that the session survives its application,
and that closing removes the container.
