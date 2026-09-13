package capture

import (
	"context"
	"fmt"
	"image"
	"time"

	"github.com/bnema/wlvision/internal/result"
)

// BurstPlan is what a caller asks a burst for. Every zero field selects the
// matching default from vision.go, so a caller that names nothing gets a
// bounded two-second sample rather than an unbounded recording.
type BurstPlan struct {
	// Interval is the spacing between scheduled deadlines. Zero selects
	// DefaultInterval.
	Interval time.Duration
	// Duration bounds the burst: no sample is scheduled at or after it. Zero
	// selects DefaultDuration.
	Duration time.Duration
	// MaxFrames caps the sample count. Zero selects DefaultMaxFrames.
	MaxFrames int
	// Prefix names the stored frames "<prefix>-%04d.png", where the number is
	// the sample's 1-based position in the schedule, the same number a contact
	// sheet cell shows. Empty selects "frame".
	Prefix string
	// ContactSheet asks for a contact sheet of the burst's frames.
	ContactSheet bool

	// bytes is the byte cap this burst enforces in place of MaxBurstBytes. It
	// is unexported and only this package's tests set it, so no test can
	// change the cap another test runs with.
	bytes int64
}

// BurstDeps is what a burst runs against. All four are required.
type BurstDeps struct {
	Clock       Clock
	Capturer    Capturer
	Fetcher     Fetcher
	Destination Destination
}

// BurstReport is what one burst did, in schedule order.
type BurstReport struct {
	// ManifestPath is where the manifest was stored; it is written last, so
	// its presence is what makes the burst publishable.
	ManifestPath string
	// ContactSheetPath is where the contact sheet was stored, empty when the
	// plan did not ask for one.
	ContactSheetPath string
	// Interval and Duration are the effective values the burst ran with.
	Interval time.Duration
	Duration time.Duration
	// Frames holds one entry per scheduled sample, missed and failed ones
	// included, in schedule order.
	Frames       []Sample
	Captured     int
	Deduplicated int
	Missed       int
	Failed       int
	// Bytes is how many frame bytes the burst stored.
	Bytes            int64
	Truncated        bool
	TruncationReason string
}

// Artifact names and the operation a burst failure names.
const (
	burstManifestName     = "manifest.json"
	burstContactSheetName = "contact.png"
	defaultBurstPrefix    = "frame"
	operationBurst        = "capture.burst"
)

// Burst captures a bounded series of frames on absolute deadlines.
//
// Sample n is due at start + n*Interval, computed from the injected clock, so a
// slow capture never shifts the deadlines that follow: it only causes the
// samples whose deadlines passed while it ran to be recorded as missed. Those
// are never queued or retried. Captures are taken one at a time, because the
// resident controller owns a single capture source.
//
// Frames are stored as they complete, then the optional contact sheet, and the
// manifest last: a burst that did not finish leaves no manifest behind to be
// mistaken for a finished one.
func Burst(ctx context.Context, plan BurstPlan, deps BurstDeps) (BurstReport, error) {
	settings, err := burstSettings(plan)
	if err != nil {
		return BurstReport{}, err
	}
	if err := checkBurstDeps(deps); err != nil {
		return BurstReport{}, err
	}

	start := deps.Clock.Now()
	report := BurstReport{Interval: settings.Interval, Duration: settings.Duration}
	total := burstFrameCount(settings)
	frames := make([]Sample, total)

	var (
		state       burstState
		sheetFrames []SheetFrame
		index       int
	)

	finalise := func(produced []Sample) {
		report.Frames = produced
		report.Bytes = state.StoredBytes
		for _, sample := range produced {
			switch sample.Status {
			case StatusCaptured:
				report.Captured++
			case StatusDeduplicated:
				report.Deduplicated++
			case StatusMissed:
				report.Missed++
			case StatusFailed:
				report.Failed++
			}
		}
	}

	for index < total {
		scheduled := time.Duration(index) * settings.Interval
		if err := waitUntil(ctx, deps.Clock, start.Add(scheduled)); err != nil {
			finalise(frames[:index])
			return report, err
		}

		out, err := captureSample(ctx, deps, settings, index, scheduled, deps.Clock.Now().Sub(start), state)
		if err != nil {
			finalise(frames[:index])
			return report, err
		}
		frames[index] = out.sample
		frames[index].Completed = deps.Clock.Now().Sub(start)
		state = out.state
		if settings.ContactSheet {
			// Only a frame that was stored, or a duplicate that points at one,
			// has pixels to show; a missed or failed sample must not borrow a
			// neighbour's.
			var cell image.Image
			switch out.sample.Status {
			case StatusCaptured, StatusDeduplicated:
				if state.Cell != nil {
					cell = state.Cell
				}
			}
			sheetFrames = append(sheetFrames, SheetFrame{Label: sheetLabel(index, out.sample), Image: cell})
		}
		index++
		if out.truncated {
			report.Truncated = true
			report.TruncationReason = out.reason
			break
		}

		// Deadlines that passed while the capture was in flight are missed:
		// they are recorded and skipped, never retried.
		elapsed := deps.Clock.Now().Sub(start)
		for index < total && time.Duration(index)*settings.Interval < elapsed {
			frames[index] = Sample{Scheduled: time.Duration(index) * settings.Interval, Status: StatusMissed}
			if settings.ContactSheet {
				sheetFrames = append(sheetFrames, SheetFrame{Label: sheetLabel(index, frames[index])})
			}
			index++
		}
	}

	finalise(frames[:index])

	if len(sheetFrames) > 0 && settings.ContactSheet {
		sheet, err := ContactSheet(sheetFrames, SheetColumns)
		if err != nil {
			return report, err
		}
		encoded, err := EncodePNG(sheet)
		if err != nil {
			return report, err
		}
		path, err := deps.Destination.Store(burstContactSheetName, encoded)
		if err != nil {
			return report, err
		}
		report.ContactSheetPath = path
	}

	manifestPath, err := WriteManifest(deps.Destination, BuildManifest(report))
	if err != nil {
		return report, err
	}
	report.ManifestPath = manifestPath
	return report, nil
}

