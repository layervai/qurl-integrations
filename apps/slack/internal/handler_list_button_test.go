package internal

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/shared/client"
)

// parseSlackBlocks unwraps the `blocks` array from a response_url
// payload. Returns nil when the payload carried no blocks (text-only).
func parseSlackBlocks(t *testing.T, body []byte) []any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal reply: %v body=%s", err, body)
	}
	blocks, _ := got["blocks"].([]any)
	return blocks
}

// createQurlButtonValues extracts the `value` of every "Create qURL"
// accessory button across the list blocks, in order.
func createQurlButtonValues(t *testing.T, blocks []any) []string {
	t.Helper()
	var vals []string
	for _, b := range blocks {
		block, _ := b.(map[string]any)
		acc, ok := block["accessory"].(map[string]any)
		if !ok {
			continue
		}
		if acc["action_id"] != listCreateQurlActionID {
			continue
		}
		v, _ := acc["value"].(string)
		vals = append(vals, v)
	}
	return vals
}

// listCreateQurlBlockActionsBody encodes a Slack block_actions
// interaction for the "Create qURL" list button — the wire shape a row
// click produces. `value` is the row's token (sigil stripped).
func listCreateQurlBlockActionsBody(t *testing.T, teamID, userID, channelID, responseURL, actionID, value string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"type":         "block_actions",
		"team":         map[string]any{"id": teamID},
		"user":         map[string]any{"id": userID},
		"channel":      map[string]any{"id": channelID},
		"trigger_id":   "trigger_test",
		"response_url": responseURL,
		"actions": []map[string]any{
			{"action_id": actionID, "block_id": "row_block", "value": value},
		},
	})
	if err != nil {
		t.Fatalf("marshal block_actions payload: %v", err)
	}
	return url.Values{"payload": {string(payload)}}.Encode()
}

// TestHandleList_RendersCreateQurlButtons fences the interactive list:
// every tunnel with a `$<token>` renders a "Create qURL" accessory
// button valued by that token, the slug-less row gets NO button (it has
// no token to mint against), and the plain-text fallback still carries
// the complete listing for notifications / accessibility.
func TestHandleList_RendersCreateQurlButtons(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: testListResIDProdDB, testKeyType: client.ResourceTypeTunnel, testKeySlug: testListAliasProdDB},
			{testKeyResourceID: "r_stage_db_bb", testKeyType: client.ResourceTypeTunnel, testKeySlug: "stage-db"},
			// Slug-less, alias-less tunnel: no `$<token>` → no button.
			{testKeyResourceID: "r_noslug0001", testKeyType: client.ResourceTypeTunnel},
		}, "", false)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	if status, _ := inv.invokeAdmin("list", testAdminTeamID, testAdminUserID); status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	body := inv.captured.waitForBody(t, 2*time.Second)

	blocks := parseSlackBlocks(t, body)
	if len(blocks) == 0 {
		t.Fatalf("list response carried no blocks: %s", body)
	}
	vals := createQurlButtonValues(t, blocks)
	// Rows sort by token: stage-db < prod-db? No — "prod-db" < "stage-db",
	// and the tokenless row sorts last. So buttons are [prod-db, stage-db].
	wantVals := []string{testListAliasProdDB, "stage-db"}
	if len(vals) != len(wantVals) {
		t.Fatalf("Create qURL button values = %v, want %v", vals, wantVals)
	}
	for i := range wantVals {
		if vals[i] != wantVals[i] {
			t.Errorf("button[%d] value = %q, want %q (all: %v)", i, vals[i], wantVals[i], vals)
		}
	}

	// Text fallback still lists every tunnel, including the buttonless
	// slug-less row, plus the one-time-use guidance.
	fallback := parseSlackText(t, body)
	for _, want := range []string{"`$prod-db`", "`$stage-db`", "no slug", "/qurl get", "one-time use"} {
		if !strings.Contains(fallback, want) {
			t.Errorf("text fallback missing %q: %q", want, fallback)
		}
	}
}

// TestHandleBlockActions_CreateQurlMints fences the button click: a
// block_actions interaction for the "Create qURL" button acks 200 with
// an empty body and mints a one-time qURL for the row's tunnel via the
// same pipeline as `/qurl get $<slug>`, delivering the link to the
// interaction's response_url.
func TestHandleBlockActions_CreateQurlMints(t *testing.T) {
	ts := newAdminTestServers(t)
	// Bind $prod-db → r_prod_db in C_test so the token resolves, exactly
	// as the typed `/qurl get $prod-db` happy path does.
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "prod-db", []string{testResourceIDFix})
	ts.addCustomer("POST", mintByTestResourcePath, func(w http.ResponseWriter, _ *http.Request) {
		writeCreateFixture(t, w, "https://qurl.link/btn", testResourceIDFix)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	body := listCreateQurlBlockActionsBody(t, testAdminTeamID, testAdminUserID, "C_test", inv.responseU.URL, listCreateQurlActionID, "prod-db")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", w.Code, w.Body.String())
	}
	if strings.TrimSpace(w.Body.String()) != "{}" {
		t.Fatalf("block_actions ack = %q, want empty JSON object", w.Body.String())
	}

	async := parseSlackText(t, inv.captured.waitForBody(t, 2*time.Second))
	if !strings.Contains(async, "https://qurl.link/btn") {
		t.Errorf("async reply missing minted link: %q", async)
	}
	if !strings.Contains(async, "one-time use") {
		t.Errorf("async reply missing one-time-use note: %q", async)
	}
}

