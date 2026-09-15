package internal

import (
	"strings"
	"testing"
)

func TestTunnelInstallCarriesConfiguredHub(t *testing.T) {
	hub := TunnelHub{Host: "hub.nhp.layerv.xyz", Port: "443", PublicKey: "UhVQcrKoJ2LhQlRtuIItBjxXR2wA/VvZvTmqnzT+GS8="}
	for _, env := range []tunnelInstallEnvironment{tunnelEnvDocker, tunnelEnvCompose, tunnelEnvKubernetes, tunnelEnvECSFargate} {
		t.Run(string(env), func(t *testing.T) {
			args := testTunnelInstallArgs()
			args.Environment = env
			h := &Handler{cfg: Config{TunnelHub: hub}}
			got, err := h.renderTunnelInstallInstructions(args, testTunnelImageRef)
			if err != nil {
				t.Fatal(err)
			}
			for _, value := range []string{hub.Host, hub.PublicKey, "--hub-host", "--hub-port", "--hub-server-public-key-b64"} {
				if !strings.Contains(got, value) {
					t.Errorf("missing pinned Hub value %q", value)
				}
			}
		})
	}
}

func TestTunnelHubRejectsPartialAndUnsafeConfig(t *testing.T) {
	valid := TunnelHub{Host: "hub.nhp.layerv.xyz", Port: "443", PublicKey: "UhVQcrKoJ2LhQlRtuIItBjxXR2wA/VvZvTmqnzT+GS8="}
	for _, tc := range []struct {
		name   string
		change func(*TunnelHub)
	}{
		{"missing host", func(h *TunnelHub) { h.Host = "" }},
		{"missing port", func(h *TunnelHub) { h.Port = "" }},
		{"missing key", func(h *TunnelHub) { h.PublicKey = "" }},
		{"noncanonical port", func(h *TunnelHub) { h.Port = "0443" }},
		{"injected host", func(h *TunnelHub) { h.Host = "hub.$(echo bad).layerv.xyz" }},
		{"foreign host", func(h *TunnelHub) { h.Host = "hub.example.com" }},
		{"empty label", func(h *TunnelHub) { h.Host = "hub..layerv.xyz" }},
		{"invalid label", func(h *TunnelHub) { h.Host = "-hub.layerv.xyz" }},
		{"empty interior label", func(h *TunnelHub) { h.Host = "hub..edge.layerv.xyz" }},
		{"invalid interior label", func(h *TunnelHub) { h.Host = "hub.-edge.layerv.xyz" }},
		{"overlong label", func(h *TunnelHub) { h.Host = "hub." + strings.Repeat("a", 64) + ".layerv.xyz" }},
		{"invalid key", func(h *TunnelHub) { h.PublicKey = "not-base64" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub := valid
			tc.change(&hub)
			h := &Handler{cfg: Config{TunnelHub: hub}}
			args := testTunnelInstallArgs()
			args.Environment = tunnelEnvDocker
			got, err := h.renderTunnelInstallInstructions(args, testTunnelImageRef)
			if err == nil || got != "" {
				t.Fatalf("unsafe configuration rendered: %q, %v", got, err)
			}
		})
	}
	if err := (TunnelHub{}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestS3WebsiteInstallCarriesConfiguredHub(t *testing.T) {
	hub := TunnelHub{Host: "hub.nhp.layerv.xyz", Port: "443", PublicKey: "UhVQcrKoJ2LhQlRtuIItBjxXR2wA/VvZvTmqnzT+GS8="}
	for _, env := range []tunnelInstallEnvironment{tunnelEnvDocker, tunnelEnvCompose, tunnelEnvKubernetes, tunnelEnvECSFargate} {
		t.Run(string(env), func(t *testing.T) {
			h := &Handler{cfg: Config{TunnelHub: hub}}
			msg, err := h.prepareS3WebsiteInstallMessage(testS3WebsiteArgs(env))
			if err != nil {
				t.Fatal(err)
			}
			for _, value := range []string{hub.Host, hub.PublicKey, "--hub-host", "--hub-port", "--hub-server-public-key-b64"} {
				if !strings.Contains(msg.instructions, value) {
					t.Errorf("S3 install missing Hub value %q", value)
				}
			}
		})
	}
}
