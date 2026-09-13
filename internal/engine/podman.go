package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bnema/wlvision/internal/result"
)

var _ Engine = (*Podman)(nil)

// podmanLocalName is the endpoint identity the adapter records when the caller
// did not select a Podman connection. Podman has no `context show` command
// comparable to Docker's: without --connection the CLI talks to the user's own
// rootless socket, which the session record names "local" so the engine
// context is never empty.
const podmanLocalName = "local"

// Podman drives a Podman CLI. Like the Docker adapter it shells out to the
// engine's own command line, so the user's rootless configuration, socket, and
// credentials keep working unchanged. It builds the same argument vectors from
// the same typed specs, and translates Podman's own reporting into the shared
// Capabilities shape so no Podman-specific type escapes the adapter.
type Podman struct {
	runner     CommandRunner
	connection string
}

// NewPodman builds the adapter. The runner is required and is the only way the
// adapter can reach the engine. A non-empty Options.Context selects a Podman
// connection and is passed as --connection.
func NewPodman(runner CommandRunner, options Options) (*Podman, error) {
	if runner == nil {
		return nil, errors.New("engine: a command runner is required")
	}
	if options.Context != "" && isFlagLike(options.Context) {
		return nil, fmt.Errorf("engine: invalid connection name %q", options.Context)
	}
	return &Podman{runner: runner, connection: options.Context}, nil
}

// Kind implements Engine.
func (p *Podman) Kind() Kind { return KindPodman }

// podmanInfo is the subset of `podman info --format json` wlvision reads.
// Podman reports the protections under host.security and the delegated
// resource controllers under host.cgroupControllers.
type podmanInfo struct {
	Host struct {
		CgroupVersion string          `json:"cgroupVersion"`
		CgroupManager string          `json:"cgroupManager"`
		Controllers   json.RawMessage `json:"cgroupControllers"`
		Security      struct {
			Rootless           bool   `json:"rootless"`
			SeccompEnabled     bool   `json:"seccompEnabled"`
			SeccompProfilePath string `json:"seccompProfilePath"`
		} `json:"security"`
	} `json:"host"`
	Version struct {
		Version string `json:"Version"`
	} `json:"version"`
}

// Capabilities implements Engine. Every field is translated into the shared
// typed shape; a controller Podman does not report as delegated is reported as
// unavailable (a degradation), never as available.
func (p *Podman) Capabilities(ctx context.Context) (Capabilities, error) {
	out, err := p.output(ctx, "engine.capabilities", "info", "--format", "json")
	if err != nil {
		return Capabilities{}, err
	}

	var info podmanInfo
	if err := json.Unmarshal(out, &info); err != nil {
		return Capabilities{}, result.NewFailure(result.CodeEngineUnavailable, "engine.capabilities",
			"cannot read the engine report: %v", err)
	}

	capabilities := Capabilities{
		Kind:          KindPodman,
		Context:       p.contextName(),
		ServerVersion: info.Version.Version,
		Rootless:      info.Host.Security.Rootless,
		CgroupVersion: canonicalCgroupVersion(info.Host.CgroupVersion),
		CgroupDriver:  info.Host.CgroupManager,
	}

	// Podman names the profile by host path; the contract names the fact that
	// the engine applies its profile, so only the normalized name is reported.
	// A profile explicitly reported as unconfined is no profile at all.
	if info.Host.Security.SeccompEnabled {
		profile := strings.TrimSpace(info.Host.Security.SeccompProfilePath)
		if !strings.Contains(strings.ToLower(profile), "unconfined") {
			capabilities.SeccompProfile = "default"
		}
	}

	controllers := podmanControllers(info.Host.Controllers)
	capabilities.MemoryLimit = controllers["memory"]
	capabilities.PidsLimit = controllers["pids"]

	if err := capabilities.Validate(); err != nil {
		return Capabilities{}, err
	}
	return capabilities, nil
}

