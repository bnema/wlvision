package capture

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/bnema/wlvision/internal/agentapi"
	"github.com/bnema/wlvision/internal/result"
)

// waitPending blocks until the operation under test is waiting on the clock.
func waitPending(t *testing.T, clock *fakeClock) {
	t.Helper()

	real := time.Now().Add(2 * time.Second)
	for clock.pending() == 0 {
		if time.Now().After(real) {
			t.Fatal("the operation never waited on the clock")
		}
		time.Sleep(50 * time.Microsecond)
	}
}

func waitDeps(clock *fakeClock, src *fakeSource) WaitDeps {
	return WaitDeps{Clock: clock, Capturer: src}
}

func TestWaitStableSucceedsAfterTwoEqualCapturesSpanningTheDuration(t *testing.T) {
	clock := newFakeClock()
	src := newFakeSource()
	src.defaultPayload = []byte("still")
	stableFor := 100 * time.Millisecond

	out, err := spin(t, clock, ProbeInterval(stableFor), func() (WaitResult, error) {
		return WaitStable(context.Background(), stableFor, time.Second, waitDeps(clock, src))
	})
	if err != nil {
		t.Fatalf("WaitStable returned %v", err)
	}

	if out.Kind != KindStable {
		t.Errorf("kind %q, want %q", out.Kind, KindStable)
	}
	if out.Probes != 3 {
		t.Errorf("made %d probes, want 3 (0ms, 50ms, 100ms)", out.Probes)
	}
	if out.Observations != 3 {
		t.Errorf("counted %d observations, want the 3 identical captures of the run", out.Observations)
	}
	if out.Duration != stableFor {
		t.Errorf("stabilised after %s, want %s", out.Duration, stableFor)
	}
	if out.Last == nil {
		t.Fatal("the wait reported no last capture")
	}
	if want := digestOf([]byte("still")); out.Last.Digest != want {
		t.Errorf("last digest %s, want the still frame's %s", DigestString(out.Last.Digest), DigestString(want))
	}
	if out.Last.Completed != stableFor {
		t.Errorf("last capture completed at %s, want %s", out.Last.Completed, stableFor)
	}
}

func TestWaitStableRestartsWhenThePictureChanges(t *testing.T) {
	clock := newFakeClock()
	src := newFakeSource()
	src.steps = []captureStep{{payload: []byte("first")}}
	src.defaultPayload = []byte("second")
	stableFor := 100 * time.Millisecond

	out, err := spin(t, clock, ProbeInterval(stableFor), func() (WaitResult, error) {
		return WaitStable(context.Background(), stableFor, time.Second, waitDeps(clock, src))
	})
	if err != nil {
		t.Fatalf("WaitStable returned %v", err)
	}

	// The change at 50ms restarts the window, so stability needs 150ms.
	if out.Duration != 150*time.Millisecond {
		t.Errorf("stabilised after %s, want 150ms", out.Duration)
	}
	if out.Probes != 4 {
		t.Errorf("made %d probes, want 4", out.Probes)
	}
	if out.Observations != 3 {
		t.Errorf("counted %d observations, want 3: the run restarted at the change, so the pre-change capture is not counted", out.Observations)
	}
}

func TestWaitStableRestartsWhenTheRevisionChanges(t *testing.T) {
	clock := newFakeClock()
	src := newFakeSource()
	src.defaultPayload = []byte("still")
	src.steps = []captureStep{{revision: 1}}
	stableFor := 100 * time.Millisecond

	out, err := spin(t, clock, ProbeInterval(stableFor), func() (WaitResult, error) {
		return WaitStable(context.Background(), stableFor, time.Second, waitDeps(clock, src))
	})
	if err != nil {
		t.Fatalf("WaitStable returned %v", err)
	}

	if out.Duration != 150*time.Millisecond {
		t.Errorf("stabilised after %s, want 150ms after the revision changed", out.Duration)
	}
	if out.Observations != 3 {
		t.Errorf("counted %d observations, want 3: the run restarted at the revision change", out.Observations)
	}
}

