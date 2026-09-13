package inject

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/bnema/wlvision/internal/result"
)

// failureOf asserts that err is a *result.Failure with the given code and a
// non-empty details["reason"], and returns it.
func failureOf(t *testing.T, err error, code result.Code) *result.Failure {
	t.Helper()
	if err == nil {
		t.Fatalf("Receive returned no error, want a %s failure", code)
	}
	var failure *result.Failure
	if !errors.As(err, &failure) {
		t.Fatalf("Receive error is %T (%v), want *result.Failure", err, err)
	}
	if failure.Code != code {
		t.Fatalf("failure code is %q, want %q (message: %s)", failure.Code, code, failure.Message)
	}
	if failure.Details["reason"] == "" {
		t.Fatalf("failure %q carries no details[\"reason\"]", failure.Message)
	}
	return failure
}

// namesIn returns the sorted names directly inside dir.
func namesIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}

// assertPayloadDir asserts dir holds exactly want and no staging leftovers. A
// rejected payload must leave the directory as it was, minus any staging
// entry, so tests seed dir with a sentinel when they need one.
func assertPayloadDir(t *testing.T, dir string, want ...string) {
	t.Helper()
	got := namesIn(t, dir)
	visible := make([]string, 0, len(got))
	for _, name := range got {
		if strings.HasPrefix(name, stagingPrefix) {
			t.Errorf("payload directory %s still holds staging entry %q", dir, name)
			continue
		}
		visible = append(visible, name)
	}
	sort.Strings(want)
	if !slices.Equal(visible, want) {
		t.Errorf("payload directory %s holds %v, want %v", dir, visible, want)
	}
}

// digestOf returns the contract digest of data.
func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// tarEntry is one entry of a test archive. body is only used for regular
// files.
type tarEntry struct {
	header tar.Header
	body   []byte
}

func fileEntry(name string, mode int64, body string) tarEntry {
	return tarEntry{
		header: tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: mode},
		body:   []byte(body),
	}
}

func dirEntry(name string, mode int64) tarEntry {
	return tarEntry{header: tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: mode}}
}

// buildTar encodes entries with archive/tar, the same way a caller would.
func buildTar(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	for _, entry := range entries {
		header := entry.header
		regular := header.Typeflag == tar.TypeReg || header.Typeflag == tar.TypeRegA
		if regular {
			header.Size = int64(len(entry.body))
		} else {
			header.Size = 0
		}
		if err := writer.WriteHeader(&header); err != nil {
			t.Fatalf("tar WriteHeader(%q): %v", header.Name, err)
		}
		if regular && len(entry.body) > 0 {
			if _, err := writer.Write(entry.body); err != nil {
				t.Fatalf("tar Write(%q): %v", header.Name, err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("tar Close: %v", err)
	}
	return buffer.Bytes()
}

func TestReceiveBinaryValid(t *testing.T) {
	t.Parallel()
	source := []byte("#!/bin/sh\necho wlvision\n")
	cases := []struct {
		name string
		mode uint32
		want os.FileMode
	}{
		{"0o777 is reduced to 0o700", 0o777, 0o700},
		{"0o4755 loses setuid", 0o4755, 0o700},
		{"0o644 is reduced to 0o600", 0o644, 0o600},
		{"0o600 is kept", 0o600, 0o600},
		{"0o700 is kept", 0o700, 0o700},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			res, err := Receive(context.Background(), dir, Spec{
				Kind:     KindBinary,
				Name:     "payload.bin",
				Mode:     tc.mode,
				MaxBytes: int64(len(source)),
			}, bytes.NewReader(source))
			if err != nil {
				t.Fatalf("Receive: %v", err)
			}
			if want := filepath.Join(dir, "payload.bin"); res.Path != want {
				t.Errorf("Path is %q, want %q", res.Path, want)
			}
			if res.Bytes != int64(len(source)) {
				t.Errorf("Bytes is %d, want %d", res.Bytes, len(source))
			}
			if res.Files != 1 {
				t.Errorf("Files is %d, want 1", res.Files)
			}
			if want := digestOf(source); res.Digest != want {
				t.Errorf("Digest is %q, want %q", res.Digest, want)
			}
			got, err := os.ReadFile(res.Path)
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			if !bytes.Equal(got, source) {
				t.Errorf("installed bytes are %q, want %q", got, source)
			}
			info, err := os.Stat(res.Path)
			if err != nil {
				t.Fatalf("Stat: %v", err)
			}
			if perm := info.Mode().Perm(); perm != tc.want {
				t.Errorf("mode is %#o, want %#o", perm, tc.want)
			}
			if info.Mode()&os.ModeSetuid != 0 {
				t.Errorf("mode %v kept setuid", info.Mode())
			}
			assertPayloadDir(t, dir, "payload.bin")
		})
	}
}

func TestReceiveBinaryExactlyAtLimit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	source := []byte("0123456789")
	res, err := Receive(context.Background(), dir, Spec{
		Kind: KindBinary, Name: "exact", MaxBytes: int64(len(source)),
	}, bytes.NewReader(source))
	if err != nil {
		t.Fatalf("Receive at the exact limit: %v", err)
	}
	if res.Bytes != int64(len(source)) {
		t.Errorf("Bytes is %d, want %d", res.Bytes, len(source))
	}
	assertPayloadDir(t, dir, "exact")
}

