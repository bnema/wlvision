// Package session owns the lifecycle of a wlvision session: the states it may
// pass through, the durable record that survives a crash of the outer CLI, and
// the reconciliation of that record against what the engine actually runs.
package session

import (
	"path"
	"regexp"
	"time"

	"github.com/bnema/wlvision/internal/engine"
	"github.com/bnema/wlvision/internal/result"
)

// State is the lifecycle position of one session.
type State string

// The session states. A session reaches ready only once the compositor,
// control plane, capture protocol, and seat are all available.
const (
	// StateCreating is the window between creating the container and the
	// supervisor reporting readiness. A crash here leaves a discoverable
	// record instead of an orphaned container.
	StateCreating State = "creating"
	// StateReady means the session accepts commands but runs no application.
	StateReady State = "ready"
	// StateRunning means an application process is alive in the session.
	StateRunning State = "running"
	// StateFailed means the compositor, the controller, or the container is
	// gone. Observation and cleanup stay available; interaction does not.
	StateFailed State = "failed"
	// StateClosing means cleanup started and is not finished.
	StateClosing State = "closing"
	// StateClosed is terminal.
	StateClosed State = "closed"
)

// transitions is the complete set of accepted state changes. Anything absent
// is refused, so a service cannot move a session into a state its own history
// contradicts.
var transitions = map[State][]State{
	StateCreating: {StateReady, StateFailed, StateClosing},
	StateReady:    {StateRunning, StateFailed, StateClosing},
	StateRunning:  {StateReady, StateFailed, StateClosing},
	StateFailed:   {StateClosing, StateClosed},
	StateClosing:  {StateClosed, StateFailed},
	StateClosed:   {},
}

// Valid reports whether state is one of the defined states.
func (s State) Valid() bool {
	_, ok := transitions[s]
	return ok
}

// CanTransitionTo reports whether moving from s to next is allowed.
func (s State) CanTransitionTo(next State) bool {
	for _, candidate := range transitions[s] {
		if candidate == next {
			return true
		}
	}
	return false
}

// Terminal reports whether no further transition is possible.
func (s State) Terminal() bool { return s == StateClosed }

// Interactive reports whether the session can accept window, input, or capture
// commands in this state.
func (s State) Interactive() bool { return s == StateReady || s == StateRunning }

// EngineInfo records which engine, context, and version created a session, so
// a later command never has to guess which daemon to ask.
type EngineInfo struct {
	Kind          string `json:"kind"`
	Context       string `json:"context"`
	ServerVersion string `json:"server_version"`
}

// ProcessExit records why an application process ended. The session survives
// it: the compositor, the final framebuffer, and the logs stay available.
type ProcessExit struct {
	Code    int       `json:"code"`
	At      time.Time `json:"at"`
	Command []string  `json:"command,omitempty"`
}

// Payload records one stream wlvision received into the session.
type Payload struct {
	Digest     string    `json:"digest"`
	Path       string    `json:"path"`
	Bytes      int64     `json:"bytes"`
	ReceivedAt time.Time `json:"received_at"`
}

// Record is the durable description of one session. It is written before the
// container is started and updated at every transition, so a crash of the
// outer CLI can never orphan a container without a record.
type Record struct {
	Schema        string        `json:"schema"`
	Session       string        `json:"session"`
	ContainerID   string        `json:"container_id,omitempty"`
	ContainerName string        `json:"container_name"`
	State         State         `json:"state"`
	Revision      uint64        `json:"revision"`
	Engine        EngineInfo    `json:"engine"`
	Image         string        `json:"image"`
	Weston        string        `json:"weston_revision"`
	Limits        engine.Limits `json:"limits"`
	Degradations  []string      `json:"degradations,omitempty"`
	Payloads      []Payload     `json:"payloads,omitempty"`
	Process       *ProcessExit  `json:"process,omitempty"`
	CreatedAt     time.Time     `json:"created_at"`
	UpdatedAt     time.Time     `json:"updated_at"`
	// RetentionDeadline is when a retained session becomes eligible for
	// automatic cleanup. Retained failures stay observable until then.
	RetentionDeadline time.Time `json:"retention_deadline"`
}

