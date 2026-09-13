package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bnema/wlvision/internal/result"
)

// TestEngineContract is the one contract every adapter must satisfy, run
// against each adapter twice.
//
// The fake half pins the exact argument vector of every operation and the
// typed failure each one reports, so a regression in either adapter is caught
// without a daemon. The live half runs the same operations against an installed
// rootless engine. A live half that cannot run is skipped with a printed
// reason: the package summary always names it, so a missing engine can never
// look like a pass.
func TestEngineContract(t *testing.T) {
	for _, adapter := range contractAdapters() {
		t.Run(adapter.name, func(t *testing.T) {
			t.Run("fake", func(t *testing.T) { fakeEngineContract(t, adapter) })
			t.Run("live", func(t *testing.T) { liveEngineContract(t, adapter) })
		})
	}
}

// contractAdapter describes one engine as the contract sees it.
type contractAdapter struct {
	name string
	kind Kind
	// binary is the CLI the live half drives.
	binary string
	// prefix is the argument selector for Options.Context, where the engines
	// differ: Docker selects a context, Podman a connection.
	prefix []string
	// capabilityArgs is how the adapter asks the engine to report itself.
	capabilityArgs []string
	// Fixtures for the engine's own capability report.
	capsFixture      string
	rootfulFixture   string
	degradedFixture  string
	noSeccompFixture string
	// buildImageID is what `image inspect` answers for a built image; Podman
	// reports it without the algorithm prefix, which the adapter normalizes.
	buildImageID string
	newAdapter   func(runner CommandRunner, options Options) (Engine, error)
}

func contractAdapters() []contractAdapter {
	return []contractAdapter{
		{
			name:             "docker",
			kind:             KindDocker,
			binary:           "docker",
			prefix:           []string{"--context", "wlvision-test"},
			capabilityArgs:   []string{"info", "--format", "{{json .}}"},
			capsFixture:      infoRootlessCgroupV2,
			rootfulFixture:   infoRootful,
			degradedFixture:  infoRootlessCgroupV1WithoutDelegation,
			noSeccompFixture: infoWithoutSeccomp,
			buildImageID:     "sha256:c0ffee\n",
			newAdapter:       func(runner CommandRunner, options Options) (Engine, error) { return NewDocker(runner, options) },
		},
		{
			name:             "podman",
			kind:             KindPodman,
			binary:           "podman",
			prefix:           []string{"--connection", "wlvision-test"},
			capabilityArgs:   []string{"info", "--format", "json"},
			capsFixture:      podmanInfoRootlessCgroupV2,
			rootfulFixture:   podmanInfoRootful,
			degradedFixture:  podmanInfoRootlessCgroupV1NoDelegation,
			noSeccompFixture: podmanInfoWithoutSeccomp,
			buildImageID:     "c0ffee\n",
			newAdapter:       func(runner CommandRunner, options Options) (Engine, error) { return NewPodman(runner, options) },
		},
	}
}

// with builds the adapter over a scripted fake runner. The replies are copied
// because the fake consumes its script as it answers.
func (a contractAdapter) with(t *testing.T, replies ...reply) (Engine, *fakeRunner) {
	t.Helper()

	runner := &fakeRunner{replies: append([]reply(nil), replies...), t: t}
	eng, err := a.newAdapter(runner, Options{Context: "wlvision-test"})
	if err != nil {
		t.Fatalf("constructing the %s adapter: %v", a.name, err)
	}
	return eng, runner
}

// expect renders the full vector, including the engine selector.
func (a contractAdapter) expect(args ...string) []string {
	return append(append([]string{}, a.prefix...), args...)
}

