package input

import (
	"context"
	"errors"
	"math"
	"strconv"
	"testing"

	"github.com/bnema/wlvision/internal/agentapi"
	"github.com/bnema/wlvision/internal/result"
)

// call is one operation the fake caller was asked to perform.
type call struct {
	operation string
	params    agentapi.Params
	// ctxErr is the state of the context the call was made with. Cleanup
	// calls must run on a context of their own, so their ctxErr is nil even
	// when the caller's context was cancelled.
	ctxErr error
}

// fakeCaller is a Caller that records what it was asked and answers with a
// fixed snapshot unless respond says otherwise.
type fakeCaller struct {
	state   agentapi.State
	respond func(call) (agentapi.Reply, error)
	calls   []call
}

func (f *fakeCaller) Call(ctx context.Context, operation string, params agentapi.Params) (agentapi.Reply, error) {
	recorded := call{operation: operation, params: params, ctxErr: ctx.Err()}
	f.calls = append(f.calls, recorded)
	if f.respond != nil {
		return f.respond(recorded)
	}
	state := f.state
	return agentapi.Reply{Revision: state.Revision, State: &state}, nil
}

func (f *fakeCaller) operations() []string {
	operations := make([]string, 0, len(f.calls))
	for _, recorded := range f.calls {
		operations = append(operations, recorded.operation)
	}
	return operations
}

func (f *fakeCaller) last() call {
	return f.calls[len(f.calls)-1]
}

const (
	testHandle   = "app-1"
	testRevision = 7

	// Keycodes the embedded US artifact resolves the typing tests to.
	keyShiftL uint32 = 42
	keyH      uint32 = 35
	keyI      uint32 = 23
	keyD1     uint32 = 2
	keyReturn uint32 = 28
)

// demoState is the snapshot every test starts from: one window at (100, 50)
// with an 800x600 content area.
func demoState() agentapi.State {
	return agentapi.State{
		Revision: testRevision,
		Toplevels: []agentapi.Toplevel{{
			Handle:   testHandle,
			Title:    "Demo",
			AppID:    "demo",
			X:        100,
			Y:        50,
			Width:    800,
			Height:   600,
			Revision: 3,
		}},
	}
}

// newFixture returns a service over a fake caller holding one window.
func newFixture() (*Service, *fakeCaller) {
	fake := &fakeCaller{state: demoState()}
	return NewService(fake), fake
}

// failureOf asserts err is a *result.Failure and returns it.
func failureOf(t *testing.T, err error) *result.Failure {
	t.Helper()
	if err == nil {
		t.Fatal("error = nil, want a failure")
	}
	var failure *result.Failure
	if !errors.As(err, &failure) {
		t.Fatalf("error = %v (%T), want a *result.Failure", err, err)
	}
	return failure
}

func TestWindowsReturnsTheSnapshot(t *testing.T) {
	service, fake := newFixture()

	state, err := service.Windows(context.Background())
	if err != nil {
		t.Fatalf("Windows: %v", err)
	}
	if state.Revision != testRevision || len(state.Toplevels) != 1 || state.Toplevels[0].Handle != testHandle {
		t.Errorf("state = %+v, want the fixture snapshot", state)
	}
	if operations := fake.operations(); len(operations) != 1 || operations[0] != agentapi.OpSnapshot {
		t.Errorf("operations = %v, want one snapshot", operations)
	}
	if fake.calls[0].params != (agentapi.Params{}) {
		t.Errorf("snapshot params = %+v, want empty", fake.calls[0].params)
	}
}

func TestActivateSendsTheObservedRevision(t *testing.T) {
	service, fake := newFixture()

	if _, err := service.Activate(context.Background(), Target{Handle: testHandle}); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	want := []string{agentapi.OpSnapshot, agentapi.OpActivate}
	if got := fake.operations(); !equalStrings(got, want) {
		t.Fatalf("operations = %v, want %v", got, want)
	}
	params := fake.calls[1].params
	if params.Handle != testHandle || params.Revision != testRevision {
		t.Errorf("activate params = %+v, want handle %q at revision %d", params, testHandle, testRevision)
	}
}

