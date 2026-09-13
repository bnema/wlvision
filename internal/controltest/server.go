// Package controltest provides an in-process fake Wayland compositor for
// wlvision's control client.
//
// The server speaks just enough of the Wayland wire protocol to let tests
// drive the generated wlvision_control_v1 bindings end to end: it frames
// messages itself, answers the core handshake (wl_display.get_registry and
// wl_display.sync), records every request it receives, and lets a test inject
// arbitrary events or a wl_display.error. Tests therefore exercise real
// framing, real object bookkeeping and the real client bindings without a
// compositor, a display or Docker.
//
// It deliberately does not try to be a compositor: it validates nothing
// against the protocol XML, it serves one client connection at a time, and it
// only implements the protocol messages listed on handle. Everything else is
// recorded and ignored.
package controltest

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bnema/wlturbo/wl"
)

const (
	// displayObjectID is the well-known object ID of wl_display.
	displayObjectID = 1

	// HeaderSize is the fixed Wayland message header size: a uint32 object ID
	// followed by size<<16|opcode.
	HeaderSize = 8

	// maxMessageSize bounds a single message the server is willing to read.
	maxMessageSize = 1 << 20

	// waitTimeout bounds every WaitFor* poll.
	waitTimeout = 2 * time.Second
)

// Interface names the harness tracks beyond plain registry binds.
const (
	// WlvisionControlInterface is the singleton manager global.
	WlvisionControlInterface = "wlvision_control_v1"

	// WlvisionControllerInterface is the controller object created by
	// create_controller.
	WlvisionControllerInterface = "wlvision_controller_v1"
)

// wl_display opcodes.
const (
	displayOpcodeSync        = 0
	displayOpcodeGetRegistry = 1
)

// wl_registry opcode.
const registryOpcodeBind = 0

// wlvision_control_v1 opcode.
const controlOpcodeCreateController = 0

// Global describes one wl_registry global the server announces.
type Global struct {
	Name      uint32
	Interface string
	Version   uint32
}

// Request is one recorded client request.
type Request struct {
	Object uint32
	Opcode uint16
	Body   []byte
}

// Uint32 decodes a uint32 argument at the given byte offset. It returns 0 when
// the body is too short, so a malformed request fails the calling assertion
// instead of panicking.
func (r Request) Uint32(offset int) uint32 {
	if offset < 0 || offset+4 > len(r.Body) {
		return 0
	}
	return binary.LittleEndian.Uint32(r.Body[offset : offset+4])
}

// Int32 decodes an int32 argument at the given byte offset.
func (r Request) Int32(offset int) int32 {
	return int32(r.Uint32(offset))
}

// Fixed decodes a 24.8 fixed-point argument at the given byte offset.
func (r Request) Fixed(offset int) wl.Fixed {
	return wl.Fixed(r.Int32(offset))
}

// String decodes a Wayland string at the given byte offset. It returns the
// string and the number of bytes it occupied, including the NUL terminator and
// the alignment padding. A malformed or truncated string yields "" and 0.
func (r Request) String(offset int) (string, int) {
	if offset < 0 || offset+4 > len(r.Body) {
		return "", 0
	}
	length := int(binary.LittleEndian.Uint32(r.Body[offset : offset+4]))
	if length <= 0 || offset+4+length > len(r.Body) {
		return "", 0
	}
	value := string(r.Body[offset+4 : offset+4+length-1])
	consumed := 4 + length
	if pad := consumed % 4; pad != 0 {
		consumed += 4 - pad
	}
	return value, consumed
}

// Server is a minimal Wayland server serving one connection at a time.
type Server struct {
	runtimeDir string
	socketName string

	listener net.Listener

	mu       sync.Mutex
	conn     net.Conn
	requests []Request
	ids      map[string]uint32 // interface name -> object ID
	names    map[uint32]string // object ID -> interface name
	globals  []Global
	registry uint32
	err      error
	closed   bool

	writeMu sync.Mutex
}

// Start launches a server that announces the given globals. With no globals it
// announces wlvision_control_v1 version 1. The socket is removed and the
// listener closed when the test finishes.
func Start(t *testing.T, globals ...Global) *Server {
	t.Helper()

	if len(globals) == 0 {
		globals = []Global{{Name: 1, Interface: WlvisionControlInterface, Version: 1}}
	}

	runtimeDir := t.TempDir()
	socketName := "wayland-controltest"
	socketPath := filepath.Join(runtimeDir, socketName)

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("controltest: listen %s: %v", socketPath, err)
	}

	s := &Server{
		runtimeDir: runtimeDir,
		socketName: socketName,
		listener:   listener,
		ids:        make(map[string]uint32),
		names:      make(map[uint32]string),
		globals:    globals,
	}

	go s.serve()

	t.Cleanup(s.Close)
	return s
}

