package internal

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

const testCustomerAPIKey = "lv_live_test_customer_key"

// Wire-shape strings repeated across resource and setalias test
// files. Lifted to package-level test-only constants so goconst
// doesn't bury per-file findings under shared duplication, and so a
// future rename only edits one site.
const (
	pathByAlias        = "/v1/resources/by-alias/"
	pathResources      = "/v1/resources"
	pathSlackCommands  = "/slack/commands"
	pathSlackInteract  = "/slack/interactions"
	testAliasProdDB    = "prod-db"
	jsonAliasNotFound  = `{"error":{"code":"alias_not_found","status":404}}`
	jsonContentType    = "application/json"
	viewSubmissionType = "view_submission"
)

// resourceFixture spins a fake `/v1/resources*` endpoint for a single
// request. Mirrors adminFixture's shape so the two test files read
// uniformly.
type resourceFixture struct {
	srv         *httptest.Server
	gotMethod   string
	gotPath     string
	gotAuth     string
	gotUA       string
	gotContent  string
	gotBody     []byte
	respondCode int
	respondBody string
}

func newResourceFixture(t *testing.T, status int, body string) *resourceFixture {
	t.Helper()
	fx := &resourceFixture{respondCode: status, respondBody: body}
	fx.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fx.gotMethod = r.Method
		fx.gotPath = r.URL.RequestURI()
		fx.gotAuth = r.Header.Get("Authorization")
		fx.gotUA = r.Header.Get("User-Agent")
		fx.gotContent = r.Header.Get("Content-Type")
		fx.gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", jsonContentType)
		w.WriteHeader(fx.respondCode)
		_, _ = w.Write([]byte(fx.respondBody))
	}))
	t.Cleanup(fx.srv.Close)
	return fx
}

func (fx *resourceFixture) client() *ResourceClient {
	return NewResourceClient(fx.srv.URL, testCustomerAPIKey)
}

// TestResourceClient_AuthHeader fences the customer-API auth shape.
// Resource calls go to `/v1/resources*`, NOT the internal-admin
// surface — so they carry the workspace's customer API key, not the
// LayerV-internal HMAC.
func TestResourceClient_AuthHeader(t *testing.T) {
	t.Parallel()
	fx := newResourceFixture(t, http.StatusOK, `{"data":{"resource_id":"r_abc"}}`)
	rc := fx.client()
	if _, err := rc.GetResourceByAlias(context.Background(), testAliasProdDB); err != nil {
		t.Fatalf("GetResourceByAlias: %v", err)
	}
	if fx.gotAuth != "Bearer "+testCustomerAPIKey {
		t.Errorf("auth = %q, want Bearer %s", fx.gotAuth, testCustomerAPIKey)
	}
	if !strings.HasPrefix(fx.gotUA, "qurl-slack-resource/") {
		t.Errorf("user-agent = %q, want qurl-slack-resource/* prefix", fx.gotUA)
	}
}

// TestResourceClient_GetByAlias_HappyPath fences the GET shape and
// the URL-encoded alias path. The alias is already validated upstream
// (parser strips `$` sigil), so the path segment is the bare alias.
func TestResourceClient_GetByAlias_HappyPath(t *testing.T) {
	t.Parallel()
	fx := newResourceFixture(t, http.StatusOK, `{"data":{"resource_id":"r_abc","alias":"prod-db","target_url":"https://int.example"}}`)
	rc := fx.client()
	got, err := rc.GetResourceByAlias(context.Background(), testAliasProdDB)
	if err != nil {
		t.Fatalf("GetResourceByAlias: %v", err)
	}
	if got.ResourceID != "r_abc" || got.Alias != testAliasProdDB {
		t.Errorf("got = %+v, want resource_id=r_abc alias=prod-db", got)
	}
	if fx.gotMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", fx.gotMethod)
	}
	if !strings.Contains(fx.gotPath, "/v1/resources/by-alias/prod-db") {
		t.Errorf("path = %q, want /v1/resources/by-alias/prod-db", fx.gotPath)
	}
}

// TestResourceClient_GetByAlias_404 fences the not-found path. Server
// returns the qurl-service envelope shape with a 404 + alias_not_found
// code; the client must surface a [*ResourceError] that
// `isResourceNotFound` recognizes.
func TestResourceClient_GetByAlias_404(t *testing.T) {
	t.Parallel()
	fx := newResourceFixture(t, http.StatusNotFound, `{"error":{"title":"Not Found","detail":"alias not found","code":"alias_not_found","status":404}}`)
	rc := fx.client()
	_, err := rc.GetResourceByAlias(context.Background(), "ghost")
	if err == nil {
		t.Fatal("error = nil, want non-nil")
	}
	if !isResourceNotFound(err) {
		t.Errorf("isResourceNotFound = false, want true (err=%v)", err)
	}
}

