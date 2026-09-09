package facts

import (
	"path"
	"regexp"
	"strings"
)

// testAssertionDiff returns the assertion-touching hunks of a unit's test
// files, for a code_unit's unit.test_diff (facts.md, "unit.test_diff" —
// talooner#94). Unlike DiffSlice — the unit's whole diff, prod and test code
// mixed — this is scoped to just the lines an llm_review weakened-test
// detector needs: a test got its assertion loosened or removed, not its setup
// or fixtures touched.
//
// files is the unit's changed paths (already resolved by resolveUnit);
// slices is the same path → diff-hunk-body lookup architectureFacts builds
// once per PR from diffSlicesByPath. Non-test files contribute nothing. A
// test file with no assertion-matching hunk also contributes nothing — most
// test-file changes are fixture or setup churn, and shipping that as
// "test_diff" would bury the one hunk a reviewer actually needs among noise
// the detector isn't asking about.
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

// isTestFile recognises the common per-language test-file naming
// conventions. Best-effort, not exhaustive (issue #94): a naming convention
// this misses simply never contributes to unit.test_diff, the same "safer
// left out than guessed" call pr.new_dependencies makes for an unrecognised
// manifest (facts.md).
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

// assertionPattern matches an assertion call across the languages
// isTestFile recognises: xUnit-style assert*/expect* (Go, Python, JS),
// testify's require.Equal/NoError/etc (Go), RSpec's should/expect...to
// (Ruby), Jest/Chai's toEqual/toBe (JS). Best-effort per issue #94's own
// framing — a call shape this misses simply isn't flagged, same direction as
// an unrecognised test-file name.
var assertionPattern = regexp.MustCompile(`(?i)\bassert\w*|\bexpect\w*|\brequire\.(equal|noerror|error|true|false|nil|len|contains)\b|\bshould\w*|\bwanterr\b|\braises\w*|\bto(equal|be)\b`)

// assertionHunks splits body (one file's diff hunks, as diffSlicesByPath
// produces) and keeps only the hunks where an added or removed line matches
// assertionPattern — dropping hunks that only touch setup/fixture code, and
// dropping context lines within a kept hunk's surrounding is not attempted:
// the hunk is the smallest unit unified diff offers, and splitting further
// would need re-deriving line numbers no consumer here asks for.
func assertionHunks(body string) string {
	var kept []string
	for _, h := range splitHunks(body) {
		if hunkTouchesAssertion(h) {
			kept = append(kept, h)
		}
	}
	return strings.Join(kept, "\n")
}

// splitHunks breaks one file's diff body (everything after the `diff --git`
// header, `---`/`+++` lines included) into its `@@ ... @@` hunks. Lines
// before the first hunk header — the `---`/`+++` file markers — belong to no
// hunk and are dropped.
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

// hunkTouchesAssertion reports whether an added or removed line (never a
// context line, and never the `@@ ... @@` header itself) matches
// assertionPattern.
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
