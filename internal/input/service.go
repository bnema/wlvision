// Package input performs window and input operations on a running session.
//
// It is the host-side half of the interaction contract: it validates what a
// caller asked for, resolves a window handle against a fresh snapshot,
// converts window-content coordinates to output coordinates, and sends the
// resulting operations over the frozen agentapi.Caller boundary. It never
// speaks the control protocol itself, so it is tested with a fake caller and
// needs no engine, no container and no compositor to compile or to test.
//
// Every operation that names a window reads a snapshot first and sends the
// revision that snapshot reported. A mutation never carries a revision the
// service has not just observed, so a stale target is refused before anything
// reaches the session rather than after it has acted on the wrong window.
package input

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/bnema/wlvision/internal/agentapi"
	"github.com/bnema/wlvision/internal/result"
)

// releaseTimeout bounds the independent context a cleanup operation gets when
// the caller's own context is gone. A button or a key that stays held outlives
// the request that pressed it, so its release is attempted on a context of its
// own even after a cancellation.
const releaseTimeout = 2 * time.Second

// defaultButton is the pointer button a click uses when the caller names none.
const defaultButton uint32 = 1

// Scroll axes, matching wl_pointer and the controller's own axis values.
const (
	AxisVertical   uint32 = 0
	AxisHorizontal uint32 = 1
)

// shiftName is the keysym name of the modifier the typing path holds.
const shiftName = "Shift_L"

// Service performs window and input operations over a Caller.
//
// It keeps no state: every operation that names a window reads a fresh
// snapshot, so the coordinates and the revision it sends are always the ones
// the session just reported.
//
// Pointer events are the honest exception. The control protocol carries no
// revision on a pointer event, so a motion built from one snapshot can land on
// a layout that changed before the injection reached the compositor; only
// window requests are revision-checked by the module. Reading a snapshot per
// click narrows that window but cannot close it without a revision on the
// pointer operation itself.
type Service struct {
	caller agentapi.Caller
}

// NewService returns a service that sends every operation through caller.
func NewService(caller agentapi.Caller) *Service {
	return &Service{caller: caller}
}

// Target names a window and, optionally, the revision the caller saw.
//
// A zero Revision means "the revision the service reads itself": the service
// reads a snapshot and uses that snapshot's revision, which is the same
// reading its coordinates come from.
type Target struct {
	Handle   string
	Revision uint64
}

// MoveRequest places a window at an output position.
type MoveRequest struct {
	Target
	// X and Y are output coordinates, not window-content ones.
	X float64
	Y float64
}

// ResizeRequest asks a window for a size.
type ResizeRequest struct {
	Target
	Width  uint32
	Height uint32
	// TimeoutMS bounds how long the session may spend waiting for the
	// application to commit the configured size. Zero leaves the session's own
	// budget in force; a caller that sets it hears what the session observed at
	// that deadline instead of giving up blind.
	TimeoutMS uint32
}

// ClickRequest clicks inside a window's content.
type ClickRequest struct {
	Target
	// X and Y are window-content-relative: (0,0) is the window's top-left
	// corner, not the output's.
	X float64
	Y float64
	// Button 0 means the default, BTN_LEFT.
	Button uint32
}

// PointerRequest is one raw pointer action selected by Kind: a motion, a
// button transition or a scroll step. Only the fields of the selected shape
// may be set.
type PointerRequest struct {
	Kind   string
	X      float64
	Y      float64
	Button uint32
	State  string
	Axis   uint32
	Value  float64
}

// ScrollRequest is one scroll step on an axis.
type ScrollRequest struct {
	// Axis is AxisVertical (0) or AxisHorizontal (1).
	Axis  uint32
	Value float64
}

// KeyRequest is one key transition. Exactly one of Keycode and Name is set;
// Name resolves through the embedded layout artifact.
type KeyRequest struct {
	Keycode uint32
	Name    string
	State   string
}

// TypeRequest types text with the embedded US layout.
type TypeRequest struct {
	Text string
}

// ClickResult reports where a click landed and in which revision.
type ClickResult struct {
	Handle   string
	Button   uint32
	OutputX  float64
	OutputY  float64
	Revision uint64
	// State is the snapshot the click's coordinates and revision came from.
	State agentapi.State
}

