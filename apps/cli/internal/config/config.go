// Package config handles CLI configuration file loading and saving.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"gopkg.in/yaml.v3"
)

// Config holds CLI configuration.
type Config struct {
	APIKey   string `yaml:"api_key,omitempty"`
	KeyID    string `yaml:"key_id,omitempty"`
	Endpoint string `yaml:"endpoint,omitempty"`
	Output   string `yaml:"output,omitempty"`
}

var validKeys = map[string]bool{
	"api_key":  true,
	"endpoint": true,
	"output":   true,
}

var profileNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

func validateProfileName(name string) error {
	if !profileNamePattern.MatchString(name) {
		return fmt.Errorf("invalid profile name %q: must contain only alphanumeric, hyphen, or underscore", name)
	}
	return nil
}

// IsValidKey reports whether key is a recognized configuration key.
func IsValidKey(key string) bool {
	return validKeys[key]
}

// Get returns a config value by key.
func (c *Config) Get(key string) string {
	switch key {
	case "api_key":
		return c.APIKey
	case "endpoint":
		return c.Endpoint
	case "output":
		return c.Output
	default:
		return ""
	}
}

// Set sets a config value by key.
func (c *Config) Set(key, value string) error {
	if !validKeys[key] {
		return fmt.Errorf("unknown key %q (valid: api_key, endpoint, output)", key)
	}
	switch key {
	case "api_key":
		c.APIKey = value
	case "endpoint":
		c.Endpoint = value
	case "output":
		c.Output = value
	}
	return nil
}

// ValidKeys returns the list of valid configuration keys.
func ValidKeys() []string {
	return []string{"api_key", "endpoint", "output"}
}

// configDir returns the base config directory (~/.config/qurl).
func configDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "qurl")
}

// Path returns the default config file path.
func Path() string {
	dir := configDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "config.yaml")
}

// ProfilePath returns the config file path for a named profile.
func ProfilePath(name string) (string, error) {
	if err := validateProfileName(name); err != nil {
		return "", err
	}
	dir := configDir()
	if dir == "" {
		return "", nil
	}
	return filepath.Join(dir, "profiles", name+".yaml"), nil
}

// ListProfiles returns the names of available config profiles.
func ListProfiles() ([]string, error) {
	dir := configDir()
	if dir == "" {
		return nil, nil
	}
	profileDir := filepath.Join(dir, "profiles")
	entries, err := os.ReadDir(profileDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read profiles dir: %w", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if ext := filepath.Ext(name); ext == ".yaml" || ext == ".yml" {
			names = append(names, name[:len(name)-len(ext)])
		}
	}
	return names, nil
}

// Load reads the default config file.
func Load() (*Config, error) {
	return loadFile(Path())
}

// LoadProfile loads a named profile, falling back to the default config.
func LoadProfile(name string) (*Config, error) {
	if name == "" {
		return Load()
	}
	p, err := ProfilePath(name)
	if err != nil {
		return nil, err
	}
	return loadFile(p)
}

func loadFile(p string) (*Config, error) {
	if p == "" {
		return &Config{}, nil
	}

	data, err := os.ReadFile(filepath.Clean(p))
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{}, nil
		}
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return &cfg, nil
}

// Save writes the default config file with restricted permissions.
func Save(cfg *Config) error {
	return saveFile(Path(), cfg)
}

// SaveProfile writes a named profile config file.
// If name is empty, it saves to the default config file.
func SaveProfile(name string, cfg *Config) error {
	if name == "" {
		return Save(cfg)
	}
	p, err := ProfilePath(name)
	if err != nil {
		return err
	}
	return saveFile(p, cfg)
}

func saveFile(p string, cfg *Config) error {
	if p == "" {
		return errors.New("cannot determine config path")
	}

	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	return os.WriteFile(p, data, 0o600) // Restricted: may contain API key
}
