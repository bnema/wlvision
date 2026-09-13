package capture

import (
	"context"
	"strconv"
	"time"

	"github.com/bnema/wlvision/internal/agentapi"
	"github.com/bnema/wlvision/internal/result"
)

// Wait kinds, reported in WaitResult.Kind.
const (
	// KindStable is a wait that watched the picture stop changing.
	KindStable = "stable"
	// KindWindowCount is a wait for a number of windows.
	KindWindowCount = "window_count"
	// KindExited is a wait for the application to exit.
	KindExited = "exited"
	// KindNewFrame is a wait for a frame newer than a given sequence.
	KindNewFrame = "new_frame"
	// KindWindowState is a wait for a window to reach a described state.
	KindWindowState = "window_state"
)

// WaitPollInterval is the cadence of the waits that poll without a captured
// frame to compare: the window-state waits and the new-frame wait.
const WaitPollInterval = 100 * time.Millisecond

// operationWait names a wait in a failure.
const operationWait = "capture.wait"

// WaitDeps is what a wait runs against. Every wait needs the clock; a wait
// that captures needs the capturer and a wait that reads session state needs
// the observer.
type WaitDeps struct {
	Clock    Clock
	Capturer Capturer
	Observer Observer
}

// WaitResult is what a wait saw, however it ended. On a timeout it is returned
// alongside the failure so a caller can still describe the last thing that was
// observed.
type WaitResult struct {
	// Kind names the wait that produced this result.
	Kind string
	// Duration is how long the wait took, measured on the injected clock.
	Duration time.Duration
	// Probes is how many captures the wait attempted, counting the ones that
	// failed or were missed as well as the ones that succeeded. It stays zero
	// for the waits that read session state instead of capturing.
	Probes int
	// Observations is the evidence the wait counted behind its result, and what
	// it counts follows the wait kind. A stable wait counts the comparable
	// identical captures that formed its current stable run, so a successful
	// one reports at least two and a restart starts the count again; the
	// window-count, window-state and exited waits count how many times they read
	// session state. A new-frame wait leaves it zero, because its result is a
	// frame sequence rather than an observation.
	Observations int
	// Revision is the last session revision the wait saw.
	Revision uint64
	// Last is the last capture the wait made, nil when it made none.
	Last *Sample
	// State is the last window state the wait observed, nil when it observed
	// none.
	State *agentapi.State
}

// WindowState describes the window a WaitWindowState caller wants. A zero
// field means the caller does not care about it; the handle is matched only
// when it is named.
type WindowState struct {
	Handle        string
	Width, Height uint32
	State         *uint32
}

// WaitStable waits for the picture to stop changing for stableFor.
//
// It takes an initial capture and then probes every ProbeInterval(stableFor),
// never with a sleep: the schedule comes from the injected clock. A probe that
// differs from the comparable capture, a changed session revision, different
// frame dimensions or format, a probe that could not be issued at its deadline
// and a failed probe all restart the stability window, because none of them
// proves the picture held still.
//
// Stability is reached only after two comparable identical captures whose
// requested instants are at least stableFor apart: the wait measures the
// stillness the caller asked for, not the delay a slow answer adds to it.
// Because a probe that was not issued at its deadline restarts the run,
// stability can never be proven in less than one capture round trip. That is
// deliberate: a false "stable" is worse than a timeout. The result counts every
// probe it made and the comparable captures of the run that stabilised.
func WaitStable(ctx context.Context, stableFor, timeout time.Duration, deps WaitDeps) (WaitResult, error) {
	if err := checkWaitDeps(deps, true, false); err != nil {
		return WaitResult{}, err
	}
	if err := checkWaitTimeout(timeout); err != nil {
		return WaitResult{}, err
	}
	if stableFor <= 0 {
		return WaitResult{}, result.NewFailure(result.CodeUsageError, operationWait,
			"stable duration must be positive, got %s", stableFor)
	}

	start := deps.Clock.Now()
	deadline := start.Add(timeout)
	interval := ProbeInterval(stableFor)
	out := WaitResult{Kind: KindStable}

	// comparable is the frame a probe must match to count as stable.
	type comparable struct {
		digest        [32]byte
		width, height int
		format        string
		revision      uint64
	}
	var (
		current *comparable
		// since is the requested offset of the run's first comparable capture.
		// Stability is claimed on the instants the captures were asked for,
		// not on when they returned, so a slow answer cannot close a wait
		// whose samples were requested less than stableFor apart.
		since     time.Duration
		identical int
		// lastCompleted is when the previous capture returned, which decides
		// whether this probe could be issued at its deadline.
		lastCompleted time.Time
	)
	reset := func() {
		current = nil
		identical = 0
		out.Observations = 0
	}

	for probe := 0; ; probe++ {
		scheduled := start.Add(time.Duration(probe) * interval)
		if err := waitUntil(ctx, deps.Clock, earlier(scheduled, deadline)); err != nil {
			out.Duration = deps.Clock.Now().Sub(start)
			return out, err
		}
		now := deps.Clock.Now()
		if !now.Before(deadline) {
			return out, timeoutFailure(deps, KindStable, start, &out)
		}

		// A probe is missed when the previous capture was still in flight at
		// this probe's deadline: the picture was not observed then, so this
		// probe cannot count towards a run that claims it held still. The probe
		// is still taken, late, to give the next run a start.
		if !lastCompleted.IsZero() && lastCompleted.After(scheduled) {
			reset()
		}

		requested := now.Sub(start)
		captured, err := deps.Capturer.Capture(ctx)
		out.Probes++
		completed := deps.Clock.Now()
		lastCompleted = completed
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				out.Duration = completed.Sub(start)
				return out, ctxErr
			}
			reset()
			continue
		}

		sample := sampleOf(captured, scheduled.Sub(start), requested, completed.Sub(start), StatusCaptured)
		out.Last = &sample
		out.Revision = captured.Revision

		digest, err := ParseDigest(captured.Frame.Digest)
		if err != nil {
			reset()
			continue
		}
		frame := comparable{
			digest:   digest,
			width:    captured.Frame.Width,
			height:   captured.Frame.Height,
			format:   captured.Frame.Format,
			revision: captured.Revision,
		}
		if current != nil && *current == frame {
			identical++
			out.Observations = identical
			if identical >= 2 && requested-since >= stableFor {
				out.Duration = completed.Sub(start)
				return out, nil
			}
			continue
		}
		current = &frame
		since = requested
		identical = 1
		out.Observations = 1
	}
}

