// Phase 5 gates: window interaction and temporal vision against a real
// rootless session.
//
// These tests drive the outer CLI exactly as an agent does: create a session,
// start the fixture application in it, then interact and capture. The fixture's
// own output is the evidence for input, and the artifacts the CLI stores under
// the session's export directory are the evidence for capture.
package integration

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bnema/wlvision/internal/agentapi"
)

// fixtureBinary is the Phase 5 fixture the pinned shell image installs.
const fixtureBinary = "/usr/local/bin/wlvision-fixture"

// lockedBuffer collects the output of a running application, which is written
// from the CLI's process while the test reads it.
type lockedBuffer struct {
	mu   sync.Mutex
	data bytes.Buffer
}

func (b *lockedBuffer) Write(payload []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.data.Write(payload)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.data.String()
}

// fixtureRun is the fixture application running as the session's application.
type fixtureRun struct {
	app    *exec.Cmd
	stdout *lockedBuffer
	stderr *lockedBuffer
	done   chan struct{}
	code   int
}

// startFixtureApplication runs the fixture through the CLI's run command, which
// is the path an agent takes: the application's own output comes back on the
// CLI's streams (its stdout on stderr, because --json keeps stdout for the one
// envelope).
func (h *harness) startFixtureApplication(args ...string) *fixtureRun {
	h.t.Helper()

	argv := append([]string{"run", "--session", h.session, "--", fixtureBinary}, args...)
	run := &fixtureRun{stdout: &lockedBuffer{}, stderr: &lockedBuffer{}, done: make(chan struct{})}
	run.app = exec.Command(h.cli, h.cliArgs(argv...)...)
	run.app.Stdout = run.stdout
	run.app.Stderr = run.stderr

	if err := run.app.Start(); err != nil {
		h.t.Fatalf("the fixture application could not start: %v", err)
	}
	go func() {
		defer close(run.done)
		err := run.app.Wait()
		if exit, ok := err.(*exec.ExitError); ok {
			run.code = exit.ExitCode()
		} else if err != nil {
			run.code = -1
		}
	}()

	h.t.Cleanup(func() { run.stop(h.t) })
	return run
}

// output returns everything the application printed, on both streams.
func (r *fixtureRun) output() string { return r.stdout.String() + r.stderr.String() }

// stop ends the application and waits for the CLI to report its exit. A fixture
// that outlives its test is killed so it cannot disturb the next one.
func (r *fixtureRun) stop(t *testing.T) {
	t.Helper()

	select {
	case <-r.done:
		return
	case <-time.After(10 * time.Second):
	}
	if r.app.Process != nil {
		_ = r.app.Process.Kill()
	}
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
		t.Logf("the fixture application did not stop; its output was:\n%s", r.output())
	}
}

