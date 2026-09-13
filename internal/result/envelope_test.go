package result

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

// Golden documents. RenderJSON and RenderHuman each end their output with a
// single newline, so the raw strings below do too. Nothing in these documents
// depends on the clock, the environment, or map iteration order.
const wantSuccessJSON = `{
  "schema": "wlvision/v1",
  "ok": true,
  "operation": "session.create",
  "session": "sess-1",
  "revision": 7,
  "result": {
    "id": "sess-1"
  }
}
`

const wantFailureJSON = `{
  "schema": "wlvision/v1",
  "ok": false,
  "operation": "window.activate",
  "session": "sess-1",
  "error": {
    "code": "window_not_found",
    "message": "no window \"0x1\"",
    "operation": "window.activate",
    "session": "sess-1",
    "retriable": false,
    "details": {
      "hint": "run wlvision windows"
    }
  }
}
`

const wantMinimalJSON = `{
  "schema": "wlvision/v1",
  "ok": true,
  "operation": "session.status"
}
`

const wantDegradedJSON = `{
  "schema": "wlvision/v1",
  "ok": false,
  "operation": "session.start",
  "error": {
    "code": "protection_degraded",
    "message": "seccomp profile unavailable",
    "operation": "session.start",
    "retriable": true
  },
  "warnings": [
    "protection degraded: seccomp profile unavailable"
  ]
}
`

const wantSuccessHuman = `ok: session.create
{
  "id": "sess-1"
}
`

const wantFailureHuman = `error: window_not_found: no window "0x1"
hint: run wlvision windows
`

func windowNotFound() *Failure {
	return &Failure{
		Code:      CodeWindowNotFound,
		Message:   `no window "0x1"`,
		Operation: "window.activate",
		Session:   "sess-1",
		Retriable: false,
		Details:   map[string]string{"hint": "run wlvision windows"},
	}
}

func TestRenderJSONSuccessGolden(t *testing.T) {
	env := OK("session.create", "sess-1", 7, map[string]string{"id": "sess-1"})
	var buf bytes.Buffer
	if err := RenderJSON(&buf, env); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	if got := buf.String(); got != wantSuccessJSON {
		t.Errorf("RenderJSON success mismatch\n got: %q\nwant: %q", got, wantSuccessJSON)
	}
}

func TestRenderJSONFailureGolden(t *testing.T) {
	env := Fail[any]("window.activate", "sess-1", windowNotFound())
	var buf bytes.Buffer
	if err := RenderJSON(&buf, env); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	if got := buf.String(); got != wantFailureJSON {
		t.Errorf("RenderJSON failure mismatch\n got: %q\nwant: %q", got, wantFailureJSON)
	}
}

func TestRenderJSONWarningsGolden(t *testing.T) {
	env := Fail[any]("session.start", "", NewFailure(CodeProtectionDegraded, "session.start", "seccomp profile unavailable"))
	env.Warnings = append(env.Warnings, "protection degraded: seccomp profile unavailable")
	var buf bytes.Buffer
	if err := RenderJSON(&buf, env); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	if got := buf.String(); got != wantDegradedJSON {
		t.Errorf("RenderJSON warnings mismatch\n got: %q\nwant: %q", got, wantDegradedJSON)
	}
}

// TestRenderJSONOneDocument pins the property the CLI depends on: one call
// writes exactly one JSON document, terminated by exactly one newline.
func TestRenderJSONOneDocument(t *testing.T) {
	tests := []struct {
		name string
		env  Envelope[any]
	}{
		{"success", OK[any]("session.create", "sess-1", 7, map[string]string{"id": "sess-1"})},
		{"failure", Fail[any]("window.activate", "sess-1", windowNotFound())},
		{"empty result", OK[any]("session.status", "", 0, nil)},
		{"newline inside a string", OK[any]("echo", "", 0, map[string]string{"text": "line1\nline2"})},
		{"nil failure", Fail[any]("session.stop", "", nil)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := RenderJSON(&buf, tt.env); err != nil {
				t.Fatalf("RenderJSON: %v", err)
			}
			assertOneDocument(t, buf.Bytes())
		})
	}
}

