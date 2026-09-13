package control

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bnema/wlturbo/wl"
	"github.com/bnema/wlvision/internal/control/generated"
	"github.com/bnema/wlvision/internal/controltest"
	"github.com/bnema/wlvision/internal/result"
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
	evResizeConfigured
	evResizeDone
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

func TestResizeReportsAllFourSizes(t *testing.T) {
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
		resize, err := client.Resize(ctx, "app-1", Size{Width: 1024, Height: 768}, 1)
		return asyncResult{resize: resize, err: err}
	})
	defer cancel()

	request := srv.WaitForRequest(t, generated.WlvisionControllerInterface, opResize)
	requestID := request.Uint32(0)
	handle, consumed := request.String(4)
	if handle != "app-1" {
		t.Fatalf("handle = %q", handle)
	}
	if got := request.Uint32(12 + consumed); got != 1024 {
		t.Errorf("width = %d, want 1024", got)
	}
	if got := request.Uint32(16 + consumed); got != 768 {
		t.Errorf("height = %d, want 768", got)
	}

	// The module configured the application with one size and answered first
	// with resize_configured; the resize only completes on the resize_done that
	// follows the commit. Each field below carries its own source, which is why
	// the values differ: requested 1024x768, configured 640x480, committed and
	// visible 800x600.
	if err := srv.SendEvent(controller, evResizeConfigured, requestID, int32(640), int32(480)); err != nil {
		t.Fatalf("SendEvent(resize_configured): %v", err)
	}
	if err := srv.SendEvent(controller, evResizeDone,
		requestID, int32(640), int32(480), int32(800), int32(600),
		int32(800), int32(600), uint32(0), uint32(2)); err != nil {
		t.Fatalf("SendEvent(resize_done): %v", err)
	}

	got := await(t, results)
	if got.err != nil {
		t.Fatalf("Resize: %v", got.err)
	}
	if got.resize.Requested != (Size{Width: 1024, Height: 768}) {
		t.Errorf("requested = %s, want 1024x768", got.resize.Requested)
	}
	if got.resize.Configured != (Size{Width: 640, Height: 480}) {
		t.Errorf("configured = %s, want 640x480 (the configure, not the request)", got.resize.Configured)
	}
	if got.resize.Committed != (Size{Width: 800, Height: 600}) {
		t.Errorf("committed = %s, want 800x600", got.resize.Committed)
	}
	if got.resize.Visible != (Size{Width: 800, Height: 600}) {
		t.Errorf("visible = %s, want 800x600", got.resize.Visible)
	}
	if got.resize.Revision != 2 {
		t.Errorf("revision = %d, want 2", got.resize.Revision)
	}
}

// A resize the module refuses still travels as request_failed with its code.
func TestResizeRefusalKeepsTheProtocolCode(t *testing.T) {
	for _, tc := range []struct {
		name   string
		handle Handle
		code   ErrorCode
		want   error
	}{
		{"stale revision", "app-1", CodeStaleRevision, ErrStaleRevision},
		{"unknown handle", "app-missing", CodeWindowNotFound, ErrWindowNotFound},
		{"invalid argument", "app-1", CodeInvalidArgument, ErrInvalidArgument},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newServer(t)
			client := newClient(t, srv)

			results, cancel := runAsync(func(ctx context.Context) asyncResult {
				resize, err := client.Resize(ctx, tc.handle, Size{Width: 400, Height: 300}, 9)
				return asyncResult{resize: resize, err: err}
			})
			defer cancel()

			request := srv.WaitForRequest(t, generated.WlvisionControllerInterface, opResize)
			if err := srv.SendEvent(controllerObject(t, srv), evRequestFailed,
				request.Uint32(0), uint32(tc.code), "refused"); err != nil {
				t.Fatalf("SendEvent(request_failed): %v", err)
			}

			got := await(t, results)
			if !errors.Is(got.err, tc.want) {
				t.Fatalf("error = %v, want it to unwrap to %v", got.err, tc.want)
			}
			if got.resize != (ResizeResult{}) {
				t.Errorf("failed resize returned %+v, want the zero result", got.resize)
			}
		})
	}
}

