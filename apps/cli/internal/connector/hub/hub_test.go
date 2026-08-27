package hub

import (
	"bytes"
	"encoding/base64"
	"os"
	"strings"
	"testing"

	qurl "github.com/layervai/qurl-go/qurl"
	"golang.org/x/crypto/curve25519"
)

const validTestPublicKeyB64 = "CQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

func clearOverrideEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{EnvHost, EnvPort, EnvServerPublicKey} {
		t.Setenv(name, "restore-after-test")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBootstrapOverridePresence(t *testing.T) {
	valid := map[string]string{
		EnvHost:            "hub.test.nhp.layerv.ai",
		EnvPort:            "443",
		EnvServerPublicKey: validTestPublicKeyB64,
	}
	tests := []struct {
		name       string
		set        []string
		wantErr    string
		wantCustom bool
	}{
		{name: "zero fails closed without production pin", wantErr: "no pinned production Hub key"},
		{name: "one rejected", set: []string{EnvHost}, wantErr: "must be set together"},
		{name: "two rejected", set: []string{EnvHost, EnvPort}, wantErr: "must be set together"},
		{name: "three accepted", set: []string{EnvHost, EnvPort, EnvServerPublicKey}, wantCustom: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearOverrideEnv(t)
			for _, name := range tt.set {
				t.Setenv(name, valid[name])
			}
			got, err := Bootstrap()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Bootstrap error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !tt.wantCustom || got.Host != valid[EnvHost] || got.Port != DefaultPort || got.ServerPublicKeyB64 != validTestPublicKeyB64 {
				t.Fatalf("Hub bootstrap = %#v", got)
			}
		})
	}
}

func TestBootstrapRejectsEmptyAndMalformedValues(t *testing.T) {
	shortKey := base64.StdEncoding.EncodeToString(make([]byte, 31))
	noncanonicalKey := make([]byte, 32)
	for i := range noncanonicalKey {
		noncanonicalKey[i] = 0xff
	}
	noncanonicalKey[0] = 0xed
	noncanonicalKey[len(noncanonicalKey)-1] = 0x7f
	tests := []struct {
		name, host, port, key, wantErr string
	}{
		{name: "empty host", host: "", port: "443", key: validTestPublicKeyB64, wantErr: EnvHost + " must be non-empty"},
		{name: "IP host", host: "127.0.0.1", port: "443", key: validTestPublicKeyB64, wantErr: EnvHost},
		{name: "raw cloud hostname", host: "some-host.elb.amazonaws.com", port: "443", key: validTestPublicKeyB64, wantErr: EnvHost},
		{name: "uppercase host", host: "Hub.nhp.layerv.ai", port: "443", key: validTestPublicKeyB64, wantErr: EnvHost},
		{name: "trailing dot host", host: "hub.test.nhp.layerv.ai.", port: "443", key: validTestPublicKeyB64, wantErr: EnvHost},
		{name: "empty port", host: "hub.test.nhp.layerv.ai", port: "", key: validTestPublicKeyB64, wantErr: EnvPort + " must be non-empty"},
		{name: "nonnumeric port", host: "hub.test.nhp.layerv.ai", port: "udp", key: validTestPublicKeyB64, wantErr: EnvPort},
		{name: "unsupported port", host: "hub.test.nhp.layerv.ai", port: "62206", key: validTestPublicKeyB64, wantErr: EnvPort},
		{name: "empty key", host: "hub.test.nhp.layerv.ai", port: "443", key: "", wantErr: EnvServerPublicKey + " must be non-empty"},
		{name: "malformed base64 key", host: "hub.test.nhp.layerv.ai", port: "443", key: "not-base64", wantErr: EnvServerPublicKey},
		{name: "wrong length key", host: "hub.test.nhp.layerv.ai", port: "443", key: shortKey, wantErr: EnvServerPublicKey},
		{name: "noncanonical key", host: "hub.test.nhp.layerv.ai", port: "443", key: base64.StdEncoding.EncodeToString(noncanonicalKey), wantErr: EnvServerPublicKey},
		{name: "low order key", host: "hub.test.nhp.layerv.ai", port: "443", key: base64.StdEncoding.EncodeToString(make([]byte, 32)), wantErr: EnvServerPublicKey},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearOverrideEnv(t)
			t.Setenv(EnvHost, tt.host)
			t.Setenv(EnvPort, tt.port)
			t.Setenv(EnvServerPublicKey, tt.key)
			if _, err := Bootstrap(); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Bootstrap error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestBootstrapRejectsNoncanonicalPortSpellings(t *testing.T) {
	tests := []struct {
		port    string
		wantErr string
	}{
		{port: "0443", wantErr: "canonical decimal form"},
		{port: "+443", wantErr: "canonical decimal form"},
		{port: " 443 "},
		{port: "65536", wantErr: "standard NHP UDP port"},
		{port: "0", wantErr: "standard NHP UDP port"},
	}
	for _, tt := range tests {
		t.Run(tt.port, func(t *testing.T) {
			clearOverrideEnv(t)
			t.Setenv(EnvHost, "hub.test.nhp.layerv.ai")
			t.Setenv(EnvPort, tt.port)
			t.Setenv(EnvServerPublicKey, validTestPublicKeyB64)
			got, err := Bootstrap()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) || !strings.Contains(err.Error(), EnvPort) {
					t.Fatalf("Bootstrap(%q) error = %v, want %s rejection naming %s", tt.port, err, tt.wantErr, EnvPort)
				}
				return
			}
			if err != nil || got.Port != DefaultPort {
				t.Fatalf("Bootstrap(%q) = (%#v, %v), want trimmed canonical port %d", tt.port, got, err, DefaultPort)
			}
		})
	}
}

