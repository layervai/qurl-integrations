package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	testBindingPath = externalBindingPath
	testAPIKeysPath = apiKeysPath
)

func writeBindingSuccess(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"api_key": map[string]string{
			"plaintext": testAPIKey,
			"key_id":    testKeyID,
		},
	})
}

func writeLegacyMintSuccess(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"data": map[string]string{
			"api_key":    testAPIKey,
			"key_id":     testKeyID,
			"key_prefix": testKeyPrefix,
		},
	})
}

func TestDependencyAuthFailureErrorIncludesWireFields(t *testing.T) {
	err := (&DependencyAuthFailureError{
		Method:     http.MethodPost,
		Path:       testAPIKeysPath,
		StatusCode: http.StatusForbidden,
		Code:       "insufficient_scope",
		RequestID:  "req_auth",
	}).Error()
	for _, want := range []string{http.MethodPost, testAPIKeysPath, "403", "insufficient_scope", "req_auth"} {
		if !strings.Contains(err, want) {
			t.Fatalf("error string %q missing %q", err, want)
		}
	}
}

func TestHTTPAPIKeyMinterMintWorkspaceHappyPath(t *testing.T) {
	var gotBody bindingRequest
	var gotAuth, gotMethod, gotPath, gotIdempotency string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotIdempotency = r.Header.Get("Idempotency-Key")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		writeBindingSuccess(t, w)
	}))
	t.Cleanup(srv.Close)

	m := &HTTPAPIKeyMinter{BaseURL: srv.URL, HTTPClient: srv.Client()}
	minted, err := m.MintWorkspaceAPIKey(context.Background(), "tok", testTenantID)
	if err != nil {
		t.Fatalf("MintWorkspaceAPIKey: %v", err)
	}
	if !minted.BindingBacked || minted.KeyPrefix != testKeyPrefix {
		t.Fatalf("unexpected minted key: %+v", minted)
	}
	if gotMethod != http.MethodPost || gotPath != testBindingPath || gotAuth != "Bearer tok" {
		t.Fatalf("unexpected request: method=%q path=%q auth=%q", gotMethod, gotPath, gotAuth)
	}
	if gotIdempotency != bindingIdempotencyKey(testTenantID) {
		t.Fatalf("Idempotency-Key = %q", gotIdempotency)
	}
	if gotBody.Provider != "teams" || gotBody.ExternalID != testTenantID || gotBody.DisplayName != "Teams tenant "+testTenantID {
		t.Fatalf("binding body = %+v", gotBody)
	}
}

func TestHTTPAPIKeyMinterMintWorkspaceFallsBackToLegacyOnRouteMissing(t *testing.T) {
	var paths []string
	var legacyIdempotency string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case testBindingPath:
			http.NotFound(w, r)
		case testAPIKeysPath:
			legacyIdempotency = r.Header.Get("Idempotency-Key")
			writeLegacyMintSuccess(t, w)
		default:
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	m := &HTTPAPIKeyMinter{BaseURL: srv.URL, HTTPClient: srv.Client()}
	minted, err := m.MintWorkspaceAPIKey(context.Background(), "tok", testTenantID)
	if err != nil {
		t.Fatalf("MintWorkspaceAPIKey fallback: %v", err)
	}
	if minted.BindingBacked {
		t.Fatalf("fallback mint should not be marked binding-backed: %+v", minted)
	}
	if strings.Join(paths, ",") != testBindingPath+","+testAPIKeysPath {
		t.Fatalf("paths = %v", paths)
	}
	if legacyIdempotency != "" {
		t.Fatalf("legacy fallback should omit Idempotency-Key, got %q", legacyIdempotency)
	}
}

func TestHTTPAPIKeyMinterMintWorkspaceReplacementUsesAPIKeysEndpoint(t *testing.T) {
	const oldKeyID = "key_oldoldold"
	var gotBody mintRequest
	var gotPath, gotIdempotency string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotIdempotency = r.Header.Get("Idempotency-Key")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		writeLegacyMintSuccess(t, w)
	}))
	t.Cleanup(srv.Close)

	m := &HTTPAPIKeyMinter{BaseURL: srv.URL, HTTPClient: srv.Client()}
	minted, err := m.MintWorkspaceReplacementAPIKey(context.Background(), "tok", testTenantID, oldKeyID)
	if err != nil {
		t.Fatalf("MintWorkspaceReplacementAPIKey: %v", err)
	}
	if minted.BindingBacked {
		t.Fatalf("replacement mint must not be binding-backed: %+v", minted)
	}
	if gotPath != testAPIKeysPath || gotIdempotency != replacementIdempotencyKey(testTenantID, oldKeyID) {
		t.Fatalf("unexpected request path/idempotency: %q %q", gotPath, gotIdempotency)
	}
	if gotBody.Name != "Teams tenant "+testTenantID {
		t.Fatalf("name = %q", gotBody.Name)
	}
}

