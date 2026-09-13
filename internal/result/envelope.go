package result

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// Schema identifies the version of the envelope and error contract. Every
// document a renderer writes carries it, and callers must reject documents
// that carry a different one.
const Schema = "wlvision/v1"

// Envelope is the single result document every wlvision command and every
// session-service response speaks. It is generic in the payload so that a
// command can return its own typed result while the framing, the schema tag,
// and the error shape stay identical everywhere.
//
// Every field is deterministic: nothing here is derived from the clock, the
// environment, or map iteration order, so the marshalled form can be compared
// byte for byte in golden tests.
//
// The optional fields are documented with omitempty. For scalar result types
// (string, numbers, pointers, maps, slices, interfaces) encoding/json also
// omits the zero value, so a zero revision or an empty session never appears
// as an explicit null. For a struct result type encoding/json cannot omit the
// zero value and writes its zeroed form; callers that need a bare envelope
// should instantiate Envelope[...] with a pointer type or check the
// documented behaviour of their own result type.
type Envelope[T any] struct {
	Schema    string   `json:"schema"`
	Ok        bool     `json:"ok"`
	Operation string   `json:"operation"`
	Session   string   `json:"session,omitempty"`
	Revision  uint64   `json:"revision,omitempty"`
	Result    T        `json:"result,omitempty"`
	Error     *Failure `json:"error,omitempty"`
	// Warnings reports degraded protection: the operation succeeded, but one
	// of the hardening layers the operator asked for is not in force. It is
	// never a substitute for a failure.
	Warnings []string `json:"warnings,omitempty"`
}

// OK builds a success envelope for one operation. session may be empty when
// the command has no session, and revision may be zero when the operation does
// not touch compositor state; both are omitted from the JSON.
func OK[T any](operation string, session string, revision uint64, value T) Envelope[T] {
	return Envelope[T]{
		Schema:    Schema,
		Ok:        true,
		Operation: operation,
		Session:   session,
		Revision:  revision,
		Result:    value,
	}
}

// Fail builds a failure envelope for one operation. failure may be nil, in
// which case the envelope carries no error object; callers should prefer a
// described failure so that the code reaches the user.
func Fail[T any](operation string, session string, failure *Failure) Envelope[T] {
	return Envelope[T]{
		Schema:    Schema,
		Ok:        false,
		Operation: operation,
		Session:   session,
		Error:     failure,
	}
}

// Failure is the machine-readable description of one failed operation. The
// code is authoritative; the message is for humans and may change between
// releases.
type Failure struct {
	Code      Code   `json:"code"`
	Message   string `json:"message"`
	Operation string `json:"operation"`
	Session   string `json:"session,omitempty"`
	// Retriable tells a caller whether repeating the operation can succeed.
	// It is the caller's decision, seeded by NewFailure from Code.Fatal, and
	// it may be set freely after construction.
	Retriable bool `json:"retriable"`
	// Details carries small, stable key/value annotations such as "hint" or
	// "elapsed". Keys are sorted by encoding/json, so details do not affect
	// the determinism of the document.
	Details map[string]string `json:"details,omitempty"`
}

// Error implements error so a *Failure can travel through ordinary Go error
// plumbing. The text is stable: "<operation>: <code>: <message>", or
// "<code>: <message>" when the failure names no operation.
func (f *Failure) Error() string {
	if f == nil {
		return "result: nil failure"
	}
	if f.Operation != "" {
		return fmt.Sprintf("%s: %s: %s", f.Operation, f.Code, f.Message)
	}
	return fmt.Sprintf("%s: %s", f.Code, f.Message)
}

// NewFailure formats a failure. Retriable is seeded from !code.Fatal(); a
// caller that knows an exception may override the field before rendering.
func NewFailure(code Code, operation, format string, args ...any) *Failure {
	return &Failure{
		Code:      code,
		Message:   fmt.Sprintf(format, args...),
		Operation: operation,
		Retriable: !code.Fatal(),
	}
}

// RenderJSON writes env as exactly one JSON document followed by one newline,
// indented by two spaces and without HTML escaping. Callers that promise JSON
// output write the envelope once per command and nothing else to stdout, so
// the stream holds a single document.
func RenderJSON[T any](w io.Writer, env Envelope[T]) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(env); err != nil {
		return fmt.Errorf("result: encoding envelope: %w", err)
	}
	return nil
}

// RenderHuman writes env as plain text for a terminal: a short success line
// naming the operation with the result, when it is not empty, marshalled
// beneath it; or "error: <code>: <message>" and, when details["hint"] is set
// to a non-empty value, one "hint:" line. The output carries no ANSI colour,
// no timestamp, and no warnings: a caller that wants to report degraded
// protection reads Envelope.Warnings itself and decides how loud to be.
//
// Nothing is written when the result cannot be marshalled, so a failed render
// never leaves half an envelope behind.
func RenderHuman[T any](w io.Writer, env Envelope[T]) error {
	if !env.Ok {
		return renderHumanFailure(w, env.Error)
	}
	body, err := humanResult(env.Result)
	if err != nil {
		return fmt.Errorf("result: rendering %s: %w", env.Operation, err)
	}
	line := "ok"
	if env.Operation != "" {
		line += ": " + env.Operation
	}
	if _, err := io.WriteString(w, line+"\n"+string(body)); err != nil {
		return err
	}
	return nil
}

func renderHumanFailure(w io.Writer, failure *Failure) error {
	if failure == nil {
		return fmt.Errorf("result: failure envelope without an error")
	}
	_, err := fmt.Fprintf(w, "error: %s: %s\n", failure.Code, failure.Message)
	if err != nil {
		return err
	}
	if hint := failure.Details["hint"]; hint != "" {
		if _, err := fmt.Fprintf(w, "hint: %s\n", hint); err != nil {
			return err
		}
	}
	return nil
}

// humanResult returns the indented, newline-terminated form of value, or nil
// when the value marshals to JSON null (no result to show).
func humanResult(value any) ([]byte, error) {
	probe, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if string(probe) == "null" {
		return nil, nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(value); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
