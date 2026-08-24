// Package config is the tenant's .github/talooner/config.yaml, read from the
// base branch at its own ref so a fork PR cannot swap the policy it is reviewed
// under (architecture.md, "Fork safety"). It is the same trust boundary as the
// ruleset: both come from the maintainers' base branch, never the head.
//
// C3 landed the checks section here. modules.yaml and teams.yaml (C6/E1) are
// read from the same directory by their own tickets; this file is the spot they
// hang off of.
package config

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Checks are the tenant-declared name patterns that decide which of the head
// sha's CI a pr.tests_passing / pr.lint_passing fact reads. A pattern is a
// case-insensitive wildcard: "*" matches any run of characters, including a
// slash, so "ci/*" catches "ci/build" and "*unit*" catches "my-unit-tests".
// An empty list for either means "no checks named" — the matching fact is then
// left unset, the safe answer for a repo that has not declared one (facts.md,
// "tests_passing / lint_passing").
type Checks struct {
	Tests []string `yaml:"tests"`
	Lint  []string `yaml:"lint"`
}

// Config is the whole file. Only checks is parsed today; the rest is reserved
// for the C6/E1 loaders, which will widen this struct without moving it.
type Config struct {
	Checks Checks `yaml:"checks"`
}

// Parse decodes config.yaml. A file with no checks key (or no checks section at
// all) is a valid, empty config — a tenant that has not declared patterns gets
// no tests_passing / lint_passing facts, not an error.
func Parse(data []byte) (Config, error) {
	if err := rejectCredentialFields(data); err != nil {
		return Config{}, err
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return Config{}, err
	}
	return c, nil
}

// credentialFieldNames are substrings that mark a YAML key as credential-shaped.
// .github/talooner/*.yaml is parsed as data only (issue #20, "no credential
// fields") — it must never grow a place a secret looks at home, so a field
// shaped like one is refused by name before anything reads it.
var credentialFieldNames = []string{"token", "secret", "password", "credential", "apikey", "api_key", "auth"}

// rejectCredentialFields fails the parse when any key, at any depth, looks like
// a credential. It walks the raw document rather than the typed struct so it
// also catches an unrecognised field — a field this build ignores today is
// exactly the one a later reader would trust without anyone re-checking it.
func rejectCredentialFields(data []byte) error {
	var raw any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return err
	}
	return walkFieldNames(raw)
}

func walkFieldNames(v any) error {
	switch t := v.(type) {
	case map[string]any:
		for k, vv := range t {
			if suspiciousFieldName(k) {
				return fmt.Errorf("field %q is not allowed: looks like a credential", k)
			}
			if err := walkFieldNames(vv); err != nil {
				return err
			}
		}
	case []any:
		for _, vv := range t {
			if err := walkFieldNames(vv); err != nil {
				return err
			}
		}
	}
	return nil
}

func suspiciousFieldName(k string) bool {
	lk := strings.ToLower(k)
	for _, bad := range credentialFieldNames {
		if strings.Contains(lk, bad) {
			return true
		}
	}
	return false
}
