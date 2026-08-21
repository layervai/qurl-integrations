package frpgen

import (
	"testing"

	v1 "github.com/fatedier/frp/pkg/config/v1"
)

// generatedForFRPTest builds one fully populated ClientConfig through the real
// Generate path so the adapter assertions cover the model as produced, not a
// hand-rolled literal that could drift from Generate's field decisions.
func generatedForFRPTest(t *testing.T) *ClientConfig {
	t.Helper()
	cfg, err := Generate(
		&Route{
			Slug:               "files",
			ResourceID:         "r_public",
			ConnectorRoutingID: testRoutingID("route-frp"),
			LocalIP:            "127.0.0.9",
			LocalPort:          8443,
			HostHeaderRewrite:  "origin.internal",
			RequestHeaders:     map[string]string{"X-From-Tunnel": "1"},
		},
		&Options{
			ServerAddr:           "tunnel.example",
			ServerPort:           7000,
			Protocol:             "tcp",
			ReplicaDiscriminator: "replica-1",
			ClientVersion:        "9.9.9",
			KeepaliveSeconds:     45,
			DialTimeoutSeconds:   12,
			LoginFailExit:        false,
		},
	)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return cfg
}

// TestFRPClientConfigMapsEveryGeneratedField pins the mechanical 1:1 mapping
// between the neutral model and FRP's v1 types, field by field, over a config
// produced by the real Generate path.
func TestFRPClientConfigMapsEveryGeneratedField(t *testing.T) {
	cfg := generatedForFRPTest(t)
	common, proxies := cfg.FRPClientConfig()

	if common.ServerAddr != "tunnel.example" || common.ServerPort != 7000 {
		t.Fatalf("dial target = %s:%d, want tunnel.example:7000", common.ServerAddr, common.ServerPort)
	}
	if common.Transport.Protocol != "tcp" {
		t.Fatalf("Transport.Protocol = %q, want tcp", common.Transport.Protocol)
	}
	if common.Transport.DialServerTimeout != 12 || common.Transport.DialServerKeepAlive != 45 {
		t.Fatalf("dial tuning = timeout %d keepalive %d, want 12/45",
			common.Transport.DialServerTimeout, common.Transport.DialServerKeepAlive)
	}
	if common.LoginFailExit == nil || *common.LoginFailExit {
		t.Fatalf("LoginFailExit = %v, want explicit false pointer", common.LoginFailExit)
	}
	if got := common.Metadatas[MetaClientVersion]; got != "9.9.9" {
		t.Fatalf("Metadatas[%s] = %q, want 9.9.9", MetaClientVersion, got)
	}

	if len(proxies) != 1 {
		t.Fatalf("proxies = %d, want exactly the one managed route", len(proxies))
	}
	proxy, ok := proxies[0].(*v1.HTTPProxyConfig)
	if !ok {
		t.Fatalf("proxy configurer type = %T, want *v1.HTTPProxyConfig", proxies[0])
	}
	if proxy.Name != cfg.Proxy.Name || proxy.Type != "http" {
		t.Fatalf("proxy identity = %s/%s, want %s/http", proxy.Name, proxy.Type, cfg.Proxy.Name)
	}
	if proxy.LocalIP != "127.0.0.9" || proxy.LocalPort != 8443 {
		t.Fatalf("proxy backend = %s:%d, want 127.0.0.9:8443", proxy.LocalIP, proxy.LocalPort)
	}
	routingID := cfg.Proxy.SubDomain
	if proxy.SubDomain != routingID || proxy.LoadBalancer.Group != routingID || proxy.LoadBalancer.GroupKey != routingID {
		t.Fatalf("routing identity = subdomain %q group %q key %q, want all %q",
			proxy.SubDomain, proxy.LoadBalancer.Group, proxy.LoadBalancer.GroupKey, routingID)
	}
	if proxy.HostHeaderRewrite != "origin.internal" {
		t.Fatalf("HostHeaderRewrite = %q, want origin.internal", proxy.HostHeaderRewrite)
	}
	if got := proxy.Metadatas[MetaResourceID]; got != "r_public" {
		t.Fatalf("proxy Metadatas[%s] = %q, want r_public", MetaResourceID, got)
	}
	if got := proxy.RequestHeaders.Set["X-From-Tunnel"]; got != "1" {
		t.Fatalf("RequestHeaders.Set = %v, want X-From-Tunnel:1", proxy.RequestHeaders.Set)
	}
}

// TestFRPClientConfigDoesNotAliasModelMaps proves the adapter's clone
// guarantee: mutating the converted maps must not write back into the
// generated model (the supervisor stamps per-cycle Login metadata on the FRP
// side and the model is documented read-only).
func TestFRPClientConfigDoesNotAliasModelMaps(t *testing.T) {
	cfg := generatedForFRPTest(t)
	common, proxies := cfg.FRPClientConfig()
	proxy := proxies[0].(*v1.HTTPProxyConfig)

	common.Metadatas[MetaQURLKnockToken] = "stamped-at-runtime"
	proxy.Metadatas["injected"] = "x"
	proxy.RequestHeaders.Set["injected"] = "x"

	if _, leaked := cfg.Common.Metadatas[MetaQURLKnockToken]; leaked {
		t.Fatal("common Metadatas aliased: runtime knock-token stamp reached the generated model")
	}
	if _, leaked := cfg.Proxy.Metadatas["injected"]; leaked {
		t.Fatal("proxy Metadatas aliased into the generated model")
	}
	if _, leaked := cfg.Proxy.RequestHeadersSet["injected"]; leaked {
		t.Fatal("proxy RequestHeadersSet aliased into the generated model")
	}
}

// TestFRPCommonLoginFailExitSurvivesComplete documents the *bool trap the
// adapter exists to defuse: v1's Complete() defaults a nil LoginFailExit to
// TRUE, so an adapter that only mapped non-zero values would silently invert
// the model's explicit false. The materialized pointer must survive
// Complete() in both directions.
func TestFRPCommonLoginFailExitSurvivesComplete(t *testing.T) {
	for _, want := range []bool{true, false} {
		common := FRPCommon(&CommonConfig{LoginFailExit: want})
		if err := common.Complete(); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		if common.LoginFailExit == nil || *common.LoginFailExit != want {
			t.Fatalf("LoginFailExit after Complete = %v, want explicit %v", common.LoginFailExit, want)
		}
	}
}

// TestFRPCommonNilMetadatasStaysNil pins the conversion of an absent metadata
// map: Generate omits Metadatas when there is no client version, and the
// adapter must not manufacture an empty map where the model has none.
func TestFRPCommonNilMetadatasStaysNil(t *testing.T) {
	if got := FRPCommon(&CommonConfig{}).Metadatas; got != nil {
		t.Fatalf("Metadatas = %#v, want nil for an absent model map", got)
	}
}
