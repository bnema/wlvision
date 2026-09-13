package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/bnema/wlvision/internal/result"
	"github.com/bnema/wlvision/internal/session"
)

// fakeProcess is one supervised child the test controls.
type fakeProcess struct {
	pid     int
	reaper  *fakeReaper
	mu      sync.Mutex
	signals []os.Signal
	killed  bool
}

func newFakeProcess(pid int, reaper *fakeReaper) *fakeProcess {
	return &fakeProcess{pid: pid, reaper: reaper}
}

func (p *fakeProcess) Pid() int { return p.pid }

func (p *fakeProcess) Signal(signal os.Signal) error {
	p.mu.Lock()
	p.signals = append(p.signals, signal)
	p.mu.Unlock()

	// A real child ends when it is asked to stop, so the exit it owes the
	// session's single reaper arrives here.
	if p.reaper != nil {
		p.reaper.exit(p.pid, syscall.WaitStatus(0))
	}
	return nil
}

func (p *fakeProcess) Kill() error {
	p.mu.Lock()
	p.killed = true
	p.mu.Unlock()

	if p.reaper != nil {
		p.reaper.exit(p.pid, syscall.WaitStatus(syscall.SIGKILL))
	}
	return nil
}

func (p *fakeProcess) observedSignals() []os.Signal {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]os.Signal(nil), p.signals...)
}

func (p *fakeProcess) wasKilled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.killed
}

// reapedProcess is one scripted return value of the fake reaper.
type reapedProcess struct {
	pid    int
	status syscall.WaitStatus
	err    error
}

// fakeReaper stands in for wait4(-1, ...): it hands the supervisor the exits
// the test scripts, including exits of processes the supervisor never started.
type fakeReaper struct {
	results chan reapedProcess
	mu      sync.Mutex
	calls   int
}

func newFakeReaper() *fakeReaper {
	return &fakeReaper{results: make(chan reapedProcess, 64)}
}

func (r *fakeReaper) Reap() (int, syscall.WaitStatus, error) {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()

	result := <-r.results
	return result.pid, result.status, result.err
}

// exit scripts one child exit.
func (r *fakeReaper) exit(pid int, status syscall.WaitStatus) {
	r.results <- reapedProcess{pid: pid, status: status}
}

// noChildren scripts the moment every child is gone.
func (r *fakeReaper) noChildren() {
	r.results <- reapedProcess{pid: -1, err: syscall.ECHILD}
}

func (r *fakeReaper) reapCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// fakeSpawner hands out fake children and records what was started.
type fakeSpawner struct {
	mu        sync.Mutex
	reaper    *fakeReaper
	argv      [][]string
	env       [][]string
	processes []*fakeProcess
	onStart   func(process *fakeProcess, argv []string, env []string)
}

func (s *fakeSpawner) Start(argv []string, env []string, _ io.Writer) (Process, error) {
	s.mu.Lock()
	// Live identifiers: a status probe only believes a running child while its
	// process exists, and the two children must be told apart by pid.
	pid := os.Getpid()
	if len(s.processes) > 0 {
		pid = os.Getppid()
	}
	process := newFakeProcess(pid, s.reaper)
	s.argv = append(s.argv, append([]string(nil), argv...))
	s.env = append(s.env, append([]string(nil), env...))
	s.processes = append(s.processes, process)
	s.mu.Unlock()

	if s.onStart != nil {
		s.onStart(process, argv, env)
	}
	return process, nil
}

func (s *fakeSpawner) started() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]string(nil), s.argv...)
}

func (s *fakeSpawner) environments() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]string(nil), s.env...)
}

func (s *fakeSpawner) child(index int) *fakeProcess {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.processes[index]
}

// syncBuffer collects a session log while Run is still writing to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func testOptions(t *testing.T) (Options, string) {
	t.Helper()

	root := t.TempDir()
	options := Options{
		RuntimeDir:   filepath.Join(root, "run"),
		ControlDir:   filepath.Join(root, "run", "control"),
		WaylandDir:   filepath.Join(root, "run", "wayland"),
		Display:      session.WaylandDisplay,
		AgentPath:    "/usr/libexec/wlvision-agent",
		Width:        640,
		Height:       480,
		ReadyTimeout: 2 * time.Second,
		StopTimeout:  200 * time.Millisecond,
	}
	return options, root
}