func TestWaitStableRestartsWhenTheDimensionsChange(t *testing.T) {
	clock := newFakeClock()
	src := newFakeSource()
	src.defaultPayload = []byte("still")
	src.steps = []captureStep{{width: 4, height: 4}}
	stableFor := 100 * time.Millisecond

	out, err := spin(t, clock, ProbeInterval(stableFor), func() (WaitResult, error) {
		return WaitStable(context.Background(), stableFor, time.Second, waitDeps(clock, src))
	})
	if err != nil {
		t.Fatalf("WaitStable returned %v", err)
	}

	if out.Duration != 150*time.Millisecond {
		t.Errorf("stabilised after %s, want 150ms after the frame size changed", out.Duration)
	}
	if out.Observations != 3 {
		t.Errorf("counted %d observations, want 3: the run restarted at the size change", out.Observations)
	}
}

func TestWaitStableUsesTheProbeFloorForShortDurations(t *testing.T) {
	if got := ProbeInterval(10 * time.Millisecond); got != ProbeIntervalFloor {
		t.Fatalf("ProbeInterval(10ms) = %s, want the %s floor", got, ProbeIntervalFloor)
	}

	clock := newFakeClock()
	src := newFakeSource()
	src.defaultPayload = []byte("still")
	stableFor := 10 * time.Millisecond

	out, err := spin(t, clock, ProbeInterval(stableFor), func() (WaitResult, error) {
		return WaitStable(context.Background(), stableFor, time.Second, waitDeps(clock, src))
	})
	if err != nil {
		t.Fatalf("WaitStable returned %v", err)
	}

	if out.Duration != ProbeIntervalFloor {
		t.Errorf("stabilised after %s, want the %s probe floor", out.Duration, ProbeIntervalFloor)
	}
	if out.Probes != 2 {
		t.Errorf("made %d probes, want 2", out.Probes)
	}
	if out.Observations != 2 {
		t.Errorf("counted %d observations, want the 2 identical captures of the run", out.Observations)
	}
}

func TestWaitStableRestartsAfterAFailedProbe(t *testing.T) {
	clock := newFakeClock()
	src := newFakeSource()
	src.defaultPayload = []byte("still")
	src.steps = []captureStep{{}, {err: errors.New("compositor refused")}}
	stableFor := 100 * time.Millisecond

	out, err := spin(t, clock, ProbeInterval(stableFor), func() (WaitResult, error) {
		return WaitStable(context.Background(), stableFor, time.Second, waitDeps(clock, src))
	})
	if err != nil {
		t.Fatalf("WaitStable returned %v", err)
	}

	// The failed probe at 50ms proves nothing, so the window starts again
	// there and stability is only reached at 200ms.
	if out.Duration != 200*time.Millisecond {
		t.Errorf("stabilised after %s, want 200ms after the failed probe", out.Duration)
	}
	if out.Probes != 5 {
		t.Errorf("made %d probes, want 5 including the failed one", out.Probes)
	}
	if out.Observations != 3 {
		t.Errorf("counted %d observations, want 3: the failed probe restarted the run", out.Observations)
	}
}

func TestWaitStableRestartsAfterAMissedProbe(t *testing.T) {
	clock := newFakeClock()
	src := newFakeSource()
	release := make(chan struct{})
	src.defaultPayload = []byte("still")
	src.steps = []captureStep{{}, {block: release}}
	stableFor := 2 * time.Second // probe interval 100ms

	type outcome struct {
		value WaitResult
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		value, err := WaitStable(context.Background(), stableFor, 20*time.Second, waitDeps(clock, src))
		done <- outcome{value: value, err: err}
	}()

	select {
	case <-src.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the first probe never started")
	}
	waitPending(t, clock)
	clock.Advance(ProbeInterval(stableFor)) // the second probe starts and blocks
	select {
	case <-src.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the blocked probe never started")
	}
	clock.Advance(250 * time.Millisecond) // several probe slots pass while it is stuck

	close(release)

	got := drive(t, clock, ProbeInterval(stableFor), done)
	if got.err != nil {
		t.Fatalf("WaitStable returned %v", got.err)
	}
	if got.value.Duration <= stableFor {
		t.Errorf("stabilised after %s, so the wait accepted a span a missed probe interrupted", got.value.Duration)
	}
	if got.value.Probes < 4 {
		t.Errorf("made %d probes, want at least 4", got.value.Probes)
	}
	if got.value.Observations < 2 || got.value.Observations >= got.value.Probes {
		t.Errorf("counted %d observations over %d probes, want the restarted run's count and below the attempts",
			got.value.Observations, got.value.Probes)
	}
}

