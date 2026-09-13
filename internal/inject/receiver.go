// Package inject receives a payload into a session container's payload
// directory.
//
// The receiver exists to be safe against a buggy or careless caller, not
// against a privileged attacker: a caller streams either a single executable
// or a tar bundle into the container over stdin, and the receiver must never
// write outside its payload directory, never follow a symlink out of it,
// never create a device node, and never let a stream exhaust memory or disk.
//
// Every payload is first written into a throwaway ".staging-*" directory
// inside the payload directory and installed under its final name with one
// atomic rename, so a partial or rejected payload is never visible there.
package inject

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"io/fs"
	"math"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/bnema/wlvision/internal/result"
)

// operationReceive is the operation name attached to every failure the
// receiver reports, so callers route errors without parsing the message.
const operationReceive = "inject.receive"

// errTruncatedBody marks an archive whose regular file body ended before the
// size its header declared. The caller turns it into a failure that names the
// real cause: a truncated archive, or the size limit that cut it short.
var errTruncatedBody = errors.New("truncated archive body")

const (
	// defaultMaxBytes bounds a payload stream when Spec.MaxBytes is zero.
	defaultMaxBytes int64 = 64 << 20
	// defaultMaxFiles bounds the number of tar entries when Spec.MaxFiles is
	// zero. It is ignored for a binary payload, which is a single file.
	defaultMaxFiles = 1024
	// stagingPrefix names the throwaway directory each payload is written
	// into. The random suffix comes from os.MkdirTemp.
	stagingPrefix = ".staging-"
	// payloadPermMask is the permission policy applied to a requested mode.
	// It keeps what the application identity needs to read and execute a
	// payload — injected code runs as the application UID, not as the control
	// UID that owns it — while dropping group and other write and every
	// setuid, setgid, and sticky bit.
	payloadPermMask os.FileMode = 0o755
	// nonExecMask is the policy for a payload that asked for no execute bit.
	nonExecMask os.FileMode = 0o644
	// dirPerm is the mode of every directory created while extracting a tar
	// bundle. The application identity has to traverse it.
	dirPerm os.FileMode = 0o755
	// filePerm is the mode a file is created with before its final mode is
	// applied, so no intermediate state is wider than the payload policy.
	filePerm os.FileMode = 0o600
	// defaultBinaryPerm is the mode used when a binary payload requests no
	// permission bits at all: an injected binary is meant to be run.
	defaultBinaryPerm os.FileMode = 0o755
)

// Kind selects how the payload stream is interpreted.
type Kind string

const (
	// KindBinary is a single executable streamed directly to the payload.
	KindBinary Kind = "binary"
	// KindTar is a tar archive of a directory tree.
	KindTar Kind = "tar"
)

// Spec describes the payload a caller wants the receiver to install.
//
// Name is the final file or directory name: a plain file name, never a path.
// Anything else is rejected rather than resolved.
//
// Mode is the requested permission bits. It is only meaningful for a binary
// payload and is masked by the receiver's policy; for a tar bundle each entry
// carries its own mode. A zero Mode on a binary requests an executable
// payload (0o755), because a payload with no permission bits at all could not
// be used.
//
// MaxBytes bounds the stream and MaxFiles bounds the number of tar entries. A
// zero MaxBytes means defaultMaxBytes (64 MiB), a zero or negative MaxFiles
// means defaultMaxFiles (1024), and MaxFiles is ignored for a binary payload.
type Spec struct {
	Kind     Kind
	Name     string
	Mode     uint32
	MaxBytes int64
	MaxFiles int
}

// Result describes an installed payload.
type Result struct {
	// Path is the final absolute path of the payload under the payload
	// directory.
	Path string
	// Digest is "sha256:<lowercase hex>" over the bytes received: the file
	// bytes for a binary, the archive bytes as received for a tar.
	Digest string
	// Bytes is the number of payload bytes received: the file size for a
	// binary, the number of archive bytes read for a tar.
	Bytes int64
	// Files is the number of regular files installed; it is 1 for a binary.
	Files int
}

