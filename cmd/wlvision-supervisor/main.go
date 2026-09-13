// Command wlvision-supervisor is PID 1 of a wlvision session container.
//
// It owns the session's own processes: it prepares the runtime directories,
// starts the compositor with the wlvision shell, starts the resident
// controller, waits for the controller to report that the control plane is up,
// reaps everything the container leaves behind, and shuts both down on SIGTERM
// or when one of them dies.
//
// The same binary serves two short-lived modes that the outer CLI runs through
// `docker exec`:
//
//	wlvision-supervisor status    report the session's processes as JSON
//	wlvision-supervisor receive   stream one payload into the session
//
// Readiness is not a guess: the resident controller writes a marker once it has
// bound the control global, the capture protocol, and the output, and the
// status mode combines that marker with the liveness of both children.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/bnema/wlvision/internal/inject"
	"github.com/bnema/wlvision/internal/result"
	"github.com/bnema/wlvision/internal/session"
)

// Defaults for a session's geometry and shutdown window.
const (
	DefaultWidth       = 1280
	DefaultHeight      = 800
	DefaultStopTimeout = 5 * time.Second
	DefaultSocketWait  = 20 * time.Second
	readyPollInterval  = 100 * time.Millisecond
	westonBinary       = "weston"
	readyMarkerName    = "agent.ready"
	westonPIDFile      = "weston.pid"
	agentPIDFile       = "agent.pid"
	displayGIDEnv      = "WLVISION_DISPLAY_GID"
)

// Process is one supervised child. Its exit is collected by the session's
// single reaper, never by the process itself: wait4(-1, ...) must not race
// with a second waiter.
type Process interface {
	// Pid is the process identifier, or 0 when it never started.
	Pid() int
	// Signal delivers a signal to the process.
	Signal(signal os.Signal) error
	// Kill terminates the process without waiting for it to be polite.
	Kill() error
}

// Spawner starts a child process. It exists so the supervision logic can be
// tested without starting real processes.
type Spawner interface {
	Start(argv []string, env []string, out io.Writer) (Process, error)
}

// Reaper collects the session's exited children. Exactly one reaper owns them:
// as PID 1 the supervisor must reap every child, including a process that lost
// its parent and was reparented to it, and those exits have no other collector.
type Reaper interface {
	// Reap blocks until the next child exits and returns its identifier and
	// exit status, whoever that child is. It reports syscall.ECHILD when no
	// children remain.
	Reap() (pid int, status syscall.WaitStatus, err error)
}

// outputDrainer is implemented by a process whose output is copied from a pipe.
// Drained reports when the copy loop has ended, so the supervisor can flush the
// last partial line itself: nothing calls exec.Cmd.Wait any more.
type outputDrainer interface {
	Drained() <-chan struct{}
}

// exitEvent is one child exit as the reaper reported it.
type exitEvent struct {
	pid    int
	status syscall.WaitStatus
}

// exitEventBuffer holds exits that arrive before Run consumes them, so the
// status of a child that exits early is never lost.
const exitEventBuffer = 16

// child is a started process together with the exit the reaper reported for it.
type child struct {
	name    string
	process Process
	out     *lineWriter
	// exited is closed once the supervisor has reaped this child, so a status
	// write and stopAll can tell a running child from one already gone.
	exited chan struct{}
	once   sync.Once
	status syscall.WaitStatus
}

// markExited records the child's exit exactly once.
func (c *child) markExited(status syscall.WaitStatus) {
	c.once.Do(func() {
		c.status = status
		close(c.exited)
	})
}

// exitError describes how the child ended.
func (c *child) exitError() error {
	return errors.New(describeWaitStatus(c.status))
}

// describeWaitStatus says how a reaped process ended, because syscall.WaitStatus
// has no stringer of its own.
func describeWaitStatus(status syscall.WaitStatus) string {
	switch {
	case status.Exited():
		return fmt.Sprintf("exit status %d", status.ExitStatus())
	case status.Signaled():
		return fmt.Sprintf("killed by %s", status.Signal())
	case status.Stopped():
		return fmt.Sprintf("stopped by %s", status.StopSignal())
	case status.Continued():
		return "continued"
	default:
		return fmt.Sprintf("wait status %d", uint32(status))
	}
}

