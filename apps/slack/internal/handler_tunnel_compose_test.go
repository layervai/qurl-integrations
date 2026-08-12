package internal

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRenderDockerComposeTunnelInstructionsUsesWebService(t *testing.T) {
	t.Parallel()
	got := mustRenderDockerComposeTunnelInstructions(t, &tunnelInstallArgs{
		Slug:               testTunnelSlug,
		Alias:              testTunnelSlug,
		LocalPort:          9090,
		Environment:        tunnelEnvCompose,
		WebRef:             testTunnelDockerWeb,
		ResourceID:         testTunnelResourceID,
		ConnectorRoutingID: testTunnelRoutingID,
		KnockResourceID:    testTunnelKnockID,
		APIURL:             testTunnelAPIURL,
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
		`AUDIT_DIR="/var/log/layerv/qurl-connector/${QURL_CONNECTOR_ID}"`,
		`$SUDO install -d -m 0700 -o 65532 -g 65532 "$AUDIT_DIR"`,
		"client-safe public/routing metadata",
		`$SUDO chmod 0644 "$CONFIG_FILE"`,
		`QURL_COMPOSE_FILE="$PWD/qurl-connector-${QURL_CONNECTOR_ID}.compose.yaml"`,
		testTunnelKeyPromptLine,
		testTunnelKeyInstallLine,
		"qurl-connector-" + testTunnelSlug + ".compose.yaml",
		"'qurl-connector-" + testTunnelSlug + "':",
		`network_mode: "service:${WEB_SERVICE}"`,
		`user: "65532:65532"`,
		"read_only: true",
		"- /tmp:rw,size=64m",
		"pids_limit: 512",
		"- ALL",
		"- 'no-new-privileges:true'",
		"QURL_AUDIT_FILE: /var/log/layerv/qurl-connector/audit.log",
		"do not hand-edit the generated fragment",
		"bring the qURL Connector service up again too",
		"depends_on:",
		"condition: service_started",
		testTunnelAgentDirFragment,
		"QURL_CONNECTOR_ID: ${QURL_CONNECTOR_ID}",
		"QURL_CONNECTOR_ID='" + testTunnelSlug + "'",
		"resource_id: '" + testTunnelResourceID + "'",
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
	for _, forbidden := range []string{testForbiddenSlackYAMLFence, testForbiddenSlackShellFence, testForbiddenResourceLabel, testForbiddenBootstrapArgv, testTunnelAPIKey, "QURL_CONNECTOR_SLUG", "QURL_BOOTSTRAP_URL", "knock_resource_id", "LAYERV_KNOCK_RESOURCE_ID"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("Docker Compose instructions leaked %q:\n%s", forbidden, got)
		}
	}
}

func TestRenderDockerComposeTunnelInstructionsEmitsParseableComposeFragment(t *testing.T) {
	t.Parallel()
	got := mustRenderDockerComposeTunnelInstructions(t, &tunnelInstallArgs{
		Slug:               testTunnelSlug,
		Alias:              testTunnelSlug,
		LocalPort:          9090,
		Environment:        tunnelEnvCompose,
		WebRef:             "web",
		ResourceID:         testTunnelResourceID,
		ConnectorRoutingID: testTunnelRoutingID,
		KnockResourceID:    testTunnelKnockID,
		APIURL:             testTunnelAPIURL,
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
			Image       string            `yaml:"image"`
			User        string            `yaml:"user"`
			ReadOnly    bool              `yaml:"read_only"`
			Tmpfs       []string          `yaml:"tmpfs"`
			PidsLimit   int               `yaml:"pids_limit"`
			CapDrop     []string          `yaml:"cap_drop"`
			SecurityOpt []string          `yaml:"security_opt"`
			Volumes     []string          `yaml:"volumes"`
			Environment map[string]string `yaml:"environment"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(got[bodyStart:bodyStart+bodyEnd]), &parsed); err != nil {
		t.Fatalf("Compose fragment did not parse: %v", err)
	}
	service := parsed.Services["qurl-connector-"+testTunnelSlug]
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
	if got := service.Environment[connectorAuditFileEnv]; got != connectorAuditFilePath {
		t.Fatalf("Compose %s = %q, want %q", connectorAuditFileEnv, got, connectorAuditFilePath)
	}
	if _, ok := service.Environment["LAYERV_KNOCK_RESOURCE_ID"]; ok {
		t.Fatal("Compose service rendered the advanced knock-resource override")
	}
}

func TestRenderDockerComposeTunnelInstructionsPinsValidatedExpansionInputs(t *testing.T) {
	t.Parallel()
	got := mustRenderDockerComposeTunnelInstructions(t, &tunnelInstallArgs{
		Slug:               testTunnelSlug,
		Alias:              testTunnelSlug,
		LocalPort:          9090,
		Environment:        tunnelEnvCompose,
		WebRef:             testTunnelComposeWeb,
		ResourceID:         testTunnelResourceID,
		ConnectorRoutingID: testTunnelRoutingID,
		KnockResourceID:    testTunnelKnockID,
		APIURL:             testTunnelAPIURL,
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

func TestRenderDockerComposeTunnelInstructionsHubTrustSet(t *testing.T) {
	t.Parallel()
	args := testTunnelInstallArgs()
	args.HubTrust = testTunnelHubTrust()

	got := mustRenderDockerComposeTunnelInstructions(t, args, testTunnelImageRef)

	quotedHost, err := yamlSingleQuoted(testTunnelHubHost)
	if err != nil {
		t.Fatalf("yamlSingleQuoted(host): %v", err)
	}
	quotedPort, err := yamlSingleQuoted(testTunnelHubPort)
	if err != nil {
		t.Fatalf("yamlSingleQuoted(port): %v", err)
	}
	quotedKey, err := yamlSingleQuoted(testTunnelHubKeyB64)
	if err != nil {
		t.Fatalf("yamlSingleQuoted(key): %v", err)
	}
	for _, want := range []string{
		"QURL_CONNECTOR_HUB_HOST_YAML=" + shellSingleQuote(quotedHost),
		"QURL_CONNECTOR_HUB_PORT_YAML=" + shellSingleQuote(quotedPort),
		"QURL_CONNECTOR_HUB_SERVER_PUBLIC_KEY_B64_YAML=" + shellSingleQuote(quotedKey),
		"      QURL_CONNECTOR_HUB_HOST: ${QURL_CONNECTOR_HUB_HOST_YAML}",
		"      QURL_CONNECTOR_HUB_PORT: ${QURL_CONNECTOR_HUB_PORT_YAML}",
		"      QURL_CONNECTOR_HUB_SERVER_PUBLIC_KEY_B64: ${QURL_CONNECTOR_HUB_SERVER_PUBLIC_KEY_B64_YAML}",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("Docker Compose instructions missing Hub trust entry %q:\n%s", want, got)
		}
	}
	// The Hub trust block sits between QURL_CONNECTOR_ID and QURL_API_URL,
	// matching the Docker renderer's placement.
	idIdx := strings.Index(got, "QURL_CONNECTOR_ID: ${QURL_CONNECTOR_ID}")
	hubIdx := strings.Index(got, "QURL_CONNECTOR_HUB_HOST: ${QURL_CONNECTOR_HUB_HOST_YAML}")
	apiIdx := strings.Index(got, "QURL_API_URL: ${QURL_API_URL_YAML}")
	if idIdx < 0 || hubIdx < 0 || apiIdx < 0 || idIdx >= hubIdx || hubIdx >= apiIdx {
		t.Fatalf("Docker Compose Hub trust entries out of order: id=%d hub=%d api=%d:\n%s", idIdx, hubIdx, apiIdx, got)
	}
}

// wantComposeHubUnsetGolden is byte-for-byte what renderDockerComposeTunnelInstructions
// produced for testTunnelInstallArgs()+testTunnelImageRef before Hub trust
// passthrough existed, captured mechanically (not hand-transcribed) to prove
// the unset path is unchanged rather than assume it.
const wantComposeHubUnsetGolden = "Run this from your Docker Compose project directory on the Linux Docker host. It prompts for the enrollment token so the secret does not land in shell history; use a trusted host and shell because local administrators can inspect process state during setup. If your terminal echoes pasted input, stop and use a platform secret manager instead. Replace `YOUR_COMPOSE_SERVICE_NAME` in the block first, fill the Docker service/container field, or use `service:<name>` / `web_container:<name>` in the typed command. If your app file is not compose.yaml, set `APP_COMPOSE_FILE` before running it. Re-run this install to regenerate the same qURL Connector's Compose fragment when the port or service changes; do not hand-edit the generated fragment because the next install replaces it. If Compose recreates the web service container, bring the qURL Connector service up again too.\n\n```\nset -eu\nif (set -o pipefail) 2>/dev/null; then\n  set -o pipefail\nfi\n\nif [ \"$(id -u)\" -eq 0 ]; then\n  SUDO=\"\"\nelif command -v sudo >/dev/null 2>&1 && sudo -n true 2>/dev/null; then\n  SUDO=\"sudo -n\"\nelse\n  echo \"Run as root or configure passwordless sudo so the state and secret directories can be owned by UID 65532.\" >&2\n  exit 1\nfi\n\n# Run from the directory with your existing Compose file.\nAPP_COMPOSE_FILE=${APP_COMPOSE_FILE:-compose.yaml}\nWEB_SERVICE='YOUR_COMPOSE_SERVICE_NAME'\nif [ \"$WEB_SERVICE\" = \"YOUR_COMPOSE_SERVICE_NAME\" ] || [ -z \"$WEB_SERVICE\" ]; then\n  echo \"Set WEB_SERVICE to the Compose service name for your local HTTP server.\" >&2\n  exit 1\nfi\ncase \"$WEB_SERVICE\" in\n  [A-Za-z0-9]*) ;;\n  *)\n    echo \"WEB_SERVICE must start with a letter or number.\" >&2\n    exit 1\n    ;;\nesac\ncase \"$WEB_SERVICE\" in\n  *[!A-Za-z0-9_-]*)\n    echo \"WEB_SERVICE may contain only letters, numbers, underscores, and hyphens.\" >&2\n    exit 1\n    ;;\nesac\n\nQURL_CONNECTOR_ID='prod-dashboard'\nCONNECTOR_SERVICE='qurl-connector-prod-dashboard'\nQURL_API_URL_YAML=''\"'\"'https://api.sandbox.example/v1'\"'\"''\nSECRET_DIR=\"/run/secrets/qurl-connector/${QURL_CONNECTOR_ID}\"\nAGENT_STATE_DIR=\"/var/lib/layerv/qurl-connector/${QURL_CONNECTOR_ID}/agent\"\nAUDIT_DIR=\"/var/log/layerv/qurl-connector/${QURL_CONNECTOR_ID}\"\nCONFIG_FILE=\"$PWD/qurl-proxy-${QURL_CONNECTOR_ID}.yaml\"\nQURL_COMPOSE_FILE=\"$PWD/qurl-connector-${QURL_CONNECTOR_ID}.compose.yaml\"\n\ncat > \"$CONFIG_FILE\" <<'QURL_PROXY_YAML_EOF'\nroutes:\n  - id: 'prod-dashboard'\n    type: http\n    local_ip: 127.0.0.1\n    local_port: 8080\n    resource_id: 'MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE2cTVv5_3eeYCcLLq5ROYCqcmY50HiKZ9ATglIkPnCji1E_S63UMtXba1moR8-Q6EV7oM6zwwh9_j2CDujzXvLA'\n    connector_routing_id: 'c-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'\nQURL_PROXY_YAML_EOF\n# The generated config contains only client-safe public/routing metadata; 0644\n# lets the nonroot Connector (UID 65532) read the bind mount. Its bootstrap\n# credential stays separately protected in the 0600 api_key file below.\n$SUDO chmod 0644 \"$CONFIG_FILE\"\n\n$SUDO install -d -m 0700 -o 65532 -g 65532 \"$SECRET_DIR\"\n$SUDO install -d -m 0700 -o 65532 -g 65532 \"$AGENT_STATE_DIR\"\n$SUDO install -d -m 0700 -o 65532 -g 65532 \"$AUDIT_DIR\"\nif [ -z \"${QURL_BOOTSTRAP_KEY:-}\" ]; then\n  if [ ! -t 0 ]; then\n    echo \"Set QURL_BOOTSTRAP_KEY or run this block from an interactive terminal.\" >&2\n    exit 1\n  fi\n  printf 'Paste qURL enrollment token (input hidden): ' >&2\n  STTY_STATE=\"$(stty -g 2>/dev/null | tr -d '[:space:]' || true)\"\n  if [ -n \"$STTY_STATE\" ]; then\n    stty -echo\n    trap 'if [ -n \"$STTY_STATE\" ]; then stty \"$STTY_STATE\" 2>/dev/null || true; fi' INT TERM EXIT\n  fi\n  if ! IFS= read -r QURL_BOOTSTRAP_KEY; then\n    if [ -n \"$STTY_STATE\" ]; then\n      stty \"$STTY_STATE\"\n      trap - INT TERM EXIT\n    fi\n    printf '\\n' >&2\n    echo \"Enrollment token is required.\" >&2\n    exit 1\n  fi\n  if [ -n \"$STTY_STATE\" ]; then\n    stty \"$STTY_STATE\"\n    trap - INT TERM EXIT\n  fi\n  printf '\\n' >&2\nfi\nif [ -z \"$QURL_BOOTSTRAP_KEY\" ]; then\n  echo \"Enrollment token is required.\" >&2\n  exit 1\nfi\nQURL_BOOTSTRAP_KEY_LEN=${#QURL_BOOTSTRAP_KEY}\n$SUDO sh -c 'set -eu\numask 077\nhead -c \"$2\" > \"$1\"\nchown 65532:65532 \"$1\"\nchmod 0400 \"$1\"\n' _ \"$SECRET_DIR/api_key\" \"$QURL_BOOTSTRAP_KEY_LEN\" <<QURL_BOOTSTRAP_KEY_EOF\n$QURL_BOOTSTRAP_KEY\nQURL_BOOTSTRAP_KEY_EOF\nunset QURL_BOOTSTRAP_KEY QURL_BOOTSTRAP_KEY_LEN\n\n# This heredoc is intentionally unquoted so it expands the validated variables\n# now and writes a static per-connector Compose fragment. Future compose commands\n# do not need WEB_SERVICE exported unless you regenerate the fragment.\n# If you edit this generated file by hand later, rerun the install instead of\n# adding new shell variables here.\ncat > \"$QURL_COMPOSE_FILE\" <<QURL_COMPOSE_YAML_EOF\nservices:\n  'qurl-connector-prod-dashboard':\n    image: 'ghcr.io/layervai/qurl-connector:v-test'\n    user: \"65532:65532\"\n    restart: on-failure:5\n    read_only: true\n    tmpfs:\n      - /tmp:rw,size=64m\n    pids_limit: 512\n    cap_drop:\n      - ALL\n    security_opt:\n      - 'no-new-privileges:true'\n    network_mode: \"service:${WEB_SERVICE}\"\n    depends_on:\n      ${WEB_SERVICE}:\n        condition: service_started\n    volumes:\n      - ${AGENT_STATE_DIR}:/var/lib/layerv/agent\n      - ${AUDIT_DIR}:/var/log/layerv/qurl-connector\n      - ${SECRET_DIR}:/run/secrets/qurl-connector:ro\n      - ./qurl-proxy-${QURL_CONNECTOR_ID}.yaml:/work/qurl-proxy.yaml:ro\n    environment:\n      QURL_API_KEY_FILE: /run/secrets/qurl-connector/api_key\n      QURL_AUDIT_FILE: /var/log/layerv/qurl-connector/audit.log\n      QURL_CONNECTOR_ID: ${QURL_CONNECTOR_ID}\n      QURL_API_URL: ${QURL_API_URL_YAML}\nQURL_COMPOSE_YAML_EOF\n\ndocker compose -f \"$APP_COMPOSE_FILE\" -f \"$QURL_COMPOSE_FILE\" up -d \"$CONNECTOR_SERVICE\"\n```\n\nVerify with `docker compose -f compose.yaml -f qurl-connector-prod-dashboard.compose.yaml logs -f qurl-connector-prod-dashboard`; if you changed `APP_COMPOSE_FILE`, use that file there too. After the qURL Connector connects, delete the enrollment-token file."

func TestRenderDockerComposeTunnelInstructionsHubTrustUnsetOutputUnchanged(t *testing.T) {
	t.Parallel()
	got := mustRenderDockerComposeTunnelInstructions(t, testTunnelInstallArgs(), testTunnelImageRef)
	if got != wantComposeHubUnsetGolden {
		t.Fatalf("Docker Compose instructions changed with HubTrust unset:\ngot:\n%s\nwant:\n%s", got, wantComposeHubUnsetGolden)
	}
}
