package manifest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/bnema/wlvision/internal/engine"
	"github.com/bnema/wlvision/internal/result"
)

const (
	// containerfileName is the name of the generated Containerfile inside the
	// build context.
	containerfileName = "Containerfile"
	// manifestCopyName is the name of the manifest copy kept in the context,
	// so an image records the manifest that built it.
	manifestCopyName = "manifest.toml"
	// tagPrefix names every image a manifest build creates.
	tagPrefix = "wlvision-build:"
	// tagDigestLength is how many hexadecimal digits of the manifest digest
	// the tag keeps.
	tagDigestLength = 12
	// buildOutputTail bounds the build output a failure carries.
	buildOutputTail = 4096
)

// BuildPlan is a manifest resolved against its own directory: every host path
// is absolute, every build-context path is fixed, and the Containerfile and
// image tag are already computed. Planning is pure and deterministic — the
// same manifest bytes always produce the same plan, so a tag never moves
// because a checkout moved.
type BuildPlan struct {
	// ManifestDir is the manifest's directory with symbolic links resolved.
	ManifestDir string
	// Image is the digest-pinned base image, or the reference to an image
	// the engine already holds when Digest is set.
	Image string
	// Digest is the base.digest an engine-held base image must report. It is
	// empty when Image carries its own digest, so the Containerfile needs no
	// change for either form.
	Digest string
	// Tag is the image tag a build creates.
	Tag string
	// AllowNetwork permits network access during the build only.
	AllowNetwork bool
	// Packages are installed in one transaction, in a stable order.
	Packages []string
	// Command, Workdir, Env, and Writable mirror the application section.
	Command  []string
	Workdir  string
	Env      []string
	Writable []string
	// Sources are the declared copies, in declaration order.
	Sources []PlannedSource
	// Modules are the declared sibling Go modules, in declaration order.
	Modules []PlannedModule
	// Containerfile is the generated build recipe.
	Containerfile []byte
	// ManifestTOML is the manifest's own bytes, staged next to the
	// Containerfile so an image can record what built it.
	ManifestTOML []byte
}

// PlannedSource is one source resolved to a host path and a context path.
type PlannedSource struct {
	// HostPath is the absolute host path, with symbolic links resolved.
	HostPath string
	// ContextPath is the source's place in the build context, relative and
	// slash-separated.
	ContextPath string
	// Target is the absolute destination inside the image.
	Target string
	// Mode is the file mode the Containerfile declares, or zero to keep the
	// source's own mode.
	Mode uint32
	// Directory reports whether the source is a directory tree.
	Directory bool
}

// PlannedModule is one sibling Go module copied into the context. The schema
// declares no target for a module, so the plan fixes one: the tree lands under
// /src in the image, named after the module directory.
type PlannedModule struct {
	HostPath    string
	ContextPath string
	Name        string
	Target      string
}

// Result reports one completed image build.
type Result struct {
	// Tag is the tag the build created.
	Tag string
	// ImageID is the identifier the engine reported for the built image.
	ImageID string
	// Output is the package transaction and build output the engine printed.
	Output string
}

