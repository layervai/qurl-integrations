package internal

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/shared/client"
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
		assertHubEnv(t, "compose", compose, configured, "QURL_CONNECTOR_HUB_HOST_YAML="+shellSingleQuote("'hub.nhp.example'"), "QURL_CONNECTOR_HUB_PORT_YAML="+shellSingleQuote("'443'"), "      QURL_CONNECTOR_HUB_HOST: ${QURL_CONNECTOR_HUB_HOST_YAML}", "      QURL_CONNECTOR_HUB_PORT: ${QURL_CONNECTOR_HUB_PORT_YAML}", "      QURL_CONNECTOR_HUB_SERVER_PUBLIC_KEY_B64: ${QURL_CONNECTOR_HUB_SERVER_PUBLIC_KEY_B64_YAML}")
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
		{name: "compose ambiguous", render: withHubTrustComposeEnv, rendered: "QURL_ENDPOINT_YAML=x\n" + composeAnchor + "x:\n" + composeAnchor, want: "found 2"},
		{name: "compose missing shell var", render: withHubTrustComposeEnv, rendered: composeAnchor, want: "found 0"},
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
		assertHubEnv(t, "s3 compose", compose, configured, "QURL_CONNECTOR_HUB_HOST_YAML="+shellSingleQuote("'hub.nhp.example'"), "      QURL_CONNECTOR_HUB_HOST: ${QURL_CONNECTOR_HUB_HOST_YAML}", "      QURL_CONNECTOR_HUB_SERVER_PUBLIC_KEY_B64: ${QURL_CONNECTOR_HUB_SERVER_PUBLIC_KEY_B64_YAML}")
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

// TestComposeHubValuesNeverReachTheShell pins the compose layering: values go
// through shell-quoted KEY_YAML variables outside the unquoted heredoc, so a
// "$" in a value is preserved literally rather than expanded on paste.
func TestComposeHubValuesNeverReachTheShell(t *testing.T) {
	t.Parallel()
	h := hubTrust{Host: "hub$1.example", Port: "443", ServerPublicKeyB64: testHub.ServerPublicKeyB64}
	got, err := withHubTrustComposeEnv("QURL_ENDPOINT_YAML='x'\ncat <<EOF\n      QURL_ENDPOINT: ${QURL_ENDPOINT_YAML}\nEOF\n", h)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "QURL_CONNECTOR_HUB_HOST_YAML="+shellSingleQuote("'hub$1.example'")) || strings.Contains(got, "QURL_CONNECTOR_HUB_HOST: 'hub") {
		t.Fatalf("compose block inlined a value into the heredoc:\n%s", got)
	}
}

// TestInstallFlowsRenderConfiguredHubTrust pins that both install entry points
// attach Config.ConnectorHub: an omission at either call site would render an
// install that fails closed at the daemon with no error here.
func TestInstallFlowsRenderConfiguredHubTrust(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.addCustomer(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		respondQURLEnvelope(t, w, map[string]any{
			testKeyResourceID:      testTunnelResourceID,
			testKeyKnockResourceID: testS3WebsiteKnockResource,
			testKeyType:            client.ResourceTypeTunnel,
			testKeySlug:            testTunnelSlug,
			testKeyStatus:          client.StatusActive,
		})
	})
	ts.addCustomer(http.MethodPost, "/v1/api-keys", func(w http.ResponseWriter, _ *http.Request) {
		respondQURLEnvelope(t, w, map[string]any{
			testKeyKeyID:     testTunnelAPIKeyID,
			testKeyAPIKey:    testTunnelModalKey,
			testKeyStatus:    client.StatusActive,
			testKeyKeyType:   client.APIKeyTypeTunnelBootstrap,
			"kind":           client.CredentialKindEnrollmentToken,
			"target":         client.CredentialTargetConnector,
			testKeyExpiresAt: fixedNow.Add(time.Hour).Format(time.RFC3339),
		})
	})
	var responseBodies []string
	responseURL := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		responseBodies = append(responseBodies, string(body))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(responseURL.Close)
	h := newAdminTestHandler(t, ts)
	h.cfg.TunnelImage = testTunnelImageRef
	h.cfg.S3OriginImage = testS3OriginImageRef
	h.cfg.ConnectorHub = testHub
	captureTunnelPostDMSuccess(h)
	h.SetAliasStore(h.cfg.AdminStore)
	h.processS3WebsiteInstall(context.Background(), slog.Default(), testS3WebsiteInstallRequest(responseURL.URL, fixedNow, tunnelEnvDocker))
	if joined := strings.Join(responseBodies, "\n"); !strings.Contains(joined, "QURL_CONNECTOR_HUB_HOST='hub.nhp.example'") {
		t.Fatalf("S3 website install did not render the configured Hub triple:\n%s", joined)
	}
	_, _, async := newAdminSlashInvoker(t, h).invokeAdminAsync(testTunnelInstallCmd, testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "QURL_CONNECTOR_HUB_HOST") {
		t.Fatalf("tunnel install did not render the configured Hub triple: %q", async)
	}
}
