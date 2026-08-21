package frpgen

import (
	"bytes"
	"strings"
	"testing"
)

func TestEncodeTOMLDeterministicShape(t *testing.T) {
	t.Parallel()
	route := managedRoute()
	route.HostHeaderRewrite = testHostRewrite
	route.RequestHeaders = map[string]string{"X-Zulu": "z", "X-Alpha": "a"}
	cfg, err := Generate(&route, &Options{
		ServerAddr:           "tunnel.test.layerv.ai",
		ServerPort:           7000,
		Protocol:             "quic",
		ReplicaDiscriminator: "replica-1",
		ClientVersion:        "v1.2.3",
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := EncodeTOML(cfg)
	if err != nil {
		t.Fatal(err)
	}
	second, err := EncodeTOML(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("EncodeTOML is not deterministic")
	}
	routing := route.ConnectorRoutingID
	for _, want := range []string{
		`serverAddr = "tunnel.test.layerv.ai"`,
		"serverPort = 7000",
		"loginFailExit = false",
		`transport.protocol = "quic"`,
		"transport.dialServerTimeout = 10",
		"transport.dialServerKeepalive = 60",
		"[metadatas]",
		`client_version = "v1.2.3"`,
		"[[proxies]]",
		`name = "dashboard-replica-1"`,
		`type = "http"`,
		`localIP = "127.0.0.1"`,
		"localPort = 8080",
		`subdomain = "` + routing + `"`,
		`hostHeaderRewrite = "` + testHostRewrite + `"`,
		`loadBalancer.group = "` + routing + `"`,
		`loadBalancer.groupKey = "` + routing + `"`,
		"[proxies.metadatas]",
		`resource_id = "` + testResourceID + `"`,
		"[proxies.requestHeaders.set]",
	} {
		if !strings.Contains(string(first), want) {
			t.Errorf("rendered TOML missing %q:\n%s", want, first)
		}
	}
	// Sorted header keys: X-Alpha before X-Zulu.
	if bytes.Index(first, []byte("X-Alpha")) > bytes.Index(first, []byte("X-Zulu")) {
		t.Fatalf("header table is not sorted:\n%s", first)
	}
}

func TestEncodeTOMLOmitsUnsetServerTarget(t *testing.T) {
	t.Parallel()
	// In knock-then-login operation the ACK stamps the live dial target per
	// cycle; a config generated without one must simply omit it rather than
	// render an empty address.
	route := managedRoute()
	cfg, err := Generate(&route, &Options{})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := EncodeTOML(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "serverAddr") || strings.Contains(string(raw), "serverPort") {
		t.Fatalf("rendered TOML carries an unset server target:\n%s", raw)
	}
}

func TestEncodeTOMLEscapesQuotesAndBackslashes(t *testing.T) {
	t.Parallel()
	route := managedRoute()
	route.RequestHeaders = map[string]string{`X-Odd"Key`: `value "quoted" with \backslash`}
	cfg, err := Generate(&route, &Options{})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := EncodeTOML(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"X-Odd\"Key" = "value \"quoted\" with \\backslash"`) {
		t.Fatalf("escaping missing from rendered TOML:\n%s", raw)
	}
}

func TestEncodeTOMLRefusesKnockTokenMetadata(t *testing.T) {
	t.Parallel()
	route := managedRoute()
	cfg, err := Generate(&route, &Options{})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Common.Metadatas = map[string]string{MetaQURLKnockToken: "short-lived-admission-token"}
	if _, err := EncodeTOML(cfg); err == nil || !strings.Contains(err.Error(), MetaQURLKnockToken) {
		t.Fatalf("EncodeTOML = %v, want a refusal naming the knock-token key", err)
	}
}

func TestEncodeTOMLRejectsControlCharacters(t *testing.T) {
	t.Parallel()
	route := managedRoute()
	cfg, err := Generate(&route, &Options{})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Common.ServerAddr = "bad\nhost"
	if _, err := EncodeTOML(cfg); err == nil || !strings.Contains(err.Error(), "control characters") {
		t.Fatalf("EncodeTOML = %v, want control-character rejection instead of multi-line injection", err)
	}
	if _, err := EncodeTOML(nil); err == nil {
		t.Fatal("EncodeTOML(nil) = nil error")
	}
}

func TestValidateConnectorRoutingIDTable(t *testing.T) {
	t.Parallel()
	valid := testRoutingID("routing")
	if err := ValidateConnectorRoutingID(valid); err != nil {
		t.Fatalf("valid routing id rejected: %v", err)
	}
	for name, id := range map[string]string{
		"empty":              "",
		"missing prefix":     valid[2:],
		"wrong prefix":       "d-" + valid[2:],
		"uppercase payload":  "c-" + strings.ToUpper(valid[2:]),
		"short payload":      valid[:53],
		"long payload":       valid + "a",
		"padding characters": valid[:52] + "==",
		"invalid alphabet":   "c-" + strings.Repeat("0", 52),
	} {
		if err := ValidateConnectorRoutingID(id); err == nil {
			t.Errorf("ValidateConnectorRoutingID accepted %s: %q", name, id)
		}
	}
}
