package internal

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// recordedOpenView is a test stub for Config.OpenView. Captures the
// trigger_id + view JSON for assertion.
type recordedOpenView struct {
	calls atomic.Int32
	mu    struct {
		triggerID string
		view      []byte
	}
	returnErr error
}

func (r *recordedOpenView) fn() func(context.Context, string, []byte) error {
	return func(_ context.Context, triggerID string, view []byte) error {
		r.calls.Add(1)
		r.mu.triggerID = triggerID
		r.mu.view = view
		return r.returnErr
	}
}

// TestHandleAdminClaim_OpensModal fences the canonical claim flow:
// the slash command synchronously invokes OpenView with the modal
// JSON, then acks 200 with no body (Slack treats empty 200 as the
// modal-opening dismiss).
func TestHandleAdminClaim_OpensModal(t *testing.T) {
	ts := newAdminTestServers(t)
	rov := &recordedOpenView{}
	h := newAdminTestHandler(t, ts)
	h.cfg.OpenView = rov.fn()
	inv := newAdminSlashInvoker(t, h)

	status, ack := inv.invokeAdmin("admin claim", testAdminTeamID, testAdminUserID)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if rov.calls.Load() != 1 {
		t.Fatalf("OpenView calls = %d, want 1", rov.calls.Load())
	}
	if rov.mu.triggerID != "trigger_test" {
		t.Errorf("OpenView trigger_id = %q, want trigger_test", rov.mu.triggerID)
	}
	// The view JSON must carry the admin-claim callback_id so the
	// view_submission router can route the submit.
	if !strings.Contains(string(rov.mu.view), callbackIDAdminClaim) {
		t.Errorf("OpenView view missing callback_id %q: %s", callbackIDAdminClaim, rov.mu.view)
	}
	// Slack-compatible empty ack — must NOT carry a "text" field
	// (which would render as an unwanted ephemeral on top of the
	// modal).
	if strings.Contains(ack, ":") {
		t.Errorf("ack carries an emoji-prefixed reply (modal was supposed to be the response): %q", ack)
	}
}

// TestHandleAdminClaim_NoOpenView fences the sandbox/no-bot-token
// path: OpenView nil → user gets a "modal cannot be opened" reply
// rather than nil-deref.
func TestHandleAdminClaim_NoOpenView(t *testing.T) {
	ts := newAdminTestServers(t)
	h := newAdminTestHandler(t, ts)
	// OpenView nil by default.
	inv := newAdminSlashInvoker(t, h)

	_, ack := inv.invokeAdmin("admin claim", testAdminTeamID, testAdminUserID)
	if !strings.Contains(ack, "Modal cannot be opened") {
		t.Errorf("ack missing not-configured message: %q", ack)
	}
}

// TestHandleAdminClaim_OpenViewError fences the views.open failure
// path: a raw Slack error is logged but NOT surfaced to the user
// (cr round 4 nit #1 — leaking trigger_id_expired / not_authed
// codes confuses end users).
func TestHandleAdminClaim_OpenViewError(t *testing.T) {
	ts := newAdminTestServers(t)
	rov := &recordedOpenView{returnErr: errors.New("trigger_id_expired")}
	h := newAdminTestHandler(t, ts)
	h.cfg.OpenView = rov.fn()
	inv := newAdminSlashInvoker(t, h)

	_, ack := inv.invokeAdmin("admin claim", testAdminTeamID, testAdminUserID)
	if strings.Contains(ack, "trigger_id_expired") {
		t.Errorf("ack leaked raw Slack error code: %q", ack)
	}
	if !strings.Contains(ack, "Could not open the modal") {
		t.Errorf("ack missing generic modal-open failure message: %q", ack)
	}
}

// TestHandleAdminClaim_NoTriggerID fences the malformed-payload
// path: a slash command without trigger_id (synthetic test payload
// or wire-shape bug) gets a friendly error.
func TestHandleAdminClaim_NoTriggerID(t *testing.T) {
	ts := newAdminTestServers(t)
	rov := &recordedOpenView{}
	h := newAdminTestHandler(t, ts)
	h.cfg.OpenView = rov.fn()

	// Build a bespoke request without trigger_id — the standard
	// invoker always sets one.
	body := url.Values{
		fieldCommand:     {testCmdSlash},
		fieldText:        {testCmdAdminClaim},
		fieldTeamID:      {testAdminTeamID},
		fieldUserID:      {testAdminUserID},
		fieldChannelID:   {"C_test"},
		fieldResponseURL: {"https://hooks.slack.com/services/x"},
	}.Encode()
	w := httptest.NewRecorder()
	r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/slack/commands", strings.NewReader(body))
	sig, ts2 := signSlackBody(t, body)
	r.Header.Set(headerSlackSignature, sig)
	r.Header.Set(headerSlackTimestamp, ts2)
	h.ServeHTTP(w, r)

	if rov.calls.Load() != 0 {
		t.Errorf("OpenView called despite missing trigger_id: %d calls", rov.calls.Load())
	}
	var ack map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &ack)
	if !strings.Contains(ack["text"], "trigger_id") {
		t.Errorf("ack missing trigger_id-error: %v", ack)
	}
}

