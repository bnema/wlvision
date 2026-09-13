package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/bnema/wlvision/internal/result"
)

// maxStderrDetails bounds the engine diagnostic kept for one failed command.
// A failing engine can print an unbounded amount of text, and the whole point
// of the bound is that the failure stays readable in a JSON envelope.
const maxStderrDetails = 4096

// Options configures the Docker adapter.
type Options struct {
	// Context selects a Docker CLI context. Empty means the user's current
	// context, which the adapter records in the capability report.
	Context string
}

// Docker drives a Docker CLI context. It uses the CLI rather than the API so
// the user's rootless context, socket, and credentials keep working unchanged.
type Docker struct {
	runner  CommandRunner
	context string
}

// Kind implements Engine.
func (d *Docker) Kind() Kind { return KindDocker }

// NewDocker builds the adapter. The runner is required and is the only way the
// adapter can reach the engine, which is what makes the argument lists it
// builds testable without a daemon.
func NewDocker(runner CommandRunner, options Options) (*Docker, error) {
	if runner == nil {
		return nil, errors.New("engine: a command runner is required")
	}
	if options.Context != "" && isFlagLike(options.Context) {
		return nil, fmt.Errorf("engine: invalid context name %q", options.Context)
	}
	return &Docker{runner: runner, context: options.Context}, nil
}

// dockerInfo is the subset of `docker info --format '{{json .}}'` wlvision
// reads. The engine reports the protection flags and the delegated resource
// controllers here, so a session never assumes a limit is in force.
type dockerInfo struct {
	ServerVersion   string   `json:"ServerVersion"`
	SecurityOptions []string `json:"SecurityOptions"`
	CgroupVersion   string   `json:"CgroupVersion"`
	CgroupDriver    string   `json:"CgroupDriver"`
	MemoryLimit     bool     `json:"MemoryLimit"`
	PidsLimit       bool     `json:"PidsLimit"`
}

// Capabilities implements Engine.
func (d *Docker) Capabilities(ctx context.Context) (Capabilities, error) {
	name, err := d.contextName(ctx)
	if err != nil {
		return Capabilities{}, err
	}

	out, err := d.output(ctx, name, "engine.capabilities", "info", "--format", "{{json .}}")
	if err != nil {
		return Capabilities{}, err
	}

	var info dockerInfo
	if err := json.Unmarshal(out, &info); err != nil {
		return Capabilities{}, result.NewFailure(result.CodeEngineUnavailable, "engine.capabilities",
			"cannot read the engine report: %v", err)
	}

	capabilities := Capabilities{
		Kind:          KindDocker,
		Context:       name,
		ServerVersion: info.ServerVersion,
		CgroupVersion: info.CgroupVersion,
		CgroupDriver:  info.CgroupDriver,
		MemoryLimit:   info.MemoryLimit,
		PidsLimit:     info.PidsLimit,
	}
	for _, option := range info.SecurityOptions {
		fields := parseSecurityOption(option)
		switch {
		case fields["name"] == "rootless":
			capabilities.Rootless = true
		case fields["name"] == "seccomp":
			capabilities.SeccompProfile = fields["profile"]
			if capabilities.SeccompProfile == "" {
				// The engine applies a profile but does not name it.
				capabilities.SeccompProfile = "default"
			}
		}
	}

	if err := capabilities.Validate(); err != nil {
		return Capabilities{}, err
	}
	return capabilities, nil
}

// Create implements Engine. Every flag here is fixed policy; the spec cannot
// add, replace, or drop one.
func (d *Docker) Create(ctx context.Context, spec CreateSpec) (string, error) {
	if err := spec.validate(); err != nil {
		return "", err
	}

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

	// The built-in seccomp profile is Docker's default and is verified from
	// the capability report, so the adapter must never disable it.
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
	args = append(args, spec.Command...)

	out, err := d.output(ctx, d.context, "engine.create", args...)
	if err != nil {
		return "", err
	}

	id := strings.TrimSpace(string(out))
	if id == "" {
		return "", result.NewFailure(result.CodeEngineUnavailable, "engine.create",
			"the engine created a container but reported no identifier")
	}
	return id, nil
}

// Start implements Engine. A container that is already running is the desired
// state, so the engine's refusal is treated as success; that makes a retried
// session creation converge instead of failing on a stale record.
func (d *Docker) Start(ctx context.Context, id string) error {
	if id == "" {
		return usageFailure("engine.start", "a container identifier is required")
	}

	stderr, err := d.run(ctx, d.context, "engine.start", Command{Args: d.args("start", id)})
	if err == nil {
		return nil
	}
	if strings.Contains(stderr.String(), "is already running") {
		return nil
	}
	return err
}

