// Command wlvision-agent is the resident controller of a wlvision session.
//
// It owns the session's only privileged connection: it binds the private
// control protocol, creates the session's single controller, attaches the
// capture path to that same connection, and serves the operations the outer CLI
// asks for through a short-lived `wlvision-call` process.
//
// The agent exists so that the privileged surface is a long-lived process with
// one connection, one revision counter, and one capture source. Requests arrive
// as one JSON document each, are answered with one JSON document each, and only
// the control UID can open the socket. The module checks those same credentials
// before it exposes anything, so the socket's mode is not the boundary.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bnema/wlvision/internal/agentapi"
	"github.com/bnema/wlvision/internal/capture"
	"github.com/bnema/wlvision/internal/control"
	"github.com/bnema/wlvision/internal/result"
	"github.com/bnema/wlvision/internal/rpc"
	"github.com/bnema/wlvision/internal/session"

	"github.com/bnema/wlturbo/wl"
)

// The wire contract lives in internal/agentapi because the short-lived caller
// shares it; these aliases keep the operation code reading as it should.
const (
	OpSnapshot         = agentapi.OpSnapshot
	OpActivate         = agentapi.OpActivate
	OpMove             = agentapi.OpMove
	OpResize           = agentapi.OpResize
	OpCloseWindow      = agentapi.OpCloseWindow
	OpPointer          = agentapi.OpPointer
	OpKey              = agentapi.OpKey
	OpAuthorizeCapture = agentapi.OpAuthorizeCapture
	OpCapture          = agentapi.OpCapture
)

// Pointer kinds the pointer operation accepts.
const (
	PointerMotion = agentapi.PointerMotion
	PointerButton = agentapi.PointerButton
	PointerAxis   = agentapi.PointerAxis
)

// The request and reply shapes are the shared ones.
type (
	Request     = agentapi.Request
	Params      = agentapi.Params
	Reply       = agentapi.Reply
	FrameResult = agentapi.FrameResult
)

// readyMarkerName is the marker the supervisor waits for. It is written only
// once the control plane and the capture path are both usable.
const readyMarkerName = "agent.ready"

// DefaultSetupTimeout bounds how long the compositor may take to announce the
// capture source's size and format.
const DefaultSetupTimeout = 30 * time.Second

// DefaultOperationTimeout bounds one request the agent serves. Every request
// gets a finite budget: without one, a resize the application never commits
// would hold the resident controller forever and no later operation could be
// answered.
const DefaultOperationTimeout = 15 * time.Second

// MinOperationTimeoutMS is the smallest budget a caller may ask for. A shorter
// one cannot cover a compositor round trip and would only produce noise.
const MinOperationTimeoutMS = 100

// Codes of the control refusal contract, as the result contract declares them.
// They are named here so a refused request reaches the caller with the code the
// approved error model requires instead of only inside the message text.
const (
	codeNotAuthorized      = result.CodeNotAuthorized
	codeCaptureUnavailable = result.CodeCaptureUnavailable
)

// controlUIDEnv carries the identity the module accepts on the control global.
// It is the same key the engine adapter sets when it creates the session
// container; it is repeated here because the adapter and this binary sit on
// opposite sides of the container boundary and must not import each other.
const controlUIDEnv = "WLVISION_CONTROL_UID"

// Controller is the privileged control surface the agent serves. control.Client
// satisfies it; the interface is narrow so the operations can be exercised
// without a compositor.
type Controller interface {
	Snapshot(ctx context.Context) (control.State, error)
	Activate(ctx context.Context, handle control.Handle, revision control.Revision) (control.State, error)
	Move(ctx context.Context, handle control.Handle, revision control.Revision, at control.Point) (control.State, error)
	Resize(ctx context.Context, handle control.Handle, size control.Size, revision control.Revision) (control.ResizeResult, error)
	CloseWindow(ctx context.Context, handle control.Handle, revision control.Revision) (control.State, error)
	Pointer(ctx context.Context, event control.PointerEvent) error
	Key(ctx context.Context, event control.KeyEvent) error
	AuthorizeCapture(ctx context.Context) (uint64, error)
	Revision() control.Revision
	State() control.State
	Frames() <-chan control.Frame
	// Context and Registry expose the connection the module authorized, which
	// is where the capture path binds.
	Context() *wl.Context
	Registry() *wl.Registry
}

// CapturePlane is the capture path attached to the controller's connection.
type CapturePlane interface {
	WaitForSetup(ctx context.Context) (capture.Size, capture.Format, error)
	Capture(ctx context.Context) (capture.Frame, error)
	Close() error
}

