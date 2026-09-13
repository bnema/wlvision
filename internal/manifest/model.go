// Package manifest reads, validates, plans, and builds the wlvision image
// manifest: the one description of a derived session image wlvision accepts.
//
// A manifest names a digest-pinned base, an optional package transaction,
// files copied out of the manifest's own directory, sibling Go modules, and
// the application the session runs. Every path a manifest declares is resolved
// and checked here, before any engine command is built, so the build context
// can never contain a host file the manifest did not ask for.
//
// The schema has no runtime-network key. A session never gets networking; the
// only network a manifest may request is [base].allow_network, which affects
// the build alone and never the session. A manifest that tries to set a
// runtime network key is refused by the unknown-key rule rather than silently
// ignored.
package manifest

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/bnema/wlvision/internal/result"
)

// Schema is the only manifest schema version this package accepts.
const Schema = "wlvision-manifest/v1"

// digestPattern is the digest form a base image must carry: 64 lowercase
// hexadecimal digits, as an OCI digest after "@sha256:".
var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// credentialDirectories are never copied into an image. A build context is
// handed to the container engine and may be retained in a layer, so a
// credential that reaches it is a credential that leaked.
var credentialDirectories = map[string]bool{
	".ssh":    true,
	".gnupg":  true,
	".aws":    true,
	".docker": true,
	".kube":   true,
}

// credentialNames are file names that are always refused, whatever they hold.
var credentialNames = map[string]bool{
	"id_rsa":          true,
	"id_ed25519":      true,
	".netrc":          true,
	"authorized_keys": true,
	"credentials":     true,
}

// credentialSuffixes are file suffixes that are always refused.
var credentialSuffixes = []string{".pem", ".key", ".sock"}

// Manifest is one decoded and validated wlvision manifest.
//
// The decoded struct holds the declared fields only; the directory the
// manifest was read from is kept so every relative path resolves against it,
// and the raw bytes are kept so the build context can carry a copy of the
// manifest that produced it.
type Manifest struct {
	Schema      string      `toml:"schema"`
	Base        Base        `toml:"base"`
	Application Application `toml:"application"`
	Copy        Copy        `toml:"copy"`

	// dir is the absolute directory holding the manifest file. Relative
	// paths in every section resolve against it.
	dir string
	// realDir is dir with symbolic links resolved, so containment checks
	// compare like with like.
	realDir string
	// file is the absolute path the manifest was read from.
	file string
	// source is the manifest's own bytes, copied into the build context.
	source []byte
}

// Base describes the image a build starts from and the packages it installs.
type Base struct {
	// Image must be digest-pinned. A tag without a digest is refused: a tag
	// moves, and a session must record the bytes it actually ran.
	Image string `toml:"image"`
	// Packages are installed in one transaction when non-empty.
	Packages []string `toml:"packages"`
	// AllowNetwork permits network access during the build only. It defaults
	// to false, and there is deliberately no runtime counterpart.
	AllowNetwork bool `toml:"allow_network"`
}

// Application describes what the session runs.
type Application struct {
	// Command is the image's default command. Its first element must be an
	// absolute path so the engine never resolves it through PATH.
	Command []string `toml:"command"`
	// Workdir is the image's working directory. Empty means the base's.
	Workdir string `toml:"workdir"`
	// Env entries are "KEY=VALUE" with a non-empty key.
	Env []string `toml:"env"`
	// Writable lists absolute paths that must be writable at runtime. A copy
	// target may not live inside one, and /run is wlvision's own mount.
	Writable []string `toml:"writable"`
}

// Copy describes what enters the build context from the manifest directory.
type Copy struct {
	// Sources are files or directory trees copied into the image.
	Sources []Source `toml:"sources"`
	// Modules are sibling Go modules copied into the context for local
	// dependency iteration. They are never bind-mounted into a session.
	Modules []string `toml:"modules"`
}

// Source is one host path copied into the image.
type Source struct {
	// From is a non-absolute path below the manifest directory.
	From string `toml:"from"`
	// To is the absolute path in the image.
	To string `toml:"to"`
	// Mode is an octal file mode such as "0755". Empty means 0644 for a
	// file and the source's own mode for a directory tree.
	Mode string `toml:"mode"`
}

