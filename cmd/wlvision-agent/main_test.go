package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bnema/wlvision/internal/capture"
	"github.com/bnema/wlvision/internal/control"
	"github.com/bnema/wlvision/internal/result"
	"github.com/bnema/wlvision/internal/rpc"

	"github.com/bnema/wlturbo/wl"
)

// fakeController is the privileged surface the agent serves, without a
// compositor.
type fakeController struct {
	state        control.State
	revision     control.Revision
	resizeResult control.ResizeResult
	frames       chan control.Frame
	authorizeID  uint64
	err          error
	calls        []string
	pointers     []control.PointerEvent
	keys         []control.KeyEvent
}

func newFakeController() *fakeController {
	return &fakeController{
		state: control.State{
			Revision: 7,
			Toplevels: []control.Toplevel{
				{Handle: "app-1", Title: "probe", AppID: "probe", Size: control.Size{Width: 320, Height: 200}, Revision: 7},
			},
		},
		revision:     7,
		resizeResult: control.ResizeResult{Handle: "app-1", Requested: control.Size{Width: 400, Height: 300}, Visible: control.Size{Width: 400, Height: 300}, Revision: 9},
		frames:       make(chan control.Frame, 4),
		authorizeID:  11,
	}
}

func (f *fakeController) record(call string) { f.calls = append(f.calls, call) }

func (f *fakeController) Snapshot(context.Context) (control.State, error) {
	f.record("snapshot")
	return f.state, f.err
}

func (f *fakeController) Activate(_ context.Context, handle control.Handle, revision control.Revision) (control.State, error) {
	f.record("activate " + string(handle))
	return f.state, f.err
}

func (f *fakeController) Move(_ context.Context, handle control.Handle, revision control.Revision, at control.Point) (control.State, error) {
	f.record("move " + string(handle))
	return f.state, f.err
}

func (f *fakeController) Resize(_ context.Context, handle control.Handle, size control.Size, revision control.Revision) (control.ResizeResult, error) {
	f.record("resize " + string(handle))
	return f.resizeResult, f.err
}

func (f *fakeController) CloseWindow(_ context.Context, handle control.Handle, revision control.Revision) (control.State, error) {
	f.record("close " + string(handle))
	return f.state, f.err
}

func (f *fakeController) Pointer(_ context.Context, event control.PointerEvent) error {
	f.record("pointer")
	f.pointers = append(f.pointers, event)
	return f.err
}

func (f *fakeController) Key(_ context.Context, event control.KeyEvent) error {
	f.record("key")
	f.keys = append(f.keys, event)
	return f.err
}

func (f *fakeController) AuthorizeCapture(context.Context) (uint64, error) {
	f.record("authorize")
	return f.authorizeID, f.err
}

func (f *fakeController) Revision() control.Revision { return f.revision }
func (f *fakeController) State() control.State       { return f.state }
func (f *fakeController) Frames() <-chan control.Frame {
	return f.frames
}
func (f *fakeController) Context() *wl.Context   { return nil }
func (f *fakeController) Registry() *wl.Registry { return nil }

// fakePlane is the capture path of a session.
type fakePlane struct {
	frame  capture.Frame
	size   capture.Size
	format capture.Format
	err    error
	closed bool
}

func (p *fakePlane) WaitForSetup(context.Context) (capture.Size, capture.Format, error) {
	return p.size, p.format, p.err
}

func (p *fakePlane) Capture(context.Context) (capture.Frame, error) {
	if p.err != nil {
		return capture.Frame{}, p.err
	}
	return p.frame, nil
}

func (p *fakePlane) Close() error {
	p.closed = true
	return nil
}

