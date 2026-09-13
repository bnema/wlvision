package controltest

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"testing"

	"github.com/bnema/wlturbo"
	"github.com/bnema/wlturbo/wl"

	"github.com/bnema/wlvision/internal/control/generated"
)

// startServer starts a fake compositor, points the environment at it and
// connects the real client bindings through wl.Connect("").
func startServer(t *testing.T) (*Server, *wl.Display) {
	t.Helper()

	s := Start(t)
	s.Env(t)

	display, err := wl.Connect("")
	if err != nil {
		t.Fatalf("connect to fake compositor: %v", err)
	}
	t.Cleanup(func() { _ = display.Close() })

	if err := display.Roundtrip(); err != nil {
		t.Fatalf("roundtrip the handshake: %v", err)
	}
	if err := s.Err(); err != nil {
		t.Fatalf("server error during handshake: %v", err)
	}
	return s, display
}

// mustGlobal returns the announced wlvision_control_v1 global.
func mustGlobal(t *testing.T, display *wl.Display) wl.Global {
	t.Helper()

	global, ok := display.Registry().FindGlobal(generated.WlvisionControlInterface)
	if !ok {
		t.Fatalf("registry did not announce %s", generated.WlvisionControlInterface)
	}
	return global
}

// bindControl binds the manager global through the registry and completes a
// roundtrip so the server has recorded the bind.
func bindControl(t *testing.T, display *wl.Display) *generated.WlvisionControl {
	t.Helper()

	global := mustGlobal(t, display)
	ctl := generated.NewWlvisionControl(display.Context())
	if err := display.Registry().Bind(global.Name, global.Interface, global.Version, ctl); err != nil {
		t.Fatalf("bind %s: %v", global.Interface, err)
	}
	if err := display.Roundtrip(); err != nil {
		t.Fatalf("roundtrip after bind: %v", err)
	}
	return ctl
}

// createController issues create_controller and verifies the recorded request
// carries the client-allocated controller ID as its new_id argument.
func createController(t *testing.T, s *Server, ctl *generated.WlvisionControl) (*generated.WlvisionController, uint32) {
	t.Helper()

	controller, err := ctl.CreateController()
	if err != nil {
		t.Fatalf("create_controller: %v", err)
	}

	req := s.WaitForRequest(t, generated.WlvisionControlInterface, 0)
	if req.Object != ctl.ID() {
		t.Fatalf("create_controller object = %d, want manager object %d", req.Object, ctl.ID())
	}
	if len(req.Body) != 4 {
		t.Fatalf("create_controller body = % x, want one new_id word", req.Body)
	}
	newID := req.Uint32(0)
	if newID != controller.ID() {
		t.Fatalf("create_controller new_id = %d, want controller ID %d", newID, controller.ID())
	}
	if got := s.ObjectID(generated.WlvisionControllerInterface); got != newID {
		t.Fatalf("ObjectID(%s) = %d, want %d", generated.WlvisionControllerInterface, got, newID)
	}
	return controller, newID
}

// findBindRequest returns the request the client sent for wl_registry.bind.
func findBindRequest(t *testing.T, s *Server) Request {
	t.Helper()

	registryID := s.RegistryID()
	for _, req := range s.Requests() {
		if req.Object == registryID && req.Opcode == 0 {
			return req
		}
	}
	t.Fatalf("no wl_registry.bind recorded on registry object %d: %+v", registryID, s.Requests())
	return Request{}
}

func TestRegistryRoundtripAndBind(t *testing.T) {
	s, display := startServer(t)

	global := mustGlobal(t, display)
	if global.Version != 1 {
		t.Fatalf("global version = %d, want 1", global.Version)
	}
	if got := display.Registry().ID(); got != s.RegistryID() {
		t.Fatalf("registry object ID = %d, server saw %d", got, s.RegistryID())
	}
	if got := s.ObjectID(generated.WlvisionControlInterface); got != 0 {
		t.Fatalf("ObjectID before bind = %d, want 0", got)
	}

	ctl := bindControl(t, display)
	if got := s.ObjectID(generated.WlvisionControlInterface); got != ctl.ID() {
		t.Fatalf("ObjectID(%s) = %d, want %d", generated.WlvisionControlInterface, got, ctl.ID())
	}
	if got := s.InterfaceFor(ctl.ID()); got != generated.WlvisionControlInterface {
		t.Fatalf("InterfaceFor(%d) = %q, want %q", ctl.ID(), got, generated.WlvisionControlInterface)
	}

	req := findBindRequest(t, s)
	name := req.Uint32(0)
	iface, consumed := req.String(4)
	version := req.Uint32(4 + consumed)
	id := req.Uint32(8 + consumed)

	if name != global.Name {
		t.Fatalf("bind global name = %d, want %d", name, global.Name)
	}
	if iface != generated.WlvisionControlInterface {
		t.Fatalf("bind interface = %q, want %q", iface, generated.WlvisionControlInterface)
	}
	if version != 1 {
		t.Fatalf("bind version = %d, want 1", version)
	}
	if id != ctl.ID() {
		t.Fatalf("bind object ID = %d, want %d", id, ctl.ID())
	}
}

