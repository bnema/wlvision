// Package integration holds the gates that need a real container engine.
//
// It is skipped unless a harness provides a session image and a built CLI, so
// `go test ./...` stays runnable on a machine without Docker. `run.sh` in this
// directory builds both and sets the environment this file reads.
package integration

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bnema/wlvision/internal/result"
	"github.com/bnema/wlvision/internal/session"
)

// Container layout the session image and the CLI agree on. They are repeated
// here so the gate checks the deployed reality rather than the constants it is
// supposed to be verifying.
const (
	runtimeDir     = "/run/wlvision"
	controlDir     = runtimeDir + "/control"
	waylandDir     = runtimeDir + "/wayland"
	waylandDisplay = "wlvision-1"
	payloadDir     = runtimeDir + "/payload"
	exportDir      = runtimeDir + "/export"
	callBinary     = "/usr/libexec/wlvision-call"
	fixtureClient  = "/usr/local/bin/wlvision-shell-client"
)

const (
	controlUID     = 1000
	applicationUID = 1001
)

// harness drives one session through the CLI and the engine.
type harness struct {
	t         *testing.T
	cli       string
	image     string
	stateRoot string
	session   string
	container string
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	image := os.Getenv("WLVISION_TEST_IMAGE")
	cli := os.Getenv("WLVISION_TEST_CLI")
	stateRoot := os.Getenv("WLVISION_TEST_STATE")
	if image == "" || cli == "" || stateRoot == "" {
		t.Skip("set WLVISION_TEST_IMAGE, WLVISION_TEST_CLI and WLVISION_TEST_STATE (test/integration/run.sh does)")
	}
	if _, err := os.Stat(cli); err != nil {
		t.Fatalf("the CLI binary is not usable: %v", err)
	}

	// The state root is shared between the commands, which is how a session
	// created by one invocation is found by the next.
	sessionID := fmt.Sprintf("lifecycle-%d", time.Now().UnixNano()%100000)
	h := &harness{
		t:         t,
		cli:       cli,
		image:     image,
		stateRoot: stateRoot,
		session:   sessionID,
		container: "wlvision-" + sessionID,
	}

	t.Cleanup(func() {
		// A failed test must not leave a container behind.
		_, _ = h.docker("rm", "--force", h.container)
	})

	return h
}

func (h *harness) cliArgs(args ...string) []string {
	return append([]string{"--json", "--state-root", h.stateRoot, "--image", h.image}, args...)
}

// run executes the CLI and returns its stdout, stderr and exit code.
func (h *harness) run(args ...string) (string, string, int) {
	h.t.Helper()

	command := exec.Command(h.cli, h.cliArgs(args...)...)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr

	err := command.Run()
	code := 0
	var exit *exec.ExitError
	if err != nil {
		if !asExitError(err, &exit) {
			h.t.Fatalf("the CLI could not run: %v", err)
		}
		code = exit.ExitCode()
	}
	return stdout.String(), stderr.String(), code
}

// succeed runs the CLI and requires an accepted envelope.
func (h *harness) succeed(args ...string) result.Envelope[json.RawMessage] {
	h.t.Helper()

	stdout, stderr, code := h.run(args...)
	if code != 0 {
		h.t.Fatalf("%v exited %d\nstdout: %s\nstderr: %s", args, code, stdout, stderr)
	}

	var envelope result.Envelope[json.RawMessage]
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		h.t.Fatalf("the CLI did not answer with one document: %v (%q)", err, stdout)
	}
	if !envelope.Ok {
		h.t.Fatalf("%v failed: %+v (stderr: %s)", args, envelope.Error, stderr)
	}
	return envelope
}

// docker runs a command with the engine CLI and returns its combined output.
func (h *harness) docker(args ...string) (string, int) {
	h.t.Helper()

	command := exec.Command("docker", args...)
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output

	err := command.Run()
	code := 0
	var exit *exec.ExitError
	if err != nil {
		if !asExitError(err, &exit) {
			h.t.Fatalf("docker could not run: %v", err)
		}
		code = exit.ExitCode()
	}
	return output.String(), code
}

// inContainer runs a command in the session container as one identity.
func (h *harness) inContainer(uid uint32, argv ...string) (string, int) {
	h.t.Helper()

	args := []string{"exec", "--user", fmt.Sprint(uid), h.container}
	args = append(args, argv...)
	return h.docker(args...)
}

// requireInContainer runs a command and requires it to succeed.
func (h *harness) requireInContainer(uid uint32, argv ...string) string {
	h.t.Helper()

	output, code := h.inContainer(uid, argv...)
	if code != 0 {
		h.t.Fatalf("in container as %d: %v exited %d: %s", uid, argv, code, output)
	}
	return output
}

