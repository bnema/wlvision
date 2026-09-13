package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path"
	"strings"

	"github.com/bnema/wlvision/internal/agentapi"
	"github.com/bnema/wlvision/internal/engine"
	"github.com/bnema/wlvision/internal/result"
)

// MaxFetchBytes bounds one capture read out of a session. A capture is the
// session's own pixels, so the bound is generous; it exists so that a
// compromised or confused controller cannot stream an unbounded artifact into
// the host through a command that promises a picture.
const MaxFetchBytes int64 = 64 << 20

// maxExportNameLength bounds the name of one stored capture.
const maxExportNameLength = 200

// Call exchanges one operation with a session's resident controller.
//
// The outer CLI never holds a privileged Wayland connection. It runs the
// image's one-shot caller as the control identity, hands it one request
// document, and reads the one reply document it prints. A refusal the
// controller reported travels back as a *result.Failure, so the caller sees
// the code the controller assigned rather than a transport error.
func (s *Service) Call(ctx context.Context, id, operation string, params agentapi.Params) (agentapi.Reply, error) {
	operation = strings.TrimSpace(operation)
	if operation == "" {
		return agentapi.Reply{}, result.NewFailure(result.CodeUsageError, "session.call", "an operation is required")
	}

	record, err := s.store.Load(id)
	if err != nil {
		return agentapi.Reply{}, err
	}
	if !record.State.Interactive() {
		return agentapi.Reply{}, result.NewFailure(result.CodeSessionNotReady, operation,
			"session %q is %s and cannot accept %s", id, record.State, operation)
	}

	encoded, err := json.Marshal(params)
	if err != nil {
		return agentapi.Reply{}, result.NewFailure(result.CodeUsageError, operation,
			"cannot encode the arguments: %v", err)
	}

	// The caller takes its flags before the operation, like every other
	// command in this project.
	argv := []string{CallPath, "--socket", AgentSocket, "--params", string(encoded), operation}

	var stdout, stderr bytes.Buffer
	execution, err := s.engine.Exec(ctx, engine.ExecSpec{
		ContainerID: record.ContainerID,
		User:        s.controlUID,
		Argv:        argv,
		Stdout:      &stdout,
		Stderr:      &stderr,
	})
	if err != nil {
		return agentapi.Reply{}, err
	}
	if execution.ExitCode != 0 {
		if failure := decodeFailure(stderr.Bytes()); failure != nil {
			return agentapi.Reply{}, bindFailure(failure, operation, id)
		}
		return agentapi.Reply{}, result.NewFailure(result.CodeSessionNotReady, operation,
			"the session's controller exited %d: %s", execution.ExitCode, firstLine(stderr.String()))
	}

	var reply agentapi.Reply
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &reply); err != nil {
		return agentapi.Reply{}, result.NewFailure(result.CodeSessionNotReady, operation,
			"the session's controller answered with an unreadable reply: %v", err)
	}
	return reply, nil
}

// Record returns the stored record of one session without reconciling it
// against the engine.
//
// It exists for a caller that polls a cheap signal, such as the recorded exit
// of an application, where asking the engine on every poll would be wasteful.
func (s *Service) Record(id string) (Record, error) { return s.store.Load(id) }

// StateRoot reports the directory the session records live under. It is also
// where wlvision keeps the artifacts an agent asks for, so a caller resolves an
// export tree against it rather than recomputing the default.
func (s *Service) StateRoot() string { return s.store.Root() }

// Caller returns the resident controller of one session, reached one operation
// at a time.
//
// The interaction and temporal-vision services depend on agentapi.Caller, so
// binding the session identifier here is what keeps them unaware of the engine,
// the store, and the container boundary.
func (s *Service) Caller(id string) *Caller { return &Caller{service: s, session: id} }

// Caller is one session's resident controller.
type Caller struct {
	service *Service
	session string
}

// Session reports which session the caller is bound to.
func (c *Caller) Session() string { return c.session }

// Call implements agentapi.Caller.
func (c *Caller) Call(ctx context.Context, operation string, params agentapi.Params) (agentapi.Reply, error) {
	return c.service.Call(ctx, c.session, operation, params)
}

var _ agentapi.Caller = (*Caller)(nil)

