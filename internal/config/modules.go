package config

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Module is one entry of .github/talooner/modules.yaml (facts.md, "module.*").
// path is a directory prefix the PR's changed files are matched against;
// documentation_url and owner are asserted as the module.* facts of the primary
// touched module.
type Module struct {
	Path             string `yaml:"path"`
	DocumentationURL string `yaml:"documentation_url"`
	Owner            string `yaml:"owner"`
}

// ParseModules decodes modules.yaml. A present but unparseable file is a tenant
// error that fails the run, the same shape as a broken config.yaml; an empty
// file is a valid, empty module set, not an error.
func ParseModules(data []byte) ([]Module, error) {
	if err := rejectCredentialFields(data); err != nil {
		return nil, err
	}
	var ms []Module
	if err := yaml.Unmarshal(data, &ms); err != nil {
		return nil, err
	}
	for _, m := range ms {
		if err := validateModulePath(m.Path); err != nil {
			return nil, err
		}
	}
	return ms, nil
}

// validateModulePath rejects a path that could escape the repository it is
// matched against. moduleOwns only ever compares it against a changed file's
// path — it is never dereferenced on disk — but config.yaml is tenant-editable
// data (issue #20), and ".." has no meaning as a prefix a changed file could
// ever match, so it can only be a mistake or a probe.
func validateModulePath(p string) error {
	if p == "" {
		return fmt.Errorf("module path is empty")
	}
	if strings.HasPrefix(p, "/") {
		return fmt.Errorf("module path %q must not be absolute", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return fmt.Errorf("module path %q escapes the repository", p)
		}
	}
	return nil
}

// ArchitectureRule is one entry of .github/talooner/architecture.yaml
// (facts.md, "code.*"). It overrides or extends the built-in per-language layer
// conventions architecture.go matches changed files against: path is a
// directory or file prefix, kind names which code.*_changed roll-up the
// matched files count toward, and doc_ref is the repo path fed to the LLM
// review as that unit's doc_ref (expert-review-system.md, "Key decisions" #3).
// doc_ref may be left empty — a unit still exists as a code_unit record with a
// kind, it simply carries no doc.
type ArchitectureRule struct {
	Path   string `yaml:"path"`
	Kind   string `yaml:"kind"`
	DocRef string `yaml:"doc_ref"`
}

// validKinds is the closed set of code_unit kinds a rule can name — the same
// three architecture.go's built-in conventions produce, so an override can
// never invent a roll-up the rest of the system does not know how to gate on.
var validKinds = map[string]bool{"model": true, "controller": true, "service": true}

// ParseArchitecture decodes architecture.yaml. A present but unparseable file,
// an escaping path, or a kind outside {model, controller, service} is a tenant
// error that fails the run, the same shape as modules.yaml; an empty file is a
// valid, empty rule set — no override, conventions alone decide.
func ParseArchitecture(data []byte) ([]ArchitectureRule, error) {
	if err := rejectCredentialFields(data); err != nil {
		return nil, err
	}
	var rs []ArchitectureRule
	if err := yaml.Unmarshal(data, &rs); err != nil {
		return nil, err
	}
	for _, r := range rs {
		if err := validateModulePath(r.Path); err != nil {
			return nil, err
		}
		if !validKinds[r.Kind] {
			return nil, fmt.Errorf("architecture rule %q has kind %q, want model, controller or service", r.Path, r.Kind)
		}
	}
	return rs, nil
}

// Teams is the logical-team → GitHub-team mapping of .github/talooner/teams.yaml
// (facts.md, "team.*"). A require target of review.<name> resolves through here
// when present, so a ruleset written before the file exists keeps working and a
// repo can redirect a logical team to any GitHub team.
type Teams map[string]string

// ParseTeams decodes teams.yaml. A present but unparseable file is a tenant error
// that fails the run; an empty file is a valid, empty map, not an error.
func ParseTeams(data []byte) (Teams, error) {
	if err := rejectCredentialFields(data); err != nil {
		return nil, err
	}
	var t Teams
	if err := yaml.Unmarshal(data, &t); err != nil {
		return nil, err
	}
	if t == nil {
		t = Teams{}
	}
	return t, nil
}