func TestReceiveBinaryOverLimitRejected(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	source := []byte("0123456789")
	_, err := Receive(context.Background(), dir, Spec{
		Kind: KindBinary, Name: "too-big", MaxBytes: int64(len(source)) - 1,
	}, bytes.NewReader(source))
	failureOf(t, err, result.CodePayloadRejected)
	if _, err := os.Stat(filepath.Join(dir, "too-big")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("rejected payload left %s behind (stat error %v)", filepath.Join(dir, "too-big"), err)
	}
	assertPayloadDir(t, dir)
}

func TestReceiveTarValid(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	archive := buildTar(t,
		dirEntry("bin/", 0o755),
		fileEntry("bin/run", 0o755, "#!/bin/sh\necho run\n"),
		fileEntry("notes.txt", 0o644, "notes\n"),
	)
	res, err := Receive(context.Background(), dir, Spec{
		Kind: KindTar, Name: "bundle", MaxBytes: 1 << 20, MaxFiles: 16,
	}, bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if want := filepath.Join(dir, "bundle"); res.Path != want {
		t.Errorf("Path is %q, want %q", res.Path, want)
	}
	if res.Files != 2 {
		t.Errorf("Files is %d, want 2", res.Files)
	}
	if res.Bytes != int64(len(archive)) {
		t.Errorf("Bytes is %d, want %d", res.Bytes, len(archive))
	}
	if want := digestOf(archive); res.Digest != want {
		t.Errorf("Digest is %q, want %q", res.Digest, want)
	}
	if got := namesIn(t, res.Path); !slices.Equal(got, []string{"bin", "notes.txt"}) {
		t.Errorf("bundle holds %v, want [bin notes.txt]", got)
	}
	assertMode(t, res.Path, 0o700)
	assertMode(t, filepath.Join(res.Path, "bin"), 0o700)
	assertMode(t, filepath.Join(res.Path, "bin", "run"), 0o700)
	assertMode(t, filepath.Join(res.Path, "notes.txt"), 0o600)
	assertFileContent(t, filepath.Join(res.Path, "bin", "run"), "#!/bin/sh\necho run\n")
	assertFileContent(t, filepath.Join(res.Path, "notes.txt"), "notes\n")
	assertPayloadDir(t, dir, "bundle")
}

func TestReceiveTarRejectsHostileArchives(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		entries  []tarEntry
		maxBytes int64
		maxFiles int
	}{
		{
			name:    "absolute entry name",
			entries: []tarEntry{fileEntry("/etc/passwd", 0o644, "root:x:0:0\n")},
		},
		{
			name:    "parent directory element",
			entries: []tarEntry{fileEntry("../escape", 0o644, "pwned\n")},
		},
		{
			name:    "nested parent directory element",
			entries: []tarEntry{fileEntry("bundle/../../escape", 0o644, "pwned\n")},
		},
		{
			name: "symlink entry",
			entries: []tarEntry{{header: tar.Header{
				Name: "escape-link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd",
			}}},
		},
		{
			name: "hard link entry",
			entries: []tarEntry{{header: tar.Header{
				Name: "hard-link", Typeflag: tar.TypeLink, Linkname: "bundle/run",
			}}},
		},
		{
			name: "device node entry",
			entries: []tarEntry{{header: tar.Header{
				Name: "bundle/null", Typeflag: tar.TypeChar, Mode: 0o600, Devmajor: 1, Devminor: 3,
			}}},
		},
		{
			name: "fifo entry",
			entries: []tarEntry{{header: tar.Header{
				Name: "bundle/pipe", Typeflag: tar.TypeFifo, Mode: 0o600,
			}}},
		},
		{
			name: "irregular entry type",
			entries: []tarEntry{{header: tar.Header{
				Name: "bundle/weird", Typeflag: 'Z', Mode: 0o600,
			}}},
		},
		{
			name:    "duplicate entry name",
			entries: []tarEntry{fileEntry("bundle/app", 0o755, "one\n"), fileEntry("bundle/app", 0o755, "two\n")},
		},
		{
			name:    "file inside an earlier file",
			entries: []tarEntry{fileEntry("bundle", 0o644, "one\n"), fileEntry("bundle/app", 0o644, "two\n")},
		},
		{
			name:    "file enclosing an earlier entry",
			entries: []tarEntry{fileEntry("bundle/app", 0o644, "one\n"), fileEntry("bundle", 0o644, "two\n")},
		},
		{
			name:    "setuid mode",
			entries: []tarEntry{fileEntry("bundle/setuid", 0o4755, "x\n")},
		},
		{
			name:    "setgid mode",
			entries: []tarEntry{fileEntry("bundle/setgid", 0o2755, "x\n")},
		},
		{
			name:    "sticky mode",
			entries: []tarEntry{fileEntry("bundle/sticky", 0o1777, "x\n")},
		},
		{
			name:    "directory with setgid mode",
			entries: []tarEntry{dirEntry("bundle/", 0o2775)},
		},
		{
			name:     "declared size exceeds MaxBytes",
			entries:  []tarEntry{{header: tar.Header{Name: "bundle/huge", Typeflag: tar.TypeReg, Mode: 0o600}, body: make([]byte, 8192)}},
			maxBytes: 1024,
		},
		{
			name:     "archive bytes exceed MaxBytes",
			entries:  []tarEntry{fileEntry("bundle/tiny", 0o600, "x")},
			maxBytes: 512,
		},
		{
			name:     "entry count exceeds MaxFiles",
			entries:  []tarEntry{fileEntry("bundle/one", 0o600, "1\n"), fileEntry("bundle/two", 0o600, "2\n")},
			maxFiles: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			// A pre-existing file proves rejection never disturbs the payload
			// directory beyond removing the staging entry.
			if err := os.WriteFile(filepath.Join(dir, "existing"), []byte("keep\n"), 0o600); err != nil {
				t.Fatalf("seed payload directory: %v", err)
			}
			spec := Spec{Kind: KindTar, Name: "bundle", MaxBytes: 1 << 20, MaxFiles: 16}
			if tc.maxBytes != 0 {
				spec.MaxBytes = tc.maxBytes
			}
			if tc.maxFiles != 0 {
				spec.MaxFiles = tc.maxFiles
			}
			archive := buildTar(t, tc.entries...)
			_, err := Receive(context.Background(), dir, spec, bytes.NewReader(archive))
			failureOf(t, err, result.CodePayloadRejected)
			assertPayloadDir(t, dir, "existing")
			if _, statErr := os.Stat(filepath.Join(dir, "bundle")); !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("rejected archive left %s behind (stat error %v)", filepath.Join(dir, "bundle"), statErr)
			}
		})
	}
}