// --- view_submission tests ---

// invokeInteraction issues a signed POST /slack/interactions request
// carrying a view_submission payload. Returns the response body as
// a map for assertion.
func invokeInteraction(t *testing.T, h *Handler, payloadJSON string) (status int, replyBody map[string]any) {
	t.Helper()
	form := url.Values{"payload": {payloadJSON}}.Encode()
	w := httptest.NewRecorder()
	r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/slack/interactions", strings.NewReader(form))
	sig, ts := signSlackBody(t, form)
	r.Header.Set(headerSlackSignature, sig)
	r.Header.Set(headerSlackTimestamp, ts)
	h.ServeHTTP(w, r)
	var got map[string]any
	if len(w.Body.Bytes()) > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("unmarshal reply: %v body=%s", err, w.Body)
		}
	}
	return w.Code, got
}

// buildClaimSubmission constructs the JSON shape Slack sends for a
// view_submission on the admin-claim modal.
func buildClaimSubmission(teamID, userID, code string) string {
	payload := map[string]any{
		"type": "view_submission",
		"team": map[string]any{"id": teamID},
		"user": map[string]any{"id": userID},
		"view": map[string]any{
			"id":                "V_test",
			testFieldCallbackID: callbackIDAdminClaim,
			"state": map[string]any{
				"values": map[string]any{
					blockIDClaimCode: map[string]any{
						actionIDClaimCode: map[string]any{"value": code},
					},
				},
			},
		},
		fieldTriggerID: "trig_submit",
	}
	b, _ := json.Marshal(payload)
	return string(b)
}

// TestHandleAdminClaimSubmit_HappyPath fences the canonical
// view_submission path: a valid code is consumed via RedeemBootstrap,
// the workspace_mappings row is persisted via BindWorkspace, PostDM
// gets the success message, and the modal closes (empty 200).
func TestHandleAdminClaimSubmit_HappyPath(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.ddb.seedItem(t, ts.tableNames.bootstrapCodes, seedBootstrapCode(t, "BOOT-VALID", testAdminOwnerID, "k_xxx", time.Now().Add(time.Hour), false))
	var dmCalls atomic.Int32
	var dmText string
	h := newAdminTestHandler(t, ts)
	h.cfg.PostDM = func(_ context.Context, _, text string) error {
		dmCalls.Add(1)
		dmText = text
		return nil
	}

	status, reply := invokeInteraction(t, h, buildClaimSubmission(testAdminTeamID, testAdminUserID, "BOOT-VALID"))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if len(reply) != 0 {
		// An empty-body 200 unmarshals to nil map; an `errors`
		// envelope unmarshals to a non-empty map. Either an empty
		// body or {} is "modal close".
		if _, ok := reply[modalKeyResponseAction]; ok {
			t.Errorf("reply carried response_action on success: %v", reply)
		}
	}
	if dmCalls.Load() != 1 {
		t.Errorf("PostDM calls = %d, want 1", dmCalls.Load())
	}
	if !strings.Contains(dmText, "admin for this workspace") {
		t.Errorf("DM text missing success message: %q", dmText)
	}
	// Persistence fence — the regression we just fixed was that the
	// success DM landed but workspace_mappings was never written, so
	// the very next /qurl admin command returned "you are not an
	// admin." Assert the row is present with the redeemer on the
	// admin set.
	if !ts.ddb.workspaceMappingHasAdmin(t, testAdminTeamID, testAdminUserID) {
		t.Errorf("workspace_mappings row missing or did not include %q on admin_slack_user_ids — admin-claim persistence regression", testAdminUserID)
	}
}

