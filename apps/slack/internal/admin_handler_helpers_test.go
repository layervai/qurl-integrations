package internal

// Test-only helpers for the post-rebase net/http admin handlers.
// The pre-rebase #231 admin tests used `events.APIGatewayProxyRequest`
// + `h.Handle(ctx, ...)` against a Lambda-shape handler. After #228
// rebased onto main (which absorbed the net/http runtime swap from
// #254), the handler is `*Handler.ServeHTTP(w, r)` and tests drive
// it via httptest.NewRequest + httptest.NewRecorder.
//
// Two helpers, in test-helper convention:
//   - [newAdminTestHandler] wires a *Handler with AdminStore backed
//     by an in-memory fakeDDB. Tests seed table rows via
//     `ts.seedAdmin(...)`, `ts.seedPolicySingle(...)` before invoking.
//   - [invokeAdminSlash] signs a slash-command form body and drives
//     ServeHTTP. Returns (status, replyText) so assertion sites stay
//     terse. The reply text is parsed from the JSON envelope — for
//     async verbs that's the synchronous `ackWorkingOnIt`; the
//     response_url follow-up text is read off the captured POST.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/layervai/qurl-integrations/shared/auth"
	"github.com/layervai/qurl-integrations/shared/client"
)

// capturedResponseURL holds the bodies POSTed to a test response_url
// endpoint. invokeAdminSlashAsync passes one of these to runAsync via
// values.Get("response_url") and tests assert against captured[0].
type capturedResponseURL struct {
	mu     sync.Mutex
	bodies [][]byte
}

func (c *capturedResponseURL) record(body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Defensive copy: the request body buffer may be recycled.
	dup := make([]byte, len(body))
	copy(dup, body)
	c.bodies = append(c.bodies, dup)
}

