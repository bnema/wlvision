package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"time"

	"github.com/bnema/wlvision/internal/engine"
	"github.com/bnema/wlvision/internal/result"
)

// Defaults for a session that the caller does not configure.
const (
	// DefaultImage is the runtime image reference.
	DefaultImage = "wlvision-runtime:latest"
	// DefaultControlUID runs the compositor, the supervisor, and the resident
	// controller.
	DefaultControlUID uint32 = 1000
	// DefaultApplicationUID runs injected applications. It can reach the
	// display socket and nothing else of the control plane.
	DefaultApplicationUID uint32 = 1001
	// DefaultReadyTimeout bounds how long a created session may take to
	// become ready.
	DefaultReadyTimeout = 30 * time.Second
	// DefaultStopTimeout bounds how long a container may take to stop.
	DefaultStopTimeout = 10 * time.Second
	// DefaultLogTail is how many log lines a log request returns by default.
	DefaultLogTail = 200
	// MaxPayloadBytes and MaxPayloadFiles bound one injected stream.
	MaxPayloadBytes int64 = 64 << 20
	MaxPayloadFiles       = 1024
)

// readyPollInterval is how often a readiness probe asks the supervisor.
const readyPollInterval = 100 * time.Millisecond

// Service is the whole application surface of wlvision. Every command maps to
// one method here, so the renderers never touch the engine, and the engine
// never learns about command syntax.
type Service struct {
	engine         engine.Engine
	store          *Store
	image          string
	now            func() time.Time
	controlUID     uint32
	applicationUID uint32
	retention      time.Duration
	readyTimeout   time.Duration
	stopTimeout    time.Duration
	supervisorArgv []string
}

// Options configures a Service. Only the engine and the store are required.
type Options struct {
	Engine engine.Engine
	Store  *Store
	Image  string
	// Now is the clock, injected so lifecycle tests are deterministic.
	Now            func() time.Time
	ControlUID     uint32
	ApplicationUID uint32
	Retention      time.Duration
	ReadyTimeout   time.Duration
	StopTimeout    time.Duration
}

// NewService builds the service.
func NewService(options Options) (*Service, error) {
	if options.Engine == nil || options.Store == nil {
		return nil, errors.New("session: a service needs an engine and a store")
	}

	service := &Service{
		engine:         options.Engine,
		store:          options.Store,
		image:          options.Image,
		now:            options.Now,
		controlUID:     options.ControlUID,
		applicationUID: options.ApplicationUID,
		retention:      options.Retention,
		readyTimeout:   options.ReadyTimeout,
		stopTimeout:    options.StopTimeout,
		supervisorArgv: []string{SupervisorPath},
	}
	if service.image == "" {
		service.image = DefaultImage
	}
	if service.now == nil {
		service.now = time.Now
	}
	if service.controlUID == 0 {
		service.controlUID = DefaultControlUID
	}
	if service.applicationUID == 0 {
		service.applicationUID = DefaultApplicationUID
	}
	if service.controlUID == service.applicationUID {
		return nil, errors.New("session: the control and application identities must differ")
	}
	if service.retention == 0 {
		service.retention = DefaultRetention
	}
	if service.readyTimeout == 0 {
		service.readyTimeout = DefaultReadyTimeout
	}
	if service.stopTimeout == 0 {
		service.stopTimeout = DefaultStopTimeout
	}
	return service, nil
}

// Anomaly is one reconciliation finding, tied to the session it concerns.
type Anomaly struct {
	Session string `json:"session"`
	Kind    string `json:"kind"`
}

// DoctorReport answers "can wlvision run here, and what does it already run".
type DoctorReport struct {
	StateRoot    string              `json:"state_root"`
	Image        string              `json:"image"`
	ControlUID   uint32              `json:"control_uid"`
	AppUID       uint32              `json:"application_uid"`
	Capabilities engine.Capabilities `json:"capabilities"`
	Degradations []string            `json:"degradations,omitempty"`
	Sessions     []Record            `json:"sessions,omitempty"`
	Anomalies    []Anomaly           `json:"anomalies,omitempty"`
}

