// Package engine adapts one container engine CLI to the narrow set of
// operations a wlvision session performs.
//
// Adapters own every engine flag they pass: a caller describes what it wants
// with the typed specs below, never with an argument list. The isolation
// contract of a session — a rootless engine, no network, a read-only root, no
// capabilities, no new privileges, the built-in seccomp profile, separate
// control and application identities, and bounded tmpfs mounts — is therefore
// stated once, in the adapter, and cannot be weakened from the outside.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/bnema/wlvision/internal/result"
)

// Kind names a container engine implementation.
type Kind string

// KindDocker is the engine selected by the Docker adapter.
const KindDocker Kind = "docker"

// KindPodman is the engine selected by the Podman adapter.
const KindPodman Kind = "podman"

// LabelSession marks every container wlvision creates, so a session record can
// be reconciled against engine state after a crash.
const LabelSession = "wlvision.session"

// Degradations are stable identifiers for protections an engine cannot
// enforce. They are reported per session instead of being silently ignored.
const (
	// DegradationCgroupV1 means the engine uses cgroup v1, where per-container
	// resource control depends on the host's controller setup.
	DegradationCgroupV1 = "cgroup_v1"
	// DegradationMemoryLimit means the memory controller is not delegated, so
	// a session's memory limit cannot be enforced.
	DegradationMemoryLimit = "memory_limit_unavailable"
	// DegradationPidsLimit means the pids controller is not delegated, so a
	// session's process limit cannot be enforced.
	DegradationPidsLimit = "pids_limit_unavailable"
)

// Environment keys the supervisor reads inside the container. The adapter
// derives them from the create spec so the two identities cannot drift apart.
const (
	EnvSession        = "WLVISION_SESSION"
	EnvControlUID     = "WLVISION_CONTROL_UID"
	EnvApplicationUID = "WLVISION_APPLICATION_UID"
)

// Capabilities describes what an engine can actually enforce. It is recorded
// in session metadata and checked before a session is created.
type Capabilities struct {
	Kind          Kind   `json:"kind"`
	Context       string `json:"context"`
	ServerVersion string `json:"server_version"`
	Rootless      bool   `json:"rootless"`
	// SeccompProfile is the name of the profile the engine applies, empty when
	// it applies none.
	SeccompProfile string `json:"seccomp_profile"`
	CgroupVersion  string `json:"cgroup_version"`
	CgroupDriver   string `json:"cgroup_driver"`
	// MemoryLimit and PidsLimit report whether the corresponding controller is
	// delegated to the rootless user.
	MemoryLimit bool `json:"memory_limit"`
	PidsLimit   bool `json:"pids_limit"`
}

// Validate refuses an engine wlvision cannot confine a session with. A rootful
// engine is a permanent configuration error; a missing seccomp profile is a
// missing prerequisite, which the exit-code contract maps to code 3.
func (c Capabilities) Validate() error {
	if !c.Rootless {
		return result.NewFailure(result.CodeEngineNotRootless, "engine.capabilities",
			"the container engine runs as root; wlvision requires a rootless engine and has no override")
	}
	if c.SeccompProfile == "" {
		failure := result.NewFailure(result.CodeProtectionDegraded, "engine.capabilities",
			"the container engine provides no seccomp profile, so a session cannot be confined")
		failure.Details = map[string]string{"missing": "seccomp"}
		return failure
	}
	return nil
}

// Degradations lists the protections this engine cannot enforce, in a stable
// order. An operator reads them as warnings; the session records them.
func (c Capabilities) Degradations() []string {
	var degradations []string
	if c.CgroupVersion != "" && c.CgroupVersion != "2" {
		degradations = append(degradations, DegradationCgroupV1)
	}
	if !c.MemoryLimit {
		degradations = append(degradations, DegradationMemoryLimit)
	}
	if !c.PidsLimit {
		degradations = append(degradations, DegradationPidsLimit)
	}
	return degradations
}

// MissingLimits reports which of the requested limits this engine cannot
// enforce, so a caller can refuse the request or record the gap. A zero limit
// asks for nothing and is never reported. File-size and open-file limits are
// per-process rlimits and are always enforceable.
func (c Capabilities) MissingLimits(limits Limits) []string {
	var missing []string
	if limits.MemoryBytes > 0 && !c.MemoryLimit {
		missing = append(missing, DegradationMemoryLimit)
	}
	if limits.Pids > 0 && !c.PidsLimit {
		missing = append(missing, DegradationPidsLimit)
	}
	return missing
}

// Limits bounds one session. Values are absolute; zero means "do not set".
// The wall-clock limit is enforced by the session service, not by the engine.
type Limits struct {
	MemoryBytes   int64 `json:"memory_bytes"`
	Pids          int   `json:"pids"`
	FileSizeBytes int64 `json:"file_size_bytes"`
	OpenFiles     int   `json:"open_files"`
}