func TestMoveSendsOutputCoordinatesAndTheObservedRevision(t *testing.T) {
	service, fake := newFixture()

	if _, err := service.Move(context.Background(), MoveRequest{
		Target: Target{Handle: testHandle, Revision: testRevision},
		X:      -5,
		Y:      12.5,
	}); err != nil {
		t.Fatalf("Move: %v", err)
	}
	params := fake.calls[1].params
	if params.Handle != testHandle || params.Revision != testRevision || params.X != -5 || params.Y != 12.5 {
		t.Errorf("move params = %+v, want handle %q at revision %d to (-5, 12.5)", params, testHandle, testRevision)
	}
}

func TestMoveRejectsNonFiniteCoordinates(t *testing.T) {
	cases := []struct {
		name    string
		request MoveRequest
		axis    string
	}{
		{"NaN x", MoveRequest{Target: Target{Handle: testHandle}, X: math.NaN()}, "x"},
		{"infinite y", MoveRequest{Target: Target{Handle: testHandle}, Y: math.Inf(1)}, "y"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			service, fake := newFixture()
			_, err := service.Move(context.Background(), c.request)
			failure := failureOf(t, err)
			if failure.Code != result.CodeUsageError {
				t.Errorf("code = %s, want %s", failure.Code, result.CodeUsageError)
			}
			if failure.Details["axis"] != c.axis {
				t.Errorf("details = %v, want axis %q", failure.Details, c.axis)
			}
			if operations := fake.operations(); len(operations) != 1 {
				t.Errorf("operations = %v, want the snapshot only", operations)
			}
		})
	}
}

func TestResizeReturnsWhatBecameVisible(t *testing.T) {
	service, fake := newFixture()
	fake.respond = func(c call) (agentapi.Reply, error) {
		if c.operation == agentapi.OpResize {
			return agentapi.Reply{Resize: &agentapi.ResizeResult{
				Handle:          testHandle,
				RequestedWidth:  640,
				RequestedHeight: 480,
				VisibleWidth:    640,
				VisibleHeight:   480,
				Revision:        8,
			}}, nil
		}
		state := demoState()
		return agentapi.Reply{Revision: state.Revision, State: &state}, nil
	}

	got, err := service.Resize(context.Background(), ResizeRequest{
		Target: Target{Handle: testHandle},
		Width:  640,
		Height: 480,
	})
	if err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if got.VisibleWidth != 640 || got.VisibleHeight != 480 || got.Revision != 8 {
		t.Errorf("resize result = %+v, want the controller's result", got)
	}
	params := fake.calls[1].params
	if params.Handle != testHandle || params.Revision != testRevision || params.Width != 640 || params.Height != 480 {
		t.Errorf("resize params = %+v, want handle %q at revision %d sized 640x480", params, testHandle, testRevision)
	}
}

func TestCloseWindowSendsTheObservedRevision(t *testing.T) {
	service, fake := newFixture()

	if _, err := service.CloseWindow(context.Background(), Target{Handle: testHandle}); err != nil {
		t.Fatalf("CloseWindow: %v", err)
	}
	params := fake.calls[1].params
	if fake.calls[1].operation != agentapi.OpCloseWindow || params.Handle != testHandle || params.Revision != testRevision {
		t.Errorf("close call = %s %+v, want %s of %q at revision %d",
			fake.calls[1].operation, params, agentapi.OpCloseWindow, testHandle, testRevision)
	}
}

func TestOperationsRefuseAnEmptyHandle(t *testing.T) {
	cases := []struct {
		name string
		run  func(*Service) error
	}{
		{"activate", func(s *Service) error {
			_, err := s.Activate(context.Background(), Target{})
			return err
		}},
		{"move", func(s *Service) error {
			_, err := s.Move(context.Background(), MoveRequest{Target: Target{}})
			return err
		}},
		{"resize", func(s *Service) error {
			_, err := s.Resize(context.Background(), ResizeRequest{Target: Target{}})
			return err
		}},
		{"close", func(s *Service) error {
			_, err := s.CloseWindow(context.Background(), Target{})
			return err
		}},
		{"click", func(s *Service) error {
			_, err := s.Click(context.Background(), ClickRequest{Target: Target{}})
			return err
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			service, fake := newFixture()
			failure := failureOf(t, c.run(service))
			if failure.Code != result.CodeUsageError {
				t.Errorf("code = %s, want %s", failure.Code, result.CodeUsageError)
			}
			if operations := fake.operations(); len(operations) != 0 {
				t.Errorf("operations = %v, want nothing to reach the session", operations)
			}
		})
	}
}