func TestReadStatusDistinguishesNeverStartedFromExited(t *testing.T) {
	options, _ := testOptions(t)

	status := ReadStatus(options)
	if status.Weston != session.ProcessNotStarted || status.Agent != session.ProcessNotStarted {
		t.Errorf("status = %+v, want both processes not started", status)
	}
	if status.Ready {
		t.Error("a session that never started reported readiness")
	}
}

func TestReadStatusTrustsLivenessOverTheLastObservation(t *testing.T) {
	options, _ := testOptions(t)
	if err := os.MkdirAll(options.ControlDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	// A supervisor that died leaves its last optimistic observation behind.
	recordStatus(t, options, session.ContainerStatus{Ready: true, Weston: session.ProcessRunning, Agent: session.ProcessRunning})
	writePIDFile(t, options, westonPIDFile, deadPID(t))
	writePIDFile(t, options, agentPIDFile, deadPID(t))

	status := ReadStatus(options)
	if status.Weston != session.ProcessExited || status.Agent != session.ProcessExited {
		t.Errorf("status = %+v, want both processes reported as exited", status)
	}
	if status.Ready {
		t.Error("a session whose processes are gone reported readiness")
	}
}

func TestReadStatusRequiresTheControllersMarker(t *testing.T) {
	options, _ := testOptions(t)
	if err := os.MkdirAll(options.ControlDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	recordStatus(t, options, session.ContainerStatus{Ready: true, Weston: session.ProcessRunning, Agent: session.ProcessRunning})
	writePIDFile(t, options, westonPIDFile, os.Getpid())
	writePIDFile(t, options, agentPIDFile, os.Getpid())

	// Readiness is the controller's own report, not the supervisor's opinion:
	// without the marker the session is not ready even though both processes
	// are alive.
	if status := ReadStatus(options); status.Ready {
		t.Errorf("status = %+v, want readiness to wait for the controller", status)
	}

	if err := os.WriteFile(filepath.Join(options.ControlDir, readyMarkerName), []byte("1"), 0o600); err != nil {
		t.Fatalf("write the marker: %v", err)
	}
	if status := ReadStatus(options); !status.Ready {
		t.Errorf("status = %+v, want a ready session", status)
	}
}

func TestSupervisorStartsTheSessionAndStopsItCleanly(t *testing.T) {
	reaper := newFakeReaper()
	spawn := &fakeSpawner{reaper: reaper}
	options, _ := testOptions(t)

	socket := filepath.Join(options.WaylandDir, options.Display)
	spawn.onStart = func(_ *fakeProcess, argv []string, _ []string) {
		switch argv[0] {
		case westonBinary:
			if err := os.WriteFile(socket, nil, 0o600); err != nil {
				t.Errorf("publish the socket: %v", err)
			}
		default:
			// The controller announces readiness once it has bound the
			// control plane; the fake agent writes the marker instead.
			if err := os.WriteFile(filepath.Join(options.ControlDir, readyMarkerName), []byte("1"), 0o600); err != nil {
				t.Errorf("write the readiness marker: %v", err)
			}
		}
	}

	supervisor := newSupervisor(options, spawn, reaper, &bytes.Buffer{}, time.Now)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- supervisor.Run(ctx) }()

	waitFor(t, func() bool { return ReadStatus(options).Ready })

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the supervisor did not stop")
	}

	started := spawn.started()
	if len(started) != 2 {
		t.Fatalf("started %d processes, want the compositor and the controller", len(started))
	}
	compositor := strings.Join(started[0], " ")
	for _, want := range []string{"--backend=headless", "--renderer=pixman", "--shell=wlvision-shell", "--fake-seat", "--socket=" + options.Display, "--width=640", "--height=480"} {
		if !strings.Contains(compositor, want) {
			t.Errorf("compositor command %q is missing %q", compositor, want)
		}
	}
	if len(started[1]) != 1 || started[1][0] != options.AgentPath {
		t.Errorf("controller command = %v, want %s", started[1], options.AgentPath)
	}
	if env := strings.Join(spawn.environments()[1], " "); !strings.Contains(env, session.EnvWaylandDisplay+"="+options.Display) {
		t.Error("the controller was not told which display to use")
	}

	// Both children are asked to stop politely, and neither has to be killed.
	for index, child := range []*fakeProcess{spawn.child(0), spawn.child(1)} {
		signals := child.observedSignals()
		if len(signals) != 1 || signals[0] != syscall.SIGTERM {
			t.Errorf("child %d signals = %v, want one SIGTERM", index, signals)
		}
		if child.wasKilled() {
			t.Errorf("child %d was killed although it exited on SIGTERM", index)
		}
	}

	// The last thing the session publishes is its final state.
	if status := ReadStatus(options); status.Weston != session.ProcessExited || status.Agent != session.ProcessExited {
		t.Errorf("status = %+v, want both children reported as exited", status)
	}
}

