// Command wlvision is the outer CLI: it parses one command line, builds the
// session service, dispatches to exactly one operation, renders one result
// envelope, and returns the exit code of the wlvision/v1 contract.
//
// Process wiring lives in main. run holds the whole CLI so tests can drive it
// with an in-process engine and inspectable streams.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bnema/wlvision/internal/engine"
	"github.com/bnema/wlvision/internal/result"
	"github.com/bnema/wlvision/internal/session"
)

// ServiceFactory builds the session service a command runs against. main
// supplies one that talks to the Docker adapter; tests inject an enginetest
// fake so the whole CLI is exercised without a daemon.
type ServiceFactory func(session.Options) (*session.Service, error)

func main() {
	// The global flags are parsed once, so the factory that builds the service
	// does not have to look at the process arguments again.
	globals, _, _ := parseGlobals(os.Args[1:])

	newService := func(options session.Options) (*session.Service, error) {
		return newDockerService(options, globals)
	}
	os.Exit(run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr, newService))
}

// newDockerService is the production factory: a Docker CLI adapter plus a store
// under the configured state root.
func newDockerService(options session.Options, globals globalFlags) (*session.Service, error) {
	stateRoot := globals.stateRoot
	if stateRoot == "" {
		var err error
		stateRoot, err = session.DefaultRoot()
		if err != nil {
			return nil, err
		}
	}
	store, err := session.NewStore(stateRoot)
	if err != nil {
		return nil, err
	}
	docker, err := engine.NewDocker(engine.CLIRunner{Binary: "docker"}, engine.Options{Context: globals.context})
	if err != nil {
		return nil, err
	}
	options.Engine = docker
	options.Store = store
	return session.NewService(options)
}

// globalFlags are the options that apply to every command. They must appear
// before the command, which is why they are parsed separately.
type globalFlags struct {
	json      bool
	stateRoot string
	image     string
	context   string
}

func parseGlobals(args []string) (globalFlags, []string, error) {
	var globals globalFlags
	flags := flag.NewFlagSet("wlvision", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Usage = func() {}
	flags.BoolVar(&globals.json, "json", false, "write one JSON envelope on stdout")
	flags.StringVar(&globals.stateRoot, "state-root", "", "session state directory")
	flags.StringVar(&globals.image, "image", "", "session image reference")
	flags.StringVar(&globals.context, "context", "", "container engine context")
	if err := flags.Parse(args); err != nil {
		return globals, nil, err
	}
	return globals, flags.Args(), nil
}

// cli carries the process context of one run invocation.
type cli struct {
	ctx     context.Context
	stdin   io.Reader
	stdout  io.Writer
	stderr  io.Writer
	json    bool
	image   string
	factory ServiceFactory
	// readyTimeout is set by run before the service is built.
	readyTimeout time.Duration
}

// run executes one command line. It is the whole CLI except process wiring.
func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, newService ServiceFactory) int {
	globals, remaining, err := parseGlobals(args)
	command := &cli{
		ctx:     ctx,
		stdin:   stdin,
		stdout:  stdout,
		stderr:  stderr,
		json:    globals.json,
		image:   globals.image,
		factory: newService,
	}
	if err != nil {
		return command.usageError("wlvision", "", err)
	}
	if len(remaining) == 0 {
		return command.usageError("wlvision", "", errors.New("a command is required"))
	}

	switch remaining[0] {
	case "doctor":
		return command.doctor(remaining[1:])
	case "session":
		return command.session(remaining[1:])
	case "run":
		return command.runApplication(remaining[1:])
	case "inject":
		return command.inject(remaining[1:])
	case "logs":
		return command.logs(remaining[1:])
	default:
		return command.usageError(remaining[0], "", fmt.Errorf("unknown command %q", remaining[0]))
	}
}

// flagSet returns a flag set whose errors the caller renders, never the
// standard library's own usage dump.
func (c *cli) flagSet(name string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Usage = func() {}
	return flags
}

func (c *cli) sessionOptions() session.Options {
	return session.Options{Image: c.image, ReadyTimeout: c.readyTimeout}
}

