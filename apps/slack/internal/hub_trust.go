package internal

import (
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
	return h.Host != "" || h.Port != "" || h.ServerPublicKeyB64 != ""
}

// pairs returns the env entries in a stable order.
func (h hubTrust) pairs() [][2]string {
	if !h.configured() {
		return nil
	}
	return [][2]string{{hubEnvHost, h.Host}, {hubEnvPort, h.Port}, {hubEnvServerPublicKey, h.ServerPublicKeyB64}}
}

var (
	dockerEndpointLine  = regexp.MustCompile(`(?m)^(\s*)-e QURL_ENDPOINT=[^\n]*\\\n`)
	composeEndpointLine = regexp.MustCompile(`(?m)^(\s*)QURL_ENDPOINT: \$\{QURL_ENDPOINT_YAML\}\n`)
	k8sEndpointEntry    = regexp.MustCompile(`(?m)^(\s*)- name: QURL_ENDPOINT\n\s*value: [^\n]*\n`)
)

// withHubTrustDockerEnv appends the Hub triple as -e flags right after the
// QURL_ENDPOINT flag of a rendered docker run block.
func withHubTrustDockerEnv(rendered string, h hubTrust) (string, error) {
	if !h.configured() {
		return rendered, nil
	}
	var b strings.Builder
	for _, kv := range h.pairs() {
		b.WriteString("${1}-e " + kv[0] + "=" + shellSingleQuote(kv[1]) + " \\\n")
	}
	return replaceOnce(dockerEndpointLine, rendered, "${0}"+b.String(), "docker run block")
}

// withHubTrustComposeEnv appends the Hub triple under the compose service
// environment map right after QURL_ENDPOINT.
func withHubTrustComposeEnv(rendered string, h hubTrust) (string, error) {
	if !h.configured() {
		return rendered, nil
	}
	var b strings.Builder
	for _, kv := range h.pairs() {
		quoted, err := yamlSingleQuoted(kv[1])
		if err != nil {
			return "", fmt.Errorf("%s: %w", kv[0], err)
		}
		b.WriteString("${1}" + kv[0] + ": " + quoted + "\n")
	}
	return replaceOnce(composeEndpointLine, rendered, "${0}"+b.String(), "compose environment")
}

// withHubTrustKubernetesEnv appends the Hub triple as env entries right after
// the QURL_ENDPOINT entry of a rendered container spec.
func withHubTrustKubernetesEnv(rendered string, h hubTrust) (string, error) {
	if !h.configured() {
		return rendered, nil
	}
	var b strings.Builder
	for _, kv := range h.pairs() {
		quoted, err := yamlSingleQuoted(kv[1])
		if err != nil {
			return "", fmt.Errorf("%s: %w", kv[0], err)
		}
		b.WriteString("${1}- name: " + kv[0] + "\n${1}  value: " + quoted + "\n")
	}
	return replaceOnce(k8sEndpointEntry, rendered, "${0}"+b.String(), "kubernetes env")
}

// hubTrustECSEnv returns the Hub triple as ECS environment entries.
func hubTrustECSEnv(h hubTrust) []ecsEnvironmentVar {
	out := make([]ecsEnvironmentVar, 0, 3)
	for _, kv := range h.pairs() {
		out = append(out, ecsEnvironmentVar{Name: kv[0], Value: kv[1]})
	}
	return out
}

// replaceOnce fails closed if the anchor is missing or ambiguous so a
// template edit cannot silently drop the Hub triple.
func replaceOnce(re *regexp.Regexp, rendered, repl, what string) (string, error) {
	if n := len(re.FindAllStringIndex(rendered, -1)); n != 1 {
		return "", fmt.Errorf("hub trust env: expected exactly one QURL_ENDPOINT anchor in %s, found %d", what, n)
	}
	return re.ReplaceAllString(rendered, repl), nil
}
