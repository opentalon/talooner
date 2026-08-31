package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/opentalon/talooner/internal/credentials"
	"github.com/opentalon/talooner/internal/onboard"
)

// fakeGH scripts gh responses by subcommand and records every call's full
// args and stdin, so a test can assert exactly what init asked gh to do
// without shelling out to a real binary.
type fakeGH struct {
	fail  map[string]error
	calls [][]string
	stdin []string
}

func (f *fakeGH) Run(_ context.Context, stdin string, args ...string) (string, error) {
	f.calls = append(f.calls, append([]string(nil), args...))
	f.stdin = append(f.stdin, stdin)
	if len(args) > 0 {
		if err, ok := f.fail[args[0]]; ok {
			return "", err
		}
	}
	return "", nil
}

func seedCreds(t *testing.T) credentials.Credentials {
	t.Helper()
	withHome(t)
	path, err := credentials.DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath: %v", err)
	}
	creds := credentials.Credentials{Host: "grpc://talon.example.com:9090", APIKey: testKey}
	if err := credentials.Save(path, creds); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return creds
}

func TestInitRequiresRepoFlag(t *testing.T) {
	t.Chdir(t.TempDir())
	var out, errw bytes.Buffer
	code := runInit(context.Background(), nil, &out, &errw, &fakeGH{})
	if code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
	if !strings.Contains(errw.String(), "--repo") {
		t.Errorf("stderr = %q, want it to name --repo", errw.String())
	}
}

func TestInitRejectsMalformedRepo(t *testing.T) {
	t.Chdir(t.TempDir())
	var out, errw bytes.Buffer
	code := runInit(context.Background(), []string{"--repo", "not-owner-slash-name"}, &out, &errw, &fakeGH{})
	if code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
}

func TestInitNoStoredCredentials(t *testing.T) {
	withHome(t)
	t.Chdir(t.TempDir())
	var out, errw bytes.Buffer
	code := runInit(context.Background(), []string{"--repo", "acme/api"}, &out, &errw, &fakeGH{})
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(errw.String(), "cluster login") {
		t.Errorf("stderr = %q, want it to point at cluster login", errw.String())
	}
}

func TestInitHappyPathSetsSecretsOnly(t *testing.T) {
	creds := seedCreds(t)
	t.Chdir(t.TempDir())
	gh := &fakeGH{}
	var out, errw bytes.Buffer
	code := runInit(context.Background(), []string{"--repo", "acme/api"}, &out, &errw, gh)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, errw.String())
	}
	if strings.Contains(out.String(), creds.APIKey) || strings.Contains(errw.String(), creds.APIKey) {
		t.Error("API key leaked into stdout/stderr")
	}

	// init writes no local files at all — workflow, ruleset, and PR are all
	// `talooner onboard`'s job.
	for _, path := range []string{onboard.WorkflowPath, onboard.RulesetPath, onboard.RulesetTestPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("expected %s NOT to exist after init, got err=%v", path, err)
		}
	}

	if len(gh.calls) != 3 {
		t.Fatalf("gh calls = %v, want 3 (auth status + 2 secret sets)", gh.calls)
	}
	if gh.calls[0][0] != "auth" {
		t.Errorf("first gh call = %v, want auth status first", gh.calls[0])
	}
	wantStdin := map[string]bool{creds.Host: false, creds.APIKey: false}
	for _, s := range gh.stdin {
		if _, ok := wantStdin[s]; ok {
			wantStdin[s] = true
		}
	}
	for v, seen := range wantStdin {
		if !seen {
			t.Errorf("secret value %q never sent as gh stdin", v)
		}
	}
	for _, call := range gh.calls[1:] {
		for _, arg := range call {
			if arg == creds.APIKey {
				t.Errorf("secret value appeared in gh argv %v — must travel on stdin only", call)
			}
		}
	}
}

func TestInitOrgFlagSetsOrgSecretsWithVisibility(t *testing.T) {
	seedCreds(t)
	t.Chdir(t.TempDir())
	gh := &fakeGH{}
	var out, errw bytes.Buffer
	code := runInit(context.Background(), []string{"--repo", "acme/api", "--org", "acme"}, &out, &errw, gh)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, errw.String())
	}
	for _, call := range gh.calls[1:] {
		if !containsArg(call, "--org") || !containsArg(call, "--visibility") {
			t.Errorf("gh call = %v, want --org and --visibility", call)
		}
		if containsArg(call, "--repo") {
			t.Errorf("gh call = %v, must not target a repo when --org is set", call)
		}
	}
}

func TestInitGHNotAuthenticatedReportsWhichStepFailed(t *testing.T) {
	seedCreds(t)
	t.Chdir(t.TempDir())
	gh := &fakeGH{fail: map[string]error{"auth": errors.New("not logged in")}}
	var out, errw bytes.Buffer
	code := runInit(context.Background(), []string{"--repo", "acme/api"}, &out, &errw, gh)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(errw.String(), "not authenticated") {
		t.Errorf("stderr = %q, want a not-authenticated message", errw.String())
	}
}

func TestInitSecretSetFailureNamesTheSecret(t *testing.T) {
	seedCreds(t)
	t.Chdir(t.TempDir())
	gh := &fakeGH{fail: map[string]error{"secret": errors.New("HTTP 403")}}
	var out, errw bytes.Buffer
	code := runInit(context.Background(), []string{"--repo", "acme/api"}, &out, &errw, gh)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(errw.String(), "OPENTALON_HOST") {
		t.Errorf("stderr = %q, want it to name which secret failed", errw.String())
	}
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}