// WaitNewFrame captures until the compositor reports a frame sequence newer
// than after.
func WaitNewFrame(ctx context.Context, after uint64, timeout time.Duration, deps WaitDeps) (WaitResult, error) {
	if err := checkWaitDeps(deps, true, false); err != nil {
		return WaitResult{}, err
	}
	if err := checkWaitTimeout(timeout); err != nil {
		return WaitResult{}, err
	}

	start := deps.Clock.Now()
	deadline := start.Add(timeout)
	out := WaitResult{Kind: KindNewFrame}

	for probe := 0; ; probe++ {
		scheduled := start.Add(time.Duration(probe) * WaitPollInterval)
		if err := waitUntil(ctx, deps.Clock, earlier(scheduled, deadline)); err != nil {
			out.Duration = deps.Clock.Now().Sub(start)
			return out, err
		}
		now := deps.Clock.Now()
		if !now.Before(deadline) {
			return out, timeoutFailure(deps, KindNewFrame, start, &out)
		}

		requested := now.Sub(start)
		captured, err := deps.Capturer.Capture(ctx)
		out.Probes++
		completed := deps.Clock.Now()
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				out.Duration = completed.Sub(start)
				return out, ctxErr
			}
			continue
		}

		sample := sampleOf(captured, requested, requested, completed.Sub(start), StatusCaptured)
		out.Last = &sample
		out.Revision = captured.Revision
		if captured.Frame.Sequence > after {
			out.Duration = completed.Sub(start)
			return out, nil
		}
	}
}

// WaitWindowCount waits until the session reports want windows.
func WaitWindowCount(ctx context.Context, want int, timeout time.Duration, deps WaitDeps) (WaitResult, error) {
	return waitObservations(ctx, KindWindowCount, timeout, deps, func(out *WaitResult) (bool, error) {
		state, err := deps.Observer.Snapshot(ctx)
		if err != nil {
			return false, err
		}
		out.Revision = state.Revision
		out.State = &state
		return len(state.Toplevels) == want, nil
	})
}

// WaitExited waits until the session records that the application exited.
func WaitExited(ctx context.Context, timeout time.Duration, deps WaitDeps) (WaitResult, error) {
	since := deps.Clock.Now()
	return waitObservations(ctx, KindExited, timeout, deps, func(out *WaitResult) (bool, error) {
		exited, _, err := deps.Observer.Exited(ctx, since)
		if err != nil {
			return false, err
		}
		return exited, nil
	})
}

// WaitWindowState waits until a window matches want: the handle when the
// caller named one, and every non-zero field.
func WaitWindowState(ctx context.Context, want WindowState, timeout time.Duration, deps WaitDeps) (WaitResult, error) {
	return waitObservations(ctx, KindWindowState, timeout, deps, func(out *WaitResult) (bool, error) {
		state, err := deps.Observer.Snapshot(ctx)
		if err != nil {
			return false, err
		}
		out.Revision = state.Revision
		out.State = &state
		for _, toplevel := range state.Toplevels {
			if windowMatches(toplevel, want) {
				return true, nil
			}
		}
		return false, nil
	})
}

