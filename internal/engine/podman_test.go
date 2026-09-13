package engine

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/bnema/wlvision/internal/result"
)

// Podman info fixtures. Field names follow `podman info --format json`.
const (
	podmanInfoRootlessCgroupV2 = `{
		"host": {
			"cgroupVersion": "v2",
			"cgroupManager": "systemd",
			"cgroupControllers": ["cpuset", "cpu", "io", "memory", "pids"],
			"security": {
				"rootless": true,
				"seccompEnabled": true,
				"seccompProfilePath": "/usr/share/containers/seccomp.json"
			}
		},
		"version": {"Version": "5.4.1"}
	}`

	podmanInfoRootlessControllersAsText = `{
		"host": {
			"cgroupVersion": "v2",
			"cgroupManager": "systemd",
			"cgroupControllers": "cpuset cpu io memory pids",
			"security": {
				"rootless": true,
				"seccompEnabled": true,
				"seccompProfilePath": "default"
			}
		},
		"version": {"Version": "4.9.3"}
	}`

	// A version that reports no delegated controllers at all must not be read
	// as one that delegates them.
	podmanInfoRootlessWithoutControllerReport = `{
		"host": {
			"cgroupVersion": "v2",
			"cgroupManager": "systemd",
			"security": {
				"rootless": true,
				"seccompEnabled": true,
				"seccompProfilePath": "/usr/share/containers/seccomp.json"
			}
		},
		"version": {"Version": "4.4.1"}
	}`

	podmanInfoRootful = `{
		"host": {
			"cgroupVersion": "v2",
			"cgroupManager": "systemd",
			"cgroupControllers": ["memory", "pids"],
			"security": {
				"rootless": false,
				"seccompEnabled": true,
				"seccompProfilePath": "/usr/share/containers/seccomp.json"
			}
		},
		"version": {"Version": "5.4.1"}
	}`

	podmanInfoWithoutSeccomp = `{
		"host": {
			"cgroupVersion": "v2",
			"cgroupManager": "systemd",
			"cgroupControllers": ["memory", "pids"],
			"security": {
				"rootless": true,
				"seccompEnabled": false,
				"seccompProfilePath": ""
			}
		},
		"version": {"Version": "5.4.1"}
	}`

	podmanInfoUnconfinedSeccomp = `{
		"host": {
			"cgroupVersion": "v2",
			"cgroupManager": "systemd",
			"cgroupControllers": ["memory", "pids"],
			"security": {
				"rootless": true,
				"seccompEnabled": true,
				"seccompProfilePath": "unconfined"
			}
		},
		"version": {"Version": "5.4.1"}
	}`

	podmanInfoRootlessCgroupV1NoDelegation = `{
		"host": {
			"cgroupVersion": "v1",
			"cgroupManager": "cgroupfs",
			"cgroupControllers": [],
			"security": {
				"rootless": true,
				"seccompEnabled": true,
				"seccompProfilePath": "/usr/share/containers/seccomp.json"
			}
		},
		"version": {"Version": "4.1.0"}
	}`
)

func podmanWith(t *testing.T, replies ...reply) (*Podman, *fakeRunner) {
	t.Helper()

	runner := &fakeRunner{replies: append([]reply(nil), replies...), t: t}
	eng, err := NewPodman(runner, Options{Context: "wlvision-test"})
	if err != nil {
		t.Fatalf("NewPodman: %v", err)
	}
	return eng, runner
}