// state reports the child's process state as the supervisor last observed it.
func (c *child) state() string {
	select {
	case <-c.exited:
		return session.ProcessExited
	default:
		return session.ProcessRunning
	}
}

// Options describes the session the supervisor runs.
type Options struct {
	RuntimeDir string
	ControlDir string
	WaylandDir string
	// PayloadDir receives injected payloads. It defaults to a directory inside
	// RuntimeDir.
	PayloadDir string
	// ExportDir receives captures. It defaults to a directory inside
	// RuntimeDir.
	ExportDir string
	Display   string
	AgentPath string
	Width     int
	Height    int
	// ReadyTimeout bounds how long the compositor socket may take to appear.
	ReadyTimeout time.Duration
	// StopTimeout bounds how long children may take to stop.
	StopTimeout time.Duration
	// DisplayGID, when set, is the group that owns the display socket.
	DisplayGID int
}

// Supervisor runs one session.
type Supervisor struct {
	options Options
	spawn   Spawner
	reaper  Reaper
	now     func() time.Time
	log     io.Writer

	weston *child
	agent  *child

	mu     sync.Mutex
	ready  bool
	reason string
}

func newSupervisor(options Options, spawn Spawner, reaper Reaper, log io.Writer, now func() time.Time) *Supervisor {
	if options.Width <= 0 {
		options.Width = DefaultWidth
	}
	if options.Height <= 0 {
		options.Height = DefaultHeight
	}
	if options.ReadyTimeout <= 0 {
		options.ReadyTimeout = DefaultSocketWait
	}
	if options.StopTimeout <= 0 {
		options.StopTimeout = DefaultStopTimeout
	}
	if options.AgentPath == "" {
		options.AgentPath = session.AgentPath
	}
	if options.RuntimeDir == "" {
		options.RuntimeDir = session.RuntimeDir
	}
	if options.ControlDir == "" {
		options.ControlDir = session.ControlDir
	}
	if options.WaylandDir == "" {
		options.WaylandDir = session.WaylandDir
	}
	if options.PayloadDir == "" {
		options.PayloadDir = filepath.Join(options.RuntimeDir, "payload")
	}
	if options.ExportDir == "" {
		options.ExportDir = filepath.Join(options.RuntimeDir, "export")
	}
	if options.Display == "" {
		options.Display = session.WaylandDisplay
	}
	if now == nil {
		now = time.Now
	}

	return &Supervisor{options: options, spawn: spawn, reaper: reaper, log: log, now: now}
}

// Run supervises the session until the context is cancelled or one of the
// children dies. It returns nil on an orderly shutdown.
func (s *Supervisor) Run(ctx context.Context) error {
	if err := s.prepare(); err != nil {
		return err
	}

	// One reaper owns every child of the session, started before the first
	// child so no exit can be missed. The buffer holds exits that arrive
	// before Run consumes them.
	events := make(chan exitEvent, exitEventBuffer)
	reaped := make(chan struct{})
	go s.reapLoop(events, reaped)

	if err := s.startCompositor(); err != nil {
		s.setReason("the compositor did not start")
		s.writeStatus()
		return err
	}
	if err := s.waitForSocket(ctx, events); err != nil {
		s.writeStatus()
		s.stopAll(events)
		return err
	}
	if err := s.startAgent(); err != nil {
		s.setReason("the controller did not start")
		s.writeStatus()
		s.stopAll(events)
		return err
	}
	s.writeStatus()

	// Readiness arrives from the controller, not from a timer.
	watchDone := make(chan struct{})
	go s.watchReadiness(ctx, watchDone)

	var failure error
waiting:
	for {
		select {
		case <-ctx.Done():
			fmt.Fprintf(s.log, "supervisor: stopping on request\n")
			break waiting

		case event := <-events:
			switch exited := s.observe(event); {
			case exited == nil:
				// An orphan was collected; the session keeps running. That is
				// the point of the single reaper.
				continue
			case exited == s.weston:
				failure = fmt.Errorf("the compositor exited: %w", exited.exitError())
				s.setReason("the compositor exited")
			case exited == s.agent:
				failure = fmt.Errorf("the controller exited: %w", exited.exitError())
				s.setReason("the controller exited")
			}
			s.writeStatus()
			break waiting
		}
	}

	close(watchDone)
	s.stopAll(events)
	return failure
}