// Plan resolves and validates a loaded manifest. It returns a plan whose
// Containerfile text and tag are deterministic.
func Plan(manifest Manifest) (BuildPlan, error) {
	if manifest.dir == "" {
		return BuildPlan{}, errors.New("manifest: Plan needs a manifest loaded with Load")
	}

	packages := append([]string(nil), manifest.Base.Packages...)
	sort.Strings(packages)

	plan := BuildPlan{
		ManifestDir:  manifest.realDir,
		Image:        manifest.Base.Image,
		Digest:       manifest.Base.Digest,
		AllowNetwork: manifest.Base.AllowNetwork,
		Packages:     packages,
		Command:      append([]string(nil), manifest.Application.Command...),
		Workdir:      manifest.Application.Workdir,
		Env:          append([]string(nil), manifest.Application.Env...),
		Writable:     append([]string(nil), manifest.Application.Writable...),
		ManifestTOML: append([]byte(nil), manifest.source...),
	}

	for i, source := range manifest.Copy.Sources {
		field := fmt.Sprintf("copy.sources[%d].from", i)
		resolved, err := filepath.EvalSymlinks(filepath.Join(manifest.dir, filepath.FromSlash(source.From)))
		if err != nil {
			return BuildPlan{}, usage(field, "cannot resolve %q: %v", source.From, err)
		}
		info, err := os.Lstat(resolved)
		if err != nil {
			return BuildPlan{}, usage(field, "cannot inspect %q: %v", source.From, err)
		}

		var mode uint32
		if source.Mode != "" {
			parsed, err := parseMode(source.Mode)
			if err != nil {
				return BuildPlan{}, usage(fmt.Sprintf("copy.sources[%d].mode", i), "%q is not an octal file mode: %v", source.Mode, err)
			}
			mode = parsed
		}

		plan.Sources = append(plan.Sources, PlannedSource{
			HostPath:    resolved,
			ContextPath: path.Join("src", strconv.Itoa(i), filepath.Base(filepath.FromSlash(source.From))),
			Target:      filepath.Clean(source.To),
			Mode:        mode,
			Directory:   info.IsDir(),
		})
	}

	for i, module := range manifest.Copy.Modules {
		field := fmt.Sprintf("copy.modules[%d]", i)
		resolved, err := filepath.EvalSymlinks(filepath.Join(manifest.dir, filepath.FromSlash(module)))
		if err != nil {
			return BuildPlan{}, usage(field, "cannot resolve %q: %v", module, err)
		}
		name := filepath.Base(resolved)
		plan.Modules = append(plan.Modules, PlannedModule{
			HostPath:    resolved,
			ContextPath: path.Join("modules", strconv.Itoa(i), name),
			Name:        name,
			Target:      path.Join("/src", name),
		})
	}

	plan.Containerfile = renderContainerfile(plan)
	plan.Tag = tagPrefix + shortDigest(manifest)
	return plan, nil
}

// Stage writes the build context: the Containerfile, a copy of the manifest,
// every declared source, and every declared module. It copies nothing else,
// never leaves the staging root, and never follows a symbolic link out of the
// tree it is copying. The staging directory is what the engine builds from;
// nothing is bind-mounted.
func Stage(ctx context.Context, plan BuildPlan, stagingDir string) error {
	if strings.TrimSpace(stagingDir) == "" || !filepath.IsAbs(stagingDir) {
		return errors.New("manifest: the staging directory must be an absolute path")
	}
	root := filepath.Clean(stagingDir)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("manifest: cannot create the staging directory: %w", err)
	}

	if err := writeStaged(root, containerfileName, plan.Containerfile, 0o644); err != nil {
		return err
	}
	if len(plan.ManifestTOML) > 0 {
		if err := writeStaged(root, manifestCopyName, plan.ManifestTOML, 0o644); err != nil {
			return err
		}
	}

	for i, source := range plan.Sources {
		if err := ctx.Err(); err != nil {
			return err
		}
		target, err := stagedPath(root, source.ContextPath)
		if err != nil {
			return fmt.Errorf("copy.sources[%d]: %w", i, err)
		}
		if source.Directory {
			if err := copyTree(ctx, source.HostPath, target, source.Mode); err != nil {
				return fmt.Errorf("copy.sources[%d].from: %w", i, err)
			}
			continue
		}
		mode := os.FileMode(0o644)
		if source.Mode != 0 {
			mode = os.FileMode(source.Mode)
		}
		if err := copyFile(ctx, source.HostPath, target, mode); err != nil {
			return fmt.Errorf("copy.sources[%d].from: %w", i, err)
		}
	}

	for i, module := range plan.Modules {
		if err := ctx.Err(); err != nil {
			return err
		}
		target, err := stagedPath(root, module.ContextPath)
		if err != nil {
			return fmt.Errorf("copy.modules[%d]: %w", i, err)
		}
		if err := copyTree(ctx, module.HostPath, target, 0); err != nil {
			return fmt.Errorf("copy.modules[%d]: %w", i, err)
		}
	}
	return nil
}

