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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func textResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
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

func TestHTTPAPIKeyMinterMintWorkspaceReplacementUsesAPIKeysEndpoint(t *testing.T) {
	const oldKeyID = "key_oldoldold"
	var (
		gotMethod         string
		gotPath           string
		gotAuth           string
		gotIdempotencyKey string
		gotBody           mintRequest
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotIdempotencyKey = r.Header.Get("Idempotency-Key")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		writeLegacyMintSuccess(t, w)
	}))
	t.Cleanup(srv.Close)

	m := &HTTPAPIKeyMinter{BaseURL: srv.URL, HTTPClient: srv.Client()}
	minted, err := m.MintWorkspaceReplacementAPIKey(context.Background(), "tok", testTeamID, oldKeyID)
	if err != nil {
		t.Fatalf("MintWorkspaceReplacementAPIKey: %v", err)
	}
	if minted.APIKey != testAPIKey || minted.KeyID != testKeyID || minted.KeyPrefix != testKeyPrefix {
		t.Errorf("unexpected fields: %+v", minted)
	}
	if minted.BindingBacked {
		t.Error("replacement mint must not mark the key binding-backed")
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method: got %q want POST", gotMethod)
	}
	if gotPath != testAPIKeysPath {
		t.Errorf("path: got %q want %s", gotPath, testAPIKeysPath)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("auth: got %q want Bearer tok", gotAuth)
	}
	if gotIdempotencyKey != replacementIdempotencyKey(testTeamID, oldKeyID) || len(gotIdempotencyKey) < 32 {
		t.Errorf("Idempotency-Key = %q, want stable replacement key", gotIdempotencyKey)
	}
	if len(gotIdempotencyKey) > 256 || strings.ContainsAny(gotIdempotencyKey, " \t\r\n") {
		t.Errorf("Idempotency-Key = %q, want qurl-service header-safe 32-256 char value", gotIdempotencyKey)
	}
	if gotBody.Name != "Slack workspace "+testTeamID {
		t.Errorf("name = %q", gotBody.Name)
	}
	if strings.Join(gotBody.Scopes, ",") != strings.Join(apiKeyScopes(), ",") {
		t.Errorf("scopes = %#v want %#v", gotBody.Scopes, apiKeyScopes())
	}
}

