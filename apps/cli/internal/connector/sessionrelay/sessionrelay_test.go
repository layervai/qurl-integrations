package sessionrelay

import (
	"errors"
	"os"
	"strings"
	"testing"
)

const releaseRelayEnv = "QURL_RELEASE_SESSION_RELAY_URL"

func TestResolveUsesExactCustomDeploymentOrigin(t *testing.T) {
	t.Setenv(EnvURL, "https://relay.example.com")
	got, err := Resolve()
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got != "https://relay.example.com" {
		t.Fatalf("Resolve() = %q", got)
	}
}

func TestResolveStaysDarkWithoutReviewedDeployment(t *testing.T) {
	got, err := ResolveWithLookup(func(string) (string, bool) { return "", false })
	if err != nil || got != "" {
		t.Fatalf("Resolve() = %q, %v; want empty", got, err)
	}
}

func TestResolveRejectsExplicitEmptyOverride(t *testing.T) {
	_, err := ResolveWithLookup(func(string) (string, bool) { return "", true })
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("ResolveWithLookup() error = %v, want ErrConfig", err)
	}
}

func TestEmbeddedProductionURLFailsDarkAndValidatesProvisionedValue(t *testing.T) {
	old := defaultURL
	t.Cleanup(func() { defaultURL = old })
	defaultURL = ""
	if _, err := EmbeddedProductionURL(); !errors.Is(err, ErrConfig) {
		t.Fatalf("dark EmbeddedProductionURL() error = %v", err)
	}
	defaultURL = "https://relay.example.com"
	got, err := EmbeddedProductionURL()
	if err != nil || got != defaultURL {
		t.Fatalf("EmbeddedProductionURL() = %q, %v", got, err)
	}
}

func TestReleaseSessionRelayEnvironment(t *testing.T) {
	if os.Getenv("QURL_REQUIRE_RELEASE_SESSION_RELAY") != "1" {
		t.Skip("release session-relay gate is not armed")
	}
	raw, set := os.LookupEnv(releaseRelayEnv)
	if !set || raw == "" {
		t.Fatal("required release session relay is missing")
	}
	if err := Validate(raw); err != nil {
		t.Fatalf("required release session relay is invalid: %v", err)
	}
}

func TestResolveWithLookupUsesInjectedEnvironmentOnly(t *testing.T) {
	t.Setenv(EnvURL, "https://process.example.com")
	got, err := ResolveWithLookup(func(name string) (string, bool) {
		if name != EnvURL {
			t.Fatalf("lookup name = %q", name)
		}
		return "https://injected.example.com", true
	})
	if err != nil || got != "https://injected.example.com" {
		t.Fatalf("ResolveWithLookup() = %q, %v", got, err)
	}
}

func TestResolveWithLookupRejectsNilLookupAsConfiguration(t *testing.T) {
	_, err := ResolveWithLookup(nil)
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("ResolveWithLookup(nil) error = %v, want ErrConfig", err)
	}
}

func TestValidateAcceptsCanonicalCustomDeploymentOrigins(t *testing.T) {
	for _, raw := range []string{
		"https://relay.example.com",
		"https://relay.example.com:8443",
		"https://one.two.relay.example.com",
	} {
		t.Run(raw, func(t *testing.T) {
			if err := Validate(raw); err != nil {
				t.Fatalf("Validate(%q) error = %v", raw, err)
			}
		})
	}
}

func TestValidateRejectsNonOriginAndRedactsValue(t *testing.T) {
	tests := []string{
		"http://relay.example.com",
		"https://user@relay.example.com",
		"https://relay.example.com/relay",
		"https://relay.example.com/?secret=value",
		"https://relay.example.com/#internal",
		"https://relay.example.com#",
		"https://127.0.0.1",
		"https://[fe80::1%25eth0]",
		"https://relay_.example.com",
		"https://relay..example.com",
		"https://-relay.example.com",
		"https://relay-.example.com",
		"https://relay.example.com:443",
		"https://relay.example.com:",
		" https://relay.example.com",
	}
	for _, raw := range tests {
		t.Run(strings.ReplaceAll(raw, "/", "_"), func(t *testing.T) {
			err := Validate(raw)
			if err == nil {
				t.Fatal("Validate() succeeded")
			}
			if strings.Contains(err.Error(), raw) {
				t.Fatalf("error exposed configured relay URL: %v", err)
			}
		})
	}
}