func TestSupervisorFailsWhenTheCompositorNeverPublishesASocket(t *testing.T) {
	reaper := newFakeReaper()
	spawn := &fakeSpawner{reaper: reaper}
	spawn.onStart = func(process *fakeProcess, argv []string, _ []string) {
		if argv[0] == westonBinary {
			// The compositor dies immediately, as a broken image would.
			reaper.exit(process.pid, syscall.WaitStatus(2<<8))
		}
	}
	options, _ := testOptions(t)
	supervisor := newSupervisor(options, spawn, reaper, &bytes.Buffer{}, time.Now)

	err := supervisor.Run(context.Background())
	if err == nil {
		t.Fatal("a session whose compositor died was reported as started")
	}
	if !strings.Contains(err.Error(), "compositor") {
		t.Errorf("error = %v, want it to name the compositor", err)
	}
	if started := spawn.started(); len(started) != 1 {
		t.Errorf("started %d processes, want only the compositor", len(started))
	}

	status := ReadStatus(options)
	if status.Weston != session.ProcessExited {
		t.Errorf("status = %+v, want the compositor reported as exited", status)
	}
	if status.Ready {
		t.Error("a session whose compositor died reported readiness")
	}
}

func TestSupervisorFailsWhenTheControllerExits(t *testing.T) {
	reaper := newFakeReaper()
	spawn := &fakeSpawner{reaper: reaper}
	options, _ := testOptions(t)
	socket := filepath.Join(options.WaylandDir, options.Display)

	spawn.onStart = func(process *fakeProcess, argv []string, _ []string) {
		switch argv[0] {
		case westonBinary:
			if err := os.WriteFile(socket, nil, 0o600); err != nil {
				t.Errorf("publish the socket: %v", err)
			}
		default:
			// The controller exits on its own, right after it started.
			reaper.exit(process.pid, syscall.WaitStatus(1<<8))
		}
	}

	supervisor := newSupervisor(options, spawn, reaper, &bytes.Buffer{}, time.Now)
	err := supervisor.Run(context.Background())
	if err == nil {
		t.Fatal("a session whose controller died was reported as running")
	}
	if !strings.Contains(err.Error(), "controller") {
		t.Errorf("error = %v, want it to name the controller", err)
	}
	if signals := spawn.child(0).observedSignals(); len(signals) != 1 {
		t.Errorf("the compositor was not stopped: %v", signals)
	}
	if status := ReadStatus(options); status.Agent != session.ProcessExited {
		t.Errorf("status = %+v, want the controller reported as exited", status)
	}
}

