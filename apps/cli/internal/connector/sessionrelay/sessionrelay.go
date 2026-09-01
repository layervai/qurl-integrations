// Package sessionrelay resolves and validates the trusted HTTPS origin used
// for registered Connector session admission and recovery.
package sessionrelay

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// EnvURL selects the trusted HTTPS NHP relay used only for registered
// connector-session KNK/RKN exchanges in a custom deployment.
const EnvURL = "QURL_CONNECTOR_SESSION_RELAY_URL"

// defaultURL is injected into a release build only after that deployment's
// relay has been reviewed. Source builds stay dark and use native UDP unless
// the custom-deployment environment variable is set.
var defaultURL string

// ErrConfig identifies every invalid or missing release-session relay value.
// Diagnostics never include the configured URL.
var ErrConfig = errors.New("qURL Connector session relay configuration")

// EmbeddedProductionURL validates and returns the release-build value without
// consulting custom-deployment environment overrides.
func EmbeddedProductionURL() (string, error) {
	if defaultURL == "" {
		return "", fmt.Errorf("%w: this build has no embedded session relay origin", ErrConfig)
	}
	if err := Validate(defaultURL); err != nil {
		return "", err
	}
	return defaultURL, nil
}

// Resolve returns the configured non-secret relay origin. An empty result
// keeps registered connector sessions on native UDP.
func Resolve() (string, error) {
	return ResolveWithLookup(os.LookupEnv)
}

// ResolveWithLookup applies the CLI's injected environment boundary. This
// keeps packaged journey tests hermetic while production passes os.LookupEnv.
func ResolveWithLookup(lookup func(string) (string, bool)) (string, error) {
	if lookup == nil {
		return "", fmt.Errorf("%w: session relay environment is unavailable", ErrConfig)
	}
	raw, set := lookup(EnvURL)
	if !set {
		raw = defaultURL
	}
	if raw == "" {
		if set {
			return "", fmt.Errorf("%w: %s must not be empty when set", ErrConfig, EnvURL)
		}
		return "", nil
	}
	if err := Validate(raw); err != nil {
		return "", err
	}
	return raw, nil
}

// Validate accepts one canonical HTTPS origin. Errors never include the
// configured value because custom deployment coordinates must not leak into
// user diagnostics or public CI logs.
func Validate(raw string) error {
	if raw == "" || raw != strings.TrimSpace(raw) || strings.Contains(raw, "#") {
		return fmt.Errorf("%w: session relay URL is empty or non-canonical", ErrConfig)
	}
	u, err := url.Parse(raw)
	if err != nil || !validOriginShape(u) {
		return fmt.Errorf("%w: session relay URL must be one canonical HTTPS origin", ErrConfig)
	}
	return validateHostPort(u)
}

func validOriginShape(u *url.URL) bool {
	return u != nil && u.Scheme == "https" && u.Host != "" && u.User == nil &&
		u.RawQuery == "" && u.Fragment == "" && u.Path == "" && u.RawPath == "" &&
		!u.ForceQuery && u.Opaque == ""
}

func validateHostPort(u *url.URL) error {
	_, ipErr := netip.ParseAddr(u.Hostname())
	if u.Host != strings.ToLower(u.Host) || strings.HasSuffix(u.Host, ":") ||
		!validDNSHost(u.Hostname()) || ipErr == nil {
		return fmt.Errorf("%w: session relay URL must use a canonical DNS host", ErrConfig)
	}
	if port := u.Port(); port != "" {
		n, portErr := strconv.Atoi(port)
		if portErr != nil || n < 1 || n > 65535 {
			return fmt.Errorf("%w: session relay URL has an invalid port", ErrConfig)
		}
		if n == 443 {
			return fmt.Errorf("%w: session relay URL must omit the default HTTPS port", ErrConfig)
		}
	}
	return nil
}

func validDNSHost(host string) bool {
	if host == "" || len(host) > 253 || strings.HasSuffix(host, ".") {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := range len(label) {
			b := label[i]
			if (b < 'a' || b > 'z') && (b < '0' || b > '9') && b != '-' {
				return false
			}
		}
	}
	return true
}
