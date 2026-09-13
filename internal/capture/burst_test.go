package capture

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/bnema/wlvision/internal/agentapi"
	"github.com/bnema/wlvision/internal/result"
)

// ------------------------------------------------------------------ the fakes

type fakeTimer struct {
	at time.Time
	ch chan time.Time
}

// fakeClock is a Clock that only moves when a test advances it.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Unix(1700000000, 0)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.timers = append(c.timers, &fakeTimer{at: c.now.Add(d), ch: ch})
	return ch
}

// Advance moves the clock and releases every timer that has come due.
func (c *fakeClock) Advance(d time.Duration) int {
	c.mu.Lock()
	c.now = c.now.Add(d)
	now := c.now
	kept := c.timers[:0]
	var due []*fakeTimer
	for _, timer := range c.timers {
		if timer.at.After(now) {
			kept = append(kept, timer)
		} else {
			due = append(due, timer)
		}
	}
	c.timers = kept
	c.mu.Unlock()

	for _, timer := range due {
		timer.ch <- now
	}
	return len(due)
}

// pending reports how many timers are waiting to be released.
func (c *fakeClock) pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.timers)
}

// drive releases the fake clock whenever the operation under test is waiting
// on it, until the operation delivers, and fails the test if that takes longer
// than a real-time budget.
func drive[T any](t *testing.T, clock *fakeClock, step time.Duration, done <-chan T) T {
	t.Helper()

	real := time.Now().Add(3 * time.Second)
	for {
		select {
		case got := <-done:
			return got
		default:
		}
		if time.Now().After(real) {
			t.Fatal("the operation did not finish within the test's real time budget")
		}
		if clock.pending() == 0 {
			time.Sleep(50 * time.Microsecond)
			continue
		}
		clock.Advance(step)
	}
}

// spin runs fn in a goroutine and drives the clock while it waits.
func spin[T any](t *testing.T, clock *fakeClock, step time.Duration, fn func() (T, error)) (T, error) {
	t.Helper()

	type outcome struct {
		value T
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		value, err := fn()
		done <- outcome{value: value, err: err}
	}()

	got := drive(t, clock, step, done)
	return got.value, got.err
}

// captureStep scripts one capture. Its zero value captures a fresh generated
// frame.
type captureStep struct {
	payload  []byte
	digest   string
	seq      uint64
	revision uint64
	width    int
	height   int
	format   string
	path     string
	err      error
	block    chan struct{}
}

// storedFrame is one frame the fake session holds: either its bytes, or a
// generator a test supplies, which lets a memory test keep the fake from
// retaining payloads on the burst's behalf.
type storedFrame struct {
	payload  []byte
	generate func() []byte
}

// fakeSource is a Capturer and Fetcher a test scripts capture by capture.
type fakeSource struct {
	mu sync.Mutex

	steps           []captureStep
	defaultPayload  []byte
	generatedFrames func(call int) []byte

	calls       int
	inflight    int
	maxInflight int
	lastSeq     uint64
	stored      map[string]storedFrame
	fetchErr    map[string]error
	started     chan struct{}
}

func newFakeSource() *fakeSource {
	return &fakeSource{
		stored:   map[string]storedFrame{},
		fetchErr: map[string]error{},
		started:  make(chan struct{}, 32),
	}
}