func TestUnknownHandleIsWindowNotFound(t *testing.T) {
	service, fake := newFixture()

	_, err := service.Click(context.Background(), ClickRequest{Target: Target{Handle: "app-9"}, X: 1, Y: 1})
	failure := failureOf(t, err)
	if failure.Code != result.CodeWindowNotFound {
		t.Errorf("code = %s, want %s", failure.Code, result.CodeWindowNotFound)
	}
	if failure.Details["handle"] != "app-9" {
		t.Errorf("details = %v, want handle app-9", failure.Details)
	}
	if operations := fake.operations(); len(operations) != 1 || operations[0] != agentapi.OpSnapshot {
		t.Errorf("operations = %v, want the snapshot only", operations)
	}
}

func TestStaleRevisionFailsBeforeTheMutation(t *testing.T) {
	service, fake := newFixture()

	_, err := service.Move(context.Background(), MoveRequest{Target: Target{Handle: testHandle, Revision: 9}, X: 1})
	failure := failureOf(t, err)
	if failure.Code != result.CodeStaleRevision {
		t.Errorf("code = %s, want %s", failure.Code, result.CodeStaleRevision)
	}
	if failure.Details["requested"] != "9" || failure.Details["current"] != strconv.FormatUint(testRevision, 10) {
		t.Errorf("details = %v, want requested 9 and current %d", failure.Details, testRevision)
	}
	if operations := fake.operations(); len(operations) != 1 || operations[0] != agentapi.OpSnapshot {
		t.Errorf("operations = %v, want the snapshot only", operations)
	}
}

func TestClickConvertsAndPairsTheButton(t *testing.T) {
	service, fake := newFixture()

	got, err := service.Click(context.Background(), ClickRequest{
		Target: Target{Handle: testHandle},
		X:      10,
		Y:      20,
	})
	if err != nil {
		t.Fatalf("Click: %v", err)
	}
	if got.Handle != testHandle || got.Button != 1 || got.OutputX != 110 || got.OutputY != 70 ||
		got.Revision != testRevision || got.State.Revision != testRevision {
		t.Errorf("click result = %+v, want app-1 button 1 at (110, 70) in revision %d", got, testRevision)
	}

	want := []call{
		{operation: agentapi.OpSnapshot},
		{operation: agentapi.OpPointer, params: agentapi.Params{Kind: agentapi.PointerMotion, X: 110, Y: 70}},
		{operation: agentapi.OpPointer, params: agentapi.Params{Kind: agentapi.PointerButton, Button: 1, State: agentapi.StatePressed}},
	}
	if len(fake.calls) != 4 {
		t.Fatalf("calls = %v, want snapshot, motion, press and release", fake.calls)
	}
	for i, expected := range want {
		if fake.calls[i].operation != expected.operation || fake.calls[i].params != expected.params {
			t.Errorf("call %d = %s %+v, want %s %+v", i, fake.calls[i].operation, fake.calls[i].params,
				expected.operation, expected.params)
		}
	}
	release := fake.last()
	if release.operation != agentapi.OpPointer || release.params != (agentapi.Params{
		Kind:   agentapi.PointerButton,
		Button: 1,
		State:  agentapi.StateReleased,
	}) {
		t.Errorf("release = %s %+v, want a button release", release.operation, release.params)
	}
	if release.ctxErr != nil {
		t.Errorf("release ran on a cancelled context: %v", release.ctxErr)
	}
}

func TestClickRejectsPositionsOutsideTheContent(t *testing.T) {
	cases := []struct {
		name string
		x, y float64
		axis string
	}{
		{"negative x", -1, 10, "x"},
		{"negative y", 10, -1, "y"},
		{"x at the width", 800, 10, "x"},
		{"y at the height", 10, 600, "y"},
		{"NaN x", math.NaN(), 10, "x"},
		{"infinite y", 10, math.Inf(1), "y"},
		{"negative infinite x", math.Inf(-1), 10, "x"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			service, fake := newFixture()
			_, err := service.Click(context.Background(), ClickRequest{
				Target: Target{Handle: testHandle},
				X:      c.x,
				Y:      c.y,
			})
			failure := failureOf(t, err)
			if failure.Code != result.CodeUsageError {
				t.Errorf("code = %s, want %s", failure.Code, result.CodeUsageError)
			}
			if failure.Details["axis"] != c.axis {
				t.Errorf("details = %v, want axis %q", failure.Details, c.axis)
			}
			if failure.Details["size"] != "800x600" {
				t.Errorf("details = %v, want the window size", failure.Details)
			}
			if operations := fake.operations(); len(operations) != 1 || operations[0] != agentapi.OpSnapshot {
				t.Errorf("operations = %v, want the snapshot only", operations)
			}
		})
	}
}