func TestValidateAPIKey(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		wantErr error
	}{
		{name: "ok", status: http.StatusOK},
		{name: "unauthorized", status: http.StatusUnauthorized, wantErr: ErrStoredAPIKeyInvalid},
		{name: "forbidden", status: http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}))
			t.Cleanup(srv.Close)
			m := &HTTPAPIKeyMinter{BaseURL: srv.URL, HTTPClient: srv.Client()}
			err := m.ValidateAPIKey(context.Background(), testAPIKey)
			switch {
			case tc.wantErr == nil && tc.status == http.StatusOK && err != nil:
				t.Fatalf("ValidateAPIKey() err = %v", err)
			case tc.wantErr != nil && !errors.Is(err, tc.wantErr):
				t.Fatalf("ValidateAPIKey() err = %v, want %v", err, tc.wantErr)
			case tc.wantErr == nil && tc.status >= 400 && err == nil:
				t.Fatal("ValidateAPIKey() unexpectedly succeeded")
			}
		})
	}
	if err := (&HTTPAPIKeyMinter{BaseURL: "https://api.example.test"}).ValidateAPIKey(context.Background(), " "); !errors.Is(err, ErrStoredAPIKeyInvalid) {
		t.Fatalf("empty API key err = %v", err)
	}
}

func TestReadRevokedAPIKeyList(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`{"data":[{"key_id":"k_1","status":"revoked"},{"key_id":"k_2","status":"active"}],"meta":{"has_more":true,"next_cursor":"cursor-2"}}`)),
	}
	found, nextCursor, hasMore, err := readRevokedAPIKeyList(resp, "k_1")
	if err != nil {
		t.Fatalf("readRevokedAPIKeyList: %v", err)
	}
	if !found || nextCursor != "" || hasMore {
		t.Fatalf("unexpected result: found=%v next=%q hasMore=%v", found, nextCursor, hasMore)
	}
	resp = &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`{"data":[{"key_id":"k_2","status":"revoked"}],"meta":{"has_more":true,"next_cursor":"cursor-2"}}`)),
	}
	found, nextCursor, hasMore, err = readRevokedAPIKeyList(resp, "k_1")
	if err != nil {
		t.Fatalf("readRevokedAPIKeyList: %v", err)
	}
	if found || nextCursor != "cursor-2" || !hasMore {
		t.Fatalf("unexpected result: found=%v next=%q hasMore=%v", found, nextCursor, hasMore)
	}
}

func TestMinterHelperFunctions(t *testing.T) {
	if err := validateIdempotencyKey("short"); err == nil {
		t.Fatal("validateIdempotencyKey short key unexpectedly succeeded")
	}
	if err := validateIdempotencyKey(strings.Repeat("a", 32)); err != nil {
		t.Fatalf("validateIdempotencyKey valid key err = %v", err)
	}
	if !shouldFallbackToLegacyMint(http.StatusNotFound, "") {
		t.Fatal("404 without code should fallback")
	}
	if !shouldFallbackToLegacyMint(http.StatusServiceUnavailable, errCodeBindingsDisabled) {
		t.Fatal("503 bindings_disabled should fallback")
	}
	if shouldFallbackToLegacyMint(http.StatusServiceUnavailable, "other") {
		t.Fatal("unexpected fallback for unrelated 503")
	}
	fields := errorEnvelopeFields([]byte(`{"error":{"code":"quota_exceeded"},"meta":{"request_id":"req-1"}}`))
	if fields.Code != ErrorCodeQuotaExceeded || fields.RequestID != "req-1" {
		t.Fatalf("errorEnvelopeFields = %+v", fields)
	}
	if got := errorEnvelopeCode([]byte(`"plain string"`)); got != structuredErrorEnvelopeCode {
		t.Fatalf("errorEnvelopeCode plain string = %q", got)
	}
	if !apiKeyQuotaError([]byte(`{"error":{"code":"api_key_limit"}}`)) {
		t.Fatal("apiKeyQuotaError did not recognize api_key_limit")
	}
}