// Tmpfs is one bounded, memory-backed mount. Device nodes are always denied
// and the mount is always read-write with setuid denied.
type Tmpfs struct {
	Path      string
	SizeBytes int64
	Mode      uint32
	// Exec allows executing files from the mount. Injected payloads live under
	// /run, so that mount allows execution while the others do not.
	Exec bool
}

// CreateSpec describes the container of one session. It deliberately has no
// field for network mode, mounts, capabilities, or engine flags: those are the
// adapter's fixed policy.
type CreateSpec struct {
	Image   string
	Name    string
	Session string
	// ControlUID runs Weston, the supervisor, and the resident controller.
	ControlUID uint32
	ControlGID uint32
	// ApplicationUID runs injected applications and can reach the display, but
	// cannot bind the control protocols or open the controller socket.
	ApplicationUID uint32
	ApplicationGID uint32
	// Command is the container's PID 1: the supervisor.
	Command []string
	Tmpfs   []Tmpfs
	Limits  Limits
}

func (s CreateSpec) validate() error {
	switch {
	case s.Image == "":
		return usageFailure("engine.create", "a session image is required")
	case s.Name == "":
		return usageFailure("engine.create", "a container name is required")
	case s.Session == "":
		return usageFailure("engine.create", "a session identifier is required")
	case len(s.Command) == 0:
		return usageFailure("engine.create", "the session needs a supervisor command")
	case s.ControlUID == s.ApplicationUID:
		return usageFailure("engine.create", "the control and application identities must differ")
	}
	for _, mount := range s.Tmpfs {
		if !strings.HasPrefix(mount.Path, "/") || mount.SizeBytes <= 0 {
			return usageFailure("engine.create", "tmpfs mount %q needs an absolute path and a size", mount.Path)
		}
	}
	return nil
}

// createArgs renders the container argument list every adapter passes to its
// engine. It is the isolation contract of a session in argument form — no
// network, a read-only root, no capabilities, no new privileges, separate
// control and application identities, and bounded tmpfs mounts — stated once
// so that no engine can be talked into weakening a session.
func createArgs(spec CreateSpec) []string {
	args := []string{
		"create",
		"--name", spec.Name,
		"--label", LabelSession + "=" + spec.Session,
		"--network", "none",
		"--read-only",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--user", fmt.Sprintf("%d:%d", spec.ControlUID, spec.ControlGID),
	}

	// Both engines apply their built-in seccomp profile by default and report
	// it through Capabilities, so neither adapter may disable it here.

	if spec.Limits.MemoryBytes > 0 {
		args = append(args, "--memory", strconv.FormatInt(spec.Limits.MemoryBytes, 10))
	}
	if spec.Limits.Pids > 0 {
		args = append(args, "--pids-limit", strconv.Itoa(spec.Limits.Pids))
	}
	if spec.Limits.FileSizeBytes > 0 {
		args = append(args, "--ulimit", "fsize="+strconv.FormatInt(spec.Limits.FileSizeBytes, 10))
	}
	if spec.Limits.OpenFiles > 0 {
		args = append(args, "--ulimit", "nofile="+strconv.Itoa(spec.Limits.OpenFiles))
	}
	for _, mount := range spec.Tmpfs {
		args = append(args, "--tmpfs", mount.Path+":"+mount.option())
	}

	args = append(args,
		"-e", EnvSession+"="+spec.Session,
		"-e", EnvControlUID+"="+strconv.FormatUint(uint64(spec.ControlUID), 10),
		"-e", EnvApplicationUID+"="+strconv.FormatUint(uint64(spec.ApplicationUID), 10),
		spec.Image,
	)
	return append(args, spec.Command...)
}

// validateBuildSpec refuses a build wlvision cannot reproduce: the context is
// the generated directory, the Containerfile is a plain name inside it, and
// the tag is explicit.
func validateBuildSpec(spec BuildSpec) error {
	switch {
	case spec.Context == "" || !filepath.IsAbs(spec.Context):
		return usageFailure("engine.build", "the build context must be an absolute directory")
	case spec.Tag == "":
		return usageFailure("engine.build", "an image tag is required")
	case spec.Containerfile == "":
		return usageFailure("engine.build", "a Containerfile name is required")
	}
	if filepath.IsAbs(spec.Containerfile) || spec.Containerfile == "." || spec.Containerfile == ".." || strings.ContainsAny(spec.Containerfile, `/:\\`) {
		return usageFailure("engine.build",
			"the Containerfile %q is not a name inside the build context", spec.Containerfile)
	}
	return nil
}

// ExecSpec runs one command in an existing session under an explicit identity.
type ExecSpec struct {
	ContainerID string
	User        uint32
	WorkDir     string
	Env         []string
	Argv        []string
	Stdin       io.Reader
	Stdout      io.Writer
	Stderr      io.Writer
}