// Build implements Engine. It mirrors Docker: one generated context, one
// Containerfile inside it, one tag, and network access only when the spec asks
// for it. A session never uses this operation's network mode.
func (p *Podman) Build(ctx context.Context, spec BuildSpec) (BuildResult, error) {
	if err := validateBuildSpec(spec); err != nil {
		return BuildResult{}, err
	}

	network := "none"
	if spec.Network {
		network = "default"
	}

	stderr, merged := captureStderr(spec.Stderr)
	// The engine resolves --file against its own working directory, not against
	// the context, so the Containerfile is named by its absolute path.
	containerfile := filepath.Join(spec.Context, spec.Containerfile)
	err := p.runner.Run(ctx, Command{
		Args:   p.args("build", "--network="+network, "--file", containerfile, "--tag", spec.Tag, spec.Context),
		Stdout: spec.Stdout,
		Stderr: merged,
	})
	if err != nil {
		failure := result.NewFailure(result.CodeImageUnavailable, "engine.build",
			"the engine could not build %s: %v", spec.Tag, err)
		if output := strings.TrimSpace(stderr.String()); output != "" {
			failure.Details = map[string]string{"build_output": output}
		}
		return BuildResult{}, failure
	}

	id, err := p.output(ctx, "engine.build", "image", "inspect", "--format", "{{.Id}}", spec.Tag)
	if err != nil {
		return BuildResult{}, err
	}
	imageID := canonicalImageID(string(id))
	if imageID == "" {
		return BuildResult{}, result.NewFailure(result.CodeImageUnavailable, "engine.build",
			"the engine built %s but reported no image identifier", spec.Tag)
	}
	return BuildResult{ImageID: imageID}, nil
}

// ImageID implements Engine. It mirrors Docker: the reference names an image
// the engine already holds, and the identifier is normalized to the same shape
// so a manifest pin means the same bytes whichever engine built it.
func (p *Podman) ImageID(ctx context.Context, reference string) (string, error) {
	if reference == "" || strings.HasPrefix(reference, "-") {
		return "", usageFailure("engine.image_id", "an image reference is required")
	}

	out, err := p.output(ctx, "engine.image_id", "image", "inspect", "--format", "{{.Id}}", reference)
	if err != nil {
		return "", missingContainer(err, reference)
	}
	imageID := canonicalImageID(string(out))
	if imageID == "" {
		return "", result.NewFailure(result.CodeImageUnavailable, "engine.image_id",
			"the engine holds %q but reported no image identifier", reference)
	}
	return imageID, nil
}