// CaptureAttacher binds the capture path on the controller's own connection,
// which is the only connection the module authorizes for capture.
type CaptureAttacher func(ctx context.Context, controller Controller) (CapturePlane, error)

// Options configures the agent.
type Options struct {
	SocketPath   string
	ControlUID   uint32
	ControlDir   string
	ExportDir    string
	SetupTimeout time.Duration
}

// Agent serves one session's control plane.
type Agent struct {
	options    Options
	controller Controller
	attach     CaptureAttacher
	now        func() time.Time
	log        io.Writer

	plane CapturePlane
}

func newAgent(options Options, controller Controller, attach CaptureAttacher, log io.Writer, now func() time.Time) *Agent {
	if options.SocketPath == "" {
		options.SocketPath = session.AgentSocket
	}
	if options.ControlDir == "" {
		options.ControlDir = session.ControlDir
	}
	if options.ExportDir == "" {
		options.ExportDir = session.ExportDir
	}
	if options.SetupTimeout <= 0 {
		options.SetupTimeout = DefaultSetupTimeout
	}
	if now == nil {
		now = time.Now
	}
	return &Agent{options: options, controller: controller, attach: attach, log: log, now: now}
}

// Serve announces readiness and then answers requests until the context is
// cancelled.
func (a *Agent) Serve(ctx context.Context) error {
	server, err := rpc.Listen(rpc.Config{Path: a.options.SocketPath, ExpectedUID: a.options.ControlUID})
	if err != nil {
		return err
	}
	defer func() { _ = server.Close() }()

	if a.attach != nil {
		plane, err := a.attach(ctx, a.controller)
		if err != nil {
			return fmt.Errorf("wlvision-agent: cannot attach the capture path: %w", err)
		}
		a.plane = plane
		defer func() { _ = plane.Close() }()

		setupCtx, cancel := context.WithTimeout(ctx, a.options.SetupTimeout)
		size, format, err := plane.WaitForSetup(setupCtx)
		cancel()
		if err != nil {
			return fmt.Errorf("wlvision-agent: the capture source never became usable: %w", err)
		}
		fmt.Fprintf(a.log, "controller ready: capture %s %s\n", size, format)
	}

	if err := a.markReady(); err != nil {
		return err
	}

	server.SetHandler(a.handle)
	return server.Serve(ctx)
}

// markReady publishes the marker the supervisor waits for.
func (a *Agent) markReady() error {
	path := filepath.Join(a.options.ControlDir, readyMarkerName)
	if err := os.MkdirAll(a.options.ControlDir, 0o700); err != nil {
		return fmt.Errorf("wlvision-agent: cannot create %s: %w", a.options.ControlDir, err)
	}
	if err := os.WriteFile(path, []byte(a.now().UTC().Format(time.RFC3339)+"\n"), 0o600); err != nil {
		return fmt.Errorf("wlvision-agent: cannot announce readiness: %w", err)
	}
	return nil
}

