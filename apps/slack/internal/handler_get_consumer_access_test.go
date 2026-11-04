package internal

import (
	"context"
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
		// noCRID reads the resource without a crid.
		noCRID            bool
		version           int64
		readStatus        int
		noLinkOrigin      bool
		linkSiteAccessURL string
		want              []string
		wantAbsent        []string
	}{
		{name: "403 consumer_access_required", version: 1,
			want: []string{"Verified access required", entry}, wantAbsent: []string{commonGetMintFailedMessage}},
		{name: "legacy 500 creation_failed", legacy: true, version: 3,
			want: []string{"Verified access required", entry}},
		{name: "private link site gets the unlock step first", version: 1, linkSiteAccessURL: testLinkSiteAccessURL,
			want: []string{"First open " + testLinkSiteAccessURL + ", then " + entry}},
		{name: "500 without a policy is still a service failure", legacy: true,
			want: []string{serviceUnreachableMessage}, wantAbsent: []string{entry}},
		{name: "a CRID that is not the resource's is never delivered", foreign: true, version: 1,
			want: []string{consumerAccessRequiredMessage}, wantAbsent: []string{testTunnelCRID}},
		{name: "a 403 for a resource read without a policy answers the refusal",
			want: []string{consumerAccessRequiredMessage}, wantAbsent: []string{testTunnelCRID}},
		{name: "an id that is not base64url answers the refusal without a read", notKey: true, version: 1,
			want: []string{consumerAccessRequiredMessage}, wantAbsent: []string{testTunnelCRID}},
		{name: "a resource read without a CRID answers the refusal", version: 1, noCRID: true,
			want: []string{consumerAccessRequiredMessage}, wantAbsent: []string{testLinkOrigin + "/"}},
		{name: "a legacy 500 with a failed read is still a service failure", legacy: true, version: 1, readStatus: http.StatusNotFound,
			want: []string{serviceUnreachableMessage}, wantAbsent: []string{consumerAccessRequiredMessage}},
		{name: "a failed resource read answers the refusal", version: 1, readStatus: http.StatusNotFound,
			want: []string{consumerAccessRequiredMessage}, wantAbsent: []string{testTunnelCRID}},
		{name: "no link origin answers the refusal", version: 1, noLinkOrigin: true,
			want: []string{consumerAccessRequiredMessage}, wantAbsent: []string{testTunnelCRID}},
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
			ts := newAdminTestServers(t)
			ts.seedPolicySet(t, testAdminTeamID, "C_test", "dashboard", []string{resourceID})
			ts.addCustomer(http.MethodPost, "/v1/resources/"+resourceID+"/qurls", func(w http.ResponseWriter, _ *http.Request) {
				writeAPIError(t, w, mintStatus, mintCode, "refused")
			})
			ts.addCustomer(http.MethodGet, "/v1/resources/"+resourceID, func(w http.ResponseWriter, _ *http.Request) {
				if tc.notKey {
					t.Error("an id that is not base64url must not be read")
				}
				if tc.readStatus != 0 {
					writeAPIError(t, w, tc.readStatus, "not_found", "gone")
					return
				}
				cridValue := testTunnelCRID
				if tc.noCRID {
					cridValue = ""
				}
				writeConsumerAccessResource(t, w, resourceID, cridValue, tc.version)
			})
			h := newAdminTestHandler(t, ts)
			if !tc.noLinkOrigin {
				h.cfg.LinkOrigin = testLinkOrigin
			}
			h.cfg.LinkSiteAccessURL = tc.linkSiteAccessURL

			_, _, async := newAdminSlashInvoker(t, h).invokeAdminAsync("get $dashboard", testAdminTeamID, testAdminUserID)
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

// `/qurl crid` shares the entry delivery, and `dm:true` sends the entry page to
// the DM, not the channel.
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

	_, _, async := newAdminSlashInvoker(t, h).invokeAdminAsync("crid "+testTunnelCRID+" dm:true", testAdminTeamID, testAdminUserID)
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

// The entry render puts the unlock button before Enter Portal, and Enter
// Portal carries the entry page.
func TestRenderConsumerAccessEntry(t *testing.T) {
	entry := testLinkOrigin + "/" + testTunnelCRID
	_, blocks := renderConsumerAccessEntry(entry, "")
	if got := enterPortalButton(t, blocks)["url"]; got != entry {
		t.Fatalf("Enter Portal url = %v, want %q", got, entry)
	}

	_, blocks = renderConsumerAccessEntry(entry, testLinkSiteAccessURL)
	buttons, _ := blocks[1].(map[string]any)[blockKitFieldElements].([]any)
	if len(buttons) != 2 {
		t.Fatalf("buttons = %+v, want unlock then Enter Portal", buttons)
	}
	unlock, _ := buttons[0].(map[string]any)
	portal, _ := buttons[1].(map[string]any)
	if unlock["url"] != testLinkSiteAccessURL || unlock["action_id"] != linkSiteAccessActionID || portal["url"] != entry || portal["action_id"] != enterPortalActionID {
		t.Fatalf("buttons = %+v, want the link-site unlock then Enter Portal on %q", buttons, entry)
	}
}
