package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testToken = "ghs_0123456789abcdefghijklmnopqrstuvwxyz"

// newTestClient points a client at srv with the waits recorded rather than
// slept, so a backoff test costs no wall time.
func newTestClient(t *testing.T, srv *httptest.Server, opts ...Option) (*Client, *[]time.Duration) {
	t.Helper()
	var waits []time.Duration
	base := []Option{WithBaseURL(srv.URL), WithHTTPClient(srv.Client())}
	c, err := New(testToken, append(base, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.sleep = func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	}
	return c, &waits
}

func TestNewRejectsEmptyToken(t *testing.T) {
	for _, token := range []string{"", "   "} {
		if _, err := New(token); err == nil {
			t.Errorf("New(%q): want error, got nil", token)
		}
	}
}

func TestNewFromEnv(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	if _, err := NewFromEnv(); err == nil {
		t.Error("NewFromEnv with no token: want error, got nil")
	}

	t.Setenv("GITHUB_TOKEN", testToken)
	t.Setenv("GITHUB_API_URL", "https://ghe.example.com/api/v3")
	c, err := NewFromEnv()
	if err != nil {
		t.Fatalf("NewFromEnv: %v", err)
	}
	u, err := c.resolve("/repos/o/r", nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if want := "https://ghe.example.com/api/v3/repos/o/r"; u.String() != want {
		t.Errorf("resolve = %s, want %s", u, want)
	}
}

func TestRequestCarriesAuthAndVersion(t *testing.T) {
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(r.Context())
		_, _ = fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	if _, err := c.do(context.Background(), request{method: http.MethodGet, path: "/rate_limit"}, nil); err != nil {
		t.Fatalf("do: %v", err)
	}
	if want := "Bearer " + testToken; got.Header.Get("Authorization") != want {
		t.Errorf("Authorization = %q, want %q", got.Header.Get("Authorization"), want)
	}
	if got.Header.Get("X-GitHub-Api-Version") != apiVersion {
		t.Errorf("X-GitHub-Api-Version = %q, want %q", got.Header.Get("X-GitHub-Api-Version"), apiVersion)
	}
}

func TestRetriesServerErrorThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = fmt.Fprint(w, `{"permission":"admin"}`)
	}))
	defer srv.Close()

	c, waits := newTestClient(t, srv)
	ok, err := c.HasWriteAccess(context.Background(), "o", "r", "evgeny")
	if err != nil {
		t.Fatalf("HasWriteAccess: %v", err)
	}
	if !ok {
		t.Error("HasWriteAccess = false, want true")
	}
	if calls.Load() != 3 {
		t.Errorf("calls = %d, want 3", calls.Load())
	}
	if want := []time.Duration{time.Second, 2 * time.Second}; !equalDurations(*waits, want) {
		t.Errorf("waits = %v, want %v", *waits, want)
	}
}

func TestServerErrorGivesUpAfterMaxRetries(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv, WithMaxRetries(2))
	_, err := c.PullRequest(context.Background(), "o", "r", 7)
	if !errors.Is(err, ErrServer) {
		t.Fatalf("err = %v, want ErrServer", err)
	}
	if calls.Load() != 3 {
		t.Errorf("calls = %d, want 3 (one attempt plus two retries)", calls.Load())
	}
}

func TestClientErrorIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = fmt.Fprint(w, `{"message":"Validation Failed"}`)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	_, err := c.PullRequest(context.Background(), "o", "r", 7)
	if err == nil {
		t.Fatal("PullRequest: want error, got nil")
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1", calls.Load())
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("StatusCode = %d, want 422", apiErr.StatusCode)
	}
	if !strings.Contains(err.Error(), "Validation Failed") {
		t.Errorf("err = %v, want GitHub's own message in it", err)
	}
}

// A 403 that is a permissions failure carries no rate-limit headers, and must
// not be retried — the token will not grow permissions on the second try.
func TestForbiddenWithoutRateLimitHeadersIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprint(w, `{"message":"Resource not accessible by integration"}`)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	_, err := c.HasWriteAccess(context.Background(), "o", "r", "evgeny")
	if err == nil {
		t.Fatal("HasWriteAccess: want error, got nil")
	}
	if errors.Is(err, ErrRateLimited) {
		t.Errorf("err = %v, want a plain 403 rather than a rate limit", err)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1", calls.Load())
	}
}

func TestRateLimitWaitsWhenResetIsClose(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(now.Add(20*time.Second).Unix(), 10))
			w.WriteHeader(http.StatusForbidden)
			_, _ = fmt.Fprint(w, `{"message":"API rate limit exceeded"}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"permission":"write"}`)
	}))
	defer srv.Close()

	c, waits := newTestClient(t, srv, WithMaxWait(60*time.Second))
	c.now = func() time.Time { return now }

	ok, err := c.HasWriteAccess(context.Background(), "o", "r", "evgeny")
	if err != nil {
		t.Fatalf("HasWriteAccess: %v", err)
	}
	if !ok {
		t.Error("HasWriteAccess = false, want true")
	}
	if want := []time.Duration{20 * time.Second}; !equalDurations(*waits, want) {
		t.Errorf("waits = %v, want %v", *waits, want)
	}
}

func TestRateLimitIsTerminalWhenResetIsFarAway(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(now.Add(45*time.Minute).Unix(), 10))
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprint(w, `{"message":"API rate limit exceeded"}`)
	}))
	defer srv.Close()

	c, waits := newTestClient(t, srv, WithMaxWait(30*time.Second))
	c.now = func() time.Time { return now }

	_, err := c.HasWriteAccess(context.Background(), "o", "r", "evgeny")
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1: a limit that outlives the job must not be retried", calls.Load())
	}
	if len(*waits) != 0 {
		t.Errorf("waits = %v, want none", *waits)
	}
	if !strings.Contains(err.Error(), "45m") {
		t.Errorf("err = %v, want the reset distance in the message", err)
	}
}