// Exec implements Engine. The command's exit status belongs to the caller, so
// it is returned as a result; only a failure to run the command at all is an
// engine error.
func (d *Docker) Exec(ctx context.Context, spec ExecSpec) (ExecResult, error) {
	if spec.ContainerID == "" || len(spec.Argv) == 0 {
		return ExecResult{}, usageFailure("engine.exec", "a container identifier and a command are required")
	}

	args := []string{"exec", "-i", "--user", strconv.FormatUint(uint64(spec.User), 10)}
	if spec.WorkDir != "" {
		args = append(args, "--workdir", spec.WorkDir)
	}
	for _, entry := range spec.Env {
		args = append(args, "-e", entry)
	}
	args = append(args, spec.ContainerID)
	args = append(args, spec.Argv...)

	stderr, merged := captureStderr(spec.Stderr)
	err := d.runner.Run(ctx, Command{
		Args:   d.args(args...),
		Stdin:  spec.Stdin,
		Stdout: spec.Stdout,
		Stderr: merged,
	})

	var exit *ExitError
	switch {
	case err == nil:
		return ExecResult{}, nil
	case errors.As(err, &exit):
		// The command ran: its status is the caller's, not a failure to reach
		// the engine.
		return ExecResult{ExitCode: exit.Code}, nil
	default:
		return ExecResult{}, commandFailure("engine.exec", stderr.String(), err)
	}
}

// Stream implements Engine.
func (d *Docker) Stream(ctx context.Context, spec StreamSpec) error {
	if spec.ContainerID == "" || len(spec.Argv) == 0 {
		return usageFailure("engine.stream", "a container identifier and a command are required")
	}
	if spec.Source == nil {
		return usageFailure("engine.stream", "a source reader is required")
	}

	args := []string{"exec", "-i", "--user", strconv.FormatUint(uint64(spec.User), 10), spec.ContainerID}
	args = append(args, spec.Argv...)

	_, err := d.run(ctx, d.context, "engine.stream", Command{
		Args:   d.args(args...),
		Stdin:  spec.Source,
		Stdout: spec.Stdout,
		Stderr: spec.Stderr,
	})
	return err
}

