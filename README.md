# wlvision

wlvision runs a graphical program in a disposable, isolated Linux session and
lets an agent see and drive it: a rootless container runs a pinned, headless
Weston with a native Wayland client inside it, and a JSON CLI enumerates
windows, injects pointer and keyboard input, resizes, captures screenshots and
frame sequences, and waits on what the session shows.

It exists for the case where a program must actually run and be seen, on a
machine with no display, no session bus, and no interactive user.

## Requirements

- A rootless container engine: Docker Rootless, or Podman in rootless mode with
  `--engine podman`. A rootful engine is refused; there is no override.
- Go 1.27 to build the CLI and the session binaries.
- The pinned session image. `doctor` reports what the host provides, including
  any protection the engine cannot enforce.

## Use

```bash
wlvision --json doctor                          # capabilities, keyboard, degradations
wlvision --json session create --session demo --wait
wlvision --json inject --session demo --binary app  < ./app
wlvision --json run --session demo -- /run/wlvision/payload/app
wlvision --json windows --session demo          # handles, geometry, revisions
wlvision --json screenshot --session demo --output first.png
wlvision --json click --session demo --window app-1 --x 120 --y 80
wlvision --json type --session demo "hello"
wlvision --json wait --session demo --stable-for 300ms --timeout 5s
wlvision --json capture --session demo --interval 100ms --duration 2s --output frames --contact-sheet
wlvision --json logs --session demo
wlvision --json session close --session demo
```

Every command writes one JSON envelope with `schema: wlvision/v1`; `--json` goes
before the command. Exit codes are 0 success, 2 invalid input, 3 unavailable
prerequisite, 4 session or application failure, and 5 timeout, and the
authoritative reason is always `error.code` in the envelope. Artifacts are
written under `$XDG_STATE_HOME/wlvision/export/<session>/` and nowhere else.

## Documentation

| Document | What it covers |
| --- | --- |
| [docs/cli.md](docs/cli.md) | every command, flag, result shape, and exit code |
| [docs/security.md](docs/security.md) | what a session can reach, degraded protection, the threat model |
| [docs/session-runtime.md](docs/session-runtime.md) | the session contract: identities, paths, keyboard, capture, verification |
| [docs/weston-baseline.md](docs/weston-baseline.md) | the pinned Weston revision and why the shell is wlvision's |
| [docs/transport-decision.md](docs/transport-decision.md) | the Wayland transport and the protocol generation chain |
| [docs/dependency-readiness.md](docs/dependency-readiness.md) | the state of the dependencies wlvision builds on |
| [skills/wlvision/SKILL.md](skills/wlvision/SKILL.md) | the workflow an agent follows |
| [wlvision.example.toml](wlvision.example.toml) | a manifest for building a session image |

## Layout

| Path | What lives there |
| --- | --- |
| `cmd/wlvision` | the outer CLI an agent drives |
| `cmd/wlvision-supervisor` | PID 1 of a session: compositor, controller, reaping, payloads |
| `cmd/wlvision-agent` | the resident controller that owns the privileged connection |
| `cmd/wlvision-call` | one request, one reply, then exit |
| `internal/engine` | the container engine boundary and its adapters |
| `internal/session` | session state, records, reconciliation, the agent bridge |
| `internal/control`, `protocol/` | the private control protocol and its generated bindings |
| `weston-module/` | the Weston shell that owns window handles, input and capture authorization |
| `internal/input`, `internal/capture` | window/input semantics, bursts, manifests, waits |
| `internal/manifest` | manifests that build a session image reproducibly |
| `images/arch` | the session image |
| `test/` | the gates: module runtime, session lifecycle, interaction and vision, isolation, acceptance |

## Development

wlvision is developed beside its two sibling modules, WLTurbo and LibWL Devices.
Check them out as siblings and apply the local replacements:

```bash
scripts/use-local-deps.sh     # apply, or refresh, the sibling replacements
scripts/check-local-deps.sh   # assert they are exactly what the modules expect
scripts/clear-local-deps.sh   # remove them for a release build
```

`scripts/verify.sh` is the gate a change must survive: formatting, the pinned
Weston baseline, the regenerated protocol bindings, the development
replacements, the build, the vet, the race suite, the keyboard table, the module
runtime gate, the session gates (lifecycle, interaction and vision, isolation,
acceptance), and the release hygiene check. `--fast` skips the stages that build
container images.

## Not in V1

X11 clients are out of scope: V1 runs native Wayland clients only. An optional
`xwayland-satellite` image capability, GPU rendering, multiple outputs,
accessibility inspection, perceptual image thresholds, a live viewer, and an MCP
adapter over the CLI are follow-ups.
