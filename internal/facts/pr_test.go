package facts

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/opentalon/talooner/internal/config"
	"github.com/opentalon/talooner/internal/github"
)

// fakeSource stands in for *github.Client. The extractor is a pure function of
// the API responses, so there is nothing here worth an httptest server.
type fakeSource struct {
	pr       *github.PullRequest
	prErr    error
	files    []string
	fileErr  error
	checks   github.Checks
	checkErr error
	diff     string
	trunc    bool
	diffErr  error
}

func (f fakeSource) ResolveMergeable(_ context.Context, _, _ string, _ int) (*github.PullRequest, error) {
	return f.pr, f.prErr
}

func (f fakeSource) ChangedFiles(_ context.Context, _, _ string, _ int) ([]string, error) {
	return f.files, f.fileErr
}

func (f fakeSource) CommitChecks(_ context.Context, _, _, _ string) (github.Checks, error) {
	return f.checks, f.checkErr
}

func (f fakeSource) Diff(_ context.Context, _, _ string, _, _ int) (string, bool, error) {
	return f.diff, f.trunc, f.diffErr
}

func samplePR() *github.PullRequest {
	return &github.PullRequest{
		Number:       42,
		HeadSHA:      "abc123",
		BaseSHA:      "def456",
		Author:       "evgeny",
		Title:        "Add a thing",
		Body:         "why the thing",
		Draft:        true,
		IsFork:       false,
		Additions:    10,
		Deletions:    3,
		ChangedFiles: 2,
		Commits:      4,
		Labels:       []string{"bug", "v1"},
	}
}

func TestPRAssertsEveryCoreFact(t *testing.T) {
	src := fakeSource{pr: samplePR(), files: []string{"internal/auth/token.go", "README.md"}, diff: "@@ -1 +1 @@\n+hello", trunc: false}

	got, err := PR(context.Background(), src, "opentalon", "talooner", 42, config.Checks{})
	if err != nil {
		t.Fatalf("PR: %v", err)
	}

	want := map[string]any{
		"pr.number":          42,
		"pr.head_sha":        "abc123",
		"pr.base_sha":        "def456",
		"pr.author":          "evgeny",
		"pr.is_fork":         false,
		"pr.draft":           true,
		"pr.title":           "Add a thing",
		"pr.body":            "why the thing",
		"pr.has_description": true,
		"pr.lines_changed":   13,
		"pr.additions":       10,
		"pr.deletions":       3,
		"pr.files_changed":   2,
		"pr.commits":         4,
		"pr.diff":            "@@ -1 +1 @@\n+hello",
		"pr.diff_truncated":  false,
	}
	for name, v := range want {
		if got[name] != v {
			t.Errorf("%s = %v (%T), want %v", name, got[name], got[name], v)
		}
	}

	files, ok := got["pr.changed_files"].([]string)
	if !ok || len(files) != 2 || files[0] != "internal/auth/token.go" {
		t.Errorf("pr.changed_files = %v, want the two paths", got["pr.changed_files"])
	}
	labels, ok := got["pr.labels"].([]string)
	if !ok || len(labels) != 2 || labels[0] != "bug" {
		t.Errorf("pr.labels = %v, want [bug v1]", got["pr.labels"])
	}

	// A fact this package forgot is a fact that reads as a dead extractor, so
	// the count is pinned rather than left to the loop above. checks_pending,
	// pr.diff, pr.diff_truncated and pr.new_dependencies are always asserted
	// (an empty CI, an empty diff and zero new deps are all honest answers, not
	// dead extractors); mergeable is not, because samplePR has none and it is the
	// one fact that may legitimately be omitted (pr_mergeable test below).
	if len(got) != len(want)+4 {
		t.Errorf("asserted %d facts, want %d", len(got), len(want)+4)
	}
}

func TestPRHasDescription(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
		want bool
	}{
		{"prose", "why the thing", true},
		{"empty", "", false},
		{"whitespace only", "  \n\t\r\n ", false},
		{"single character", "x", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pr := samplePR()
			pr.Body = tt.body
			got, err := PR(context.Background(), fakeSource{pr: pr}, "opentalon", "talooner", 42, config.Checks{})
			if err != nil {
				t.Fatalf("PR: %v", err)
			}
			if got["pr.has_description"] != tt.want {
				t.Errorf("pr.has_description = %v, want %v", got["pr.has_description"], tt.want)
			}
			// The empty body itself is still asserted; a rule quoting pr.body
			// on a description-less PR should see "", not a dead extractor.
			if got["pr.body"] != tt.body {
				t.Errorf("pr.body = %q, want %q", got["pr.body"], tt.body)
			}
		})
	}
}