// sessionIDPattern keeps a session identifier usable as a file name and as a
// container name suffix. It is the only place a caller-supplied string reaches
// the filesystem, so it is validated rather than sanitized.
var sessionIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ValidSessionID reports whether id is an acceptable session identifier.
func ValidSessionID(id string) bool { return sessionIDPattern.MatchString(id) }

// ContainerName is the container name derived from a session identifier.
func ContainerName(id string) string { return "wlvision-" + id }

// Validate reports whether a record is complete enough to persist.
func (r Record) Validate() error {
	if !ValidSessionID(r.Session) {
		return usageFailure("session.record", "invalid session identifier %q", r.Session)
	}
	if r.ContainerName == "" || path.Base(r.ContainerName) != r.ContainerName {
		return usageFailure("session.record", "invalid container name %q", r.ContainerName)
	}
	if !r.State.Valid() {
		return usageFailure("session.record", "unknown session state %q", r.State)
	}
	if r.Schema != result.Schema {
		return usageFailure("session.record", "record schema %q is not %q", r.Schema, result.Schema)
	}
	return nil
}

// Transition moves the record to next, stamping the update time. It refuses a
// change the state machine does not allow.
func (r *Record) Transition(next State, now time.Time) error {
	if !next.Valid() {
		return usageFailure("session.transition", "unknown session state %q", next)
	}
	if !r.State.CanTransitionTo(next) {
		return usageFailure("session.transition", "cannot move session %q from %s to %s", r.Session, r.State, next)
	}
	r.State = next
	r.UpdatedAt = now
	return nil
}

// Expired reports whether a retained session has outlived its retention
// deadline. A closed record is already gone from the engine's point of view.
func (r Record) Expired(now time.Time) bool {
	if r.State == StateClosed || r.RetentionDeadline.IsZero() {
		return false
	}
	return now.After(r.RetentionDeadline)
}

// Anomalies reported by reconciliation. They are stable identifiers so the CLI
// can render them and an agent can act on them.
const (
	// AnomalyContainerMissing: the record describes a live session, but the
	// engine no longer has the container.
	AnomalyContainerMissing = "container_missing"
	// AnomalyCreationInterrupted: the outer CLI died between creating the
	// container and the session becoming ready, so the container exists but
	// nothing is tracking it.
	AnomalyCreationInterrupted = "creation_interrupted"
	// AnomalyContainerRetained: a closed session still has its container.
	AnomalyContainerRetained = "container_retained_after_close"
	// AnomalyContainerStopped: the container is still registered but no longer
	// runs, so the compositor is gone and the session cannot be used.
	AnomalyContainerStopped = "container_stopped"
)

// Observed is what the engine currently reports for one session container: the
// container is either gone, present and stopped, or present and running.
type Observed struct {
	Found   bool
	Running bool
}

// Reconcile returns the record as the store should now hold it, together with
// the anomalies an operator needs to see. It never invents progress: a record
// that claims more than the engine shows is pulled back to failed, and a
// record whose container leaked past close is reported for cleanup.
func Reconcile(record Record, observed Observed, now time.Time) (Record, []string) {
	var anomalies []string

	switch {
	case record.State == StateClosed:
		if observed.Found {
			anomalies = append(anomalies, AnomalyContainerRetained)
		}
		return record, anomalies

	case !observed.Found:
		anomalies = append(anomalies, AnomalyContainerMissing)
		return fail(record, now), anomalies

	case !observed.Running:
		anomalies = append(anomalies, AnomalyContainerStopped)
		return fail(record, now), anomalies

	case record.State == StateCreating:
		// The CLI died after creating the container but before the supervisor
		// reported readiness. Nothing owns the container, so it is a failure
		// the operator can see and close, not a usable session.
		anomalies = append(anomalies, AnomalyCreationInterrupted)
		return fail(record, now), anomalies
	}

	return record, anomalies
}

// fail records a session the engine no longer sustains, without disturbing a
// cleanup that is already in progress or re-stamping a session that is already
// recorded as failed. Reconciliation is therefore idempotent: listing sessions
// twice leaves the records byte for byte identical.
func fail(record Record, now time.Time) Record {
	if record.State.Terminal() || record.State == StateClosing || record.State == StateFailed {
		return record
	}
	record.State = StateFailed
	record.UpdatedAt = now
	return record
}

func usageFailure(operation, format string, args ...any) *result.Failure {
	return result.NewFailure(result.CodeUsageError, operation, format, args...)
}