// contractCreateSpec is the session creation the contract describes.
func contractCreateSpec() CreateSpec {
	return CreateSpec{
		Image:          "wlvision-runtime:contract",
		Name:           "wlvision-contract",
		Session:        "contract",
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
}

// contractCreateVector is the session isolation contract in argument form. It
// is stated here independently of the adapters so that both are measured
// against the same bytes, and so weakening it in one adapter fails the other.
var contractCreateVector = []string{
	"create",
	"--name", "wlvision-contract",
	"--label", "wlvision.session=contract",
	"--network", "none",
	"--read-only",
	"--cap-drop", "ALL",
	"--security-opt", "no-new-privileges",
	"--user", "1000:1000",
	"--memory", "1073741824",
	"--pids-limit", "128",
	"--ulimit", "fsize=67108864",
	"--ulimit", "nofile=512",
	"--tmpfs", "/run:rw,size=67108864,mode=0755,exec,nosuid,nodev",
	"--tmpfs", "/tmp:rw,size=33554432,mode=01777,noexec,nosuid,nodev",
	"--tmpfs", "/home/agent:rw,size=33554432,mode=01777,noexec,nosuid,nodev",
	"-e", "WLVISION_SESSION=contract",
	"-e", "WLVISION_CONTROL_UID=1000",
	"-e", "WLVISION_APPLICATION_UID=1001",
	"wlvision-runtime:contract",
	"/usr/libexec/wlvision-supervisor",
}

// fakeEngineContract pins argument construction and typed failures.
func fakeEngineContract(t *testing.T, adapter contractAdapter) {
	t.Helper()

	t.Run("capabilities", func(t *testing.T) {
		eng, runner := adapter.with(t, reply{contains: []string{"info", "--format"}, stdout: adapter.capsFixture})

		caps, err := eng.Capabilities(context.Background())
		if err != nil {
			t.Fatalf("Capabilities: %v", err)
		}
		if caps.Kind != adapter.kind {
			t.Errorf("Kind = %s, want %s", caps.Kind, adapter.kind)
		}
		if !caps.Rootless {
			t.Error("Rootless = false, want true")
		}
		if caps.SeccompProfile == "" || strings.Contains(caps.SeccompProfile, "/") {
			t.Errorf("SeccompProfile = %q, want a normalized profile name", caps.SeccompProfile)
		}
		if caps.CgroupVersion != "2" || caps.CgroupDriver == "" {
			t.Errorf("cgroups = %s/%s, want 2/<driver>", caps.CgroupVersion, caps.CgroupDriver)
		}
		if !caps.MemoryLimit || !caps.PidsLimit {
			t.Errorf("controllers = memory:%v pids:%v, want both delegated", caps.MemoryLimit, caps.PidsLimit)
		}
		if caps.Context != "wlvision-test" {
			t.Errorf("Context = %q, want the configured selector", caps.Context)
		}
		if len(caps.Degradations()) != 0 {
			t.Errorf("Degradations = %v, want none", caps.Degradations())
		}
		assertCalls(t, runner, [][]string{adapter.expect(adapter.capabilityArgs...)})
	})

	t.Run("capabilities-reject-a-rootful-engine", func(t *testing.T) {
		eng, _ := adapter.with(t, reply{contains: []string{"info", "--format"}, stdout: adapter.rootfulFixture})

		_, err := eng.Capabilities(context.Background())
		if err == nil {
			t.Fatal("a rootful engine was accepted")
		}
		failure := failureOf(t, err)
		if failure.Code != result.CodeEngineNotRootless {
			t.Errorf("code = %s, want %s", failure.Code, result.CodeEngineNotRootless)
		}
		if failure.Code.ExitCode() != 3 || failure.Retriable {
			t.Errorf("exit code = %d, retriable = %v; want 3 and false", failure.Code.ExitCode(), failure.Retriable)
		}
	})

	t.Run("capabilities-refuse-a-missing-seccomp-profile", func(t *testing.T) {
		eng, _ := adapter.with(t, reply{contains: []string{"info", "--format"}, stdout: adapter.noSeccompFixture})

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
	})

	t.Run("capabilities-report-degraded-limits", func(t *testing.T) {
		eng, _ := adapter.with(t, reply{contains: []string{"info", "--format"}, stdout: adapter.degradedFixture})

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
	})

	cases := []struct {
		name    string
		replies []reply
		run     func(t *testing.T, eng Engine, runner *fakeRunner)
		expect  func(adapter contractAdapter) [][]string
	}{
		{
			name:    "create",
			replies: []reply{{contains: []string{"create"}, stdout: "c0ffee\n"}},
			run: func(t *testing.T, eng Engine, _ *fakeRunner) {
				id, err := eng.Create(context.Background(), contractCreateSpec())
				if err != nil {
					t.Fatalf("Create: %v", err)
				}
				if id != "c0ffee" {
					t.Errorf("container id = %q, want the engine's answer", id)
				}
			},
			expect: func(adapter contractAdapter) [][]string {
				return [][]string{adapter.expect(contractCreateVector...)}
			},
		},
		{
			name:    "start",
			replies: []reply{{contains: []string{"start"}}},
			run: func(t *testing.T, eng Engine, _ *fakeRunner) {
				if err := eng.Start(context.Background(), "c0ffee"); err != nil {
					t.Fatalf("Start: %v", err)
				}
			},
			expect: func(adapter contractAdapter) [][]string { return [][]string{adapter.expect("start", "c0ffee")} },
		},
		{
			name: "start-an-already-running-container",
			replies: []reply{{
				contains: []string{"start"},
				stderr:   "Error response from daemon: container c0ffee is already running",
				err:      &exec.ExitError{},
			}},
			run: func(t *testing.T, eng Engine, _ *fakeRunner) {
				if err := eng.Start(context.Background(), "c0ffee"); err != nil {
					t.Fatalf("Start on a running container: %v", err)
				}
			},
			expect: func(adapter contractAdapter) [][]string { return [][]string{adapter.expect("start", "c0ffee")} },
		},
		{
			name:    "exec",
			replies: []reply{{contains: []string{"exec"}}},
			run: func(t *testing.T, eng Engine, runner *fakeRunner) {
				stdin := strings.NewReader("payload")
				var stdout, stderr strings.Builder
				if _, err := eng.Exec(context.Background(), ExecSpec{
					ContainerID: "c0ffee",
					User:        1001,
					WorkDir:     "/home/agent",
					Env:         []string{"APP_ENV=test"},
					Argv:        []string{"/usr/bin/hello", "--flag"},
					Stdin:       stdin,
					Stdout:      &stdout,
					Stderr:      &stderr,
				}); err != nil {
					t.Fatalf("Exec: %v", err)
				}
				if runner.calls[0].Stdin != stdin {
					t.Error("exec did not receive the caller's stdin")
				}
			},
			expect: func(adapter contractAdapter) [][]string {
				return [][]string{adapter.expect("exec", "-i", "--user", "1001", "--workdir", "/home/agent",
					"-e", "APP_ENV=test", "c0ffee", "/usr/bin/hello", "--flag")}
			},
		},
		{
			name: "exec-reports-the-application-exit-code",
			replies: []reply{{
				contains: []string{"exec"},
				err:      &ExitError{Code: 7, Err: errors.New("exit status 7")},
			}},
			run: func(t *testing.T, eng Engine, _ *fakeRunner) {
				execution, err := eng.Exec(context.Background(), ExecSpec{
					ContainerID: "c0ffee",
					User:        1001,
					Argv:        []string{"/bin/false"},
				})
				if err != nil {
					t.Fatalf("Exec: %v", err)
				}
				if execution.ExitCode != 7 {
					t.Errorf("ExitCode = %d, want 7", execution.ExitCode)
				}
			},
			expect: func(adapter contractAdapter) [][]string {
				return [][]string{adapter.expect("exec", "-i", "--user", "1001", "c0ffee", "/bin/false")}
			},
		},
		{
			name:    "stream",
			replies: []reply{{contains: []string{"exec"}}},
			run: func(t *testing.T, eng Engine, runner *fakeRunner) {
				source := strings.NewReader("payload bytes")
				var stdout strings.Builder
				if err := eng.Stream(context.Background(), StreamSpec{
					ContainerID: "c0ffee",
					User:        1000,
					Argv:        []string{"/usr/libexec/wlvision-receive", "/run/wlvision/payload"},
					Source:      source,
					Stdout:      &stdout,
				}); err != nil {
					t.Fatalf("Stream: %v", err)
				}
				if runner.calls[0].Stdin != source {
					t.Error("stream did not pipe the caller's reader")
				}
			},
			expect: func(adapter contractAdapter) [][]string {
				return [][]string{adapter.expect("exec", "-i", "--user", "1000", "c0ffee",
					"/usr/libexec/wlvision-receive", "/run/wlvision/payload")}
			},
		},
		{
			name:    "state",
			replies: []reply{{contains: []string{"inspect"}, stdout: infoState}},
			run: func(t *testing.T, eng Engine, _ *fakeRunner) {
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
			},
			expect: func(adapter contractAdapter) [][]string {
				return [][]string{adapter.expect("inspect", "--format", "{{json .State}}", "c0ffee")}
			},
		},
		{
			name:    "logs",
			replies: []reply{{contains: []string{"logs"}, stdout: "weston: ready\n"}},
			run: func(t *testing.T, eng Engine, _ *fakeRunner) {
				var logs strings.Builder
				if err := eng.Logs(context.Background(), "c0ffee", 200, &logs); err != nil {
					t.Fatalf("Logs: %v", err)
				}
				if logs.String() != "weston: ready\n" {
					t.Errorf("logs = %q", logs.String())
				}
			},
			expect: func(adapter contractAdapter) [][]string {
				return [][]string{adapter.expect("logs", "--tail", "200", "c0ffee")}
			},
		},
		{
			name:    "stop",
			replies: []reply{{contains: []string{"stop"}}},
			run: func(t *testing.T, eng Engine, _ *fakeRunner) {
				if err := eng.Stop(context.Background(), "c0ffee", 5*time.Second); err != nil {
					t.Fatalf("Stop: %v", err)
				}
			},
			expect: func(adapter contractAdapter) [][]string {
				return [][]string{adapter.expect("stop", "--time", "5", "c0ffee")}
			},
		},
		{
			name:    "remove",
			replies: []reply{{contains: []string{"rm"}}},
			run: func(t *testing.T, eng Engine, _ *fakeRunner) {
				if err := eng.Remove(context.Background(), "c0ffee"); err != nil {
					t.Fatalf("Remove: %v", err)
				}
			},
			expect: func(adapter contractAdapter) [][]string { return [][]string{adapter.expect("rm", "--force", "c0ffee")} },
		},
		{
			name: "a-container-the-engine-no-longer-has",
			replies: []reply{{
				contains: []string{"inspect"},
				stderr:   "Error: No such object: c0ffee",
				err:      &exec.ExitError{},
			}},
			run: func(t *testing.T, eng Engine, _ *fakeRunner) {
				if _, err := eng.State(context.Background(), "c0ffee"); !errors.Is(err, ErrNotFound) {
					t.Errorf("State error = %v, want ErrNotFound", err)
				}
			},
			expect: func(adapter contractAdapter) [][]string {
				return [][]string{adapter.expect("inspect", "--format", "{{json .State}}", "c0ffee")}
			},
		},
		{
			name:    "image-id",
			replies: []reply{{contains: []string{"image", "inspect"}, stdout: "sha256:c0ffee\n"}},
			run: func(t *testing.T, eng Engine, _ *fakeRunner) {
				id, err := eng.ImageID(context.Background(), "wlvision-runtime:contract")
				if err != nil {
					t.Fatalf("ImageID: %v", err)
				}
				if id != "sha256:c0ffee" {
					t.Errorf("ImageID = %q, want the normalized identifier", id)
				}
			},
			expect: func(adapter contractAdapter) [][]string {
				return [][]string{adapter.expect("image", "inspect", "--format", "{{.Id}}", "wlvision-runtime:contract")}
			},
		},
		{
			name: "build",
			replies: []reply{
				{contains: []string{"build"}, stdout: "build output\n"},
				{contains: []string{"image", "inspect"}, stdout: adapter.buildImageID},
			},
			run: func(t *testing.T, eng Engine, _ *fakeRunner) {
				var stdout strings.Builder
				built, err := eng.Build(context.Background(), BuildSpec{
					Context:       "/tmp/wlvision-build",
					Containerfile: "Containerfile",
					Tag:           "wlvision-build:abc123",
					Stdout:        &stdout,
				})
				if err != nil {
					t.Fatalf("Build: %v", err)
				}
				// Both engines name the same bytes the same way: the algorithm
				// prefix is part of the contract, not of one CLI's output.
				if built.ImageID != "sha256:c0ffee" {
					t.Errorf("ImageID = %q, want the normalized identifier", built.ImageID)
				}
				if stdout.String() != "build output\n" {
					t.Errorf("the build output was not forwarded: %q", stdout.String())
				}
			},
			expect: func(adapter contractAdapter) [][]string {
				return [][]string{
					adapter.expect("build", "--network=none", "--file", "/tmp/wlvision-build/Containerfile", "--tag", "wlvision-build:abc123", "/tmp/wlvision-build"),
					adapter.expect("image", "inspect", "--format", "{{.Id}}", "wlvision-build:abc123"),
				}
			},
		},
		{
			name: "build-with-the-network-only-when-asked",
			replies: []reply{
				{contains: []string{"build"}},
				{contains: []string{"image", "inspect"}, stdout: adapter.buildImageID},
			},
			run: func(t *testing.T, eng Engine, _ *fakeRunner) {
				if _, err := eng.Build(context.Background(), BuildSpec{
					Context:       "/tmp/wlvision-build",
					Containerfile: "Containerfile",
					Tag:           "wlvision-build:abc123",
					Network:       true,
				}); err != nil {
					t.Fatalf("Build: %v", err)
				}
			},
			expect: func(adapter contractAdapter) [][]string {
				return [][]string{
					adapter.expect("build", "--network=default", "--file", "/tmp/wlvision-build/Containerfile", "--tag", "wlvision-build:abc123", "/tmp/wlvision-build"),
					adapter.expect("image", "inspect", "--format", "{{.Id}}", "wlvision-build:abc123"),
				}
			},
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			eng, runner := adapter.with(t, test.replies...)
			test.run(t, eng, runner)
			assertCalls(t, runner, test.expect(adapter))
		})
	}

	t.Run("invalid-specs-never-reach-the-engine", func(t *testing.T) {
		eng, runner := adapter.with(t)

		if _, err := eng.Create(context.Background(), CreateSpec{Session: "contract"}); err == nil {
			t.Error("a spec without an image was accepted")
		} else if failureOf(t, err).Code != result.CodeUsageError {
			t.Errorf("create code = %s, want %s", failureOf(t, err).Code, result.CodeUsageError)
		}
		if _, err := eng.Build(context.Background(), BuildSpec{Context: "relative", Tag: "tag", Containerfile: "Containerfile"}); err == nil {
			t.Error("a relative build context was accepted")
		} else if failureOf(t, err).Code != result.CodeUsageError {
			t.Errorf("build code = %s, want %s", failureOf(t, err).Code, result.CodeUsageError)
		}
		if len(runner.calls) != 0 {
			t.Errorf("the engine ran %d commands for invalid specs", len(runner.calls))
		}
	})
}