// An empty list matches no predicate, so it is an answer. Omitting it turns a
// rule that quietly does not fire into a not-shaped rule that quietly does
// (facts.md, C1's acceptance).
func TestPRAssertsEmptyListsRatherThanOmittingThem(t *testing.T) {
	pr := samplePR()
	pr.Labels = nil
	pr.ChangedFiles = 0

	got, err := PR(context.Background(), fakeSource{pr: pr, files: nil}, "opentalon", "talooner", 42, config.Checks{})
	if err != nil {
		t.Fatalf("PR: %v", err)
	}

	for _, name := range []string{"pr.changed_files", "pr.labels"} {
		v, ok := got[name]
		if !ok {
			t.Fatalf("%s missing, want an empty list", name)
		}
		list, ok := v.([]string)
		if !ok || len(list) != 0 {
			t.Errorf("%s = %v, want an empty []string", name, v)
		}
	}
}

func TestPRForkIsCarriedThrough(t *testing.T) {
	pr := samplePR()
	pr.IsFork = true

	got, err := PR(context.Background(), fakeSource{pr: pr}, "opentalon", "talooner", 42, config.Checks{})
	if err != nil {
		t.Fatalf("PR: %v", err)
	}
	if got["pr.is_fork"] != true {
		t.Errorf("pr.is_fork = %v, want true", got["pr.is_fork"])
	}
}

// A big PR is the case where a paginated fetch is tempting to truncate.
func TestPRCarriesEveryChangedFile(t *testing.T) {
	paths := make([]string, 400)
	for i := range paths {
		paths[i] = fmt.Sprintf("pkg/file%03d.go", i)
	}

	got, err := PR(context.Background(), fakeSource{pr: samplePR(), files: paths}, "opentalon", "talooner", 42, config.Checks{})
	if err != nil {
		t.Fatalf("PR: %v", err)
	}
	if list := got["pr.changed_files"].([]string); len(list) != 400 {
		t.Errorf("pr.changed_files has %d entries, want 400", len(list))
	}
}

// Half a fact set is worse than none: the facts that did land would be read as
// the whole truth, and the missing ones as determined-false by any not-shaped
// rule.
func TestPRReturnsNoPartialSetOnFailure(t *testing.T) {
	boom := errors.New("boom")

	for _, tt := range []struct {
		name string
		src  fakeSource
	}{
		{"pull request fetch fails", fakeSource{prErr: boom}},
		{"changed files fetch fails", fakeSource{pr: samplePR(), fileErr: boom}},
		{"checks fetch fails", fakeSource{pr: samplePR(), checkErr: boom}},
		{"diff fetch fails", fakeSource{pr: samplePR(), diffErr: boom}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := PR(context.Background(), tt.src, "opentalon", "talooner", 42, config.Checks{})
			if err == nil {
				t.Fatal("err = nil, want the fetch failure")
			}
			if !errors.Is(err, boom) {
				t.Errorf("err = %v, want it to wrap boom", err)
			}
			if got != nil {
				t.Errorf("facts = %v, want nil", got)
			}
		})
	}
}

// The client returns (nil, nil) for nothing today, but a fact set built from a
// nil PR would be silently all-zero — every string empty, every count 0 — which
// is the one failure mode this package exists to prevent.
func TestPRRejectsAMissingPullRequest(t *testing.T) {
	got, err := PR(context.Background(), fakeSource{}, "opentalon", "talooner", 42, config.Checks{})
	if err == nil {
		t.Fatal("err = nil, want a refusal to extract from a nil pull request")
	}
	if got != nil {
		t.Errorf("facts = %v, want nil", got)
	}
}

