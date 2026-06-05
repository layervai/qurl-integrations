package internal

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRenderDockerComposeTunnelInstructionsUsesWebService(t *testing.T) {
	t.Parallel()
	got := mustRenderDockerComposeTunnelInstructions(t, &tunnelInstallArgs{
		Slug:        testTunnelSlug,
		Alias:       testTunnelSlug,
		LocalPort:   9090,
		Environment: tunnelEnvCompose,
		WebRef:      testTunnelDockerWeb,
	}, testTunnelImageRef)

	for _, want := range []string{
		"Run this from your Docker Compose project directory on the Linux Docker host.",
		testTunnelKeyHistoryNote,
		testTunnelPipefailLine,
		"sudo -n true",
		"configure passwordless sudo",
		"WEB_SERVICE='" + testTunnelDockerWeb + "'",
		`case "$WEB_SERVICE" in`,
		"WEB_SERVICE may contain only letters, numbers, underscores, and hyphens.",
		"CONNECTOR_SERVICE='qurl-connector-" + testTunnelSlug + "'",
		`CONFIG_FILE="$PWD/qurl-proxy-${QURL_CONNECTOR_ID}.yaml"`,
		`QURL_COMPOSE_FILE="$PWD/qurl-connector-${QURL_CONNECTOR_ID}.compose.yaml"`,
		testTunnelKeyPromptLine,
		testTunnelKeyInstallLine,
		"qurl-connector-" + testTunnelSlug + ".compose.yaml",
		"'qurl-connector-" + testTunnelSlug + "':",
		`network_mode: "service:${WEB_SERVICE}"`,
		"do not hand-edit the generated fragment",
		"bring the qURL Connector service up again too",
		"depends_on:",
		"condition: service_started",
		testTunnelAgentDirFragment,
		"QURL_CONNECTOR_ID: ${QURL_CONNECTOR_ID}",
		"QURL_CONNECTOR_ID='" + testTunnelSlug + "'",
		`docker compose -f "$APP_COMPOSE_FILE" -f "$QURL_COMPOSE_FILE" up -d "$CONNECTOR_SERVICE"`,
		"Verify with `docker compose -f compose.yaml -f qurl-connector-" + testTunnelSlug + ".compose.yaml logs -f qurl-connector-" + testTunnelSlug + "`",
		"if you changed `APP_COMPOSE_FILE`, use that file there too",
		"logs -f qurl-connector-" + testTunnelSlug,
		testTunnelLocalPort9090Line,
		testTunnelImageRef,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("Docker Compose instructions missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Replace `YOUR_COMPOSE_SERVICE_NAME`") {
		t.Fatalf("Docker Compose instructions still included placeholder warning:\n%s", got)
	}
	for _, forbidden := range []string{
		"\n  qurl-connector:\n",
		"up -d qurl-connector\n",
		"logs -f qurl-connector`;",
		"Verify with `docker compose -f \"$APP_COMPOSE_FILE\"",
	} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("Docker Compose instructions used unscoped service %q:\n%s", forbidden, got)
		}
	}
	for _, forbidden := range []string{testForbiddenSlackYAMLFence, testForbiddenSlackShellFence, testForbiddenResourceLabel, testForbiddenBootstrapArgv, testTunnelResourceID, testTunnelAPIKey, "QURL_CONNECTOR_SLUG"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("Docker Compose instructions leaked %q:\n%s", forbidden, got)
		}
	}
}

func TestRenderDockerComposeTunnelInstructionsEmitsParseableComposeFragment(t *testing.T) {
	t.Parallel()
	got := mustRenderDockerComposeTunnelInstructions(t, &tunnelInstallArgs{
		Slug:        testTunnelSlug,
		Alias:       testTunnelSlug,
		LocalPort:   9090,
		Environment: tunnelEnvCompose,
		WebRef:      "web",
	}, testTunnelImageRef)

	start := "cat > \"$QURL_COMPOSE_FILE\" <<QURL_COMPOSE_YAML_EOF\n"
	bodyStart := strings.Index(got, start)
	if bodyStart < 0 {
		t.Fatalf("Compose instructions missing generated fragment heredoc:\n%s", got)
	}
	bodyStart += len(start)
	bodyEnd := strings.Index(got[bodyStart:], "\nQURL_COMPOSE_YAML_EOF")
	if bodyEnd < 0 {
		t.Fatalf("Compose instructions missing generated fragment heredoc terminator:\n%s", got)
	}
	var parsed struct {
		Services map[string]struct {
			Image string `yaml:"image"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(got[bodyStart:bodyStart+bodyEnd]), &parsed); err != nil {
		t.Fatalf("Compose fragment did not parse: %v", err)
	}
	service := parsed.Services["qurl-connector-"+testTunnelSlug]
	if service.Image != testTunnelImageRef {
		t.Fatalf("Compose service image = %q, want %q", service.Image, testTunnelImageRef)
	}
}

func TestRenderDockerComposeTunnelInstructionsPinsValidatedExpansionInputs(t *testing.T) {
	t.Parallel()
	got := mustRenderDockerComposeTunnelInstructions(t, &tunnelInstallArgs{
		Slug:        testTunnelSlug,
		Alias:       testTunnelSlug,
		LocalPort:   9090,
		Environment: tunnelEnvCompose,
		WebRef:      testTunnelComposeWeb,
	}, testTunnelImageRef)

	for _, want := range []string{
		"WEB_SERVICE='" + testTunnelComposeWeb + "'",
		"QURL_CONNECTOR_ID='" + testTunnelSlug + "'",
		`case "$WEB_SERVICE" in`,
		`*[!A-Za-z0-9_-]*)`,
		"adding new shell variables here",
		"intentionally unquoted so it expands the validated variables",
		"<<QURL_COMPOSE_YAML_EOF",
		`'qurl-connector-` + testTunnelSlug + `':`,
		`network_mode: "service:${WEB_SERVICE}"`,
		`QURL_CONNECTOR_ID: ${QURL_CONNECTOR_ID}`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("Docker Compose instructions missing validated-expansion guard %q:\n%s", want, got)
		}
	}
}