// provisionedDefaultPinForTest installs a canonical X25519 build-time default
// pin; the pin must come from real scalar multiplication so the full
// trust-root validation chain runs against a usable key.
func provisionedDefaultPinForTest(t *testing.T) string {
	t.Helper()
	scalar := bytes.Repeat([]byte{0x42}, curve25519.ScalarSize)
	public, err := curve25519.X25519(scalar, curve25519.Basepoint)
	if err != nil {
		t.Fatal(err)
	}
	keyB64 := base64.StdEncoding.EncodeToString(public)
	old := defaultServerPublicKeyB64
	defaultServerPublicKeyB64 = keyB64
	t.Cleanup(func() { defaultServerPublicKeyB64 = old })
	return keyB64
}

func TestBootstrapUsesProvisionedDefaultPin(t *testing.T) {
	clearOverrideEnv(t)
	keyB64 := provisionedDefaultPinForTest(t)
	got, err := Bootstrap()
	if err != nil {
		t.Fatal(err)
	}
	want := qurl.HubBootstrap{Host: DefaultHost, Port: DefaultPort, ServerPublicKeyB64: keyB64}
	if got != want {
		t.Fatalf("provisioned default Hub bootstrap = %#v, want %#v", got, want)
	}
	if got.Host != "hub.nhp.layerv.ai" || got.Port != 443 {
		t.Fatalf("shipping default endpoint = %s:%d, want hub.nhp.layerv.ai:443", got.Host, got.Port)
	}
}

func TestBootstrapRejectsMalformedProvisionedDefaultPin(t *testing.T) {
	for name, pin := range map[string]string{
		"malformed base64":   "%%%not-base64%%%",
		"unpadded base64":    strings.TrimRight(validTestPublicKeyB64, "="),
		"wrong length":       base64.StdEncoding.EncodeToString(make([]byte, 31)),
		"low order zero key": base64.StdEncoding.EncodeToString(make([]byte, 32)),
	} {
		t.Run(name, func(t *testing.T) {
			clearOverrideEnv(t)
			old := defaultServerPublicKeyB64
			defaultServerPublicKeyB64 = pin
			t.Cleanup(func() { defaultServerPublicKeyB64 = old })
			// The rejection must name the override env var: the documented
			// escape hatch for a bad build-time pin is the all-or-none
			// QURL_CONNECTOR_HUB_* custom deployment triple.
			if _, err := Bootstrap(); err == nil || !strings.Contains(err.Error(), EnvServerPublicKey) {
				t.Fatalf("Bootstrap error = %v, want rejection naming %s", err, EnvServerPublicKey)
			}
		})
	}
}

func TestBootstrapProductionDefaultFailsClosedUntilProvisioned(t *testing.T) {
	clearOverrideEnv(t)
	old := defaultServerPublicKeyB64
	defaultServerPublicKeyB64 = ""
	t.Cleanup(func() { defaultServerPublicKeyB64 = old })
	if _, err := Bootstrap(); err == nil || !strings.Contains(err.Error(), "no pinned production Hub key") {
		t.Fatalf("Bootstrap error = %v", err)
	}
}

