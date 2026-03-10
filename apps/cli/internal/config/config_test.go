package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSaveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")

	cfg := &Config{
		APIKey:   "test-key",
		Endpoint: "https://example.com",
		Output:   "json",
	}

	if err := saveFile(p, cfg); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := loadFile(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if loaded.APIKey != cfg.APIKey {
		t.Errorf("APIKey = %q, want %q", loaded.APIKey, cfg.APIKey)
	}
	if loaded.Endpoint != cfg.Endpoint {
		t.Errorf("Endpoint = %q, want %q", loaded.Endpoint, cfg.Endpoint)
	}
	if loaded.Output != cfg.Output {
		t.Errorf("Output = %q, want %q", loaded.Output, cfg.Output)
	}
}

func TestLoadFileNotExist(t *testing.T) {
	cfg, err := loadFile(filepath.Join(t.TempDir(), "nonexistent.yaml"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.APIKey != "" || cfg.Endpoint != "" || cfg.Output != "" {
		t.Error("expected empty config for missing file")
	}
}

func TestLoadFileCorruptYAML(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(p, []byte("api_key: [unterminated\n  - broken: {nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := loadFile(p)
	if err == nil {
		t.Fatal("expected error for corrupt YAML")
	}
}

func TestSetInvalidKey(t *testing.T) {
	cfg := &Config{}
	if err := cfg.Set("unknown_key", "value"); err == nil {
		t.Fatal("expected error for unknown key")
	}
}

func TestSetValidKeys(t *testing.T) {
	cfg := &Config{}
	keys := map[string]string{"api_key": "k1", "endpoint": "e1", "output": "o1"}
	for key, val := range keys {
		if err := cfg.Set(key, val); err != nil {
			t.Errorf("Set(%q) unexpected error: %v", key, err)
		}
	}
	if cfg.APIKey != "k1" || cfg.Endpoint != "e1" || cfg.Output != "o1" {
		t.Error("Set did not update fields correctly")
	}
}

func TestGetValues(t *testing.T) {
	cfg := &Config{APIKey: "k", Endpoint: "e", Output: "o"}
	tests := []struct {
		key  string
		want string
	}{
		{"api_key", "k"},
		{"endpoint", "e"},
		{"output", "o"},
		{"unknown", ""},
	}
	for _, tt := range tests {
		if got := cfg.Get(tt.key); got != tt.want {
			t.Errorf("Get(%q) = %q, want %q", tt.key, got, tt.want)
		}
	}
}

func TestIsValidKey(t *testing.T) {
	for _, key := range []string{"api_key", "endpoint", "output"} {
		if !IsValidKey(key) {
			t.Errorf("IsValidKey(%q) = false, want true", key)
		}
	}
	if IsValidKey("bogus") {
		t.Error("IsValidKey(\"bogus\") = true, want false")
	}
}

func TestFilePermissions(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &Config{APIKey: "secret"}
	if err := saveFile(p, cfg); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	perm := info.Mode().Perm()
	if perm != 0o600 {
		t.Errorf("file permissions = %o, want 0600", perm)
	}
}

func TestProfileNameValidation(t *testing.T) {
	tests := []struct {
		name    string
		wantErr bool
	}{
		{"default", false},
		{"staging", false},
		{"my-profile", false},
		{"test_123", false},
		{"../../etc/passwd", true},
		{"path/traversal", true},
		{"has spaces", true},
		{"", true},
	}
	for _, tt := range tests {
		err := validateProfileName(tt.name)
		if (err != nil) != tt.wantErr {
			t.Errorf("validateProfileName(%q) error = %v, wantErr %v", tt.name, err, tt.wantErr)
		}
	}
}

func TestProfileIsolation(t *testing.T) {
	dir := t.TempDir()
	// Override configDir for testing by using saveFile/loadFile directly
	pDefault := filepath.Join(dir, "config.yaml")
	pStaging := filepath.Join(dir, "profiles", "staging.yaml")

	defaultCfg := &Config{APIKey: "default-key"}
	stagingCfg := &Config{APIKey: "staging-key"}

	if err := saveFile(pDefault, defaultCfg); err != nil {
		t.Fatal(err)
	}
	if err := saveFile(pStaging, stagingCfg); err != nil {
		t.Fatal(err)
	}

	loaded, err := loadFile(pDefault)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.APIKey != "default-key" {
		t.Errorf("default APIKey = %q, want %q", loaded.APIKey, "default-key")
	}

	loadedStaging, err := loadFile(pStaging)
	if err != nil {
		t.Fatal(err)
	}
	if loadedStaging.APIKey != "staging-key" {
		t.Errorf("staging APIKey = %q, want %q", loadedStaging.APIKey, "staging-key")
	}
}
