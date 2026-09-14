package facts

import (
	"path"
	"regexp"
	"strings"
)

func testAssertionDiff(files []string, slices map[string]string) string {
	var parts []string
	for _, f := range files {
		if !isTestFile(f) {
			continue
		}
		if hunks := assertionHunks(slices[f]); hunks != "" {
			parts = append(parts, hunks)
		}
	}
	return strings.Join(parts, "\n")
}

func isTestFile(filePath string) bool {
	base := path.Base(filePath)
	switch {
	case strings.HasSuffix(base, "_test.go"):
		return true
	case strings.HasSuffix(base, "_test.rb"), strings.HasSuffix(base, "_spec.rb"):
		return true
	case strings.HasSuffix(base, "_test.py"),
		strings.HasPrefix(base, "test_") && strings.HasSuffix(base, ".py"):
		return true
	case strings.HasSuffix(base, ".test.js"), strings.HasSuffix(base, ".test.jsx"),
		strings.HasSuffix(base, ".test.ts"), strings.HasSuffix(base, ".test.tsx"),
		strings.HasSuffix(base, ".spec.js"), strings.HasSuffix(base, ".spec.jsx"),
		strings.HasSuffix(base, ".spec.ts"), strings.HasSuffix(base, ".spec.tsx"):
		return true
	}
	return false
}

var assertionPattern = regexp.MustCompile(`(?i)\bassert\w*|\bexpect\w*|\brequire\.(equal|noerror|error|true|false|nil|len|contains)\b|\bshould\w*|\bwanterr\b|\braises\w*|\bto(equal|be)\b`)

func assertionHunks(body string) string {
	var kept []string
	for _, h := range splitHunks(body) {
		if hunkTouchesAssertion(h) {
			kept = append(kept, h)
		}
	}
	return strings.Join(kept, "\n")
}

func splitHunks(body string) []string {
	var hunks []string
	var cur strings.Builder
	inHunk := false
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "@@") {
			if inHunk {
				hunks = append(hunks, cur.String())
				cur.Reset()
			}
			inHunk = true
		}
		if !inHunk {
			continue
		}
		cur.WriteString(line)
		cur.WriteString("\n")
	}
	if inHunk {
		hunks = append(hunks, cur.String())
	}
	return hunks
}

func hunkTouchesAssertion(hunk string) bool {
	for _, line := range strings.Split(hunk, "\n") {
		if len(line) == 0 {
			continue
		}
		if line[0] != '+' && line[0] != '-' {
			continue
		}
		if assertionPattern.MatchString(line) {
			return true
		}
	}
	return false
}