// Doctor inspects the engine and reconciles every stored session.
func (s *Service) Doctor(ctx context.Context) (DoctorReport, error) {
	capabilities, err := s.engine.Capabilities(ctx)
	if err != nil {
		return DoctorReport{}, err
	}

	records, anomalies, err := s.List(ctx)
	if err != nil {
		return DoctorReport{}, err
	}

	return DoctorReport{
		StateRoot:    s.store.Root(),
		Image:        s.image,
		ControlUID:   s.controlUID,
		AppUID:       s.applicationUID,
		Capabilities: capabilities,
		Degradations: capabilities.Degradations(),
		Sessions:     records,
		Anomalies:    anomalies,
	}, nil
}

// CreateRequest asks for a new session.
type CreateRequest struct {
	Session   string
	Limits    engine.Limits
	Retention time.Duration
}

// Create writes the session record before creating the container, so a crash
// at any point leaves a record an operator can act on.
func (s *Service) Create(ctx context.Context, request CreateRequest) (Record, error) {
	if !ValidSessionID(request.Session) {
		return Record{}, result.NewFailure(result.CodeUsageError, "session.create",
			"invalid session identifier %q", request.Session)
	}

	capabilities, err := s.engine.Capabilities(ctx)
	if err != nil {
		return Record{}, err
	}

	if existing, err := s.store.Load(request.Session); err == nil && existing.State != StateClosed {
		return Record{}, result.NewFailure(result.CodeSessionNotReady, "session.create",
			"session %q already exists in state %s", request.Session, existing.State)
	}

	now := s.now()
	retention := request.Retention
	if retention == 0 {
		retention = s.retention
	}

	degradations := mergeDegradations(capabilities.Degradations(), capabilities.MissingLimits(request.Limits))

	record := Record{
		Schema:            result.Schema,
		Session:           request.Session,
		ContainerName:     ContainerName(request.Session),
		State:             StateCreating,
		Engine:            EngineInfo{Kind: string(capabilities.Kind), Context: capabilities.Context, ServerVersion: capabilities.ServerVersion},
		Image:             s.image,
		Limits:            request.Limits,
		Degradations:      degradations,
		CreatedAt:         now,
		UpdatedAt:         now,
		RetentionDeadline: now.Add(retention),
	}
	if err := s.store.Save(record); err != nil {
		return Record{}, err
	}

	id, err := s.engine.Create(ctx, engine.CreateSpec{
		Image:          s.image,
		Name:           record.ContainerName,
		Session:        record.Session,
		ControlUID:     s.controlUID,
		ControlGID:     s.controlUID,
		ApplicationUID: s.applicationUID,
		ApplicationGID: s.applicationUID,
		Command:        s.supervisorArgv,
		Tmpfs:          sessionMounts(),
		Limits:         request.Limits,
	})
	if err != nil {
		record.State = StateFailed
		record.UpdatedAt = s.now()
		if saveErr := s.store.Save(record); saveErr != nil {
			return Record{}, fmt.Errorf("session: %v (and the record could not be updated: %w)", err, saveErr)
		}
		return record, err
	}

	record.ContainerID = id
	record.UpdatedAt = s.now()
	if err := s.store.Save(record); err != nil {
		return Record{}, err
	}
	return record, nil
}

