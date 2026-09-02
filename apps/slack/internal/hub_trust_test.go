package internal

import (
	"strings"
	"testing"
)

var testHub = hubTrust{Host: "hub.nhp.example", Port: "443", ServerPublicKeyB64: "qmvYisCByN6gTC89Pp6hzBEoYajNDnHj2HgdWf4LOkY="}

func assertHubEnv(t *testing.T, name, got string, want bool, lines ...string) {
	t.Helper()
	for _, line := range lines {
		if strings.Contains(got, line) != want {
			t.Fatalf("%s: contains %q = %v, want %v:\n%s", name, line, !want, want, got)
		}
	}
}

func TestRenderedTunnelInstallsCarryHubTrustEnv(t *testing.T) {
	t.Parallel()
	for _, configured := range []bool{true, false} {
		args := testTunnelInstallArgs()
		if configured {
			args.Hub = testHub
		}
		docker := mustRenderDockerTunnelInstructions(t, args, testTunnelImageRef)
		assertHubEnv(t, "docker", docker, configured, "-e QURL_CONNECTOR_HUB_HOST='hub.nhp.example' \\", "-e QURL_CONNECTOR_HUB_PORT='443' \\", "-e QURL_CONNECTOR_HUB_SERVER_PUBLIC_KEY_B64='qmvYisCByN6gTC89Pp6hzBEoYajNDnHj2HgdWf4LOkY=' \\")
		compose := mustRenderDockerComposeTunnelInstructions(t, args, testTunnelImageRef)
		assertHubEnv(t, "compose", compose, configured, "      QURL_CONNECTOR_HUB_HOST: 'hub.nhp.example'", "      QURL_CONNECTOR_HUB_PORT: '443'", "      QURL_CONNECTOR_HUB_SERVER_PUBLIC_KEY_B64: 'qmvYisCByN6gTC89Pp6hzBEoYajNDnHj2HgdWf4LOkY='")
		k8s := mustRenderKubernetesTunnelInstructions(t, args, testTunnelImageRef)
		assertHubEnv(t, "kubernetes", k8s, configured, "      - name: QURL_CONNECTOR_HUB_HOST\n        value: 'hub.nhp.example'", "      - name: QURL_CONNECTOR_HUB_PORT\n        value: '443'", "      - name: QURL_CONNECTOR_HUB_SERVER_PUBLIC_KEY_B64\n")
		ecs := mustRenderECSFargateTunnelInstructions(t, args, testTunnelImageRef)
		assertHubEnv(t, "ecs", ecs, configured, `"name": "QURL_CONNECTOR_HUB_HOST"`, `"value": "hub.nhp.example"`, `"name": "QURL_CONNECTOR_HUB_SERVER_PUBLIC_KEY_B64"`)
	}
}

func TestHubTrustEnvFailsClosedWithoutEndpointAnchor(t *testing.T) {
	t.Parallel()
	if _, err := withHubTrustDockerEnv("docker run img", testHub); err == nil || !strings.Contains(err.Error(), "found 0") {
		t.Fatalf("err = %v, want missing-anchor failure", err)
	}
}

func TestSidecarImageLine(t *testing.T) {
	t.Parallel()
	if got := sidecarImageLine("ghcr.io/layervai/qurl@sha256:abc", "v2.1.1"); got != "Sidecar image: `ghcr.io/layervai/qurl@sha256:abc` (qurl v2.1.1)." {
		t.Fatalf("with version = %q", got)
	}
	if got := sidecarImageLine("ghcr.io/layervai/qurl@sha256:abc", ""); got != "Sidecar image: `ghcr.io/layervai/qurl@sha256:abc`." {
		t.Fatalf("without version = %q", got)
	}
}

func TestRenderedS3WebsiteInstallsCarryHubTrustEnv(t *testing.T) {
	t.Parallel()
	for _, configured := range []bool{true, false} {
		args := testS3WebsiteArgs(tunnelEnvDocker)
		if configured {
			args.Hub = testHub
		}
		docker, err := renderDockerS3WebsiteInstructions(args, testTunnelImageRef, testS3OriginImageRef)
		if err != nil {
			t.Fatalf("docker: %v", err)
		}
		assertHubEnv(t, "s3 docker", docker, configured, "-e QURL_CONNECTOR_HUB_HOST='hub.nhp.example' \\", "-e QURL_CONNECTOR_HUB_PORT='443' \\")
		compose, err := renderDockerComposeS3WebsiteInstructions(args, testTunnelImageRef, testS3OriginImageRef)
		if err != nil {
			t.Fatalf("compose: %v", err)
		}
		assertHubEnv(t, "s3 compose", compose, configured, "      QURL_CONNECTOR_HUB_HOST: 'hub.nhp.example'", "      QURL_CONNECTOR_HUB_SERVER_PUBLIC_KEY_B64: 'qmvYisCByN6gTC89Pp6hzBEoYajNDnHj2HgdWf4LOkY='")
		k8s, err := renderKubernetesS3WebsiteInstructions(args, testTunnelImageRef, testS3OriginImageRef)
		if err != nil {
			t.Fatalf("kubernetes: %v", err)
		}
		assertHubEnv(t, "s3 kubernetes", k8s, configured, "- name: QURL_CONNECTOR_HUB_HOST\n", "- name: QURL_CONNECTOR_HUB_PORT\n")
		ecs, err := renderS3WebsiteECSContainerJSON(args, testTunnelImageRef, testS3OriginImageRef)
		if err != nil {
			t.Fatalf("ecs: %v", err)
		}
		assertHubEnv(t, "s3 ecs", ecs, configured, `"name": "QURL_CONNECTOR_HUB_HOST"`, `"value": "hub.nhp.example"`)
	}
}
