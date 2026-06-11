package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

const (
	testBindingPath            = "/v1/external-identity-bindings"
	testAPIKeysPath            = "/v1/api-keys"
	bindingUnavailableRetrySec = "60"
)

// mintWorkspaceOnlyErr is a test helper that discards the success result the
// success-path tests assert on. Test error-only branches use it so they aren't
// tripped by dogsled.
func mintWorkspaceOnlyErr(m *HTTPAPIKeyMinter) error {
	_, err := m.MintWorkspaceAPIKey(context.Background(), "tok", testTeamID)
	return err
}

func writeBindingSuccess(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"api_key": map[string]string{
			"plaintext":  testAPIKey,
			"key_id":     testKeyID,
			"key_prefix": testKeyPrefix,
		},
	})
}

func writeBindingSuccessWithoutPrefix(t *testing.T, w http.ResponseWriter) {
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

func writeLegacyMintSuccessWithoutPrefix(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"data": map[string]string{
			"api_key": testAPIKey,
			"key_id":  testKeyID,
		},
	})
}

func TestHTTPAPIKeyMinterMintWorkspaceHappyPath(t *testing.T) {
	var (
		gotMethod         string
		gotPath           string
		gotAuth           string
		gotIdempotencyKey string
		gotBody           bindingRequest
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotIdempotencyKey = r.Header.Get("Idempotency-Key")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		writeBindingSuccess(t, w)
	}))
	t.Cleanup(srv.Close)

	m := &HTTPAPIKeyMinter{BaseURL: srv.URL, HTTPClient: srv.Client()}
	minted, err := m.MintWorkspaceAPIKey(context.Background(), "tok", testTeamID)
	if err != nil {
		t.Fatalf("MintWorkspaceAPIKey: %v", err)
	}
	if minted.APIKey != testAPIKey || minted.KeyID != testKeyID || minted.KeyPrefix != testKeyPrefix {
		t.Errorf("unexpected fields: %+v", minted)
	}
	if !minted.BindingBacked {
		t.Error("binding path must mark the key binding-backed")
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method: got %q want POST", gotMethod)
	}
	if gotPath != testBindingPath {
		t.Errorf("path: got %q want %s", gotPath, testBindingPath)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("auth: got %q want Bearer tok", gotAuth)
	}
	if gotIdempotencyKey != bindingIdempotencyKey(testTeamID) || len(gotIdempotencyKey) < 32 {
		t.Errorf("Idempotency-Key = %q, want stable 32+ char key", gotIdempotencyKey)
	}
	if gotBody.Provider != "slack" || gotBody.ExternalID != testTeamID {
		t.Errorf("binding body = %+v, want slack/%s", gotBody, testTeamID)
	}
	if gotBody.DisplayName != "Slack workspace "+testTeamID {
		t.Errorf("display_name = %q", gotBody.DisplayName)
	}
}

func TestHTTPAPIKeyMinterMintWorkspaceRejectsEmptyTeamID(t *testing.T) {
	m := &HTTPAPIKeyMinter{BaseURL: "https://api.example.test"}
	if _, err := m.MintWorkspaceAPIKey(context.Background(), "tok", " \t "); err == nil {
		t.Fatal("expected error for empty teamID")
	}
}

func TestHTTPAPIKeyMinterMintWorkspaceDerivesBindingKeyPrefix(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeBindingSuccessWithoutPrefix(t, w)
	}))
	t.Cleanup(srv.Close)

	m := &HTTPAPIKeyMinter{BaseURL: srv.URL, HTTPClient: srv.Client()}
	minted, err := m.MintWorkspaceAPIKey(context.Background(), "tok", testTeamID)
	if err != nil {
		t.Fatalf("MintWorkspaceAPIKey: %v", err)
	}
	if minted.KeyPrefix != testKeyPrefix {
		t.Fatalf("derived keyPrefix = %q, want %q", minted.KeyPrefix, testKeyPrefix)
	}
	if !minted.BindingBacked {
		t.Error("binding path must mark the key binding-backed")
	}
}

