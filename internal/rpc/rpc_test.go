package rpc

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/bnema/wlvision/internal/result"
)

// maxEchoPayload is the largest request payload whose JSON-wrapped response
// still fits in one MaxMessage frame. Response.Payload is a []byte, so
// encoding/json base64-encodes it and the framed body grows to roughly 4/3 of
// the payload plus the small object wrapper. The value stays comfortably under
// that limit.
func maxEchoPayload() int { return MaxMessage*3/4 - 64 }

// serve starts a server on a fresh socket and returns it with a function that
// waits for Serve to return. Cleanup cancels and closes the server.
func serve(t *testing.T, cfg Config, h Handler) (*Server, context.CancelFunc, func() error) {
	t.Helper()
	if cfg.Path == "" {
		cfg.Path = filepath.Join(t.TempDir(), "control.sock")
	}
	s, err := Listen(cfg)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	s.SetHandler(h)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()

	var (
		once     sync.Once
		serveErr error
	)
	wait := func() error {
		once.Do(func() {
			select {
			case serveErr = <-done:
			case <-time.After(time.Second):
				serveErr = errors.New("Serve did not return")
			}
		})
		return serveErr
	}
	t.Cleanup(func() {
		cancel()
		_ = s.Close()
		if err := wait(); err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return s, cancel, wait
}

// echo returns a handler that returns the request unchanged.
func echo(t *testing.T, seen *int) Handler {
	var mu sync.Mutex
	return func(_ context.Context, request []byte) ([]byte, error) {
		mu.Lock()
		*seen++
		mu.Unlock()
		return request, nil
	}
}

func call(t *testing.T, path string, payload []byte) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c, err := Dial(ctx, path)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	return c.Call(ctx, payload)
}

func TestRoundTrip(t *testing.T) {
	fill := func(n int) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(i)
		}
		return b
	}
	cases := []struct {
		name string
		size int
	}{
		{"empty", 0},
		{"small", 37},
		{"near MaxMessage", maxEchoPayload()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seen int
			s, _, _ := serve(t, Config{}, echo(t, &seen))
			want := fill(tc.size)
			got, err := call(t, s.Path(), want)
			if err != nil {
				t.Fatalf("Call: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("payload mismatch: got %d bytes, want %d", len(got), len(want))
			}
			if seen != 1 {
				t.Fatalf("handler calls = %d, want 1", seen)
			}
		})
	}
}

func readResponse(t *testing.T, r io.Reader) Response {
	t.Helper()
	body, err := readMessage(r)
	if err != nil {
		t.Fatalf("readMessage: %v", err)
	}
	var resp Response
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal response %q: %v", body, err)
	}
	return resp
}

func TestFragmentedWrites(t *testing.T) {
	body := []byte("fragmented request body")
	cases := []struct {
		name  string
		write func(t *testing.T, c net.Conn, body []byte)
	}{
		{"prefix and body in separate writes", func(t *testing.T, c net.Conn, body []byte) {
			var hdr [4]byte
			binary.BigEndian.PutUint32(hdr[:], uint32(len(body)))
			if _, err := c.Write(hdr[:]); err != nil {
				t.Fatalf("write prefix: %v", err)
			}
			if _, err := c.Write(body); err != nil {
				t.Fatalf("write body: %v", err)
			}
		}},
		{"body one byte at a time", func(t *testing.T, c net.Conn, body []byte) {
			var hdr [4]byte
			binary.BigEndian.PutUint32(hdr[:], uint32(len(body)))
			if _, err := c.Write(hdr[:]); err != nil {
				t.Fatalf("write prefix: %v", err)
			}
			for i := range body {
				if _, err := c.Write(body[i : i+1]); err != nil {
					t.Fatalf("write byte %d: %v", i, err)
				}
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seen int
			s, _, _ := serve(t, Config{}, echo(t, &seen))
			conn, err := net.Dial("unix", s.Path())
			if err != nil {
				t.Fatalf("Dial: %v", err)
			}
			defer conn.Close()
			if err := conn.SetDeadline(time.Now().Add(time.Second)); err != nil {
				t.Fatalf("SetDeadline: %v", err)
			}
			tc.write(t, conn, body)
			resp := readResponse(t, conn)
			if !bytes.Equal(resp.Payload, body) {
				t.Fatalf("payload = %q, want %q", resp.Payload, body)
			}
			if seen != 1 {
				t.Fatalf("handler calls = %d, want 1", seen)
			}
		})
	}
}

func TestCoalescedReadsServeOneMessage(t *testing.T) {
	first := []byte("first")
	second := []byte("second")

	var (
		mu   sync.Mutex
		got  [][]byte
		done = make(chan struct{})
	)
	h := func(_ context.Context, request []byte) ([]byte, error) {
		mu.Lock()
		got = append(got, append([]byte(nil), request...))
		mu.Unlock()
		close(done)
		return request, nil
	}
	s, _, _ := serve(t, Config{}, h)

	conn, err := net.Dial("unix", s.Path())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}

	// Both frames plus a third request's prefix arrive in a single write. The
	// server must consume exactly the first message and drop the rest.
	var coalesced bytes.Buffer
	if err := writeFrameTo(&coalesced, first); err != nil {
		t.Fatalf("frame first: %v", err)
	}
	if err := writeFrameTo(&coalesced, second); err != nil {
		t.Fatalf("frame second: %v", err)
	}
	if _, err := conn.Write(coalesced.Bytes()); err != nil {
		t.Fatalf("write coalesced frames: %v", err)
	}

	resp := readResponse(t, conn)
	if !bytes.Equal(resp.Payload, first) {
		t.Fatalf("payload = %q, want %q", resp.Payload, first)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler was never called")
	}
	time.Sleep(20 * time.Millisecond) // give a stray second dispatch a chance
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || !bytes.Equal(got[0], first) {
		t.Fatalf("handler saw %q, want only the first message %q", got, first)
	}
}

