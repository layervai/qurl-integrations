package hub

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"

	qurl "github.com/layervai/qurl-go/qurl"
)

// The all-or-none custom-deployment override triple. Setting any one of the
// three requires setting all three.
const (
	// EnvHost overrides the NHP Hub DNS endpoint.
	EnvHost = "QURL_CONNECTOR_HUB_HOST"
	// EnvPort overrides the NHP Hub UDP port. The value must spell the
	// standard NHP UDP port in canonical decimal form; the variable exists so
	// the triple stays all-or-none and explicit rather than implying a port.
	EnvPort = "QURL_CONNECTOR_HUB_PORT"
	// EnvServerPublicKey overrides the pinned Hub server public key
	// (canonical padded standard base64 of a 32-byte X25519 public key).
	EnvServerPublicKey = "QURL_CONNECTOR_HUB_SERVER_PUBLIC_KEY_B64"
	// ReleaseEnvSandboxServerPublicKey is release-pipeline input, not a
	// runtime override. GoReleaser injects it into the sandbox-only build pin
	// after the verifier matches its reviewed fingerprint.
	ReleaseEnvSandboxServerPublicKey = "QURL_CONNECTOR_SANDBOX_HUB_SERVER_PUBLIC_KEY_B64"
)

const (
	// DefaultHost is the production NHP Hub endpoint compiled into release
	// builds. It is only usable once a production pin is provisioned; a dark
	// build fails closed instead of resolving it.
	DefaultHost = "hub.nhp.layerv.ai"
	// SandboxHost is selected only when the CLI is configured for the exact
	// sandbox API endpoint. It is never a fallback for production or custom
	// deployments.
	SandboxHost = "hub.nhp.layerv.xyz"
	// SandboxAPIEndpoint is the only API endpoint that selects the reviewed
	// sandbox Hub pin compiled into an official release.
	SandboxAPIEndpoint = "https://api.layerv.xyz"
	// DefaultPort is the standard NHP UDP port. Every NHP UDP endpoint is
	// pinned to 443.
	DefaultPort = 443
)

// defaultServerPublicKeyB64 is injected only after the production Hub trust
// root is provisioned. A blank build-time value deliberately requires the
// all-or-none custom/test override instead of trusting DNS without a pinned
// server identity.
//
// The only injection path is release-build ldflags
// (-X .../internal/connector/hub.defaultServerPublicKeyB64=…), validated by
// DecodeServerPublicKeyB64 before any artifact is published. Dev and CI
// builds never set it — TestDefaultPinRemainsUnprovisionedInSource fences
// that. See the package comment for the flip procedure.
var defaultServerPublicKeyB64 string

// defaultSandboxServerPublicKeyB64 is injected by the official release build
// after the public value is checked against the reviewed raw-key fingerprint.
// Source, test, and snapshot builds keep it blank.
var defaultSandboxServerPublicKeyB64 string

// ErrConfig is the identity of every Hub trust-bootstrap configuration
// failure this package can return: a dark build with no override triple, a
// partially set triple, or a malformed value in it. One sentinel for the
// family — the wrapped message names the offending variable — because the
// remedy class is uniform (fix the QURL_CONNECTOR_HUB_* configuration), and
// the CLI's exit-code contract keys on the class, not the spelling.
var ErrConfig = errors.New("qURL Connector Hub configuration")

// Bootstrap resolves the Hub trust bootstrap from the environment triple, or
// from the build's production pin when no override is present. It fails
// closed when the build is dark and no override is set, when the triple is
// partially set, or when any value is malformed.
func Bootstrap() (qurl.HubBootstrap, error) {
	return bootstrap(DefaultHost, defaultServerPublicKeyB64, "production")
}

// BootstrapForEndpoint resolves the same all-or-none operator override as
// Bootstrap. Without an override, only the exact sandbox API endpoint selects
// the sandbox Hub and its independently reviewed release pin. Every other API
// endpoint stays on the production default, which remains dark until the
// production Control root publishes its Hub identity.
func BootstrapForEndpoint(apiEndpoint string) (qurl.HubBootstrap, error) {
	if isSandboxAPIEndpoint(apiEndpoint) {
		return bootstrap(SandboxHost, defaultSandboxServerPublicKeyB64, "sandbox")
	}
	return Bootstrap()
}

