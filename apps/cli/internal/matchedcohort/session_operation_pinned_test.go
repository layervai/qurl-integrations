package matchedcohort

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	conformance "github.com/layervai/qurl-conformance"
	qurl "github.com/layervai/qurl-go/qurl"
	"github.com/layervai/qurl-go/relayknock"
	"github.com/layervai/qurl-go/relayknock/relayknocktest"
)

const pinnedTestDeviceKey = "lv_live_AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"

func TestQURLSessionRuntimePinnedAdmissionKeepsPhysicalAssignment(t *testing.T) {
	contract, err := conformance.AgentAssignmentGolden()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name             string
		generation       int64
		revision         int64
		wantStateAdvance bool
	}{
		{name: "same-cell generation move is refused", generation: 2, revision: 1},
		{name: "same-assignment lease and revision renew", generation: 1, revision: 2, wantStateAdvance: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now().UTC()
			agentPrivate := pinnedHex(t, contract.Keys.Agent.StaticPrivHex)
			agentPublic := pinnedHex(t, contract.Keys.Agent.StaticPubHex)
			hubPrivate := pinnedHex(t, contract.Keys.Hub.StaticPrivHex)
			hubPublic := pinnedHex(t, contract.Keys.Hub.StaticPubHex)
			cellPrivate := pinnedHex(t, contract.Keys.AssignedCell.StaticPrivHex)
			cellPublic := pinnedHex(t, contract.Keys.AssignedCell.StaticPubHex)
			cellKey := base64.StdEncoding.EncodeToString(cellPublic)

			assignmentBody := rewritePinnedRefreshAssignment(contract.RefreshAssignment.Result.BodyJSON,
				test.generation, test.revision, now.Add(time.Hour), cellKey)
			hub := newPinnedHubServer(t, hubPrivate, agentPublic, assignmentBody)
			cell := newPinnedCellServer(t, cellPrivate, agentPublic)
			resolver := pinnedResolver{hosts: map[string]netip.Addr{
				"hub.sandbox.layerv.xyz": netip.MustParseAddr("8.8.8.8"),
				"cell0.nhp.layerv.ai":    netip.MustParseAddr("9.9.9.9"),
			}}
			dialer := pinnedDialer{targets: map[string]string{
				"8.8.8.8": hub.conn.LocalAddr().String(),
				"9.9.9.9": cell.conn.LocalAddr().String(),
			}}

			blobs := newMemoryBlobs()
			store, err := NewDurableAgentStateStore(blobs, "pinned/agent-state")
			if err != nil {
				t.Fatal(err)
			}
			registered := now.Add(-time.Hour)
			state := &qurl.AgentState{AgentID: "agent-conform", PrivateKeyB64: base64.StdEncoding.EncodeToString(agentPrivate),
				PublicKeyB64: base64.StdEncoding.EncodeToString(agentPublic), SchemaVersion: 7, RegisteredAt: &registered,
				DeviceAPIKey: pinnedTestDeviceKey, DeviceAPIKeyID: "key_DvK9mN2pQr7S",
				Assignment: &qurl.AgentAssignment{CellID: "cell0", AssignmentGeneration: 1, EndpointRevision: 1,
					LeaseExpiresAt: now.Add(time.Minute), Endpoint: qurl.NHPUDPEndpoint{Host: "cell0.nhp.layerv.ai", Port: 443,
						ServerPublicKeyB64: cellKey}}}
			if err := store.SaveAgentState(context.Background(), state); err != nil {
				t.Fatal(err)
			}
			before, err := store.Reference(context.Background())
			if err != nil {
				t.Fatal(err)
			}

			runtime := qurlSessionRuntime{
				registrationOptions: []qurl.AgentRuntimeRegistrationOption{
					qurl.WithAgentRuntimeHub(qurl.HubBootstrap{Host: "hub.sandbox.layerv.xyz", Port: 443,
						ServerPublicKeyB64: base64.StdEncoding.EncodeToString(hubPublic)}),
					qurl.WithAgentRuntimeUDPResolver(resolver), qurl.WithAgentRuntimeUDPDialer(dialer),
					qurl.WithAgentRuntimeUDPBounds(2*time.Second, 1), qurl.WithAgentRuntimeAssignmentRetryBudget(1, 2*time.Second),
				},
				udpOptions: []qurl.AgentRuntimeUDPOption{qurl.WithAgentRuntimeUDPResolver(resolver),
					qurl.WithAgentRuntimeUDPDialer(dialer), qurl.WithAgentRuntimeUDPBounds(2*time.Second, 1)},
			}
			preparedAt := now.Add(-time.Second)
			operation, err := runtime.Prepare(context.Background(), store, PrepareOperationRequest{AWSAccountID: "111122223333",
				AWSRegion: "us-east-2", Identity: FixedIdentity{AgentID: "agent-conform", OwnerID: "sandbox-owner@clients",
					ResourceID: testProtectedResourceID(t, 1), KnockResourceID: "resource-public-key"}, Cohort: CohortPlan{CellID: "cell0", SessionControlTable: "sandbox-session-control",
					QURLAgentKeysTable: "sandbox-agent-keys"}, RunID: "0123456789abcdef", RunAttempt: 1,
				PreparedAt: preparedAt, ExpiresAt: preparedAt.Add(20 * time.Minute)})
			if err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			live, admission, err := runtime.Admit(context.Background(), store, OperationRecord{Operation: *operation})
			if err != nil || live == nil || admission.CellID != "cell0" {
				t.Fatalf("pinned Admit = %#v %#v %v", live, admission, err)
			}
			owned := live.value.(*qurlLiveSession)
			clear(owned.privateKey)
			owned.binding.Destroy()
			live.ACToken = ""
			live.value = nil
			after, err := store.Reference(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			persisted, err := store.LoadAgentState(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if test.wantStateAdvance {
				if after == before || persisted.Assignment.AssignmentGeneration != 1 || persisted.Assignment.EndpointRevision != 2 {
					t.Fatalf("same assignment renewal did not advance exact state: before=%#v after=%#v assignment=%#v", before, after, persisted.Assignment)
				}
			} else if after != before || persisted.Assignment.AssignmentGeneration != 1 || persisted.Assignment.EndpointRevision != 1 {
				t.Fatalf("generation move changed durable state: before=%#v after=%#v assignment=%#v", before, after, persisted.Assignment)
			}
			hub.wait(t)
			cell.wait(t)
		})
	}
}