// TestResourceClient_CreateResource fences the POST shape including
// `alias` carried in the create body.
func TestResourceClient_CreateResource(t *testing.T) {
	t.Parallel()
	fx := newResourceFixture(t, http.StatusCreated, `{"data":{"resource_id":"r_xyz","alias":"prod-db","target_url":"https://int.example"}}`)
	rc := fx.client()
	got, err := rc.CreateResource(context.Background(), CreateResourceInput{
		Type:      "url",
		TargetURL: "https://int.example",
		Alias:     testAliasProdDB,
	})
	if err != nil {
		t.Fatalf("CreateResource: %v", err)
	}
	if got.ResourceID != "r_xyz" {
		t.Errorf("resource_id = %q, want r_xyz", got.ResourceID)
	}
	if fx.gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", fx.gotMethod)
	}
	if fx.gotContent != jsonContentType {
		t.Errorf("content-type = %q, want application/json", fx.gotContent)
	}
	var body map[string]any
	if err := json.Unmarshal(fx.gotBody, &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body["alias"] != testAliasProdDB {
		t.Errorf("body.alias = %v, want prod-db", body["alias"])
	}
	if body["target_url"] != "https://int.example" {
		t.Errorf("body.target_url = %v, want https://int.example", body["target_url"])
	}
}

// TestResourceClient_UpdateResource_SetAlias fences the PATCH shape
// for the rebind path: a non-empty `alias` field, no `clear_alias`.
func TestResourceClient_UpdateResource_SetAlias(t *testing.T) {
	t.Parallel()
	fx := newResourceFixture(t, http.StatusOK, `{"data":{"resource_id":"r_abc","alias":"prod-db"}}`)
	rc := fx.client()
	got, err := rc.UpdateResource(context.Background(), "r_abc", UpdateResourceInput{Alias: testAliasProdDB})
	if err != nil {
		t.Fatalf("UpdateResource: %v", err)
	}
	if got.Alias != testAliasProdDB {
		t.Errorf("alias = %q, want prod-db", got.Alias)
	}
	if fx.gotMethod != http.MethodPatch {
		t.Errorf("method = %q, want PATCH", fx.gotMethod)
	}
	if !strings.Contains(fx.gotPath, "/v1/resources/r_abc") {
		t.Errorf("path = %q, want /v1/resources/r_abc", fx.gotPath)
	}
	var body map[string]any
	if err := json.Unmarshal(fx.gotBody, &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body["alias"] != testAliasProdDB {
		t.Errorf("body.alias = %v, want prod-db", body["alias"])
	}
	if _, hasClear := body["clear_alias"]; hasClear {
		t.Errorf("body has clear_alias when only Alias was set: %v", body)
	}
}

// TestResourceClient_UpdateResource_ClearAlias fences the unsetalias
// path: explicit `clear_alias: true`. Server differentiates this
// from "alias omitted" (which is a no-op) — both client and server
// must agree on the flag's meaning.
func TestResourceClient_UpdateResource_ClearAlias(t *testing.T) {
	t.Parallel()
	fx := newResourceFixture(t, http.StatusOK, `{"data":{"resource_id":"r_abc"}}`)
	rc := fx.client()
	if _, err := rc.UpdateResource(context.Background(), "r_abc", UpdateResourceInput{ClearAlias: true}); err != nil {
		t.Fatalf("UpdateResource: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(fx.gotBody, &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body["clear_alias"] != true {
		t.Errorf("body.clear_alias = %v, want true", body["clear_alias"])
	}
}

// TestResourceClient_AliasInUse_409 fences the 409 conflict shape so
// the handler's friendly-error mapping has a stable wire-shape input.
func TestResourceClient_AliasInUse_409(t *testing.T) {
	t.Parallel()
	fx := newResourceFixture(t, http.StatusConflict, `{"error":{"title":"Conflict","detail":"alias in use","code":"alias_in_use","status":409}}`)
	rc := fx.client()
	_, err := rc.UpdateResource(context.Background(), "r_abc", UpdateResourceInput{Alias: testAliasProdDB})
	if err == nil {
		t.Fatal("error = nil, want non-nil")
	}
	var rerr *ResourceError
	if !errors.As(err, &rerr) {
		t.Fatalf("err %T does not unwrap to *ResourceError", err)
	}
	if rerr.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", rerr.StatusCode)
	}
	if rerr.Code != errCodeAliasInUse {
		t.Errorf("code = %q, want %s", rerr.Code, errCodeAliasInUse)
	}
}

// TestResourceClient_NoBaseURL guards the misconfiguration path. Same
// shape as the admin client's NoBaseURL fence.
func TestResourceClient_NoBaseURL(t *testing.T) {
	t.Parallel()
	rc := NewResourceClient("", testCustomerAPIKey)
	_, err := rc.GetResourceByAlias(context.Background(), "x")
	if err == nil {
		t.Error("error = nil, want non-nil for empty base URL")
	}
}

// TestResourceClient_NoAPIKey guards the second misconfig: missing
// API key.
func TestResourceClient_NoAPIKey(t *testing.T) {
	t.Parallel()
	rc := NewResourceClient("http://example", "")
	_, err := rc.GetResourceByAlias(context.Background(), "x")
	if err == nil {
		t.Error("error = nil, want non-nil for empty API key")
	}
}

// TestResourceClient_WithHTTPClientOption fences the test injection
// hook.
func TestResourceClient_WithHTTPClientOption(t *testing.T) {
	t.Parallel()
	called := false
	rt := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		called = true
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader(`{"data":{"resource_id":"r_x"}}`)),
			Header:     make(http.Header),
		}, nil
	})
	rc := NewResourceClient("http://example", testCustomerAPIKey, WithResourceHTTPClient(&http.Client{Transport: rt}))
	if _, err := rc.GetResourceByAlias(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Error("custom round-tripper not invoked")
	}
}
