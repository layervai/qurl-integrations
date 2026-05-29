package internal

import (
	"encoding/json"
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