// Receive streams source into a freshly created staging directory inside dir
// and installs it under spec.Name.
//
// On success the staging directory is renamed to dir/spec.Name, so a partial
// payload is never visible there. On any failure the whole staging tree is
// removed and a *result.Failure with result.CodePayloadRejected is returned,
// except when ctx is cancelled or its deadline expires, which returns
// result.CodeWaitTimeout instead.
func Receive(ctx context.Context, dir string, spec Spec, source io.Reader) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, waitFailure(err)
	}
	// Validate everything that does not need the filesystem before creating
	// any staging state, so an invalid request leaves nothing behind.
	if dir == "" {
		return Result{}, rejected("payload directory is empty")
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return Result{}, rejectedDetail("payload directory is not usable", err.Error())
	}
	if !plainName(spec.Name) {
		return Result{}, rejectedDetail("payload name is not a plain file name", strconv.Quote(spec.Name))
	}
	switch spec.Kind {
	case KindBinary, KindTar:
	default:
		return Result{}, rejectedDetail("unknown payload kind", strconv.Quote(string(spec.Kind)))
	}

	dirRoot, err := os.OpenRoot(absDir)
	if err != nil {
		return Result{}, rejectedDetail("payload directory is not available", err.Error())
	}
	defer dirRoot.Close()

	stagingPath, err := os.MkdirTemp(absDir, stagingPrefix)
	if err != nil {
		return Result{}, rejectedDetail("payload directory is not writable", err.Error())
	}
	// For a bundle the staging directory becomes the bundle itself, and the
	// application identity has to traverse it; MkdirTemp is subject to the
	// process umask, this policy is not.
	if err := os.Chmod(stagingPath, dirPerm); err != nil {
		return Result{}, rejectedDetail("cannot set the payload directory mode", err.Error())
	}
	stagingName := filepath.Base(stagingPath)
	installed := false
	defer func() {
		if !installed {
			_ = dirRoot.RemoveAll(stagingName)
		}
	}()

	stagingRoot, err := os.OpenRoot(stagingPath)
	if err != nil {
		return Result{}, rejectedDetail("cannot open the staging directory", err.Error())
	}
	defer stagingRoot.Close()

	receiver := &receiver{root: stagingRoot}
	var (
		digest string
		bytes  int64
		files  int
	)
	switch spec.Kind {
	case KindBinary:
		digest, bytes, err = receiver.binary(ctx, spec, source)
		files = 1
	case KindTar:
		digest, bytes, files, err = receiver.tar(ctx, spec, source)
	}
	if err != nil {
		return Result{}, err
	}
	// Close the staging root so the rename below cannot race with a pending
	// write; the deferred Close then reports nothing.
	if err := stagingRoot.Close(); err != nil {
		return Result{}, rejectedDetail("cannot flush the staged payload", err.Error())
	}
	// A tar bundle is the staged directory itself; a binary is a single file
	// inside it, so only that file is moved out and the directory is dropped.
	staged := stagingName
	if spec.Kind == KindBinary {
		staged = path.Join(stagingName, spec.Name)
	}
	if err := dirRoot.Rename(staged, spec.Name); err != nil {
		return Result{}, rejectedDetail("cannot install the payload under its final name", err.Error())
	}
	if spec.Kind == KindBinary {
		if err := dirRoot.RemoveAll(stagingName); err != nil {
			return Result{}, rejectedDetail("cannot remove the staging directory", err.Error())
		}
	}
	installed = true
	return Result{
		Path:   filepath.Join(absDir, spec.Name),
		Digest: digest,
		Bytes:  bytes,
		Files:  files,
	}, nil
}

// receiver holds the root of the staging directory and performs the
// extraction. Every filesystem operation goes through root, so no entry name
// can ever reach outside the staging directory.
type receiver struct {
	root *os.Root
}

