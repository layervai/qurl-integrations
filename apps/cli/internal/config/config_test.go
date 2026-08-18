package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func lookupFrom(env map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	}
}

// TestResolvePrecedence pins the one precedence chain every setting uses:
// flag > env > config > default, at each boundary.
func TestResolvePrecedence(t *testing.T) {
	env := map[string]string{"QURL_ENDPOINT": "https://from-env.example"}

	cases := []struct {
		name        string
		flag        string
		env         map[string]string
		configValue string
		want        string
	}{
		{"flag wins over everything", "https://from-flag.example", env, "https://from-config.example", "https://from-flag.example"},
		{"env wins over config", "", env, "https://from-config.example", "https://from-env.example"},
		{"config wins over default", "", nil, "https://from-config.example", "https://from-config.example"},
		{"default when nothing set", "", nil, "", DefaultEndpoint},
		{"empty env value falls through", "", map[string]string{"QURL_ENDPOINT": ""}, "", DefaultEndpoint},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Resolve(tc.flag, "QURL_ENDPOINT", lookupFrom(tc.env), tc.configValue, DefaultEndpoint)
			if got != tc.want {
				t.Errorf("Resolve = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestProfileNameValidation(t *testing.T) {
	for _, name := range []string{"sandbox", "team-a", "p_1", "A9"} {
		if _, err := ProfilePath(t.TempDir(), name); err != nil {
			t.Errorf("valid profile name %q rejected: %v", name, err)
		}
	}
	for _, name := range []string{"", "../evil", "a b", "dot.dot", "sla/sh"} {
		_, err := ProfilePath(t.TempDir(), name)
		if !errors.Is(err, ErrInvalidProfileName) {
			t.Errorf("profile name %q: err = %v, want ErrInvalidProfileName", name, err)
		}
	}
}

func TestLoadProfileMissingIsZeroConfig(t *testing.T) {
	cfg, err := LoadProfile(t.TempDir(), "nonexistent")
	if err != nil {
		t.Fatalf("missing profile must not error: %v", err)
	}
	if cfg.Endpoint != "" || cfg.Output != "" || cfg.Color != "" {
		t.Errorf("missing profile must yield the zero config, got %+v", cfg)
	}
}

func TestLoadProfileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	profiles := filepath.Join(dir, "profiles")
	if err := os.MkdirAll(profiles, 0o700); err != nil {
		t.Fatal(err)
	}
	body := []byte("endpoint: https://sandbox.example\noutput: json\ncolor: never\n")
	if err := os.WriteFile(filepath.Join(profiles, "sandbox.yaml"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadProfile(dir, "sandbox")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Endpoint != "https://sandbox.example" || cfg.Output != "json" || cfg.Color != "never" {
		t.Errorf("unexpected profile contents: %+v", cfg)
	}
}

func TestMalformedConfigIsTypedError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(":\tnot yaml ["), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(dir)
	if !errors.Is(err, ErrConfigFile) {
		t.Errorf("err = %v, want ErrConfigFile", err)
	}
}

// TestSecretsInConfigRefused pins the no-secrets contract: api_key and
// secret-ish keys make the whole file unusable, loudly.
func TestSecretsInConfigRefused(t *testing.T) {
	for name, body := range map[string]string{
		"api_key":    "api_key: lv_live_abcdef1234567890abcdef\n",
		"secret-ish": "client_secret: shhh\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(dir)
			if !errors.Is(err, ErrSecretInConfig) {
				t.Errorf("err = %v, want ErrSecretInConfig", err)
			}
		})
	}
}

// TestLoadReadsHandWrittenFile pins the on-disk YAML shape a customer edits
// by hand — the v2 config layer only reads config files, it never writes them.
func TestLoadReadsHandWrittenFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(Path(dir), []byte("endpoint: https://sandbox.example\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Endpoint != "https://sandbox.example" {
		t.Errorf("load lost endpoint: %+v", cfg)
	}
}

func TestIsProductionEndpoint(t *testing.T) {
	cases := map[string]bool{
		DefaultEndpoint:              true,
		"https://API.LAYERV.AI":      true,
		"https://api.layerv.ai/":     true,
		"https://sandbox.layerv.ai":  false,
		"http://127.0.0.1:8080":      false,
		"http://localhost:3000":      false,
		"not a url at all ::":        false,
		"https://api.layerv.ai.evil": false,
	}
	for endpoint, want := range cases {
		if got := IsProductionEndpoint(endpoint); got != want {
			t.Errorf("IsProductionEndpoint(%q) = %v, want %v", endpoint, got, want)
		}
	}
}
