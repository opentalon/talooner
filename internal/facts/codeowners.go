package facts

import (
	"regexp"
	"sort"
	"strings"
)

type codeownerRule struct {
	pattern string
	owners  []string
}

// GitLab CODEOWNERS sections: "[Name]", "^[Name]" (optional section), and
// "[Name][N]" (N required approvals) headers, each optionally followed by
// default owners for entries in that section that list none of their own.
// GitHub's CODEOWNERS format has no section syntax, so a plain file never
// matches this and falls through to the entry branch unchanged.
var sectionHeaderPattern = regexp.MustCompile(`^\^?\[[^\]]*\](?:\[\d+\])?(?:\s+(.*))?$`)

func parseCodeowners(data []byte) []codeownerRule {
	var rules []codeownerRule
	var sectionDefaults []string
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if m := sectionHeaderPattern.FindStringSubmatch(line); m != nil {
			sectionDefaults = strings.Fields(m[1])
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		owners := fields[1:]
		if len(owners) == 0 {
			owners = sectionDefaults
		}
		if len(owners) == 0 {
			continue
		}
		rules = append(rules, codeownerRule{pattern: fields[0], owners: owners})
	}
	return rules
}

func resolveOwners(rules []codeownerRule, paths []string) (primary string, owners []string) {
	seen := make(map[string]bool)
	first := true
	for _, path := range paths {
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
		return false
	}
	return re.MatchString(path)
}

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
