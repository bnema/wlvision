package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/bnema/wlvision/internal/engine"
	"github.com/bnema/wlvision/internal/enginetest"
	"github.com/bnema/wlvision/internal/result"
	"github.com/bnema/wlvision/internal/session"
)

// harness wires run to an in-process engine and a store under a temporary
// state root, so no test touches a daemon, an image, or the real state.
type harness struct {
	fake    *enginetest.Fake
	store   *session.Store
	factory ServiceFactory
}

func newHarness(t *testing.T, fake *enginetest.Fake) *harness {
	t.Helper()
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("session.NewStore: %v", err)
	}
	return &harness{
		fake:  fake,
		store: store,
		factory: func(options session.Options) (*session.Service, error) {
			options.Engine = fake
			options.Store = store
			return session.NewService(options)
		},
	}
}

func (h *harness) run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	return h.runWithInput(t, "", args...)
}

func (h *harness) runWithInput(t *testing.T, input string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), args, strings.NewReader(input), &stdout, &stderr, h.factory)
	return code, stdout.String(), stderr.String()
}

// testEnvelope is the subset of the wlvision/v1 document the CLI tests read.
type testEnvelope struct {
	Schema    string          `json:"schema"`
	Ok        bool            `json:"ok"`
	Operation string          `json:"operation"`
	Session   string          `json:"session"`
	Result    json.RawMessage `json:"result"`
	Error     *testFailure    `json:"error"`
}

type testFailure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// decodeEnvelope proves stdout holds exactly one JSON document and returns it.
func decodeEnvelope(t *testing.T, document string) testEnvelope {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(document))
	var envelope testEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		t.Fatalf("stdout does not hold one JSON envelope: %v\noutput:\n%s", err, document)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatalf("stdout holds more than one JSON document: %v\noutput:\n%s", err, document)
	}
	return envelope
}

// statusExecFunc answers readiness probes and records the application argv.
func statusExecFunc(exitCode int, recorded *[]string) func(engine.ExecSpec) (engine.ExecResult, error) {
	return func(spec engine.ExecSpec) (engine.ExecResult, error) {
		if isStatusProbe(spec.Argv) {
			if _, err := io.WriteString(spec.Stdout, enginetest.ReadyStatus); err != nil {
				return engine.ExecResult{}, err
			}
			return engine.ExecResult{}, nil
		}
		if recorded != nil {
			*recorded = append([]string(nil), spec.Argv...)
		}
		return engine.ExecResult{ExitCode: exitCode}, nil
	}
}

func isStatusProbe(argv []string) bool {
	return len(argv) == 2 && argv[0] == session.SupervisorPath && argv[1] == "status"
}

func TestDoctorJSONEnvelope(t *testing.T) {
	fake := enginetest.New()
	h := newHarness(t, fake)

	if code, _, stderr := h.run(t, "--json", "session", "create", "--session", "demo", "--wait"); code != 0 {
		t.Fatalf("session create --wait exit = %d, stderr:\n%s", code, stderr)
	}

	code, stdout, stderr := h.run(t, "--json", "doctor")
	if code != 0 {
		t.Fatalf("doctor exit = %d, stderr:\n%s", code, stderr)
	}
	if stderr != "" {
		t.Errorf("doctor wrote to stderr: %q", stderr)
	}
	envelope := decodeEnvelope(t, stdout)
	if envelope.Schema != result.Schema {
		t.Errorf("schema = %q, want %q", envelope.Schema, result.Schema)
	}
	if !envelope.Ok {
		t.Errorf("doctor reported a failure: %+v", envelope.Error)
	}
	if envelope.Operation != "doctor" {
		t.Errorf("operation = %q, want doctor", envelope.Operation)
	}
	if envelope.Session != "" {
		t.Errorf("doctor session = %q, want empty", envelope.Session)
	}
	if !strings.Contains(string(envelope.Result), `"state": "ready"`) {
		t.Errorf("doctor result does not report the ready session:\n%s", envelope.Result)
	}
}

func TestDoctorRefusingEngine(t *testing.T) {
	fake := enginetest.New()
	fake.CapabilitiesErr = result.NewFailure(result.CodeEngineNotRootless, "engine.capabilities",
		"the container engine runs as root")
	h := newHarness(t, fake)

	code, stdout, _ := h.run(t, "--json", "doctor")
	if code != 3 {
		t.Fatalf("doctor exit = %d, want 3", code)
	}
	envelope := decodeEnvelope(t, stdout)
	if envelope.Ok {
		t.Fatal("doctor reported success against a refusing engine")
	}
	if envelope.Error == nil || envelope.Error.Code != string(result.CodeEngineNotRootless) {
		t.Fatalf("error = %+v, want code %s", envelope.Error, result.CodeEngineNotRootless)
	}
}

