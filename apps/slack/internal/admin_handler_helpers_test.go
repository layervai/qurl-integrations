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
//     `ts.seedAdmin(...)`, `ts.seedPolicyDualShape(...)` before invoking.
//   - [invokeAdminSlash] signs a slash-command form body and drives
//     ServeHTTP. Returns (status, replyText) so assertion sites stay
//     terse. The reply text is parsed from the JSON envelope — for
//     async verbs that's the synchronous `ackWorkingOnIt`; the
//     response_url follow-up text is read off the captured POST.

import (
	"context"
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
// Tests seed table rows via ts.seedAdmin / ts.seedPolicyDualShape / etc.
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
	channelID    string
	enterpriseID string
}

// newAdminSlashInvoker spins up a response_url-capturing httptest
// server and returns an invoker bound to it. Defaults the slash-
// command channel_id to "C_test" — most tests don't care which
// channel, so the default keeps boilerplate down. Tests that need a
// non-default or empty channel_id use [newAdminSlashInvokerOnChannel].
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
	return &adminSlashInvoker{t: t, h: h, captured: captured, responseU: srv, channelID: "C_test"}
}

// newAdminSlashInvokerOnChannel is newAdminSlashInvoker with an
// explicit channel_id. Passing "" sends a truly-empty channel_id on
// the wire — the default-to-C_test fallback lives in
// [newAdminSlashInvoker], not in invokeAdmin, so the
// fail-closed-on-missing-channel branch can be exercised honestly.
func newAdminSlashInvokerOnChannel(t *testing.T, h *Handler, channelID string) *adminSlashInvoker {
	t.Helper()
	inv := newAdminSlashInvoker(t, h)
	inv.channelID = channelID
	return inv
}

// slashCommandForVerb picks which slash command (`/qurl` vs
// `/qurl-admin`) a given verb text is invoked under, mirroring the
// production dispatch split: user verbs (get / list / aliases / create /
// setup) arrive on `/qurl`; everything else — the admin verbs plus the
// admin-help and bare-`admin` cases — arrives on `/qurl-admin`. Centralized
// here so the shared admin invoker drives both surfaces with the command
// Slack would actually stamp, rather than hardcoding one and silently
// exercising the wrong-surface redirect.
func slashCommandForVerb(text string) string {
	if isUserVerb(text) {
		return commandUser
	}
	return commandAdmin
}

// invokeAdmin issues a signed slash-command request and returns
// (status, syncReplyText). Use this for sync verbs (allow, disallow,
// status, revoke) — the rendered reply is in the sync body.
func (a *adminSlashInvoker) invokeAdmin(text, teamID, userID string) (status int, replyText string) {
	a.t.Helper()
	body := url.Values{
		"command":      {slashCommandForVerb(text)},
		"text":         {text},
		"team_id":      {teamID},
		"user_id":      {userID},
		"channel_id":   {a.channelID},
		"response_url": {a.responseU.URL},
		"trigger_id":   {"trigger_test"},
	}
	if a.enterpriseID != "" {
		body.Set(fieldEnterpriseID, a.enterpriseID)
	}
	encoded := body.Encode()
	w := httptest.NewRecorder()
	r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/slack/commands", strings.NewReader(encoded))
	sig, ts := signSlackBody(a.t, encoded)
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
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal reply: %v body=%s", err, body)
	}
	text, _ := got["text"].(string)
	return text
}

func parseSlackReplyBool(t *testing.T, body []byte, field string) bool {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal reply: %v body=%s", err, body)
	}
	value, _ := got[field].(bool)
	return value
}

// workspaceMappingHasAdmin returns true iff the workspace_mappings
// row for `teamID` exists AND carries `slackUserID` in its
// admin_slack_user_ids SS (set). The OAuth callback writes this row
// via BindWorkspace as the installer becomes the seed admin;
// without it CheckAdmin returns (false, "", nil) on every subsequent
// admin verb. This helper is the post-state fence for the persistence bug.
func (f *fakeDDB) workspaceMappingHasAdmin(t *testing.T, teamID, slackUserID string) bool {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, tbl := range f.tables {
		for _, item := range tbl {
			team, _ := item[fAttrSlackTeamID].(*ddbtypes.AttributeValueMemberS)
			if team == nil || team.Value != teamID {
				continue
			}
			// Skip channel_policies rows (they also carry slack_team_id
			// in the same fake-table scan).
			if _, isChannelRow := item[fAttrSlackChannelID]; isChannelRow {
				continue
			}
			ss, ok := item[fAttrAdminSlackUserIDs].(*ddbtypes.AttributeValueMemberSS)
			if !ok {
				return false
			}
			for _, u := range ss.Value {
				if u == slackUserID {
					return true
				}
			}
			return false
		}
	}
	return false
}