// pr.mergeable is the one fact that is omitted rather than asserted: nil means
// GitHub's background job has not computed it, which is "we do not know", not
// "there are conflicts". Asserting false there would block a clean PR.
func TestPRMergeableCarriedThrough(t *testing.T) {
	for _, tt := range []struct {
		name      string
		mergeable *bool
		present   bool
	}{
		{"mergeable", boolPtr(true), true},
		{"unmergeable", boolPtr(false), true},
		{"unknown, omitted", nil, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pr := samplePR()
			pr.Mergeable = tt.mergeable
			got, err := PR(context.Background(), fakeSource{pr: pr}, "opentalon", "talooner", 42, config.Checks{})
			if err != nil {
				t.Fatalf("PR: %v", err)
			}
			v, ok := got["pr.mergeable"]
			if ok != tt.present {
				t.Errorf("pr.mergeable present = %v, want %v", ok, tt.present)
			}
			if tt.present && v != *tt.mergeable {
				t.Errorf("pr.mergeable = %v, want %v", v, *tt.mergeable)
			}
		})
	}
}

// An unknown mergeable must not read as false to a not-shaped rule, which is the
// one way this omission could turn into a wrong approval.
func TestPRMergeableOmittedDoesNotReadAsFalse(t *testing.T) {
	pr := samplePR()
	pr.Mergeable = nil
	got, err := PR(context.Background(), fakeSource{pr: pr}, "opentalon", "talooner", 42, config.Checks{})
	if err != nil {
		t.Fatalf("PR: %v", err)
	}
	if _, ok := got["pr.mergeable"]; ok {
		t.Error("pr.mergeable asserted when unknown, want it omitted")
	}
}

// checks_pending is derived from the whole round of CI on the head sha, and is
// asserted either way — an empty CI is settled, not an unknown one.
func TestPRChecksPending(t *testing.T) {
	for _, tt := range []struct {
		name   string
		checks github.Checks
		want   bool
	}{
		{"queued check run", github.Checks{Runs: []github.CheckRunReport{{Status: "queued"}}}, true},
		{"in_progress check run", github.Checks{Runs: []github.CheckRunReport{{Status: "in_progress"}}}, true},
		{"pending status", github.Checks{Statuses: []github.CommitStatus{{State: "pending"}}}, true},
		{"everything settled", github.Checks{Runs: []github.CheckRunReport{{Status: "completed", Conclusion: "success"}}, Statuses: []github.CommitStatus{{State: "success"}}}, false},
		{"no CI at all", github.Checks{}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := PR(context.Background(), fakeSource{pr: samplePR(), checks: tt.checks}, "opentalon", "talooner", 42, config.Checks{})
			if err != nil {
				t.Fatalf("PR: %v", err)
			}
			if got["pr.checks_pending"] != tt.want {
				t.Errorf("pr.checks_pending = %v, want %v", got["pr.checks_pending"], tt.want)
			}
		})
	}
}

// pr.diff and pr.diff_truncated are always asserted, both values verbatim from
// the source — including an empty, untruncated diff, which is the honest answer
// for a PR whose changes are all binary (issue #9).
func TestPRDiffAssertedWithTruncationFlag(t *testing.T) {
	for _, tt := range []struct {
		name      string
		diff      string
		trunc     bool
		wantDiff  string
		wantTrunc bool
	}{
		{"complete diff", "@@ -1 +1 @@\n+x", false, "@@ -1 +1 @@\n+x", false},
		{"truncated diff", "part", true, "part", true},
		{"empty and complete", "", false, "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := PR(context.Background(), fakeSource{pr: samplePR(), diff: tt.diff, trunc: tt.trunc}, "opentalon", "talooner", 42, config.Checks{})
			if err != nil {
				t.Fatalf("PR: %v", err)
			}
			if got["pr.diff"] != tt.wantDiff {
				t.Errorf("pr.diff = %q, want %q", got["pr.diff"], tt.wantDiff)
			}
			if got["pr.diff_truncated"] != tt.wantTrunc {
				t.Errorf("pr.diff_truncated = %v, want %v", got["pr.diff_truncated"], tt.wantTrunc)
			}
		})
	}
}

