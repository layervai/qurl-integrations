package internal

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestHandleCRID_HappyPath fences the direct CRID share flow independently of
// alias resolution: the SDK receives the CRID /share request and Slack gets the
// same Enter Portal block rendering as `/qurl get`.
func TestHandleCRID_HappyPath(t *testing.T) {
	ts := newAdminTestServers(t)
	path := "/v1/resources/" + testTunnelCRID + "/share"
	ts.addCustomer(http.MethodPost, path, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q, want workspace bearer key", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode share body: %v", err)
		}
		if len(body) != 0 {
			t.Errorf("share body = %#v, want SDK defaults ({})", body)
		}
		respondQURLEnvelope(t, w, map[string]any{
			"qurl":               "https://qurl.link/crid-share",
			"crid":               testTunnelCRID,
			"expires_in_seconds": 60,
			"single_use":         true,
		})
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	status, ack := inv.invokeAdmin("crid "+testTunnelCRID, testAdminTeamID, testAdminUserID)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if ack != ackWorkingOnIt {
		t.Errorf("ack = %q, want %q", ack, ackWorkingOnIt)
	}
	body := inv.captured.waitForBody(t, 2e9)
	async := parseSlackText(t, body)
	if !strings.Contains(async, "https://qurl.link/crid-share") {
		t.Errorf("async reply missing link: %q", async)
	}
	if !strings.Contains(async, "one-time use") {
		t.Errorf("async reply does not use get rendering fallback: %q", async)
	}
	var response struct {
		Blocks []any `json:"blocks"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("decode response blocks: %v", err)
	}
	if got := enterPortalButton(t, response.Blocks)["url"]; got != "https://qurl.link/crid-share" {
		t.Errorf("Enter Portal URL = %q, want CRID share link", got)
	}
}

// TestHandleCRID_ResponseMismatchNeverDeliversLink ensures a service response
// that is not bound to the requested CRID is discarded before Slack rendering.
func TestHandleCRID_ResponseMismatchNeverDeliversLink(t *testing.T) {
	ts := newAdminTestServers(t)
	path := "/v1/resources/" + testTunnelCRID + "/share"
	ts.addCustomer(http.MethodPost, path, func(w http.ResponseWriter, _ *http.Request) {
		respondQURLEnvelope(t, w, map[string]any{
			"qurl": "https://qurl.link/must-not-deliver",
			"crid": "ahpviqz46qwcvx56glfatm3p3ooccwfcf2it4sdgjervwdkapykw2j2vj4uq",
		})
	})
	h := newAdminTestHandler(t, ts)
	_, _, async := newAdminSlashInvoker(t, h).invokeAdminAsync("crid "+testTunnelCRID, testAdminTeamID, testAdminUserID)
	if strings.Contains(async, "https://qurl.link/must-not-deliver") {
		t.Fatalf("mismatched response leaked link: %q", async)
	}
	if !strings.Contains(async, commonGetMintFailedMessage) {
		t.Errorf("mismatch reply = %q, want safe failure", async)
	}
}