func TestPodmanCapabilitiesTranslateTheEngineReport(t *testing.T) {
	eng, runner := podmanWith(t, reply{contains: []string{"info", "--format"}, stdout: podmanInfoRootlessCgroupV2})

	caps, err := eng.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}

	if caps.Kind != KindPodman {
		t.Errorf("Kind = %s, want %s", caps.Kind, KindPodman)
	}
	if caps.Context != "wlvision-test" {
		t.Errorf("Context = %q, want the configured connection", caps.Context)
	}
	if caps.ServerVersion != "5.4.1" {
		t.Errorf("ServerVersion = %q, want 5.4.1", caps.ServerVersion)
	}
	if !caps.Rootless {
		t.Error("Rootless = false, want true")
	}
	// Podman reports the profile by host path; the capability is the fact that
	// a profile applies, and no host path may escape the adapter.
	if caps.SeccompProfile != "default" {
		t.Errorf("SeccompProfile = %q, want the normalized profile name", caps.SeccompProfile)
	}
	if caps.CgroupVersion != "2" || caps.CgroupDriver != "systemd" {
		t.Errorf("cgroups = %s/%s, want 2/systemd", caps.CgroupVersion, caps.CgroupDriver)
	}
	if !caps.MemoryLimit || !caps.PidsLimit {
		t.Errorf("controllers = memory:%v pids:%v, want both delegated", caps.MemoryLimit, caps.PidsLimit)
	}
	if len(caps.Degradations()) != 0 {
		t.Errorf("Degradations = %v, want none", caps.Degradations())
	}

	call := runner.lastCall(t)
	want := []string{"--connection", "wlvision-test", "info", "--format", "json"}
	if strings.Join(call.Args, " ") != strings.Join(want, " ") {
		t.Errorf("info command = %v, want %v", call.Args, want)
	}
}

func TestPodmanCapabilitiesReadSpaceSeparatedControllers(t *testing.T) {
	eng, _ := podmanWith(t, reply{contains: []string{"info", "--format"}, stdout: podmanInfoRootlessControllersAsText})

	caps, err := eng.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if !caps.MemoryLimit || !caps.PidsLimit {
		t.Errorf("controllers = memory:%v pids:%v, want both delegated", caps.MemoryLimit, caps.PidsLimit)
	}
	if caps.SeccompProfile != "default" {
		t.Errorf("SeccompProfile = %q, want the normalized profile name", caps.SeccompProfile)
	}
}

func TestPodmanCapabilitiesReportUnconfirmedControllersAsDegradations(t *testing.T) {
	eng, _ := podmanWith(t, reply{contains: []string{"info", "--format"}, stdout: podmanInfoRootlessWithoutControllerReport})

	caps, err := eng.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if caps.MemoryLimit || caps.PidsLimit {
		t.Errorf("controllers = memory:%v pids:%v, want both unavailable when unconfirmed", caps.MemoryLimit, caps.PidsLimit)
	}

	want := []string{DegradationMemoryLimit, DegradationPidsLimit}
	if strings.Join(caps.Degradations(), ",") != strings.Join(want, ",") {
		t.Errorf("Degradations = %v, want %v", caps.Degradations(), want)
	}
	// A caller asking for what the engine cannot enforce learns which limits
	// are missing instead of assuming they are active.
	missing := caps.MissingLimits(Limits{MemoryBytes: 512 << 20, Pids: 64})
	if strings.Join(missing, ",") != strings.Join(want, ",") {
		t.Errorf("MissingLimits = %v, want %v", missing, want)
	}
}

func TestPodmanCapabilitiesRejectARootfulEngine(t *testing.T) {
	eng, _ := podmanWith(t, reply{contains: []string{"info", "--format"}, stdout: podmanInfoRootful})

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
}

func TestPodmanCapabilitiesRefuseAnEngineWithoutASeccompProfile(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
	}{
		{name: "seccomp disabled", fixture: podmanInfoWithoutSeccomp},
		{name: "seccomp unconfined", fixture: podmanInfoUnconfinedSeccomp},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			eng, _ := podmanWith(t, reply{contains: []string{"info", "--format"}, stdout: test.fixture})

			_, err := eng.Capabilities(context.Background())
			if err == nil {
				t.Fatal("an engine without an effective seccomp profile was accepted")
			}
			failure := failureOf(t, err)
			if failure.Code != result.CodeProtectionDegraded {
				t.Errorf("code = %s, want %s", failure.Code, result.CodeProtectionDegraded)
			}
			if failure.Details["missing"] != "seccomp" {
				t.Errorf("details = %v, want missing=seccomp", failure.Details)
			}
		})
	}
}

