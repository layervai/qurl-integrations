package qurlapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
)

const (
	testConnectorID    = "conn-cli-enrollment"
	testIdempotencyKey = "0123456789abcdef0123456789abcdef"
	testEnrollmentKey  = "lv_test_enrollmentsecret123456789"
)

func TestMintAgentEnrollmentTokenSendsUnboundOneShotShape(t *testing.T) {
	expiresAt := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/api-keys" {
			t.Errorf("request = %s %s, want POST /v1/api-keys", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Idempotency-Key"); got != testIdempotencyKey {
			t.Errorf("Idempotency-Key = %q", got)
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body) != 4 {
			t.Errorf("request keys = %v, want kind/name/target/expires_in only", body)
		}
		if _, ok := body["claims"]; ok {
			t.Error("unbound agent enrollment must omit claims")
		}
		if _, ok := body["scopes"]; ok {
			t.Error("agent enrollment must not send caller-selected scopes")
		}
		assertJSONString(t, body, "kind", connectorEnrollmentKind)
		assertJSONString(t, body, "name", "qURL CLI registered device")
		assertJSONString(t, body, "target", agentEnrollmentTarget)
		assertJSONString(t, body, "expires_in", connectorEnrollmentLifetime)
		data := validEnrollmentData(expiresAt)
		data["target"] = agentEnrollmentTarget
		data["claims"] = []map[string]string{}
		data["scopes"] = []string{"qurl:agent"}
		apitest.WriteEnvelope(t, w, http.StatusCreated, data, nil)
	}))
	t.Cleanup(srv.Close)
	client := newEnrollmentTestClient(t, srv.URL)
	account, ok := client.(AccountClient)
	if !ok {
		t.Fatalf("New returned %T, want AccountClient", client)
	}
	token, err := account.MintAgentEnrollmentToken(context.Background(), MintAgentEnrollmentTokenOptions{IdempotencyKey: testIdempotencyKey})
	if err != nil {
		t.Fatal(err)
	}
	if token.Token != testEnrollmentKey || token.KeyID != "key_enrollment_1" || !token.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("agent token = %+v", token)
	}
}

