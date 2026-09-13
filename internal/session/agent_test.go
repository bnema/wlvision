package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/bnema/wlvision/internal/agentapi"
	"github.com/bnema/wlvision/internal/engine"
	"github.com/bnema/wlvision/internal/enginetest"
	"github.com/bnema/wlvision/internal/result"
)

// readyService returns a service holding one interactive session, which is what
// every operation that reaches the resident controller requires.
func readyService(t *testing.T) (*Service, *enginetest.Fake) {
	t.Helper()

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
	return service, fake
}

func TestCallExchangesOneOperationAsTheControlIdentity(t *testing.T) {
	service, fake := readyService(t)

	var executed engine.ExecSpec
	fake.ExecFunc = func(spec engine.ExecSpec) (engine.ExecResult, error) {
		executed = spec
		reply, err := json.Marshal(agentapi.Reply{Revision: 7})
		if err != nil {
			return engine.ExecResult{}, err
		}
		_, err = spec.Stdout.Write(reply)
		return engine.ExecResult{}, err
	}

	reply, err := service.Call(context.Background(), "demo", agentapi.OpSnapshot, agentapi.Params{Handle: "app-1"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if reply.Revision != 7 {
		t.Errorf("revision = %d, want the controller's reply", reply.Revision)
	}

	if executed.User != DefaultControlUID {
		t.Errorf("caller uid = %d, want the control identity", executed.User)
	}
	want := []string{CallPath, "--socket", AgentSocket, "--params", `{"handle":"app-1"}`, agentapi.OpSnapshot}
	if strings.Join(executed.Argv, " ") != strings.Join(want, " ") {
		t.Errorf("caller argv = %v, want %v", executed.Argv, want)
	}
}

func TestCallReturnsTheControllersRefusal(t *testing.T) {
	service, fake := readyService(t)

	fake.ExecFunc = func(spec engine.ExecSpec) (engine.ExecResult, error) {
		envelope := result.Fail[any]("window.activate", "",
			result.NewFailure(result.CodeWindowNotFound, "window.activate", "no window %q", "app-9"))
		if err := result.RenderJSON(spec.Stderr, envelope); err != nil {
			return engine.ExecResult{}, err
		}
		return engine.ExecResult{ExitCode: 4}, nil
	}

	_, err := service.Call(context.Background(), "demo", agentapi.OpActivate, agentapi.Params{Handle: "app-9"})
	failure := failureOfSession(t, err)
	if failure.Code != result.CodeWindowNotFound {
		t.Errorf("code = %s, want %s", failure.Code, result.CodeWindowNotFound)
	}
	if failure.Session != "demo" {
		t.Errorf("session = %q, want the failed session", failure.Session)
	}
}

func TestCallReportsAnUnreadableReply(t *testing.T) {
	service, fake := readyService(t)

	fake.ExecFunc = func(spec engine.ExecSpec) (engine.ExecResult, error) {
		_, err := spec.Stdout.Write([]byte("not a reply"))
		return engine.ExecResult{}, err
	}

	_, err := service.Call(context.Background(), "demo", agentapi.OpSnapshot, agentapi.Params{})
	if failure := failureOfSession(t, err); failure.Code != result.CodeSessionNotReady {
		t.Errorf("code = %s, want %s", failure.Code, result.CodeSessionNotReady)
	}
}

func TestCallRefusesASessionThatCannotInteract(t *testing.T) {
	service, fake := readyService(t)

	store, err := NewStore(service.store.Root())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	record, err := store.Load("demo")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	record.State = StateClosed
	if err := store.Save(record); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if _, err := service.Call(context.Background(), "demo", agentapi.OpSnapshot, agentapi.Params{}); err == nil {
		t.Fatal("a closed session accepted an operation")
	}
	if fake.CountCalls("exec ") != 0 {
		t.Error("a closed session was reached in the container")
	}
}

func TestCallReportsACallerDeadlineAsATimeout(t *testing.T) {
	service, fake := readyService(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A deadline kills the container exec, which is what the engine reports as
	// a failed command. The failure the caller must see is the timeout, not an
	// unavailable engine.
	fake.ExecFunc = func(engine.ExecSpec) (engine.ExecResult, error) {
		cancel()
		return engine.ExecResult{}, errors.New("the container engine command failed: signal: killed")
	}

	_, err := service.Call(ctx, "demo", agentapi.OpResize, agentapi.Params{Width: 640, Height: 480})
	failure := failureOfSession(t, err)
	if failure.Code != result.CodeWaitTimeout {
		t.Errorf("code = %s, want %s", failure.Code, result.CodeWaitTimeout)
	}
	if failure.Operation != agentapi.OpResize {
		t.Errorf("operation = %q, want the operation that timed out", failure.Operation)
	}
}

func TestCallerBindsOneSession(t *testing.T) {
	service, fake := readyService(t)

	var executed engine.ExecSpec
	fake.ExecFunc = func(spec engine.ExecSpec) (engine.ExecResult, error) {
		executed = spec
		_, err := spec.Stdout.Write([]byte("{}"))
		return engine.ExecResult{}, err
	}

	caller := service.Caller("demo")
	if caller.Session() != "demo" {
		t.Errorf("session = %q, want the bound session", caller.Session())
	}
	if _, err := caller.Call(context.Background(), agentapi.OpSnapshot, agentapi.Params{}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if len(executed.Argv) == 0 || executed.Argv[len(executed.Argv)-1] != agentapi.OpSnapshot {
		t.Errorf("caller argv = %v, want the bound operation", executed.Argv)
	}
}

func TestFetchStreamsAStoredCapture(t *testing.T) {
	service, fake := readyService(t)

	var executed engine.ExecSpec
	fake.ExecFunc = func(spec engine.ExecSpec) (engine.ExecResult, error) {
		executed = spec
		_, err := spec.Stdout.Write([]byte("png-bytes"))
		return engine.ExecResult{}, err
	}

	var captured bytes.Buffer
	if err := service.Fetch(context.Background(), "demo", "frames/frame-0001.png", &captured); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if captured.String() != "png-bytes" {
		t.Errorf("fetch = %q, want the stored bytes", captured.String())
	}

	want := []string{"cat", ExportDir + "/frames/frame-0001.png"}
	if strings.Join(executed.Argv, " ") != strings.Join(want, " ") {
		t.Errorf("fetch argv = %v, want %v", executed.Argv, want)
	}
	if executed.User != DefaultControlUID {
		t.Errorf("fetch uid = %d, want the control identity", executed.User)
	}
}

func TestFetchRefusesANameOutsideTheExportDirectory(t *testing.T) {
	service, fake := readyService(t)

	fake.ExecFunc = func(engine.ExecSpec) (engine.ExecResult, error) {
		t.Error("a refused name reached the container")
		return engine.ExecResult{}, nil
	}

	for _, name := range []string{
		"", "  ", "/etc/passwd", "../record.json", "frames/../../etc/passwd",
		"..", "-c", "frames/../../../",
	} {
		var captured bytes.Buffer
		err := service.Fetch(context.Background(), "demo", name, &captured)
		if err == nil {
			t.Errorf("the name %q was accepted", name)
		}
		if failure := failureOfSession(t, err); failure.Code != result.CodeUsageError {
			t.Errorf("the name %q reported %s, want %s", name, failure.Code, result.CodeUsageError)
		}
	}
}

func TestFetchNormalizesANestedName(t *testing.T) {
	service, fake := readyService(t)

	var executed engine.ExecSpec
	fake.ExecFunc = func(spec engine.ExecSpec) (engine.ExecResult, error) {
		executed = spec
		return engine.ExecResult{}, nil
	}

	var captured bytes.Buffer
	if err := service.Fetch(context.Background(), "demo", "frames/./frame-0001.png", &captured); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if want := ExportDir + "/frames/frame-0001.png"; executed.Argv[1] != want {
		t.Errorf("fetch path = %q, want %q", executed.Argv[1], want)
	}
}

func TestFetchReportsAMissingCapture(t *testing.T) {
	service, fake := readyService(t)

	fake.ExecFunc = func(spec engine.ExecSpec) (engine.ExecResult, error) {
		if _, err := spec.Stderr.Write([]byte("cat: no such file\n")); err != nil {
			return engine.ExecResult{}, err
		}
		return engine.ExecResult{ExitCode: 1}, nil
	}

	var captured bytes.Buffer
	err := service.Fetch(context.Background(), "demo", "missing.png", &captured)
	if failure := failureOfSession(t, err); failure.Code != result.CodeCaptureFailed {
		t.Errorf("code = %s, want %s", failure.Code, result.CodeCaptureFailed)
	}
}

func TestFetchRefusesASessionThatCannotInteract(t *testing.T) {
	fake := enginetest.New()
	store := newTestStore(t)
	service := newTestService(t, fake, store)

	if _, err := service.Create(context.Background(), CreateRequest{Session: "demo"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	var captured bytes.Buffer
	err := service.Fetch(context.Background(), "demo", "shot.png", &captured)
	if failure := failureOfSession(t, err); failure.Code != result.CodeSessionNotReady {
		t.Errorf("code = %s, want %s", failure.Code, result.CodeSessionNotReady)
	}
	if fake.CountCalls("exec ") != 0 {
		t.Error("a session that is not ready was reached in the container")
	}
}

func TestBoundedWriterStopsAnOversizedStream(t *testing.T) {
	var captured bytes.Buffer
	writer := &boundedWriter{w: &captured, remaining: 4}

	if _, err := writer.Write([]byte("1234")); err != nil {
		t.Fatalf("write within the bound: %v", err)
	}
	if _, err := writer.Write([]byte("5")); !errors.Is(err, errBoundExceeded) {
		t.Fatalf("write past the bound = %v, want %v", err, errBoundExceeded)
	}
	if !writer.exceeded {
		t.Error("the writer did not record the refusal")
	}
	if captured.String() != "1234" {
		t.Errorf("captured = %q, want only what fit the bound", captured.String())
	}
}
