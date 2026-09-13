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

// Process is one supervised child.
type Process interface {
	// Pid is the process identifier, or 0 when it never started.
	Pid() int
	// Wait blocks until the process exits.
	Wait() error
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

// Options describes the session the supervisor runs.
type Options struct {
	RuntimeDir string
	ControlDir string
	WaylandDir string
	// PayloadDir receives injected payloads. It defaults to a directory inside
	// RuntimeDir.
	PayloadDir string
	Display    string
	AgentPath  string
	Width      int
	Height     int
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
	now     func() time.Time
	log     io.Writer

	weston *child
	agent  *child

	mu     sync.Mutex
	ready  bool
	reason string
}

// child is a started process together with its exit channel.
type child struct {
	name    string
	process Process
	out     *lineWriter
	done    chan error
	// exited is closed when the process is reaped, so a status write can tell a
	// running child from one that is already gone without consuming its exit.
	exited chan struct{}
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

func newSupervisor(options Options, spawn Spawner, log io.Writer, now func() time.Time) *Supervisor {
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
	if options.Display == "" {
		options.Display = session.WaylandDisplay
	}
	if now == nil {
		now = time.Now
	}

	return &Supervisor{options: options, spawn: spawn, log: log, now: now}
}

// Run supervises the session until the context is cancelled or one of the
// children dies. It returns nil on an orderly shutdown.
func (s *Supervisor) Run(ctx context.Context) error {
	if err := s.prepare(); err != nil {
		return err
	}
	if err := s.startCompositor(); err != nil {
		s.setReason("the compositor did not start")
		s.writeStatus()
		return err
	}
	if err := s.waitForSocket(ctx); err != nil {
		s.writeStatus()
		s.stopAll()
		return err
	}
	if err := s.startAgent(); err != nil {
		s.setReason("the controller did not start")
		s.writeStatus()
		s.stopAll()
		return err
	}
	s.writeStatus()

	// Readiness arrives from the controller, not from a timer.
	watchDone := make(chan struct{})
	go s.watchReadiness(ctx, watchDone)

	var failure error
	select {
	case <-ctx.Done():
		fmt.Fprintf(s.log, "supervisor: stopping on request\n")

	case err := <-s.weston.done:
		failure = fmt.Errorf("the compositor exited: %w", err)
		s.setReason("the compositor exited")
		s.writeStatus()

	case err := <-s.agent.done:
		failure = fmt.Errorf("the controller exited: %w", err)
		s.setReason("the controller exited")
		s.writeStatus()
	}

	close(watchDone)
	s.stopAll()
	return failure
}

// prepare creates the runtime layout. Permissions are the supervisor's
// business because it is the only writer of the session's own state.
func (s *Supervisor) prepare() error {
	directories := []struct {
		path string
		mode os.FileMode
	}{
		{s.options.RuntimeDir, 0o711},
		{s.options.ControlDir, 0o700},
		{s.options.WaylandDir, 0o770},
		{s.options.PayloadDir, 0o755},
	}
	for _, directory := range directories {
		if err := os.MkdirAll(directory.path, directory.mode); err != nil {
			return fmt.Errorf("supervisor: cannot create %s: %w", directory.path, err)
		}
		if err := os.Chmod(directory.path, directory.mode); err != nil {
			return fmt.Errorf("supervisor: cannot set the mode of %s: %w", directory.path, err)
		}
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
	}
	env := append(os.Environ(),
		session.EnvRuntimeDir+"="+s.options.WaylandDir,
		"WLVISION_SHELL_LOG=1",
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

// start runs one child and starts collecting its exit.
func (s *Supervisor) start(name string, argv []string, env []string) (*child, error) {
	out := &lineWriter{w: s.log, now: s.now}
	process, err := s.spawn.Start(argv, env, out)
	if err != nil {
		out.Flush()
		return nil, fmt.Errorf("supervisor: cannot start the %s: %w", name, err)
	}

	child := &child{name: name, process: process, out: out, done: make(chan error, 1), exited: make(chan struct{})}
	go func() {
		err := process.Wait()
		close(child.exited)
		child.done <- err
		out.Flush()
	}()
	return child, nil
}

// waitForSocket waits until the compositor publishes its display socket.
func (s *Supervisor) waitForSocket(ctx context.Context) error {
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
		case err := <-s.weston.done:
			return fmt.Errorf("supervisor: the compositor exited before publishing a socket: %w", err)
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
// terminates whatever is left.
func (s *Supervisor) stopAll() {
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

	deadline := time.After(s.options.StopTimeout)
	pending := children
	for len(pending) > 0 {
		select {
		case <-pending[0].done:
			pending = pending[1:]
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
	supervisor := newSupervisor(options, execSpawner{}, os.Stdout, time.Now)
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

// execSpawner starts real child processes.
type execSpawner struct{}

// Start implements Spawner.
func (execSpawner) Start(argv []string, env []string, out io.Writer) (Process, error) {
	if len(argv) == 0 {
		return nil, errors.New("supervisor: no command to start")
	}

	command := exec.Command(argv[0], argv[1:]...)
	command.Env = env
	command.Stdout = out
	command.Stderr = out
	// Children are meant to die with the session, never to linger as orphans.
	command.WaitDelay = DefaultStopTimeout
	if err := command.Start(); err != nil {
		return nil, err
	}
	return &execProcess{command: command}, nil
}

// execProcess adapts os/exec to the supervised Process interface.
type execProcess struct {
	command *exec.Cmd
}

func (p *execProcess) Pid() int { return p.command.Process.Pid }

func (p *execProcess) Wait() error { return p.command.Wait() }

func (p *execProcess) Signal(signal os.Signal) error { return p.command.Process.Signal(signal) }

func (p *execProcess) Kill() error { return p.command.Process.Kill() }
