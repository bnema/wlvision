package result

// Code is the stable, machine-readable identifier of a failure. The JSON code
// is authoritative for callers: the CLI renders it verbatim and scripts switch
// on it, so the string values below are part of the public contract and must
// not change.
type Code string

// The complete code set of the wlvision/v1 contract.
const (
	// CodeEngineUnavailable means the container engine (podman) could not be
	// reached or started.
	CodeEngineUnavailable Code = "engine_unavailable"
	// CodeEngineNotRootless means the engine works but is not rootless, which
	// wlvision refuses to run on.
	CodeEngineNotRootless Code = "engine_not_rootless"
	// CodeProtectionDegraded means the session started without one of its
	// hardening layers. A successful operation reports it through
	// Envelope.Warnings; when it fails an operation, the requested protection
	// was unavailable, which is a prerequisite problem.
	CodeProtectionDegraded Code = "protection_degraded"
	// CodeImageUnavailable means the session image is missing and could not be
	// fetched.
	CodeImageUnavailable Code = "image_unavailable"
	// CodePayloadRejected means the request body or arguments were refused by
	// the session service.
	CodePayloadRejected Code = "payload_rejected"
	// CodeSessionNotReady means the session exists but cannot serve the
	// request yet.
	CodeSessionNotReady Code = "session_not_ready"
	// CodeWindowNotFound means the target window handle is unknown.
	CodeWindowNotFound Code = "window_not_found"
	// CodeStaleRevision means the request carried a revision the compositor
	// has already superseded.
	CodeStaleRevision Code = "stale_revision"
	// CodeCaptureFailed means capture was authorized but did not produce a
	// frame.
	CodeCaptureFailed Code = "capture_failed"
	// CodeWaitTimeout means a condition was not reached before its deadline.
	CodeWaitTimeout Code = "wait_timeout"
	// CodeProcessExited means an observed application process exited before
	// the operation completed.
	CodeProcessExited Code = "process_exited"
	// CodeWestonProtocolMismatch means the compositor does not speak the
	// Weston protocol version wlvision requires.
	CodeWestonProtocolMismatch Code = "weston_protocol_mismatch"
	// CodeUsageError means the command line itself was invalid.
	CodeUsageError Code = "usage_error"
)

// allCodes lists every declared code. Tests use it to prove the exit-code and
// fatal tables stay exhaustive when a code is added.
var allCodes = []Code{
	CodeEngineUnavailable,
	CodeEngineNotRootless,
	CodeProtectionDegraded,
	CodeImageUnavailable,
	CodePayloadRejected,
	CodeSessionNotReady,
	CodeWindowNotFound,
	CodeStaleRevision,
	CodeCaptureFailed,
	CodeWaitTimeout,
	CodeProcessExited,
	CodeWestonProtocolMismatch,
	CodeUsageError,
}

// ExitCode maps a code onto the process exit status the CLI must return.
//
// The rule, which is part of the wlvision/v1 contract:
//
//	0  success
//	2  invalid CLI input
//	3  unavailable prerequisite
//	4  session/application failure
//	5  timeout
//
// The JSON code is authoritative: a caller that needs to distinguish causes
// reads it rather than the exit status, which only selects the rendering and
// retry behaviour of the shell. Unknown codes are session/application
// failures (4) so that an unrecognized code can never masquerade as success,
// as invalid input, or as a timeout.
func (c Code) ExitCode() int {
	switch c {
	case CodeUsageError:
		return 2
	case CodeEngineUnavailable, CodeEngineNotRootless, CodeProtectionDegraded, CodeImageUnavailable,
		CodeWestonProtocolMismatch:
		return 3
	case CodeWaitTimeout:
		return 5
	case CodePayloadRejected, CodeSessionNotReady, CodeWindowNotFound,
		CodeStaleRevision, CodeCaptureFailed, CodeProcessExited:
		return 4
	default:
		return 4
	}
}

// Fatal reports whether a failure of this code is expected to be permanent for
// the current environment: retrying the same operation without external change
// cannot succeed.
//
// This is only a default. Some conditions are permanent for one caller and
// transient for another — an image that is missing now may be pulled by
// another command, a stale revision is never fixed by the server but may be
// fixed by a client re-reading state — so the caller decides the exception by
// setting Failure.Retriable. NewFailure seeds Retriable from !Fatal() and the
// caller may override that field afterwards.
func (c Code) Fatal() bool {
	switch c {
	case CodeEngineNotRootless, CodeImageUnavailable, CodeWestonProtocolMismatch, CodeUsageError,
		CodePayloadRejected, CodeWindowNotFound, CodeStaleRevision:
		return true
	default:
		return false
	}
}
