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

// config.yaml is parsed as data only (issue #20): it must never grow a place a
// secret looks at home. A field shaped like a credential is rejected by name,
// at any depth, even one this build does not otherwise recognise.
func TestParseRejectsCredentialFields(t *testing.T) {
	for _, data := range [][]byte{
		[]byte("api_key: xyz\n"),
		[]byte("token: xyz\n"),
		[]byte("checks:\n  password: xyz\n"),
		[]byte("nested:\n  deeper:\n    auth_secret: xyz\n"),
	} {
		if _, err := Parse(data); err == nil {
			t.Errorf("Parse(%q) = nil error, want one naming the credential-shaped field", data)
		}
	}
}

// A field that merely mentions an unrelated word must not be rejected —
// TestParseEmptyIsValid already pins "something_else: foo: bar" as tolerated.
func TestParseToleratesOrdinaryUnknownFields(t *testing.T) {
	if _, err := Parse([]byte("checks:\n  tests: [\"test\"]\nnotes: \"reviewed by security\"\n")); err != nil {
		t.Errorf("Parse: %v, want nil: an ordinary unknown field is not a credential", err)
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

// A module path is compared as a prefix against a changed file's path; it is
// never dereferenced on disk. ".." still has no meaning there, so it can only
// be a mistake or a probe, and issue #20 says to reject it outright.
func TestParseModulesRejectsPathEscape(t *testing.T) {
	for _, data := range [][]byte{
		[]byte("- path: ../../etc\n  owner: \"@alice\"\n"),
		[]byte("- path: internal/../../etc\n"),
		[]byte("- path: /etc/passwd\n"),
		[]byte("- path: \"\"\n"),
	} {
		if _, err := ParseModules(data); err == nil {
			t.Errorf("ParseModules(%q) = nil error, want a rejected path", data)
		}
	}
}

func TestParseModulesRejectsCredentialFields(t *testing.T) {
	if _, err := ParseModules([]byte("- path: internal/\n  api_token: xyz\n")); err == nil {
		t.Error("ParseModules = nil error, want one naming the credential-shaped field")
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

func TestParseTeamsRejectsCredentialFields(t *testing.T) {
	if _, err := ParseTeams([]byte("webhook_secret: xyz\n")); err == nil {
		t.Error("ParseTeams = nil error, want one naming the credential-shaped field")
	}
}

// architecture.yaml overrides or extends the built-in layer conventions: a
// path prefix, its kind, and the doc it should be checked against.
func TestParseArchitecture(t *testing.T) {
	rules, err := ParseArchitecture([]byte(`
- path: app/services/orders_service.rb
  kind: service
  doc_ref: docs/services/orders.md
- path: legacy/
  kind: model
`))
	if err != nil {
		t.Fatalf("ParseArchitecture: %v", err)
	}
	if len(rules) != 2 {
		t.Fatalf("rules = %d, want 2", len(rules))
	}
	if rules[0].Path != "app/services/orders_service.rb" || rules[0].Kind != "service" || rules[0].DocRef != "docs/services/orders.md" {
		t.Errorf("rule[0] = %+v, want the orders entry verbatim", rules[0])
	}
	if rules[1].DocRef != "" {
		t.Errorf("rule[1].DocRef = %q, want empty (no doc override declared)", rules[1].DocRef)
	}

	if _, err := ParseArchitecture([]byte("path: [\n")); err == nil {
		t.Error("ParseArchitecture returned nil error for malformed YAML, want one")
	}
}

func TestParseArchitectureRejectsUnknownKind(t *testing.T) {
	if _, err := ParseArchitecture([]byte("- path: internal/\n  kind: repository\n")); err == nil {
		t.Error("ParseArchitecture = nil error, want one naming the invalid kind")
	}
}

func TestParseArchitectureRejectsPathEscape(t *testing.T) {
	if _, err := ParseArchitecture([]byte("- path: ../../etc\n  kind: model\n")); err == nil {
		t.Error("ParseArchitecture = nil error, want a rejected path")
	}
}

func TestParseArchitectureRejectsCredentialFields(t *testing.T) {
	if _, err := ParseArchitecture([]byte("- path: internal/\n  kind: service\n  api_token: xyz\n")); err == nil {
		t.Error("ParseArchitecture = nil error, want one naming the credential-shaped field")
	}
}