func TestHTTPAPIKeyMinterMintWorkspaceRejectsShortKeyWithoutPrefix(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
	}{
		{
			name: "binding",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"api_key": map[string]string{
						"plaintext": "short",
						"key_id":    testKeyID,
					},
				})
			},
		},
		{
			name: "legacy fallback",
			handler: func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case testBindingPath:
					http.NotFound(w, r)
				case testAPIKeysPath:
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{
						"data": map[string]string{
							"api_key": "short",
							"key_id":  testKeyID,
						},
					})
				default:
					http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			t.Cleanup(srv.Close)

			m := &HTTPAPIKeyMinter{BaseURL: srv.URL, HTTPClient: srv.Client()}
			err := mintWorkspaceOnlyErr(m)
			if err == nil || !strings.Contains(err.Error(), "key_prefix") {
				t.Fatalf("expected key_prefix error, got %v", err)
			}
		})
	}
}

func TestHTTPAPIKeyMinterTolerateBaseURLTrailingSlash(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify the path doesn't get the double-slash treatment ("//v1").
		if r.URL.Path != testBindingPath {
			http.Error(w, "wrong path "+r.URL.Path, http.StatusBadRequest)
			return
		}
		writeBindingSuccess(t, w)
	}))
	t.Cleanup(srv.Close)

	m := &HTTPAPIKeyMinter{BaseURL: srv.URL + "/", HTTPClient: srv.Client()}
	if _, err := m.MintWorkspaceAPIKey(context.Background(), "tok", testTeamID); err != nil {
		t.Fatalf("MintWorkspaceAPIKey: %v", err)
	}
}

func TestHTTPAPIKeyMinterMintWorkspaceDoesNotFallbackOnStructured404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == testAPIKeysPath {
			t.Fatal("must not fall back to legacy mint on structured qurl-service 404")
		}
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"code":"not_found","detail":"not found"}}`)
	}))
	t.Cleanup(srv.Close)

	m := &HTTPAPIKeyMinter{BaseURL: srv.URL, HTTPClient: srv.Client()}
	err := mintWorkspaceOnlyErr(m)
	if err == nil {
		t.Fatal("expected error on structured 404")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("expected status code in error, got %q", err.Error())
	}
}

func TestHTTPAPIKeyMinterMintWorkspaceDoesNotFallbackOnOversizedStructured404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == testAPIKeysPath {
			t.Fatal("must not fall back to legacy mint on oversized structured qurl-service 404")
		}
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"code":"not_found","detail":"`+strings.Repeat("x", minterBodyLimit)+`"}}`)
	}))
	t.Cleanup(srv.Close)

	m := &HTTPAPIKeyMinter{BaseURL: srv.URL, HTTPClient: srv.Client()}
	err := mintWorkspaceOnlyErr(m)
	if err == nil || !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("expected oversized structured error, got %v", err)
	}
}

func TestHTTPAPIKeyMinterMintWorkspaceFallsBackWhenBindingRouteMissing(t *testing.T) {
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
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	m := &HTTPAPIKeyMinter{BaseURL: srv.URL, HTTPClient: srv.Client()}
	minted, err := m.MintWorkspaceAPIKey(context.Background(), "tok", testTeamID)
	if err != nil {
		t.Fatalf("MintWorkspaceAPIKey: %v", err)
	}
	if minted.APIKey != testAPIKey || minted.KeyID != testKeyID || minted.KeyPrefix != testKeyPrefix {
		t.Errorf("unexpected fallback fields: %+v", minted)
	}
	if minted.BindingBacked {
		t.Error("legacy fallback path must not mark the key binding-backed")
	}
	if strings.Join(paths, ",") != testBindingPath+","+testAPIKeysPath {
		t.Errorf("paths = %v", paths)
	}
	if legacyIdempotency != legacyFallbackIdempotencyKey(testTeamID) {
		t.Errorf("legacy fallback Idempotency-Key = %q", legacyIdempotency)
	}
}

func TestHTTPAPIKeyMinterMintWorkspaceFallsBackWhenRouteMissingBodyTooLarge(t *testing.T) {
	var legacyCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case testBindingPath:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, strings.Repeat("route missing\n", minterBodyLimit))
		case testAPIKeysPath:
			legacyCalled = true
			writeLegacyMintSuccess(t, w)
		default:
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	m := &HTTPAPIKeyMinter{BaseURL: srv.URL, HTTPClient: srv.Client()}
	minted, err := m.MintWorkspaceAPIKey(context.Background(), "tok", testTeamID)
	if err != nil {
		t.Fatalf("MintWorkspaceAPIKey: %v", err)
	}
	if !legacyCalled {
		t.Fatal("expected legacy fallback for oversized unstructured 404")
	}
	if minted.BindingBacked {
		t.Error("oversized route-missing fallback must not mark the key binding-backed")
	}
}

