package session

import (
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

// imageManifest writes a manifest whose base carries its own digest, so no
// image lookup is involved.
func imageManifest(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	body := `schema = "wlvision-manifest/v1"

[base]
image = "example.invalid/runtime@sha256:` + strings.Repeat("ab", 32) + `"
packages = ["example-package"]
allow_network = true

[application]
command = ["/app/example"]
`
	path := filepath.Join(dir, "wlvision.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write the manifest: %v", err)
	}
	return path
}

func TestBuildImageStagesAndBuildsThroughTheEngine(t *testing.T) {
	fake := enginetest.New()
	fake.BuildValue = engine.BuildResult{ImageID: "sha256:" + strings.Repeat("cd", 32)}
	fake.BuildOutput = "==> installing example-package\n"
	store := newTestStore(t)
	service := newTestService(t, fake, store)

	built, err := service.BuildImage(context.Background(), BuildRequest{Manifest: imageManifest(t)})
	if err != nil {
		t.Fatalf("BuildImage: %v", err)
	}
	if built.Tag == "" || built.ImageID == "" {
		t.Errorf("result = %+v, want a tag and an image identifier", built)
	}
	if calls := fake.CountCalls("build "); calls != 1 {
		t.Errorf("the engine built %d times, want once: %v", calls, fake.CallLog())
	}
	if !strings.Contains(built.Output, "installing example-package") {
		t.Errorf("the build recorded no output: %q", built.Output)
	}
}

func TestBuildImageKeepsTheContextOfAFailedBuild(t *testing.T) {
	fake := enginetest.New()
	fake.BuildErr = errors.New("the package transaction failed")
	store := newTestStore(t)
	service := newTestService(t, fake, store)

	_, err := service.BuildImage(context.Background(), BuildRequest{Manifest: imageManifest(t)})
	if err == nil {
		t.Fatal("a failed build was reported as success")
	}
	var failure *result.Failure
	if !errors.As(err, &failure) {
		t.Fatalf("error %v is not a *result.Failure", err)
	}
	staging := failure.Details["staging"]
	if staging == "" {
		t.Fatalf("the failure names no build context: %+v", failure.Details)
	}
	if _, statErr := os.Stat(filepath.Join(staging, "Containerfile")); statErr != nil {
		t.Errorf("the build context is not kept: %v", statErr)
	}
}

func TestBuildImageRemovesTheContextOfASuccessfulBuild(t *testing.T) {
	fake := enginetest.New()
	store := newTestStore(t)
	service := newTestService(t, fake, store)

	staging := t.TempDir()
	if _, err := service.BuildImage(context.Background(), BuildRequest{
		Manifest:   imageManifest(t),
		StagingDir: staging,
	}); err != nil {
		t.Fatalf("BuildImage: %v", err)
	}
	// A caller that named the directory owns it: wlvision does not remove it.
	if _, err := os.Stat(filepath.Join(staging, "Containerfile")); err != nil {
		t.Errorf("the caller's build context was removed: %v", err)
	}
}

func TestBuildImageRefusesWhatItCannotBuild(t *testing.T) {
	fake := enginetest.New()
	store := newTestStore(t)
	service := newTestService(t, fake, store)

	for name, request := range map[string]BuildRequest{
		"no manifest":  {},
		"missing file": {Manifest: filepath.Join(t.TempDir(), "absent.toml")},
		"bad tag":      {Manifest: imageManifest(t), Tag: "--privileged"},
		"unknown key":  {Manifest: writeBrokenManifest(t)},
	} {
		if _, err := service.BuildImage(context.Background(), request); err == nil {
			t.Errorf("%s: was accepted", name)
		}
	}
	if calls := fake.CountCalls("build "); calls != 0 {
		t.Errorf("the engine was asked to build %d times, want none", calls)
	}
}

// writeBrokenManifest writes a manifest with a key the schema does not declare.
func writeBrokenManifest(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "wlvision.toml")
	body := `schema = "wlvision-manifest/v1"

[base]
image = "example.invalid/runtime@sha256:` + strings.Repeat("ab", 32) + `"
surprise = true

[application]
command = ["/app/example"]
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write the manifest: %v", err)
	}
	return path
}
