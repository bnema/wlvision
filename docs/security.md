# Security and operations

A wlvision session confines a graphical application so that a buggy or careless
program can be run and observed without a path to user data. This document
states what the session contains, what it can reach, how to read degraded
protection, and what the containment does not defend against.

## Prerequisites

- A rootless container engine: Docker Rootless or Podman in rootless mode.
  `doctor` reports the engine kind, context, server version, and whether the
  engine is rootless. wlvision refuses a rootful engine and has no override;
  the failure is `engine_not_rootless` (exit 3).
- The engine's built-in seccomp profile. A session cannot be confined without
  one; the failure is `protection_degraded` with `details.missing: seccomp`
  (exit 3).
- The pinned runtime image. `--image` overrides the reference for development;
  the default is the image the release gate built. A missing image that cannot
  be fetched fails with `image_unavailable`.
- A writable state root, `$XDG_STATE_HOME/wlvision` by default, for session
  records and exported artifacts.

## What a session can reach

| Reachable | Why |
| --- | --- |
| The compositor's display socket | applications must create Wayland clients |
| Its own pid, mount, and network namespace | the session's processes |
| Injected payloads under `/run/wlvision/payload` | the caller put them there |
| Bounded tmpfs at `/run/wlvision`, `/tmp`, and `/home/agent` | session scratch space |

## What a session cannot reach

| Denied | How |
| --- | --- |
| The network | `--network none`; no interface, no route, no DNS |
| Host files | no bind mount and no host path is mounted; payloads arrive over stdin |
| Host devices | engine default device policy; no device is added |
| Host namespaces | the session runs in its own user, pid, mount, and network namespaces |
| The image's own filesystem | the image root is read-only |
| Linux capabilities | `--cap-drop ALL` |
| New privileges | `--security-opt no-new-privileges` |
| Unsafe syscalls | the engine's built-in seccomp profile |
| The control plane | the application identity cannot reach it (below) |
| Unbounded resources | process, memory, file-size, and open-file limits where the engine supports them |

The container adapter owns every engine flag it passes. A caller describes what
it wants with typed limits and mounts; it cannot add, replace, or drop an
isolation flag, because the create spec has no field for them.

## Identities

| Identity | UID | Runs |
| --- | --- | --- |
| control | 1000 | compositor, supervisor, resident controller, the one-shot caller, payload reception |
| application | 1001 | injected payloads and the commands `run` starts |

The two identities are the isolation boundary between the control plane and the
application. The compositor's control global is refused to any client that is
not the control UID, with deny-by-default when `WLVISION_CONTROL_UID` is unset,
so the check does not depend on filesystem permissions. The control directory
is mode `0700` as a second line, not as the boundary; the runtime test proves
the refusal with a world-accessible directory. The display socket is reachable
by both identities because applications must connect to it, and the asymmetry
between the display socket and the control directory is what keeps the
application a normal Wayland client rather than a controller.

## Limits and tmpfs bounds

A session mounts three bounded, memory-backed tmpfs mounts:

| Mount | Size | Options |
| --- | --- | --- |
| `/run/wlvision` | 64 MiB | read-write, `exec`, `nosuid`, `nodev` |
| `/tmp` | 32 MiB | read-write, `noexec`, `nosuid`, `nodev` |
| `/home/agent` | 32 MiB | read-write, `noexec`, `nosuid`, `nodev` |

Only the runtime mount allows execution, because injected payloads live in it.
The others cannot run code and cannot host device or setuid files. Memory, pids,
file-size, and open-file limits are passed to the engine when the caller asks
for them; a limit the engine cannot enforce is reported, never silently dropped.

## Reading doctor

`wlvision --json doctor` answers whether this host can host a session and what
is already running. Read:

- `result.capabilities`: `rootless`, `seccomp_profile`, `cgroup_version`,
  `cgroup_driver`, `memory_limit`, `pids_limit`, `server_version`, `context`;
- `result.degradations`: the protections the engine cannot enforce;
- `result.sessions` and `result.anomalies`: stored records and reconciliation
  findings;
- `result.keyboard`: the layout the session guarantees, not the host's.

Degradations are warnings, not failures:

| Degradation | Meaning |
| --- | --- |
| `cgroup_v1` | the engine uses cgroup v1, where per-container resource control depends on the host's controller setup |
| `memory_limit_unavailable` | the memory controller is not delegated; a memory limit cannot be enforced |
| `pids_limit_unavailable` | the pids controller is not delegated; a process limit cannot be enforced |

A successful operation carries degraded protection in the envelope's
`warnings`; an operation that needs a missing protection fails instead. Treat a
degradation as a property of the host that the report must carry forward.