func TestClickReleasesTheButtonWhenThePressFails(t *testing.T) {
	service, fake := newFixture()
	pressFailure := result.NewFailure(result.CodeSessionNotReady, agentapi.OpPointer, "no seat yet")
	fake.respond = func(c call) (agentapi.Reply, error) {
		if c.params.Kind == agentapi.PointerButton && c.params.State == agentapi.StatePressed {
			return agentapi.Reply{}, pressFailure
		}
		state := demoState()
		return agentapi.Reply{Revision: state.Revision, State: &state}, nil
	}

	_, err := service.Click(context.Background(), ClickRequest{Target: Target{Handle: testHandle}, X: 1, Y: 1})
	if err != pressFailure {
		t.Fatalf("error = %v, want the controller's own failure unchanged", err)
	}
	release := fake.last()
	if release.operation != agentapi.OpPointer || release.params.State != agentapi.StateReleased || release.params.Button != 1 {
		t.Errorf("last call = %s %+v, want a button release after the failed press", release.operation, release.params)
	}
	if release.ctxErr != nil {
		t.Errorf("release ran on a cancelled context: %v", release.ctxErr)
	}
}

func TestClickReleasesTheButtonWhenTheContextIsCancelled(t *testing.T) {
	service, fake := newFixture()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake.respond = func(c call) (agentapi.Reply, error) {
		if c.params.Kind == agentapi.PointerButton && c.params.State == agentapi.StatePressed {
			cancel()
			return agentapi.Reply{}, ctx.Err()
		}
		state := demoState()
		return agentapi.Reply{Revision: state.Revision, State: &state}, nil
	}

	_, err := service.Click(ctx, ClickRequest{Target: Target{Handle: testHandle}, X: 1, Y: 1})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want the cancellation", err)
	}
	release := fake.last()
	if release.params.State != agentapi.StateReleased {
		t.Errorf("last call = %+v, want a button release after the cancelled press", release.params)
	}
	if release.ctxErr != nil {
		t.Errorf("release ran on the cancelled context: %v", release.ctxErr)
	}
}

func TestPointerSendsOneShape(t *testing.T) {
	cases := []struct {
		name    string
		request PointerRequest
		want    agentapi.Params
	}{
		{"motion", PointerRequest{Kind: agentapi.PointerMotion, X: 3, Y: 4},
			agentapi.Params{Kind: agentapi.PointerMotion, X: 3, Y: 4}},
		{"button", PointerRequest{Kind: agentapi.PointerButton, Button: 1, State: agentapi.StatePressed},
			agentapi.Params{Kind: agentapi.PointerButton, Button: 1, State: agentapi.StatePressed}},
		{"axis", PointerRequest{Kind: agentapi.PointerAxis, Axis: AxisHorizontal, Value: -1},
			agentapi.Params{Kind: agentapi.PointerAxis, Axis: AxisHorizontal, Value: -1}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			service, fake := newFixture()
			if err := service.Pointer(context.Background(), c.request); err != nil {
				t.Fatalf("Pointer: %v", err)
			}
			if len(fake.calls) != 1 || fake.calls[0].operation != agentapi.OpPointer || fake.calls[0].params != c.want {
				t.Errorf("calls = %v, want one pointer %+v", fake.calls, c.want)
			}
		})
	}
}

