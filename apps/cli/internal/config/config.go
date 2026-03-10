// Package config handles CLI configuration file loading and saving.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config holds CLI configuration.
type Config struct {
	APIKey   string `yaml:"api_key,omitempty"`
	Endpoint string `yaml:"endpoint,omitempty"`
	Output   string `yaml:"output,omitempty"`
}

var validKeys = map[string]bool{
	"api_key":  true,
	"endpoint": true,
	"output":   true,
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
func ProfilePath(name string) string {
	dir := configDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "profiles", name+".yaml")
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
	return loadFile(ProfilePath(name))
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
func SaveProfile(name string, cfg *Config) error {
	return saveFile(ProfilePath(name), cfg)
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