// sha256Hex is the digest form the session reports for a payload.
func sha256Hex(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func asExitError(err error, target **exec.ExitError) bool {
	exit, ok := err.(*exec.ExitError)
	if ok {
		*target = exit
	}
	return ok
}

// TestLifecycle is the Phase 4 gate: one session, created through the CLI
// against a real rootless engine, driven from start to cleanup while the
// isolation contract is checked at every step.
func TestLifecycle(t *testing.T) {
	h := newHarness(t)

	t.Run("doctor accepts the engine", func(t *testing.T) {
		envelope := h.succeed("doctor")

		var report session.DoctorReport
		if err := json.Unmarshal(envelope.Result, &report); err != nil {
			t.Fatalf("the doctor report is unreadable: %v", err)
		}
		if !report.Capabilities.Rootless {
			t.Fatalf("the engine is not rootless: %+v", report.Capabilities)
		}
		if report.Capabilities.SeccompProfile == "" {
			t.Error("the engine applies no seccomp profile")
		}
	})

	t.Run("create waits for readiness", func(t *testing.T) {
		h.succeed("session", "create", "--session", h.session, "--wait")

		envelope := h.succeed("session", "inspect", "--session", h.session)
		var record session.Record
		if err := json.Unmarshal(envelope.Result, &record); err != nil {
			t.Fatalf("the session record is unreadable: %v", err)
		}
		if record.State != session.StateReady {
			t.Fatalf("session state = %s, want ready", record.State)
		}
		if record.ContainerID == "" {
			t.Error("the record names no container")
		}
	})

	t.Run("the session is isolated", func(t *testing.T) {
		// The application identity cannot read the control directory nor reach
		// the controller, which is the whole point of the two identities.
		if output, code := h.inContainer(applicationUID, "ls", controlDir); code == 0 {
			t.Errorf("the application identity listed the control directory: %s", output)
		}
		if output, code := h.inContainer(applicationUID, callBinary, "snapshot"); code == 0 {
			t.Errorf("the application identity reached the controller: %s", output)
		}

		// The image is read-only.
		if output, code := h.inContainer(controlUID, "touch", "/usr/libexec/forbidden"); code == 0 {
			t.Errorf("the session wrote outside its mounts: %s", output)
		}

		// No network interface was granted.
		interfaces := h.requireInContainer(controlUID, "ls", "/sys/class/net")
		if strings.TrimSpace(interfaces) != "lo" {
			t.Errorf("the session has interfaces %q, want only lo", interfaces)
		}

		// A host path is not visible inside the session, so nothing of the
		// host was mounted into it.
		sentinel := filepath.Join(os.TempDir(), fmt.Sprintf("wlvision-sentinel-%d", time.Now().UnixNano()))
		if err := os.WriteFile(sentinel, []byte("host"), 0o600); err != nil {
			t.Fatalf("write the host sentinel: %v", err)
		}
		defer func() { _ = os.Remove(sentinel) }()

		if output, code := h.inContainer(controlUID, "test", "!", "-e", sentinel); code != 0 {
			t.Errorf("the host sentinel %s is visible in the session: %s", sentinel, output)
		}
	})

	t.Run("a payload is executable by the application identity", func(t *testing.T) {
		payload := "#!/bin/sh\necho payload-ran\n"
		digest := "sha256:" + sha256Hex([]byte(payload))

		envelope := h.succeedStdin(payload, "inject", "--session", h.session, "--binary", "app", "--mode", "0755")
		var injected session.PayloadResult
		if err := json.Unmarshal(envelope.Result, &injected); err != nil {
			t.Fatalf("the injection result is unreadable: %v", err)
		}
		if injected.Digest != digest {
			t.Errorf("digest = %s, want %s", injected.Digest, digest)
		}

		// The control UID owns the payload; the application UID runs it.
		output := h.requireInContainer(applicationUID, payloadDir+"/app")
		if strings.TrimSpace(output) != "payload-ran" {
			t.Errorf("the payload printed %q, want its own output", output)
		}
	})

	t.Run("the CLI runs an application in the session", func(t *testing.T) {
		h.succeed("run", "--session", h.session, "--", "/bin/true")
	})

	t.Run("an application's window is observable", func(t *testing.T) {
		h.startFixtureClient()
		h.waitForWindow()
	})

	t.Run("the session can be captured", func(t *testing.T) {
		reply := h.call("capture", `{"path":"shot.png"}`)

		var captured struct {
			Frame struct {
				Path   string `json:"path"`
				Digest string `json:"digest"`
				Width  int    `json:"width"`
			} `json:"frame"`
		}
		if err := json.Unmarshal([]byte(reply), &captured); err != nil {
			t.Fatalf("the capture reply is unreadable: %v (%q)", err, reply)
		}
		if captured.Frame.Digest == "" || captured.Frame.Width == 0 {
			t.Fatalf("the capture reported %+v", captured.Frame)
		}

		stored := exportDir + "/shot.png"
		if captured.Frame.Path != stored {
			t.Errorf("capture path = %q, want %q", captured.Frame.Path, stored)
		}
		if output, code := h.inContainer(controlUID, "test", "-s", stored); code != 0 {
			t.Errorf("the capture is not stored: %s", output)
		}
	})

	t.Run("the logs carry the session's own progress", func(t *testing.T) {
		envelope := h.succeed("logs", "--session", h.session)
		var reported struct {
			Logs string `json:"logs"`
		}
		if err := json.Unmarshal(envelope.Result, &reported); err != nil {
			t.Fatalf("the log result is unreadable: %v", err)
		}
		for _, want := range []string{"supervisor: the session is ready", "supervisor: compositor started"} {
			if !strings.Contains(reported.Logs, want) {
				t.Errorf("the logs do not contain %q:\n%s", want, reported.Logs)
			}
		}
	})

	t.Run("an application that dies leaves the session usable", func(t *testing.T) {
		// The compositor, the controller, and the record outlive the
		// application: that is what makes a failed run diagnosable.
		_, _ = h.inContainer(controlUID, "pkill", "-f", "wlvision-shell-client")

		envelope := h.succeed("session", "inspect", "--session", h.session)
		var record session.Record
		if err := json.Unmarshal(envelope.Result, &record); err != nil {
			t.Fatalf("the session record is unreadable: %v", err)
		}
		if record.State != session.StateReady {
			t.Errorf("session state = %s, want the session to survive its application", record.State)
		}
	})

	t.Run("close removes the container", func(t *testing.T) {
		h.succeed("session", "close", "--session", h.session)

		if output, code := h.docker("inspect", h.container); code == 0 {
			t.Errorf("the container survived close: %s", output)
		}

		envelope := h.succeed("session", "list")
		var listed struct {
			Sessions []session.Record `json:"sessions"`
		}
		if err := json.Unmarshal(envelope.Result, &listed); err != nil {
			t.Fatalf("the session list is unreadable: %v", err)
		}
		for _, record := range listed.Sessions {
			if record.Session == h.session && record.State != session.StateClosed {
				t.Errorf("session %s is %s after close", record.Session, record.State)
			}
		}
	})
}

// startFixtureClient runs the image's Wayland client as the application
// identity, which is what an `inject`ed payload would do in a real run.
func (h *harness) startFixtureClient() {
	h.t.Helper()

	output, code := h.docker("exec", "-d",
		"--user", fmt.Sprint(applicationUID),
		"-e", "XDG_RUNTIME_DIR="+waylandDir,
		"-e", "WAYLAND_DISPLAY="+waylandDisplay,
		h.container, fixtureClient,
	)
	if code != 0 {
		h.t.Fatalf("the fixture client did not start: %s", output)
	}
}

// waitForWindow waits until the controller reports a window.
func (h *harness) waitForWindow() {
	h.t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		reply := h.call("snapshot", "")
		if strings.Contains(reply, `"toplevels"`) && strings.Contains(reply, "app-") {
			return
		}
		last = reply
		time.Sleep(200 * time.Millisecond)
	}
	h.t.Fatalf("no window appeared in the session; the last snapshot was %s", last)
}

// call exchanges one operation with the resident controller, as the control
// identity, through the one-shot caller the image ships.
func (h *harness) call(operation, params string) string {
	h.t.Helper()

	argv := []string{callBinary, operation}
	if params != "" {
		argv = append(argv, "--params", params)
	}

	output, code := h.inContainer(controlUID, argv...)
	if code != 0 {
		h.t.Fatalf("%s failed with %d: %s", operation, code, output)
	}
	return strings.TrimSpace(output)
}

// succeedStdin runs the CLI with a payload on its standard input.
func (h *harness) succeedStdin(stdin string, args ...string) result.Envelope[json.RawMessage] {
	h.t.Helper()

	command := exec.Command(h.cli, h.cliArgs(args...)...)
	command.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr

	if err := command.Run(); err != nil {
		h.t.Fatalf("%v failed: %v\nstdout: %s\nstderr: %s", args, err, stdout.String(), stderr.String())
	}

	var envelope result.Envelope[json.RawMessage]
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		h.t.Fatalf("the CLI did not answer with one document: %v (%q)", err, stdout.String())
	}
	if !envelope.Ok {
		h.t.Fatalf("%v failed: %+v (stderr: %s)", args, envelope.Error, stderr.String())
	}
	return envelope
}