// Build stages the plan and asks the engine to build the image. The returned
// result carries the image tag, the engine's image identifier, and the build
// output. A failed build is an image_unavailable failure carrying the tail of
// that output, so a caller sees why the image could not be produced without
// re-running the build.
//
// When the base is an image the engine already holds rather than a
// digest-pinned reference, the engine's identifier for that reference is read
// first and must equal the plan's digest: a locally built base is pinned the
// same way a registry digest pins one, and a mismatch is refused before the
// build starts.
func Build(ctx context.Context, containerEngine engine.Engine, plan BuildPlan, stagingDir string) (Result, error) {
	if containerEngine == nil {
		return Result{}, errors.New("manifest: an engine is required")
	}
	if plan.Tag == "" {
		return Result{}, errors.New("manifest: the plan carries no image tag; call Plan first")
	}
	if plan.Digest != "" {
		imageID, err := containerEngine.ImageID(ctx, plan.Image)
		if err != nil {
			// The lookup failed for a reason that belongs to the engine or to
			// the reference itself; wrapping keeps that identity (a missing
			// image stays ErrNotFound, a malformed reference stays a usage
			// error) instead of relabelling it as an unavailable image.
			return Result{}, fmt.Errorf("manifest.build: cannot pin the base image %q: %w", plan.Image, err)
		}
		imageID = strings.TrimSpace(imageID)
		if imageID != plan.Digest {
			return Result{}, result.NewFailure(result.CodeImageUnavailable, "manifest.build",
				"the base image %q has id %q, but the manifest pins %q", plan.Image, imageID, plan.Digest)
		}
	}
	if err := Stage(ctx, plan, stagingDir); err != nil {
		return Result{}, err
	}

	// One transcript collects both streams, because the engine writes to them at
	// the same time: handing the same plain buffer to stdout and stderr would put
	// two writers inside it at once.
	transcript := &transcript{}
	built, err := containerEngine.Build(ctx, engine.BuildSpec{
		Context:       filepath.Clean(stagingDir),
		Containerfile: containerfileName,
		Tag:           plan.Tag,
		Network:       plan.AllowNetwork,
		Stdout:        transcript,
		Stderr:        transcript,
	})
	if err != nil {
		failure := result.NewFailure(result.CodeImageUnavailable, "manifest.build",
			"the image build failed: %v", err)
		if tail := outputTail(transcript.bytes()); tail != "" {
			failure.Details = map[string]string{"build_output": tail}
		}
		return Result{}, failure
	}

	return Result{Tag: plan.Tag, ImageID: built.ImageID, Output: transcript.string()}, nil
}

// transcript collects a build's output from both of its streams. The engine
// writes to them concurrently, so the writer itself is what keeps the text
// whole.
type transcript struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write implements io.Writer.
func (t *transcript) Write(payload []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.buf.Write(payload)
}

// bytes returns the transcript as it stands.
func (t *transcript) bytes() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]byte(nil), t.buf.Bytes()...)
}

// string returns the transcript as it stands.
func (t *transcript) string() string { return string(t.bytes()) }

// renderContainerfile writes the build recipe. It is reproducible and
// readable: a digest-pinned FROM, one package transaction when packages are
// listed, a COPY per declared source and module, and the application's ENV,
// WORKDIR, and CMD. It never names a host path the plan did not declare, and
// it carries no secret, no credential, and no runtime network configuration.
//
// The schema stays distribution-neutral: this profile names the Arch package
// manager because that is the profile wlvision ships first, not because the
// manifest requires Arch.
func renderContainerfile(plan BuildPlan) []byte {
	lines := []string{
		"# Generated by wlvision from a wlvision-manifest/v1 manifest. Do not edit.",
		"# The base is digest-pinned so the image cannot move under a session.",
		"FROM " + plan.Image,
	}

	if len(plan.Packages) > 0 {
		lines = append(lines,
			"",
			"# One transaction, so the image never holds a partial installation.",
			"RUN pacman -Sy --noconfirm "+strings.Join(plan.Packages, " "),
		)
	}

	for _, source := range plan.Sources {
		reference, target := source.ContextPath, source.Target
		if source.Directory {
			// A directory's contents land in the target, which is how COPY
			// treats a directory source.
			reference += "/"
			target += "/"
		}
		line := "COPY"
		if source.Mode != 0 {
			line += " --chmod=0" + strconv.FormatUint(uint64(source.Mode), 8)
		}
		lines = append(lines, "", line+" "+reference+" "+target)
	}

	for _, module := range plan.Modules {
		lines = append(lines, "", "COPY "+module.ContextPath+"/ "+module.Target+"/")
	}

	for _, entry := range plan.Env {
		lines = append(lines, "", "ENV "+entry)
	}
	if plan.Workdir != "" {
		lines = append(lines, "", "WORKDIR "+plan.Workdir)
	}
	lines = append(lines, "", "CMD "+jsonArray(plan.Command), "")

	return []byte(strings.Join(lines, "\n"))
}

