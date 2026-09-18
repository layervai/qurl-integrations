package internal

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackaudit"
)

const mintByTestTunnelPath = "/v1/resources/" + testTunnelResourceID + "/qurls"

// TestHandleCRID_HappyPath fences that a CRID matching a channel-allowed
// resource mints through the same pinned-policy request and Enter Portal
// render as `/qurl get`.
func TestHandleCRID_HappyPath(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "tunnel", []string{testResourceIDFix, testTunnelResourceID})
	var body map[string]any
	ts.addCustomer(http.MethodPost, mintByTestTunnelPath, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		writeCreateFixture(t, w, "https://qurl.link/crid", testTunnelResourceID)
	})
	h := newAdminTestHandler(t, ts)

	status, ack, async := newAdminSlashInvoker(t, h).invokeAdminAsync("crid "+testTunnelCRID, testAdminTeamID, testAdminUserID)
	if status != http.StatusOK || ack != ackWorkingOnIt {
		t.Fatalf("status/ack = %d/%q, want 200/%q", status, ack, ackWorkingOnIt)
	}
	if !strings.Contains(async, "https://qurl.link/crid") || !strings.Contains(async, "one-time use") {
		t.Errorf("async reply = %q, want get's one-time link render", async)
	}
	if body["one_time_use"] != true || body["expires_in"] == nil {
		t.Errorf("mint body = %#v, want get's pinned one-time/expiry policy", body)
	}
}

// TestHandleCRID_NotInChannelFailsClosed pins the authorization posture: a
// valid CRID for a resource not in this channel's allow-set (or a channel with
// no policy, e.g. a DM) never reaches the mint.
func TestHandleCRID_NotInChannelFailsClosed(t *testing.T) {
	for name, seed := range map[string]func(*adminTestServers){
		"other resource allowed": func(ts *adminTestServers) {
			ts.seedPolicySet(t, testAdminTeamID, "C_test", "prod-db", []string{testResourceIDFix})
		},
		"cold channel": func(ts *adminTestServers) { ts.seedNonAdmin(t) },
	} {
		t.Run(name, func(t *testing.T) {
			ts := newAdminTestServers(t)
			seed(ts)
			var mintHits atomic.Int32
			ts.addCustomerPrefix(http.MethodPost, "/v1/resources/", func(w http.ResponseWriter, _ *http.Request) {
				mintHits.Add(1)
				writeCreateFixture(t, w, "https://qurl.link/must-not", testTunnelResourceID)
			})
			h := newAdminTestHandler(t, ts)

			_, _, async := newAdminSlashInvoker(t, h).invokeAdminAsync("crid "+testTunnelCRID, testAdminTeamID, testAdminUserID)
			if !strings.Contains(async, cridNotInChannelMessage) {
				t.Errorf("async reply = %q, want not-in-channel copy", async)
			}
			if mintHits.Load() != 0 {
				t.Errorf("mint reached for a CRID outside the channel allow-set (hits = %d)", mintHits.Load())
			}
		})
	}
}

func TestHandleCRID_AdminStoreNil(t *testing.T) {
	ts := newAdminTestServers(t)
	h := newAdminTestHandler(t, ts)
	h.cfg.AdminStore = nil

	_, _, async := newAdminSlashInvoker(t, h).invokeAdminAsync("crid "+testTunnelCRID, testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "qURL admin features are not yet configured") {
		t.Errorf("async reply = %q, want not-configured copy", async)
	}
}

// TestHandleCRID_ReasonAuditsResourceID pins that the reason audit record
// carries the resolved resource_id (joinable to the mint), not the CRID.
func TestHandleCRID_ReasonAuditsResourceID(t *testing.T) {
	logs := captureDefaultSlog(t)
	ts := newAdminTestServers(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "tunnel", []string{testTunnelResourceID})
	ts.addCustomer(http.MethodPost, mintByTestTunnelPath, func(w http.ResponseWriter, _ *http.Request) {
		writeCreateFixture(t, w, "https://qurl.link/crid", testTunnelResourceID)
	})
	h := newAdminTestHandler(t, ts)

	newAdminSlashInvoker(t, h).invokeAdminAsync(`crid `+testTunnelCRID+` reason:"incident #9"`, testAdminTeamID, testAdminUserID)

	audit := findAuditRecord(logs, slackaudit.QURLMintReason)
	if audit == nil {
		t.Fatalf("no %s audit record; logs=%s", slackaudit.QURLMintReason, logs.String())
	}
	if audit["resource_id"] != testTunnelResourceID || audit["reason"] != "incident #9" {
		t.Errorf("audit = %#v, want resource_id %q and the reason", audit, testTunnelResourceID)
	}
}

func TestResourceIDForCRID(t *testing.T) {
	allowed := map[string]struct{}{
		testResourceIDFix:         {},
		"r_legacy":                {},
		"https://legacy.example/": {},
		testTunnelResourceID:      {},
	}
	if got, ok := resourceIDForCRID(allowed, testTunnelCRID); !ok || got != testTunnelResourceID {
		t.Errorf("resourceIDForCRID = %q, %v; want %q, true", got, ok, testTunnelResourceID)
	}
	delete(allowed, testTunnelResourceID)
	if got, ok := resourceIDForCRID(allowed, testTunnelCRID); ok {
		t.Errorf("resourceIDForCRID matched %q without the CRID's resource in the set", got)
	}
}

func TestHandleCRID_DMRefusedWhenPostDMBlocksNil(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "tunnel", []string{testTunnelResourceID})
	var mintHits atomic.Int32
	ts.addCustomer(http.MethodPost, mintByTestTunnelPath, func(w http.ResponseWriter, _ *http.Request) {
		mintHits.Add(1)
		writeCreateFixture(t, w, "https://qurl.link/must-not", testTunnelResourceID)
	})
	h := newAdminTestHandler(t, ts) // PostDMBlocks is nil by default.

	_, _, async := newAdminSlashInvoker(t, h).invokeAdminAsync("crid "+testTunnelCRID+" dm:true", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, errDMNotConfigured.msg) || mintHits.Load() != 0 {
		t.Errorf("async = %q, mint hits = %d; want DM refusal before any mint", async, mintHits.Load())
	}
}