// Capture implements Capturer.
func (s *fakeSource) Capture(ctx context.Context) (Captured, error) {
	s.mu.Lock()
	index := s.calls
	s.calls++
	s.inflight++
	if s.inflight > s.maxInflight {
		s.maxInflight = s.inflight
	}
	var step captureStep
	if index < len(s.steps) {
		step = s.steps[index]
	}
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.inflight--
		s.mu.Unlock()
	}()
	select {
	case s.started <- struct{}{}:
	default:
	}

	if step.block != nil {
		select {
		case <-step.block:
		case <-ctx.Done():
			return Captured{}, ctx.Err()
		}
	}
	if step.err != nil {
		return Captured{}, step.err
	}

	s.mu.Lock()
	if step.seq != 0 {
		s.lastSeq = step.seq
	} else {
		s.lastSeq++
	}
	seq := s.lastSeq
	s.mu.Unlock()

	payload := step.payload
	if payload == nil {
		payload = s.defaultPayload
	}
	if payload == nil && s.generatedFrames != nil {
		payload = s.generatedFrames(index)
	}
	if payload == nil {
		payload = []byte(fmt.Sprintf("frame-%d", seq))
	}
	digest := step.digest
	if digest == "" {
		digest = DigestString(digestOf(payload))
	}
	width, height := step.width, step.height
	if width == 0 {
		width = 2
	}
	if height == 0 {
		height = 2
	}
	format := step.format
	if format == "" {
		format = FormatARGB8888.String()
	}
	path := step.path
	if path == "" {
		path = fmt.Sprintf("/session/export/frame-%d.png", index+1)
	}

	s.mu.Lock()
	if step.payload == nil && s.defaultPayload == nil && s.generatedFrames != nil {
		call := index
		s.stored[path] = storedFrame{generate: func() []byte { return s.generatedFrames(call) }}
	} else {
		s.stored[path] = storedFrame{payload: payload}
	}
	s.mu.Unlock()

	return Captured{
		Frame: agentapi.FrameResult{
			Path:             path,
			Digest:           digest,
			Width:            width,
			Height:           height,
			Format:           format,
			Sequence:         seq,
			CaptureRequestID: seq,
		},
		Revision: step.revision,
	}, nil
}

// Fetch implements Fetcher.
func (s *fakeSource) Fetch(_ context.Context, name string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.fetchErr[name]; err != nil {
		return nil, err
	}
	frame, ok := s.stored[name]
	if !ok {
		return nil, fmt.Errorf("capture: no stored frame %q", name)
	}
	if frame.generate != nil {
		return frame.generate(), nil
	}
	return append([]byte(nil), frame.payload...), nil
}

func (s *fakeSource) concurrentPeak() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxInflight
}

func (s *fakeSource) callsMade() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// memDestination is a Destination that keeps artifacts in memory.
type memDestination struct {
	mu    sync.Mutex
	names []string
	data  map[string][]byte
	errs  map[string]error
}

func newMemDestination() *memDestination {
	return &memDestination{data: map[string][]byte{}, errs: map[string]error{}}
}

func (d *memDestination) Store(name string, data []byte) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if err := d.errs[name]; err != nil {
		return "", err
	}
	d.names = append(d.names, name)
	d.data[name] = append([]byte(nil), data...)
	return "/host/export/" + name, nil
}

func (d *memDestination) artifacts() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.names...)
}

func (d *memDestination) artifact(name string) []byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]byte(nil), d.data[name]...)
}

func (d *memDestination) has(name string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.data[name]
	return ok
}

// fakeObserver is an Observer a test scripts with a sequence of states.
type fakeObserver struct {
	mu sync.Mutex

	states      []agentapi.State
	index       int
	snapshotErr error
	exitedAfter *int
	exitCode    int
	snapshots   int
	exits       int
}

func (o *fakeObserver) Snapshot(context.Context) (agentapi.State, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.snapshots++
	if o.snapshotErr != nil {
		return agentapi.State{}, o.snapshotErr
	}
	if len(o.states) == 0 {
		return agentapi.State{}, nil
	}
	state := o.states[o.index]
	if o.index < len(o.states)-1 {
		o.index++
	}
	return state, nil
}

func (o *fakeObserver) Exited(context.Context, time.Time) (bool, int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.exits++
	if o.exitedAfter == nil || o.exits < *o.exitedAfter {
		return false, 0, nil
	}
	return true, o.exitCode, nil
}

func burstDeps(clock *fakeClock, src *fakeSource, dst *memDestination) BurstDeps {
	return BurstDeps{Clock: clock, Capturer: src, Fetcher: src, Destination: dst}
}

// sizeDestination records what a burst stored without keeping the frames, so a
// memory test measures the burst's retention rather than the destination's.
type sizeDestination struct {
	mu      sync.Mutex
	names   []string
	kept    map[string][]byte
	keep    func(name string) bool
	peak    int64
	onStore func()
}