func TestHTTPAPIKeyMinterMintWorkspaceDerivesLegacyKeyPrefix(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case testBindingPath:
			http.NotFound(w, r)
		case testAPIKeysPath:
			writeLegacyMintSuccessWithoutPrefix(t, w)
		default:
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	m := &HTTPAPIKeyMinter{BaseURL: srv.URL, HTTPClient: srv.Client()}
	minted, err := m.MintWorkspaceAPIKey(context.Background(), "tok", testTeamID)
	if err != nil {
		t.Fatalf("MintWorkspaceAPIKey: %v", err)
	}
	if minted.KeyPrefix != testKeyPrefix {
		t.Fatalf("derived legacy keyPrefix = %q, want %q", minted.KeyPrefix, testKeyPrefix)
	}
	if minted.BindingBacked {
		t.Error("legacy fallback path must not mark the key binding-backed")
	}
}

func TestHTTPAPIKeyMinterMintWorkspaceFallsBackWhenBindingsDisabled(t *testing.T) {
	var legacyCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case testBindingPath:
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"code":"bindings_disabled","detail":"External identity bindings are not enabled in this environment."}}`)
		case testAPIKeysPath:
			legacyCalled = true
			writeLegacyMintSuccess(t, w)
		default:
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	m := &HTTPAPIKeyMinter{BaseURL: srv.URL, HTTPClient: srv.Client()}
	minted, err := m.MintWorkspaceAPIKey(context.Background(), "tok", testTeamID)
	if err != nil {
		t.Fatalf("MintWorkspaceAPIKey: %v", err)
	}
	if minted.BindingBacked {
		t.Error("disabled fallback path must not mark the key binding-backed")
	}
	if !legacyCalled {
		t.Fatal("expected legacy fallback when bindings are disabled")
	}
}

func TestHTTPAPIKeyMinterMintWorkspaceDoesNotFallbackOnDisabledProseWithoutCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == testAPIKeysPath {
			t.Fatal("must not fall back to legacy mint without the stable bindings_disabled code")
		}
		w.Header().Set("Retry-After", bindingUnavailableRetrySec)
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"code":"service_unavailable","detail":"External identity bindings are not enabled in this environment."}}`)
	}))
	t.Cleanup(srv.Close)

	m := &HTTPAPIKeyMinter{BaseURL: srv.URL, HTTPClient: srv.Client()}
	err := mintWorkspaceOnlyErr(m)
	if err == nil {
		t.Fatal("expected error when disabled response lacks bindings_disabled code")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("expected status code in error, got %q", err.Error())
	}
}

func TestHTTPAPIKeyMinterMintWorkspaceDoesNotFallbackOnTransient503(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == testAPIKeysPath {
			t.Fatal("must not fall back to legacy mint on transient binding 503")
		}
		w.Header().Set("Retry-After", "5")
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"code":"service_unavailable","detail":"Idempotency lookup transiently unavailable; retry with the same Idempotency-Key."}}`)
	}))
	t.Cleanup(srv.Close)

	m := &HTTPAPIKeyMinter{BaseURL: srv.URL, HTTPClient: srv.Client()}
	err := mintWorkspaceOnlyErr(m)
	if err == nil {
		t.Fatal("expected error on transient 503")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("expected status code in error, got %q", err.Error())
	}
}

func TestHTTPAPIKeyMinterValidateAPIKeyHappyPath(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotAuth   string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"plan":"free"}}`)
	}))
	t.Cleanup(srv.Close)
	m := &HTTPAPIKeyMinter{BaseURL: srv.URL + "/", HTTPClient: srv.Client()}
	if err := m.ValidateAPIKey(context.Background(), "lv_live_existing"); err != nil {
		t.Fatalf("ValidateAPIKey: %v", err)
	}
	if gotMethod != http.MethodGet || gotPath != "/v1/quota" {
		t.Fatalf("request = %s %s, want GET /v1/quota", gotMethod, gotPath)
	}
	if gotAuth != "Bearer lv_live_existing" {
		t.Errorf("auth: got %q want Bearer lv_live_existing", gotAuth)
	}
}

