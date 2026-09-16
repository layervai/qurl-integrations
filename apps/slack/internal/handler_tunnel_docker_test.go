package internal

import (
	"strings"
	"testing"
)

func TestRenderDockerTunnelInstructionsUsesWebRef(t *testing.T) {
	t.Parallel()
	args := testTunnelInstallArgs()
	args.LocalPort = 9090
	args.Environment = tunnelEnvDocker
	args.WebRef = "web.1_2-3"
	got := mustRenderDockerTunnelInstructions(t, args, testTunnelImageRef)

	for _, want := range []string{
		testTunnelKeyHistoryNote,
		testTunnelPipefailLine,
		"sudo -n true",
		"configure passwordless sudo",
		"WEB_CONTAINER='web.1_2-3'",
		"WEB_CONTAINER may contain only letters, numbers, dots, underscores, and hyphens.",
		`CONFIG_FILE="$PWD/qurl-share-${QURL_CONNECTOR_ID}.yaml"`,
		"client-safe public/routing metadata",
		`$SUDO chmod 0644 "$CONFIG_FILE"`,
		testTunnelKeyPromptLine,
		testTunnelKeyInstallLine,
		"crid: '" + testTunnelCRID + "'",
		"resource_id: '" + testTunnelResourceID + "'",
		"knock_resource_id: '" + testTunnelKnockID + "'",
		"desired_state: on",
		"serving_epoch: 1",
		`--network "container:${WEB_CONTAINER}"`,
		"--restart=unless-stopped",
		"--read-only",
		"--tmpfs /tmp:rw,size=64m",
		"--cap-drop=ALL",
		"--security-opt=no-new-privileges:true",
		"--pids-limit=512",
		`-v "$AGENT_STATE_DIR:/var/lib/qurl"`,
		`-v "$SECRET_DIR:/run/secrets/qurl:ro"`,
		`-v "$CONFIG_FILE:/etc/qurl/share.yaml:ro"`,
		"--entrypoint /usr/local/bin/qurl",
		"daemon run",
		"--headless-config /etc/qurl/share.yaml",
		"--enrollment-token-file /run/secrets/qurl/enrollment-token",
		"Re-running this install briefly restarts the qurl container",
		"restart qurl after replacing or recreating the web container",
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
	for _, forbidden := range []string{testTunnelAPIKey, testForbiddenBootstrapArgv, "QURL_BOOTSTRAP_URL", "LAYERV_KNOCK_RESOURCE_ID", "ghcr.io/layervai/qurl-connector", "/usr/local/bin/qurl-connector"} {
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
	endpoint, err := qurlEndpointFromConnectorAPIURL(args.APIURL)
	if err != nil {
		t.Fatalf("qurlEndpointFromConnectorAPIURL: %v", err)
	}
	if !strings.Contains(got, "-e QURL_ENDPOINT="+shellSingleQuote(endpoint)) {
		t.Fatalf("Docker instructions did not shell-quote QURL_ENDPOINT:\n%s", got)
	}
	if strings.Contains(got, "QURL_BOOTSTRAP_URL") {
		t.Fatalf("Docker instructions rendered retired bootstrap URL:\n%s", got)
	}
}
