// Command controlprobe verifies the wlvision Weston module at runtime.
//
// It runs inside the session container, next to a Weston that loads
// wlvision-shell, and drives the control protocol the way the resident
// controller will: enumerate the windows, activate one, refuse a stale
// revision, resize and wait for the application to commit, inject pointer and
// keyboard input, and capture a frame once the module has authorized this
// connection.
//
// Every check prints one PASS or FAIL line. Any failure exits non-zero, so the
// harness can treat the run as a gate.
//
// With -expect-denied it instead proves the negative: a client whose UID is not
// the control UID must not be able to bind the control protocol at all.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/bnema/wlturbo/wl"
	"github.com/bnema/wlvision/internal/capture"
	"github.com/bnema/wlvision/internal/control"
)

// What the fixture application announces. It is the same purpose-built
// toplevel client the shell probe uses (test/weston-shell/wlvision-shell-client.c),
// so the runtime verification adds no second client to maintain.
const (
	appTitle = "wlvision-probe-title"
	appID    = "wlvision.probe"
)

var failed int

func check(name string, ok bool, detail string) bool {
	if ok {
		fmt.Printf("PASS: %s\n", name)
		return true
	}

	fmt.Printf("FAIL: %s", name)
	if detail != "" {
		fmt.Printf(": %s", detail)
	}
	fmt.Println()
	failed++
	return false
}

func main() {
	expectDenied := flag.Bool("expect-denied", false,
		"assert that this UID may not bind the control protocol")
	timeout := flag.Duration("timeout", 30*time.Second, "overall deadline")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	if *expectDenied {
		os.Exit(runExpectDenied(ctx))
	}

	os.Exit(run(ctx))
}

// runExpectDenied checks that a foreign UID cannot bind the control protocol.
func runExpectDenied(ctx context.Context) int {
	client, err := control.Connect(ctx, "")
	if err == nil {
		_ = client.Close()
		check("a foreign UID is refused the control protocol", false,
			"the connection succeeded")
		return 1
	}

	check("a foreign UID is refused the control protocol", true, "")
	fmt.Printf("INFO: refusal was: %v\n", err)
	return 0
}