// A deadline reports what was observed: the configure that was sent, that no
// commit matched it, and the geometry the facade last saw.
func TestResizeTimeoutReportsWhatWasObserved(t *testing.T) {
	srv := newServer(t)
	client := newClient(t, srv)
	controller := controllerObject(t, srv)

	if err := srv.SendEvent(controller, evToplevelChanged,
		"app-1", "Example", "org.example.App",
		int32(0), int32(0), uint32(100), uint32(100), uint32(0), uint32(0), uint32(1)); err != nil {
		t.Fatalf("SendEvent(toplevel_changed): %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	results := make(chan asyncResult, 1)
	go func() {
		resize, err := client.Resize(ctx, "app-1", Size{Width: 1024, Height: 768}, client.Revision())
		results <- asyncResult{resize: resize, err: err}
	}()

	request := srv.WaitForRequest(t, generated.WlvisionControllerInterface, opResize)
	// The module configured the application, but no commit ever followed.
	if err := srv.SendEvent(controller, evResizeConfigured, request.Uint32(0), int32(640), int32(480)); err != nil {
		t.Fatalf("SendEvent(resize_configured): %v", err)
	}

	got := await(t, results)

	var failure *result.Failure
	if !errors.As(got.err, &failure) {
		t.Fatalf("error = %v (%T), want *result.Failure", got.err, got.err)
	}
	if failure.Code != result.CodeWaitTimeout {
		t.Errorf("code = %s, want %s", failure.Code, result.CodeWaitTimeout)
	}
	if !strings.Contains(failure.Message, "did not commit the configured size") {
		t.Errorf("message = %q, want it to state the application did not commit in time", failure.Message)
	}

	want := map[string]string{
		"handle":     "app-1",
		"requested":  "1024x768",
		"configured": "640x480",
		"committed":  "unknown",
		"visible":    "100x100",
	}
	for key, value := range want {
		if detail := failure.Details[key]; detail != value {
			t.Errorf("details[%q] = %q, want %q", key, detail, value)
		}
	}
}

// The module keeps one outstanding resize per window: a second request
// replaces the first, whose caller learns about it by timing out with no
// progress of its own.
func TestResizeSupersededBySecondRequestTimesOut(t *testing.T) {
	srv := newServer(t)
	client := newClient(t, srv)
	controller := controllerObject(t, srv)

	firstCtx, cancelFirst := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancelFirst()
	first := make(chan asyncResult, 1)
	go func() {
		resize, err := client.Resize(firstCtx, "app-1", Size{Width: 300, Height: 200}, 1)
		first <- asyncResult{resize: resize, err: err}
	}()

	firstRequest := srv.WaitForRequest(t, generated.WlvisionControllerInterface, opResize)
	firstID := firstRequest.Uint32(0)

	// The first request was configured before it was replaced, so its own
	// progress must be what its deadline reports.
	if err := srv.SendEvent(controller, evResizeConfigured, firstID, int32(300), int32(200)); err != nil {
		t.Fatalf("SendEvent(resize_configured): %v", err)
	}

	second, cancelSecond := runAsync(func(ctx context.Context) asyncResult {
		resize, err := client.Resize(ctx, "app-1", Size{Width: 400, Height: 300}, 1)
		return asyncResult{resize: resize, err: err}
	})
	defer cancelSecond()

	var secondRequest controltest.Request
	deadline := time.Now().Add(5 * time.Second)
	for secondRequest.Body == nil {
		for _, request := range srv.Requests() {
			if request.Object == controller && request.Opcode == opResize && request.Uint32(0) != firstID {
				secondRequest = request
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the second resize was never sent")
		}
		time.Sleep(time.Millisecond)
	}

	if err := srv.SendEvent(controller, evResizeConfigured, secondRequest.Uint32(0), int32(400), int32(300)); err != nil {
		t.Fatalf("SendEvent(resize_configured): %v", err)
	}
	if err := srv.SendEvent(controller, evResizeDone,
		secondRequest.Uint32(0), int32(400), int32(300), int32(400), int32(300),
		int32(400), int32(300), uint32(0), uint32(2)); err != nil {
		t.Fatalf("SendEvent(resize_done): %v", err)
	}

	secondResult := await(t, second)
	if secondResult.err != nil {
		t.Fatalf("second Resize: %v", secondResult.err)
	}
	if secondResult.resize.Requested != (Size{Width: 400, Height: 300}) {
		t.Errorf("second requested = %s, want 400x300", secondResult.resize.Requested)
	}

	firstResult := await(t, first)
	var failure *result.Failure
	if !errors.As(firstResult.err, &failure) {
		t.Fatalf("superseded resize error = %v, want *result.Failure", firstResult.err)
	}
	if failure.Code != result.CodeWaitTimeout {
		t.Errorf("superseded code = %s, want %s", failure.Code, result.CodeWaitTimeout)
	}
	// The first request reported its own configure before it was replaced, so
	// its deadline names that configure, never the successor's 400x300.
	if detail := failure.Details["configured"]; detail != "300x200" {
		t.Errorf("superseded configured = %q, want its own 300x200", detail)
	}
	if detail := failure.Details["requested"]; detail != "300x200" {
		t.Errorf("superseded requested = %q, want 300x200", detail)
	}
	if detail := failure.Details["handle"]; detail != "app-1" {
		t.Errorf("superseded handle = %q, want app-1", detail)
	}
}

// A resize that times out is a failure of that request, not of the
// connection: the facade must keep serving afterwards.
func TestFacadeServesAfterAResizeDeadline(t *testing.T) {
	srv := newServer(t)
	client := newClient(t, srv)
	controller := controllerObject(t, srv)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := client.Resize(ctx, "app-1", Size{Width: 300, Height: 200}, 1); err == nil {
		t.Fatal("Resize succeeded, want a deadline failure")
	}

	results, cancelSnapshot := runAsync(func(ctx context.Context) asyncResult {
		state, err := client.Snapshot(ctx)
		return asyncResult{state: state, err: err}
	})
	defer cancelSnapshot()

	request := srv.WaitForRequest(t, generated.WlvisionControllerInterface, opSnapshot)
	if err := srv.SendEvent(controller, evSnapshot, uint32(0), uint32(3)); err != nil {
		t.Fatalf("SendEvent(snapshot): %v", err)
	}
	if err := srv.SendEvent(controller, evRequestDone, request.Uint32(0), uint32(0), uint32(3)); err != nil {
		t.Fatalf("SendEvent(request_done): %v", err)
	}

	got := await(t, results)
	if got.err != nil {
		t.Fatalf("Snapshot after a resize deadline: %v", got.err)
	}
	if got.state.Revision != 3 {
		t.Errorf("revision = %d, want 3", got.state.Revision)
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

// A snapshot lists the windows that existed when it was requested. Events that
// arrived afterwards have already updated the cache, and a snapshot must not
// undo them: a window that moved on keeps its newer state, and a window that
// was removed stays removed.
func TestMergeSnapshotKeepsNewerStateAndRemovedHandles(t *testing.T) {
	cached := map[Handle]Toplevel{
		"app-1": {Handle: "app-1", Title: "newer", Revision: 5},
		"app-4": {Handle: "app-4", Title: "unchanged", Revision: 9},
	}
	removed := map[Handle]struct{}{"app-2": {}}

	snapshot := []Toplevel{
		{Handle: "app-1", Title: "stale", Revision: 3},
		{Handle: "app-2", Title: "gone", Revision: 7},
		{Handle: "app-3", Title: "new", Revision: 7},
		{Handle: "app-4", Title: "same revision", Revision: 9},
	}

	mergeSnapshot(cached, removed, snapshot)

	if got := cached["app-1"].Title; got != "newer" {
		t.Errorf("app-1 title = %q, want the newer cached state", got)
	}
	if _, ok := cached["app-2"]; ok {
		t.Error("a removed window was revived by a snapshot")
	}
	if got := cached["app-3"].Title; got != "new" {
		t.Errorf("app-3 title = %q, want the window the snapshot added", got)
	}
	if got := cached["app-4"].Title; got != "unchanged" {
		t.Errorf("app-4 title = %q, want the cached state to win a tie", got)
	}
	if len(cached) != 3 {
		t.Errorf("cached windows = %d, want 3", len(cached))
	}
}