func TestPointerRejectsMixedOrIncompleteShapes(t *testing.T) {
	cases := []struct {
		name    string
		request PointerRequest
	}{
		{"unknown kind", PointerRequest{Kind: "wheel", X: 1}},
		{"no kind", PointerRequest{X: 1}},
		{"motion with a button", PointerRequest{Kind: agentapi.PointerMotion, Button: 1}},
		{"motion with a state", PointerRequest{Kind: agentapi.PointerMotion, State: agentapi.StatePressed}},
		{"motion with NaN", PointerRequest{Kind: agentapi.PointerMotion, X: math.NaN()}},
		{"button without a button", PointerRequest{Kind: agentapi.PointerButton, State: agentapi.StatePressed}},
		{"button with an unknown state", PointerRequest{Kind: agentapi.PointerButton, Button: 1, State: "down"}},
		{"button with an axis", PointerRequest{Kind: agentapi.PointerButton, Button: 1, State: agentapi.StatePressed, Axis: 1}},
		{"axis out of range", PointerRequest{Kind: agentapi.PointerAxis, Axis: 2, Value: 1}},
		{"axis with a button", PointerRequest{Kind: agentapi.PointerAxis, Button: 1}},
		{"axis with an infinite value", PointerRequest{Kind: agentapi.PointerAxis, Value: math.Inf(1)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			service, fake := newFixture()
			failure := failureOf(t, service.Pointer(context.Background(), c.request))
			if failure.Code != result.CodeUsageError {
				t.Errorf("code = %s, want %s", failure.Code, result.CodeUsageError)
			}
			if operations := fake.operations(); len(operations) != 0 {
				t.Errorf("operations = %v, want nothing to reach the session", operations)
			}
		})
	}
}

func TestScrollMapsAxisAndValue(t *testing.T) {
	service, fake := newFixture()

	if err := service.Scroll(context.Background(), ScrollRequest{Axis: AxisHorizontal, Value: -2.5}); err != nil {
		t.Fatalf("Scroll: %v", err)
	}
	want := agentapi.Params{Kind: agentapi.PointerAxis, Axis: AxisHorizontal, Value: -2.5}
	if len(fake.calls) != 1 || fake.calls[0].params != want {
		t.Errorf("calls = %v, want one axis pointer %+v", fake.calls, want)
	}
}

func TestScrollRejectsAnUnknownAxis(t *testing.T) {
	for _, axis := range []uint32{2, 9} {
		service, fake := newFixture()
		failure := failureOf(t, service.Scroll(context.Background(), ScrollRequest{Axis: axis, Value: 1}))
		if failure.Code != result.CodeUsageError {
			t.Errorf("axis %d: code = %s, want %s", axis, failure.Code, result.CodeUsageError)
		}
		if failure.Details["axis"] != strconv.FormatUint(uint64(axis), 10) {
			t.Errorf("axis %d: details = %v, want the axis named", axis, failure.Details)
		}
		if len(fake.calls) != 0 {
			t.Errorf("axis %d: operations = %v, want nothing", axis, fake.operations())
		}
	}
}

func TestKeyResolvesKeycodesAndNames(t *testing.T) {
	cases := []struct {
		name    string
		request KeyRequest
		want    agentapi.Params
	}{
		{"keycode", KeyRequest{Keycode: 30, State: agentapi.StatePressed},
			agentapi.Params{Key: 30, State: agentapi.StatePressed}},
		{"name", KeyRequest{Name: "Return", State: agentapi.StateReleased},
			agentapi.Params{Key: keyReturn, State: agentapi.StateReleased}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			service, fake := newFixture()
			if err := service.Key(context.Background(), c.request); err != nil {
				t.Fatalf("Key: %v", err)
			}
			if len(fake.calls) != 1 || fake.calls[0].operation != agentapi.OpKey || fake.calls[0].params != c.want {
				t.Errorf("calls = %v, want one key %+v", fake.calls, c.want)
			}
		})
	}
}

func TestKeyRejectsAmbiguousIncompleteOrUnknownKeys(t *testing.T) {
	cases := []struct {
		name    string
		request KeyRequest
		details map[string]string
	}{
		{"keycode and name", KeyRequest{Keycode: 30, Name: "Return", State: agentapi.StatePressed}, nil},
		{"neither", KeyRequest{State: agentapi.StatePressed}, nil},
		{"unknown name", KeyRequest{Name: "return", State: agentapi.StatePressed}, map[string]string{"name": "return"}},
		{"unknown state", KeyRequest{Keycode: 30, State: "down"}, map[string]string{"state": "down"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			service, fake := newFixture()
			failure := failureOf(t, service.Key(context.Background(), c.request))
			if failure.Code != result.CodeUsageError {
				t.Errorf("code = %s, want %s", failure.Code, result.CodeUsageError)
			}
			for key, value := range c.details {
				if failure.Details[key] != value {
					t.Errorf("details = %v, want %s=%q", failure.Details, key, value)
				}
			}
			if operations := fake.operations(); len(operations) != 0 {
				t.Errorf("operations = %v, want nothing to reach the session", operations)
			}
		})
	}
}

