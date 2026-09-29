package gitlab

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testToken = "glpat-0123456789abcdefghij"

const minimalMR = `{"iid":7,"sha":"abc123","source_branch":"feat","target_branch":"main","state":"opened"}`

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
	t.Setenv("GITLAB_TOKEN", "")
	t.Setenv("CI_API_V4_URL", "")
	t.Setenv("GITLAB_API_URL", "")
	if _, err := NewFromEnv(); err == nil {
		t.Error("NewFromEnv with no token: want error, got nil")
	}

	t.Setenv("GITLAB_TOKEN", testToken)
	t.Setenv("CI_API_V4_URL", "https://gitlab.example.com/api/v4")
	c, err := NewFromEnv()
	if err != nil {
		t.Fatalf("NewFromEnv: %v", err)
	}
	u, err := c.resolve("/projects/o%2Fr", nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if want := "https://gitlab.example.com/api/v4/projects/o%2Fr"; u.String() != want {
		t.Errorf("resolve = %s, want %s", u, want)
	}
}

// GITLAB_API_URL must win over CI_API_V4_URL: it's the explicit override for
// non-CI use, CI_API_V4_URL is just GitLab's own ambient default.
func TestNewFromEnvExplicitURLWinsOverCIURL(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", testToken)
	t.Setenv("CI_API_V4_URL", "https://ci-instance.example.com/api/v4")
	t.Setenv("GITLAB_API_URL", "https://explicit.example.com/api/v4")
	c, err := NewFromEnv()
	if err != nil {
		t.Fatalf("NewFromEnv: %v", err)
	}
	u, err := c.resolve("/projects/o%2Fr", nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if want := "https://explicit.example.com/api/v4/projects/o%2Fr"; u.String() != want {
		t.Errorf("resolve = %s, want %s", u, want)
	}
}

func TestRequestCarriesPrivateToken(t *testing.T) {
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(r.Context())
		_, _ = fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	if _, err := c.do(context.Background(), request{method: http.MethodGet, path: "/version"}, nil); err != nil {
		t.Fatalf("do: %v", err)
	}
	if got.Header.Get("PRIVATE-TOKEN") != testToken {
		t.Errorf("PRIVATE-TOKEN = %q, want %q", got.Header.Get("PRIVATE-TOKEN"), testToken)
	}
	if got.Header.Get("Authorization") != "" {
		t.Errorf("Authorization = %q, want empty: gitlab uses PRIVATE-TOKEN, not bearer auth", got.Header.Get("Authorization"))
	}
}

func TestRetriesServerErrorThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = fmt.Fprint(w, minimalMR)
	}))
	defer srv.Close()

	c, waits := newTestClient(t, srv)
	pr, err := c.PullRequest(context.Background(), "o", "r", 7)
	if err != nil {
		t.Fatalf("PullRequest: %v", err)
	}
	if pr.Number != 7 {
		t.Errorf("Number = %d, want 7", pr.Number)
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
		t.Errorf("err = %v, want gitlab's own message in it", err)
	}
}

// GitLab's message field is sometimes a nested object/array on validation
// errors rather than a plain string; the error path must degrade to that
// raw JSON rather than failing to extract any message at all.
func TestErrorMessageToleratesNonStringPayload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"message":{"title":["can't be blank"]}}`)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	_, err := c.PullRequest(context.Background(), "o", "r", 7)
	if err == nil {
		t.Fatal("PullRequest: want error, got nil")
	}
	if !strings.Contains(err.Error(), "can't be blank") {
		t.Errorf("err = %v, want the nested message content in it", err)
	}
}

func TestRateLimitUsesRetryAfter(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = fmt.Fprint(w, minimalMR)
	}))
	defer srv.Close()

	c, waits := newTestClient(t, srv)
	if _, err := c.PullRequest(context.Background(), "o", "r", 7); err != nil {
		t.Fatalf("PullRequest: %v", err)
	}
	if want := []time.Duration{7 * time.Second}; !equalDurations(*waits, want) {
		t.Errorf("waits = %v, want %v", *waits, want)
	}
}

func TestRateLimitFallsBackToResetHeader(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("RateLimit-Reset", strconv.FormatInt(now.Add(20*time.Second).Unix(), 10))
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = fmt.Fprint(w, minimalMR)
	}))
	defer srv.Close()

	c, waits := newTestClient(t, srv, WithMaxWait(60*time.Second))
	c.now = func() time.Time { return now }

	if _, err := c.PullRequest(context.Background(), "o", "r", 7); err != nil {
		t.Fatalf("PullRequest: %v", err)
	}
	if want := []time.Duration{20 * time.Second}; !equalDurations(*waits, want) {
		t.Errorf("waits = %v, want %v", *waits, want)
	}
}

func TestRateLimitIsTerminalWhenResetIsFarAway(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "2700")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c, waits := newTestClient(t, srv, WithMaxWait(30*time.Second))
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
			w.Header().Set("Link", fmt.Sprintf(`<%s/projects/o%%2Fr/merge_requests/9/diffs?per_page=100&page=2>; rel="next"`, srv.URL))
			_, _ = fmt.Fprint(w, `[{"new_path":"a.go"},{"new_path":"b.go"}]`)
		case "2":
			_, _ = fmt.Fprint(w, `[{"new_path":"c.go"}]`)
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
		w.Header().Set("Link", fmt.Sprintf(`<%s/projects/o%%2Fr/merge_requests/9/diffs?per_page=100&page=2>; rel="next"`, srv.URL))
		_, _ = fmt.Fprint(w, `[{"new_path":"a.go"}]`)
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
		_, _ = fmt.Fprint(w, `[{"new_path":"a.go"}]`)
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

func TestResolveRejectsOtherHosts(t *testing.T) {
	c, err := New(testToken, WithBaseURL("https://gitlab.com/api/v4"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.resolve("http://gitlab.com/api/v4/x", nil); err == nil {
		t.Error("resolve over http on the api host: want error, got nil")
	}
	if _, err := c.resolve("https://gitlab.com.evil.test/x", nil); err == nil {
		t.Error("resolve on a lookalike host: want error, got nil")
	}
	u, err := c.resolve("/projects/o%2Fr", nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if u.String() != "https://gitlab.com/api/v4/projects/o%2Fr" {
		t.Errorf("resolve = %s, want https://gitlab.com/api/v4/projects/o%%2Fr", u)
	}
}

// The retry path logs the failing request. That log must not carry the
// token, and this is the case a new call site is most likely to get wrong.
func TestClientRetryLogHasNoToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = fmt.Fprintf(w, `{"message":"upstream said %s"}`, testToken)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	c, _ := newTestClient(t, srv,
		WithMaxRetries(1),
		WithLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))),
	)

	_, err := c.PullRequest(context.Background(), "o", "r", 1)
	if err == nil {
		t.Fatal("PullRequest: want error, got nil")
	}
	if strings.Contains(buf.String(), testToken) {
		t.Errorf("log = %q, want no token in it", buf.String())
	}
	if buf.Len() == 0 {
		t.Error("log is empty, want the retry recorded")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Errorf("err = %v, want no token in it", err)
	}
}

func TestWithSecretsKeepsTheToken(t *testing.T) {
	c, err := New(testToken, WithSecrets("another-long-secret-value"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got := c.redactor.String(testToken + " and another-long-secret-value")
	if strings.Contains(got, testToken) || strings.Contains(got, "another-long-secret-value") {
		t.Errorf("String = %q, want both secrets gone", got)
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
