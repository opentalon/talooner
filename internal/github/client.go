// Package github is the REST client Talooner makes every GitHub call through.
//
// It authenticates with the GITHUB_TOKEN the Actions runtime mints for the job,
// which is scoped to the one repo and dies with the job (auth.md, "GitHub
// auth"). What the token may do is decided by the workflow's permissions block,
// not by this package declining to call an endpoint.
//
// Three behaviours here are load-bearing rather than incidental:
//
//   - Pagination is followed to the end or the call fails. A short
//     pr.changed_files list is a wrong review, not a degraded one, so a page
//     that errors takes the whole call down instead of returning what arrived.
//   - A rate limit waits only if the reset is close. Past that it is a terminal
//     error: a retry loop that outlives the job just burns a runner.
//   - Secrets are filtered on the way to the log by the handler, not by every
//     call site remembering to (auth.md, "Redaction on the log path").
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/opentalon/talooner/internal/version"
)

const (
	defaultBaseURL = "https://api.github.com"
	// apiVersion pins the REST schema; GitHub changes shapes behind this header.
	apiVersion = "2022-11-28"
	// maxBodyBytes caps a response read. The largest thing Talooner fetches is a
	// page of file patches.
	maxBodyBytes = 16 << 20
	// maxPages bounds pagination so a cyclic Link header cannot spin forever.
	maxPages = 100
	// perPage is the API maximum, so the common PR needs one request.
	perPage = 100
)

var (
	// ErrNotFound is a 404. For some calls that is an answer rather than a
	// failure — a login that is not a collaborator, say — so it is a sentinel.
	ErrNotFound = errors.New("not found")
	// ErrRateLimited is a primary or secondary rate limit whose reset is further
	// away than the client is willing to wait. Terminal.
	ErrRateLimited = errors.New("rate limited")
	// ErrServer is a 5xx that survived every retry.
	ErrServer = errors.New("server error")
)

// APIError is a GitHub response that is not a success.
type APIError struct {
	Method     string
	URL        string
	StatusCode int
	Message    string // GitHub's own "message" field, redacted and truncated
	kind       error  // ErrNotFound, ErrRateLimited, ErrServer, or nil
}

func (e *APIError) Error() string {
	s := fmt.Sprintf("%s %s: %d %s", e.Method, e.URL, e.StatusCode, http.StatusText(e.StatusCode))
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

// Unwrap exposes the sentinel so callers can use errors.Is on the class of
// failure without matching status codes by hand.
func (e *APIError) Unwrap() error { return e.kind }

// Client is a GitHub REST client. The zero value is not usable; call New.
type Client struct {
	baseURL    *url.URL
	token      string
	http       *http.Client
	log        *slog.Logger
	redactor   *Redactor
	maxRetries int
	maxWait    time.Duration

	// Injection seams for the tests; no production caller sets them.
	sleep func(context.Context, time.Duration) error
	now   func() time.Time
}

// Option configures a Client.
type Option func(*Client)

// WithBaseURL points the client at another API root, such as a GitHub
// Enterprise host or a test server.
func WithBaseURL(raw string) Option {
	return func(c *Client) {
		if u, err := url.Parse(raw); err == nil {
			c.baseURL = u
		}
	}
}

// WithHTTPClient replaces the underlying transport.
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// WithLogger sets the destination for request logs. The logger is wrapped in a
// redacting handler, so a caller cannot opt out of redaction by passing its own.
func WithLogger(l *slog.Logger) Option { return func(c *Client) { c.log = l } }

// WithSecrets registers extra values — the cluster key, for one — to strip from
// logs and error messages. The token is registered by New.
func WithSecrets(secrets ...string) Option {
	return func(c *Client) { c.redactor = NewRedactor(append(secrets, c.token)...) }
}

// WithMaxRetries caps how many times a retryable failure is tried again. Zero
// means one attempt and no retry.
func WithMaxRetries(n int) Option { return func(c *Client) { c.maxRetries = max(n, 0) } }

// WithMaxWait caps a single rate-limit wait. A reset further out than this is a
// terminal error instead.
func WithMaxWait(d time.Duration) Option { return func(c *Client) { c.maxWait = d } }

// New returns a client authenticating as token.
func New(token string, opts ...Option) (*Client, error) {
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("github token is empty")
	}
	base, err := url.Parse(defaultBaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse default base url %s: %w", defaultBaseURL, err)
	}
	c := &Client{
		baseURL:    base,
		token:      token,
		http:       &http.Client{Timeout: 30 * time.Second},
		log:        slog.New(slog.DiscardHandler),
		redactor:   NewRedactor(token),
		maxRetries: 3,
		maxWait:    60 * time.Second,
		sleep:      sleepCtx,
		now:        time.Now,
	}
	for _, opt := range opts {
		opt(c)
	}
	if c.baseURL == nil {
		return nil, errors.New("base url is empty")
	}
	c.log = slog.New(RedactHandler(c.log.Handler(), c.redactor))
	return c, nil
}

