package config

import "testing"

func TestParseReadsCheckPatterns(t *testing.T) {
	data := []byte(`
checks:
  tests: ["test", "ci/*", "*unit*"]
  lint: ["lint", "golangci-lint"]
`)
	c, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(c.Checks.Tests) != 3 || c.Checks.Tests[1] != "ci/*" {
		t.Errorf("tests = %v, want the three patterns", c.Checks.Tests)
	}
	if len(c.Checks.Lint) != 2 || c.Checks.Lint[1] != "golangci-lint" {
		t.Errorf("lint = %v, want the two patterns", c.Checks.Lint)
	}
}

// A file with no checks key is a valid, empty config: a tenant that has not
// declared patterns gets no tests_passing / lint_passing facts, not an error.
func TestParseEmptyIsValid(t *testing.T) {
	for _, data := range [][]byte{
		nil,
		[]byte(""),
		[]byte("checks: {}\n"),
		[]byte("something_else:\n  foo: bar\n"),
	} {
		c, err := Parse(data)
		if err != nil {
			t.Fatalf("Parse(%q): %v", data, err)
		}
		if len(c.Checks.Tests) != 0 || len(c.Checks.Lint) != 0 {
			t.Errorf("Parse(%q) = %+v, want empty checks", data, c.Checks)
		}
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	if _, err := Parse([]byte("\tnot: [valid\n")); err == nil {
		t.Error("Parse returned nil error for malformed YAML, want one")
	}
}

// modules.yaml maps a path prefix to a documentation URL and an owner. The owner
// may be empty, and the path keeps its trailing slash — that is how moduleOwns
// recognises a directory prefix.
func TestParseModules(t *testing.T) {
	modules, err := ParseModules([]byte(`
- path: internal/auth/
  documentation_url: https://docs/auth
  owner: "@alice"
- path: billing/
  documentation_url: https://docs/billing
  owner: "@org/payments"
- path: docs/
  documentation_url: https://docs/site
`))
	if err != nil {
		t.Fatalf("ParseModules: %v", err)
	}
	if len(modules) != 3 {
		t.Fatalf("modules = %d, want 3", len(modules))
	}
	if modules[0].Path != "internal/auth/" || modules[0].DocumentationURL != "https://docs/auth" || modules[0].Owner != "@alice" {
		t.Errorf("module[0] = %+v, want the auth entry verbatim", modules[0])
	}
	if modules[2].Owner != "" {
		t.Errorf("module[2].Owner = %q, want empty (docs has no owner)", modules[2].Owner)
	}

	if _, err := ParseModules([]byte("path: [\n")); err == nil {
		t.Error("ParseModules returned nil error for malformed YAML, want one")
	}
}

// teams.yaml is a flat logical-name → GitHub-team map. The value is trusted repo
// config, so it is passed through verbatim (including a cross-org "@org/team").
func TestParseTeams(t *testing.T) {
	teams, err := ParseTeams([]byte(`
senior_oncall: "@org/security"
payments: "@org/payments"
`))
	if err != nil {
		t.Fatalf("ParseTeams: %v", err)
	}
	if teams["senior_oncall"] != "@org/security" || teams["payments"] != "@org/payments" {
		t.Errorf("teams = %+v, want the two mapped teams", teams)
	}

	if _, err := ParseTeams([]byte("key: [\n")); err == nil {
		t.Error("ParseTeams returned nil error for malformed YAML, want one")
	}
}
