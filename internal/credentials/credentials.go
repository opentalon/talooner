package credentials

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

var ErrNotFound = errors.New("no stored credentials")

type Credentials struct {
	Host   string `yaml:"host"`
	APIKey string `yaml:"api_key"`
}

func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, ".talooner", "credentials"), nil
}

func Load(path string) (Credentials, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Credentials{}, ErrNotFound
		}
		return Credentials{}, fmt.Errorf("reading %s: %w", path, err)
	}
	var c Credentials
	if err := yaml.Unmarshal(data, &c); err != nil {
		return Credentials{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	return c, nil
}

func Save(path string, c Credentials) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("encoding credentials: %w", err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}