func TestWaitStableRequiresTheRequestedInstantsToSpanTheDuration(t *testing.T) {
	clock := newFakeClock()
	src := newFakeSource()
	release := make(chan struct{})
	src.defaultPayload = []byte("still")
	src.steps = []captureStep{{}, {block: release}}
	stableFor := 100 * time.Millisecond // probe interval 50ms

	type outcome struct {
		value WaitResult
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		value, err := WaitStable(context.Background(), stableFor, 5*time.Second, waitDeps(clock, src))
		done <- outcome{value: value, err: err}
	}()

	select {
	case <-src.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the first probe never started")
	}
	waitPending(t, clock)
	clock.Advance(ProbeInterval(stableFor)) // the second probe is requested at 50ms
	select {
	case <-src.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the second probe never started")
	}
	// The second probe only answers at 110ms, so its completion is more than
	// stableFor after the first one while the two captures were requested only
	// 50ms apart: that is not the stillness the caller asked for.
	clock.Advance(60 * time.Millisecond)
	close(release)

	got := drive(t, clock, ProbeInterval(stableFor), done)
	if got.err != nil {
		t.Fatalf("WaitStable returned %v", got.err)
	}
	if got.value.Duration < 2*stableFor {
		t.Fatalf("stabilised after %s: two identical captures requested only %s apart were accepted",
			got.value.Duration, ProbeInterval(stableFor))
	}
}

func TestWaitStableRestartsWhenAProbeCouldNotBeIssuedOnTime(t *testing.T) {
	clock := newFakeClock()
	src := newFakeSource()
	release := make(chan struct{})
	src.defaultPayload = []byte("still")
	src.steps = []captureStep{{}, {block: release}}
	stableFor := 100 * time.Millisecond // probe interval 50ms

	type outcome struct {
		value WaitResult
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		value, err := WaitStable(context.Background(), stableFor, 5*time.Second, waitDeps(clock, src))
		done <- outcome{value: value, err: err}
	}()

	select {
	case <-src.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the first probe never started")
	}
	waitPending(t, clock)
	clock.Advance(ProbeInterval(stableFor)) // the second probe is requested at 50ms
	select {
	case <-src.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the second probe never started")
	}
	// The second probe answers 20ms after the third probe's deadline, less
	// than one probe interval late, but still too late for that probe to be
	// issued on time: the run has to start again.
	clock.Advance(70 * time.Millisecond)
	close(release)

	got := drive(t, clock, ProbeInterval(stableFor), done)
	if got.err != nil {
		t.Fatalf("WaitStable returned %v", got.err)
	}
	if got.value.Duration < 2*stableFor {
		t.Fatalf("stabilised after %s, so the run survived a probe that could not be issued at its deadline",
			got.value.Duration)
	}
	if got.value.Observations < 2 {
		t.Errorf("counted %d observations, want the restarted run's count", got.value.Observations)
	}
}

func TestWaitStableTimeoutCarriesTheLastCapture(t *testing.T) {
	clock := newFakeClock()
	src := newFakeSource() // generated frames differ, so stability is never reached
	stableFor := 100 * time.Millisecond

	out, err := spin(t, clock, ProbeInterval(stableFor), func() (WaitResult, error) {
		return WaitStable(context.Background(), stableFor, 250*time.Millisecond, waitDeps(clock, src))
	})

	var failure *result.Failure
	if !errors.As(err, &failure) {
		t.Fatalf("WaitStable returned %v, want a failure", err)
	}
	if failure.Code != result.CodeWaitTimeout {
		t.Errorf("failure code %q, want %q", failure.Code, result.CodeWaitTimeout)
	}
	if out.Last == nil {
		t.Fatal("a timed out wait reported no last capture")
	}
	if out.Probes != 5 {
		t.Errorf("made %d probes, want 5", out.Probes)
	}
	if out.Observations != 1 {
		t.Errorf("counted %d observations, want 1: every frame differed, so no run grew past its first capture", out.Observations)
	}
	if got, want := failure.Details["digest"], DigestString(out.Last.Digest); got != want {
		t.Errorf("failure digest detail %q, want %q", got, want)
	}
	if got, want := failure.Details["frame_sequence"], strconv.FormatUint(out.Last.FrameSeq, 10); got != want {
		t.Errorf("failure frame sequence detail %q, want %q", got, want)
	}
	if got, want := failure.Details["revision"], strconv.FormatUint(out.Revision, 10); got != want {
		t.Errorf("failure revision detail %q, want %q", got, want)
	}
}