// jsonArray renders an argument list as the JSON form CMD uses. The encoding
// is deterministic and escapes every byte that could end the array early.
func jsonArray(argv []string) string {
	encoded, err := json.Marshal(argv)
	if err != nil {
		// A []string cannot fail to marshal; keep the recipe well-formed.
		return "[]"
	}
	return string(encoded)
}

// shortDigest is the manifest's content digest, truncated for a tag. The
// digest covers only declared values, never a host path or the clock, so the
// same manifest in two checkouts produces the same tag.
func shortDigest(manifest Manifest) string {
	sum := sha256.Sum256(canonical(manifest))
	return hex.EncodeToString(sum[:])[:tagDigestLength]
}

// ValidateTag refuses an image tag an engine would misread.
//
// The rules are the image reference grammar reduced to a tag: a name that
// starts with the build prefix, or any other `name:tag` pair, with no scheme,
// no digest, no whitespace and no leading dash that an engine could read as a
// flag.
func ValidateTag(tag string) error {
	trimmed := strings.TrimSpace(tag)
	switch {
	case trimmed == "":
		return usage("tag", "an image tag is required")
	case trimmed != tag:
		return usage("tag", "an image tag must not have surrounding whitespace")
	case strings.HasPrefix(trimmed, "-"):
		return usage("tag", "%q must not begin with a dash", tag)
	case len(trimmed) > 255:
		return usage("tag", "an image tag is longer than 255 characters")
	}
	for _, r := range trimmed {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			continue
		}
		switch r {
		case '.', '_', '-', ':', '/', '@':
			continue
		}
		return usage("tag", "%q contains %q, which an image tag cannot carry", tag, r)
	}
	if strings.Contains(trimmed, "@") {
		return usage("tag", "%q looks like a digest pin, not a tag", tag)
	}
	return nil
}

// canonical encodes a manifest deterministically: fixed field order, one
// quoted value per line, packages sorted. It is an internal fingerprint, not a
// wire format.
func canonical(manifest Manifest) []byte {
	packages := append([]string(nil), manifest.Base.Packages...)
	sort.Strings(packages)

	var builder strings.Builder
	write := func(name string, values ...string) {
		builder.WriteString(name)
		builder.WriteByte('=')
		for _, value := range values {
			builder.WriteString(strconv.Quote(value))
			builder.WriteByte('\x1f')
		}
		builder.WriteByte('\n')
	}

	write("schema", manifest.Schema)
	write("base.image", manifest.Base.Image)
	write("base.allow_network", strconv.FormatBool(manifest.Base.AllowNetwork))
	write("base.packages", packages...)
	write("application.command", manifest.Application.Command...)
	write("application.workdir", manifest.Application.Workdir)
	write("application.env", manifest.Application.Env...)
	write("application.writable", manifest.Application.Writable...)
	for _, source := range manifest.Copy.Sources {
		write("copy.sources", source.From, source.To, source.Mode)
	}
	write("copy.modules", manifest.Copy.Modules...)
	return []byte(builder.String())
}

// stagedPath resolves a context-relative path inside the staging root and
// refuses anything that would leave it.
func stagedPath(root, relative string) (string, error) {
	if relative == "" || filepath.IsAbs(relative) {
		return "", fmt.Errorf("manifest: %q is not a relative build-context path", relative)
	}
	target := filepath.Join(root, filepath.FromSlash(relative))
	if !within(root, target) {
		return "", fmt.Errorf("manifest: %q escapes the staging directory", relative)
	}
	return target, nil
}

// writeStaged writes a generated file through a temporary file in its target
// directory and renames it into place, so a reader never sees a partial write.
func writeStaged(root, relative string, data []byte, mode os.FileMode) error {
	target, err := stagedPath(root, relative)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("manifest: cannot create %s: %w", filepath.Dir(target), err)
	}

	staged, err := os.CreateTemp(filepath.Dir(target), ".wlvision-*")
	if err != nil {
		return fmt.Errorf("manifest: cannot stage %s: %w", relative, err)
	}
	name := staged.Name()
	if _, err := staged.Write(data); err != nil {
		_ = staged.Close()
		_ = os.Remove(name)
		return fmt.Errorf("manifest: cannot write %s: %w", relative, err)
	}
	if err := staged.Chmod(mode); err != nil {
		_ = staged.Close()
		_ = os.Remove(name)
		return fmt.Errorf("manifest: cannot set the mode of %s: %w", relative, err)
	}
	if err := staged.Close(); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("manifest: cannot close %s: %w", relative, err)
	}
	if err := os.Rename(name, target); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("manifest: cannot publish %s: %w", relative, err)
	}
	return nil
}

