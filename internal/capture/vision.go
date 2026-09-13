package capture

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/bnema/wlvision/internal/agentapi"
)

// Temporal defaults and bounds.
//
// The defaults are what a caller gets when it asks for nothing; the bounds are
// what no caller can exceed. They exist so a burst cannot become an unbounded
// recording: an agent asks for a bounded animation sample, not for a session
// that fills a disk.
const (
	// DefaultInterval is the default spacing between scheduled captures.
	DefaultInterval = 100 * time.Millisecond
	// DefaultDuration is the default length of a burst.
	DefaultDuration = 2 * time.Second
	// DefaultMaxFrames is the default sample cap of a burst.
	DefaultMaxFrames = 25
	// MinInterval is the shortest spacing a caller may ask for. Below it, the
	// container exec that carries one capture takes longer than the interval,
	// so every sample would be recorded as missed.
	MinInterval = 16 * time.Millisecond
	// MaxDuration is the longest burst a caller may ask for.
	MaxDuration = 60 * time.Second
	// MaxFrames is the hard sample cap of a burst.
	MaxFrames = 120
	// MaxBurstBytes is the hard byte cap of one burst's stored artifacts.
	MaxBurstBytes int64 = 256 << 20
	// ProbeIntervalCap and ProbeIntervalFloor bound the spacing of stability
	// probes: a probe is scheduled at min(cap, stableFor/2) but never closer
	// than the floor.
	ProbeIntervalCap   = 100 * time.Millisecond
	ProbeIntervalFloor = 16 * time.Millisecond
)

// Clock is the monotonic time source the temporal services schedule against.
//
// Scheduling uses absolute deadlines computed from Now, so a slow capture does
// not shift the ones that follow it. A test injects a fake clock and releases
// its timers by hand, which is what makes cadence and stability deterministic.
type Clock interface {
	// Now reports the current time.
	Now() time.Time
	// After reports when a deadline has passed. A fake clock releases the
	// channel when the test advances time past it.
	After(d time.Duration) <-chan time.Time
}

// RealClock is the process clock.
type RealClock struct{}

// Now implements Clock.
func (RealClock) Now() time.Time { return time.Now() }

// After implements Clock.
func (RealClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

var _ Clock = RealClock{}

// Captured is one compositor capture: what the session stored and the revision
// it was taken at.
type Captured struct {
	Frame    agentapi.FrameResult
	Revision uint64
}

// Capturer performs one capture through the session's resident controller.
//
// One call is one capture: the controller owns the single capture source and
// serializes requests, so a caller must not have two of these in flight.
type Capturer interface {
	Capture(ctx context.Context) (Captured, error)
}

// Fetcher reads a stored capture's bytes back out of the session. The name is
// relative to the session's export directory.
type Fetcher interface {
	Fetch(ctx context.Context, name string) ([]byte, error)
}

// Destination stores one artifact on the host and reports where it went. The
// name is relative to the destination's own root; the implementation owns the
// rules that keep it there.
type Destination interface {
	Store(name string, data []byte) (string, error)
}

// Observer reads the session state the waits that are not about pixels need.
type Observer interface {
	// Snapshot reports the session's windows at one revision.
	Snapshot(ctx context.Context) (agentapi.State, error)
	// Exited reports whether the session recorded an application exit at or
	// after since, and its code.
	Exited(ctx context.Context, since time.Time) (bool, int, error)
}

// Status is what happened to one scheduled sample.
type Status string

// Sample statuses.
const (
	// StatusCaptured means the sample was captured and stored.
	StatusCaptured Status = "captured"
	// StatusDeduplicated means the sample was captured, its digest equal to
	// the previous sample's, so no second copy was stored.
	StatusDeduplicated Status = "deduplicated"
	// StatusMissed means the deadline arrived while a previous capture was
	// still in flight, so the sample was recorded and skipped.
	StatusMissed Status = "missed"
	// StatusFailed means the capture or its storage failed.
	StatusFailed Status = "failed"
)

// Sample is the manifest entry of one scheduled capture. The times are
// offsets from the start of the burst, so a manifest reads as a cadence.
type Sample struct {
	// Scheduled is the deadline the sample was planned for.
	Scheduled time.Duration
	// Requested is when the capture was actually asked for. It differs from
	// Scheduled when the scheduler was late.
	Requested time.Duration
	// Completed is when the capture's bytes were stored.
	Completed time.Duration
	// RequestID is the controller's capture request identifier.
	RequestID uint64
	// FrameSeq is the compositor frame sequence the capture completed at.
	FrameSeq uint64
	// Digest is the digest of the stored frame.
	Digest [32]byte
	// Status is what happened to this sample.
	Status Status
	// Path is where the sample was stored on the host, when it was stored.
	Path string
	// Revision is the session revision the capture was taken at.
	Revision uint64
	// Error describes a failed sample.
	Error string
}

// ProbeInterval reports the spacing of stability probes for a requested
// stability duration: min(100ms, stableFor/2), never below 16ms.
func ProbeInterval(stableFor time.Duration) time.Duration {
	interval := stableFor / 2
	if interval > ProbeIntervalCap {
		interval = ProbeIntervalCap
	}
	if interval < ProbeIntervalFloor {
		interval = ProbeIntervalFloor
	}
	return interval
}

// ParseDigest reads the digest form the controller reports: "sha256:" followed
// by the lowercase hexadecimal digest of the stored bytes.
func ParseDigest(text string) ([32]byte, error) {
	var digest [32]byte

	encoded, ok := strings.CutPrefix(strings.TrimSpace(text), "sha256:")
	if !ok {
		return digest, fmt.Errorf("capture: digest %q is not a sha256 digest", text)
	}
	decoded, err := hex.DecodeString(encoded)
	if err != nil {
		return digest, fmt.Errorf("capture: digest %q is not hexadecimal: %w", text, err)
	}
	if len(decoded) != len(digest) {
		return digest, fmt.Errorf("capture: digest %q is %d bytes, want %d", text, len(decoded), len(digest))
	}
	copy(digest[:], decoded)
	return digest, nil
}

// DigestString renders a digest in the form the controller reports.
func DigestString(digest [32]byte) string {
	return "sha256:" + hex.EncodeToString(digest[:])
}

// digestOf returns the digest of stored bytes, which is what a caller compares
// to decide that two samples are the same picture.
func digestOf(payload []byte) [32]byte { return sha256.Sum256(payload) }
