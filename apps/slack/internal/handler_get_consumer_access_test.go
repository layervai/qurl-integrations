package internal

import (
	"context"
	"net/http"
	"strings"
	"testing"

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
		name              string
		resourceID        string
		mintStatus        int
		mintCode          string
		version           int64
		readStatus        int
		linkOrigin        string
		linkSiteAccessURL string
		want              []string
		wantAbsent        []string
	}{
		{name: "403 consumer_access_required", resourceID: testTunnelResourceID, mintStatus: http.StatusForbidden, mintCode: errCodeConsumerAccessRequired, version: 1, linkOrigin: testLinkOrigin,
			want: []string{"Verified access required", entry}, wantAbsent: []string{"Failed to create qURL"}},
		{name: "legacy 500 creation_failed", resourceID: testTunnelResourceID, mintStatus: http.StatusInternalServerError, mintCode: errCodeCreationFailed, version: 3, linkOrigin: testLinkOrigin,
			want: []string{"Verified access required", entry}},
		{name: "private link site gets the unlock step first", resourceID: testTunnelResourceID, mintStatus: http.StatusForbidden, mintCode: errCodeConsumerAccessRequired, version: 1, linkOrigin: testLinkOrigin, linkSiteAccessURL: testLinkSiteAccessURL,
			want: []string{"First open " + testLinkSiteAccessURL + ", then " + entry}},
		{name: "500 without a policy is still a service failure", resourceID: testTunnelResourceID, mintStatus: http.StatusInternalServerError, mintCode: errCodeCreationFailed, version: 0, linkOrigin: testLinkOrigin,
			want: []string{serviceUnreachableMessage}, wantAbsent: []string{entry}},
		{name: "a CRID that is not the resource's is never delivered", resourceID: testResourceIDFix, mintStatus: http.StatusForbidden, mintCode: errCodeConsumerAccessRequired, version: 1, linkOrigin: testLinkOrigin,
			want: []string{consumerAccessRequiredMessage}, wantAbsent: []string{testTunnelCRID}},
		{name: "a failed resource read answers the refusal", resourceID: testTunnelResourceID, mintStatus: http.StatusForbidden, mintCode: errCodeConsumerAccessRequired, version: 1, readStatus: http.StatusNotFound, linkOrigin: testLinkOrigin,
			want: []string{consumerAccessRequiredMessage}, wantAbsent: []string{testTunnelCRID}},
		{name: "no link origin answers the refusal", resourceID: testTunnelResourceID, mintStatus: http.StatusForbidden, mintCode: errCodeConsumerAccessRequired, version: 1,
			want: []string{consumerAccessRequiredMessage}, wantAbsent: []string{testTunnelCRID}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := newAdminTestServers(t)
			ts.seedPolicySet(t, testAdminTeamID, "C_test", "dashboard", []string{tc.resourceID})
			ts.addCustomer(http.MethodPost, "/v1/resources/"+tc.resourceID+"/qurls", func(w http.ResponseWriter, _ *http.Request) {
				writeAPIError(t, w, tc.mintStatus, tc.mintCode, "refused")
			})
			ts.addCustomer(http.MethodGet, "/v1/resources/"+tc.resourceID, func(w http.ResponseWriter, _ *http.Request) {
				if tc.readStatus != 0 {
					writeAPIError(t, w, tc.readStatus, "not_found", "gone")
					return
				}
				writeConsumerAccessResource(t, w, tc.resourceID, tc.version)
			})
			h := newAdminTestHandler(t, ts)
			h.cfg.LinkOrigin = tc.linkOrigin
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
		writeConsumerAccessResource(t, w, testTunnelResourceID, 1)
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

// writeConsumerAccessResource answers GET /v1/resources/{id} for a resource
// whose CRID is testTunnelCRID and whose consumer access policy is at version.
func writeConsumerAccessResource(t *testing.T, w http.ResponseWriter, resourceID string, version int64) {
	t.Helper()
	respondQURLEnvelope(t, w, map[string]any{"resource": map[string]any{
		testKeyResourceID:         resourceID,
		testKeyType:               client.ResourceTypeTunnel,
		testKeyStatus:             client.StatusActive,
		"crid":                    testTunnelCRID,
		"consumer_access_version": version,
	}})
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