// TypeResult reports what typing sent.
type TypeResult struct {
	Text string
	// Strokes is the number of key strokes the text resolved to, one per
	// character; a shifted character is one stroke.
	Strokes int
}

// Windows returns the session's windows at a fresh revision.
func (s *Service) Windows(ctx context.Context) (agentapi.State, error) {
	return s.snapshot(ctx)
}

// Activate gives the target window keyboard focus.
func (s *Service) Activate(ctx context.Context, target Target) (agentapi.State, error) {
	toplevel, state, err := s.resolve(ctx, agentapi.OpActivate, target)
	if err != nil {
		return agentapi.State{}, err
	}
	return s.mutate(ctx, agentapi.OpActivate, agentapi.Params{
		Handle:   toplevel.Handle,
		Revision: state.Revision,
	})
}

// Move places the target window at an output position.
func (s *Service) Move(ctx context.Context, request MoveRequest) (agentapi.State, error) {
	toplevel, state, err := s.resolve(ctx, agentapi.OpMove, request.Target)
	if err != nil {
		return agentapi.State{}, err
	}
	if err := finiteCoordinate(agentapi.OpMove, "x", request.X); err != nil {
		return agentapi.State{}, err
	}
	if err := finiteCoordinate(agentapi.OpMove, "y", request.Y); err != nil {
		return agentapi.State{}, err
	}
	return s.mutate(ctx, agentapi.OpMove, agentapi.Params{
		Handle:   toplevel.Handle,
		Revision: state.Revision,
		X:        request.X,
		Y:        request.Y,
	})
}

// Resize asks the target window for a size and reports what became visible.
func (s *Service) Resize(ctx context.Context, request ResizeRequest) (agentapi.ResizeResult, error) {
	if request.Width == 0 || request.Height == 0 {
		failure := usageFailure(agentapi.OpResize, "a resize needs a nonzero width and height")
		failure.Details = map[string]string{
			"width":  strconv.FormatUint(uint64(request.Width), 10),
			"height": strconv.FormatUint(uint64(request.Height), 10),
		}
		return agentapi.ResizeResult{}, failure
	}
	toplevel, state, err := s.resolve(ctx, agentapi.OpResize, request.Target)
	if err != nil {
		return agentapi.ResizeResult{}, err
	}
	reply, err := s.caller.Call(ctx, agentapi.OpResize, agentapi.Params{
		Handle:    toplevel.Handle,
		Revision:  state.Revision,
		Width:     request.Width,
		Height:    request.Height,
		TimeoutMS: request.TimeoutMS,
	})
	if err != nil {
		return agentapi.ResizeResult{}, err
	}
	if reply.Resize == nil {
		return agentapi.ResizeResult{}, result.NewFailure(result.CodeSessionNotReady, agentapi.OpResize,
			"the controller answered resize without a result")
	}
	return *reply.Resize, nil
}

// CloseWindow asks the target window to close.
func (s *Service) CloseWindow(ctx context.Context, target Target) (agentapi.State, error) {
	toplevel, state, err := s.resolve(ctx, agentapi.OpCloseWindow, target)
	if err != nil {
		return agentapi.State{}, err
	}
	return s.mutate(ctx, agentapi.OpCloseWindow, agentapi.Params{
		Handle:   toplevel.Handle,
		Revision: state.Revision,
	})
}

// Click clicks inside the target window's content.
//
// The position is window-content-relative and is converted to the output
// coordinates the controller injects pointer motion in. A click is exactly a
// motion, a button press and a button release: the motion happens before the
// press so the button goes down where the caller asked, and the release is
// attempted even when the press or the caller's context fails, so a failed
// click cannot leave the button held.
func (s *Service) Click(ctx context.Context, request ClickRequest) (ClickResult, error) {
	toplevel, state, err := s.resolve(ctx, agentapi.OpPointer, request.Target)
	if err != nil {
		return ClickResult{}, err
	}

	button := request.Button
	if button == 0 {
		button = defaultButton
	}
	outputX, outputY, err := contentPoint(toplevel, request.X, request.Y)
	if err != nil {
		return ClickResult{}, err
	}

	if _, err := s.caller.Call(ctx, agentapi.OpPointer, agentapi.Params{
		Kind: agentapi.PointerMotion,
		X:    outputX,
		Y:    outputY,
	}); err != nil {
		return ClickResult{}, err
	}

	_, pressErr := s.caller.Call(ctx, agentapi.OpPointer, agentapi.Params{
		Kind:   agentapi.PointerButton,
		Button: button,
		State:  agentapi.StatePressed,
	})
	releaseErr := s.releaseButton(button)
	if pressErr != nil {
		return ClickResult{}, pressErr
	}
	if releaseErr != nil {
		return ClickResult{}, releaseErr
	}

	return ClickResult{
		Handle:   toplevel.Handle,
		Button:   button,
		OutputX:  outputX,
		OutputY:  outputY,
		Revision: state.Revision,
		State:    state,
	}, nil
}

