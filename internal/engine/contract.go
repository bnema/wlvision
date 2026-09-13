package engine

import "io"

// BuildSpec describes one image build. The context is a generated directory,
// never a host path the caller chose: wlvision stages exactly the inputs a
// manifest declared and hands the engine that directory alone.
//
// A build is the only operation in this package that may reach the network,
// and only when the spec asks for it. A session never does: CreateSpec has no
// network field at all.
type BuildSpec struct {
	// Context is the absolute directory holding the Containerfile and the
	// staged inputs. A relative or empty context is refused.
	Context string
	// Containerfile names the Containerfile inside Context. It is relative
	// to Context and is refused when empty or when it is not a name.
	Containerfile string
	// Tag is the image tag to create.
	Tag string
	// Network allows network access during the build. It is never true for a
	// session.
	Network bool
	// Stdout and Stderr receive the build's output. Either may be nil.
	Stdout io.Writer
	Stderr io.Writer
}

// BuildResult reports the image a build created.
type BuildResult struct {
	// ImageID is the identifier the engine assigned to the built image, for
	// example "sha256:...". A session records it so it names the bytes it
	// actually ran.
	ImageID string
}
