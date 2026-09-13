// Package agentapi is the wire contract between the short-lived wlvision-call
// process and the resident controller.
//
// One request and one reply are JSON documents. The contract lives here, rather
// than in either binary, because the two sides are versioned together: a
// session's container and the CLI that drives it must agree on the operation
// names and argument fields, and neither may drift from the other.
package agentapi

import "encoding/json"

// Operations the resident controller serves. They mirror the control facade.
const (
	OpSnapshot         = "snapshot"
	OpActivate         = "activate"
	OpMove             = "move"
	OpResize           = "resize"
	OpCloseWindow      = "close_window"
	OpPointer          = "pointer"
	OpKey              = "key"
	OpAuthorizeCapture = "authorize_capture"
	OpCapture          = "capture"
)

// Pointer kinds the pointer operation accepts. Exactly one action is built.
const (
	PointerMotion = "motion"
	PointerButton = "button"
	PointerAxis   = "axis"
)

// Key and button states.
const (
	StatePressed  = "pressed"
	StateReleased = "released"
)

// Request is one operation and its arguments.
type Request struct {
	Operation string          `json:"operation"`
	Params    json.RawMessage `json:"params,omitempty"`
}

// Params are the arguments of an operation in one flat shape.
//
// A single shape keeps the wire readable and versionable: the CLI turns these
// fields into typed flags, and the controller turns them into typed calls.
type Params struct {
	Handle   string  `json:"handle,omitempty"`
	Revision uint64  `json:"revision,omitempty"`
	X        float64 `json:"x,omitempty"`
	Y        float64 `json:"y,omitempty"`
	Width    uint32  `json:"width,omitempty"`
	Height   uint32  `json:"height,omitempty"`
	Kind     string  `json:"kind,omitempty"`
	Button   uint32  `json:"button,omitempty"`
	State    string  `json:"state,omitempty"`
	Axis     uint32  `json:"axis,omitempty"`
	Value    float64 `json:"value,omitempty"`
	Key      uint32  `json:"key,omitempty"`
	// Path is where a capture is written, relative to the session's export
	// directory. An absolute path or a traversing one is refused.
	Path string `json:"path,omitempty"`
}

// Reply is the result of one operation. At most one payload field is set.
type Reply struct {
	Revision         uint64        `json:"revision,omitempty"`
	State            *State        `json:"state,omitempty"`
	Resize           *ResizeResult `json:"resize,omitempty"`
	Frame            *FrameResult  `json:"frame,omitempty"`
	CaptureRequestID uint64        `json:"capture_request_id,omitempty"`
}

// State is the session's windows at one revision, as the controller reports
// them. It mirrors the control facade without importing it, so a caller that
// only reads a reply does not depend on the transport.
type State struct {
	Revision  uint64     `json:"revision"`
	Toplevels []Toplevel `json:"toplevels,omitempty"`
}

// Toplevel is one window.
type Toplevel struct {
	Handle   string `json:"handle"`
	Title    string `json:"title,omitempty"`
	AppID    string `json:"app_id,omitempty"`
	X        int32  `json:"x"`
	Y        int32  `json:"y"`
	Width    uint32 `json:"width"`
	Height   uint32 `json:"height"`
	State    uint32 `json:"state"`
	Revision uint64 `json:"revision"`
}

// ResizeResult reports a completed configure-to-commit resize.
type ResizeResult struct {
	Handle          string `json:"handle"`
	RequestedWidth  uint32 `json:"requested_width"`
	RequestedHeight uint32 `json:"requested_height"`
	VisibleWidth    uint32 `json:"visible_width"`
	VisibleHeight   uint32 `json:"visible_height"`
	Revision        uint64 `json:"revision"`
}

// FrameResult describes one stored capture.
type FrameResult struct {
	Path             string `json:"path"`
	Digest           string `json:"digest"`
	Width            int    `json:"width"`
	Height           int    `json:"height"`
	Format           string `json:"format"`
	Sequence         uint64 `json:"frame_sequence"`
	CaptureRequestID uint64 `json:"capture_request_id,omitempty"`
}