func TestSessionCreateWaits(t *testing.T) {
	fake := enginetest.New()
	h := newHarness(t, fake)

	code, stdout, stderr := h.run(t, "--json", "session", "create", "--session", "demo", "--wait")
	if code != 0 {
		t.Fatalf("exit = %d, stderr:\n%s", code, stderr)
	}
	envelope := decodeEnvelope(t, stdout)
	if !envelope.Ok || envelope.Session != "demo" {
		t.Fatalf("envelope = %+v", envelope)
	}
	record, err := h.store.Load("demo")
	if err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	if record.State != session.StateReady {
		t.Errorf("state = %s, want ready", record.State)
	}
}

func TestSessionCreateWithoutWaitStaysCreating(t *testing.T) {
	fake := enginetest.New()
	h := newHarness(t, fake)

	code, stdout, stderr := h.run(t, "--json", "session", "create", "--session", "demo")
	if code != 0 {
		t.Fatalf("exit = %d, stderr:\n%s", code, stderr)
	}
	envelope := decodeEnvelope(t, stdout)
	if !envelope.Ok {
		t.Fatalf("envelope = %+v", envelope)
	}
	if !strings.Contains(string(envelope.Result), "creating") {
		t.Errorf("result does not say the session is creating:\n%s", envelope.Result)
	}
	record, err := h.store.Load("demo")
	if err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	if record.State != session.StateCreating {
		t.Errorf("state = %s, want creating", record.State)
	}
}

func TestSessionCreateInvalidNameIsUsageError(t *testing.T) {
	fake := enginetest.New()
	h := newHarness(t, fake)

	code, stdout, _ := h.run(t, "--json", "session", "create", "--session", "bad/name")
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	envelope := decodeEnvelope(t, stdout)
	if envelope.Ok || envelope.Error == nil || envelope.Error.Code != string(result.CodeUsageError) {
		t.Fatalf("envelope = %+v", envelope)
	}
	if calls := fake.CallLog(); len(calls) != 0 {
		t.Errorf("the engine was touched: %v", calls)
	}
}

func TestUsageErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"unknown command", []string{"frobnicate"}},
		{"unknown flag", []string{"session", "list", "--bogus"}},
		{"session without subcommand", []string{"session"}},
		{"create without session", []string{"session", "create"}},
		{"inspect without session", []string{"session", "inspect"}},
		{"close without session", []string{"session", "close"}},
		{"run without session", []string{"run", "--", "app"}},
		{"run without application", []string{"run", "--session", "demo"}},
		{"inject without session", []string{"inject", "--binary", "app"}},
		{"inject without payload", []string{"inject", "--session", "demo"}},
		{"inject with two payloads", []string{"inject", "--session", "demo", "--binary", "app", "--bundle", "b"}},
		{"logs without session", []string{"logs"}},
		{"doctor with an argument", []string{"doctor", "extra"}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fake := enginetest.New()
			h := newHarness(t, fake)
			args := append([]string{"--json"}, testCase.args...)
			code, stdout, _ := h.run(t, args...)
			if code != 2 {
				t.Fatalf("exit = %d, want 2", code)
			}
			envelope := decodeEnvelope(t, stdout)
			if envelope.Ok || envelope.Error == nil || envelope.Error.Code != string(result.CodeUsageError) {
				t.Fatalf("envelope = %+v", envelope)
			}
			if calls := fake.CallLog(); len(calls) != 0 {
				t.Errorf("a usage error touched the engine: %v", calls)
			}
		})
	}
}

