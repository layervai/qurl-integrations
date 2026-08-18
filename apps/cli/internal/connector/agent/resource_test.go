package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	qurl "github.com/layervai/qurl-go/qurl"
)

// connectorWire is the producer's generic resource payload as the mock hub
// serves it.
type connectorWire struct {
	ResourceID         string `json:"resource_id"`
	ConnectorRoutingID string `json:"connector_routing_id"`
	KnockResourceID    string `json:"knock_resource_id"`
	Type               string `json:"type"`
	Status             string `json:"status"`
	Slug               string `json:"slug"`
}

var routingIDEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// testConnectorRow mints a wire row that satisfies the SDK's fail-closed
// response validation: a real P-256 resource id, a canonical c-prefixed
// routing id, and a distinct opaque knock resource.
func testConnectorRow(t *testing.T, slug string) connectorWire {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(der)
	return connectorWire{
		ResourceID:         base64.RawURLEncoding.EncodeToString(der),
		ConnectorRoutingID: "c-" + routingIDEncoding.EncodeToString(digest[:]),
		KnockResourceID:    "resource-public-key",
		Type:               "tunnel",
		Status:             "active",
		Slug:               slug,
	}
}

// mockHub is a scriptable mock of the producer's Connector-resource routes,
// following the CLI's apitest pattern: default happy-path handlers plus a
// consume-once script queue keyed by "METHOD /path".
type mockHub struct {
	*httptest.Server
	t *testing.T

	mu       sync.Mutex
	requests []string
	scripts  map[string][]http.HandlerFunc
	rows     []connectorWire
}

func newMockHub(t *testing.T, rows ...connectorWire) *mockHub {
	t.Helper()
	m := &mockHub{t: t, scripts: map[string][]http.HandlerFunc{}, rows: rows}
	m.Server = httptest.NewServer(http.HandlerFunc(m.handle))
	t.Cleanup(m.Close)
	return m
}

func (m *mockHub) script(method, path string, handlers ...http.HandlerFunc) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := method + " " + path
	m.scripts[key] = append(m.scripts[key], handlers...)
}

func (m *mockHub) requestLog() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.requests...)
}