func (c *cli) newService() (*session.Service, error) {
	return c.factory(c.sessionOptions())
}

func (c *cli) doctor(args []string) int {
	const operation = "doctor"
	flags := c.flagSet("wlvision doctor")
	if err := flags.Parse(args); err != nil {
		return c.usageError(operation, "", err)
	}
	if flags.NArg() > 0 {
		return c.usageError(operation, "", fmt.Errorf("unexpected argument %q", flags.Arg(0)))
	}

	service, err := c.newService()
	if err != nil {
		return c.fail(operation, "", err, result.CodeEngineUnavailable)
	}
	report, err := service.Doctor(c.ctx)
	if err != nil {
		return c.fail(operation, "", err, result.CodeEngineUnavailable)
	}
	return c.success(operation, "", 0, report)
}

func (c *cli) session(args []string) int {
	if len(args) == 0 {
		return c.usageError("session", "", errors.New("a session subcommand is required"))
	}
	switch args[0] {
	case "create":
		return c.sessionCreate(args[1:])
	case "list":
		return c.sessionList(args[1:])
	case "inspect":
		return c.sessionInspect(args[1:])
	case "close":
		return c.sessionClose(args[1:])
	case "purge":
		return c.sessionPurge(args[1:])
	default:
		return c.usageError("session", "", fmt.Errorf("unknown session subcommand %q", args[0]))
	}
}

// createResult is what `session create` reports. Without --wait the session is
// left in state creating, and Message says so.
type createResult struct {
	Session session.Record `json:"session"`
	Message string         `json:"message,omitempty"`
}

func (c *cli) sessionCreate(args []string) int {
	const operation = "session.create"
	var (
		id        string
		memory    int64
		pids      int
		fileSize  int64
		openFiles int
		retention time.Duration
		wait      bool
	)

	flags := c.flagSet("wlvision session create")
	flags.StringVar(&id, "session", "", "session identifier")
	flags.Var(sizeFlag{&memory}, "memory", "memory limit in bytes")
	flags.IntVar(&pids, "pids", 0, "process limit")
	flags.Var(sizeFlag{&fileSize}, "file-size", "per-process file size limit in bytes")
	flags.IntVar(&openFiles, "open-files", 0, "open file limit")
	flags.Var(durationFlag{&retention}, "retention", "retention duration")
	flags.BoolVar(&wait, "wait", false, "start the session and wait until it is ready")
	if err := flags.Parse(args); err != nil {
		return c.usageError(operation, id, err)
	}
	if flags.NArg() > 0 {
		return c.usageError(operation, id, fmt.Errorf("unexpected argument %q", flags.Arg(0)))
	}
	if id == "" {
		return c.usageError(operation, "", errors.New("--session is required"))
	}
	if pids < 0 || openFiles < 0 {
		return c.usageError(operation, id, errors.New("--pids and --open-files must not be negative"))
	}

	service, err := c.newService()
	if err != nil {
		return c.fail(operation, id, err, result.CodeEngineUnavailable)
	}
	record, err := service.Create(c.ctx, session.CreateRequest{
		Session:   id,
		Limits:    engine.Limits{MemoryBytes: memory, Pids: pids, FileSizeBytes: fileSize, OpenFiles: openFiles},
		Retention: retention,
	})
	if err != nil {
		return c.fail(operation, id, err, result.CodeSessionNotReady)
	}

	view := createResult{Session: record}
	if wait {
		record, err = service.Start(c.ctx, id)
		if err != nil {
			return c.fail(operation, id, err, result.CodeSessionNotReady)
		}
		view.Session = record
	} else {
		view.Message = "session created in state creating; pass --wait to start it"
	}
	return c.success(operation, id, record.Revision, view)
}

// listResult is what `session list` reports.
type listResult struct {
	Sessions  []session.Record  `json:"sessions"`
	Anomalies []session.Anomaly `json:"anomalies,omitempty"`
}