func TestTapPressesAndReleasesOneKey(t *testing.T) {
	cases := []struct {
		name    string
		request KeyRequest
		keycode uint32
	}{
		{"name", KeyRequest{Name: "Return"}, keyReturn},
		{"keycode", KeyRequest{Keycode: 30}, 30},
		{"state is not part of a tap", KeyRequest{Name: "Return", State: agentapi.StatePressed}, keyReturn},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			service, fake := newFixture()
			if err := service.Tap(context.Background(), c.request); err != nil {
				t.Fatalf("Tap: %v", err)
			}

			want := []keyStep{{c.keycode, agentapi.StatePressed}, {c.keycode, agentapi.StateReleased}}
			if len(fake.calls) != len(want) {
				t.Fatalf("calls = %v, want one press and one release", fake.calls)
			}
			for i, expected := range want {
				got := fake.calls[i]
				if got.operation != agentapi.OpKey || got.params.Key != expected.keycode || got.params.State != expected.state {
					t.Errorf("call %d = %s %+v, want key %d %s", i, got.operation, got.params, expected.keycode, expected.state)
				}
			}
			if release := fake.last(); release.ctxErr != nil {
				t.Errorf("release ran on a cancelled context: %v", release.ctxErr)
			}
		})
	}
}

func TestTapRejectsAnUnresolvableKey(t *testing.T) {
	cases := []struct {
		name    string
		request KeyRequest
	}{
		{"neither", KeyRequest{}},
		{"keycode and name", KeyRequest{Keycode: 30, Name: "Return"}},
		{"unknown name", KeyRequest{Name: "return"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			service, fake := newFixture()
			failure := failureOf(t, service.Tap(context.Background(), c.request))
			if failure.Code != result.CodeUsageError {
				t.Errorf("code = %s, want %s", failure.Code, result.CodeUsageError)
			}
			if operations := fake.operations(); len(operations) != 0 {
				t.Errorf("operations = %v, want nothing to reach the session", operations)
			}
		})
	}
}

func TestTapStillReleasesWhenThePressFails(t *testing.T) {
	service, fake := newFixture()
	pressFailure := result.NewFailure(result.CodeSessionNotReady, agentapi.OpKey, "seat maybe unreached")
	fake.respond = func(c call) (agentapi.Reply, error) {
		if c.params.State == agentapi.StatePressed {
			return agentapi.Reply{}, pressFailure
		}
		return agentapi.Reply{}, nil
	}

	err := service.Tap(context.Background(), KeyRequest{Keycode: 30})
	if err != pressFailure {
		t.Fatalf("error = %v, want the original press failure unchanged", err)
	}
	if len(fake.calls) != 2 {
		t.Fatalf("calls = %v, want a press and a release", fake.calls)
	}
	release := fake.last()
	if release.operation != agentapi.OpKey || release.params.Key != 30 || release.params.State != agentapi.StateReleased {
		t.Errorf("last call = %s %+v, want a key release even after the failed press", release.operation, release.params)
	}
	if release.ctxErr != nil {
		t.Errorf("release ran on a cancelled context: %v", release.ctxErr)
	}
}

func TestTapReleasesOnAnIndependentContextAfterTheCallerIsCancelled(t *testing.T) {
	service, fake := newFixture()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake.respond = func(c call) (agentapi.Reply, error) {
		if c.params.State == agentapi.StatePressed {
			cancel()
		}
		return agentapi.Reply{}, nil
	}

	if err := service.Tap(ctx, KeyRequest{Keycode: 30}); err != nil {
		t.Fatalf("Tap: %v", err)
	}
	release := fake.last()
	if release.params.State != agentapi.StateReleased {
		t.Errorf("last call = %+v, want a release after the cancelled press", release.params)
	}
	if release.ctxErr != nil {
		t.Errorf("release ran on the cancelled context: %v", release.ctxErr)
	}
}

