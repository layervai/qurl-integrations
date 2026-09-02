package internal

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/shared/client"
)

func mustHubTrust(host, port, key string) ConnectorHubTrust {
	h, err := NewConnectorHubTrust(host, port, key)
	if err != nil {
		panic(err)
	}
	return h
}

var testHub = mustHubTrust("hub.nhp.example", "443", "qmvYisCByN6gTC89Pp6hzBEoYajNDnHj2HgdWf4LOkY=")

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
		t.Run(fmt.Sprintf("configured=%v", configured), func(t *testing.T) {
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
		})
	}
}

func TestHubTrustEnvFailsClosedOnMissingOrAmbiguousAnchor(t *testing.T) {
	t.Parallel()
	dockerAnchor := "  -e QURL_ENDPOINT='https://api.example' \\\n"
	composeAnchor := "      QURL_ENDPOINT: ${QURL_ENDPOINT_YAML}\n"
	k8sAnchor := "      - name: QURL_ENDPOINT\n        value: 'https://api.example'\n"
	for _, tc := range []struct {
		name     string
		render   func(string, ConnectorHubTrust) (string, error)
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
	h := ConnectorHubTrust{host: "hub$1.example", port: "443", serverPublicKeyB64: testHub.ServerPublicKeyB64()}
	got, err := withHubTrustDockerEnv("  -e QURL_ENDPOINT='https://api.example' \\\n  img", h)
	if err != nil || !strings.Contains(got, "-e QURL_CONNECTOR_HUB_HOST='hub$1.example' \\") {
		t.Fatalf("got %q, %v; want literal host", got, err)
	}
}

func TestKubernetesDryRunWithConfiguredHub(t *testing.T) {
	t.Parallel()
	kubectl, err := exec.LookPath("kubectl")
	if err != nil {
		t.Skip("kubectl not on PATH")
	}
	args := testTunnelInstallArgs()
	args.Environment = tunnelEnvKubernetes
	args.Hub = testHub
	got := mustRenderKubernetesTunnelInstructions(t, args, testTunnelImageRef)
	fragment := kubernetesPodSpecFragmentFromInstructions(t, got)
	pod := "apiVersion: v1\nkind: Pod\nmetadata:\n  name: qurl-hub-render-test\nspec:\n" + indentLines(fragment, 2) + "\n"
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, kubectl, "apply", "--dry-run=client", "--validate=false", "-f", "-") //nolint:gosec // kubectl path comes from exec.LookPath
	cmd.Stdin = strings.NewReader(pod)
	if out, err := cmd.CombinedOutput(); err != nil {
		skipIfKubectlNeedsClusterDiscovery(t, out)
		t.Fatalf("kubectl dry-run failed with a configured Hub: %v\n%s\n%s", err, out, pod)
	}
}

func TestPrepareTunnelInstallMessageThreadsImageVersion(t *testing.T) {
	ts := newAdminTestServers(t)
	h := newAdminTestHandler(t, ts)
	h.cfg.TunnelImage = testTunnelImageRef
	h.cfg.ConnectorImageVersion = "v2.1.1"
	prepared, err := h.prepareTunnelInstallMessage(testTunnelInstallArgs())
	if err != nil {
		t.Fatalf("prepareTunnelInstallMessage: %v", err)
	}
	if want := "Sidecar image: `" + testTunnelImageRef + "` (`qurl` v2.1.1)."; prepared.imageLine != want {
		t.Fatalf("imageLine = %q, want %q", prepared.imageLine, want)
	}
}

func TestImageVersionSuffix(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, image, version, want string }{
		{name: "digest pin with version", image: "ghcr.io/layervai/qurl@sha256:abc", version: "v2.1.1", want: " (`qurl` v2.1.1)"},
		{name: "digest pin without version", image: "ghcr.io/layervai/qurl@sha256:abc", version: "", want: ""},
		{name: "mutable fallback never labelled", image: "ghcr.io/layervai/qurl:latest", version: "v2.1.1", want: ""},
		{name: "whitespace version ignored", image: "ghcr.io/layervai/qurl@sha256:abc", version: "  ", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := imageVersionSuffix(tc.image, tc.version); got != tc.want {
				t.Fatalf("suffix = %q, want %q", got, tc.want)
			}
		})
	}
	if got := sidecarImageLine("ghcr.io/layervai/qurl@sha256:abc", "v2.1.1"); got != "Sidecar image: `ghcr.io/layervai/qurl@sha256:abc` (`qurl` v2.1.1)." {
		t.Fatalf("sidecar line = %q", got)
	}
}

func TestRenderedS3WebsiteInstallsCarryHubTrustEnv(t *testing.T) {
	t.Parallel()
	for _, configured := range []bool{true, false} {
		t.Run(fmt.Sprintf("configured=%v", configured), func(t *testing.T) {
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
		})
	}
}

// TestComposeHubValuesNeverReachTheShell pins the compose layering: values go
// through shell-quoted KEY_YAML variables outside the unquoted heredoc, so a
// "$" in a value is preserved literally rather than expanded on paste.
func TestComposeHubValuesNeverReachTheShell(t *testing.T) {
	t.Parallel()
	h := ConnectorHubTrust{host: "hub$1.example", port: "443", serverPublicKeyB64: testHub.ServerPublicKeyB64()}
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
	h.processS3WebsiteInstall(context.Background(), slog.Default(), testS3WebsiteInstallRequest(responseURL.URL, fixedNow, tunnelEnvKubernetes))
	if joined := strings.Join(responseBodies, "\n"); !strings.Contains(joined, "QURL_CONNECTOR_HUB_HOST='hub.nhp.example'") || !strings.Contains(joined, "- name: QURL_CONNECTOR_HUB_HOST") {
		t.Fatalf("S3 website install did not render the configured Hub triple:\n%s", joined)
	}
	_, _, async := newAdminSlashInvoker(t, h).invokeAdminAsync(testTunnelInstallCmd, testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "QURL_CONNECTOR_HUB_HOST") {
		t.Fatalf("tunnel install did not render the configured Hub triple: %q", async)
	}
}
