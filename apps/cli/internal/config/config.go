// Package config loads qURL CLI configuration files and resolves setting
// precedence. The precedence contract for every setting is:
//
//	command-line flag > environment variable > profile/config file > built-in default
//
// Config files never hold secrets: the API key lives in the credential store
// (or the QURL_API_KEY environment variable), and a config file that tries to
// smuggle one in is rejected outright rather than silently honored.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultEndpoint is the production qURL API endpoint.
const DefaultEndpoint = "https://api.layerv.ai"

// productionHost is the host of DefaultEndpoint, used to classify whether a
// configured endpoint talks to the production environment.
const productionHost = "api.layerv.ai"

// Sentinel errors for configuration failures. Every one maps to the same
// configuration exit code; see internal/exitcode.
var (
	// ErrInvalidProfileName reports a profile name outside [A-Za-z0-9_-]+.
	ErrInvalidProfileName = errors.New("cli: invalid profile name")
	// ErrConfigFile reports a config file that exists but cannot be read or parsed.
	ErrConfigFile = errors.New("cli: invalid config file")
	// ErrSecretInConfig reports a config file carrying an api_key entry.
	// Config files never hold secrets; the credential store and QURL_API_KEY do.
	ErrSecretInConfig = errors.New("cli: config files must not contain an API key")
)

// Config holds the non-secret CLI settings a config file may carry.
type Config struct {
	Endpoint string `yaml:"endpoint,omitempty"`
	Output   string `yaml:"output,omitempty"`
	Color    string `yaml:"color,omitempty"`
	// ConnectorID names the Connector `qurl connector run` serves when --id
	// is not passed — its ID, the route name the app serves under, the same
	// identity the standalone qurl-connector configures as QURL_CONNECTOR_ID
	// / YAML `id:`. It is an identity, not a secret: the enrollment token
	// and Connector state never live in config files.
	ConnectorID string `yaml:"connector_id,omitempty"`
	// ConnectorSlug is v1.1.0's spelling of ConnectorID, still read so an
	// existing connector_slug profile keeps working; ConnectorID wins when
	// both are set.
	//
	// Deprecated: remove at the next major.
	ConnectorSlug string `yaml:"connector_slug,omitempty"`
}

// Enum vocabularies for config-file values. These mirror the output
// package's Format and color-mode constants (config cannot import output —
// it sits below it); config_test pins the two vocabularies together so they
// cannot drift.
var (
	validOutputs = []string{"text", "json"}
	validColors  = []string{"auto", "always", "never"}
)

// validate rejects enum-valued settings a config file spelled wrongly. The
// config layer is the only place that knows the value came from a FILE, so
// this is what routes a config-file typo to the configuration exit code
// (ErrConfigFile → 3) while the same typo on a flag or environment variable
// stays a usage error (exit 2) at the resolution site.
func (c *Config) validate(path string) error {
	if err := validateEnum(path, "output", c.Output, validOutputs); err != nil {
		return err
	}
	return validateEnum(path, "color", c.Color, validColors)
}

func validateEnum(path, setting, value string, valid []string) error {
	if value == "" {
		return nil
	}
	for _, v := range valid {
		if value == v {
			return nil
		}
	}
	return fmt.Errorf("%w: %s: %s %q is not valid — use %s", ErrConfigFile, path, setting, value, strings.Join(valid, " or "))
}

var profileNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

func validateProfileName(name string) error {
	if !profileNamePattern.MatchString(name) {
		return fmt.Errorf("%w: %q must contain only alphanumeric, hyphen, or underscore characters", ErrInvalidProfileName, name)
	}
	return nil
}

// DefaultDir returns the base config directory (~/.config/qurl), or "" when
// the home directory cannot be determined.
func DefaultDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "qurl")
}

// Path returns the default config file path inside dir ("" when dir is "").
func Path(dir string) string {
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "config.yaml")
}

// ProfilePath returns the config file path for a named profile inside dir.
func ProfilePath(dir, name string) (string, error) {
	if err := validateProfileName(name); err != nil {
		return "", err
	}
	if dir == "" {
		return "", nil
	}
	return filepath.Join(dir, "profiles", name+".yaml"), nil
}

// Load reads the default config file inside dir. A missing file is not an
// error: it yields the zero config so defaults apply.
func Load(dir string) (*Config, error) {
	return loadFile(Path(dir))
}

// LoadProfile loads a named profile from dir, falling back to the default
// config file when name is empty. A named profile that does not exist is not
// an error (it yields the zero config); an unreadable or malformed file is.
func LoadProfile(dir, name string) (*Config, error) {
	if name == "" {
		return Load(dir)
	}
	p, err := ProfilePath(dir, name)
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
		return nil, fmt.Errorf("%w: read %s: %w", ErrConfigFile, p, err)
	}

	if err := rejectSecrets(data, p); err != nil {
		return nil, err
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("%w: parse %s: %w", ErrConfigFile, p, err)
	}
	if err := cfg.validate(p); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// rejectSecrets refuses config files that carry credential-shaped keys. The
// v2 contract is that config files hold no secrets, so an api_key entry left
// over from an older setup is surfaced loudly instead of silently ignored —
// silence would leave a live credential sitting in a plaintext file.
func rejectSecrets(data []byte, path string) error {
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		// Structural problems are reported by the typed parse in loadFile.
		return nil //nolint:nilerr // malformed YAML is diagnosed by the caller's typed parse
	}
	for key := range raw {
		if key == "api_key" || strings.Contains(strings.ToLower(key), "secret") {
			return fmt.Errorf("%w: remove %q from %s and use `qurl login` or QURL_API_KEY instead", ErrSecretInConfig, key, path)
		}
	}
	return nil
}

// Resolve returns the first non-empty value from: explicit flag value, the
// named environment variable, the config file value, then the default. This
// is the single precedence implementation every setting goes through.
func Resolve(flagValue, envKey string, lookup func(string) (string, bool), configValue, defaultValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if lookup != nil {
		if v, ok := lookup(envKey); ok && v != "" {
			return v
		}
	}
	if configValue != "" {
		return configValue
	}
	return defaultValue
}

// IsProductionEndpoint reports whether endpoint addresses the production qURL
// environment. Anything that is not the production host — sandboxes, local
// mocks, self-hosted deployments — is treated as non-production, which errs
// on the side of allowing test CRIDs through.
func IsProductionEndpoint(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Hostname(), productionHost)
}
