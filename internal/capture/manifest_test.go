package capture

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/bnema/wlvision/internal/result"
)

func digestFixture() [32]byte {
	var digest [32]byte
	for index := range digest {
		digest[index] = 0xab
	}
	return digest
}

func sampleFixture() Sample {
	return Sample{
		Scheduled: 0,
		Requested: 0,
		Completed: 20 * time.Millisecond,
		RequestID: 3,
		FrameSeq:  9,
		Digest:    digestFixture(),
		Status:    StatusCaptured,
		Path:      "/host/export/frame-0001.png",
		Revision:  2,
	}
}

func TestManifestRendersDeterministicBytes(t *testing.T) {
	sample := sampleFixture()
	manifest := BuildManifest(BurstReport{
		Interval: 20 * time.Millisecond,
		Duration: time.Second,
		Bytes:    42,
		Captured: 1,
		Frames:   []Sample{sample},
	})

	first, err := RenderManifest(manifest)
	if err != nil {
		t.Fatalf("RenderManifest returned %v", err)
	}
	second, err := RenderManifest(manifest)
	if err != nil {
		t.Fatalf("RenderManifest returned %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("the same manifest rendered two different documents:\n%s\n%s", first, second)
	}
	if !bytes.HasSuffix(first, []byte("\n")) {
		t.Errorf("the manifest does not end with a newline: %q", first)
	}

	want := fmt.Sprintf(`{
  "schema": "wlvision/burst/v1",
  "interval_ns": 20000000,
  "duration_ns": 1000000000,
  "bytes": 42,
  "captured": 1,
  "deduplicated": 0,
  "missed": 0,
  "failed": 0,
  "truncated": false,
  "frames": [
    {
      "scheduled_ns": 0,
      "requested_ns": 0,
      "completed_ns": 20000000,
      "request_id": 3,
      "frame_seq": 9,
      "digest": %q,
      "status": "captured",
      "path": "/host/export/frame-0001.png",
      "revision": 2
    }
  ]
}
`, DigestString(sample.Digest))
	if string(first) != want {
		t.Errorf("manifest bytes:\n%s\nwant:\n%s", first, want)
	}
}

func TestManifestKeepsScheduleOrderAndStatuses(t *testing.T) {
	manifest := BuildManifest(BurstReport{
		Interval: 10 * time.Millisecond,
		Duration: 30 * time.Millisecond,
		Frames: []Sample{
			{Scheduled: 0, Status: StatusCaptured, Path: "/host/a.png"},
			{Scheduled: 10 * time.Millisecond, Status: StatusMissed},
			{Scheduled: 20 * time.Millisecond, Status: StatusFailed, Error: "compositor refused"},
		},
	})

	encoded, err := RenderManifest(manifest)
	if err != nil {
		t.Fatalf("RenderManifest returned %v", err)
	}
	var decoded struct {
		Frames []struct {
			ScheduledNS int64  `json:"scheduled_ns"`
			Status      string `json:"status"`
			Path        string `json:"path"`
			Error       string `json:"error"`
		} `json:"frames"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("the manifest is not valid json: %v", err)
	}
	want := []struct {
		scheduled int64
		status    string
		path      string
		err       string
	}{
		{0, string(StatusCaptured), "/host/a.png", ""},
		{int64(10 * time.Millisecond), string(StatusMissed), "", ""},
		{int64(20 * time.Millisecond), string(StatusFailed), "", "compositor refused"},
	}
	if len(decoded.Frames) != len(want) {
		t.Fatalf("rendered %d frames, want %d", len(decoded.Frames), len(want))
	}
	for index, frame := range decoded.Frames {
		if frame.ScheduledNS != want[index].scheduled {
			t.Errorf("frame %d scheduled_ns %d, want %d", index, frame.ScheduledNS, want[index].scheduled)
		}
		if frame.Status != want[index].status {
			t.Errorf("frame %d status %q, want %q", index, frame.Status, want[index].status)
		}
		if frame.Path != want[index].path {
			t.Errorf("frame %d path %q, want %q", index, frame.Path, want[index].path)
		}
		if frame.Error != want[index].err {
			t.Errorf("frame %d error %q, want %q", index, frame.Error, want[index].err)
		}
	}
}

func TestManifestRendersAnEmptyFrameList(t *testing.T) {
	encoded, err := RenderManifest(BuildManifest(BurstReport{Interval: time.Millisecond}))
	if err != nil {
		t.Fatalf("RenderManifest returned %v", err)
	}
	if !bytes.Contains(encoded, []byte(`"frames": []`)) {
		t.Errorf("an empty manifest rendered %s, want an empty frames array", encoded)
	}
}

func TestBuildManifestCopiesTheFrames(t *testing.T) {
	report := BurstReport{Interval: time.Millisecond, Frames: []Sample{{Scheduled: 0, Status: StatusCaptured}}}
	manifest := BuildManifest(report)

	report.Frames[0].Status = StatusFailed
	if manifest.Frames[0].Status != StatusCaptured {
		t.Errorf("a manifest changed after the report it was built from changed")
	}
}

func TestWriteManifestStoresTheRenderedDocument(t *testing.T) {
	dst := newMemDestination()
	manifest := BuildManifest(BurstReport{
		Interval: 50 * time.Millisecond,
		Duration: time.Second,
		Frames:   []Sample{{Scheduled: 0, Status: StatusMissed}},
	})

	path, err := WriteManifest(dst, manifest)
	if err != nil {
		t.Fatalf("WriteManifest returned %v", err)
	}
	if path != "/host/export/manifest.json" {
		t.Errorf("manifest path %q, want the destination's host path", path)
	}
	if got := dst.artifacts(); !equalStrings(got, []string{"manifest.json"}) {
		t.Errorf("artifacts %v, want only the manifest", got)
	}

	rendered, err := RenderManifest(manifest)
	if err != nil {
		t.Fatalf("RenderManifest returned %v", err)
	}
	if got := dst.artifact("manifest.json"); !bytes.Equal(got, rendered) {
		t.Errorf("the stored manifest is not the rendered one")
	}
}

func TestManifestCarriesTheTruncationDecision(t *testing.T) {
	reason := "storing /session/export/frame-3.png would exceed the 8 byte burst limit"
	encoded, err := RenderManifest(BuildManifest(BurstReport{
		Interval:         time.Millisecond,
		Duration:         time.Second,
		Truncated:        true,
		TruncationReason: reason,
	}))
	if err != nil {
		t.Fatalf("RenderManifest returned %v", err)
	}
	if !bytes.Contains(encoded, []byte(`"truncated": true`)) {
		t.Errorf("the manifest does not report the truncation:\n%s", encoded)
	}
	if !bytes.Contains(encoded, []byte(`"truncation_reason": `+strconv.Quote(reason))) {
		t.Errorf("the manifest does not report the truncation reason:\n%s", encoded)
	}

	truncatedAt := bytes.Index(encoded, []byte(`"truncated"`))
	framesAt := bytes.Index(encoded, []byte(`"frames"`))
	if truncatedAt < 0 || framesAt < 0 || truncatedAt > framesAt {
		t.Errorf("the truncation fields are not rendered before the frames:\n%s", encoded)
	}
}

func TestManifestRendersNoDigestForFramesThatWereNeverCaptured(t *testing.T) {
	encoded, err := RenderManifest(BuildManifest(BurstReport{
		Interval: time.Millisecond,
		Frames: []Sample{
			{Scheduled: 0, Status: StatusMissed},
			{Scheduled: time.Millisecond, Status: StatusFailed, Error: "compositor refused"},
		},
	}))
	if err != nil {
		t.Fatalf("RenderManifest returned %v", err)
	}
	if bytes.Contains(encoded, []byte("sha256:0000")) {
		t.Errorf("a sample that was never captured rendered a fabricated digest:\n%s", encoded)
	}
	if got, want := bytes.Count(encoded, []byte(`"digest": ""`)), 2; got != want {
		t.Errorf("rendered %d empty digests, want %d:\n%s", got, want, encoded)
	}
}

func TestWriteManifestRequiresADestination(t *testing.T) {
	_, err := WriteManifest(nil, Manifest{})
	var failure *result.Failure
	if !errors.As(err, &failure) {
		t.Fatalf("WriteManifest returned %v, want a failure", err)
	}
	if failure.Code != result.CodeUsageError {
		t.Errorf("failure code %q, want %q", failure.Code, result.CodeUsageError)
	}
}
