package daemon

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/fatedier/frp/pkg/transport"
	connectorshare "github.com/layervai/qurl-connector/pkg/share"
)

func testTunnelCA(t *testing.T) string {
	t.Helper()
	server := httptest.NewTLSServer(http.NotFoundHandler())
	defer server.Close()
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.TLS.Certificates[0].Certificate[0]}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRuntimeHeadersRequireConfiguredTunnelTrust(t *testing.T) {
	ca := testTunnelCA(t)
	for _, configured := range []bool{false, true} {
		name := "ordinary daemon"
		caFile, serverName := "", ""
		if configured {
			name, caFile, serverName = "verified tunnel", ca, "example.com"
		}
		t.Run(name, func(t *testing.T) {
			common, err := ConfiguredFRPCommon(1, 1, caFile, serverName)
			if err != nil {
				t.Fatal(err)
			}
			factory, err := connectorshare.NewFRPSessionGroupFactory(connectorshare.FRPGroupFactoryConfig{Common: common})
			if err != nil {
				t.Fatal(err)
			}
			route := connectorshare.LocalHTTPRoute{RouteID: "a", LocalIP: "127.0.0.1", LocalPort: 3000, ResourcePublicKey: "resource-a", ConnectorRoutingID: "routing-a"}
			if err := factory.ValidateRoutes([]connectorshare.LocalHTTPRoute{route}); err != nil {
				t.Fatalf("ordinary route refused: %v", err)
			}
			route.RequestHeaders = map[string]string{"X-Origin-Token": "test-value"}
			err = factory.ValidateRoutes([]connectorshare.LocalHTTPRoute{route})
			if (err == nil) != configured {
				t.Fatalf("header route validation = %v, configured = %t", err, configured)
			}
			if !configured {
				return
			}
			// Check the actual FRP TLS builder, not just the Connector guard.
			tlsConfig, err := transport.NewClientTLSConfig("", "", common.Transport.TLS.TrustedCaFile, common.Transport.TLS.ServerName)
			if err != nil || tlsConfig.InsecureSkipVerify || tlsConfig.RootCAs == nil || tlsConfig.ServerName != "example.com" {
				t.Fatalf("configured tunnel does not verify its peer: %v", err)
			}
			if common.Transport.TLS.Enable == nil || !*common.Transport.TLS.Enable || common.WebServer.Port != 0 {
				t.Fatal("header-bearing transport must use TLS with no client admin server")
			}
		})
	}
}

func TestConfiguredTunnelRejectsInvalidTrust(t *testing.T) {
	badPEM := filepath.Join(t.TempDir(), "bad.pem")
	if err := os.WriteFile(badPEM, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, ca, server string }{
		{"name without trust", "", "example.com"},
		{"relative path", "ca.pem", "example.com"},
		{"missing file", filepath.Join(t.TempDir(), "missing.pem"), "example.com"},
		{"invalid PEM", badPEM, "example.com"},
		{"URL instead of name", testTunnelCA(t), "https://example.com"},
		{"whitespace name", testTunnelCA(t), "example.com\t"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if common, err := ConfiguredFRPCommon(1, 1, tc.ca, tc.server); err == nil || common != nil {
				t.Fatal("invalid trust configuration was accepted")
			}
		})
	}
}
