package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	conformance "github.com/layervai/qurl-conformance"
	connectorstateowner "github.com/layervai/qurl-connector/pkg/agentstate"
	connectorshare "github.com/layervai/qurl-connector/pkg/share"
	qurl "github.com/layervai/qurl-go/qurl"
	"github.com/layervai/qurl-go/relayknock"
	"github.com/layervai/qurl-go/relayknock/relayknocktest"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

const (
	connectorIntegrationAgentID  = "agent-cli-integration"
	connectorIntegrationHubHost  = "a.test.layerv.xyz"
	connectorIntegrationCellHost = "b.test.layerv.xyz"
)

// nativeRecoveryRoute keeps the real qurl-go encrypted transport in this
// cross-repository test while routing only its synthetic hosts to local UDP
// sockets. No production endpoint or credential is used.
type nativeRecoveryRoute struct {
	hosts   map[string]netip.Addr
	targets map[string]string
}

func (r nativeRecoveryRoute) LookupNetIP(_ context.Context, network, host string) ([]netip.Addr, error) {
	if network != "ip" {
		return nil, fmt.Errorf("unexpected resolver network %q", network)
	}
	address, ok := r.hosts[host]
	if !ok {
		return nil, fmt.Errorf("unexpected synthetic native host %q", host)
	}
	return []netip.Addr{address}, nil
}

func (r nativeRecoveryRoute) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	target, ok := r.targets[host]
	if !ok {
		return nil, fmt.Errorf("unexpected synthetic native address %q", host)
	}
	return (&net.Dialer{}).DialContext(ctx, network, target)
}

type nativeRecoveryUDPServer struct {
	t          *testing.T
	conn       *net.UDPConn
	serverPriv []byte
	agentPub   []byte
	hub        bool
	replies    func([]byte) ([]byte, error)
	done       chan struct{}

	mu       sync.Mutex
	requests [][]byte
}

func newNativeRecoveryUDPServer(
	t *testing.T,
	serverPriv, agentPub []byte,
	hub bool,
	replies func([]byte) ([]byte, error),
) *nativeRecoveryUDPServer {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	server := &nativeRecoveryUDPServer{
		t: t, conn: conn, serverPriv: bytes.Clone(serverPriv), agentPub: bytes.Clone(agentPub),
		hub: hub, replies: replies, done: make(chan struct{}),
	}
	go server.serve()
	t.Cleanup(func() {
		_ = conn.Close()
		select {
		case <-server.done:
		case <-time.After(5 * time.Second):
			t.Error("synthetic native UDP server did not stop")
		}
	})
	return server
}

func (s *nativeRecoveryUDPServer) serve() {
	defer close(s.done)
	buffer := make([]byte, 4096)
	var proofBody, proofCookie []byte
	sequence := 0
	for {
		n, remote, err := s.conn.ReadFromUDP(buffer)
		if err != nil {
			return
		}
		packet := bytes.Clone(buffer[:n])
		if proofCookie != nil {
			opened, openErr := relayknocktest.OpenHubLSTCookieProofMessage(
				s.serverPriv, s.agentPub, proofCookie, packet,
			)
			if openErr != nil {
				s.t.Errorf("open synthetic Hub proof: %v", openErr)
				continue
			}
			if !bytes.Equal(opened.Body, proofBody) {
				s.t.Errorf("synthetic Hub proof changed its request body")
				continue
			}
			proofCookie = nil
			s.record(opened.Body)
			body, replyErr := s.replies(opened.Body)
			if replyErr != nil {
				s.t.Errorf("build synthetic Hub result: %v", replyErr)
				continue
			}
			if writeErr := s.writeReply(remote, relayknock.TypeListResult, opened.Counter, body, sequence); writeErr != nil {
				s.t.Errorf("write synthetic Hub result: %v", writeErr)
			}
			sequence++
			continue
		}

		opened, openErr := relayknocktest.OpenInitiatorMessage(s.serverPriv, s.agentPub, packet)
		if openErr != nil {
			s.t.Errorf("open synthetic native request: %v", openErr)
			continue
		}
		if s.hub {
			cookie := bytes.Repeat([]byte{0x5a}, 32)
			challenge, marshalErr := json.Marshal(map[string]any{
				"trxId":  opened.Counter,
				"cookie": base64.StdEncoding.EncodeToString(cookie),
			})
			if marshalErr != nil {
				s.t.Errorf("marshal synthetic Hub challenge: %v", marshalErr)
				continue
			}
			if writeErr := s.writeReply(remote, relayknock.TypeCookieChallenge, opened.Counter+99, challenge, sequence); writeErr != nil {
				s.t.Errorf("write synthetic Hub challenge: %v", writeErr)
				continue
			}
			proofBody, proofCookie = bytes.Clone(opened.Body), cookie
			sequence++
			continue
		}

		s.record(opened.Body)
		body, replyErr := s.replies(opened.Body)
		if replyErr != nil {
			s.t.Errorf("build synthetic cell result: %v", replyErr)
			continue
		}
		if writeErr := s.writeReply(remote, relayknock.TypeListResult, opened.Counter, body, sequence); writeErr != nil {
			s.t.Errorf("write synthetic cell result: %v", writeErr)
		}
		sequence++
	}
}