// copyFile copies one regular file to an exact mode through a staged write.
func copyFile(ctx context.Context, source, target string, mode os.FileMode) error {
	sourceFile, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("manifest: cannot read %s: %w", source, err)
	}
	defer func() { _ = sourceFile.Close() }()

	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("manifest: cannot create %s: %w", filepath.Dir(target), err)
	}
	staged, err := os.CreateTemp(filepath.Dir(target), ".wlvision-*")
	if err != nil {
		return fmt.Errorf("manifest: cannot stage %s: %w", target, err)
	}
	name := staged.Name()

	if _, err := io.Copy(staged, sourceFile); err != nil {
		_ = staged.Close()
		_ = os.Remove(name)
		return fmt.Errorf("manifest: cannot copy %s: %w", source, err)
	}
	if err := staged.Chmod(mode); err != nil {
		_ = staged.Close()
		_ = os.Remove(name)
		return fmt.Errorf("manifest: cannot set the mode of %s: %w", target, err)
	}
	if err := staged.Close(); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("manifest: cannot close %s: %w", target, err)
	}
	if err := os.Rename(name, target); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("manifest: cannot publish %s: %w", target, err)
	}
	return nil
}

// copyTree copies a file or a directory tree. It skips every .git directory,
// refuses a symbolic link that leaves the tree being copied, and refuses a
// source that is not a regular file. When mode is non-zero every copied file
// gets it; otherwise files keep their own permission bits.
func copyTree(ctx context.Context, source, target string, mode uint32) error {
	// ancestors detects a link cycle by remembering the resolved directories
	// currently being walked.
	ancestors := make(map[string]bool)

	var walk func(from, to string) error
	walk = func(from, to string) error {
		if err := ctx.Err(); err != nil {
			return err
		}

		info, err := os.Lstat(from)
		if err != nil {
			return fmt.Errorf("manifest: cannot read %s: %w", from, err)
		}

		if info.Mode()&os.ModeSymlink != 0 {
			resolved, err := filepath.EvalSymlinks(from)
			if err != nil {
				return fmt.Errorf("manifest: cannot resolve the symbolic link %s: %w", from, err)
			}
			if !within(source, resolved) {
				return fmt.Errorf("manifest: the symbolic link %s points to %s, outside %s; a source never follows a link out of its own tree", from, resolved, source)
			}
			if ancestors[resolved] {
				return fmt.Errorf("manifest: the symbolic link %s forms a cycle through %s", from, resolved)
			}
			ancestors[resolved] = true
			defer delete(ancestors, resolved)
			return walk(resolved, to)
		}

		if info.IsDir() {
			if err := os.MkdirAll(to, 0o755); err != nil {
				return fmt.Errorf("manifest: cannot create %s: %w", to, err)
			}
			entries, err := os.ReadDir(from)
			if err != nil {
				return fmt.Errorf("manifest: cannot read %s: %w", from, err)
			}
			for _, entry := range entries {
				if entry.IsDir() && entry.Name() == ".git" {
					continue
				}
				if err := walk(filepath.Join(from, entry.Name()), filepath.Join(to, entry.Name())); err != nil {
					return err
				}
			}
			return nil
		}

		if !info.Mode().IsRegular() {
			return fmt.Errorf("manifest: %s is a %s, not a regular file", from, describeMode(info.Mode()))
		}

		fileMode := info.Mode().Perm()
		if mode != 0 {
			fileMode = os.FileMode(mode)
		}
		if fileMode == 0 {
			fileMode = 0o644
		}
		return copyFile(ctx, from, to, fileMode)
	}

	return walk(source, target)
}

// outputTail returns the readable tail of a build's output, bounded so a noisy
// engine cannot inflate a failure envelope.
func outputTail(data []byte) string {
	text := strings.TrimSpace(string(data))
	if len(text) <= buildOutputTail {
		return text
	}
	return "..." + text[len(text)-buildOutputTail:]
}
