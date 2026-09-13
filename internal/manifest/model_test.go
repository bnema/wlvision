package manifest

import (
	"bytes"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bnema/wlvision/internal/result"
)

// digest is a syntactically valid digest-pinned reference. It is deliberately
// not a real image: no test here contacts an engine.
const digest = "docker.io/library/archlinux@sha256:6d9b8f3c2a1e4f5d7c8b9a0e1f2d3c4b5a69788796a5b4c3d2e1f0a9b8c7d6e5"

// localImageID is the identifier an engine reports for a locally built image,
// which is how a base without a registry digest is pinned.
const localImageID = "sha256:a1b2c3d4e5f60718293a4b5c6d7e8f901234567890abcdef1234567890abcdef"

// localBaseBody pins a base the engine already holds, the form used for a
// locally built session image with no RepoDigests entry.
func localBaseBody() string {
	return `
schema = "wlvision-manifest/v1"

[base]
image = "wlvision-runtime:local"
digest = "` + localImageID + `"

[application]
command = ["/app/example"]

[copy]
sources = [
  { from = "app.bin", to = "/app/example", mode = "0755" },
]
`
}

// validManifestBody is a manifest every fixture path in the tests satisfies.
func validManifestBody() string {
	return `
schema = "wlvision-manifest/v1"

[base]
image = "` + digest + `"
packages = ["sqlite", "gtk4"]
allow_network = true

[application]
command = ["/app/example", "--flag"]
workdir = "/app"
env = ["KEY=value"]
writable = ["/tmp", "/home/agent"]

[copy]
sources = [
  { from = "app.bin", to = "/app/example", mode = "0755" },
]
`
}

