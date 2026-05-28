package internal

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/shared/client"
)

func TestRenderECSFargateTunnelInstructions(t *testing.T) {
	t.Parallel()
	got := renderECSFargateTunnelInstructions(&tunnelInstallArgs{
		Slug:        testTunnelSlug,
		Alias:       testTunnelSlug,
		LocalPort:   9090,
		Environment: tunnelEnvECSFargate,
	}, &client.APIKey{APIKey: testTunnelAPIKey}, testTunnelImageRef)

	for _, want := range []string{
		"ECS/Fargate task-definition checklist",
		"non-essential sidecar container",
		"Fargate's awsvpc network mode",
		"Replace `<region>`, `<account-id>`, and `<suffix>`",
		"AWS appends a random suffix",
		"127.0.0.1:9090",
		"AWS Secrets Manager",
		"Store the bootstrap key shown above",
		"ECS injects the bootstrap secret as `QURL_API_KEY`",
		"file-mounted secret runtimes use `QURL_API_KEY_FILE` instead",
		"secret as `qurl-tunnel-" + testTunnelSlug + "`",
		"treat this Slack message as secret until the sidecar connects",
		testTunnelImageRef,
		"Put qurl-proxy.yaml at `/work/qurl-proxy.yaml` on an EFS access point",
		"mounted into the task as the `qurl-config` volume",
		testTunnelLocalPort9090Line,
		`"name": "QURL_TUNNEL_SLUG"`,
		`"value": "` + testTunnelSlug + `"`,
		`"name": "QURL_API_KEY"`,
		`"sourceVolume": "qurl-agent-state"`,
		`"sourceVolume": "qurl-config"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("ECS instructions missing %q:\n%s", want, got)
		}
	}
	for _, forbidden := range []string{testForbiddenSlackYAMLFence, testForbiddenSlackShellFence, testForbiddenResourceLabel, testTunnelResourceID, testTunnelAPIKey} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("ECS instructions leaked %q:\n%s", forbidden, got)
		}
	}
	if gotFenceCount := strings.Count(got, "```"); gotFenceCount != 4 {
		t.Fatalf("ECS instructions rendered %d code fences, want 4 for two independently copyable artifacts:\n%s", gotFenceCount, got)
	}

	var container ecsContainerDefinition
	if err := json.Unmarshal([]byte(renderECSSidecarContainerJSON(&tunnelInstallArgs{
		Slug:        testTunnelSlug,
		Alias:       testTunnelSlug,
		LocalPort:   9090,
		Environment: tunnelEnvECSFargate,
	}, testTunnelImageRef)), &container); err != nil {
		t.Fatalf("ECS sidecar JSON did not parse: %v", err)
	}
	if container.Essential {
		t.Fatal("ECS sidecar Essential = true, want false so the tunnel does not take down the app task")
	}
	if len(container.Secrets) != 1 || container.Image != testTunnelImageRef || container.Secrets[0].Name != tunnelEnvAPIKey {
		t.Fatalf("ECS sidecar = %+v, want image and bootstrap secret wiring", container)
	}
	if !strings.Contains(container.Secrets[0].ValueFrom, "-<suffix>") {
		t.Fatalf("ECS secret ValueFrom = %q, want full Secrets Manager ARN suffix placeholder", container.Secrets[0].ValueFrom)
	}
}
