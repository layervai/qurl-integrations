package hub

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	qurl "github.com/layervai/qurl-go/qurl"
)

// The all-or-none custom-deployment override triple. Setting any one of the
// three requires setting all three; the names are the qURL Connector operator
// contract and must not drift from the standalone Connector's.
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
)

const (
	// DefaultHost is the production NHP Hub endpoint compiled into release
	// builds. It is only usable once a production pin is provisioned; a dark
	// build fails closed instead of resolving it.
	DefaultHost = "hub.nhp.layerv.ai"
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

// Bootstrap resolves the Hub trust bootstrap from the environment triple, or
// from the build's production pin when no override is present. It fails
// closed when the build is dark and no override is set, when the triple is
// partially set, or when any value is malformed.
func Bootstrap() (qurl.HubBootstrap, error) {
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
		return qurl.HubBootstrap{}, fmt.Errorf("%s, %s, and %s must be set together", EnvHost, EnvPort, EnvServerPublicKey)
	}
	if setCount == 0 {
		host = DefaultHost
		portRaw = strconv.Itoa(DefaultPort)
		key = defaultServerPublicKeyB64
	} else {
		for _, required := range []struct{ name, value string }{
			{EnvHost, host},
			{EnvPort, portRaw},
			{EnvServerPublicKey, key},
		} {
			if strings.TrimSpace(required.value) == "" {
				return qurl.HubBootstrap{}, fmt.Errorf("%s must be non-empty when the custom Hub triple is set", required.name)
			}
		}
	}
	host = strings.TrimSpace(host)
	key = strings.TrimSpace(key)
	portRaw = strings.TrimSpace(portRaw)
	port, err := strconv.Atoi(portRaw)
	if err != nil {
		return qurl.HubBootstrap{}, fmt.Errorf("%s must be a valid port: %w", EnvPort, err)
	}
	// A pinned trust-root endpoint must have one byte spelling; Atoi alone
	// accepts "0443" and "+443".
	if strconv.Itoa(port) != portRaw {
		return qurl.HubBootstrap{}, fmt.Errorf("%s must be a valid port in canonical decimal form; got %q", EnvPort, portRaw)
	}
	if key == "" && setCount == 0 {
		return qurl.HubBootstrap{}, fmt.Errorf("this build has no pinned production Hub key; set the all-or-none %s/%s/%s custom deployment triple", EnvHost, EnvPort, EnvServerPublicKey)
	}
	hub := qurl.HubBootstrap{Host: host, Port: port, ServerPublicKeyB64: key}
	if err := validateBootstrap(hub); err != nil {
		return qurl.HubBootstrap{}, err
	}
	return hub, nil
}

// validateBootstrap mirrors qurl-go's native-assignment trust-root checks at
// configuration load time. qurl-go remains authoritative before network I/O;
// this early check prevents a warm persisted lease from masking a malformed
// replacement Hub pin until a later refresh. The key checks are pin.go's,
// shared with the release-side pin verifier, so a pin the release pipeline
// injects is exactly a pin this path accepts.
func validateBootstrap(hub qurl.HubBootstrap) error {
	if !validHost(hub.Host) {
		return fmt.Errorf("%s must be a canonical lowercase DNS name below a LayerV-owned apex", EnvHost)
	}
	if hub.Port != DefaultPort {
		return fmt.Errorf("%s must be the standard NHP UDP port %d", EnvPort, DefaultPort)
	}
	if _, err := DecodeServerPublicKeyB64(hub.ServerPublicKeyB64); err != nil {
		return fmt.Errorf("%s %w", EnvServerPublicKey, err)
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