// Pointer injects one raw pointer action.
func (s *Service) Pointer(ctx context.Context, request PointerRequest) error {
	params, err := pointerParams(request)
	if err != nil {
		return err
	}
	_, err = s.caller.Call(ctx, agentapi.OpPointer, params)
	return err
}

// Scroll scrolls on one axis.
func (s *Service) Scroll(ctx context.Context, request ScrollRequest) error {
	if request.Axis != AxisVertical && request.Axis != AxisHorizontal {
		failure := usageFailure(agentapi.OpPointer,
			"scroll axis %d is neither vertical (0) nor horizontal (1)", request.Axis)
		failure.Details = map[string]string{"axis": strconv.FormatUint(uint64(request.Axis), 10)}
		return failure
	}
	if err := finiteCoordinate(agentapi.OpPointer, "value", request.Value); err != nil {
		return err
	}
	_, err := s.caller.Call(ctx, agentapi.OpPointer, agentapi.Params{
		Kind:  agentapi.PointerAxis,
		Axis:  request.Axis,
		Value: request.Value,
	})
	return err
}

// Key injects one key transition. Exactly one of the request's Keycode and
// Name names the key; a name resolves through the embedded layout artifact.
func (s *Service) Key(ctx context.Context, request KeyRequest) error {
	keycode, err := keyTransition(request)
	if err != nil {
		return err
	}
	_, err = s.caller.Call(ctx, agentapi.OpKey, agentapi.Params{
		Key:   keycode,
		State: request.State,
	})
	return err
}

// Tap presses and releases one key.
//
// It exists so a caller that wants "press Return" does not have to pair two
// commands itself: the service owns the pairing, and a release that fails or
// a cancellation after the press still attempts the release on an
// independent short context, exactly like Click. The keycode is resolved
// through the same rules as Key (keycode XOR name), and the state field of
// the request is ignored.
//
// The release is attempted even when the press call itself failed: a press
// whose reply was lost -- a cancelled or timed-out container exec -- is
// indistinguishable from one that never arrived, and a key left held outlives
// the request that pressed it.
func (s *Service) Tap(ctx context.Context, request KeyRequest) error {
	keycode, err := keyIdentity(request)
	if err != nil {
		return err
	}
	_, pressErr := s.caller.Call(ctx, agentapi.OpKey, agentapi.Params{
		Key:   keycode,
		State: agentapi.StatePressed,
	})
	releaseErr := s.releaseKey(keycode)
	if pressErr != nil {
		return pressErr
	}
	return releaseErr
}