func newSizeDestination() *sizeDestination {
	return &sizeDestination{kept: map[string][]byte{}}
}

func (d *sizeDestination) Store(name string, data []byte) (string, error) {
	if d.onStore != nil {
		d.onStore()
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	d.names = append(d.names, name)
	if d.keep != nil && d.keep(name) {
		d.kept[name] = append([]byte(nil), data...)
	}
	return "/host/export/" + name, nil
}

// recordPeak samples the live heap, collecting first so only what is really
// retained is measured.
func (d *sizeDestination) recordPeak() {
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)

	d.mu.Lock()
	defer d.mu.Unlock()
	if int64(stats.HeapAlloc) > d.peak {
		d.peak = int64(stats.HeapAlloc)
	}
}

func (d *sizeDestination) peakBytes() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.peak
}

func (d *sizeDestination) artifact(name string) []byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]byte(nil), d.kept[name]...)
}

// bulkFrame builds a frame whose png is about as large as its pixels, so a
// burst that kept the payload would be easy to see in the heap. It stays cheap
// to rebuild and to compress once it is cropped, which keeps the test fast
// however many times it runs.
func bulkFrame(width, height int, seed uint32) []byte {
	fill := color.RGBA{R: uint8(seed), G: 0x80, B: uint8(seed * 3), A: 0xff}
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(img, img.Bounds(), image.NewUniform(fill), image.Point{}, draw.Src)

	var buffer bytes.Buffer
	encoder := png.Encoder{CompressionLevel: png.NoCompression}
	if err := encoder.Encode(&buffer, img); err != nil {
		panic(fmt.Sprintf("encoding a bulk frame: %v", err))
	}
	return buffer.Bytes()
}

// decodeSheet decodes a png the burst stored.
func decodeSheet(t *testing.T, data []byte) *image.RGBA {
	t.Helper()

	decoded, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decoding the sheet: %v", err)
	}
	rgba, ok := decoded.(*image.RGBA)
	if !ok {
		t.Fatalf("the sheet decoded as %T, want *image.RGBA", decoded)
	}
	return rgba
}

// --------------------------------------------------------------- burst tests

func TestBurstExactDeadlines(t *testing.T) {
	clock := newFakeClock()
	src := newFakeSource()
	dst := newMemDestination()
	plan := BurstPlan{Interval: 20 * time.Millisecond, Duration: 100 * time.Millisecond}

	report, err := spin(t, clock, plan.Interval, func() (BurstReport, error) {
		return Burst(context.Background(), plan, burstDeps(clock, src, dst))
	})
	if err != nil {
		t.Fatalf("Burst returned %v", err)
	}

	if len(report.Frames) != 5 {
		t.Fatalf("scheduled %d frames, want 5", len(report.Frames))
	}
	for index, sample := range report.Frames {
		want := time.Duration(index) * plan.Interval
		if sample.Scheduled != want {
			t.Errorf("frame %d scheduled at %s, want %s", index, sample.Scheduled, want)
		}
		if sample.Requested != want {
			t.Errorf("frame %d requested at %s, want %s", index, sample.Requested, want)
		}
		if sample.Completed != want {
			t.Errorf("frame %d completed at %s, want %s", index, sample.Completed, want)
		}
		if sample.Status != StatusCaptured {
			t.Errorf("frame %d status %q, want %q", index, sample.Status, StatusCaptured)
		}
		wantPath := fmt.Sprintf("/host/export/frame-%04d.png", index+1)
		if sample.Path != wantPath {
			t.Errorf("frame %d path %q, want %q", index, sample.Path, wantPath)
		}
	}
	if report.Captured != 5 || report.Failed != 0 || report.Missed != 0 || report.Deduplicated != 0 {
		t.Errorf("counters captured=%d failed=%d missed=%d deduplicated=%d",
			report.Captured, report.Failed, report.Missed, report.Deduplicated)
	}
	if report.Bytes <= 0 {
		t.Errorf("bytes %d, want a positive count", report.Bytes)
	}
	if report.ManifestPath != "/host/export/manifest.json" {
		t.Errorf("manifest path %q, want the destination's host path", report.ManifestPath)
	}
	if names := dst.artifacts(); names[len(names)-1] != "manifest.json" {
		t.Errorf("artifact order %v, want the manifest written last", names)
	}
	if src.concurrentPeak() != 1 {
		t.Errorf("%d captures ran concurrently, want at most one in flight", src.concurrentPeak())
	}
}

