// Package export owns the only host writes wlvision performs for an agent: the
// artifacts a capture asks for, kept under the state root and partitioned by
// session.
//
// The rules are the point of the package. A name an agent supplies names
// something inside one session's export directory: it is never an absolute
// path, it never climbs out with a parent reference, it never follows a
// symbolic link, and it never replaces anything but a regular file wlvision
// itself wrote. Writes are staged next to their target and renamed, so a reader
// never sees a half-written picture.
package export

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bnema/wlvision/internal/result"
	"github.com/bnema/wlvision/internal/session"
)

// DirName is the subdirectory of the state root that holds artifacts.
const DirName = "export"

const (
	// dirMode is the mode of every directory wlvision creates here. A staged
	// file is created by os.CreateTemp, which is already owner-only.
	dirMode = 0o700
	// maxNameLength bounds one artifact name.
	maxNameLength = 200
	// MaxArtifactBytes bounds one stored artifact.
	MaxArtifactBytes int64 = 256 << 20
)

// Tree is the export area of one session.
type Tree struct {
	// base is the shared export directory of the state root. Ancestors of it are
	// the user's own directories; everything below it is wlvision's.
	base string
	// root is the session's own directory.
	root string
}

// Open returns the export tree of one session under a state root. It creates
// nothing: a tree that holds no artifact is a directory that does not exist.
func Open(stateRoot, sessionID string) (*Tree, error) {
	if strings.TrimSpace(stateRoot) == "" {
		return nil, errors.New("export: a state root is required")
	}
	if !session.ValidSessionID(sessionID) {
		return nil, result.NewFailure(result.CodeUsageError, "capture.export",
			"%q is not a usable session identifier", sessionID)
	}
	root, err := filepath.Abs(filepath.Join(stateRoot, DirName, sessionID))
	if err != nil {
		return nil, fmt.Errorf("export: %w", err)
	}
	base, err := filepath.Abs(filepath.Join(stateRoot, DirName))
	if err != nil {
		return nil, fmt.Errorf("export: %w", err)
	}
	return &Tree{base: base, root: root}, nil
}

// Dir returns the tree's root directory. It is where a caller that stores
// nothing would look, and it is not created until an artifact is stored.
func (t *Tree) Dir() string { return t.root }

// Path resolves one artifact name to its host path without creating anything.
func (t *Tree) Path(name string) (string, error) {
	cleaned, err := cleanName(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(t.root, cleaned), nil
}

// Store writes one artifact and returns the host path it was written to.
//
// The write is staged in the target's directory and renamed into place, so the
// path either holds the previous artifact or the new one, never a partial
// write.
func (t *Tree) Store(name string, data []byte) (string, error) {
	if int64(len(data)) > MaxArtifactBytes {
		return "", result.NewFailure(result.CodeCaptureFailed, "capture.export",
			"the artifact is %d bytes, more than the %d byte limit", len(data), MaxArtifactBytes)
	}

	target, err := t.Path(name)
	if err != nil {
		return "", err
	}
	if err := t.ensureDir(filepath.Dir(target)); err != nil {
		return "", err
	}
	if err := refuseUnsafeTarget(target); err != nil {
		return "", err
	}

	staged, err := os.CreateTemp(filepath.Dir(target), ".staging-*")
	if err != nil {
		return "", result.NewFailure(result.CodeCaptureFailed, "capture.export",
			"cannot stage %s: %v", filepath.Base(target), err)
	}
	staging := staged.Name()
	if err := writeAll(staged, data); err != nil {
		_ = staged.Close()
		_ = os.Remove(staging)
		return "", result.NewFailure(result.CodeCaptureFailed, "capture.export",
			"cannot write %s: %v", filepath.Base(target), err)
	}
	if err := staged.Close(); err != nil {
		_ = os.Remove(staging)
		return "", result.NewFailure(result.CodeCaptureFailed, "capture.export",
			"cannot close %s: %v", filepath.Base(target), err)
	}
	if err := os.Rename(staging, target); err != nil {
		_ = os.Remove(staging)
		return "", result.NewFailure(result.CodeCaptureFailed, "capture.export",
			"cannot publish %s: %v", filepath.Base(target), err)
	}
	return target, nil
}

// Read returns one stored artifact. It exists for a caller that has to inspect
// what a capture produced, and it applies the same name rules as Store.
func (t *Tree) Read(name string) ([]byte, error) {
	target, err := t.Path(name)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return nil, result.NewFailure(result.CodeCaptureFailed, "capture.export",
			"cannot read %s: %v", filepath.Base(target), err)
	}
	return data, nil
}