func TestPodmanCapabilitiesNameTheLocalEndpoint(t *testing.T) {
	runner := &fakeRunner{
		t:       t,
		replies: []reply{{contains: []string{"info", "--format"}, stdout: podmanInfoRootlessCgroupV2}},
	}
	eng, err := NewPodman(runner, Options{})
	if err != nil {
		t.Fatalf("NewPodman: %v", err)
	}

	caps, err := eng.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if caps.Context == "" {
		t.Error("Context is empty; the session record would not name the engine endpoint")
	}
	if call := runner.lastCall(t); strings.Contains(strings.Join(call.Args, " "), "--connection") {
		t.Errorf("info command %v selected a connection the caller did not configure", call.Args)
	}
}

func TestPodmanCreateIsTheDockerIsolationContract(t *testing.T) {
	eng, runner := podmanWith(t, reply{contains: []string{"create"}, stdout: "c0ffee\n"})

	if _, err := eng.Create(context.Background(), contractCreateSpec()); err != nil {
		t.Fatalf("Create: %v", err)
	}

	want := append([]string{"--connection", "wlvision-test"}, contractCreateVector...)
	got := runner.lastCall(t).Args
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("create command = %v\nwant %v", got, want)
	}
}

func TestPodmanBuildConstructsTheCommandAndNormalizesTheImageID(t *testing.T) {
	eng, runner := podmanWith(t,
		reply{contains: []string{"build"}, stdout: "build output\n"},
		// Podman reports a bare digest where Docker reports a prefixed one.
		reply{contains: []string{"image", "inspect"}, stdout: "c0ffee\n"},
	)

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
	if built.ImageID != "sha256:c0ffee" {
		t.Errorf("ImageID = %q, want the normalized identifier", built.ImageID)
	}
	if stdout.String() != "build output\n" {
		t.Errorf("the build output was not forwarded: %q", stdout.String())
	}

	if len(runner.calls) != 2 {
		t.Fatalf("the engine ran %d commands, want a build and an inspect", len(runner.calls))
	}
	wantBuild := []string{"--connection", "wlvision-test", "build", "--network=none", "--file", "/tmp/wlvision-build/Containerfile", "--tag", "wlvision-build:abc123", "/tmp/wlvision-build"}
	if got := strings.Join(runner.calls[0].Args, " "); got != strings.Join(wantBuild, " ") {
		t.Errorf("build command = %v, want %v", got, wantBuild)
	}
}

func TestPodmanImageIDNormalizesTheImageIdentifier(t *testing.T) {
	eng, runner := podmanWith(t, reply{contains: []string{"image", "inspect"}, stdout: "c0ffee\n"})

	id, err := eng.ImageID(context.Background(), "wlvision-runtime:test")
	if err != nil {
		t.Fatalf("ImageID: %v", err)
	}
	if id != "sha256:c0ffee" {
		t.Errorf("ImageID = %q, want the normalized identifier", id)
	}
	call := runner.lastCall(t)
	want := []string{"--connection", "wlvision-test", "image", "inspect", "--format", "{{.Id}}", "wlvision-runtime:test"}
	if strings.Join(call.Args, " ") != strings.Join(want, " ") {
		t.Errorf("image inspect command = %v, want %v", call.Args, want)
	}
}

func TestPodmanImageIDReportsAnImageTheEngineDoesNotHold(t *testing.T) {
	eng, _ := podmanWith(t, reply{
		contains: []string{"image", "inspect"},
		stderr:   "Error: no such image: wlvision-runtime:test",
		err:      &exec.ExitError{},
	})

	_, err := eng.ImageID(context.Background(), "wlvision-runtime:test")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("ImageID error = %v, want ErrNotFound", err)
	}
}