func TestBurstRecordsMissedSlotWithoutDistortingCadence(t *testing.T) {
	clock := newFakeClock()
	src := newFakeSource()
	release := make(chan struct{})
	src.steps = []captureStep{{block: release}}
	dst := newMemDestination()
	plan := BurstPlan{Interval: 100 * time.Millisecond, Duration: 500 * time.Millisecond}

	type outcome struct {
		report BurstReport
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		report, err := Burst(context.Background(), plan, burstDeps(clock, src, dst))
		done <- outcome{report: report, err: err}
	}()

	select {
	case <-src.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the first capture never started")
	}
	// The first capture stays in flight well past the second deadline.
	clock.Advance(150 * time.Millisecond)
	close(release)

	got := drive(t, clock, plan.Interval, done)
	if got.err != nil {
		t.Fatalf("Burst returned %v", got.err)
	}
	report := got.report

	if len(report.Frames) != 5 {
		t.Fatalf("scheduled %d frames, want 5", len(report.Frames))
	}
	if report.Missed != 1 {
		t.Fatalf("recorded %d missed samples, want exactly 1", report.Missed)
	}
	if report.Frames[1].Status != StatusMissed {
		t.Fatalf("frame 1 status %q, want %q", report.Frames[1].Status, StatusMissed)
	}
	if report.Frames[1].Requested != 0 || report.Frames[1].Completed != 0 {
		t.Errorf("a missed sample carries times %s/%s, want zero",
			report.Frames[1].Requested, report.Frames[1].Completed)
	}
	if report.Frames[1].Scheduled != plan.Interval {
		t.Errorf("missed frame scheduled at %s, want %s", report.Frames[1].Scheduled, plan.Interval)
	}
	for index, sample := range report.Frames {
		want := time.Duration(index) * plan.Interval
		if sample.Scheduled != want {
			t.Errorf("frame %d scheduled at %s, want %s, so the cadence was distorted", index, sample.Scheduled, want)
		}
		if sample.Status != StatusMissed && sample.Requested < want {
			t.Errorf("frame %d requested at %s, before its deadline %s", index, sample.Requested, want)
		}
	}
	if src.concurrentPeak() != 1 {
		t.Errorf("%d captures ran concurrently, want at most one in flight", src.concurrentPeak())
	}
}

func TestBurstDeduplicatesIdenticalFrames(t *testing.T) {
	clock := newFakeClock()
	src := newFakeSource()
	src.steps = []captureStep{
		{payload: []byte("same")},
		{payload: []byte("same")},
		{payload: []byte("other")},
	}
	dst := newMemDestination()
	plan := BurstPlan{Interval: 20 * time.Millisecond, Duration: 60 * time.Millisecond}

	report, err := spin(t, clock, plan.Interval, func() (BurstReport, error) {
		return Burst(context.Background(), plan, burstDeps(clock, src, dst))
	})
	if err != nil {
		t.Fatalf("Burst returned %v", err)
	}

	if report.Captured != 2 || report.Deduplicated != 1 {
		t.Fatalf("captured=%d deduplicated=%d, want 2 and 1", report.Captured, report.Deduplicated)
	}
	if report.Frames[0].Status != StatusCaptured || report.Frames[1].Status != StatusDeduplicated {
		t.Fatalf("statuses %q/%q, want captured/deduplicated",
			report.Frames[0].Status, report.Frames[1].Status)
	}
	if report.Frames[1].Path != report.Frames[0].Path {
		t.Errorf("a deduplicated frame points at %q, want the stored frame %q",
			report.Frames[1].Path, report.Frames[0].Path)
	}
	if report.Frames[1].Digest != report.Frames[0].Digest {
		t.Errorf("a deduplicated frame carries another digest")
	}
	if report.Frames[2].Status != StatusCaptured {
		t.Errorf("frame 2 status %q, want %q", report.Frames[2].Status, StatusCaptured)
	}
	wantBytes := int64(len("same") + len("other"))
	if report.Bytes != wantBytes {
		t.Errorf("stored %d bytes, want %d", report.Bytes, wantBytes)
	}
	wantNames := []string{"frame-0001.png", "frame-0003.png", "manifest.json"}
	if got := dst.artifacts(); !equalStrings(got, wantNames) {
		t.Errorf("artifacts %v, want %v", got, wantNames)
	}
}

