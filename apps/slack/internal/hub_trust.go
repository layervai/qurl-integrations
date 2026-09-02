package internal

import (
	"errors"
	"fmt"
	"regexp"
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
	k8sEndpointEntry    = regexp.MustCompile(`(?m)^([ \t]*)- name: QURL_ENDPOINT\n[ \t]*value: [^\n]*\n`)
)

// withHubTrustDockerEnv appends the Hub triple as -e flags right after the
// QURL_ENDPOINT flag of a rendered docker run block.
func withHubTrustDockerEnv(rendered string, h hubTrust) (string, error) {
	return spliceHubEnv(dockerEndpointLine, rendered, h, "docker run block", func(indent string, kv [2]string) (string, error) {
		return indent + "-e " + kv[0] + "=" + shellSingleQuote(kv[1]) + " \\\n", nil
	})
}

// withHubTrustComposeEnv appends the Hub triple under the compose service
// environment map right after QURL_ENDPOINT.
func withHubTrustComposeEnv(rendered string, h hubTrust) (string, error) {
	return spliceHubEnv(composeEndpointLine, rendered, h, "compose environment", func(indent string, kv [2]string) (string, error) {
		quoted, err := yamlSingleQuoted(kv[1])
		if err != nil {
			return "", fmt.Errorf("%s: %w", kv[0], err)
		}
		return indent + kv[0] + ": " + quoted + "\n", nil
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