func assertOneDocument(t *testing.T, out []byte) {
	t.Helper()
	if len(out) == 0 {
		t.Fatal("RenderJSON wrote nothing")
	}
	if out[len(out)-1] != '\n' {
		t.Errorf("output does not end with a newline: %q", out)
	}
	if len(out) >= 2 && out[len(out)-2] == '\n' {
		t.Errorf("output ends with more than one newline: %q", out)
	}
	dec := json.NewDecoder(bytes.NewReader(out))
	var first json.RawMessage
	if err := dec.Decode(&first); err != nil {
		t.Fatalf("decoding first document: %v", err)
	}
	var second json.RawMessage
	if err := dec.Decode(&second); !errors.Is(err, io.EOF) {
		t.Errorf("stream holds more than one document: err=%v extra=%q", err, second)
	}
	if !json.Valid(first) {
		t.Errorf("first document is not valid JSON: %q", first)
	}
}

func TestRenderJSONDoesNotEscapeHTML(t *testing.T) {
	env := OK("echo", "", 0, map[string]string{"text": "<b>&amp;</b>"})
	var buf bytes.Buffer
	if err := RenderJSON(&buf, env); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, `"text": "<b>&amp;</b>"`) {
		t.Errorf("HTML characters were escaped: %q", got)
	}
	if strings.Contains(got, `\u003c`) || strings.Contains(got, `\u0026`) {
		t.Errorf("output contains HTML escapes: %q", got)
	}
}

func TestEnvelopeOmitsEmptyOptionalFields(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderJSON(&buf, OK[any]("session.status", "", 0, nil)); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	got := buf.String()
	if got != wantMinimalJSON {
		t.Errorf("minimal envelope mismatch\n got: %q\nwant: %q", got, wantMinimalJSON)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"session", "revision", "result", "error", "warnings"} {
		if _, ok := decoded[key]; ok {
			t.Errorf("optional field %q was emitted with %v", key, decoded[key])
		}
	}
	if strings.Contains(got, "null") {
		t.Errorf("output contains an explicit null: %q", got)
	}
}

func TestEnvelopeOmitsZeroRevision(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderJSON(&buf, OK("session.status", "sess-1", 0, map[string]string{"state": "ready"})); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	got := buf.String()
	var decoded map[string]any
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := decoded["revision"]; ok {
		t.Errorf("zero revision was emitted: %q", got)
	}
	if strings.Contains(got, "null") {
		t.Errorf("output contains an explicit null: %q", got)
	}
	if _, ok := decoded["session"]; !ok {
		t.Errorf("non-empty session was omitted: %q", got)
	}
}

func TestRenderHumanSuccessGolden(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderHuman(&buf, OK("session.create", "sess-1", 7, map[string]string{"id": "sess-1"})); err != nil {
		t.Fatalf("RenderHuman: %v", err)
	}
	if got := buf.String(); got != wantSuccessHuman {
		t.Errorf("RenderHuman success mismatch\n got: %q\nwant: %q", got, wantSuccessHuman)
	}
}

func TestRenderHumanFailureGolden(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderHuman(&buf, Fail[any]("window.activate", "sess-1", windowNotFound())); err != nil {
		t.Fatalf("RenderHuman: %v", err)
	}
	if got := buf.String(); got != wantFailureHuman {
		t.Errorf("RenderHuman failure mismatch\n got: %q\nwant: %q", got, wantFailureHuman)
	}
}

func TestRenderHumanDetails(t *testing.T) {
	tests := []struct {
		name string
		env  Envelope[any]
		want string
	}{
		{
			name: "no hint",
			env: Fail[any]("wait", "", &Failure{
				Code:    CodeWaitTimeout,
				Message: "timed out after 5s",
				Details: map[string]string{"elapsed": "5s"},
			}),
			want: "error: wait_timeout: timed out after 5s\n",
		},
		{
			name: "empty hint",
			env: Fail[any]("wait", "", &Failure{
				Code:    CodeWaitTimeout,
				Message: "timed out after 5s",
				Details: map[string]string{"hint": ""},
			}),
			want: "error: wait_timeout: timed out after 5s\n",
		},
		{
			name: "empty result",
			env:  OK[any]("session.stop", "", 0, nil),
			want: "ok: session.stop\n",
		},
		{
			name: "empty operation",
			env:  OK[any]("", "", 0, nil),
			want: "ok\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := RenderHuman(&buf, tt.env); err != nil {
				t.Fatalf("RenderHuman: %v", err)
			}
			if got := buf.String(); got != tt.want {
				t.Errorf("RenderHuman mismatch\n got: %q\nwant: %q", got, tt.want)
			}
			if strings.Contains(buf.String(), "\x1b[") {
				t.Errorf("human output contains ANSI colour: %q", buf.String())
			}
		})
	}
}

func TestRenderHumanUnrenderableResult(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderHuman(&buf, OK[any]("echo", "", 0, make(chan int))); err == nil {
		t.Errorf("RenderHuman(channel result) = nil error, want error")
	}
}

