package control

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/bnema/wlturbo/wl"
	"github.com/bnema/wlvision/internal/control/generated"
)

// ErrClosed is returned when the control connection is closed, either by the
// caller or because the compositor went away.
var ErrClosed = errors.New("wlvision control: connection closed")

// Client is the typed facade over the private control protocol.
//
// It owns the only privileged Wayland connection in a session, keeps the
// revision and window state the compositor reported, and serializes requests.
// Generated protocol types never leak through it.
type Client interface {
	// Snapshot returns the live windows at one revision.
	Snapshot(ctx context.Context) (State, error)

	// Activate gives a window keyboard focus.
	Activate(ctx context.Context, handle Handle, revision Revision) (State, error)

	// Move places a window at a logical position.
	Move(ctx context.Context, handle Handle, revision Revision, at Point) (State, error)

	// Resize requests a window size. It returns only after the application
	// committed a matching buffer, so a successful result means the size took
	// effect and a failure means it did not.
	Resize(ctx context.Context, handle Handle, size Size, revision Revision) (ResizeResult, error)

	// CloseWindow asks a window to close.
	CloseWindow(ctx context.Context, handle Handle, revision Revision) (State, error)

	// Pointer injects exactly one pointer action.
	Pointer(ctx context.Context, event PointerEvent) error

	// Key injects one key transition.
	Key(ctx context.Context, event KeyEvent) error

	// AuthorizeCapture asks the module to authorize capture for this session
	// and returns the capture request id the module correlated it with.
	AuthorizeCapture(ctx context.Context) (uint64, error)

	// Binds a test or a checker can reach, and the connection the module
	// authorizes for capture.
	//
	// The controller owns the session's only privileged connection. Capture in
	// particular must happen here: the module authorizes the controller's own
	// client and denies every other attempt.
	Context() *wl.Context

	// Registry exposes the connection's registry, so a caller can bind further
	// globals on the connection the module authorized.
	Registry() *wl.Registry

	// Frames reports every repaint the module observed, including repaints it
	// forced for a capture request.
	Frames() <-chan Frame

	// Revision is the last revision the compositor reported.
	Revision() Revision

	// State is the current window state as last observed.
	State() State

	// Close releases the connection.
	Close() error
}

// Connect binds the control manager on the compositor's display socket and
// creates the session's single controller.
//
// An empty socket path uses the Wayland environment variables, exactly as the
// rest of the client library does.
func Connect(ctx context.Context, socketPath string) (Client, error) {
	display, err := wl.Connect(socketPath)
	if err != nil {
		return nil, fmt.Errorf("connect to compositor: %w", err)
	}

	registry := display.GetRegistry()
	if err := display.Roundtrip(); err != nil {
		_ = display.Close()
		return nil, fmt.Errorf("enumerate globals: %w", err)
	}

	global, ok := registry.FindGlobal(generated.WlvisionControlInterface)
	if !ok {
		_ = display.Close()
		return nil, fmt.Errorf("compositor does not offer %s", generated.WlvisionControlInterface)
	}

	c := &client{
		display:   display,
		pending:   make(map[uint32]*pendingCall),
		toplevels: make(map[Handle]Toplevel),
		removed:   make(map[Handle]struct{}),
		frames:    make(chan Frame, 64),
		closed:    make(chan struct{}),
		done:      make(chan struct{}),
	}

	manager := generated.NewWlvisionControl(display.Context())
	if err := registry.Bind(global.Name, generated.WlvisionControlInterface, global.Version, manager); err != nil {
		_ = display.Close()
		return nil, fmt.Errorf("bind %s: %w", generated.WlvisionControlInterface, err)
	}
	c.manager = manager

	controller, err := manager.CreateController()
	if err != nil {
		_ = display.Close()
		return nil, fmt.Errorf("create controller: %w", err)
	}
	c.controller = controller
	c.registerHandlers(controller)

	if err := display.Roundtrip(); err != nil {
		_ = display.Close()
		return nil, fmt.Errorf("controller handshake: %w", err)
	}

	go c.dispatch()

	return c, nil
}

// pendingKind distinguishes the events a caller waits for.
type pendingKind int

const (
	waitRequest pendingKind = iota // request_done / request_failed
	waitCapture                    // capture_authorized
)

