package onboard

import (
	"bytes"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
)

//go:embed templates/talooner.yml
var Workflow []byte

//go:embed templates/rules.tln
var Ruleset []byte

//go:embed templates/rules.tln.test
var RulesetTest []byte

const (
	WorkflowPath    = ".github/workflows/talooner.yml"
	RulesetPath     = ".github/talooner/rules.tln"
	RulesetTestPath = ".github/talooner/rules.tln.test"
)

type Outcome int

const (
	Created Outcome = iota
	Unchanged
	Conflict
)

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
