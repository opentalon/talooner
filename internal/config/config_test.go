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
