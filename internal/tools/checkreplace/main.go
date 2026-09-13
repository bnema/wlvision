// Command checkreplace reports whether any of the given Go module
// directories contains a replace directive.
//
// Usage:
//
//	go run ./internal/tools/checkreplace <dir>...
//
// For each directory it runs `go mod edit -json` and inspects the Replace
// entries. Local development replaces point at sibling checkouts on the
// filesystem and must never be present in release-resolved module files, so
// any entry makes the command fail with a message naming the directory, the
// replaced module path and the replacement target. When no directory has a
// replace directive the command exits 0 without printing anything.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
)

// moduleFile mirrors the subset of `go mod edit -json` output that this
// command needs.
type moduleFile struct {
	Replace []replaceEntry
}

type replaceEntry struct {
	Old moduleRef
	New moduleRef
}

type moduleRef struct {
	Path    string
	Version string
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: checkreplace <dir>...")
		os.Exit(2)
	}

	failed := false
	for _, dir := range os.Args[1:] {
		entries, err := replaces(dir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "checkreplace: %s: %v\n", dir, err)
			failed = true
			continue
		}
		for _, entry := range entries {
			fmt.Fprintf(os.Stderr, "checkreplace: %s: found replace %s => %s\n",
				dir, entry.Old, entry.New)
			failed = true
		}
	}
	if failed {
		os.Exit(1)
	}
}

// replaces returns the replace entries declared in dir/go.mod.
func replaces(dir string) ([]replaceEntry, error) {
	cmd := exec.Command("go", "mod", "edit", "-json")
	cmd.Dir = dir
	cmd.Stderr = os.Stderr

	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go mod edit -json: %w", err)
	}

	var mod moduleFile
	if err := json.Unmarshal(out, &mod); err != nil {
		return nil, fmt.Errorf("parsing go mod edit -json output: %w", err)
	}
	return mod.Replace, nil
}

// String renders a module reference as path or "path version".
func (ref moduleRef) String() string {
	if ref.Version != "" {
		return ref.Path + " " + ref.Version
	}
	return ref.Path
}
