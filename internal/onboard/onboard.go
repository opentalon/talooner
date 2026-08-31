// Package onboard is the guts of `talooner init` and `talooner onboard`: the
// starter files a repo gets wired up with, and writing them without
// clobbering something a maintainer already edited.
package onboard

import (
	"bytes"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
)

// Workflow is not yet written by anything — `init` only sets secrets, and
// `onboard` only scaffolds the ruleset. It's here for the workflow file to
// land in once `onboard` grows the step that writes it.
//
//go:embed templates/talooner.yml
var Workflow []byte

// Ruleset and RulesetTest are no longer written by `init` — they are
// `talooner onboard`'s fallback pair, substituted in when the cluster's
// generate_ruleset reports source == "fallback" (no host, quota exhausted,
// or an unparseable/invalid model reply).
//
//go:embed templates/rules.tln
var Ruleset []byte

//go:embed templates/rules.tln.test
var RulesetTest []byte

// WorkflowPath is where the workflow file will be written once something
// writes it. RulesetPath and RulesetTestPath are where `onboard` writes the
// generated (or fallback) ruleset — the same paths internal/run reads at
// runtime (RulesetPath there is the canonical source; this package doesn't
// import internal/run to avoid a dependency edge from onboarding back into
// the run loop).
const (
	WorkflowPath    = ".github/workflows/talooner.yml"
	RulesetPath     = ".github/talooner/rules.tln"
	RulesetTestPath = ".github/talooner/rules.tln.test"
)

// Outcome is what happened to one file `init` tried to write.
type Outcome int

const (
	// Created means the file didn't exist and now does.
	Created Outcome = iota
	// Unchanged means the file already existed with identical content — not
	// an error, just nothing to do.
	Unchanged
	// Conflict means the file exists with different content and force was
	// false. Diff carries a naive line diff for the caller to show.
	Conflict
)

// WriteFile writes content to path unless a different file is already there,
// in which case it refuses and returns Conflict (plus a diff) rather than
// clobbering something a maintainer edited by hand. force skips that check.
func WriteFile(path string, content []byte, force bool) (Outcome, string, error) {
	existing, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return 0, "", fmt.Errorf("reading %s: %w", path, err)
		}
		if err := create(path, content); err != nil {
			return 0, "", err
		}
		return Created, "", nil
	}

	if bytes.Equal(existing, content) {
		return Unchanged, "", nil
	}

	if !force {
		return Conflict, diff(path, string(existing), string(content)), nil
	}

	if err := create(path, content); err != nil {
		return 0, "", err
	}
	return Created, "", nil
}

func create(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, content, 0644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// diff is a naive line-oriented comparison, not a minimal edit script — good
// enough to show a maintainer which lines changed on two short config files
// without pulling in a diff library. It walks both files by index; an
// inserted or deleted line partway through reads as every line after it
// changing, which a real LCS diff wouldn't do.
func diff(path, existing, next string) string {
	oldLines := splitLines(existing)
	newLines := splitLines(next)
	var b bytes.Buffer
	fmt.Fprintf(&b, "--- %s (on disk)\n+++ %s (starter template)\n", path, path)
	max := len(oldLines)
	if len(newLines) > max {
		max = len(newLines)
	}
	for i := 0; i < max; i++ {
		var o, n string
		haveOld := i < len(oldLines)
		haveNew := i < len(newLines)
		if haveOld {
			o = oldLines[i]
		}
		if haveNew {
			n = newLines[i]
		}
		switch {
		case haveOld && haveNew && o == n:
			continue
		case haveOld && haveNew:
			fmt.Fprintf(&b, "-%s\n+%s\n", o, n)
		case haveOld:
			fmt.Fprintf(&b, "-%s\n", o)
		case haveNew:
			fmt.Fprintf(&b, "+%s\n", n)
		}
	}
	return b.String()
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	var lines []string
	start := 0
	for i, r := range s {
		if r == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}
