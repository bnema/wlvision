package capture

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bnema/wlturbo/wl"
	"github.com/bnema/wlvision/internal/capture/generated"
	"github.com/bnema/wlvision/internal/controltest"
)

// Global names announced by the fake compositor.
const (
	shmGlobalName     = 1
	outputGlobalName  = 2
	captureGlobalName = 3
)

// Request opcodes. The capture protocol declares destroy before create, and
// destroy before capture, so the useful requests are opcode 1 in both
// interfaces.
const (
	opCaptureCreate uint16 = 1
	opSourceCapture uint16 = 1
)

// Capture source event opcodes, in declaration order.
const (
	evFormat uint16 = iota
	evSize
	evComplete
	evRetry
	evFailed
	evFormatsDone
)

type fixture struct {
	server  *controltest.Server
	display *wl.Display
	context *wl.Context
	output  *wl.Output
	shm     *Shm
	capture *generated.WestonCapture
	creates int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	server := controltest.Start(t,
		controltest.Global{Name: shmGlobalName, Interface: "wl_shm", Version: 1},
		controltest.Global{Name: outputGlobalName, Interface: "wl_output", Version: 4},
		controltest.Global{Name: captureGlobalName, Interface: generated.WestonCaptureInterface, Version: 2},
	)
	server.Env(t)

	display, err := wl.Connect("")
	if err != nil {
		t.Fatalf("wl.Connect: %v", err)
	}
	// Cleanups run last-registered-first, so the dispatcher's cleanup must close
	// the connection it is blocked reading from before anything waits on it.
	t.Cleanup(func() { _ = display.Close() })

	if err := display.Roundtrip(); err != nil {
		t.Fatalf("Roundtrip: %v", err)
	}

	registry := display.GetRegistry()
	context := display.Context()

	output := &wl.Output{}
	outputID, err := registry.BindID(outputGlobalName, "wl_output", 4)
	if err != nil {
		t.Fatalf("bind wl_output: %v", err)
	}
	output.SetContext(context)
	output.SetID(outputID)
	context.Register(output)

	shm, err := BindShm(context, registry)
	if err != nil {
		t.Fatalf("BindShm: %v", err)
	}
	t.Cleanup(func() { _ = shm.Close() })

	captureObject, err := BindCapture(context, registry)
	if err != nil {
		t.Fatalf("BindCapture: %v", err)
	}

	// The capture path waits on events, so something must dispatch them.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if err := display.Dispatch(); err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		_ = display.Close()
		<-done
	})

	return &fixture{
		server:  server,
		display: display,
		context: context,
		output:  output,
		shm:     shm,
		capture: captureObject,
	}
}

// newSource creates a capture source and announces its size and format, the way
// a compositor does right after the create request.
func (f *fixture) newSource(t *testing.T, kind SourceKind) (*Source, uint32) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	source, err := CreateSource(ctx, f.capture, f.output, kind)
	if err != nil {
		t.Fatalf("CreateSource: %v", err)
	}
	t.Cleanup(func() { _ = source.Close() })

	f.creates++
	// The bind request for the capture global is asynchronous, so wait for the
	// object ID instead of reading it once and waiting on a zero.
	manager := f.server.WaitForObjectID(t, generated.WestonCaptureInterface)
	create := f.nthRequest(t, manager, opCaptureCreate, f.creates)
	if got := create.Uint32(0); got != f.output.ID() {
		t.Fatalf("create output = %d, want %d", got, f.output.ID())
	}
	if got := create.Uint32(4); got != uint32(kind) {
		t.Fatalf("create source kind = %d, want %d", got, kind)
	}

	// The harness tracks registry binds and the control protocol's controller,
	// not this protocol's child objects, so read the new_id the client sent.
	sourceID := create.Uint32(8)
	if sourceID == 0 {
		t.Fatal("the create request carried no source object ID")
	}

	if err := f.server.SendEvent(sourceID, evSize, int32(64), int32(48)); err != nil {
		t.Fatalf("SendEvent(size): %v", err)
	}
	if err := f.server.SendEvent(sourceID, evFormat, formatXRGB8888); err != nil {
		t.Fatalf("SendEvent(format): %v", err)
	}

	return source, sourceID
}