// Load reads, decodes, and validates one manifest. It returns a manifest that
// keeps its own directory, so Plan can resolve the relative paths it declares.
func Load(path string) (Manifest, error) {
	if strings.TrimSpace(path) == "" {
		return Manifest{}, usage("path", "a manifest path is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return Manifest{}, usage("path", "cannot resolve %q: %v", path, err)
	}
	data, err := os.ReadFile(absolute)
	if err != nil {
		return Manifest{}, usage("path", "cannot read the manifest %q: %v", absolute, err)
	}

	var manifest Manifest
	metadata, err := toml.Decode(string(data), &manifest)
	if err != nil {
		return Manifest{}, usage("manifest", "%s is not valid TOML: %v", filepath.Base(absolute), err)
	}
	if undecoded := metadata.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, 0, len(undecoded))
		for _, key := range undecoded {
			keys = append(keys, key.String())
		}
		sort.Strings(keys)
		return Manifest{}, usage(strings.Join(keys, ","),
			"unknown key %s: the %s schema has no such field (there is no runtime network key; base.allow_network is build-time only)",
			strings.Join(keys, ", "), Schema)
	}

	manifest.dir = filepath.Dir(absolute)
	manifest.file = absolute
	manifest.source = append([]byte(nil), data...)
	if real, err := filepath.EvalSymlinks(manifest.dir); err == nil {
		manifest.realDir = real
	} else {
		manifest.realDir = manifest.dir
	}
	if err := manifest.validate(); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

// validate applies every rule the schema states. Each refusal is a usage
// failure naming the offending field.
func (m Manifest) validate() error {
	if m.Schema != Schema {
		return usage("schema", "schema is %q, want %q", m.Schema, Schema)
	}
	if err := m.Base.validate(); err != nil {
		return err
	}
	if err := m.Application.validate(); err != nil {
		return err
	}
	return m.Copy.validate(m)
}

// validate checks the base image and package list.
func (b Base) validate() error {
	index := strings.LastIndex(b.Image, "@sha256:")
	if index <= 0 {
		return usage("base.image",
			"%q has no image digest; a tag or bare name is refused and a @sha256: digest is required", b.Image)
	}
	digest := b.Image[index+len("@sha256:"):]
	if !digestPattern.MatchString(digest) {
		return usage("base.image",
			"the digest in %q is not 64 lowercase hexadecimal digits", b.Image)
	}
	for i, name := range b.Packages {
		if name == "" {
			return usage(fmt.Sprintf("base.packages[%d]", i), "a package name must not be empty")
		}
	}
	return nil
}

// validate checks the application section.
func (a Application) validate() error {
	if len(a.Command) == 0 {
		return usage("application.command", "a command is required")
	}
	if !filepath.IsAbs(a.Command[0]) {
		return usage("application.command",
			"the first element %q is not an absolute path", a.Command[0])
	}

	for i, entry := range a.Env {
		field := fmt.Sprintf("application.env[%d]", i)
		key, _, found := strings.Cut(entry, "=")
		if !found || key == "" {
			return usage(field, "%q is not KEY=VALUE with a non-empty key", entry)
		}
		if strings.ContainsRune(entry, 0) {
			return usage(field, "%q contains a NUL byte", entry)
		}
	}

	seen := make(map[string]bool, len(a.Writable))
	for i, path := range a.Writable {
		field := fmt.Sprintf("application.writable[%d]", i)
		if !filepath.IsAbs(path) {
			return usage(field, "%q is not an absolute path", path)
		}
		clean := filepath.Clean(path)
		if clean == "/" {
			return usage(field, "the image root cannot be declared writable")
		}
		if within("/run", clean) {
			return usage(field, "%q is under /run, which wlvision mounts itself", path)
		}
		if seen[clean] {
			return usage(field, "%q is declared twice", path)
		}
		seen[clean] = true
	}
	return nil
}

// validate checks the copy section against the manifest directory and the
// declared writable paths.
func (c Copy) validate(m Manifest) error {
	targets := make(map[string]int, len(c.Sources))
	for i, source := range c.Sources {
		field := fmt.Sprintf("copy.sources[%d]", i)
		if err := source.validateSource(m, field); err != nil {
			return err
		}

		clean := filepath.Clean(source.To)
		for _, writable := range m.Application.Writable {
			if within(filepath.Clean(writable), clean) {
				return usage(field+".to",
					"%q is inside the declared writable path %q", source.To, writable)
			}
		}
		for _, reserved := range []string{"/run", "/proc", "/sys", "/dev"} {
			if within(reserved, clean) {
				return usage(field+".to",
					"%q is under %s, which a session owns", source.To, reserved)
			}
		}
		if previous, ok := targets[clean]; ok {
			return usage(field+".to",
				"%q duplicates the target of copy.sources[%d]", source.To, previous)
		}
		targets[clean] = i
	}

	for i, module := range c.Modules {
		if err := m.validateModule(fmt.Sprintf("copy.modules[%d]", i), module); err != nil {
			return err
		}
	}
	return nil
}