// outcome is what a completed request produced.
type outcome struct {
	err      error
	revision Revision
}

// pendingCall is one in-flight request.
type pendingCall struct {
	kind  pendingKind
	reply chan outcome
	// snapshot collects the events a snapshot answer is made of.
	snapshot *[]Toplevel
}

type client struct {
	display    *wl.Display
	manager    *generated.WlvisionControl
	controller *generated.WlvisionController

	sendMu sync.Mutex

	mu            sync.Mutex
	nextRequestID uint32
	pending       map[uint32]*pendingCall
	toplevels     map[Handle]Toplevel
	removed       map[Handle]struct{}
	revision      Revision
	closedFlag    bool

	frames chan Frame
	closed chan struct{}
	done   chan struct{}
	once   sync.Once
}

func (c *client) registerHandlers(controller *generated.WlvisionController) {
	controller.OnSnapshot(func(revisionHi, revisionLo uint32) {
		revision := RevisionFromWords(revisionHi, revisionLo)

		c.mu.Lock()
		defer c.mu.Unlock()
		c.revision = revision
		for _, call := range c.pending {
			if call.snapshot != nil {
				*call.snapshot = []Toplevel{}
			}
		}
	})

	controller.OnToplevelChanged(func(handle, title, appID string, x, y int32, width, height uint32, state uint32, revisionHi, revisionLo uint32) {
		toplevel := Toplevel{
			Handle:   Handle(handle),
			Title:    title,
			AppID:    appID,
			X:        x,
			Y:        y,
			Size:     Size{Width: width, Height: height},
			State:    state,
			Revision: RevisionFromWords(revisionHi, revisionLo),
		}

		c.mu.Lock()
		defer c.mu.Unlock()
		c.revision = toplevel.Revision
		c.toplevels[toplevel.Handle] = toplevel
		for _, call := range c.pending {
			if call.snapshot != nil {
				*call.snapshot = upsertToplevel(*call.snapshot, toplevel)
			}
		}
	})

	controller.OnToplevelRemoved(func(handle string) {
		c.mu.Lock()
		defer c.mu.Unlock()
		delete(c.toplevels, Handle(handle))
		// Handles are never reused in a session, so remembering that one is
		// gone stops a snapshot that predates the removal from reviving it.
		c.removed[Handle(handle)] = struct{}{}
		for _, call := range c.pending {
			if call.snapshot != nil {
				*call.snapshot = removeToplevel(*call.snapshot, Handle(handle))
			}
		}
	})

	controller.OnRequestDone(func(requestID, revisionHi, revisionLo uint32) {
		revision := RevisionFromWords(revisionHi, revisionLo)

		c.mu.Lock()
		if revision > c.revision {
			c.revision = revision
		}
		c.mu.Unlock()

		c.finish(requestID, outcome{revision: revision})
	})

	controller.OnRequestFailed(func(requestID, code uint32, message string) {
		c.finish(requestID, outcome{
			err: &RequestError{Code: ErrorCode(code), Message: message},
		})
	})

	controller.OnFrame(func(captureRequestID, frameSequence uint32) {
		frame := Frame{
			CaptureRequestID: uint64(captureRequestID),
			Sequence:         uint64(frameSequence),
		}
		select {
		case c.frames <- frame:
		default:
			// A caller that does not drain frames must not stall dispatch.
		}
	})

	controller.OnCaptureAuthorized(func(captureRequestID uint32) {
		c.finish(captureRequestID, outcome{})
	})
}

// finish delivers the result of a request to whoever is waiting for it.
func (c *client) finish(requestID uint32, result outcome) {
	c.mu.Lock()
	call, ok := c.pending[requestID]
	if ok {
		delete(c.pending, requestID)
	}
	c.mu.Unlock()

	if !ok {
		return
	}
	call.reply <- result
}

func (c *client) dispatch() {
	defer close(c.done)

	for {
		err := c.display.Dispatch()
		if err != nil {
			c.failAll(fmt.Errorf("%w: %v", ErrClosed, err))
			return
		}
	}
}