func writeFrameTo(w io.Writer, body []byte) error {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(body)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(body)
	return err
}

func TestOversizedDeclaredLength(t *testing.T) {
	t.Run("readMessage refuses before allocating", func(t *testing.T) {
		// Prefix only, declaring 0xFFFFFFFF bytes: no body is available and
		// none must be allocated.
		prefix := []byte{0xff, 0xff, 0xff, 0xff}
		_, err := readMessage(bytes.NewReader(prefix))
		if !errors.Is(err, errMessageTooLarge) {
			t.Fatalf("err = %v, want errMessageTooLarge", err)
		}
	})

	t.Run("server answers with a usage failure", func(t *testing.T) {
		var seen int
		s, _, _ := serve(t, Config{}, echo(t, &seen))

		conn, err := net.Dial("unix", s.Path())
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		defer conn.Close()
		if err := conn.SetDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatalf("SetDeadline: %v", err)
		}
		if _, err := conn.Write([]byte{0xff, 0xff, 0xff, 0xff}); err != nil {
			t.Fatalf("write oversized prefix: %v", err)
		}
		resp := readResponse(t, conn)
		if resp.Failure == nil || resp.Failure.Code != result.CodeUsageError {
			t.Fatalf("failure = %+v, want code %s", resp.Failure, result.CodeUsageError)
		}
		if seen != 0 {
			t.Fatalf("handler calls = %d, want 0", seen)
		}
	})
}

func TestTruncatedBody(t *testing.T) {
	var seen int
	s, _, _ := serve(t, Config{}, echo(t, &seen))

	conn, err := net.Dial("unix", s.Path())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], 64)
	if _, err := conn.Write(hdr[:]); err != nil {
		t.Fatalf("write prefix: %v", err)
	}
	if _, err := conn.Write([]byte("short")); err != nil {
		t.Fatalf("write partial body: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The server must survive the truncated request and keep serving.
	got, err := call(t, s.Path(), []byte("after truncation"))
	if err != nil {
		t.Fatalf("Call after truncation: %v", err)
	}
	if !bytes.Equal(got, []byte("after truncation")) {
		t.Fatalf("payload = %q", got)
	}
	if seen != 1 {
		t.Fatalf("handler calls = %d, want 1", seen)
	}
}

func TestPeerCredentials(t *testing.T) {
	uid := uint32(os.Getuid())

	t.Run("mismatched uid is refused", func(t *testing.T) {
		var seen int
		s, _, _ := serve(t, Config{ExpectedUID: uid ^ 1}, echo(t, &seen))
		_, err := call(t, s.Path(), []byte("hello"))
		var f *result.Failure
		if !errors.As(err, &f) {
			t.Fatalf("err = %v, want *result.Failure", err)
		}
		if f.Code != result.CodeSessionNotReady {
			t.Fatalf("code = %s, want %s", f.Code, result.CodeSessionNotReady)
		}
		if got := f.Details["uid"]; got != strconv.FormatUint(uint64(uid), 10) {
			t.Fatalf("details uid = %q, want %d", got, uid)
		}
		if seen != 0 {
			t.Fatalf("handler calls = %d, want 0", seen)
		}
	})

	t.Run("matching uid is served", func(t *testing.T) {
		var seen int
		s, _, _ := serve(t, Config{ExpectedUID: uid}, echo(t, &seen))
		got, err := call(t, s.Path(), []byte("hello"))
		if err != nil {
			t.Fatalf("Call: %v", err)
		}
		if !bytes.Equal(got, []byte("hello")) {
			t.Fatalf("payload = %q", got)
		}
		if seen != 1 {
			t.Fatalf("handler calls = %d, want 1", seen)
		}
	})
}

func TestListenModes(t *testing.T) {
	cases := []struct {
		name       string
		dirMode    os.FileMode
		socketMode os.FileMode
		wantDir    os.FileMode
		wantSocket os.FileMode
	}{
		{"defaults", 0, 0, 0o700, 0o600},
		{"explicit", 0o750, 0o640, 0o750, 0o640},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "run")
			path := filepath.Join(dir, "control.sock")
			s, err := Listen(Config{Path: path, DirMode: tc.dirMode, SocketMode: tc.socketMode})
			if err != nil {
				t.Fatalf("Listen: %v", err)
			}
			defer s.Close()

			dirInfo, err := os.Stat(dir)
			if err != nil {
				t.Fatalf("stat dir: %v", err)
			}
			if got := dirInfo.Mode().Perm(); got != tc.wantDir {
				t.Fatalf("dir mode = %#o, want %#o", got, tc.wantDir)
			}
			sockInfo, err := os.Stat(path)
			if err != nil {
				t.Fatalf("stat socket: %v", err)
			}
			if got := sockInfo.Mode().Perm(); got != tc.wantSocket {
				t.Fatalf("socket mode = %#o, want %#o", got, tc.wantSocket)
			}
		})
	}

	t.Run("world writable directory is refused", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "unsafe")
		if err := os.MkdirAll(dir, 0o777); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.Chmod(dir, 0o777); err != nil {
			t.Fatalf("Chmod: %v", err)
		}
		if _, err := Listen(Config{Path: filepath.Join(dir, "control.sock")}); err == nil {
			t.Fatal("Listen accepted a world-writable directory")
		}
	})
}