// derivePassing is the pass-gate the plugin's pr.tests_passing / pr.lint_passing
// read. The table pins every branch: the cases that resolve to a value, and the
// three that leave the fact deliberately unset (facts.md, "tests_passing /
// lint_passing").
func TestDerivePassing(t *testing.T) {
	for _, tt := range []struct {
		name     string
		runs     []github.CheckRunReport
		statuses []github.CommitStatus
		patterns []string
		want     *bool
	}{
		{"no patterns", nil, nil, nil, nil},
		{"no matching check",
			[]github.CheckRunReport{{Name: "deploy", Status: "completed", Conclusion: "success"}},
			nil, []string{"test"}, nil},
		{"all matched success",
			[]github.CheckRunReport{{Name: "test", Status: "completed", Conclusion: "success"}},
			nil, []string{"test"}, boolPtr(true)},
		{"matched failure",
			[]github.CheckRunReport{{Name: "test", Status: "completed", Conclusion: "failure"}},
			nil, []string{"test"}, boolPtr(false)},
		{"matched timed_out",
			[]github.CheckRunReport{{Name: "test", Status: "completed", Conclusion: "timed_out"}},
			nil, []string{"test"}, boolPtr(false)},
		{"matched cancelled",
			[]github.CheckRunReport{{Name: "test", Status: "completed", Conclusion: "cancelled"}},
			nil, []string{"test"}, boolPtr(false)},
		{"matched still queued",
			[]github.CheckRunReport{{Name: "test", Status: "queued"}},
			nil, []string{"test"}, nil},
		{"matched in_progress",
			[]github.CheckRunReport{{Name: "test", Status: "in_progress"}},
			nil, []string{"test"}, nil},
		{"matched neutral is unknown, unset",
			[]github.CheckRunReport{{Name: "test", Status: "completed", Conclusion: "neutral"}},
			nil, []string{"test"}, nil},
		{"matched skipped is unknown, unset",
			[]github.CheckRunReport{{Name: "test", Status: "completed", Conclusion: "skipped"}},
			nil, []string{"test"}, nil},
		// Precedence: a recognised failure wins over an unknown conclusion, so a
		// PR with one red test and one neutral test is not passing.
		{"failure beats unknown",
			[]github.CheckRunReport{
				{Name: "test", Status: "completed", Conclusion: "failure"},
				{Name: "test", Status: "completed", Conclusion: "neutral"},
			}, nil, []string{"test"}, boolPtr(false)},
		// Statuses (the older commit-status API) are matched too.
		{"status success",
			nil, []github.CommitStatus{{Context: "test", State: "success"}},
			[]string{"test"}, boolPtr(true)},
		{"status failure",
			nil, []github.CommitStatus{{Context: "test", State: "failure"}},
			[]string{"test"}, boolPtr(false)},
		{"status error",
			nil, []github.CommitStatus{{Context: "test", State: "error"}},
			[]string{"test"}, boolPtr(false)},
		{"status pending",
			nil, []github.CommitStatus{{Context: "test", State: "pending"}},
			[]string{"test"}, nil},
		// A check outside the pattern does not count; a check inside does.
		{"pattern ci/* matches across a slash",
			[]github.CheckRunReport{{Name: "ci/build", Status: "completed", Conclusion: "success"}},
			nil, []string{"ci/*"}, boolPtr(true)},
		{"pattern *unit* substring",
			[]github.CheckRunReport{{Name: "my-unit-tests", Status: "completed", Conclusion: "success"}},
			nil, []string{"*unit*"}, boolPtr(true)},
		{"case insensitive",
			[]github.CheckRunReport{{Name: "Unit Tests", Status: "completed", Conclusion: "success"}},
			nil, []string{"UNIT TESTS"}, boolPtr(true)},
		// Two patterns, one matched failing, one matched passing: the failure
		// wins.
		{"mixed patterns, one fails",
			[]github.CheckRunReport{
				{Name: "test", Status: "completed", Conclusion: "success"},
				{Name: "integration", Status: "completed", Conclusion: "failure"},
			}, nil, []string{"test", "integration"}, boolPtr(false)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := derivePassing(tt.runs, tt.statuses, tt.patterns)
			if (got == nil) != (tt.want == nil) {
				t.Fatalf("derivePassing = %v, want %v", got, tt.want)
			}
			if got != nil && *got != *tt.want {
				t.Errorf("derivePassing = %v, want %v", *got, *tt.want)
			}
		})
	}
}

// pr.tests_passing and pr.lint_passing are asserted only when determined, and
// read the same CI C8 fetched. The unset cases must be absent from the set, not
// false — that is the whole point of the gate (facts.md, "Unset is false").
func TestPRDerivesPassingFacts(t *testing.T) {
	checks := github.Checks{
		Runs: []github.CheckRunReport{
			{Name: "test", Status: "completed", Conclusion: "success"},
			{Name: "lint", Status: "completed", Conclusion: "failure"},
		},
	}
	cfg := config.Checks{Tests: []string{"test"}, Lint: []string{"lint"}}

	got, err := PR(context.Background(),
		fakeSource{pr: samplePR(), checks: checks}, "opentalon", "talooner", 42, cfg)
	if err != nil {
		t.Fatalf("PR: %v", err)
	}
	if got["pr.tests_passing"] != true {
		t.Errorf("pr.tests_passing = %v, want true", got["pr.tests_passing"])
	}
	if got["pr.lint_passing"] != false {
		t.Errorf("pr.lint_passing = %v, want false", got["pr.lint_passing"])
	}
}

