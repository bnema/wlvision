---
name: wlvision
description: Run a graphical program in an isolated headless Wayland session and
  observe it through screenshots, window state, and bounded frame captures. Use
  when a program must actually run and be seen on a machine with no display and
  no interactive user.
---

# wlvision

wlvision starts an application in an isolated session that runs a headless
native Wayland compositor, and drives it through a JSON CLI: window enumeration,
pointer and keyboard input, resize, screenshots, frame bursts, and event waits.
It is the tool for "run this GUI and show me what happened".

## When to use it

Use wlvision when all of these hold:

- a program must actually run, not merely start;
- its window must be seen, so a screenshot or a frame sequence is the evidence;
- no display, no session bus, and no interactive user are available.

Do not use wlvision when:

- the program can be exercised headlessly as a library or a command line;
- it needs the network, the host filesystem, a device, or a GPU, because a
  session has none of these;
- it is an X11-only program, because V1 runs native Wayland clients only;
- the goal is to inspect the host desktop, which wlvision does not touch.

## The contract

- Every command writes exactly one JSON envelope to stdout. Parse it; never
  scrape human-readable text.
- The envelope carries `schema`, `ok`, `operation`, `session`, `revision`,
  `result`, `error`, and `warnings`. `schema` is `wlvision/v1`.
- On failure `ok` is false and `error.code` is the authoritative reason;
  `error.message` is for humans, `error.details` carries small annotations, and
  `error.retriable` says whether repeating can help.
- Exit codes select retry and rendering behavior: 0 success, 2 invalid input, 3
  unavailable prerequisite, 4 session or application failure, 5 timeout.
  Distinguish causes by `error.code`, not by the exit status.
- Put `--json` before the command.
- Artifacts are written under the state root, one directory per session, at
  `$XDG_STATE_HOME/wlvision/export/<session>/`. Use the `path` a result reports;
  never name a path elsewhere.

## Workflow

1. Diagnose the environment. Run `doctor` and read `result.capabilities`
   (rootless, seccomp, cgroup, limit support) and `result.degradations`. Exit
   code 3 means the prerequisite is missing: stop and report it.
2. Create a named session and wait for readiness. `session create` with `--wait`
   returns only once the compositor, the controller, and the display are ready.
   A session created without `--wait` is not usable yet.
3. Put the payload in the session. `inject` streams a binary or a tar bundle
   over stdin and reports the `path` the session stored it at. Use that path when
   starting the application. `--binary` and `--bundle` are mutually exclusive.
4. Start the application with `run`. It blocks until the application exits, so
   launch it in the background and drive the session from other invocations. The
   application's own output goes to stderr when `--json` is set, keeping stdout
   to the one envelope.
5. Learn handles and revisions with `windows`. A handle such as `app-1` is
   session-local and is not reused; the `revision` is the layout generation.
6. Capture before interacting. `screenshot` the current window state so you have
   a baseline to compare against, and keep its `path` and `frame_sequence`.
7. Interact with handle-relative coordinates, naming the revision you saw. Pass
   `--window` and `--revision` to every window command that takes them. If the
   reply is `stale_revision`, re-read `windows` and re-issue against the new
   revision; never retry the old one unchanged.
8. Prefer a wait predicate over a sleep. Use `wait` with `--stable-for`,
   `--window-count`, `--new-frame`, `--window`, or `--process-exit`. Every wait
   has a finite timeout; a timeout is exit code 5 and its result describes what
   was observed, so a caller can decide to widen it or retry.
9. For an animation, take a short bounded burst with `capture` rather than
   several screenshots: name an `--interval` and a `--duration`, and ask for
   `--contact-sheet` to read many frames as one image. A burst reports captured,
   deduplicated, missed, and failed samples in its manifest.
10. On failure, collect evidence before closing. Read `logs` and
    `session inspect`, and keep the session open until the evidence is stored.
    A failed session is retained for exactly that.
11. Close when done: `session close`, or pass `--ephemeral` to `run`. Never leave
    a session behind on success.

## When the image lacks a dependency

A session image carries the runtime a session has. If the application needs a
package the image does not have, build a derived image from a manifest and use
that image for the session:

```bash
wlvision --json image build --manifest wlvision.toml --tag my-app:local
wlvision --json --image my-app:local session create --session <session> --wait
```

The build may reach the network when the manifest allows it; the session that
runs from the result never has one. A manifest names a digest-pinned base (or an
image the engine already holds, pinned by the id it must report), its packages,
the files to copy, and the application's environment and command.

## Happy path

```bash
wlvision --json doctor
wlvision --json session create --session <session> --wait
wlvision --json inject --session <session> --binary <name>
wlvision --json run --session <session> -- /run/wlvision/payload/<name> &
wlvision --json windows --session <session>
wlvision --json screenshot --session <session> --output before.png
wlvision --json click --session <session> --window <handle> --revision <revision> --x 120 --y 80
wlvision --json wait --session <session> --stable-for 500ms
wlvision --json screenshot --session <session> --output after.png
wlvision --json session close --session <session>
```

## Rules

- Never sleep where a wait predicate exists.
- Never send a coordinate without first reading `windows`.
- Never retry a stale-revision request without re-reading `windows`.
- Never mix the mutually exclusive shapes of one command: `inject` takes
  `--binary` or `--bundle`; `key` takes `--keycode` or `--name`; `pointer` takes
  one of motion, button, or axis; `wait` takes one predicate.
- Never write outside the export root; `--output` is a name relative to the
  session export directory.
- Never leave a failed session unclosed once its logs and state are read.