func TestBurstFailedSamplesDoNotAbortTheBurst(t *testing.T) {
	clock := newFakeClock()
	src := newFakeSource()
	src.steps = []captureStep{
		{path: "/session/export/a.png"},
		{err: errors.New("compositor refused")},
		{path: "/session/export/c.png", digest: "not-a-digest"},
		{path: "/session/export/d.png"},
		{path: "/session/export/e.png", digest: DigestString([32]byte{0xaa})},
	}
	src.fetchErr["/session/export/d.png"] = errors.New("the export is missing")
	dst := newMemDestination()
	plan := BurstPlan{Interval: 20 * time.Millisecond, Duration: 120 * time.Millisecond}

	report, err := spin(t, clock, plan.Interval, func() (BurstReport, error) {
		return Burst(context.Background(), plan, burstDeps(clock, src, dst))
	})
	if err != nil {
		t.Fatalf("a failed sample aborted the burst: %v", err)
	}

	want := []Status{StatusCaptured, StatusFailed, StatusFailed, StatusFailed, StatusFailed, StatusCaptured}
	if len(report.Frames) != len(want) {
		t.Fatalf("scheduled %d frames, want %d", len(report.Frames), len(want))
	}
	for index, status := range want {
		if report.Frames[index].Status != status {
			t.Errorf("frame %d status %q, want %q", index, report.Frames[index].Status, status)
		}
		if status == StatusFailed && report.Frames[index].Error == "" {
			t.Errorf("frame %d failed without an error", index)
		}
	}
	if report.Captured != 2 || report.Failed != 4 {
		t.Errorf("captured=%d failed=%d, want 2 and 4", report.Captured, report.Failed)
	}
	if report.ManifestPath == "" {
		t.Errorf("the burst did not publish its manifest")
	}
}

func TestBurstAssociatesRequestFrameAndRevision(t *testing.T) {
	clock := newFakeClock()
	src := newFakeSource()
	src.steps = []captureStep{
		{seq: 7, revision: 3},
		{seq: 9, revision: 4},
		{seq: 11, revision: 5},
	}
	dst := newMemDestination()
	plan := BurstPlan{Interval: 20 * time.Millisecond, Duration: 60 * time.Millisecond}

	report, err := spin(t, clock, plan.Interval, func() (BurstReport, error) {
		return Burst(context.Background(), plan, burstDeps(clock, src, dst))
	})
	if err != nil {
		t.Fatalf("Burst returned %v", err)
	}

	wantSeq := []uint64{7, 9, 11}
	wantRevision := []uint64{3, 4, 5}
	for index := range report.Frames {
		sample := report.Frames[index]
		if sample.RequestID != wantSeq[index] {
			t.Errorf("frame %d request id %d, want %d", index, sample.RequestID, wantSeq[index])
		}
		if sample.FrameSeq != wantSeq[index] {
			t.Errorf("frame %d frame sequence %d, want %d", index, sample.FrameSeq, wantSeq[index])
		}
		if sample.Revision != wantRevision[index] {
			t.Errorf("frame %d revision %d, want %d", index, sample.Revision, wantRevision[index])
		}
	}
}