func TestTapReportsAFailedRelease(t *testing.T) {
	service, fake := newFixture()
	releaseFailure := result.NewFailure(result.CodeSessionNotReady, agentapi.OpKey, "seat gone")
	fake.respond = func(c call) (agentapi.Reply, error) {
		if c.params.State == agentapi.StateReleased {
			return agentapi.Reply{}, releaseFailure
		}
		return agentapi.Reply{}, nil
	}

	err := service.Tap(context.Background(), KeyRequest{Keycode: 30})
	if err != releaseFailure {
		t.Fatalf("error = %v, want the release failure", err)
	}
	if got := fake.calls[len(fake.calls)-1].ctxErr; got != nil {
		t.Errorf("release ran on a cancelled context: %v", got)
	}
}

// keyStep is one key transition a test expects.
type keyStep struct {
	keycode uint32
	state   string
}

func TestKeyRejectsAnOutOfRangeKeycode(t *testing.T) {
	for _, keycode := range []uint32{256, 1 << 20} {
		t.Run(strconv.FormatUint(uint64(keycode), 10), func(t *testing.T) {
			service, fake := newFixture()
			failure := failureOf(t, service.Key(context.Background(), KeyRequest{
				Keycode: keycode,
				State:   agentapi.StatePressed,
			}))
			if failure.Code != result.CodeUsageError {
				t.Errorf("code = %s, want %s", failure.Code, result.CodeUsageError)
			}
			if failure.Details["keycode"] != strconv.FormatUint(uint64(keycode), 10) {
				t.Errorf("details = %v, want the keycode named", failure.Details)
			}
			if operations := fake.operations(); len(operations) != 0 {
				t.Errorf("operations = %v, want nothing to reach the session", operations)
			}
		})
	}

	service, fake := newFixture()
	failure := failureOf(t, service.Tap(context.Background(), KeyRequest{Keycode: 256}))
	if failure.Code != result.CodeUsageError {
		t.Errorf("Tap code = %s, want %s", failure.Code, result.CodeUsageError)
	}
	if operations := fake.operations(); len(operations) != 0 {
		t.Errorf("Tap operations = %v, want nothing to reach the session", operations)
	}
}

func TestResizeRefusesAZeroSize(t *testing.T) {
	cases := []struct {
		name          string
		width, height uint32
	}{
		{"zero width", 0, 480},
		{"zero height", 640, 0},
		{"both zero", 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			service, fake := newFixture()
			_, err := service.Resize(context.Background(), ResizeRequest{
				Target: Target{Handle: testHandle},
				Width:  c.width,
				Height: c.height,
			})
			failure := failureOf(t, err)
			if failure.Code != result.CodeUsageError {
				t.Errorf("code = %s, want %s", failure.Code, result.CodeUsageError)
			}
			if operations := fake.operations(); len(operations) != 0 {
				t.Errorf("operations = %v, want nothing to reach the session", operations)
			}
		})
	}
}

func TestTypePressesShiftOnlyForShiftedStrokes(t *testing.T) {
	service, fake := newFixture()

	got, err := service.Type(context.Background(), TypeRequest{Text: "Hi!"})
	if err != nil {
		t.Fatalf("Type: %v", err)
	}
	if got.Text != "Hi!" || got.Strokes != 3 {
		t.Errorf("result = %+v, want three strokes of %q", got, "Hi!")
	}

	want := []keyStep{
		{keyShiftL, agentapi.StatePressed},
		{keyH, agentapi.StatePressed},
		{keyH, agentapi.StateReleased},
		{keyShiftL, agentapi.StateReleased},
		{keyI, agentapi.StatePressed},
		{keyI, agentapi.StateReleased},
		{keyShiftL, agentapi.StatePressed},
		{keyD1, agentapi.StatePressed},
		{keyD1, agentapi.StateReleased},
		{keyShiftL, agentapi.StateReleased},
	}
	if len(fake.calls) != len(want) {
		t.Fatalf("calls = %v, want %v", fake.calls, want)
	}
	for i, expected := range want {
		got := fake.calls[i]
		if got.operation != agentapi.OpKey || got.params.Key != expected.keycode || got.params.State != expected.state {
			t.Errorf("call %d = %s %+v, want key %d %s", i, got.operation, got.params, expected.keycode, expected.state)
		}
	}
}

