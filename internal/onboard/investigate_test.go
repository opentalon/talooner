package onboard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestInvestigateDetectsLanguageAndLayout(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/thing\n")
	writeFile(t, filepath.Join(root, "README.md"), "# Thing\n\nA CLI that does the thing.\n")
	writeFile(t, filepath.Join(root, "CODEOWNERS"), "* @acme/team\n")
	writeFile(t, filepath.Join(root, ".github", "workflows", "ci.yml"), "name: ci\n")
	if err := os.Mkdir(filepath.Join(root, "internal"), 0755); err != nil {
		t.Fatalf("mkdir internal: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, "node_modules"), 0755); err != nil {
		t.Fatalf("mkdir node_modules: %v", err)
	}

	summary, err := Investigate(root)
	if err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	for _, want := range []string{"Go", "internal", "ci.yml", "CODEOWNERS present: true", "A CLI that does the thing"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary missing %q:\n%s", want, summary)
		}
	}
	if strings.Contains(summary, "node_modules") {
		t.Error("node_modules should be skipped, not listed as a top-level directory")
	}
}

func TestInvestigateHandlesEmptyRepo(t *testing.T) {
	root := t.TempDir()
	summary, err := Investigate(root)
	if err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if !strings.Contains(summary, "none found") {
		t.Errorf("expected 'none found' for an empty repo, got:\n%s", summary)
	}
}

func TestInvestigateMissingRootErrors(t *testing.T) {
	if _, err := Investigate(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Fatal("expected an error for a missing root")
	}
}