// State implements Engine.
func (d *Docker) State(ctx context.Context, id string) (ContainerState, error) {
	if id == "" {
		return ContainerState{}, usageFailure("engine.state", "a container identifier is required")
	}

	out, err := d.output(ctx, d.context, "engine.state", "inspect", "--format", "{{json .State}}", id)
	if err != nil {
		return ContainerState{}, missingContainer(err, id)
	}

	var reported struct {
		Running    bool   `json:"Running"`
		ExitCode   int    `json:"ExitCode"`
		StartedAt  string `json:"StartedAt"`
		FinishedAt string `json:"FinishedAt"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &reported); err != nil {
		return ContainerState{}, result.NewFailure(result.CodeEngineUnavailable, "engine.state",
			"cannot read the container state: %v", err)
	}

	state := ContainerState{Running: reported.Running, ExitCode: reported.ExitCode}
	if state.StartedAt, err = parseEngineTime(reported.StartedAt); err != nil {
		return ContainerState{}, result.NewFailure(result.CodeEngineUnavailable, "engine.state",
			"the container start time is not a timestamp: %v", err)
	}
	if state.FinishedAt, err = parseEngineTime(reported.FinishedAt); err != nil {
		return ContainerState{}, result.NewFailure(result.CodeEngineUnavailable, "engine.state",
			"the container finish time is not a timestamp: %v", err)
	}
	return state, nil
}

// Logs implements Engine.
func (d *Docker) Logs(ctx context.Context, id string, tail int, w io.Writer) error {
	if id == "" || w == nil {
		return usageFailure("engine.logs", "a container identifier and a writer are required")
	}
	if tail <= 0 {
		return usageFailure("engine.logs", "a positive tail is required")
	}

	_, err := d.run(ctx, d.context, "engine.logs", Command{
		Args:   d.args("logs", "--tail", strconv.Itoa(tail), id),
		Stdout: w,
	})
	return err
}

// Stop implements Engine.
func (d *Docker) Stop(ctx context.Context, id string, timeout time.Duration) error {
	if id == "" {
		return usageFailure("engine.stop", "a container identifier is required")
	}

	seconds := int(math.Ceil(timeout.Seconds()))
	if seconds < 1 {
		seconds = 1
	}

	_, err := d.run(ctx, d.context, "engine.stop", Command{
		Args: d.args("stop", "--time", strconv.Itoa(seconds), id),
	})
	if err != nil {
		return missingContainer(err, id)
	}
	return nil
}

// Remove implements Engine. It forces removal: a session being closed must not
// stay behind because its main process ignored a signal.
func (d *Docker) Remove(ctx context.Context, id string) error {
	if id == "" {
		return usageFailure("engine.remove", "a container identifier is required")
	}

	_, err := d.run(ctx, d.context, "engine.remove", Command{Args: d.args("rm", "--force", id)})
	if err != nil {
		return missingContainer(err, id)
	}
	return nil
}

// contextName resolves the context this adapter talks to. Naming it makes the
// session record complete even when the user relies on the current context.
func (d *Docker) contextName(ctx context.Context) (string, error) {
	if d.context != "" {
		return d.context, nil
	}

	out, err := d.output(ctx, "", "engine.capabilities", "context", "show")
	if err != nil {
		return "", err
	}
	name := strings.TrimSpace(string(out))
	if name == "" {
		return "", result.NewFailure(result.CodeEngineUnavailable, "engine.capabilities",
			"the engine reports no current context")
	}
	return name, nil
}

// args returns the argument list for a subcommand, pinned to a context when
// one is selected.
func (d *Docker) args(args ...string) []string {
	if d.context == "" {
		return args
	}
	return append([]string{"--context", d.context}, args...)
}

// output runs a command and returns its standard output.
func (d *Docker) output(ctx context.Context, contextName, operation string, args ...string) ([]byte, error) {
	if contextName != "" {
		args = append([]string{"--context", contextName}, args...)
	}

	var stdout bytes.Buffer
	if _, err := d.run(ctx, contextName, operation, Command{Args: args, Stdout: &stdout}); err != nil {
		return nil, err
	}
	return stdout.Bytes(), nil
}

// run executes one engine command, keeping a bounded copy of its diagnostics
// for whatever fails. The caller's own stderr, when it has one, still receives
// everything the engine printed.
func (d *Docker) run(ctx context.Context, contextName, operation string, cmd Command) (*boundedBuffer, error) {
	stderr, merged := captureStderr(cmd.Stderr)
	cmd.Stderr = merged

	if err := d.runner.Run(ctx, cmd); err != nil {
		return stderr, commandFailure(operation, stderr.String(), err)
	}
	return stderr, nil
}

// captureStderr returns the bounded diagnostic copy of a command's standard
// error together with the writer the engine should be given.
func captureStderr(caller io.Writer) (*boundedBuffer, io.Writer) {
	stderr := &boundedBuffer{limit: maxStderrDetails}
	if caller == nil {
		return stderr, stderr
	}
	return stderr, io.MultiWriter(caller, stderr)
}

// missingContainer reports the CLI's answer for a container that no longer
// exists. Unlike the engine API, the CLI has no structured reason, so its text
// is the only signal available; anything else is a real failure.
func missingContainer(err error, id string) error {
	var failure *result.Failure
	if !errors.As(err, &failure) || failure.Code != result.CodeEngineUnavailable {
		return err
	}

	stderr := strings.ToLower(failure.Details["stderr"])
	if strings.Contains(stderr, "no such object") || strings.Contains(stderr, "no such container") {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return err
}

// commandFailure turns an engine CLI failure into the typed failure the exit
// contract expects: a missed deadline is a timeout, anything else means the
// engine could not do what was asked.
func commandFailure(operation, stderr string, err error) *result.Failure {
	stderr = strings.TrimSpace(stderr)

	failure := result.NewFailure(result.CodeEngineUnavailable, operation,
		"the container engine command failed: %v", err)
	if errDeadline(err) {
		failure = result.NewFailure(result.CodeWaitTimeout, operation,
			"the container engine did not answer in time")
	}
	if stderr != "" {
		failure.Details = map[string]string{"stderr": stderr}
	}
	return failure
}

// parseSecurityOption splits one engine security option such as
// "name=seccomp,profile=builtin" into its fields.
func parseSecurityOption(option string) map[string]string {
	fields := make(map[string]string)
	for _, pair := range strings.Split(option, ",") {
		key, value, found := strings.Cut(pair, "=")
		if !found {
			continue
		}
		fields[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return fields
}

// parseEngineTime parses an engine timestamp, accepting the zero value the
// engine reports for a container that has not started or finished.
func parseEngineTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, value)
}

// option renders one tmpfs mount. Device nodes and setuid are always denied;
// execution is denied unless the mount explicitly allows it.
func (m Tmpfs) option() string {
	options := []string{
		"rw",
		"size=" + strconv.FormatInt(m.SizeBytes, 10),
		"mode=0" + strconv.FormatUint(uint64(m.Mode), 8),
	}
	if !m.Exec {
		options = append(options, "noexec")
	}
	options = append(options, "nosuid", "nodev")
	return strings.Join(options, ",")
}

// boundedBuffer keeps at most limit bytes and marks truncation, so an engine
// that prints megabytes cannot inflate a failure envelope.
type boundedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	room := b.limit - b.buf.Len()
	switch {
	case room <= 0:
		return len(p), nil
	case len(p) <= room:
		b.buf.Write(p)
		return len(p), nil
	case room <= len("..."):
		return len(p), nil
	default:
		b.buf.Write(p[:room-len("...")])
		b.buf.WriteString("...")
		return len(p), nil
	}
}

func (b *boundedBuffer) String() string { return b.buf.String() }