// NewFromEnv builds a client from what the Actions runtime sets: GITHUB_TOKEN
// for auth and GITHUB_API_URL for the host, which is what makes the same binary
// work on GitHub Enterprise without a flag. Options passed here win over both.
func NewFromEnv(opts ...Option) (*Client, error) {
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return nil, errors.New("GITHUB_TOKEN is not set")
	}
	env := []Option{}
	if api := os.Getenv("GITHUB_API_URL"); api != "" {
		env = append(env, WithBaseURL(api))
	}
	return New(token, append(env, opts...)...)
}

// request is one call, kept as a value so a retry can replay it — including the
// body, which is why it is bytes and not an io.Reader.
type request struct {
	method string
	// path is either a path relative to the base URL or an absolute URL from a
	// Link header. Absolute URLs pointing at another host are refused: following
	// one would hand the token to whoever wrote the redirect.
	path  string
	query url.Values
	body  []byte
}

// do performs req with retries and decodes a successful body into out, which
// may be nil. It returns the response headers, which is how pagination reads
// the Link header.
func (c *Client) do(ctx context.Context, req request, out any) (http.Header, error) {
	u, err := c.resolve(req.path, req.query)
	if err != nil {
		return nil, err
	}

	var lastErr error
	for attempt := 0; ; attempt++ {
		header, retryIn, err := c.attempt(ctx, req, u, out)
		if err == nil {
			return header, nil
		}
		lastErr = err
		if retryIn < 0 || attempt >= c.maxRetries {
			return nil, lastErr
		}
		if retryIn == 0 {
			retryIn = c.backoff(attempt)
		}
		c.log.Warn("github request failed, retrying",
			"method", req.method, "url", u.String(), "attempt", attempt+1,
			"retry_in", retryIn.String(), "err", lastErr)
		if err := c.sleep(ctx, retryIn); err != nil {
			return nil, fmt.Errorf("%s %s: %w", req.method, u.String(), err)
		}
	}
}

// attempt makes one HTTP call. The second return value says what to do next:
// terminal for a failure that will not improve, retryable to let the caller pick
// a backoff, or a positive duration the server itself asked for.
func (c *Client) attempt(ctx context.Context, req request, u *url.URL, out any) (http.Header, time.Duration, error) {
	const (
		terminal  = -1 * time.Second
		retryable = 0 * time.Second
	)

	var body io.Reader
	if len(req.body) > 0 {
		body = bytes.NewReader(req.body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, req.method, u.String(), body)
	if err != nil {
		return nil, terminal, fmt.Errorf("build request %s %s: %w", req.method, u.String(), c.redactor.Error(err))
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.token)
	httpReq.Header.Set("Accept", "application/vnd.github+json")
	httpReq.Header.Set("X-GitHub-Api-Version", apiVersion)
	httpReq.Header.Set("User-Agent", "talooner/"+version.Version)
	if len(req.body) > 0 {
		httpReq.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		// A transport failure is retryable unless the context is done: a
		// cancelled run must not keep dialling.
		if ctx.Err() != nil {
			return nil, terminal, fmt.Errorf("%s %s: %w", req.method, u.String(), ctx.Err())
		}
		return nil, retryable, fmt.Errorf("%s %s: %w", req.method, u.String(), c.redactor.Error(err))
	}
	defer resp.Body.Close() //nolint:errcheck // read-only

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, retryable, fmt.Errorf("read body of %s %s: %w", req.method, u.String(), c.redactor.Error(err))
	}

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		if out != nil {
			if err := json.Unmarshal(raw, out); err != nil {
				return nil, terminal, fmt.Errorf("decode body of %s %s: %w", req.method, u.String(), c.redactor.Error(err))
			}
		}
		return resp.Header, terminal, nil

	case isRateLimited(resp):
		wait := c.rateLimitWait(resp)
		apiErr := c.apiError(req.method, u, resp, raw, ErrRateLimited)
		if wait > c.maxWait {
			return nil, terminal, fmt.Errorf("%w: reset is %s away, longer than the %s this run will wait",
				apiErr, wait.Round(time.Second), c.maxWait)
		}
		return nil, wait, apiErr

	case resp.StatusCode >= 500:
		return nil, retryable, c.apiError(req.method, u, resp, raw, ErrServer)

	case resp.StatusCode == http.StatusNotFound:
		return nil, terminal, c.apiError(req.method, u, resp, raw, ErrNotFound)

	default:
		return nil, terminal, c.apiError(req.method, u, resp, raw, nil)
	}
}

