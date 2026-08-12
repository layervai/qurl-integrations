package internal

import (
	"strings"
	"testing"
)

func TestRenderDockerTunnelInstructionsUsesWebRef(t *testing.T) {
	t.Parallel()
	got := mustRenderDockerTunnelInstructions(t, &tunnelInstallArgs{
		Slug:               testTunnelSlug,
		Alias:              testTunnelSlug,
		LocalPort:          9090,
		Environment:        tunnelEnvDocker,
		WebRef:             "web.1_2-3",
		ResourceID:         testTunnelResourceID,
		ConnectorRoutingID: testTunnelRoutingID,
		KnockResourceID:    testTunnelKnockID,
		APIURL:             testTunnelAPIURL,
	}, testTunnelImageRef)

	for _, want := range []string{
		testTunnelKeyHistoryNote,
		testTunnelPipefailLine,
		"sudo -n true",
		"configure passwordless sudo",
		"WEB_CONTAINER='web.1_2-3'",
		"WEB_CONTAINER may contain only letters, numbers, dots, underscores, and hyphens.",
		`CONFIG_FILE="$PWD/qurl-proxy-${QURL_CONNECTOR_ID}.yaml"`,
		"client-safe public/routing metadata",
		`$SUDO chmod 0644 "$CONFIG_FILE"`,
		`AUDIT_DIR="/var/log/layerv/qurl-connector/${QURL_CONNECTOR_ID}"`,
		`$SUDO install -d -m 0700 -o 65532 -g 65532 "$AUDIT_DIR"`,
		testTunnelKeyPromptLine,
		testTunnelKeyInstallLine,
		"resource_id: '" + testTunnelResourceID + "'",
		`--network "container:${WEB_CONTAINER}"`,
		"--read-only",
		"--tmpfs /tmp:rw,size=64m",
		"--cap-drop=ALL",
		"--security-opt=no-new-privileges:true",
		"--pids-limit=512",
		`-v "$AUDIT_DIR:/var/log/layerv/qurl-connector"`,
		"-e QURL_AUDIT_FILE='/var/log/layerv/qurl-connector/audit.log'",
		"Re-running this install briefly restarts the qURL Connector container",
		"restart the qURL Connector after replacing or recreating the web container",
		testTunnelDockerLine,
		testTunnelAgentDirFragment,
		testTunnelLocalPort9090Line,
		testTunnelImageRef,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("Docker instructions missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Replace `YOUR_WEB_CONTAINER_NAME`") {
		t.Fatalf("Docker instructions still included placeholder warning:\n%s", got)
	}
	for _, forbidden := range []string{testTunnelAPIKey, testForbiddenBootstrapArgv, "QURL_BOOTSTRAP_URL", "knock_resource_id", "LAYERV_KNOCK_RESOURCE_ID"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("Docker instructions leaked %q:\n%s", forbidden, got)
		}
	}
}

func TestRenderDockerTunnelInstructionsShellQuotesAPIURL(t *testing.T) {
	t.Parallel()
	args := testTunnelInstallArgs()
	args.APIURL = testShellSignificantTunnelAPIURL

	got := mustRenderDockerTunnelInstructions(t, args, testTunnelImageRef)
	quoted := shellSingleQuote(args.APIURL)
	for _, name := range []string{"QURL_API_URL"} {
		if !strings.Contains(got, "-e "+name+"="+quoted) {
			t.Fatalf("Docker instructions did not shell-quote %s:\n%s", name, got)
		}
	}
	if strings.Contains(got, "QURL_BOOTSTRAP_URL") {
		t.Fatalf("Docker instructions rendered retired bootstrap URL:\n%s", got)
	}
}

func TestRenderDockerTunnelInstructionsHubTrustSet(t *testing.T) {
	t.Parallel()
	args := testTunnelInstallArgs()
	args.HubTrust = testTunnelHubTrust()

	got := mustRenderDockerTunnelInstructions(t, args, testTunnelImageRef)

	want := "  -e QURL_CONNECTOR_ID=\"$QURL_CONNECTOR_ID\" \\\n" +
		"  -e QURL_CONNECTOR_HUB_HOST='" + testTunnelHubHost + "' \\\n" +
		"  -e QURL_CONNECTOR_HUB_PORT='" + testTunnelHubPort + "' \\\n" +
		"  -e QURL_CONNECTOR_HUB_SERVER_PUBLIC_KEY_B64='" + testTunnelHubKeyB64 + "' \\\n" +
		"  -e QURL_API_URL='" + testTunnelAPIURL + "' \\\n"
	if !strings.Contains(got, want) {
		t.Fatalf("Docker instructions missing Hub trust env block:\n%s", got)
	}
}

