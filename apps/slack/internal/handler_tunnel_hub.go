package internal

import (
	"encoding/base64"
	"errors"
	"regexp"
	"strings"
)

// TunnelHub is the explicit deployment trust rendered into headless installs.
// Empty uses the published CLI's production default.
type TunnelHub struct{ Host, Port, PublicKey string }

var tunnelHubHostPattern = regexp.MustCompile(`^[a-z0-9]+(?:[a-z0-9.-]*[a-z0-9])?\.layerv\.(?:ai|xyz)$`)

// Validate rejects partial or unsafe install configuration before enrollment.
// The CLI remains authoritative for native protocol/key validation.
func (h TunnelHub) Validate() error {
	if h == (TunnelHub{}) {
		return nil
	}
	key, err := base64.StdEncoding.Strict().DecodeString(h.PublicKey)
	if !tunnelHubHostPattern.MatchString(h.Host) || len(h.Host) > 253 || h.Port != "443" || err != nil || len(key) != 32 || base64.StdEncoding.EncodeToString(key) != h.PublicKey {
		return errors.New("QURL_CONNECTOR_HUB_HOST, QURL_CONNECTOR_HUB_PORT and QURL_CONNECTOR_HUB_SERVER_PUBLIC_KEY_B64 must form a complete pinned LayerV Hub on port 443")
	}
	for _, label := range strings.Split(h.Host, ".") {
		if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return errors.New("invalid Connector Hub hostname")
		}
	}
	return nil
}

// TODO(upstream-contract): qurl CLI v2.5.1 daemon run accepts these flags;
// leaving them unset selects its published production Hub trust.
func (h TunnelHub) flags() []string {
	if h == (TunnelHub{}) {
		return nil
	}
	return []string{"--hub-host", h.Host, "--hub-port", h.Port, "--hub-server-public-key-b64", h.PublicKey}
}

// Validate excludes quotes, making shellSingleQuote safe for these YAML scalars
// too. Revisit this if the allowed values change.
func (h TunnelHub) quotedFlags(separator string) string {
	flags := h.flags()
	for i, value := range flags {
		flags[i] = shellSingleQuote(value)
	}
	if len(flags) == 0 {
		return ""
	}
	return separator + strings.Join(flags, separator)
}