func (m *mockHub) handle(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	m.requests = append(m.requests, r.Method+" "+r.URL.Path)
	var scripted http.HandlerFunc
	key := r.Method + " " + r.URL.Path
	if queue := m.scripts[key]; len(queue) > 0 {
		scripted = queue[0]
		m.scripts[key] = queue[1:]
	}
	m.mu.Unlock()
	if scripted != nil {
		scripted(w, r)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/resources":
		slug := r.URL.Query().Get("slug")
		matches := []connectorWire{}
		for _, row := range m.rows {
			if row.Slug == slug {
				matches = append(matches, row)
			}
		}
		writeJSON(m.t, w, http.StatusOK, map[string]any{"data": matches})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/resources":
		var body struct {
			Type         string `json:"type"`
			Slug         string `json:"slug"`
			FindOrCreate bool   `json:"find_or_create"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Type != "tunnel" || !body.FindOrCreate {
			writeJSON(m.t, w, http.StatusBadRequest, map[string]any{"code": "invalid_request", "title": "Bad Request", "detail": "ensure body must pin type and find_or_create"})
			return
		}
		row := testConnectorRow(m.t, body.Slug)
		m.mu.Lock()
		m.rows = append(m.rows, row)
		m.mu.Unlock()
		writeJSON(m.t, w, http.StatusCreated, map[string]any{"data": row, "meta": map[string]any{"found_existing": false}})
	default:
		writeJSON(m.t, w, http.StatusNotFound, map[string]any{"code": "not_found", "title": "Not Found", "detail": "no such route in the mock hub"})
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, body any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Errorf("encode mock response: %v", err)
	}
}

func newMockClient(t *testing.T, m *mockHub) *qurl.Client {
	t.Helper()
	client, err := qurl.NewClient(qurl.BearerToken("test-device-credential"), qurl.WithBaseURL(m.URL))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestResolveResourceFindsExistingBySlug(t *testing.T) {
	t.Parallel()
	row := testConnectorRow(t, "my-service")
	m := newMockHub(t, row)
	client := newMockClient(t, m)

	got, err := ResolveResource(context.Background(), client, "my-service")
	if err != nil {
		t.Fatal(err)
	}
	if got.ResourceID != row.ResourceID || got.Slug != "my-service" || got.KnockResourceID != row.KnockResourceID {
		t.Fatalf("ResolveResource = %+v, want the existing row", got)
	}
	for _, req := range m.requestLog() {
		if strings.HasPrefix(req, http.MethodPost) {
			t.Fatalf("existing slug still triggered a mutation: %v", m.requestLog())
		}
	}
}

func TestResolveResourceEnsuresWhenAbsent(t *testing.T) {
	t.Parallel()
	m := newMockHub(t)
	client := newMockClient(t, m)

	got, err := ResolveResource(context.Background(), client, "fresh-connector")
	if err != nil {
		t.Fatal(err)
	}
	if got.Slug != "fresh-connector" || got.ResourceID == "" || got.ConnectorRoutingID == "" {
		t.Fatalf("ensured resource = %+v", got)
	}
	log := m.requestLog()
	if len(log) != 2 || log[0] != "GET /v1/resources" || log[1] != "POST /v1/resources" {
		t.Fatalf("request order = %v, want read-by-slug then ensure", log)
	}
}

func TestResolveResourceSurfacesAuthoritativeEnsureRejection(t *testing.T) {
	t.Parallel()
	m := newMockHub(t)
	client := newMockClient(t, m)
	m.script(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusForbidden, map[string]any{"code": "quota_exceeded", "title": "Forbidden", "detail": "connector quota reached"})
	})

	_, err := ResolveResource(context.Background(), client, "quota-blocked")
	if err == nil {
		t.Fatal("ResolveResource = nil error, want the platform rejection")
	}
	var apiErr *qurl.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusForbidden {
		t.Fatalf("rejection = %v, want the 403 APIError preserved", err)
	}
	// A definitive rejection must not trigger the read-back adoption path.
	log := m.requestLog()
	if len(log) != 2 {
		t.Fatalf("requests after authoritative rejection = %v, want no reconcile read", log)
	}
}

func TestResolveResourceReconcilesUncertainEnsure(t *testing.T) {
	t.Parallel()
	row := testConnectorRow(t, "flaky-net")
	m := newMockHub(t)
	client := newMockClient(t, m)
	// First read: not found. Ensure: 500 (outcome unknown). Reconcile read:
	// the row is visible — the mutation had committed server-side.
	m.script(http.MethodGet, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{"data": []connectorWire{}})
	})
	m.script(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusInternalServerError, map[string]any{"code": "internal", "title": "Internal", "detail": "please retry"})
	})
	m.script(http.MethodGet, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{"data": []connectorWire{row}})
	})

	got, err := ResolveResource(context.Background(), client, "flaky-net")
	if err != nil {
		t.Fatalf("uncertain ensure with committed row = %v, want adoption", err)
	}
	if got.ResourceID != row.ResourceID {
		t.Fatalf("adopted resource = %+v, want the committed row", got)
	}
}

func TestResolveResourceUncertainEnsureWithoutRowFailsClosed(t *testing.T) {
	t.Parallel()
	m := newMockHub(t)
	client := newMockClient(t, m)
	m.script(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusInternalServerError, map[string]any{"code": "internal", "title": "Internal", "detail": "please retry"})
	})

	_, err := ResolveResource(context.Background(), client, "lost-outcome")
	if err == nil || !strings.Contains(err.Error(), "not retrying automatically") {
		t.Fatalf("uncertain ensure without a visible row = %v, want fail-closed uncertainty", err)
	}
	if !errors.Is(err, qurl.ErrConnectorResourceOutcomeUnknown) {
		t.Fatalf("uncertainty error = %v, want ErrConnectorResourceOutcomeUnknown preserved", err)
	}
}

func TestResolveResourceRejectsNilClient(t *testing.T) {
	t.Parallel()
	if _, err := ResolveResource(context.Background(), nil, "any"); err == nil {
		t.Fatal("nil client accepted")
	}
}

func TestKnockResourceIDPrefersTrimmedEnvOverride(t *testing.T) {
	resource := &qurl.ConnectorResource{KnockResourceID: "producer-assigned"}
	tests := []struct {
		name     string
		env      string
		envSet   bool
		resource *qurl.ConnectorResource
		want     string
		wantErr  bool
	}{
		{name: "producer mapping without override", resource: resource, want: "producer-assigned"},
		{name: "explicit override wins", env: "  custom-admission-target  ", envSet: true, resource: resource, want: "custom-admission-target"},
		{name: "whitespace override falls back to producer mapping", env: " \t ", envSet: true, resource: resource, want: "producer-assigned"},
		{name: "missing mapping without override fails closed", resource: &qurl.ConnectorResource{}, wantErr: true},
		{name: "nil resource without override fails closed", wantErr: true},
		{name: "override supplies missing producer mapping", env: "custom-admission-target", envSet: true, resource: &qurl.ConnectorResource{}, want: "custom-admission-target"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(EnvKnockResourceID, "restore-after-test")
			if tt.envSet {
				t.Setenv(EnvKnockResourceID, tt.env)
			} else if err := os.Unsetenv(EnvKnockResourceID); err != nil {
				t.Fatal(err)
			}
			got, err := KnockResourceID(tt.resource)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("KnockResourceID = %q, want fail-closed error", got)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("KnockResourceID = (%q, %v), want %q", got, err, tt.want)
			}
		})
	}
}