func TestPodmanImageIDRefusesAnUnusableReference(t *testing.T) {
	eng, runner := podmanWith(t)

	for _, reference := range []string{"", "-wlvision-runtime:test"} {
		if _, err := eng.ImageID(context.Background(), reference); err == nil {
			t.Errorf("ImageID(%q) was accepted", reference)
		} else if failure := failureOf(t, err); failure.Code != result.CodeUsageError {
			t.Errorf("ImageID(%q) code = %s, want %s", reference, failure.Code, result.CodeUsageError)
		}
	}
	if len(runner.calls) != 0 {
		t.Errorf("the engine ran %d commands for invalid references", len(runner.calls))
	}
}

func TestPodmanImageIDRefusesAnImageWithoutAnIdentifier(t *testing.T) {
	eng, _ := podmanWith(t, reply{contains: []string{"image", "inspect"}, stdout: "\n"})

	_, err := eng.ImageID(context.Background(), "wlvision-runtime:test")
	if err == nil {
		t.Fatal("an image without an identifier was accepted")
	}
	if failure := failureOf(t, err); failure.Code != result.CodeImageUnavailable {
		t.Errorf("code = %s, want %s", failure.Code, result.CodeImageUnavailable)
	}
}

func TestPodmanReportsAContainerTheEngineNoLongerHas(t *testing.T) {
	// Podman words a missing container differently from Docker; both must map
	// to the same ErrNotFound.
	missing := "Error: no container with name or ID \"c0ffee\" found: no such container"

	t.Run("state", func(t *testing.T) {
		eng, _ := podmanWith(t, reply{contains: []string{"inspect"}, stderr: missing, err: &exec.ExitError{}})

		if _, err := eng.State(context.Background(), "c0ffee"); !errors.Is(err, ErrNotFound) {
			t.Errorf("State error = %v, want ErrNotFound", err)
		}
	})

	t.Run("another failure stays an engine failure", func(t *testing.T) {
		eng, _ := podmanWith(t, reply{
			contains: []string{"inspect"},
			stderr:   "Error: cannot connect to Podman socket",
			err:      &exec.ExitError{},
		})

		err := func() error { _, err := eng.State(context.Background(), "c0ffee"); return err }()
		if errors.Is(err, ErrNotFound) {
			t.Error("an unavailable engine was reported as a missing container")
		}
		if failure := failureOf(t, err); failure.Code != result.CodeEngineUnavailable {
			t.Errorf("code = %s, want %s", failure.Code, result.CodeEngineUnavailable)
		}
	})
}

func TestPodmanStartTreatsAnAlreadyRunningContainerAsSuccess(t *testing.T) {
	eng, _ := podmanWith(t, reply{
		contains: []string{"start"},
		stderr:   "Error: container c0ffee is already running",
		err:      &exec.ExitError{},
	})

	if err := eng.Start(context.Background(), "c0ffee"); err != nil {
		t.Fatalf("Start on a running container: %v", err)
	}
}

func TestPodmanExecReportsTheApplicationExitCode(t *testing.T) {
	eng, _ := podmanWith(t, reply{
		contains: []string{"exec"},
		err:      &ExitError{Code: 3, Err: errors.New("exit status 3")},
	})

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

func TestPodmanRejectsAnInvalidConfiguration(t *testing.T) {
	if _, err := NewPodman(nil, Options{}); err == nil {
		t.Error("NewPodman accepted a nil runner")
	}
	if _, err := NewPodman(&fakeRunner{t: t}, Options{Context: "--privileged"}); err == nil {
		t.Error("NewPodman accepted a connection that could be read as a flag")
	}
	if err := (&Podman{}).Stop(context.Background(), "", time.Second); err == nil {
		t.Error("Stop accepted an empty container identifier")
	}
}
