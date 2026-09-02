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

func TestHubTrustEnvFailsClosedOnMissingOrAmbiguousAnchor(t *testing.T) {
	t.Parallel()
	dockerAnchor := "  -e QURL_ENDPOINT='https://api.example' \\\n"
	composeAnchor := "      QURL_ENDPOINT: ${QURL_ENDPOINT_YAML}\n"
	k8sAnchor := "      - name: QURL_ENDPOINT\n        value: 'https://api.example'\n"
	for _, tc := range []struct {
		name     string
		render   func(string, hubTrust) (string, error)
		rendered string
		want     string
	}{
		{name: "docker missing", render: withHubTrustDockerEnv, rendered: "docker run img", want: "found 0"},
		{name: "docker ambiguous", render: withHubTrustDockerEnv, rendered: dockerAnchor + dockerAnchor, want: "found 2"},
		{name: "compose ambiguous", render: withHubTrustComposeEnv, rendered: composeAnchor + "x:\n" + composeAnchor, want: "found 2"},
		{name: "kubernetes missing", render: withHubTrustKubernetesEnv, rendered: "env: []", want: "found 0"},
		{name: "kubernetes ambiguous", render: withHubTrustKubernetesEnv, rendered: k8sAnchor + k8sAnchor, want: "found 2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := tc.render(tc.rendered, testHub); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestHubTrustEnvValueIsSplicedLiterally(t *testing.T) {
	t.Parallel()
	// "$1" must survive: the splice must not run through a regexp
	// replacement template. (The startup validator rejects such hosts; this
	// pins the renderer independently.)
	h := hubTrust{Host: "hub$1.example", Port: "443", ServerPublicKeyB64: testHub.ServerPublicKeyB64}
	got, err := withHubTrustDockerEnv("  -e QURL_ENDPOINT='https://api.example' \\\n  img", h)
	if err != nil || !strings.Contains(got, "-e QURL_CONNECTOR_HUB_HOST='hub$1.example' \\") {
		t.Fatalf("got %q, %v; want literal host", got, err)
	}
}

func TestHubTrustPartialTripleNeverRenders(t *testing.T) {
	t.Parallel()
	partial := hubTrust{Host: "hub.nhp.example"}
	if _, err := withHubTrustDockerEnv("  -e QURL_ENDPOINT='x' \\\n", partial); err == nil || !strings.Contains(err.Error(), "partial triple") {
		t.Fatalf("docker: err = %v, want partial-triple failure", err)
	}
	if _, err := hubTrustECSEnv(partial); err == nil || !strings.Contains(err.Error(), "partial triple") {
		t.Fatalf("ecs: err = %v, want partial-triple failure", err)
	}
}

func TestPrepareTunnelInstallMessageThreadsImageVersion(t *testing.T) {
	ts := newAdminTestServers(t)
	h := newAdminTestHandler(t, ts)
	h.cfg.TunnelImage = testTunnelImageRef
	h.cfg.TunnelImageVersion = "v2.1.1"
	prepared, err := h.prepareTunnelInstallMessage(testTunnelInstallArgs())
	if err != nil {
		t.Fatalf("prepareTunnelInstallMessage: %v", err)
	}
	if want := "Sidecar image: `" + testTunnelImageRef + "` (qurl v2.1.1)."; prepared.imageLine != want {
		t.Fatalf("imageLine = %q, want %q", prepared.imageLine, want)
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
		assertHubEnv(t, "s3 kubernetes", k8s, configured, "      - name: QURL_CONNECTOR_HUB_HOST\n        value: 'hub.nhp.example'", "      - name: QURL_CONNECTOR_HUB_PORT\n        value: '443'")
		ecs, err := renderS3WebsiteECSContainerJSON(args, testTunnelImageRef, testS3OriginImageRef)
		if err != nil {
			t.Fatalf("ecs: %v", err)
		}
		assertHubEnv(t, "s3 ecs", ecs, configured, `"name": "QURL_CONNECTOR_HUB_HOST"`, `"value": "hub.nhp.example"`)
	}
}