func TestCreateControllerCarriesNewID(t *testing.T) {
	s, display := startServer(t)
	ctl := bindControl(t, display)

	_, controllerID := createController(t, s, ctl)

	// The bind that created the manager object, decoded independently.
	req := findBindRequest(t, s)
	iface, consumed := req.String(4)
	if iface != generated.WlvisionControlInterface {
		t.Fatalf("bind interface = %q, want %q", iface, generated.WlvisionControlInterface)
	}
	if version := req.Uint32(4 + consumed); version != 1 {
		t.Fatalf("bind version = %d, want 1", version)
	}
	if id := req.Uint32(8 + consumed); id != ctl.ID() {
		t.Fatalf("bind object ID = %d, want %d", id, ctl.ID())
	}
	if controllerID == ctl.ID() {
		t.Fatalf("controller ID %d collides with the manager object ID", controllerID)
	}
}

func TestActivateRequestBody(t *testing.T) {
	s, display := startServer(t)
	ctl := bindControl(t, display)
	controller, controllerID := createController(t, s, ctl)

	const (
		requestID  = uint32(0xDEADBEEF)
		revisionHi = uint32(0x00000001)
		revisionLo = uint32(0x00000002)
		handle     = "win-7"
	)

	if err := controller.Activate(requestID, handle, revisionHi, revisionLo); err != nil {
		t.Fatalf("activate: %v", err)
	}

	req := s.WaitForRequest(t, generated.WlvisionControllerInterface, 1)
	if req.Object != controllerID {
		t.Fatalf("activate object = %d, want %d", req.Object, controllerID)
	}

	// Byte for byte: request_id, handle string with NUL and padding, hi, lo.
	want := []byte{
		0xef, 0xbe, 0xad, 0xde, // request_id
		0x06, 0x00, 0x00, 0x00, // string length including NUL
		'w', 'i', 'n', '-', '7', 0x00, 0x00, 0x00, // "win-7" plus NUL and padding
		0x01, 0x00, 0x00, 0x00, // revision_hi
		0x02, 0x00, 0x00, 0x00, // revision_lo
	}
	if !bytes.Equal(req.Body, want) {
		t.Fatalf("activate body = % x\nwant            % x", req.Body, want)
	}

	// And through the decode helpers the harness exposes.
	if got := req.Uint32(0); got != requestID {
		t.Fatalf("request_id = %#x, want %#x", got, requestID)
	}
	gotHandle, consumed := req.String(4)
	if gotHandle != handle || consumed != 12 {
		t.Fatalf("handle = %q consumed %d, want %q consumed 12", gotHandle, consumed, handle)
	}
	if got := req.Uint32(4 + consumed); got != revisionHi {
		t.Fatalf("revision_hi = %d, want %d", got, revisionHi)
	}
	if got := req.Uint32(8 + consumed); got != revisionLo {
		t.Fatalf("revision_lo = %d, want %d", got, revisionLo)
	}
}

