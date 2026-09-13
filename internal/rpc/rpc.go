// Package rpc implements the resident-control transport used between a
// short-lived wlvision-call process (run via "docker exec --user CONTROL_UID")
// and the resident controller inside the session container. It moves exactly
// one request/response pair per Unix-socket connection.
//
// Framing is fixed and payload-agnostic: a 4-byte big-endian length prefix
// followed by that many bytes, in both directions, with MaxMessage as the hard
// ceiling. A zero-length body is valid and round-trips. The server reads one
// request, invokes the Handler once, writes one response, and closes the
// connection; requests are handled one at a time, and a peer whose credentials
// do not match Config.ExpectedUID is closed without ever reaching the Handler.
//
// The two directions are deliberately asymmetric:
//
//   - client to server: the message body is exactly the bytes the caller
//     passed to Conn.Call. The Handler receives them verbatim, with no
//     wrapper, so the caller owns its own serialization entirely.
//   - server to client: the message body is the JSON encoding of a Response.
//     The server must be able to report a Handler failure the same way it
//     reports a payload, so the client can tell them apart. Conn.Call returns
//     (Response.Payload, nil) on success and (nil, the reported
//     *result.Failure) when the Handler failed.
//
// Every read is bounded by the caller's context: the socket deadline is the
// context deadline when one is set, and otherwise readCeiling (30 seconds)
// from the start of the read. A cancelled context additionally interrupts an
// in-flight read immediately, so a client that connects and sends nothing
// cannot stall shutdown beyond the caller's deadline or that ceiling.
package rpc

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/bnema/wlvision/internal/result"
)

// MaxMessage is the hard ceiling, in bytes, for one message body in either
// direction. A peer that declares a longer body is rejected before any buffer
// is allocated for it.
const MaxMessage = 4 << 20

// readCeiling bounds one socket read when the caller's context carries no
// deadline. It exists so that a peer that connects and reads or writes nothing
// can never block a server or a client forever.
const readCeiling = 30 * time.Second

// Default modes applied by Listen when the corresponding Config field is zero.
const (
	defaultDirMode    os.FileMode = 0o700
	defaultSocketMode os.FileMode = 0o600
)

// opControl names the transport itself in failures the outer CLI can see.
const opControl = "control"

var (
	// errMessageTooLarge is returned before allocating when a declared or
	// produced message body exceeds MaxMessage.
	errMessageTooLarge = errors.New("rpc: message exceeds MaxMessage")
	// errOneRequestPerConn enforces the one-request-per-connection contract.
	errOneRequestPerConn = errors.New("rpc: one request per connection")
	// errNoHandler is returned by Serve when no handler was installed.
	errNoHandler = errors.New("rpc: no handler installed")
)

// Config configures a server.
type Config struct {
	// Path is the Unix-socket path to bind. It must not be empty.
	Path string
	// ExpectedUID is the only peer UID allowed to reach the Handler. Zero
	// disables the check and exists for tests only: no production caller may
	// pass 0, because it would let any local user drive the controller.
	ExpectedUID uint32
	// DirMode is the mode of the socket's parent directory, created when
	// missing. Zero means 0o700.
	DirMode os.FileMode
	// SocketMode is the mode applied to the socket file. Zero means 0o600.
	SocketMode os.FileMode
}

// Handler serves one decoded request body and returns the response body. It is
// never invoked concurrently. On failure it should return a *result.Failure,
// which is transmitted to the client verbatim; any other error is transmitted
// with its message preserved under result.CodeSessionNotReady.
type Handler func(ctx context.Context, request []byte) ([]byte, error)

// Response is the only typed envelope this package puts on the wire. It is the
// body of every server-to-client message; the client-to-server body is the
// caller's raw bytes with no wrapper at all.
type Response struct {
	Payload []byte          `json:"payload,omitempty"`
	Failure *result.Failure `json:"failure,omitempty"`
}

// Server owns the listening socket end of the transport.
type Server struct {
	path string
	cfg  Config

	mu      sync.Mutex
	closed  bool
	handler Handler
	ln      *net.UnixListener
}

// Listen creates the socket's parent directory with Config.DirMode when it is
// missing, binds the socket at Config.Path, applies Config.SocketMode, and
// starts listening. An existing directory is accepted only when it is owned by
// the current user and is not group- or world-writable.
//
// Listen does not serve; call Serve for that. Use SetHandler to install the
// request handler before serving.
func Listen(cfg Config) (*Server, error) {
	if cfg.Path == "" {
		return nil, errors.New("rpc: empty socket path")
	}
	dirMode := cfg.DirMode
	if dirMode == 0 {
		dirMode = defaultDirMode
	}
	socketMode := cfg.SocketMode
	if socketMode == 0 {
		socketMode = defaultSocketMode
	}

	dir := filepath.Dir(cfg.Path)
	if err := prepareDir(dir, dirMode); err != nil {
		return nil, err
	}

	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: cfg.Path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("rpc: listening on %s: %w", cfg.Path, err)
	}
	if err := os.Chmod(cfg.Path, socketMode); err != nil {
		_ = ln.Close()
		_ = os.Remove(cfg.Path)
		return nil, fmt.Errorf("rpc: setting socket mode: %w", err)
	}
	return &Server{path: cfg.Path, cfg: cfg, ln: ln}, nil
}