// reapLoop reaps every child of the session, known or not, until none remain.
// A pid is delivered once: wait4 consumes an exit status exactly once, so a
// reaper that repeats a pid must not be mistaken for a second exit.
func (s *Supervisor) reapLoop(events chan<- exitEvent, reaped chan<- struct{}) {
	defer close(reaped)

	seen := make(map[int]struct{})
	for {
		pid, status, err := s.reaper.Reap()
		if err != nil {
			if !errors.Is(err, syscall.ECHILD) {
				fmt.Fprintf(s.log, "supervisor: cannot reap a child: %v\n", err)
			}
			return
		}
		if _, duplicate := seen[pid]; duplicate {
			continue
		}
		seen[pid] = struct{}{}
		events <- exitEvent{pid: pid, status: status}
	}
}

// observe records one exit event on the child it belongs to. It reports nil
// for a pid this supervisor never started: that is a reparented orphan, and
// collecting it without ending the session is the point of the single reaper.
func (s *Supervisor) observe(event exitEvent) *child {
	for _, candidate := range []*child{s.weston, s.agent} {
		if candidate != nil && candidate.process.Pid() == event.pid {
			candidate.markExited(event.status)
			return candidate
		}
	}
	fmt.Fprintf(s.log, "supervisor: reaped orphan pid=%d status=%s\n", event.pid, describeWaitStatus(event.status))
	return nil
}

// prepare creates the directories the session owns.
//
// The runtime directory itself is a mount the container engine created, so it
// may already exist and belong to another identity: this process only creates
// what it owns, which is why a directory it cannot change is not an error.
func (s *Supervisor) prepare() error {
	if err := os.MkdirAll(s.options.RuntimeDir, 0o1777); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("supervisor: cannot create %s: %w", s.options.RuntimeDir, err)
	}

	directories := []struct {
		path string
		mode os.FileMode
	}{
		{s.options.ControlDir, 0o700},
		// The application identity has to reach the display socket; the
		// control plane is protected by peer credentials, not by this mode.
		{s.options.WaylandDir, 0o755},
		{s.options.PayloadDir, 0o755},
		{s.options.ExportDir, 0o700},
	}
	for _, directory := range directories {
		if err := os.MkdirAll(directory.path, directory.mode); err != nil {
			return fmt.Errorf("supervisor: cannot create %s: %w", directory.path, err)
		}
		if err := os.Chmod(directory.path, directory.mode); err != nil {
			return fmt.Errorf("supervisor: cannot set the mode of %s: %w", directory.path, err)
		}
	}

	return s.writeCompositorConfig()
}

// writeCompositorConfig records the configuration Weston runs with.
//
// The keyboard section pins the layout the session guarantees: the control
// protocol injects evdev keycodes, so what a key means is decided by the keymap
// the compositor hands to its clients. Keeping that decision in the session
// configuration, rather than in the host's xkb defaults, is what makes typing
// reproducible across hosts.
func (s *Supervisor) writeCompositorConfig() error {
	path := filepath.Join(s.options.ControlDir, filepath.Base(session.WestonConfigPath))
	if err := os.WriteFile(path, []byte(session.WestonConfig), 0o600); err != nil {
		return fmt.Errorf("supervisor: cannot write the compositor configuration: %w", err)
	}
	return nil
}

