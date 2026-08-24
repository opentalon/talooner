package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opentalon/opentalon/proto/pluginpb"
	"github.com/opentalon/talooner-plugin/proto/taloonerpb"

	"github.com/opentalon/talooner/internal/cluster"
	"github.com/opentalon/talooner/internal/credentials"
)

// withWhoami routes the whoami handshake Dial always performs to a fixed
// success, and everything else to respond — cluster.Dial never returns a
// client that hasn't handshaken, so any test dialling a fake plugin needs
// both.
func withWhoami(t *testing.T, respond func(*pluginpb.ToolCallRequest) *pluginpb.ToolResultResponse) func(*pluginpb.ToolCallRequest) *pluginpb.ToolResultResponse {
	t.Helper()
	whoami := whoamiOK(t)
	return func(req *pluginpb.ToolCallRequest) *pluginpb.ToolResultResponse {
		if req.GetAction() == cluster.ActionWhoami {
			return whoami(req)
		}
		return respond(req)
	}
}

func writeRuleset(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rules.tln"), []byte(body), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func seedRulesCreds(t *testing.T, host string) {
	t.Helper()
	path, err := credentials.DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath: %v", err)
	}
	if err := credentials.Save(path, credentials.Credentials{Host: host, APIKey: testKey}); err != nil {
		t.Fatalf("Save: %v", err)
	}
}

func TestRulesValidateRequiresPathArg(t *testing.T) {
	withHome(t)
	t.Chdir(t.TempDir())
	var out, errw bytes.Buffer
	code := run(context.Background(), []string{"rules", "validate"}, &out, &errw)
	if code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
}

func TestRulesValidateMissingFile(t *testing.T) {
	withHome(t)
	dir := t.TempDir()
	var out, errw bytes.Buffer
	code := run(context.Background(), []string{"rules", "validate", filepath.Join(dir, "nope")}, &out, &errw)
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(errw.String(), "rules.tln") {
		t.Errorf("stderr = %q, want it to name the ruleset file", errw.String())
	}
}

func TestRulesValidateNoStoredCredentials(t *testing.T) {
	withHome(t)
	dir := t.TempDir()
	writeRuleset(t, dir, "rule \"x\" { do approve }\n")
	var out, errw bytes.Buffer
	code := run(context.Background(), []string{"rules", "validate", dir}, &out, &errw)
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(errw.String(), "cluster login") {
		t.Errorf("stderr = %q, want it to point at `cluster login`", errw.String())
	}
}

func TestRulesValidateValid(t *testing.T) {
	withHome(t)
	host := serve(t, &fakePlugin{respond: withWhoami(t, structured(t, &taloonerpb.ValidateRulesetResponse{Valid: true}))})
	seedRulesCreds(t, host)
	dir := t.TempDir()
	writeRuleset(t, dir, "rule \"x\" { do approve }\n")

	var out, errw bytes.Buffer
	code := run(context.Background(), []string{"rules", "validate", dir}, &out, &errw)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, errw.String())
	}
	if !strings.Contains(out.String(), "is valid") {
		t.Errorf("stdout = %q, want it to say the ruleset is valid", out.String())
	}
	if errw.Len() != 0 {
		t.Errorf("stderr = %q, want empty on a valid ruleset", errw.String())
	}
}

func TestRulesValidateInvalidPrintsDiagnostics(t *testing.T) {
	withHome(t)
	host := serve(t, &fakePlugin{respond: withWhoami(t, structured(t, &taloonerpb.ValidateRulesetResponse{
		Valid: false,
		Diagnostics: []*taloonerpb.Diagnostic{
			{
				Severity: taloonerpb.Severity_SEVERITY_ERROR,
				Message:  `unknown verb "aprove", want one of: approve, block, comment, assign, require, emit`,
				Line:     3,
				Column:   8,
			},
		},
	}))})
	seedRulesCreds(t, host)
	dir := t.TempDir()
	writeRuleset(t, dir, "rule \"x\" { do aprove }\n")

	var out, errw bytes.Buffer
	code := run(context.Background(), []string{"rules", "validate", dir}, &out, &errw)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(errw.String(), "aprove") {
		t.Errorf("stderr = %q, want it to name the bad verb", errw.String())
	}
	if !strings.Contains(errw.String(), "approve, block") {
		t.Errorf("stderr = %q, want it to list the valid verbs", errw.String())
	}
	if !strings.Contains(errw.String(), "rules.tln:3:8") {
		t.Errorf("stderr = %q, want a path:line:column position", errw.String())
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want empty on an invalid ruleset", out.String())
	}
}

func TestRulesValidateClusterUnreachable(t *testing.T) {
	withHome(t)
	seedRulesCreds(t, "http://127.0.0.1:1")
	dir := t.TempDir()
	writeRuleset(t, dir, "rule \"x\" { do approve }\n")

	var out, errw bytes.Buffer
	code := run(context.Background(), []string{"rules", "validate", dir}, &out, &errw)
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(errw.String(), "cannot reach cluster") {
		t.Errorf("stderr = %q, want the unreachable-host message", errw.String())
	}
}