// Create implements Engine. The argument vector is the session isolation
// contract, rendered once in createArgs, so Podman and Docker cannot drift.
func (p *Podman) Create(ctx context.Context, spec CreateSpec) (string, error) {
	if err := spec.validate(); err != nil {
		return "", err
	}

	out, err := p.output(ctx, "engine.create", createArgs(spec)...)
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

// Start implements Engine. As in Docker, an already-running container is the
// desired state, so a retried start converges instead of failing.
func (p *Podman) Start(ctx context.Context, id string) error {
	if id == "" {
		return usageFailure("engine.start", "a container identifier is required")
	}

	stderr, err := p.run(ctx, "engine.start", Command{Args: p.args("start", id)})
	if err == nil {
		return nil
	}
	if strings.Contains(stderr.String(), "is already running") {
		return nil
	}
	return err
}

// Exec implements Engine. The command's exit status belongs to the caller; a
// non-zero status is a result, not an engine failure.
func (p *Podman) Exec(ctx context.Context, spec ExecSpec) (ExecResult, error) {
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
	err := p.runner.Run(ctx, Command{
		Args:   p.args(args...),
		Stdin:  spec.Stdin,
		Stdout: spec.Stdout,
		Stderr: merged,
	})

	var exit *ExitError
	switch {
	case err == nil:
		return ExecResult{}, nil
	case errors.As(err, &exit):
		return ExecResult{ExitCode: exit.Code}, nil
	default:
		return ExecResult{}, commandFailure("engine.exec", stderr.String(), err)
	}
}

// Stream implements Engine.
func (p *Podman) Stream(ctx context.Context, spec StreamSpec) error {
	if spec.ContainerID == "" || len(spec.Argv) == 0 {
		return usageFailure("engine.stream", "a container identifier and a command are required")
	}
	if spec.Source == nil {
		return usageFailure("engine.stream", "a source reader is required")
	}

	args := []string{"exec", "-i", "--user", strconv.FormatUint(uint64(spec.User), 10), spec.ContainerID}
	args = append(args, spec.Argv...)

	_, err := p.run(ctx, "engine.stream", Command{
		Args:   p.args(args...),
		Stdin:  spec.Source,
		Stdout: spec.Stdout,
		Stderr: spec.Stderr,
	})
	return err
}

// State implements Engine.
func (p *Podman) State(ctx context.Context, id string) (ContainerState, error) {
	if id == "" {
		return ContainerState{}, usageFailure("engine.state", "a container identifier is required")
	}

	out, err := p.output(ctx, "engine.state", "inspect", "--format", "{{json .State}}", id)
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
func (p *Podman) Logs(ctx context.Context, id string, tail int, w io.Writer) error {
	if id == "" || w == nil {
		return usageFailure("engine.logs", "a container identifier and a writer are required")
	}
	if tail <= 0 {
		return usageFailure("engine.logs", "a positive tail is required")
	}

	_, err := p.run(ctx, "engine.logs", Command{
		Args:   p.args("logs", "--tail", strconv.Itoa(tail), id),
		Stdout: w,
	})
	return err
}

// Stop implements Engine.
func (p *Podman) Stop(ctx context.Context, id string, timeout time.Duration) error {
	if id == "" {
		return usageFailure("engine.stop", "a container identifier is required")
	}

	seconds := int(math.Ceil(timeout.Seconds()))
	if seconds < 1 {
		seconds = 1
	}

	_, err := p.run(ctx, "engine.stop", Command{
		Args: p.args("stop", "--time", strconv.Itoa(seconds), id),
	})
	if err != nil {
		return missingContainer(err, id)
	}
	return nil
}

// Remove implements Engine. It forces removal for the same reason Docker does:
// a session being closed must not stay behind because PID 1 ignored a signal.
func (p *Podman) Remove(ctx context.Context, id string) error {
	if id == "" {
		return usageFailure("engine.remove", "a container identifier is required")
	}

	_, err := p.run(ctx, "engine.remove", Command{Args: p.args("rm", "--force", id)})
	if err != nil {
		return missingContainer(err, id)
	}
	return nil
}

// contextName reports the endpoint the session record should name.
func (p *Podman) contextName() string {
	if p.connection != "" {
		return p.connection
	}
	return podmanLocalName
}

// args returns the argument list for a subcommand, pinned to a connection when
// one is selected. Podman's flag is --connection, where Docker's is --context.
func (p *Podman) args(args ...string) []string {
	if p.connection == "" {
		return args
	}
	return append([]string{"--connection", p.connection}, args...)
}

// output runs a command and returns its standard output.
func (p *Podman) output(ctx context.Context, operation string, args ...string) ([]byte, error) {
	var stdout bytes.Buffer
	if _, err := p.run(ctx, operation, Command{Args: p.args(args...), Stdout: &stdout}); err != nil {
		return nil, err
	}
	return stdout.Bytes(), nil
}

// run executes one engine command, keeping a bounded copy of its diagnostics
// for whatever fails.
func (p *Podman) run(ctx context.Context, operation string, cmd Command) (*boundedBuffer, error) {
	stderr, merged := captureStderr(cmd.Stderr)
	cmd.Stderr = merged

	if err := p.runner.Run(ctx, cmd); err != nil {
		return stderr, commandFailure(operation, stderr.String(), err)
	}
	return stderr, nil
}

// podmanControllers decodes host.cgroupControllers. Podman renders it as a
// JSON array and some versions as a space-separated string; both translate to
// the same set of delegated controllers.
func podmanControllers(raw json.RawMessage) map[string]bool {
	controllers := make(map[string]bool)
	if len(raw) == 0 {
		return controllers
	}

	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		for _, controller := range list {
			controllers[controller] = true
		}
		return controllers
	}

	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		for _, controller := range strings.Fields(text) {
			controllers[controller] = true
		}
	}
	return controllers
}

// canonicalCgroupVersion strips the "v" Podman prefixes its cgroup version
// with, so Capabilities reports the same "1"/"2" shapes whichever engine
// answered. An unknown version stays empty and is never read as v2.
func canonicalCgroupVersion(reported string) string {
	return strings.TrimPrefix(strings.TrimSpace(reported), "v")
}

// canonicalImageID normalizes the identifier the engine reports to the shape
// the Docker adapter returns, so a session record names the same image bytes
// whichever engine built them.
func canonicalImageID(reported string) string {
	id := strings.TrimSpace(reported)
	if id == "" || strings.Contains(id, ":") {
		return id
	}
	return "sha256:" + id
}
