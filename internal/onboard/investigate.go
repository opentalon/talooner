package onboard

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// manifestLanguages maps a manifest file at repo root to the language(s) its
// presence implies. Not exhaustive — enough to ground the generation prompt,
// not a build-system detector.
var manifestLanguages = map[string]string{
	"go.mod":           "Go",
	"package.json":     "JavaScript/TypeScript (Node)",
	"Gemfile":          "Ruby",
	"requirements.txt": "Python",
	"pyproject.toml":   "Python",
	"Cargo.toml":       "Rust",
	"mix.exs":          "Elixir",
	"pom.xml":          "Java (Maven)",
	"build.gradle":     "Java/Kotlin (Gradle)",
}

// readmeExcerptLines caps how much of the README goes into the prompt — a
// grounding excerpt, not the whole file.
const readmeExcerptLines = 40

// maxTopLevelDirs caps how many top-level directory names are listed, so a
// monorepo with hundreds of packages doesn't blow out the prompt.
const maxTopLevelDirs = 40

// dirsToSkip are never descended into or listed: not source, or too large to
// usefully summarize.
var dirsToSkip = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "bin": true,
	"dist": true, "build": true, ".venv": true,
}

// Investigate walks root (a repo checkout) and builds a compact text summary
// for generate_ruleset's prompt: which manifests are present (and so which
// languages are in play), the top-level directory layout, existing GitHub
// Actions workflow names, whether CODEOWNERS exists, and a short README
// excerpt. It does no AST/deep parsing — just enough for the model to ground
// itself, not a full repo profiler.
func Investigate(root string) (string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", root, err)
	}

	var languages []string
	var topDirs []string
	hasCodeowners := false
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			if dirsToSkip[name] {
				continue
			}
			if len(topDirs) < maxTopLevelDirs {
				topDirs = append(topDirs, name)
			}
			continue
		}
		if lang, ok := manifestLanguages[name]; ok {
			languages = append(languages, lang)
		}
		if name == "CODEOWNERS" {
			hasCodeowners = true
		}
	}
	sort.Strings(languages)
	sort.Strings(topDirs)

	workflows, err := workflowNames(root)
	if err != nil {
		return "", err
	}

	readme, err := readmeExcerpt(root)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Languages (by manifest present): %s\n", joinOrNone(languages))
	fmt.Fprintf(&b, "Top-level directories: %s\n", joinOrNone(topDirs))
	fmt.Fprintf(&b, "Existing GitHub Actions workflows: %s\n", joinOrNone(workflows))
	fmt.Fprintf(&b, "CODEOWNERS present: %v\n", hasCodeowners)
	if readme != "" {
		fmt.Fprintf(&b, "\nREADME excerpt:\n%s\n", readme)
	}
	return b.String(), nil
}

func workflowNames(root string) ([]string, error) {
	dir := filepath.Join(root, ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", dir, err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), ".yml") || strings.HasSuffix(e.Name(), ".yaml") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// readmeExcerpt reads the first readmeExcerptLines lines of the repo's
// top-level README (case-insensitive, .md preferred), or "" if there isn't
// one.
func readmeExcerpt(root string) (string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", root, err)
	}
	var readme string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		lower := strings.ToLower(e.Name())
		if lower == "readme.md" {
			readme = e.Name()
			break
		}
		if strings.HasPrefix(lower, "readme") && readme == "" {
			readme = e.Name()
		}
	}
	if readme == "" {
		return "", nil
	}

	content, err := os.ReadFile(filepath.Join(root, readme))
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", readme, err)
	}
	lines := strings.Split(string(content), "\n")
	if len(lines) > readmeExcerptLines {
		lines = lines[:readmeExcerptLines]
	}
	return strings.TrimSpace(strings.Join(lines, "\n")), nil
}

func joinOrNone(items []string) string {
	if len(items) == 0 {
		return "none found"
	}
	return strings.Join(items, ", ")
}
