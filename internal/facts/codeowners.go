package facts

import (
	"regexp"
	"sort"
	"strings"
)

// codeownerRule is one line of a CODEOWNERS file: a path pattern plus the owners
// GitHub would request for paths it matches.
type codeownerRule struct {
	pattern string
	owners  []string
}

// parseCodeowners turns raw CODEOWNERS content into the rules that apply to a
// repo, preserving file order. GitHub resolves an owner by the last rule that
// matches a given path ("most specific / last wins"), so order is kept, not
// deduplicated. Comments (a leading "#") and blank lines are dropped, as are
// ownerless patterns, which would otherwise shadow a real rule beneath them.
func parseCodeowners(data []byte) []codeownerRule {
	var rules []codeownerRule
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		rules = append(rules, codeownerRule{pattern: fields[0], owners: fields[1:]})
	}
	return rules
}

// resolveOwners finds who owns the changed paths. The primary owner is the first
// owner of the first touched path CODEOWNERS assigns; owners is the sorted,
// de-duplicated union of every owner across every touched path. Both come back
// empty when CODEOWNERS assigns nothing to any touched path — which is the
// truthful answer here, not "nobody owns it": the modules.yaml and git-history
// fallbacks (facts.md, "user.owner") are later tickets, so a path with no
// CODEOWNERS rule is left unowned rather than guessed at pr.author.
func resolveOwners(rules []codeownerRule, paths []string) (primary string, owners []string) {
	seen := make(map[string]bool)
	first := true
	for _, path := range paths {
		// Last matching rule wins, so scan from the end and take the first hit.
		for i := len(rules) - 1; i >= 0; i-- {
			if !codeownersMatch(rules[i].pattern, path) {
				continue
			}
			for _, o := range rules[i].owners {
				if seen[o] {
					continue
				}
				seen[o] = true
				owners = append(owners, o)
				if first {
					primary = o
					first = false
				}
			}
			break
		}
	}
	if len(owners) == 0 {
		return "", nil
	}
	sort.Strings(owners)
	return primary, owners
}

// codeownersMatch reports whether a CODEOWNERS pattern matches a repo-relative
// path. It follows GitHub's documented semantics: "*" matches any run of
// characters including a "/", "?" matches one character, "**" the same as "*"
// here, a leading "/" anchors at the repo root (already the case — the file lives
// at the root), and a trailing "/" matches a directory and everything under it.
//
// The one shape "*" alone does not cover is "match at any depth including the
// root", so a leading "**/" is treated as an optional leading path prefix.
func codeownersMatch(pattern, path string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return false
	}
	dirOnly := strings.HasSuffix(pattern, "/")
	pattern = strings.TrimSuffix(pattern, "/")
	pattern = strings.TrimPrefix(pattern, "/")

	prefixAny := false
	if strings.HasPrefix(pattern, "**/") {
		prefixAny = true
		pattern = strings.TrimPrefix(pattern, "**/")
	}

	var b strings.Builder
	b.WriteString("^")
	if prefixAny {
		b.WriteString("(?:.*/)?")
	}
	segs := strings.Split(pattern, "/")
	for i, seg := range segs {
		if i > 0 {
			b.WriteString("/")
		}
		b.WriteString(convertGlob(seg))
	}
	if dirOnly {
		b.WriteString("(?:/.*)?")
	}
	b.WriteString("$")

	re, err := regexp.Compile(b.String())
	if err != nil {
		// An uncompilable pattern matches nothing rather than crashing a run.
		return false
	}
	return re.MatchString(path)
}

// convertGlob turns one pattern segment into an anchored regexp fragment.
// "*" and "**" become ".*" (any characters, spanning a "/"), "?" becomes "."
// (one character), and everything else is matched literally.
func convertGlob(seg string) string {
	var b strings.Builder
	for _, r := range seg {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	return b.String()
}