func (s *nativeRecoveryUDPServer) record(body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, bytes.Clone(body))
}

func (s *nativeRecoveryUDPServer) snapshot() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([][]byte, len(s.requests))
	for i := range s.requests {
		result[i] = bytes.Clone(s.requests[i])
	}
	return result
}

func (s *nativeRecoveryUDPServer) writeReply(
	remote *net.UDPAddr,
	replyType int,
	counter uint64,
	body []byte,
	sequence int,
) error {
	packet, err := relayknocktest.BuildReply(replyType, &relayknock.KnockInputs{
		DeviceStaticPriv: s.serverPriv,
		ServerStaticPub:  s.agentPub,
		EphemeralPriv:    bytes.Repeat([]byte{byte(0x40 + sequence)}, 32),
		TimestampNanos:   uint64(time.Now().UnixNano()),
		Counter:          counter,
		Preamble:         uint32(0x50607080 + sequence),
		Body:             body,
	})
	if err != nil {
		return err
	}
	_, err = s.conn.WriteToUDP(packet, remote)
	return err
}

func nativeRecoveryFixtureKey(t *testing.T, raw string) []byte {
	t.Helper()
	decoded, err := hex.DecodeString(raw)
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}

func nativeRecoveryQuery(body []byte) (query, mode string, err error) {
	var request struct {
		UserData struct {
			Query string `json:"query"`
			Mode  string `json:"mode"`
		} `json:"usrData"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		return "", "", err
	}
	return request.UserData.Query, request.UserData.Mode, nil
}

func nativeRecoveryHubReply(
	t *testing.T,
	recovery *conformance.AgentCredentialRecoveryFile,
	assignment *conformance.AgentAssignmentFile,
	cellPublicKeyB64 string,
	now time.Time,
) func([]byte) ([]byte, error) {
	t.Helper()
	return func(request []byte) ([]byte, error) {
		query, mode, err := nativeRecoveryQuery(request)
		if err != nil || query != "cell_assignment" {
			return nil, fmt.Errorf("unexpected Hub request %q/%q: %w", query, mode, err)
		}
		var body map[string]any
		switch mode {
		case "recover":
			if err := json.Unmarshal([]byte(recovery.PublicExchanges[conformance.AgentCredentialRecoveryHubPhase].SuccessBodyJSON), &body); err != nil {
				return nil, err
			}
			list := body["list"].(map[string]any)
			list["agent_id"] = connectorIntegrationAgentID
			list["recovery_grant"] = "qrg1.integration-recovery-grant-0001"
			list["recovery_grant_issued_at"] = now.Add(-time.Minute).Format(time.RFC3339)
			list["recovery_grant_expires_at"] = now.Add(14 * time.Minute).Format(time.RFC3339)
			setNativeRecoveryAssignment(list["assignment"].(map[string]any), cellPublicKeyB64, now)
		case "refresh":
			if err := json.Unmarshal([]byte(assignment.RefreshAssignment.Result.BodyJSON), &body); err != nil {
				return nil, err
			}
			list := body["list"].(map[string]any)
			list["agent_id"] = connectorIntegrationAgentID
			setNativeRecoveryAssignment(list["assignment"].(map[string]any), cellPublicKeyB64, now)
		default:
			return nil, fmt.Errorf("unexpected Hub assignment mode %q", mode)
		}
		return json.Marshal(body)
	}
}

func setNativeRecoveryAssignment(assignment map[string]any, cellPublicKeyB64 string, now time.Time) {
	assignment["cell_id"] = "cell-test"
	assignment["assignment_generation"] = float64(2)
	assignment["endpoint_revision"] = float64(2)
	assignment["lease_expires_at"] = now.Add(time.Hour).Format(time.RFC3339)
	endpoint := assignment["nhp_udp_endpoint"].(map[string]any)
	endpoint["host"] = connectorIntegrationCellHost
	endpoint["port"] = float64(443)
	endpoint["server_public_key_b64"] = cellPublicKeyB64
}

func nativeRecoveryCellReply(recovery *conformance.AgentCredentialRecoveryFile) func([]byte) ([]byte, error) {
	return func(request []byte) ([]byte, error) {
		query, _, err := nativeRecoveryQuery(request)
		if err != nil || query != "agent_credential_recovery" {
			return nil, fmt.Errorf("unexpected cell request %q: %w", query, err)
		}
		return []byte(recovery.PublicExchanges[conformance.AgentCredentialRecoveryCellPhase].SuccessBodyJSON), nil
	}
}

func TestOpenNativeRegisteredClient_ExplicitLoginUsesRealConnectorRecovery(t *testing.T) {
	recovery, err := conformance.AgentCredentialRecovery()
	if err != nil {
		t.Fatal(err)
	}
	assignment, err := conformance.AgentAssignmentGolden()
	if err != nil {
		t.Fatal(err)
	}
	agentPrivate := nativeRecoveryFixtureKey(t, assignment.Keys.Agent.StaticPrivHex)
	agentPublic := nativeRecoveryFixtureKey(t, assignment.Keys.Agent.StaticPubHex)
	hubPrivate := nativeRecoveryFixtureKey(t, assignment.Keys.Hub.StaticPrivHex)
	hubPublic := nativeRecoveryFixtureKey(t, assignment.Keys.Hub.StaticPubHex)
	cellPrivate := nativeRecoveryFixtureKey(t, assignment.Keys.AssignedCell.StaticPrivHex)
	cellPublic := nativeRecoveryFixtureKey(t, assignment.Keys.AssignedCell.StaticPubHex)
	cellPublicB64 := base64.StdEncoding.EncodeToString(cellPublic)
	now := time.Now().UTC().Round(time.Second)

	hub := newNativeRecoveryUDPServer(t, hubPrivate, agentPublic, true,
		nativeRecoveryHubReply(t, recovery, assignment, cellPublicB64, now))
	cell := newNativeRecoveryUDPServer(t, cellPrivate, agentPublic, false, nativeRecoveryCellReply(recovery))
	// The native transport rejects special-purpose IP ranges before dialing.
	// The injected dialer maps these synthetic route labels to local
	// sockets, so the test sends no packet to either public address.
	hubAddress := netip.MustParseAddr("8.8.4.4")
	cellAddress := netip.MustParseAddr("9.9.9.9")
	route := nativeRecoveryRoute{
		hosts: map[string]netip.Addr{
			connectorIntegrationHubHost:  hubAddress,
			connectorIntegrationCellHost: cellAddress,
		},
		targets: map[string]string{
			hubAddress.String():  hub.conn.LocalAddr().String(),
			cellAddress.String(): cell.conn.LocalAddr().String(),
		},
	}

	stateDir, err := filepath.EvalSymlinks(connectorStateTestDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(connectorstateowner.EnvKeyProvider, connectorstateowner.KeyProviderFile)
	stateOwner, err := connectorstateowner.NewSDKStore(stateDir, connectorIntegrationAgentID)
	if err != nil {
		t.Fatal(err)
	}
	stateStore, err := stateOwner.Handoff()
	if err != nil {
		t.Fatal(err)
	}
	registeredAt := now.Add(-time.Hour)
	oldDeviceKey := conformance.AgentAssignmentDeviceAPIKeyFixture
	if err := stateStore.SaveAgentState(context.Background(), &qurl.AgentState{
		AgentID:                  connectorIntegrationAgentID,
		PrivateKeyB64:            base64.StdEncoding.EncodeToString(agentPrivate),
		PublicKeyB64:             base64.StdEncoding.EncodeToString(agentPublic),
		SchemaVersion:            8,
		RegisteredAt:             &registeredAt,
		DeviceAPIKey:             oldDeviceKey,
		DeviceAPIKeyID:           "key_OldCli123456",
		EnrollmentCredentialKind: "bootstrap",
		Assignment: &qurl.AgentAssignment{
			CellID: "cell-test", AssignmentGeneration: 1, EndpointRevision: 1,
			LeaseExpiresAt: now.Add(time.Hour),
			Endpoint: qurl.NHPUDPEndpoint{
				Host: connectorIntegrationCellHost, Port: 443, ServerPublicKeyB64: cellPublicB64,
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := stateOwner.Close(); err != nil {
		t.Fatal(err)
	}

	srv := apitest.NewServer(t)
	srv.Script(http.MethodGet, "/v1/me", apitest.HandlerAPIKeyInvalid401(t))
	registry := &ownerOnlyTestShareRegistry{}
	opts := &globalOpts{
		resolvedEndpoint: srv.URL,
		version:          "real-connector-recovery-test",
		lookupEnv: func(string) (string, bool) {
			t.Fatal("explicit connector recovery unexpectedly read ambient account authority")
			return "", false
		},
		resolveShareStateDir: func(string) (string, error) { return stateDir, nil },
		resolveHubBootstrap: func() (qurl.HubBootstrap, error) {
			return qurl.HubBootstrap{
				Host: connectorIntegrationHubHost, Port: 443,
				ServerPublicKeyB64: base64.StdEncoding.EncodeToString(hubPublic),
			}, nil
		},
		openShareRegistry: func(string) (localShareRegistry, error) { return registry, nil },
		openNativeRuntime: func(ctx context.Context, cfg connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
			cfg.UDPOptions = []qurl.AgentRuntimeUDPOption{
				qurl.WithAgentRuntimeUDPResolver(route),
				qurl.WithAgentRuntimeUDPDialer(route),
				qurl.WithAgentRuntimeUDPBounds(5*time.Second, 1),
			}
			return connectorshare.OpenNativeRuntime(ctx, cfg)
		},
	}
	validatedAccountKey := recovery.Fixtures.RecoveryCredential
	account, err := opts.apiClient(validatedAccountKey)
	if err != nil {
		t.Fatal(err)
	}
	client, identity, err := opts.openNativeRegisteredClient(
		context.Background(), account, validatedAccountKey, &qurlapi.Identity{OwnerID: apitest.MeOwnerID},
	)
	if err != nil {
		t.Fatal(err)
	}
	if client == nil || identity == nil || identity.OwnerID != apitest.MeOwnerID {
		t.Fatalf("recovered identity = %#v", identity)
	}
	requests := srv.Requests()
	if len(requests) != 2 {
		t.Fatalf("registered identity requests = %d, want initial rejection and one retry", len(requests))
	}
	if got := strings.TrimPrefix(requests[0].Header.Get("Authorization"), "Bearer "); got != oldDeviceKey {
		t.Fatalf("initial registered-device key changed: %q", got)
	}
	replacementKey := strings.TrimPrefix(requests[1].Header.Get("Authorization"), "Bearer ")
	if replacementKey == "" || replacementKey == oldDeviceKey || !strings.HasPrefix(replacementKey, "lv_live_") {
		t.Fatalf("retry did not use the connector-promoted replacement credential")
	}
	if got := len(hub.snapshot()); got != 2 {
		t.Fatalf("real connector Hub exchanges = %d, want recovery and required post-recovery refresh", got)
	}
	if got := len(cell.snapshot()); got != 1 {
		t.Fatalf("real connector cell exchanges = %d, want one recovery completion", got)
	}
	if err := opts.closeAPIClient(); err != nil {
		t.Fatal(err)
	}
	if registry.bindCalls != 1 {
		t.Fatalf("owner bindings = %d, want one after the successful retry", registry.bindCalls)
	}
	if got := connectorstate.ConfiguredAgentID(); got != "" {
		t.Fatalf("test unexpectedly changed the configured connector identity: %q", got)
	}
}
