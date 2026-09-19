package internal

import (
	"context"
	"encoding/json"
	"errors"
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
		"another workspace": func(t *testing.T, ts *adminTestServers) {
			ts.seedPolicySet(t, "T_other", "C_test", "tunnel", []string{testTunnelResourceID})
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
			if strings.Contains(async, testTunnelCRID) || strings.Contains(async, testTunnelResourceID) {
				t.Errorf("denial disclosed resource identity: %q", async)
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
		"get " + strings.Repeat("a", 60):          invalidCRIDMessage,
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
		"bG9jYWwtZml4dHVyZQ":      {},
		"r_legacy":                {},
		"https://legacy.example/": {},
		testTunnelResourceID:      {},
	}
	if got, _ := resourceIDForCRID(allowed, testTunnelCRID); got != testTunnelResourceID {
		t.Errorf("resourceIDForCRID = %q, want %q", got, testTunnelResourceID)
	}
	delete(allowed, testTunnelResourceID)
	// Go base64 decoding ignores line breaks even in strict mode.
	allowed[testTunnelResourceID[:8]+"\n"+testTunnelResourceID[8:]] = struct{}{}
	// The local base64url fixture and "r_legacy" decode as base64url (candidates whose
	// digest cannot match); the URL does not decode at all.
	if got, candidates := resourceIDForCRID(allowed, testTunnelCRID); got != "" || candidates != 2 {
		t.Errorf("resourceIDForCRID = %q, %d candidates; want miss with 2 candidates", got, candidates)
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
	ts.ddb.SetGetItemErr(ts.tableNames.channelPolicy, errors.New("policy read must not run before DM refusal"))

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

// TestHandleGet_DMGuardRunsBeforeResolution checks the early privacy refusal.
func TestHandleGet_DMGuardRunsBeforeResolution(t *testing.T) {
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
	ts.ddb.SetGetItemErr(ts.tableNames.channelPolicy, errors.New("policy read must not run before DM refusal"))

	_, _, typo := newAdminSlashInvoker(t, h).invokeAdminAsync("get $typo dm:true", testAdminTeamID, testAdminUserID)
	if !strings.Contains(typo, errDMNotConfigured.msg) {
		t.Errorf("unknown alias + dm:true = %q, want DM refusal", typo)
	}
	_, _, valid := newAdminSlashInvoker(t, h).invokeAdminAsync("get $prod-db dm:true", testAdminTeamID, testAdminUserID)
	if !strings.Contains(valid, errDMNotConfigured.msg) || mintHits.Load() != 0 {
		t.Errorf("valid alias + dm:true = %q (mints = %d), want DM refusal before mint", valid, mintHits.Load())
	}
}

func TestHandleCRID_MintErrors(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			logs := captureDefaultSlog(t)
			ts := newAdminTestServers(t)
			ts.seedPolicySet(t, testAdminTeamID, "C_test", "tunnel", []string{testTunnelResourceID})
			ts.addCustomer(http.MethodPost, mintByTestTunnelPath, func(w http.ResponseWriter, _ *http.Request) {
				writeAPIError(t, w, status, "unexpected", "private upstream detail")
			})
			h := newAdminTestHandler(t, ts)
			_, _, reply := newAdminSlashInvoker(t, h).invokeAdminAsync("crid "+testTunnelCRID, testAdminTeamID, testAdminUserID)
			if !strings.Contains(reply, commonGetMintFailedMessage) || strings.Contains(reply, "private upstream detail") || strings.Contains(reply, testTunnelResourceID) {
				t.Fatalf("unsafe or incorrect failure reply: %q", reply)
			}
			if got := findAuditRecord(logs, slackaudit.DependencyAuthFailure); (got != nil) != (status != http.StatusNotFound) {
				t.Errorf("dependency auth audit = %#v for status %d", got, status)
			}
			if findAuditRecord(logs, slackaudit.QURLMintCRID) != nil {
				t.Error("failed mint recorded as successful")
			}
		})
	}
}

func TestHandleCRID_AuditsWithoutReason(t *testing.T) {
	logs := captureDefaultSlog(t)
	ts := newAdminTestServers(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "tunnel", []string{testTunnelResourceID})
	ts.addCustomer(http.MethodPost, mintByTestTunnelPath, func(w http.ResponseWriter, _ *http.Request) {
		writeCreateFixture(t, w, "https://qurl.link/crid", testTunnelResourceID)
	})
	h := newAdminTestHandler(t, ts)
	newAdminSlashInvoker(t, h).invokeAdminAsync("crid "+testTunnelCRID, testAdminTeamID, testAdminUserID)
	audit := findAuditRecord(logs, slackaudit.QURLMintCRID)
	if audit == nil || audit["resource_id"] != testTunnelResourceID || audit["channel_id"] != "C_test" || audit["user_id"] != testAdminUserID || audit["team_id"] != testAdminTeamID {
		t.Fatalf("missing CRID mint identity: %#v", audit)
	}
	if strings.Contains(logs.String(), "https://qurl.link/crid") {
		t.Error("mint audit disclosed the access link")
	}
}