func TestSessionListReportsAnomalies(t *testing.T) {
	fake := enginetest.New()
	h := newHarness(t, fake)

	if code, _, stderr := h.run(t, "--json", "session", "create", "--session", "demo", "--wait"); code != 0 {
		t.Fatalf("create exit = %d, stderr:\n%s", code, stderr)
	}
	record, err := h.store.Load("demo")
	if err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	fake.Forget(record.ContainerID)

	code, stdout, stderr := h.run(t, "--json", "session", "list")
	if code != 0 {
		t.Fatalf("list exit = %d, stderr:\n%s", code, stderr)
	}
	envelope := decodeEnvelope(t, stdout)
	if !envelope.Ok {
		t.Fatalf("envelope = %+v", envelope)
	}
	var view struct {
		Sessions  []session.Record  `json:"sessions"`
		Anomalies []session.Anomaly `json:"anomalies"`
	}
	if err := json.Unmarshal(envelope.Result, &view); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if len(view.Sessions) != 1 || view.Sessions[0].State != session.StateFailed {
		t.Errorf("sessions = %+v", view.Sessions)
	}
	if len(view.Anomalies) != 1 || view.Anomalies[0].Kind != session.AnomalyContainerMissing {
		t.Errorf("anomalies = %+v, want one %s", view.Anomalies, session.AnomalyContainerMissing)
	}
}

func TestSessionInspectUnknownSession(t *testing.T) {
	fake := enginetest.New()
	h := newHarness(t, fake)

	code, stdout, _ := h.run(t, "--json", "session", "inspect", "--session", "ghost")
	if code != 4 {
		t.Fatalf("exit = %d, want 4", code)
	}
	envelope := decodeEnvelope(t, stdout)
	if envelope.Ok || envelope.Error == nil || envelope.Error.Code != string(result.CodeSessionNotReady) {
		t.Fatalf("envelope = %+v", envelope)
	}
}

func TestRunProcessExitIsFailureAndSessionSurvives(t *testing.T) {
	fake := enginetest.New()
	fake.ExecFunc = statusExecFunc(3, nil)
	h := newHarness(t, fake)

	code, stdout, _ := h.run(t, "--json", "run", "--session", "demo", "--", "/payload/app")
	if code != 4 {
		t.Fatalf("exit = %d, want 4", code)
	}
	envelope := decodeEnvelope(t, stdout)
	if envelope.Ok || envelope.Error == nil || envelope.Error.Code != string(result.CodeProcessExited) {
		t.Fatalf("envelope = %+v", envelope)
	}
	record, err := h.store.Load("demo")
	if err != nil {
		t.Fatalf("the session record did not survive: %v", err)
	}
	if record.State != session.StateReady {
		t.Errorf("state = %s, want ready", record.State)
	}
	if record.Process == nil || record.Process.Code != 3 {
		t.Errorf("process = %+v, want exit code 3", record.Process)
	}
}

func TestRunEphemeralClosesSession(t *testing.T) {
	fake := enginetest.New()
	fake.ExecFunc = statusExecFunc(0, nil)
	h := newHarness(t, fake)

	code, stdout, stderr := h.run(t, "--json", "run", "--session", "demo", "--ephemeral", "--", "/payload/app")
	if code != 0 {
		t.Fatalf("exit = %d, stderr:\n%s", code, stderr)
	}
	envelope := decodeEnvelope(t, stdout)
	if !envelope.Ok {
		t.Fatalf("envelope = %+v", envelope)
	}
	record, err := h.store.Load("demo")
	if err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	if record.State != session.StateClosed {
		t.Errorf("state = %s, want closed", record.State)
	}
}