// assertCalls compares every recorded command with the expected vector, in
// order, so an extra, missing, or altered argument fails.
func assertCalls(t *testing.T, runner *fakeRunner, want [][]string) {
	t.Helper()

	if len(runner.calls) != len(want) {
		t.Fatalf("the engine ran %d commands, want %d", len(runner.calls), len(want))
	}
	for i, expected := range want {
		got := runner.calls[i].Args
		if strings.Join(got, " ") != strings.Join(expected, " ") {
			t.Errorf("command %d = %v, want %v", i, got, expected)
		}
	}
}

// liveSkips records why an engine's live half did not run. TestMain prints the
// reasons, so a plain `go test` run is never silently green about an engine
// that was never exercised.
var liveSkips sync.Map

func liveSkip(t *testing.T, format string, args ...any) {
	t.Helper()

	reason := fmt.Sprintf(format, args...)
	liveSkips.Store(reason, true)
	t.Skip(reason)
}

func TestMain(m *testing.M) {
	code := m.Run()

	liveSkips.Range(func(key, _ any) bool {
		fmt.Fprintln(os.Stderr, "engine contract: live half skipped:", key)
		return true
	})
	os.Exit(code)
}

// liveEngineContract runs the contract against an installed rootless engine.
// Everything it cannot run is skipped with a reason, never failed silently.
func liveEngineContract(t *testing.T, adapter contractAdapter) {
	t.Helper()

	path, err := exec.LookPath(adapter.binary)
	if err != nil {
		liveSkip(t, "%s: the %q CLI is not installed, so the live half cannot run", adapter.name, adapter.binary)
		return
	}

	eng, err := adapter.newAdapter(CLIRunner{Binary: path}, Options{})
	if err != nil {
		t.Fatalf("constructing the live %s adapter: %v", adapter.name, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	caps, err := eng.Capabilities(ctx)
	if err != nil {
		var failure *result.Failure
		if errors.As(err, &failure) &&
			(failure.Code == result.CodeEngineNotRootless || failure.Code == result.CodeEngineUnavailable) {
			liveSkip(t, "%s: the CLI is installed but no rootless engine is reachable: %v", adapter.name, err)
			return
		}
		t.Fatalf("%s: Capabilities: %v", adapter.name, err)
	}

	// The live half asserts behaviour, not output shapes: the typed fields are
	// normalized by the adapter, whatever the CLI printed.
	if caps.Kind != adapter.kind {
		t.Errorf("live Kind = %s, want %s", caps.Kind, adapter.kind)
	}
	if !caps.Rootless {
		t.Errorf("live engine is not rootless: %+v", caps)
	}
	if caps.SeccompProfile == "" || strings.Contains(caps.SeccompProfile, "/") {
		t.Errorf("live SeccompProfile = %q, want a normalized profile name", caps.SeccompProfile)
	}
	switch caps.CgroupVersion {
	case "", "1", "2":
	default:
		t.Errorf("live CgroupVersion = %q, want the normalized 1 or 2", caps.CgroupVersion)
	}
	if caps.CgroupDriver == "" {
		t.Error("live CgroupDriver is empty")
	}
	for _, degradation := range caps.Degradations() {
		switch degradation {
		case DegradationCgroupV1, DegradationMemoryLimit, DegradationPidsLimit:
		default:
			t.Errorf("live degradation %q is not one of the typed degradations", degradation)
		}
	}

	image := liveContractImage(t, eng, adapter)
	if image == "" {
		liveSkip(t, "%s: no local image with a POSIX shell is available for the live lifecycle; set WLVISION_TEST_IMAGE", adapter.name)
		return
	}
	t.Logf("%s: live contract against image %s, version %s, context %s, cgroups %s/%s, memory:%v pids:%v, degradations %v",
		adapter.name, image, caps.ServerVersion, caps.Context, caps.CgroupVersion, caps.CgroupDriver,
		caps.MemoryLimit, caps.PidsLimit, caps.Degradations())

	liveLifecycle(t, eng, image)
}

// liveImages caches the image a live contract found usable, keyed by engine, so
// repeated runs in one process do not probe again.
var liveImages sync.Map

// liveContractImage picks an image the engine already holds and proves it can
// host the lifecycle. It never pulls: an image that is not local is skipped.
func liveContractImage(t *testing.T, eng Engine, adapter contractAdapter) string {
	t.Helper()

	if cached, ok := liveImages.Load(adapter.kind); ok {
		return cached.(string)
	}

	candidates := make([]string, 0, 6)
	if image := os.Getenv("WLVISION_TEST_IMAGE"); image != "" {
		candidates = append(candidates, image)
	}
	candidates = append(candidates,
		"wlvision-session:local",
		"wlvision-module-runtime:local",
		"alpine:latest",
		"busybox:latest",
		"debian:latest",
	)

	runner := CLIRunner{Binary: adapter.binary}
	for _, image := range candidates {
		if !liveImagePresent(t, runner, image) {
			continue
		}
		if liveImageRunsShell(t, eng, image) {
			liveImages.Store(adapter.kind, image)
			return image
		}
		t.Logf("%s: local image %s cannot run /bin/sh, trying the next candidate", adapter.name, image)
	}

	liveImages.Store(adapter.kind, "")
	return ""
}

func liveImagePresent(t *testing.T, runner CommandRunner, image string) bool {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var stdout bytes.Buffer
	err := runner.Run(ctx, Command{Args: []string{"image", "inspect", "--format", "{{.Id}}", image}, Stdout: &stdout})
	return err == nil && strings.TrimSpace(stdout.String()) != ""
}

// liveImageRunsShell proves the candidate can host the contract's lifecycle
// using the adapter's own fixed policy: create, start, run, remove.
func liveImageRunsShell(t *testing.T, eng Engine, image string) bool {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	spec := contractCreateSpec()
	spec.Image = image
	spec.Name = fmt.Sprintf("wlvision-contract-probe-%d", time.Now().UnixNano())
	spec.Session = "contract-probe"
	spec.Command = []string{"/bin/sh", "-c", "trap 'exit 0' TERM INT; while :; do sleep 1 & wait $!; done"}

	id, err := eng.Create(ctx, spec)
	if err != nil {
		return false
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := eng.Remove(cleanup, id); err != nil && !errors.Is(err, ErrNotFound) {
			t.Logf("probe cleanup of %s: %v", id, err)
		}
	}()

	if err := eng.Start(ctx, id); err != nil {
		return false
	}
	for attempt := 0; attempt < 20; attempt++ {
		execution, err := eng.Exec(ctx, ExecSpec{ContainerID: id, User: spec.ControlUID, Argv: []string{"/bin/sh", "-c", "exit 0"}})
		if err == nil && execution.ExitCode == 0 {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// liveLifecycle runs one session's operations end to end: the state a session
// actually traverses, with each identity, and the cleanup that must follow.
func liveLifecycle(t *testing.T, eng Engine, image string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	spec := contractCreateSpec()
	spec.Image = image
	spec.Name = fmt.Sprintf("wlvision-contract-%s-%d", eng.Kind(), time.Now().UnixNano())
	spec.Command = []string{"/bin/sh", "-c", "trap 'exit 0' TERM INT; echo contract-ready; while :; do sleep 1 & wait $!; done"}

	id, err := eng.Create(ctx, spec)
	if err != nil {
		t.Fatalf("live Create: %v", err)
	}
	removed := false
	t.Cleanup(func() {
		if removed {
			return
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := eng.Remove(cleanup, id); err != nil && !errors.Is(err, ErrNotFound) {
			t.Errorf("live cleanup of %s failed: %v", id, err)
		}
	})

	if state, err := eng.State(ctx, id); err != nil {
		t.Fatalf("live State on the created container: %v", err)
	} else if state.Running {
		t.Error("a created container is already running")
	}

	if err := eng.Start(ctx, id); err != nil {
		t.Fatalf("live Start: %v", err)
	}
	waitForCondition(t, "the container to run", func() bool {
		state, err := eng.State(ctx, id)
		return err == nil && state.Running
	})

	// Each identity must be able to run a command as itself.
	for _, uid := range []uint32{spec.ControlUID, spec.ApplicationUID} {
		stdout, err := liveExec(ctx, eng, id, uid, "/bin/sh", "-c", "id -u")
		if err != nil {
			t.Fatalf("live Exec as uid %d: %v", uid, err)
		}
		if got := strings.TrimSpace(stdout); got != fmt.Sprint(uid) {
			t.Errorf("exec --user %d ran as uid %s", uid, got)
		}
	}

	// A command that ran and exited non-zero is a result, not a failure.
	execution, err := eng.Exec(ctx, ExecSpec{ContainerID: id, User: spec.ControlUID, Argv: []string{"/bin/sh", "-c", "exit 7"}})
	if err != nil {
		t.Fatalf("live Exec of a failing command: %v", err)
	}
	if execution.ExitCode != 7 {
		t.Errorf("live ExitCode = %d, want 7", execution.ExitCode)
	}

	// A stream must land in the container's writable tmpfs.
	var payload strings.Builder
	if err := eng.Stream(ctx, StreamSpec{
		ContainerID: id,
		User:        spec.ApplicationUID,
		Argv:        []string{"/bin/sh", "-c", "cat > /tmp/contract-payload"},
		Source:      strings.NewReader("contract payload"),
		Stdout:      &payload,
	}); err != nil {
		t.Fatalf("live Stream: %v", err)
	}
	readBack, err := liveExec(ctx, eng, id, spec.ApplicationUID, "/bin/sh", "-c", "cat /tmp/contract-payload")
	if err != nil {
		t.Fatalf("live read back of the streamed payload: %v", err)
	}
	if readBack != "contract payload" {
		t.Errorf("streamed payload = %q, want %q", readBack, "contract payload")
	}

	waitForCondition(t, "the container log", func() bool {
		var logs strings.Builder
		return eng.Logs(ctx, id, 100, &logs) == nil && strings.Contains(logs.String(), "contract-ready")
	})

	// The image reference resolves to an identifier the same way everywhere.
	imageID, err := eng.ImageID(ctx, image)
	if err != nil {
		t.Fatalf("live ImageID: %v", err)
	}
	if imageID == "" || !strings.HasPrefix(imageID, "sha256:") {
		t.Errorf("live ImageID = %q, want a sha256 identifier", imageID)
	}

	if err := eng.Stop(ctx, id, 5*time.Second); err != nil {
		t.Fatalf("live Stop: %v", err)
	}
	waitForCondition(t, "the container to stop", func() bool {
		state, err := eng.State(ctx, id)
		return err == nil && !state.Running
	})

	if err := eng.Remove(ctx, id); err != nil {
		t.Fatalf("live Remove: %v", err)
	}
	removed = true

	// A container the engine no longer has is ErrNotFound, not a CLI failure.
	if _, err := eng.State(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("live State after Remove = %v, want ErrNotFound", err)
	}
}

func liveExec(ctx context.Context, eng Engine, id string, uid uint32, argv ...string) (string, error) {
	var stdout strings.Builder
	if _, err := eng.Exec(ctx, ExecSpec{ContainerID: id, User: uid, Argv: argv, Stdout: &stdout}); err != nil {
		return "", err
	}
	return stdout.String(), nil
}

// waitForCondition polls a live engine on a bounded schedule; every wait in
// the contract has a deadline, and a missed one is a test failure.
func waitForCondition(t *testing.T, what string, condition func() bool) {
	t.Helper()

	for attempt := 0; attempt < 50; attempt++ {
		if condition() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
