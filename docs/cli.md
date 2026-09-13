# wlvision CLI reference

`wlvision` drives one isolated graphical session from the command line: it
creates a session, starts an application in it, enumerates and manipulates its
windows, injects input, captures frames, and waits on events. Every command is
one process, one invocation, one result.

## Global options

Global options come before the command.

| Option | Value | Meaning |
| --- | --- | --- |
| `--json` | – | Write one JSON envelope on stdout. |
| `--state-root` | directory | Session state directory. Default `$XDG_STATE_HOME/wlvision`. |
| `--image` | reference | Session image. Default is the runtime image. |
| `--context` | name | Container engine context (Docker) or connection name (Podman). |
| `--engine` | `docker` or `podman` | Which container engine to drive. Docker is the default; Podman must be asked for. |

## Envelope

With `--json`, every command and every failure writes exactly one envelope, and
nothing else, on stdout. Application output goes to stderr so it cannot corrupt
the document.

```json
{
  "schema": "wlvision/v1",
  "ok": true,
  "operation": "screenshot",
  "session": "<session>",
  "revision": 12,
  "result": {},
  "warnings": []
}
```

| Field | Meaning |
| --- | --- |
| `schema` | Always `wlvision/v1`. A reader must reject another version. |
| `ok` | True when the requested operation completed. |
| `operation` | Stable operation name, for example `session.create` or `wait`. |
| `session` | Session the operation names, omitted when none applies. |
| `revision` | Compositor revision the operation produced or observed, omitted when none applies. |
| `result` | Operation-specific payload, omitted when there is none. |
| `error` | Failure object with `code`, `message`, `operation`, `session`, `retriable`, and `details`. Present only when `ok` is false. |
| `warnings` | Degraded-protection notices; the operation still succeeded. |

The failure code is authoritative; the message is for humans and may change.

## Exit codes

| Code | Meaning | Typical codes |
| --- | --- | --- |
| 0 | Success. | – |
| 2 | Invalid CLI input. | `usage_error` |
| 3 | Unavailable prerequisite. | `engine_unavailable`, `engine_not_rootless`, `protection_degraded`, `image_unavailable`, `weston_protocol_mismatch` |
| 4 | Session or application failure. | `payload_rejected`, `session_not_ready`, `window_not_found`, `stale_revision`, `capture_failed`, `process_exited` |
| 5 | Timeout. | `wait_timeout` |

Distinguish causes by the JSON code; the exit status only selects retry and
rendering behavior.

## Coordinates and revisions

- Window handles are session-local opaque strings such as `app-1`. A destroyed
  handle is never reused.
- Coordinates passed with a window handle are window-content logical pixels.
- `--revision` names the layout the caller saw. A request whose revision the
  compositor has superseded fails with `stale_revision` instead of acting on a
  window that moved. Re-read `windows` and retry with the new revision.
- Sizes accept a byte count or a `k`, `m`, or `g` suffix (1024-based).
- Durations are Go duration strings such as `500ms`, `30s`, or `5m`.

## Resize completion

`resize` succeeds only once the application has committed a buffer that matches
the configure it was given. `--timeout` bounds how long the command waits for
that commit; without it the command uses the session's deadline. A success
reports the sizes each source observed, and a `wait_timeout` failure reports
them in its details:

| Field | Source |
| --- | --- |
| `requested_width`, `requested_height` | what the caller asked for |
| `configured_width`, `configured_height` | the size the module configured the application with |
| `committed_width`, `committed_height` | the content size the application committed |
| `visible_width`, `visible_height` | the toplevel geometry the module reports |

## Session lifecycle

| Command | Flags | Result |
| --- | --- | --- |
| `doctor` | – | `state_root`, `image`, `control_uid`, `application_uid`, `keyboard`, `capabilities`, `degradations`, `sessions`, `anomalies` |
| `session create` | `--session`, `--memory`, `--pids`, `--file-size`, `--open-files`, `--retention`, `--wait` | `session` record, and a `message` when `--wait` is absent |
| `session list` | – | `sessions`, `anomalies` |
| `session inspect` | `--session` | `session`, `anomalies` |
| `session close` | `--session`, `--stop-timeout` | the closed `session` record |
| `session purge` | – | `purged`, `message` |
| `run` | `--session`, `--ephemeral`, `--workdir`, `--env`, `--ready-timeout`, `-- CMD [ARGS...]` | the `session` record |
| `inject` | `--session`, and exactly one of `--binary` or `--bundle`, `--mode`, `--max-bytes`, `--max-files` | `digest`, `path`, `bytes`, `received_at` |
| `logs` | `--session`, `--tail` | `tail`, and `logs` in JSON mode |

Notes:

- `session create` without `--wait` leaves the session in `creating`; pass
  `--wait` to start it and return once it is ready.
- `run` blocks until the application exits. Start it in the background when
  other invocations must drive the session, and use `wait --process-exit` to
  reap it. `--ephemeral` closes the session when the application exits.
- `inject` streams stdin into the session, so the payload never becomes a host
  file.
- `logs` streams the log to stdout in human mode; `--json` carries it in the
  envelope instead.

Worked example:

```bash
wlvision --json doctor
wlvision --json session create --session <session> --wait
wlvision --json inject --session <session> --binary app --mode 0755 < payload
wlvision --json run --session <session> --ephemeral -- /run/wlvision/payload/app &
wlvision --json logs --session <session> --tail 100
wlvision --json session close --session <session>
```

## Windows and input

| Command | Flags | Result |
| --- | --- | --- |
| `windows` | `--session` | `revision`, `toplevels[]` with `handle`, `title`, `app_id`, `x`, `y`, `width`, `height`, `state`, `revision` |
| `activate` | `--session`, `--window`, `--revision` | window state |
| `move` | `--session`, `--window`, `--revision`, `--x`, `--y` | window state |
| `resize` | `--session`, `--window`, `--revision`, `--width`, `--height`, `--timeout` | requested, configured, committed, and visible sizes |
| `close-window` | `--session`, `--window`, `--revision` | window state |
| `click` | `--session`, `--window`, `--revision`, `--x`, `--y`, `--button` | the revision produced |
| `pointer` | `--session`, one of `--x`/`--y`, `--button`/`--state`, or `--axis`/`--value` | `kind` |
| `scroll` | `--session`, `--axis`, `--value` | `axis`, `value` |
| `key` | `--session`, exactly one of `--keycode` or `--name`, `--state` | the key transition sent |
| `type` | `--session`, and either `--text` or one positional text | the typed result |

Notes:

- Without `--state`, `key` sends a press and a release.
- `--button` is a pointer button number; the default is the left button.
- `--axis 1` is the horizontal scroll axis; `--axis 0` is vertical.

Worked example:

```bash
wlvision --json windows --session <session>
wlvision --json click --session <session> --window <handle> --revision <revision> --x 120 --y 80
wlvision --json type --session <session> --text hello
wlvision --json key --session <session> --name Return
wlvision --json resize --session <session> --window <handle> --revision <revision> --width 800 --height 600 --timeout 5s
```

## Capture and timing

| Command | Flags | Result |
| --- | --- | --- |
| `screenshot` | `--session`, `--output` | `path`, `digest`, `width`, `height`, `format`, `frame_sequence`, `revision` |
| `capture` | `--session`, `--interval`, `--duration`, `--frames`, `--output`, `--contact-sheet` | `manifest`, `contact_sheet`, `interval`, `duration`, `bytes`, `captured`, `deduplicated`, `missed`, `failed`, `truncated`, and `frames[]` |
| `wait` | `--session`, `--timeout`, and one of `--stable-for`, `--window-count`, `--process-exit`, `--new-frame`, or `--window` with `--width`/`--height`/`--state` | `kind`, `duration`, `probes`, `observations`, `revision`, and the last `frame`, `windows`, or `exit_code` |

Notes:

- `screenshot` defaults to `screenshot.png` inside the session export
  directory.
- `capture` defaults to a 100 ms interval over 2 s, capped at 25 frames. It
  writes numbered frames, a `manifest.json`, and, with `--contact-sheet`, a
  contact sheet.
- `wait` has a finite deadline: `--timeout`, or a 10 s default. On a timeout it
  fails with `wait_timeout` and still reports what it observed.
- `wait --process-exit` reports the application's `exit_code`.

Worked example:

```bash
wlvision --json screenshot --session <session> --output before.png
wlvision --json capture --session <session> --duration 2s --interval 100ms --contact-sheet
wlvision --json wait --session <session> --stable-for 500ms --timeout 10s
wlvision --json wait --session <session> --window <handle> --width 800 --height 600
```

## Artifacts

Screenshots and captures are the only host writes wlvision performs for a
caller. They land under the state root, partitioned by session:

```text
$XDG_STATE_HOME/wlvision/export/<session>/
```

An artifact name supplied with `--output` is relative to that directory. An
absolute path, a parent traversal, a symbolic link, and an existing target that
is not a regular file are all refused with `usage_error`; a name is never
resolved outside the tree. Writes are staged next to the target and renamed, so
a reader never sees a half-written picture. Moving an artifact elsewhere is the
caller's own action, outside this CLI.

Session records live beside the export tree in the same state root. Inspect or
close them with the `session` commands rather than by editing files.
