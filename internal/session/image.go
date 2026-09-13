package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/bnema/wlvision/internal/manifest"
	"github.com/bnema/wlvision/internal/result"
)

// BuildRequest asks for one image built from a manifest.
type BuildRequest struct {
	// Manifest is the path of the manifest to build.
	Manifest string
	// Tag overrides the tag the manifest's own digest produces. Empty keeps
	// the deterministic tag.
	Tag string
	// StagingDir is where the build context is assembled. Empty asks for a
	// temporary directory, which is removed again on success and kept on
	// failure so the input that failed can be inspected.
	StagingDir string
}

// BuildImage stages a manifest and builds the image it describes.
//
// The engine is the only thing that reaches a container engine, and the build
// context is generated rather than mounted: a manifest names what goes in, and
// nothing else can.
func (s *Service) BuildImage(ctx context.Context, request BuildRequest) (manifest.Result, error) {
	if strings.TrimSpace(request.Manifest) == "" {
		return manifest.Result{}, result.NewFailure(result.CodeUsageError, "image.build",
			"a manifest path is required")
	}

	loaded, err := manifest.Load(request.Manifest)
	if err != nil {
		return manifest.Result{}, err
	}
	plan, err := manifest.Plan(loaded)
	if err != nil {
		return manifest.Result{}, err
	}
	if tag := strings.TrimSpace(request.Tag); tag != "" {
		if err := manifest.ValidateTag(tag); err != nil {
			return manifest.Result{}, err
		}
		plan.Tag = tag
	}

	staging := strings.TrimSpace(request.StagingDir)
	temporary := staging == ""
	if temporary {
		staging, err = os.MkdirTemp("", "wlvision-build-")
		if err != nil {
			return manifest.Result{}, result.NewFailure(result.CodeImageUnavailable, "image.build",
				"cannot create a build context: %v", err)
		}
	} else if err := os.MkdirAll(staging, 0o700); err != nil {
		return manifest.Result{}, result.NewFailure(result.CodeImageUnavailable, "image.build",
			"cannot use the build context %s: %v", staging, err)
	}

	built, err := manifest.Build(ctx, s.engine, plan, staging)
	if err != nil {
		if temporary {
			// The context is what a failed build was made of: keeping it makes
			// the failure diagnosable instead of leaving a log line to explain.
			return built, withDetail(err, "staging", staging)
		}
		return built, err
	}
	if temporary {
		_ = os.RemoveAll(staging)
	}
	return built, nil
}

// withDetail returns the failure with one detail added, keeping its code.
func withDetail(err error, key, value string) error {
	var failure *result.Failure
	if !errors.As(err, &failure) {
		return fmt.Errorf("%w (the build context is at %s)", err, value)
	}
	described := *failure
	details := make(map[string]string, len(described.Details)+1)
	for name, existing := range described.Details {
		details[name] = existing
	}
	details[key] = value
	described.Details = details
	return &described
}
