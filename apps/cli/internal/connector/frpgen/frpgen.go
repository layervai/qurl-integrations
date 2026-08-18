// Package frpgen generates the single-route FRP client configuration for the
// CLI qURL Connector: the managed HTTP route shape the tunnel server
// authorizes, expressed as a neutral, fully typed config model plus a
// deterministic TOML rendering.
//
// Dependency posture (deliberate, PR-scoped): this package does NOT import
// the FRP client library. Field names mirror the FRP v1 client-config
// surface exactly (serverAddr, transport.protocol, proxies[].subdomain,
// loadBalancer.group, …), so the supervisor change that pulls in the FRP
// fork converts this model to FRP's own types mechanically; until then the
// TOML rendering is a complete, loadable FRP client config file for the
// one-route case.
//
// Identity model for a managed route (wire contract with the tunnel server):
//
//   - SubDomain and the load-balancer Group/GroupKey carry the
//     producer-issued connector_routing_id verbatim — the routing identity.
//   - Metadatas[resource_id] carries the public resource identity.
//   - The per-cycle knock token is stamped into Login metadata by the
//     supervisor at runtime and is NEVER rendered into a config file: it is
//     short-lived admission evidence, not configuration.
//
// GroupKey deliberately equals Group: the FRP load-balancer group key only
// requires replicas joining one group to present the same value, and the
// routing identity is not a secret. Tenant authorization is the knock-token
// plus resource metadata contract, never the group key.
package frpgen

import (
	"encoding/base32"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/replica"
)

// MetaQURLKnockToken is the tunnel Login metadata key the supervisor
// populates per cycle with the admission-controller-issued knock token
// harvested from a successful NHP knock. Cross-repo wire contract — the
// tunnel server validates the inbound Login by this exact key. It is exported
// for the supervisor's per-cycle stamping; Generate never renders it.
const MetaQURLKnockToken = "qurl_knock_token"

// MetaClientVersion is the Login metadata key reporting the client build
// version. The tunnel server's minimum-client-version kill switch reads this
// value at Login; cross-repo wire contract.
const MetaClientVersion = "client_version"

// MetaResourceID is the per-proxy metadata key carrying the public qURL
// resource identity. Cross-repo wire contract: the tunnel server requires
// this metadata for managed routes and never substitutes the subdomain,
// because the subdomain carries the independent routing identity.
const MetaResourceID = "resource_id"

// Route describes the one managed HTTP route this Connector serves.
type Route struct {
	// Slug is the customer-facing Connector identity; it is the base of the
	// rendered proxy name.
	Slug string
	// ResourceID is the producer-issued public resource identity, carried in
	// per-proxy metadata.
	ResourceID string
	// ConnectorRoutingID is the producer-issued routing identity, used
	// verbatim for the subdomain and load-balancer group. It is never
	// client-derived from ResourceID; the producer owns the calculation.
	ConnectorRoutingID string
	// LocalIP is the local service address; empty defaults to 127.0.0.1.
	LocalIP string
	// LocalPort is the local service port.
	LocalPort int
	// HostHeaderRewrite optionally rewrites the Host header toward the local
	// service.
	HostHeaderRewrite string
	// RequestHeaders are additional headers set on proxied requests.
	RequestHeaders map[string]string
}

// Options carries the common client tuning.
type Options struct {
	// ServerAddr/ServerPort are the tunnel dial target. In knock-then-login
	// operation the per-knock ACK is the source of truth for the live dial
	// target and the supervisor overrides these per cycle, so they may be
	// left zero here.
	ServerAddr string
	ServerPort int
	// Protocol optionally selects the FRP transport protocol.
	Protocol string
	// ReplicaDiscriminator salts the proxy name so co-deployed replicas of
	// the same slug register distinct names; normalized via
	// replica.Normalize.
	ReplicaDiscriminator string
	// ClientVersion is reported in Login metadata (see MetaClientVersion).
	ClientVersion string
	// KeepaliveSeconds is the dial keepalive interval; 0 defaults to 60.
	KeepaliveSeconds int
	// DialTimeoutSeconds is the server connection timeout; 0 defaults to 10.
	DialTimeoutSeconds int
	// LoginFailExit controls whether the client exits on a failed login.
	// The default false keeps FRP retrying with its internal backoff, which
	// is what the supervisor's cycle model expects.
	LoginFailExit bool
}

// ClientConfig is the generated client configuration: the common block plus
// exactly one managed HTTP proxy.
type ClientConfig struct {
	Common CommonConfig
	Proxy  HTTPProxyConfig
}

// CommonConfig mirrors the FRP v1 common client fields the Connector sets.
type CommonConfig struct {
	ServerAddr                 string
	ServerPort                 int
	Protocol                   string
	DialServerTimeoutSeconds   int
	DialServerKeepaliveSeconds int
	LoginFailExit              bool
	// Metadatas ship verbatim with the Login. Generate populates only
	// MetaClientVersion; the supervisor stamps MetaQURLKnockToken per cycle.
	Metadatas map[string]string
}

// HTTPProxyConfig mirrors the FRP v1 HTTP proxy fields the Connector sets.
type HTTPProxyConfig struct {
	Name              string
	Type              string
	LocalIP           string
	LocalPort         int
	SubDomain         string
	Group             string
	GroupKey          string
	HostHeaderRewrite string
	RequestHeadersSet map[string]string
	Metadatas         map[string]string
}

const (
	defaultKeepaliveSeconds   = 60
	defaultDialTimeoutSeconds = 10
	defaultLocalIP            = "127.0.0.1"
	proxyTypeHTTP             = "http"
)

var routingIDEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// FRPProxyName renders the proxy name for a route by joining the route's
// stable identity with the normalized per-replica discriminator. An empty
// discriminator returns the raw route identity; the supervisor normally
// supplies a discriminator at boot, so the empty branch mainly serves direct
// config-generation callers.
//
// Wire contract: the name must be unique per (server instance, group) tuple
// but not globally unique — two routes with the same name in different
// load-balancer groups route correctly. The salt covers the one collision
// shape hit in practice: same group, same route, different replica.
func FRPProxyName(routeID, replicaDiscriminator string) string {
	replicaDiscriminator = replica.Normalize(replicaDiscriminator)
	if replicaDiscriminator == "" {
		return routeID
	}
	return routeID + "-" + replicaDiscriminator
}

// Generate validates the managed single-route shape and produces the client
// configuration. It fails closed on any identity violation rather than
// emitting a config the tunnel server would reject (or worse, one that would
// impersonate a different routing identity).
func Generate(route *Route, opts *Options) (*ClientConfig, error) {
	if err := validateRoute(route); err != nil {
		return nil, err
	}
	if route.LocalIP == "" {
		route.LocalIP = defaultLocalIP
	}
	keepalive := opts.KeepaliveSeconds
	if keepalive == 0 {
		keepalive = defaultKeepaliveSeconds
	}
	dialTimeout := opts.DialTimeoutSeconds
	if dialTimeout == 0 {
		dialTimeout = defaultDialTimeoutSeconds
	}

	common := CommonConfig{
		ServerAddr:                 opts.ServerAddr,
		ServerPort:                 opts.ServerPort,
		Protocol:                   opts.Protocol,
		DialServerTimeoutSeconds:   dialTimeout,
		DialServerKeepaliveSeconds: keepalive,
		LoginFailExit:              opts.LoginFailExit,
	}
	if version := strings.TrimSpace(opts.ClientVersion); version != "" {
		common.Metadatas = map[string]string{MetaClientVersion: version}
	}

	proxy := HTTPProxyConfig{
		Name:      FRPProxyName(route.Slug, opts.ReplicaDiscriminator),
		Type:      proxyTypeHTTP,
		LocalIP:   route.LocalIP,
		LocalPort: route.LocalPort,
		// Managed routes split the FRP surfaces: the subdomain and
		// load-balancer group use the routing identity; only
		// Metadatas[resource_id] carries the public resource identity.
		SubDomain:         route.ConnectorRoutingID,
		Group:             route.ConnectorRoutingID,
		GroupKey:          route.ConnectorRoutingID,
		HostHeaderRewrite: route.HostHeaderRewrite,
		Metadatas:         map[string]string{MetaResourceID: route.ResourceID},
	}
	if len(route.RequestHeaders) > 0 {
		proxy.RequestHeadersSet = make(map[string]string, len(route.RequestHeaders))
		for name, value := range route.RequestHeaders {
			proxy.RequestHeadersSet[name] = value
		}
	}
	return &ClientConfig{Common: common, Proxy: proxy}, nil
}

func validateRoute(route *Route) error {
	if strings.TrimSpace(route.Slug) == "" {
		return errors.New("route slug is required before FRP generation")
	}
	if err := validateExactOpaqueIdentifier("resource_id", route.ResourceID); err != nil {
		return fmt.Errorf("managed resource identity is invalid: %w", err)
	}
	if err := ValidateConnectorRoutingID(route.ConnectorRoutingID); err != nil {
		return fmt.Errorf("managed resource %q requires its exact server-issued routing identity before FRP generation: %w", route.ResourceID, err)
	}
	if route.LocalPort < 1 || route.LocalPort > 65535 {
		return fmt.Errorf("local_port %d must be between 1 and 65535", route.LocalPort)
	}
	for name, value := range route.RequestHeaders {
		if err := validateExactOpaqueIdentifier("request header name", name); err != nil {
			return err
		}
		if !utf8.ValidString(value) || strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return fmt.Errorf("request header %q value must be valid UTF-8 without control characters", name)
		}
	}
	return nil
}

// ValidateConnectorRoutingID rejects a routing identity that is not exactly
// the producer's c-prefixed canonical unpadded lowercase base32 encoding of a
// 32-byte digest. The producer owns the calculation; the client only verifies
// the exact spelling before stamping it on the wire.
func ValidateConnectorRoutingID(s string) error {
	// A 32-byte producer digest is 52 characters in unpadded base32; the c-
	// namespace prefix makes the complete label exactly 54 bytes.
	if len(s) != 54 || !strings.HasPrefix(s, "c-") {
		return fmt.Errorf("connector_routing_id %q must be c- plus 52 canonical lowercase unpadded base32 characters", s)
	}
	payload := s[2:]
	decoded, err := routingIDEncoding.DecodeString(payload)
	if err != nil || routingIDEncoding.EncodeToString(decoded) != payload {
		return fmt.Errorf("connector_routing_id %q must be c- plus the canonical lowercase unpadded base32 encoding of 32 bytes", s)
	}
	return nil
}

// validateExactOpaqueIdentifier rejects transport-hostile spellings without
// inventing semantics for a producer-owned identifier: the exact value must
// be non-empty, valid UTF-8, unpadded by surrounding whitespace, and free of
// control characters before it enters FRP metadata or config comparisons.
func validateExactOpaqueIdentifier(field, value string) error {
	if value == "" {
		return fmt.Errorf("%s is required", field)
	}
	if strings.TrimSpace(value) != value {
		return fmt.Errorf("%s must not contain leading or trailing whitespace", field)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s must contain valid UTF-8", field)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("%s must not contain control characters", field)
		}
	}
	return nil
}
