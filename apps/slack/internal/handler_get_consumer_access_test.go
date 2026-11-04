package internal

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/layervai/qurl-integrations/apps/slack/internal/agent"
	"github.com/layervai/qurl-integrations/shared/client"
)

const (
	testLinkOrigin        = "https://link.example"
	testLinkSiteAccessURL = "https://link.example/unlock"
)

// A resource that requires verified recipient access issues no bearer link, so
// `/qurl get` delivers its CRID entry page instead of a mint failure. Both the
// 403 of current qurl-service and the 500 of earlier builds lead there; every
// other case keeps the usual error mapping.
func TestHandleGet_ConsumerAccessDeliversCRIDEntry(t *testing.T) {
	entry := testLinkOrigin + "/" + testTunnelCRID
	for _, tc := range []struct {
		name string
		// legacy answers the mint with qurl-service's earlier 500 instead of the 403.
		legacy bool
		// foreign authorizes a resource whose key testTunnelCRID does not digest.
		foreign bool
		// notKey authorizes an id that is not base64url, so it cannot be a key.
		notKey bool
		// otherCode refuses the mint with another 403 code, which keeps its mapping.
		otherCode bool
		// noCRID reads the resource without a crid; badCRID with a malformed one.
		noCRID, badCRID bool
		version         int64
		readFails       bool
		noLinkOrigin    bool
		privateLinkSite bool
		want            []string
		wantAbsent      []string
		// wantLog is the operator signal the case must leave.
		wantLog string
	}{
		{name: "403 consumer_access_required", version: 1, wantLog: "answering with the CRID entry page",
			want: []string{"Verified access required", entry}, wantAbsent: []string{commonGetMintFailedMessage}},
		{name: "legacy 500 creation_failed", legacy: true, version: 3, wantLog: `"status":500`,
			want: []string{"Verified access required", entry}},
		{name: "private link site gets the unlock step first", version: 1, privateLinkSite: true,
			want: []string{"First open " + testLinkSiteAccessURL + ", then " + entry}},
		{name: "500 without a policy is still a service failure", legacy: true,
			want: []string{serviceUnreachableMessage}, wantAbsent: []string{entry}},
		{name: "a CRID that is not the resource's is never delivered", foreign: true, version: 1, wantLog: "does not match its key",
			want: []string{consumerAccessRequiredMessage("")}, wantAbsent: []string{testTunnelCRID}},
		{name: "a 403 for a resource read without a policy answers the refusal", wantLog: "consumer_access_version absent or zero",
			want: []string{consumerAccessRequiredMessage("")}, wantAbsent: []string{testTunnelCRID}},
		{name: "another 403 code keeps its mapping without a read", otherCode: true,
			want: []string{connectorDisabledMessage}, wantAbsent: []string{consumerAccessRequiredMessage("")}},
		{name: "an id that is not base64url answers the refusal without a read", notKey: true,
			want: []string{consumerAccessRequiredMessage("")}, wantAbsent: []string{testTunnelCRID}},
		{name: "a resource read without a CRID answers the refusal", version: 1, noCRID: true, wantLog: "has no CRID",
			want: []string{consumerAccessRequiredMessage("")}, wantAbsent: []string{testLinkOrigin + "/"}},
		{name: "a malformed CRID is never delivered", version: 1, badCRID: true, wantLog: "resource CRID is malformed",
			want: []string{consumerAccessRequiredMessage("")}, wantAbsent: []string{testLinkOrigin + "/"}},
		{name: "a legacy 500 with a failed read is still a service failure", legacy: true, version: 1, readFails: true,
			want: []string{serviceUnreachableMessage}, wantAbsent: []string{consumerAccessRequiredMessage("")}},
		{name: "a failed resource read answers the refusal", version: 1, readFails: true, wantLog: "resource read after a refused mint failed",
			want: []string{consumerAccessRequiredMessage("")}, wantAbsent: []string{testTunnelCRID}},
		{name: "no link origin answers the refusal", noLinkOrigin: true,
			want: []string{consumerAccessRequiredMessage("")}, wantAbsent: []string{testTunnelCRID}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resourceID, mintStatus, mintCode := testTunnelResourceID, http.StatusForbidden, errCodeConsumerAccessRequired
			if tc.foreign {
				resourceID = testResourceIDFix
				// A real key, so the case reaches crid.KeyMatches and not the decode guard.
				if _, ok := client.ResourceKeyDER(resourceID); !ok {
					t.Fatalf("foreign resource id %q must be a public key", resourceID)
				}
			}
			if tc.notKey {
				resourceID = "r.legacy"
			}
			if tc.legacy {
				mintStatus, mintCode = http.StatusInternalServerError, errCodeCreationFailed
			}
			if tc.otherCode {
				mintCode = errCodeConnectorDisabled
			}
			ts := newAdminTestServers(t)
			ts.seedPolicySet(t, testAdminTeamID, "C_test", "dashboard", []string{resourceID})
			ts.addCustomer(http.MethodPost, "/v1/resources/"+resourceID+"/qurls", func(w http.ResponseWriter, _ *http.Request) {
				writeAPIError(t, w, mintStatus, mintCode, "refused")
			})
			ts.addCustomer(http.MethodGet, "/v1/resources/"+resourceID, func(w http.ResponseWriter, _ *http.Request) {
				if tc.notKey || tc.otherCode {
					t.Error("this refusal must not read the resource")
				}
				if tc.readFails {
					writeAPIError(t, w, http.StatusNotFound, "not_found", "gone")
					return
				}
				cridValue := testTunnelCRID
				if tc.noCRID {
					cridValue = ""
				}
				if tc.badCRID {
					cridValue = "not-a-crid"
				}
				writeConsumerAccessResource(t, w, resourceID, cridValue, tc.version)
			})
			h := newAdminTestHandler(t, ts)
			if !tc.noLinkOrigin {
				h.cfg.LinkOrigin = testLinkOrigin
			}
			if tc.privateLinkSite {
				h.cfg.LinkSiteAccessURL = testLinkSiteAccessURL
			}

			logs := captureDefaultSlog(t)
			t.Cleanup(h.Wait) // Runs before the logger is restored.

			_, _, async := newAdminSlashInvoker(t, h).invokeAdminAsync("get $dashboard", testAdminTeamID, testAdminUserID)
			h.Wait()
			if tc.wantLog != "" && !logs.contains(tc.wantLog) {
				t.Errorf("logs missing %q", tc.wantLog)
			}
			for _, s := range tc.want {
				if !strings.Contains(async, s) {
					t.Errorf("async reply missing %q: %q", s, async)
				}
			}
			for _, s := range tc.wantAbsent {
				if strings.Contains(async, s) {
					t.Errorf("async reply must not contain %q: %q", s, async)
				}
			}
		})
	}
}

