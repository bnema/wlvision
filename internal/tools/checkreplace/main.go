// Command checkreplace inspects the module graph of one or more Go module
// directories and verifies which filesystem replacements they contain.
//
// Usage:
//
//	go run ./internal/tools/checkreplace <dir>...
//	go run ./internal/tools/checkreplace -expect OLD=NEW [-expect OLD=NEW]... <dir>...
//
// Without -expect it is the release guard: local development replaces point at
// sibling checkouts on the filesystem and must never reach a release-resolved
// module file, so any replace entry makes the command fail with a message
// naming the directory, the replaced module path and the replacement target.
//
// With one or more -expect flags it is the local development check: every
// listed replacement must be present, and no replacement outside the list may
// exist. Since Go replacements are not transitive, pass every module
// directory whose graph matters.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
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

// String renders a module reference as path or "path version".
func (ref moduleRef) String() string {
	if ref.Version != "" {
		return ref.Path + " " + ref.Version
	}
	return ref.Path
}

// expected is one OLD=NEW pair from the command line.
type expected struct {
	old string
	new string
}

func (e expected) String() string { return e.old + " => " + e.new }

// expectations collects the repeated -expect flag values.
type expectations []expected

func (e *expectations) String() string {
	parts := make([]string, 0, len(*e))
	for _, item := range *e {
		parts = append(parts, item.String())
	}
	return strings.Join(parts, ", ")
}

func (e *expectations) Set(value string) error {
	old, newPath, ok := strings.Cut(value, "=")
	if !ok || old == "" || newPath == "" {
		return fmt.Errorf("expectation %q must be OLD=NEW", value)
	}
	*e = append(*e, expected{old: old, new: newPath})
	return nil
}

func main() {
	var want expectations
	flag.Var(&want, "expect", "required replacement as OLD=NEW (repeatable)")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: checkreplace [-expect OLD=NEW]... <dir>...")
		flag.PrintDefaults()
	}
	flag.Parse()

	dirs := flag.Args()
	if len(dirs) == 0 {
		fmt.Fprintln(os.Stderr, "usage: checkreplace [-expect OLD=NEW]... <dir>...")
		os.Exit(2)
	}

	found := make(map[string]string) // OLD => "dir: NEW"
	failed := false

	for _, dir := range dirs {
		entries, err := replaces(dir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "checkreplace: %s: %v\n", dir, err)
			failed = true
			continue
		}
		for _, entry := range entries {
			key := entry.Old.String()
			if _, seen := found[key]; !seen {
				found[key] = dir + ": " + entry.New.String()
			}
		}
	}

	if len(want) == 0 {
		for _, key := range sortedKeys(found) {
			owner, target, _ := strings.Cut(found[key], ": ")
			fmt.Fprintf(os.Stderr, "checkreplace: %s: found replace %s => %s\n", owner, key, target)
			failed = true
		}
		if failed {
			os.Exit(1)
		}
		return
	}

	listed := make(map[string]bool, len(want))
	for _, item := range want {
		listed[item.old] = true
		if _, ok := found[item.old]; !ok {
			fmt.Fprintf(os.Stderr, "checkreplace: expected replace %s is missing\n", item)
			failed = true
		}
	}
	for _, key := range sortedKeys(found) {
		if !listed[key] {
			fmt.Fprintf(os.Stderr, "checkreplace: unexpected replace %s => %s is present\n", key, found[key])
			failed = true
		}
	}

	if failed {
		os.Exit(1)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
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