// waitForLine waits until the application printed a line containing text.
func (r *fixtureRun) waitForLine(t *testing.T, text string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if contains(r.output(), text) {
			return
		}
		select {
		case <-r.done:
			t.Fatalf("the application exited before printing %q; it printed:\n%s", text, r.output())
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatalf("the application never printed %q; it printed:\n%s", text, r.output())
}

func contains(haystack, needle string) bool {
	return bytes.Contains([]byte(haystack), []byte(needle))
}

// startSession creates and starts a session, which every gate here needs before
// it can drive an application. The session is closed again when the test ends,
// so a gate leaves no container behind.
func (h *harness) startSession() {
	h.t.Helper()

	h.succeed("session", "create", "--session", h.session, "--wait")
	h.t.Cleanup(func() { _, _, _ = h.run("session", "close", "--session", h.session) })

	envelope := h.succeed("session", "inspect", "--session", h.session)
	var inspected struct {
		Session struct {
			State string `json:"state"`
		} `json:"session"`
	}
	if err := json.Unmarshal(envelope.Result, &inspected); err != nil {
		h.t.Fatalf("the session record is unreadable: %v", err)
	}
	if inspected.Session.State != "ready" {
		h.t.Fatalf("the session is %s, want ready", inspected.Session.State)
	}
}

// sessionWindows reports the windows the session currently has.
func (h *harness) sessionWindows() agentapi.State {
	h.t.Helper()

	envelope := h.succeed("windows", "--session", h.session)
	var state agentapi.State
	if err := json.Unmarshal(envelope.Result, &state); err != nil {
		h.t.Fatalf("the window state is unreadable: %v", err)
	}
	return state
}

// waitForTitledWindow waits until a window with the given title exists.
func (h *harness) waitForTitledWindow(title string) agentapi.Toplevel {
	h.t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	var seen agentapi.State
	for time.Now().Before(deadline) {
		seen = h.sessionWindows()
		for _, toplevel := range seen.Toplevels {
			if toplevel.Title == title && toplevel.Width > 0 && toplevel.Height > 0 {
				return toplevel
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	h.t.Fatalf("no window titled %q appeared; the session reported %+v", title, seen)
	return agentapi.Toplevel{}
}

// waitForTitledWindowSize waits until a window has an exact size, which is how a gate
// proves a resize took effect in the compositor and not only in the reply.
func (h *harness) waitForTitledWindowSize(title string, width, height uint32) agentapi.Toplevel {
	h.t.Helper()

	deadline := time.Now().Add(20 * time.Second)
	var last agentapi.Toplevel
	for time.Now().Before(deadline) {
		for _, toplevel := range h.sessionWindows().Toplevels {
			if toplevel.Title != title {
				continue
			}
			last = toplevel
			if toplevel.Width == width && toplevel.Height == height {
				return toplevel
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	h.t.Fatalf("window %q is %dx%d, want %dx%d", title, last.Width, last.Height, width, height)
	return agentapi.Toplevel{}
}

// exportPath is where the CLI stores an artifact of this session on the host.
func (h *harness) exportPath(name string) string {
	return filepath.Join(h.stateRoot, "export", h.session, name)
}

// resizeOutcome is the resize reply, with each size the design distinguishes.
type resizeOutcome struct {
	Handle           string `json:"handle"`
	RequestedWidth   uint32 `json:"requested_width"`
	RequestedHeight  uint32 `json:"requested_height"`
	ConfiguredWidth  uint32 `json:"configured_width"`
	ConfiguredHeight uint32 `json:"configured_height"`
	CommittedWidth   uint32 `json:"committed_width"`
	CommittedHeight  uint32 `json:"committed_height"`
	VisibleWidth     uint32 `json:"visible_width"`
	VisibleHeight    uint32 `json:"visible_height"`
	Revision         uint64 `json:"revision"`
}

func (r resizeOutcome) requireAll(t *testing.T, width, height uint32) {
	t.Helper()

	for _, size := range []struct {
		what   string
		width  uint32
		height uint32
	}{
		{"requested", r.RequestedWidth, r.RequestedHeight},
		{"configured", r.ConfiguredWidth, r.ConfiguredHeight},
		{"committed", r.CommittedWidth, r.CommittedHeight},
		{"visible", r.VisibleWidth, r.VisibleHeight},
	} {
		if size.width != width || size.height != height {
			t.Errorf("the %s size is %dx%d, want %dx%d", size.what, size.width, size.height, width, height)
		}
	}
}

// TestResizeCommit is the Phase 5 resize gate: a successful resize means the
// application committed the size it was configured with, and nothing else is
// reported as success.
func TestResizeCommit(t *testing.T) {
	h := newHarness(t)
	h.startSession()

	t.Run("a cooperative application commits the configured size", func(t *testing.T) {
		h.startFixtureApplication("--title", "resize-cooperative", "--resize", "cooperative", "--exit-after", "10000")
		window := h.waitForTitledWindow("resize-cooperative")

		envelope := h.succeed("resize", "--session", h.session, "--window", window.Handle,
			"--width", "640", "--height", "480", "--timeout", "10s")

		var resized resizeOutcome
		if err := json.Unmarshal(envelope.Result, &resized); err != nil {
			t.Fatalf("the resize result is unreadable: %v", err)
		}
		resized.requireAll(t, 640, 480)
		h.waitForTitledWindowSize("resize-cooperative", 640, 480)
	})

	t.Run("a delayed commit is waited for instead of reported early", func(t *testing.T) {
		h.startFixtureApplication("--title", "resize-delayed", "--resize", "delayed", "--resize-delay", "600", "--exit-after", "10000")
		window := h.waitForTitledWindow("resize-delayed")

		start := time.Now()
		envelope := h.succeed("resize", "--session", h.session, "--window", window.Handle,
			"--width", "700", "--height", "500", "--timeout", "10s")
		elapsed := time.Since(start)

		var resized resizeOutcome
		if err := json.Unmarshal(envelope.Result, &resized); err != nil {
			t.Fatalf("the resize result is unreadable: %v", err)
		}
		resized.requireAll(t, 700, 500)
		if elapsed < 500*time.Millisecond {
			t.Errorf("the resize returned after %s, before the application committed; a configure was reported as success", elapsed)
		}
	})

	t.Run("an application that ignores the configure times out", func(t *testing.T) {
		h.startFixtureApplication("--title", "resize-ignored", "--resize", "ignore", "--exit-after", "10000")
		window := h.waitForTitledWindow("resize-ignored")

		stdout, stderr, code := h.run("resize", "--session", h.session, "--window", window.Handle,
			"--width", "720", "--height", "540", "--timeout", "2s")
		if code == 0 {
			t.Fatalf("a resize the application ignored was reported as success: %s", stdout)
		}
		if code != 5 {
			t.Errorf("the resize exited %d, want 5 (timeout); stdout: %s stderr: %s", code, stdout, stderr)
		}
		if !contains(stdout+stderr, "wait_timeout") {
			t.Errorf("the failure does not name wait_timeout: %s %s", stdout, stderr)
		}
	})

	t.Run("an application that disappears during a resize is not a success", func(t *testing.T) {
		h.startFixtureApplication("--title", "resize-abandoned", "--resize", "exit", "--exit-after", "10000")
		window := h.waitForTitledWindow("resize-abandoned")

		stdout, stderr, code := h.run("resize", "--session", h.session, "--window", window.Handle,
			"--width", "800", "--height", "600", "--timeout", "5s")
		if code == 0 {
			t.Fatalf("a resize of a window that went away was reported as success: %s", stdout)
		}
		if code != 4 && code != 5 {
			t.Errorf("the resize exited %d, want 4 or 5; stdout: %s stderr: %s", code, stdout, stderr)
		}
	})
}

// TestVisionInput is the Phase 5 input gate: the application receives the keys
// and the pointer the CLI injected, decoded through the keymap the compositor
// sent it.
func TestVisionInput(t *testing.T) {
	h := newHarness(t)
	h.startSession()

	app := h.startFixtureApplication("--title", "input-target", "--resize", "none",
		"--report-input", "--exit-after", "20000")
	window := h.waitForTitledWindow("input-target")
	app.waitForLine(t, "wlvision-fixture: connected", 20*time.Second)

	if envelope := h.succeed("activate", "--session", h.session, "--window", window.Handle); envelope.Revision == 0 {
		t.Error("activation reported revision 0")
	}

	h.succeed("type", "--session", h.session, "hi")
	app.waitForLine(t, "state=pressed utf8=h", 10*time.Second)
	app.waitForLine(t, "state=pressed utf8=i", 10*time.Second)

	h.succeed("key", "--session", h.session, "--name", "Return")
	app.waitForLine(t, "keycode=28 state=pressed", 10*time.Second)

	h.succeed("click", "--session", h.session, "--window", window.Handle, "--x", "20", "--y", "30")
	app.waitForLine(t, "pointer button button=1 state=pressed", 10*time.Second)
	app.waitForLine(t, "pointer button button=1 state=released", 10*time.Second)

	// A coordinate that is not inside the window is refused before anything is
	// injected, which is what keeps a click off a different window.
	stdout, stderr, code := h.run("click", "--session", h.session, "--window", window.Handle, "--x", "100000", "--y", "0")
	if code != 2 {
		t.Errorf("an out-of-bounds click exited %d, want 2; stdout: %s stderr: %s", code, stdout, stderr)
	}

	// A stale revision is refused instead of being replayed on a new layout.
	stdout, stderr, code = h.run("click", "--session", h.session, "--window", window.Handle,
		"--revision", "1", "--x", "1", "--y", "1")
	if code != 4 {
		t.Errorf("a click naming a stale revision exited %d, want 4; stdout: %s stderr: %s", code, stdout, stderr)
	}
	if !contains(stdout+stderr, "stale_revision") {
		t.Errorf("the refusal does not name stale_revision: %s %s", stdout, stderr)
	}
}

// burstReport mirrors the capture command's result.
type burstReport struct {
	Manifest     string `json:"manifest"`
	ContactSheet string `json:"contact_sheet"`
	Interval     string `json:"interval"`
	Duration     string `json:"duration"`
	Bytes        int64  `json:"bytes"`
	Captured     int    `json:"captured"`
	Deduplicated int    `json:"deduplicated"`
	Missed       int    `json:"missed"`
	Failed       int    `json:"failed"`
	Frames       []struct {
		Scheduled string `json:"scheduled"`
		Requested string `json:"requested"`
		Completed string `json:"completed"`
		FrameSeq  uint64 `json:"frame_sequence"`
		Digest    string `json:"digest"`
		Status    string `json:"status"`
		Path      string `json:"path"`
		Error     string `json:"error"`
	} `json:"frames"`
}

// TestAnimationBurst samples a deterministic animation and proves the burst
// kept its cadence: several distinct frames, ordered sequences and a manifest
// that names every scheduled sample.
func TestAnimationBurst(t *testing.T) {
	h := newHarness(t)
	h.startSession()

	h.startFixtureApplication("--title", "burst-animation", "--animate", "--frames", "200",
		"--period", "20", "--resize", "none", "--exit-after", "20000")
	h.waitForTitledWindow("burst-animation")

	envelope := h.succeed("capture", "--session", h.session, "--interval", "100ms",
		"--duration", "2s", "--frames", "25", "--output", "frames", "--contact-sheet")

	var report burstReport
	if err := json.Unmarshal(envelope.Result, &report); err != nil {
		t.Fatalf("the burst result is unreadable: %v", err)
	}
	if len(report.Frames) == 0 {
		t.Fatal("the burst scheduled no samples")
	}
	if report.Manifest == "" {
		t.Fatal("the burst published no manifest")
	}
	if _, err := os.Stat(report.Manifest); err != nil {
		t.Fatalf("the manifest is not on the host: %v", err)
	}

	digests := make(map[string]bool)
	stored := 0
	var lastSequence uint64
	var lastScheduled time.Duration
	for index, frame := range report.Frames {
		digests[frame.Digest] = true
		if frame.Status == "captured" || frame.Status == "deduplicated" {
			stored++
		}
		scheduled, err := time.ParseDuration(frame.Scheduled)
		if err != nil {
			t.Fatalf("sample %d has an unreadable schedule %q: %v", index, frame.Scheduled, err)
		}
		if scheduled < lastScheduled {
			t.Errorf("sample %d is scheduled at %s, after %s", index, scheduled, lastScheduled)
		}
		lastScheduled = scheduled
		if frame.FrameSeq != 0 {
			if frame.FrameSeq < lastSequence {
				t.Errorf("sample %d has frame sequence %d, before %d", index, frame.FrameSeq, lastSequence)
			}
			lastSequence = frame.FrameSeq
		}
		if frame.Path != "" {
			if _, err := os.Stat(frame.Path); err != nil {
				t.Errorf("sample %d names a stored frame that is not there: %v", index, err)
			}
		}
	}
	if stored == 0 {
		t.Fatal("the burst stored no frame")
	}
	if len(digests) < 2 {
		t.Errorf("the burst saw %d distinct frames while the application was animating", len(digests))
	}
	if report.Captured+report.Deduplicated+report.Missed+report.Failed != len(report.Frames) {
		t.Errorf("the burst summary %d/%d/%d/%d does not account for %d samples",
			report.Captured, report.Deduplicated, report.Missed, report.Failed, len(report.Frames))
	}
	if report.Duration != "2s" || report.Interval != "100ms" {
		t.Errorf("the burst ran at %s for %s, want 100ms for 2s", report.Interval, report.Duration)
	}
	if report.ContactSheet == "" {
		t.Error("no contact sheet was stored")
	} else if _, err := os.Stat(report.ContactSheet); err != nil {
		t.Errorf("the contact sheet is not on the host: %v", err)
	}

	// The manifest the session published must agree with the reply.
	payload, err := os.ReadFile(report.Manifest)
	if err != nil {
		t.Fatalf("read the manifest: %v", err)
	}
	var manifest struct {
		Schema string            `json:"schema"`
		Frames []json.RawMessage `json:"frames"`
	}
	if err := json.Unmarshal(payload, &manifest); err != nil {
		t.Fatalf("the manifest is not readable: %v", err)
	}
	if manifest.Schema == "" {
		t.Error("the manifest carries no schema")
	}
	if len(manifest.Frames) != len(report.Frames) {
		t.Errorf("the manifest holds %d samples, the reply reported %d", len(manifest.Frames), len(report.Frames))
	}
}

// TestQuietStability is the Phase 5 timing gate: a picture that stops changing
// is proven stable by active probes, not by waiting, and a resize between
// captures is visible in the captured revision.
func TestQuietStability(t *testing.T) {
	h := newHarness(t)
	h.startSession()

	h.startFixtureApplication("--title", "stable-target", "--frames", "1", "--resize", "none", "--exit-after", "20000")
	window := h.waitForTitledWindow("stable-target")

	envelope := h.succeed("wait", "--session", h.session, "--stable-for", "300ms", "--timeout", "10s")
	var stable struct {
		Kind         string `json:"kind"`
		Probes       int    `json:"probes"`
		Observations int    `json:"observations"`
		Revision     uint64 `json:"revision"`
	}
	if err := json.Unmarshal(envelope.Result, &stable); err != nil {
		t.Fatalf("the wait result is unreadable: %v", err)
	}
	if stable.Observations < 2 || stable.Probes < 1 {
		t.Errorf("stability was proven with %d observations and %d probes, want at least two observations", stable.Observations, stable.Probes)
	}

	first := h.captureFrame("before-resize")
	h.succeed("resize", "--session", h.session, "--window", window.Handle, "--width", "640", "--height", "480", "--timeout", "10s")
	h.waitForTitledWindowSize("stable-target", 640, 480)
	second := h.captureFrame("after-resize")

	if second.Revision <= first.Revision {
		t.Errorf("the revision after the resize is %d, before it was %d", second.Revision, first.Revision)
	}
	if first.Width == second.Width && first.Height == second.Height {
		t.Errorf("both captures are %dx%d, so the resize is not visible in the capture", first.Width, first.Height)
	}

	// A wait that cannot succeed must time out, report wait_timeout, and leave
	// the session usable.
	stdout, stderr, code := h.run("wait", "--session", h.session, "--window-count", "7", "--timeout", "1s")
	if code != 5 {
		t.Errorf("an unsatisfiable wait exited %d, want 5; stdout: %s stderr: %s", code, stdout, stderr)
	}
	if !contains(stdout+stderr, "wait_timeout") {
		t.Errorf("the failure does not name wait_timeout: %s %s", stdout, stderr)
	}
	if state := h.sessionWindows(); len(state.Toplevels) == 0 {
		t.Error("the session has no windows after a timed-out wait")
	}
}

// frameOutcome mirrors one screenshot result.
type frameOutcome struct {
	Path     string `json:"path"`
	Digest   string `json:"digest"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	Format   string `json:"format"`
	Revision uint64 `json:"revision"`
}

// captureFrame takes one screenshot and returns what it stored.
func (h *harness) captureFrame(name string) frameOutcome {
	h.t.Helper()

	envelope := h.succeed("screenshot", "--session", h.session, "--output", name+".png")
	var frame frameOutcome
	if err := json.Unmarshal(envelope.Result, &frame); err != nil {
		h.t.Fatalf("the screenshot result is unreadable: %v", err)
	}
	if frame.Digest == "" || frame.Width == 0 || frame.Height == 0 {
		h.t.Fatalf("the screenshot reported %+v", frame)
	}
	if _, err := os.Stat(frame.Path); err != nil {
		h.t.Fatalf("the screenshot is not on the host: %v", err)
	}
	if want := h.exportPath(name + ".png"); frame.Path != want {
		h.t.Errorf("the screenshot path is %q, want %q", frame.Path, want)
	}
	return frame
}