func TestWaitStableCancellationReturnsTheContextError(t *testing.T) {
	clock := newFakeClock()
	src := newFakeSource()
	src.steps = []captureStep{{block: make(chan struct{})}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := WaitStable(ctx, 100*time.Millisecond, time.Second, waitDeps(clock, src))
		done <- err
	}()

	select {
	case <-src.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the first probe never started")
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("WaitStable returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitStable did not return after cancellation")
	}
}

func TestWaitNewFrameReturnsTheNewerSequence(t *testing.T) {
	clock := newFakeClock()
	src := newFakeSource()
	src.steps = []captureStep{{seq: 5}, {seq: 6}, {seq: 7}}

	out, err := spin(t, clock, WaitPollInterval, func() (WaitResult, error) {
		return WaitNewFrame(context.Background(), 6, time.Second, waitDeps(clock, src))
	})
	if err != nil {
		t.Fatalf("WaitNewFrame returned %v", err)
	}

	if out.Kind != KindNewFrame {
		t.Errorf("kind %q, want %q", out.Kind, KindNewFrame)
	}
	if out.Probes != 3 {
		t.Errorf("made %d probes, want 3", out.Probes)
	}
	if out.Duration != 2*WaitPollInterval {
		t.Errorf("waited %s, want %s", out.Duration, 2*WaitPollInterval)
	}
	if out.Last == nil || out.Last.FrameSeq != 7 {
		t.Errorf("last frame sequence %v, want 7", out.Last)
	}
}

func TestWaitNewFrameTimeoutCarriesTheLastSequence(t *testing.T) {
	clock := newFakeClock()
	src := newFakeSource()
	src.steps = []captureStep{{seq: 5}, {seq: 6}, {seq: 6}, {seq: 6}}

	out, err := spin(t, clock, WaitPollInterval, func() (WaitResult, error) {
		return WaitNewFrame(context.Background(), 100, 250*time.Millisecond, waitDeps(clock, src))
	})

	var failure *result.Failure
	if !errors.As(err, &failure) {
		t.Fatalf("WaitNewFrame returned %v, want a failure", err)
	}
	if failure.Code != result.CodeWaitTimeout {
		t.Errorf("failure code %q, want %q", failure.Code, result.CodeWaitTimeout)
	}
	if out.Last == nil {
		t.Fatal("a timed out wait reported no last capture")
	}
	if got, want := failure.Details["frame_sequence"], strconv.FormatUint(out.Last.FrameSeq, 10); got != want {
		t.Errorf("failure frame sequence detail %q, want %q", got, want)
	}
}

func TestWaitWindowCountReachesTheTarget(t *testing.T) {
	clock := newFakeClock()
	observer := &fakeObserver{states: []agentapi.State{
		{Revision: 1, Toplevels: []agentapi.Toplevel{{Handle: "a"}}},
		{Revision: 2, Toplevels: []agentapi.Toplevel{{Handle: "a"}, {Handle: "b"}}},
	}}

	out, err := spin(t, clock, WaitPollInterval, func() (WaitResult, error) {
		return WaitWindowCount(context.Background(), 2, time.Second,
			WaitDeps{Clock: clock, Observer: observer})
	})
	if err != nil {
		t.Fatalf("WaitWindowCount returned %v", err)
	}

	if out.Kind != KindWindowCount {
		t.Errorf("kind %q, want %q", out.Kind, KindWindowCount)
	}
	if out.Observations != 2 {
		t.Errorf("observed %d times, want 2", out.Observations)
	}
	if out.Revision != 2 {
		t.Errorf("revision %d, want 2", out.Revision)
	}
	if out.State == nil || len(out.State.Toplevels) != 2 {
		t.Errorf("state %+v, want two windows", out.State)
	}
	if out.Duration != WaitPollInterval {
		t.Errorf("waited %s, want %s", out.Duration, WaitPollInterval)
	}
}

func TestWaitWindowCountTimeoutCarriesTheLastRevision(t *testing.T) {
	clock := newFakeClock()
	observer := &fakeObserver{states: []agentapi.State{
		{Revision: 7, Toplevels: []agentapi.Toplevel{{Handle: "a"}}},
	}}

	out, err := spin(t, clock, WaitPollInterval, func() (WaitResult, error) {
		return WaitWindowCount(context.Background(), 3, 250*time.Millisecond,
			WaitDeps{Clock: clock, Observer: observer})
	})

	var failure *result.Failure
	if !errors.As(err, &failure) {
		t.Fatalf("WaitWindowCount returned %v, want a failure", err)
	}
	if failure.Code != result.CodeWaitTimeout {
		t.Errorf("failure code %q, want %q", failure.Code, result.CodeWaitTimeout)
	}
	if got, want := failure.Details["revision"], "7"; got != want {
		t.Errorf("failure revision detail %q, want %q", got, want)
	}
	if out.State == nil || out.Revision != 7 {
		t.Errorf("the timed out wait did not carry its last observation: %+v", out.State)
	}
	if out.Observations != 3 {
		t.Errorf("observed %d times, want 3", out.Observations)
	}
}

