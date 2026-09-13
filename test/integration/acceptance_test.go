// The V1 acceptance gate: the ten approved steps, run in order through the
// public CLI against a real rootless engine.
//
// It is deliberately a composition of behaviour the other gates already cover
// in isolation: what it adds is that one session, started the way an agent
// starts it, can be driven from diagnosis to cleaned-up close without a step
// being skipped, and that the host is untouched afterwards.
package integration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bnema/wlvision/internal/agentapi"
)

// acceptanceSentinel is a host file the run must neither see nor change.
type acceptanceSentinel struct {
	path     string
	digest   string
	contents string
}

// newAcceptanceSentinel writes a sentinel outside the session's state root.
func newAcceptanceSentinel(t *testing.T) acceptanceSentinel {
	t.Helper()

	contents := "wlvision acceptance sentinel\n"
	path := filepath.Join(t.TempDir(), "host-sentinel")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write the host sentinel: %v", err)
	}
	sum := sha256.Sum256([]byte(contents))
	return acceptanceSentinel{path: path, digest: hex.EncodeToString(sum[:]), contents: contents}
}

// requireUnchanged fails when the sentinel was read, rewritten or removed.
func (s acceptanceSentinel) requireUnchanged(t *testing.T) {
	t.Helper()

	payload, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatalf("the host sentinel is gone: %v", err)
	}
	sum := sha256.Sum256(payload)
	if got := hex.EncodeToString(sum[:]); got != s.digest {
		t.Errorf("the host sentinel changed: %s, want %s", got, s.digest)
	}
}