// startCompositor launches Weston with the wlvision shell.
func (s *Supervisor) startCompositor() error {
	argv := []string{
		westonBinary,
		"--backend=headless",
		"--renderer=pixman",
		"--shell=wlvision-shell",
		"--fake-seat",
		"--width=" + strconv.Itoa(s.options.Width),
		"--height=" + strconv.Itoa(s.options.Height),
		"--idle-time=0",
		"--socket=" + s.options.Display,
		"--config=" + filepath.Join(s.options.ControlDir, filepath.Base(session.WestonConfigPath)),
	}
	env := append(os.Environ(),
		session.EnvRuntimeDir+"="+s.options.WaylandDir,
		"WLVISION_SHELL_LOG=1",
		// The configuration file already pins the layout; the environment is
		// repeated here so a compositor that ignored its configuration would
		// still create the same keymap, and so the layout never depends on the
		// host's defaults.
		"XKB_DEFAULT_RULES="+session.KeyboardRules,
		"XKB_DEFAULT_MODEL="+session.KeyboardModel,
		"XKB_DEFAULT_LAYOUT="+session.KeyboardLayout,
		"XKB_DEFAULT_VARIANT=",
		"XKB_DEFAULT_OPTIONS=",
	)

	child, err := s.start("compositor", argv, env)
	if err != nil {
		return err
	}
	s.weston = child
	if err := s.writePID(westonPIDFile, child.process.Pid()); err != nil {
		return err
	}
	fmt.Fprintf(s.log, "supervisor: compositor started pid=%d socket=%s\n", child.process.Pid(), s.options.Display)
	return nil
}

// startAgent launches the resident controller.
func (s *Supervisor) startAgent() error {
	env := append(os.Environ(),
		session.EnvRuntimeDir+"="+s.options.WaylandDir,
		session.EnvWaylandDisplay+"="+s.options.Display,
	)

	child, err := s.start("controller", []string{s.options.AgentPath}, env)
	if err != nil {
		return err
	}
	s.agent = child
	if err := s.writePID(agentPIDFile, child.process.Pid()); err != nil {
		return err
	}
	fmt.Fprintf(s.log, "supervisor: controller started pid=%d\n", child.process.Pid())
	return nil
}

// start runs one child and arranges for its output to reach the session log.
// Its exit is collected by the session's single reaper, never here.
func (s *Supervisor) start(name string, argv []string, env []string) (*child, error) {
	out := &lineWriter{w: s.log, now: s.now}
	process, err := s.spawn.Start(argv, env, out)
	if err != nil {
		out.Flush()
		return nil, fmt.Errorf("supervisor: cannot start the %s: %w", name, err)
	}

	// The spawner copies the child's output into out on its own goroutine and
	// closes the pipe afterwards. Nothing calls exec.Cmd.Wait any more, so the
	// supervisor flushes the last partial line itself once that copy loop ends.
	if drainer, ok := process.(outputDrainer); ok {
		go func() {
			<-drainer.Drained()
			out.Flush()
		}()
	}

	return &child{name: name, process: process, out: out, exited: make(chan struct{})}, nil
}

// waitForSocket waits until the compositor publishes its display socket.
func (s *Supervisor) waitForSocket(ctx context.Context, events <-chan exitEvent) error {
	socket := filepath.Join(s.options.WaylandDir, s.options.Display)
	deadline := time.Now().Add(s.options.ReadyTimeout)

	for {
		if _, err := os.Stat(socket); err == nil {
			// The socket is what an application needs; the control plane is
			// protected by peer credentials, not by this mode.
			mode := os.FileMode(0o666)
			if s.options.DisplayGID != 0 {
				mode = 0o660
			}
			if err := os.Chmod(socket, mode); err != nil {
				return fmt.Errorf("supervisor: cannot set the mode of %s: %w", socket, err)
			}
			if s.options.DisplayGID != 0 {
				if err := os.Chown(socket, -1, s.options.DisplayGID); err != nil {
					return fmt.Errorf("supervisor: cannot set the group of %s: %w", socket, err)
				}
			}
			fmt.Fprintf(s.log, "supervisor: compositor socket ready at %s\n", socket)
			return nil
		}

		select {
		case event := <-events:
			if compositor := s.observe(event); compositor == s.weston {
				return fmt.Errorf("supervisor: the compositor exited before publishing a socket: %w", compositor.exitError())
			}
		case <-ctx.Done():
			return errors.New("supervisor: stopped while waiting for the compositor")
		case <-time.After(readyPollInterval):
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("supervisor: the compositor did not publish %s within %s", socket, s.options.ReadyTimeout)
		}
	}
}

