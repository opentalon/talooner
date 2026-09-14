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
	apiVersion     = "2022-11-28"
	maxBodyBytes   = 16 << 20
	maxPages       = 100
	perPage        = 100
)

var (
	ErrNotFound    = errors.New("not found")
	ErrRateLimited = errors.New("rate limited")
	ErrServer      = errors.New("server error")
)

type APIError struct {
	Method     string
	URL        string
	StatusCode int
	Message    string
	kind       error
}

func (e *APIError) Error() string {
	s := fmt.Sprintf("%s %s: %d %s", e.Method, e.URL, e.StatusCode, http.StatusText(e.StatusCode))
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

func (e *APIError) Unwrap() error { return e.kind }

type Client struct {
	baseURL    *url.URL
	token      string
	http       *http.Client
	log        *slog.Logger
	redactor   *Redactor
	maxRetries int
	maxWait    time.Duration

	sleep func(context.Context, time.Duration) error
	now   func() time.Time
}

type Option func(*Client)

func WithBaseURL(raw string) Option {
	return func(c *Client) {
		if u, err := url.Parse(raw); err == nil {
			c.baseURL = u
		}
	}
}

func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

func WithLogger(l *slog.Logger) Option { return func(c *Client) { c.log = l } }

func WithSecrets(secrets ...string) Option {
	return func(c *Client) { c.redactor = NewRedactor(append(secrets, c.token)...) }
}

func WithMaxRetries(n int) Option { return func(c *Client) { c.maxRetries = max(n, 0) } }

func WithMaxWait(d time.Duration) Option { return func(c *Client) { c.maxWait = d } }

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

type request struct {
	method string
	path   string
	query  url.Values
	body   []byte
}

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
		if ctx.Err() != nil {
			return nil, terminal, fmt.Errorf("%s %s: %w", req.method, u.String(), ctx.Err())
		}
		return nil, retryable, fmt.Errorf("%s %s: %w", req.method, u.String(), c.redactor.Error(err))
	}
	defer resp.Body.Close() //nolint:errcheck

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
	_ = json.Unmarshal(raw, &payload) //nolint:errcheck
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

func isRateLimited(resp *http.Response) bool {
	if resp.Header.Get("Retry-After") != "" &&
		(resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests) {
		return true
	}
	return resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0"
}

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
		req = request{method: http.MethodGet, path: next}
	}
}

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