// handle executes one request. A failure travels back as a result failure, so
// the outer CLI reports the same code it would for any other operation.
func (a *Agent) handle(ctx context.Context, payload []byte) ([]byte, error) {
	var request Request
	if err := json.Unmarshal(payload, &request); err != nil {
		return nil, usageFailure("the request is not readable: %v", err)
	}

	var params Params
	if len(request.Params) > 0 {
		if err := json.Unmarshal(request.Params, &params); err != nil {
			return nil, usageFailure("the arguments of %s are not readable: %v", request.Operation, err)
		}
	}

	// Every request gets a finite budget. Without one, a resize the application
	// never commits would hold this resident controller forever.
	timeout, err := operationTimeout(params)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	switch operation := strings.ToLower(strings.TrimSpace(request.Operation)); operation {
	case OpSnapshot:
		state, err := a.controller.Snapshot(ctx)
		if err != nil {
			return nil, failure("window.snapshot", err)
		}
		return encode(Reply{Revision: uint64(state.Revision), State: wireState(state)})

	case OpActivate:
		state, err := a.controller.Activate(ctx, control.Handle(params.Handle), control.Revision(params.Revision))
		if err != nil {
			return nil, failure("window.activate", err)
		}
		return encode(Reply{Revision: uint64(state.Revision), State: wireState(state)})

	case OpMove:
		state, err := a.controller.Move(ctx, control.Handle(params.Handle), control.Revision(params.Revision),
			control.Point{X: params.X, Y: params.Y})
		if err != nil {
			return nil, failure("window.move", err)
		}
		return encode(Reply{Revision: uint64(state.Revision), State: wireState(state)})

	case OpResize:
		resized, err := a.controller.Resize(ctx, control.Handle(params.Handle),
			control.Size{Width: params.Width, Height: params.Height}, control.Revision(params.Revision))
		if err != nil {
			return nil, failure("window.resize", err)
		}
		return encode(Reply{Revision: uint64(resized.Revision), Resize: wireResize(resized)})

	case OpCloseWindow:
		state, err := a.controller.CloseWindow(ctx, control.Handle(params.Handle), control.Revision(params.Revision))
		if err != nil {
			return nil, failure("window.close", err)
		}
		return encode(Reply{Revision: uint64(state.Revision), State: wireState(state)})

	case OpPointer:
		event, err := pointerEvent(params)
		if err != nil {
			return nil, err
		}
		if err := a.controller.Pointer(ctx, event); err != nil {
			return nil, failure("input.pointer", err)
		}
		return encode(Reply{Revision: uint64(a.controller.Revision())})

	case OpKey:
		if params.State != "pressed" && params.State != "released" {
			return nil, usageFailure("the key state %q is neither pressed nor released", params.State)
		}
		event := control.KeyEvent{Key: params.Key, Pressed: params.State == "pressed"}
		if err := a.controller.Key(ctx, event); err != nil {
			return nil, failure("input.key", err)
		}
		return encode(Reply{Revision: uint64(a.controller.Revision())})

	case OpAuthorizeCapture:
		requestID, err := a.controller.AuthorizeCapture(ctx)
		if err != nil {
			return nil, failure("capture.authorize", err)
		}
		return encode(Reply{Revision: uint64(a.controller.Revision()), CaptureRequestID: requestID})

	case OpCapture:
		return a.capture(ctx, params)

	default:
		return nil, usageFailure("unknown operation %q", request.Operation)
	}
}

// capture authorizes the connection, captures one frame, stores it, and reports
// what it stored.
func (a *Agent) capture(ctx context.Context, params Params) ([]byte, error) {
	target, err := a.exportPath(params.Path)
	if err != nil {
		return nil, err
	}

	if a.plane == nil {
		return nil, result.NewFailure(result.CodeSessionNotReady, "capture.screenshot",
			"the session has no capture source")
	}

	// The module authorizes the connection that captures, so the authorization
	// happens here, on this connection, and its identifier travels back with
	// the frame that it forced.
	requestID, err := a.controller.AuthorizeCapture(ctx)
	if err != nil {
		return nil, failure("capture.screenshot", err)
	}

	frame, err := a.plane.Capture(ctx)
	if err != nil {
		return nil, failure("capture.screenshot", err)
	}

	if err := writeFile(target, frame.PNG); err != nil {
		return nil, failure("capture.screenshot", err)
	}

	digest := sha256.Sum256(frame.PNG)
	reply := Reply{
		Revision: uint64(a.controller.Revision()),
		Frame: &FrameResult{
			Path:             target,
			Digest:           "sha256:" + hex.EncodeToString(digest[:]),
			Width:            frame.Size.Width,
			Height:           frame.Size.Height,
			Format:           frame.Format.String(),
			CaptureRequestID: requestID,
		},
	}

	// The module numbers the repaint the capture forced. A caller that needs
	// the sequence reads it from the frame the module reported for this
	// authorization; when it has not arrived yet the fields stay zero instead
	// of being guessed.
	select {
	case observed := <-a.controller.Frames():
		reply.Frame.Sequence = observed.Sequence
		if observed.CaptureRequestID != 0 {
			reply.Frame.CaptureRequestID = observed.CaptureRequestID
		}
	default:
	}

	return encode(reply)
}

// exportPath resolves a capture destination inside the session's export
// directory. Only relative paths are accepted, so a caller cannot write
// anywhere the session does not own.
func (a *Agent) exportPath(requested string) (string, error) {
	if requested == "" {
		return "", usageFailure("a capture path is required")
	}
	if filepath.IsAbs(requested) {
		return "", usageFailure("the capture path %q must be relative", requested)
	}

	cleaned := filepath.Clean(requested)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", usageFailure("the capture path %q leaves the export directory", requested)
	}

	root, err := filepath.Abs(a.options.ExportDir)
	if err != nil {
		return "", failure("capture.screenshot", err)
	}
	target := filepath.Join(root, cleaned)
	if !strings.HasPrefix(target, root+string(filepath.Separator)) {
		return "", usageFailure("the capture path %q leaves the export directory", requested)
	}
	return target, nil
}

