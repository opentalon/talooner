package onboard

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeRunner scripts gh responses by the subcommand (args[0]) it's called
// with, and records every stdin it was fed so a test can assert a secret
// value never rides in argv.
type fakeRunner struct {
	responses map[string]error
	stdins    []string
}

func (f *fakeRunner) Run(_ context.Context, stdin string, args ...string) (string, error) {
	f.stdins = append(f.stdins, stdin)
	if len(args) == 0 {
		return "", errors.New("no args")
	}
	if err, ok := f.responses[args[0]]; ok {
		return "", err
	}
	return "", nil
}

func TestCheckGHNotFound(t *testing.T) {
	r := &fakeRunner{responses: map[string]error{"auth": ErrGHNotFound}}
	err := CheckGH(context.Background(), r)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("CheckGH = %v, want a not-found error", err)
	}
}

func TestCheckGHNotAuthenticated(t *testing.T) {
	r := &fakeRunner{responses: map[string]error{"auth": errors.New("You are not logged into any GitHub hosts")}}
	err := CheckGH(context.Background(), r)
	if err == nil || !strings.Contains(err.Error(), "not authenticated") {
		t.Errorf("CheckGH = %v, want a not-authenticated error", err)
	}
}

func TestCheckGHOK(t *testing.T) {
	r := &fakeRunner{}
	if err := CheckGH(context.Background(), r); err != nil {
		t.Errorf("CheckGH = %v, want nil", err)
	}
}

func TestSetRepoSecretPassesValueOnStdinNotArgv(t *testing.T) {
	r := &fakeRunner{}
	if err := SetRepoSecret(context.Background(), r, "acme/api", "OPENTALON_API_KEY", "otk_super_secret"); err != nil {
		t.Fatalf("SetRepoSecret: %v", err)
	}
	if len(r.stdins) != 1 || r.stdins[0] != "otk_super_secret" {
		t.Errorf("stdin = %v, want the secret value piped in, never as an argument", r.stdins)
	}
}

func TestSetOrgSecretRequiresVisibility(t *testing.T) {
	var gotArgs []string
	r := &fakeRunnerCapture{fakeRunner: &fakeRunner{}, capture: &gotArgs}
	if err := SetOrgSecret(context.Background(), r, "acme", "OPENTALON_HOST", "grpc://x:9090"); err != nil {
		t.Fatalf("SetOrgSecret: %v", err)
	}
	if !containsAll(gotArgs, "--org", "acme", "--visibility", "all") {
		t.Errorf("args = %v, want --org acme --visibility all", gotArgs)
	}
}

func TestSetRepoSecretFailureIsNamed(t *testing.T) {
	r := &fakeRunner{responses: map[string]error{"secret": errors.New("HTTP 403: Resource not accessible")}}
	err := SetRepoSecret(context.Background(), r, "acme/api", "OPENTALON_HOST", "grpc://x:9090")
	if err == nil || !strings.Contains(err.Error(), "OPENTALON_HOST") || !strings.Contains(err.Error(), "acme/api") {
		t.Errorf("SetRepoSecret error = %v, want it to name the secret and the repo", err)
	}
}

// fakeRunnerCapture wraps fakeRunner to also record the full arg list of the
// last call, for asserting exact flags passed to gh.
type fakeRunnerCapture struct {
	*fakeRunner
	capture *[]string
}

func (f *fakeRunnerCapture) Run(ctx context.Context, stdin string, args ...string) (string, error) {
	*f.capture = args
	return f.fakeRunner.Run(ctx, stdin, args...)
}

func containsAll(haystack []string, needles ...string) bool {
	for _, n := range needles {
		found := false
		for _, h := range haystack {
			if h == n {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