func provisionedSandboxPinForTest(t *testing.T) string {
	t.Helper()
	scalar := bytes.Repeat([]byte{0x24}, curve25519.ScalarSize)
	public, err := curve25519.X25519(scalar, curve25519.Basepoint)
	if err != nil {
		t.Fatal(err)
	}
	keyB64 := base64.StdEncoding.EncodeToString(public)
	old := defaultSandboxServerPublicKeyB64
	defaultSandboxServerPublicKeyB64 = keyB64
	t.Cleanup(func() { defaultSandboxServerPublicKeyB64 = old })
	return keyB64
}

func TestBootstrapForEndpointUsesOnlyExactSandboxPin(t *testing.T) {
	clearOverrideEnv(t)
	keyB64 := provisionedSandboxPinForTest(t)

	for _, endpoint := range []string{
		SandboxAPIEndpoint,
		"https://API.LAYERV.XYZ/",
		"https://api.layerv.xyz:443",
	} {
		got, err := BootstrapForEndpoint(endpoint)
		if err != nil {
			t.Fatalf("BootstrapForEndpoint(%q): %v", endpoint, err)
		}
		want := qurl.HubBootstrap{Host: SandboxHost, Port: DefaultPort, ServerPublicKeyB64: keyB64}
		if got != want {
			t.Fatalf("BootstrapForEndpoint(%q) = %#v, want %#v", endpoint, got, want)
		}
	}
}

func TestBootstrapForEndpointRejectsSandboxLookalikes(t *testing.T) {
	clearOverrideEnv(t)
	_ = provisionedSandboxPinForTest(t)
	oldProduction := defaultServerPublicKeyB64
	defaultServerPublicKeyB64 = ""
	t.Cleanup(func() { defaultServerPublicKeyB64 = oldProduction })

	for _, endpoint := range []string{
		"http://api.layerv.xyz",
		"https://api.layerv.xyz:8443",
		"https://api.layerv.xyz.evil",
		"https://user@api.layerv.xyz",
		"https://api.layerv.xyz/path",
		"https://api.layerv.xyz?query=1",
		"not a URL",
	} {
		if _, err := BootstrapForEndpoint(endpoint); err == nil || !strings.Contains(err.Error(), "no pinned production Hub key") {
			t.Fatalf("BootstrapForEndpoint(%q) error = %v, want dark production rejection", endpoint, err)
		}
	}
}

func TestBootstrapForEndpointOperatorOverrideWinsInSandbox(t *testing.T) {
	override := validTestPublicKeyB64
	t.Setenv(EnvHost, "hub.custom.layerv.xyz")
	t.Setenv(EnvPort, "443")
	t.Setenv(EnvServerPublicKey, override)
	defaultSandbox := provisionedSandboxPinForTest(t)

	got, err := BootstrapForEndpoint(SandboxAPIEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	if got.Host != "hub.custom.layerv.xyz" || got.ServerPublicKeyB64 != override || got.ServerPublicKeyB64 == defaultSandbox {
		t.Fatalf("custom override did not win: %#v", got)
	}
}

// The production pin reaches a binary ONLY via release-build ldflags
// injection (-X …/internal/connector/hub.defaultServerPublicKeyB64=…), never
// via a source default. Tests compile without that ldflag, so a hardcoded pin
// — which would leak into every dev and CI build and defeat the fail-closed
// dark default — fails here.
func TestDefaultPinRemainsUnprovisionedInSource(t *testing.T) {
	if defaultServerPublicKeyB64 != "" {
		t.Fatalf("defaultServerPublicKeyB64 = %q in source; the production pin must arrive only through the release build wiring (see the package comment's flip procedure)", defaultServerPublicKeyB64)
	}
	if defaultSandboxServerPublicKeyB64 != "" {
		t.Fatalf("defaultSandboxServerPublicKeyB64 = %q in source; the sandbox pin must arrive only through reviewed release build wiring", defaultSandboxServerPublicKeyB64)
	}
}

// TestReleaseInjectedSandboxPinIsUsable is skipped by ordinary source tests.
// The release-contract gate can run it with the same -X value that GoReleaser
// embeds, which proves the linker target and runtime selection together.
func TestReleaseInjectedSandboxPinIsUsable(t *testing.T) {
	clearOverrideEnv(t)
	if defaultSandboxServerPublicKeyB64 == "" {
		t.Skip("sandbox release pin is not injected in source builds")
	}
	got, err := BootstrapForEndpoint(SandboxAPIEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	if got.Host != SandboxHost || got.Port != DefaultPort || got.ServerPublicKeyB64 != defaultSandboxServerPublicKeyB64 {
		t.Fatalf("injected sandbox bootstrap = %#v", got)
	}
}