// burstSettings applies the plan's defaults and refuses the values outside the
// bounds, so a caller cannot ask for a burst that cannot be delivered.
func burstSettings(plan BurstPlan) (BurstPlan, error) {
	switch {
	case plan.Interval < 0:
		return BurstPlan{}, result.NewFailure(result.CodeUsageError, operationBurst,
			"interval %s is negative", plan.Interval)
	case plan.Duration < 0:
		return BurstPlan{}, result.NewFailure(result.CodeUsageError, operationBurst,
			"duration %s is negative", plan.Duration)
	case plan.MaxFrames < 0:
		return BurstPlan{}, result.NewFailure(result.CodeUsageError, operationBurst,
			"max frames %d is negative", plan.MaxFrames)
	case plan.Interval != 0 && plan.Interval < MinInterval:
		return BurstPlan{}, result.NewFailure(result.CodeUsageError, operationBurst,
			"interval %s is shorter than the %s minimum", plan.Interval, MinInterval)
	case plan.Duration > MaxDuration:
		return BurstPlan{}, result.NewFailure(result.CodeUsageError, operationBurst,
			"duration %s is longer than the %s maximum", plan.Duration, MaxDuration)
	case plan.MaxFrames > MaxFrames:
		return BurstPlan{}, result.NewFailure(result.CodeUsageError, operationBurst,
			"max frames %d is above the %d maximum", plan.MaxFrames, MaxFrames)
	}

	if plan.Interval == 0 {
		plan.Interval = DefaultInterval
	}
	if plan.Duration == 0 {
		plan.Duration = DefaultDuration
	}
	if plan.MaxFrames == 0 {
		plan.MaxFrames = DefaultMaxFrames
	}
	if plan.Prefix == "" {
		plan.Prefix = defaultBurstPrefix
	}
	if plan.bytes <= 0 {
		plan.bytes = MaxBurstBytes
	}
	return plan, nil
}

// checkBurstDeps refuses a burst that cannot run.
func checkBurstDeps(deps BurstDeps) error {
	switch {
	case deps.Clock == nil:
		return result.NewFailure(result.CodeUsageError, operationBurst, "a clock is required")
	case deps.Capturer == nil:
		return result.NewFailure(result.CodeUsageError, operationBurst, "a capturer is required")
	case deps.Fetcher == nil:
		return result.NewFailure(result.CodeUsageError, operationBurst, "a fetcher is required")
	case deps.Destination == nil:
		return result.NewFailure(result.CodeUsageError, operationBurst, "a destination is required")
	}
	return nil
}

// burstFrameCount reports how many samples fit in a plan: the frame cap and
// the duration bound whichever is reached first, and the first sample is
// always due at the burst's start.
func burstFrameCount(plan BurstPlan) int {
	count := 0
	for count < plan.MaxFrames && time.Duration(count)*plan.Interval < plan.Duration {
		count++
	}
	return count
}

