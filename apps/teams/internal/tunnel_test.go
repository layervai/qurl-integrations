package internal

import (
	"strings"
	"testing"
)

func TestParseTunnelArgsRejectsInvalidSlugEnvAndService(t *testing.T) {
	t.Run("invalid slug", func(t *testing.T) {
		if _, err := parseTunnelArgs([]string{"BadSlug"}); err == nil {
			t.Fatal("expected invalid slug error")
		}
	})

	t.Run("invalid env", func(t *testing.T) {
		if _, err := parseTunnelArgs([]string{"prod-dashboard", "env:nomad"}); err == nil {
			t.Fatal("expected invalid env error")
		}
	})

	t.Run("service requires compose", func(t *testing.T) {
		if _, err := parseTunnelArgs([]string{"prod-dashboard", "service:web"}); err == nil {
			t.Fatal("expected service/env validation error")
		}
	})
}

func TestRenderTunnelInstallMessageRejectsUnknownEnvironment(t *testing.T) {
	_, err := renderTunnelInstallMessage(TunnelInstallArgs{
		Slug:         "prod-dashboard",
		Alias:        "prod-dashboard",
		Environment:  "nomad",
		BootstrapKey: "secret",
	})
	if err == nil {
		t.Fatal("expected render error")
	}
}

func TestRenderTunnelInstallMessageComposeIncludesService(t *testing.T) {
	msg, err := renderTunnelInstallMessage(TunnelInstallArgs{
		Slug:         "prod-dashboard",
		Alias:        "dash",
		Environment:  tunnelEnvCompose,
		Service:      "web",
		BootstrapKey: "secret",
	})
	if err != nil {
		t.Fatalf("renderTunnelInstallMessage error = %v", err)
	}
	if !strings.Contains(msg, "Docker Compose update for service `web`") {
		t.Fatalf("message = %q, want compose service instructions", msg)
	}
}

func TestRenderTunnelInstallMessageKubernetesUsesBootstrapSecret(t *testing.T) {
	msg, err := renderTunnelInstallMessage(TunnelInstallArgs{
		Slug:         "prod-dashboard",
		Alias:        "dash",
		Environment:  tunnelEnvKubernetes,
		BootstrapKey: "secret",
		Port:         8080,
		TunnelImage:  "ghcr.io/layervai/qurl-connector:v1.2.3",
	})
	if err != nil {
		t.Fatalf("renderTunnelInstallMessage error = %v", err)
	}
	for _, want := range []string{
		"type: Opaque",
		"name: QURL_BOOTSTRAP_KEY",
		"valueFrom:",
		"secretKeyRef:",
		`value: "prod-dashboard"`,
		`value: "8080"`,
		`image: "ghcr.io/layervai/qurl-connector:v1.2.3"`,
		`api_key: "secret"`,
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message missing %q:\n%s", want, msg)
		}
	}
}