// pointerEvent builds exactly one pointer action.
func pointerEvent(params Params) (control.PointerEvent, error) {
	switch params.Kind {
	case PointerMotion:
		return control.PointerEvent{Motion: &control.Point{X: params.X, Y: params.Y}}, nil
	case PointerButton:
		if params.State != "pressed" && params.State != "released" {
			return control.PointerEvent{}, usageFailure("the button state %q is neither pressed nor released", params.State)
		}
		return control.PointerEvent{Button: &control.ButtonEvent{Button: params.Button, Pressed: params.State == "pressed"}}, nil
	case PointerAxis:
		return control.PointerEvent{Axis: &control.AxisEvent{Axis: control.Axis(params.Axis), Value: params.Value}}, nil
	default:
		return control.PointerEvent{}, usageFailure("the pointer kind %q is not motion, button or axis", params.Kind)
	}
}

// writeFile stores a capture, creating its directory if the caller asked for a
// nested path.
func writeFile(path string, payload []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, payload, 0o600)
}

// wireState renders the facade's window state as the wire shape.
func wireState(state control.State) *agentapi.State {
	wire := &agentapi.State{Revision: uint64(state.Revision)}
	for _, toplevel := range state.Toplevels {
		wire.Toplevels = append(wire.Toplevels, agentapi.Toplevel{
			Handle:   string(toplevel.Handle),
			Title:    toplevel.Title,
			AppID:    toplevel.AppID,
			X:        toplevel.X,
			Y:        toplevel.Y,
			Width:    toplevel.Size.Width,
			Height:   toplevel.Size.Height,
			State:    toplevel.State,
			Revision: uint64(toplevel.Revision),
		})
	}
	return wire
}

// wireResize renders a completed resize as the wire shape. Every size is
// copied from the facade; none is derived, so a size the controller did not
// report stays absent instead of becoming zero.
func wireResize(resized control.ResizeResult) *agentapi.ResizeResult {
	return &agentapi.ResizeResult{
		Handle:           string(resized.Handle),
		RequestedWidth:   resized.Requested.Width,
		RequestedHeight:  resized.Requested.Height,
		ConfiguredWidth:  resized.Configured.Width,
		ConfiguredHeight: resized.Configured.Height,
		CommittedWidth:   resized.Committed.Width,
		CommittedHeight:  resized.Committed.Height,
		VisibleWidth:     resized.Visible.Width,
		VisibleHeight:    resized.Visible.Height,
		Revision:         uint64(resized.Revision),
	}
}

// encode renders one reply.
func encode(reply Reply) ([]byte, error) {
	payload, err := json.Marshal(reply)
	if err != nil {
		return nil, failure("agent.encode", err)
	}
	return payload, nil
}

// operationTimeout derives one request's budget. DefaultOperationTimeout
// applies unless the caller asked for less; a budget below the floor is a
// usage error rather than a request that cannot succeed.
func operationTimeout(params Params) (time.Duration, error) {
	if params.TimeoutMS == 0 {
		return DefaultOperationTimeout, nil
	}
	if params.TimeoutMS < MinOperationTimeoutMS {
		return 0, usageFailure("timeout_ms=%d is below the %dms floor",
			params.TimeoutMS, MinOperationTimeoutMS)
	}

	timeout := time.Duration(params.TimeoutMS) * time.Millisecond
	if timeout > DefaultOperationTimeout {
		timeout = DefaultOperationTimeout
	}
	return timeout, nil
}

func usageFailure(format string, args ...any) error {
	return result.NewFailure(result.CodeUsageError, "agent", format, args...)
}

// failure reports what happened, keeping the code the control layer assigned
// when it has one.
func failure(operation string, err error) error {
	var typed *result.Failure
	if errors.As(err, &typed) {
		return typed
	}

	if refused, ok := refusalFailure(operation, err); ok {
		return refused
	}

	code := result.CodeSessionNotReady
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		code = result.CodeWaitTimeout
	case errors.Is(err, capture.ErrCaptureUnavailable):
		code = result.CodeCaptureFailed
	case errors.Is(err, control.ErrClosed):
		code = result.CodeSessionNotReady
	}
	return result.NewFailure(code, operation, "%v", err)
}

