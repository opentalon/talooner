package config

import "gopkg.in/yaml.v3"

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
	var ms []Module
	if err := yaml.Unmarshal(data, &ms); err != nil {
		return nil, err
	}
	return ms, nil
}

// Teams is the logical-team → GitHub-team mapping of .github/talooner/teams.yaml
// (facts.md, "team.*"). A require target of review.<name> resolves through here
// when present, so a ruleset written before the file exists keeps working and a
// repo can redirect a logical team to any GitHub team.
type Teams map[string]string

// ParseTeams decodes teams.yaml. A present but unparseable file is a tenant error
// that fails the run; an empty file is a valid, empty map, not an error.
func ParseTeams(data []byte) (Teams, error) {
	var t Teams
	if err := yaml.Unmarshal(data, &t); err != nil {
		return nil, err
	}
	if t == nil {
		t = Teams{}
	}
	return t, nil
}