// The secondary limit is a 429 (or a 403) with Retry-After and no rate-limit
// headers at all.
func TestSecondaryRateLimitUsesRetryAfter(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = fmt.Fprint(w, `{"permission":"read"}`)
	}))
	defer srv.Close()

	c, waits := newTestClient(t, srv)
	if _, err := c.HasWriteAccess(context.Background(), "o", "r", "evgeny"); err != nil {
		t.Fatalf("HasWriteAccess: %v", err)
	}
	if want := []time.Duration{7 * time.Second}; !equalDurations(*waits, want) {
		t.Errorf("waits = %v, want %v", *waits, want)
	}
}

// A reset stamp already in the past must not turn into a hot loop.
func TestRateLimitWaitFloorsAtOneSecond(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c := &Client{now: func() time.Time { return now }}
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("X-RateLimit-Reset", strconv.FormatInt(now.Add(-time.Hour).Unix(), 10))
	if got := c.rateLimitWait(resp); got != time.Second {
		t.Errorf("rateLimitWait = %v, want 1s", got)
	}
}

func TestContextCancellationStopsRetrying(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	c.sleep = func(ctx context.Context, _ time.Duration) error { return context.Canceled }

	_, err := c.PullRequest(context.Background(), "o", "r", 3)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1", calls.Load())
	}
}

func TestPaginationCollectsEveryPage(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("per_page") != strconv.Itoa(perPage) {
			t.Errorf("per_page = %q, want %d", r.URL.Query().Get("per_page"), perPage)
		}
		switch r.URL.Query().Get("page") {
		case "", "1":
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/r/pulls/9/files?per_page=100&page=2>; rel="next", <%s/x>; rel="last"`, srv.URL, srv.URL))
			_, _ = fmt.Fprint(w, `[{"filename":"a.go"},{"filename":"b.go"}]`)
		case "2":
			_, _ = fmt.Fprint(w, `[{"filename":"c.go"}]`)
		default:
			t.Errorf("unexpected page %q", r.URL.Query().Get("page"))
		}
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	files, err := c.ChangedFiles(context.Background(), "o", "r", 9)
	if err != nil {
		t.Fatalf("ChangedFiles: %v", err)
	}
	if want := []string{"a.go", "b.go", "c.go"}; strings.Join(files, ",") != strings.Join(want, ",") {
		t.Errorf("files = %v, want %v", files, want)
	}
}

// The whole point of the pagination rule: a later page failing must not hand
// back the pages that did arrive. A short list is a wrong review.
func TestPaginationFailsRatherThanTruncating(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/r/pulls/9/files?per_page=100&page=2>; rel="next"`, srv.URL))
		_, _ = fmt.Fprint(w, `[{"filename":"a.go"}]`)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv, WithMaxRetries(1))
	files, err := c.ChangedFiles(context.Background(), "o", "r", 9)
	if err == nil {
		t.Fatalf("ChangedFiles = %v, want an error", files)
	}
	if files != nil {
		t.Errorf("files = %v, want nil on error", files)
	}
	if !strings.Contains(err.Error(), "page 2") {
		t.Errorf("err = %v, want the failing page in the message", err)
	}
}

// A Link header pointing somewhere else is a way to make the client post the
// token to another host. Refuse it.
func TestPaginationRefusesForeignNextLink(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Link", `<https://evil.example.com/steal>; rel="next"`)
		_, _ = fmt.Fprint(w, `[{"filename":"a.go"}]`)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	_, err := c.ChangedFiles(context.Background(), "o", "r", 9)
	if err == nil {
		t.Fatal("ChangedFiles: want error, got nil")
	}
	if !strings.Contains(err.Error(), "refusing to follow") {
		t.Errorf("err = %v, want a refusal to leave the API host", err)
	}
}

func TestNextLink(t *testing.T) {
	for _, tt := range []struct {
		header string
		want   string
	}{
		{"", ""},
		{`<https://api.github.com/x?page=2>; rel="next"`, "https://api.github.com/x?page=2"},
		{`<https://api.github.com/x?page=9>; rel="last"`, ""},
		{`<https://api.github.com/x?page=1>; rel="prev", <https://api.github.com/x?page=3>; rel="next"`, "https://api.github.com/x?page=3"},
		{`<https://api.github.com/x?page=3>; rel="NEXT"`, "https://api.github.com/x?page=3"},
		{"garbage", ""},
		{"<unterminated; rel=\"next\"", ""},
	} {
		if got := nextLink(tt.header); got != tt.want {
			t.Errorf("nextLink(%q) = %q, want %q", tt.header, got, tt.want)
		}
	}
}

func TestResolveRejectsOtherHosts(t *testing.T) {
	c, err := New(testToken, WithBaseURL("https://api.github.com"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.resolve("http://api.github.com/x", nil); err == nil {
		t.Error("resolve over http on the api host: want error, got nil")
	}
	if _, err := c.resolve("https://api.github.com.evil.test/x", nil); err == nil {
		t.Error("resolve on a lookalike host: want error, got nil")
	}
	u, err := c.resolve("/repos/o/r", nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if u.String() != "https://api.github.com/repos/o/r" {
		t.Errorf("resolve = %s, want https://api.github.com/repos/o/r", u)
	}
}

func equalDurations(got, want []time.Duration) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