// Env points the Wayland client library at this compositor for the duration of
// the test. The environment is restored and the socket removed by the cleanup
// Start registered.
func (s *Server) Env(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", s.runtimeDir)
	t.Setenv("WAYLAND_DISPLAY", s.socketName)
}

// SocketPath returns the compositor socket path.
func (s *Server) SocketPath() string {
	return filepath.Join(s.runtimeDir, s.socketName)
}

// Requests returns a copy of every request received so far.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Request, len(s.requests))
	copy(out, s.requests)
	return out
}

// WaitForRequests blocks until at least n requests have been recorded or the
// deadline expires, then returns everything recorded.
func (s *Server) WaitForRequests(t *testing.T, n int) []Request {
	t.Helper()

	deadline := time.Now().Add(waitTimeout)
	for {
		requests := s.Requests()
		if len(requests) >= n {
			return requests
		}
		if time.Now().After(deadline) {
			t.Fatalf("controltest: waited for %d requests, saw %d: %+v", n, len(requests), requests)
		}
		time.Sleep(time.Millisecond)
	}
}

// WaitForRequest blocks until a request for the named interface and opcode has
// been received, then returns it.
func (s *Server) WaitForRequest(t *testing.T, iface string, opcode uint16) Request {
	t.Helper()

	id := s.ObjectID(iface)
	if id == 0 {
		t.Fatalf("controltest: interface %q was never bound", iface)
	}

	deadline := time.Now().Add(waitTimeout)
	for {
		for _, req := range s.Requests() {
			if req.Object == id && req.Opcode == opcode {
				return req
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("controltest: no request object=%d opcode=%d for %q: %+v", id, opcode, iface, s.Requests())
		}
		time.Sleep(time.Millisecond)
	}
}

// ObjectID returns the object ID last seen for an interface, or 0 when unknown.
// It reports both registry binds and the controller allocated by
// wlvision_control_v1.create_controller.
func (s *Server) ObjectID(iface string) uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ids[iface]
}

// RegistryID returns the object ID the client used in get_registry, or 0.
func (s *Server) RegistryID() uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.registry
}

// InterfaceFor returns the interface bound to an object ID, or "".
func (s *Server) InterfaceFor(id uint32) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.names[id]
}

// SendEvent pushes one event to the client. Arguments may be uint32, int32,
// int, wl.Fixed or string.
func (s *Server) SendEvent(object uint32, opcode uint16, args ...any) error {
	body, err := marshal(args...)
	if err != nil {
		return err
	}
	return s.write(message(object, opcode, body))
}

// SendDisplayError reports a protocol error to the client.
func (s *Server) SendDisplayError(object uint32, code uint32, text string) error {
	return s.SendEvent(displayObjectID, 0, object, code, text)
}

// Close stops the server and removes its socket. It is safe to call twice.
func (s *Server) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	conn := s.conn
	s.mu.Unlock()

	if conn != nil {
		_ = conn.Close()
	}
	_ = s.listener.Close()
	_ = os.Remove(s.SocketPath())
}

// Err reports the first unexpected server-side error.
func (s *Server) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *Server) setErr(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.mu.Unlock()
}

func (s *Server) serve() {
	conn, err := s.listener.Accept()
	if err != nil {
		return // listener closed during cleanup
	}

	s.mu.Lock()
	s.conn = conn
	s.mu.Unlock()

	for {
		object, opcode, body, err := readMessage(conn)
		if err != nil {
			if !isConnectionEnd(err) {
				s.setErr(err)
			}
			return
		}

		s.mu.Lock()
		s.requests = append(s.requests, Request{Object: object, Opcode: opcode, Body: body})
		registry := s.registry
		manager := s.ids[WlvisionControlInterface]
		s.mu.Unlock()

		if err := s.handle(conn, object, opcode, body, registry, manager); err != nil {
			s.setErr(err)
			return
		}
	}
}

