package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bnema/wlvision/internal/engine"
	"github.com/bnema/wlvision/internal/enginetest"
	"github.com/bnema/wlvision/internal/result"
)

func newTestService(t *testing.T, fake *enginetest.Fake, store *Store) *Service {
	t.Helper()

	service, err := NewService(Options{
		Engine:    fake,
		Store:     store,
		Now:       func() time.Time { return testTime },
		Image:     "wlvision-runtime:test",
		Retention: time.Hour,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return service
}

func isStatusProbe(argv []string) bool {
	return len(argv) == 2 && argv[0] == SupervisorPath && argv[1] == "status"
}

func TestServiceCreateWritesTheRecordBeforeTheContainer(t *testing.T) {
	fake := enginetest.New()
	fake.CreateErr = errors.New("engine: no such image")

	store := newTestStore(t)
	service := newTestService(t, fake, store)

	if _, err := service.Create(context.Background(), CreateRequest{Session: "demo"}); err == nil {
		t.Fatal("Create without an image succeeded")
	}

	// The record must exist even though the container was never created:
	// that is what makes an interrupted creation discoverable.
	record, err := store.Load("demo")
	if err != nil {
		t.Fatalf("Load after a failed create: %v", err)
	}
	if record.State != StateFailed {
		t.Errorf("state = %s, want failed", record.State)
	}
	if record.ContainerID != "" {
		t.Errorf("ContainerID = %q, want empty", record.ContainerID)
	}
}

func TestServiceCreateRefusesASecondLiveSession(t *testing.T) {
	fake := enginetest.New()
	store := newTestStore(t)
	service := newTestService(t, fake, store)

	if _, err := service.Create(context.Background(), CreateRequest{Session: "demo"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err := service.Create(context.Background(), CreateRequest{Session: "demo"})
	if err == nil {
		t.Fatal("a duplicate session was accepted")
	}
	if failure := failureOfSession(t, err); failure.Code != result.CodeSessionNotReady {
		t.Errorf("code = %s, want %s", failure.Code, result.CodeSessionNotReady)
	}
	if calls := fake.CountCalls("create "); calls != 1 {
		t.Errorf("the engine created %d containers, want 1", calls)
	}
}

func TestServiceCreateRefusesAnInvalidSessionIdentifier(t *testing.T) {
	fake := enginetest.New()
	service := newTestService(t, fake, newTestStore(t))

	_, err := service.Create(context.Background(), CreateRequest{Session: "../../etc/passwd"})
	if failure := failureOfSession(t, err); failure.Code != result.CodeUsageError {
		t.Errorf("code = %s, want %s", failure.Code, result.CodeUsageError)
	}
	if len(fake.CallLog()) != 0 {
		t.Errorf("the engine was contacted for an invalid session: %v", fake.CallLog())
	}
}

func TestServiceCreateRecordsUndelegatedLimits(t *testing.T) {
	fake := enginetest.New()
	fake.CapabilitiesValue.CgroupVersion = "1"
	fake.CapabilitiesValue.MemoryLimit = false
	fake.CapabilitiesValue.PidsLimit = false

	store := newTestStore(t)
	service := newTestService(t, fake, store)

	record, err := service.Create(context.Background(), CreateRequest{
		Session: "demo",
		Limits:  engine.Limits{MemoryBytes: 1 << 30, Pids: 64, FileSizeBytes: 1 << 20, OpenFiles: 256},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	want := []string{engine.DegradationCgroupV1, engine.DegradationMemoryLimit, engine.DegradationPidsLimit}
	if strings.Join(record.Degradations, ",") != strings.Join(want, ",") {
		t.Errorf("Degradations = %v, want %v", record.Degradations, want)
	}
	if record.Engine.Kind != string(engine.KindDocker) || record.Engine.Context != "default" {
		t.Errorf("engine info = %+v, want the inspected engine", record.Engine)
	}
}

func TestServiceStartWaitsUntilTheSessionIsReady(t *testing.T) {
	fake := enginetest.New()
	store := newTestStore(t)
	service := newTestService(t, fake, store)

	// The first two probes see a session that is still coming up.
	probes := 0
	fake.ExecFunc = func(spec engine.ExecSpec) (engine.ExecResult, error) {
		if !isStatusProbe(spec.Argv) {
			return engine.ExecResult{}, nil
		}
		probes++
		status := ContainerStatus{Weston: ProcessRunning, Agent: ProcessNotStarted, Message: "starting"}
		if probes > 2 {
			status = ContainerStatus{Ready: true, Weston: ProcessRunning, Agent: ProcessRunning}
		}
		payload, _ := json.Marshal(status)
		_, err := spec.Stdout.Write(payload)
		return engine.ExecResult{}, err
	}

	if _, err := service.Create(context.Background(), CreateRequest{Session: "demo"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	record, err := service.Start(context.Background(), "demo")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if record.State != StateReady {
		t.Errorf("state = %s, want ready", record.State)
	}
	if probes < 3 {
		t.Errorf("readiness was probed %d times, want the session to be waited for", probes)
	}

	container, ok := fake.Container(record.ContainerID)
	if !ok || !container.Running {
		t.Error("the session container is not running")
	}
	if container.Spec.ControlUID == container.Spec.ApplicationUID {
		t.Error("the control and application identities are the same")
	}
}

func TestServiceStartFailsWhenTheCompositorExits(t *testing.T) {
	fake := enginetest.New()
	fake.Status = `{"ready":false,"weston":"exited","agent":"not-started","message":"weston: could not open the output"}`
	store := newTestStore(t)
	service := newTestService(t, fake, store)

	if _, err := service.Create(context.Background(), CreateRequest{Session: "demo"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err := service.Start(context.Background(), "demo")
	if err == nil {
		t.Fatal("a session whose compositor exited was reported as ready")
	}

	failure := failureOfSession(t, err)
	if failure.Code != result.CodeSessionNotReady {
		t.Errorf("code = %s, want %s", failure.Code, result.CodeSessionNotReady)
	}

	record, loadErr := store.Load("demo")
	if loadErr != nil {
		t.Fatalf("Load: %v", loadErr)
	}
	if record.State != StateFailed {
		t.Errorf("state = %s, want failed", record.State)
	}
}

func TestServiceWaitReadyTimesOut(t *testing.T) {
	fake := enginetest.New()
	fake.Status = `{"ready":false,"weston":"running","agent":"running","message":"starting"}`
	store := newTestStore(t)
	service := newTestService(t, fake, store)

	if _, err := service.Create(context.Background(), CreateRequest{Session: "demo"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err := service.WaitReady(context.Background(), "demo", time.Nanosecond)
	if err == nil {
		t.Fatal("a session that never became ready succeeded")
	}

	failure := failureOfSession(t, err)
	if failure.Code != result.CodeWaitTimeout {
		t.Errorf("code = %s, want %s", failure.Code, result.CodeWaitTimeout)
	}
	if failure.Code.ExitCode() != 5 {
		t.Errorf("exit code = %d, want 5", failure.Code.ExitCode())
	}
}

func TestServiceRunUsesTheApplicationIdentityAndRetainsTheSession(t *testing.T) {
	fake := enginetest.New()
	store := newTestStore(t)
	service := newTestService(t, fake, store)

	var application engine.ExecSpec
	fake.ExecFunc = func(spec engine.ExecSpec) (engine.ExecResult, error) {
		if isStatusProbe(spec.Argv) {
			payload, _ := json.Marshal(ContainerStatus{Ready: true, Weston: ProcessRunning, Agent: ProcessRunning})
			_, err := spec.Stdout.Write(payload)
			return engine.ExecResult{}, err
		}
		application = spec
		return engine.ExecResult{ExitCode: 0}, nil
	}

	record, err := service.Run(context.Background(), RunRequest{
		Session: "demo",
		Argv:    []string{"/run/wlvision/payload/app"},
		WorkDir: "/home/agent",
		Stdout:  &strings.Builder{},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if application.User != DefaultApplicationUID {
		t.Errorf("application uid = %d, want %d", application.User, DefaultApplicationUID)
	}
	if !containsString(application.Env, EnvRuntimeDir+"="+WaylandDir) {
		t.Errorf("application env = %v, want the compositor runtime directory", application.Env)
	}
	if !containsString(application.Env, EnvWaylandDisplay+"="+WaylandDisplay) {
		t.Errorf("application env = %v, want the compositor socket name", application.Env)
	}
	if application.ContainerID != record.ContainerID {
		t.Errorf("the application ran in %q, want %q", application.ContainerID, record.ContainerID)
	}

	// The session survives the application.
	if record.State != StateReady {
		t.Errorf("state = %s, want ready", record.State)
	}
	if record.Process == nil || record.Process.Code != 0 {
		t.Errorf("Process = %+v, want the recorded exit", record.Process)
	}
	if _, ok := fake.Container(record.ContainerID); !ok {
		t.Error("the session container was removed with the application")
	}
}

func TestServiceRunReportsANonZeroExitAndKeepsTheSession(t *testing.T) {
	fake := enginetest.New()
	store := newTestStore(t)
	service := newTestService(t, fake, store)

	fake.ExecFunc = func(spec engine.ExecSpec) (engine.ExecResult, error) {
		if isStatusProbe(spec.Argv) {
			payload, _ := json.Marshal(ContainerStatus{Ready: true, Weston: ProcessRunning, Agent: ProcessRunning})
			_, err := spec.Stdout.Write(payload)
			return engine.ExecResult{}, err
		}
		return engine.ExecResult{ExitCode: 3}, nil
	}

	record, err := service.Run(context.Background(), RunRequest{Session: "demo", Argv: []string{"/payload/app"}})
	if err == nil {
		t.Fatal("a failing application was reported as success")
	}

	failure := failureOfSession(t, err)
	if failure.Code != result.CodeProcessExited {
		t.Errorf("code = %s, want %s", failure.Code, result.CodeProcessExited)
	}
	if failure.Details["exit_code"] != "3" {
		t.Errorf("details = %v, want the application exit code", failure.Details)
	}
	if record.Process == nil || record.Process.Code != 3 {
		t.Errorf("Process = %+v, want exit code 3", record.Process)
	}
	if _, ok := fake.Container(record.ContainerID); !ok {
		t.Error("a failed application removed the session it ran in")
	}
}

func TestServiceRunEphemeralRemovesTheSession(t *testing.T) {
	fake := enginetest.New()
	store := newTestStore(t)
	service := newTestService(t, fake, store)

	fake.ExecFunc = func(spec engine.ExecSpec) (engine.ExecResult, error) {
		if isStatusProbe(spec.Argv) {
			payload, _ := json.Marshal(ContainerStatus{Ready: true, Weston: ProcessRunning, Agent: ProcessRunning})
			_, err := spec.Stdout.Write(payload)
			return engine.ExecResult{}, err
		}
		return engine.ExecResult{ExitCode: 0}, nil
	}

	record, err := service.Run(context.Background(), RunRequest{Session: "demo", Argv: []string{"/payload/app"}, Ephemeral: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if record.State != StateClosed {
		t.Errorf("state = %s, want closed", record.State)
	}
	if _, ok := fake.Container(record.ContainerID); ok {
		t.Error("an ephemeral session kept its container")
	}
}

func TestServiceCloseIsIdempotent(t *testing.T) {
	fake := enginetest.New()
	store := newTestStore(t)
	service := newTestService(t, fake, store)

	created, err := service.Create(context.Background(), CreateRequest{Session: "demo"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	record, err := service.Close(context.Background(), "demo", time.Second)
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if record.State != StateClosed {
		t.Errorf("state = %s, want closed", record.State)
	}
	if _, ok := fake.Container(created.ContainerID); ok {
		t.Error("the container survived close")
	}

	if _, err := service.Close(context.Background(), "demo", time.Second); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if stops := fake.CountCalls("stop "); stops != 1 {
		t.Errorf("the engine was asked to stop %d times, want 1", stops)
	}
}

func TestServiceCloseKeepsAFailureVisible(t *testing.T) {
	fake := enginetest.New()
	fake.RemoveErr = &result.Failure{Code: result.CodeEngineUnavailable, Message: "daemon is gone"}
	store := newTestStore(t)
	service := newTestService(t, fake, store)

	if _, err := service.Create(context.Background(), CreateRequest{Session: "demo"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	record, err := service.Close(context.Background(), "demo", time.Second)
	if err == nil {
		t.Fatal("a failed cleanup was reported as success")
	}
	if record.State != StateFailed {
		t.Errorf("state = %s, want failed so cleanup can be retried", record.State)
	}

	stored, loadErr := store.Load("demo")
	if loadErr != nil {
		t.Fatalf("Load: %v", loadErr)
	}
	if stored.State != StateFailed {
		t.Errorf("stored state = %s, want failed", stored.State)
	}
}

func TestServiceListReconcilesAndIsIdempotent(t *testing.T) {
	fake := enginetest.New()
	store := newTestStore(t)
	service := newTestService(t, fake, store)

	created, err := service.Create(context.Background(), CreateRequest{Session: "demo"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fake.Forget(created.ContainerID)

	records, anomalies, err := service.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(records) != 1 || records[0].State != StateFailed {
		t.Errorf("records = %+v, want one failed session", records)
	}
	if len(anomalies) != 1 || anomalies[0].Kind != AnomalyContainerMissing {
		t.Errorf("anomalies = %+v, want a missing container", anomalies)
	}

	first := readRecordFile(t, store, "demo")
	if _, _, err := service.List(context.Background()); err != nil {
		t.Fatalf("second List: %v", err)
	}
	if second := readRecordFile(t, store, "demo"); string(first) != string(second) {
		t.Error("listing sessions rewrote an already reconciled record")
	}
}

func TestServiceInjectRecordsWhatTheReceiverStored(t *testing.T) {
	fake := enginetest.New()
	store := newTestStore(t)
	service := newTestService(t, fake, store)

	created, err := service.Create(context.Background(), CreateRequest{Session: "demo"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := fake.Start(context.Background(), created.ContainerID); err != nil {
		t.Fatalf("Start: %v", err)
	}
	record, err := store.Load("demo")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	record.State = StateReady
	if err := store.Save(record); err != nil {
		t.Fatalf("Save: %v", err)
	}

	var receiver engine.StreamSpec
	fake.StreamFunc = func(spec engine.StreamSpec) error {
		receiver = spec
		payload, _ := json.Marshal(PayloadResult{Path: PayloadDir + "/app", Digest: "sha256:abc", Bytes: 3, Files: 1})
		_, err := spec.Stdout.Write(payload)
		return err
	}

	injected, err := service.Inject(context.Background(), InjectRequest{
		Session: "demo",
		Kind:    "binary",
		Name:    "app",
		Mode:    0o755,
		Source:  strings.NewReader("abc"),
	})
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if injected.Digest != "sha256:abc" || injected.Bytes != 3 {
		t.Errorf("payload = %+v, want the receiver's report", injected)
	}

	if receiver.User != DefaultControlUID {
		t.Errorf("receiver uid = %d, want the control identity", receiver.User)
	}
	if receiver.Argv[0] != SupervisorPath || receiver.Argv[1] != "receive" {
		t.Errorf("receiver argv = %v, want the supervisor's receive mode", receiver.Argv)
	}
	if !containsString(receiver.Argv, "--mode") {
		t.Errorf("receiver argv = %v, want the requested mode", receiver.Argv)
	}

	stored, err := store.Load("demo")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(stored.Payloads) != 1 || stored.Payloads[0].Digest != "sha256:abc" {
		t.Errorf("stored payloads = %+v, want the injected payload", stored.Payloads)
	}
}

func TestServiceInjectRefusesANonInteractiveSession(t *testing.T) {
	fake := enginetest.New()
	store := newTestStore(t)
	service := newTestService(t, fake, store)

	if _, err := service.Create(context.Background(), CreateRequest{Session: "demo"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err := service.Inject(context.Background(), InjectRequest{
		Session: "demo", Kind: "binary", Name: "app", Source: strings.NewReader("abc"),
	})
	if failure := failureOfSession(t, err); failure.Code != result.CodeSessionNotReady {
		t.Errorf("code = %s, want %s", failure.Code, result.CodeSessionNotReady)
	}
	if fake.CountCalls("stream ") != 0 {
		t.Error("a payload was streamed into a session that is not ready")
	}
}

func TestServiceLogsStreamsTheContainerLog(t *testing.T) {
	fake := enginetest.New()
	store := newTestStore(t)
	service := newTestService(t, fake, store)

	created, err := service.Create(context.Background(), CreateRequest{Session: "demo"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var logs strings.Builder
	if err := service.Logs(context.Background(), "demo", 50, &logs); err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if !strings.Contains(logs.String(), "wlvision-supervisor") {
		t.Errorf("logs = %q, want the container's output", logs.String())
	}
	if calls := fake.CountCalls("logs " + created.ContainerID); calls != 1 {
		t.Errorf("the engine was asked for logs %d times, want 1", calls)
	}
}

func TestServicePurgeClosesExpiredSessions(t *testing.T) {
	fake := enginetest.New()
	store := newTestStore(t)
	service := newTestService(t, fake, store)

	created, err := service.Create(context.Background(), CreateRequest{Session: "demo"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	record, err := store.Load("demo")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	record.RetentionDeadline = testTime.Add(-time.Minute)
	if err := store.Save(record); err != nil {
		t.Fatalf("Save: %v", err)
	}

	purged, err := service.Purge(context.Background())
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if len(purged) != 1 || purged[0] != "demo" {
		t.Errorf("purged = %v, want [demo]", purged)
	}
	if _, ok := fake.Container(created.ContainerID); ok {
		t.Error("an expired session kept its container")
	}

	stored, err := store.Load("demo")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if stored.State != StateClosed {
		t.Errorf("state = %s, want closed", stored.State)
	}
}

func TestNewServiceRefusesSharedIdentities(t *testing.T) {
	_, err := NewService(Options{
		Engine:         enginetest.New(),
		Store:          newTestStore(t),
		ControlUID:     1000,
		ApplicationUID: 1000,
	})
	if err == nil {
		t.Error("a service with one identity for control and applications was accepted")
	}
}

func failureOfSession(t *testing.T, err error) *result.Failure {
	t.Helper()

	if err == nil {
		t.Fatal("expected a failure, got none")
	}
	var failure *result.Failure
	if !errors.As(err, &failure) {
		t.Fatalf("error %v (%T) is not a *result.Failure", err, err)
	}
	return failure
}

func readRecordFile(t *testing.T, store *Store, id string) []byte {
	t.Helper()

	payload, err := os.ReadFile(filepath.Join(store.dir(), id+".json"))
	if err != nil {
		t.Fatalf("read the record of %s: %v", id, err)
	}
	return payload
}

// A payload the receiver refuses must reach the caller as a payload failure,
// not as a broken engine stream: the receiver reports it on its standard error.
func TestServiceInjectReportsTheReceiversRefusal(t *testing.T) {
	fake := enginetest.New()
	store := newTestStore(t)
	service := newTestService(t, fake, store)

	created, err := service.Create(context.Background(), CreateRequest{Session: "demo"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	record, err := store.Load("demo")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	record.State = StateReady
	record.ContainerID = created.ContainerID
	if err := store.Save(record); err != nil {
		t.Fatalf("Save: %v", err)
	}

	refusal := result.Fail[any]("payload.receive", "demo",
		result.NewFailure(result.CodePayloadRejected, "payload.receive", "the bundle contains a parent traversal"))
	fake.StreamFunc = func(spec engine.StreamSpec) error {
		if err := result.RenderJSON(spec.Stderr, refusal); err != nil {
			return err
		}
		return &result.Failure{Code: result.CodeEngineUnavailable, Message: "the exec returned a non-zero status"}
	}

	_, err = service.Inject(context.Background(), InjectRequest{
		Session: "demo", Kind: "tar", Name: "bundle", Source: strings.NewReader("payload"),
	})
	if err == nil {
		t.Fatal("a refused payload was accepted")
	}
	if failure := failureOfSession(t, err); failure.Code != result.CodePayloadRejected {
		t.Errorf("code = %s, want %s", failure.Code, result.CodePayloadRejected)
	}
}

// An agent sets a session up once and then runs applications in it, so a run
// must reuse the session it finds instead of trying to create it again.
func TestServiceRunReusesAReadySession(t *testing.T) {
	fake := enginetest.New()
	store := newTestStore(t)
	service := newTestService(t, fake, store)

	if _, err := service.Create(context.Background(), CreateRequest{Session: "demo"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := service.Start(context.Background(), "demo"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	record, err := service.Run(context.Background(), RunRequest{Session: "demo", Argv: []string{"/payload/app"}})
	if err != nil {
		t.Fatalf("Run in an existing session: %v", err)
	}
	if record.State != StateReady {
		t.Errorf("state = %s, want ready", record.State)
	}
	if record.Process == nil {
		t.Error("the run recorded no exit")
	}
	if created := fake.CountCalls("create "); created != 1 {
		t.Errorf("the engine created %d containers, want the session to be reused", created)
	}
}

func TestServiceRunRefusesASessionThatCannotRun(t *testing.T) {
	fake := enginetest.New()
	store := newTestStore(t)
	service := newTestService(t, fake, store)

	if _, err := service.Create(context.Background(), CreateRequest{Session: "demo"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	fake.Forget("container-1")
	if _, _, err := service.Inspect(context.Background(), "demo"); err != nil {
		t.Fatalf("Inspect: %v", err)
	}

	_, err := service.Run(context.Background(), RunRequest{Session: "demo", Argv: []string{"/payload/app"}})
	if failure := failureOfSession(t, err); failure.Code != result.CodeSessionNotReady {
		t.Errorf("code = %s, want %s", failure.Code, result.CodeSessionNotReady)
	}
	if fake.CountCalls("exec ") != 0 {
		t.Error("an application was started in a session that cannot run one")
	}
}