func TestCodeExitCode(t *testing.T) {
	tests := []struct {
		code Code
		want int
	}{
		{CodeUsageError, 2},
		{CodeEngineUnavailable, 3},
		{CodeEngineNotRootless, 3},
		{CodeImageUnavailable, 3},
		{CodeWestonProtocolMismatch, 3},
		{CodePayloadRejected, 4},
		{CodeSessionNotReady, 4},
		{CodeWindowNotFound, 4},
		{CodeStaleRevision, 4},
		{CodeCaptureFailed, 4},
		{CodeProcessExited, 4},
		{CodeProtectionDegraded, 3},
		{CodeWaitTimeout, 5},
	}
	if len(tests) != len(allCodes) {
		t.Fatalf("exit code table covers %d codes, package declares %d", len(tests), len(allCodes))
	}
	covered := make(map[Code]bool, len(tests))
	for _, tt := range tests {
		covered[tt.code] = true
		if got := tt.code.ExitCode(); got != tt.want {
			t.Errorf("%q.ExitCode() = %d, want %d", tt.code, got, tt.want)
		}
	}
	for _, code := range allCodes {
		if !covered[code] {
			t.Errorf("code %q is not covered by the exit code table", code)
		}
	}
}

func TestCodeExitCodeUnknown(t *testing.T) {
	tests := []Code{"", "not_a_code", "engine_unavailable ", "ENGINE_UNAVAILABLE"}
	for _, code := range tests {
		if got := code.ExitCode(); got != 4 {
			t.Errorf("%q.ExitCode() = %d, want 4", code, got)
		}
	}
}

func TestCodeFatal(t *testing.T) {
	tests := []struct {
		code Code
		want bool
	}{
		{CodeEngineNotRootless, true},
		{CodeImageUnavailable, true},
		{CodeWestonProtocolMismatch, true},
		{CodeUsageError, true},
		{CodePayloadRejected, true},
		{CodeWindowNotFound, true},
		{CodeStaleRevision, true},
		{CodeEngineUnavailable, false},
		{CodeProtectionDegraded, false},
		{CodeSessionNotReady, false},
		{CodeCaptureFailed, false},
		{CodeWaitTimeout, false},
		{CodeProcessExited, false},
		{"not_a_code", false},
	}
	if len(tests) != len(allCodes)+1 {
		t.Fatalf("fatal table covers %d codes, want %d", len(tests), len(allCodes)+1)
	}
	for _, tt := range tests {
		if got := tt.code.Fatal(); got != tt.want {
			t.Errorf("%q.Fatal() = %v, want %v", tt.code, got, tt.want)
		}
	}
}

func TestFailureErrorText(t *testing.T) {
	tests := []struct {
		name string
		f    *Failure
		want string
	}{
		{
			name: "operation and message",
			f:    &Failure{Code: CodeWindowNotFound, Operation: "window.activate", Message: `no window "0x1"`},
			want: `window.activate: window_not_found: no window "0x1"`,
		},
		{
			name: "no operation",
			f:    &Failure{Code: CodeWaitTimeout, Message: "timed out"},
			want: "wait_timeout: timed out",
		},
		{
			name: "nil receiver",
			f:    nil,
			want: "result: nil failure",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.f.Error(); got != tt.want {
				t.Errorf("Error() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNewFailure(t *testing.T) {
	f := NewFailure(CodeCaptureFailed, "capture.screenshot", "wrote %d of %d bytes", 3, 10)
	if f.Code != CodeCaptureFailed {
		t.Errorf("Code = %q, want %q", f.Code, CodeCaptureFailed)
	}
	if f.Operation != "capture.screenshot" {
		t.Errorf("Operation = %q, want %q", f.Operation, "capture.screenshot")
	}
	if f.Message != "wrote 3 of 10 bytes" {
		t.Errorf("Message = %q, want %q", f.Message, "wrote 3 of 10 bytes")
	}
	if !f.Retriable {
		t.Errorf("Retriable = false, want true (capture_failed is not fatal by default)")
	}
	if f.Details != nil {
		t.Errorf("Details = %v, want nil", f.Details)
	}

	fatal := NewFailure(CodeUsageError, "usage", "unknown flag %q", "--nope")
	if fatal.Retriable {
		t.Errorf("usage_error Retriable = true, want false")
	}
	fatal.Retriable = true // the caller owns the exception
	if !fatal.Retriable {
		t.Errorf("caller override of Retriable did not stick")
	}
}