type pinnedHubServer struct {
	conn *net.UDPConn
	done chan struct{}
}

func newPinnedHubServer(t *testing.T, serverPrivate, agentPublic []byte, assignmentBody string) *pinnedHubServer {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	server := &pinnedHubServer{conn: conn, done: make(chan struct{})}
	go func() {
		defer close(server.done)
		buffer := make([]byte, 4096)
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, remote, readErr := conn.ReadFromUDP(buffer)
		if readErr != nil {
			t.Errorf("read Hub assignment request: %v", readErr)
			return
		}
		request, openErr := relayknocktest.OpenInitiatorMessage(serverPrivate, agentPublic, bytes.Clone(buffer[:n]))
		if openErr != nil || request.Type != relayknock.TypeListRequest || !pinnedAssignmentRequest(request.Body) {
			t.Errorf("open Hub assignment request: %#v %v", request, openErr)
			return
		}
		cookie := bytes.Repeat([]byte{0x5a}, 32)
		challenge := []byte(fmt.Sprintf(`{"trxId":%d,"cookie":%q}`, request.Counter, base64.StdEncoding.EncodeToString(cookie)))
		if writeErr := writePinnedReply(conn, remote, serverPrivate, agentPublic, relayknock.TypeCookieChallenge,
			request.Counter+99, challenge, 1); writeErr != nil {
			t.Errorf("write Hub cookie: %v", writeErr)
			return
		}
		n, remote, readErr = conn.ReadFromUDP(buffer)
		if readErr != nil {
			t.Errorf("read Hub assignment proof: %v", readErr)
			return
		}
		proof, openErr := relayknocktest.OpenHubLSTCookieProofMessage(serverPrivate, agentPublic, cookie, bytes.Clone(buffer[:n]))
		if openErr != nil || !bytes.Equal(proof.Body, request.Body) {
			t.Errorf("open Hub assignment proof: %#v %v", proof, openErr)
			return
		}
		if writeErr := writePinnedReply(conn, remote, serverPrivate, agentPublic, relayknock.TypeListResult,
			proof.Counter, []byte(assignmentBody), 2); writeErr != nil {
			t.Errorf("write Hub assignment: %v", writeErr)
		}
	}()
	t.Cleanup(func() { _ = conn.Close() })
	return server
}

type pinnedCellServer struct {
	conn *net.UDPConn
	done chan struct{}
}