func TestSupervisorCollectsAnOrphanWithoutEndingTheSession(t *testing.T) {
	reaper := newFakeReaper()
	spawn := &fakeSpawner{reaper: reaper}
	options, _ := testOptions(t)
	var log syncBuffer

	socket := filepath.Join(options.WaylandDir, options.Display)
	spawn.onStart = func(_ *fakeProcess, argv []string, _ []string) {
		switch argv[0] {
		case westonBinary:
			if err := os.WriteFile(socket, nil, 0o600); err != nil {
				t.Errorf("publish the socket: %v", err)
			}
		default:
			if err := os.WriteFile(filepath.Join(options.ControlDir, readyMarkerName), []byte("1"), 0o600); err != nil {
				t.Errorf("write the readiness marker: %v", err)
			}
		}
	}

	supervisor := newSupervisor(options, spawn, reaper, &log, time.Now)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- supervisor.Run(ctx) }()

	waitFor(t, func() bool { return ReadStatus(options).Ready })

	// This pid belongs to a process the supervisor never started: it was
	// reparented to the supervisor because the supervisor is PID 1. Reaping it
	// must be logged and must not end the session.
	orphan := os.Getpid() + 7919
	for _, child := range []*fakeProcess{spawn.child(0), spawn.child(1)} {
		for orphan == child.Pid() {
			orphan++
		}
	}
	reaper.exit(orphan, syscall.WaitStatus(3<<8))

	waitFor(t, func() bool { return strings.Contains(log.String(), "orphan") })
	if logged := log.String(); !strings.Contains(logged, strconv.Itoa(orphan)) || !strings.Contains(logged, "exit status 3") {
		t.Errorf("the orphan log %q does not name its pid and exit status", logged)
	}

	select {
	case err := <-done:
		t.Fatalf("Run returned after reaping an orphan: %v", err)
	default:
	}

	// The compositor is still supervised while the orphan was only collected.
	status := ReadStatus(options)
	if status.Weston != session.ProcessRunning || status.Agent != session.ProcessRunning {
		t.Errorf("status = %+v, want both children still running", status)
	}
	if !status.Ready {
		t.Errorf("status = %+v, want the session still ready", status)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run after collecting an orphan: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the supervisor did not stop after an orphan was reaped")
	}
}

func TestReapLoopDeliversEachExitOnlyOnce(t *testing.T) {
	reaper := newFakeReaper()
	options, _ := testOptions(t)
	supervisor := newSupervisor(options, &fakeSpawner{}, reaper, &bytes.Buffer{}, time.Now)

	reaper.exit(101, syscall.WaitStatus(0))
	// A later Reap returning the same pid is not a second exit and must not
	// panic.
	reaper.exit(101, syscall.WaitStatus(0))
	reaper.noChildren()

	events := make(chan exitEvent, exitEventBuffer)
	reaped := make(chan struct{})
	go supervisor.reapLoop(events, reaped)

	select {
	case <-reaped:
	case <-time.After(2 * time.Second):
		t.Fatal("the reaper loop did not stop after Reap reported ECHILD")
	}

	if got := len(events); got != 1 {
		t.Fatalf("the reaper delivered %d exits, want 1", got)
	}
	if event := <-events; event.pid != 101 {
		t.Errorf("delivered pid = %d, want 101", event.pid)
	}
}

func TestReapLoopStopsWhenNoChildrenRemain(t *testing.T) {
	reaper := newFakeReaper()
	options, _ := testOptions(t)
	supervisor := newSupervisor(options, &fakeSpawner{}, reaper, &bytes.Buffer{}, time.Now)

	reaper.exit(101, syscall.WaitStatus(0))
	reaper.exit(202, syscall.WaitStatus(0))
	reaper.noChildren()

	events := make(chan exitEvent, exitEventBuffer)
	reaped := make(chan struct{})
	go supervisor.reapLoop(events, reaped)

	select {
	case <-reaped:
	case <-time.After(2 * time.Second):
		t.Fatal("the reaper loop did not stop after Reap reported ECHILD")
	}
	if got := reaper.reapCalls(); got != 3 {
		t.Errorf("Reap calls = %d, want two exits and one ECHILD", got)
	}
	if got := len(events); got != 2 {
		t.Errorf("the reaper delivered %d exits, want 2", got)
	}
}

