package facts

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/opentalon/talooner/internal/github"
)

// countDependencyChanges counts new and upgraded dependencies across the PR's
// recognised manifest files, parsed from the concatenated unified diff
// (facts.md, "pr.new_dependencies", "pr.upgraded_dependencies"). Lockfiles are
// ignored: churn in package-lock.json, Cargo.lock, go.sum and friends is a
// consequence of a manifest change, not a change itself, so counting it would
// double-count. A dependency name both added and removed in the same file is a
// version bump — counted as upgraded, not new. A name only removed, with no
// matching add, is neither: it's a removal, and no base rule reads that from
// either count (issue #11).
//
// stats is the PR's changed-file list (C2 already fetched, no extra API call
// either). It is what catches the case the diff alone cannot: a manifest with
// real additions/deletions that contributes nothing to diff — GitHub's Files
// API returns a null patch for binary or oversized files, and Diff (C2) drops
// those files entirely rather than representing them. Without stats, that
// manifest would silently read as untouched and count zero — a confident
// "no security review needed" the bot never actually checked. A manifest that
// shows real changes in stats but never appears in the diff is refused as an
// error instead (issue #11): zero here would be a guess, not an answer.
//
// The diff is the one pr.diff already fetched, so this adds no API call. It reads
// only added/removed lines; context lines are neither and are skipped.
func countDependencyChanges(diff string, stats []github.FileStat) (newDeps, upgraded int, err error) {
	present := map[string]bool{}
	for _, f := range splitDiffFiles(diff) {
		if !isManifest(f.name) {
			continue
		}
		present[f.name] = true
		added, removed := parseManifestDeps(f.name, f.body)
		for name := range added {
			if removed[name] {
				upgraded++
			} else {
				newDeps++
			}
		}
	}
	for _, stat := range stats {
		if !isManifest(stat.Path) || stat.Additions+stat.Deletions == 0 {
			continue
		}
		if !present[stat.Path] {
			return 0, 0, fmt.Errorf("manifest %s changed (+%d/-%d) but has no readable diff, refusing to count dependencies as zero", stat.Path, stat.Additions, stat.Deletions)
		}
	}
	return newDeps, upgraded, nil
}

// diffFile is one file's slice of a concatenated diff: its path and the raw
// patch body that follows the `diff --git` header.
type diffFile struct {
	name string
	body string
}

// splitDiffFiles breaks a concatenated unified diff into per-file chunks. The
// Files API joins each file's patch with a newline (github.Diff), so every file
// begins a new `diff --git a/... b/...` line.
func splitDiffFiles(diff string) []diffFile {
	var files []diffFile
	var cur *diffFile
	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			if cur != nil {
				files = append(files, *cur)
			}
			cur = &diffFile{name: diffName(line)}
			continue
		}
		if cur != nil {
			cur.body += line + "\n"
		}
	}
	if cur != nil {
		files = append(files, *cur)
	}
	return files
}

// diffName extracts the post-image path from a `diff --git` header. GitHub quotes
// paths containing spaces; the unquoted form is `a/path b/path`.
func diffName(line string) string {
	rest := strings.TrimPrefix(line, "diff --git ")
	if strings.HasPrefix(rest, `"`) {
		close := strings.Index(rest[1:], `"`)
		if close < 0 {
			return ""
		}
		rest = rest[1+close+1:]
		rest = strings.TrimSpace(rest)
		if !strings.HasPrefix(rest, `"`) {
			return ""
		}
		end := strings.Index(rest[1:], `"`)
		if end < 0 {
			return ""
		}
		return rest[1 : 1+end]
	}
	fields := strings.Fields(rest)
	if len(fields) < 2 {
		return ""
	}
	return strings.TrimPrefix(fields[1], "b/")
}