// pr.new_dependencies is parsed from the diff C2 already fetched and is always
// asserted — a PR that adds no dependencies gets 0, which is the honest answer,
// not a dead extractor (facts.md, "pr.new_dependencies").
func TestPRAssertsNewDependenciesAlways(t *testing.T) {
	for _, tt := range []struct {
		name string
		diff string
		want int
	}{
		{"no manifest in diff", "@@ -1 +1 @@\n+hello", 0},
		{"empty diff", "", 0},
		{"three go.mod requires added", diffGitFile("go.mod", "+require github.com/a v1.0.0\n+require github.com/b v2.0.0\n+require github.com/c v0.1.0"), 3},
		// A version bump removes and re-adds the same name: an upgrade, not new.
		{"go.mod upgrade is not new", diffGitFile("go.mod", "-require github.com/a v1.0.0\n+require github.com/a v2.0.0"), 0},
		// Lockfile churn alone is ignored.
		{"go.sum churn ignored", diffGitFile("go.sum", "+github.com/a v1.0.0 h1:abc=\n+github.com/b v2.0.0 h1:def="), 0},
		{"package-lock churn ignored", diffGitFile("package-lock.json", `+"a": {"version":"1.0.0"}`), 0},
		// package.json deps-block entries count; a top-level field of the same
		// shape does not, and an upgrade nets to zero.
		{"package.json new dep", diffGitFile("package.json", "+\"dependencies\": {\n+\"leftpad\": \"1.0.0\"\n+}"), 1},
		{"package.json upgrade ignored", diffGitFile("package.json", "+\"dependencies\": {\n-\"leftpad\": \"1.0.0\"\n+\"leftpad\": \"2.0.0\"\n+}"), 0},
		// requirements.txt and Gemfile and Cargo.toml all count.
		{"requirements new", diffGitFile("requirements.txt", "+requests>=2.0\n+flask==3.0"), 2},
		{"gemfile new", diffGitFile("Gemfile", `+gem "rails"`+"\n"+`+gem 'pg'`), 2},
		{"cargo new", diffGitFile("Cargo.toml", "[dependencies]\n+serde = \"1.0\"\n+tokio = { version = \"1\" }"), 2},
		{"cargo package fields ignored", diffGitFile("Cargo.toml", "[package]\n+name = \"x\"\n+version = \"0.1.0\"\n+edition = \"2021\""), 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := PR(context.Background(),
				fakeSource{pr: samplePR(), diff: tt.diff}, "opentalon", "talooner", 42, config.Checks{})
			if err != nil {
				t.Fatalf("PR: %v", err)
			}
			if got["pr.new_dependencies"] != tt.want {
				t.Errorf("pr.new_dependencies = %v, want %v", got["pr.new_dependencies"], tt.want)
			}
		})
	}
}

// diffGitFile wraps a single-file patch body in the header countNewDependencies
// splits on, so tests assert against the real concatenated-diff shape.
func diffGitFile(name, body string) string {
	return "diff --git a/" + name + " b/" + name + "\n" + body
}

func TestPRLeavesPassingFactsUnsetWithoutPatterns(t *testing.T) {
	checks := github.Checks{
		Runs: []github.CheckRunReport{{Name: "test", Status: "completed", Conclusion: "success"}},
	}
	// No patterns: the gate must not fire, so the fact is omitted rather than
	// guessed true from unmatched CI.
	got, err := PR(context.Background(),
		fakeSource{pr: samplePR(), checks: checks}, "opentalon", "talooner", 42, config.Checks{})
	if err != nil {
		t.Fatalf("PR: %v", err)
	}
	if _, ok := got["pr.tests_passing"]; ok {
		t.Error("pr.tests_passing asserted without patterns, want it omitted")
	}
	if _, ok := got["pr.lint_passing"]; ok {
		t.Error("pr.lint_passing asserted without patterns, want it omitted")
	}
}