// A successful mint never reads the resource: the fallback runs only after a
// refusal.
func TestHandleGet_SuccessfulMintMakesNoResourceRead(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "dashboard", []string{testTunnelResourceID})
	ts.addCustomer(http.MethodPost, mintByTestTunnelPath, func(w http.ResponseWriter, _ *http.Request) {
		writeCreateFixture(t, w, "https://qurl.link/minted", testTunnelResourceID)
	})
	ts.addCustomer(http.MethodGet, "/v1/resources/"+testTunnelResourceID, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("a successful mint must not read the resource")
		w.WriteHeader(http.StatusInternalServerError)
	})
	h := newAdminTestHandler(t, ts)
	h.cfg.LinkOrigin = testLinkOrigin

	_, _, async := newAdminSlashInvoker(t, h).invokeAdminAsync("get $dashboard", testAdminTeamID, testAdminUserID)
	h.Wait()
	if !strings.Contains(async, "https://qurl.link/minted") {
		t.Fatalf("async = %q, want the minted link", async)
	}
}

// `/qurl crid` shares the entry delivery, `dm:true` sends the entry page to the
// DM, not the channel, and the answer's log line records who asked and why.
func TestHandleCRID_ConsumerAccessEntryGoesToDM(t *testing.T) {
	entry := testLinkOrigin + "/" + testTunnelCRID
	ts := newAdminTestServers(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "tunnel", []string{testTunnelResourceID})
	ts.addCustomer(http.MethodPost, mintByTestTunnelPath, func(w http.ResponseWriter, _ *http.Request) {
		writeAPIError(t, w, http.StatusForbidden, errCodeConsumerAccessRequired, "refused")
	})
	ts.addCustomer(http.MethodGet, "/v1/resources/"+testTunnelResourceID, func(w http.ResponseWriter, _ *http.Request) {
		writeConsumerAccessResource(t, w, testTunnelResourceID, testTunnelCRID, 1)
	})
	h := newAdminTestHandler(t, ts)
	h.cfg.LinkOrigin = testLinkOrigin
	var dmText string
	h.cfg.PostDMBlocks = func(_ context.Context, _, _, _ string, _ []any, fallbackText string) error {
		dmText = fallbackText
		return nil
	}

	logs := captureDefaultSlog(t)
	t.Cleanup(h.Wait) // Runs before the logger is restored.

	_, _, async := newAdminSlashInvoker(t, h).invokeAdminAsync(`crid `+testTunnelCRID+` dm:true reason:"quarterly review"`, testAdminTeamID, testAdminUserID)
	h.Wait()
	// No mint audit is written, so the answer's log line carries who asked and why.
	if !logs.contains("answering with the CRID entry page") || !logs.contains(`"reason":"quarterly review"`) || !logs.contains(`"user_id":"`+testAdminUserID+`"`) {
		t.Errorf("entry log missing the user or reason")
	}
	if !strings.Contains(dmText, entry) {
		t.Errorf("DM text = %q, want the entry page %q", dmText, entry)
	}
	if !strings.Contains(async, ":incoming_envelope:") || strings.Contains(async, entry) {
		t.Errorf("async = %q, want the DM confirmation without the entry page", async)
	}
}