// Type types text with the embedded US layout.
//
// The whole text is resolved first, so a text with a character the layout
// cannot type sends nothing. Each stroke then presses shift only when the
// stroke needs it and it is not already down, presses the key, releases the
// key and releases shift. On any failure every key and modifier this call
// pressed is released on an independent context, and the original failure is
// returned.
func (s *Service) Type(ctx context.Context, request TypeRequest) (TypeResult, error) {
	if request.Text == "" {
		return TypeResult{}, usageFailure(agentapi.OpKey, "text to type is empty")
	}
	strokes, err := Sequence(request.Text)
	if err != nil {
		return TypeResult{}, err
	}
	shift, err := Named(shiftName)
	if err != nil {
		return TypeResult{}, err
	}

	var (
		shiftDown bool
		pressed   []uint32
	)
	for _, stroke := range strokes {
		if stroke.Shift && !shiftDown {
			// The press is recorded before it is sent: an answer that is lost
			// may still have left shift held, so cleanup covers it too.
			shiftDown = true
			if err := s.sendKey(ctx, shift.Keycode, agentapi.StatePressed); err != nil {
				s.releaseKeys(shiftDown, pressed)
				return TypeResult{}, err
			}
		}
		pressed = append(pressed, stroke.Keycode)
		if err := s.sendKey(ctx, stroke.Keycode, agentapi.StatePressed); err != nil {
			s.releaseKeys(shiftDown, pressed)
			return TypeResult{}, err
		}
		if err := s.sendKey(ctx, stroke.Keycode, agentapi.StateReleased); err != nil {
			// The release may have been delivered although its answer was
			// lost, so the cleanup release below is a deliberate retry of an
			// ambiguous transition, not an unbalanced release.
			s.releaseKeys(shiftDown, pressed)
			return TypeResult{}, err
		}
		pressed = pressed[:len(pressed)-1]
		if stroke.Shift && shiftDown {
			if err := s.sendKey(ctx, shift.Keycode, agentapi.StateReleased); err != nil {
				// Same as the key release above: the shift release may already
				// have reached the controller, so its retry is deliberate.
				s.releaseKeys(shiftDown, pressed)
				return TypeResult{}, err
			}
			shiftDown = false
		}
	}
	return TypeResult{Text: request.Text, Strokes: len(strokes)}, nil
}

// snapshot reads the session's windows. Every operation that names a window
// starts here, so the revision and the geometry it acts on come from one
// reading of the session.
func (s *Service) snapshot(ctx context.Context) (agentapi.State, error) {
	reply, err := s.caller.Call(ctx, agentapi.OpSnapshot, agentapi.Params{})
	if err != nil {
		return agentapi.State{}, err
	}
	if reply.State == nil {
		return agentapi.State{}, result.NewFailure(result.CodeSessionNotReady, agentapi.OpSnapshot,
			"the controller answered a snapshot without state")
	}
	return *reply.State, nil
}

// resolve reads a fresh snapshot, finds the target window in it and checks the
// caller's revision against it. It returns the window, the state whose
// revision a mutation must carry, and the caller's failure when the target
// cannot be acted on.
func (s *Service) resolve(ctx context.Context, operation string, target Target) (agentapi.Toplevel, agentapi.State, error) {
	if target.Handle == "" {
		return agentapi.Toplevel{}, agentapi.State{},
			usageFailure(operation, "a window handle is required")
	}
	state, err := s.snapshot(ctx)
	if err != nil {
		return agentapi.Toplevel{}, agentapi.State{}, err
	}
	toplevel, ok := findToplevel(state, target.Handle)
	if !ok {
		failure := result.NewFailure(result.CodeWindowNotFound, operation,
			"no window %q at revision %d", target.Handle, state.Revision)
		failure.Details = map[string]string{"handle": target.Handle}
		return agentapi.Toplevel{}, agentapi.State{}, failure
	}
	if target.Revision != 0 && target.Revision != state.Revision {
		failure := result.NewFailure(result.CodeStaleRevision, operation,
			"window %q is at revision %d, not %d", target.Handle, state.Revision, target.Revision)
		failure.Details = map[string]string{
			"requested": strconv.FormatUint(target.Revision, 10),
			"current":   strconv.FormatUint(state.Revision, 10),
		}
		return agentapi.Toplevel{}, agentapi.State{}, failure
	}
	return toplevel, state, nil
}

// mutate sends a window operation and returns the state it produced.
func (s *Service) mutate(ctx context.Context, operation string, params agentapi.Params) (agentapi.State, error) {
	reply, err := s.caller.Call(ctx, operation, params)
	if err != nil {
		return agentapi.State{}, err
	}
	if reply.State == nil {
		return agentapi.State{}, result.NewFailure(result.CodeSessionNotReady, operation,
			"the controller answered %s without state", operation)
	}
	return *reply.State, nil
}

// sendKey sends one key transition.
func (s *Service) sendKey(ctx context.Context, keycode uint32, state string) error {
	_, err := s.caller.Call(ctx, agentapi.OpKey, agentapi.Params{Key: keycode, State: state})
	return err
}