// manifestNames are the manifests pr.new_dependencies and pr.upgraded_dependencies
// know how to parse. The
// set is deliberately narrow: a format we misread counts the wrong number of
// dependencies, and no base rule depends on this fact, so a format we do not
// recognise is safer left at zero than guessed (facts.md). Extend here.
var manifestNames = map[string]bool{
	"go.mod":           true,
	"package.json":     true,
	"Gemfile":          true,
	"requirements.txt": true,
	"Cargo.toml":       true,
}

// isLockfile reports whether name is a lockfile, which pr.new_dependencies and
// pr.upgraded_dependencies must not read. Anything ending in `.lock` is treated
// as one; the rest are named explicitly because their extensions vary.
func isLockfile(name string) bool {
	if strings.HasSuffix(name, ".lock") {
		return true
	}
	switch path.Base(name) {
	case "go.sum", "package-lock.json", "yarn.lock", "pnpm-lock.yaml",
		"Gemfile.lock", "Cargo.lock", "composer.lock", "poetry.lock",
		"Pipfile.lock":
		return true
	}
	return false
}

// isManifest reports whether the file at name is a recognised, parseable
// manifest — that is, its basename is known and it is not a lockfile. Paths are
// matched by basename so a manifest in any directory (vendor aside) counts.
func isManifest(name string) bool {
	if isLockfile(name) {
		return false
	}
	return manifestNames[path.Base(name)]
}

// parseManifestDeps returns the dependency names added and removed in body,
// dispatched by the manifest's basename. Names are matched by the manifest's own
// declaration syntax; anything that is not a dependency line is ignored.
func parseManifestDeps(name, body string) (added, removed map[string]bool) {
	switch path.Base(name) {
	case "go.mod":
		return goModDeps(body)
	case "requirements.txt":
		return reqDeps(body)
	case "Gemfile":
		return gemDeps(body)
	case "Cargo.toml":
		return cargoDeps(body)
	case "package.json":
		return packageJSONDeps(body)
	}
	return map[string]bool{}, map[string]bool{}
}

// goModBlockDep matches a require entry in a `require ( ... )` block: a module
// path followed by a version, with optional `// indirect`. goModRequire matches
// the single-line `require module vX` form. The `module`, `go`, `toolchain`,
// `replace` and `exclude` directives either lack a `v`-prefixed version after
// the directive keyword or are not added lines, so they do not match.
var goModBlockDep = regexp.MustCompile(`^\s*([\w./\-+]+)\s+v[\w.\-+]+`)
var goModRequire = regexp.MustCompile(`^require\s+([\w./\-+]+)\s+v[\w.\-+]+`)

func goModDeps(body string) (added, removed map[string]bool) {
	added, removed = map[string]bool{}, map[string]bool{}
	for _, line := range strings.Split(body, "\n") {
		if len(line) == 0 {
			continue
		}
		var m []string
		switch line[0] {
		case '+':
			m = goModName(line[1:])
		case '-':
			m = goModName(line[1:])
		default:
			continue
		}
		if m != nil {
			if line[0] == '+' {
				added[m[1]] = true
			} else {
				removed[m[1]] = true
			}
		}
	}
	return added, removed
}

// goModName returns the module path of a go.mod dependency line, in either the
// block or single-line require form.
func goModName(content string) []string {
	if m := goModBlockDep.FindStringSubmatch(content); m != nil {
		return m
	}
	return goModRequire.FindStringSubmatch(content)
}

// reqDep matches a requirements.txt spec: a package name followed by a version
// operator. Comments and `-r`/`-c` includes carry no operator, so they are
// skipped.
var reqDep = regexp.MustCompile(`^\s*([\w.\-]+)\s*[<>=~!]`)

func reqDeps(body string) (added, removed map[string]bool) {
	added, removed = map[string]bool{}, map[string]bool{}
	for _, line := range strings.Split(body, "\n") {
		if len(line) == 0 {
			continue
		}
		switch line[0] {
		case '+':
			if m := reqDep.FindStringSubmatch(line[1:]); m != nil {
				added[m[1]] = true
			}
		case '-':
			if m := reqDep.FindStringSubmatch(line[1:]); m != nil {
				removed[m[1]] = true
			}
		}
	}
	return added, removed
}

