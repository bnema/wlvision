// Session adapters.
//
// The interaction and temporal-vision services are written against their own
// narrow boundaries, never against the session service: internal/input talks to
// an agentapi.Caller, and internal/capture talks to a Capturer, a Fetcher, an
// Observer and a Destination. This file is where a session becomes those four
// things, so the services stay testable with fakes and the CLI stays thin.
package main

import (
	"bytes"
	"context"
	"path"
	"strings"
	"time"

	"github.com/bnema/wlvision/internal/agentapi"
	"github.com/bnema/wlvision/internal/capture"
	"github.com/bnema/wlvision/internal/export"
	"github.com/bnema/wlvision/internal/result"
	"github.com/bnema/wlvision/internal/session"
)

// scratchCapture is the name inside the session's export directory where the
// controller stores the frame of one capture. Every capture overwrites it, and
// the bytes are read back before the next capture is asked for, because the
// controller keeps one capture in flight.
const scratchCapture = "scratch/frame.png"

// call performs one operation for a session and requires a reply shape.
func call(ctx context.Context, service *session.Service, id, operation string, params agentapi.Params) (agentapi.Reply, error) {
	return service.Call(ctx, id, operation, params)
}

// sessionCapture performs one capture through a session's controller.
type sessionCapture struct {
	service *session.Service
	id      string
}

// Capture implements capture.Capturer.
func (c sessionCapture) Capture(ctx context.Context) (capture.Captured, error) {
	reply, err := call(ctx, c.service, c.id, agentapi.OpCapture, agentapi.Params{Path: scratchCapture})
	if err != nil {
		return capture.Captured{}, err
	}
	if reply.Frame == nil {
		return capture.Captured{}, result.NewFailure(result.CodeCaptureFailed, "capture.screenshot",
			"the session's controller reported no frame")
	}
	return capture.Captured{Frame: *reply.Frame, Revision: reply.Revision}, nil
}

// sessionFetcher reads one stored capture back out of a session.
//
// The controller reports where it stored a frame as an absolute container path,
// while the session's fetch operation works with a name relative to the export
// directory. Both forms are accepted here; anything else is refused, so a
// reply can never turn into a read of an unrelated file.
type sessionFetcher struct {
	service *session.Service
	id      string
}

// Fetch implements capture.Fetcher.
func (f sessionFetcher) Fetch(ctx context.Context, stored string) ([]byte, error) {
	name, ok := exportNameFromContainerPath(stored)
	if !ok {
		return nil, result.NewFailure(result.CodeUsageError, "capture.fetch",
			"the session reported the capture %q, which is not inside its export directory", stored)
	}

	var buffered bytes.Buffer
	if err := f.service.Fetch(ctx, f.id, name, &buffered); err != nil {
		return nil, err
	}
	return buffered.Bytes(), nil
}

// exportNameFromContainerPath turns the path the controller reported into the
// name the fetch operation takes.
func exportNameFromContainerPath(stored string) (string, bool) {
	if !path.IsAbs(stored) {
		return stored, stored != ""
	}
	name, ok := strings.CutPrefix(stored, session.ExportDir+"/")
	if !ok {
		return "", false
	}
	return name, name != ""
}

// exportDestination stores artifacts under one session's export tree.
type exportDestination struct {
	tree   *export.Tree
	prefix string
}

// Store implements capture.Destination. The prefix is the caller's --output
// directory; every name the service chooses lands inside it.
func (d exportDestination) Store(name string, data []byte) (string, error) {
	if d.prefix == "" {
		return d.tree.Store(name, data)
	}
	return d.tree.Store(path.Join(d.prefix, name), data)
}

// sessionObserver reads the session state the non-visual waits need.
type sessionObserver struct {
	service *session.Service
	id      string
}

// Snapshot implements capture.Observer.
func (o sessionObserver) Snapshot(ctx context.Context) (agentapi.State, error) {
	reply, err := call(ctx, o.service, o.id, agentapi.OpSnapshot, agentapi.Params{})
	if err != nil {
		return agentapi.State{}, err
	}
	if reply.State == nil {
		return agentapi.State{}, result.NewFailure(result.CodeSessionNotReady, "wait.windows",
			"the session's controller reported no window state")
	}
	return *reply.State, nil
}

// Exited implements capture.Observer. It reads the durable record rather than
// asking the engine, because a wait polls this signal and the record is the
// cheap answer.
func (o sessionObserver) Exited(ctx context.Context, since time.Time) (bool, int, error) {
	record, err := o.service.Record(o.id)
	if err != nil {
		return false, 0, err
	}
	if record.Process == nil || record.Process.At.Before(since) {
		return false, 0, nil
	}
	return true, record.Process.Code, nil
}