func TestReceiveTarSymlinkCannotEscape(t *testing.T) {
	t.Parallel()
	outside := t.TempDir()
	dir := t.TempDir()
	archive := buildTar(t,
		tarEntry{header: tar.Header{Name: "pwn", Typeflag: tar.TypeSymlink, Linkname: outside}},
		fileEntry("pwn/evil.txt", 0o600, "pwned\n"),
	)
	_, err := Receive(context.Background(), dir, Spec{
		Kind: KindTar, Name: "bundle", MaxBytes: 1 << 20, MaxFiles: 16,
	}, bytes.NewReader(archive))
	failureOf(t, err, result.CodePayloadRejected)
	if _, err := os.Stat(filepath.Join(outside, "evil.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("symlink escape created %s (stat error %v)", filepath.Join(outside, "evil.txt"), err)
	}
	if names := namesIn(t, outside); len(names) != 0 {
		t.Errorf("symlink target directory holds %v, want nothing", names)
	}
	assertPayloadDir(t, dir)
}

func TestReceiveTarTruncatedArchive(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	archive := buildTar(t, fileEntry("bundle/app", 0o755, strings.Repeat("x", 4096)))
	// Cut in the middle of the file body, after its header.
	truncated := archive[:512+1024]
	_, err := Receive(context.Background(), dir, Spec{
		Kind: KindTar, Name: "bundle", MaxBytes: 1 << 20, MaxFiles: 16,
	}, bytes.NewReader(truncated))
	failure := failureOf(t, err, result.CodePayloadRejected)
	if !strings.Contains(failure.Details["reason"], "truncated") {
		t.Errorf("reason is %q, want it to name truncation", failure.Details["reason"])
	}
	assertPayloadDir(t, dir)
}

// cancelAfter cancels ctx once it has delivered after bytes, so a test can
// cancel a stream that is already being received.
type cancelAfter struct {
	source io.Reader
	after  int
	cancel context.CancelFunc
}

func (c *cancelAfter) Read(p []byte) (int, error) {
	n, err := c.source.Read(p)
	c.after -= n
	if c.after <= 0 {
		c.cancel()
	}
	return n, err
}

func TestReceiveTarCancelledBetweenEntries(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	archive := buildTar(t,
		fileEntry("bundle/one", 0o600, "one\n"),
		fileEntry("bundle/two", 0o600, "two\n"),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 512 bytes is exactly the first header, so the cancellation happens after
	// the first entry was read and before the second one starts.
	source := &cancelAfter{source: bytes.NewReader(archive), after: 512, cancel: cancel}
	_, err := Receive(ctx, dir, Spec{
		Kind: KindTar, Name: "bundle", MaxBytes: 1 << 20, MaxFiles: 16,
	}, source)
	failureOf(t, err, result.CodeWaitTimeout)
	assertPayloadDir(t, dir)
}

func TestReceiveCancelledContextCreatesNothing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Receive(ctx, dir, Spec{
		Kind: KindBinary, Name: "app", MaxBytes: 16,
	}, strings.NewReader("x"))
	failureOf(t, err, result.CodeWaitTimeout)
	assertPayloadDir(t, dir)
}

func TestReceiveMissingPayloadDirectory(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	missing := filepath.Join(parent, "missing")
	_, err := Receive(context.Background(), missing, Spec{
		Kind: KindBinary, Name: "app", MaxBytes: 16,
	}, strings.NewReader("x"))
	failureOf(t, err, result.CodePayloadRejected)
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Receive created the missing directory %s (stat error %v)", missing, err)
	}
	assertPayloadDir(t, parent)
}

func TestReceiveRejectsNonPlainName(t *testing.T) {
	t.Parallel()
	cases := []string{"", ".", "..", "nested/app", "/absolute"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			_, err := Receive(context.Background(), dir, Spec{
				Kind: KindBinary, Name: name, MaxBytes: 16,
			}, strings.NewReader("x"))
			failureOf(t, err, result.CodePayloadRejected)
			assertPayloadDir(t, dir)
		})
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%s): %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("mode of %s is %#o, want %#o", path, got, want)
	}
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	if string(got) != want {
		t.Errorf("%s holds %q, want %q", path, got, want)
	}
}