// waitForBody polls captured.bodies up to d for at least one entry.
// Async handlers spawn goroutines that POST to response_url after
// the sync ack returns — tests use this to drive past that hand-off
// without a fragile sleep.
func (c *capturedResponseURL) waitForBody(t *testing.T, d time.Duration) []byte {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		c.mu.Lock()
		if len(c.bodies) > 0 {
			b := c.bodies[0]
			c.mu.Unlock()
			return b
		}
		c.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatalf("response_url body not received within %s", d)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// newAdminTestHandler wires a *Handler with:
//   - QURLEndpoint pointed at ts.customerServer (httptest).
//   - AdminStore backed by ts.ddb (in-memory fakeDDB).
//   - Clock pinned to fixedNow so signed-request fixtures stay stable.
//   - validateResponseURLFn relaxed (httptest server URLs are
//     http://127.0.0.1:NNNNN — the production validator rejects those).
//
// Tests seed table rows via ts.seedAdmin / ts.seedPolicySingle / etc.
// before invoking the handler.
func newAdminTestHandler(t *testing.T, ts *adminTestServers) *Handler {
	t.Helper()
	t.Setenv("QURL_API_KEY", "test-key")
	store := newStoreFromFake(t, ts.ddb, ts.tableNames, nil)
	store.Now = func() time.Time { return fixedNow }
	h := NewHandler(Config{
		AuthProvider:       &auth.EnvProvider{EnvVar: "QURL_API_KEY"},
		SlackSigningSecret: testSigningSecret,
		NewClient: func(apiKey string) *client.Client {
			return client.New(ts.customerServer.URL, apiKey, client.WithRetry(0))
		},
		AdminStore: store,
	})
	h.now = func() time.Time { return fixedNow }
	// httptest URLs are http://127.0.0.1:NNNNN — the production
	// SSRF-pinned validator rejects them. Relax so async paths can
	// POST to the captured response_url.
	h.validateResponseURLFn = url.Parse
	// t.Cleanup runs in LIFO order: this is registered FIRST so it
	// runs LAST, which means async goroutines drain after httptest
	// servers close. That's the wrong direction — a goroutine still
	// mid-POST to a closed response_url server would hit a "closed
	// connection" error and log noise. The right shape is to register
	// h.Wait AFTER httptest registration so it drains first. But
	// httptest.NewServer's t.Cleanup is registered inside
	// newAdminTestServers BEFORE this helper runs, so flipping the
	// order from inside newAdminTestHandler is impossible without
	// reordering newAdminTestServers's registration. In practice
	// httptest.Server.Close blocks until in-flight handlers return
	// (test-time goroutines hit it inside response_url POST and
	// complete the write), so this races but doesn't fail. Leaving
	// the comment honest rather than misleading.
	t.Cleanup(h.Wait)
	return h
}

// adminSlashInvoker bundles the captured response_url and provides
// invokeAdmin* helpers. The captured response_url is necessary for
// async verbs (policies, revoke-all) — the synchronous ack is just
// `ackWorkingOnIt`; the actual rendered reply arrives via the
// follow-up POST.
type adminSlashInvoker struct {
	t         *testing.T
	h         *Handler
	captured  *capturedResponseURL
	responseU *httptest.Server
	// channelID overrides the slash-command channel_id form field
	// for the next invocation. Empty falls back to "C_test".
	channelID string
}

// newAdminSlashInvoker spins up a response_url-capturing httptest
// server and returns an invoker bound to it.
func newAdminSlashInvoker(t *testing.T, h *Handler) *adminSlashInvoker {
	t.Helper()
	captured := &capturedResponseURL{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("response_url POST read: %v", err)
			return
		}
		captured.record(body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return &adminSlashInvoker{t: t, h: h, captured: captured, responseU: srv}
}

// newAdminSlashInvokerOnChannel is newAdminSlashInvoker with a
// non-default channel_id. Used by tests that exercise filter logic
// against a specific channel.
func newAdminSlashInvokerOnChannel(t *testing.T, h *Handler, channelID string) *adminSlashInvoker {
	t.Helper()
	inv := newAdminSlashInvoker(t, h)
	inv.channelID = channelID
	return inv
}

// invokeAdmin issues a signed slash-command request and returns
// (status, syncReplyText). Use this for sync verbs (allow, disallow,
// status, revoke) — the rendered reply is in the sync body.
func (a *adminSlashInvoker) invokeAdmin(text, teamID, userID string) (status int, replyText string) {
	a.t.Helper()
	channelID := a.channelID
	if channelID == "" {
		channelID = "C_test"
	}
	body := url.Values{
		"command":      {"/qurl"},
		"text":         {text},
		"team_id":      {teamID},
		"user_id":      {userID},
		"channel_id":   {channelID},
		"response_url": {a.responseU.URL},
		"trigger_id":   {"trigger_test"},
	}.Encode()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/slack/commands", strings.NewReader(body))
	sig, ts := signSlackBody(a.t, body)
	r.Header.Set(headerSlackSignature, sig)
	r.Header.Set(headerSlackTimestamp, ts)
	a.h.ServeHTTP(w, r)
	return w.Code, parseSlackText(a.t, w.Body.Bytes())
}

// invokeAdminAsync issues a signed slash-command request, waits for
// the response_url body, and returns the response_url-rendered text.
// Use this for async verbs (policies, revoke-all).
func (a *adminSlashInvoker) invokeAdminAsync(text, teamID, userID string) (syncStatus int, syncReply, asyncReply string) {
	a.t.Helper()
	status, ack := a.invokeAdmin(text, teamID, userID)
	body := a.captured.waitForBody(a.t, 2*time.Second)
	return status, ack, parseSlackText(a.t, body)
}

// parseSlackText unwraps {"response_type":"ephemeral","text":"..."}
// to the text field. Tests assert on text directly.
func parseSlackText(t *testing.T, body []byte) string {
	t.Helper()
	var got map[string]string
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal reply: %v body=%s", err, body)
	}
	return got["text"]
}

// policyHasResource returns true iff the channel_policies row for
// (teamID, channelID) carries resourceID in its allowed_resource_ids
// SS (set). The production AllowResource path stores resources in
// the SS shape so the post-mutation check needs to look for set
// membership, not bare equality.
func (f *fakeDDB) policyHasResource(t *testing.T, teamID, channelID, resourceID string) bool {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	// Look up the row by composite key teamID:channelID.
	for _, tbl := range f.tables {
		for _, item := range tbl {
			team, _ := item[fAttrSlackTeamID].(*ddbtypes.AttributeValueMemberS)
			ch, _ := item[fAttrSlackChannelID].(*ddbtypes.AttributeValueMemberS)
			if team == nil || ch == nil {
				continue
			}
			if team.Value != teamID || ch.Value != channelID {
				continue
			}
			if ss, ok := item[fAttrAllowedResourceIDs].(*ddbtypes.AttributeValueMemberSS); ok {
				for _, v := range ss.Value {
					if v == resourceID {
						return true
					}
				}
			}
			if rid, ok := item[fAttrResourceID].(*ddbtypes.AttributeValueMemberS); ok && rid.Value == resourceID {
				return true
			}
		}
	}
	return false
}
