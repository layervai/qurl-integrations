package internal

import (
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// hubTrust is the NHP Hub trust triple the released qurl CLI needs at
// bootstrap. The CLI ships dark (no baked Hub pin), so a rendered install
// without it fails closed. All-or-none: cmd/main.go rejects partial triples
// at startup, so a renderer only ever sees a complete triple or none.
//
// TODO(upstream-contract): mirrors the qurl CLI env contract
// QURL_CONNECTOR_HUB_HOST / QURL_CONNECTOR_HUB_PORT /
// QURL_CONNECTOR_HUB_SERVER_PUBLIC_KEY_B64 (apps/cli/internal/connector/hub).
type hubTrust struct {
	Host               string
	Port               string
	ServerPublicKeyB64 string
}

// ConnectorHubTrust is the exported spelling cmd/main.go builds from env.
type ConnectorHubTrust = hubTrust

// Env var names the bot reads for the triple; the same names are rendered
// into installs (the CLI reads them), so they are defined exactly once.
const (
	EnvConnectorHubHost      = hubEnvHost
	EnvConnectorHubPort      = hubEnvPort
	EnvConnectorHubServerKey = hubEnvServerPublicKey

	connectorHubServerKeyLen  = 32
	connectorHubHostMaxLength = 253
)

// Allowlist (RFC 1123 labels): the value is rendered into shell and YAML
// install blocks, so anything outside a DNS name is rejected. IP literals
// (v4 or v6) are deliberately not accepted; the Hub is addressed by name.
var (
	connectorHubHostPattern = regexp.MustCompile(`^[A-Za-z\d]([A-Za-z\d-]{0,61}[A-Za-z\d])?(\.[A-Za-z\d]([A-Za-z\d-]{0,61}[A-Za-z\d])?)*$`)
	ipv4LiteralPattern      = regexp.MustCompile(`^\d+(\.\d+){3}$`)
)

// NewConnectorHubTrust validates the triple next to the renderers it feeds.
// All-or-none: all empty means "not configured" (nothing rendered); a partial
// triple is a deployment mistake and is rejected. A single trailing dot
// (FQDN spelling) is accepted and stripped.
func NewConnectorHubTrust(host, port, key string) (ConnectorHubTrust, error) {
	host, port, key = strings.TrimSpace(host), strings.TrimSpace(port), strings.TrimSpace(key)
	if host == "" && port == "" && key == "" {
		return ConnectorHubTrust{}, nil
	}
	if host == "" || port == "" || key == "" {
		return ConnectorHubTrust{}, fmt.Errorf("%s, %s and %s must be set together", EnvConnectorHubHost, EnvConnectorHubPort, EnvConnectorHubServerKey)
	}
	host = strings.TrimSuffix(host, ".")
	if len(host) > connectorHubHostMaxLength || !connectorHubHostPattern.MatchString(host) || ipv4LiteralPattern.MatchString(host) {
		return ConnectorHubTrust{}, fmt.Errorf("%s=%q must be a bare DNS host name", EnvConnectorHubHost, host)
	}
	// Canonical decimal only: Atoi would accept "+443" / "0443", which the
	// CLI then rejects at bootstrap.
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
		return ConnectorHubTrust{}, fmt.Errorf("%s=%q must be a port between 1 and 65535", EnvConnectorHubPort, port)
	}
	raw, err := base64.StdEncoding.DecodeString(key)
	if err != nil || len(raw) != connectorHubServerKeyLen {
		return ConnectorHubTrust{}, fmt.Errorf("%s must be the standard base64 encoding of a %d-byte key", EnvConnectorHubServerKey, connectorHubServerKeyLen)
	}
	return ConnectorHubTrust{Host: host, Port: port, ServerPublicKeyB64: key}, nil
}

const (
	hubEnvHost            = "QURL_CONNECTOR_HUB_HOST"
	hubEnvPort            = "QURL_CONNECTOR_HUB_PORT"
	hubEnvServerPublicKey = "QURL_CONNECTOR_HUB_SERVER_PUBLIC_KEY_B64"
)

func (h hubTrust) configured() bool {
	return h.Host != "" && h.Port != "" && h.ServerPublicKeyB64 != ""
}

// pairs returns the env entries in a stable order. A partial triple is a
// programming error upstream of the renderer and is reported, never rendered.
func (h hubTrust) pairs() ([][2]string, error) {
	if !h.configured() {
		if h.Host != "" || h.Port != "" || h.ServerPublicKeyB64 != "" {
			return nil, errors.New("hub trust env: partial triple (host, port and server public key are all required)")
		}
		return nil, nil
	}
	return [][2]string{{hubEnvHost, h.Host}, {hubEnvPort, h.Port}, {hubEnvServerPublicKey, h.ServerPublicKeyB64}}, nil
}