func TestWaitWindowStateMatchesOnlyTheDescribedWindow(t *testing.T) {
	state := uint32(4)
	observer := &fakeObserver{states: []agentapi.State{
		{Revision: 1, Toplevels: []agentapi.Toplevel{{Handle: "win", Width: 50, State: 4}}},
		{Revision: 2, Toplevels: []agentapi.Toplevel{{Handle: "other", Width: 100, State: 4}}},
		{Revision: 3, Toplevels: []agentapi.Toplevel{{Handle: "win", Width: 100, State: 4}}},
	}}
	clock := newFakeClock()

	out, err := spin(t, clock, WaitPollInterval, func() (WaitResult, error) {
		return WaitWindowState(context.Background(),
			WindowState{Handle: "win", Width: 100, State: &state}, time.Second,
			WaitDeps{Clock: clock, Observer: observer})
	})
	if err != nil {
		t.Fatalf("WaitWindowState returned %v", err)
	}

	if out.Observations != 3 {
		t.Errorf("observed %d times, want 3", out.Observations)
	}
	if out.Revision != 3 {
		t.Errorf("revision %d, want 3", out.Revision)
	}
	if out.Kind != KindWindowState {
		t.Errorf("kind %q, want %q", out.Kind, KindWindowState)
	}
}

func TestWaitExitedReturnsWhenTheApplicationExits(t *testing.T) {
	clock := newFakeClock()
	after := 2
	observer := &fakeObserver{exitedAfter: &after, exitCode: 3}

	out, err := spin(t, clock, WaitPollInterval, func() (WaitResult, error) {
		return WaitExited(context.Background(), time.Second, WaitDeps{Clock: clock, Observer: observer})
	})
	if err != nil {
		t.Fatalf("WaitExited returned %v", err)
	}

	if out.Kind != KindExited {
		t.Errorf("kind %q, want %q", out.Kind, KindExited)
	}
	if out.Observations != 2 {
		t.Errorf("observed %d times, want 2", out.Observations)
	}
	if out.Duration != WaitPollInterval {
		t.Errorf("waited %s, want %s", out.Duration, WaitPollInterval)
	}
}

func TestWaitUsageErrors(t *testing.T) {
	clock := newFakeClock()
	src := newFakeSource()
	observer := &fakeObserver{}

	cases := []struct {
		name string
		call func() error
	}{
		{"no timeout", func() error {
			_, err := WaitStable(context.Background(), 100*time.Millisecond, 0, waitDeps(clock, src))
			return err
		}},
		{"no stable duration", func() error {
			_, err := WaitStable(context.Background(), 0, time.Second, waitDeps(clock, src))
			return err
		}},
		{"no clock", func() error {
			_, err := WaitStable(context.Background(), 100*time.Millisecond, time.Second, WaitDeps{Capturer: src})
			return err
		}},
		{"no capturer", func() error {
			_, err := WaitNewFrame(context.Background(), 0, time.Second, WaitDeps{Clock: clock})
			return err
		}},
		{"no observer", func() error {
			_, err := WaitWindowCount(context.Background(), 1, time.Second, WaitDeps{Clock: clock})
			return err
		}},
		{"no observer for exit", func() error {
			_, err := WaitExited(context.Background(), time.Second, WaitDeps{Clock: clock})
			return err
		}},
		{"no observer for state", func() error {
			_, err := WaitWindowState(context.Background(), WindowState{}, time.Second, WaitDeps{Clock: clock})
			return err
		}},
		{"no timeout for observations", func() error {
			_, err := WaitWindowCount(context.Background(), 1, -time.Second, WaitDeps{Clock: clock, Observer: observer})
			return err
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			var failure *result.Failure
			if !errors.As(err, &failure) {
				t.Fatalf("the wait returned %v, want a failure", err)
			}
			if failure.Code != result.CodeUsageError {
				t.Errorf("failure code %q, want %q", failure.Code, result.CodeUsageError)
			}
		})
	}
}