// Start starts a created session and waits until it is ready to accept
// commands.
func (s *Service) Start(ctx context.Context, id string) (Record, error) {
	record, err := s.store.Load(id)
	if err != nil {
		return Record{}, err
	}
	if record.State != StateCreating {
		return record, result.NewFailure(result.CodeSessionNotReady, "session.start",
			"session %q is %s, not creating", id, record.State)
	}

	if err := s.engine.Start(ctx, record.ContainerID); err != nil {
		record.State = StateFailed
		record.UpdatedAt = s.now()
		if saveErr := s.store.Save(record); saveErr != nil {
			return record, fmt.Errorf("session: %v (and the record could not be updated: %w)", err, saveErr)
		}
		return record, err
	}

	if _, err := s.WaitReady(ctx, id, s.readyTimeout); err != nil {
		record.State = StateFailed
		record.UpdatedAt = s.now()
		if saveErr := s.store.Save(record); saveErr != nil {
			return record, fmt.Errorf("session: %v (and the record could not be updated: %w)", err, saveErr)
		}
		return record, err
	}

	if err := record.Transition(StateReady, s.now()); err != nil {
		return record, err
	}
	if err := s.store.Save(record); err != nil {
		return record, err
	}
	return record, nil
}

// WaitReady asks the supervisor until the session reports readiness. A
// compositor or controller that exited during startup is reported as a failed
// session rather than as a timeout, because waiting longer cannot help.
func (s *Service) WaitReady(ctx context.Context, id string, timeout time.Duration) (ContainerStatus, error) {
	record, err := s.store.Load(id)
	if err != nil {
		return ContainerStatus{}, err
	}
	if timeout == 0 {
		timeout = s.readyTimeout
	}

	// The deadline is wall-clock: readiness is a bound on how long a real
	// container may take to come up, not a value stamped into a record, so it
	// must not come from the injected record clock.
	deadline := time.Now().Add(timeout)
	var last ContainerStatus
	for {
		status, err := s.containerStatus(ctx, record.ContainerID)
		if err == nil {
			last = status
			if status.Ready {
				return status, nil
			}
			for _, process := range []struct {
				name  string
				state string
			}{{"compositor", status.Weston}, {"controller", status.Agent}} {
				if process.state == ProcessExited {
					return status, result.NewFailure(result.CodeSessionNotReady, "session.ready",
						"the session %s exited before the session became ready", process.name)
				}
			}
		} else if !isTransient(err) {
			return status, err
		}

		if time.Now().After(deadline) {
			failure := result.NewFailure(result.CodeWaitTimeout, "session.ready",
				"session %q did not become ready within %s", id, timeout)
			failure.Details = map[string]string{"session": id, "last_status": last.Message}
			return last, failure
		}

		select {
		case <-ctx.Done():
			return last, result.NewFailure(result.CodeWaitTimeout, "session.ready",
				"waiting for session %q was cancelled", id)
		case <-time.After(readyPollInterval):
		}
	}
}

// RunRequest runs an application in a session, creating the session first.
type RunRequest struct {
	Session   string
	Limits    engine.Limits
	Argv      []string
	Env       []string
	WorkDir   string
	Ephemeral bool
	Retention time.Duration
	Stdin     io.Reader
	Stdout    io.Writer
	Stderr    io.Writer
}

// Run creates, starts, and runs an application, then records how it exited.
//
// The session outlives the application: the compositor, the final framebuffer,
// and the logs stay available until an explicit close, unless the caller asked
// for an ephemeral session.
func (s *Service) Run(ctx context.Context, request RunRequest) (Record, error) {
	if len(request.Argv) == 0 {
		return Record{}, result.NewFailure(result.CodeUsageError, "session.run", "an application command is required")
	}
	if _, err := s.Create(ctx, CreateRequest{Session: request.Session, Limits: request.Limits, Retention: request.Retention}); err != nil {
		return Record{}, err
	}

	record, err := s.Start(ctx, request.Session)
	if err != nil {
		if request.Ephemeral {
			_, _ = s.Close(ctx, request.Session, s.stopTimeout)
		}
		return record, err
	}

	if err := record.Transition(StateRunning, s.now()); err != nil {
		return record, err
	}
	if err := s.store.Save(record); err != nil {
		return record, err
	}

	execution, execErr := s.engine.Exec(ctx, engine.ExecSpec{
		ContainerID: record.ContainerID,
		User:        s.applicationUID,
		WorkDir:     request.WorkDir,
		Env:         append(applicationEnv(), request.Env...),
		Argv:        request.Argv,
		Stdin:       request.Stdin,
		Stdout:      request.Stdout,
		Stderr:      request.Stderr,
	})

	// Whatever happened, the session is no longer running an application.
	record.Process = &ProcessExit{Code: execution.ExitCode, At: s.now(), Command: request.Argv}
	if err := record.Transition(StateReady, s.now()); err != nil {
		return record, err
	}
	if err := s.store.Save(record); err != nil {
		return record, err
	}

	if execErr != nil {
		if request.Ephemeral {
			_, _ = s.Close(ctx, request.Session, s.stopTimeout)
		}
		return record, execErr
	}

	if execution.ExitCode != 0 {
		failure := result.NewFailure(result.CodeProcessExited, "session.run",
			"the application exited with code %d", execution.ExitCode)
		failure.Details = map[string]string{"exit_code": fmt.Sprint(execution.ExitCode)}
		if request.Ephemeral {
			_, _ = s.Close(ctx, request.Session, s.stopTimeout)
		}
		return record, failure
	}

	if request.Ephemeral {
		return s.Close(ctx, request.Session, s.stopTimeout)
	}
	return record, nil
}

