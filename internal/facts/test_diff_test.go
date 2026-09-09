package facts

import (
	"strings"
	"testing"
)

// unit.test_diff exists to catch an agent "fixing" a failing test by
// weakening the assertion instead of the underlying bug (talooner#94). The
// core behaviour: only the hunk touching the assertion line survives, not the
// whole test file's diff, and non-test files never contribute at all.
func TestAssertionDiff(t *testing.T) {
	for _, tt := range []struct {
		name  string
		files []string
		diff  string
		want  []string // substrings that must appear
		none  []string // substrings that must NOT appear
	}{
		{
			name:  "loosened tolerance survives, unrelated setup hunk is dropped",
			files: []string{"internal/auth/token_test.go"},
			diff: "diff --git a/internal/auth/token_test.go b/internal/auth/token_test.go\n" +
				"--- a/internal/auth/token_test.go\n+++ b/internal/auth/token_test.go\n" +
				"@@ -1,3 +1,3 @@\n" +
				" func setup() {\n" +
				"-mockClock = fixedClock\n" +
				"+mockClock = wallClock\n" +
				"@@ -10,3 +10,3 @@\n" +
				" func TestExpiry(t *testing.T) {\n" +
				"-require.Equal(t, 5, ttl)\n" +
				"+require.Equal(t, 500, ttl)\n",
			want: []string{"require.Equal(t, 500, ttl)"},
			none: []string{"mockClock = wallClock"},
		},
		{
			name:  "removed assertion line (a pure deletion hunk) still counts",
			files: []string{"pkg/order_test.rb"},
			diff: "diff --git a/pkg/order_test.rb b/pkg/order_test.rb\n" +
				"--- a/pkg/order_test.rb\n+++ b/pkg/order_test.rb\n" +
				"@@ -5,2 +5,1 @@\n" +
				" it \"rejects a negative total\" do\n" +
				"-expect(order.total).to be_negative\n",
			want: []string{"expect(order.total).to be_negative"},
		},
		{
			name:  "non-test file contributes nothing even with an assert-shaped line",
			files: []string{"internal/auth/token.go"},
			diff: "diff --git a/internal/auth/token.go b/internal/auth/token.go\n" +
				"--- a/internal/auth/token.go\n+++ b/internal/auth/token.go\n" +
				"@@ -1,1 +1,1 @@\n" +
				"-assertNever(\"unreachable\")\n" +
				"+assertNever(\"changed\")\n",
			want: nil,
		},
		{
			name:  "test file with no assertion-matching hunk contributes nothing",
			files: []string{"internal/auth/token_test.go"},
			diff: "diff --git a/internal/auth/token_test.go b/internal/auth/token_test.go\n" +
				"--- a/internal/auth/token_test.go\n+++ b/internal/auth/token_test.go\n" +
				"@@ -1,1 +1,1 @@\n" +
				"-fixture := loadFixtureV1()\n" +
				"+fixture := loadFixtureV2()\n",
			want: nil,
		},
		{
			name:  "context-only lines around an assertion do not themselves trigger a match",
			files: []string{"pkg/thing.test.ts"},
			diff: "diff --git a/pkg/thing.test.ts b/pkg/thing.test.ts\n" +
				"--- a/pkg/thing.test.ts\n+++ b/pkg/thing.test.ts\n" +
				"@@ -1,3 +1,3 @@\n" +
				" expect(unrelatedContextLine).toBe(1)\n" +
				"-x = compute()\n" +
				"+x = compute2()\n",
			want: nil,
		},
		{
			name:  "js expect().toEqual( is recognised",
			files: []string{"pkg/thing.spec.js"},
			diff: "diff --git a/pkg/thing.spec.js b/pkg/thing.spec.js\n" +
				"--- a/pkg/thing.spec.js\n+++ b/pkg/thing.spec.js\n" +
				"@@ -1,1 +1,1 @@\n" +
				"-expect(result).toEqual(42)\n" +
				"+expect(result).toEqual(0)\n",
			want: []string{"toEqual(0)"},
		},
		{
			name:  "two test files in one unit: only the matching file's hunk is kept",
			files: []string{"internal/auth/token_test.go", "internal/auth/session_test.go"},
			diff: "diff --git a/internal/auth/token_test.go b/internal/auth/token_test.go\n" +
				"--- a/internal/auth/token_test.go\n+++ b/internal/auth/token_test.go\n" +
				"@@ -1,1 +1,1 @@\n" +
				"-fixture := v1\n" +
				"+fixture := v2\n" +
				"diff --git a/internal/auth/session_test.go b/internal/auth/session_test.go\n" +
				"--- a/internal/auth/session_test.go\n+++ b/internal/auth/session_test.go\n" +
				"@@ -1,1 +1,1 @@\n" +
				"-assert.True(t, valid)\n" +
				"+assert.False(t, valid)\n",
			want: []string{"assert.False(t, valid)"},
			none: []string{"fixture := v2"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			slices := diffSlicesByPath(tt.diff)
			got := testAssertionDiff(tt.files, slices)
			if tt.want == nil && got != "" {
				t.Errorf("testAssertionDiff = %q, want empty", got)
			}
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("testAssertionDiff = %q, want to contain %q", got, w)
				}
			}
			for _, n := range tt.none {
				if strings.Contains(got, n) {
					t.Errorf("testAssertionDiff = %q, must not contain %q", got, n)
				}
			}
		})
	}
}

// isTestFile recognises Go, Ruby, Python and JS/TS test-naming conventions,
// and nothing else — a naming convention it misses is left out, not guessed
// (facts.md's own call on pr.new_dependencies' unrecognised manifests).
func TestIsTestFile(t *testing.T) {
	for _, tt := range []struct {
		path string
		want bool
	}{
		{"internal/auth/token_test.go", true},
		{"app/models/user_test.rb", true},
		{"spec/models/user_spec.rb", true},
		{"tests/test_auth.py", true},
		{"tests/auth_test.py", true},
		{"src/thing.test.ts", true},
		{"src/thing.spec.tsx", true},
		{"internal/auth/token.go", false},
		{"app/models/user.rb", false},
		{"docs/testing.md", false},
		{"internal/auth/contest.go", false}, // "test" as a substring, not the suffix convention
	} {
		if got := isTestFile(tt.path); got != tt.want {
			t.Errorf("isTestFile(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}
