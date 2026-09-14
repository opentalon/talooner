package facts

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/opentalon/talooner/internal/github"
)

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

type diffFile struct {
	name string
	body string
}

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

var manifestNames = map[string]bool{
	"go.mod":           true,
	"package.json":     true,
	"Gemfile":          true,
	"requirements.txt": true,
	"Cargo.toml":       true,
}

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

func isManifest(name string) bool {
	if isLockfile(name) {
		return false
	}
	return manifestNames[path.Base(name)]
}

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

func goModName(content string) []string {
	if m := goModBlockDep.FindStringSubmatch(content); m != nil {
		return m
	}
	return goModRequire.FindStringSubmatch(content)
}

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

var pkgDepBlocks = map[string]bool{
	"dependencies":         true,
	"devDependencies":      true,
	"optionalDependencies": true,
	"peerDependencies":     true,
}

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