func TestHTTPAPIKeyMinterMintWorkspaceReplacementRejectsUnsafeIdempotencyKey(t *testing.T) {
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		writeLegacyMintSuccess(t, w)
	}))
	t.Cleanup(srv.Close)

	m := &HTTPAPIKeyMinter{BaseURL: srv.URL, HTTPClient: srv.Client()}
	_, err := m.MintWorkspaceReplacementAPIKey(context.Background(), "tok", testTeamID, "key_bad\nid")
	if err == nil || !strings.Contains(err.Error(), "idempotency key contains non-header-safe byte") {
		t.Fatalf("MintWorkspaceReplacementAPIKey err = %v, want unsafe idempotency key error", err)
	}
	if requests != 0 {
		t.Fatalf("requests = %d, want 0", requests)
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

func TestHTTPAPIKeyMinterMintWorkspaceFallsBackOnJSON404WithoutQURLErrorCode(t *testing.T) {
	var legacyCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case testBindingPath:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"not found"}`)
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
		t.Fatal("expected legacy fallback for JSON 404 without qURL error code")
	}
	if minted.BindingBacked {
		t.Error("JSON route-missing fallback must not mark the key binding-backed")
	}
}

func TestHTTPAPIKeyMinterMintWorkspaceDoesNotFallbackOnJSONString404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == testAPIKeysPath {
			t.Fatal("must not fall back to legacy mint on JSON string qurl-service 404")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `"not found"`)
	}))
	t.Cleanup(srv.Close)

	m := &HTTPAPIKeyMinter{BaseURL: srv.URL, HTTPClient: srv.Client()}
	err := mintWorkspaceOnlyErr(m)
	if err == nil {
		t.Fatal("expected error on JSON string 404")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("expected status code in error, got %q", err.Error())
	}
}

func TestHTTPAPIKeyMinterMintWorkspaceDoesNotFallbackOnStructured404WithoutCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == testAPIKeysPath {
			t.Fatal("must not fall back to legacy mint on object-shaped qurl-service 404 without code")
		}
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"detail":"not found"}}`)
	}))
	t.Cleanup(srv.Close)

	m := &HTTPAPIKeyMinter{BaseURL: srv.URL, HTTPClient: srv.Client()}
	err := mintWorkspaceOnlyErr(m)
	if err == nil {
		t.Fatal("expected error on structured 404 without code")
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

func TestHTTPAPIKeyMinterMintWorkspaceDoesNotFallbackOnOversizedDetailFirstStructured404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == testAPIKeysPath {
			t.Fatal("must not fall back to legacy mint when structured qurl-service 404 truncates before code")
		}
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"detail":"`+strings.Repeat("x", minterBodyLimit)+`","code":"not_found"}}`)
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
	if legacyIdempotency != "" {
		t.Errorf("legacy fallback Idempotency-Key = %q, want empty so revoked persist-failure retries mint fresh keys", legacyIdempotency)
	}
}

func TestHTTPAPIKeyMinterMintWorkspaceFallbackUsesCallerContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var paths []string
	m := &HTTPAPIKeyMinter{
		BaseURL: "https://qurl.example",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			paths = append(paths, req.URL.Path)
			switch req.URL.Path {
			case testBindingPath:
				cancel()
				return textResponse(req, http.StatusNotFound, "404 page not found"), nil
			case testAPIKeysPath:
				if !errors.Is(req.Context().Err(), context.Canceled) {
					t.Fatalf("legacy fallback context error = %v, want context.Canceled", req.Context().Err())
				}
				return nil, req.Context().Err()
			default:
				t.Fatalf("unexpected path %s", req.URL.Path)
			}
			return textResponse(req, http.StatusInternalServerError, ""), nil
		})},
	}

	_, err := m.MintWorkspaceAPIKey(ctx, "tok", testTeamID)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("MintWorkspaceAPIKey error = %v, want context.Canceled", err)
	}
	wantPaths := testBindingPath + "," + testAPIKeysPath
	if strings.Join(paths, ",") != wantPaths {
		t.Fatalf("paths = %v, want %s", paths, wantPaths)
	}
}

func TestHTTPAPIKeyMinterMintWorkspaceDoesNotFallbackWhenRouteMissingBodyTooLarge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case testBindingPath:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, strings.Repeat("route missing\n", minterBodyLimit))
		case testAPIKeysPath:
			t.Fatal("must not fall back to legacy mint on oversized route-missing body")
		default:
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	m := &HTTPAPIKeyMinter{BaseURL: srv.URL, HTTPClient: srv.Client()}
	err := mintWorkspaceOnlyErr(m)
	if err == nil || !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("expected oversized route-missing error, got %v", err)
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
	var authErr *DependencyAuthFailureError
	if !errors.As(err, &authErr) {
		t.Fatalf("403 mint error should be typed as DependencyAuthFailureError, got %T %[1]v", err)
	}
	if authErr.Method != http.MethodPost || authErr.Path != testBindingPath || authErr.StatusCode != http.StatusForbidden {
		t.Fatalf("auth error = %+v, want POST /v1/external-identity-bindings 403", authErr)
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
	var authErr *DependencyAuthFailureError
	if errors.As(err, &authErr) {
		t.Fatalf("api_key_limit must not be classified as dependency auth failure: %+v", authErr)
	}
}

func TestHTTPAPIKeyMinterMintWorkspaceQuotaExceeded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"code":"quota_exceeded","title":"Quota Exceeded","detail":"quota exceeded"}}`)
	}))
	t.Cleanup(srv.Close)
	m := &HTTPAPIKeyMinter{BaseURL: srv.URL, HTTPClient: srv.Client()}
	err := mintWorkspaceOnlyErr(m)
	if !errors.Is(err, ErrAPIKeyLimitReached) {
		t.Fatalf("expected ErrAPIKeyLimitReached, got %v", err)
	}
	var authErr *DependencyAuthFailureError
	if errors.As(err, &authErr) {
		t.Fatalf("quota_exceeded must not be classified as dependency auth failure: %+v", authErr)
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
			name:        "quota exceeded on unexpected status",
			code:        errCodeQuotaExceeded,
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
		_, _ = io.WriteString(w, `{"error":{"code":"insufficient_scope","title":"Forbidden","detail":"token missing qurl:write"},"meta":{"request_id":"req_scope"}}`)
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
	var authErr *DependencyAuthFailureError
	if !errors.As(err, &authErr) {
		t.Fatalf("non-limit 403 must be classified as dependency auth failure, got %T %[1]v", err)
	}
	if authErr.Code != "insufficient_scope" {
		t.Fatalf("auth error code = %q, want insufficient_scope", authErr.Code)
	}
	if authErr.RequestID != "req_scope" {
		t.Fatalf("auth error request id = %q, want req_scope", authErr.RequestID)
	}
}