// releaseButton releases a pointer button on an independent context, so a
// failed or cancelled click cannot leave the button held.
func (s *Service) releaseButton(button uint32) error {
	ctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
	defer cancel()
	_, err := s.caller.Call(ctx, agentapi.OpPointer, agentapi.Params{
		Kind:   agentapi.PointerButton,
		Button: button,
		State:  agentapi.StateReleased,
	})
	return err
}

// releaseKey releases one key on an independent context, so a failed or
// cancelled tap cannot leave the key held.
func (s *Service) releaseKey(keycode uint32) error {
	ctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
	defer cancel()
	_, err := s.caller.Call(ctx, agentapi.OpKey, agentapi.Params{
		Key:   keycode,
		State: agentapi.StateReleased,
	})
	return err
}

// releaseKeys releases every key and modifier this call pressed, the modifier
// first and the keys in reverse order, on an independent context so a
// cancelled request still cleans up. It is best effort: the original failure
// is what the caller must see.
func (s *Service) releaseKeys(shiftDown bool, pressed []uint32) {
	ctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
	defer cancel()
	if shiftDown {
		if shift, err := Named(shiftName); err == nil {
			_, _ = s.caller.Call(ctx, agentapi.OpKey, agentapi.Params{
				Key:   shift.Keycode,
				State: agentapi.StateReleased,
			})
		}
	}
	for i := len(pressed) - 1; i >= 0; i-- {
		_, _ = s.caller.Call(ctx, agentapi.OpKey, agentapi.Params{
			Key:   pressed[i],
			State: agentapi.StateReleased,
		})
	}
}

// findToplevel returns the window with the given handle.
func findToplevel(state agentapi.State, handle string) (agentapi.Toplevel, bool) {
	for _, toplevel := range state.Toplevels {
		if toplevel.Handle == handle {
			return toplevel, true
		}
	}
	return agentapi.Toplevel{}, false
}

// pointerParams turns one raw pointer request into the single flat operation
// the controller accepts. Exactly one of the motion, button and axis shapes
// may be set; a field belonging to another shape is a usage error rather than
// a silently ignored value.
func pointerParams(request PointerRequest) (agentapi.Params, error) {
	switch request.Kind {
	case agentapi.PointerMotion:
		if request.Button != 0 || request.State != "" || request.Axis != 0 || request.Value != 0 {
			return agentapi.Params{}, usageFailure(agentapi.OpPointer,
				"a motion pointer carries only a position")
		}
		if err := finiteCoordinate(agentapi.OpPointer, "x", request.X); err != nil {
			return agentapi.Params{}, err
		}
		if err := finiteCoordinate(agentapi.OpPointer, "y", request.Y); err != nil {
			return agentapi.Params{}, err
		}
		return agentapi.Params{Kind: agentapi.PointerMotion, X: request.X, Y: request.Y}, nil

	case agentapi.PointerButton:
		if request.X != 0 || request.Y != 0 || request.Axis != 0 || request.Value != 0 {
			return agentapi.Params{}, usageFailure(agentapi.OpPointer,
				"a button pointer carries only a button and a state")
		}
		if request.Button == 0 {
			return agentapi.Params{}, usageFailure(agentapi.OpPointer, "a button pointer needs a button")
		}
		if err := validState(agentapi.OpPointer, request.State); err != nil {
			return agentapi.Params{}, err
		}
		return agentapi.Params{
			Kind:   agentapi.PointerButton,
			Button: request.Button,
			State:  request.State,
		}, nil

	case agentapi.PointerAxis:
		if request.X != 0 || request.Y != 0 || request.Button != 0 || request.State != "" {
			return agentapi.Params{}, usageFailure(agentapi.OpPointer,
				"an axis pointer carries only an axis and a value")
		}
		if request.Axis != AxisVertical && request.Axis != AxisHorizontal {
			failure := usageFailure(agentapi.OpPointer,
				"scroll axis %d is neither vertical (0) nor horizontal (1)", request.Axis)
			failure.Details = map[string]string{"axis": strconv.FormatUint(uint64(request.Axis), 10)}
			return agentapi.Params{}, failure
		}
		if err := finiteCoordinate(agentapi.OpPointer, "value", request.Value); err != nil {
			return agentapi.Params{}, err
		}
		return agentapi.Params{
			Kind:  agentapi.PointerAxis,
			Axis:  request.Axis,
			Value: request.Value,
		}, nil

	default:
		return agentapi.Params{}, usageFailure(agentapi.OpPointer,
			"pointer kind %q is not %q, %q or %q",
			request.Kind, agentapi.PointerMotion, agentapi.PointerButton, agentapi.PointerAxis)
	}
}