func TestMintConnectorEnrollmentTokenSendsPinnedWireShape(t *testing.T) {
	expiresAt := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/api-keys" {
			t.Errorf("request = %s %s, want POST /v1/api-keys", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Idempotency-Key"); got != testIdempotencyKey {
			t.Errorf("Idempotency-Key = %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer lv_test_logincredential123456789" {
			t.Errorf("Authorization = %q", got)
		}

		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if len(body) != 5 {
			t.Errorf("request keys = %v, want exactly kind/name/target/claims/expires_in", body)
		}
		if _, ok := body["scopes"]; ok {
			t.Error("enrollment request must not send caller-selected scopes")
		}
		assertJSONString(t, body, "kind", connectorEnrollmentKind)
		assertJSONString(t, body, "name", "qURL CLI Connector "+testConnectorID)
		assertJSONString(t, body, "target", connectorEnrollmentTarget)
		assertJSONString(t, body, "expires_in", connectorEnrollmentLifetime)
		var claims []connectorEnrollmentClaim
		if err := json.Unmarshal(body["claims"], &claims); err != nil {
			t.Fatalf("decode claims: %v", err)
		}
		if len(claims) != 1 || claims[0] != (connectorEnrollmentClaim{Type: "connector", ID: testConnectorID}) {
			t.Errorf("claims = %+v", claims)
		}

		apitest.WriteEnvelope(t, w, http.StatusCreated, validEnrollmentData(expiresAt), nil)
	}))
	t.Cleanup(srv.Close)

	var verbose []string
	client, err := New(&Config{
		BaseURL: srv.URL,
		APIKey:  "lv_test_logincredential123456789",
		Version: "test",
		Verbose: func(format string, args ...any) {
			verbose = append(verbose, fmt.Sprintf(format, args...))
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.MintConnectorEnrollmentToken(context.Background(), MintConnectorEnrollmentTokenOptions{
		ConnectorID:    testConnectorID,
		IdempotencyKey: testIdempotencyKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Token != testEnrollmentKey || result.KeyID != "key_enrollment_1" || !result.ExpiresAt.Equal(expiresAt) {
		t.Errorf("result = %+v", result)
	}
	if strings.Contains(strings.Join(verbose, "\n"), testEnrollmentKey) {
		t.Error("enrollment token leaked into verbose diagnostics")
	}
}

func TestMintConnectorEnrollmentTokenCanonicalOriginAppendsV1ExactlyOnce(t *testing.T) {
	expiresAt := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	responseBody, err := json.Marshal(map[string]any{"data": validEnrollmentData(expiresAt)})
	if err != nil {
		t.Fatal(err)
	}

	var requestURL string
	client, err := New(&Config{
		BaseURL: "https://api.layerv.xyz",
		APIKey:  "lv_test_logincredential123456789",
		Version: "test",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			requestURL = req.URL.String()
			return &http.Response{
				StatusCode: http.StatusCreated,
				Header:     make(http.Header),
				Body:       io.NopCloser(bytes.NewReader(responseBody)),
				Request:    req,
			}, nil
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.MintConnectorEnrollmentToken(context.Background(), MintConnectorEnrollmentTokenOptions{
		ConnectorID:    testConnectorID,
		IdempotencyKey: testIdempotencyKey,
	}); err != nil {
		t.Fatal(err)
	}
	if requestURL != "https://api.layerv.xyz/v1/api-keys" {
		t.Fatalf("enrollment request URL = %q, want one version prefix", requestURL)
	}
}

func TestMintConnectorEnrollmentTokenValidatesOptionsLocally(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	t.Cleanup(srv.Close)
	client := newEnrollmentTestClient(t, srv.URL)

	cases := map[string]MintConnectorEnrollmentTokenOptions{
		"missing connector": {IdempotencyKey: testIdempotencyKey},
		"short idempotency": {ConnectorID: testConnectorID, IdempotencyKey: strings.Repeat("x", 31)},
		"long idempotency":  {ConnectorID: testConnectorID, IdempotencyKey: strings.Repeat("x", 257)},
		"padded key":        {ConnectorID: testConnectorID, IdempotencyKey: " " + testIdempotencyKey},
		"newline key":       {ConnectorID: testConnectorID, IdempotencyKey: testIdempotencyKey + "\n"},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := client.MintConnectorEnrollmentToken(context.Background(), opts)
			if !errors.Is(err, qurl.ErrInvalidResourceRequest) {
				t.Errorf("err = %v, want ErrInvalidResourceRequest", err)
			}
		})
	}
	if requests != 0 {
		t.Errorf("invalid options sent %d requests", requests)
	}
}

func TestMintConnectorEnrollmentTokenRejectsInvalidResponses(t *testing.T) {
	future := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	cases := map[string]func(map[string]any){
		"missing token":    func(data map[string]any) { delete(data, "api_key") },
		"whitespace token": func(data map[string]any) { data["api_key"] = " " + testEnrollmentKey },
		"missing key id":   func(data map[string]any) { delete(data, "key_id") },
		"padded key id":    func(data map[string]any) { data["key_id"] = " key_enrollment_1" },
		"wrong kind":       func(data map[string]any) { data["kind"] = "api_key" },
		"wrong target":     func(data map[string]any) { data["target"] = "agent" },
		"missing claim":    func(data map[string]any) { data["claims"] = []any{} },
		"extra claim": func(data map[string]any) {
			data["claims"] = []map[string]string{{"type": "connector", "id": testConnectorID}, {"type": "connector", "id": "other"}}
		},
		"wrong claim type": func(data map[string]any) {
			data["claims"] = []map[string]string{{"type": "agent", "id": testConnectorID}}
		},
		"wrong claim id": func(data map[string]any) {
			data["claims"] = []map[string]string{{"type": "connector", "id": "other"}}
		},
		"inactive":       func(data map[string]any) { data["status"] = "revoked" },
		"missing expiry": func(data map[string]any) { delete(data, "expires_at") },
		"expired":        func(data map[string]any) { data["expires_at"] = time.Now().Add(-time.Minute) },
		"malformed expiry": func(data map[string]any) {
			data["expires_at"] = "not-a-timestamp"
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				data := validEnrollmentData(future)
				mutate(data)
				apitest.WriteEnvelope(t, w, http.StatusCreated, data, nil)
			}))
			t.Cleanup(srv.Close)
			client := newEnrollmentTestClient(t, srv.URL)

			_, err := client.MintConnectorEnrollmentToken(context.Background(), MintConnectorEnrollmentTokenOptions{
				ConnectorID:    testConnectorID,
				IdempotencyKey: testIdempotencyKey,
			})
			if err == nil {
				t.Fatal("invalid response unexpectedly succeeded")
			}
			if !errors.Is(err, qurl.ErrInvalidAPIResponse) {
				t.Errorf("err = %v, want ErrInvalidAPIResponse", err)
			}
			if strings.Contains(err.Error(), testEnrollmentKey) {
				t.Error("invalid-response error leaked the enrollment token")
			}
		})
	}
}