// TestHandleBlockActions_UnparseableTokenRejected fences the
// defense-in-depth re-validation: a button value that our renderer would
// never emit (here, uppercase + spaces) fails parseAliasToken the same
// way a typed token would, so the click is rejected with the
// "couldn't process" copy and never reaches the mint.
func TestHandleBlockActions_UnparseableTokenRejected(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "prod-db", []string{testResourceIDFix})
	var mintHits atomic.Int32
	ts.addCustomer("POST", mintByTestResourcePath, func(w http.ResponseWriter, _ *http.Request) {
		mintHits.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	body := listCreateQurlBlockActionsBody(t, testAdminTeamID, testAdminUserID, "C_test", inv.responseU.URL, listCreateQurlActionID, "Not A Token")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))

	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "{}" {
		t.Fatalf("ack = %d %q, want 200 and {}", w.Code, w.Body.String())
	}
	async := parseSlackText(t, inv.captured.waitForBody(t, 2*time.Second))
	if !strings.Contains(async, "Couldn't process") {
		t.Errorf("async reply missing rejection copy: %q", async)
	}
	if mintHits.Load() != 0 {
		t.Errorf("mint reached for an unparseable token (hits = %d)", mintHits.Load())
	}
}

// TestHandleBlockActions_NotAllowedNonAdmin fences the headline security
// property at the button boundary: clicking "Create qURL" cannot mint a
// tunnel the user couldn't already mint by typing the command. A
// non-admin clicks the button for a slug whose resource isn't in this
// channel's allow-set; the button routes through the same
// resolveTokenForGet → resourceAllowedForUser gate as `/qurl get`, so it
// gets the anti-enumeration "not configured for this channel" copy, the
// resolved resource_id never leaks, and the mint never runs. Mirrors
// TestHandleGet_DollarSlugNotAllowedNonAdmin on the interaction path.
func TestHandleBlockActions_NotAllowedNonAdmin(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedNonAdmin(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "", []string{"r_other_alloc"})
	addTunnelSlugResource(t, ts)
	var mintHits atomic.Int32
	ts.addCustomer("POST", mintByTestResourcePath, func(w http.ResponseWriter, _ *http.Request) {
		mintHits.Add(1)
		writeCreateFixture(t, w, "https://qurl.link/should-not", testResourceIDFix)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	body := listCreateQurlBlockActionsBody(t, testAdminTeamID, testAdminUserID, "C_test", inv.responseU.URL, listCreateQurlActionID, testTunnelSlug)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))

	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "{}" {
		t.Fatalf("ack = %d %q, want 200 and {}", w.Code, w.Body.String())
	}
	async := parseSlackText(t, inv.captured.waitForBody(t, 2*time.Second))
	if !strings.Contains(async, "`$"+testTunnelSlug+"` is not configured for this channel") {
		t.Errorf("button bypassed channel authorization — missing not-configured copy: %q", async)
	}
	if strings.Contains(async, testResourceIDFix) {
		t.Errorf("resolved resource_id leaked in rejection message: %q", async)
	}
	if mintHits.Load() != 0 {
		t.Errorf("button minted a tunnel not allowed in this channel (hits = %d)", mintHits.Load())
	}
}

// TestHandleList_OverflowDegradesToText fences the block-ceiling guard:
// a tunnel set larger than listCreateButtonMaxRows renders as plain text
// (no blocks) so every tunnel still shows — rather than a >50-block
// message Slack would reject. Locks the cap so a future bump that would
// breach the ceiling fails here.
func TestHandleList_OverflowDegradesToText(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	n := listCreateButtonMaxRows + 1 // one past the cap → text-only path
	resources := make([]map[string]any, 0, n)
	for i := 0; i < n; i++ {
		resources = append(resources, map[string]any{
			testKeyResourceID: fmt.Sprintf("r_tun_%03d", i),
			testKeyType:       client.ResourceTypeTunnel,
			testKeySlug:       fmt.Sprintf("tun-%03d", i),
		})
	}
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, resources, "", false)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	if status, _ := inv.invokeAdmin("list", testAdminTeamID, testAdminUserID); status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	body := inv.captured.waitForBody(t, 2*time.Second)
	if blocks := parseSlackBlocks(t, body); blocks != nil {
		t.Errorf("expected text-only fallback past the button cap, got %d blocks", len(blocks))
	}
	text := parseSlackText(t, body)
	// First and last rows both present → the text path truncates nothing.
	for _, want := range []string{"`$tun-000`", fmt.Sprintf("`$tun-%03d`", n-1), "/qurl get"} {
		if !strings.Contains(text, want) {
			t.Errorf("text fallback missing %q", want)
		}
	}
}

// TestHandleBlockActions_UnknownActionIgnored fences the routing: a
// block_actions payload whose action_id we don't recognize is acked 200
// (so Slack doesn't error the user) and never reaches the mint.
func TestHandleBlockActions_UnknownActionIgnored(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "prod-db", []string{testResourceIDFix})
	var mintHits atomic.Int32
	ts.addCustomer("POST", mintByTestResourcePath, func(w http.ResponseWriter, _ *http.Request) {
		mintHits.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	body := listCreateQurlBlockActionsBody(t, testAdminTeamID, testAdminUserID, "C_test", inv.responseU.URL, "some_other_button", "prod-db")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))

	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "{}" {
		t.Fatalf("unknown-action ack = %d %q, want 200 and {}", w.Code, w.Body.String())
	}
	// No async worker is started for an unmatched action; the brief wait
	// gives a (buggy) one a chance to fire so the negative assertion bites.
	time.Sleep(50 * time.Millisecond)
	if mintHits.Load() != 0 {
		t.Errorf("mint reached for an unrecognized action (hits = %d)", mintHits.Load())
	}
}
