package control

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bnema/wlturbo/wl"
	"github.com/bnema/wlvision/internal/control/generated"
	"github.com/bnema/wlvision/internal/controltest"
)

// Controller request opcodes, in declaration order.
const (
	opSnapshot uint16 = iota
	opActivate
	opMove
	opResize
	opCloseWindow
	opPointerMotion
	opPointerButton
	opPointerAxis
	opKey
	opAuthorizeCapture
	opDestroyController
)

// Controller event opcodes, in declaration order.
const (
	evSnapshot uint16 = iota
	evToplevelChanged
	evToplevelRemoved
	evRequestDone
	evRequestFailed
	evFrame
	evCaptureAuthorized
)

func newServer(t *testing.T) *controltest.Server {
	t.Helper()

	srv := controltest.Start(t)
	srv.Env(t)
	return srv
}

func newClient(t *testing.T, srv *controltest.Server) Client {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := Connect(ctx, "")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	return client
}

// controllerObject is the object ID the compositor gave the controller.
func controllerObject(t *testing.T, srv *controltest.Server) uint32 {
	t.Helper()

	id := srv.ObjectID(generated.WlvisionControllerInterface)
	if id == 0 {
		t.Fatalf("no %s object was created", generated.WlvisionControllerInterface)
	}
	return id
}

// async runs one client call and returns its result channel.
type asyncResult struct {
	state   State
	resize  ResizeResult
	capture uint64
	err     error
}

func runAsync(fn func(ctx context.Context) asyncResult) (<-chan asyncResult, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	results := make(chan asyncResult, 1)
	go func() {
		results <- fn(ctx)
	}()
	return results, cancel
}

func await(t *testing.T, results <-chan asyncResult) asyncResult {
	t.Helper()

	select {
	case result := <-results:
		return result
	case <-time.After(10 * time.Second):
		t.Fatal("client call did not return")
		return asyncResult{}
	}
}

// Connect binds the manager, creates the controller and records both, so the
// facade works against the real wire protocol rather than a mock.
func TestConnectCreatesController(t *testing.T) {
	srv := newServer(t)
	client := newClient(t, srv)

	bind := waitForBind(t, srv, generated.WlvisionControlInterface)
	iface, consumed := bind.String(4)
	if iface != generated.WlvisionControlInterface {
		t.Fatalf("bound interface = %q, want %q", iface, generated.WlvisionControlInterface)
	}
	if got := bind.Uint32(4 + consumed); got != 1 {
		t.Errorf("bound version = %d, want 1", got)
	}
	if got := bind.Uint32(8 + consumed); got != srv.ObjectID(generated.WlvisionControlInterface) {
		t.Errorf("bound object ID = %d, want the manager the client then used", got)
	}

	managerID := srv.ObjectID(generated.WlvisionControlInterface)
	create := srv.WaitForRequest(t, generated.WlvisionControlInterface, 0)
	if create.Object != managerID {
		t.Fatalf("create_controller sent on object %d, want the manager %d", create.Object, managerID)
	}
	if got, want := create.Uint32(0), controllerObject(t, srv); got != want {
		t.Fatalf("create_controller new_id = %d, want %d", got, want)
	}

	if got := client.Revision(); got != 0 {
		t.Fatalf("initial revision = %d, want 0", got)
	}
}

