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

| Repository | Revision | Branch | State |
| --- | --- | --- | --- |
| WLTurbo | `ca248e67adf46bd1a715e64f8570fa9385b40826` | `phase1/operational-health` | verified below |
| LibWL Devices | `49f8c0bbf428f10840b3715af64ace8d86b8355e` | `phase1/operational-health` | verified below, one item outstanding |
| wlvision | `b936aa302848d5707c18b6c99d3bbf06f6199294` | `phase1/dependency-foundation` | module skeleton and scripts only |

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
| One canonical generator | `scanner` is the only generator; the stub generator `tools/generate.go` and its dead output `output_management/generated.go` are deleted |
| Generated tree reproducible | `go test ./scanner` regenerates `scanner/testdata/protocol_fixture.xml` and requires byte equality with the committed `internal/protocoltest/bindings.go`, twice, from different input paths |
| Generated request/event roundtrip green | `go test ./internal/protocoltest` binds generated objects against the in-process compositor and asserts bind requests, new_id allocation, fixed point, string, array, nil-object, out-of-band descriptor, event decoding including a server-created object, and destructor unregistration |
| Virtual pointer and keyboard smoke green on a pinned headless compositor | `bash test/integration/run.sh` builds a digest-pinned Alpine image (`sha256:48b0309c…`) running sway 1.10.1 on the headless pixman backend and passes all consumer assertions |
| Standalone consumer compiles and runs | `test/consumer` is a separate module; its committed module file has no filesystem replacement, and the harness applies local replacements in a throwaway modfile |

### Defects found and fixed while proving the criteria

Driving real wire traffic instead of a live session exposed six defects that
made the library unusable for its stated purpose:

- `NewLockedPointer`, `NewConfinedPointer`, `NewOutputConfiguration` and
  `NewOutputConfigurationHead` never set a context or allocated an object ID, so
  they sent `new_id 0` and panicked when used.
- `NewOutputManager` started its dispatch goroutine before the initial
  roundtrip, deadlocking the constructor against its own receive lock.
- A `finished` handler mutated the head map without the lock, racing `GetHeads`.

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
- The virtual pointer and keyboard managers own private Wayland connections and
  expose no roundtrip or error accessor, so a compositor-side protocol error on
  those connections is not observable through the public API. Tests can detect
  it only on the connection they own.
- `keyboard_shortcuts_inhibitor` was a stub that reported success without
  connecting; replacing it with a real protocol client is in progress on the
  same branch.
- The repository is not yet fully `gofmt`-clean: formatting drift predates this
  branch and is swept separately so the functional commits stay reviewable.

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

## Release preparation (Phase 2, not yet published)

| Repository | Proposed tag | Commit to tag | Notes |
| --- | --- | --- | --- |
| WLTurbo | `v0.1.1` | `ca248e67adf46bd1a715e64f8570fa9385b40826` | framing, descriptor ownership, lifecycle |
| LibWL Devices | `v0.2.1` | head of `phase1/operational-health` after the outstanding item lands | requires WLTurbo `v0.1.1`; testability, generator, defect fixes |

Publication, signing and push require the Phase 1 review to approve this
document first. Once published, wlvision replaces its local replacements with
the published revisions and re-runs the same gates from a clean checkout.