func testAgent(t *testing.T) (*Agent, *fakeController, *fakePlane, string) {
	t.Helper()

	root := t.TempDir()
	controller := newFakeController()
	plane := &fakePlane{
		size:   capture.Size{Width: 64, Height: 48},
		format: capture.FormatXRGB8888,
		frame: capture.Frame{
			PNG:    []byte("not really a png"),
			Size:   capture.Size{Width: 64, Height: 48},
			Format: capture.FormatXRGB8888,
		},
	}

	options := Options{
		SocketPath: filepath.Join(root, "control", "agent.sock"),
		ControlUID: uint32(os.Getuid()),
		ControlDir: filepath.Join(root, "control"),
		ExportDir:  filepath.Join(root, "export"),
	}
	agent := newAgent(options, controller, func(context.Context, Controller) (CapturePlane, error) {
		return plane, nil
	}, &discard{}, time.Now)
	// Serve attaches the capture path before it serves anything; a test that
	// calls the handler directly sets the same state.
	agent.plane = plane

	return agent, controller, plane, root
}

type discard struct{}

func (*discard) Write(p []byte) (int, error) { return len(p), nil }

func request(t *testing.T, agent *Agent, operation string, params any) ([]byte, error) {
	t.Helper()

	payload, err := json.Marshal(Request{Operation: operation})
	if err != nil {
		t.Fatalf("encode the request: %v", err)
	}
	if params != nil {
		encoded, err := json.Marshal(params)
		if err != nil {
			t.Fatalf("encode the arguments: %v", err)
		}
		payload, err = json.Marshal(Request{Operation: operation, Params: encoded})
		if err != nil {
			t.Fatalf("encode the request: %v", err)
		}
	}
	return agent.handle(context.Background(), payload)
}

func replyOf(t *testing.T, payload []byte) Reply {
	t.Helper()

	var reply Reply
	if err := json.Unmarshal(payload, &reply); err != nil {
		t.Fatalf("decode the reply: %v (%q)", err, payload)
	}
	return reply
}

func failureCode(t *testing.T, err error) result.Code {
	t.Helper()

	var failure *result.Failure
	if !errors.As(err, &failure) {
		t.Fatalf("error %v (%T) is not a *result.Failure", err, err)
	}
	return failure.Code
}