// validateSource checks one declared source, resolving it through symbolic
// links so a link cannot point out of the manifest directory.
func (s Source) validateSource(m Manifest, field string) error {
	if s.From == "" {
		return usage(field+".from", "a source path is required")
	}
	if filepath.IsAbs(s.From) {
		return usage(field+".from",
			"%q is absolute; a source path is relative to the manifest directory", s.From)
	}

	resolved, err := filepath.EvalSymlinks(filepath.Join(m.dir, filepath.FromSlash(s.From)))
	if err != nil {
		if os.IsNotExist(err) {
			return usage(field+".from", "%q does not exist under %s", s.From, m.dir)
		}
		return usage(field+".from", "cannot resolve %q: %v", s.From, err)
	}
	if !within(m.realDir, resolved) {
		return usage(field+".from",
			"%q resolves to %s, outside the manifest directory %s; a source may not escape with .. or a symbolic link",
			s.From, resolved, m.realDir)
	}

	info, err := os.Lstat(resolved)
	if err != nil {
		return usage(field+".from", "cannot inspect %q: %v", s.From, err)
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return usage(field+".from",
			"%q is a %s, not a regular file or a directory tree", s.From, describeMode(info.Mode()))
	}

	if relative, err := filepath.Rel(m.realDir, resolved); err == nil {
		if ok, why := credential(relative); ok {
			return usage(field+".from", "%q %s and is never copied into an image", s.From, why)
		}
	}

	if s.To == "" {
		return usage(field+".to", "a target path is required")
	}
	if !filepath.IsAbs(s.To) {
		return usage(field+".to", "%q is not an absolute path", s.To)
	}
	if s.Mode != "" {
		if _, err := parseMode(s.Mode); err != nil {
			return usage(field+".mode", "%q is not an octal file mode: %v", s.Mode, err)
		}
	}
	return nil
}

// validateModule checks one sibling Go module: it must exist, hold a go.mod,
// and resolve inside the parent of the repository root, which is where a
// sibling checkout lives.
func (m Manifest) validateModule(field, module string) error {
	if module == "" {
		return usage(field, "a module path is required")
	}
	if filepath.IsAbs(module) {
		return usage(field, "%q is absolute; a module is relative to the manifest directory", module)
	}

	resolved, err := filepath.EvalSymlinks(filepath.Join(m.dir, filepath.FromSlash(module)))
	if err != nil {
		if os.IsNotExist(err) {
			return usage(field, "%q does not exist under %s", module, m.dir)
		}
		return usage(field, "cannot resolve %q: %v", module, err)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return usage(field, "%q is not a directory", module)
	}
	if mod, err := os.Stat(filepath.Join(resolved, "go.mod")); err != nil || !mod.Mode().IsRegular() {
		return usage(field, "%q has no go.mod, so it is not a Go module", module)
	}

	neighbourhood := filepath.Dir(m.moduleRoot())
	if !within(neighbourhood, resolved) {
		return usage(field,
			"%q resolves to %s, outside %s, the parent of the module root; a module must be a sibling of the repository",
			module, resolved, neighbourhood)
	}
	return nil
}

// moduleRoot walks up from the manifest directory to the repository root, the
// nearest ancestor holding a go.mod.
func (m Manifest) moduleRoot() string {
	dir := m.realDir
	for {
		if info, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil && info.Mode().IsRegular() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return m.realDir
		}
		dir = parent
	}
}

// usage builds the failure every validation rule returns: a usage error naming
// the field that is wrong.
func usage(field, format string, args ...any) *result.Failure {
	return result.NewFailure(result.CodeUsageError, "manifest",
		"%s: %s", field, fmt.Sprintf(format, args...))
}

// within reports whether path is root itself or below it.
func within(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// credential reports whether a path below the manifest directory is a
// credential path and why.
func credential(relative string) (bool, string) {
	for _, element := range strings.Split(filepath.ToSlash(relative), "/") {
		if credentialDirectories[element] {
			return true, fmt.Sprintf("is under the %q directory", element)
		}
		name := strings.ToLower(element)
		if credentialNames[name] {
			return true, fmt.Sprintf("names the credential file %q", element)
		}
		for _, suffix := range credentialSuffixes {
			if strings.HasSuffix(name, suffix) {
				return true, fmt.Sprintf("names the credential file %q", element)
			}
		}
	}
	return false, ""
}

// describeMode names a file's kind for a refusal message.
func describeMode(mode os.FileMode) string {
	switch {
	case mode&os.ModeSocket != 0:
		return "socket"
	case mode&os.ModeNamedPipe != 0:
		return "FIFO"
	case mode&os.ModeDevice != 0:
		return "device"
	case mode&os.ModeSymlink != 0:
		return "symbolic link"
	default:
		return "special file"
	}
}

// parseMode parses an octal file mode such as "0755".
func parseMode(value string) (uint32, error) {
	parsed, err := strconv.ParseUint(value, 8, 32)
	if err != nil {
		return 0, err
	}
	if parsed > 0o7777 {
		return 0, fmt.Errorf("%s is larger than 07777", value)
	}
	return uint32(parsed), nil
}