func newPinnedCellServer(t *testing.T, serverPrivate, agentPublic []byte) *pinnedCellServer {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	server := &pinnedCellServer{conn: conn, done: make(chan struct{})}
	go func() {
		defer close(server.done)
		buffer := make([]byte, 4096)
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, remote, readErr := conn.ReadFromUDP(buffer)
		if readErr != nil {
			t.Errorf("read cell knock: %v", readErr)
			return
		}
		request, openErr := relayknocktest.OpenInitiatorMessage(serverPrivate, agentPublic, bytes.Clone(buffer[:n]))
		if openErr != nil || request.Type != relayknock.TypeKnock {
			t.Errorf("open cell knock: %#v %v", request, openErr)
			return
		}
		body := []byte(`{"errCode":"0","sessId":123,"cellId":"cell0","sessIssuedAtMillis":1800000000000,"runId":"0123456789abcdef","runAttempt":1,"resHost":{"resource-public-key":"frps.cell0.example:7000"},"opnTime":900,"agentAddr":"203.0.113.9:49152","acTokens":{"resource-public-key":"ac-session"},"preActions":{"resource-public-key":null}}`)
		if writeErr := writePinnedReply(conn, remote, serverPrivate, agentPublic, relayknock.TypeACK, request.Counter, body, 3); writeErr != nil {
			t.Errorf("write cell ACK: %v", writeErr)
		}
	}()
	t.Cleanup(func() { _ = conn.Close() })
	return server
}

func (s *pinnedHubServer) wait(t *testing.T)  { t.Helper(); waitPinnedServer(t, s.done) }
func (s *pinnedCellServer) wait(t *testing.T) { t.Helper(); waitPinnedServer(t, s.done) }

func waitPinnedServer(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(6 * time.Second):
		t.Fatal("timed out waiting for pinned-assignment server")
	}
}

func writePinnedReply(conn *net.UDPConn, remote *net.UDPAddr, serverPrivate, agentPublic []byte,
	replyType int, counter uint64, body []byte, sequence byte,
) error {
	packet, err := relayknocktest.BuildReply(replyType, &relayknock.KnockInputs{DeviceStaticPriv: serverPrivate,
		ServerStaticPub: agentPublic, EphemeralPriv: bytes.Repeat([]byte{0x40 + sequence}, 32),
		TimestampNanos: uint64(time.Now().UnixNano()), Counter: counter, Preamble: uint32(0x50607080) + uint32(sequence), Body: body})
	if err != nil {
		return err
	}
	_, err = conn.WriteToUDP(packet, remote)
	return err
}

func pinnedAssignmentRequest(body []byte) bool {
	var request struct {
		UsrData struct {
			Query string `json:"query"`
		} `json:"usrData"`
	}
	return json.Unmarshal(body, &request) == nil && request.UsrData.Query == "cell_assignment"
}

func rewritePinnedRefreshAssignment(body string, generation, revision int64, lease time.Time, cellKey string) string {
	return strings.NewReplacer(`"assignment_generation":1`, fmt.Sprintf(`"assignment_generation":%d`, generation),
		`"endpoint_revision":1`, fmt.Sprintf(`"endpoint_revision":%d`, revision),
		`"lease_expires_at":"2026-07-16T12:00:00Z"`, fmt.Sprintf(`"lease_expires_at":%q`, lease.Format(time.RFC3339)),
		`"server_public_key_b64":"Xwm3+XpAtQIgaXBktDsnQRsHpKof4FNwsnUZgmmD0w0="`, fmt.Sprintf(`"server_public_key_b64":%q`, cellKey)).Replace(body)
}

func pinnedHex(t *testing.T, value string) []byte {
	t.Helper()
	raw, err := hex.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

type pinnedResolver struct {
	hosts map[string]netip.Addr
}

func (r pinnedResolver) LookupNetIP(_ context.Context, network, host string) ([]netip.Addr, error) {
	if network != "ip" {
		return nil, fmt.Errorf("unexpected network %q", network)
	}
	address, ok := r.hosts[host]
	if !ok {
		return nil, fmt.Errorf("unexpected host %q", host)
	}
	return []netip.Addr{address}, nil
}

type pinnedDialer struct {
	targets map[string]string
}

func (d pinnedDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	target, ok := d.targets[host]
	if !ok {
		return nil, fmt.Errorf("unexpected address %q", host)
	}
	return (&net.Dialer{}).DialContext(ctx, network, target)
}