// refusalFailure maps a control protocol refusal onto the stable result code
// the approved error model names, so a refused window request reaches the
// caller with its own code rather than as an opaque session failure. A code
// with no mapping stays session_not_ready with the control code named in the
// message.
func refusalFailure(operation string, err error) (*result.Failure, bool) {
	var refusal *control.RequestError
	var sentinel control.ErrorCode

	switch {
	case errors.As(err, &refusal):
		if code, ok := refusalCode(refusal.Code); ok {
			return result.NewFailure(code, operation, "%v", err), true
		}
		return result.NewFailure(result.CodeSessionNotReady, operation,
			"%v (control code %s)", err, refusal.Code), true
	case errors.As(err, &sentinel):
		if code, ok := refusalCode(sentinel); ok {
			return result.NewFailure(code, operation, "%v", err), true
		}
		return result.NewFailure(result.CodeSessionNotReady, operation,
			"%v (control code %s)", err, sentinel), true
	}
	return nil, false
}

// refusalCode maps a control protocol error onto the result contract.
func refusalCode(code control.ErrorCode) (result.Code, bool) {
	switch code {
	case control.CodeWindowNotFound:
		return result.CodeWindowNotFound, true
	case control.CodeStaleRevision:
		return result.CodeStaleRevision, true
	case control.CodeNotAuthorized:
		return codeNotAuthorized, true
	case control.CodeCaptureUnavailable:
		return codeCaptureUnavailable, true
	default:
		return "", false
	}
}

// compositorAttacher binds the capture protocol on the controller's connection.
type compositorAttacher struct{}

// CapturePlane implements CaptureAttacher.
func (compositorAttacher) Attach(ctx context.Context, controller Controller) (CapturePlane, error) {
	registry := controller.Registry()
	if registry == nil {
		return nil, errors.New("the controller connection has no registry")
	}

	output, err := bindOutput(controller)
	if err != nil {
		return nil, err
	}

	captureObject, err := capture.BindCapture(controller.Context(), registry)
	if err != nil {
		return nil, err
	}

	source, err := capture.CreateSource(ctx, captureObject, output, capture.SourceFramebuffer)
	if err != nil {
		return nil, err
	}

	shm, err := capture.BindShm(controller.Context(), registry)
	if err != nil {
		_ = source.Close()
		return nil, err
	}

	return &compositorPlane{source: source, shm: shm}, nil
}

// compositorPlane is the capture path of one session.
type compositorPlane struct {
	source *capture.Source
	shm    *capture.Shm
}

func (p *compositorPlane) WaitForSetup(ctx context.Context) (capture.Size, capture.Format, error) {
	return p.source.WaitForSetup(ctx)
}

func (p *compositorPlane) Capture(ctx context.Context) (capture.Frame, error) {
	return p.source.Capture(ctx, p.shm)
}

func (p *compositorPlane) Close() error {
	sourceErr := p.source.Close()
	shmErr := p.shm.Close()
	if sourceErr != nil {
		return sourceErr
	}
	return shmErr
}

// bindOutput binds the session's output, which the capture path needs: the
// control protocol deliberately carries no outputs.
func bindOutput(controller Controller) (*wl.Output, error) {
	registry := controller.Registry()

	global, ok := registry.FindGlobal("wl_output")
	if !ok {
		return nil, errors.New("the compositor offers no wl_output")
	}

	output := &wl.Output{}
	id, err := registry.BindID(global.Name, "wl_output", global.Version)
	if err != nil {
		return nil, err
	}

	output.SetContext(controller.Context())
	output.SetID(id)
	controller.Context().Register(output)
	return output, nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	controller, err := control.Connect(ctx, "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "wlvision-agent: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = controller.Close() }()

	agent := newAgent(Options{
		SocketPath: session.AgentSocket,
		ControlUID: controlUIDFromEnv(),
		ControlDir: session.ControlDir,
		ExportDir:  session.ExportDir,
	}, controller, compositorAttacher{}.Attach, os.Stdout, time.Now)

	if err := agent.Serve(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "wlvision-agent: %v\n", err)
		os.Exit(1)
	}
}

// controlUIDFromEnv reads the only identity allowed to reach the control plane.
// The module refuses every bind when it is unset, so an unset value here means
// the session cannot be used at all.
func controlUIDFromEnv() uint32 {
	value := os.Getenv(controlUIDEnv)
	if value == "" {
		return 0
	}
	uid, err := strconv.ParseUint(value, 10, 32)
	if err != nil {
		return 0
	}
	return uint32(uid)
}
