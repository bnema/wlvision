# Wayland transport decision

## Decision

WLTurbo (`github.com/bnema/wlturbo`) is the Wayland transport for wlvision's
resident controller. It is accepted on the strength of the socketpair contract
suite below, and remains on trial until the same contract passes against a live
compositor in the Weston vertical slice.

Accepted revision for Phase 1 development:

| Repository | Revision | State |
| --- | --- | --- |
| WLTurbo | `ca248e6` (`fix: give each connection ownership of its received descriptors`), on top of `58d830c` (`fix: make Wayland stream framing deterministic`) | local branch `phase1/operational-health` |
| wlvision | consumed through the local replacement from `scripts/use-local-deps.sh` | not yet pinned to a published tag |

The protocol bindings stay project-owned. Generated request/event code lives in
wlvision and drives a narrow transport facade, so replacing WLTurbo does not
touch protocol definitions or command semantics.

## Gate the decision answers

The transport had to prove that message framing, descriptor ownership and
connection lifecycle are deterministic before any compositor work depended on
it. It failed that gate at the start of Phase 1: `client_test.go` did not
compile against `ProxyID`, framing assumed one read per message, received
descriptors were queued in a package-global ring capped at four and were never
closed, and `Close` was not idempotent.

## Evidence

Commands run in the WLTurbo checkout on the accepted revision, with no
compositor and no display:

```bash
go test ./... -count=1
go test -race ./... -count=1
go test -race ./... -run 'TestDisplay' -count=20
go vet ./...
```

All pass. The suite covers:

- framing: an 8-byte header split across three reads, a body split byte by byte,
  two messages coalesced into one read, and a read boundary inside the second
  message's body;
- validation: size below the header, misaligned size, size above the configured
  maximum, event for an unknown object, unknown `wl_display` opcode;
- errors: `wl_display.error` surfaced as a typed `DisplayError` carrying object,
  code and message, `wl_display.delete_id` removing the object, peer close before
  a header, mid header, mid body and after a complete message;
- lifecycle: `Close` idempotent, `Dispatch` after `Close` returning
  `net.ErrClosed`;
- descriptors: one descriptor per control message, several descriptors in one
  control message, an event without descriptors after one with descriptors, two
  concurrently connected displays staying isolated, control data truncated past
  64 descriptors reported as a protocol failure, unconsumed descriptors closed
  on shutdown and after a malformed frame, and a hundred-connection loop that
  asserts the process descriptor count does not grow.

Descriptor identity is verified by writing unique sentinel bytes through each
received descriptor and reading them from the paired pipe end, not by comparing
descriptor numbers.

## Known limits of the evidence

- Every test drives a private socketpair. No test in this suite talks to a real
  compositor, so `wl_shm` capture, registry binding against a live server and
  object lifetime under real event ordering remain unproven.
- The maximum accepted message size defaults to 1 MiB and is configurable per
  connection; no test yet exercises a compositor that legitimately exceeds it.
- Throughput and allocation behaviour were not measured. WLTurbo's
  zero-allocation claims are not part of this decision.

## Trial exit

The decision becomes final when the Phase 3 vertical slice — native Wayland
fixture plus a real GTK client under the pinned Weston image — passes
enumeration, activation, pointer and keyboard input, configure-to-commit
resize, and `wl_shm` capture on this transport.

If that slice fails and the failure cannot be repaired narrowly in WLTurbo, the
transport is replaced rather than patched around: the alternative is the
smallest maintained Go Wayland client that passes the same socketpair contract
suite, wired behind the existing protocol facade. Replacing the transport is a
bounded change to the connection layer; it must not change the control protocol,
the CLI contract or the generated bindings.
