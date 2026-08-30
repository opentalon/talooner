package facts

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/opentalon/talooner/internal/config"
	"github.com/opentalon/talooner/internal/github"
)

// fakeSource stands in for *github.Client. The extractor is a pure function of
// the API responses, so there is nothing here worth an httptest server.
type fakeSource struct {
	pr         *github.PullRequest
	prErr      error
	files      []string
	fileStats  []github.FileStat
	fileErr    error
	checks     github.Checks
	checkErr   error
	diff       string
	trunc      bool
	diffErr    error
	reviews    []github.ReviewReport
	reviewErr  error
	toucher    string
	toucherErr error
}

func (f fakeSource) ResolveMergeable(_ context.Context, _, _ string, _ int) (*github.PullRequest, error) {
	return f.pr, f.prErr
}

func (f fakeSource) ChangedFileStats(_ context.Context, _, _ string, _ int) ([]github.FileStat, error) {
	if f.fileStats != nil {
		return f.fileStats, f.fileErr
	}
	stats := make([]github.FileStat, 0, len(f.files))
	for _, p := range f.files {
		stats = append(stats, github.FileStat{Path: p})
	}
	return stats, f.fileErr
}

func (f fakeSource) CommitChecks(_ context.Context, _, _, _ string) (github.Checks, error) {
	return f.checks, f.checkErr
}

func (f fakeSource) Diff(_ context.Context, _, _ string, _, _ int) (string, bool, error) {
	return f.diff, f.trunc, f.diffErr
}

func (f fakeSource) PullRequestReviews(_ context.Context, _, _ string, _ int) ([]github.ReviewReport, error) {
	return f.reviews, f.reviewErr
}

