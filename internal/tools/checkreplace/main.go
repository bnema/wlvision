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

	found := make(map[string][]finding)
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
			found[key] = append(found[key], finding{dir: dir, target: entry.New.String()})
		}
	}

	for _, complaint := range problems(found, want) {
		fmt.Fprintf(os.Stderr, "checkreplace: %s\n", complaint)
		failed = true
	}

	if failed {
		os.Exit(1)
	}
}

// finding is one replacement found in one module directory.
type finding struct {
	dir    string
	target string
}

// problems compares the replacements found in the module graphs against what
// the caller expects.
//
// Without expectations every replacement is a problem: a filesystem
// replacement must never reach a released module file. With expectations both
// halves of each pair matter, because a replacement that points at the wrong
// checkout is exactly what this check exists to catch.
func problems(found map[string][]finding, want expectations) []string {
	var complaints []string

	if len(want) == 0 {
		for _, key := range sortedKeys(found) {
			for _, item := range found[key] {
				complaints = append(complaints, fmt.Sprintf("found replace %s => %s (%s)", key, item.target, item.dir))
			}
		}
		return complaints
	}

	listed := make(map[string]bool, len(want))
	for _, item := range want {
		listed[item.old] = true

		targets := found[item.old]
		if len(targets) == 0 {
			complaints = append(complaints, fmt.Sprintf("expected replace %s is missing", item))
			continue
		}
		matched := false
		for _, target := range targets {
			if target.target == item.new {
				matched = true
			}
		}
		if !matched {
			complaints = append(complaints, fmt.Sprintf("expected replace %s is missing; found %s", item, describe(targets)))
		}
	}

	for _, key := range sortedKeys(found) {
		if !listed[key] {
			for _, item := range found[key] {
				complaints = append(complaints, fmt.Sprintf("unexpected replace %s => %s (%s) is present", key, item.target, item.dir))
			}
		}
	}
	return complaints
}

// describe renders the targets found for one module path.
func describe(found []finding) string {
	parts := make([]string, 0, len(found))
	for _, item := range found {
		parts = append(parts, item.target+" ("+item.dir+")")
	}
	return strings.Join(parts, ", ")
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