func (c *cli) sessionList(args []string) int {
	const operation = "session.list"
	flags := c.flagSet("wlvision session list")
	if err := flags.Parse(args); err != nil {
		return c.usageError(operation, "", err)
	}
	if flags.NArg() > 0 {
		return c.usageError(operation, "", fmt.Errorf("unexpected argument %q", flags.Arg(0)))
	}

	service, err := c.newService()
	if err != nil {
		return c.fail(operation, "", err, result.CodeEngineUnavailable)
	}
	records, anomalies, err := service.List(c.ctx)
	if err != nil {
		return c.fail(operation, "", err, result.CodeSessionNotReady)
	}
	if records == nil {
		records = []session.Record{}
	}
	return c.success(operation, "", 0, listResult{Sessions: records, Anomalies: anomalies})
}

// inspectResult is what `session inspect` reports.
type inspectResult struct {
	Session   session.Record    `json:"session"`
	Anomalies []session.Anomaly `json:"anomalies,omitempty"`
}

func (c *cli) sessionInspect(args []string) int {
	const operation = "session.inspect"
	var id string
	flags := c.flagSet("wlvision session inspect")
	flags.StringVar(&id, "session", "", "session identifier")
	if err := flags.Parse(args); err != nil {
		return c.usageError(operation, id, err)
	}
	if flags.NArg() > 0 {
		return c.usageError(operation, id, fmt.Errorf("unexpected argument %q", flags.Arg(0)))
	}
	if id == "" {
		return c.usageError(operation, "", errors.New("--session is required"))
	}

	service, err := c.newService()
	if err != nil {
		return c.fail(operation, id, err, result.CodeEngineUnavailable)
	}
	record, anomalies, err := service.Inspect(c.ctx, id)
	if err != nil {
		return c.fail(operation, id, err, result.CodeSessionNotReady)
	}
	return c.success(operation, id, record.Revision, inspectResult{Session: record, Anomalies: anomalies})
}

func (c *cli) sessionClose(args []string) int {
	const operation = "session.close"
	var (
		id          string
		stopTimeout time.Duration
	)
	flags := c.flagSet("wlvision session close")
	flags.StringVar(&id, "session", "", "session identifier")
	flags.Var(durationFlag{&stopTimeout}, "stop-timeout", "container stop timeout")
	if err := flags.Parse(args); err != nil {
		return c.usageError(operation, id, err)
	}
	if flags.NArg() > 0 {
		return c.usageError(operation, id, fmt.Errorf("unexpected argument %q", flags.Arg(0)))
	}
	if id == "" {
		return c.usageError(operation, "", errors.New("--session is required"))
	}

	service, err := c.newService()
	if err != nil {
		return c.fail(operation, id, err, result.CodeEngineUnavailable)
	}
	record, err := service.Close(c.ctx, id, stopTimeout)
	if err != nil {
		return c.fail(operation, id, err, result.CodeSessionNotReady)
	}
	return c.success(operation, id, record.Revision, record)
}

// purgeResult reports which retained sessions cleanup released.
type purgeResult struct {
	Purged  []string `json:"purged,omitempty"`
	Message string   `json:"message,omitempty"`
}

// sessionPurge closes every session whose retention deadline has passed. It is
// what keeps a crashed or failed session from becoming permanent state: a run
// that is retained for diagnosis is released once it has been diagnosable for
// as long as the caller asked for.
func (c *cli) sessionPurge(args []string) int {
	const operation = "session.purge"

	flags := c.flagSet("wlvision session purge")
	if err := flags.Parse(args); err != nil {
		return c.usageError(operation, "", err)
	}
	if flags.NArg() > 0 {
		return c.usageError(operation, "", fmt.Errorf("unexpected argument %q", flags.Arg(0)))
	}

	service, err := c.newService()
	if err != nil {
		return c.fail(operation, "", err, result.CodeEngineUnavailable)
	}

	purged, err := service.Purge(c.ctx)
	if err != nil {
		return c.fail(operation, "", err, result.CodeSessionNotReady)
	}

	message := "nothing was past its retention deadline"
	if len(purged) > 0 {
		message = fmt.Sprintf("released %d session(s)", len(purged))
	}
	return c.success(operation, "", 0, purgeResult{Purged: purged, Message: message})
}

