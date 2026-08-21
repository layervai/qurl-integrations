package config

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
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

// TestEnumValuesValidatedAtLoad pins the round-4 disposition: enum-valued
// settings a config FILE spells wrongly are ErrConfigFile (the configuration
// exit code), decided here in the config layer — the only place that knows
// the value's source. Valid spellings and absent keys load clean.
func TestEnumValuesValidatedAtLoad(t *testing.T) {
	invalid := map[string]string{
		"bad output":   "output: yaml\n",
		"bad color":    "color: sometimes\n",
		"cased output": "output: JSON\n",
		"color on":     "color: on\n",
	}
	for name, body := range invalid {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(dir)
			if !errors.Is(err, ErrConfigFile) {
				t.Errorf("err = %v, want ErrConfigFile", err)
			}
		})
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("output: json\ncolor: never\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("valid enums must load: %v", err)
	}
	if cfg.Output != "json" || cfg.Color != "never" {
		t.Errorf("cfg = %+v", cfg)
	}

	profiles := filepath.Join(dir, "profiles")
	if err := os.MkdirAll(profiles, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profiles, "bad.yaml"), []byte("output: table\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadProfile(dir, "bad"); !errors.Is(err, ErrConfigFile) {
		t.Errorf("profile err = %v, want ErrConfigFile", err)
	}
}

// TestEnumVocabulariesMatchOutputPackage pins config's duplicated enum
// literals to the output package's authoritative constants (config sits
// below output and cannot import it; this test can).
func TestEnumVocabulariesMatchOutputPackage(t *testing.T) {
	if want := []string{string(output.FormatText), string(output.FormatJSON)}; !slices.Equal(validOutputs, want) {
		t.Errorf("validOutputs = %v, want output's %v", validOutputs, want)
	}
	if want := []string{output.ColorAuto, output.ColorAlways, output.ColorNever}; !slices.Equal(validColors, want) {
		t.Errorf("validColors = %v, want output's %v", validColors, want)
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
