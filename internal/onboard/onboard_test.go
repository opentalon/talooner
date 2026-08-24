package onboard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteFileCreatesNewFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "talooner.yml")
	outcome, diff, err := WriteFile(path, []byte("hello\n"), false)
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if outcome != Created {
		t.Errorf("outcome = %v, want Created", outcome)
	}
	if diff != "" {
		t.Errorf("diff = %q, want empty on create", diff)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "hello\n" {
		t.Errorf("file content = %q, want %q", got, "hello\n")
	}
}

func TestWriteFileIdenticalContentIsUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "talooner.yml")
	if err := os.WriteFile(path, []byte("hello\n"), 0644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	outcome, _, err := WriteFile(path, []byte("hello\n"), false)
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if outcome != Unchanged {
		t.Errorf("outcome = %v, want Unchanged", outcome)
	}
}

func TestWriteFileDifferentContentIsConflictWithoutForce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "talooner.yml")
	if err := os.WriteFile(path, []byte("mine\n"), 0644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	outcome, diff, err := WriteFile(path, []byte("starter\n"), false)
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if outcome != Conflict {
		t.Errorf("outcome = %v, want Conflict", outcome)
	}
	if !strings.Contains(diff, "-mine") || !strings.Contains(diff, "+starter") {
		t.Errorf("diff = %q, want it to show both sides", diff)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "mine\n" {
		t.Errorf("file was clobbered without --force: content = %q", got)
	}
}

func TestWriteFileForceOverwritesConflict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "talooner.yml")
	if err := os.WriteFile(path, []byte("mine\n"), 0644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	outcome, _, err := WriteFile(path, []byte("starter\n"), true)
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if outcome != Created {
		t.Errorf("outcome = %v, want Created", outcome)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "starter\n" {
		t.Errorf("file content = %q, want it overwritten", got)
	}
}

func TestEmbeddedTemplatesAreNonEmpty(t *testing.T) {
	for name, content := range map[string][]byte{
		"Workflow":    Workflow,
		"Ruleset":     Ruleset,
		"RulesetTest": RulesetTest,
	} {
		if len(content) == 0 {
			t.Errorf("%s is empty", name)
		}
	}
}

func TestWorkflowReferencesTheActionAndSecrets(t *testing.T) {
	for _, want := range []string{"opentalon/talooner@v1", "OPENTALON_HOST", "OPENTALON_API_KEY", "pull-requests: write", "checks: write"} {
		if !strings.Contains(string(Workflow), want) {
			t.Errorf("Workflow template missing %q", want)
		}
	}
}

func TestRulesetDoesNotDeclareStrictOrNotify(t *testing.T) {
	// strict is the plugin's own base ruleset; declaring the same rule name in
	// a tenant ruleset is a compile error. notify has no executor yet, and a
	// fired notify action fails the whole run. Neither belongs in a starter a
	// tenant is meant to run unmodified.
	if strings.Contains(string(Ruleset), "strict rule") {
		t.Error("starter ruleset declares a strict rule, which collides with the plugin's own base ruleset")
	}
	if strings.Contains(string(Ruleset), `do notify "`) {
		t.Error("starter ruleset uses do notify, which has no executor and fails the whole run")
	}
}