## Exports

The only host writes wlvision performs for a caller are the artifacts an agent
asks for: screenshots and capture bursts. They are written under the state root,
partitioned by session:

```text
$XDG_STATE_HOME/wlvision/export/<session>/
```

An agent-supplied name is relative to that directory. Absolute paths, parent
traversals, symbolic links, and existing non-regular targets are refused with
`usage_error`. Directories are created one component at a time, refusing to walk
through a symlink, and a file is staged next to its target and renamed into
place, so a reader never sees a partial artifact. An agent therefore cannot use
`--output` to write anywhere else on the host: the name never resolves outside
the tree, and no host path is mounted into the session for it to reach. Stored
artifacts are bounded per file and per burst.

## Retention and cleanup

- `session close` stops and removes the container and marks the record closed.
  It is idempotent, so a retried close converges.
- A failed or terminated session is not deleted immediately. The supervisor
  preserves the compositor, the final framebuffer, the process exit record, and
  bounded logs so the failure can be diagnosed; the record carries a retention
  deadline (24 hours by default, `--retention` at create time overrides it).
- `session purge` releases every session whose retention deadline has passed.
- `session list` reports stored records and any anomaly where the record and the
  engine disagree, which is how a crashed CLI leaves a discoverable session
  rather than an orphan container.
- Closing a session is the caller's job; `run --ephemeral` does it
  automatically when the application exits.

## Weston and libweston upgrades

| Item | Value |
| --- | --- |
| Weston version | 16.0.0 |
| Commit | `d1882b0a544ae2197b597a6e39478e719bc54302` |
| libweston ABI major | 16 |
| Lock file | `images/arch/weston.lock` |
| Baseline checker | `scripts/check-weston-baseline.sh` |

The session image builds Weston from that pinned release archive and compiles
`wlvision-control.so` against the same build tree, so the module never crosses a
libweston ABI boundary. The lock records the digest of every Weston file the
module depends on; the checker verifies the lock's shape and fails when the
vendored capture protocol stops matching the pinned revision. A silent edit or a
version drift cannot pass unnoticed.

To upgrade Weston:

1. Change the pin in `docs/weston-baseline.md`, the lock, and the image profile
   together.
2. Rebuild the image and run the module-runtime gate, which exercises
   enumeration, activation, configure-to-commit resize, stale-revision refusal,
   input injection, capture authorization, and the foreign-UID refusal against
   the new revision.
3. Build the next intended revision in CI before accepting it, so an upgrade is
   measured rather than assumed.

## Local dependency replacements

The two Go Wayland dependencies are consumed through local filesystem
replacements during development because no published tags are accepted yet. The
committed module files carry the last published requirements, so the module
graph stays release-clean; `scripts/use-local-deps.sh` applies the replacements,
`scripts/clear-local-deps.sh` reverts them so the module files are byte-identical
to their pre-loop state, and `scripts/check-local-deps.sh` accepts only the
expected replacements.

The release gate resolves them: `scripts/check-release-modules.sh` must pass on
a copy whose replacements are removed, and the module files must contain no
filesystem replacement before a release is cut. The pins themselves are
published only after the transport and protocol gates pass from a clean
checkout.

## Threat model

The goal is to contain a buggy or careless application: one that reads the wrong
file, opens a network connection, allocates until it is killed, or crashes. The
session gives it no user data to read, no network to reach, no host mount to
walk, no device to open, and no privilege to gain, and it bounds the resources
it can consume.

The boundary that matters is the container and the identity split inside it.
Rootless containers rely on the host kernel for that boundary, and they do
**not** defend against a hostile kernel exploit: a local privilege escalation in
the kernel defeats the user namespace and the container with it. wlvision
therefore does not treat a session as a sandbox for intentionally malicious
binaries, and it does not claim to. Run untrusted code only if the host kernel is
patched and the risk is accepted deliberately.

Two further limits are explicit:

- The control plane is protected by peer credentials and a `0700` directory, not
  by a separate kernel boundary. It assumes the application identity is the
  only untrusted one inside the session.
- The engine CLI is invoked with the user's own rootless context and
  credentials. Whatever that user can do to the engine, the session inherits;
  wlvision narrows the per-session flags but does not sandbox the engine itself.

## Follow-up: X11 and xwayland-satellite

V1 is native Wayland only. An application that requires X11 does not run in a
session, and no X server, XWayland, or compatibility surface is part of the
image or the acceptance criteria. Adding xwayland-satellite as an optional image
capability is a follow-up: it needs its own protocol, clipboard, scaling, popup,
and integration tests, and it must not change native Wayland semantics. Do not
assume any X11 behavior in a V1 session.