// binary streams source into a single file, hashing as it writes. The limit is
// enforced by reading at most MaxBytes bytes and then probing for one more, so
// an oversized stream is reported rather than silently truncated.
func (r *receiver) binary(ctx context.Context, spec Spec, source io.Reader) (string, int64, error) {
	limit := spec.MaxBytes
	if limit <= 0 {
		limit = defaultMaxBytes
	}
	if err := ctx.Err(); err != nil {
		return "", 0, waitFailure(err)
	}
	file, err := r.root.OpenFile(spec.Name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, filePerm)
	if err != nil {
		return "", 0, rejectedDetail("cannot create the payload file", err.Error())
	}
	hasher := sha256.New()
	written, err := io.Copy(io.MultiWriter(file, hasher), io.LimitReader(source, limit))
	if err != nil {
		_ = file.Close()
		return "", 0, rejectedDetail("payload stream could not be read", err.Error())
	}
	if written == limit {
		extra, err := io.CopyN(io.Discard, source, 1)
		switch {
		case err == nil && extra > 0:
			_ = file.Close()
			return "", 0, rejectedDetail("payload exceeds MaxBytes", strconv.FormatInt(limit, 10))
		case err != nil && !errors.Is(err, io.EOF):
			_ = file.Close()
			return "", 0, rejectedDetail("payload stream could not be read", err.Error())
		}
	}
	if err := file.Chmod(maskBinaryMode(spec.Mode)); err != nil {
		_ = file.Close()
		return "", 0, rejectedDetail("cannot apply the payload file mode", err.Error())
	}
	if err := file.Close(); err != nil {
		return "", 0, rejectedDetail("cannot flush the payload file", err.Error())
	}
	return digestString(hasher), written, nil
}

// tar validates and extracts an archive entry by entry, entirely in Go. The
// hash covers every archive byte read, so the returned digest is the digest of
// the stream as received. limit bounds both the sum of the regular file sizes
// a header declares and the archive bytes actually read, so a lying header can
// never bypass the limit.
func (r *receiver) tar(ctx context.Context, spec Spec, source io.Reader) (string, int64, int, error) {
	limit := spec.MaxBytes
	if limit <= 0 {
		limit = defaultMaxBytes
	}
	maxFiles := spec.MaxFiles
	if maxFiles <= 0 {
		maxFiles = defaultMaxFiles
	}

	hasher := sha256.New()
	limited := &io.LimitedReader{R: source, N: limitedBound(limit)}
	archive := io.TeeReader(limited, hasher)
	header := tar.NewReader(archive)

	var (
		tree     pathTree
		declared int64
		files    int
		entries  int
	)
	for {
		if err := ctx.Err(); err != nil {
			return "", 0, 0, waitFailure(err)
		}
		next, err := header.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", 0, 0, archiveFailure(limited, limit, err)
		}
		entries++
		if entries > maxFiles {
			return "", 0, 0, rejectedDetail("entry count exceeds MaxFiles", strconv.Itoa(maxFiles))
		}
		name, err := cleanEntryName(next.Name)
		if err != nil {
			return "", 0, 0, err
		}
		if name == "." {
			// The archive root ("." or "./") is the staging directory itself.
			if next.Typeflag == tar.TypeDir {
				continue
			}
			return "", 0, 0, rejectedDetail("entry name resolves to the payload root", strconv.Quote(next.Name))
		}
		if next.Mode&0o7000 != 0 {
			return "", 0, 0, rejectedDetail("entry mode requests setuid, setgid, or sticky bits",
				strconv.Quote(next.Name))
		}
		switch next.Typeflag {
		case tar.TypeReg, tar.TypeRegA:
			if next.Size < 0 {
				return "", 0, 0, rejectedDetail("entry declares a negative size", strconv.Quote(next.Name))
			}
			if next.Size > limit || declared > limit-next.Size {
				return "", 0, 0, rejectedDetail("declared entry size exceeds MaxBytes", strconv.Quote(next.Name))
			}
			if err := tree.check(name, false); err != nil {
				return "", 0, 0, err
			}
			if err := r.ensureDirs(path.Dir(name)); err != nil {
				return "", 0, 0, err
			}
			if err := r.writeFile(name, next.Mode, next.Size, header); err != nil {
				if errors.Is(err, errTruncatedBody) {
					return "", 0, 0, archiveFailure(limited, limit, errTruncatedBody)
				}
				return "", 0, 0, err
			}
			declared += next.Size
			files++
		case tar.TypeDir:
			if err := tree.check(name, true); err != nil {
				return "", 0, 0, err
			}
			if err := r.ensureDirs(name); err != nil {
				return "", 0, 0, err
			}
		case tar.TypeSymlink:
			return "", 0, 0, rejectedDetail("symlink entry is not supported", strconv.Quote(next.Name))
		case tar.TypeLink:
			return "", 0, 0, rejectedDetail("hard link entry is not supported", strconv.Quote(next.Name))
		case tar.TypeChar, tar.TypeBlock:
			return "", 0, 0, rejectedDetail("device node entry is not allowed", strconv.Quote(next.Name))
		case tar.TypeFifo:
			return "", 0, 0, rejectedDetail("fifo entry is not allowed", strconv.Quote(next.Name))
		default:
			return "", 0, 0, rejectedDetail("irregular entry type is not allowed", strconv.Quote(next.Name))
		}
	}

	// Read whatever the tar reader left behind (padding, trailing bytes) so the
	// digest covers the stream as received and the limit covers every byte.
	if _, err := io.Copy(io.Discard, archive); err != nil {
		return "", 0, 0, archiveFailure(limited, limit, err)
	}
	if limited.N <= 0 {
		return "", 0, 0, rejectedDetail("payload exceeds MaxBytes", strconv.FormatInt(limit, 10))
	}
	return digestString(hasher), limitedBound(limit) - limited.N, files, nil
}