func bootstrap(defaultHost, defaultKey, deployment string) (qurl.HubBootstrap, error) {
	host, hostSet := os.LookupEnv(EnvHost)
	portRaw, portSet := os.LookupEnv(EnvPort)
	key, keySet := os.LookupEnv(EnvServerPublicKey)
	setCount := 0
	for _, set := range []bool{hostSet, portSet, keySet} {
		if set {
			setCount++
		}
	}
	if setCount != 0 && setCount != 3 {
		return qurl.HubBootstrap{}, fmt.Errorf("%w: %s, %s, and %s must be set together", ErrConfig, EnvHost, EnvPort, EnvServerPublicKey)
	}
	if setCount == 0 {
		host = defaultHost
		portRaw = strconv.Itoa(DefaultPort)
		key = defaultKey
	} else {
		for _, required := range []struct{ name, value string }{
			{EnvHost, host},
			{EnvPort, portRaw},
			{EnvServerPublicKey, key},
		} {
			if strings.TrimSpace(required.value) == "" {
				return qurl.HubBootstrap{}, fmt.Errorf("%w: %s must be non-empty when the custom Hub triple is set", ErrConfig, required.name)
			}
		}
	}
	host = strings.TrimSpace(host)
	key = strings.TrimSpace(key)
	portRaw = strings.TrimSpace(portRaw)
	port, err := strconv.Atoi(portRaw)
	if err != nil {
		return qurl.HubBootstrap{}, fmt.Errorf("%w: %s must be a valid port: %w", ErrConfig, EnvPort, err)
	}
	// A pinned trust-root endpoint must have one byte spelling; Atoi alone
	// accepts "0443" and "+443".
	if strconv.Itoa(port) != portRaw {
		return qurl.HubBootstrap{}, fmt.Errorf("%w: %s must be a valid port in canonical decimal form; got %q", ErrConfig, EnvPort, portRaw)
	}
	if key == "" && setCount == 0 {
		return qurl.HubBootstrap{}, fmt.Errorf("%w: this build has no pinned %s Hub key; set the all-or-none %s/%s/%s custom deployment triple", ErrConfig, deployment, EnvHost, EnvPort, EnvServerPublicKey)
	}
	hub := qurl.HubBootstrap{Host: host, Port: port, ServerPublicKeyB64: key}
	if err := ValidateBootstrap(hub); err != nil {
		return qurl.HubBootstrap{}, err
	}
	return hub, nil
}

func isSandboxAPIEndpoint(endpoint string) bool {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || !strings.EqualFold(u.Scheme, "https") ||
		!strings.EqualFold(u.Hostname(), "api.layerv.xyz") ||
		(u.Port() != "" && u.Port() != "443") || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	return true
}

// ValidateBootstrap mirrors qurl-go's native-assignment trust-root checks at
// configuration load time. qurl-go remains authoritative before network I/O;
// this early check prevents a warm persisted lease from masking a malformed
// replacement Hub pin until a later refresh. The key checks are pin.go's,
// shared with the release-side pin verifier, so a pin the release pipeline
// injects is exactly a pin this path accepts.
// The share daemon also uses it to validate the non-secret Hub identity in
// its durable LaunchAgent arguments.
func ValidateBootstrap(hub qurl.HubBootstrap) error {
	if !validHost(hub.Host) {
		return fmt.Errorf("%w: %s must be a canonical lowercase DNS name below a LayerV-owned apex", ErrConfig, EnvHost)
	}
	if hub.Port != DefaultPort {
		return fmt.Errorf("%w: %s must be the standard NHP UDP port %d", ErrConfig, EnvPort, DefaultPort)
	}
	if _, err := DecodeServerPublicKeyB64(hub.ServerPublicKeyB64); err != nil {
		return fmt.Errorf("%w: %s %w", ErrConfig, EnvServerPublicKey, err)
	}
	return nil
}

func validHost(host string) bool {
	if host == "" || len(host) > 253 || strings.HasSuffix(host, ".") || net.ParseIP(host) != nil {
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
	return strings.HasSuffix(host, ".layerv.ai") || strings.HasSuffix(host, ".layerv.xyz")
}
