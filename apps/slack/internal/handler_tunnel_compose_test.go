package internal

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRenderDockerComposeTunnelInstructionsUsesWebService(t *testing.T) {
	t.Parallel()
	args := testTunnelInstallArgs()
	args.LocalPort = 9090
	args.Environment = tunnelEnvCompose
	args.WebRef = testTunnelDockerWeb
	got := mustRenderDockerComposeTunnelInstructions(t, args, testTunnelImageRef)

	for _, want := range []string{
		"Run this from your Docker Compose project directory on the Linux Docker host.",
		testTunnelKeyHistoryNote,
		testTunnelPipefailLine,
		"sudo -n true",
		"configure passwordless sudo",
		"WEB_SERVICE='" + testTunnelDockerWeb + "'",
		`case "$WEB_SERVICE" in`,
		"WEB_SERVICE may contain only letters, numbers, underscores, and hyphens.",
		"CONNECTOR_SERVICE='qurl-" + testTunnelSlug + "'",
		`CONFIG_FILE="$PWD/qurl-share-${QURL_CONNECTOR_ID}.yaml"`,
		"client-safe public/routing metadata",
		`$SUDO chmod 0644 "$CONFIG_FILE"`,
		`QURL_COMPOSE_FILE="$PWD/qurl-${QURL_CONNECTOR_ID}.compose.yaml"`,
		testTunnelKeyPromptLine,
		testTunnelKeyInstallLine,
		"qurl-" + testTunnelSlug + ".compose.yaml",
		"'qurl-" + testTunnelSlug + "':",
		`network_mode: "service:${WEB_SERVICE}"`,
		`user: "65532:65532"`,
		"read_only: true",
		"- /tmp:rw,size=64m",
		"pids_limit: 512",
		"- ALL",
		"- 'no-new-privileges:true'",
		"restart: unless-stopped",
		"entrypoint: ['/usr/local/bin/qurl']",
		"command: ['daemon', 'run', '--state-dir', '/var/lib/qurl', '--headless-config', '/etc/qurl/share.yaml', '--enrollment-token-file', '/run/secrets/qurl/enrollment-token']",
		"do not hand-edit the generated fragment",
		"bring the qurl service up again too",
		"depends_on:",
		"condition: service_started",
		testTunnelAgentDirFragment,
		"QURL_CONNECTOR_ID='" + testTunnelSlug + "'",
		"crid: '" + testTunnelCRID + "'",
		"resource_id: '" + testTunnelResourceID + "'",
		"knock_resource_id: '" + testTunnelKnockID + "'",
		`docker compose -f "$APP_COMPOSE_FILE" -f "$QURL_COMPOSE_FILE" up -d "$CONNECTOR_SERVICE"`,
		"Verify with `docker compose -f compose.yaml -f qurl-" + testTunnelSlug + ".compose.yaml logs -f qurl-" + testTunnelSlug + "`",
		"if you changed `APP_COMPOSE_FILE`, use that file there too",
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
	for _, forbidden := range []string{testForbiddenSlackYAMLFence, testForbiddenSlackShellFence, testForbiddenResourceLabel, testForbiddenBootstrapArgv, testTunnelAPIKey, "QURL_CONNECTOR_SLUG", "QURL_BOOTSTRAP_URL", "LAYERV_KNOCK_RESOURCE_ID", "ghcr.io/layervai/qurl-connector", "/usr/local/bin/qurl-connector"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("Docker Compose instructions leaked %q:\n%s", forbidden, got)
		}
	}
}

func TestRenderDockerComposeTunnelInstructionsEmitsParseableComposeFragment(t *testing.T) {
	t.Parallel()
	args := testTunnelInstallArgs()
	args.LocalPort = 9090
	args.Environment = tunnelEnvCompose
	args.WebRef = "web"
	got := mustRenderDockerComposeTunnelInstructions(t, args, testTunnelImageRef)

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
			Image       string            `yaml:"image"`
			User        string            `yaml:"user"`
			ReadOnly    bool              `yaml:"read_only"`
			Tmpfs       []string          `yaml:"tmpfs"`
			PidsLimit   int               `yaml:"pids_limit"`
			CapDrop     []string          `yaml:"cap_drop"`
			SecurityOpt []string          `yaml:"security_opt"`
			Volumes     []string          `yaml:"volumes"`
			Environment map[string]string `yaml:"environment"`
			EntryPoint  []string          `yaml:"entrypoint"`
			Command     []string          `yaml:"command"`
			Restart     string            `yaml:"restart"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(got[bodyStart:bodyStart+bodyEnd]), &parsed); err != nil {
		t.Fatalf("Compose fragment did not parse: %v", err)
	}
	service := parsed.Services["qurl-"+testTunnelSlug]
	if service.Image != testTunnelImageRef {
		t.Fatalf("Compose service image = %q, want %q", service.Image, testTunnelImageRef)
	}
	if service.User != ecsConnectorUser || !service.ReadOnly || service.PidsLimit != connectorPIDsLimit {
		t.Fatalf("Compose hardening = user %q read_only %v pids_limit %d", service.User, service.ReadOnly, service.PidsLimit)
	}
	if len(service.Tmpfs) != 1 || service.Tmpfs[0] != connectorTmpfsCompose {
		t.Fatalf("Compose tmpfs = %v, want [%s]", service.Tmpfs, connectorTmpfsCompose)
	}
	if len(service.CapDrop) != 1 || service.CapDrop[0] != testCapabilityAll || len(service.SecurityOpt) != 1 || service.SecurityOpt[0] != "no-new-privileges:true" {
		t.Fatalf("Compose capability/security options = %v / %v", service.CapDrop, service.SecurityOpt)
	}
	if service.Restart != "unless-stopped" || len(service.EntryPoint) != 1 || service.EntryPoint[0] != "/usr/local/bin/qurl" {
		t.Fatalf("Compose runtime = restart %q entrypoint %v", service.Restart, service.EntryPoint)
	}
	if len(service.Command) < 4 || service.Command[0] != "daemon" || service.Command[1] != "run" {
		t.Fatalf("Compose command = %v, want hidden daemon run", service.Command)
	}
	if _, ok := service.Environment["QURL_AUDIT_FILE"]; ok {
		t.Fatal("Compose rendered retired QURL_AUDIT_FILE")
	}
	if _, ok := service.Environment["LAYERV_KNOCK_RESOURCE_ID"]; ok {
		t.Fatal("Compose service rendered the advanced knock-resource override")
	}
}

func TestRenderDockerComposeTunnelInstructionsPinsValidatedExpansionInputs(t *testing.T) {
	t.Parallel()
	args := testTunnelInstallArgs()
	args.LocalPort = 9090
	args.Environment = tunnelEnvCompose
	args.WebRef = testTunnelComposeWeb
	got := mustRenderDockerComposeTunnelInstructions(t, args, testTunnelImageRef)

	for _, want := range []string{
		"WEB_SERVICE='" + testTunnelComposeWeb + "'",
		"QURL_CONNECTOR_ID='" + testTunnelSlug + "'",
		`case "$WEB_SERVICE" in`,
		`*[!A-Za-z0-9_-]*)`,
		"adding new shell variables here",
		"intentionally unquoted so it expands the validated variables",
		"<<QURL_COMPOSE_YAML_EOF",
		`'qurl-` + testTunnelSlug + `':`,
		`network_mode: "service:${WEB_SERVICE}"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("Docker Compose instructions missing validated-expansion guard %q:\n%s", want, got)
		}
	}
}