// Path returns the socket path the server is bound to.
func (s *Server) Path() string { return s.path }

// SetHandler installs the handler Serve invokes for each request. It must be
// called before Serve; it is not safe to call concurrently with a running
// Serve.
func (s *Server) SetHandler(h Handler) {
	s.mu.Lock()
	s.handler = h
	s.mu.Unlock()
}

// Serve accepts connections until ctx is cancelled or Close is called, then
// returns nil once the in-flight handler, if any, has returned. Exactly one
// request is handled per connection: read one framed message, call the
// Handler, write one framed response, close the connection. Requests are
// handled one at a time, so Handlers never run concurrently.
//
// Serve returns an error only for an unexpected accept failure or when no
// handler was installed.
func (s *Server) Serve(ctx context.Context) error {
	s.mu.Lock()
	h := s.handler
	s.mu.Unlock()
	if h == nil {
		return errNoHandler
	}

	// Unblock Accept when the context is cancelled. The listener is left for
	// Close to unlink, so the caller must still call Close during shutdown.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = s.ln.SetDeadline(time.Now())
		case <-stop:
		}
	}()

	for {
		nc, err := s.ln.AcceptUnix()
		if err != nil {
			if s.isClosed() || ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return fmt.Errorf("rpc: accepting connection: %w", err)
		}
		s.handleConn(ctx, h, nc)
	}
}

// Close stops the listener and removes the socket file. It is safe to call
// twice and safe to call while Serve is running; a second call is a no-op.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	err := s.ln.Close()
	if errors.Is(err, net.ErrClosed) {
		err = nil
	}
	if rmErr := os.Remove(s.path); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) && err == nil {
		err = rmErr
	}
	return err
}

func (s *Server) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// handleConn serves exactly one request/response pair on nc and closes it.
func (s *Server) handleConn(ctx context.Context, h Handler, nc *net.UnixConn) {
	defer nc.Close()
	_ = nc.SetDeadline(readDeadline(ctx))

	// Interrupt the read (or the response write) as soon as ctx is cancelled.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = nc.SetDeadline(time.Now())
		case <-stop:
		}
	}()

	if s.cfg.ExpectedUID != 0 {
		uid, err := peerUID(nc)
		if err != nil || uid != s.cfg.ExpectedUID {
			f := result.NewFailure(result.CodeSessionNotReady, opControl,
				"the control socket is not available to this user")
			f.Details = map[string]string{"uid": strconv.FormatUint(uint64(uid), 10)}
			s.respond(nc, Response{Failure: f})
			return
		}
	}

	request, err := readMessage(nc)
	if err != nil {
		if errors.Is(err, errMessageTooLarge) {
			s.respond(nc, Response{Failure: result.NewFailure(result.CodeUsageError, opControl,
				"malformed or oversized control message")})
		}
		// A short read means the peer went away before sending a complete
		// request; there is nobody left to answer.
		return
	}

	payload, herr := h(ctx, request)
	if herr != nil {
		s.respond(nc, Response{Failure: failureFromError(herr)})
		return
	}
	s.respond(nc, Response{Payload: payload})
}

// respond writes one framed Response. A response that would exceed MaxMessage
// (possible when a handler returns an oversized payload) is replaced by a small
// usage failure the client can always read.
func (s *Server) respond(nc *net.UnixConn, resp Response) {
	body, err := json.Marshal(resp)
	if err != nil {
		return
	}
	if len(body) > MaxMessage {
		body, err = json.Marshal(Response{Failure: result.NewFailure(result.CodeUsageError, opControl,
			"malformed or oversized control response")})
		if err != nil {
			return
		}
	}
	_ = writeMessage(nc, body)
}

// failureFromError transmits a handler error. A *result.Failure travels
// verbatim; any other error has no code to preserve, so its message is carried
// under result.CodeSessionNotReady.
func failureFromError(err error) *result.Failure {
	var f *result.Failure
	if errors.As(err, &f) {
		return f
	}
	return result.NewFailure(result.CodeSessionNotReady, opControl, "%v", err)
}

// peerUID reads the peer credentials of a connected Unix socket.
func peerUID(conn *net.UnixConn) (uint32, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var (
		uid  uint32
		cerr error
	)
	if err := raw.Control(func(fd uintptr) {
		cred, e := syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
		if e != nil {
			cerr = e
			return
		}
		uid = cred.Uid
	}); err != nil {
		return 0, err
	}
	return uid, cerr
}

