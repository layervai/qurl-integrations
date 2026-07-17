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
		"sudo -n true",
		"configure passwordless sudo",
		"WEB_CONTAINER='web.1_2-3'",
		"WEB_CONTAINER may contain only letters, numbers, dots, underscores, and hyphens.",
		`CONFIG_FILE="$PWD/qurl-proxy-${QURL_CONNECTOR_ID}.yaml"`,
		"client-safe public/routing metadata",
		`$SUDO chmod 0644 "$CONFIG_FILE"`,
		testTunnelKeyPromptLine,
		testTunnelKeyInstallLine,
		`--network "container:${WEB_CONTAINER}"`,
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
	for _, forbidden := range []string{testTunnelAPIKey, testForbiddenBootstrapArgv, "QURL_BOOTSTRAP_URL"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("Docker instructions leaked %q:\n%s", forbidden, got)
		}
	}
}

func TestRenderDockerTunnelInstructionsShellQuotesAPIURL(t *testing.T) {
	t.Parallel()
	args := testPinnedTunnelInstallArgs()
	args.APIURL = "https://api.$(touch-should-not-run).example.test/v1"

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
