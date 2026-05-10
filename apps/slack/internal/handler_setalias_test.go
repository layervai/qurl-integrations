package internal

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"

	"github.com/layervai/qurl-integrations/shared/auth"
	"github.com/layervai/qurl-integrations/shared/client"
)

// stubAdminBackend simulates the qurl-service `/internal/v1/admin/check`
// endpoint for the setalias/unsetalias admin gate. Adjust isAdmin per
// test row.
func stubAdminBackend(t *testing.T, isAdmin bool) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/internal/v1/admin/check") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", jsonContentType)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"is_admin": isAdmin,
				"owner_id": "u_owner_1",
			},
		})
	}))
	t.Cleanup(s.Close)
	return s
}

// resourceMock is a richer fixture than resourceFixture: it routes
// per-method+path so a single httptest server can satisfy multiple
// resource calls (e.g. GetByAlias then UpdateResource).
type resourceMock struct {
	srv      *httptest.Server
	requests []resourceMockRequest
}

type resourceMockRequest struct {
	Method string
	Path   string
	Body   string
}

// resourceMockResponse describes one of the canned responses the mock
// returns, keyed by URL substring.
type resourceMockResponse struct {
	matchPath string // substring match on URL.RequestURI
	matchVerb string // optional exact match on Method (empty = any)
	status    int
	body      string
}

