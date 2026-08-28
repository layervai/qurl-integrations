package qurlapi

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"encoding/base64"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
)

type registeredAPIStateStore struct {
	state *qurl.AgentState
}

func (s *registeredAPIStateStore) LoadAgentState(context.Context) (*qurl.AgentState, error) {
	if s == nil || s.state == nil {
		return nil, qurl.ErrAgentStateNotFound
	}
	stateCopy := *s.state
	if s.state.RegisteredAt != nil {
		registered := *s.state.RegisteredAt
		stateCopy.RegisteredAt = &registered
	}
	if s.state.Assignment != nil {
		assignment := *s.state.Assignment
		stateCopy.Assignment = &assignment
	}
	return &stateCopy, nil
}

func (*registeredAPIStateStore) SaveAgentState(context.Context, *qurl.AgentState) error {
	return errors.New("unexpected registered API state save")
}

func registeredAPIState(t *testing.T) *qurl.AgentState {
	t.Helper()
	private, err := ecdh.X25519().GenerateKey(bytes.NewReader(bytes.Repeat([]byte{0x61}, 64)))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Round(time.Second)
	return &qurl.AgentState{
		AgentID: "cli-registered-api", PrivateKeyB64: base64.StdEncoding.EncodeToString(private.Bytes()),
		PublicKeyB64: base64.StdEncoding.EncodeToString(private.PublicKey().Bytes()), SchemaVersion: 7,
		RegisteredAt: &now, DeviceAPIKey: "lv_live_AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8",
		DeviceAPIKeyID: "key_AbCdEf123456",
		Assignment: &qurl.AgentAssignment{CellID: "cell-01", AssignmentGeneration: 7, EndpointRevision: 1,
			LeaseExpiresAt: now.Add(time.Hour), Endpoint: qurl.NHPUDPEndpoint{Host: "cell0.nhp.layerv.ai", Port: 443,
				ServerPublicKeyB64: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x52}, 32))}},
	}
}

func newRegisteredTestClient(t *testing.T, srv *apitest.Server) Client {
	t.Helper()
	client, err := NewRegistered(context.Background(), &Config{
		BaseURL: srv.URL, Version: "registered-test", HTTPClient: srv.Client(),
		NewRequestID: func() string { return testRequestID },
	}, &registeredAPIStateStore{state: registeredAPIState(t)})
	if err != nil {
		t.Fatalf("NewRegistered: %v", err)
	}
	return client
}

func TestNewRegistered_UsesDeviceCredentialAcrossCustomerResourceSurface(t *testing.T) {
	srv := apitest.NewServer(t)
	sharingPath := "/v1/resources/" + srv.Key.CRID + "/sharing"
	srv.Script(http.MethodGet, sharingPath, func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{
			"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID,
			"desired_state": "on", "serving_epoch": 1, "connection_state": "serving",
		}, nil)
	})
	client := newRegisteredTestClient(t, srv)

	if _, err := client.Me(context.Background()); err != nil {
		t.Fatalf("Me: %v", err)
	}
	if _, err := client.Publish(context.Background(), "https://example.test", PublishOptions{}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if _, err := client.List(context.Background(), ListOptions{Limit: 20}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if _, err := client.Resolve(context.Background(), srv.Key.CRID, ResolveOptions{}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, err := client.Sharing(context.Background(), srv.Key.CRID); err != nil {
		t.Fatalf("Sharing: %v", err)
	}

	want := "Bearer " + registeredAPIState(t).DeviceAPIKey
	requests := srv.Requests()
	if len(requests) != 5 {
		t.Fatalf("registered customer requests = %d, want 5", len(requests))
	}
	for _, request := range requests {
		if got := request.Header.Get("Authorization"); got != want {
			t.Errorf("%s %s authorization = %q, want sealed device credential", request.Method, request.Path, got)
		}
	}
}

func TestNewRegistered_AllowsConnectorEnrollmentButHidesAccountAuthority(t *testing.T) {
	srv := apitest.NewServer(t)
	state := registeredAPIState(t)
	if client, err := NewRegistered(context.Background(), &Config{
		BaseURL: srv.URL, APIKey: "account-key-must-not-be-retained", Version: "test",
	}, &registeredAPIStateStore{state: state}); client != nil || !errors.Is(err, qurl.ErrInvalidClientConfig) {
		t.Fatalf("registered open with account key = %T, %v", client, err)
	}

	srv.Script(http.MethodPost, "/v1/api-keys", func(w http.ResponseWriter, _ *http.Request) {
		expiresAt := time.Now().Add(time.Hour)
		apitest.WriteEnvelope(t, w, http.StatusCreated, map[string]any{
			"api_key": "lv_live_AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8",
			"key_id":  "key_Connector1234",
			"kind":    "enrollment_token",
			"target":  "connector",
			"claims": []map[string]string{{
				"type": "connector", "id": "registered-client-connector",
			}},
			"status": "active", "expires_at": expiresAt,
		}, nil)
	})
	client := newRegisteredTestClient(t, srv)
	token, err := client.MintConnectorEnrollmentToken(context.Background(), MintConnectorEnrollmentTokenOptions{
		ConnectorID: "registered-client-connector", IdempotencyKey: "0123456789abcdef0123456789abcdef",
	})
	if err != nil || token == nil || token.Token == "" {
		t.Fatalf("registered Connector enrollment = %#v, %v", token, err)
	}
	if _, ok := client.(AccountClient); ok {
		t.Fatal("registered client recovered account enrollment authority through type assertion")
	}
	requests := srv.Requests()
	if len(requests) != 1 || requests[0].Header.Get("Authorization") != "Bearer "+state.DeviceAPIKey {
		t.Fatalf("Connector enrollment requests = %#v", requests)
	}
}

func TestNewRegistered_RestartNeverReplaysRateLimit(t *testing.T) {
	srv := apitest.NewServer(t)
	path := "/v1/resources/" + srv.Key.CRID + "/sharing/restart"
	srv.Script(http.MethodPost, path, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "0")
		apitest.WriteProblem(t, w, http.StatusTooManyRequests, "rate_limited", "Rate limited", "result is ambiguous")
	})
	client := newRegisteredTestClient(t, srv)
	if _, err := client.RestartSharing(context.Background(), srv.Key.CRID); err == nil {
		t.Fatal("registered RestartSharing unexpectedly succeeded")
	}
	if got := len(srv.Requests()); got != 1 {
		t.Fatalf("registered restart requests = %d, want one", got)
	}
}