func TestInjectStreamsPayloadIntoRecord(t *testing.T) {
	fake := enginetest.New()
	h := newHarness(t, fake)

	if code, _, stderr := h.run(t, "--json", "session", "create", "--session", "demo", "--wait"); code != 0 {
		t.Fatalf("create exit = %d, stderr:\n%s", code, stderr)
	}
	fake.StreamFunc = func(spec engine.StreamSpec) error {
		body, err := json.Marshal(session.PayloadResult{
			Path:   session.PayloadDir + "/app",
			Digest: "sha256:deadbeef",
			Bytes:  5,
			Files:  1,
		})
		if err != nil {
			return err
		}
		_, err = spec.Stdout.Write(body)
		return err
	}

	code, stdout, stderr := h.runWithInput(t, "hello", "--json", "inject", "--session", "demo", "--binary", "app")
	if code != 0 {
		t.Fatalf("exit = %d, stderr:\n%s", code, stderr)
	}
	envelope := decodeEnvelope(t, stdout)
	if !envelope.Ok {
		t.Fatalf("envelope = %+v", envelope)
	}
	var payload session.Payload
	if err := json.Unmarshal(envelope.Result, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.Digest != "sha256:deadbeef" || payload.Path != session.PayloadDir+"/app" {
		t.Errorf("payload = %+v", payload)
	}
	record, err := h.store.Load("demo")
	if err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	if len(record.Payloads) != 1 || record.Payloads[0].Digest != "sha256:deadbeef" {
		t.Errorf("record payloads = %+v", record.Payloads)
	}
	calls := strings.Join(fake.CallLog(), "\n")
	if !strings.Contains(calls, "--kind binary") || !strings.Contains(calls, "--name app") {
		t.Errorf("the receiver was not asked for a binary payload:\n%s", calls)
	}
}

func TestLogsWritesContainerLog(t *testing.T) {
	fake := enginetest.New()
	h := newHarness(t, fake)

	if code, _, stderr := h.run(t, "--json", "session", "create", "--session", "demo", "--wait"); code != 0 {
		t.Fatalf("create exit = %d, stderr:\n%s", code, stderr)
	}

	code, stdout, stderr := h.run(t, "logs", "--session", "demo", "--tail", "10")
	if code != 0 {
		t.Fatalf("exit = %d, stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "wlvision-supervisor: starting") {
		t.Errorf("stdout does not hold the container log:\n%s", stdout)
	}
	if calls := fake.CountCalls("logs"); calls != 1 {
		t.Errorf("log requests = %d, want 1", calls)
	}
}

func TestHumanMode(t *testing.T) {
	t.Run("success on stdout", func(t *testing.T) {
		fake := enginetest.New()
		h := newHarness(t, fake)
		code, stdout, stderr := h.run(t, "doctor")
		if code != 0 {
			t.Fatalf("exit = %d, stderr:\n%s", code, stderr)
		}
		if !strings.HasPrefix(stdout, "ok: doctor\n") {
			t.Errorf("stdout = %q", stdout)
		}
		if strings.Contains(stderr, "ok: doctor") {
			t.Errorf("the success line leaked to stderr: %q", stderr)
		}
	})

	t.Run("failure on stderr", func(t *testing.T) {
		fake := enginetest.New()
		h := newHarness(t, fake)
		code, stdout, stderr := h.run(t, "session", "inspect", "--session", "ghost")
		if code != 4 {
			t.Fatalf("exit = %d, want 4", code)
		}
		if stdout != "" {
			t.Errorf("failure wrote to stdout: %q", stdout)
		}
		if !strings.Contains(stderr, "error: session_not_ready:") {
			t.Errorf("stderr = %q", stderr)
		}
	})

	t.Run("usage text on stderr only", func(t *testing.T) {
		fake := enginetest.New()
		h := newHarness(t, fake)
		code, stdout, stderr := h.run(t, "inspect")
		if code != 2 {
			t.Fatalf("exit = %d, want 2", code)
		}
		if stdout != "" {
			t.Errorf("usage error wrote to stdout: %q", stdout)
		}
		if !strings.Contains(stderr, "Usage: wlvision") {
			t.Errorf("stderr does not hold the usage text: %q", stderr)
		}
	})
}

func TestRunDoubleDashProtectsApplicationFlags(t *testing.T) {
	fake := enginetest.New()
	var argv []string
	fake.ExecFunc = statusExecFunc(0, &argv)
	h := newHarness(t, fake)

	code, stdout, stderr := h.run(t, "run", "--session", "demo", "--", "app", "--json", "-x")
	if code != 0 {
		t.Fatalf("exit = %d, stderr:\n%s", code, stderr)
	}
	if got := strings.Join(argv, " "); got != "app --json -x" {
		t.Errorf("application argv = %q, want %q", got, "app --json -x")
	}
	if !strings.Contains(stdout, "ok: run") {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestParseSize(t *testing.T) {
	cases := []struct {
		text    string
		want    int64
		wantErr bool
	}{
		{"0", 0, false},
		{"1024", 1024, false},
		{"2k", 2 << 10, false},
		{"3m", 3 << 20, false},
		{"1G", 1 << 30, false},
		{"10K", 10 << 10, false},
		{"", 0, true},
		{"-1", 0, true},
		{"1x", 0, true},
		{"k", 0, true},
	}
	for _, testCase := range cases {
		got, err := parseSize(testCase.text)
		if testCase.wantErr {
			if err == nil {
				t.Errorf("parseSize(%q) = %d, want an error", testCase.text, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSize(%q): %v", testCase.text, err)
			continue
		}
		if got != testCase.want {
			t.Errorf("parseSize(%q) = %d, want %d", testCase.text, got, testCase.want)
		}
	}
}