func TestHTTPAPIKeyMinterValidateAPIKeyInvalid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	m := &HTTPAPIKeyMinter{BaseURL: srv.URL, HTTPClient: srv.Client()}
	err := m.ValidateAPIKey(context.Background(), "lv_live_revoked")
	if !errors.Is(err, ErrStoredAPIKeyInvalid) {
		t.Fatalf("ValidateAPIKey error = %v, want ErrStoredAPIKeyInvalid", err)
	}
	if !strings.Contains(err.Error(), strconv.Itoa(http.StatusUnauthorized)) {
		t.Errorf("expected status context in error, got %q", err.Error())
	}
}

func TestHTTPAPIKeyMinterValidateAPIKeyForbiddenIsNotReplaceable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	m := &HTTPAPIKeyMinter{BaseURL: srv.URL, HTTPClient: srv.Client()}
	err := m.ValidateAPIKey(context.Background(), "lv_live_existing")
	if err == nil {
		t.Fatal("expected error on 403")
	}
	if errors.Is(err, ErrStoredAPIKeyInvalid) {
		t.Fatalf("403 must not be classified replaceable-invalid, got %v", err)
	}
	if !strings.Contains(err.Error(), strconv.Itoa(http.StatusForbidden)) {
		t.Errorf("expected status context in error, got %q", err.Error())
	}
}

func TestHTTPAPIKeyMinterValidateAPIKeyTransientFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)
	m := &HTTPAPIKeyMinter{BaseURL: srv.URL, HTTPClient: srv.Client()}
	err := m.ValidateAPIKey(context.Background(), "lv_live_existing")
	if err == nil {
		t.Fatal("expected error on 502")
	}
	if errors.Is(err, ErrStoredAPIKeyInvalid) {
		t.Fatalf("502 must stay transient, got ErrStoredAPIKeyInvalid: %v", err)
	}
}

func TestHTTPAPIKeyMinterValidateAPIKeyRejectsEmptyKey(t *testing.T) {
	m := &HTTPAPIKeyMinter{BaseURL: "http://example.invalid"}
	if err := m.ValidateAPIKey(context.Background(), " \t "); !errors.Is(err, ErrStoredAPIKeyInvalid) {
		t.Fatalf("ValidateAPIKey empty key error = %v, want ErrStoredAPIKeyInvalid", err)
	}
}

func TestHTTPAPIKeyMinterMintWorkspaceNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":"forbidden"}`)
	}))
	t.Cleanup(srv.Close)
	m := &HTTPAPIKeyMinter{BaseURL: srv.URL, HTTPClient: srv.Client()}
	err := mintWorkspaceOnlyErr(m)
	if err == nil {
		t.Fatal("expected error on 4xx")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("expected status code in error, got %q", err.Error())
	}
}

func TestHTTPAPIKeyMinterMintWorkspaceAPIKeyLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"code":"api_key_limit","title":"API Key Limit Exceeded","detail":"api key limit exceeded: plan limit reached"}}`)
	}))
	t.Cleanup(srv.Close)
	m := &HTTPAPIKeyMinter{BaseURL: srv.URL, HTTPClient: srv.Client()}
	err := mintWorkspaceOnlyErr(m)
	if !errors.Is(err, ErrAPIKeyLimitReached) {
		t.Fatalf("expected ErrAPIKeyLimitReached, got %v", err)
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("expected wrapped status code in error, got %q", err.Error())
	}
}

func TestHTTPAPIKeyMinterMintWorkspaceAlreadyBound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"error":{"code":"already_exists","title":"Already Exists","detail":"Binding already exists"}}`)
	}))
	t.Cleanup(srv.Close)
	m := &HTTPAPIKeyMinter{BaseURL: srv.URL, HTTPClient: srv.Client()}
	err := mintWorkspaceOnlyErr(m)
	if !errors.Is(err, ErrExternalIdentityAlreadyBound) {
		t.Fatalf("expected ErrExternalIdentityAlreadyBound, got %v", err)
	}
}

