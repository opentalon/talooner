package config

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

type Module struct {
	Path             string `yaml:"path"`
	DocumentationURL string `yaml:"documentation_url"`
	Owner            string `yaml:"owner"`
}

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

type ArchitectureRule struct {
	Path   string `yaml:"path"`
	Kind   string `yaml:"kind"`
	DocRef string `yaml:"doc_ref"`
}

var validKinds = map[string]bool{"model": true, "controller": true, "service": true}

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

type Teams map[string]string

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