func run(ctx context.Context) int {
	client, err := control.Connect(ctx, "")
	if !check("connect to the compositor and create a controller", err == nil, errText(err)) {
		return 1
	}
	defer func() { _ = client.Close() }()

	state, err := client.Snapshot(ctx)
	if !check("snapshot returns the session's windows", err == nil, errText(err)) {
		return 1
	}
	fmt.Printf("INFO: snapshot revision=%d windows=%d\n", state.Revision, len(state.Toplevels))

	window, ok := findApp(state)
	if !check("the application's window is enumerated with its metadata", ok,
		fmt.Sprintf("no window with app id %q in %+v", appID, state.Toplevels)) {
		return 1
	}
	fmt.Printf("INFO: window handle=%s title=%q app_id=%q size=%s revision=%d\n",
		window.Handle, window.Title, window.AppID, window.Size, window.Revision)

	// A window operation with the revision the caller saw succeeds, and the
	// same operation with a stale one is refused with its own code.
	if _, err := client.Activate(ctx, window.Handle, window.Revision); !check(
		"activate with the current revision succeeds", err == nil, errText(err)) {
		return 1
	}

	stale := window.Revision - 1
	_, err = client.Activate(ctx, window.Handle, stale)
	check("activate with a stale revision is refused as stale_revision",
		errors.Is(err, control.ErrStaleRevision), errText(err))

	_, err = client.Activate(ctx, "app-does-not-exist", client.Revision())
	check("activate on an unknown handle is refused as window_not_found",
		errors.Is(err, control.ErrWindowNotFound), errText(err))

	// Resize completes only once the application commits the buffer it was
	// configured with. The requested, configured, committed and visible sizes
	// each come from their own source: configured is set by resize_configured,
	// committed and visible by resize_done.
	//
	// The fixture (test/weston-shell/wlvision-shell-client.c) derives every
	// buffer it commits from the configure it was sent, so it cannot be driven
	// into committing a size different from its configure, and a single resize
	// cannot time out here. The deadline path is proven deterministically
	// against the fake compositor instead (internal/control/client_test.go);
	// this gate proves the four sizes on a real configure-to-commit round trip.
	wanted := control.Size{Width: 400, Height: 300}
	resize, err := client.Resize(ctx, window.Handle, wanted, client.Revision())
	if !check("resize completes after the application commits", err == nil, errText(err)) {
		return 1
	}
	fmt.Printf("INFO: resize requested=%s configured=%s committed=%s visible=%s revision=%d\n",
		resize.Requested, resize.Configured, resize.Committed, resize.Visible, resize.Revision)
	check("resize reports the requested, configured, committed and visible sizes",
		resize.Requested == wanted && resize.Configured == wanted &&
			resize.Committed == wanted && resize.Visible == wanted,
		fmt.Sprintf("requested=%s configured=%s committed=%s visible=%s, want all %s",
			resize.Requested, resize.Configured, resize.Committed, resize.Visible, wanted))

	// Input injection is accepted, and the connection survives it.
	motionErr := client.Pointer(ctx, control.PointerEvent{
		Motion: &control.Point{X: float64(wanted.Width) / 2, Y: float64(wanted.Height) / 2},
	})
	check("pointer motion is injected", motionErr == nil, errText(motionErr))

	buttonErr := client.Pointer(ctx, control.PointerEvent{
		Button: &control.ButtonEvent{Button: 0x110, Pressed: true},
	})
	check("pointer button is injected", buttonErr == nil, errText(buttonErr))
	_ = client.Pointer(ctx, control.PointerEvent{
		Button: &control.ButtonEvent{Button: 0x110, Pressed: false},
	})

	keyErr := client.Key(ctx, control.KeyEvent{Key: 30, Pressed: true})
	check("key press is injected", keyErr == nil, errText(keyErr))
	_ = client.Key(ctx, control.KeyEvent{Key: 30, Pressed: false})

	// Capture must be refused before the module authorizes this connection.
	captureObject, err := capture.BindCapture(client.Context(), client.Registry())
	if !check("the compositor offers weston_capture_v1", err == nil, errText(err)) {
		return 1
	}

	output, err := bindOutput(client)
	if !check("the session's output can be bound", err == nil, errText(err)) {
		return 1
	}

	source, err := capture.CreateSource(ctx, captureObject, output, capture.SourceFramebuffer)
	if !check("a capture source can be created", err == nil, errText(err)) {
		return 1
	}
	defer func() { _ = source.Close() }()

	shm, err := capture.BindShm(client.Context(), client.Registry())
	if !check("wl_shm can be bound", err == nil, errText(err)) {
		return 1
	}
	defer func() { _ = shm.Close() }()

	_, err = source.Capture(ctx, shm)
	check("capture is refused before the module authorizes the connection",
		errors.Is(err, capture.ErrCaptureUnavailable), errText(err))

	// Frames observed before the capture are not evidence about it, so drop
	// whatever is already buffered.
	drainFrames(client)

	// Once authorized, the same capture must succeed, produce pixels, and be
	// numbered by the module's frame sequencing.
	captureRequestID, err := client.AuthorizeCapture(ctx)
	if !check("the module authorizes capture for the controller", err == nil, errText(err)) {
		return 1
	}

	frame, err := source.Capture(ctx, shm)
	if !check("capture completes once authorized", err == nil, errText(err)) {
		return 1
	}
	fmt.Printf("INFO: captured %s %s digest=%x png=%d bytes\n",
		frame.Size, frame.Format, frame.Digest[:8], len(frame.PNG))

	select {
	case observed := <-client.Frames():
		check("the module reports a frame sequence after the capture", observed.Sequence > 0,
			fmt.Sprintf("frame sequence %d", observed.Sequence))
		check("the frame carries the capture authorization it belongs to",
			observed.CaptureRequestID == captureRequestID,
			fmt.Sprintf("frame carries %d, authorization was %d",
				observed.CaptureRequestID, captureRequestID))
		fmt.Printf("INFO: frame capture_request_id=%d sequence=%d\n",
			observed.CaptureRequestID, observed.Sequence)
	case <-time.After(5 * time.Second):
		check("the module reports a frame sequence after the capture", false,
			"no frame event arrived")
	case <-ctx.Done():
		check("the module reports a frame sequence after the capture", false, ctx.Err().Error())
	}

	if failed > 0 {
		fmt.Printf("FAIL: %d check(s) failed\n", failed)
		return 1
	}

	fmt.Println("PASS: all control protocol checks succeeded")
	return 0
}

// drainFrames drops frames observed before a capture, so the frame read
// afterwards is the one the capture produced.
func drainFrames(client control.Client) {
	for {
		select {
		case <-client.Frames():
		case <-time.After(200 * time.Millisecond):
			return
		}
	}
}

func findApp(state control.State) (control.Toplevel, bool) {
	for _, toplevel := range state.Toplevels {
		if toplevel.AppID == appID && toplevel.Title == appTitle {
			return toplevel, true
		}
	}

	return control.Toplevel{}, false
}

// bindOutput binds the session's output, which the capture path needs: the
// control protocol deliberately carries no outputs.
func bindOutput(client control.Client) (*wl.Output, error) {
	registry := client.Registry()

	global, ok := registry.FindGlobal("wl_output")
	if !ok {
		return nil, errors.New("the compositor offers no wl_output")
	}

	output := &wl.Output{}
	id, err := registry.BindID(global.Name, "wl_output", global.Version)
	if err != nil {
		return nil, err
	}

	output.SetContext(client.Context())
	output.SetID(id)
	client.Context().Register(output)

	return output, nil
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