func TestHTTPAPIKeyMinterReplacementMintAuthFailureTyped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"code":"`+testInvalidToken+`","title":"Unauthorized"},"meta":{"request_id":"req_replace"}}`)
	}))
	t.Cleanup(srv.Close)

	m := &HTTPAPIKeyMinter{BaseURL: srv.URL, HTTPClient: srv.Client()}
	_, err := m.MintWorkspaceReplacementAPIKey(context.Background(), "tok", testTeamID, "k_old")

	var authErr *DependencyAuthFailureError
	if !errors.As(err, &authErr) {
		t.Fatalf("replacement mint 401 should be typed as DependencyAuthFailureError, got %T %[1]v", err)
	}
	if authErr.Method != http.MethodPost || authErr.Path != testAPIKeysPath || authErr.StatusCode != http.StatusUnauthorized || authErr.Code != testInvalidToken || authErr.RequestID != "req_replace" {
		t.Fatalf("auth error = %+v, want POST /v1/api-keys 401 invalid_token req_replace", authErr)
	}
}

func TestHTTPAPIKeyMinterReplacementMintQuotaExceeded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"code":"quota_exceeded","title":"Quota Exceeded","detail":"quota exceeded"}}`)
	}))
	t.Cleanup(srv.Close)

	m := &HTTPAPIKeyMinter{BaseURL: srv.URL, HTTPClient: srv.Client()}
	_, err := m.MintWorkspaceReplacementAPIKey(context.Background(), "access-token", testTeamID, "k_old")
	if !errors.Is(err, ErrAPIKeyLimitReached) {
		t.Fatalf("expected ErrAPIKeyLimitReached, got %v", err)
	}
	var authErr *DependencyAuthFailureError
	if errors.As(err, &authErr) {
		t.Fatalf("quota_exceeded replacement mint must not be classified as dependency auth failure: %+v", authErr)
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

func TestHTTPAPIKeyMinterRevokeNotFoundClassified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	m := &HTTPAPIKeyMinter{BaseURL: srv.URL, HTTPClient: srv.Client()}
	err := m.RevokeAPIKey(context.Background(), "tok", "k_1")
	if !errors.Is(err, ErrAPIKeyNotFound) {
		t.Fatalf("RevokeAPIKey 404 error = %v, want ErrAPIKeyNotFound", err)
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

func TestHTTPAPIKeyMinterRevokeAuthFailureTyped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"code":"insufficient_scope","title":"Forbidden"},"meta":{"request_id":"req_revoke"}}`)
	}))
	t.Cleanup(srv.Close)

	m := &HTTPAPIKeyMinter{BaseURL: srv.URL, HTTPClient: srv.Client()}
	err := m.RevokeAPIKey(context.Background(), "tok", "k_1")

	var authErr *DependencyAuthFailureError
	if !errors.As(err, &authErr) {
		t.Fatalf("revoke 403 should be typed as DependencyAuthFailureError, got %T %[1]v", err)
	}
	if authErr.Method != http.MethodDelete || authErr.Path != "/v1/api-keys/:id" || authErr.StatusCode != http.StatusForbidden {
		t.Fatalf("auth error = %+v, want DELETE /v1/api-keys/:id 403", authErr)
	}
	if authErr.Code != "insufficient_scope" {
		t.Fatalf("auth error code = %q, want insufficient_scope", authErr.Code)
	}
	if authErr.RequestID != "req_revoke" {
		t.Fatalf("auth error request id = %q, want req_revoke", authErr.RequestID)
	}
}

func TestHTTPAPIKeyMinterAPIKeyRevokedFindsRevokedKey(t *testing.T) {
	var gotQueries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQueries = append(gotQueries, r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("cursor") {
		case "":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]string{{"key_id": "k_other", "status": "revoked"}},
				"meta": map[string]any{"has_more": true, "next_cursor": "page-2"},
			})
		case "page-2":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]string{{"key_id": "k_target", "status": "Revoked"}},
				"meta": map[string]any{"has_more": false},
			})
		default:
			t.Fatalf("unexpected cursor %q", r.URL.Query().Get("cursor"))
		}
	}))
	t.Cleanup(srv.Close)
	m := &HTTPAPIKeyMinter{BaseURL: srv.URL, HTTPClient: srv.Client()}
	got, err := m.APIKeyRevoked(context.Background(), "tok", "k_target")
	if err != nil {
		t.Fatalf("APIKeyRevoked: %v", err)
	}
	if !got {
		t.Fatal("APIKeyRevoked = false, want true")
	}
	if len(gotQueries) != 2 || !strings.Contains(gotQueries[0], "status=revoked") || !strings.Contains(gotQueries[1], "cursor=page-2") {
		t.Fatalf("queries = %#v, want revoked status and page-2 cursor", gotQueries)
	}
}

func TestHTTPAPIKeyMinterAPIKeyRevokedMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{{"key_id": "k_other", "status": "revoked"}},
			"meta": map[string]any{"has_more": false},
		})
	}))
	t.Cleanup(srv.Close)
	m := &HTTPAPIKeyMinter{BaseURL: srv.URL, HTTPClient: srv.Client()}
	got, err := m.APIKeyRevoked(context.Background(), "tok", "k_target")
	if err != nil {
		t.Fatalf("APIKeyRevoked: %v", err)
	}
	if got {
		t.Fatal("APIKeyRevoked = true, want false")
	}
}

func TestHTTPAPIKeyMinterAPIKeyRevokedCapsPagination(t *testing.T) {
	logs := captureDefaultSlogJSON(t)
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		nextCursor := "page-" + strconv.Itoa(requests)
		if r.URL.Query().Get("cursor") == nextCursor {
			t.Fatalf("server test setup produced non-advancing cursor %q", nextCursor)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{{"key_id": "k_other", "status": "revoked"}},
			"meta": map[string]any{"has_more": true, "next_cursor": nextCursor},
		})
	}))
	t.Cleanup(srv.Close)
	m := &HTTPAPIKeyMinter{BaseURL: srv.URL, HTTPClient: srv.Client()}
	got, err := m.APIKeyRevoked(context.Background(), "tok", "k_target")
	if err == nil || !strings.Contains(err.Error(), "pagination exceeded") {
		t.Fatalf("APIKeyRevoked err = %v, want pagination exceeded", err)
	}
	if got {
		t.Fatal("APIKeyRevoked = true, want false")
	}
	if requests != apiKeyRevokedMaxPages {
		t.Fatalf("requests = %d, want %d", requests, apiKeyRevokedMaxPages)
	}
	records := logs()
	if len(records) != 1 {
		t.Fatalf("log records = %d, want 1: %#v", len(records), records)
	}
	rec := records[0]
	if rec["msg"] != "oauth/minter revoked API key scan page cap exceeded" {
		t.Fatalf("warning msg = %v", rec["msg"])
	}
	if rec["key_id"] != "k_target" || rec["max_pages"] != float64(apiKeyRevokedMaxPages) {
		t.Fatalf("page-cap warning missing key_id/max_pages attrs: %#v", rec)
	}
}

func TestHTTPAPIKeyMinterAPIKeyRevokedRejectsNonAdvancingCursor(t *testing.T) {
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{{"key_id": "k_other", "status": "revoked"}},
			"meta": map[string]any{"has_more": true, "next_cursor": "same-page"},
		})
	}))
	t.Cleanup(srv.Close)

	m := &HTTPAPIKeyMinter{BaseURL: srv.URL, HTTPClient: srv.Client()}
	got, err := m.APIKeyRevoked(context.Background(), "tok", "k_target")
	if err == nil || !strings.Contains(err.Error(), "pagination did not advance") {
		t.Fatalf("APIKeyRevoked err = %v, want non-advancing pagination error", err)
	}
	if got {
		t.Fatal("APIKeyRevoked = true, want false")
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
}

func TestHTTPAPIKeyMinterAPIKeyRevokedReportsStatusBeforeLargeErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, strings.Repeat("x", apiKeyListBodyLimit+1))
	}))
	t.Cleanup(srv.Close)

	m := &HTTPAPIKeyMinter{BaseURL: srv.URL, HTTPClient: srv.Client()}
	got, err := m.APIKeyRevoked(context.Background(), "tok", "k_target")
	if err == nil || !strings.Contains(err.Error(), "GET /v1/api-keys returned 500") {
		t.Fatalf("APIKeyRevoked err = %v, want status error", err)
	}
	if got {
		t.Fatal("APIKeyRevoked = true, want false")
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