// failAll wakes every waiter when the connection is gone.
func (c *client) failAll(err error) {
	c.mu.Lock()
	pending := c.pending
	c.pending = make(map[uint32]*pendingCall)
	c.closedFlag = true
	c.mu.Unlock()

	for _, call := range pending {
		call.reply <- outcome{err: err}
	}
}

// send performs one request and waits for its result.
func (c *client) send(ctx context.Context, kind pendingKind, snapshot *[]Toplevel, issue func(requestID uint32) error) (outcome, error) {
	c.mu.Lock()
	if c.closedFlag {
		c.mu.Unlock()
		return outcome{}, ErrClosed
	}
	c.nextRequestID++
	requestID := c.nextRequestID
	call := &pendingCall{kind: kind, reply: make(chan outcome, 1), snapshot: snapshot}
	c.pending[requestID] = call
	c.mu.Unlock()

	c.sendMu.Lock()
	err := issue(requestID)
	c.sendMu.Unlock()

	if err != nil {
		c.forget(requestID)
		return outcome{}, fmt.Errorf("send request %d: %w", requestID, err)
	}

	select {
	case result := <-call.reply:
		return result, result.err
	case <-ctx.Done():
		c.forget(requestID)
		return outcome{}, ctx.Err()
	case <-c.closed:
		return outcome{}, ErrClosed
	}
}

func (c *client) forget(requestID uint32) {
	c.mu.Lock()
	delete(c.pending, requestID)
	c.mu.Unlock()
}

func (c *client) Snapshot(ctx context.Context) (State, error) {
	toplevels := []Toplevel{}
	result, err := c.send(ctx, waitRequest, &toplevels, func(requestID uint32) error {
		return c.controller.Snapshot(requestID)
	})
	if err != nil {
		return State{}, err
	}

	c.mu.Lock()
	mergeSnapshot(c.toplevels, c.removed, toplevels)
	state := make([]Toplevel, 0, len(c.toplevels))
	for _, toplevel := range c.toplevels {
		state = append(state, toplevel)
	}
	c.mu.Unlock()

	sortToplevels(state)
	return State{Revision: result.revision, Toplevels: state}, nil
}

func (c *client) Activate(ctx context.Context, handle Handle, revision Revision) (State, error) {
	hi, lo := RevisionWords(revision)
	_, err := c.send(ctx, waitRequest, nil, func(requestID uint32) error {
		return c.controller.Activate(requestID, string(handle), hi, lo)
	})
	if err != nil {
		return State{}, err
	}
	return c.State(), nil
}

func (c *client) Move(ctx context.Context, handle Handle, revision Revision, at Point) (State, error) {
	hi, lo := RevisionWords(revision)
	_, err := c.send(ctx, waitRequest, nil, func(requestID uint32) error {
		return c.controller.Move(requestID, string(handle), hi, lo, int32(at.X), int32(at.Y))
	})
	if err != nil {
		return State{}, err
	}
	return c.State(), nil
}

func (c *client) CloseWindow(ctx context.Context, handle Handle, revision Revision) (State, error) {
	hi, lo := RevisionWords(revision)
	_, err := c.send(ctx, waitRequest, nil, func(requestID uint32) error {
		return c.controller.Close(requestID, string(handle), hi, lo)
	})
	if err != nil {
		return State{}, err
	}
	return c.State(), nil
}

func (c *client) Resize(ctx context.Context, handle Handle, size Size, revision Revision) (ResizeResult, error) {
	hi, lo := RevisionWords(revision)
	result, err := c.send(ctx, waitRequest, nil, func(requestID uint32) error {
		return c.controller.Resize(requestID, string(handle), hi, lo, size.Width, size.Height)
	})
	if err != nil {
		return ResizeResult{}, err
	}

	resize := ResizeResult{Handle: handle, Requested: size, Revision: result.revision}
	if toplevel, ok := c.State().Find(handle); ok {
		resize.Visible = toplevel.Size
	}
	return resize, nil
}

func (c *client) Pointer(ctx context.Context, event PointerEvent) error {
	_, err := c.send(ctx, waitRequest, nil, func(requestID uint32) error {
		switch {
		case event.Motion != nil:
			return c.controller.PointerMotion(requestID, fixedFromFloat(event.Motion.X), fixedFromFloat(event.Motion.Y))
		case event.Button != nil:
			return c.controller.PointerButton(requestID, event.Button.Button, buttonState(event.Button.Pressed))
		case event.Axis != nil:
			return c.controller.PointerAxis(requestID, uint32(event.Axis.Axis), fixedFromFloat(event.Axis.Value))
		default:
			return errors.New("empty pointer event")
		}
	})
	return err
}

