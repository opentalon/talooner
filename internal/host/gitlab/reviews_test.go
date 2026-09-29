package gitlab

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opentalon/talooner/internal/host"
)

func reviewsServer(t *testing.T, headSHA, discussions string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/discussions"):
			_, _ = fmt.Fprint(w, discussions)
		case strings.HasSuffix(r.URL.Path, "/merge_requests/1"):
			_, _ = fmt.Fprintf(w, `{"iid":1,"sha":%s,"state":"opened"}`, jsonString(headSHA))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
}

func TestGitLabPullRequestReviewsMapsApprovalSystemNotes(t *testing.T) {
	discussions := `[
		{"id":"d1","notes":[{"id":1,"body":"approved this merge request","system":true,"author":{"username":"alice","bot":false}}]}
	]`
	srv := reviewsServer(t, "headsha1", discussions)
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	reviews, err := c.PullRequestReviews(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatalf("PullRequestReviews: %v", err)
	}
	if len(reviews) != 1 {
		t.Fatalf("reviews = %+v, want 1", reviews)
	}
	r := reviews[0]
	if r.Login != "alice" || r.State != host.StateApproved || r.CommitID != "headsha1" || r.Bot {
		t.Errorf("review = %+v, want alice/APPROVED/headsha1/human", r)
	}
}

func TestGitLabPullRequestReviewsMapsUnapproveToDismissed(t *testing.T) {
	discussions := `[
		{"id":"d1","notes":[{"id":1,"body":"unapproved this merge request","system":true,"author":{"username":"alice"}}]}
	]`
	srv := reviewsServer(t, "headsha1", discussions)
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	reviews, err := c.PullRequestReviews(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatalf("PullRequestReviews: %v", err)
	}
	if len(reviews) != 1 || reviews[0].State != "DISMISSED" {
		t.Errorf("reviews = %+v, want one DISMISSED review", reviews)
	}
}

func TestGitLabPullRequestReviewsMapsUnresolvedThreadToChangesRequested(t *testing.T) {
	discussions := `[
		{"id":"d1","notes":[{"id":2,"body":"please fix this","system":false,"resolvable":true,"resolved":false,"author":{"username":"bob"}}]}
	]`
	srv := reviewsServer(t, "headsha1", discussions)
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	reviews, err := c.PullRequestReviews(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatalf("PullRequestReviews: %v", err)
	}
	if len(reviews) != 1 || reviews[0].Login != "bob" || reviews[0].State != host.StateChangesRequested {
		t.Errorf("reviews = %+v, want bob/CHANGES_REQUESTED", reviews)
	}
}

func TestGitLabPullRequestReviewsSkipsResolvedThreads(t *testing.T) {
	discussions := `[
		{"id":"d1","notes":[{"id":2,"body":"nit","system":false,"resolvable":true,"resolved":true,"author":{"username":"bob"}}]}
	]`
	srv := reviewsServer(t, "headsha1", discussions)
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	reviews, err := c.PullRequestReviews(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatalf("PullRequestReviews: %v", err)
	}
	if len(reviews) != 0 {
		t.Errorf("reviews = %+v, want none: a resolved thread carries no standing decision", reviews)
	}
}

func TestGitLabPullRequestReviewsSkipsNonResolvableComments(t *testing.T) {
	discussions := `[
		{"id":"d1","notes":[{"id":2,"body":"nice work","system":false,"resolvable":false,"author":{"username":"bob"}}]}
	]`
	srv := reviewsServer(t, "headsha1", discussions)
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	reviews, err := c.PullRequestReviews(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatalf("PullRequestReviews: %v", err)
	}
	if len(reviews) != 0 {
		t.Errorf("reviews = %+v, want none: a plain comment isn't a decision", reviews)
	}
}

func TestGitLabPullRequestReviewsSkipsOtherSystemNotes(t *testing.T) {
	discussions := `[
		{"id":"d1","notes":[{"id":2,"body":"requested review from @bob","system":true,"author":{"username":"alice"}}]}
	]`
	srv := reviewsServer(t, "headsha1", discussions)
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	reviews, err := c.PullRequestReviews(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatalf("PullRequestReviews: %v", err)
	}
	if len(reviews) != 0 {
		t.Errorf("reviews = %+v, want none: an unrelated system note isn't a decision", reviews)
	}
}

func TestGitLabPullRequestReviewsMarksBotReviewer(t *testing.T) {
	discussions := `[
		{"id":"d1","notes":[{"id":1,"body":"approved this merge request","system":true,"author":{"username":"talooner-bot","bot":true}}]}
	]`
	srv := reviewsServer(t, "headsha1", discussions)
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	reviews, err := c.PullRequestReviews(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatalf("PullRequestReviews: %v", err)
	}
	if len(reviews) != 1 || !reviews[0].Bot {
		t.Errorf("reviews = %+v, want Bot=true", reviews)
	}
}

func TestGitLabPullRequestReviewsIgnoresReplyNotesInAThread(t *testing.T) {
	discussions := `[
		{"id":"d1","notes":[
			{"id":2,"body":"please fix this","system":false,"resolvable":true,"resolved":false,"author":{"username":"bob"}},
			{"id":3,"body":"working on it","system":false,"resolvable":false,"author":{"username":"alice"}}
		]}
	]`
	srv := reviewsServer(t, "headsha1", discussions)
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	reviews, err := c.PullRequestReviews(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatalf("PullRequestReviews: %v", err)
	}
	if len(reviews) != 1 || reviews[0].Login != "bob" {
		t.Errorf("reviews = %+v, want only the thread-opening note's decision", reviews)
	}
}

func TestGitLabPullRequestReviewsSkipsEmptyDiscussions(t *testing.T) {
	srv := reviewsServer(t, "headsha1", `[{"id":"d1","notes":[]}]`)
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	reviews, err := c.PullRequestReviews(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatalf("PullRequestReviews: %v", err)
	}
	if len(reviews) != 0 {
		t.Errorf("reviews = %+v, want none", reviews)
	}
}

func TestGitLabPullRequestReviewsRejectsNonPositiveNumber(t *testing.T) {
	c, err := New(testToken)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.PullRequestReviews(context.Background(), "o", "r", 0); err == nil {
		t.Error("PullRequestReviews with number 0: want error, got nil")
	}
}

var _ host.Source = (*Client)(nil)

func TestGitLabPullRequestReviewsPropagatesHeadSHALookupFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv, WithMaxRetries(1))
	if _, err := c.PullRequestReviews(context.Background(), "o", "r", 1); err == nil {
		t.Fatal("PullRequestReviews: want error, got nil")
	}
}
