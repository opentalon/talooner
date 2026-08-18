package facts

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/opentalon/talooner/internal/github"
)

// fakeSource stands in for *github.Client. The extractor is a pure function of
// two API responses, so there is nothing here worth an httptest server.
type fakeSource struct {
	pr      *github.PullRequest
	prErr   error
	files   []string
	fileErr error
}

func (f fakeSource) PullRequest(_ context.Context, _, _ string, _ int) (*github.PullRequest, error) {
	return f.pr, f.prErr
}

func (f fakeSource) ChangedFiles(_ context.Context, _, _ string, _ int) ([]string, error) {
	return f.files, f.fileErr
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
	src := fakeSource{pr: samplePR(), files: []string{"internal/auth/token.go", "README.md"}}

	got, err := PR(context.Background(), src, "opentalon", "talooner", 42)
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
	// the count is pinned rather than left to the loop above.
	if len(got) != len(want)+2 {
		t.Errorf("asserted %d facts, want %d", len(got), len(want)+2)
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
			got, err := PR(context.Background(), fakeSource{pr: pr}, "opentalon", "talooner", 42)
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

	got, err := PR(context.Background(), fakeSource{pr: pr, files: nil}, "opentalon", "talooner", 42)
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

	got, err := PR(context.Background(), fakeSource{pr: pr}, "opentalon", "talooner", 42)
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

	got, err := PR(context.Background(), fakeSource{pr: samplePR(), files: paths}, "opentalon", "talooner", 42)
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
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := PR(context.Background(), tt.src, "opentalon", "talooner", 42)
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
	got, err := PR(context.Background(), fakeSource{}, "opentalon", "talooner", 42)
	if err == nil {
		t.Fatal("err = nil, want a refusal to extract from a nil pull request")
	}
	if got != nil {
		t.Errorf("facts = %v, want nil", got)
	}
}
