package capture

import (
	"context"
	"errors"
	"fmt"
	"image"
	"sync"
	"time"

	"github.com/bnema/wlturbo/wl"
	"github.com/bnema/wlvision/internal/capture/generated"
)

// ErrCaptureUnavailable reports that the compositor cannot capture: it refused
// the request, or the source was created for an output that produces nothing.
var ErrCaptureUnavailable = errors.New("capture: compositor refused the capture")

// ErrSourceClosed reports use of a source after it was destroyed.
var ErrSourceClosed = errors.New("capture: source is closed")

// ErrSourceRetired reports a capture attempt on a source whose previous
// capture was abandoned. Once a request is abandoned the compositor still owes
// this source a result for it, and that result cannot be told apart from the
// answer to a new request, so the source must be recreated instead of reused.
var ErrSourceRetired = errors.New("capture: a previous capture was abandoned; create a new source")

// SourceKind selects what the compositor captures.
type SourceKind uint32

// Capture sources, matching weston_capture_v1.source.
const (
	SourceFramebuffer     SourceKind = SourceKind(generated.SOURCE_FRAMEBUFFER)
	SourceFullFramebuffer SourceKind = SourceKind(generated.SOURCE_FULL_FRAMEBUFFER)
)

// Frame is one captured picture with the metadata an agent needs to reason
// about it.
type Frame struct {
	Image    *image.RGBA
	PNG      []byte
	Digest   [32]byte
	Size     Size
	Format   Format
	Captured time.Time
}

// Size is a pixel size.
type Size struct {
	Width  int
	Height int
}

// String renders a size.
func (s Size) String() string { return fmt.Sprintf("%dx%d", s.Width, s.Height) }

// Source captures one output.
//
// A source owns the compositor's capture object for that output and serializes
// captures through it: only one capture may be in flight, because the protocol
// has no way to tell two completions apart.
type Source struct {
	source *generated.WestonCaptureSource

	mu        sync.Mutex
	format    Format
	formatSet bool
	size      Size
	inFlight  bool
	retired   bool

	complete chan struct{}
	retry    chan struct{}
	failed   chan string

	closed bool
}

// BindCapture binds weston_capture_v1 from the registry.
func BindCapture(ctx *wl.Context, registry *wl.Registry) (*generated.WestonCapture, error) {
	if ctx == nil || registry == nil {
		return nil, fmt.Errorf("bind capture: nil context or registry")
	}

	global, ok := registry.FindGlobal(generated.WestonCaptureInterface)
	if !ok {
		return nil, fmt.Errorf("compositor does not offer %s", generated.WestonCaptureInterface)
	}

	capture := generated.NewWestonCapture(ctx)
	if err := registry.Bind(global.Name, generated.WestonCaptureInterface, global.Version, capture); err != nil {
		return nil, fmt.Errorf("bind %s: %w", generated.WestonCaptureInterface, err)
	}
	return capture, nil
}

// CreateSource creates a capture source for one output.
//
// The compositor announces the source's size and pixel format right after
// creation, so a caller waits for both before allocating a buffer.
func CreateSource(ctx context.Context, capture *generated.WestonCapture, output *wl.Output, kind SourceKind) (*Source, error) {
	if capture == nil {
		return nil, errors.New("capture: nil capture object")
	}
	if output == nil {
		return nil, errors.New("capture: nil output")
	}

	source := &Source{
		complete: make(chan struct{}, 1),
		retry:    make(chan struct{}, 1),
		failed:   make(chan string, 1),
	}

	created, err := capture.Create(output, uint32(kind))
	if err != nil {
		return nil, fmt.Errorf("create capture source: %w", err)
	}
	source.source = created

	created.OnFormat(func(drmFormat uint32) {
		format, err := FormatFromDRM(drmFormat)
		if err != nil {
			source.fail(err.Error())
			return
		}

		source.mu.Lock()
		source.format = format
		source.formatSet = true
		source.mu.Unlock()
	})

	created.OnSize(func(width, height int32) {
		if width <= 0 || height <= 0 {
			source.fail(fmt.Sprintf("compositor reported size %dx%d", width, height))
			return
		}

		source.mu.Lock()
		source.size = Size{Width: int(width), Height: int(height)}
		source.mu.Unlock()
	})

	created.OnComplete(func() {
		select {
		case source.complete <- struct{}{}:
		default:
		}
	})

	created.OnRetry(func() {
		select {
		case source.retry <- struct{}{}:
		default:
		}
	})

	created.OnFailed(func(message string) {
		source.fail(message)
	})

	return source, nil
}