// TestHandleAdminClaimSubmit_PersistsAdminMapping is a dedicated fence
// for the persistence behavior: RedeemBootstrap burns the one-time
// code and BindWorkspace writes the workspace_mappings row carrying
// the redeemer on admin_slack_user_ids. Without the BindWorkspace
// step, the bootstrap code is consumed but the workspace remains
// unbound — the user gets the success DM and then `/qurl admin
// allow` returns "you are not an admin." This test exists so a
// refactor that drops the BindWorkspace call (or reverts to the
// `_, err :=` discard shape) regresses loudly instead of silently.
func TestHandleAdminClaimSubmit_PersistsAdminMapping(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.ddb.seedItem(t, ts.tableNames.bootstrapCodes, seedBootstrapCode(t, "BOOT-PERSIST", testAdminOwnerID, "k_persist", time.Now().Add(time.Hour), false))
	h := newAdminTestHandler(t, ts)
	// No PostDM wired — keeps the test focused on the persistence
	// surface rather than the DM surface.

	// Pre-state: no workspace row yet.
	if ts.ddb.workspaceMappingHasAdmin(t, testAdminTeamID, testAdminUserID) {
		t.Fatalf("pre-state: workspace_mappings row should not exist before claim")
	}

	status, _ := invokeInteraction(t, h, buildClaimSubmission(testAdminTeamID, testAdminUserID, "BOOT-PERSIST"))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}

	// Post-state: row exists with redeemer on admin_slack_user_ids.
	if !ts.ddb.workspaceMappingHasAdmin(t, testAdminTeamID, testAdminUserID) {
		t.Fatalf("workspace_mappings row missing or did not include %q after admin claim — bootstrap code was burned but workspace is not bound", testAdminUserID)
	}

	// And the slackdata-side CheckAdmin (the surface every other
	// admin verb reads) agrees the user is an admin.
	store := newStoreFromFake(t, ts.ddb, ts.tableNames, nil)
	isAdmin, _, err := store.CheckAdmin(context.Background(), testAdminTeamID, testAdminUserID)
	if err != nil {
		t.Fatalf("CheckAdmin: %v", err)
	}
	if !isAdmin {
		t.Errorf("CheckAdmin(%q, %q) = false, want true after successful claim", testAdminTeamID, testAdminUserID)
	}
}

// TestHandleAdminClaimSubmit_InvalidCode fences the canonical
// rejection path: a wrong/expired/already-used code returns the
// view_submission errors envelope ("Code is invalid or expired.")
// so the modal stays open with the field highlighted.
func TestHandleAdminClaimSubmit_InvalidCode(t *testing.T) {
	ts := newAdminTestServers(t)
	// No seeded code → RedeemBootstrap returns 410 + errCodeBootstrapInvalid.
	h := newAdminTestHandler(t, ts)
	_, reply := invokeInteraction(t, h, buildClaimSubmission(testAdminTeamID, testAdminUserID, "BOOT-WRONG"))
	if got, _ := reply[modalKeyResponseAction].(string); got != modalResponseActionErrors {
		t.Errorf("reply response_action = %v, want \"errors\"", reply[modalKeyResponseAction])
	}
	errs, _ := reply[modalKeyErrors].(map[string]any)
	msg, _ := errs[blockIDClaimCode].(string)
	if !strings.Contains(msg, "invalid or expired") {
		t.Errorf("field error missing invalid-or-expired copy: %q", msg)
	}
}

// TestHandleAdminClaimSubmit_EmptyCode fences the validation surface:
// an empty code (hand-crafted POST bypassing Slack's client-side
// required check) gets a field-level "Bootstrap code is required."
func TestHandleAdminClaimSubmit_EmptyCode(t *testing.T) {
	ts := newAdminTestServers(t)
	h := newAdminTestHandler(t, ts)
	_, reply := invokeInteraction(t, h, buildClaimSubmission(testAdminTeamID, testAdminUserID, ""))
	if got, _ := reply[modalKeyResponseAction].(string); got != modalResponseActionErrors {
		t.Errorf("reply response_action = %v, want \"errors\"", reply[modalKeyResponseAction])
	}
}

// TestInteractionPayload_LogValueRedactsClaimCode fences the
// defense-in-depth: slog of an interactionPayload that carries a
// bootstrap code in the redacted block emits "<redacted>" instead
// of the code.
func TestInteractionPayload_LogValueRedactsClaimCode(t *testing.T) {
	p, err := parseInteractionPayload("payload=" + url.QueryEscape(buildClaimSubmission(testAdminTeamID, testAdminUserID, "SUPER-SECRET-CODE")))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	v := p.LogValue()
	// LogValue returns a slog.Value carrying a Group; render it
	// through a JSON-style string and assert no plaintext leak.
	rendered := v.Resolve().String()
	if strings.Contains(rendered, "SUPER-SECRET-CODE") {
		t.Errorf("LogValue leaked bootstrap code: %s", rendered)
	}
	if !strings.Contains(rendered, "<redacted>") {
		t.Errorf("LogValue missing redacted sentinel: %s", rendered)
	}
}

