package internal

import (
	"strings"
	"testing"
)

func TestRenderDockerTunnelInstructionsUsesWebRef(t *testing.T) {
	t.Parallel()
	got := mustRenderDockerTunnelInstructions(t, &tunnelInstallArgs{
		Slug:            testTunnelSlug,
		Alias:           testTunnelSlug,
		LocalPort:       9090,
		Environment:     tunnelEnvDocker,
		WebRef:          "web.1_2-3",
		ResourceID:      testTunnelResourceID,
		KnockResourceID: testTunnelKnockID,
	}, testTunnelImageRef)

	for _, want := range []string{
		testTunnelKeyHistoryNote,
		testTunnelPipefailLine,
		"sudo -n true",
		"configure passwordless sudo",
		"WEB_CONTAINER='web.1_2-3'",
		"WEB_CONTAINER may contain only letters, numbers, dots, underscores, and hyphens.",
		`CONFIG_FILE="$PWD/qurl-proxy-${QURL_CONNECTOR_ID}.yaml"`,
		testTunnelKeyPromptLine,
		testTunnelKeyInstallLine,
		"resource_id: '" + testTunnelResourceID + "'",
		"LAYERV_KNOCK_RESOURCE_ID='" + testTunnelKnockID + "'",
		`--network "container:${WEB_CONTAINER}"`,
		`-e LAYERV_KNOCK_RESOURCE_ID="$LAYERV_KNOCK_RESOURCE_ID"`,
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
	for _, forbidden := range []string{testTunnelAPIKey, testForbiddenBootstrapArgv} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("Docker instructions leaked %q:\n%s", forbidden, got)
		}
	}
}