// fail records a compositor failure as a pending result.
func (s *Source) fail(message string) {
	if message == "" {
		message = "capture failed"
	}
	select {
	case s.failed <- message:
	default:
	}
}

// WaitForSetup blocks until the compositor has announced the source's size and
// format, so a caller can size a buffer before capturing.
func (s *Source) WaitForSetup(ctx context.Context) (Size, Format, error) {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()

	for {
		s.mu.Lock()
		size, format, known := s.size, s.format, s.formatSet
		s.mu.Unlock()

		// The announced format is what makes the source usable, not a non-zero
		// value: a valid format is allowed to be zero.
		if size.Width > 0 && known {
			return size, format, nil
		}

		select {
		case <-ctx.Done():
			return Size{}, 0, fmt.Errorf("wait for capture source setup: %w", ctx.Err())
		case message := <-s.failed:
			return Size{}, 0, fmt.Errorf("%w: %s", ErrCaptureUnavailable, message)
		case <-ticker.C:
		}
	}
}

// Capture captures one frame into a freshly allocated buffer.
//
// The buffer is sized from what the compositor announced, and the returned
// frame carries the decoded image, its PNG encoding and a stride-independent
// digest. Only one capture may be in flight; a second call waits.
func (s *Source) Capture(ctx context.Context, shm *Shm) (Frame, error) {
	size, format, err := s.WaitForSetup(ctx)
	if err != nil {
		return Frame{}, err
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return Frame{}, ErrSourceClosed
	}
	if s.inFlight {
		s.mu.Unlock()
		return Frame{}, errors.New("capture: another capture is in flight")
	}
	s.inFlight = true
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.inFlight = false
		s.mu.Unlock()
	}()

	buffer, err := shm.NewBuffer(format, size.Width, size.Height)
	if err != nil {
		return Frame{}, err
	}
	defer func() { _ = buffer.Release() }()

	return s.captureInto(ctx, format, buffer)
}

// captureInto requests a capture into an already allocated buffer and waits for
// the compositor to report the result.
//
// It is separate from Capture so a test can supply a buffer whose pixels it
// controls: the compositor fills the mapping, which a test harness cannot do
// without speaking the descriptor protocol.
func (s *Source) captureInto(ctx context.Context, format Format, buffer *Buffer) (Frame, error) {
	// A source whose previous capture was abandoned is unusable: the
	// compositor's late answer to that request would be indistinguishable from
	// the answer to this one, which is how a caller would receive a frame that
	// was never written.
	s.mu.Lock()
	retired := s.retired
	s.mu.Unlock()
	if retired {
		return Frame{}, ErrSourceRetired
	}

	if err := s.source.Capture(buffer.Proxy()); err != nil {
		return Frame{}, fmt.Errorf("request capture: %w", err)
	}

	select {
	case <-ctx.Done():
		s.mu.Lock()
		s.retired = true
		s.mu.Unlock()
		return Frame{}, fmt.Errorf("wait for capture: %w", ctx.Err())
	case message := <-s.failed:
		return Frame{}, fmt.Errorf("%w: %s", ErrCaptureUnavailable, message)
	case <-s.retry:
		// The compositor asks for another attempt; the caller decides when.
		return Frame{}, ErrCaptureUnavailable
	case <-s.complete:
	}

	captured := time.Now()
	size := Size{Width: buffer.Width(), Height: buffer.Height()}
	img, err := Decode(format, buffer.Stride(), size.Width, size.Height, buffer.Pixels())
	if err != nil {
		return Frame{}, err
	}
	encoded, err := EncodePNG(img)
	if err != nil {
		return Frame{}, err
	}

	return Frame{
		Image:    img,
		PNG:      encoded,
		Digest:   Digest(img),
		Size:     size,
		Format:   format,
		Captured: captured,
	}, nil
}

// Close releases the capture source.
func (s *Source) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	if s.source != nil {
		return s.source.Destroy()
	}
	return nil
}
