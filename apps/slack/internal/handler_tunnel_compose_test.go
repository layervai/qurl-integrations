package internal

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/layervai/qurl-integrations/shared/client"
)

func TestRenderDockerComposeTunnelInstructionsUsesWebService(t *testing.T) {
	t.Parallel()
	got := renderDockerComposeTunnelInstructions(&tunnelInstallArgs{
		Slug:        testTunnelSlug,
		Alias:       testTunnelSlug,
		LocalPort:   9090,
		Environment: tunnelEnvCompose,
		WebRef:      testTunnelDockerWeb,
	}, &client.APIKey{APIKey: testTunnelAPIKey}, testTunnelImageRef)

	for _, want := range []string{
		"Run this from your Docker Compose project directory on the Linux Docker host.",
		testTunnelKeyHistoryNote,
		testTunnelPipefailLine,
		"WEB_SERVICE='" + testTunnelDockerWeb + "'",
		`case "$WEB_SERVICE" in`,
		"WEB_SERVICE may contain only letters, numbers, underscores, and hyphens.",
		"TUNNEL_SERVICE='qurl-tunnel-" + testTunnelSlug + "'",
		`CONFIG_FILE="$PWD/qurl-proxy-${QURL_TUNNEL_SLUG}.yaml"`,
		`QURL_COMPOSE_FILE="$PWD/qurl-tunnel-${QURL_TUNNEL_SLUG}.compose.yaml"`,
		testTunnelKeyPromptLine,
		testTunnelKeyInstallLine,
		"qurl-tunnel-" + testTunnelSlug + ".compose.yaml",
		"qurl-tunnel-" + testTunnelSlug + ":",
		`network_mode: "service:${WEB_SERVICE}"`,
		"a different slug creates a separate per-slug fragment",
		"bring the tunnel service up again too",
		"depends_on:",
		testTunnelAgentDirFragment,
		"QURL_TUNNEL_SLUG: ${QURL_TUNNEL_SLUG}",
		"QURL_TUNNEL_SLUG='" + testTunnelSlug + "'",
		`docker compose -f "$APP_COMPOSE_FILE" -f "$QURL_COMPOSE_FILE" up -d "$TUNNEL_SERVICE"`,
		"Verify with `docker compose -f compose.yaml -f qurl-tunnel-" + testTunnelSlug + ".compose.yaml logs -f qurl-tunnel-" + testTunnelSlug + "`",
		"if you changed `APP_COMPOSE_FILE`, use that file there too",
		"logs -f qurl-tunnel-" + testTunnelSlug,
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
		"\n  qurl-tunnel:\n",
		"up -d qurl-tunnel\n",
		"logs -f qurl-tunnel`;",
		"Verify with `docker compose -f \"$APP_COMPOSE_FILE\"",
	} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("Docker Compose instructions used unscoped service %q:\n%s", forbidden, got)
		}
	}
	for _, forbidden := range []string{testForbiddenSlackYAMLFence, testForbiddenSlackShellFence, testForbiddenResourceLabel, testTunnelResourceID, testTunnelAPIKey} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("Docker Compose instructions leaked %q:\n%s", forbidden, got)
		}
	}
}

func TestRenderDockerComposeTunnelInstructionsEmitsParseableComposeFragment(t *testing.T) {
	t.Parallel()
	got := renderDockerComposeTunnelInstructions(&tunnelInstallArgs{
		Slug:        testTunnelSlug,
		Alias:       testTunnelSlug,
		LocalPort:   9090,
		Environment: tunnelEnvCompose,
		WebRef:      "web",
	}, &client.APIKey{APIKey: testTunnelAPIKey}, testTunnelImageRef)

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
	service := parsed.Services["qurl-tunnel-"+testTunnelSlug]
	if service.Image != testTunnelImageRef {
		t.Fatalf("Compose service image = %q, want %q", service.Image, testTunnelImageRef)
	}
}