// TestHandleAdminClaimSubmit_ContradictorySignalsPreventedOnError
// fences the round-1 cr issue (#2) + round-4 fix: when PostDM is
// wired AND a redeem hits the generic error branch, we DM ONE
// :warning: and close the modal — never both modal-open AND DM.
func TestHandleAdminClaimSubmit_ContradictorySignalsPreventedOnError(t *testing.T) {
	ts := newAdminTestServers(t)
	// Seed a code but force the store's RedeemBootstrap to return a
	// 500 by injecting a fakeDDB UpdateItem error.
	ts.ddb.seedItem(t, ts.tableNames.bootstrapCodes, seedBootstrapCode(t, "BOOT-WILL-500", testAdminOwnerID, "k_xxx", time.Now().Add(time.Hour), false))
	ts.ddb.SetUpdateItemErr(ts.tableNames.bootstrapCodes, errors.New("ddb unavailable"))

	var dmCalls atomic.Int32
	h := newAdminTestHandler(t, ts)
	h.cfg.PostDM = func(_ context.Context, _, _ string) error {
		dmCalls.Add(1)
		return nil
	}
	status, reply := invokeInteraction(t, h, buildClaimSubmission(testAdminTeamID, testAdminUserID, "BOOT-WILL-500"))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if dmCalls.Load() != 1 {
		t.Errorf("PostDM calls = %d, want 1 (single-signal contract on generic error path)", dmCalls.Load())
	}
	// Body should be empty (modal close); the absence of
	// response_action="errors" is the fence.
	if _, ok := reply[modalKeyResponseAction]; ok {
		t.Errorf("reply carried response_action despite DM landing: %v (contradictory signal!)", reply)
	}
}

// TestHandleAdminClaimSubmit_DMFailureFallback fences the
// round-3 cr fix: when PostDM is wired but fails on a generic-error
// branch, we fall back to the modal field-level error so the user
// has *some* signal.
func TestHandleAdminClaimSubmit_DMFailureFallback(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.ddb.seedItem(t, ts.tableNames.bootstrapCodes, seedBootstrapCode(t, "BOOT-WILL-500", testAdminOwnerID, "k_xxx", time.Now().Add(time.Hour), false))
	ts.ddb.SetUpdateItemErr(ts.tableNames.bootstrapCodes, errors.New("ddb unavailable"))

	h := newAdminTestHandler(t, ts)
	h.cfg.PostDM = func(_ context.Context, _, _ string) error {
		return errors.New("slack 503")
	}
	_, reply := invokeInteraction(t, h, buildClaimSubmission(testAdminTeamID, testAdminUserID, "BOOT-WILL-500"))
	if got, _ := reply[modalKeyResponseAction].(string); got != modalResponseActionErrors {
		t.Errorf("reply response_action = %v, want \"errors\" on DM failure", reply[modalKeyResponseAction])
	}
}

// TestHandleAdminClaimSubmit_AdminStoreNil fences the sandbox path:
// no DDB → no redeem path → user gets a friendly field-level error.
func TestHandleAdminClaimSubmit_AdminStoreNil(t *testing.T) {
	ts := newAdminTestServers(t)
	h := newAdminTestHandler(t, ts)
	h.cfg.AdminStore = nil
	_, reply := invokeInteraction(t, h, buildClaimSubmission(testAdminTeamID, testAdminUserID, "BOOT-X"))
	if got, _ := reply[modalKeyResponseAction].(string); got != modalResponseActionErrors {
		t.Errorf("reply response_action = %v, want \"errors\"", reply[modalKeyResponseAction])
	}
}

// TestHandleAdminClaim_AsyncDoesNotRouteHere fences the dispatcher:
// `/qurl admin claim` MUST hit the sync handler (views.open
// requires the live request goroutine because trigger_id rotates).
// A future refactor that routed claim through runAsync would break
// the modal — this test catches that.
func TestHandleAdminClaim_DoesNotUseRunAsync(t *testing.T) {
	ts := newAdminTestServers(t)
	rov := &recordedOpenView{}
	h := newAdminTestHandler(t, ts)
	h.cfg.OpenView = rov.fn()
	inv := newAdminSlashInvoker(t, h)

	// Time the call — sync dispatch should complete in <20ms; async
	// would have to wait for the response_url POST (>>20ms because
	// the dispatcher's goroutine + httptest server overhead).
	start := time.Now()
	_, ack := inv.invokeAdmin("admin claim", testAdminTeamID, testAdminUserID)
	elapsed := time.Since(start)
	if elapsed > 100*time.Millisecond {
		t.Errorf("claim took %v, want sync-fast (<100ms)", elapsed)
	}
	if strings.Contains(ack, ackWorkingOnIt) {
		t.Errorf("claim acked with %q — should NOT use runAsync (modal needs live trigger_id)", ack)
	}
}