// Fetch copies one stored capture out of a session.
//
// requested is a name inside the session's export directory: the same name the
// capture was asked for, never a path the controller reported back. Confining
// the read to that directory is what keeps a capture reply from turning into an
// arbitrary read of the container's filesystem.
func (s *Service) Fetch(ctx context.Context, id, requested string, w io.Writer) error {
	if w == nil {
		return result.NewFailure(result.CodeUsageError, "capture.fetch", "a destination writer is required")
	}

	record, err := s.store.Load(id)
	if err != nil {
		return err
	}
	if !record.State.Interactive() {
		return result.NewFailure(result.CodeSessionNotReady, "capture.fetch",
			"session %q is %s and holds no readable capture", id, record.State)
	}

	name, err := exportName(requested)
	if err != nil {
		return err
	}

	var stderr bytes.Buffer
	destination := &boundedWriter{w: w, remaining: MaxFetchBytes}
	execution, err := s.engine.Exec(ctx, engine.ExecSpec{
		ContainerID: record.ContainerID,
		User:        s.controlUID,
		Argv:        []string{"cat", path.Join(ExportDir, name)},
		Stdout:      destination,
		Stderr:      &stderr,
	})
	if err != nil {
		if destination.exceeded {
			return result.NewFailure(result.CodeCaptureFailed, "capture.fetch",
				"the stored capture exceeds the %d byte limit", MaxFetchBytes)
		}
		return err
	}
	if execution.ExitCode != 0 {
		if failure := decodeFailure(stderr.Bytes()); failure != nil {
			return bindFailure(failure, "capture.fetch", id)
		}
		return result.NewFailure(result.CodeCaptureFailed, "capture.fetch",
			"the session holds no capture named %q: %s", name, firstLine(stderr.String()))
	}
	return nil
}

// exportName validates a name inside a session's export directory.
//
// The rules are the strict form of a relative path: no absolute path, no
// parent traversal, no empty name, no name that a reader could mistake for one
// of its own flags.
func exportName(requested string) (string, error) {
	name := strings.TrimSpace(requested)
	switch {
	case name == "":
		return "", result.NewFailure(result.CodeUsageError, "capture.fetch", "a capture name is required")
	case strings.ContainsRune(name, 0):
		return "", result.NewFailure(result.CodeUsageError, "capture.fetch",
			"the capture name contains a NUL byte")
	case path.IsAbs(name):
		return "", result.NewFailure(result.CodeUsageError, "capture.fetch",
			"the capture name %q must be relative to the session export directory", requested)
	case strings.HasPrefix(name, "-"):
		return "", result.NewFailure(result.CodeUsageError, "capture.fetch",
			"the capture name %q must not begin with a dash", requested)
	}

	cleaned := path.Clean(name)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", result.NewFailure(result.CodeUsageError, "capture.fetch",
			"the capture name %q leaves the session export directory", requested)
	}
	if len(cleaned) > maxExportNameLength {
		return "", result.NewFailure(result.CodeUsageError, "capture.fetch",
			"the capture name is longer than %d bytes", maxExportNameLength)
	}
	return cleaned, nil
}

// bindFailure fills in the operation and session of a failure that travelled
// from another process, which reports neither.
func bindFailure(failure *result.Failure, operation, id string) *result.Failure {
	bound := *failure
	if bound.Operation == "" {
		bound.Operation = operation
	}
	if bound.Session == "" {
		bound.Session = id
	}
	return &bound
}

// firstLine returns the first line of a process's diagnostics, so one failure
// message stays one line.
func firstLine(text string) string {
	trimmed := strings.TrimSpace(text)
	if index := strings.IndexByte(trimmed, '\n'); index >= 0 {
		trimmed = trimmed[:index]
	}
	if trimmed == "" {
		return "no diagnostics"
	}
	return trimmed
}

// errBoundExceeded reports a read that hit the byte bound of a fetch.
var errBoundExceeded = errors.New("session: the fetched capture exceeds the byte limit")

// boundedWriter fails once more than remaining bytes have been written, so a
// stream whose size was not announced cannot exhaust the host.
type boundedWriter struct {
	w         io.Writer
	remaining int64
	exceeded  bool
}

func (b *boundedWriter) Write(payload []byte) (int, error) {
	if int64(len(payload)) > b.remaining {
		b.exceeded = true
		return 0, errBoundExceeded
	}
	written, err := b.w.Write(payload)
	b.remaining -= int64(written)
	return written, err
}