// gemDep matches a Gemfile `gem` declaration.
var gemDep = regexp.MustCompile(`^\s*gem\s+['"]([\w.\-]+)['"]`)

func gemDeps(body string) (added, removed map[string]bool) {
	added, removed = map[string]bool{}, map[string]bool{}
	for _, line := range strings.Split(body, "\n") {
		if len(line) == 0 {
			continue
		}
		switch line[0] {
		case '+':
			if m := gemDep.FindStringSubmatch(line[1:]); m != nil {
				added[m[1]] = true
			}
		case '-':
			if m := gemDep.FindStringSubmatch(line[1:]); m != nil {
				removed[m[1]] = true
			}
		}
	}
	return added, removed
}

// cargoHeader matches a Cargo.toml dependency table: `[dependencies]`,
// `[dev-dependencies]` and the per-dependency `[dependencies.foo]` form.
var cargoHeader = regexp.MustCompile(`^\[(dev-)?dependencies(\.[\w\-]+)?\]`)
var cargoKV = regexp.MustCompile(`^\s*([\w\-]+)\s*=`)

func cargoDeps(body string) (added, removed map[string]bool) {
	added, removed = map[string]bool{}, map[string]bool{}
	inDeps := false
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			inDeps = cargoHeader.MatchString(trimmed)
			continue
		}
		if !inDeps {
			continue
		}
		var m []string
		switch {
		case strings.HasPrefix(line, "+"):
			m = cargoKV.FindStringSubmatch(line[1:])
		case strings.HasPrefix(line, "-"):
			m = cargoKV.FindStringSubmatch(line[1:])
		default:
			continue
		}
		if m != nil {
			name := m[1]
			if strings.HasPrefix(line, "+") {
				added[name] = true
			} else {
				removed[name] = true
			}
		}
	}
	return added, removed
}

// pkgDepBlocks are the package.json objects that hold dependency entries.
var pkgDepBlocks = map[string]bool{
	"dependencies":         true,
	"devDependencies":      true,
	"optionalDependencies": true,
	"peerDependencies":     true,
}

// pkgDepKey matches a dependency entry: a quoted key with a quoted string value
// (a version). Keys with object or boolean values are not dependencies and are
// skipped.
var pkgDepKey = regexp.MustCompile(`^\s*"([^"]+)"\s*:\s*"`)

func packageJSONDeps(body string) (added, removed map[string]bool) {
	added, removed = map[string]bool{}, map[string]bool{}
	inDeps := false
	depth := 0
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "@@") {
			continue
		}
		isAdd, isDel := strings.HasPrefix(line, "+"), strings.HasPrefix(line, "-")
		content := line
		if isAdd || isDel || strings.HasPrefix(line, " ") {
			content = line[1:]
		}
		trimmed := strings.TrimSpace(content)
		if !inDeps {
			key := strings.TrimPrefix(trimmed, `"`)
			if end := strings.Index(key, `"`); end >= 0 {
				k := key[:end]
				// A dependency block is "key": { — a string value ("key": "x")
				// is a top-level field and must not open the block.
				if len(trimmed) >= len(k)+2 && pkgDepBlocks[k] && strings.Contains(trimmed[len(k)+2:], "{") {
					inDeps = true
					depth = 1
					continue
				}
			}
			continue
		}
		for _, ch := range content {
			switch ch {
			case '{':
				depth++
			case '}':
				depth--
			}
		}
		if depth <= 0 {
			inDeps = false
			continue
		}
		if isAdd {
			if m := pkgDepKey.FindStringSubmatch(content); m != nil {
				added[m[1]] = true
			}
		} else if isDel {
			if m := pkgDepKey.FindStringSubmatch(content); m != nil {
				removed[m[1]] = true
			}
		}
	}
	return added, removed
}