// watchReadiness turns the controller's marker into the session's readiness.
func (s *Supervisor) watchReadiness(ctx context.Context, done <-chan struct{}) {
	marker := filepath.Join(s.options.ControlDir, readyMarkerName)
	ticker := time.NewTicker(readyPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := os.Stat(marker); err != nil {
				continue
			}
			s.mu.Lock()
			already := s.ready
			s.ready = true
			s.mu.Unlock()
			if !already {
				fmt.Fprintf(s.log, "supervisor: the session is ready\n")
				s.writeStatus()
			}
		}
	}
}

// stopAll asks the children to stop, gives them the configured window, and
// terminates whatever is left. It consumes the reaper's exit events itself:
// the session has one reaper reporting to one channel, and Run is not reading
// that channel while it stops the children.
func (s *Supervisor) stopAll(events <-chan exitEvent) {
	var children []*child
	for _, candidate := range []*child{s.agent, s.weston} {
		if candidate != nil {
			children = append(children, candidate)
		}
	}
	if len(children) == 0 {
		return
	}

	for _, child := range children {
		if err := child.process.Signal(syscall.SIGTERM); err != nil {
			fmt.Fprintf(s.log, "supervisor: cannot signal the %s: %v\n", child.name, err)
		}
	}

	// A child Run already reported is done; only the others still owe an exit.
	var pending []*child
	for _, child := range children {
		if child.state() == session.ProcessRunning {
			pending = append(pending, child)
		}
	}

	deadline := time.After(s.options.StopTimeout)
	for len(pending) > 0 {
		select {
		case event := <-events:
			child := s.observe(event)
			if child == nil {
				continue // an orphan, not one of the children being stopped
			}
			for index, candidate := range pending {
				if candidate == child {
					pending = append(pending[:index], pending[index+1:]...)
					break
				}
			}
		case <-deadline:
			for _, child := range pending {
				_ = child.process.Kill()
			}
			fmt.Fprintf(s.log, "supervisor: killed %d child process(es) that ignored the stop\n", len(pending))
			pending = nil
		}
	}
	s.writeStatus()
}

// writeStatus publishes what the session's processes are doing.
func (s *Supervisor) writeStatus() {
	status := s.currentStatus()
	payload, err := json.Marshal(status)
	if err != nil {
		fmt.Fprintf(s.log, "supervisor: cannot encode the session status: %v\n", err)
		return
	}
	payload = append(payload, '\n')

	path := filepath.Join(s.options.ControlDir, filepath.Base(session.StatusFile))
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		fmt.Fprintf(s.log, "supervisor: cannot write the session status: %v\n", err)
	}
}

// currentStatus describes the children as the supervisor sees them.
func (s *Supervisor) currentStatus() session.ContainerStatus {
	s.mu.Lock()
	ready, reason := s.ready, s.reason
	s.mu.Unlock()

	status := session.ContainerStatus{
		Ready:   ready,
		Weston:  session.ProcessNotStarted,
		Agent:   session.ProcessNotStarted,
		Message: reason,
	}
	if s.weston != nil {
		status.Weston = s.weston.state()
	}
	if s.agent != nil {
		status.Agent = s.agent.state()
	}
	return status
}

func (s *Supervisor) setReason(reason string) {
	s.mu.Lock()
	s.reason = reason
	s.ready = false
	s.mu.Unlock()
}

// writePID records a child's identifier so a later status probe can tell
// whether it is still alive.
func (s *Supervisor) writePID(name string, pid int) error {
	path := filepath.Join(s.options.ControlDir, name)
	if err := os.WriteFile(path, []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		return fmt.Errorf("supervisor: cannot record the %s: %w", name, err)
	}
	return nil
}