// StreamSpec pipes a caller's stream into a container command, which is how a
// payload reaches the session without ever becoming a host file.
type StreamSpec struct {
	ContainerID string
	User        uint32
	Argv        []string
	Source      io.Reader
	Stdout      io.Writer
	Stderr      io.Writer
}

// ExecResult reports how a container command ended. A non-zero exit code is a
// result, not an engine failure: the command ran and said so.
type ExecResult struct {
	ExitCode int
}

// ErrNotFound reports that the engine no longer has the container. Cleanup
// treats it as success so a retried close converges.
var ErrNotFound = errors.New("engine: no such container")

// ExitError reports a command that ran and exited non-zero. Adapters that
// distinguish an application's status from an engine failure match on it.
type ExitError struct {
	Code int
	Err  error
}

// Error implements error.
func (e *ExitError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return fmt.Sprintf("exit status %d", e.Code)
}

// Unwrap exposes the underlying process error.
func (e *ExitError) Unwrap() error { return e.Err }

// ContainerState is the engine's view of a session container.
type ContainerState struct {
	Running    bool
	ExitCode   int
	StartedAt  time.Time
	FinishedAt time.Time
}

// Engine is the whole boundary between wlvision and a container engine.
type Engine interface {
	// Kind identifies the adapter.
	Kind() Kind
	// Capabilities inspects the engine and refuses one wlvision cannot use.
	Capabilities(ctx context.Context) (Capabilities, error)
	// Build builds an image from a generated context and returns its id. It is
	// the only operation that may use the network, and only for the build.
	Build(ctx context.Context, spec BuildSpec) (BuildResult, error)
	// ImageID reports the identifier the engine holds for a reference, which
	// is how a locally built image is pinned without a registry digest.
	ImageID(ctx context.Context, reference string) (string, error)
	// Create creates a stopped container and returns its identifier.
	Create(ctx context.Context, spec CreateSpec) (string, error)
	// Start starts a created container. Starting a running container succeeds.
	Start(ctx context.Context, id string) error
	// Exec runs a command in the container and waits for it. The command's own
	// exit status is returned as a result.
	Exec(ctx context.Context, spec ExecSpec) (ExecResult, error)
	// Stream pipes a reader into a container command.
	Stream(ctx context.Context, spec StreamSpec) error
	// State reports whether the container runs and how it exited.
	State(ctx context.Context, id string) (ContainerState, error)
	// Logs writes the container's bounded log tail.
	Logs(ctx context.Context, id string, tail int, w io.Writer) error
	// Stop asks the container to stop within the timeout.
	Stop(ctx context.Context, id string, timeout time.Duration) error
	// Remove deletes the container and everything it owns.
	Remove(ctx context.Context, id string) error
}

// Command is one engine CLI invocation.
type Command struct {
	Args   []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// CommandRunner runs one engine command. Adapters depend on this instead of
// os/exec so that argument construction is testable without a daemon, and so
// that a test can never create a container by accident.
type CommandRunner interface {
	Run(ctx context.Context, cmd Command) error
}

// CLIRunner executes engine commands with the engine's own command-line tool.
// Using the CLI keeps the user's existing rootless context and credentials in
// play and avoids depending on an engine SDK.
type CLIRunner struct {
	// Binary is the engine CLI, for example "docker".
	Binary string
}

// Run implements CommandRunner. It reports a command that ran and exited
// non-zero as an ExitError, so an adapter can tell an application's status
// apart from a failure to reach the engine.
func (r CLIRunner) Run(ctx context.Context, cmd Command) error {
	command := exec.CommandContext(ctx, r.Binary, cmd.Args...)
	command.Stdin = cmd.Stdin
	command.Stdout = cmd.Stdout
	command.Stderr = cmd.Stderr

	err := command.Run()
	if err == nil {
		return nil
	}

	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ProcessState != nil {
		if code := exit.ProcessState.ExitCode(); code > 0 {
			return &ExitError{Code: code, Err: err}
		}
	}
	return err
}

// usageFailure reports a programming error in wlvision itself: the caller
// built a spec the engine contract does not allow.
func usageFailure(operation, format string, args ...any) *result.Failure {
	return result.NewFailure(result.CodeUsageError, operation, format, args...)
}

// isFlagLike reports whether value could be read as an engine flag rather than
// a value, which is how an option would escape its position.
func isFlagLike(value string) bool {
	if value == "" || value == "-" || strings.HasPrefix(value, "--") {
		return true
	}
	for _, r := range value {
		if r == '-' || unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '.' {
			continue
		}
		return true
	}
	return false
}

var _ Engine = (*Docker)(nil)

// errDeadline reports whether an engine command hit its context deadline.
func errDeadline(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}