func (f *fixture) request(t *testing.T, iface string, opcode uint16) controltest.Request {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for {
		// The compositor learns the object ID from the bind request, which may
		// still be in flight, so look it up on every pass.
		if object := f.server.ObjectID(iface); object != 0 {
			if request, ok := lastRequest(f.server.Requests(), object, opcode); ok {
				return request
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no request opcode=%d for %s (object=%d)", opcode, iface, f.server.ObjectID(iface))
		}
		time.Sleep(time.Millisecond)
	}
}

// nthRequest waits for the nth request on an explicit object, which is what
// child objects need: the harness only tracks registry binds and the control
// protocol's controller, not every new_id a client allocates.
func (f *fixture) nthRequest(t *testing.T, object uint32, opcode uint16, n int) controltest.Request {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	var seen int
	for {
		seen = 0
		var match controltest.Request
		for _, request := range f.server.Requests() {
			if request.Object == object && request.Opcode == opcode {
				seen++
				match = request
			}
		}
		if seen >= n {
			return match
		}
		if time.Now().After(deadline) {
			t.Fatalf("saw %d requests object=%d opcode=%d, want at least %d", seen, object, opcode, n)
		}
		time.Sleep(time.Millisecond)
	}
}

func lastRequest(requests []controltest.Request, object uint32, opcode uint16) (controltest.Request, bool) {
	for i := len(requests) - 1; i >= 0; i-- {
		if requests[i].Object == object && requests[i].Opcode == opcode {
			return requests[i], true
		}
	}
	return controltest.Request{}, false
}

// formatXRGB8888 is the four-character code the capture protocol announces.
const formatXRGB8888 = 0x34325258

// formatARGB8888 is the other format the pinned compositor offers. Its value is
// the zero value of Format, which is why the setup path must not treat zero as
// "nothing announced yet".
const formatARGB8888 = 0x34325241

func TestCreateSourceAndWaitForSetup(t *testing.T) {
	f := newFixture(t)
	source, _ := f.newSource(t, SourceFramebuffer)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	size, format, err := source.WaitForSetup(ctx)
	if err != nil {
		t.Fatalf("WaitForSetup: %v", err)
	}
	if size != (Size{Width: 64, Height: 48}) {
		t.Errorf("size = %s, want 64x48", size)
	}
	if format != FormatXRGB8888 {
		t.Errorf("format = %s, want xrgb8888", format)
	}

	// A full-framebuffer source asks the compositor for the full framebuffer.
	if _, _ = f.newSource(t, SourceFullFramebuffer); true {
		requests := f.server.Requests()
		found := false
		for _, request := range requests {
			if request.Object == f.server.ObjectID(generated.WestonCaptureInterface) &&
				request.Opcode == opCaptureCreate && request.Uint32(4) == uint32(SourceFullFramebuffer) {
				found = true
			}
		}
		if !found {
			t.Error("no create request asked for the full framebuffer")
		}
	}
}

// A compositor may announce the format whose value is zero first, so the setup
// path must track that a format arrived rather than comparing it to zero.
func TestWaitForSetupAcceptsTheZeroValuedFormat(t *testing.T) {
	f := newFixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	source, err := CreateSource(ctx, f.capture, f.output, SourceFramebuffer)
	if err != nil {
		t.Fatalf("CreateSource: %v", err)
	}
	t.Cleanup(func() { _ = source.Close() })

	f.creates++
	manager := f.server.WaitForObjectID(t, generated.WestonCaptureInterface)
	create := f.nthRequest(t, manager, opCaptureCreate, f.creates)
	sourceID := create.Uint32(8)
	if sourceID == 0 {
		t.Fatal("the create request carried no source object ID")
	}

	if err := f.server.SendEvent(sourceID, evSize, int32(64), int32(48)); err != nil {
		t.Fatalf("SendEvent(size): %v", err)
	}
	if err := f.server.SendEvent(sourceID, evFormat, formatARGB8888); err != nil {
		t.Fatalf("SendEvent(format): %v", err)
	}

	size, format, err := source.WaitForSetup(ctx)
	if err != nil {
		t.Fatalf("WaitForSetup: %v", err)
	}
	if format != FormatARGB8888 {
		t.Errorf("format = %s, want argb8888", format)
	}
	if size != (Size{Width: 64, Height: 48}) {
		t.Errorf("size = %s, want 64x48", size)
	}
}

// An abandoned capture leaves the compositor owing this source a result. That
// result cannot be told apart from the answer to a later request, so the source
// must be retired instead of reused: otherwise a caller receives a frame that
// was never written.
func TestAnAbandonedCaptureRetiresTheSource(t *testing.T) {
	f := newFixture(t)
	source, sourceID := f.newSource(t, SourceFramebuffer)

	setupCtx, cancelSetup := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelSetup()
	if _, _, err := source.WaitForSetup(setupCtx); err != nil {
		t.Fatalf("WaitForSetup: %v", err)
	}

	buffer, err := f.shm.NewBuffer(FormatXRGB8888, 64, 48)
	if err != nil {
		t.Fatalf("NewBuffer: %v", err)
	}
	defer func() { _ = buffer.Release() }()

	// No compositor answer arrives within the deadline, so the capture is
	// abandoned while its request is still outstanding.
	abandoned, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := source.captureInto(abandoned, FormatXRGB8888, buffer); err == nil {
		t.Fatal("a capture that was never answered succeeded")
	}

	// The next attempt is refused before it reaches the compositor, so the late
	// answer to the abandoned request can never be mistaken for a fresh one.
	retry, cancelRetry := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancelRetry()
	if _, err := source.captureInto(retry, FormatXRGB8888, buffer); !errors.Is(err, ErrSourceRetired) {
		t.Fatalf("second capture error = %v, want ErrSourceRetired", err)
	}

	requests := 0
	for _, request := range f.server.Requests() {
		if request.Object == sourceID && request.Opcode == opSourceCapture {
			requests++
		}
	}
	if requests != 1 {
		t.Errorf("capture requests = %d, want only the abandoned one", requests)
	}
}

// The capture request must carry the buffer the compositor should fill, and the
// frame that comes back must be the pixels that buffer holds.
func TestCaptureRequestsBufferAndDecodesIt(t *testing.T) {
	f := newFixture(t)
	source, sourceID := f.newSource(t, SourceFramebuffer)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, _, err := source.WaitForSetup(ctx); err != nil {
		t.Fatalf("WaitForSetup: %v", err)
	}

	buffer, err := f.shm.NewBuffer(FormatXRGB8888, 64, 48)
	if err != nil {
		t.Fatalf("NewBuffer: %v", err)
	}
	defer func() { _ = buffer.Release() }()

	// Fill the mapping the way the compositor would: B, G, R, X in memory.
	pixels := buffer.Pixels()
	for i := 0; i < len(pixels); i += 4 {
		pixels[i] = 0x33   // blue
		pixels[i+1] = 0x22 // green
		pixels[i+2] = 0x11 // red
		pixels[i+3] = 0x00 // unused
	}

	type result struct {
		frame Frame
		err   error
	}
	results := make(chan result, 1)
	go func() {
		frame, err := source.captureInto(ctx, FormatXRGB8888, buffer)
		results <- result{frame: frame, err: err}
	}()

	captureRequest := f.nthRequest(t, sourceID, opSourceCapture, 1)
	if got := captureRequest.Uint32(0); got != buffer.Proxy().ID() {
		t.Fatalf("capture buffer = %d, want the buffer the client created (%d)", got, buffer.Proxy().ID())
	}

	if err := f.server.SendEvent(sourceID, evComplete); err != nil {
		t.Fatalf("SendEvent(complete): %v", err)
	}

	select {
	case got := <-results:
		if got.err != nil {
			t.Fatalf("capture: %v", got.err)
		}
		if got.frame.Size != (Size{Width: 64, Height: 48}) {
			t.Errorf("frame size = %s, want 64x48", got.frame.Size)
		}
		if got.frame.Format != FormatXRGB8888 {
			t.Errorf("frame format = %s", got.frame.Format)
		}
		if got.frame.Image == nil {
			t.Fatal("frame has no decoded image")
		}
		if got.frame.Image.Bounds().Dx() != 64 || got.frame.Image.Bounds().Dy() != 48 {
			t.Errorf("image bounds = %v, want 64x48", got.frame.Image.Bounds())
		}
		if len(got.frame.PNG) == 0 {
			t.Error("frame has no PNG bytes")
		}
		expected, err := Decode(FormatXRGB8888, buffer.Stride(), 64, 48, pixels)
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if got.frame.Digest != Digest(expected) {
			t.Error("the captured digest does not describe the pixels the compositor wrote")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("capture did not return")
	}
}

func TestCaptureFailureIsReported(t *testing.T) {
	f := newFixture(t)
	source, sourceID := f.newSource(t, SourceFramebuffer)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, _, err := source.WaitForSetup(ctx); err != nil {
		t.Fatalf("WaitForSetup: %v", err)
	}

	if err := f.server.SendEvent(sourceID, evFailed, "output is gone"); err != nil {
		t.Fatalf("SendEvent(failed): %v", err)
	}

	buffer, err := f.shm.NewBuffer(FormatXRGB8888, 64, 48)
	if err != nil {
		t.Fatalf("NewBuffer: %v", err)
	}
	defer func() { _ = buffer.Release() }()

	_, err = source.captureInto(ctx, FormatXRGB8888, buffer)
	if !errors.Is(err, ErrCaptureUnavailable) {
		t.Fatalf("error = %v, want ErrCaptureUnavailable", err)
	}
}

// A retry means the compositor could not produce a frame this time; wlvision
// reports it rather than silently capturing something else.
func TestCaptureRetryIsReported(t *testing.T) {
	f := newFixture(t)
	source, sourceID := f.newSource(t, SourceFramebuffer)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, _, err := source.WaitForSetup(ctx); err != nil {
		t.Fatalf("WaitForSetup: %v", err)
	}

	if err := f.server.SendEvent(sourceID, evRetry); err != nil {
		t.Fatalf("SendEvent(retry): %v", err)
	}

	buffer, err := f.shm.NewBuffer(FormatXRGB8888, 64, 48)
	if err != nil {
		t.Fatalf("NewBuffer: %v", err)
	}
	defer func() { _ = buffer.Release() }()

	if _, err := source.captureInto(ctx, FormatXRGB8888, buffer); !errors.Is(err, ErrCaptureUnavailable) {
		t.Fatalf("error = %v, want ErrCaptureUnavailable", err)
	}
}

// A compositor that never announces the source must not hang the caller.
func TestWaitForSetupHonoursTheDeadline(t *testing.T) {
	f := newFixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	source, err := CreateSource(ctx, f.capture, f.output, SourceFramebuffer)
	cancel()
	if err != nil {
		t.Fatalf("CreateSource: %v", err)
	}
	t.Cleanup(func() { _ = source.Close() })

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer waitCancel()

	if _, _, err := source.WaitForSetup(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", err)
	}
}

// An unknown pixel format is refused rather than decoded as something else.
func TestUnknownFormatFailsSetup(t *testing.T) {
	f := newFixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	source, err := CreateSource(ctx, f.capture, f.output, SourceFramebuffer)
	if err != nil {
		t.Fatalf("CreateSource: %v", err)
	}
	t.Cleanup(func() { _ = source.Close() })

	create := f.request(t, generated.WestonCaptureInterface, opCaptureCreate)
	sourceID := create.Uint32(8)

	if err := f.server.SendEvent(sourceID, evFormat, uint32(0xdeadbeef)); err != nil {
		t.Fatalf("SendEvent(format): %v", err)
	}
	if err := f.server.SendEvent(sourceID, evSize, int32(8), int32(8)); err != nil {
		t.Fatalf("SendEvent(size): %v", err)
	}

	if _, _, err := source.WaitForSetup(ctx); !errors.Is(err, ErrCaptureUnavailable) {
		t.Fatalf("error = %v, want the unsupported format to be refused", err)
	}
}
