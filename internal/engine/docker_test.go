package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/bnema/wlvision/internal/result"
)

// Engine info fixtures. Field names follow `docker info --format '{{json .}}'`.
const (
	infoRootlessCgroupV2 = `{
		"ServerVersion": "29.7.2",
		"SecurityOptions": ["name=seccomp,profile=builtin", "name=rootless", "name=cgroupns"],
		"CgroupVersion": "2",
		"CgroupDriver": "systemd",
		"MemoryLimit": true,
		"PidsLimit": true,
		"CPUShares": true,
		"Warnings": ["WARNING: No cpuset support"]
	}`

	infoRootful = `{
		"ServerVersion": "29.7.2",
		"SecurityOptions": ["name=seccomp,profile=builtin", "name=cgroupns"],
		"CgroupVersion": "2",
		"CgroupDriver": "systemd",
		"MemoryLimit": true,
		"PidsLimit": true
	}`

	infoWithoutSeccomp = `{
		"ServerVersion": "29.7.2",
		"SecurityOptions": ["name=rootless", "name=cgroupns"],
		"CgroupVersion": "2",
		"CgroupDriver": "systemd",
		"MemoryLimit": true,
		"PidsLimit": true
	}`

	infoRootlessCgroupV1WithoutDelegation = `{
		"ServerVersion": "27.5.1",
		"SecurityOptions": ["name=seccomp,profile=builtin", "name=rootless"],
		"CgroupVersion": "1",
		"CgroupDriver": "cgroupfs",
		"MemoryLimit": false,
		"PidsLimit": false,
		"CPUShares": true,
		"Warnings": ["WARNING: No memory limit support", "WARNING: No pids limit support"]
	}`

	infoState = `{"Running":true,"ExitCode":0,"StartedAt":"2026-01-02T03:04:05.123456789Z","FinishedAt":"0001-01-01T00:00:00Z"}`
)

// reply is one canned engine response. The first reply whose contains list is
// a subsequence-free substring match of the arguments answers the call.
type reply struct {
	contains []string
	stdout   string
	stderr   string
	err      error
}

// fakeRunner records every command and answers it from a script of replies. It
// stands in for the engine CLI so no test touches a daemon.
type fakeRunner struct {
	replies []reply
	calls   []Command
	t       *testing.T
}

func (f *fakeRunner) Run(_ context.Context, cmd Command) error {
	f.calls = append(f.calls, cmd)

	for i, r := range f.replies {
		if !containsAll(cmd.Args, r.contains) {
			continue
		}
		f.replies = append(f.replies[:i], f.replies[i+1:]...)

		if r.stdout != "" && cmd.Stdout != nil {
			if _, err := io.WriteString(cmd.Stdout, r.stdout); err != nil {
				return err
			}
		}
		if r.stderr != "" && cmd.Stderr != nil {
			if _, err := io.WriteString(cmd.Stderr, r.stderr); err != nil {
				return err
			}
		}
		return r.err
	}

	f.t.Fatalf("fake runner: unexpected command: %s", strings.Join(cmd.Args, " "))
	return nil
}

func (f *fakeRunner) lastCall(t *testing.T) Command {
	t.Helper()
	if len(f.calls) == 0 {
		t.Fatal("the engine ran no command")
	}
	return f.calls[len(f.calls)-1]
}