// writeFile copies exactly size bytes of a regular file entry into the staging
// tree and applies the masked mode.
func (r *receiver) writeFile(name string, mode, size int64, body io.Reader) error {
	file, err := r.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, filePerm)
	if err != nil {
		return rejectedDetail("cannot create an archive file", err.Error())
	}
	if _, err := io.CopyN(file, body, size); err != nil {
		_ = file.Close()
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return errTruncatedBody
		}
		return rejectedDetail("cannot extract an archive file", err.Error())
	}
	if err := file.Chmod(maskTarMode(mode)); err != nil {
		_ = file.Close()
		return rejectedDetail("cannot apply an archive file mode", err.Error())
	}
	if err := file.Close(); err != nil {
		return rejectedDetail("cannot flush an archive file", err.Error())
	}
	return nil
}

// ensureDirs creates name and every parent inside the staging tree with the
// directory policy. Archive entries are not required to declare their parents.
func (r *receiver) ensureDirs(name string) error {
	if name == "." || name == "" || name == "/" {
		return nil
	}
	prefix := ""
	for _, element := range strings.Split(name, "/") {
		if prefix == "" {
			prefix = element
		} else {
			prefix += "/" + element
		}
		if err := r.root.Mkdir(prefix, dirPerm); err != nil && !errors.Is(err, fs.ErrExist) {
			return rejectedDetail("cannot create an archive directory", err.Error())
		}
		// Mkdir is subject to the process umask; the policy is not.
		if err := r.root.Chmod(prefix, dirPerm); err != nil {
			return rejectedDetail("cannot set an archive directory mode", err.Error())
		}
	}
	return nil
}

// cleanEntryName validates one archive entry name and returns its cleaned
// relative form. It rejects absolute names, empty names, NUL bytes, and every
// name that reaches outside the staging directory, before any write happens.
func cleanEntryName(name string) (string, error) {
	switch {
	case name == "":
		return "", rejected("entry name is empty")
	case strings.ContainsRune(name, 0):
		return "", rejectedDetail("entry name contains a NUL byte", strconv.Quote(name))
	case strings.HasPrefix(name, "/"):
		return "", rejectedDetail("absolute entry name", strconv.Quote(name))
	}
	for _, element := range strings.Split(name, "/") {
		if element == ".." {
			return "", rejectedDetail("entry name contains a .. path element", strconv.Quote(name))
		}
	}
	cleaned := path.Clean(name)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", rejectedDetail("entry name escapes the payload root", strconv.Quote(name))
	}
	return cleaned, nil
}

// plainName reports whether name is a usable final name: a non-empty, single
// path element that names neither the current nor the parent directory.
func plainName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	return !strings.ContainsAny(name, "/\x00")
}

// pathTree records the names an archive has created so duplicate names and
// file/directory prefix conflicts are rejected. Directories may be declared
// after their contents, so only names that would make an earlier entry
// unusable are refused.
type pathTree struct {
	names []string
	dirs  map[string]bool
}