func TestTypeRejectsTheWholeTextBeforeSendingAnything(t *testing.T) {
	cases := []struct {
		name      string
		text      string
		character string
		offset    string
	}{
		{"only unsupported", "é", strconv.QuoteRune('é'), "0"},
		{"euro sign above the evdev range", "€", strconv.QuoteRune('€'), "0"},
		{"after a supported character", "aé", strconv.QuoteRune('é'), "1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			service, fake := newFixture()
			_, err := service.Type(context.Background(), TypeRequest{Text: c.text})
			failure := failureOf(t, err)
			if failure.Code != result.CodeUsageError {
				t.Errorf("code = %s, want %s", failure.Code, result.CodeUsageError)
			}
			if failure.Details["character"] != c.character || failure.Details["offset"] != c.offset {
				t.Errorf("details = %v, want character %s at offset %s", failure.Details, c.character, c.offset)
			}
			if operations := fake.operations(); len(operations) != 0 {
				t.Errorf("operations = %v, want nothing to reach the session", operations)
			}
		})
	}
}

func TestTypeReleasesWhatItPressedWhenAStrokeFails(t *testing.T) {
	service, fake := newFixture()
	strokeFailure := result.NewFailure(result.CodeSessionNotReady, agentapi.OpKey, "seat gone")
	fake.respond = func(c call) (agentapi.Reply, error) {
		if c.operation == agentapi.OpKey && c.params.Key == keyD1 && c.params.State == agentapi.StatePressed {
			return agentapi.Reply{}, strokeFailure
		}
		return agentapi.Reply{}, nil
	}

	_, err := service.Type(context.Background(), TypeRequest{Text: "Hi!"})
	if err != strokeFailure {
		t.Fatalf("error = %v, want the controller's failure unchanged", err)
	}

	// The failed '!' press is attempted, and the shift held for it is still
	// down: both must be released, on a context of their own.
	tail := fake.calls[len(fake.calls)-2:]
	want := []keyStep{{keyShiftL, agentapi.StateReleased}, {keyD1, agentapi.StateReleased}}
	for i, expected := range want {
		got := tail[i]
		if got.operation != agentapi.OpKey || got.params.Key != expected.keycode || got.params.State != expected.state {
			t.Errorf("cleanup call %d = %s %+v, want key %d %s", i, got.operation, got.params, expected.keycode, expected.state)
		}
		if got.ctxErr != nil {
			t.Errorf("cleanup call %d ran on a cancelled context: %v", i, got.ctxErr)
		}
	}
}

func TestTypeRefusesEmptyText(t *testing.T) {
	service, fake := newFixture()

	failure := failureOf(t, func() error {
		_, err := service.Type(context.Background(), TypeRequest{})
		return err
	}())
	if failure.Code != result.CodeUsageError {
		t.Errorf("code = %s, want %s", failure.Code, result.CodeUsageError)
	}
	if len(fake.calls) != 0 {
		t.Errorf("operations = %v, want nothing to reach the session", fake.operations())
	}
}

func TestControllerFailureTravelsUnchanged(t *testing.T) {
	service, fake := newFixture()
	refusal := result.NewFailure(result.CodeWindowNotFound, "window.activate", "no window %q", "app-1")
	fake.respond = func(c call) (agentapi.Reply, error) {
		if c.operation == agentapi.OpActivate {
			return agentapi.Reply{}, refusal
		}
		state := demoState()
		return agentapi.Reply{Revision: state.Revision, State: &state}, nil
	}

	_, err := service.Activate(context.Background(), Target{Handle: testHandle})
	if err != refusal {
		t.Fatalf("error = %v, want the controller's own failure untouched", err)
	}
	if !errors.Is(err, refusal) {
		t.Errorf("errors.Is(err, refusal) = false, want true")
	}
}

func TestSnapshotWithoutStateIsNotReady(t *testing.T) {
	service, fake := newFixture()
	fake.respond = func(call) (agentapi.Reply, error) {
		return agentapi.Reply{}, nil
	}

	failure := failureOf(t, func() error {
		_, err := service.Windows(context.Background())
		return err
	}())
	if failure.Code != result.CodeSessionNotReady {
		t.Errorf("code = %s, want %s", failure.Code, result.CodeSessionNotReady)
	}
}

// equalStrings reports whether two string slices are equal.
func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