func (c *cli) runApplication(args []string) int {
	const operation = "run"
	var (
		id           string
		ephemeral    bool
		workdir      string
		env          []string
		readyTimeout time.Duration
	)
	flags := c.flagSet("wlvision run")
	flags.StringVar(&id, "session", "", "session identifier")
	flags.BoolVar(&ephemeral, "ephemeral", false, "close the session when the application exits")
	flags.StringVar(&workdir, "workdir", "", "application working directory")
	flags.Var(stringList{name: "--env", values: &env}, "env", "application environment as KEY=VALUE")
	flags.Var(durationFlag{&readyTimeout}, "ready-timeout", "how long the session may take to become ready")
	if err := flags.Parse(args); err != nil {
		return c.usageError(operation, id, err)
	}
	// Everything after -- is the application command, never a wlvision flag.
	argv := flags.Args()
	if id == "" {
		return c.usageError(operation, "", errors.New("--session is required"))
	}
	if len(argv) == 0 {
		return c.usageError(operation, id, errors.New("an application command is required after --"))
	}

	c.readyTimeout = readyTimeout
	service, err := c.newService()
	if err != nil {
		return c.fail(operation, id, err, result.CodeEngineUnavailable)
	}

	// The application's output must never corrupt the single JSON envelope:
	// with --json both of its streams go to stderr; otherwise its stdout goes
	// to stdout and its stderr to stderr.
	applicationStdout := c.stdout
	if c.json {
		applicationStdout = c.stderr
	}
	record, err := service.Run(c.ctx, session.RunRequest{
		Session:   id,
		Argv:      argv,
		Env:       env,
		WorkDir:   workdir,
		Ephemeral: ephemeral,
		Stdin:     c.stdin,
		Stdout:    applicationStdout,
		Stderr:    c.stderr,
	})
	if err != nil {
		return c.fail(operation, id, err, result.CodeSessionNotReady)
	}
	return c.success(operation, id, record.Revision, record)
}

func (c *cli) inject(args []string) int {
	const operation = "inject"
	var (
		id       string
		binary   string
		bundle   string
		mode     uint32
		maxBytes int64
		maxFiles int
	)
	flags := c.flagSet("wlvision inject")
	flags.StringVar(&id, "session", "", "session identifier")
	flags.StringVar(&binary, "binary", "", "name of a single binary payload")
	flags.StringVar(&bundle, "bundle", "", "name of a tar bundle payload")
	flags.Var(octalFlag{&mode}, "mode", "payload mode in octal")
	flags.Var(sizeFlag{&maxBytes}, "max-bytes", "maximum payload size in bytes")
	flags.IntVar(&maxFiles, "max-files", 0, "maximum number of bundle files")
	if err := flags.Parse(args); err != nil {
		return c.usageError(operation, id, err)
	}
	if flags.NArg() > 0 {
		return c.usageError(operation, id, fmt.Errorf("unexpected argument %q", flags.Arg(0)))
	}
	if id == "" {
		return c.usageError(operation, "", errors.New("--session is required"))
	}
	switch {
	case binary != "" && bundle != "":
		return c.usageError(operation, id, errors.New("--binary and --bundle are mutually exclusive"))
	case binary == "" && bundle == "":
		return c.usageError(operation, id, errors.New("one of --binary or --bundle is required"))
	}
	if maxFiles < 0 {
		return c.usageError(operation, id, errors.New("--max-files must not be negative"))
	}
	kind, name := "binary", binary
	if bundle != "" {
		kind, name = "tar", bundle
	}

	service, err := c.newService()
	if err != nil {
		return c.fail(operation, id, err, result.CodeEngineUnavailable)
	}
	payload, err := service.Inject(c.ctx, session.InjectRequest{
		Session:  id,
		Kind:     kind,
		Name:     name,
		Mode:     mode,
		MaxBytes: maxBytes,
		MaxFiles: maxFiles,
		Source:   c.stdin,
	})
	if err != nil {
		return c.fail(operation, id, err, result.CodeSessionNotReady)
	}
	return c.success(operation, id, 0, payload)
}

