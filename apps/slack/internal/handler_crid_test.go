package internal

import (
	"context"
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
	for name, seed := range map[string]func(*testing.T, *adminTestServers){
		"other resource allowed": func(t *testing.T, ts *adminTestServers) {
			ts.seedPolicySet(t, testAdminTeamID, "C_test", "prod-db", []string{testResourceIDFix})
		},
		"allowed only in another channel": func(t *testing.T, ts *adminTestServers) {
			ts.seedPolicySet(t, testAdminTeamID, "C_test", "prod-db", []string{testResourceIDFix})
			ts.seedPolicySet(t, testAdminTeamID, "C_other", "tunnel", []string{testTunnelResourceID})
		},
		"cold channel": func(t *testing.T, ts *adminTestServers) { ts.seedNonAdmin(t) },
	} {
		t.Run(name, func(t *testing.T) {
			ts := newAdminTestServers(t)
			seed(t, ts)
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

// TestHandleCRID_SyncParseReplies pins the synchronous replies for a bare and
// a malformed CRID, and the `/qurl get <CRID>` redirect.
func TestHandleCRID_SyncParseReplies(t *testing.T) {
	h := newAdminTestHandler(t, newAdminTestServers(t))
	for text, want := range map[string]string{
		"crid":                        cridUsageMessage,
		"crid " + testTunnelCRID[:40]: invalidCRIDMessage,
		"crid " + strings.ToUpper(testTunnelCRID): invalidCRIDMessage,
		"crid " + testTunnelCRID + " junk":        cridUsageMessage,
		"get " + testTunnelCRID:                   cridNotSupportedGetMessage,
	} {
		t.Run(text, func(t *testing.T) {
			_, ack := newAdminSlashInvoker(t, h).invokeAdmin(text, testAdminTeamID, testAdminUserID)
			if !strings.Contains(ack, want) {
				t.Errorf("ack = %q, want %q", ack, want)
			}
		})
	}
}

// TestHandleCRID_SharesGetRateLimit pins that crid and get draw on one in-bot
// mint quota, so alternating verbs cannot double a user's budget.
func TestHandleCRID_SharesGetRateLimit(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "prod-db", []string{testResourceIDFix, testTunnelResourceID})
	ts.addCustomer(http.MethodPost, mintByTestResourcePath, func(w http.ResponseWriter, _ *http.Request) {
		writeCreateFixture(t, w, "https://qurl.link/get", testResourceIDFix)
	})
	var cridMints atomic.Int32
	ts.addCustomer(http.MethodPost, mintByTestTunnelPath, func(w http.ResponseWriter, _ *http.Request) {
		cridMints.Add(1)
		writeCreateFixture(t, w, "https://qurl.link/crid", testTunnelResourceID)
	})
	h := newAdminTestHandler(t, ts)
	enableAdminStoreRateLimit(t, h, 1)

	if _, _, first := newAdminSlashInvoker(t, h).invokeAdminAsync("get $prod-db", testAdminTeamID, testAdminUserID); !strings.Contains(first, "https://qurl.link/get") {
		t.Fatalf("get did not mint: %q", first)
	}
	_, _, second := newAdminSlashInvoker(t, h).invokeAdminAsync("crid "+testTunnelCRID, testAdminTeamID, testAdminUserID)
	if !strings.Contains(second, "Rate limit hit") || cridMints.Load() != 0 {
		t.Errorf("crid after exhausted get quota = %q (mints = %d), want in-bot rate limit", second, cridMints.Load())
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
	if audit["resource_id"] != testTunnelResourceID || audit["reason"] != "incident #9" || audit["addressed_by"] != "crid" {
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
	if got, _ := resourceIDForCRID(allowed, testTunnelCRID); got != testTunnelResourceID {
		t.Errorf("resourceIDForCRID = %q, want %q", got, testTunnelResourceID)
	}
	delete(allowed, testTunnelResourceID)
	// Only testResourceIDFix decodes to a public key; "r_legacy" decodes as
	// base64url but is not DER, and the URL does not decode at all.
	if got, candidates := resourceIDForCRID(allowed, testTunnelCRID); got != "" || candidates != 1 {
		t.Errorf("resourceIDForCRID = %q, %d candidates; want miss with 1 candidate", got, candidates)
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

func TestHandleCRID_DMDelivers(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "tunnel", []string{testTunnelResourceID})
	ts.addCustomer(http.MethodPost, mintByTestTunnelPath, func(w http.ResponseWriter, _ *http.Request) {
		writeCreateFixture(t, w, "https://qurl.link/crid-dm", testTunnelResourceID)
	})
	h := newAdminTestHandler(t, ts)
	var dmText string
	h.cfg.PostDMBlocks = func(_ context.Context, _, _, _ string, _ []any, fallbackText string) error {
		dmText = fallbackText
		return nil
	}

	_, _, async := newAdminSlashInvoker(t, h).invokeAdminAsync("crid "+testTunnelCRID+" dm:true", testAdminTeamID, testAdminUserID)
	if !strings.Contains(dmText, "https://qurl.link/crid-dm") {
		t.Errorf("DM text = %q, want the minted link", dmText)
	}
	if !strings.Contains(async, ":incoming_envelope:") || strings.Contains(async, "https://qurl.link/crid-dm") {
		t.Errorf("async = %q, want DM confirmation without the link", async)
	}
}

// TestHandleGet_DMGuardRunsAfterResolution pins the get ordering this PR
// introduced: an unknown alias reports the alias miss even with dm:true in a
// workspace without DM delivery, while a valid alias reports the DM refusal
// before any mint.
func TestHandleGet_DMGuardRunsAfterResolution(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "prod-db", []string{testResourceIDFix})
	var mintHits atomic.Int32
	ts.addCustomerPrefix(http.MethodPost, "/v1/resources/", func(w http.ResponseWriter, _ *http.Request) {
		mintHits.Add(1)
		writeCreateFixture(t, w, "https://qurl.link/must-not", testResourceIDFix)
	})
	ts.addCustomer(http.MethodGet, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{}, "", false)
	})
	h := newAdminTestHandler(t, ts) // PostDMBlocks is nil by default.

	_, _, typo := newAdminSlashInvoker(t, h).invokeAdminAsync("get $typo dm:true", testAdminTeamID, testAdminUserID)
	if !strings.Contains(typo, "`$typo` is not configured for this channel") {
		t.Errorf("unknown alias + dm:true = %q, want alias-miss copy", typo)
	}
	_, _, valid := newAdminSlashInvoker(t, h).invokeAdminAsync("get $prod-db dm:true", testAdminTeamID, testAdminUserID)
	if !strings.Contains(valid, errDMNotConfigured.msg) || mintHits.Load() != 0 {
		t.Errorf("valid alias + dm:true = %q (mints = %d), want DM refusal before mint", valid, mintHits.Load())
	}
}