// ReadStatus reports the session's processes as an independent process sees
// them. The status file records what the supervisor last observed; the process
// identifiers decide whether that observation still holds, because a supervisor
// that died left its last observation behind.
func ReadStatus(options Options) session.ContainerStatus {
	if options.ControlDir == "" {
		options.ControlDir = session.ControlDir
	}
	path := filepath.Join(options.ControlDir, filepath.Base(session.StatusFile))

	status := session.ContainerStatus{
		Weston:  session.ProcessNotStarted,
		Agent:   session.ProcessNotStarted,
		Message: "the supervisor has not started",
	}
	if payload, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(payload, &status); err != nil {
			status = session.ContainerStatus{
				Weston:  session.ProcessNotStarted,
				Agent:   session.ProcessNotStarted,
				Message: "the supervisor status is unreadable",
			}
		}
	}

	// A state the supervisor itself observed is authoritative. A running child
	// is only believed while its process still exists: a supervisor that died
	// leaves its last observation behind.
	if status.Weston == session.ProcessRunning && !alive(pidFromFile(options.ControlDir, westonPIDFile)) {
		status.Weston = session.ProcessExited
		status.Ready = false
	}
	if status.Agent == session.ProcessRunning && !alive(pidFromFile(options.ControlDir, agentPIDFile)) {
		status.Agent = session.ProcessExited
		status.Ready = false
	}

	// Readiness is the controller's own report, not the supervisor's opinion.
	if _, err := os.Stat(filepath.Join(options.ControlDir, readyMarkerName)); err != nil {
		status.Ready = false
	}
	return status
}

func pidFromFile(dir, name string) int {
	payload, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(string(bytes.TrimSpace(payload)))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// alive reports whether a process this supervisor started still exists.
func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return process.Signal(syscall.Signal(0)) == nil
}

// lineWriter stamps every line a child writes, so the container log says when
// something happened. A partial line is held until it completes.
type lineWriter struct {
	mu  sync.Mutex
	w   io.Writer
	now func() time.Time
	buf []byte
}

func (l *lineWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.buf = append(l.buf, p...)
	for {
		index := bytes.IndexByte(l.buf, '\n')
		if index < 0 {
			break
		}
		l.emit(l.buf[:index])
		l.buf = l.buf[index+1:]
	}
	return len(p), nil
}

// Flush writes whatever is left of a partial line.
func (l *lineWriter) Flush() {
	l.mu.Lock()
	defer l.mu.Unlock()

	if len(l.buf) > 0 {
		l.emit(l.buf)
		l.buf = nil
	}
}

func (l *lineWriter) emit(line []byte) {
	fmt.Fprintf(l.w, "%s %s\n", l.now().UTC().Format(time.RFC3339), line)
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "status":
			os.Exit(statusCommand(os.Stdout))
		case "receive":
			os.Exit(receiveCommand(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
		default:
			fmt.Fprintf(os.Stderr, "wlvision-supervisor: unknown subcommand %q\n", os.Args[1])
			os.Exit(2)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	options := Options{DisplayGID: displayGIDFromEnv()}
	supervisor := newSupervisor(options, osSpawner{}, syscallReaper{}, os.Stdout, time.Now)
	if err := supervisor.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "wlvision-supervisor: %v\n", err)
		os.Exit(1)
	}
}