// logsResult is what `logs` reports. In JSON mode the log text is carried in
// the envelope so stdout holds exactly one document; in human mode the log is
// streamed to stdout and only the tail size is echoed.
type logsResult struct {
	Tail int    `json:"tail"`
	Logs string `json:"logs,omitempty"`
}

func (c *cli) logs(args []string) int {
	const operation = "logs"
	var (
		id   string
		tail int
	)
	flags := c.flagSet("wlvision logs")
	flags.StringVar(&id, "session", "", "session identifier")
	flags.IntVar(&tail, "tail", 0, "how many log lines to return")
	if err := flags.Parse(args); err != nil {
		return c.usageError(operation, id, err)
	}
	if flags.NArg() > 0 {
		return c.usageError(operation, id, fmt.Errorf("unexpected argument %q", flags.Arg(0)))
	}
	if id == "" {
		return c.usageError(operation, "", errors.New("--session is required"))
	}
	if tail < 0 {
		return c.usageError(operation, id, errors.New("--tail must not be negative"))
	}

	service, err := c.newService()
	if err != nil {
		return c.fail(operation, id, err, result.CodeEngineUnavailable)
	}
	if c.json {
		var captured bytes.Buffer
		if err := service.Logs(c.ctx, id, tail, &captured); err != nil {
			return c.fail(operation, id, err, result.CodeSessionNotReady)
		}
		return c.success(operation, id, 0, logsResult{Tail: tail, Logs: captured.String()})
	}
	if err := service.Logs(c.ctx, id, tail, c.stdout); err != nil {
		return c.fail(operation, id, err, result.CodeSessionNotReady)
	}
	return c.success(operation, id, 0, logsResult{Tail: tail})
}

// success renders a success envelope on stdout and returns exit code 0.
func (c *cli) success[T any](operation, sessionID string, revision uint64, value T) int {
	envelope := result.OK(operation, sessionID, revision, value)
	if err := c.render(envelope); err != nil {
		fmt.Fprintf(c.stderr, "wlvision: %v\n", err)
		return 1
	}
	return 0
}

// fail renders a failure envelope. JSON goes to stdout so a caller always
// reads one document; human-mode errors go to stderr, leaving stdout to data.
func (c *cli) fail(operation, sessionID string, err error, fallback result.Code) int {
	failure := failureFor(operation, sessionID, err, fallback)
	if c.json {
		_ = result.RenderJSON(c.stdout, result.Fail[any](operation, sessionID, failure))
	} else {
		_ = result.RenderHuman(c.stderr, result.Fail[any](operation, sessionID, failure))
	}
	return failure.Code.ExitCode()
}

// usageError reports invalid input: exit 2, the usage text on stderr, and a
// full envelope in JSON mode.
func (c *cli) usageError(operation, sessionID string, err error) int {
	if operation == "" {
		operation = "wlvision"
	}
	fmt.Fprint(c.stderr, usageText)
	failure := result.NewFailure(result.CodeUsageError, operation, "%v", err)
	return c.fail(operation, sessionID, failure, result.CodeUsageError)
}

func (c *cli) render[T any](envelope result.Envelope[T]) error {
	if c.json {
		return result.RenderJSON(c.stdout, envelope)
	}
	return result.RenderHuman(c.stdout, envelope)
}

// failureFor keeps an existing failure and fills in the operation and session,
// or describes an unexpected error with the command's fallback code.
func failureFor(operation, sessionID string, err error, fallback result.Code) *result.Failure {
	var failure *result.Failure
	if errors.As(err, &failure) {
		described := *failure
		if described.Operation == "" {
			described.Operation = operation
		}
		if described.Session == "" {
			described.Session = sessionID
		}
		return &described
	}
	return result.NewFailure(fallback, operation, "%v", err)
}