func TestBurstStopsAtTheFrameCap(t *testing.T) {
	clock := newFakeClock()
	src := newFakeSource()
	dst := newMemDestination()
	plan := BurstPlan{Interval: 20 * time.Millisecond, Duration: time.Second, MaxFrames: 3}

	report, err := spin(t, clock, plan.Interval, func() (BurstReport, error) {
		return Burst(context.Background(), plan, burstDeps(clock, src, dst))
	})
	if err != nil {
		t.Fatalf("Burst returned %v", err)
	}

	if len(report.Frames) != 3 {
		t.Fatalf("scheduled %d frames, want the 3 frame cap", len(report.Frames))
	}
	if src.callsMade() != 3 {
		t.Errorf("made %d captures, want 3", src.callsMade())
	}
	for index, sample := range report.Frames {
		if sample.Scheduled != time.Duration(index)*plan.Interval {
			t.Errorf("frame %d scheduled at %s, want a fixed %s cadence",
				index, sample.Scheduled, plan.Interval)
		}
	}
}

func TestBurstStopsAndReportsWhenTheByteCapIsReached(t *testing.T) {
	clock := newFakeClock()
	src := newFakeSource()
	src.steps = []captureStep{
		{payload: []byte("aaaaa")},
		{payload: []byte("bbbbb")},
		{payload: []byte("ccccc")},
	}
	dst := newMemDestination()
	// bytes is the unexported per-plan cap, so the test lowers the burst's cap
	// without touching state another test could see.
	plan := BurstPlan{Interval: 20 * time.Millisecond, Duration: 100 * time.Millisecond, bytes: 8}

	report, err := spin(t, clock, plan.Interval, func() (BurstReport, error) {
		return Burst(context.Background(), plan, burstDeps(clock, src, dst))
	})
	if err != nil {
		t.Fatalf("Burst returned %v", err)
	}

	if !report.Truncated {
		t.Fatal("the burst did not report truncation")
	}
	if report.TruncationReason == "" {
		t.Error("truncation was reported without a reason")
	}
	if len(report.Frames) != 2 {
		t.Fatalf("scheduled %d frames, want the burst to stop at the second", len(report.Frames))
	}
	if report.Frames[0].Status != StatusCaptured {
		t.Errorf("frame 0 status %q, want %q", report.Frames[0].Status, StatusCaptured)
	}
	if report.Frames[1].Status != StatusFailed || report.Frames[1].Error == "" {
		t.Errorf("the truncating frame is %q/%q, want a failed sample with a reason",
			report.Frames[1].Status, report.Frames[1].Error)
	}
	if report.Failed != 1 || report.Missed != 0 {
		t.Errorf("failed=%d missed=%d, want 1 and 0", report.Failed, report.Missed)
	}
	if dst.has("frame-0002.png") {
		t.Error("a frame beyond the byte cap was stored anyway")
	}
	wantNames := []string{"frame-0001.png", "manifest.json"}
	if got := dst.artifacts(); !equalStrings(got, wantNames) {
		t.Errorf("artifacts %v, want %v", got, wantNames)
	}
}

func TestBurstBuildsContactSheetBeforeManifest(t *testing.T) {
	clock := newFakeClock()
	src := newFakeSource()
	dst := newMemDestination()
	plan := BurstPlan{Interval: 20 * time.Millisecond, Duration: 100 * time.Millisecond, ContactSheet: true}

	report, err := spin(t, clock, plan.Interval, func() (BurstReport, error) {
		return Burst(context.Background(), plan, burstDeps(clock, src, dst))
	})
	if err != nil {
		t.Fatalf("Burst returned %v", err)
	}

	if report.ContactSheetPath != "/host/export/contact.png" {
		t.Errorf("contact sheet path %q, want the destination's host path", report.ContactSheetPath)
	}
	wantNames := []string{
		"frame-0001.png", "frame-0002.png", "frame-0003.png", "frame-0004.png", "frame-0005.png",
		"contact.png", "manifest.json",
	}
	if got := dst.artifacts(); !equalStrings(got, wantNames) {
		t.Errorf("artifacts %v, want frames then sheet then manifest %v", got, wantNames)
	}

	decoded, err := png.Decode(bytes.NewReader(dst.artifact("contact.png")))
	if err != nil {
		t.Fatalf("the contact sheet is not a png: %v", err)
	}
	want := image.Rect(0, 0, SheetColumns*SheetCellWidth, SheetCellHeight)
	if !decoded.Bounds().Eq(want) {
		t.Errorf("contact sheet bounds %v, want %v", decoded.Bounds(), want)
	}
}

