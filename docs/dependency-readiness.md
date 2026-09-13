# Dependency readiness

Phase 1 of wlvision exists to make its two Go dependencies dependable, because
every later phase builds on their transport and protocol behaviour. This
document is the gate: no compositor work in Phase 3 starts until the criteria
below are met and the named commits are accepted.

## Exit criteria

WLTurbo:

```text
unit + race + vet green; fragmented/coalesced framing proven;
SCM_RIGHTS and FD cleanup proven; two displays isolated; live roundtrip green.
```

LibWL Devices:

```text
unit + race + vet green without a host display; one canonical WLTurbo-backed
generator; generated tree reproducible; generated capture/control roundtrip
green; virtual pointer and keyboard smoke green on a pinned headless
compatible compositor; standalone consumer fixture compiles and runs.
```

## Accepted commits

These are the revisions every measurement below was taken against. They are not
published as tags; wlvision consumes them through the local replacements.

| Repository | Revision | Branch | State |
| --- | --- | --- | --- |
| WLTurbo | `a3a494e518efc605059f17179dea07edb1132a84` (transport `ca248e6`) | `phase1/operational-health` | verified below |
| LibWL Devices | `bc59c334a2a28d4a20e761af240be3a401699413` | `phase1/operational-health` | verified below |
| wlvision | `9d78581090867849793b0c90a9cab3de649938de` | `phase1/dependency-foundation` | module skeleton, scripts and records |

All measurements were taken with `go1.27.1` on Linux/amd64.

## WLTurbo

### Criteria met

| Criterion | Evidence |
| --- | --- |
| Unit tests green | `go test ./... -count=1` passes |
| Race detector green | `go test -race ./... -count=1` passes, and `-run 'TestDisplay' -count=20` |
| Vet green | `go vet ./...` silent |
| Fragmented and coalesced framing proven | `TestDisplayDispatch_{Fragmented,Coalesced,HalfCoalesced}` drive a header split across three reads, a body split byte by byte, two messages in one read, and a read boundary inside the second message's body |
| SCM_RIGHTS proven | `TestDisplayFD_MultipleInOneControlMessage` receives two descriptors in one control message and identifies each by writing a unique sentinel through it and reading the paired pipe end |
| Descriptor cleanup proven | `TestDisplayFD_UnconsumedDescriptorClosedOnShutdown`, `TestDisplayFD_ParseFailureClosesQueuedDescriptor`, `TestDisplayFD_TruncatedControlData`, and a hundred-connection loop asserting `/proc/self/fd` does not grow |
| Two displays isolated | `TestDisplayFD_TwoDisplaysAreIsolated` — the regression that the old package-global descriptor queue could not pass |
| Live roundtrip green | the standalone consumer connects to headless sway through the fixture and completes `display.Roundtrip()` against a real compositor |

Process failure modes are covered too: malformed sizes, misaligned sizes,
oversized messages, unknown objects, unknown `wl_display` opcodes, typed
`wl_display.error`, `delete_id`, idempotent `Close`, and `net.ErrClosed` after
close.

The transport decision record with the limits of this evidence is
`docs/transport-decision.md`.

## LibWL Devices

### Criteria met

| Criterion | Evidence |
| --- | --- |
| Unit and race tests green without a host display | `env -u WAYLAND_DISPLAY -u XDG_RUNTIME_DIR go test -race ./... -count=1` — every package passes with no display and no display-related skips |
| Vet green | `env -u WAYLAND_DISPLAY -u XDG_RUNTIME_DIR go vet ./...` silent |
| Formatting green | `gofmt -l .` reports nothing |
| One canonical generator | `scanner` is the only generator; the stub generator `tools/generate.go` and its dead output `output_management/generated.go` are deleted |
| Generated tree reproducible | `go test ./scanner` regenerates `scanner/testdata/protocol_fixture.xml` and requires byte equality with the committed `internal/protocoltest/bindings.go`, twice, from different input paths |
| Generated request/event roundtrip green | `go test ./internal/protocoltest` binds generated objects against the in-process compositor and asserts bind requests, new_id allocation and argument position, fixed point, string, array, nil-object, out-of-band descriptor, event decoding including a server-created object, and destructor unregistration |
| Virtual pointer and keyboard smoke green on a pinned headless compositor | `bash test/integration/run.sh` builds a digest-pinned Alpine image (`sha256:48b0309c…`) running sway 1.10.1 on the headless pixman backend and passes every consumer assertion |
| Standalone consumer compiles and runs | `test/consumer` is a separate module; its committed module file has no filesystem replacement, and the harness applies local replacements in a throwaway modfile |
| No package reports success without sending a request | `keyboard_shortcuts_inhibitor` binds the global, sends `inhibit_shortcuts`, decodes `active`/`inactive`, surfaces compositor errors and destroys both objects; that path is exercised live against sway |

### Defects found and fixed while proving the criteria

Driving real wire traffic instead of a live session exposed these defects that
made the library unusable for its stated purpose:

- `NewLockedPointer`, `NewConfinedPointer`, `NewOutputConfiguration` and
  `NewOutputConfigurationHead` never set a context or allocated an object ID, so
  they sent `new_id 0` and panicked when used.