// List reconciles every stored session against the engine. Records are written
// back only when reconciliation changed them, so listing is read-mostly and
// listing twice is a no-op.
func (s *Service) List(ctx context.Context) ([]Record, []Anomaly, error) {
	records, err := s.store.List()
	if err != nil {
		return nil, nil, err
	}

	var anomalies []Anomaly
	for i := range records {
		observed, err := s.observe(ctx, records[i])
		if err != nil {
			return nil, nil, err
		}

		updated, found := Reconcile(records[i], observed, s.now())
		if !reflect.DeepEqual(updated, records[i]) {
			if err := s.store.Save(updated); err != nil {
				return nil, nil, err
			}
			records[i] = updated
		}
		for _, kind := range found {
			anomalies = append(anomalies, Anomaly{Session: records[i].Session, Kind: kind})
		}
	}
	return records, anomalies, nil
}

// Inspect returns one session, reconciled against the engine.
func (s *Service) Inspect(ctx context.Context, id string) (Record, []Anomaly, error) {
	record, err := s.store.Load(id)
	if err != nil {
		return Record{}, nil, err
	}

	observed, err := s.observe(ctx, record)
	if err != nil {
		return Record{}, nil, err
	}

	updated, found := Reconcile(record, observed, s.now())
	if !reflect.DeepEqual(updated, record) {
		if err := s.store.Save(updated); err != nil {
			return Record{}, nil, err
		}
	}

	anomalies := make([]Anomaly, 0, len(found))
	for _, kind := range found {
		anomalies = append(anomalies, Anomaly{Session: id, Kind: kind})
	}
	return updated, anomalies, nil
}

// Logs writes the container's log tail.
func (s *Service) Logs(ctx context.Context, id string, tail int, w io.Writer) error {
	record, err := s.store.Load(id)
	if err != nil {
		return err
	}
	if record.ContainerID == "" {
		return result.NewFailure(result.CodeSessionNotReady, "session.logs",
			"session %q never reached a container", id)
	}
	if tail <= 0 {
		tail = DefaultLogTail
	}
	return s.engine.Logs(ctx, record.ContainerID, tail, w)
}

// InjectRequest streams one payload into a session.
type InjectRequest struct {
	Session  string
	Kind     string
	Name     string
	Mode     uint32
	MaxBytes int64
	MaxFiles int
	Source   io.Reader
}

