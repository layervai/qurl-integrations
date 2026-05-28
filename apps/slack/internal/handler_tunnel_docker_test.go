package internal

import (
	"strings"
	"testing"
)

func TestRenderDockerTunnelInstructionsUsesWebRef(t *testing.T) {
	t.Parallel()
	got := mustRenderDockerTunnelInstructions(t, &tunnelInstallArgs{
		Slug:        testTunnelSlug,
		Alias:       testTunnelSlug,
		LocalPort:   9090,
		Environment: tunnelEnvDocker,
		WebRef:      "web.1_2-3",
	}, testTunnelImageRef)

	for _, want := range []string{
		testTunnelKeyHistoryNote,
		testTunnelPipefailLine,
		"WEB_CONTAINER='web.1_2-3'",
		"WEB_CONTAINER may contain only letters, numbers, dots, underscores, and hyphens.",
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
	for _, forbidden := range []string{testTunnelAPIKey, testForbiddenBootstrapArgv} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("Docker instructions leaked %q:\n%s", forbidden, got)
		}
	}
}