// statusCommand prints the session's process status as one JSON document. It
// always succeeds: the caller reads the document, not the exit code.
func statusCommand(stdout io.Writer) int {
	payload, err := json.Marshal(ReadStatus(Options{}))
	if err != nil {
		fmt.Fprintf(os.Stderr, "wlvision-supervisor: cannot encode the status: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s\n", payload)
	return 0
}

// receiveCommand receives one payload from the caller's standard input.
func receiveCommand(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("receive", flag.ContinueOnError)
	flags.SetOutput(stderr)

	kind := flags.String("kind", string(inject.KindBinary), "payload kind: binary or tar")
	name := flags.String("name", "", "final file or directory name")
	dir := flags.String("dir", session.PayloadDir, "payload directory")
	mode := flags.String("mode", "", "requested permissions in octal")
	maxBytes := flags.Int64("max-bytes", 0, "maximum payload size in bytes")
	maxFiles := flags.Int("max-files", 0, "maximum number of files in a bundle")

	if err := flags.Parse(args); err != nil {
		return reportFailure(stderr, result.NewFailure(result.CodeUsageError, "payload.receive", "%v", err))
	}
	if *name == "" {
		return reportFailure(stderr, result.NewFailure(result.CodeUsageError, "payload.receive",
			"a payload name is required"))
	}

	spec := inject.Spec{
		Kind:     inject.Kind(*kind),
		Name:     *name,
		MaxBytes: *maxBytes,
		MaxFiles: *maxFiles,
	}
	if *mode != "" {
		permissions, err := strconv.ParseUint(*mode, 8, 32)
		if err != nil {
			return reportFailure(stderr, result.NewFailure(result.CodeUsageError, "payload.receive",
				"the mode %q is not an octal permission", *mode))
		}
		spec.Mode = uint32(permissions)
	}

	received, err := inject.Receive(context.Background(), *dir, spec, stdin)
	if err != nil {
		var failure *result.Failure
		if errors.As(err, &failure) {
			return reportFailure(stderr, failure)
		}
		return reportFailure(stderr, result.NewFailure(result.CodePayloadRejected, "payload.receive", "%v", err))
	}

	payload, err := json.Marshal(session.PayloadResult{
		Path:   received.Path,
		Digest: received.Digest,
		Bytes:  received.Bytes,
		Files:  received.Files,
	})
	if err != nil {
		return reportFailure(stderr, result.NewFailure(result.CodePayloadRejected, "payload.receive",
			"cannot report what was stored: %v", err))
	}
	fmt.Fprintf(stdout, "%s\n", payload)
	return 0
}

// reportFailure writes a failure for the outer CLI to read and returns the exit
// code its contract implies.
func reportFailure(stderr io.Writer, failure *result.Failure) int {
	envelope := result.Fail[any]("payload.receive", "", failure)
	if err := result.RenderJSON(stderr, envelope); err != nil {
		fmt.Fprintf(stderr, "wlvision-supervisor: %v\n", err)
		return 1
	}
	return failure.Code.ExitCode()
}

func displayGIDFromEnv() int {
	value := os.Getenv(displayGIDEnv)
	if value == "" {
		return 0
	}
	gid, err := strconv.Atoi(value)
	if err != nil || gid < 0 {
		fmt.Fprintf(os.Stderr, "wlvision-supervisor: ignoring invalid %s=%q\n", displayGIDEnv, value)
		return 0
	}
	return gid
}

// osSpawner starts real child processes without os/exec: exec.Cmd.Wait would
// compete with the session's single reaper for the child's exit status, so the
// supervisor starts, signals, and reaps its children itself.
type osSpawner struct{}

// Start implements Spawner.
func (osSpawner) Start(argv []string, env []string, out io.Writer) (Process, error) {
	if len(argv) == 0 {
		return nil, errors.New("supervisor: no command to start")
	}

	// exec.Cmd would look this up; os.StartProcess does not search $PATH.
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return nil, err
	}

	// The child's stdout and stderr share one pipe, which a goroutine copies
	// into the session log.
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	stdin, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		reader.Close()
		writer.Close()
		return nil, err
	}

	process, err := os.StartProcess(path, argv, &os.ProcAttr{
		Env:   env,
		Files: []*os.File{stdin, writer, writer},
	})
	stdin.Close()
	if err != nil {
		reader.Close()
		writer.Close()
		return nil, err
	}
	// The parent must not keep its copy of the write end, or the copy loop
	// below would never see EOF when the child exits.
	writer.Close()

	started := &osProcess{process: process, drained: make(chan struct{})}
	go func() {
		defer close(started.drained)
		defer reader.Close()
		_, _ = io.Copy(out, reader)
	}()
	return started, nil
}

// osProcess is one real child, started with os.StartProcess.
type osProcess struct {
	process *os.Process
	drained chan struct{}
}

func (p *osProcess) Pid() int { return p.process.Pid }

func (p *osProcess) Signal(signal os.Signal) error { return p.process.Signal(signal) }

func (p *osProcess) Kill() error { return p.process.Kill() }

// Drained reports when the output copy loop has ended.
func (p *osProcess) Drained() <-chan struct{} { return p.drained }

// syscallReaper reaps the session's children with wait4(-1, ...): as PID 1 the
// supervisor must collect every child, including a process that lost its parent
// and was reparented to it.
type syscallReaper struct{}

// Reap implements Reaper.
func (syscallReaper) Reap() (int, syscall.WaitStatus, error) {
	var status syscall.WaitStatus
	for {
		pid, err := syscall.Wait4(-1, &status, 0, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		return pid, status, err
	}
}