func newResourceMock(t *testing.T, responses []resourceMockResponse) *resourceMock {
	t.Helper()
	rm := &resourceMock{}
	rm.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		rm.requests = append(rm.requests, resourceMockRequest{
			Method: r.Method,
			Path:   r.URL.RequestURI(),
			Body:   string(buf),
		})
		for _, resp := range responses {
			if resp.matchVerb != "" && resp.matchVerb != r.Method {
				continue
			}
			if !strings.Contains(r.URL.RequestURI(), resp.matchPath) {
				continue
			}
			w.Header().Set("Content-Type", jsonContentType)
			w.WriteHeader(resp.status)
			_, _ = w.Write([]byte(resp.body))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(rm.srv.Close)
	return rm
}

// stubResponseURL returns an httptest server that captures whatever
// the handler POSTs to `response_url`. The atomic.Bool is the
// did-it-fire signal; the captured body is exposed via the .Body
// field for assertions.
type responseURLCapture struct {
	srv  *httptest.Server
	hit  atomic.Bool
	body atomic.Pointer[string]
}

func newResponseURLCapture(t *testing.T) *responseURLCapture {
	t.Helper()
	c := &responseURLCapture{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.hit.Store(true)
		buf, _ := io.ReadAll(r.Body)
		s := string(buf)
		c.body.Store(&s)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

// newSetAliasTestHandler wires a handler with stubbed deps.
func newSetAliasTestHandler(
	t *testing.T,
	adminURL, resourceURL string,
	openView func(ctx context.Context, triggerID string, viewJSON []byte) error,
	postResponseURL func(ctx context.Context, responseURL string, payload []byte) error,
) *Handler {
	t.Helper()
	t.Setenv("QURL_API_KEY", testCustomerAPIKey)
	h := NewHandler(Config{
		QURLEndpoint:         resourceURL,
		AuthProvider:         &auth.EnvProvider{EnvVar: "QURL_API_KEY"},
		SlackSigningSecret:   testSigningSecret,
		InternalServiceToken: testInternalToken,
		NewClient: func(apiKey string) *client.Client {
			return client.New(resourceURL, apiKey)
		},
	})
	h.now = func() time.Time { return fixedNow }
	h.SetDeps(setAliasDeps{
		NewResourceClient: func(apiKey string) *ResourceClient {
			return NewResourceClient(resourceURL, apiKey)
		},
		NewAdminClient: func() *AdminClient {
			return NewAdminClient(adminURL, testInternalToken)
		},
		OpenView:        openView,
		PostResponseURL: postResponseURL,
	})
	return h
}

// setAliasFormBody builds the form-encoded slash-command body for
// setalias / unsetalias tests.
func setAliasFormBody(text, teamID, channelID, userID, triggerID, responseURL string) string {
	v := url.Values{}
	v.Set("command", "/qurl")
	v.Set("text", text)
	v.Set("team_id", teamID)
	v.Set("channel_id", channelID)
	v.Set("user_id", userID)
	v.Set("trigger_id", triggerID)
	v.Set("response_url", responseURL)
	return v.Encode()
}

// TestSetAlias_HappyPath_NewURL fences the clean-set path: alias is
// not yet bound, target is a URL → CreateResource with `alias` set.
func TestSetAlias_HappyPath_NewURL(t *testing.T) {
	admin := stubAdminBackend(t, true)
	resmock := newResourceMock(t, []resourceMockResponse{
		// GET by-alias → 404 (alias unbound)
		{matchVerb: http.MethodGet, matchPath: pathByAlias, status: http.StatusNotFound, body: `{"error":{"code":"alias_not_found","status":404,"title":"Not Found"}}`},
		// POST /v1/resources → created
		{matchVerb: http.MethodPost, matchPath: pathResources, status: http.StatusCreated, body: `{"data":{"resource_id":"r_new","alias":"prod-db","target_url":"https://int.example"}}`},
	})
	rurl := newResponseURLCapture(t)

	h := newSetAliasTestHandler(t, admin.URL, resmock.srv.URL, nil, func(ctx context.Context, _ string, payload []byte) error {
		// Simulate a real response_url POST so we exercise the path
		// even though we don't assert on its body here.
		return nil
	})
	body := setAliasFormBody("setalias $prod-db https://int.example", "T1", "C1", "U1", "tr1", rurl.srv.URL)
	resp, err := h.Handle(context.Background(), &events.APIGatewayProxyRequest{
		Path: pathSlackCommands, HTTPMethod: methodPost, Body: body, Headers: signSlackBody(t, body),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, resp.Body)
	}
	// Last request must be the create with alias=prod-db.
	if n := len(resmock.requests); n < 2 {
		t.Fatalf("expected >=2 backend calls, got %d", n)
	}
	last := resmock.requests[len(resmock.requests)-1]
	if last.Method != http.MethodPost || !strings.HasSuffix(last.Path, pathResources) {
		t.Errorf("last request = %s %s, want POST /v1/resources", last.Method, last.Path)
	}
	if !strings.Contains(last.Body, `"alias":"prod-db"`) || !strings.Contains(last.Body, `"target_url":"https://int.example"`) {
		t.Errorf("create body missing alias/target: %s", last.Body)
	}
}

// TestSetAlias_RebindOpensModal fences the rebind branch: alias is
// already bound to a different target → opens views.open with the
// rebind confirmation modal. Slack-side ack is empty 200.
func TestSetAlias_RebindOpensModal(t *testing.T) {
	admin := stubAdminBackend(t, true)
	resmock := newResourceMock(t, []resourceMockResponse{
		// GET by-alias → 200, currently bound to https://OLD.example
		{matchVerb: http.MethodGet, matchPath: pathByAlias, status: http.StatusOK, body: `{"data":{"resource_id":"r_old","alias":"prod-db","target_url":"https://old.example"}}`},
	})
	var openTriggerID string
	var openViewJSON []byte
	openView := func(_ context.Context, triggerID string, viewJSON []byte) error {
		openTriggerID = triggerID
		openViewJSON = viewJSON
		return nil
	}
	h := newSetAliasTestHandler(t, admin.URL, resmock.srv.URL, openView, nil)

	body := setAliasFormBody("setalias $prod-db https://new.example", "T1", "C1", "U1", "trig-rebind", "")
	resp, err := h.Handle(context.Background(), &events.APIGatewayProxyRequest{
		Path: pathSlackCommands, HTTPMethod: methodPost, Body: body, Headers: signSlackBody(t, body),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if openTriggerID != "trig-rebind" {
		t.Errorf("trigger_id passed to OpenView = %q, want trig-rebind", openTriggerID)
	}
	if len(openViewJSON) == 0 {
		t.Fatal("OpenView received empty view JSON")
	}
	// View JSON must be a modal with the rebind callback ID and
	// must thread the new target through private_metadata.
	var view map[string]any
	if err := json.Unmarshal(openViewJSON, &view); err != nil {
		t.Fatalf("view JSON: %v", err)
	}
	if view["callback_id"] != callbackIDSetAliasRebind {
		t.Errorf("callback_id = %v, want %s", view["callback_id"], callbackIDSetAliasRebind)
	}
	pm, _ := view["private_metadata"].(string)
	var got rebindPrivateMetadata
	if err := json.Unmarshal([]byte(pm), &got); err != nil {
		t.Fatalf("private_metadata is not valid JSON: %v\nraw=%q", err, pm)
	}
	if got.Alias != testAliasProdDB {
		t.Errorf("private_metadata.alias = %q, want prod-db", got.Alias)
	}
	if got.Target != "https://new.example" {
		t.Errorf("private_metadata.target = %q, want https://new.example", got.Target)
	}
	if got.ResourceID != "r_old" {
		t.Errorf("private_metadata.rid = %q, want r_old", got.ResourceID)
	}
}

// TestUnsetAlias_HappyPath fences the unsetalias success path.
func TestUnsetAlias_HappyPath(t *testing.T) {
	admin := stubAdminBackend(t, true)
	resmock := newResourceMock(t, []resourceMockResponse{
		{matchVerb: http.MethodGet, matchPath: pathByAlias, status: http.StatusOK, body: `{"data":{"resource_id":"r_clear","alias":"prod-db","target_url":"https://int.example"}}`},
		{matchVerb: http.MethodPatch, matchPath: "/v1/resources/r_clear", status: http.StatusOK, body: `{"data":{"resource_id":"r_clear"}}`},
	})
	h := newSetAliasTestHandler(t, admin.URL, resmock.srv.URL, nil, nil)

	body := setAliasFormBody("unsetalias $prod-db", "T1", "C1", "U1", "tr2", "")
	resp, err := h.Handle(context.Background(), &events.APIGatewayProxyRequest{
		Path: pathSlackCommands, HTTPMethod: methodPost, Body: body, Headers: signSlackBody(t, body),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, resp.Body)
	}
	// PATCH body must carry clear_alias:true.
	var sawClear bool
	for _, r := range resmock.requests {
		if r.Method == http.MethodPatch && strings.Contains(r.Body, `"clear_alias":true`) {
			sawClear = true
			break
		}
	}
	if !sawClear {
		t.Errorf("no PATCH with clear_alias=true; requests=%+v", resmock.requests)
	}
}

// TestSetAlias_NonAdmin_RejectsEphemeral fences the admin gate. A
// non-admin user must get a friendly ephemeral error and the
// resource API must not be called.
func TestSetAlias_NonAdmin_RejectsEphemeral(t *testing.T) {
	admin := stubAdminBackend(t, false)
	resmock := newResourceMock(t, nil)
	h := newSetAliasTestHandler(t, admin.URL, resmock.srv.URL, nil, nil)

	body := setAliasFormBody("setalias $prod-db https://x.example", "T1", "C1", "U1", "tr3", "")
	resp, err := h.Handle(context.Background(), &events.APIGatewayProxyRequest{
		Path: pathSlackCommands, HTTPMethod: methodPost, Body: body, Headers: signSlackBody(t, body),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (friendly error in body)", resp.StatusCode)
	}
	if !strings.Contains(resp.Body, ":warning:") || !strings.Contains(resp.Body, "admin") {
		t.Errorf("body = %s, expected :warning: + admin language", resp.Body)
	}
	if len(resmock.requests) != 0 {
		t.Errorf("resource API was called %d times, expected 0 (admin gate must short-circuit)", len(resmock.requests))
	}
}

// TestUnsetAlias_NonAdmin_BlocksBeforeLookup fences the ordering
// requirement: the admin gate runs *before* alias resolution so a
// non-admin can't probe alias existence by reading the difference
// between "no such alias" and "you're not admin". The resource API
// must not be called at all.
func TestUnsetAlias_NonAdmin_BlocksBeforeLookup(t *testing.T) {
	admin := stubAdminBackend(t, false)
	resmock := newResourceMock(t, []resourceMockResponse{
		// Server is wired to respond, but the handler must short-
		// circuit on the admin gate so we never see this call.
		{matchVerb: http.MethodGet, matchPath: pathByAlias, status: http.StatusOK, body: `{"data":{"resource_id":"r_x","alias":"prod-db"}}`},
	})
	h := newSetAliasTestHandler(t, admin.URL, resmock.srv.URL, nil, nil)

	body := setAliasFormBody("unsetalias $prod-db", "T1", "C1", "U1", "tr4", "")
	resp, err := h.Handle(context.Background(), &events.APIGatewayProxyRequest{
		Path: pathSlackCommands, HTTPMethod: methodPost, Body: body, Headers: signSlackBody(t, body),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(resp.Body, ":warning:") || !strings.Contains(resp.Body, "admin") {
		t.Errorf("body = %s, expected :warning: admin error", resp.Body)
	}
	// Resource API must not have been called at all — admin gate
	// fires first to avoid info-disclosure on alias existence.
	if len(resmock.requests) != 0 {
		t.Errorf("resource API was called %d times, expected 0 (admin gate must run before resolution): %+v", len(resmock.requests), resmock.requests)
	}
}

// TestSetAlias_AliasInUse_409 fences the 409 → friendly mapping.
func TestSetAlias_AliasInUse_409(t *testing.T) {
	admin := stubAdminBackend(t, true)
	resmock := newResourceMock(t, []resourceMockResponse{
		{matchVerb: http.MethodGet, matchPath: pathByAlias, status: http.StatusNotFound, body: jsonAliasNotFound},
		{matchVerb: http.MethodPost, matchPath: pathResources, status: http.StatusConflict, body: `{"error":{"code":"alias_in_use","status":409,"title":"Conflict","detail":"already used"}}`},
	})
	h := newSetAliasTestHandler(t, admin.URL, resmock.srv.URL, nil, nil)

	body := setAliasFormBody("setalias $prod-db https://int.example", "T1", "C1", "U1", "tr5", "")
	resp, err := h.Handle(context.Background(), &events.APIGatewayProxyRequest{
		Path: pathSlackCommands, HTTPMethod: methodPost, Body: body, Headers: signSlackBody(t, body),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(resp.Body, "already used by another resource") {
		t.Errorf("body = %s, expected friendly alias_in_use mapping", resp.Body)
	}
}

// TestSetAlias_Reserved_422 fences the reserved-word → friendly
// mapping.
func TestSetAlias_Reserved_422(t *testing.T) {
	admin := stubAdminBackend(t, true)
	resmock := newResourceMock(t, []resourceMockResponse{
		{matchVerb: http.MethodGet, matchPath: pathByAlias, status: http.StatusNotFound, body: jsonAliasNotFound},
		{matchVerb: http.MethodPost, matchPath: pathResources, status: http.StatusUnprocessableEntity, body: `{"error":{"code":"alias_reserved","status":422,"title":"Unprocessable Entity","detail":"reserved"}}`},
	})
	h := newSetAliasTestHandler(t, admin.URL, resmock.srv.URL, nil, nil)

	body := setAliasFormBody("setalias $admin https://int.example", "T1", "C1", "U1", "tr6", "")
	resp, err := h.Handle(context.Background(), &events.APIGatewayProxyRequest{
		Path: pathSlackCommands, HTTPMethod: methodPost, Body: body, Headers: signSlackBody(t, body),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(resp.Body, "reserved word") {
		t.Errorf("body = %s, expected reserved-word mapping", resp.Body)
	}
}

// TestUnsetAlias_NotFound fences the friendly "no resource has alias"
// path.
func TestUnsetAlias_NotFound(t *testing.T) {
	admin := stubAdminBackend(t, true)
	resmock := newResourceMock(t, []resourceMockResponse{
		{matchVerb: http.MethodGet, matchPath: pathByAlias, status: http.StatusNotFound, body: jsonAliasNotFound},
	})
	h := newSetAliasTestHandler(t, admin.URL, resmock.srv.URL, nil, nil)

	body := setAliasFormBody("unsetalias $ghost", "T1", "C1", "U1", "tr7", "")
	resp, err := h.Handle(context.Background(), &events.APIGatewayProxyRequest{
		Path: pathSlackCommands, HTTPMethod: methodPost, Body: body, Headers: signSlackBody(t, body),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(resp.Body, "No resource has alias") {
		t.Errorf("body = %s, expected 'No resource has alias' message", resp.Body)
	}
}

// TestSetAliasRebindSubmit_Success fences the view_submission path:
// modal submit posts an interaction; the handler must PATCH the new
// resource with the alias set and return a 200 with empty body so
// Slack closes the modal.
func TestSetAliasRebindSubmit_Success(t *testing.T) {
	admin := stubAdminBackend(t, true)
	resmock := newResourceMock(t, []resourceMockResponse{
		// Old resource gets ClearAlias=true
		{matchVerb: http.MethodPatch, matchPath: "/v1/resources/r_old", status: http.StatusOK, body: `{"data":{"resource_id":"r_old"}}`},
		// New URL → CreateResource
		{matchVerb: http.MethodPost, matchPath: pathResources, status: http.StatusCreated, body: `{"data":{"resource_id":"r_new","alias":"prod-db"}}`},
	})
	h := newSetAliasTestHandler(t, admin.URL, resmock.srv.URL, nil, nil)

	pm, _ := json.Marshal(rebindPrivateMetadata{
		Alias:      testAliasProdDB,
		Target:     "https://new.example",
		ResourceID: "r_old",
	})
	payload := map[string]any{
		"type":       "view_submission",
		"trigger_id": "trig-submit",
		"team":       map[string]any{"id": "T1"},
		"user":       map[string]any{"id": "U1"},
		"view": map[string]any{
			"id":               "V1",
			"callback_id":      callbackIDSetAliasRebind,
			"private_metadata": string(pm),
			"state":            map[string]any{"values": map[string]any{}},
		},
	}
	pj, _ := json.Marshal(payload)
	body := url.Values{"payload": {string(pj)}}.Encode()

	resp, err := h.Handle(context.Background(), &events.APIGatewayProxyRequest{
		Path: pathSlackInteract, HTTPMethod: methodPost, Body: body, Headers: signSlackBody(t, body),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	// Body should be empty — Slack uses empty 200 to dismiss modal.
	if resp.Body != "" {
		t.Errorf("body = %q, want empty (modal close)", resp.Body)
	}
	// Backend must have seen a PATCH (ClearAlias) and a POST (Create).
	var sawPatch, sawPost bool
	for _, r := range resmock.requests {
		switch r.Method {
		case http.MethodPatch:
			if strings.Contains(r.Body, `"clear_alias":true`) {
				sawPatch = true
			}
		case http.MethodPost:
			if strings.Contains(r.Body, `"alias":"prod-db"`) {
				sawPost = true
			}
		}
	}
	if !sawPatch || !sawPost {
		t.Errorf("expected PATCH(clear_alias)+POST(alias=prod-db); requests=%+v", resmock.requests)
	}
}

// TestSlashCommand_ParserError_FriendlyEphemeral fences the parser-
// rejection path: a malformed setalias (missing target) must produce
// a friendly :warning: response, not a 500.
func TestSlashCommand_ParserError_FriendlyEphemeral(t *testing.T) {
	admin := stubAdminBackend(t, true)
	resmock := newResourceMock(t, nil)
	h := newSetAliasTestHandler(t, admin.URL, resmock.srv.URL, nil, nil)

	body := setAliasFormBody("setalias $prod-db", "T1", "C1", "U1", "tr-parse-err", "")
	resp, err := h.Handle(context.Background(), &events.APIGatewayProxyRequest{
		Path: pathSlackCommands, HTTPMethod: methodPost, Body: body, Headers: signSlackBody(t, body),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(resp.Body, ":warning:") {
		t.Errorf("body = %s, expected :warning: prefix on parser error", resp.Body)
	}
}

// submitInteractionBody builds the form-encoded view_submission body
// with the given private_metadata. Test helper for the modal-error
// tests below — keeps the per-test setup terse.
func submitInteractionBody(t *testing.T, pm string) string {
	t.Helper()
	payload := map[string]any{
		"type":       "view_submission",
		"trigger_id": "trig-x",
		"team":       map[string]any{"id": "T1"},
		"user":       map[string]any{"id": "U1"},
		"view": map[string]any{
			"id":               "V1",
			"callback_id":      callbackIDSetAliasRebind,
			"private_metadata": pm,
		},
	}
	pj, _ := json.Marshal(payload)
	return url.Values{"payload": {string(pj)}}.Encode()
}

// assertResponseActionClear fences the modal-clear envelope shape so
// every modal-side error test reads the same way. The envelope must
// be `{"response_action":"clear"}` — anything else either silently
// dismisses the modal (the bug this PR fixes) or leaves it stuck.
func assertResponseActionClear(t *testing.T, resp events.APIGatewayProxyResponse) {
	t.Helper()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(resp.Body), &got); err != nil {
		t.Fatalf("unmarshal modal response: %v\nbody=%q", err, resp.Body)
	}
	if got["response_action"] != "clear" {
		t.Errorf("response_action = %v, want \"clear\"; body=%s", got["response_action"], resp.Body)
	}
}

// TestSetAliasRebindSubmit_NonAdmin_ClearsModal fences the modal-side
// admin-gate failure path. With the rebind modal having no input
// blocks, a `response_action=errors` keyed on a fake block_id would
// silently dismiss the modal — so the handler must instead return
// `response_action=clear` and surface the failure via slog (operator
// signal). The PATCH must not fire.
func TestSetAliasRebindSubmit_NonAdmin_ClearsModal(t *testing.T) {
	admin := stubAdminBackend(t, false)
	resmock := newResourceMock(t, []resourceMockResponse{
		{matchVerb: http.MethodPatch, matchPath: "/v1/resources/r_old", status: http.StatusOK, body: `{"data":{"resource_id":"r_old"}}`},
	})
	h := newSetAliasTestHandler(t, admin.URL, resmock.srv.URL, nil, nil)

	pm, _ := json.Marshal(rebindPrivateMetadata{
		Alias:      testAliasProdDB,
		Target:     "https://new.example",
		ResourceID: "r_old",
	})
	body := submitInteractionBody(t, string(pm))
	resp, err := h.Handle(context.Background(), &events.APIGatewayProxyRequest{
		Path: pathSlackInteract, HTTPMethod: methodPost, Body: body, Headers: signSlackBody(t, body),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	assertResponseActionClear(t, resp)
	for _, r := range resmock.requests {
		if r.Method == http.MethodPatch {
			t.Errorf("non-admin reached PATCH, but admin gate should short-circuit: %+v", r)
		}
	}
}

// TestSetAliasRebindSubmit_MalformedMetadata_ClearsModal fences the
// invalid-private_metadata path. The submit must close the modal
// cleanly rather than 500 or leave it stuck.
func TestSetAliasRebindSubmit_MalformedMetadata_ClearsModal(t *testing.T) {
	admin := stubAdminBackend(t, true)
	resmock := newResourceMock(t, nil)
	h := newSetAliasTestHandler(t, admin.URL, resmock.srv.URL, nil, nil)

	body := submitInteractionBody(t, "{not-json")
	resp, err := h.Handle(context.Background(), &events.APIGatewayProxyRequest{
		Path: pathSlackInteract, HTTPMethod: methodPost, Body: body, Headers: signSlackBody(t, body),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	assertResponseActionClear(t, resp)
	if len(resmock.requests) != 0 {
		t.Errorf("malformed metadata triggered %d backend calls, expected 0", len(resmock.requests))
	}
}

// TestSetAliasRebindSubmit_MissingFields_ClearsModal fences the
// "private_metadata parsed but missing alias/target" path.
func TestSetAliasRebindSubmit_MissingFields_ClearsModal(t *testing.T) {
	admin := stubAdminBackend(t, true)
	resmock := newResourceMock(t, nil)
	h := newSetAliasTestHandler(t, admin.URL, resmock.srv.URL, nil, nil)

	pm, _ := json.Marshal(rebindPrivateMetadata{Alias: "", Target: ""})
	body := submitInteractionBody(t, string(pm))
	resp, err := h.Handle(context.Background(), &events.APIGatewayProxyRequest{
		Path: pathSlackInteract, HTTPMethod: methodPost, Body: body, Headers: signSlackBody(t, body),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	assertResponseActionClear(t, resp)
}

// TestSetAliasRebindSubmit_ResourceAPIFails_ClearsModal fences the
// PATCH-failure path. A 409 from the resource API on the rebind
// PATCH must close the modal cleanly — the operator-side log carries
// the friendly message for diagnostics.
func TestSetAliasRebindSubmit_ResourceAPIFails_ClearsModal(t *testing.T) {
	admin := stubAdminBackend(t, true)
	resmock := newResourceMock(t, []resourceMockResponse{
		{matchVerb: http.MethodPatch, matchPath: "/v1/resources/r_old", status: http.StatusConflict, body: `{"error":{"code":"alias_in_use","status":409,"title":"Conflict"}}`},
	})
	h := newSetAliasTestHandler(t, admin.URL, resmock.srv.URL, nil, nil)

	pm, _ := json.Marshal(rebindPrivateMetadata{
		Alias:      testAliasProdDB,
		Target:     "https://new.example",
		ResourceID: "r_old",
	})
	body := submitInteractionBody(t, string(pm))
	resp, err := h.Handle(context.Background(), &events.APIGatewayProxyRequest{
		Path: pathSlackInteract, HTTPMethod: methodPost, Body: body, Headers: signSlackBody(t, body),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	assertResponseActionClear(t, resp)
}

// TestSetAliasRebindSubmit_ResourceIDTarget fences the rebindAlias
// resource_id-target branch (only the URL-target branch was covered
// before). When the user picked an `r_…` target, the handler must
// emit two PATCHes: clear-alias on the old resource, set-alias on
// the new resource id.
func TestSetAliasRebindSubmit_ResourceIDTarget(t *testing.T) {
	admin := stubAdminBackend(t, true)
	resmock := newResourceMock(t, []resourceMockResponse{
		{matchVerb: http.MethodPatch, matchPath: "/v1/resources/r_old", status: http.StatusOK, body: `{"data":{"resource_id":"r_old"}}`},
		{matchVerb: http.MethodPatch, matchPath: "/v1/resources/r_new", status: http.StatusOK, body: `{"data":{"resource_id":"r_new","alias":"prod-db"}}`},
	})
	h := newSetAliasTestHandler(t, admin.URL, resmock.srv.URL, nil, nil)

	pm, _ := json.Marshal(rebindPrivateMetadata{
		Alias:      testAliasProdDB,
		Target:     "r_new",
		ResourceID: "r_old",
	})
	body := submitInteractionBody(t, string(pm))
	resp, err := h.Handle(context.Background(), &events.APIGatewayProxyRequest{
		Path: pathSlackInteract, HTTPMethod: methodPost, Body: body, Headers: signSlackBody(t, body),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if resp.Body != "" {
		t.Errorf("body = %q, want empty (modal close on happy path)", resp.Body)
	}
	// Must see PATCH-clear on r_old and PATCH-set-alias on r_new.
	var sawClear, sawSet bool
	for _, r := range resmock.requests {
		if r.Method != http.MethodPatch {
			continue
		}
		if strings.Contains(r.Path, "/v1/resources/r_old") && strings.Contains(r.Body, `"clear_alias":true`) {
			sawClear = true
		}
		if strings.Contains(r.Path, "/v1/resources/r_new") && strings.Contains(r.Body, `"alias":"prod-db"`) {
			sawSet = true
		}
	}
	if !sawClear || !sawSet {
		t.Errorf("expected PATCH(clear) on r_old and PATCH(set-alias) on r_new; requests=%+v", resmock.requests)
	}
}

// TestRebindNeedsConfirm_TrailingSlashEquivalence fences the URL
// normalization in [rebindNeedsConfirm]: a trailing-slash difference
// is treated as identical so the user isn't asked to confirm a
// no-op rebind.
func TestRebindNeedsConfirm_TrailingSlashEquivalence(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		current string
		next    string
		want    bool
	}{
		{"identical urls", "https://example.com", "https://example.com", false},
		{"trailing slash on current", "https://example.com/", "https://example.com", false},
		{"trailing slash on new", "https://example.com", "https://example.com/", false},
		{"both trailing slash", "https://example.com/", "https://example.com/", false},
		{"different paths", "https://example.com/a", "https://example.com/b", true},
		{"different hosts", "https://a.example.com", "https://b.example.com", true},
		{"empty current → no-op (no rebind)", "", "https://example.com", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := rebindNeedsConfirm(&Resource{TargetURL: tc.current}, tc.next)
			if got != tc.want {
				t.Errorf("rebindNeedsConfirm(%q, %q) = %v, want %v", tc.current, tc.next, got, tc.want)
			}
		})
	}
}