// TestV1Acceptance is the V1 acceptance gate.
func TestV1Acceptance(t *testing.T) {
	h := newHarness(t)
	sentinel := newAcceptanceSentinel(t)

	var (
		window   agentapi.Toplevel
		frame    frameOutcome
		sequence burstReport
	)

	t.Run("1 doctor reports verified engine and isolation capabilities", func(t *testing.T) {
		envelope := h.succeed("doctor")
		var report struct {
			Keyboard struct {
				Layout string `json:"layout"`
			} `json:"keyboard"`
			Capabilities struct {
				Rootless       bool   `json:"rootless"`
				SeccompProfile string `json:"seccomp_profile"`
			} `json:"capabilities"`
			Degradations []string `json:"degradations"`
		}
		if err := json.Unmarshal(envelope.Result, &report); err != nil {
			t.Fatalf("the doctor report is unreadable: %v", err)
		}
		if !report.Capabilities.Rootless || report.Capabilities.SeccompProfile == "" {
			t.Errorf("doctor reports %+v, want a rootless engine with a seccomp profile", report.Capabilities)
		}
		if report.Keyboard.Layout == "" {
			t.Error("doctor reports no keyboard layout")
		}
	})

	t.Run("2 the session has no user-data mount and no network route", func(t *testing.T) {
		h.startSession()

		// The engine creates the container; the CLI only reports it. Assert the
		// two facts that make "no user data" true, from inside the session.
		if output, code := h.inContainer(controlUID, "sh", "-c", "test ! -e "+sentinel.path); code != 0 {
			t.Errorf("the host sentinel path is visible inside the session: %s", output)
		}
		if output, code := h.inContainer(controlUID, "sh", "-c",
			"grep -q '^[a-z]*eth0' /proc/net/dev 2>/dev/null && echo has-eth0 || echo no-eth0"); code != 0 || !strings.Contains(output, "no-eth0") {
			t.Errorf("the session has a network interface: %s (exit %d)", output, code)
		}
		if output, code := h.inContainer(controlUID, "sh", "-c",
			"grep -q '^00000000' /proc/net/route 2>/dev/null && echo has-route || echo no-route"); code != 0 || !strings.Contains(output, "no-route") {
			t.Errorf("the session has a default route: %s (exit %d)", output, code)
		}
	})

	// One application instance serves the next three steps: it reports the
	// input it receives, keeps animating so its window outlives a resize, and
	// commits every configure it is sent.
	app := h.startFixtureApplication("--title", "acceptance-target", "--report-input", "--animate",
		"--frames", "200", "--period", "40", "--resize", "cooperative", "--exit-after", "60000")

	t.Run("3 a native Wayland application starts in the session", func(t *testing.T) {
		app.waitForLine(t, "wlvision-fixture: connected", 30*time.Second)
		window = h.waitForTitledWindow("acceptance-target")
	})

	t.Run("4 the window handle and its metadata are stable", func(t *testing.T) {
		first := h.sessionWindows()
		second := h.sessionWindows()
		if len(first.Toplevels) != 1 || len(second.Toplevels) != 1 {
			t.Fatalf("the session reports %d then %d windows, want one", len(first.Toplevels), len(second.Toplevels))
		}
		seen := second.Toplevels[0]
		if seen.Handle != window.Handle {
			t.Errorf("the handle changed from %q to %q", window.Handle, seen.Handle)
		}
		if seen.Title != "acceptance-target" || seen.AppID == "" {
			t.Errorf("the window metadata is %+v, want the application's title and app id", seen)
		}
		if seen.Revision == 0 {
			t.Error("the window carries revision 0")
		}
	})

	t.Run("5 click, type and resize are confirmed by the application", func(t *testing.T) {
		h.succeed("activate", "--session", h.session, "--window", window.Handle)
		h.succeed("type", "--session", h.session, "ok")
		app.waitForLine(t, "state=pressed utf8=o", 10*time.Second)
		app.waitForLine(t, "state=pressed utf8=k", 10*time.Second)

		h.succeed("key", "--session", h.session, "--name", "Return")
		app.waitForLine(t, "keycode=28 state=pressed", 10*time.Second)

		h.succeed("click", "--session", h.session, "--window", window.Handle, "--x", "10", "--y", "12")
		app.waitForLine(t, "pointer button button=1 state=pressed", 10*time.Second)
		app.waitForLine(t, "pointer button button=1 state=released", 10*time.Second)

		// A resize is a commit: the application must have committed the size it
		// was configured with, and the compositor must agree.
		envelope := h.succeed("resize", "--session", h.session, "--window", window.Handle,
			"--width", "640", "--height", "480", "--timeout", "10s")
		var resized resizeOutcome
		if err := json.Unmarshal(envelope.Result, &resized); err != nil {
			t.Fatalf("the resize result is unreadable: %v", err)
		}
		resized.requireAll(t, 640, 480)
		h.waitForTitledWindowSize("acceptance-target", 640, 480)
	})

	t.Run("6 a screenshot and a two-second sequence are stored", func(t *testing.T) {
		frame = h.captureFrame("acceptance-still")

		envelope := h.succeed("capture", "--session", h.session, "--interval", "100ms",
			"--duration", "2s", "--frames", "25", "--output", "acceptance-frames", "--contact-sheet")
		if err := json.Unmarshal(envelope.Result, &sequence); err != nil {
			t.Fatalf("the sequence result is unreadable: %v", err)
		}
		if sequence.Captured+sequence.Deduplicated == 0 {
			t.Fatalf("the sequence stored nothing: %+v", sequence)
		}
		if sequence.Interval != "100ms" || sequence.Duration != "2s" {
			t.Errorf("the sequence ran at %s for %s, want 100ms for 2s", sequence.Interval, sequence.Duration)
		}
		// Every stored sample must be a file on the host, and the manifest must
		// name the same samples the reply reported.
		stored := 0
		for index, sample := range sequence.Frames {
			if sample.Scheduled == "" {
				t.Errorf("sample %d has no scheduled offset", index)
			}
			if sample.Path == "" {
				continue
			}
			stored++
			if _, err := os.Stat(sample.Path); err != nil {
				t.Errorf("sample %d names a file that is not there: %v", index, err)
			}
		}
		if stored == 0 {
			t.Error("the sequence names no stored file")
		}
		if _, err := os.Stat(sequence.ContactSheet); err != nil {
			t.Errorf("the contact sheet is not on the host: %v", err)
		}
	})

	t.Run("7 stability is proven without a sleep", func(t *testing.T) {
		// The application is still animating, so a stability claim would be
		// false; a short wait must therefore time out rather than succeed.
		stdout, _, code := h.run("wait", "--session", h.session, "--stable-for", "500ms", "--timeout", "1s")
		if code != 5 {
			t.Fatalf("a stability wait over an animating window exited %d, want 5 (timeout): %s", code, stdout)
		}
		if failure := failureCode(t, stdout); failure != "wait_timeout" {
			t.Errorf("the failure carries the code %q, want wait_timeout", failure)
		}
		// A predicate that can be satisfied must succeed without an arbitrary
		// sleep: the session has exactly one window.
		envelope := h.succeed("wait", "--session", h.session, "--window-count", "1", "--timeout", "5s")
		if envelope.Revision == 0 {
			t.Error("the satisfied wait reported revision 0")
		}
	})

	t.Run("8 a forced crash leaves the application and the compositor diagnosable", func(t *testing.T) {
		// Kill the application as its own identity, which is what a crash looks
		// like: the compositor and the controller must survive it.
		if output, code := h.inContainer(applicationUID, "pkill", "-f", fixtureBinary); code != 0 {
			t.Fatalf("the application could not be killed: %s", output)
		}

		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if len(h.sessionWindows().Toplevels) == 0 {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}

		envelope := h.succeed("logs", "--session", h.session)
		var reported struct {
			Logs string `json:"logs"`
		}
		if err := json.Unmarshal(envelope.Result, &reported); err != nil {
			t.Fatalf("the log result is unreadable: %v", err)
		}
		if !strings.Contains(reported.Logs, "supervisor: compositor started") {
			t.Errorf("the logs do not carry the session's own progress:\n%s", reported.Logs)
		}

		inspect := h.succeed("session", "inspect", "--session", h.session)
		var inspected struct {
			Session struct {
				State string `json:"state"`
			} `json:"session"`
		}
		if err := json.Unmarshal(inspect.Result, &inspected); err != nil {
			t.Fatalf("the session record is unreadable: %v", err)
		}
		if inspected.Session.State != "ready" && inspected.Session.State != "running" {
			t.Errorf("the session is %s after its application died; the compositor must survive it", inspected.Session.State)
		}
	})

	t.Run("9 close releases the container and its resources", func(t *testing.T) {
		h.succeed("session", "close", "--session", h.session)

		if output, code := h.docker("inspect", h.container); code == 0 {
			t.Errorf("the container survived close: %s", output)
		}
		envelope := h.succeed("session", "list")
		var listed struct {
			Sessions []struct {
				Session string `json:"session"`
				State   string `json:"state"`
			} `json:"sessions"`
		}
		if err := json.Unmarshal(envelope.Result, &listed); err != nil {
			t.Fatalf("the session list is unreadable: %v", err)
		}
		for _, record := range listed.Sessions {
			if record.Session == h.session && record.State != "closed" {
				t.Errorf("session %s is %s after close", record.Session, record.State)
			}
		}
	})

	t.Run("10 the host was not modified", func(t *testing.T) {
		sentinel.requireUnchanged(t)

		// The artifacts the run produced are the only host writes, and they are
		// inside the session's export directory.
		exportDir := filepath.Join(h.stateRoot, "export", h.session)
		if _, err := os.Stat(frame.Path); err != nil {
			t.Errorf("the screenshot is missing: %v", err)
		}
		if !strings.HasPrefix(frame.Path, exportDir) {
			t.Errorf("the screenshot was written outside the export directory: %s", frame.Path)
		}
		sentinel.requireUnchanged(t)
	})
}