// The agent's confirmed get delivers the entry page privately, in the thread
// the user approved in, the same way it delivers a minted link.
func TestConfirm_GetConsumerAccessDeliversEntryPrivately(t *testing.T) {
	entry := testLinkOrigin + "/" + testTunnelCRID
	names := defaultTestTableNames()
	hc := newConfirmHarnessWithSeed(t, "Uadmin", map[string][]map[string]ddbtypes.AttributeValue{
		names.channelPolicy: {seedChannelPolicySet("T1", "D1", "dashboard", []string{testTunnelResourceID})},
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/resources/"+testTunnelResourceID+"/qurls":
			writeAPIError(t, w, http.StatusForbidden, errCodeConsumerAccessRequired, "refused")
		case r.Method == http.MethodGet && r.URL.Path == "/v1/resources/"+testTunnelResourceID:
			writeConsumerAccessResource(t, w, testTunnelResourceID, testTunnelCRID, 1)
		default:
			t.Errorf("unexpected qURL API call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	hc.h.cfg.NewClient = func(apiKey string) *client.Client { return client.New(srv.URL, apiKey, client.WithRetry(0)) }
	hc.h.cfg.LinkOrigin = testLinkOrigin

	id := hc.seedPending(t, &pendingAction{Action: agent.ActionGet, Token: "dashboard", Asker: testAskerUserID, ChannelID: "D1", ThreadTS: testConfirmThreadTS})
	hc.h.processAgentConfirm(context.Background(), slog.Default(), confirmPayload("T1", "D1", testAskerUserID, hc.respURL, id), id, true, time.Now())
	hc.h.Wait()

	if len(hc.blocks.calls) != 1 {
		t.Fatalf("want one private delivery, got %d: %+v", len(hc.blocks.calls), hc.blocks.calls)
	}
	if got, _ := enterPortalButton(t, hc.blocks.calls[0].blocks)["url"].(string); got != entry {
		t.Fatalf("Enter Portal url = %q, want the entry page %q", got, entry)
	}
	_, card := parseResponse(t, hc.waitForN(t, 1)[0])
	if !strings.Contains(card, agentConfirmGetDeliveredReply) || strings.Contains(card, entry) {
		t.Fatalf("public card = %q, want neutral delivery success without the entry page", card)
	}
}

// writeConsumerAccessResource answers GET /v1/resources/{id} for a resource
// with the given CRID (omitted when empty) whose consumer access policy is at
// version.
func writeConsumerAccessResource(t *testing.T, w http.ResponseWriter, resourceID, cridValue string, version int64) {
	t.Helper()
	resource := map[string]any{
		testKeyResourceID:         resourceID,
		testKeyType:               client.ResourceTypeTunnel,
		testKeyStatus:             client.StatusActive,
		"consumer_access_version": version,
	}
	if cridValue != "" {
		resource["crid"] = cridValue
	}
	respondQURLEnvelope(t, w, map[string]any{"resource": resource})
}

// The entry render's fallback names each step, and the unlock button comes
// before Enter Portal, which carries the entry page.
func TestRenderConsumerAccessEntry(t *testing.T) {
	entry := testLinkOrigin + "/" + testTunnelCRID
	fallback, _ := renderConsumerAccessEntry(entry, "")
	if want := ":lock: Verified access required: " + entry + " (get access there)"; fallback != want {
		t.Fatalf("fallback = %q, want %q", fallback, want)
	}

	fallback, blocks := renderConsumerAccessEntry(entry, testLinkSiteAccessURL)
	if want := ":lock: Verified access required. First open " + testLinkSiteAccessURL + ", then " + entry + " (get access there)"; fallback != want {
		t.Fatalf("fallback = %q, want %q", fallback, want)
	}
	actions, ok := blocks[1].(map[string]any)
	if !ok {
		t.Fatalf("second block = %+v, want the actions block", blocks[1])
	}
	buttons, _ := actions[blockKitFieldElements].([]any)
	if len(buttons) != 2 {
		t.Fatalf("buttons = %+v, want unlock then Enter Portal", buttons)
	}
	unlock, _ := buttons[0].(map[string]any)
	portal, _ := buttons[1].(map[string]any)
	if unlock["url"] != testLinkSiteAccessURL || unlock["action_id"] != linkSiteAccessActionID || portal["url"] != entry || portal["action_id"] != enterPortalActionID {
		t.Fatalf("buttons = %+v, want the link-site unlock then Enter Portal on %q", buttons, entry)
	}
}

// The refusal names qurl-service's request id, so support can trace it.
func TestMapMintErrorConsumerAccessCarriesReference(t *testing.T) {
	err := mapMintError(context.Background(), slog.Default(), &client.APIError{StatusCode: http.StatusForbidden, Code: errCodeConsumerAccessRequired, RequestID: "req_ca"})
	var ue *userError
	if !errors.As(err, &ue) || ue.msg != consumerAccessRequiredMessage("req_ca") || !strings.Contains(ue.msg, "`req_ca`") {
		t.Fatalf("mapMintError = %v, want the refusal with reference req_ca", err)
	}
}
