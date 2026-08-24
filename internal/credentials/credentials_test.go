package credentials

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSaveThenLoadRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "credentials")
	want := Credentials{Host: "grpc://talon.example.com:9090", APIKey: "otk_test_0123456789"}

	if err := Save(path, want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != want {
		t.Errorf("Load = %+v, want %+v", got, want)
	}
}

func TestLoadMissingFileIsErrNotFound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials")
	_, err := Load(path)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Load(missing) = %v, want ErrNotFound", err)
	}
}

func TestLoadCorruptFileIsNotErrNotFound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials")
	if err := os.WriteFile(path, []byte("host: [this is not valid\n"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load(corrupt) = nil error, want a parse failure")
	}
	if errors.Is(err, ErrNotFound) {
		t.Errorf("Load(corrupt) = %v, must not read as ErrNotFound: the file exists, it's unreadable", err)
	}
}

func TestSaveRestrictsPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits don't apply")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "state", "credentials")
	if err := Save(path, Credentials{Host: "grpc://x:9090", APIKey: "otk_test_0123456789"}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat file: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0600 {
		t.Errorf("credentials file mode = %o, want 0600 (never group/world readable)", perm)
	}

	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("Stat dir: %v", err)
	}
	if perm := di.Mode().Perm(); perm != 0700 {
		t.Errorf("credentials dir mode = %o, want 0700", perm)
	}
}

// Pins that the key round-trips through the yaml field api_key, so a future
// field rename doesn't silently start writing it under a name Load ignores.
func TestSaveWritesAPIKeyField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials")
	if err := Save(path, Credentials{Host: "grpc://x:9090", APIKey: "otk_test_marker"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(data), "otk_test_marker") {
		t.Errorf("credentials file = %q, want it to contain the saved key under api_key", data)
	}
}