func TestAgentServesTheControlPlaneOverTheSocket(t *testing.T) {
	agent, controller, _, _ := testAgent(t)

	// The capture path is attached before the socket answers, so the readiness
	// marker means the session can actually capture.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- agent.Serve(ctx) }()

	waitForFile(t, filepath.Join(agent.options.ControlDir, readyMarkerName))

	conn, err := rpc.Dial(ctx, agent.options.SocketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	payload, err := json.Marshal(Request{Operation: OpSnapshot})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	response, err := conn.Call(ctx, payload)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	reply := replyOf(t, response)
	if reply.State == nil || reply.Revision != 7 {
		t.Errorf("reply = %+v, want the session state at revision 7", reply)
	}
	if len(controller.calls) != 1 || controller.calls[0] != "snapshot" {
		t.Errorf("calls = %v, want one snapshot", controller.calls)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the agent did not stop")
	}
}

func TestAgentForwardsWindowOperationsWithTheirRevision(t *testing.T) {
	agent, controller, _, _ := testAgent(t)

	tests := []struct {
		operation string
		params    Params
		call      string
		check     func(t *testing.T, reply Reply)
	}{
		{"activate", Params{Handle: "app-1", Revision: 7}, "activate app-1", func(t *testing.T, reply Reply) {
			if reply.State == nil {
				t.Error("activate did not return the session state")
			}
		}},
		{"move", Params{Handle: "app-1", Revision: 7, X: 12, Y: 34}, "move app-1", func(t *testing.T, reply Reply) {
			if reply.State == nil {
				t.Error("move did not return the session state")
			}
		}},
		{"resize", Params{Handle: "app-1", Revision: 7, Width: 400, Height: 300}, "resize app-1", func(t *testing.T, reply Reply) {
			if reply.Resize == nil || reply.Resize.VisibleWidth != 400 {
				t.Errorf("resize reply = %+v, want the committed size", reply.Resize)
			}
		}},
		{"close_window", Params{Handle: "app-1", Revision: 7}, "close app-1", func(t *testing.T, reply Reply) {
			if reply.State == nil {
				t.Error("close did not return the session state")
			}
		}},
	}

	for _, test := range tests {
		t.Run(test.operation, func(t *testing.T) {
			payload, err := request(t, agent, test.operation, test.params)
			if err != nil {
				t.Fatalf("%s: %v", test.operation, err)
			}
			test.check(t, replyOf(t, payload))
			if calls := controller.calls; calls[len(calls)-1] != test.call {
				t.Errorf("calls = %v, want the last one to be %q", calls, test.call)
			}
		})
	}
}

func TestAgentBuildsExactlyOnePointerAction(t *testing.T) {
	agent, controller, _, _ := testAgent(t)

	payload, err := request(t, agent, OpPointer, Params{Kind: PointerMotion, X: 5, Y: 6})
	if err != nil {
		t.Fatalf("pointer motion: %v", err)
	}
	if reply := replyOf(t, payload); reply.Revision != 7 {
		t.Errorf("reply = %+v, want the session revision", reply)
	}
	if len(controller.pointers) != 1 || controller.pointers[0].Motion == nil {
		t.Fatalf("pointers = %+v, want one motion", controller.pointers)
	}
	if controller.pointers[0].Motion.X != 5 || controller.pointers[0].Button != nil || controller.pointers[0].Axis != nil {
		t.Errorf("pointer = %+v, want only the motion", controller.pointers[0])
	}

	if _, err := request(t, agent, OpPointer, Params{Kind: PointerButton, Button: 0x110, State: "pressed"}); err != nil {
		t.Fatalf("pointer button: %v", err)
	}
	if _, err := request(t, agent, OpPointer, Params{Kind: PointerAxis, Axis: 0, Value: -1}); err != nil {
		t.Fatalf("pointer axis: %v", err)
	}
	if len(controller.pointers) != 3 {
		t.Fatalf("pointers = %+v, want three actions", controller.pointers)
	}
	if controller.pointers[1].Button == nil || !controller.pointers[1].Button.Pressed {
		t.Errorf("button = %+v, want a press", controller.pointers[1])
	}
	if controller.pointers[2].Axis == nil || controller.pointers[2].Axis.Value != -1 {
		t.Errorf("axis = %+v, want the scroll step", controller.pointers[2])
	}

	_, err = request(t, agent, OpPointer, Params{Kind: "teleport"})
	if code := failureCode(t, err); code != result.CodeUsageError {
		t.Errorf("code = %s, want %s", code, result.CodeUsageError)
	}
	if len(controller.pointers) != 3 {
		t.Error("an unknown pointer kind reached the compositor")
	}
}

func TestAgentForwardsKeyTransitions(t *testing.T) {
	agent, controller, _, _ := testAgent(t)

	if _, err := request(t, agent, OpKey, Params{Key: 30, State: "pressed"}); err != nil {
		t.Fatalf("key press: %v", err)
	}
	if _, err := request(t, agent, OpKey, Params{Key: 30, State: "released"}); err != nil {
		t.Fatalf("key release: %v", err)
	}
	if len(controller.keys) != 2 || !controller.keys[0].Pressed || controller.keys[1].Pressed {
		t.Errorf("keys = %+v, want a press then a release", controller.keys)
	}

	_, err := request(t, agent, OpKey, Params{Key: 30, State: "maybe"})
	if code := failureCode(t, err); code != result.CodeUsageError {
		t.Errorf("code = %s, want %s", code, result.CodeUsageError)
	}
}

func TestAgentStoresACaptureAndReportsIt(t *testing.T) {
	agent, controller, plane, root := testAgent(t)
	controller.frames <- control.Frame{CaptureRequestID: 11, Sequence: 4}

	payload, err := request(t, agent, OpCapture, Params{Path: "shots/one.png"})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}

	reply := replyOf(t, payload)
	if reply.Frame == nil {
		t.Fatal("capture returned no frame")
	}

	stored := filepath.Join(root, "export", "shots", "one.png")
	if reply.Frame.Path != stored {
		t.Errorf("path = %q, want %q", reply.Frame.Path, stored)
	}

	written, err := os.ReadFile(stored)
	if err != nil {
		t.Fatalf("read the capture: %v", err)
	}
	if string(written) != string(plane.frame.PNG) {
		t.Error("the stored capture is not what the compositor produced")
	}

	digest := sha256.Sum256(plane.frame.PNG)
	if reply.Frame.Digest != "sha256:"+hex.EncodeToString(digest[:]) {
		t.Errorf("digest = %q, want the digest of the stored capture", reply.Frame.Digest)
	}
	if reply.Frame.Sequence != 4 || reply.Frame.CaptureRequestID != 11 {
		t.Errorf("frame = %+v, want the frame the module numbered for this authorization", reply.Frame)
	}
	if reply.Frame.Format != "xrgb8888" || reply.Frame.Width != 64 || reply.Frame.Height != 48 {
		t.Errorf("frame = %+v, want the announced format and size", reply.Frame)
	}

	if calls := controller.calls; len(calls) != 1 || calls[0] != "authorize" {
		t.Errorf("calls = %v, want the capture to authorize its own connection", calls)
	}
}

