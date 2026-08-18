package frpgen

import (
	"crypto/sha256"
	"strings"
	"testing"
)

const testHostRewrite = "localhost"

// Test identities: the resource id is an opaque producer identity from the
// generator's perspective (the SDK owns canonical key parsing), so any
// transport-safe exact value works here; the routing id must be the exact
// c-prefixed canonical base32 spelling.
const testResourceID = "MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEtestpublicresourceidentityAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func testRoutingID(seed string) string {
	digest := sha256.Sum256([]byte(seed))
	return "c-" + routingIDEncoding.EncodeToString(digest[:])
}

func managedRoute() Route {
	return Route{
		Slug:               "dashboard",
		ResourceID:         testResourceID,
		ConnectorRoutingID: testRoutingID("route-a"),
		LocalPort:          8080,
	}
}

func TestGenerateManagedSingleRoute(t *testing.T) {
	t.Parallel()
	route := managedRoute()
	route.HostHeaderRewrite = testHostRewrite
	route.RequestHeaders = map[string]string{"X-Real-IP": "pass"}
	cfg, err := Generate(&route, &Options{
		ServerAddr:           "tunnel.test.layerv.ai",
		ServerPort:           7000,
		Protocol:             "kcp",
		ReplicaDiscriminator: "replica-1",
		ClientVersion:        "v1.2.3",
	})
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Common.ServerAddr != "tunnel.test.layerv.ai" || cfg.Common.ServerPort != 7000 {
		t.Errorf("server = %s:%d", cfg.Common.ServerAddr, cfg.Common.ServerPort)
	}
	if cfg.Common.Protocol != "kcp" {
		t.Errorf("Protocol = %q, want kcp", cfg.Common.Protocol)
	}
	if cfg.Common.LoginFailExit {
		t.Error("LoginFailExit = true, want the retry-forever default")
	}
	if cfg.Common.DialServerKeepaliveSeconds != 60 || cfg.Common.DialServerTimeoutSeconds != 10 {
		t.Errorf("transport tuning = keepalive %d, timeout %d; want defaults 60/10", cfg.Common.DialServerKeepaliveSeconds, cfg.Common.DialServerTimeoutSeconds)
	}
	if got := cfg.Common.Metadatas[MetaClientVersion]; got != "v1.2.3" {
		t.Errorf("Metadatas[%s] = %q, want v1.2.3", MetaClientVersion, got)
	}

	proxy := cfg.Proxy
	if proxy.Name != "dashboard-replica-1" {
		t.Errorf("Name = %q, want the salted proxy name", proxy.Name)
	}
	if proxy.Type != "http" {
		t.Errorf("Type = %q, want http", proxy.Type)
	}
	if proxy.LocalIP != "127.0.0.1" || proxy.LocalPort != 8080 {
		t.Errorf("local = %s:%d, want default IP with the route port", proxy.LocalIP, proxy.LocalPort)
	}
	// Managed identity split: subdomain and the load-balancer group carry
	// the routing identity; metadata carries the public resource identity.
	routing := route.ConnectorRoutingID
	if proxy.SubDomain != routing || proxy.Group != routing || proxy.GroupKey != routing {
		t.Errorf("routing = subdomain %q group %q groupKey %q, want %q on all three", proxy.SubDomain, proxy.Group, proxy.GroupKey, routing)
	}
	if got := proxy.Metadatas[MetaResourceID]; got != testResourceID {
		t.Errorf("Metadatas[%s] = %q, want the public resource identity", MetaResourceID, got)
	}
	if proxy.HostHeaderRewrite != testHostRewrite || proxy.RequestHeadersSet["X-Real-IP"] != "pass" {
		t.Errorf("header shaping = rewrite %q headers %v", proxy.HostHeaderRewrite, proxy.RequestHeadersSet)
	}
	// The per-cycle knock token must never appear in generated config.
	if _, present := cfg.Common.Metadatas[MetaQURLKnockToken]; present {
		t.Fatal("generated config carries the knock-token metadata key")
	}
}

func TestGenerateRejectsBrokenIdentities(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mutate  func(*Route)
		wantErr string
	}{
		{name: "empty slug", mutate: func(r *Route) { r.Slug = " " }, wantErr: "slug is required"},
		{name: "missing resource id", mutate: func(r *Route) { r.ResourceID = "" }, wantErr: "resource_id is required"},
		{name: "control char resource id", mutate: func(r *Route) { r.ResourceID += "\x00" }, wantErr: "control characters"},
		{name: "padded resource id", mutate: func(r *Route) { r.ResourceID = " " + r.ResourceID }, wantErr: "whitespace"},
		{name: "missing routing id", mutate: func(r *Route) { r.ConnectorRoutingID = "" }, wantErr: "routing identity"},
		{name: "wrong prefix routing id", mutate: func(r *Route) { r.ConnectorRoutingID = "x-" + r.ConnectorRoutingID[2:] }, wantErr: "routing identity"},
		{name: "uppercase routing id", mutate: func(r *Route) { r.ConnectorRoutingID = strings.ToUpper(r.ConnectorRoutingID) }, wantErr: "routing identity"},
		{name: "short routing id", mutate: func(r *Route) { r.ConnectorRoutingID = r.ConnectorRoutingID[:20] }, wantErr: "routing identity"},
		{name: "zero port", mutate: func(r *Route) { r.LocalPort = 0 }, wantErr: "local_port"},
		{name: "oversized port", mutate: func(r *Route) { r.LocalPort = 70000 }, wantErr: "local_port"},
		{name: "control char header value", mutate: func(r *Route) { r.RequestHeaders = map[string]string{"X-Bad": "a\x01b"} }, wantErr: "control characters"},
		{name: "empty header name", mutate: func(r *Route) { r.RequestHeaders = map[string]string{"": "v"} }, wantErr: "header name is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			route := managedRoute()
			tt.mutate(&route)
			_, err := Generate(&route, &Options{})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Generate = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestFRPProxyName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		routeID, disc, want string
	}{
		{"web", "", "web"},
		{"web", "replica-1", "web-replica-1"},
		// The salt is normalized at the wire boundary as defense in depth for
		// direct callers.
		{"web", "REPLICA_1!", "web-replica1"},
		{"web", "  ", "web"},
	}
	for _, tt := range tests {
		if got := FRPProxyName(tt.routeID, tt.disc); got != tt.want {
			t.Errorf("FRPProxyName(%q, %q) = %q, want %q", tt.routeID, tt.disc, got, tt.want)
		}
	}
}

func TestGenerateOmitsClientVersionMetadataWhenBlank(t *testing.T) {
	t.Parallel()
	route := managedRoute()
	cfg, err := Generate(&route, &Options{ClientVersion: "  "})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Common.Metadatas != nil {
		t.Fatalf("Metadatas = %v, want none for a blank client version", cfg.Common.Metadatas)
	}
}

func TestGenerateNeverEmitsCredentialMaterial(t *testing.T) {
	t.Parallel()
	// The durable API key and the enrollment token have no input path into
	// this generator at all; the knock token has a named metadata key, so pin
	// its absence explicitly, end to end through the TOML rendering.
	route := managedRoute()
	cfg, err := Generate(&route, &Options{ClientVersion: "v1.2.3"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := EncodeTOML(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), MetaQURLKnockToken) {
		t.Fatalf("rendered config mentions the knock-token key:\n%s", raw)
	}
}