func (c *client) Key(ctx context.Context, event KeyEvent) error {
	_, err := c.send(ctx, waitRequest, nil, func(requestID uint32) error {
		return c.controller.Key(requestID, event.Key, buttonState(event.Pressed))
	})
	return err
}

func (c *client) AuthorizeCapture(ctx context.Context) (uint64, error) {
	var captureRequestID uint64

	c.mu.Lock()
	c.nextRequestID++
	requestID := c.nextRequestID
	call := &pendingCall{kind: waitCapture, reply: make(chan outcome, 1)}
	c.pending[requestID] = call
	c.mu.Unlock()

	c.sendMu.Lock()
	err := c.controller.AuthorizeCapture(requestID)
	c.sendMu.Unlock()
	if err != nil {
		c.forget(requestID)
		return 0, fmt.Errorf("authorize capture: %w", err)
	}

	select {
	case result := <-call.reply:
		if result.err != nil {
			return 0, result.err
		}
		captureRequestID = uint64(requestID)
	case <-ctx.Done():
		c.forget(requestID)
		return 0, ctx.Err()
	case <-c.closed:
		return 0, ErrClosed
	}

	return captureRequestID, nil
}

func (c *client) Context() *wl.Context { return c.display.Context() }

func (c *client) Registry() *wl.Registry { return c.display.GetRegistry() }

func (c *client) Frames() <-chan Frame { return c.frames }

func (c *client) Revision() Revision {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.revision
}

func (c *client) State() State {
	c.mu.Lock()
	defer c.mu.Unlock()

	state := State{Revision: c.revision, Toplevels: make([]Toplevel, 0, len(c.toplevels))}
	for _, toplevel := range c.toplevels {
		state.Toplevels = append(state.Toplevels, toplevel)
	}
	sortToplevels(state.Toplevels)
	return state
}

func (c *client) Close() error {
	c.once.Do(func() {
		close(c.closed)
		c.failAll(ErrClosed)
		_ = c.display.Close()
		<-c.done
		close(c.frames)
	})
	return nil
}

// fixedFromFloat converts logical pixels to the 24.8 fixed point the protocol
// carries.
func fixedFromFloat(value float64) wl.Fixed {
	return wl.Fixed(value * 256)
}

// buttonState maps a boolean transition to the protocol's state word.
func buttonState(pressed bool) uint32 {
	if pressed {
		return 1
	}
	return 0
}

// mergeSnapshot folds one snapshot enumeration into the cached window state.
//
// A snapshot lists the windows that existed when it was requested, but events
// that arrived afterwards have already updated the cache, so an entry that is
// newer than the snapshot wins and a handle that was removed is not revived.
func mergeSnapshot(cached map[Handle]Toplevel, removed map[Handle]struct{}, snapshot []Toplevel) {
	for _, toplevel := range snapshot {
		if _, gone := removed[toplevel.Handle]; gone {
			continue
		}
		if known, ok := cached[toplevel.Handle]; ok && known.Revision >= toplevel.Revision {
			continue
		}
		cached[toplevel.Handle] = toplevel
	}
}

// upsertToplevel replaces a window in a list or appends it.
func upsertToplevel(toplevels []Toplevel, toplevel Toplevel) []Toplevel {
	for i := range toplevels {
		if toplevels[i].Handle == toplevel.Handle {
			toplevels[i] = toplevel
			return toplevels
		}
	}
	return append(toplevels, toplevel)
}

func removeToplevel(toplevels []Toplevel, handle Handle) []Toplevel {
	for i := range toplevels {
		if toplevels[i].Handle == handle {
			return append(toplevels[:i], toplevels[i+1:]...)
		}
	}
	return toplevels
}

// sortToplevels orders windows by handle so repeated reads are stable.
func sortToplevels(toplevels []Toplevel) {
	slices.SortFunc(toplevels, func(a, b Toplevel) int {
		return strings.Compare(string(a.Handle), string(b.Handle))
	})
}
