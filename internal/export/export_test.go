package export

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bnema/wlvision/internal/result"
)

func newTree(t *testing.T) *Tree {
	t.Helper()

	tree, err := Open(t.TempDir(), "demo")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return tree
}

func failureOf(t *testing.T, err error) *result.Failure {
	t.Helper()

	if err == nil {
		t.Fatal("expected a failure, got none")
	}
	var failure *result.Failure
	if !errors.As(err, &failure) {
		t.Fatalf("error %v (%T) is not a *result.Failure", err, err)
	}
	return failure
}

func TestOpenPartitionsBySessionAndRefusesAnUnusableIdentifier(t *testing.T) {
	first, err := Open(t.TempDir(), "one")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	second, err := Open(first.Dir(), "two")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if first.Dir() == second.Dir() {
		t.Fatal("two sessions share one export directory")
	}

	for _, id := range []string{"", "  ", "Demo", "../etc", "demo/../../x", "de mo", strings.Repeat("a", 80)} {
		if _, err := Open(t.TempDir(), id); err == nil {
			t.Errorf("the session identifier %q was accepted", id)
		}
	}
}

func TestStoreWritesAnArtifactOwnerOnly(t *testing.T) {
	tree := newTree(t)

	path, err := tree.Store("frames/frame-0001.png", []byte("png"))
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	if want := filepath.Join(tree.Dir(), "frames", "frame-0001.png"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}

	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the artifact: %v", err)
	}
	if string(stored) != "png" {
		t.Errorf("artifact = %q, want the stored bytes", stored)
	}

	for _, dir := range []string{tree.Dir(), filepath.Join(tree.Dir(), "frames")} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if info.Mode().Perm() != dirMode {
			t.Errorf("mode of %s = %o, want %o", dir, info.Mode().Perm(), dirMode)
		}
	}
}

func TestStoreLeavesNoStagingFileBehind(t *testing.T) {
	tree := newTree(t)

	for _, payload := range []string{"first", "second"} {
		if _, err := tree.Store("shot.png", []byte(payload)); err != nil {
			t.Fatalf("Store %q: %v", payload, err)
		}
	}

	entries, err := os.ReadDir(tree.Dir())
	if err != nil {
		t.Fatalf("read the tree: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "shot.png" {
		t.Errorf("the export directory holds %v, want only the artifact", entries)
	}

	stored, err := tree.Read("shot.png")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(stored) != "second" {
		t.Errorf("artifact = %q, want the second write", stored)
	}
}

func TestNamesOutsideTheTreeAreRefused(t *testing.T) {
	tree := newTree(t)

	for _, name := range []string{
		"", "   ", "/etc/passwd", "../outside.png", "frames/../../outside.png",
		"..", "-c", "frames/../../../", "./..",
	} {
		_, err := tree.Path(name)
		if err == nil {
			t.Errorf("the name %q was accepted", name)
			continue
		}
		if failure := failureOf(t, err); failure.Code != result.CodeUsageError {
			t.Errorf("the name %q reported %s, want %s", name, failure.Code, result.CodeUsageError)
		}
		if _, err := tree.Store(name, []byte("x")); err == nil {
			t.Errorf("the name %q was stored", name)
		}
	}
}

func TestNamesAreNormalized(t *testing.T) {
	tree := newTree(t)

	path, err := tree.Path("frames/./frame-0001.png")
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if want := filepath.Join(tree.Dir(), "frames", "frame-0001.png"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
}

func TestStoreRefusesToWriteThroughASymbolicLink(t *testing.T) {
	tree := newTree(t)

	outside := t.TempDir()
	if err := os.MkdirAll(tree.Dir(), dirMode); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(tree.Dir(), "frames")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, err := tree.Store("frames/frame-0001.png", []byte("png"))
	if failure := failureOf(t, err); failure.Code != result.CodeUsageError {
		t.Errorf("code = %s, want %s", failure.Code, result.CodeUsageError)
	}
	if entries, readErr := os.ReadDir(outside); readErr != nil || len(entries) != 0 {
		t.Errorf("the outside directory holds %v (err %v), want nothing", entries, readErr)
	}
}

func TestStoreRefusesToReplaceASymbolicLink(t *testing.T) {
	tree := newTree(t)

	if err := os.MkdirAll(tree.Dir(), dirMode); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("original"), 0o600); err != nil {
		t.Fatalf("write the victim: %v", err)
	}
	if err := os.Symlink(victim, filepath.Join(tree.Dir(), "shot.png")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if _, err := tree.Store("shot.png", []byte("attack")); err == nil {
		t.Fatal("a capture replaced a symbolic link")
	}
	stored, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("read the victim: %v", err)
	}
	if string(stored) != "original" {
		t.Errorf("the victim holds %q, want it untouched", stored)
	}
}

func TestStoreRefusesADirectoryTarget(t *testing.T) {
	tree := newTree(t)

	if err := os.MkdirAll(filepath.Join(tree.Dir(), "shot.png"), dirMode); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	_, err := tree.Store("shot.png", []byte("x"))
	if failure := failureOf(t, err); failure.Code != result.CodeUsageError {
		t.Errorf("code = %s, want %s", failure.Code, result.CodeUsageError)
	}
}

func TestStoreRefusesAnOversizedArtifact(t *testing.T) {
	tree := newTree(t)

	payload := make([]byte, MaxArtifactBytes+1)
	_, err := tree.Store("huge.png", payload)
	if failure := failureOf(t, err); failure.Code != result.CodeCaptureFailed {
		t.Errorf("code = %s, want %s", failure.Code, result.CodeCaptureFailed)
	}
}

func TestReadReportsAMissingArtifact(t *testing.T) {
	tree := newTree(t)

	if _, err := tree.Read("nothing.png"); err == nil {
		t.Fatal("a missing artifact was read")
	} else if failure := failureOf(t, err); failure.Code != result.CodeCaptureFailed {
		t.Errorf("code = %s, want %s", failure.Code, result.CodeCaptureFailed)
	}
}
