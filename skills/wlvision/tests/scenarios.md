# wlvision skill scenarios

Each scenario names the situation, the commands an agent runs, the criteria that
prove it succeeded, and the shortcuts that are forbidden. Every command is real:
it is checked against the CLI before it is trusted.

A command's success is an envelope with `ok: true`; a failure is an envelope
with `ok: false` whose `error.code` is the reason. Exit codes are 0 success, 2
invalid input, 3 unavailable prerequisite, 4 session or application failure, and
5 timeout.

## 1. Startup diagnosis

Situation: an agent is about to run a graphical program and must first learn
whether this machine can host a session and what protections are in force.

```bash
wlvision --json doctor
```

Success criteria:

- Exit code is 0 and `ok` is true.
- `result.capabilities.rootless` is true and
  `result.capabilities.seccomp_profile` is non-empty.
- `result.capabilities.cgroup_version`, `result.capabilities.memory_limit`, and
  `result.capabilities.pids_limit` are read, and every entry in
  `result.degradations` is carried into the report the agent writes.

Forbidden shortcuts:

- Proceeding when `doctor` exits 3 or reports a missing prerequisite.
- Treating an empty `degradations` list as proof that limits were requested and
  enforced; it only means the engine reported none.
- Assuming a rootful engine is usable; wlvision refuses one.

## 2. First screenshot

Situation: an application must be started and its first window captured as
evidence.

```bash
wlvision --json session create --session <session> --wait
wlvision --json inject --session <session> --binary <name>
wlvision --json run --session <session> -- /run/wlvision/payload/<name> &
wlvision --json windows --session <session>
wlvision --json screenshot --session <session> --output first.png
```

Success criteria:

- `session create --wait` returns `ok` with the session in the ready state; the
  agent does not issue a second command until it does.
- `inject` returns the `path` the session stored the payload at, and `run`
  starts exactly that path.
- `windows` returns at least one toplevel with a `handle` and a `revision`.
- `screenshot` returns `ok`, a `path` inside the session export directory, a
  `digest`, and a non-zero `width` and `height`.

Forbidden shortcuts:

- Capturing before the session reports ready, or sleeping instead of using
  `--wait`.
- Guessing a window handle instead of reading `windows`.
- Writing the screenshot with a shell redirection instead of `--output`.

## 3. Stale-revision retry

Situation: an application relaid out its window after the agent read the
handles, so an input request names a revision the compositor has superseded.

```bash
wlvision --json click --session <session> --window <handle> --revision <revision> --x 40 --y 40
wlvision --json windows --session <session>
wlvision --json click --session <session> --window <handle> --revision <revision> --x 40 --y 40
```

Success criteria:

- The first click fails with `error.code` `stale_revision` and exit code 4.
- The agent re-reads `windows` and takes the new `revision` from its result.
- The second click succeeds and reports the revision it produced.
- The new revision is used for every following coordinate command.

Forbidden shortcuts:

- Repeating the click with the same `--revision`.
- Switching to a blind position or dropping `--revision` to force the request
  through.
- Ignoring a `stale_revision` because the exit code is not 2.

## 4. Animation burst

Situation: a program plays an animation and the agent must show that frames
differ over time.

```bash
wlvision --json capture --session <session> --duration 2s --interval 100ms --contact-sheet
```

Success criteria:

- `capture` returns `ok` with `result.captured` and `result.deduplicated` both
  counted, and `captured` greater than zero.
- `result.frames` is in schedule order and every captured entry carries a
  `frame_sequence` and a `digest`.
- `result.manifest` and `result.contact_sheet` are paths inside the session
  export directory.
- `result.missed` and `result.failed` are reported, whatever their value.

Forbidden shortcuts:

- Taking two screenshots with a sleep between them instead of one bounded burst.
- Asking for an unbounded recording; `--duration` and `--frames` keep the burst
  finite.
- Hiding missed or failed samples from the report.

## 5. Post-resize stability

Situation: the agent resized a window and must prove the application committed
the new size before capturing.

```bash
wlvision --json resize --session <session> --window <handle> --revision <revision> --width 800 --height 600 --timeout 5s
wlvision --json wait --session <session> --window <handle> --width 800 --height 600 --timeout 10s
wlvision --json screenshot --session <session> --output resized.png
```

Success criteria:

- `resize` returns `ok` only after the application committed a matching buffer;
  the result reports requested, configured, committed, and visible sizes.
- When the commit does not arrive, `resize` fails with `wait_timeout` (exit 5)
  and the failure details name the sizes that were observed.
- `wait --window` succeeds against the resized geometry and reports the revision
  at which it succeeded.
- The screenshot is taken after the wait, not before it.

Forbidden shortcuts:

- Treating a resize as complete because it returned without error but reports no
  committed size.
- Sleeping instead of waiting for the window predicate.
- Retrying a `wait_timeout` without widening `--timeout` or re-reading `windows`.

## 6. Application crash diagnosis

Situation: the injected application exits unexpectedly and the agent must
retrieve the evidence before it is lost.

```bash
wlvision --json run --session <session> -- /run/wlvision/payload/<name> &
wlvision --json wait --session <session> --process-exit --timeout 30s
wlvision --json logs --session <session> --tail 200
wlvision --json session inspect --session <session>
```

Success criteria:

- `wait --process-exit` succeeds and `result.exit_code` is the application's
  status, non-zero for a crash.
- `logs` returns the session's own log tail, including the application's error
  output.
- `session inspect` reports the recorded process exit and the session state.
- The session is closed only after the logs and the record have been read.

Forbidden shortcuts:

- Closing the session before reading `logs` and `session inspect`.
- Treating an application crash as a session failure and abandoning cleanup.
- Reading the container's log through the engine CLI instead of `logs`.

## 7. Export confinement refusal

Situation: a caller names a capture artifact outside the session export
directory, and wlvision must refuse it instead of writing there.

```bash
wlvision --json screenshot --session <session> --output ../escape.png
wlvision --json screenshot --session <session> --output /tmp/escape.png
wlvision --json screenshot --session <session> --output inside.png
```

Success criteria:

- The traversing and absolute names fail with `error.code` `usage_error` and
  exit code 2; no file is created outside the export root.
- The relative name succeeds and `result.path` is inside the session export
  directory.
- The refusal is reported as final, not retried.

Forbidden shortcuts:

- Retrying with another path outside the export root.
- Writing the artifact through a shell redirection or a host-side copy.
- Reporting the refusal as a transient failure.

## 8. Guaranteed session cleanup

Situation: a workflow has finished, or failed, and the session must not outlive
it.

```bash
wlvision --json session close --session <session>
wlvision --json session inspect --session <session>
wlvision --json session list
```

Success criteria:

- `session close` returns `ok` and the session state is closed.
- `session inspect` reports the closed record, and no container is left running
  for the session.
- `session list` no longer reports the session as an active session; a retained
  failure is released by the operator purge command once its retention deadline
  passes.
- A run started with `--ephemeral` closes its session when the application
  exits, with no further command needed.

Forbidden shortcuts:

- Leaving a session in creating, ready, running, or failed state after the
  work is done.
- Deleting state files or killing the container by hand instead of closing the
  session.
- Using the purge command to abandon a session whose logs have not been read.
