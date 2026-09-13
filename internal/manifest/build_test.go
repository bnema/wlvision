package manifest

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bnema/wlvision/internal/engine"
	"github.com/bnema/wlvision/internal/enginetest"
	"github.com/bnema/wlvision/internal/result"
)

// stagingBody declares one file with a mode, one plain file, and one directory
// tree, which is everything Stage has to get right.
func stagingBody() string {
	return `
schema = "wlvision-manifest/v1"

[base]
image = "` + digest + `"
packages = ["sqlite", "gtk4"]

[application]
command = ["/app/example", "--flag"]
workdir = "/app"
env = ["KEY=value"]

[copy]
sources = [
  { from = "app.bin", to = "/app/example", mode = "0755" },
  { from = "config.ini", to = "/app/config.ini" },
  { from = "tree", to = "/app/tree" },
]
`
}

func writeFixture(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func loadAndPlan(t *testing.T, path string) BuildPlan {
	t.Helper()

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	planned, err := Plan(loaded)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return planned
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if info.Mode().Perm() != want {
		t.Errorf("%s mode = %04o, want %04o", path, info.Mode().Perm(), want)
	}
}

func TestPlanIsDeterministicAndDeclaresNoHostPath(t *testing.T) {
	dir := project(t)
	first := loadAndPlan(t, writeManifest(t, dir, stagingBody()))
	second := loadAndPlan(t, writeManifest(t, dir, stagingBody()))

	if !bytes.Equal(first.Containerfile, second.Containerfile) {
		t.Errorf("two plans of one manifest produced different Containerfiles:\n%s\n---\n%s", first.Containerfile, second.Containerfile)
	}
	if first.Tag != second.Tag {
		t.Errorf("tags differ: %q vs %q", first.Tag, second.Tag)
	}
	if len(first.Tag) != len(tagPrefix)+tagDigestLength || !strings.HasPrefix(first.Tag, tagPrefix) {
		t.Errorf("Tag = %q, want %q plus %d hex digits", first.Tag, tagPrefix, tagDigestLength)
	}

	text := string(first.Containerfile)
	for _, want := range []string{
		"FROM " + digest,
		"RUN pacman -Sy --noconfirm gtk4 sqlite",
		"COPY --chmod=0755 src/0/app.bin /app/example",
		"COPY src/1/config.ini /app/config.ini",
		"COPY src/2/tree/ /app/tree/",
		"ENV KEY=value",
		"WORKDIR /app",
		`CMD ["/app/example","--flag"]`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the Containerfile is missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, dir) {
		t.Errorf("the Containerfile names the host path %q:\n%s", dir, text)
	}
	for _, forbidden := range []string{"network", "secret", "token"} {
		if strings.Contains(strings.ToLower(text), forbidden) {
			t.Errorf("the Containerfile contains %q:\n%s", forbidden, text)
		}
	}
}

func TestStageCopiesDeclaredInputsWithModes(t *testing.T) {
	dir := project(t)
	writeFixture(t, filepath.Join(dir, "config.ini"), "key=value", 0o600)
	tree := filepath.Join(dir, "tree")
	writeFixture(t, filepath.Join(tree, "run.sh"), "#!/bin/sh\n", 0o700)
	writeFixture(t, filepath.Join(tree, "data.txt"), "data", 0o600)
	writeFixture(t, filepath.Join(tree, ".git", "config"), "[core]\n", 0o644)

	planned := loadAndPlan(t, writeManifest(t, dir, stagingBody()))
	staging := t.TempDir()
	if err := Stage(context.Background(), planned, staging); err != nil {
		t.Fatalf("Stage: %v", err)
	}

	assertMode(t, filepath.Join(staging, "src", "0", "app.bin"), 0o755)
	assertMode(t, filepath.Join(staging, "src", "1", "config.ini"), 0o644)
	assertMode(t, filepath.Join(staging, "src", "2", "tree", "run.sh"), 0o700)
	assertMode(t, filepath.Join(staging, "src", "2", "tree", "data.txt"), 0o600)

	content, err := os.ReadFile(filepath.Join(staging, "src", "0", "app.bin"))
	if err != nil {
		t.Fatalf("read the staged source: %v", err)
	}
	if string(content) != "binary" {
		t.Errorf("staged content = %q, want the source content", content)
	}

	if _, err := os.Stat(filepath.Join(staging, "src", "2", "tree", ".git")); !os.IsNotExist(err) {
		t.Error("a .git directory was staged")
	}

	stagedManifest, err := os.ReadFile(filepath.Join(staging, "manifest.toml"))
	if err != nil {
		t.Fatalf("read the staged manifest: %v", err)
	}
	if string(stagedManifest) != stagingBody() {
		t.Error("manifest.toml is not a copy of the manifest")
	}

	entries, err := os.ReadDir(staging)
	if err != nil {
		t.Fatalf("read the staging root: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if strings.Join(names, ",") != "Containerfile,manifest.toml,src" {
		t.Errorf("staging root = %v, want exactly the generated inputs", names)
	}
}

func TestStageProducesStableContainerfileBytes(t *testing.T) {
	dir := project(t)
	planned := loadAndPlan(t, writeManifest(t, dir, stagingBody()))

	first := t.TempDir()
	second := t.TempDir()
	if err := Stage(context.Background(), planned, first); err != nil {
		t.Fatalf("Stage into the first context: %v", err)
	}
	if err := Stage(context.Background(), planned, second); err != nil {
		t.Fatalf("Stage into the second context: %v", err)
	}

	one, err := os.ReadFile(filepath.Join(first, "Containerfile"))
	if err != nil {
		t.Fatalf("read the first Containerfile: %v", err)
	}
	two, err := os.ReadFile(filepath.Join(second, "Containerfile"))
	if err != nil {
		t.Fatalf("read the second Containerfile: %v", err)
	}
	if !bytes.Equal(one, two) {
		t.Error("two stagings produced different Containerfile bytes")
	}
	if !bytes.Equal(one, planned.Containerfile) {
		t.Error("the staged Containerfile differs from the plan")
	}
}

func TestStageRefusesAnEscapingSymlink(t *testing.T) {
	dir := project(t)
	tree := filepath.Join(dir, "tree")
	writeFixture(t, filepath.Join(tree, "inside.txt"), "inside", 0o644)

	secret := filepath.Join(filepath.Dir(dir), "secret")
	writeFixture(t, secret, "secret", 0o600)
	if err := os.Symlink(secret, filepath.Join(tree, "escape")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	body := strings.Replace(stagingBody(), `{ from = "app.bin", to = "/app/example", mode = "0755" },
  { from = "config.ini", to = "/app/config.ini" },
`, "", 1)
	planned := loadAndPlan(t, writeManifest(t, dir, body))

	err := Stage(context.Background(), planned, t.TempDir())
	if err == nil {
		t.Fatal("a source tree with an escaping symlink was staged")
	}
	if !strings.Contains(err.Error(), "outside") {
		t.Errorf("error %q does not explain that the link escapes", err)
	}
}

func TestPlanResolvesSiblingModules(t *testing.T) {
	dir := project(t)
	sibling := filepath.Join(filepath.Dir(dir), "wlturbo")
	writeFixture(t, filepath.Join(sibling, "go.mod"), "module example.com/wlturbo\n", 0o644)

	body := validManifestBody() + "\nmodules = [\"../wlturbo\"]\n"
	planned := loadAndPlan(t, writeManifest(t, dir, body))

	if len(planned.Modules) != 1 {
		t.Fatalf("Modules = %+v, want one module", planned.Modules)
	}
	module := planned.Modules[0]
	if module.HostPath != sibling {
		t.Errorf("HostPath = %q, want %q", module.HostPath, sibling)
	}
	if module.Name != "wlturbo" || module.Target != "/src/wlturbo" {
		t.Errorf("module = %+v, want the wlturbo tree under /src", module)
	}
	if !strings.Contains(string(planned.Containerfile), "COPY "+module.ContextPath+"/ /src/wlturbo/") {
		t.Errorf("the Containerfile does not copy the module:\n%s", planned.Containerfile)
	}
}

func TestBuildUsesTheManifestNetworkPolicy(t *testing.T) {
	for _, test := range []struct {
		name    string
		allowed bool
	}{
		{name: "a manifest that stays offline", allowed: false},
		{name: "a manifest that allows the build network", allowed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := project(t)
			body := validManifestBody()
			if !test.allowed {
				body = strings.Replace(body, "allow_network = true", "", 1)
			}
			planned := loadAndPlan(t, writeManifest(t, dir, body))

			fake := enginetest.New()
			fake.BuildValue = engine.BuildResult{ImageID: "sha256:" + strings.Repeat("a", 64)}
			fake.BuildOutput = "==> building gtk4\n"

			staging := t.TempDir()
			built, err := Build(context.Background(), fake, planned, staging)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}

			if built.Tag != planned.Tag {
				t.Errorf("Tag = %q, want %q", built.Tag, planned.Tag)
			}
			if built.ImageID != fake.BuildValue.ImageID {
				t.Errorf("ImageID = %q, want the engine's answer", built.ImageID)
			}
			if !strings.Contains(built.Output, "building gtk4") {
				t.Errorf("Output = %q, want the build transcript", built.Output)
			}
			if _, err := os.Stat(filepath.Join(staging, "Containerfile")); err != nil {
				t.Errorf("Build did not stage the context: %v", err)
			}

			specs := fake.Builds()
			if len(specs) != 1 {
				t.Fatalf("the engine saw %d builds, want 1", len(specs))
			}
			spec := specs[0]
			if spec.Network != test.allowed {
				t.Errorf("Network = %t, want %t", spec.Network, test.allowed)
			}
			if spec.Tag != planned.Tag || spec.Context != staging || spec.Containerfile != "Containerfile" {
				t.Errorf("spec = %+v, want the plan's tag, the staging context, and Containerfile", spec)
			}
			if fake.CountCalls("build ") != 1 {
				t.Errorf("call log = %v, want exactly one build", fake.CallLog())
			}
			if fake.CountCalls("image-id ") != 0 {
				t.Errorf("call log = %v, want no image-id lookup for a reference-digest base", fake.CallLog())
			}
		})
	}
}

func TestBuildVerifiesAnEngineHeldBase(t *testing.T) {
	dir := project(t)
	planned := loadAndPlan(t, writeManifest(t, dir, localBaseBody()))
	if planned.Digest != localImageID {
		t.Fatalf("Digest = %q, want the declared local image id", planned.Digest)
	}

	fake := enginetest.New()
	fake.ImageIDValue = localImageID
	fake.BuildValue = engine.BuildResult{ImageID: "sha256:" + strings.Repeat("c", 64)}
	fake.BuildOutput = "==> building from the local base\n"

	built, err := Build(context.Background(), fake, planned, t.TempDir())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if built.Tag != planned.Tag {
		t.Errorf("Tag = %q, want %q", built.Tag, planned.Tag)
	}
	if fake.CountCalls("image-id wlvision-runtime:local") != 1 {
		t.Errorf("call log = %v, want the base image id to be read", fake.CallLog())
	}
	if len(fake.Builds()) != 1 {
		t.Errorf("the engine saw %d builds, want 1", len(fake.Builds()))
	}
}

func TestBuildRefusesAMismatchedEngineHeldBase(t *testing.T) {
	dir := project(t)
	planned := loadAndPlan(t, writeManifest(t, dir, localBaseBody()))

	other := "sha256:" + strings.Repeat("b", 64)
	fake := enginetest.New()
	fake.ImageIDValue = other

	staging := t.TempDir()
	_, err := Build(context.Background(), fake, planned, staging)
	if err == nil {
		t.Fatal("a base whose id does not match the manifest was accepted")
	}

	failure := failureOf(t, err)
	if failure.Code != result.CodeImageUnavailable {
		t.Errorf("code = %s, want %s", failure.Code, result.CodeImageUnavailable)
	}
	if !strings.Contains(failure.Message, localImageID) || !strings.Contains(failure.Message, other) {
		t.Errorf("message %q does not name both ids", failure.Message)
	}
	if len(fake.Builds()) != 0 || fake.CountCalls("build ") != 0 {
		t.Errorf("call log = %v, want no build for a mismatched base", fake.CallLog())
	}
	if _, err := os.Stat(filepath.Join(staging, "Containerfile")); !os.IsNotExist(err) {
		t.Error("the context was staged before the base id was verified")
	}
}

func TestBuildRefusesAnEngineHeldBaseTheEngineLacks(t *testing.T) {
	dir := project(t)
	planned := loadAndPlan(t, writeManifest(t, dir, localBaseBody()))

	fake := enginetest.New()
	fake.ImageIDErr = errors.New("engine: no such image")

	_, err := Build(context.Background(), fake, planned, t.TempDir())
	if err == nil {
		t.Fatal("a missing base image was accepted")
	}
	failure := failureOf(t, err)
	if failure.Code != result.CodeImageUnavailable {
		t.Errorf("code = %s, want %s", failure.Code, result.CodeImageUnavailable)
	}
	if fake.CountCalls("build ") != 0 {
		t.Errorf("call log = %v, want no build for a missing base", fake.CallLog())
	}
}

func TestBuildMapsAFailureToImageUnavailable(t *testing.T) {
	dir := project(t)
	planned := loadAndPlan(t, writeManifest(t, dir, validManifestBody()))

	fake := enginetest.New()
	fake.BuildErr = errors.New("exit status 1")
	fake.BuildOutput = "step 2/3: ERROR: unable to resolve package gtk4\n"

	_, err := Build(context.Background(), fake, planned, t.TempDir())
	if err == nil {
		t.Fatal("a failed build was reported as success")
	}

	failure := failureOf(t, err)
	if failure.Code != result.CodeImageUnavailable {
		t.Errorf("code = %s, want %s", failure.Code, result.CodeImageUnavailable)
	}
	if !strings.Contains(failure.Details["build_output"], "unable to resolve package gtk4") {
		t.Errorf("details = %v, want the tail of the build output", failure.Details)
	}
}