func TestSnapshotCollectsToplevels(t *testing.T) {
	srv := newServer(t)
	client := newClient(t, srv)
	controller := controllerObject(t, srv)

	results, cancel := runAsync(func(ctx context.Context) asyncResult {
		state, err := client.Snapshot(ctx)
		return asyncResult{state: state, err: err}
	})
	defer cancel()

	request := srv.WaitForRequest(t, generated.WlvisionControllerInterface, opSnapshot)
	requestID := request.Uint32(0)

	if err := srv.SendEvent(controller, evSnapshot, uint32(0), uint32(7)); err != nil {
		t.Fatalf("SendEvent(snapshot): %v", err)
	}
	if err := srv.SendEvent(controller, evToplevelChanged,
		"app-1", "Example", "org.example.App",
		int32(-5), int32(10), uint32(300), uint32(200), uint32(0), uint32(0), uint32(7)); err != nil {
		t.Fatalf("SendEvent(toplevel_changed): %v", err)
	}
	if err := srv.SendEvent(controller, evRequestDone, requestID, uint32(0), uint32(7)); err != nil {
		t.Fatalf("SendEvent(request_done): %v", err)
	}

	result := await(t, results)
	if result.err != nil {
		t.Fatalf("Snapshot: %v", result.err)
	}
	if result.state.Revision != 7 {
		t.Errorf("revision = %d, want 7", result.state.Revision)
	}
	if len(result.state.Toplevels) != 1 {
		t.Fatalf("toplevels = %+v, want exactly one", result.state.Toplevels)
	}

	toplevel := result.state.Toplevels[0]
	if toplevel.Handle != "app-1" || toplevel.Title != "Example" || toplevel.AppID != "org.example.App" {
		t.Errorf("toplevel metadata = %+v", toplevel)
	}
	if toplevel.X != -5 || toplevel.Y != 10 {
		t.Errorf("toplevel position = (%d,%d), want (-5,10)", toplevel.X, toplevel.Y)
	}
	if toplevel.Size != (Size{Width: 300, Height: 200}) {
		t.Errorf("toplevel size = %s, want 300x200", toplevel.Size)
	}

	// The state the facade caches afterwards must agree with the answer.
	if got := client.State().Revision; got != 7 {
		t.Errorf("cached revision = %d, want 7", got)
	}
	if _, ok := client.State().Find("app-1"); !ok {
		t.Error("cached state lost the window the snapshot reported")
	}
}

// Window operations carry the revision the caller saw and an opaque handle, in
// the order the protocol declares.
func TestActivateEncodesHandlesAndRevisionWords(t *testing.T) {
	srv := newServer(t)
	client := newClient(t, srv)

	const revision = Revision(1<<32 | 9)

	results, cancel := runAsync(func(ctx context.Context) asyncResult {
		state, err := client.Activate(ctx, "app-7", revision)
		return asyncResult{state: state, err: err}
	})
	defer cancel()

	request := srv.WaitForRequest(t, generated.WlvisionControllerInterface, opActivate)
	requestID := request.Uint32(0)
	handle, consumed := request.String(4)
	if handle != "app-7" {
		t.Fatalf("handle = %q, want %q", handle, "app-7")
	}
	if got := request.Uint32(4 + consumed); got != 1 {
		t.Errorf("revision high word = %d, want 1", got)
	}
	if got := request.Uint32(8 + consumed); got != 9 {
		t.Errorf("revision low word = %d, want 9", got)
	}

	if err := srv.SendEvent(controllerObject(t, srv), evRequestDone, requestID, uint32(1), uint32(9)); err != nil {
		t.Fatalf("SendEvent(request_done): %v", err)
	}

	result := await(t, results)
	if result.err != nil {
		t.Fatalf("Activate: %v", result.err)
	}
	if result.state.Revision != revision {
		t.Errorf("revision after activate = %d, want %d", result.state.Revision, revision)
	}
}

// A stale layout is reported as a stable code the CLI can act on, not as text.
func TestStaleRevisionIsReportedAsItsCode(t *testing.T) {
	srv := newServer(t)
	client := newClient(t, srv)

	results, cancel := runAsync(func(ctx context.Context) asyncResult {
		state, err := client.Activate(ctx, "app-1", 4)
		return asyncResult{state: state, err: err}
	})
	defer cancel()

	request := srv.WaitForRequest(t, generated.WlvisionControllerInterface, opActivate)
	requestID := request.Uint32(0)

	if err := srv.SendEvent(controllerObject(t, srv), evRequestFailed,
		requestID, uint32(CodeStaleRevision), "layout changed"); err != nil {
		t.Fatalf("SendEvent(request_failed): %v", err)
	}

	result := await(t, results)
	if result.err == nil {
		t.Fatal("Activate succeeded, want a failure")
	}
	if !errors.Is(result.err, ErrStaleRevision) {
		t.Fatalf("error = %v, want it to unwrap to ErrStaleRevision", result.err)
	}

	var requestErr *RequestError
	if !errors.As(result.err, &requestErr) {
		t.Fatalf("error type = %T, want *RequestError", result.err)
	}
	if requestErr.Message != "layout changed" {
		t.Errorf("message = %q", requestErr.Message)
	}
}

