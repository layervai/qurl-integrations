package internal

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

const (
	testLinkOrigin        = "https://link.example"
	testLinkSiteAccessURL = "https://link.example/unlock"
)

// writeGetResourceFixture writes a GET /v1/resources/{id} envelope for a
// resource with a consumer access policy at the given revision.
func writeGetResourceFixture(t *testing.T, w http.ResponseWriter, resourceID, cridValue string, consumerAccessVersion int64) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	body := map[string]any{testKeyData: map[string]any{"resource": map[string]any{
		testKeyResourceID:         resourceID,
		"type":                    "tunnel",
		"status":                  "active",
		"crid":                    cridValue,
		"consumer_access_version": consumerAccessVersion,
	}}}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Fatalf("encode: %v", err)
	}
}

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
		cridValue         string
		version           int64
		linkOrigin        string
		linkSiteAccessURL string
		want              []string
		wantAbsent        []string
	}{
		{name: "403 consumer_access_required", resourceID: testTunnelResourceID, mintStatus: http.StatusForbidden, mintCode: errCodeConsumerAccessRequired, cridValue: testTunnelCRID, version: 1, linkOrigin: testLinkOrigin,
			want: []string{"Verified access required", entry}, wantAbsent: []string{"Failed to create qURL"}},
		{name: "legacy 500 creation_failed", resourceID: testTunnelResourceID, mintStatus: http.StatusInternalServerError, mintCode: errCodeCreationFailed, cridValue: testTunnelCRID, version: 3, linkOrigin: testLinkOrigin,
			want: []string{"Verified access required", entry}},
		{name: "private link site gets the unlock step first", resourceID: testTunnelResourceID, mintStatus: http.StatusForbidden, mintCode: errCodeConsumerAccessRequired, cridValue: testTunnelCRID, version: 1, linkOrigin: testLinkOrigin, linkSiteAccessURL: testLinkSiteAccessURL,
			want: []string{"First open " + testLinkSiteAccessURL + ", then " + entry}},
		{name: "500 without a policy is still a service failure", resourceID: testTunnelResourceID, mintStatus: http.StatusInternalServerError, mintCode: errCodeCreationFailed, cridValue: testTunnelCRID, version: 0, linkOrigin: testLinkOrigin,
			want: []string{serviceUnreachableMessage}, wantAbsent: []string{entry}},
		{name: "a CRID that is not the resource's is never delivered", resourceID: testResourceIDFix, mintStatus: http.StatusForbidden, mintCode: errCodeConsumerAccessRequired, cridValue: testTunnelCRID, version: 1, linkOrigin: testLinkOrigin,
			want: []string{consumerAccessRequiredMessage}, wantAbsent: []string{testTunnelCRID}},
		{name: "no link origin answers the refusal", resourceID: testTunnelResourceID, mintStatus: http.StatusForbidden, mintCode: errCodeConsumerAccessRequired, cridValue: testTunnelCRID, version: 1,
			want: []string{consumerAccessRequiredMessage}, wantAbsent: []string{testTunnelCRID}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := newAdminTestServers(t)
			ts.seedPolicySet(t, testAdminTeamID, "C_test", "dashboard", []string{tc.resourceID})
			ts.addCustomer(http.MethodPost, "/v1/resources/"+tc.resourceID+"/qurls", func(w http.ResponseWriter, _ *http.Request) {
				writeAPIError(t, w, tc.mintStatus, tc.mintCode, "refused")
			})
			ts.addCustomer(http.MethodGet, "/v1/resources/"+tc.resourceID, func(w http.ResponseWriter, _ *http.Request) {
				writeGetResourceFixture(t, w, tc.resourceID, tc.cridValue, tc.version)
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

// The entry render puts the unlock button before Enter Portal, and Enter
// Portal carries the entry page.
func TestRenderConsumerAccessEntry(t *testing.T) {
	entry := testLinkOrigin + "/" + testTunnelCRID
	_, blocks := renderConsumerAccessEntry(entry, "")
	if got := enterPortalButton(t, blocks)["url"]; got != entry {
		t.Fatalf("Enter Portal url = %v, want %q", got, entry)
	}

	_, blocks = renderConsumerAccessEntry(entry, testLinkSiteAccessURL)
	first := enterPortalButton(t, blocks)
	if first["url"] != testLinkSiteAccessURL || first["action_id"] != linkSiteAccessActionID {
		t.Fatalf("first button = %+v, want the link-site unlock", first)
	}
}
