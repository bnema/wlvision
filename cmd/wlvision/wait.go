// The wait command: event predicates instead of sleeps.
package main

import (
	"errors"
	"flag"
	"math"
	"time"

	"github.com/bnema/wlvision/internal/agentapi"
	"github.com/bnema/wlvision/internal/capture"
	"github.com/bnema/wlvision/internal/result"
)

// defaultWaitTimeout bounds a wait that names no timeout. Every wait has a
// finite deadline; a wait without one is a hang waiting to happen.
const defaultWaitTimeout = 10 * time.Second

// waitResult is what a wait reports: the predicate it watched, how much it
// observed, and the last thing it saw. On a timeout the same shape is carried
// in the failure's details, so a caller still learns what the session looked
// like when it gave up.
type waitResult struct {
	Kind         string          `json:"kind"`
	Duration     string          `json:"duration,omitempty"`
	Probes       int             `json:"probes"`
	Observations int             `json:"observations"`
	Revision     uint64          `json:"revision,omitempty"`
	Frame        *frameSummary   `json:"frame,omitempty"`
	Windows      *agentapi.State `json:"windows,omitempty"`
	ExitCode     *int            `json:"exit_code,omitempty"`
}

func (c *cli) wait(args []string) int {
	const operation = "wait"
	var (
		id        string
		timeout   time.Duration
		stableFor time.Duration
		count     int
		exited    bool
		newFrame  uint
		handle    string
		width     uint
		height    uint
		state     uint
	)
	id, flags, err := c.sessionArgs("wlvision wait", args, func(flags *flag.FlagSet) {
		flags.Var(durationFlag{&timeout}, "timeout", "how long to wait")
		flags.Var(durationFlag{&stableFor}, "stable-for", "wait until the picture has been identical for this long")
		flags.IntVar(&count, "window-count", -1, "wait until this many windows exist")
		flags.BoolVar(&exited, "process-exit", false, "wait until the session records an application exit")
		flags.UintVar(&newFrame, "new-frame", 0, "wait for a compositor frame after this sequence number")
		flags.StringVar(&handle, "window", "", "wait until this window has the requested geometry")
		flags.UintVar(&width, "width", 0, "requested window width, with --window")
		flags.UintVar(&height, "height", 0, "requested window height, with --window")
		flags.UintVar(&state, "state", 0, "requested window state word, with --window")
	})
	if err != nil {
		return c.usageError(operation, id, err)
	}
	if code := c.checkSessionArgs(operation, id, flags); code != 0 {
		return code
	}
	if count < -1 {
		return c.usageError(operation, id, errors.New("--window-count must not be negative"))
	}
	if width > math.MaxUint32 || height > math.MaxUint32 {
		return c.usageError(operation, id, errors.New("--width and --height must fit in 32 bits"))
	}
	if timeout == 0 {
		timeout = defaultWaitTimeout
	}

	given := givenFlags(flags)
	if !given["stable-for"] && !given["window-count"] && !given["process-exit"] &&
		!given["new-frame"] && !given["window"] {
		return c.usageError(operation, id, errors.New(
			"one predicate is required: --stable-for, --window-count, --process-exit, --new-frame or --window"))
	}
	if !given["window"] && (given["width"] || given["height"] || given["state"]) {
		return c.usageError(operation, id, errors.New("--width, --height and --state require --window"))
	}

	service, err := c.newService()
	if err != nil {
		return c.fail(operation, id, err, result.CodeEngineUnavailable)
	}
	deps := capture.WaitDeps{
		Clock:    capture.RealClock{},
		Capturer: sessionCapture{service: service, id: id},
		Observer: sessionObserver{service: service, id: id},
	}

	var (
		outcome capture.WaitResult
		waitErr error
	)
	switch {
	case given["stable-for"]:
		if stableFor <= 0 {
			return c.usageError(operation, id, errors.New("--stable-for must be positive"))
		}
		outcome, waitErr = capture.WaitStable(c.ctx, stableFor, timeout, deps)
	case given["window-count"]:
		if count < 0 {
			return c.usageError(operation, id, errors.New("--window-count must not be negative"))
		}
		outcome, waitErr = capture.WaitWindowCount(c.ctx, count, timeout, deps)
	case given["process-exit"]:
		outcome, waitErr = capture.WaitExited(c.ctx, timeout, deps)
	case given["new-frame"]:
		outcome, waitErr = capture.WaitNewFrame(c.ctx, uint64(newFrame), timeout, deps)
	default:
		want := capture.WindowState{Handle: handle, Width: uint32(width), Height: uint32(height)}
		if given["state"] {
			word := uint32(state)
			want.State = &word
		}
		outcome, waitErr = capture.WaitWindowState(c.ctx, want, timeout, deps)
	}

	view := waitResult{
		Kind:         outcome.Kind,
		Duration:     durationString(outcome.Duration),
		Probes:       outcome.Probes,
		Observations: outcome.Observations,
		Revision:     outcome.Revision,
		Frame:        summarize(outcome.Last),
		Windows:      outcome.State,
	}
	if waitErr != nil {
		return c.fail(operation, id, waitErr, result.CodeWaitTimeout)
	}
	if given["process-exit"] {
		if record, err := service.Record(id); err == nil && record.Process != nil {
			code := record.Process.Code
			view.ExitCode = &code
		}
	}
	return c.success(operation, id, outcome.Revision, view)
}