func TestMintFlagErrorsBoundAndEscapeInput(t *testing.T) {
	for _, prefix := range []string{"get $test ", "crid " + testTunnelCRID + " "} {
		for _, token := range []string{"<!channel>", "`<!channel>", "dm:<!channel>", "<!channel>:true", strings.Repeat("z", 2048), "dm:" + strings.Repeat("z", 2048)} {
			_, err := Parse(prefix + token)
			if err == nil || len(err.Error()) > 200 {
				t.Fatalf("unbounded flag error: %v", err)
			}
			for i, part := range strings.Split(err.Error(), "`") {
				if i%2 == 0 && strings.Contains(part, "<!channel>") {
					t.Errorf("mention outside code span: %s", err)
				}
			}
		}
	}
}

func TestHandleCRID_PolicyReadFailure(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "tunnel", []string{testTunnelResourceID})
	ts.ddb.SetGetItemErr(ts.tableNames.channelPolicy, errors.New("private policy failure"))
	var mints atomic.Int32
	ts.addCustomerPrefix(http.MethodPost, "/v1/resources/", func(w http.ResponseWriter, _ *http.Request) {
		mints.Add(1)
		writeCreateFixture(t, w, "https://qurl.link/must-not", testTunnelResourceID)
	})
	h := newAdminTestHandler(t, ts)
	_, _, reply := newAdminSlashInvoker(t, h).invokeAdminAsync("crid "+testTunnelCRID, testAdminTeamID, testAdminUserID)
	if mints.Load() != 0 || !strings.Contains(reply, serviceUnreachableMessage) || strings.Contains(reply, "private policy failure") {
		t.Fatalf("policy failure did not fail closed: mints=%d reply=%q", mints.Load(), reply)
	}
}

func TestMintResponseIdentityMismatchNeverDelivers(t *testing.T) {
	for _, command := range []string{"get $tunnel", "crid " + testTunnelCRID} {
		t.Run(command, func(t *testing.T) {
			logs := captureDefaultSlog(t)
			ts := newAdminTestServers(t)
			ts.seedPolicySet(t, testAdminTeamID, "C_test", "tunnel", []string{testTunnelResourceID})
			ts.addCustomer(http.MethodPost, mintByTestTunnelPath, func(w http.ResponseWriter, _ *http.Request) {
				writeCreateFixture(t, w, "https://qurl.link/must-not", testResourceIDFix)
			})
			h := newAdminTestHandler(t, ts)
			_, _, reply := newAdminSlashInvoker(t, h).invokeAdminAsync(command+` reason:"incident"`, testAdminTeamID, testAdminUserID)
			if findAuditRecord(logs, slackaudit.QURLMintCRID) != nil || findAuditRecord(logs, slackaudit.QURLMintReason) != nil {
				t.Error("mismatched response recorded as successful mint")
			}
			if !strings.Contains(reply, commonGetMintFailedMessage) || strings.Contains(reply, "https://qurl.link/must-not") {
				t.Fatalf("mismatched mint response delivered: %q", reply)
			}
		})
	}
}

func TestHandleCRID_DenialSanitizesLogIDs(t *testing.T) {
	logs := captureDefaultSlog(t)
	h := newAdminTestHandler(t, newAdminTestServers(t))
	newAdminSlashInvokerOnChannel(t, h, "C\nforged").invokeAdminAsync("crid "+testTunnelCRID, "T\rforged", "U\nforged")
	decoder := json.NewDecoder(strings.NewReader(logs.String()))
	for {
		var record map[string]any
		if err := decoder.Decode(&record); err != nil {
			t.Fatalf("denial log not found: %v", err)
		}
		if record["msg"] != "crid: CRID not in channel allow-set" {
			continue
		}
		for _, key := range []string{"team_id", "channel_id", "user_id"} {
			value, ok := record[key].(string)
			if !ok || strings.ContainsAny(value, "\r\n") {
				t.Errorf("unsanitized %s: %#v", key, record[key])
			}
		}
		return
	}
}