// check reports whether name can be created and records it. isDir selects the
// rules: a file may not live under an earlier file and may not enclose an
// earlier entry, while a directory may replace its own implicit parents.
func (t *pathTree) check(name string, isDir bool) error {
	if t.dirs == nil {
		t.dirs = make(map[string]bool)
	}
	index := sort.SearchStrings(t.names, name)
	if index < len(t.names) && t.names[index] == name {
		return rejectedDetail("duplicate entry name", strconv.Quote(name))
	}
	for parent := path.Dir(name); parent != "." && parent != "/"; parent = path.Dir(parent) {
		if kind, ok := t.dirs[parent]; ok && !kind {
			return rejectedDetail("entry name conflicts with an earlier entry", strconv.Quote(parent))
		}
	}
	if !isDir {
		descendant := sort.SearchStrings(t.names, name+"/")
		if descendant < len(t.names) && strings.HasPrefix(t.names[descendant], name+"/") {
			return rejectedDetail("entry name conflicts with an earlier entry", strconv.Quote(t.names[descendant]))
		}
	}
	t.insert(name)
	t.dirs[name] = isDir
	return nil
}

func (t *pathTree) insert(name string) {
	index := sort.SearchStrings(t.names, name)
	t.names = append(t.names, "")
	copy(t.names[index+1:], t.names[index:])
	t.names[index] = name
}

// maskBinaryMode applies the payload permission policy to a requested binary
// mode. A zero request means the caller expressed no preference and gets an
// executable payload.
func maskBinaryMode(mode uint32) os.FileMode {
	if mode == 0 {
		return defaultBinaryPerm
	}
	return maskMode(os.FileMode(mode))
}

// maskTarMode applies the payload permission policy to a tar entry mode.
func maskTarMode(mode int64) os.FileMode {
	return maskMode(os.FileMode(mode))
}

// maskMode keeps owner, group, and other read, and the execute bits only when
// the request asked for at least one of them. Write for group and other, and
// every setuid, setgid, and sticky bit, are dropped.
func maskMode(requested os.FileMode) os.FileMode {
	if requested&0o111 == 0 {
		return requested & nonExecMask
	}
	return requested & payloadPermMask
}

// limitedBound returns the hard read bound for limit: one byte more than the
// limit so an over-limit stream can be told apart from an exactly-sized one,
// clamped so it can never overflow.
func limitedBound(limit int64) int64 {
	if limit == math.MaxInt64 {
		return limit
	}
	return limit + 1
}

// archiveFailure describes an error from the tar reader. The size limit is
// checked first: when the limited reader is exhausted the archive is not
// truncated, it is over budget.
func archiveFailure(limited *io.LimitedReader, limit int64, err error) *result.Failure {
	if limited.N <= 0 {
		return rejectedDetail("payload exceeds MaxBytes", strconv.FormatInt(limit, 10))
	}
	if errors.Is(err, errTruncatedBody) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return rejectedDetail("truncated archive", err.Error())
	}
	return rejectedDetail("invalid tar archive", err.Error())
}

func digestString(hasher hash.Hash) string {
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil))
}

// rejected builds a payload_rejected failure whose details name the specific
// problem, which is the stable part a caller may match on.
func rejected(reason string) *result.Failure {
	return rejectedDetail(reason, "")
}

func rejectedDetail(reason, detail string) *result.Failure {
	message := reason
	if detail != "" {
		message = reason + ": " + detail
	}
	failure := result.NewFailure(result.CodePayloadRejected, operationReceive, "%s", message)
	failure.Details = map[string]string{"reason": reason}
	if detail != "" {
		failure.Details["detail"] = detail
	}
	return failure
}

// waitFailure reports cancellation with the contract's timeout code, which is
// a transient condition and must not be mistaken for a rejected payload.
func waitFailure(err error) *result.Failure {
	failure := result.NewFailure(result.CodeWaitTimeout, operationReceive, "payload reception cancelled: %v", err)
	failure.Details = map[string]string{"reason": "context cancelled"}
	return failure
}