func TestHTTPAPIKeyMinterMintWorkspaceEnvelopeCodesRequireExpectedStatus(t *testing.T) {
	for _, tc := range []struct {
		name        string
		code        string
		mustNotWrap error
	}{
		{
			name:        "api key limit on unexpected status",
			code:        errCodeAPIKeyLimit,
			mustNotWrap: ErrAPIKeyLimitReached,
		},
		{
			name:        "already exists on unexpected status",
			code:        errCodeAlreadyExists,
			mustNotWrap: ErrExternalIdentityAlreadyBound,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, `{"error":{"code":"`+tc.code+`","title":"Upstream Error","detail":"unexpected status"}}`)
			}))
			t.Cleanup(srv.Close)

			m := &HTTPAPIKeyMinter{BaseURL: srv.URL, HTTPClient: srv.Client()}
			err := mintWorkspaceOnlyErr(m)
			if errors.Is(err, tc.mustNotWrap) {
				t.Fatalf("unexpected typed error for 500 envelope: %v", err)
			}
			if err == nil || !strings.Contains(err.Error(), "500") {
				t.Fatalf("expected generic 500 error, got %v", err)
			}
		})
	}
}

func TestHTTPAPIKeyMinterMintWorkspaceForbiddenEnvelopeStaysGeneric(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"code":"insufficient_scope","title":"Forbidden","detail":"token missing qurl:write"}}`)
	}))
	t.Cleanup(srv.Close)
	m := &HTTPAPIKeyMinter{BaseURL: srv.URL, HTTPClient: srv.Client()}
	err := mintWorkspaceOnlyErr(m)
	if errors.Is(err, ErrAPIKeyLimitReached) {
		t.Fatalf("non-limit envelope code must NOT map to ErrAPIKeyLimitReached, got %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("expected generic status error, got %q", err)
	}
}

func TestHTTPAPIKeyMinterMintWorkspaceMissingAPIKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"api_key":{"key_id":"k_1","key_prefix":"lv_x"}}`)
	}))
	t.Cleanup(srv.Close)
	m := &HTTPAPIKeyMinter{BaseURL: srv.URL, HTTPClient: srv.Client()}
	if err := mintWorkspaceOnlyErr(m); err == nil {
		t.Fatal("expected error when api_key plaintext is empty")
	}
}

func TestHTTPAPIKeyMinterMintWorkspaceMissingKeyID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"api_key":{"plaintext":"lv_xxx","key_prefix":"lv_x"}}`)
	}))
	t.Cleanup(srv.Close)
	m := &HTTPAPIKeyMinter{BaseURL: srv.URL, HTTPClient: srv.Client()}
	err := mintWorkspaceOnlyErr(m)
	if err == nil {
		t.Fatal("expected error when key_id is missing")
	}
}

func TestHTTPAPIKeyMinterRevokeHappyPath(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	m := &HTTPAPIKeyMinter{BaseURL: srv.URL, HTTPClient: srv.Client()}
	if err := m.RevokeAPIKey(context.Background(), "tok", "k_1"); err != nil {
		t.Fatalf("RevokeAPIKey: %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("method: got %q want DELETE", gotMethod)
	}
	if gotPath != "/v1/api-keys/k_1" {
		t.Errorf("path: got %q want /v1/api-keys/k_1", gotPath)
	}
}

func TestHTTPAPIKeyMinterRevokeEmptyKeyID(t *testing.T) {
	m := &HTTPAPIKeyMinter{BaseURL: "http://example.invalid"}
	err := m.RevokeAPIKey(context.Background(), "tok", "")
	if err == nil {
		t.Fatal("expected error for empty keyID")
	}
}

func TestHTTPAPIKeyMinterRevokeNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	m := &HTTPAPIKeyMinter{BaseURL: srv.URL, HTTPClient: srv.Client()}
	if err := m.RevokeAPIKey(context.Background(), "tok", "k_1"); err == nil {
		t.Fatal("expected error on 5xx")
	}
}

func TestHTTPAPIKeyMinterMintWorkspaceParseFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "<html>not json</html>")
	}))
	t.Cleanup(srv.Close)
	m := &HTTPAPIKeyMinter{BaseURL: srv.URL, HTTPClient: srv.Client()}
	err := mintWorkspaceOnlyErr(m)
	if err == nil {
		t.Fatal("expected error on non-JSON 200")
	}
	var syntaxErr *json.SyntaxError
	if !errors.As(err, &syntaxErr) {
		t.Errorf("expected json.SyntaxError in chain, got %v", err)
	}
}