func TestMintConnectorEnrollmentTokenRetryKeepsBodyAndIdempotencyKey(t *testing.T) {
	expiresAt := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	var bodies [][]byte
	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		bodies = append(bodies, raw)
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		if len(bodies) == 1 {
			w.Header().Set("Retry-After", "0")
			apitest.WriteProblem(t, w, http.StatusTooManyRequests, "rate_limited", "Too Many Requests", "slow down")
			return
		}
		apitest.WriteEnvelope(t, w, http.StatusCreated, validEnrollmentData(expiresAt), nil)
	}))
	t.Cleanup(srv.Close)
	client := newEnrollmentTestClient(t, srv.URL)

	_, err := client.MintConnectorEnrollmentToken(context.Background(), MintConnectorEnrollmentTokenOptions{
		ConnectorID:    testConnectorID,
		IdempotencyKey: testIdempotencyKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 || !bytes.Equal(bodies[0], bodies[1]) {
		t.Errorf("retry bodies differ: %q", bodies)
	}
	if len(keys) != 2 || keys[0] != testIdempotencyKey || keys[1] != testIdempotencyKey {
		t.Errorf("retry idempotency keys = %q", keys)
	}
}

func TestMintConnectorEnrollmentTokenRetriesIdempotent503(t *testing.T) {
	expiresAt := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	var attempts int
	var sleeps []time.Duration
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if r.Header.Get("Idempotency-Key") != testIdempotencyKey {
			t.Errorf("attempt %d lost Idempotency-Key", attempts)
		}
		if attempts == 1 {
			w.Header().Set("Retry-After", "5")
			apitest.WriteProblem(t, w, http.StatusServiceUnavailable, "service_unavailable", "Service Unavailable", "retry safely")
			return
		}
		apitest.WriteEnvelope(t, w, http.StatusCreated, validEnrollmentData(expiresAt), nil)
	}))
	t.Cleanup(srv.Close)
	client, err := New(&Config{
		BaseURL: srv.URL,
		APIKey:  "lv_test_logincredential123456789",
		Version: "test",
		Sleep:   func(delay time.Duration) { sleeps = append(sleeps, delay) },
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := client.MintConnectorEnrollmentToken(context.Background(), MintConnectorEnrollmentTokenOptions{
		ConnectorID: testConnectorID, IdempotencyKey: testIdempotencyKey,
	}); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 || len(sleeps) != 1 || sleeps[0] != 5*time.Second {
		t.Fatalf("attempts=%d sleeps=%v, want 2 attempts and one 5s backoff", attempts, sleeps)
	}
}

func TestMintConnectorEnrollmentTokenMapsProblemResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteProblem(t, w, http.StatusForbidden, "insufficient_scope", "Forbidden", "minting enrollment tokens requires qurl:agent")
	}))
	t.Cleanup(srv.Close)
	client := newEnrollmentTestClient(t, srv.URL)

	_, err := client.MintConnectorEnrollmentToken(context.Background(), MintConnectorEnrollmentTokenOptions{
		ConnectorID:    testConnectorID,
		IdempotencyKey: testIdempotencyKey,
	})
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusForbidden || apiErr.Code != "insufficient_scope" || apiErr.RequestID != "req_test" {
		t.Errorf("mapped error = %+v", apiErr)
	}
	if !apiErr.ConnectorEnrollmentScopeRequired() {
		t.Error("enrollment scope failure lost its operation-specific remedy marker")
	}
}

func newEnrollmentTestClient(t *testing.T, baseURL string) Client {
	t.Helper()
	client, err := New(&Config{
		BaseURL: baseURL,
		APIKey:  "lv_test_logincredential123456789",
		Version: "test",
		Sleep:   func(time.Duration) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func validEnrollmentData(expiresAt time.Time) map[string]any {
	return map[string]any{
		"api_key":    testEnrollmentKey,
		"key_id":     "key_enrollment_1",
		"kind":       connectorEnrollmentKind,
		"target":     connectorEnrollmentTarget,
		"claims":     []map[string]string{{"type": connectorEnrollmentTarget, "id": testConnectorID}},
		"scopes":     []string{"qurl:agent", "qurl:write"},
		"status":     connectorEnrollmentStatus,
		"expires_at": expiresAt,
	}
}

func assertJSONString(t *testing.T, body map[string]json.RawMessage, field, want string) {
	t.Helper()
	var got string
	if err := json.Unmarshal(body[field], &got); err != nil {
		t.Fatalf("decode %s: %v", field, err)
	}
	if got != want {
		t.Errorf("%s = %q, want %q", field, got, want)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
