package internal

import (
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/shared/client"
)

func TestRenderDockerTunnelInstructionsUsesWebRef(t *testing.T) {
	t.Parallel()
	got := renderDockerTunnelInstructions(&tunnelInstallArgs{
		Slug:        testTunnelSlug,
		Alias:       testTunnelSlug,
		LocalPort:   9090,
		Environment: tunnelEnvDocker,
		WebRef:      "web.1_2-3",
	}, &client.APIKey{APIKey: testTunnelAPIKey}, testTunnelImageRef)

	for _, want := range []string{
		testTunnelKeyHistoryNote,
		"WEB_CONTAINER='web.1_2-3'",
		`CONFIG_FILE="$PWD/qurl-proxy-${QURL_TUNNEL_SLUG}.yaml"`,
		testTunnelKeyPromptLine,
		testTunnelKeyInstallLine,
		`--network "container:${WEB_CONTAINER}"`,
		"restart the tunnel after replacing or recreating the web container",
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
	if strings.Contains(got, testTunnelAPIKey) {
		t.Fatalf("Docker instructions embedded bootstrap key instead of prompting:\n%s", got)
	}
}