func TestBurstUsageErrors(t *testing.T) {
	clock := newFakeClock()
	src := newFakeSource()
	dst := newMemDestination()
	good := func() BurstDeps { return burstDeps(clock, src, dst) }

	cases := []struct {
		name string
		plan BurstPlan
		deps BurstDeps
	}{
		{"nil clock", BurstPlan{}, BurstDeps{Capturer: src, Fetcher: src, Destination: dst}},
		{"nil capturer", BurstPlan{}, BurstDeps{Clock: clock, Fetcher: src, Destination: dst}},
		{"nil fetcher", BurstPlan{}, BurstDeps{Clock: clock, Capturer: src, Destination: dst}},
		{"nil destination", BurstPlan{}, BurstDeps{Clock: clock, Capturer: src, Fetcher: src}},
		{"interval below the minimum", BurstPlan{Interval: time.Millisecond}, good()},
		{"duration above the maximum", BurstPlan{Duration: MaxDuration + time.Second}, good()},
		{"too many frames", BurstPlan{MaxFrames: MaxFrames + 1}, good()},
		{"negative interval", BurstPlan{Interval: -time.Second}, good()},
		{"negative duration", BurstPlan{Duration: -time.Second}, good()},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report, err := Burst(context.Background(), tc.plan, tc.deps)
			var failure *result.Failure
			if !errors.As(err, &failure) {
				t.Fatalf("Burst returned %v, want a failure", err)
			}
			if failure.Code != result.CodeUsageError {
				t.Errorf("failure code %q, want %q", failure.Code, result.CodeUsageError)
			}
			if report.ManifestPath != "" || len(dst.artifacts()) != 0 {
				t.Errorf("a refused burst wrote artifacts %v", dst.artifacts())
			}
		})
	}
}

func TestBurstCancellationReturnsTheContextError(t *testing.T) {
	clock := newFakeClock()
	src := newFakeSource()
	src.steps = []captureStep{{block: make(chan struct{})}}
	dst := newMemDestination()
	plan := BurstPlan{Interval: 20 * time.Millisecond, Duration: 100 * time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := Burst(ctx, plan, burstDeps(clock, src, dst))
		done <- err
	}()

	select {
	case <-src.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the first capture never started")
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Burst returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Burst did not return after cancellation")
	}
	if dst.has("manifest.json") {
		t.Error("a cancelled burst published a manifest")
	}
}