func TestInjectedEventsReachHandlers(t *testing.T) {
	s, display := startServer(t)
	ctl := bindControl(t, display)
	controller, controllerID := createController(t, s, ctl)

	type doneEvent struct {
		requestID  uint32
		revisionHi uint32
		revisionLo uint32
	}
	type changedEvent struct {
		handle     string
		title      string
		appID      string
		x, y       int32
		width      uint32
		height     uint32
		state      uint32
		revisionHi uint32
		revisionLo uint32
	}

	var (
		doneCalls    int
		changedCalls int
		gotDone      doneEvent
		gotChanged   changedEvent
	)

	controller.OnRequestDone(func(requestID, revisionHi, revisionLo uint32) {
		doneCalls++
		gotDone = doneEvent{requestID, revisionHi, revisionLo}
	})
	controller.OnToplevelChanged(func(handle, title, appID string, x, y int32, width, height, state, revisionHi, revisionLo uint32) {
		changedCalls++
		gotChanged = changedEvent{handle, title, appID, x, y, width, height, state, revisionHi, revisionLo}
	})

	if err := s.SendEvent(controllerID, 3, uint32(0x11), uint32(0x22), uint32(0x33)); err != nil {
		t.Fatalf("send request_done: %v", err)
	}
	if err := s.SendEvent(controllerID, 1,
		"handle-a", "A title", "org.example.App",
		int32(-4), int32(9), uint32(800), uint32(600), uint32(2),
		uint32(0x44), uint32(0x55),
	); err != nil {
		t.Fatalf("send toplevel_changed: %v", err)
	}

	// The client's own roundtrip dispatches the queued events.
	if err := display.Roundtrip(); err != nil {
		t.Fatalf("roundtrip after injected events: %v", err)
	}

	if doneCalls != 1 {
		t.Fatalf("request_done handlers fired %d times, want 1", doneCalls)
	}
	wantDone := doneEvent{0x11, 0x22, 0x33}
	if gotDone != wantDone {
		t.Fatalf("request_done = %+v, want %+v", gotDone, wantDone)
	}

	if changedCalls != 1 {
		t.Fatalf("toplevel_changed handlers fired %d times, want 1", changedCalls)
	}
	wantChanged := changedEvent{
		handle: "handle-a", title: "A title", appID: "org.example.App",
		x: -4, y: 9, width: 800, height: 600, state: 2,
		revisionHi: 0x44, revisionLo: 0x55,
	}
	if gotChanged != wantChanged {
		t.Fatalf("toplevel_changed = %+v, want %+v", gotChanged, wantChanged)
	}
}

func TestDisplayErrorSurfacesToCaller(t *testing.T) {
	s, display := startServer(t)

	objectID := s.RegistryID()
	if err := s.SendDisplayError(objectID, 7, "capture is not available"); err != nil {
		t.Fatalf("send wl_display.error: %v", err)
	}

	// A previous roundtrip can leave its wl_display.delete_id frame queued, so
	// dispatch until the injected error surfaces. The bound is the number of
	// frames the server has sent for this test, so the loop never blocks.
	var err error
	for i := 0; i < 2 && err == nil; i++ {
		err = display.Dispatch()
	}
	if err == nil {
		t.Fatal("wl_display.error was swallowed by Dispatch")
	}
	if !strings.Contains(err.Error(), "capture is not available") {
		t.Fatalf("error %q does not carry the server message", err)
	}

	var displayErr *wlturbo.DisplayError
	if !errors.As(err, &displayErr) {
		t.Fatalf("error type = %T, want *wlturbo.DisplayError", err)
	}
	if displayErr.ObjectID != objectID || displayErr.Code != 7 {
		t.Fatalf("display error = object %d code %d, want object %d code 7",
			displayErr.ObjectID, displayErr.Code, objectID)
	}
}

// TestActivateBodyIsFramedCorrectly pins the header framing itself: the size
// field counts the whole message and the object/opcode words match.
func TestActivateBodyIsFramedCorrectly(t *testing.T) {
	s, display := startServer(t)
	ctl := bindControl(t, display)
	controller, controllerID := createController(t, s, ctl)

	if err := controller.Activate(1, "h", 0, 0); err != nil {
		t.Fatalf("activate: %v", err)
	}
	req := s.WaitForRequest(t, generated.WlvisionControllerInterface, 1)

	msg := message(req.Object, req.Opcode, req.Body)
	object := binary.LittleEndian.Uint32(msg[0:4])
	sizeOpcode := binary.LittleEndian.Uint32(msg[4:8])
	if object != controllerID {
		t.Fatalf("framed object = %d, want %d", object, controllerID)
	}
	if size := sizeOpcode >> 16; size != uint32(HeaderSize+len(req.Body)) {
		t.Fatalf("framed size = %d, want %d", size, HeaderSize+len(req.Body))
	}
	if opcode := uint16(sizeOpcode & 0xffff); opcode != 1 {
		t.Fatalf("framed opcode = %d, want 1", opcode)
	}
}