- `NewOutputManager` started its dispatch goroutine before the initial
  roundtrip, deadlocking the constructor against its own receive lock.
- A `finished` handler mutated the head map without the lock, racing `GetHeads`.
- The preferred-mode handler ran after the mode was created, so the default mode
  was never selected and `Mode` stayed nil.
- `keyboard_shortcuts_inhibitor` was a stub: it reported `connected: true`,
  accepted `interface{}` arguments and sent nothing at all.
- The generator appended every `new_id` argument last, whatever the protocol
  declared. A request whose `new_id` comes first wrote the object ID where the
  compositor expected another value; a real compositor answers that with
  `invalid arguments` and drops the connection.

Each fix carries a test that fails without it, and the module file that a
release would use is checked: `git grep "=> \.\./" HEAD -- '*.mod'` finds no
replacement in any of the three repositories.

Two more defects surfaced while wlvision started using the libraries:

- Generated objects stored event handlers in plain slices that dispatch read
  while registration wrote them. Any client that registers a handler after the
  object exists while another goroutine dispatches raced, which the race
  detector caught. Generated objects with events now carry a mutex and dispatch
  through a snapshot taken under it.
- Generated enum constants were typed `int32` while the events carrying them
  decode to `uint32`, so callers cast between two spellings of the same wire
  word. Enum values are now untyped.

WLTurbo's `wl` shim also re-exports the transport's error types, so a caller
that imports it can classify a failure without also importing the root package.

### Applied deviations from the plan

- `internal/protocols/*.go` are hand-written protocol facades with listener
  APIs, not generated output. The generator produces the WLTurbo-backed style
  and is proven by the fixture bindings and their roundtrip tests; regenerating
  these facades mechanically would replace their public API for no gain to
  wlvision, which needs the generator for its own control protocol. New
  protocols are generated.
- No `weston-output-capture.xml` fixture is vendored yet: the pinned Weston
  revision is selected in Phase 2, and copying that XML before the lock exists
  would pin nothing.

### Known gaps carried forward

- `client.NewClient` lives in `internal/client`. The consumer fixture can import
  it only because its module path is under the library's module prefix; a
  genuinely independent downstream module cannot. A public connection entry
  point is a follow-up, not a Phase 1 requirement.
- Several packages open a private connection per manager. A compositor rejects a
  request that references an object belonging to another connection: sway
  answers `invalid arguments for …inhibit_shortcuts` and closes the connection.
  `keyboard_shortcuts_inhibitor` therefore gained
  `NewKeyboardShortcutsInhibitorManagerWithClient`, which runs the manager on the
  caller's connection, and the live fixture uses it. `pointer_constraints`
  (`LockPointer(surface, pointer, region)`) has the same shape and is reached
  only through the in-process compositor today. A public connection API is the
  durable fix; until then, callers must not mix objects across connections.
- The virtual pointer and keyboard managers own private connections and expose
  no roundtrip or error accessor, so a compositor-side protocol error on those
  connections is not observable through the public API. Tests can detect it only
  on the connection they own. The live fixture compensates by round-tripping on
  the connection it owns after each injection.
- No `weston-output-capture.xml` fixture is vendored yet: the pinned Weston
  revision is selected in Phase 2, and copying that XML before the lock exists
  would pin nothing.

## Local dependency workflow

The reversible loop was verified byte-identical for both module files:

| Step | Result |
| --- | --- |
| `bash scripts/check-local-deps.sh` on clean modules | exits non-zero, names all three missing replacements |
| `bash scripts/check-release-modules.sh` on clean modules | exits zero |
| `bash scripts/use-local-deps.sh` | exits zero; local check passes, release guard fails |
| `bash scripts/clear-local-deps.sh` | exits zero; `go.mod` and `go.sum` in both modules byte-identical to their pre-loop state; release guard passes |

`scripts/check-local-deps.sh` requires exactly the expected replacements and
rejects any other, using `internal/tools/checkreplace`, without `jq`.

## Release status

Publication is deferred by decision. Development continues on local filesystem
replacements: `scripts/use-local-deps.sh` applies them, they stay uncommitted,
and `scripts/clear-local-deps.sh` reverts them. The committed module files keep
the last published requirements, so the module graph is always release-clean and
the replacement state is explicit in `git status`.

Still to be discovered bugs in either dependency are expected, which is why
tagging now would pin a moving target.

When cutting releases becomes useful, these are the prepared tags and the module
changes that follow them:

| Repository | Proposed tag | Commit to tag | Notes |
| --- | --- | --- | --- |
| WLTurbo | `v0.1.1` | `a3a494e518efc605059f17179dea07edb1132a84` | framing, descriptor ownership, lifecycle, formatting |
| LibWL Devices | `v0.2.1` | `bc59c334a2a28d4a20e761af240be3a401699413` | testability, generator, defect fixes, inhibitor client; must require WLTurbo `v0.1.1` |

```text
libwldevices-go: require github.com/bnema/wlturbo v0.1.1
wlvision:        require github.com/bnema/wlturbo v0.1.1
                 require github.com/bnema/libwldevices-go v0.2.1
```

The tags above name the commits verified in this document; a release cut later
must re-run the same gates from a clean checkout before it is pushed.
