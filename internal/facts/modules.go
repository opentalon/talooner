package facts

import (
	"sort"
	"strings"

	"github.com/opentalon/talooner/internal/config"
	"github.com/opentalon/talooner/internal/github"
)

// moduleFacts asserts the module.* namespace (facts.md, "module.*"). They are
// tenant-supplied lookup tables, so the only inputs are the changed files' line
// counts and the configured modules — no API call of their own.
//
// A PR is evaluated once, not per module, so module.* binds to the primary
// touched module: the one whose files carry the most changed lines. Ties are
// broken by path order so the same PR resolves identically on a re-run. Every
// configured module the PR touches is counted, even when it is not the primary.
func moduleFacts(s Set, files []github.FileStat, modules []config.Module) {
	// lines[path] is the sum of every changed line under that module's prefix.
	lines := make(map[string]int, len(modules))
	touched := 0
	for _, m := range modules {
		n := 0
		for _, f := range files {
			if moduleOwns(m.Path, f.Path) {
				n += f.Additions + f.Deletions
			}
		}
		if n > 0 {
			lines[m.Path] = n
			touched++
		}
	}

	// module.touched_count is always asserted: a PR that touches no configured
	// module reads 0, which is the honest answer, not an unset fact (facts.md,
	// "module.touched_count"). The rest stay unset, because an unset module.*
	// simply makes a rule gated on it not fire — the safe direction.
	s.Int("module.touched_count", touched)
	if touched == 0 {
		return
	}

	// primary is the module with the most changed lines; on a tie the one whose
	// path sorts first wins, which is the deterministic tie-break (issue #13).
	primary := ""
	best := -1
	for _, m := range sortedByPath(modules) {
		if lines[m.Path] > best {
			best = lines[m.Path]
			primary = m.Path
		}
	}

	var docURLs []string
	seen := make(map[string]bool)
	for _, m := range modules {
		if lines[m.Path] == 0 || m.DocumentationURL == "" || seen[m.DocumentationURL] {
			continue
		}
		seen[m.DocumentationURL] = true
		docURLs = append(docURLs, m.DocumentationURL)
	}
	sort.Strings(docURLs)

	// module.documentation_url is the primary module's doc URL; module.owner its
	// owner. Both are left unset when the primary module declares neither, so a
	// require/comment rule quoting them simply does not fire rather than quoting
	// an empty string. module.documentation_urls is the de-duplicated, sorted set
	// across every touched module, so a rule can reference them all.
	if p := moduleByPath(modules, primary); p != nil {
		if p.DocumentationURL != "" {
			s.String("module.documentation_url", p.DocumentationURL)
		}
		if p.Owner != "" {
			s.String("module.owner", p.Owner)
		}
	}
	if len(docURLs) > 0 {
		s.Strings("module.documentation_urls", docURLs)
	}
}

// resolveModuleOwners finds who owns the changed paths via modules.yaml, the
// second tier of the user.owner resolution order (facts.md, "user.owner"),
// consulted only when CODEOWNERS names nobody for any touched path. The primary
// owner is the first declared owner of the first touched path a module covers;
// owners is the sorted, de-duplicated union across every touched path. A path
// under no configured module, or under one with no owner declared, contributes
// nothing. Nested modules resolve to the most specific (longest) matching path —
// the same "most specific wins" rule CODEOWNERS itself uses.
func resolveModuleOwners(modules []config.Module, paths []string) (primary string, owners []string) {
	seen := make(map[string]bool)
	first := true
	for _, path := range paths {
		var best *config.Module
		for i := range modules {
			m := &modules[i]
			if m.Owner == "" || !moduleOwns(m.Path, path) {
				continue
			}
			if best == nil || len(m.Path) > len(best.Path) {
				best = m
			}
		}
		if best == nil {
			continue
		}
		if !seen[best.Owner] {
			seen[best.Owner] = true
			owners = append(owners, best.Owner)
			if first {
				primary = best.Owner
				first = false
			}
		}
	}
	if len(owners) == 0 {
		return "", nil
	}
	sort.Strings(owners)
	return primary, owners
}

// moduleOwns reports whether path is under a module's configured prefix. Modules
// are directory prefixes, so "internal/auth/" matches "internal/auth/x.go" and
// "internal/auth" matches a file of that exact name; it does not match
// "internal/authority/x.go".
func moduleOwns(prefix, path string) bool {
	prefix = strings.TrimSuffix(prefix, "/")
	if prefix == "" {
		return false
	}
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

// sortedByPath returns the modules ordered by their path for deterministic
// primary-module selection.
func sortedByPath(modules []config.Module) []config.Module {
	out := make([]config.Module, len(modules))
	copy(out, modules)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// moduleByPath finds the module configured at path, or nil when the PR touched no
// module with that key (which should not happen here, but keeps the lookup total).
func moduleByPath(modules []config.Module, path string) *config.Module {
	for i := range modules {
		if modules[i].Path == path {
			return &modules[i]
		}
	}
	return nil
}