func (f fakeSource) LastToucher(_ context.Context, _, _, _ string, _ []string) (string, error) {
	return f.toucher, f.toucherErr
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

	got, _, err := PR(context.Background(), src, "opentalon", "talooner", 42, config.Checks{}, nil, nil, nil, nil)
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
		// user.author is always asserted (facts.md, "user.*").
		"user.author": "evgeny",
		// module.touched_count is always asserted, 0 here (no modules configured,
		// facts.md, "module.touched_count").
		"module.touched_count": 0,
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
	// pr.diff, pr.diff_truncated, pr.new_dependencies and pr.upgraded_dependencies
	// are always asserted (an empty CI, an empty diff and zero new/upgraded deps
	// are all honest answers, not dead extractors); mergeable is not, because
	// samplePR has none and it is the one fact that may legitimately be omitted
	// (pr_mergeable test below). user.author and module.touched_count are always
	// asserted too (no CODEOWNERS here, so user.owner / user.owners /
	// user.reviewer stay unset; no modules configured, so module.touched_count
	// reads 0). review.human.approved and review.changes_requested are always
	// asserted too (no reviews here, so both read false); there are no
	// teams.yaml entries or requested teams, so no review.<team>.* facts exist
	// to count. The six code.*_changed / code.touches_* roll-ups are always
	// asserted as well (facts.md, "code.*") — "internal/auth/token.go" touches
	// the built-in Go service layer, so code.services_changed and
	// code.touches_service read non-empty/true, the other four read
	// empty/false.
	if len(got) != len(want)+13 {
		t.Errorf("asserted %d facts, want %d", len(got), len(want)+13)
	}
	if svc, ok := got["code.services_changed"].([]string); !ok || len(svc) != 1 || svc[0] != "internal/auth" {
		t.Errorf("code.services_changed = %v, want [internal/auth]", got["code.services_changed"])
	}
	if got["code.touches_service"] != true {
		t.Errorf("code.touches_service = %v, want true", got["code.touches_service"])
	}
	if got["code.touches_model"] != false || got["code.touches_controller"] != false {
		t.Errorf("code.touches_model/controller = %v/%v, want false/false", got["code.touches_model"], got["code.touches_controller"])
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
			got, _, err := PR(context.Background(), fakeSource{pr: pr}, "opentalon", "talooner", 42, config.Checks{}, nil, nil, nil, nil)
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

	got, _, err := PR(context.Background(), fakeSource{pr: pr, files: nil}, "opentalon", "talooner", 42, config.Checks{}, nil, nil, nil, nil)
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

	got, _, err := PR(context.Background(), fakeSource{pr: pr}, "opentalon", "talooner", 42, config.Checks{}, nil, nil, nil, nil)
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

	got, _, err := PR(context.Background(), fakeSource{pr: samplePR(), files: paths}, "opentalon", "talooner", 42, config.Checks{}, nil, nil, nil, nil)
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
			got, _, err := PR(context.Background(), tt.src, "opentalon", "talooner", 42, config.Checks{}, nil, nil, nil, nil)
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
	got, _, err := PR(context.Background(), fakeSource{}, "opentalon", "talooner", 42, config.Checks{}, nil, nil, nil, nil)
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
			got, _, err := PR(context.Background(), fakeSource{pr: pr}, "opentalon", "talooner", 42, config.Checks{}, nil, nil, nil, nil)
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
	got, _, err := PR(context.Background(), fakeSource{pr: pr}, "opentalon", "talooner", 42, config.Checks{}, nil, nil, nil, nil)
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
			got, _, err := PR(context.Background(), fakeSource{pr: samplePR(), checks: tt.checks}, "opentalon", "talooner", 42, config.Checks{}, nil, nil, nil, nil)
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
			got, _, err := PR(context.Background(), fakeSource{pr: samplePR(), diff: tt.diff, trunc: tt.trunc}, "opentalon", "talooner", 42, config.Checks{}, nil, nil, nil, nil)
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

	got, _, err := PR(context.Background(),
		fakeSource{pr: samplePR(), checks: checks}, "opentalon", "talooner", 42, cfg, nil, nil, nil, nil)
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

// pr.new_dependencies / pr.upgraded_dependencies are parsed from the diff C2
// already fetched and are always asserted — a PR that touches neither gets 0
// for both, which is the honest answer, not a dead extractor (facts.md,
// "pr.new_dependencies", "pr.upgraded_dependencies").
func TestPRAssertsNewDependenciesAlways(t *testing.T) {
	for _, tt := range []struct {
		name         string
		diff         string
		want         int
		wantUpgraded int
	}{
		{"no manifest in diff", "@@ -1 +1 @@\n+hello", 0, 0},
		{"empty diff", "", 0, 0},
		{"three go.mod requires added", diffGitFile("go.mod", "+require github.com/a v1.0.0\n+require github.com/b v2.0.0\n+require github.com/c v0.1.0"), 3, 0},
		// A version bump removes and re-adds the same name: upgraded, not new.
		{"go.mod upgrade counts as upgraded, not new", diffGitFile("go.mod", "-require github.com/a v1.0.0\n+require github.com/a v2.0.0"), 0, 1},
		// A removal with no matching add is neither new nor upgraded.
		{"go.mod removal counts as neither", diffGitFile("go.mod", "-require github.com/a v1.0.0"), 0, 0},
		// A mix of a new dep and an upgraded one in the same file counts both.
		{"go.mod new and upgraded together", diffGitFile("go.mod", "+require github.com/a v1.0.0\n-require github.com/b v1.0.0\n+require github.com/b v2.0.0"), 1, 1},
		// Lockfile churn alone is ignored.
		{"go.sum churn ignored", diffGitFile("go.sum", "+github.com/a v1.0.0 h1:abc=\n+github.com/b v2.0.0 h1:def="), 0, 0},
		{"package-lock churn ignored", diffGitFile("package-lock.json", `+"a": {"version":"1.0.0"}`), 0, 0},
		// package.json deps-block entries count; a top-level field of the same
		// shape does not, and an upgrade nets to zero new but one upgraded.
		{"package.json new dep", diffGitFile("package.json", "+\"dependencies\": {\n+\"leftpad\": \"1.0.0\"\n+}"), 1, 0},
		{"package.json upgrade counts as upgraded", diffGitFile("package.json", "+\"dependencies\": {\n-\"leftpad\": \"1.0.0\"\n+\"leftpad\": \"2.0.0\"\n+}"), 0, 1},
		// requirements.txt and Gemfile and Cargo.toml all count.
		{"requirements new", diffGitFile("requirements.txt", "+requests>=2.0\n+flask==3.0"), 2, 0},
		{"gemfile new", diffGitFile("Gemfile", `+gem "rails"`+"\n"+`+gem 'pg'`), 2, 0},
		{"cargo new", diffGitFile("Cargo.toml", "[dependencies]\n+serde = \"1.0\"\n+tokio = { version = \"1\" }"), 2, 0},
		{"cargo package fields ignored", diffGitFile("Cargo.toml", "[package]\n+name = \"x\"\n+version = \"0.1.0\"\n+edition = \"2021\""), 0, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, _, err := PR(context.Background(),
				fakeSource{pr: samplePR(), diff: tt.diff}, "opentalon", "talooner", 42, config.Checks{}, nil, nil, nil, nil)
			if err != nil {
				t.Fatalf("PR: %v", err)
			}
			if got["pr.new_dependencies"] != tt.want {
				t.Errorf("pr.new_dependencies = %v, want %v", got["pr.new_dependencies"], tt.want)
			}
			if got["pr.upgraded_dependencies"] != tt.wantUpgraded {
				t.Errorf("pr.upgraded_dependencies = %v, want %v", got["pr.upgraded_dependencies"], tt.wantUpgraded)
			}
		})
	}
}

// diffGitFile wraps a single-file patch body in the header countDependencyChanges
// splits on, so tests assert against the real concatenated-diff shape.
func diffGitFile(name, body string) string {
	return "diff --git a/" + name + " b/" + name + "\n" + body
}

// A manifest with real additions/deletions in the changed-file stats but no
// matching entry in pr.diff is GitHub's null-patch case (binary or oversized
// file): pr.diff (C2) drops it entirely, so there is nothing for the parser to
// read. That must fail the whole extraction, not report a confident zero
// (issue #11, facts.md "An unparseable manifest fails the whole extraction").
func TestPRFailsOnManifestWithNoReadableDiff(t *testing.T) {
	src := fakeSource{
		pr:        samplePR(),
		diff:      "@@ -1 +1 @@\n+hello",
		fileStats: []github.FileStat{{Path: "go.mod", Additions: 5, Deletions: 2}},
	}
	_, _, err := PR(context.Background(), src, "opentalon", "talooner", 42, config.Checks{}, nil, nil, nil, nil)
	if err == nil {
		t.Fatal("PR: want error for a manifest absent from the diff, got nil")
	}
}

// A manifest present in the diff, even with no recognised dependency line
// inside it (a pure reformat), is not unparseable — it read fine, it just had
// nothing to count. Zero is still the honest answer there.
func TestPRManifestInDiffWithNoDepLinesIsZeroNotError(t *testing.T) {
	src := fakeSource{
		pr:        samplePR(),
		diff:      diffGitFile("go.mod", " module example.com/x\n\n go 1.24"),
		fileStats: []github.FileStat{{Path: "go.mod", Additions: 1, Deletions: 1}},
	}
	got, _, err := PR(context.Background(), src, "opentalon", "talooner", 42, config.Checks{}, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("PR: %v", err)
	}
	if got["pr.new_dependencies"] != 0 || got["pr.upgraded_dependencies"] != 0 {
		t.Errorf("pr.new_dependencies/pr.upgraded_dependencies = %v/%v, want 0/0",
			got["pr.new_dependencies"], got["pr.upgraded_dependencies"])
	}
}

// A manifest that shows up in stats with zero net change (a pure rename, no
// content touched) never enters the diff either, but that is not an error:
// nothing textual changed, so zero is correct, not a guess.
func TestPRManifestWithNoStatChangeIsNotUnparseable(t *testing.T) {
	src := fakeSource{
		pr:        samplePR(),
		diff:      "@@ -1 +1 @@\n+hello",
		fileStats: []github.FileStat{{Path: "go.mod", Additions: 0, Deletions: 0}},
	}
	got, _, err := PR(context.Background(), src, "opentalon", "talooner", 42, config.Checks{}, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("PR: %v", err)
	}
	if got["pr.new_dependencies"] != 0 || got["pr.upgraded_dependencies"] != 0 {
		t.Errorf("pr.new_dependencies/pr.upgraded_dependencies = %v/%v, want 0/0",
			got["pr.new_dependencies"], got["pr.upgraded_dependencies"])
	}
}

func TestPRLeavesPassingFactsUnsetWithoutPatterns(t *testing.T) {
	checks := github.Checks{
		Runs: []github.CheckRunReport{{Name: "test", Status: "completed", Conclusion: "success"}},
	}
	// No patterns: the gate must not fire, so the fact is omitted rather than
	// guessed true from unmatched CI.
	got, _, err := PR(context.Background(),
		fakeSource{pr: samplePR(), checks: checks}, "opentalon", "talooner", 42, config.Checks{}, nil, nil, nil, nil)
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

// user.author is always asserted as pr.author, for symmetry with user.owner.
func TestPRUserAuthorIsPRAuthor(t *testing.T) {
	got, _, err := PR(context.Background(), fakeSource{pr: samplePR()}, "opentalon", "talooner", 42, config.Checks{}, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("PR: %v", err)
	}
	if got["user.author"] != "evgeny" {
		t.Errorf("user.author = %v, want evgeny", got["user.author"])
	}
}

// user.owner / user.owners come from CODEOWNERS, resolved against the changed
// paths. The last matching rule wins (GitHub), and the union is sorted.
func TestPRUserOwnerFromCodeowners(t *testing.T) {
	const co = "*           @everyone\ndocs/*      @docs\ndocs/README.md  @readme\n"
	src := fakeSource{
		pr:    samplePR(),
		files: []string{"docs/README.md", "src/app.go"},
	}
	got, _, err := PR(context.Background(), src, "opentalon", "talooner", 42, config.Checks{}, []byte(co), nil, nil, nil)
	if err != nil {
		t.Fatalf("PR: %v", err)
	}
	if got["user.owner"] != "@readme" {
		t.Errorf("user.owner = %v, want @readme (last matching rule)", got["user.owner"])
	}
	owners, ok := got["user.owners"].([]string)
	if !ok || len(owners) != 2 || owners[0] != "@everyone" || owners[1] != "@readme" {
		t.Errorf("user.owners = %v, want [@everyone @readme] sorted", got["user.owners"])
	}
}

// A path neither CODEOWNERS nor modules.yaml names leaves user.owner /
// user.owners unset rather than guessed at pr.author — the git-history tier is
// a later ticket (facts.md, "user.owner").
// A path neither CODEOWNERS nor git log resolves leaves user.owner /
// user.owners / user.last_toucher unset rather than guessed at pr.author.
func TestPRUserOwnerUnsetWhenCodeownersAndLastToucherSilent(t *testing.T) {
	const co = "/internal/secret/* @alice\n"
	src := fakeSource{pr: samplePR(), files: []string{"README.md"}}
	got, _, err := PR(context.Background(), src, "opentalon", "talooner", 42, config.Checks{}, []byte(co), nil, nil, nil)
	if err != nil {
		t.Fatalf("PR: %v", err)
	}
	for _, name := range []string{"user.owner", "user.owners", "user.last_toucher"} {
		if _, ok := got[name]; ok {
			t.Errorf("%s = %v, want unset", name, got[name])
		}
	}
}

// The git-log tier (LastToucher) is the second tier of user.owner resolution,
// consulted only when CODEOWNERS names nobody for any touched path (facts.md,
// "user.owner"). user.last_toucher is asserted alongside user.owner /
// user.owners exactly when this tier is what resolved them.
func TestPRUserOwnerFromLastToucherWhenCodeownersSilent(t *testing.T) {
	const co = "/internal/secret/* @alice\n"
	src := fakeSource{pr: samplePR(), files: []string{"billing/invoice.go"}, toucher: "carol"}
	got, _, err := PR(context.Background(), src, "opentalon", "talooner", 42, config.Checks{}, []byte(co), nil, nil, nil)
	if err != nil {
		t.Fatalf("PR: %v", err)
	}
	if got["user.owner"] != "carol" {
		t.Errorf("user.owner = %v, want carol", got["user.owner"])
	}
	owners, ok := got["user.owners"].([]string)
	if !ok || len(owners) != 1 || owners[0] != "carol" {
		t.Errorf("user.owners = %v, want [carol]", got["user.owners"])
	}
	if got["user.last_toucher"] != "carol" {
		t.Errorf("user.last_toucher = %v, want carol", got["user.last_toucher"])
	}
}

// CODEOWNERS naming an owner for even one touched path wins outright — the
// tiers are a waterfall, not a merge, so LastToucher is never even called once
// CODEOWNERS has answered, and user.last_toucher stays unset.
func TestPRUserOwnerCodeownersWinsOverLastToucher(t *testing.T) {
	const co = "billing/* @alice\n"
	src := fakeSource{pr: samplePR(), files: []string{"billing/invoice.go"}, toucher: "carol"}
	got, _, err := PR(context.Background(), src, "opentalon", "talooner", 42, config.Checks{}, []byte(co), nil, nil, nil)
	if err != nil {
		t.Fatalf("PR: %v", err)
	}
	if got["user.owner"] != "@alice" {
		t.Errorf("user.owner = %v, want @alice (CODEOWNERS, not the git-log tier)", got["user.owner"])
	}
	if _, ok := got["user.last_toucher"]; ok {
		t.Errorf("user.last_toucher = %v, want unset (LastToucher never called)", got["user.last_toucher"])
	}
}

// A repo with no CODEOWNERS at all still falls to the git-log tier, not just a
// CODEOWNERS silent on the specific path.
func TestPRUserOwnerFromLastToucherWithNoCodeowners(t *testing.T) {
	src := fakeSource{pr: samplePR(), files: []string{"billing/invoice.go"}, toucher: "carol"}
	got, _, err := PR(context.Background(), src, "opentalon", "talooner", 42, config.Checks{}, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("PR: %v", err)
	}
	if got["user.owner"] != "carol" {
		t.Errorf("user.owner = %v, want carol", got["user.owner"])
	}
}

// A LastToucher failure fails the whole extraction, same shape as any other
// extractor in this package — never a partial set (package comment).
func TestPRFailsWhenLastToucherErrors(t *testing.T) {
	src := fakeSource{pr: samplePR(), files: []string{"billing/invoice.go"}, toucherErr: errors.New("rate limited")}
	_, _, err := PR(context.Background(), src, "opentalon", "talooner", 42, config.Checks{}, nil, nil, nil, nil)
	if err == nil {
		t.Fatal("PR: want error, got nil")
	}
}

// user.reviewer is the standing review request: a user login is preferred over a
// team slug. Absent when nothing is requested.
func TestPRUserReviewerFromRequested(t *testing.T) {
	for _, tt := range []struct {
		name string
		pr   *github.PullRequest
		want string
	}{
		{"user preferred", &github.PullRequest{Requested: github.Reviewers{Users: []string{"alice"}, Teams: []string{"security"}}}, "alice"},
		{"team when no user", &github.PullRequest{Requested: github.Reviewers{Teams: []string{"security"}}}, "security"},
		{"none unset", &github.PullRequest{}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, _, err := PR(context.Background(), fakeSource{pr: tt.pr}, "opentalon", "talooner", 42, config.Checks{}, nil, nil, nil, nil)
			if err != nil {
				t.Fatalf("PR: %v", err)
			}
			v, ok := got["user.reviewer"]
			if tt.want == "" {
				if ok {
					t.Errorf("user.reviewer = %v, want unset", v)
				}
				return
			}
			if v != tt.want {
				t.Errorf("user.reviewer = %v, want %v", v, tt.want)
			}
		})
	}
}

func TestPRModuleFacts(t *testing.T) {
	modules := []config.Module{
		{Path: "internal/auth/", DocumentationURL: "https://docs/auth", Owner: "@alice"},
		{Path: "billing/", DocumentationURL: "https://docs/billing", Owner: "@org/payments"},
		{Path: "docs/", DocumentationURL: "https://docs/site", Owner: ""},
	}

	for _, tt := range []struct {
		name    string
		files   []github.FileStat
		modules []config.Module
		want    map[string]any
		unset   []string
	}{
		{
			name:    "no configured module touched",
			files:   []github.FileStat{{Path: "README.md", Additions: 5, Deletions: 1}},
			modules: modules,
			want:    map[string]any{"module.touched_count": 0},
			unset:   []string{"module.documentation_url", "module.documentation_urls", "module.owner"},
		},
		{
			name:    "no modules configured at all",
			files:   []github.FileStat{{Path: "internal/auth/x.go", Additions: 9, Deletions: 0}},
			modules: nil,
			want:    map[string]any{"module.touched_count": 0},
			unset:   []string{"module.documentation_url", "module.documentation_urls", "module.owner"},
		},
		{
			name:    "one module touched, full facts",
			files:   []github.FileStat{{Path: "internal/auth/token.go", Additions: 9, Deletions: 1}},
			modules: modules,
			want: map[string]any{
				"module.touched_count":      1,
				"module.documentation_url":  "https://docs/auth",
				"module.documentation_urls": []string{"https://docs/auth"},
				"module.owner":              "@alice",
			},
		},
		{
			name: "primary by most changed lines, tie broken by path order",
			files: []github.FileStat{
				{Path: "billing/invoice.go", Additions: 2, Deletions: 0},
				{Path: "docs/index.md", Additions: 2, Deletions: 0},
				{Path: "internal/auth/token.go", Additions: 50, Deletions: 3},
			},
			modules: modules,
			want: map[string]any{
				"module.touched_count":      3,
				"module.documentation_url":  "https://docs/auth",
				"module.documentation_urls": []string{"https://docs/auth", "https://docs/billing", "https://docs/site"},
				"module.owner":              "@alice",
			},
		},
		{
			name: "exact tie on lines falls to path order",
			files: []github.FileStat{
				// billing/ and docs/ both carry 4 lines; billing/ is the
				// lexicographically smaller path, so it wins the tie.
				{Path: "billing/x.go", Additions: 4, Deletions: 0},
				{Path: "docs/y.md", Additions: 4, Deletions: 0},
			},
			modules: modules,
			want: map[string]any{
				"module.touched_count":      2,
				"module.documentation_url":  "https://docs/billing",
				"module.documentation_urls": []string{"https://docs/billing", "https://docs/site"},
				"module.owner":              "@org/payments",
			},
		},
		{
			name:    "owner-less module does not force an empty owner fact",
			files:   []github.FileStat{{Path: "docs/y.md", Additions: 4, Deletions: 0}},
			modules: modules,
			want: map[string]any{
				"module.touched_count":      1,
				"module.documentation_url":  "https://docs/site",
				"module.documentation_urls": []string{"https://docs/site"},
			},
			unset: []string{"module.owner"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, _, err := PR(context.Background(), fakeSource{pr: samplePR(), fileStats: tt.files},
				"opentalon", "talooner", 42, config.Checks{}, nil, tt.modules, nil, nil)
			if err != nil {
				t.Fatalf("PR: %v", err)
			}
			for name, v := range tt.want {
				if !reflect.DeepEqual(got[name], v) {
					t.Errorf("%s = %v (%T), want %v", name, got[name], got[name], v)
				}
			}
			for _, name := range tt.unset {
				if _, ok := got[name]; ok {
					t.Errorf("%s = %v, want unset", name, got[name])
				}
			}
		})
	}
}

func TestModuleOwns(t *testing.T) {
	cases := []struct {
		prefix, path string
		want         bool
	}{
		{"internal/auth/", "internal/auth/token.go", true},
		{"internal/auth/", "internal/auth", true},
		{"internal/auth/", "internal/authority/x.go", false},
		{"billing/", "billing/invoice.go", true},
		{"billing/", "billing", true},
		{"docs/", "internal/docs/x.go", false},
	}
	for _, c := range cases {
		if got := moduleOwns(c.prefix, c.path); got != c.want {
			t.Errorf("moduleOwns(%q, %q) = %v, want %v", c.prefix, c.path, got, c.want)
		}
	}
}