// Inject sends a binary or a bundle to the session's receiver and records what
// arrived, using the digest computed inside the container.
func (s *Service) Inject(ctx context.Context, request InjectRequest) (Payload, error) {
	record, err := s.store.Load(request.Session)
	if err != nil {
		return Payload{}, err
	}
	if !record.State.Interactive() {
		return Payload{}, result.NewFailure(result.CodeSessionNotReady, "session.inject",
			"session %q is %s", request.Session, record.State)
	}
	if request.Source == nil {
		return Payload{}, result.NewFailure(result.CodeUsageError, "session.inject", "a payload stream is required")
	}

	maxBytes := request.MaxBytes
	if maxBytes <= 0 {
		maxBytes = MaxPayloadBytes
	}
	maxFiles := request.MaxFiles
	if maxFiles <= 0 {
		maxFiles = MaxPayloadFiles
	}

	argv := []string{
		SupervisorPath, "receive",
		"--kind", request.Kind,
		"--name", request.Name,
		"--dir", PayloadDir,
		"--max-bytes", fmt.Sprint(maxBytes),
		"--max-files", fmt.Sprint(maxFiles),
	}
	if request.Mode != 0 {
		argv = append(argv, "--mode", fmt.Sprintf("0%o", request.Mode))
	}

	var stdout, stderr bytes.Buffer
	if err := s.engine.Stream(ctx, engine.StreamSpec{
		ContainerID: record.ContainerID,
		User:        s.controlUID,
		Argv:        argv,
		Source:      request.Source,
		Stdout:      &stdout,
		Stderr:      &stderr,
	}); err != nil {
		// The receiver reports a rejected payload as a failure document on its
		// standard error, so a refusal is reported as itself rather than as a
		// broken stream.
		if failure := decodeFailure(stderr.Bytes()); failure != nil {
			return Payload{}, failure
		}
		return Payload{}, err
	}

	var received PayloadResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &received); err != nil {
		return Payload{}, result.NewFailure(result.CodePayloadRejected, "session.inject",
			"the receiver did not report what it stored: %v", err)
	}

	payload := Payload{
		Digest:     received.Digest,
		Path:       received.Path,
		Bytes:      received.Bytes,
		ReceivedAt: s.now(),
	}
	record.Payloads = append(record.Payloads, payload)
	record.UpdatedAt = s.now()
	if err := s.store.Save(record); err != nil {
		return Payload{}, err
	}
	return payload, nil
}

// Close stops and removes a session's container and marks the record closed.
// It is idempotent: closing a closed session succeeds without touching the
// engine again.
func (s *Service) Close(ctx context.Context, id string, stopTimeout time.Duration) (Record, error) {
	record, err := s.store.Load(id)
	if err != nil {
		return Record{}, err
	}
	if record.State == StateClosed {
		return record, nil
	}
	if stopTimeout == 0 {
		stopTimeout = s.stopTimeout
	}

	// A session that was closing when a previous attempt stopped can be
	// retried; the state machine refuses closing -> closing.
	if record.State != StateClosing {
		if err := record.Transition(StateClosing, s.now()); err != nil {
			return record, err
		}
		if err := s.store.Save(record); err != nil {
			return record, err
		}
	}

	var failure *result.Failure
	if record.ContainerID != "" {
		if err := s.engine.Stop(ctx, record.ContainerID, stopTimeout); err != nil && !errors.Is(err, engine.ErrNotFound) {
			failure = asFailure("session.close", err)
		}
		if err := s.engine.Remove(ctx, record.ContainerID); err != nil && !errors.Is(err, engine.ErrNotFound) {
			failure = asFailure("session.close", err)
		}
	}

	if failure != nil {
		record.State = StateFailed
		record.UpdatedAt = s.now()
		if err := s.store.Save(record); err != nil {
			return record, err
		}
		return record, failure
	}

	record.State = StateClosed
	record.UpdatedAt = s.now()
	if err := s.store.Save(record); err != nil {
		return record, err
	}
	return record, nil
}

// Purge closes every session whose retention deadline has passed. It returns
// the identifiers it closed.
func (s *Service) Purge(ctx context.Context) ([]string, error) {
	records, err := s.store.List()
	if err != nil {
		return nil, err
	}

	var purged []string
	now := s.now()
	for _, record := range records {
		if !record.Expired(now) {
			continue
		}
		if _, err := s.Close(ctx, record.Session, s.stopTimeout); err != nil {
			return purged, err
		}
		purged = append(purged, record.Session)
	}
	return purged, nil
}