func TestAgentRefusesACaptureOutsideTheExportDirectory(t *testing.T) {
	agent, _, _, root := testAgent(t)

	for _, path := range []string{"", "/etc/passwd", "../escape.png", "../../escape.png"} {
		t.Run(path, func(t *testing.T) {
			_, err := request(t, agent, OpCapture, Params{Path: path})
			if code := failureCode(t, err); code != result.CodeUsageError {
				t.Fatalf("code = %s, want %s", code, result.CodeUsageError)
			}
		})
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read the root: %v", err)
	}
	for _, entry := range entries {
		if entry.Name() == "escape.png" {
			t.Error("a capture escaped the export directory")
		}
	}
}

func TestAgentReportsACaptureFailure(t *testing.T) {
	agent, _, plane, _ := testAgent(t)
	plane.err = capture.ErrCaptureUnavailable

	_, err := request(t, agent, OpCapture, Params{Path: "one.png"})
	if code := failureCode(t, err); code != result.CodeCaptureFailed {
		t.Errorf("code = %s, want %s", code, result.CodeCaptureFailed)
	}
}

func TestAgentWithoutACaptureSourceIsNotReady(t *testing.T) {
	agent, _, _, _ := testAgent(t)
	agent.plane = nil

	_, err := request(t, agent, OpCapture, Params{Path: "one.png"})
	if code := failureCode(t, err); code != result.CodeSessionNotReady {
		t.Errorf("code = %s, want %s", code, result.CodeSessionNotReady)
	}
}

func TestAgentReportsUnknownOperationsAndUnreadableRequests(t *testing.T) {
	agent, _, _, _ := testAgent(t)

	if _, err := agent.handle(context.Background(), []byte("not json")); failureCode(t, err) != result.CodeUsageError {
		t.Error("an unreadable request was accepted")
	}
	if _, err := request(t, agent, "levitate", nil); failureCode(t, err) != result.CodeUsageError {
		t.Error("an unknown operation was accepted")
	}
}

func TestAgentKeepsTheCodeTheControlLayerAssigned(t *testing.T) {
	agent, controller, _, _ := testAgent(t)
	controller.err = result.NewFailure(result.CodeStaleRevision, "window.activate", "the layout moved")

	_, err := request(t, agent, OpActivate, Params{Handle: "app-1", Revision: 3})
	if code := failureCode(t, err); code != result.CodeStaleRevision {
		t.Errorf("code = %s, want the code the control layer assigned", code)
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never appeared", path)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