// waitUntil blocks until deadline, the context ends, or the clock passes it.
// It uses the injected clock, never a sleep, so a fake clock releases it.
func waitUntil(ctx context.Context, clock Clock, deadline time.Time) error {
	for {
		now := clock.Now()
		if !now.Before(deadline) {
			return nil
		}
		select {
		case <-clock.After(deadline.Sub(now)):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// burstState is what a burst carries from one sample to the next: the
// deduplication key of the last stored frame and the small contact sheet cell
// that stands in for it.
type burstState struct {
	// StoredBytes is how many frame bytes the burst has stored so far.
	StoredBytes int64
	// Digest, HasDigest and Path identify the last stored frame, so a repeat
	// of it is not stored twice.
	Digest    [32]byte
	HasDigest bool
	Path      string
	// Cell is the contact sheet cell of the last stored frame, already
	// decoded and cropped to one cell, so a burst never retains a whole
	// payload for the sheet.
	Cell *image.RGBA
}

// burstSample is one sample's outcome and the state that follows it.
type burstSample struct {
	sample    Sample
	state     burstState
	truncated bool
	reason    string
}

// captureSample performs one scheduled capture and stores it.
//
// It returns the burst state that follows the sample: a stored frame replaces
// the deduplication key and the sheet cell, a duplicate keeps them so its cell
// shows the frame that stored the picture, and a failed sample leaves them
// untouched. The returned error is fatal to the burst and is only ever the
// context's error: a capture, fetch, digest or store failure belongs to the
// sample and lets the next deadline try again.
func captureSample(ctx context.Context, deps BurstDeps, plan BurstPlan, index int, scheduled, requested time.Duration, previous burstState) (burstSample, error) {
	sample := Sample{Scheduled: scheduled, Requested: requested, Status: StatusFailed}
	out := burstSample{sample: sample, state: previous}
	fail := func(format string, args ...any) (burstSample, error) {
		failed := sample
		failed.Status = StatusFailed
		failed.Error = fmt.Sprintf(format, args...)
		out.sample = failed
		return out, nil
	}

	captured, err := deps.Capturer.Capture(ctx)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return burstSample{}, ctxErr
		}
		return fail("capture at %s: %v", scheduled, err)
	}

	frame := captured.Frame
	sample.RequestID = frame.CaptureRequestID
	sample.FrameSeq = frame.Sequence
	sample.Revision = captured.Revision

	digest, err := ParseDigest(frame.Digest)
	if err != nil {
		return fail("capture at %s: %v", scheduled, err)
	}
	sample.Digest = digest

	payload, err := deps.Fetcher.Fetch(ctx, frame.Path)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return burstSample{}, ctxErr
		}
		return fail("capture at %s: fetch %s: %v", scheduled, frame.Path, err)
	}
	if got := digestOf(payload); got != digest {
		return fail("capture at %s: %s hashes to %s but the controller reported %s",
			scheduled, frame.Path, DigestString(got), DigestString(digest))
	}

	// An unchanged picture is not stored twice: the entry points at the frame
	// that stored the digest, so a reader still finds pixels.
	if previous.HasDigest && digest == previous.Digest {
		sample.Status = StatusDeduplicated
		sample.Path = previous.Path
		out.sample = sample
		return out, nil
	}

	if previous.StoredBytes+int64(len(payload)) > plan.bytes {
		out.truncated = true
		out.reason = fmt.Sprintf("storing %s would exceed the %d byte burst limit", frame.Path, plan.bytes)
		return fail("capture at %s: %s", scheduled, out.reason)
	}

	// The number is the sample's 1-based position in the schedule, the same
	// number a contact sheet cell shows, so a cell and its stored file agree.
	name := fmt.Sprintf("%s-%04d.png", plan.Prefix, index+1)
	path, err := deps.Destination.Store(name, payload)
	if err != nil {
		return fail("capture at %s: store %s: %v", scheduled, name, err)
	}
	sample.Status = StatusCaptured
	sample.Path = path
	out.sample = sample
	out.state.StoredBytes = previous.StoredBytes + int64(len(payload))
	out.state.Digest, out.state.HasDigest, out.state.Path = digest, true, path
	if plan.ContactSheet {
		// Decode and crop now: retaining the payload would make a burst's
		// memory scale with every frame it stored.
		if cell, err := sheetCellImage(payload); err == nil {
			out.state.Cell = cell
		} else {
			out.state.Cell = nil
		}
	}
	return out, nil
}

// sheetLabel captions one contact sheet cell. The number is the sample's
// 1-based position in the schedule, marked "s", and it is the same number the
// frame artifact is named with, so a cell and its file can be matched. A
// deduplicated sample is marked "dup" because its cell shows the frame of the
// earlier sample that stored the same picture.
func sheetLabel(index int, sample Sample) string {
	status := string(sample.Status)
	if sample.Status == StatusDeduplicated {
		status = "dup"
	}
	return fmt.Sprintf("s%02d %s", index+1, status)
}