// backoff is the wait before retry number n, exponential from one second. No
// jitter: one job makes these calls, there is no herd to spread out.
func (c *Client) backoff(n int) time.Duration {
	d := time.Second << n
	if d > c.maxWait {
		d = c.maxWait
	}
	return d
}

func (c *Client) apiError(method string, u *url.URL, resp *http.Response, raw []byte, kind error) *APIError {
	var payload struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(raw, &payload) //nolint:errcheck // a non-JSON error body just means no message
	msg := c.redactor.String(payload.Message)
	if len(msg) > 200 {
		msg = msg[:200] + "…"
	}
	return &APIError{
		Method:     method,
		URL:        u.String(),
		StatusCode: resp.StatusCode,
		Message:    msg,
		kind:       kind,
	}
}

// isRateLimited reports whether resp is GitHub saying "slow down". A primary
// limit is a 403 with no requests remaining; a secondary limit is a 403 or 429
// carrying Retry-After. A 403 with neither is a permissions failure, which must
// not be retried.
func isRateLimited(resp *http.Response) bool {
	if resp.Header.Get("Retry-After") != "" &&
		(resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests) {
		return true
	}
	return resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0"
}

// rateLimitWait is how long resp says to wait. Always at least a second, so a
// reset already in the past does not turn into a hot loop.
func (c *Client) rateLimitWait(resp *http.Response) time.Duration {
	if v := resp.Header.Get("Retry-After"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil {
			return max(time.Duration(secs)*time.Second, time.Second)
		}
	}
	if v := resp.Header.Get("X-RateLimit-Reset"); v != "" {
		if unix, err := strconv.ParseInt(v, 10, 64); err == nil {
			return max(time.Unix(unix, 0).Sub(c.now()), time.Second)
		}
	}
	return time.Second
}

// resolve turns a request path into an absolute URL. An absolute path is only
// accepted when it stays on the base host: Link headers come from a response
// body's neighbourhood, and the token travels on the next request.
func (c *Client) resolve(path string, query url.Values) (*url.URL, error) {
	u, err := url.Parse(path)
	if err != nil {
		return nil, fmt.Errorf("parse request path %s: %w", path, err)
	}
	if u.IsAbs() {
		if u.Host != c.baseURL.Host || u.Scheme != c.baseURL.Scheme {
			return nil, fmt.Errorf("refusing to follow %s: host is not %s", u.Redacted(), c.baseURL.Host)
		}
	} else {
		u = c.baseURL.JoinPath(path)
	}
	if len(query) > 0 {
		q := u.Query()
		for k, vs := range query {
			for _, v := range vs {
				q.Set(k, v)
			}
		}
		u.RawQuery = q.Encode()
	}
	return u, nil
}

// paginate collects every page of a list endpoint. Any page failing fails the
// whole call: a truncated list asserted as complete is a wrong answer, and the
// caller has no way to tell one from the other.
func paginate[T any](ctx context.Context, c *Client, path string, query url.Values) ([]T, error) {
	if query == nil {
		query = url.Values{}
	}
	query.Set("per_page", strconv.Itoa(perPage))

	var all []T
	req := request{method: http.MethodGet, path: path, query: query}
	for page := 1; ; page++ {
		if page > maxPages {
			return nil, fmt.Errorf("%s returned more than %d pages, refusing to keep paging", path, maxPages)
		}
		var batch []T
		header, err := c.do(ctx, req, &batch)
		if err != nil {
			return nil, fmt.Errorf("page %d of %s: %w", page, path, err)
		}
		all = append(all, batch...)

		next := nextLink(header.Get("Link"))
		if next == "" {
			return all, nil
		}
		// The next URL already carries its own query, including per_page.
		req = request{method: http.MethodGet, path: next}
	}
}

// nextLink returns the rel="next" URL from a Link header, or "".
func nextLink(header string) string {
	for _, part := range strings.Split(header, ",") {
		segments := strings.Split(strings.TrimSpace(part), ";")
		if len(segments) < 2 {
			continue
		}
		raw := strings.TrimSpace(segments[0])
		if !strings.HasPrefix(raw, "<") || !strings.HasSuffix(raw, ">") {
			continue
		}
		for _, seg := range segments[1:] {
			if strings.EqualFold(strings.TrimSpace(seg), `rel="next"`) {
				return raw[1 : len(raw)-1]
			}
		}
	}
	return ""
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
