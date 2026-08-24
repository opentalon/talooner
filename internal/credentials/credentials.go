// Package credentials stores the operator's cluster host and API key locally,
// so `talooner cluster login` runs once and every later CLI command reads
// from disk instead of expecting OPENTALON_HOST / OPENTALON_API_KEY to be set
// on every invocation — those two env vars stay the Action's own way in
// (internal/cluster), untouched by this package.
package credentials

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// ErrNotFound means no credentials file exists at path. Distinct from a read
// or parse failure: nothing was ever saved, which is what tells a caller to
// say "run cluster login" rather than chase a nil dereference.
var ErrNotFound = errors.New("no stored credentials")

// Credentials is the whole file: the cluster host and API key `cluster login`
// wrote. Nothing else belongs here — widen it only for a field `cluster
// login` itself asks for.
type Credentials struct {
	Host   string `yaml:"host"`
	APIKey string `yaml:"api_key"`
}

// DefaultPath is ~/.talooner/credentials — this workspace's house convention
// for local CLI state (opentalon's own CLI keeps its state under
// ~/.opentalon; there is no XDG_CONFIG_HOME use anywhere in the workspace).
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, ".talooner", "credentials"), nil
}

// Load reads Credentials from path. A missing file is ErrNotFound, not a bare
// os.IsNotExist a caller has to know to check for.
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

// Save writes c to path, creating its parent directory if needed. Both the
// directory and the file are created owner-only — the API key is a
// credential, not a config value, and a shared home directory (or a backup
// tool that preserves modes) must not leave it group- or world-readable.
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