// waitObservations polls the observer at WaitPollInterval on the injected
// clock until observe reports the condition, the timeout passes or the context
// ends. A failed observation is not fatal: the next poll tries again, and the
// caller decides from the timeout.
func waitObservations(ctx context.Context, kind string, timeout time.Duration, deps WaitDeps,
	observe func(out *WaitResult) (bool, error)) (WaitResult, error) {
	if err := checkWaitDeps(deps, false, true); err != nil {
		return WaitResult{}, err
	}
	if err := checkWaitTimeout(timeout); err != nil {
		return WaitResult{}, err
	}

	start := deps.Clock.Now()
	deadline := start.Add(timeout)
	out := WaitResult{Kind: kind}

	for poll := 0; ; poll++ {
		scheduled := start.Add(time.Duration(poll) * WaitPollInterval)
		if err := waitUntil(ctx, deps.Clock, earlier(scheduled, deadline)); err != nil {
			out.Duration = deps.Clock.Now().Sub(start)
			return out, err
		}
		now := deps.Clock.Now()
		if !now.Before(deadline) {
			return out, timeoutFailure(deps, kind, start, &out)
		}

		out.Observations++
		done, err := observe(&out)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				out.Duration = deps.Clock.Now().Sub(start)
				return out, ctxErr
			}
			continue
		}
		if done {
			out.Duration = deps.Clock.Now().Sub(start)
			return out, nil
		}
	}
}

// checkWaitDeps refuses a wait whose dependencies cannot serve it.
func checkWaitDeps(deps WaitDeps, needCapturer, needObserver bool) error {
	switch {
	case deps.Clock == nil:
		return result.NewFailure(result.CodeUsageError, operationWait, "a clock is required")
	case needCapturer && deps.Capturer == nil:
		return result.NewFailure(result.CodeUsageError, operationWait, "a capturer is required")
	case needObserver && deps.Observer == nil:
		return result.NewFailure(result.CodeUsageError, operationWait, "an observer is required")
	}
	return nil
}

// checkWaitTimeout refuses a wait with no finite positive deadline.
func checkWaitTimeout(timeout time.Duration) error {
	if timeout <= 0 {
		return result.NewFailure(result.CodeUsageError, operationWait,
			"timeout must be positive, got %s", timeout)
	}
	return nil
}

// earlier returns the sooner of two instants, which is how a probe schedule
// meets the wait's timeout.
func earlier(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// sampleOf describes one capture a wait made, as offsets from the wait's
// start.
func sampleOf(captured Captured, scheduled, requested, completed time.Duration, status Status) Sample {
	sample := Sample{
		Scheduled: scheduled,
		Requested: requested,
		Completed: completed,
		RequestID: captured.Frame.CaptureRequestID,
		FrameSeq:  captured.Frame.Sequence,
		Status:    status,
		Revision:  captured.Revision,
	}
	if digest, err := ParseDigest(captured.Frame.Digest); err == nil {
		sample.Digest = digest
	} else {
		sample.Error = err.Error()
	}
	return sample
}

// timeoutFailure builds the CodeWaitTimeout failure a wait returns, carrying
// the last thing it observed so a caller can tell a slow condition from a
// wrong one.
func timeoutFailure(deps WaitDeps, kind string, start time.Time, out *WaitResult) *result.Failure {
	failure := result.NewFailure(result.CodeWaitTimeout, operationWait,
		"%s wait timed out after %s", kind, deps.Clock.Now().Sub(start))
	details := map[string]string{
		"kind":         kind,
		"revision":     strconv.FormatUint(out.Revision, 10),
		"probes":       strconv.Itoa(out.Probes),
		"observations": strconv.Itoa(out.Observations),
	}
	if out.Last != nil {
		details["digest"] = DigestString(out.Last.Digest)
		details["frame_sequence"] = strconv.FormatUint(out.Last.FrameSeq, 10)
		details["request_id"] = strconv.FormatUint(out.Last.RequestID, 10)
	}
	failure.Details = details
	return failure
}

// windowMatches reports whether a window is the one a caller described: the
// handle must be equal when the caller named one, and every non-zero field
// must match.
func windowMatches(window agentapi.Toplevel, want WindowState) bool {
	if want.Handle != "" && window.Handle != want.Handle {
		return false
	}
	if want.Width != 0 && window.Width != want.Width {
		return false
	}
	if want.Height != 0 && window.Height != want.Height {
		return false
	}
	if want.State != nil && window.State != *want.State {
		return false
	}
	return true
}
