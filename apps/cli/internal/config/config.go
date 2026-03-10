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

// Path returns the config file path.
func Path() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "qurl", "config.yaml")
}

// Load reads the config file.
func Load() (*Config, error) {
	p := Path()
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

// Save writes the config file with restricted permissions.
func Save(cfg *Config) error {
	p := Path()
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