func TestReceiveStoresAPayloadAndReportsIt(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer

	code := receiveCommand(
		[]string{"--kind", "binary", "--name", "app", "--mode", "0755", "--dir", dir, "--max-bytes", "1024"},
		strings.NewReader("abc"), &stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("receive exit code = %d, want 0 (stderr: %s)", code, stderr.String())
	}

	var reported session.PayloadResult
	if err := json.Unmarshal(stdout.Bytes(), &reported); err != nil {
		t.Fatalf("the receiver did not report JSON: %v (%q)", err, stdout.String())
	}
	if reported.Digest != "sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Errorf("digest = %q, want the digest of the received bytes", reported.Digest)
	}
	if reported.Bytes != 3 || reported.Files != 1 {
		t.Errorf("reported = %+v, want three bytes in one file", reported)
	}

	stored, err := os.ReadFile(filepath.Join(dir, "app"))
	if err != nil {
		t.Fatalf("read the stored payload: %v", err)
	}
	if string(stored) != "abc" {
		t.Errorf("stored %q, want the received bytes", stored)
	}
}

func TestReceiveRejectsAHostileBundle(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer

	code := receiveCommand(
		[]string{"--kind", "tar", "--name", "bundle", "--dir", dir},
		bytes.NewReader(tarWithEntry(t, "../escape", "owned")), &stdout, &stderr,
	)
	if code != result.CodePayloadRejected.ExitCode() {
		t.Fatalf("receive exit code = %d, want %d", code, result.CodePayloadRejected.ExitCode())
	}

	var envelope result.Envelope[any]
	if err := json.Unmarshal(stderr.Bytes(), &envelope); err != nil {
		t.Fatalf("the receiver did not report a failure document: %v (%q)", err, stderr.String())
	}
	if envelope.Ok || envelope.Error == nil || envelope.Error.Code != result.CodePayloadRejected {
		t.Errorf("failure = %+v, want a payload_rejected document", envelope.Error)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("the payload directory is not empty: %v (%v)", entries, err)
	}
}

func TestReceiveRequiresAName(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := receiveCommand([]string{"--kind", "binary", "--dir", t.TempDir()}, strings.NewReader("abc"), &stdout, &stderr)
	if code != result.CodeUsageError.ExitCode() {
		t.Fatalf("receive exit code = %d, want %d", code, result.CodeUsageError.ExitCode())
	}
	if !strings.Contains(stderr.String(), string(result.CodeUsageError)) {
		t.Errorf("stderr = %q, want the usage code", stderr.String())
	}
}

func TestLineWriterStampsEveryLine(t *testing.T) {
	var out bytes.Buffer
	writer := &lineWriter{w: &out, now: func() time.Time { return time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC) }}

	if _, err := writer.Write([]byte("first\nsec")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := out.String(); got != "2026-01-02T03:04:05Z first\n" {
		t.Errorf("output = %q, want one stamped complete line", got)
	}

	writer.Write([]byte("ond\n"))
	if got := strings.Count(out.String(), "\n"); got != 2 {
		t.Errorf("output has %d lines, want 2 (%q)", got, out.String())
	}

	writer.Write([]byte("partial"))
	writer.Flush()
	if !strings.Contains(out.String(), "partial") {
		t.Errorf("output = %q, want the flushed partial line", out.String())
	}
}

// deadPID returns a process identifier that is certainly not running: the
// identifier of a process the test started and reaped.
func deadPID(t *testing.T) int {
	t.Helper()

	command := exec.Command("/bin/true")
	if err := command.Run(); err != nil {
		t.Fatalf("run /bin/true: %v", err)
	}
	return command.Process.Pid
}

func writePIDFile(t *testing.T, options Options, name string, pid int) {
	t.Helper()

	path := filepath.Join(options.ControlDir, name)
	if err := os.WriteFile(path, []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func recordStatus(t *testing.T, options Options, status session.ContainerStatus) {
	t.Helper()

	payload, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("encode the status: %v", err)
	}
	if err := os.WriteFile(filepath.Join(options.ControlDir, filepath.Base(session.StatusFile)), payload, 0o600); err != nil {
		t.Fatalf("write the status: %v", err)
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("the condition was never met")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// tarWithEntry builds a one-entry archive, which is how the hostile payload
// tests reach the extractor.
func tarWithEntry(t *testing.T, name, content string) []byte {
	t.Helper()

	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatalf("write the header: %v", err)
	}
	if _, err := writer.Write([]byte(content)); err != nil {
		t.Fatalf("write the entry: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close the archive: %v", err)
	}
	return buffer.Bytes()
}
