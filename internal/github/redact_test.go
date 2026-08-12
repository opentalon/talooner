package github

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRedactorLiterals(t *testing.T) {
	r := NewRedactor("s3cr3t-cluster-key-value")
	got := r.String("posting to cluster with key s3cr3t-cluster-key-value now")
	if strings.Contains(got, "s3cr3t") {
		t.Errorf("String = %q, want the key gone", got)
	}
	if !strings.Contains(got, Placeholder) {
		t.Errorf("String = %q, want %s", got, Placeholder)
	}
}

// A value too short to be a secret would redact half the log if honoured.
func TestRedactorIgnoresShortLiterals(t *testing.T) {
	r := NewRedactor("a", "")
	if got := r.String("a plain sentence"); got != "a plain sentence" {
		t.Errorf("String = %q, want it untouched", got)
	}
}

func TestRedactorPatterns(t *testing.T) {
	for _, tt := range []struct {
		in      string
		redacts bool
	}{
		{"token ghs_0123456789abcdefghijkl leaked", true},
		{"token ghp_0123456789abcdefghijkl leaked", true},
		{"token github_pat_0123456789abcdefghijkl leaked", true},
		{"Authorization: Bearer 0123456789abcdefghijkl", true},
		// A commit sha is 40 hex characters and is logged on purpose.
		{"head sha 5f2e1c4a9b7d3e6f0a1b2c3d4e5f60718293a4b5", false},
		{"branch feat/b3-internal-github", false},
	} {
		got := NewRedactor().String(tt.in)
		if strings.Contains(got, Placeholder) != tt.redacts {
			t.Errorf("String(%q) = %q, redaction wanted: %v", tt.in, got, tt.redacts)
		}
	}
}

func TestRedactorError(t *testing.T) {
	r := NewRedactor(testToken)
	err := r.Error(fmt.Errorf("dial with %s failed", testToken))
	if strings.Contains(err.Error(), testToken) {
		t.Errorf("Error = %v, want the token gone", err)
	}
	if r.Error(nil) != nil {
		t.Error("Error(nil) = non-nil, want nil")
	}
}

// Unwrapping a redacted error must not hand back the unredacted original.
func TestRedactedErrorDoesNotUnwrap(t *testing.T) {
	inner := fmt.Errorf("leaking %s", testToken)
	err := NewRedactor(testToken).Error(inner)
	if errors.Unwrap(err) != nil {
		t.Errorf("Unwrap = %v, want nil", errors.Unwrap(err))
	}
	if errors.Is(err, inner) {
		t.Error("errors.Is reaches the unredacted error, want it not to")
	}
}

func TestRedactHandlerScrubsMessagesAndAttrs(t *testing.T) {
	var buf bytes.Buffer
	r := NewRedactor(testToken)
	log := slog.New(RedactHandler(slog.NewTextHandler(&buf, nil), r))

	log.With("preset", testToken).WithGroup("req").Info(
		"calling with "+testToken,
		"header", "Bearer "+testToken,
		"err", fmt.Errorf("boom: %s", testToken),
		"status", 500,
		"nested", slog.GroupValue(slog.String("token", testToken)),
	)

	out := buf.String()
	if strings.Contains(out, testToken) {
		t.Errorf("log = %q, want no token anywhere in it", out)
	}
	if !strings.Contains(out, "status=500") {
		t.Errorf("log = %q, want the int attribute intact", out)
	}
}

// The retry path logs the failing request. That log must not carry the token,
// and this is the case a new call site is most likely to get wrong.
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

// WithSecrets must not lose the token the client was built with.
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
