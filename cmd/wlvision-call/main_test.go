package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/bnema/wlvision/internal/agentapi"
	"github.com/bnema/wlvision/internal/result"
	"github.com/bnema/wlvision/internal/session"
)

// fakeCaller records the one request it received and answers from a script.
type fakeCaller struct {
	request  []byte
	reply    []byte
	err      error
	closed   bool
	socket   string
	dialErr  error
	dialCall int
}

func (c *fakeCaller) Call(_ context.Context, request []byte) ([]byte, error) {
	c.request = append([]byte(nil), request...)
	return c.reply, c.err
}

func (c *fakeCaller) Close() error {
	c.closed = true
	return nil
}

func dialerFor(caller *fakeCaller) Dialer {
	return func(_ context.Context, socketPath string) (Caller, error) {
		caller.dialCall++
		caller.socket = socketPath
		if caller.dialErr != nil {
			return nil, caller.dialErr
		}
		return caller, nil
	}
}

func TestCallSendsOneRequestAndPrintsOneReply(t *testing.T) {
	caller := &fakeCaller{reply: []byte(`{"revision":7}`)}
	var stdout, stderr bytes.Buffer

	code := run(context.Background(), []string{"snapshot"}, &stdout, &stderr, dialerFor(caller))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	if stdout.String() != "{\"revision\":7}\n" {
		t.Errorf("stdout = %q, want exactly one reply document", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want nothing", stderr.String())
	}

	var request agentapi.Request
	if err := json.Unmarshal(caller.request, &request); err != nil {
		t.Fatalf("the request is not readable: %v", err)
	}
	if request.Operation != agentapi.OpSnapshot {
		t.Errorf("operation = %q, want %q", request.Operation, agentapi.OpSnapshot)
	}
	if !caller.closed {
		t.Error("the connection was left open")
	}
}

func TestCallPassesTheArgumentsThroughUntouched(t *testing.T) {
	caller := &fakeCaller{reply: []byte(`{"revision":7}`)}
	var stdout, stderr bytes.Buffer

	params := `{"handle":"app-1","revision":7,"width":400,"height":300}`
	code := run(context.Background(), []string{"--params", params, "resize"}, &stdout, &stderr, dialerFor(caller))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr.String())
	}

	var request agentapi.Request
	if err := json.Unmarshal(caller.request, &request); err != nil {
		t.Fatalf("the request is not readable: %v", err)
	}
	if string(request.Params) != params {
		t.Errorf("arguments = %s, want them passed through untouched", request.Params)
	}
}

func TestCallUsesTheConfiguredSocket(t *testing.T) {
	caller := &fakeCaller{reply: []byte(`{}`)}
	var stdout, stderr bytes.Buffer

	if code := run(context.Background(), []string{"snapshot"}, &stdout, &stderr, dialerFor(caller)); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if caller.socket != session.AgentSocket {
		t.Errorf("socket = %q, want the session's control socket", caller.socket)
	}

	if code := run(context.Background(), []string{"--socket", "/tmp/other.sock", "snapshot"}, &stdout, &stderr, dialerFor(caller)); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if caller.socket != "/tmp/other.sock" {
		t.Errorf("socket = %q, want the configured one", caller.socket)
	}
}

func TestCallRefusesAUsageProblemBeforeDialing(t *testing.T) {
	tests := map[string][]string{
		"no operation":      {},
		"two operations":    {"snapshot", "activate"},
		"unknown flag":      {"--teleport", "snapshot"},
		"invalid arguments": {"--params", "not json", "snapshot"},
	}

	for name, args := range tests {
		t.Run(name, func(t *testing.T) {
			caller := &fakeCaller{}
			var stdout, stderr bytes.Buffer

			code := run(context.Background(), args, &stdout, &stderr, dialerFor(caller))
			if code != result.CodeUsageError.ExitCode() {
				t.Fatalf("exit code = %d, want %d", code, result.CodeUsageError.ExitCode())
			}
			if caller.dialCall != 0 {
				t.Error("a usage problem reached the session")
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want nothing", stdout.String())
			}
			if !strings.Contains(stderr.String(), string(result.CodeUsageError)) {
				t.Errorf("stderr = %q, want a usage failure document", stderr.String())
			}
		})
	}
}

func TestCallReportsTransportAndControllerFailures(t *testing.T) {
	tests := []struct {
		name     string
		caller   *fakeCaller
		wantCode result.Code
	}{
		{
			name:     "the socket is not available",
			caller:   &fakeCaller{dialErr: result.NewFailure(result.CodeSessionNotReady, "call", "the control socket is not available to this user")},
			wantCode: result.CodeSessionNotReady,
		},
		{
			name:     "the controller refused the operation",
			caller:   &fakeCaller{err: result.NewFailure(result.CodeStaleRevision, "window.activate", "the layout moved")},
			wantCode: result.CodeStaleRevision,
		},
		{
			name:     "the controller stopped answering",
			caller:   &fakeCaller{err: context.DeadlineExceeded},
			wantCode: result.CodeWaitTimeout,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), []string{"snapshot"}, &stdout, &stderr, dialerFor(test.caller))

			if code != test.wantCode.ExitCode() {
				t.Fatalf("exit code = %d, want %d (stderr: %s)", code, test.wantCode.ExitCode(), stderr.String())
			}

			var envelope result.Envelope[any]
			if err := json.Unmarshal(stderr.Bytes(), &envelope); err != nil {
				t.Fatalf("the failure is not a document: %v (%q)", err, stderr.String())
			}
			if envelope.Ok || envelope.Error == nil || envelope.Error.Code != test.wantCode {
				t.Errorf("failure = %+v, want %s", envelope.Error, test.wantCode)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want nothing", stdout.String())
			}
		})
	}
}

func TestCallReportsAnUnreadableReply(t *testing.T) {
	caller := &fakeCaller{reply: []byte(`{"revision":`), err: errors.New("short read")}
	var stdout, stderr bytes.Buffer

	code := run(context.Background(), []string{"snapshot"}, &stdout, &stderr, dialerFor(caller))
	if code == 0 {
		t.Fatal("a truncated exchange was reported as success")
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing", stdout.String())
	}
}
