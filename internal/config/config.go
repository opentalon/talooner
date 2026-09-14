package config

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

type Checks struct {
	Tests []string `yaml:"tests"`
	Lint  []string `yaml:"lint"`
}

type Config struct {
	Checks Checks `yaml:"checks"`
}

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

var credentialFieldNames = []string{"token", "secret", "password", "credential", "apikey", "api_key", "auth"}

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
