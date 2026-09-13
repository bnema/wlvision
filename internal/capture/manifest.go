package capture

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/bnema/wlvision/internal/result"
)

// ManifestSchema identifies the manifest document format. A reader must refuse
// a document that carries a different one.
const ManifestSchema = "wlvision/burst/v1"

// manifestName is the artifact name a manifest is stored under.
const manifestName = burstManifestName

// operationManifest names a manifest failure.
const operationManifest = "capture.manifest"

// Manifest is the publishable record of one burst: what it was asked for, what
// it stored, and one entry per scheduled sample in schedule order.
//
// A manifest is written last, after every frame, so its presence means the
// burst it describes is complete.
type Manifest struct {
	Schema       string
	Interval     time.Duration
	Duration     time.Duration
	Bytes        int64
	Captured     int
	Deduplicated int
	Missed       int
	Failed       int
	// Truncated and TruncationReason carry the burst's decision to stop at
	// the byte cap, so a manifest never reads as a complete sample when it was
	// cut short.
	Truncated        bool
	TruncationReason string
	Frames           []Sample
}

// BuildManifest reads a burst report into a manifest. The frames are copied so
// a later change to the report cannot rewrite an already built manifest.
func BuildManifest(report BurstReport) Manifest {
	frames := make([]Sample, len(report.Frames))
	copy(frames, report.Frames)

	return Manifest{
		Schema:           ManifestSchema,
		Interval:         report.Interval,
		Duration:         report.Duration,
		Bytes:            report.Bytes,
		Captured:         report.Captured,
		Deduplicated:     report.Deduplicated,
		Missed:           report.Missed,
		Failed:           report.Failed,
		Truncated:        report.Truncated,
		TruncationReason: report.TruncationReason,
		Frames:           frames,
	}
}

// manifestDocument is the wire form of a manifest.
//
// It exists so the rendered field order is fixed by this declaration rather
// than by the Go struct a caller happens to hold, and so digests and durations
// are rendered in the form the contract names: prefixed hexadecimal sha256
// digests and integer nanoseconds. Rendering the same manifest twice always
// produces the same bytes.
type manifestDocument struct {
	Schema           string       `json:"schema"`
	IntervalNS       int64        `json:"interval_ns"`
	DurationNS       int64        `json:"duration_ns"`
	Bytes            int64        `json:"bytes"`
	Captured         int          `json:"captured"`
	Deduplicated     int          `json:"deduplicated"`
	Missed           int          `json:"missed"`
	Failed           int          `json:"failed"`
	Truncated        bool         `json:"truncated"`
	TruncationReason string       `json:"truncation_reason,omitempty"`
	Frames           []sampleJSON `json:"frames"`
}

// sampleJSON is the wire form of one scheduled sample.
type sampleJSON struct {
	ScheduledNS int64  `json:"scheduled_ns"`
	RequestedNS int64  `json:"requested_ns"`
	CompletedNS int64  `json:"completed_ns"`
	RequestID   uint64 `json:"request_id"`
	FrameSeq    uint64 `json:"frame_seq"`
	Digest      string `json:"digest"`
	Status      Status `json:"status"`
	Path        string `json:"path,omitempty"`
	Revision    uint64 `json:"revision"`
	Error       string `json:"error,omitempty"`
}

// RenderManifest renders a manifest as deterministic indented JSON with a
// trailing newline.
func RenderManifest(manifest Manifest) ([]byte, error) {
	document := manifestDocument{
		Schema:           manifest.Schema,
		IntervalNS:       int64(manifest.Interval),
		DurationNS:       int64(manifest.Duration),
		Bytes:            manifest.Bytes,
		Captured:         manifest.Captured,
		Deduplicated:     manifest.Deduplicated,
		Missed:           manifest.Missed,
		Failed:           manifest.Failed,
		Truncated:        manifest.Truncated,
		TruncationReason: manifest.TruncationReason,
		Frames:           make([]sampleJSON, 0, len(manifest.Frames)),
	}
	for _, sample := range manifest.Frames {
		// Only a captured frame has a digest of its own: a missed or failed
		// sample renders an empty digest rather than a fabricated hash for
		// pixels that were never taken.
		digest := ""
		switch sample.Status {
		case StatusCaptured, StatusDeduplicated:
			digest = DigestString(sample.Digest)
		}

		document.Frames = append(document.Frames, sampleJSON{
			ScheduledNS: int64(sample.Scheduled),
			RequestedNS: int64(sample.Requested),
			CompletedNS: int64(sample.Completed),
			RequestID:   sample.RequestID,
			FrameSeq:    sample.FrameSeq,
			Digest:      digest,
			Status:      sample.Status,
			Path:        sample.Path,
			Revision:    sample.Revision,
			Error:       sample.Error,
		})
	}

	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("capture: rendering manifest: %w", err)
	}
	return append(encoded, '\n'), nil
}

// WriteManifest renders a manifest and stores it as "manifest.json", returning
// the host path it was written to.
func WriteManifest(destination Destination, manifest Manifest) (string, error) {
	if destination == nil {
		return "", result.NewFailure(result.CodeUsageError, operationManifest, "a destination is required")
	}
	encoded, err := RenderManifest(manifest)
	if err != nil {
		return "", err
	}
	path, err := destination.Store(manifestName, encoded)
	if err != nil {
		return "", fmt.Errorf("capture: storing manifest: %w", err)
	}
	return path, nil
}