var (
	dockerEndpointLine  = regexp.MustCompile(`(?m)^([ \t]*)-e QURL_ENDPOINT=[^\n]*\\\n`)
	composeEndpointLine = regexp.MustCompile(`(?m)^([ \t]*)QURL_ENDPOINT: \$\{QURL_ENDPOINT_YAML\}\n`)
	composeEndpointVar  = regexp.MustCompile(`(?m)^([ \t]*)QURL_ENDPOINT_YAML=[^\n]*\n`)
	k8sEndpointEntry    = regexp.MustCompile(`(?m)^([ \t]*)- name: QURL_ENDPOINT\n[ \t]*value: [^\n]*\n`)
)

// withHubTrustDockerEnv appends the Hub triple as -e flags right after the
// QURL_ENDPOINT flag of a rendered docker run block.
func withHubTrustDockerEnv(rendered string, h hubTrust) (string, error) {
	return spliceHubEnv(dockerEndpointLine, rendered, h, "docker run block", func(indent string, kv [2]string) (string, error) {
		return indent + "-e " + kv[0] + "=" + shellSingleQuote(kv[1]) + " \\\n", nil
	})
}

// withHubTrustComposeEnv threads the Hub triple the same way the template
// threads QURL_ENDPOINT: each value is YAML-quoted, then shell-quoted into a
// KEY_YAML shell variable assigned OUTSIDE the unquoted compose heredoc, and
// the heredoc only references ${KEY_YAML}. Values therefore never reach the
// shell parser, regardless of the startup allowlist.
func withHubTrustComposeEnv(rendered string, h hubTrust) (string, error) {
	withVars, err := spliceHubEnv(composeEndpointVar, rendered, h, "compose shell variables", func(indent string, kv [2]string) (string, error) {
		quoted, err := yamlSingleQuoted(kv[1])
		if err != nil {
			return "", fmt.Errorf("%s: %w", kv[0], err)
		}
		return indent + kv[0] + "_YAML=" + shellSingleQuote(quoted) + "\n", nil
	})
	if err != nil {
		return "", err
	}
	return spliceHubEnv(composeEndpointLine, withVars, h, "compose environment", func(indent string, kv [2]string) (string, error) {
		return indent + kv[0] + ": ${" + kv[0] + "_YAML}\n", nil
	})
}

// withHubTrustKubernetesEnv appends the Hub triple as env entries right after
// the QURL_ENDPOINT entry of a rendered container spec.
func withHubTrustKubernetesEnv(rendered string, h hubTrust) (string, error) {
	return spliceHubEnv(k8sEndpointEntry, rendered, h, "kubernetes env", func(indent string, kv [2]string) (string, error) {
		quoted, err := yamlSingleQuoted(kv[1])
		if err != nil {
			return "", fmt.Errorf("%s: %w", kv[0], err)
		}
		return indent + "- name: " + kv[0] + "\n" + indent + "  value: " + quoted + "\n", nil
	})
}

// spliceHubEnv inserts the rendered Hub entries immediately after the single
// QURL_ENDPOINT anchor. It splices by index (no regexp replacement templates,
// so a "$" in a value is never interpreted) and fails closed if the anchor is
// missing or ambiguous, so a template edit cannot silently drop the triple.
func spliceHubEnv(re *regexp.Regexp, rendered string, h hubTrust, what string, line func(indent string, kv [2]string) (string, error)) (string, error) {
	pairs, err := h.pairs()
	if err != nil {
		return "", err
	}
	if pairs == nil {
		return rendered, nil
	}
	matches := re.FindAllStringSubmatchIndex(rendered, -1)
	if len(matches) != 1 {
		return "", fmt.Errorf("hub trust env: expected exactly one QURL_ENDPOINT anchor in %s, found %d", what, len(matches))
	}
	m := matches[0]
	indent := rendered[m[2]:m[3]]
	var b strings.Builder
	for _, kv := range pairs {
		entry, err := line(indent, kv)
		if err != nil {
			return "", err
		}
		b.WriteString(entry)
	}
	return rendered[:m[1]] + b.String() + rendered[m[1]:], nil
}

// hubTrustECSEnv returns the Hub triple as ECS environment entries.
func hubTrustECSEnv(h hubTrust) ([]ecsEnvironmentVar, error) {
	pairs, err := h.pairs()
	if err != nil {
		return nil, err
	}
	out := make([]ecsEnvironmentVar, 0, len(pairs))
	for _, kv := range pairs {
		out = append(out, ecsEnvironmentVar{Name: kv[0], Value: kv[1]})
	}
	return out, nil
}