func TestServeShutdown(t *testing.T) {
	t.Run("Close stops Serve and removes the socket", func(t *testing.T) {
		var seen int
		s, _, wait := serve(t, Config{}, echo(t, &seen))
		if err := s.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if err := wait(); err != nil {
			t.Fatalf("Serve returned %v, want nil", err)
		}
		if _, err := os.Stat(s.Path()); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("socket still present: %v", err)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("second Close: %v", err)
		}
	})

	t.Run("context cancellation stops Serve", func(t *testing.T) {
		var seen int
		_, cancel, wait := serve(t, Config{}, echo(t, &seen))
		cancel()
		if err := wait(); err != nil {
			t.Fatalf("Serve returned %v, want nil", err)
		}
	})
}

func TestSequentialConnections(t *testing.T) {
	var seen int
	s, _, _ := serve(t, Config{}, echo(t, &seen))
	for _, payload := range []string{"one", "two"} {
		got, err := call(t, s.Path(), []byte(payload))
		if err != nil {
			t.Fatalf("Call(%q): %v", payload, err)
		}
		if !bytes.Equal(got, []byte(payload)) {
			t.Fatalf("payload = %q, want %q", got, payload)
		}
	}
	if seen != 2 {
		t.Fatalf("handler calls = %d, want 2", seen)
	}
}

func TestHandlerFailure(t *testing.T) {
	failure := result.NewFailure(result.CodeWindowNotFound, "activate", "no such window %q", "abc")
	h := func(_ context.Context, request []byte) ([]byte, error) {
		if string(request) == "{}" {
			return nil, failure
		}
		return request, nil
	}
	s, _, _ := serve(t, Config{}, h)

	_, err := call(t, s.Path(), []byte("{}"))
	var f *result.Failure
	if !errors.As(err, &f) {
		t.Fatalf("err = %v, want *result.Failure", err)
	}
	if f.Code != failure.Code || f.Message != failure.Message || f.Operation != failure.Operation {
		t.Fatalf("failure = %+v, want %+v", f, failure)
	}

	// The connection was still closed cleanly and the server keeps serving.
	got, err := call(t, s.Path(), []byte("next"))
	if err != nil {
		t.Fatalf("Call after failure: %v", err)
	}
	if !bytes.Equal(got, []byte("next")) {
		t.Fatalf("payload = %q", got)
	}
}

func TestCancelledContextUnblocksIdleClient(t *testing.T) {
	var seen int
	s, cancel, wait := serve(t, Config{}, echo(t, &seen))

	conn, err := net.Dial("unix", s.Path())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	// Say nothing at all, then cancel: Serve must still return promptly.
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := wait(); err != nil {
		t.Fatalf("Serve returned %v, want nil", err)
	}
}