func TestBurstContactSheetShowsADuplicateInsteadOfAPlaceholder(t *testing.T) {
	clock := newFakeClock()
	src := newFakeSource()
	red := solidPNG(t, 8, 8, color.RGBA{R: 0xff, A: 0xff})
	blue := solidPNG(t, 8, 8, color.RGBA{B: 0xff, A: 0xff})
	src.steps = []captureStep{{payload: red}, {payload: red}, {payload: blue}}
	dst := newMemDestination()
	plan := BurstPlan{Interval: 20 * time.Millisecond, Duration: 60 * time.Millisecond, ContactSheet: true}

	report, err := spin(t, clock, plan.Interval, func() (BurstReport, error) {
		return Burst(context.Background(), plan, burstDeps(clock, src, dst))
	})
	if err != nil {
		t.Fatalf("Burst returned %v", err)
	}

	if report.Deduplicated != 1 {
		t.Fatalf("deduplicated %d frames, want 1", report.Deduplicated)
	}
	if report.Frames[1].Path != report.Frames[0].Path {
		t.Fatalf("the duplicate points at %q, want the frame that stored the picture %q",
			report.Frames[1].Path, report.Frames[0].Path)
	}
	// The artifact number is the sample's schedule position, the same number
	// its cell shows, so the duplicate occupies a position with no file of its
	// own while the third frame is named for the third position.
	if report.Frames[0].Path != "/host/export/frame-0001.png" {
		t.Errorf("the first frame is stored as %q, want the schedule position 1", report.Frames[0].Path)
	}
	if report.Frames[2].Path != "/host/export/frame-0003.png" {
		t.Errorf("the third frame is stored as %q, want the schedule position 3", report.Frames[2].Path)
	}

	sheet := decodeSheet(t, dst.artifact("contact.png"))
	if got := sheet.RGBAAt(0, 0); got != (color.RGBA{R: 0xff, A: 0xff}) {
		t.Errorf("cell 1 starts with %v, want the stored red frame", got)
	}
	duplicate := sheet.RGBAAt(SheetCellWidth, 0)
	if duplicate == sheetPlaceholder {
		t.Error("the duplicate's cell is a placeholder, want the frame that stored the picture")
	}
	if duplicate != (color.RGBA{R: 0xff, A: 0xff}) {
		t.Errorf("the duplicate's cell starts with %v, want the stored red frame", duplicate)
	}
	if got := sheet.RGBAAt(2*SheetCellWidth, 0); got != (color.RGBA{B: 0xff, A: 0xff}) {
		t.Errorf("cell 3 starts with %v, want the blue frame", got)
	}
}

func TestBurstKeepsContactSheetCellsInsteadOfWholeFrames(t *testing.T) {
	const (
		frameCount  = 4
		frameWidth  = 1024
		frameHeight = 1024
		maxRetained = 12 << 20
	)

	clock := newFakeClock()
	src := newFakeSource()
	src.generatedFrames = func(call int) []byte {
		return bulkFrame(frameWidth, frameHeight, uint32(call)+1)
	}
	dst := newSizeDestination()
	dst.keep = func(name string) bool { return name == burstContactSheetName }
	// Generating and decoding real frames takes real time, so the clock is
	// advanced one interval per stored artifact rather than by a pump: every
	// sample then stays on its deadline whatever the machine is doing.
	dst.onStore = func() {
		clock.Advance(MinInterval)
		dst.recordPeak()
	}

	plan := BurstPlan{
		Interval:     MinInterval,
		Duration:     time.Duration(frameCount) * MinInterval,
		MaxFrames:    frameCount,
		ContactSheet: true,
	}

	type outcome struct {
		report BurstReport
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		report, err := Burst(context.Background(), plan,
			BurstDeps{Clock: clock, Capturer: src, Fetcher: src, Destination: dst})
		done <- outcome{report: report, err: err}
	}()

	var report BurstReport
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("Burst returned %v", got.err)
		}
		report = got.report
	case <-time.After(30 * time.Second):
		t.Fatal("the burst did not finish")
	}
	if report.Captured != frameCount {
		t.Fatalf("captured %d frames, want %d", report.Captured, frameCount)
	}
	if report.ContactSheetPath == "" {
		t.Fatal("the burst built no contact sheet")
	}

	if peak := dst.peakBytes(); peak > maxRetained {
		t.Errorf("the burst held %d bytes at its peak while storing %d frames of about %d bytes each; "+
			"keeping one cropped cell per frame should stay well below, so the payloads are still retained",
			peak, frameCount, frameWidth*frameHeight*4)
	}

	// The cells must carry the frames rather than placeholders, so the bound
	// above really is about cropped frames and not about storing nothing.
	sheet := decodeSheet(t, dst.artifact(burstContactSheetName))
	for index := 0; index < frameCount; index++ {
		column := index % SheetColumns
		row := index / SheetColumns
		if got := sheet.RGBAAt(column*SheetCellWidth, row*SheetCellHeight); got == sheetPlaceholder {
			t.Errorf("cell %d is a placeholder, so the burst kept no cropped frame", index)
		}
	}
}

// equalStrings reports whether two string slices hold the same values in the
// same order.
func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}
