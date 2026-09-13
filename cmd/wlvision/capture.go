// Capture commands: one still picture, and a bounded burst of samples.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"time"

	"github.com/bnema/wlvision/internal/agentapi"
	"github.com/bnema/wlvision/internal/capture"
	"github.com/bnema/wlvision/internal/result"
)

// defaultScreenshot is where a screenshot lands when the caller names no file.
const defaultScreenshot = "screenshot.png"

// captureResult is what one capture reports. The path is the host path of the
// stored artifact, which is the only path an agent can use.
type captureResult struct {
	Path     string `json:"path"`
	Digest   string `json:"digest"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	Format   string `json:"format"`
	FrameSeq uint64 `json:"frame_sequence"`
	Revision uint64 `json:"revision"`
}

func (c *cli) screenshot(args []string) int {
	const operation = "screenshot"
	var (
		id     string
		output string
	)
	id, flags, err := c.sessionArgs("wlvision screenshot", args, func(flags *flag.FlagSet) {
		flags.StringVar(&output, "output", defaultScreenshot, "artifact name, relative to the session export directory")
	})
	if err != nil {
		return c.usageError(operation, id, err)
	}
	if code := c.checkSessionArgs(operation, id, flags); code != 0 {
		return code
	}
	if output == "" {
		return c.usageError(operation, id, errors.New("--output must not be empty"))
	}

	service, err := c.newService()
	if err != nil {
		return c.fail(operation, id, err, result.CodeEngineUnavailable)
	}
	tree, err := c.exportTree(service, id)
	if err != nil {
		return c.fail(operation, id, err, result.CodeUsageError)
	}
	// The name is validated before anything is captured, so a request that
	// cannot be stored does not bother the session.
	if _, err := tree.Path(output); err != nil {
		return c.fail(operation, id, err, result.CodeUsageError)
	}

	caller := service.Caller(id)
	reply, err := caller.Call(c.ctx, agentapi.OpCapture, agentapi.Params{Path: output})
	if err != nil {
		return c.fail(operation, id, err, result.CodeSessionNotReady)
	}
	if reply.Frame == nil {
		return c.fail(operation, id, result.NewFailure(result.CodeCaptureFailed, operation,
			"the session's controller reported no frame"), result.CodeCaptureFailed)
	}

	var payload bytes.Buffer
	if err := service.Fetch(c.ctx, id, output, &payload); err != nil {
		return c.fail(operation, id, err, result.CodeCaptureFailed)
	}
	stored, err := tree.Store(output, payload.Bytes())
	if err != nil {
		return c.fail(operation, id, err, result.CodeCaptureFailed)
	}

	// The digest is what makes a capture usable as evidence: the bytes on the
	// host must be the bytes the session reported.
	if err := verifyDigest(reply.Frame.Digest, payload.Bytes()); err != nil {
		return c.fail(operation, id, err, result.CodeCaptureFailed)
	}

	return c.success(operation, id, reply.Frame.Sequence, captureResult{
		Path:     stored,
		Digest:   reply.Frame.Digest,
		Width:    reply.Frame.Width,
		Height:   reply.Frame.Height,
		Format:   reply.Frame.Format,
		FrameSeq: reply.Frame.Sequence,
		Revision: reply.Revision,
	})
}

// verifyDigest compares stored bytes with the digest the session reported.
func verifyDigest(reported string, payload []byte) error {
	if reported == "" {
		return result.NewFailure(result.CodeCaptureFailed, "screenshot",
			"the session reported no digest for the capture")
	}
	digest := sha256.Sum256(payload)
	actual := "sha256:" + hex.EncodeToString(digest[:])
	if actual != reported {
		return result.NewFailure(result.CodeCaptureFailed, "screenshot",
			"the stored capture hashes to %s, but the session reported %s", actual, reported)
	}
	return nil
}

// burstResult is what a bounded burst reports: where its manifest and contact
// sheet went, and what happened to every scheduled sample.
type burstResult struct {
	ManifestPath string       `json:"manifest"`
	ContactSheet string       `json:"contact_sheet,omitempty"`
	Interval     string       `json:"interval"`
	Duration     string       `json:"duration"`
	Bytes        int64        `json:"bytes"`
	Captured     int          `json:"captured"`
	Deduplicated int          `json:"deduplicated"`
	Missed       int          `json:"missed"`
	Failed       int          `json:"failed"`
	Truncated    bool         `json:"truncated,omitempty"`
	Reason       string       `json:"truncated_reason,omitempty"`
	Frames       []burstFrame `json:"frames"`
}

// burstFrame is one manifest entry as the CLI reports it.
type burstFrame struct {
	Scheduled string `json:"scheduled"`
	Requested string `json:"requested,omitempty"`
	Completed string `json:"completed,omitempty"`
	RequestID uint64 `json:"capture_request_id,omitempty"`
	FrameSeq  uint64 `json:"frame_sequence,omitempty"`
	Digest    string `json:"digest,omitempty"`
	Status    string `json:"status"`
	Path      string `json:"path,omitempty"`
	Error     string `json:"error,omitempty"`
}

func (c *cli) captureBurst(args []string) int {
	const operation = "capture"
	var (
		id           string
		interval     time.Duration
		duration     time.Duration
		frames       int
		output       string
		contactSheet bool
	)
	id, flags, err := c.sessionArgs("wlvision capture", args, func(flags *flag.FlagSet) {
		flags.Var(durationFlag{&interval}, "interval", "spacing between scheduled samples")
		flags.Var(durationFlag{&duration}, "duration", "how long to sample")
		flags.IntVar(&frames, "frames", 0, "maximum number of samples")
		flags.StringVar(&output, "output", "", "artifact directory, relative to the session export directory")
		flags.BoolVar(&contactSheet, "contact-sheet", false, "also store a contact sheet of the samples")
	})
	if err != nil {
		return c.usageError(operation, id, err)
	}
	if code := c.checkSessionArgs(operation, id, flags); code != 0 {
		return code
	}
	if frames < 0 {
		return c.usageError(operation, id, errors.New("--frames must not be negative"))
	}

	service, err := c.newService()
	if err != nil {
		return c.fail(operation, id, err, result.CodeEngineUnavailable)
	}
	tree, err := c.exportTree(service, id)
	if err != nil {
		return c.fail(operation, id, err, result.CodeUsageError)
	}
	if output != "" {
		if _, err := tree.Path(output); err != nil {
			return c.fail(operation, id, err, result.CodeUsageError)
		}
	}

	report, err := capture.Burst(c.ctx, capture.BurstPlan{
		Interval:     interval,
		Duration:     duration,
		MaxFrames:    frames,
		ContactSheet: contactSheet,
	}, capture.BurstDeps{
		Clock:       capture.RealClock{},
		Capturer:    sessionCapture{service: service, id: id},
		Fetcher:     sessionFetcher{service: service, id: id},
		Destination: exportDestination{tree: tree, prefix: output},
	})
	if err != nil {
		return c.fail(operation, id, err, result.CodeSessionNotReady)
	}

	view := burstResult{
		ManifestPath: report.ManifestPath,
		ContactSheet: report.ContactSheetPath,
		Interval:     report.Interval.String(),
		Duration:     report.Duration.String(),
		Bytes:        report.Bytes,
		Captured:     report.Captured,
		Deduplicated: report.Deduplicated,
		Missed:       report.Missed,
		Failed:       report.Failed,
		Truncated:    report.Truncated,
		Reason:       report.TruncationReason,
		Frames:       make([]burstFrame, 0, len(report.Frames)),
	}
	for _, sample := range report.Frames {
		view.Frames = append(view.Frames, burstFrame{
			Scheduled: sample.Scheduled.String(),
			Requested: durationString(sample.Requested),
			Completed: durationString(sample.Completed),
			RequestID: sample.RequestID,
			FrameSeq:  sample.FrameSeq,
			Digest:    capture.DigestString(sample.Digest),
			Status:    string(sample.Status),
			Path:      sample.Path,
			Error:     sample.Error,
		})
	}
	return c.success(operation, id, 0, view)
}

// durationString renders an offset that may not have been reached.
func durationString(offset time.Duration) string {
	if offset == 0 {
		return ""
	}
	return offset.String()
}

// frameSummary is one captured frame as the waits report it.
type frameSummary struct {
	Path     string `json:"path,omitempty"`
	Digest   string `json:"digest,omitempty"`
	FrameSeq uint64 `json:"frame_sequence,omitempty"`
	Status   string `json:"status,omitempty"`
}

// summarize renders a captured sample for a wait result.
func summarize(sample *capture.Sample) *frameSummary {
	if sample == nil {
		return nil
	}
	return &frameSummary{
		Path:     sample.Path,
		Digest:   capture.DigestString(sample.Digest),
		FrameSeq: sample.FrameSeq,
		Status:   string(sample.Status),
	}
}