// sizeFlag accepts a plain byte count or a k, m, or g suffix (1024-based).
type sizeFlag struct{ target *int64 }

func (f sizeFlag) String() string {
	if f.target == nil {
		return "0"
	}
	return strconv.FormatInt(*f.target, 10)
}

func (f sizeFlag) Set(text string) error {
	size, err := parseSize(text)
	if err != nil {
		return err
	}
	*f.target = size
	return nil
}

func parseSize(text string) (int64, error) {
	value := strings.TrimSpace(strings.ToLower(text))
	if value == "" {
		return 0, errors.New("a size is required")
	}
	multiplier := int64(1)
	switch value[len(value)-1] {
	case 'k':
		multiplier, value = 1<<10, value[:len(value)-1]
	case 'm':
		multiplier, value = 1<<20, value[:len(value)-1]
	case 'g':
		multiplier, value = 1<<30, value[:len(value)-1]
	}
	count, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: accepted forms are a byte count or a count with a k, m, or g suffix", text)
	}
	if count < 0 {
		return 0, fmt.Errorf("invalid size %q: must not be negative", text)
	}
	if count > math.MaxInt64/multiplier {
		return 0, fmt.Errorf("invalid size %q: too large", text)
	}
	return count * multiplier, nil
}

// durationFlag parses a Go duration string.
type durationFlag struct{ target *time.Duration }

func (f durationFlag) String() string {
	if f.target == nil || *f.target == 0 {
		return "0s"
	}
	return f.target.String()
}

func (f durationFlag) Set(text string) error {
	duration, err := time.ParseDuration(text)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %v", text, err)
	}
	if duration < 0 {
		return fmt.Errorf("invalid duration %q: must not be negative", text)
	}
	*f.target = duration
	return nil
}

// octalFlag parses a file mode written in octal, with or without a 0o prefix.
type octalFlag struct{ target *uint32 }

func (f octalFlag) String() string {
	if f.target == nil {
		return "0"
	}
	return strconv.FormatUint(uint64(*f.target), 8)
}

func (f octalFlag) Set(text string) error {
	value := strings.TrimSpace(text)
	value = strings.TrimPrefix(strings.TrimPrefix(value, "0o"), "0O")
	parsed, err := strconv.ParseUint(value, 8, 32)
	if err != nil {
		return fmt.Errorf("invalid mode %q: expected an octal value such as 0755", text)
	}
	if parsed > 0o7777 {
		return fmt.Errorf("invalid mode %q: expected at most 07777", text)
	}
	*f.target = uint32(parsed)
	return nil
}

// stringList collects a repeated KEY=VALUE flag.
type stringList struct {
	name   string
	values *[]string
}

func (l stringList) String() string {
	if l.values == nil {
		return ""
	}
	return strings.Join(*l.values, ",")
}

func (l stringList) Set(text string) error {
	if !strings.Contains(text, "=") {
		return fmt.Errorf("%s expects KEY=VALUE, got %q", l.name, text)
	}
	*l.values = append(*l.values, text)
	return nil
}

const usageText = `Usage: wlvision [GLOBAL OPTIONS] COMMAND [ARGS...]

Global options (before the command):
  --json                 write one JSON envelope on stdout
  --state-root DIR       session state directory (default $XDG_STATE_HOME/wlvision)
  --image REF            session image (default ` + session.DefaultImage + `)
  --context NAME         container engine context

Commands:
  doctor
  session create --session ID [--memory BYTES] [--pids N] [--file-size BYTES] [--open-files N] [--retention DURATION] [--wait]
  session list
  session inspect --session ID
  session close --session ID [--stop-timeout DURATION]
  run --session ID [--ephemeral] [--workdir DIR] [--env K=V]... [--ready-timeout DURATION] -- CMD [ARGS...]
  inject --session ID (--binary NAME | --bundle NAME) [--mode OCTAL] [--max-bytes N] [--max-files N]
  logs --session ID [--tail N]

Sizes accept a plain byte count or a k, m, or g suffix (1024-based).
Durations accept Go duration strings such as 30s, 5m, or 1h.
`
