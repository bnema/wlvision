package session

import (
	"strings"
	"testing"
	"time"

	"github.com/bnema/wlvision/internal/engine"
	"github.com/bnema/wlvision/internal/result"
)

var testTime = time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)

func testRecord(id string) Record {
	return Record{
		Schema:        result.Schema,
		Session:       id,
		ContainerName: ContainerName(id),
		State:         StateReady,
		Revision:      3,
		Engine:        EngineInfo{Kind: string(engine.KindDocker), Context: "default", ServerVersion: "29.7.2"},
		Image:         "wlvision-runtime:test",
		Weston:        "16.0.0",
		Limits:        engine.Limits{MemoryBytes: 1 << 30, Pids: 128, FileSizeBytes: 1 << 24, OpenFiles: 256},
		Degradations:  []string{engine.DegradationCgroupV1},
		CreatedAt:     testTime,
		UpdatedAt:     testTime,
	}
}

func TestStateTransitions(t *testing.T) {
	// Every state is listed, so a new state cannot be added without deciding
	// which transitions it accepts.
	allowed := map[State][]State{
		StateCreating: {StateReady, StateFailed, StateClosing},
		StateReady:    {StateRunning, StateFailed, StateClosing},
		StateRunning:  {StateReady, StateFailed, StateClosing},
		StateFailed:   {StateClosing, StateClosed},
		StateClosing:  {StateClosed, StateFailed},
		StateClosed:   {},
	}

	for from, destinations := range allowed {
		if !from.Valid() {
			t.Errorf("state %q is not recognized", from)
		}
		for _, to := range destinations {
			if !from.CanTransitionTo(to) {
				t.Errorf("%s -> %s is refused, want it allowed", from, to)
			}
		}
		for _, to := range []State{StateCreating, StateReady, StateRunning, StateFailed, StateClosing, StateClosed} {
			if from.CanTransitionTo(to) {
				continue
			}
			if contains(destinations, to) {
				t.Errorf("%s -> %s is allowed, want it refused", from, to)
			}
		}
	}

	if State("paused").Valid() {
		t.Error("an undefined state was accepted")
	}
	if !StateClosed.Terminal() {
		t.Error("closed is terminal")
	}
	if !StateReady.Interactive() || !StateRunning.Interactive() {
		t.Error("a ready or running session accepts commands")
	}
	if StateFailed.Interactive() || StateCreating.Interactive() || StateClosing.Interactive() || StateClosed.Interactive() {
		t.Error("a session that is not ready or running accepts commands")
	}
}

func contains(states []State, want State) bool {
	for _, state := range states {
		if state == want {
			return true
		}
	}
	return false
}

func TestRecordTransitionRefusesAnImpossibleMove(t *testing.T) {
	record := testRecord("demo")

	later := testTime.Add(time.Minute)
	if err := record.Transition(StateRunning, later); err != nil {
		t.Fatalf("ready -> running: %v", err)
	}
	if record.State != StateRunning || !record.UpdatedAt.Equal(later) {
		t.Errorf("record = %s updated %s, want running updated %s", record.State, record.UpdatedAt, later)
	}

	if err := record.Transition(StateCreating, later); err == nil {
		t.Error("running -> creating was accepted")
	}
	if record.State != StateRunning {
		t.Errorf("a refused transition changed the state to %s", record.State)
	}

	record.State = StateClosed
	if err := record.Transition(StateFailed, later); err == nil {
		t.Error("closed -> failed was accepted")
	}
}

func TestRecordValidation(t *testing.T) {
	if err := testRecord("demo").Validate(); err != nil {
		t.Fatalf("a complete record was refused: %v", err)
	}

	tests := map[string]func(*Record){
		"session identifier": func(r *Record) { r.Session = "../escape" },
		"container name":     func(r *Record) { r.ContainerName = "evil/name" },
		"state":              func(r *Record) { r.State = State("paused") },
		"schema":             func(r *Record) { r.Schema = "wlvision/v2" },
		"empty container":    func(r *Record) { r.ContainerName = "" },
	}
	for name, corrupt := range tests {
		record := testRecord("demo")
		corrupt(&record)
		if err := record.Validate(); err == nil {
			t.Errorf("a record with a bad %s was accepted", name)
		}
	}
}

func TestValidSessionID(t *testing.T) {
	valid := []string{"a", "demo", "demo-1", "session-2026-01-02", strings.Repeat("a", 63)}
	for _, id := range valid {
		if !ValidSessionID(id) {
			t.Errorf("ValidSessionID(%q) = false, want true", id)
		}
	}

	invalid := []string{"", "-demo", "Demo", "demo_1", "demo/1", "../escape", "..", "demo.1", strings.Repeat("a", 64)}
	for _, id := range invalid {
		if ValidSessionID(id) {
			t.Errorf("ValidSessionID(%q) = true, want false", id)
		}
	}
}

func TestReconcile(t *testing.T) {
	tests := []struct {
		name        string
		state       State
		observed    Observed
		wantState   State
		wantAnomaly string
	}{
		{"a running container matches a ready session", StateReady, Observed{Found: true, Running: true}, StateReady, ""},
		{"a running container matches a running session", StateRunning, Observed{Found: true, Running: true}, StateRunning, ""},
		{"a missing container fails the session", StateReady, Observed{}, StateFailed, AnomalyContainerMissing},
		{"a stopped container fails the session", StateRunning, Observed{Found: true}, StateFailed, AnomalyContainerStopped},
		{
			"an interrupted creation is a failure, not a usable session",
			StateCreating, Observed{Found: true, Running: true}, StateFailed, AnomalyCreationInterrupted,
		},
		{"a closing session is left alone", StateClosing, Observed{}, StateClosing, AnomalyContainerMissing},
		{"a failed session stays failed", StateFailed, Observed{}, StateFailed, AnomalyContainerMissing},
		{"a closed session with no container is clean", StateClosed, Observed{}, StateClosed, ""},
		{
			"a closed session keeps its leak visible",
			StateClosed, Observed{Found: true, Running: true}, StateClosed, AnomalyContainerRetained,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			record := testRecord("demo")
			record.State = test.state
			record.UpdatedAt = testTime

			got, anomalies := Reconcile(record, test.observed, testTime.Add(time.Hour))

			if got.State != test.wantState {
				t.Errorf("state = %s, want %s", got.State, test.wantState)
			}
			switch {
			case test.wantAnomaly == "" && len(anomalies) != 0:
				t.Errorf("anomalies = %v, want none", anomalies)
			case test.wantAnomaly != "" && (len(anomalies) != 1 || anomalies[0] != test.wantAnomaly):
				t.Errorf("anomalies = %v, want [%s]", anomalies, test.wantAnomaly)
			}
			if test.wantState == test.state && !got.UpdatedAt.Equal(testTime) {
				t.Errorf("UpdatedAt = %s, want it untouched", got.UpdatedAt)
			}
		})
	}
}

func TestRecordExpiry(t *testing.T) {
	record := testRecord("demo")
	record.RetentionDeadline = testTime.Add(time.Hour)

	if record.Expired(testTime) {
		t.Error("a retained session expired before its deadline")
	}
	if !record.Expired(testTime.Add(2 * time.Hour)) {
		t.Error("a retained session outlived its deadline")
	}

	record.State = StateClosed
	if record.Expired(testTime.Add(48 * time.Hour)) {
		t.Error("a closed session is not pending cleanup")
	}

	record.State = StateReady
	record.RetentionDeadline = time.Time{}
	if record.Expired(testTime.Add(48 * time.Hour)) {
		t.Error("a session without a deadline never expires")
	}
}