func TestResizeReportsRequestedAndVisibleSizes(t *testing.T) {
	srv := newServer(t)
	client := newClient(t, srv)
	controller := controllerObject(t, srv)

	// Give the facade a window to report geometry for.
	if err := srv.SendEvent(controller, evToplevelChanged,
		"app-1", "Example", "org.example.App",
		int32(0), int32(0), uint32(100), uint32(100), uint32(0), uint32(0), uint32(1)); err != nil {
		t.Fatalf("SendEvent(toplevel_changed): %v", err)
	}

	results, cancel := runAsync(func(ctx context.Context) asyncResult {
		resize, err := client.Resize(ctx, "app-1", Size{Width: 800, Height: 600}, 1)
		return asyncResult{resize: resize, err: err}
	})
	defer cancel()

	request := srv.WaitForRequest(t, generated.WlvisionControllerInterface, opResize)
	requestID := request.Uint32(0)
	handle, consumed := request.String(4)
	if handle != "app-1" {
		t.Fatalf("handle = %q", handle)
	}
	if got := request.Uint32(12 + consumed); got != 800 {
		t.Errorf("width = %d, want 800", got)
	}
	if got := request.Uint32(16 + consumed); got != 600 {
		t.Errorf("height = %d, want 600", got)
	}

	// The application committed the new size, and the compositor reports it.
	if err := srv.SendEvent(controller, evToplevelChanged,
		"app-1", "Example", "org.example.App",
		int32(0), int32(0), uint32(800), uint32(600), uint32(0), uint32(0), uint32(2)); err != nil {
		t.Fatalf("SendEvent(toplevel_changed): %v", err)
	}
	if err := srv.SendEvent(controller, evRequestDone, requestID, uint32(0), uint32(2)); err != nil {
		t.Fatalf("SendEvent(request_done): %v", err)
	}

	result := await(t, results)
	if result.err != nil {
		t.Fatalf("Resize: %v", result.err)
	}
	if result.resize.Requested != (Size{Width: 800, Height: 600}) {
		t.Errorf("requested = %s", result.resize.Requested)
	}
	if result.resize.Visible != (Size{Width: 800, Height: 600}) {
		t.Errorf("visible = %s, want the committed size", result.resize.Visible)
	}
	if result.resize.Revision != 2 {
		t.Errorf("revision = %d, want 2", result.resize.Revision)
	}
}