// containerStatus asks the supervisor inside the container what it sees.
func (s *Service) containerStatus(ctx context.Context, containerID string) (ContainerStatus, error) {
	if containerID == "" {
		return ContainerStatus{}, result.NewFailure(result.CodeSessionNotReady, "session.ready",
			"the session has no container yet")
	}

	var stdout bytes.Buffer
	_, err := s.engine.Exec(ctx, engine.ExecSpec{
		ContainerID: containerID,
		User:        s.controlUID,
		Argv:        []string{SupervisorPath, "status"},
		Stdout:      &stdout,
		Stderr:      io.Discard,
	})
	if err != nil {
		return ContainerStatus{}, err
	}

	var status ContainerStatus
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &status); err != nil {
		return ContainerStatus{}, result.NewFailure(result.CodeSessionNotReady, "session.ready",
			"the supervisor reported an unreadable status: %v", err)
	}
	return status, nil
}

// observe asks the engine what still exists for a session.
func (s *Service) observe(ctx context.Context, record Record) (Observed, error) {
	if record.ContainerID == "" {
		return Observed{}, nil
	}

	state, err := s.engine.State(ctx, record.ContainerID)
	switch {
	case errors.Is(err, engine.ErrNotFound):
		return Observed{}, nil
	case err != nil:
		return Observed{}, err
	}
	return Observed{Found: true, Running: state.Running}, nil
}

// sessionMounts is the fixed mount set of a session: a writable runtime
// directory, a scratch directory, and the application's home. Nothing of the
// host is mounted, and everything writable is bounded and memory-backed.
//
// The engine creates these mounts owned by root, so each one has to be writable
// by the session's own identities; the sticky bit keeps one identity from
// removing another's entries, and the control directory inside the runtime
// directory is what actually separates them.
func sessionMounts() []engine.Tmpfs {
	return []engine.Tmpfs{
		{Path: RuntimeDir, SizeBytes: 64 << 20, Mode: 0o1777, Exec: true},
		{Path: "/tmp", SizeBytes: 32 << 20, Mode: 0o1777},
		{Path: "/home/agent", SizeBytes: 32 << 20, Mode: 0o1777},
	}
}

// applicationEnv points an application at the compositor without telling it
// anything else about the session.
func applicationEnv() []string {
	return []string{
		EnvRuntimeDir + "=" + WaylandDir,
		EnvWaylandDisplay + "=" + WaylandDisplay,
	}
}

// isTransient reports whether a readiness probe failure may resolve by itself,
// which is the case while the container is still starting.
func isTransient(err error) bool {
	var failure *result.Failure
	if !errors.As(err, &failure) {
		return false
	}
	if failure.Code == result.CodeSessionNotReady && strings.Contains(failure.Message, "no container") {
		return true
	}
	return failure.Code == result.CodeEngineUnavailable
}

func asFailure(operation string, err error) *result.Failure {
	var failure *result.Failure
	if errors.As(err, &failure) {
		return failure
	}
	return result.NewFailure(result.CodeSessionNotReady, operation, "%v", err)
}

// decodeFailure reads the failure document an in-container command reported on
// its standard error. It returns nil when the output is not one.
func decodeFailure(payload []byte) *result.Failure {
	if len(bytes.TrimSpace(payload)) == 0 {
		return nil
	}

	var envelope result.Envelope[json.RawMessage]
	if err := json.Unmarshal(bytes.TrimSpace(payload), &envelope); err != nil {
		return nil
	}
	return envelope.Error
}

// mergeDegradations unions the gaps an engine reports with the limits a session
// asked for and the engine cannot enforce, keeping the first-seen order and
// reporting each gap once.
func mergeDegradations(groups ...[]string) []string {
	var merged []string
	for _, group := range groups {
		for _, item := range group {
			if !containsString(merged, item) {
				merged = append(merged, item)
			}
		}
	}
	return merged
}

// containsString reports whether values holds want.
func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