// wantDockerHubUnsetGolden is byte-for-byte what renderDockerTunnelInstructions
// produced for testTunnelInstallArgs()+testTunnelImageRef before Hub trust
// passthrough existed, captured mechanically (not hand-transcribed) to prove
// the unset path is unchanged rather than assume it.
const wantDockerHubUnsetGolden = "Run this whole block on the Linux Docker host where your local HTTP server container is running. It prompts for the enrollment token so the secret does not land in shell history; use a trusted host and shell because local administrators can inspect process state during setup. If your terminal echoes pasted input, stop and use a platform secret manager instead. Replace the value inside `WEB_CONTAINER='YOUR_WEB_CONTAINER_NAME'` first; keep the quotes. It writes or overwrites the qURL Connector's qurl-proxy config in the current directory. Re-running this install briefly restarts the qURL Connector container if it already exists. Because the qURL Connector shares the web container's network namespace, restart the qURL Connector after replacing or recreating the web container.\n\n```\nset -eu\nif (set -o pipefail) 2>/dev/null; then\n  set -o pipefail\nfi\n\nif [ \"$(id -u)\" -eq 0 ]; then\n  SUDO=\"\"\nelif command -v sudo >/dev/null 2>&1 && sudo -n true 2>/dev/null; then\n  SUDO=\"sudo -n\"\nelse\n  echo \"Run as root or configure passwordless sudo so the state and secret directories can be owned by UID 65532.\" >&2\n  exit 1\nfi\n\n# Keep this placeholder assignment so the block is pasteable; the guard below\n# fails before writing files until the operator replaces the quoted value.\nWEB_CONTAINER='YOUR_WEB_CONTAINER_NAME'\nif [ \"$WEB_CONTAINER\" = \"YOUR_WEB_CONTAINER_NAME\" ] || [ -z \"$WEB_CONTAINER\" ]; then\n  echo \"Set WEB_CONTAINER to the Docker container name or ID for your local HTTP server.\" >&2\n  exit 1\nfi\ncase \"$WEB_CONTAINER\" in\n  [A-Za-z0-9]*) ;;\n  *)\n    echo \"WEB_CONTAINER must start with a letter or number.\" >&2\n    exit 1\n    ;;\nesac\ncase \"$WEB_CONTAINER\" in\n  *[!A-Za-z0-9_.-]*)\n    echo \"WEB_CONTAINER may contain only letters, numbers, dots, underscores, and hyphens.\" >&2\n    exit 1\n    ;;\nesac\n\nQURL_CONNECTOR_ID='prod-dashboard'\nCONNECTOR_CONTAINER=\"qurl-connector-${QURL_CONNECTOR_ID}\"\nSECRET_DIR=\"/run/secrets/qurl-connector/${QURL_CONNECTOR_ID}\"\nAGENT_STATE_DIR=\"/var/lib/layerv/qurl-connector/${QURL_CONNECTOR_ID}/agent\"\nAUDIT_DIR=\"/var/log/layerv/qurl-connector/${QURL_CONNECTOR_ID}\"\nCONFIG_FILE=\"$PWD/qurl-proxy-${QURL_CONNECTOR_ID}.yaml\"\n\n# This intentionally overwrites the per-connector config so rerunning the install\n# refreshes the deterministic ID and port values in place.\ncat > \"$CONFIG_FILE\" <<'QURL_PROXY_YAML_EOF'\nroutes:\n  - id: 'prod-dashboard'\n    type: http\n    local_ip: 127.0.0.1\n    local_port: 8080\n    resource_id: 'MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE2cTVv5_3eeYCcLLq5ROYCqcmY50HiKZ9ATglIkPnCji1E_S63UMtXba1moR8-Q6EV7oM6zwwh9_j2CDujzXvLA'\n    connector_routing_id: 'c-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'\nQURL_PROXY_YAML_EOF\n# The generated config contains only client-safe public/routing metadata; 0644\n# lets the nonroot Connector (UID 65532) read the bind mount. Its bootstrap\n# credential stays separately protected in the 0600 api_key file below.\n$SUDO chmod 0644 \"$CONFIG_FILE\"\n\n$SUDO install -d -m 0700 -o 65532 -g 65532 \"$SECRET_DIR\"\n$SUDO install -d -m 0700 -o 65532 -g 65532 \"$AGENT_STATE_DIR\"\n$SUDO install -d -m 0700 -o 65532 -g 65532 \"$AUDIT_DIR\"\nif [ -z \"${QURL_BOOTSTRAP_KEY:-}\" ]; then\n  if [ ! -t 0 ]; then\n    echo \"Set QURL_BOOTSTRAP_KEY or run this block from an interactive terminal.\" >&2\n    exit 1\n  fi\n  printf 'Paste qURL enrollment token (input hidden): ' >&2\n  STTY_STATE=\"$(stty -g 2>/dev/null | tr -d '[:space:]' || true)\"\n  if [ -n \"$STTY_STATE\" ]; then\n    stty -echo\n    trap 'if [ -n \"$STTY_STATE\" ]; then stty \"$STTY_STATE\" 2>/dev/null || true; fi' INT TERM EXIT\n  fi\n  if ! IFS= read -r QURL_BOOTSTRAP_KEY; then\n    if [ -n \"$STTY_STATE\" ]; then\n      stty \"$STTY_STATE\"\n      trap - INT TERM EXIT\n    fi\n    printf '\\n' >&2\n    echo \"Enrollment token is required.\" >&2\n    exit 1\n  fi\n  if [ -n \"$STTY_STATE\" ]; then\n    stty \"$STTY_STATE\"\n    trap - INT TERM EXIT\n  fi\n  printf '\\n' >&2\nfi\nif [ -z \"$QURL_BOOTSTRAP_KEY\" ]; then\n  echo \"Enrollment token is required.\" >&2\n  exit 1\nfi\nQURL_BOOTSTRAP_KEY_LEN=${#QURL_BOOTSTRAP_KEY}\n$SUDO sh -c 'set -eu\numask 077\nhead -c \"$2\" > \"$1\"\nchown 65532:65532 \"$1\"\nchmod 0400 \"$1\"\n' _ \"$SECRET_DIR/api_key\" \"$QURL_BOOTSTRAP_KEY_LEN\" <<QURL_BOOTSTRAP_KEY_EOF\n$QURL_BOOTSTRAP_KEY\nQURL_BOOTSTRAP_KEY_EOF\nunset QURL_BOOTSTRAP_KEY QURL_BOOTSTRAP_KEY_LEN\n\nif docker ps -a --format '{{.Names}}' | grep -Fxq \"$CONNECTOR_CONTAINER\"; then\n  docker rm -f \"$CONNECTOR_CONTAINER\" >/dev/null\nfi\n\ndocker run -d \\\n  --name \"$CONNECTOR_CONTAINER\" \\\n  --network \"container:${WEB_CONTAINER}\" \\\n  --restart=on-failure:5 \\\n  --read-only \\\n  --tmpfs /tmp:rw,size=64m \\\n  --cap-drop=ALL \\\n  --security-opt=no-new-privileges:true \\\n  --pids-limit=512 \\\n  -v \"$AGENT_STATE_DIR:/var/lib/layerv/agent\" \\\n  -v \"$AUDIT_DIR:/var/log/layerv/qurl-connector\" \\\n  -v \"$SECRET_DIR:$SECRET_DIR:ro\" \\\n  -v \"$CONFIG_FILE:/work/qurl-proxy.yaml:ro\" \\\n  -e QURL_API_KEY_FILE=\"$SECRET_DIR/api_key\" \\\n  -e QURL_AUDIT_FILE='/var/log/layerv/qurl-connector/audit.log' \\\n  -e QURL_CONNECTOR_ID=\"$QURL_CONNECTOR_ID\" \\\n  -e QURL_API_URL='https://api.sandbox.example/v1' \\\n  'ghcr.io/layervai/qurl-connector:v-test'\n```\n\nVerify with `docker logs -f qurl-connector-prod-dashboard`; after the qURL Connector connects, delete the enrollment-token file."

func TestRenderDockerTunnelInstructionsHubTrustUnsetOutputUnchanged(t *testing.T) {
	t.Parallel()
	got := mustRenderDockerTunnelInstructions(t, testTunnelInstallArgs(), testTunnelImageRef)
	if got != wantDockerHubUnsetGolden {
		t.Fatalf("Docker instructions changed with HubTrust unset:\ngot:\n%s\nwant:\n%s", got, wantDockerHubUnsetGolden)
	}
}
