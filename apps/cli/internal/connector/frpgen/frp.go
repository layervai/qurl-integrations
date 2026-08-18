package frpgen

import (
	v1 "github.com/fatedier/frp/pkg/config/v1"
)

// This file is the mechanical adapter from the package's neutral config model
// to the FRP client library's own v1 types, for the supervisor that links the
// FRP client. The neutral model stays the validated source of truth (and the
// TOML rendering input) because the two surfaces disagree on shape where it
// matters: v1 nests the transport tuning, spells the dial timeouts as int64,
// and — the trap — types LoginFailExit as *bool whose nil completes to TRUE,
// silently inverting this model's explicit false default. The adapter
// therefore always materializes the pointer.

// FRPCommon converts the common block to FRP's client common config. The
// result aliases nothing: Metadatas is cloned so the supervisor's per-cycle
// stamping can never write back into the generated model.
func FRPCommon(common *CommonConfig) *v1.ClientCommonConfig {
	out := &v1.ClientCommonConfig{
		ServerAddr: common.ServerAddr,
		ServerPort: common.ServerPort,
		// Always a real pointer: leaving nil would let v1's Complete()
		// default the field to true, inverting the model's explicit value.
		LoginFailExit: ptrTo(common.LoginFailExit),
		Metadatas:     cloneStringMap(common.Metadatas),
	}
	out.Transport.Protocol = common.Protocol
	out.Transport.DialServerTimeout = int64(common.DialServerTimeoutSeconds)
	out.Transport.DialServerKeepAlive = int64(common.DialServerKeepaliveSeconds)
	return out
}

// FRPProxy converts the managed HTTP proxy to FRP's v1 HTTP proxy config.
// Maps are cloned for the same no-aliasing guarantee as FRPCommon.
func FRPProxy(proxy *HTTPProxyConfig) *v1.HTTPProxyConfig {
	out := &v1.HTTPProxyConfig{
		ProxyBaseConfig: v1.ProxyBaseConfig{
			Name:      proxy.Name,
			Type:      proxy.Type,
			Metadatas: cloneStringMap(proxy.Metadatas),
			LoadBalancer: v1.LoadBalancerConfig{
				Group:    proxy.Group,
				GroupKey: proxy.GroupKey,
			},
			ProxyBackend: v1.ProxyBackend{
				LocalIP:   proxy.LocalIP,
				LocalPort: proxy.LocalPort,
			},
		},
		HostHeaderRewrite: proxy.HostHeaderRewrite,
	}
	out.SubDomain = proxy.SubDomain
	out.RequestHeaders.Set = cloneStringMap(proxy.RequestHeadersSet)
	return out
}

// FRPClientConfig converts the full generated configuration into the pair the
// FRP client service consumes: the common config plus the one managed proxy as
// a configurer list. Callers own Complete()/validation — this conversion is
// deliberately pure so the caller controls when FRP's defaulting runs.
func (c *ClientConfig) FRPClientConfig() (*v1.ClientCommonConfig, []v1.ProxyConfigurer) {
	return FRPCommon(&c.Common), []v1.ProxyConfigurer{FRPProxy(&c.Proxy)}
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func ptrTo(v bool) *bool {
	return &v
}
