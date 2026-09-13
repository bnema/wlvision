package control

import (
	"fmt"
	"time"
)

// Handle is a session-local, opaque window identifier such as "app-1".
//
// Handles are assigned by the compositor module. They are never Wayland object
// IDs, they are never reused after a window is gone, and they mean nothing
// outside the session that produced them.
type Handle string

// Revision is the layout revision of a session. It increases whenever an
// observable window or layout change happens.
//
// The wire format has no 64-bit integer, so the protocol carries it as two
// 32-bit words.
type Revision uint64

// RevisionWords splits a revision into the two words the protocol carries.
func RevisionWords(r Revision) (hi, lo uint32) {
	return uint32(uint64(r) >> 32), uint32(uint64(r))
}

// RevisionFromWords joins the two words the protocol carries.
func RevisionFromWords(hi, lo uint32) Revision {
	return Revision(uint64(hi)<<32 | uint64(lo))
}

// Size is a width and height in logical pixels.
type Size struct {
	Width  uint32
	Height uint32
}

// String renders a size for diagnostics.
func (s Size) String() string {
	return fmt.Sprintf("%dx%d", s.Width, s.Height)
}

// Toplevel is one window as the compositor reported it.
type Toplevel struct {
	Handle   Handle
	Title    string
	AppID    string
	X        int32
	Y        int32
	Size     Size
	State    uint32
	Revision Revision
}

// State is the session's windows at one revision.
type State struct {
	Revision  Revision
	Toplevels []Toplevel
}

// Find returns the window with the given handle.
func (s State) Find(handle Handle) (Toplevel, bool) {
	for _, toplevel := range s.Toplevels {
		if toplevel.Handle == handle {
			return toplevel, true
		}
	}
	return Toplevel{}, false
}

// ResizeResult is the outcome of a resize request, with each size kept apart
// because each is observed at a different moment and none implies the others.
//
//   - Requested is what the caller asked for.
//   - Configured is the size the module actually configured the application
//     with. The module completes the request against this size rather than
//     against Requested, so a module that clamps a configure still completes
//     on the configure the application was sent.
//   - Committed is the content size of the buffer the application committed.
//     The request only completes once it matches Configured.
//   - Visible is the toplevel geometry the module reports after the commit,
//     the same size emit_toplevel_changed publishes.
//
// Revision is the session revision after the resize. A requested size is not a
// result: sending a configure proves nothing, so a successful result means the
// application committed the configured size.
type ResizeResult struct {
	Handle     Handle
	Requested  Size
	Configured Size
	Committed  Size
	Visible    Size
	Revision   Revision
}

// Point is an absolute pointer position in logical pixels of the output.
type Point struct {
	X float64
	Y float64
}

// ButtonEvent is a pointer button transition.
type ButtonEvent struct {
	Button  uint32
	Pressed bool
}

// Axis identifies a scroll axis.
type Axis uint32

// Scroll axes, matching wl_pointer.
const (
	AxisVertical Axis = iota
	AxisHorizontal
)

// AxisEvent is a scroll step.
type AxisEvent struct {
	Axis  Axis
	Value float64
}

// PointerEvent is exactly one pointer action: motion, a button transition or a
// scroll step. Exactly one field is set.
type PointerEvent struct {
	Motion *Point
	Button *ButtonEvent
	Axis   *AxisEvent
}

// KeyEvent is a keyboard key transition. Keys are evdev keycodes, which is what
// the compositor seat consumes.
type KeyEvent struct {
	Key     uint32
	Pressed bool
}

// Frame is a completed repaint the module observed.
//
// CaptureRequestID is the authorization request whose capture forced this
// repaint, and is zero for frames that arrived on their own. Sequence increases
// monotonically for the life of the session, so a caller can tell a fresh frame
// from one it already saw.
type Frame struct {
	CaptureRequestID uint64
	Sequence         uint64
	Timestamp        time.Time
}

// ErrorCode is a stable failure code from the protocol's error enum.
type ErrorCode uint32

// Failure codes. Their numbers are part of the protocol contract.
const (
	CodeStaleRevision      ErrorCode = 0
	CodeWindowNotFound     ErrorCode = 1
	CodeNotAuthorized      ErrorCode = 2
	CodeCaptureUnavailable ErrorCode = 3
	CodeInvalidArgument    ErrorCode = 4
)

// String names a failure code.
func (c ErrorCode) String() string {
	switch c {
	case CodeStaleRevision:
		return "stale_revision"
	case CodeWindowNotFound:
		return "window_not_found"
	case CodeNotAuthorized:
		return "not_authorized"
	case CodeCaptureUnavailable:
		return "capture_unavailable"
	case CodeInvalidArgument:
		return "invalid_argument"
	default:
		return fmt.Sprintf("unknown_error_%d", uint32(c))
	}
}

// Error makes a failure code usable as an error, so a RequestError can unwrap
// to it and callers can test with errors.Is.
func (c ErrorCode) Error() string { return c.String() }

// RequestError is a failure the compositor module reported for one request.
type RequestError struct {
	Code    ErrorCode
	Message string
}

func (e *RequestError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("wlvision control: %s", e.Code)
	}
	return fmt.Sprintf("wlvision control: %s: %s", e.Code, e.Message)
}

// Unwrap exposes the code as a sentinel so callers can test for it with
// errors.Is(err, ErrStaleRevision) and friends.
func (e *RequestError) Unwrap() error { return e.Code }

// Sentinel errors for the stable failure codes.
var (
	ErrStaleRevision      = CodeStaleRevision
	ErrWindowNotFound     = CodeWindowNotFound
	ErrNotAuthorized      = CodeNotAuthorized
	ErrCaptureUnavailable = CodeCaptureUnavailable
	ErrInvalidArgument    = CodeInvalidArgument
)