// keyIdentity validates which key a request names and resolves it to one evdev
// keycode. Exactly one of Keycode and Name names the key; the state is not part
// of the identity and Key validates it separately.
func keyIdentity(request KeyRequest) (uint32, error) {
	hasKeycode := request.Keycode != 0
	hasName := request.Name != ""
	switch {
	case hasKeycode && hasName:
		return 0, usageFailure(agentapi.OpKey, "a key names either a keycode or a name, not both")
	case !hasKeycode && !hasName:
		return 0, usageFailure(agentapi.OpKey, "a key needs a keycode or a name")
	}
	if request.Keycode > keycodeMax {
		failure := usageFailure(agentapi.OpKey, "keycode %d is outside the evdev range %d..%d",
			request.Keycode, keycodeMin, keycodeMax)
		failure.Details = map[string]string{"keycode": strconv.FormatUint(uint64(request.Keycode), 10)}
		return 0, failure
	}
	if hasName {
		stroke, err := Named(request.Name)
		if err != nil {
			return 0, err
		}
		return stroke.Keycode, nil
	}
	return request.Keycode, nil
}

// keyTransition validates a key request and resolves it to one evdev keycode.
func keyTransition(request KeyRequest) (uint32, error) {
	keycode, err := keyIdentity(request)
	if err != nil {
		return 0, err
	}
	if err := validState(agentapi.OpKey, request.State); err != nil {
		return 0, err
	}
	return keycode, nil
}

// validState requires a key or button state to be one of the two the wire
// defines.
func validState(operation, state string) error {
	if state == agentapi.StatePressed || state == agentapi.StateReleased {
		return nil
	}
	failure := usageFailure(operation, "state %q is not %q or %q",
		state, agentapi.StatePressed, agentapi.StateReleased)
	failure.Details = map[string]string{"state": state}
	return failure
}

// contentPoint converts a window-content-relative point to the output
// coordinates the controller injects pointer motion in.
func contentPoint(toplevel agentapi.Toplevel, x, y float64) (float64, float64, error) {
	if err := contentAxis(toplevel, "x", x); err != nil {
		return 0, 0, err
	}
	if err := contentAxis(toplevel, "y", y); err != nil {
		return 0, 0, err
	}
	return float64(toplevel.X) + x, float64(toplevel.Y) + y, nil
}

// contentAxis bounds one window-content coordinate: it must be a finite number
// inside [0, size), and a value outside that range is a usage error naming the
// axis, the value and the window size.
func contentAxis(toplevel agentapi.Toplevel, axis string, value float64) error {
	size := float64(toplevel.Width)
	if axis == "y" {
		size = float64(toplevel.Height)
	}
	if finite(value) && value >= 0 && value < size {
		return nil
	}
	formatted := strconv.FormatFloat(value, 'g', -1, 64)
	failure := usageFailure(agentapi.OpPointer,
		"the %s coordinate %s is outside the %dx%d window %q",
		axis, formatted, toplevel.Width, toplevel.Height, toplevel.Handle)
	failure.Details = map[string]string{
		"axis":  axis,
		"value": formatted,
		"size":  fmt.Sprintf("%dx%d", toplevel.Width, toplevel.Height),
	}
	return failure
}

// finiteCoordinate rejects a non-finite value: JSON cannot carry NaN or an
// infinity, so a caller mistake must be a usage error here rather than an
// encoding failure later.
func finiteCoordinate(operation, axis string, value float64) error {
	if finite(value) {
		return nil
	}
	failure := usageFailure(operation, "%s is not a finite number", axis)
	failure.Details = map[string]string{
		"axis":  axis,
		"value": strconv.FormatFloat(value, 'g', -1, 64),
	}
	return failure
}

// finite reports whether a value can cross the wire.
func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

// usageFailure builds a usage error for one operation.
func usageFailure(operation, format string, args ...any) *result.Failure {
	return result.NewFailure(result.CodeUsageError, operation, format, args...)
}