func TestPointerAndKeyEncoding(t *testing.T) {
	srv := newServer(t)
	client := newClient(t, srv)
	controller := controllerObject(t, srv)

	cases := []struct {
		name   string
		event  PointerEvent
		opcode uint16
		assert func(t *testing.T, request controltest.Request)
	}{
		{
			name:   "motion",
			event:  PointerEvent{Motion: &Point{X: 12.5, Y: 40}},
			opcode: opPointerMotion,
			assert: func(t *testing.T, request controltest.Request) {
				if got := request.Fixed(4); got != wl.Fixed(12.5*256) {
					t.Errorf("x = %d, want %d", got, wl.Fixed(12.5*256))
				}
				if got := request.Fixed(8); got != wl.Fixed(40*256) {
					t.Errorf("y = %d, want %d", got, wl.Fixed(40*256))
				}
			},
		},
		{
			name:   "button press",
			event:  PointerEvent{Button: &ButtonEvent{Button: 0x110, Pressed: true}},
			opcode: opPointerButton,
			assert: func(t *testing.T, request controltest.Request) {
				if got := request.Uint32(4); got != 0x110 {
					t.Errorf("button = %#x, want 0x110", got)
				}
				if got := request.Uint32(8); got != 1 {
					t.Errorf("state = %d, want 1 (pressed)", got)
				}
			},
		},
		{
			name:   "axis",
			event:  PointerEvent{Axis: &AxisEvent{Axis: AxisHorizontal, Value: -1}},
			opcode: opPointerAxis,
			assert: func(t *testing.T, request controltest.Request) {
				if got := request.Uint32(4); got != uint32(AxisHorizontal) {
					t.Errorf("axis = %d, want %d", got, AxisHorizontal)
				}
				if got := request.Fixed(8); got != wl.Fixed(-1*256) {
					t.Errorf("value = %d, want %d", got, wl.Fixed(-1*256))
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := len(srv.Requests())

			results, cancel := runAsync(func(ctx context.Context) asyncResult {
				return asyncResult{err: client.Pointer(ctx, tc.event)}
			})
			defer cancel()

			request := waitForNext(t, srv, before, generated.WlvisionControllerInterface, tc.opcode)
			tc.assert(t, request)
			if err := srv.SendEvent(controller, evRequestDone, request.Uint32(0), uint32(0), uint32(1)); err != nil {
				t.Fatalf("SendEvent(request_done): %v", err)
			}
			if result := await(t, results); result.err != nil {
				t.Fatalf("Pointer: %v", result.err)
			}
		})
	}

	t.Run("key release", func(t *testing.T) {
		before := len(srv.Requests())

		results, cancel := runAsync(func(ctx context.Context) asyncResult {
			return asyncResult{err: client.Key(ctx, KeyEvent{Key: 30, Pressed: false})}
		})
		defer cancel()

		request := waitForNext(t, srv, before, generated.WlvisionControllerInterface, opKey)
		if got := request.Uint32(4); got != 30 {
			t.Errorf("key = %d, want 30", got)
		}
		if got := request.Uint32(8); got != 0 {
			t.Errorf("state = %d, want 0 (released)", got)
		}
		if err := srv.SendEvent(controller, evRequestDone, request.Uint32(0), uint32(0), uint32(1)); err != nil {
			t.Fatalf("SendEvent(request_done): %v", err)
		}
		if result := await(t, results); result.err != nil {
			t.Fatalf("Key: %v", result.err)
		}
	})
}

func TestAuthorizeCaptureWaitsForItsOwnEvent(t *testing.T) {
	srv := newServer(t)
	client := newClient(t, srv)

	results, cancel := runAsync(func(ctx context.Context) asyncResult {
		id, err := client.AuthorizeCapture(ctx)
		return asyncResult{capture: id, err: err}
	})
	defer cancel()

	request := srv.WaitForRequest(t, generated.WlvisionControllerInterface, opAuthorizeCapture)
	requestID := request.Uint32(0)

	if err := srv.SendEvent(controllerObject(t, srv), evCaptureAuthorized, requestID); err != nil {
		t.Fatalf("SendEvent(capture_authorized): %v", err)
	}

	result := await(t, results)
	if result.err != nil {
		t.Fatalf("AuthorizeCapture: %v", result.err)
	}
	if result.capture != uint64(requestID) {
		t.Fatalf("capture request id = %d, want the request it authorized (%d)", result.capture, requestID)
	}
}

func TestFramesAreDelivered(t *testing.T) {
	srv := newServer(t)
	client := newClient(t, srv)

	if err := srv.SendEvent(controllerObject(t, srv), evFrame, uint32(11), uint32(3)); err != nil {
		t.Fatalf("SendEvent(frame): %v", err)
	}

	select {
	case frame := <-client.Frames():
		if frame.CaptureRequestID != 11 || frame.Sequence != 3 {
			t.Fatalf("frame = %+v, want capture 11 sequence 3", frame)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no frame was delivered")
	}
}

// A compositor that never answers must not hang the caller.
func TestRequestTimeoutIsReported(t *testing.T) {
	srv := newServer(t)
	client := newClient(t, srv)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := client.Activate(ctx, "app-1", 1)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", err)
	}
}

func TestCallsAfterCloseReportClosed(t *testing.T) {
	srv := newServer(t)
	client := newClient(t, srv)

	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if _, err := client.Snapshot(ctx); !errors.Is(err, ErrClosed) {
		t.Fatalf("Snapshot after Close = %v, want ErrClosed", err)
	}
}

// waitForBind waits for the registry bind request for an interface.
func waitForBind(t *testing.T, srv *controltest.Server, iface string) controltest.Request {
	t.Helper()

	registry := srv.RegistryID()
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, request := range srv.Requests() {
			if request.Object != registry || request.Opcode != 0 {
				continue
			}
			if got, _ := request.String(4); got == iface {
				return request
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no bind request for %q", iface)
		}
		time.Sleep(time.Millisecond)
	}
}

// waitForNext returns the first request recorded after index, avoiding the
// harness's "first match ever" behaviour for repeated request pairs.
func waitForNext(t *testing.T, srv *controltest.Server, index int, iface string, opcode uint16) controltest.Request {
	t.Helper()

	object := srv.ObjectID(iface)
	deadline := time.Now().Add(5 * time.Second)
	for {
		requests := srv.Requests()
		for i := index; i < len(requests); i++ {
			if requests[i].Object == object && requests[i].Opcode == opcode {
				return requests[i]
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no request object=%d opcode=%d after index %d", object, opcode, index)
		}
		time.Sleep(time.Millisecond)
	}
}