func containsAll(args []string, want []string) bool {
	for _, w := range want {
		found := false
		for _, a := range args {
			if a == w {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func dockerWith(t *testing.T, replies ...reply) (*Docker, *fakeRunner) {
	t.Helper()

	runner := &fakeRunner{replies: replies, t: t}
	eng, err := NewDocker(runner, Options{Context: "wlvision-test"})
	if err != nil {
		t.Fatalf("NewDocker: %v", err)
	}
	return eng, runner
}

func failureOf(t *testing.T, err error) *result.Failure {
	t.Helper()

	var failure *result.Failure
	if !errors.As(err, &failure) {
		t.Fatalf("error %v (%T) is not a *result.Failure", err, err)
	}
	return failure
}

func TestDockerCapabilitiesRecordTheEngineAndItsProtections(t *testing.T) {
	eng, runner := dockerWith(t, reply{contains: []string{"info", "--format"}, stdout: infoRootlessCgroupV2})

	caps, err := eng.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}

	if !caps.Rootless {
		t.Error("Rootless = false, want true")
	}
	if caps.SeccompProfile != "builtin" {
		t.Errorf("SeccompProfile = %q, want builtin", caps.SeccompProfile)
	}
	if caps.CgroupVersion != "2" || caps.CgroupDriver != "systemd" {
		t.Errorf("cgroups = %s/%s, want 2/systemd", caps.CgroupVersion, caps.CgroupDriver)
	}
	if caps.ServerVersion != "29.7.2" {
		t.Errorf("ServerVersion = %q, want 29.7.2", caps.ServerVersion)
	}
	if caps.Context != "wlvision-test" {
		t.Errorf("Context = %q, want the configured context", caps.Context)
	}
	if len(caps.Degradations()) != 0 {
		t.Errorf("Degradations = %v, want none", caps.Degradations())
	}

	if call := runner.lastCall(t); !containsAll(call.Args, []string{"--context", "wlvision-test", "info"}) {
		t.Errorf("info command = %v, want the configured context", call.Args)
	}
}

func TestDockerCapabilitiesResolveTheCurrentContext(t *testing.T) {
	runner := &fakeRunner{
		t: t,
		replies: []reply{
			{contains: []string{"context", "show"}, stdout: "desktop-linux\n"},
			{contains: []string{"info", "--format"}, stdout: infoRootlessCgroupV2},
		},
	}
	eng, err := NewDocker(runner, Options{})
	if err != nil {
		t.Fatalf("NewDocker: %v", err)
	}

	caps, err := eng.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if caps.Context != "desktop-linux" {
		t.Errorf("Context = %q, want the engine's current context", caps.Context)
	}
}

func TestDockerCapabilitiesRejectARootfulEngine(t *testing.T) {
	eng, _ := dockerWith(t, reply{contains: []string{"info", "--format"}, stdout: infoRootful})

	_, err := eng.Capabilities(context.Background())
	if err == nil {
		t.Fatal("a rootful engine was accepted")
	}

	failure := failureOf(t, err)
	if failure.Code != result.CodeEngineNotRootless {
		t.Errorf("code = %s, want %s", failure.Code, result.CodeEngineNotRootless)
	}
	if failure.Code.ExitCode() != 3 {
		t.Errorf("exit code = %d, want 3", failure.Code.ExitCode())
	}
	if failure.Retriable {
		t.Error("a rootful engine is not retriable by repeating the operation")
	}
}

func TestDockerCapabilitiesRefuseAnEngineWithoutSeccomp(t *testing.T) {
	eng, _ := dockerWith(t, reply{contains: []string{"info", "--format"}, stdout: infoWithoutSeccomp})

	_, err := eng.Capabilities(context.Background())
	if err == nil {
		t.Fatal("an engine without a seccomp profile was accepted")
	}

	failure := failureOf(t, err)
	if failure.Code != result.CodeProtectionDegraded {
		t.Errorf("code = %s, want %s", failure.Code, result.CodeProtectionDegraded)
	}
	if failure.Details["missing"] != "seccomp" {
		t.Errorf("details = %v, want missing=seccomp", failure.Details)
	}
	if failure.Code.ExitCode() != 3 {
		t.Errorf("exit code = %d, want 3", failure.Code.ExitCode())
	}
}

func TestDockerCapabilitiesReportUndelegatedLimits(t *testing.T) {
	eng, _ := dockerWith(t, reply{contains: []string{"info", "--format"}, stdout: infoRootlessCgroupV1WithoutDelegation})

	caps, err := eng.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}

	want := []string{DegradationCgroupV1, DegradationMemoryLimit, DegradationPidsLimit}
	if strings.Join(caps.Degradations(), ",") != strings.Join(want, ",") {
		t.Errorf("Degradations = %v, want %v", caps.Degradations(), want)
	}

	// A caller asking for limits the engine cannot enforce learns exactly
	// which ones are missing instead of assuming they are active.
	missing := caps.MissingLimits(Limits{MemoryBytes: 512 << 20, Pids: 64, FileSizeBytes: 1 << 20, OpenFiles: 256})
	if strings.Join(missing, ",") != strings.Join([]string{DegradationMemoryLimit, DegradationPidsLimit}, ",") {
		t.Errorf("MissingLimits = %v, want the two cgroup limits", missing)
	}

	// A zero limit asks for nothing, so nothing is missing.
	if missing := caps.MissingLimits(Limits{}); len(missing) != 0 {
		t.Errorf("MissingLimits(no limits) = %v, want none", missing)
	}
}

func TestDockerCreateAppliesTheIsolationContract(t *testing.T) {
	eng, runner := dockerWith(t, reply{contains: []string{"create"}, stdout: "c0ffee\n"})

	spec := CreateSpec{
		Image:          "wlvision-runtime:test",
		Name:           "wlvision-demo",
		Session:        "demo",
		ControlUID:     1000,
		ControlGID:     1000,
		ApplicationUID: 1001,
		ApplicationGID: 1001,
		Command:        []string{"/usr/libexec/wlvision-supervisor"},
		Tmpfs: []Tmpfs{
			{Path: "/run", SizeBytes: 64 << 20, Mode: 0o755, Exec: true},
			{Path: "/tmp", SizeBytes: 32 << 20, Mode: 0o1777},
			{Path: "/home/agent", SizeBytes: 32 << 20, Mode: 0o1777},
		},
		Limits: Limits{MemoryBytes: 1 << 30, Pids: 128, FileSizeBytes: 64 << 20, OpenFiles: 512},
	}

	id, err := eng.Create(context.Background(), spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id != "c0ffee" {
		t.Errorf("container id = %q, want the engine's answer", id)
	}

	args := strings.Join(runner.lastCall(t).Args, " ")
	required := []string{
		"create",
		"--name wlvision-demo",
		"--label wlvision.session=demo",
		"--network none",
		"--read-only",
		"--cap-drop ALL",
		"--security-opt no-new-privileges",
		"--user 1000:1000",
		"--memory 1073741824",
		"--pids-limit 128",
		"--ulimit fsize=67108864",
		"--ulimit nofile=512",
		"--tmpfs /run:rw,size=67108864,mode=0755,exec,nosuid,nodev",
		"--tmpfs /tmp:rw,size=33554432,mode=01777,noexec,nosuid,nodev",
		"--tmpfs /home/agent:rw,size=33554432,mode=01777,noexec,nosuid,nodev",
		"-e WLVISION_SESSION=demo",
		"-e WLVISION_CONTROL_UID=1000",
		"-e WLVISION_APPLICATION_UID=1001",
		"wlvision-runtime:test /usr/libexec/wlvision-supervisor",
	}
	for _, want := range required {
		if !strings.Contains(args, want) {
			t.Errorf("create command %q\nis missing %q", args, want)
		}
	}

	for _, forbidden := range []string{"--privileged", "--cap-add", "--pid host", "--ipc host", "--network host", "-v ", "--volume", "--device"} {
		if strings.Contains(args, forbidden) {
			t.Errorf("create command %q contains %q", args, forbidden)
		}
	}
}

func TestDockerCreateRefusesAnIncompleteSpec(t *testing.T) {
	eng, runner := dockerWith(t)

	_, err := eng.Create(context.Background(), CreateSpec{Session: "demo"})
	if err == nil {
		t.Fatal("a spec without an image was accepted")
	}
	if failure := failureOf(t, err); failure.Code != result.CodeUsageError {
		t.Errorf("code = %s, want %s", failure.Code, result.CodeUsageError)
	}
	if len(runner.calls) != 0 {
		t.Errorf("the engine ran %d commands for an invalid spec", len(runner.calls))
	}
}

func TestDockerExecRunsAsTheApplicationIdentity(t *testing.T) {
	eng, runner := dockerWith(t, reply{contains: []string{"exec"}})

	stdin := strings.NewReader("payload")
	var stdout, stderr strings.Builder
	_, err := eng.Exec(context.Background(), ExecSpec{
		ContainerID: "c0ffee",
		User:        1001,
		WorkDir:     "/home/agent",
		Env:         []string{"APP_ENV=test"},
		Argv:        []string{"/usr/bin/hello", "--flag"},
		Stdin:       stdin,
		Stdout:      &stdout,
		Stderr:      &stderr,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	call := runner.lastCall(t)
	want := []string{"--context", "wlvision-test", "exec", "-i", "--user", "1001", "--workdir", "/home/agent", "-e", "APP_ENV=test", "c0ffee", "/usr/bin/hello", "--flag"}
	if strings.Join(call.Args, " ") != strings.Join(want, " ") {
		t.Errorf("exec command = %v, want %v", call.Args, want)
	}
	if call.Stdin != stdin {
		t.Error("exec did not receive the caller's stdin")
	}
}

func TestDockerStreamPipesAPayloadIntoTheContainer(t *testing.T) {
	eng, runner := dockerWith(t, reply{contains: []string{"exec"}})

	source := strings.NewReader("payload bytes")
	var result strings.Builder
	err := eng.Stream(context.Background(), StreamSpec{
		ContainerID: "c0ffee",
		User:        1000,
		Argv:        []string{"/usr/libexec/wlvision-receive", "/run/wlvision/payload"},
		Source:      source,
		Stdout:      &result,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	call := runner.lastCall(t)
	want := []string{"--context", "wlvision-test", "exec", "-i", "--user", "1000", "c0ffee", "/usr/libexec/wlvision-receive", "/run/wlvision/payload"}
	if strings.Join(call.Args, " ") != strings.Join(want, " ") {
		t.Errorf("stream command = %v, want %v", call.Args, want)
	}
	if call.Stdin != source {
		t.Error("stream did not pipe the caller's reader")
	}
}

func TestDockerStateReadsTheEngineReport(t *testing.T) {
	eng, runner := dockerWith(t, reply{contains: []string{"inspect"}, stdout: infoState})

	state, err := eng.State(context.Background(), "c0ffee")
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if !state.Running || state.ExitCode != 0 {
		t.Errorf("state = %+v, want a running container", state)
	}
	if state.StartedAt.IsZero() {
		t.Error("StartedAt was not parsed")
	}
	if !state.FinishedAt.IsZero() {
		t.Errorf("FinishedAt = %s, want the zero time for a running container", state.FinishedAt)
	}

	call := runner.lastCall(t)
	if !containsAll(call.Args, []string{"inspect", "--format", "{{json .State}}", "c0ffee"}) {
		t.Errorf("inspect command = %v", call.Args)
	}
}

func TestDockerLogsAndLifecycleCommands(t *testing.T) {
	eng, runner := dockerWith(t,
		reply{contains: []string{"logs"}, stdout: "weston: ready\n"},
		reply{contains: []string{"stop"}},
		reply{contains: []string{"rm"}},
	)

	var logs strings.Builder
	if err := eng.Logs(context.Background(), "c0ffee", 200, &logs); err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if logs.String() != "weston: ready\n" {
		t.Errorf("logs = %q", logs.String())
	}
	if call := runner.lastCall(t); !containsAll(call.Args, []string{"logs", "--tail", "200", "c0ffee"}) {
		t.Errorf("logs command = %v", call.Args)
	}

	if err := eng.Stop(context.Background(), "c0ffee", 5*time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if call := runner.lastCall(t); !containsAll(call.Args, []string{"stop", "--time", "5", "c0ffee"}) {
		t.Errorf("stop command = %v", call.Args)
	}

	if err := eng.Remove(context.Background(), "c0ffee"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if call := runner.lastCall(t); !containsAll(call.Args, []string{"rm", "--force", "c0ffee"}) {
		t.Errorf("remove command = %v", call.Args)
	}
}

func TestDockerExecReportsTheApplicationExitCode(t *testing.T) {
	eng, _ := dockerWith(t, reply{
		contains: []string{"exec"},
		err:      &ExitError{Code: 3, Err: errors.New("exit status 3")},
	})

	// A command that ran and exited non-zero is a result, not an engine
	// failure: the caller must see the application's status.
	execution, err := eng.Exec(context.Background(), ExecSpec{
		ContainerID: "c0ffee",
		User:        1001,
		Argv:        []string{"/bin/false"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if execution.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", execution.ExitCode)
	}
}

func TestDockerReportsAContainerTheEngineNoLongerHas(t *testing.T) {
	missing := reply{
		stderr: "Error: No such object: c0ffee",
		err:    &exec.ExitError{},
	}

	t.Run("state", func(t *testing.T) {
		eng, _ := dockerWith(t, reply{contains: []string{"inspect"}, stderr: missing.stderr, err: missing.err})

		_, err := eng.State(context.Background(), "c0ffee")
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("State error = %v, want ErrNotFound", err)
		}
	})

	t.Run("remove", func(t *testing.T) {
		eng, _ := dockerWith(t, reply{contains: []string{"rm"}, stderr: missing.stderr, err: missing.err})

		if err := eng.Remove(context.Background(), "c0ffee"); !errors.Is(err, ErrNotFound) {
			t.Errorf("Remove error = %v, want ErrNotFound", err)
		}
	})

	t.Run("stop", func(t *testing.T) {
		eng, _ := dockerWith(t, reply{contains: []string{"stop"}, stderr: missing.stderr, err: missing.err})

		if err := eng.Stop(context.Background(), "c0ffee", time.Second); !errors.Is(err, ErrNotFound) {
			t.Errorf("Stop error = %v, want ErrNotFound", err)
		}
	})

	t.Run("another failure stays an engine failure", func(t *testing.T) {
		eng, _ := dockerWith(t, reply{
			contains: []string{"inspect"},
			stderr:   "permission denied while trying to connect to the Docker daemon socket",
			err:      &exec.ExitError{},
		})

		err := func() error { _, err := eng.State(context.Background(), "c0ffee"); return err }()
		if errors.Is(err, ErrNotFound) {
			t.Error("a daemon failure was reported as a missing container")
		}
		if failure := failureOf(t, err); failure.Code != result.CodeEngineUnavailable {
			t.Errorf("code = %s, want %s", failure.Code, result.CodeEngineUnavailable)
		}
	})
}

func TestDockerStartTreatsAnAlreadyRunningContainerAsSuccess(t *testing.T) {
	eng, _ := dockerWith(t, reply{
		contains: []string{"start"},
		stderr:   "Error response from daemon: container c0ffee is already running",
		err:      &exec.ExitError{},
	})

	// A retried session creation must converge instead of failing because the
	// container it wants is already in the state it asked for.
	if err := eng.Start(context.Background(), "c0ffee"); err != nil {
		t.Fatalf("Start on a running container: %v", err)
	}
}

func TestDockerWrapsCLIFailuresWithTheEngineCode(t *testing.T) {
	eng, _ := dockerWith(t, reply{
		contains: []string{"info"},
		stderr:   "Cannot connect to the Docker daemon at unix:///run/user/1000/docker.sock. Is the docker daemon running?",
		err:      &exec.ExitError{},
	})

	_, err := eng.Capabilities(context.Background())
	if err == nil {
		t.Fatal("an unreachable engine was reported as available")
	}

	failure := failureOf(t, err)
	if failure.Code != result.CodeEngineUnavailable {
		t.Errorf("code = %s, want %s", failure.Code, result.CodeEngineUnavailable)
	}
	if failure.Code.ExitCode() != 3 {
		t.Errorf("exit code = %d, want 3", failure.Code.ExitCode())
	}
	if !strings.Contains(failure.Details["stderr"], "Cannot connect to the Docker daemon") {
		t.Errorf("details = %v, want the engine's message", failure.Details)
	}
	if !failure.Retriable {
		t.Error("an unreachable engine is retriable")
	}
}

func TestDockerReportsADeadlineAsATimeout(t *testing.T) {
	eng, _ := dockerWith(t, reply{
		contains: []string{"stop"},
		err:      fmt.Errorf("signal: killed: %w", context.DeadlineExceeded),
	})

	err := eng.Stop(context.Background(), "c0ffee", time.Second)
	if err == nil {
		t.Fatal("a cancelled stop was reported as success")
	}
	if failure := failureOf(t, err); failure.Code != result.CodeWaitTimeout {
		t.Errorf("code = %s, want %s", failure.Code, result.CodeWaitTimeout)
	}
}

func TestDockerRejectsAnInvalidConfiguration(t *testing.T) {
	if _, err := NewDocker(nil, Options{}); err == nil {
		t.Error("NewDocker accepted a nil runner")
	}
	if _, err := NewDocker(&fakeRunner{t: t}, Options{Context: "--privileged"}); err == nil {
		t.Error("NewDocker accepted an option that could be read as a flag")
	}
}

func TestCommandFailureKeepsStderrBounded(t *testing.T) {
	long := strings.Repeat("x", 64<<10)
	eng, _ := dockerWith(t, reply{contains: []string{"info"}, stderr: long, err: &exec.ExitError{}})

	_, err := eng.Capabilities(context.Background())
	failure := failureOf(t, err)
	if len(failure.Details["stderr"]) > 4096 {
		t.Errorf("stderr details are %d bytes, want them bounded", len(failure.Details["stderr"]))
	}
}