// ensureDir creates the tree's directories one component at a time, refusing
// to walk through a symbolic link. os.MkdirAll would follow one, which is how a
// name would reach a directory the session never owned.
func (t *Tree) ensureDir(dir string) error {
	relative, err := filepath.Rel(t.base, dir)
	if err != nil {
		return result.NewFailure(result.CodeUsageError, "capture.export",
			"%s is not inside the session export directory", dir)
	}
	if relative != "." && (relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
		return result.NewFailure(result.CodeUsageError, "capture.export",
			"%s is not inside the session export directory", dir)
	}

	// The export directory itself sits in the user's state root; creating its
	// ancestors is the caller's own directory work, exactly as the session store
	// does it.
	if err := os.MkdirAll(t.base, dirMode); err != nil {
		return result.NewFailure(result.CodeCaptureFailed, "capture.export",
			"cannot create %s: %v", filepath.Base(t.base), err)
	}
	if relative == "." {
		return nil
	}

	current := t.base
	for _, element := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, element)
		if err := mkdirRefusingSymlink(current); err != nil {
			return err
		}
	}
	return nil
}

// mkdirRefusingSymlink creates one directory, or accepts the one that is
// already there only when it is a real directory.
func mkdirRefusingSymlink(path string) error {
	err := os.Mkdir(path, dirMode)
	if err == nil {
		return nil
	}
	if !errors.Is(err, os.ErrExist) {
		return result.NewFailure(result.CodeCaptureFailed, "capture.export",
			"cannot create %s: %v", filepath.Base(path), err)
	}

	info, statErr := os.Lstat(path)
	if statErr != nil {
		return result.NewFailure(result.CodeCaptureFailed, "capture.export",
			"cannot inspect %s: %v", filepath.Base(path), statErr)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return result.NewFailure(result.CodeUsageError, "capture.export",
			"%s is not a directory wlvision can write through", path)
	}
	return nil
}

// refuseUnsafeTarget rejects a destination that exists and is not a regular
// file, which is what keeps a symbolic link from turning a capture into a write
// somewhere else.
func refuseUnsafeTarget(target string) error {
	info, err := os.Lstat(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return result.NewFailure(result.CodeCaptureFailed, "capture.export",
			"cannot inspect %s: %v", filepath.Base(target), err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return result.NewFailure(result.CodeUsageError, "capture.export",
			"%s is a symbolic link; wlvision does not write through one", filepath.Base(target))
	}
	if info.IsDir() {
		return result.NewFailure(result.CodeUsageError, "capture.export",
			"%s is a directory", filepath.Base(target))
	}
	if !info.Mode().IsRegular() {
		return result.NewFailure(result.CodeUsageError, "capture.export",
			"%s is not a regular file", filepath.Base(target))
	}
	return nil
}

// cleanName validates one artifact name.
//
// A name is a relative path inside the tree: no absolute path, no parent
// reference, no empty element, nothing that resolves outside.
func cleanName(name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", result.NewFailure(result.CodeUsageError, "capture.export", "an artifact name is required")
	}
	switch {
	case strings.ContainsRune(name, 0):
		return "", result.NewFailure(result.CodeUsageError, "capture.export", "the artifact name contains a NUL byte")
	case filepath.IsAbs(name):
		return "", result.NewFailure(result.CodeUsageError, "capture.export",
			"the artifact name %q must be relative to the session export directory", name)
	case strings.HasPrefix(name, "-"):
		return "", result.NewFailure(result.CodeUsageError, "capture.export",
			"the artifact name %q must not begin with a dash", name)
	}

	cleaned := filepath.Clean(name)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", result.NewFailure(result.CodeUsageError, "capture.export",
			"the artifact name %q leaves the session export directory", name)
	}
	if len(cleaned) > maxNameLength {
		return "", result.NewFailure(result.CodeUsageError, "capture.export",
			"the artifact name is longer than %d bytes", maxNameLength)
	}
	return cleaned, nil
}

// writeAll writes the whole payload, which os.File.Write already does in
// practice but does not guarantee.
func writeAll(file *os.File, data []byte) error {
	for len(data) > 0 {
		written, err := file.Write(data)
		if err != nil {
			return err
		}
		if written == 0 {
			return errors.New("no progress")
		}
		data = data[written:]
	}
	return file.Sync()
}