// prepareDir ensures dir exists and is safe to place a control socket in.
func prepareDir(dir string, mode os.FileMode) error {
	info, err := os.Stat(dir)
	switch {
	case err == nil:
		if !info.IsDir() {
			return fmt.Errorf("rpc: %s exists and is not a directory", dir)
		}
		if perm := info.Mode().Perm(); perm&0o022 != 0 {
			return fmt.Errorf("rpc: socket directory %s is group or world writable (%#o)", dir, perm)
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("rpc: cannot determine the owner of socket directory %s", dir)
		}
		if st.Uid != uint32(os.Getuid()) {
			return fmt.Errorf("rpc: socket directory %s is owned by uid %d, not %d", dir, st.Uid, os.Getuid())
		}
		return nil
	case errors.Is(err, fs.ErrNotExist):
		if err := os.MkdirAll(dir, mode); err != nil {
			return fmt.Errorf("rpc: creating socket directory %s: %w", dir, err)
		}
		// MkdirAll is subject to umask; make the requested mode exact.
		if err := os.Chmod(dir, mode); err != nil {
			return fmt.Errorf("rpc: setting socket directory mode: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("rpc: inspecting socket directory %s: %w", dir, err)
	}
}

// readDeadline returns the instant a read must give up: the caller's deadline
// when it has one, and otherwise the fixed internal ceiling.
func readDeadline(ctx context.Context) time.Time {
	if d, ok := ctx.Deadline(); ok {
		return d
	}
	return time.Now().Add(readCeiling)
}

// readMessage reads one length-prefixed message. The declared length is checked
// against MaxMessage before the body buffer is allocated.
func readMessage(r io.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > MaxMessage {
		return nil, fmt.Errorf("%w: declared %d bytes", errMessageTooLarge, n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return body, nil
}

// writeMessage writes one length-prefixed message.
func writeMessage(w io.Writer, body []byte) error {
	if len(body) > MaxMessage {
		return fmt.Errorf("%w: %d bytes", errMessageTooLarge, len(body))
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(body)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(body)
	return err
}

// Conn is a client connection that carries exactly one request/response pair.
type Conn struct {
	mu     sync.Mutex
	conn   *net.UnixConn
	used   bool
	closed bool
}

// Dial connects to the control socket at path. The context bounds the dial; a
// subsequent Call is bounded by its own context.
func Dial(ctx context.Context, path string) (*Conn, error) {
	if path == "" {
		return nil, errors.New("rpc: empty socket path")
	}
	var d net.Dialer
	raw, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("rpc: dialing %s: %w", path, err)
	}
	uc, ok := raw.(*net.UnixConn)
	if !ok {
		_ = raw.Close()
		return nil, fmt.Errorf("rpc: %s is not a Unix socket", path)
	}
	return &Conn{conn: uc}, nil
}

// Call sends one request and returns the response payload, or the failure the
// server reported. A second call on the same Conn is an error: each connection
// carries exactly one request.
func (c *Conn) Call(ctx context.Context, request []byte) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errors.New("rpc: connection is closed")
	}
	if c.used {
		return nil, errOneRequestPerConn
	}
	c.used = true

	_ = c.conn.SetDeadline(readDeadline(ctx))
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = c.conn.SetDeadline(time.Now())
		case <-stop:
		}
	}()

	if err := writeMessage(c.conn, request); err != nil {
		if errors.Is(err, errMessageTooLarge) {
			return nil, result.NewFailure(result.CodeUsageError, opControl,
				"malformed or oversized control request")
		}
		if ctx.Err() != nil {
			return nil, cancelledRead()
		}
		return nil, fmt.Errorf("rpc: sending request: %w", err)
	}

	body, err := readMessage(c.conn)
	if err != nil {
		return nil, c.readFailure(ctx, err)
	}
	var resp Response
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, result.NewFailure(result.CodeUsageError, opControl,
			"malformed control response")
	}
	if resp.Failure != nil {
		return nil, resp.Failure
	}
	return resp.Payload, nil
}

// readFailure maps a response-read error onto the failures the outer CLI can
// see. Ordinary transport errors stay plain.
func (c *Conn) readFailure(ctx context.Context, err error) error {
	if errors.Is(err, errMessageTooLarge) {
		return result.NewFailure(result.CodeUsageError, opControl,
			"malformed or oversized control response")
	}
	if ctx.Err() != nil {
		return cancelledRead()
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, os.ErrDeadlineExceeded) {
		return result.NewFailure(result.CodeSessionNotReady, opControl,
			"the control socket closed before a complete response arrived")
	}
	return fmt.Errorf("rpc: reading response: %w", err)
}

func cancelledRead() *result.Failure {
	return result.NewFailure(result.CodeSessionNotReady, opControl,
		"the control request was cancelled before a complete response arrived")
}

// Close releases the connection. It is safe to call twice.
func (c *Conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	return c.conn.Close()
}