func (s *Server) handle(conn net.Conn, object uint32, opcode uint16, body []byte, registry, manager uint32) error {
	switch {
	case object == displayObjectID && opcode == displayOpcodeSync:
		// Answer a roundtrip: wl_callback.done, then release the callback ID.
		callback := readUint32(body, 0)
		if err := s.write(message(callback, 0, marshalRaw(uint32(1)))); err != nil {
			return err
		}
		return s.write(message(displayObjectID, 1, marshalRaw(callback)))

	case object == displayObjectID && opcode == displayOpcodeGetRegistry:
		id := readUint32(body, 0)

		s.mu.Lock()
		s.registry = id
		globals := append([]Global(nil), s.globals...)
		s.mu.Unlock()

		for _, global := range globals {
			args, err := marshal(global.Name, global.Interface, global.Version)
			if err != nil {
				return err
			}
			if err := s.write(message(id, 0, args)); err != nil {
				return err
			}
		}
		return nil

	case registry != 0 && object == registry && opcode == registryOpcodeBind:
		req := Request{Body: body}
		req.Uint32(0) // global name; recorded for the test, not inspected here
		iface, consumed := req.String(4)
		req.Uint32(4 + consumed) // interface version
		id := req.Uint32(8 + consumed)

		s.mu.Lock()
		s.ids[iface] = id
		s.names[id] = iface
		s.mu.Unlock()
		return nil

	case manager != 0 && object == manager && opcode == controlOpcodeCreateController:
		// new_id argument: the controller object the client allocated. The
		// harness tracks its ID so tests can address events at it.
		id := readUint32(body, 0)

		s.mu.Lock()
		s.ids[WlvisionControllerInterface] = id
		s.names[id] = WlvisionControllerInterface
		s.mu.Unlock()
		return nil
	}

	return nil
}

func (s *Server) write(msg []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()

	if conn == nil {
		return nil
	}
	_, err := conn.Write(msg)
	return err
}

// readMessage reads one complete Wayland message from conn.
func readMessage(conn net.Conn) (object uint32, opcode uint16, body []byte, err error) {
	var hdr [HeaderSize]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		return 0, 0, nil, err
	}

	object = binary.LittleEndian.Uint32(hdr[0:4])
	sizeOpcode := binary.LittleEndian.Uint32(hdr[4:8])
	size := sizeOpcode >> 16
	opcode = uint16(sizeOpcode & 0xffff)

	if size < HeaderSize || size%4 != 0 || size > maxMessageSize {
		return 0, 0, nil, fmt.Errorf("controltest: malformed header: object=%d opcode=%d size=%d", object, opcode, size)
	}
	if size == HeaderSize {
		return object, opcode, nil, nil
	}

	body = make([]byte, size-HeaderSize)
	if _, err := io.ReadFull(conn, body); err != nil {
		return 0, 0, nil, err
	}
	return object, opcode, body, nil
}

// isConnectionEnd reports whether err is the ordinary end of a test connection
// rather than a protocol failure worth surfacing through Err.
func isConnectionEnd(err error) bool {
	return errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed)
}

// message builds a framed Wayland message.
func message(object uint32, opcode uint16, body []byte) []byte {
	msg := make([]byte, HeaderSize+len(body))
	binary.LittleEndian.PutUint32(msg[0:4], object)
	binary.LittleEndian.PutUint32(msg[4:8], (uint32(HeaderSize+len(body))<<16)|uint32(opcode))
	copy(msg[HeaderSize:], body)
	return msg
}

// marshalRaw wraps an already encoded body.
func marshalRaw(values ...any) []byte {
	body, err := marshal(values...)
	if err != nil {
		panic(err)
	}
	return body
}

// marshal encodes Wayland arguments the same way the client library does.
func marshal(args ...any) ([]byte, error) {
	var body []byte
	for _, arg := range args {
		switch v := arg.(type) {
		case uint32:
			body = binary.LittleEndian.AppendUint32(body, v)
		case int32:
			body = binary.LittleEndian.AppendUint32(body, uint32(v))
		case int:
			body = binary.LittleEndian.AppendUint32(body, uint32(v))
		case wl.Fixed:
			body = binary.LittleEndian.AppendUint32(body, uint32(int32(v)))
		case string:
			length := uint32(len(v) + 1)
			body = binary.LittleEndian.AppendUint32(body, length)
			body = append(body, v...)
			body = append(body, 0)
			for len(body)%4 != 0 {
				body = append(body, 0)
			}
		default:
			return nil, fmt.Errorf("controltest: unsupported argument type %T", arg)
		}
	}
	return body, nil
}

func readUint32(body []byte, offset int) uint32 {
	if offset < 0 || offset+4 > len(body) {
		return 0
	}
	return binary.LittleEndian.Uint32(body[offset : offset+4])
}
