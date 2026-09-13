// Package enginetest provides an in-process container engine for tests.
//
// It records the commands a caller issues and answers them from a script, so
// lifecycle tests can assert the exact operations wlvision performs without a
// daemon, an image, or a container.
package enginetest

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/bnema/wlvision/internal/engine"
)

// ReadyStatus is what a healthy session's supervisor reports to a readiness
// probe. It is the wire contract of the supervisor's status mode.
const ReadyStatus = `{"ready":true,"weston":"running","agent":"running"}`

// Container is one container the fake engine holds.
type Container struct {
	ID       string
	Name     string
	Session  string
	Spec     engine.CreateSpec
	Running  bool
	ExitCode int
	Logs     string
}

// Fake is a scripted container engine. The zero value is not usable; call New.
type Fake struct {
	// CapabilitiesValue is the engine report returned by Capabilities.
	CapabilitiesValue engine.Capabilities
	// Status is the JSON body the fake supervisor reports to a readiness
	// probe. Tests set it to describe a starting, ready, or broken session.
	Status string

	// Failure injection. A non-nil error is returned by the matching method.
	CapabilitiesErr error
	CreateErr       error
	StartErr        error
	StopErr         error
	RemoveErr       error
	StateErr        error
	LogsErr         error

	// ExecFunc and StreamFunc override the default behaviour. When unset, a
	// supervisor status probe answers from Status and any other command exits
	// with code 0.
	ExecFunc   func(spec engine.ExecSpec) (engine.ExecResult, error)
	StreamFunc func(spec engine.StreamSpec) error

	calls      []string
	containers map[string]*Container
	nextID     int
	mu         sync.Mutex
}

// New returns a fake engine that looks like a healthy rootless Docker with the
// built-in seccomp profile and delegated cgroup controllers.
func New() *Fake {
	return &Fake{
		CapabilitiesValue: engine.Capabilities{
			Kind:           engine.KindDocker,
			Context:        "default",
			ServerVersion:  "29.7.2",
			Rootless:       true,
			SeccompProfile: "builtin",
			CgroupVersion:  "2",
			CgroupDriver:   "systemd",
			MemoryLimit:    true,
			PidsLimit:      true,
		},
		Status:     ReadyStatus,
		containers: make(map[string]*Container),
	}
}

// Kind implements engine.Engine.
func (f *Fake) Kind() engine.Kind { return engine.KindDocker }

// Capabilities implements engine.Engine.
func (f *Fake) Capabilities(context.Context) (engine.Capabilities, error) {
	f.record("capabilities")
	return f.CapabilitiesValue, f.CapabilitiesErr
}

// Create implements engine.Engine.
func (f *Fake) Create(_ context.Context, spec engine.CreateSpec) (string, error) {
	f.record("create " + spec.Name)
	if f.CreateErr != nil {
		return "", f.CreateErr
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := fmt.Sprintf("container-%d", f.nextID)
	f.containers[id] = &Container{ID: id, Name: spec.Name, Session: spec.Session, Spec: spec, Logs: "wlvision-supervisor: starting\n"}
	return id, nil
}

// Start implements engine.Engine.
func (f *Fake) Start(_ context.Context, id string) error {
	f.record("start " + id)
	if f.StartErr != nil {
		return f.StartErr
	}

	container, ok := f.container(id)
	if !ok {
		return fmt.Errorf("%w: %s", engine.ErrNotFound, id)
	}
	f.mu.Lock()
	container.Running = true
	f.mu.Unlock()
	return nil
}

// Exec implements engine.Engine.
func (f *Fake) Exec(_ context.Context, spec engine.ExecSpec) (engine.ExecResult, error) {
	f.record("exec user=" + fmt.Sprint(spec.User) + " " + strings.Join(spec.Argv, " "))
	if f.ExecFunc != nil {
		return f.ExecFunc(spec)
	}

	if isStatusProbe(spec.Argv) {
		if spec.Stdout != nil {
			if _, err := io.WriteString(spec.Stdout, f.Status); err != nil {
				return engine.ExecResult{}, err
			}
		}
		return engine.ExecResult{}, nil
	}
	return engine.ExecResult{}, nil
}

// Stream implements engine.Engine.
func (f *Fake) Stream(_ context.Context, spec engine.StreamSpec) error {
	f.record("stream user=" + fmt.Sprint(spec.User) + " " + strings.Join(spec.Argv, " "))
	if f.StreamFunc != nil {
		return f.StreamFunc(spec)
	}
	return nil
}

// State implements engine.Engine.
func (f *Fake) State(_ context.Context, id string) (engine.ContainerState, error) {
	f.record("state " + id)
	if f.StateErr != nil {
		return engine.ContainerState{}, f.StateErr
	}

	container, ok := f.container(id)
	if !ok {
		return engine.ContainerState{}, fmt.Errorf("%w: %s", engine.ErrNotFound, id)
	}
	return engine.ContainerState{Running: container.Running, ExitCode: container.ExitCode}, nil
}

// Logs implements engine.Engine.
func (f *Fake) Logs(_ context.Context, id string, tail int, w io.Writer) error {
	f.record(fmt.Sprintf("logs %s tail=%d", id, tail))
	if f.LogsErr != nil {
		return f.LogsErr
	}

	container, ok := f.container(id)
	if !ok {
		return fmt.Errorf("%w: %s", engine.ErrNotFound, id)
	}
	_, err := io.WriteString(w, container.Logs)
	return err
}

// Stop implements engine.Engine.
func (f *Fake) Stop(_ context.Context, id string, _ time.Duration) error {
	f.record("stop " + id)
	if f.StopErr != nil {
		return f.StopErr
	}

	if container, ok := f.container(id); ok {
		f.mu.Lock()
		container.Running = false
		f.mu.Unlock()
		return nil
	}
	return fmt.Errorf("%w: %s", engine.ErrNotFound, id)
}

// Remove implements engine.Engine.
func (f *Fake) Remove(_ context.Context, id string) error {
	f.record("remove " + id)
	if f.RemoveErr != nil {
		return f.RemoveErr
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.containers[id]; !ok {
		return fmt.Errorf("%w: %s", engine.ErrNotFound, id)
	}
	delete(f.containers, id)
	return nil
}

// Forget drops a container without any stop or remove, which is what a crashed
// or externally cleaned-up session looks like to the engine.
func (f *Fake) Forget(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.containers, id)
}

// Container returns one container, if the fake still holds it.
func (f *Fake) Container(id string) (*Container, bool) { return f.container(id) }

// CallLog returns the recorded commands in order.
func (f *Fake) CallLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// CountCalls returns how many recorded commands start with prefix.
func (f *Fake) CountCalls(prefix string) int {
	count := 0
	for _, call := range f.CallLog() {
		if strings.HasPrefix(call, prefix) {
			count++
		}
	}
	return count
}

func (f *Fake) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *Fake) container(id string) (*Container, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	container, ok := f.containers[id]
	return container, ok
}

func isStatusProbe(argv []string) bool {
	return len(argv) == 2 && argv[0] == supervisorPath && argv[1] == "status"
}

// supervisorPath mirrors the supervisor's location in the session image. It is
// repeated here rather than imported so this package stays a pure engine fake
// that a session package test can import without a cycle.
const supervisorPath = "/usr/libexec/wlvision-supervisor"
