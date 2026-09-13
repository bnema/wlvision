// Image commands: build a session image from a manifest.
package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/bnema/wlvision/internal/result"
	"github.com/bnema/wlvision/internal/session"
)

// imageResult is what one build reports. A failed build keeps its build
// context and names it in the failure's details, so the input to the failure
// stays inspectable.
type imageResult struct {
	Manifest string `json:"manifest"`
	Tag      string `json:"tag"`
	ImageID  string `json:"image_id"`
	Output   string `json:"output,omitempty"`
}

func (c *cli) imageCommand(args []string) int {
	if len(args) == 0 {
		return c.usageError("image", "", errors.New("an image subcommand is required"))
	}
	switch args[0] {
	case "build":
		return c.imageBuild(args[1:])
	default:
		return c.usageError("image", "", fmt.Errorf("unknown image subcommand %q", args[0]))
	}
}

func (c *cli) imageBuild(args []string) int {
	const operation = "image.build"
	var (
		manifest   string
		tag        string
		stagingDir string
	)
	flags := c.flagSet("wlvision image build")
	flags.StringVar(&manifest, "manifest", "", "manifest that describes the image")
	flags.StringVar(&tag, "tag", "", "image tag; the manifest's own digest names it by default")
	flags.StringVar(&stagingDir, "staging", "", "directory to assemble the build context in")
	if err := flags.Parse(args); err != nil {
		return c.usageError(operation, "", err)
	}
	if flags.NArg() > 0 {
		return c.usageError(operation, "", errors.New("unexpected argument "+flags.Arg(0)))
	}
	if strings.TrimSpace(manifest) == "" {
		return c.usageError(operation, "", errors.New("--manifest is required"))
	}

	service, err := c.newService()
	if err != nil {
		return c.fail(operation, "", err, result.CodeEngineUnavailable)
	}
	built, err := service.BuildImage(c.ctx, session.BuildRequest{
		Manifest:   manifest,
		Tag:        tag,
		StagingDir: stagingDir,
	})
	if err != nil {
		return c.fail(operation, "", err, result.CodeImageUnavailable)
	}
	return c.success(operation, "", 0, imageResult{
		Manifest: manifest,
		Tag:      built.Tag,
		ImageID:  built.ImageID,
		Output:   built.Output,
	})
}