// project returns a manifest directory holding the fixtures the test bodies
// declare, together with a go.mod so the module-neighbourhood rule has a root.
func project(t *testing.T) string {
	t.Helper()

	dir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create fixture directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.bin"), []byte("binary"), 0o755); err != nil {
		t.Fatalf("write app.bin: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "nested"), 0o755); err != nil {
		t.Fatalf("create nested: %v", err)
	}
	// Fixtures the staging bodies declare: a plain file and a directory tree
	// holding files with distinct modes and a .git directory.
	if err := os.WriteFile(filepath.Join(dir, "config.ini"), []byte("key=value"), 0o600); err != nil {
		t.Fatalf("write config.ini: %v", err)
	}
	tree := filepath.Join(dir, "tree")
	if err := os.MkdirAll(filepath.Join(tree, ".git"), 0o755); err != nil {
		t.Fatalf("create tree/.git: %v", err)
	}
	for path, content := range map[string]string{
		filepath.Join(tree, "run.sh"):         "#!/bin/sh\n",
		filepath.Join(tree, "data.txt"):       "data",
		filepath.Join(tree, ".git", "config"): "[core]\n",
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	if err := os.Chmod(filepath.Join(tree, "run.sh"), 0o700); err != nil {
		t.Fatalf("chmod run.sh: %v", err)
	}
	return dir
}

// writeManifest writes one manifest body into dir and returns its path.
func writeManifest(t *testing.T, dir, body string) string {
	t.Helper()

	path := filepath.Join(dir, "wlvision.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return path
}

// failureOf asserts an error is the typed failure the contract promises.
func failureOf(t *testing.T, err error) *result.Failure {
	t.Helper()

	var failure *result.Failure
	if !errors.As(err, &failure) {
		t.Fatalf("error %v (%T) is not a *result.Failure", err, err)
	}
	return failure
}

func TestLoadValidArchProfile(t *testing.T) {
	dir := project(t)
	loaded, err := Load(writeManifest(t, dir, validManifestBody()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if loaded.Schema != Schema {
		t.Errorf("Schema = %q, want %q", loaded.Schema, Schema)
	}
	if loaded.Base.Image != digest {
		t.Errorf("Base.Image = %q, want the pinned digest", loaded.Base.Image)
	}
	if !loaded.Base.AllowNetwork {
		t.Error("Base.AllowNetwork = false, want true")
	}
	if strings.Join(loaded.Base.Packages, ",") != "sqlite,gtk4" {
		t.Errorf("Base.Packages = %v, want the declared order", loaded.Base.Packages)
	}
	if len(loaded.Application.Command) != 2 || loaded.Application.Command[0] != "/app/example" {
		t.Errorf("Application.Command = %v, want the declared command", loaded.Application.Command)
	}
	if loaded.Application.Workdir != "/app" {
		t.Errorf("Application.Workdir = %q, want /app", loaded.Application.Workdir)
	}
	if strings.Join(loaded.Application.Writable, ",") != "/tmp,/home/agent" {
		t.Errorf("Application.Writable = %v", loaded.Application.Writable)
	}
	if len(loaded.Copy.Sources) != 1 || loaded.Copy.Sources[0].Mode != "0755" {
		t.Errorf("Copy.Sources = %+v, want one declared source", loaded.Copy.Sources)
	}
}

func TestLoadRefusals(t *testing.T) {
	base := validManifestBody()

	tests := []struct {
		name  string
		body  string
		field string
	}{
		{
			name:  "tag without digest",
			body:  strings.Replace(base, `image = "`+digest+`"`, `image = "docker.io/library/archlinux:latest"`, 1),
			field: "base.image",
		},
		{
			name:  "absolute source",
			body:  strings.Replace(base, `from = "app.bin"`, `from = "/etc/passwd"`, 1),
			field: "copy.sources[0].from",
		},
		{
			name:  "escaping symlink",
			body:  strings.Replace(base, `from = "app.bin"`, `from = "escape"`, 1),
			field: "copy.sources[0].from",
		},
		{
			name:  "socket source",
			body:  strings.Replace(base, `from = "app.bin"`, `from = "app.sock"`, 1),
			field: "copy.sources[0].from",
		},
		{
			name:  "credential path",
			body:  strings.Replace(base, `from = "app.bin"`, `from = ".ssh/id_rsa"`, 1),
			field: "copy.sources[0].from",
		},
		{
			name: "duplicate target",
			body: strings.Replace(base, `sources = [
  { from = "app.bin", to = "/app/example", mode = "0755" },
]`, `sources = [
  { from = "app.bin", to = "/app/example", mode = "0755" },
  { from = "app.bin", to = "/app/example", mode = "0644" },
]`, 1),
			field: "copy.sources[1].to",
		},
		{
			name:  "local reference without any digest",
			body:  strings.Replace(base, `image = "`+digest+`"`, `image = "wlvision-runtime:local"`, 1),
			field: "base.image",
		},
		{
			name:  "malformed local digest",
			body:  strings.Replace(localBaseBody(), localImageID, "sha256:nothex", 1),
			field: "base.digest",
		},
		{
			name:  "digest declared beside a reference digest",
			body:  strings.Replace(base, "allow_network = true", "allow_network = true\ndigest = \""+localImageID+"\"", 1),
			field: "base.digest",
		},
		{
			name:  "runtime network key",
			body:  base + "\n[runtime]\nnetwork = true\n",
			field: "runtime.network",
		},
		{
			name:  "relative writable path",
			body:  strings.Replace(base, `writable = ["/tmp", "/home/agent"]`, `writable = ["tmp", "/home/agent"]`, 1),
			field: "application.writable[0]",
		},
		{
			name:  "writable under /run",
			body:  strings.Replace(base, `writable = ["/tmp", "/home/agent"]`, `writable = ["/tmp", "/run/wlvision/data"]`, 1),
			field: "application.writable[1]",
		},
		{
			name:  "env without equals",
			body:  strings.Replace(base, `env = ["KEY=value"]`, `env = ["KEY"]`, 1),
			field: "application.env[0]",
		},
		{
			name:  "non-absolute command",
			body:  strings.Replace(base, `command = ["/app/example", "--flag"]`, `command = ["app/example", "--flag"]`, 1),
			field: "application.command",
		},
		{
			name:  "target inside a writable path",
			body:  strings.Replace(base, `to = "/app/example"`, `to = "/tmp/example"`, 1),
			field: "copy.sources[0].to",
		},
		{
			name:  "module without go.mod",
			body:  base + "\nmodules = [\"nested\"]\n",
			field: "copy.modules[0]",
		},
		{
			name:  "module outside the neighbourhood",
			body:  base + "\nmodules = [\"{{OUTSIDE}}\"]\n",
			field: "copy.modules[0]",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := project(t)

			if err := os.MkdirAll(filepath.Join(dir, ".ssh"), 0o700); err != nil {
				t.Fatalf("create .ssh: %v", err)
			}
			if err := os.WriteFile(filepath.Join(dir, ".ssh", "id_rsa"), []byte("key"), 0o600); err != nil {
				t.Fatalf("write id_rsa: %v", err)
			}

			secret := filepath.Join(filepath.Dir(dir), "secret")
			if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
				t.Fatalf("write secret: %v", err)
			}
			if err := os.Symlink(secret, filepath.Join(dir, "escape")); err != nil {
				t.Fatalf("symlink: %v", err)
			}

			listener, err := net.Listen("unix", filepath.Join(dir, "app.sock"))
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			t.Cleanup(func() { _ = listener.Close() })

			outside := t.TempDir()
			if err := os.WriteFile(filepath.Join(outside, "go.mod"), []byte("module outside\n"), 0o644); err != nil {
				t.Fatalf("write outside go.mod: %v", err)
			}
			relative, err := filepath.Rel(dir, outside)
			if err != nil {
				t.Fatalf("relative path: %v", err)
			}

			body := strings.ReplaceAll(test.body, "{{OUTSIDE}}", relative)
			_, err = Load(writeManifest(t, dir, body))
			if err == nil {
				t.Fatal("the manifest was accepted")
			}

			failure := failureOf(t, err)
			if failure.Code != result.CodeUsageError {
				t.Errorf("code = %s, want %s", failure.Code, result.CodeUsageError)
			}
			if !strings.Contains(failure.Message, test.field) {
				t.Errorf("message %q does not name %q", failure.Message, test.field)
			}
		})
	}
}

func TestLoadRefusesAnUnknownSchema(t *testing.T) {
	dir := project(t)
	body := strings.Replace(validManifestBody(), `schema = "`+Schema+`"`, `schema = "wlvision-manifest/v2"`, 1)

	_, err := Load(writeManifest(t, dir, body))
	failure := failureOf(t, err)
	if failure.Code != result.CodeUsageError {
		t.Errorf("code = %s, want %s", failure.Code, result.CodeUsageError)
	}
	if !strings.Contains(failure.Message, "schema") {
		t.Errorf("message %q does not name the schema field", failure.Message)
	}
}

func TestLoadValidLocalBase(t *testing.T) {
	dir := project(t)
	loaded, err := Load(writeManifest(t, dir, localBaseBody()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Base.Image != "wlvision-runtime:local" {
		t.Errorf("Base.Image = %q, want the engine-held reference", loaded.Base.Image)
	}
	if loaded.Base.Digest != localImageID {
		t.Errorf("Base.Digest = %q, want %q", loaded.Base.Digest, localImageID)
	}

	planned, err := Plan(loaded)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if planned.Digest != localImageID {
		t.Errorf("Digest = %q, want the verified local image id", planned.Digest)
	}
	if !strings.Contains(string(planned.Containerfile), "FROM wlvision-runtime:local") {
		t.Errorf("the Containerfile must name the reference it builds from:\n%s", planned.Containerfile)
	}
	if strings.Contains(string(planned.Containerfile), localImageID) {
		t.Errorf("the Containerfile must not need the image id:\n%s", planned.Containerfile)
	}
}

func TestExampleManifestLoadsAndPlans(t *testing.T) {
	loaded, err := Load(filepath.Join("..", "..", "wlvision.example.toml"))
	if err != nil {
		t.Fatalf("the shipped example does not load: %v", err)
	}

	planned, err := Plan(loaded)
	if err != nil {
		t.Fatalf("the shipped example does not plan: %v", err)
	}
	if !bytes.Contains(planned.Containerfile, []byte("FROM "+digest)) {
		t.Errorf("the example Containerfile must carry the pinned digest:\n%s", planned.Containerfile)
	}
	if !bytes.Equal(planned.Containerfile, renderContainerfile(planned)) {
		t.Error("the example Containerfile is not reproducible")
	}
	if !strings.HasPrefix(planned.Tag, tagPrefix) {
		t.Errorf("Tag = %q, want the %q prefix", planned.Tag, tagPrefix)
	}
}
