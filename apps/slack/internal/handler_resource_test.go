package internal

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/layervai/qurl-integrations/shared/client"
)

const (
	testResourceExposeAlias        = "docs"
	testResourceExposeChannelAlias = "handbook"
	testResourceExposeID           = "r_url_docs"
	testResourceExposeExistingID   = "r_existing"
	testResourceExposeURL          = "https://docs.example.com/handbook"
)

func TestParseResourceExposeArgs(t *testing.T) {
	tests := []struct {
		name              string
		text              string
		wantResourceAlias string
		wantTargetURL     string
		wantChannelAlias  string
		wantMsg           string
	}{
		{
			name:              "resource alias defaults channel alias",
			text:              "expose $docs",
			wantResourceAlias: testResourceExposeAlias,
			wantChannelAlias:  testResourceExposeAlias,
		},
		{
			name:              "resource alias with channel alias",
			text:              "expose $docs as:$handbook",
			wantResourceAlias: testResourceExposeAlias,
			wantChannelAlias:  testResourceExposeChannelAlias,
		},
		{
			name:             "url target requires channel alias",
			text:             "expose url:" + testResourceExposeURL + " as:$handbook",
			wantTargetURL:    testResourceExposeURL,
			wantChannelAlias: testResourceExposeChannelAlias,
		},
		{
			name:    "bare resource",
			text:    "resource",
			wantMsg: resourceExposeUsage,
		},
		{
			name:    "missing as alias for url",
			text:    "expose url:" + testResourceExposeURL,
			wantMsg: "requires `as:$channel-alias`",
		},
		{
			name:    "url target must be web URL",
			text:    "expose url:ftp://docs.example.com/handbook as:$handbook",
			wantMsg: "absolute http or https URL",
		},
		{
			name:    "resource id rejected",
			text:    "expose r_opaque as:$docs",
			wantMsg: "Target must be a resource alias",
		},
		{
			name:    "bad as option",
			text:    "expose $docs alias:$handbook",
			wantMsg: "Usage:",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, msg := parseResourceExposeArgs(tc.text)
			if tc.wantMsg != "" {
				if !strings.Contains(msg, tc.wantMsg) {
					t.Fatalf("message = %q, want substring %q", msg, tc.wantMsg)
				}
				return
			}
			if msg != "" {
				t.Fatalf("unexpected parse message: %q", msg)
			}
			if got.ResourceAlias != tc.wantResourceAlias || got.TargetURL != tc.wantTargetURL || got.ChannelAlias != tc.wantChannelAlias {
				t.Fatalf("args = %+v, want resource_alias=%q target_url=%q channel_alias=%q", got, tc.wantResourceAlias, tc.wantTargetURL, tc.wantChannelAlias)
			}
		})
	}
}

func TestHandleResourceExpose_BindsURLResourceAlias(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	inv := newAdminSlashInvoker(t, h)

	ts.addCustomer(http.MethodGet, "/v1/resources", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("limit"); got != "100" {
			t.Errorf("limit = %q, want 100", got)
		}
		writeResourceListFixture(t, w, []map[string]any{{
			testKeyResourceID: testResourceExposeID,
			testKeyType:       client.ResourceTypeURL,
			fAttrAlias:        testResourceExposeAlias,
			testKeyTargetURL:  testResourceExposeURL,
			testKeyStatus:     client.StatusActive,
		}}, "", false)
	})

	status, ack, async := inv.invokeAdminAsync("resource expose $"+testResourceExposeAlias, testAdminTeamID, testAdminUserID)
	if status != http.StatusOK || !strings.Contains(ack, "Working on it") {
		t.Fatalf("sync = (%d, %q), want async ack", status, ack)
	}
	if !strings.Contains(async, "URL resource `$docs` is now available as `$docs`") ||
		!strings.Contains(async, "/qurl get $docs") {
		t.Fatalf("async reply = %q", async)
	}
	got, found, err := h.cfg.AdminStore.LookupChannelAlias(context.Background(), testAdminTeamID, "C_test", testResourceExposeAlias)
	if err != nil {
		t.Fatalf("LookupChannelAlias: %v", err)
	}
	if !found || got != testResourceExposeID {
		t.Fatalf("channel alias = (%q, %v), want (%q, true)", got, found, testResourceExposeID)
	}
}

func TestHandleResourceExpose_BindsNoAliasURLResourceByTargetURL(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	inv := newAdminSlashInvoker(t, h)

	ts.addCustomer(http.MethodGet, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{{
			testKeyResourceID: testResourceExposeID,
			testKeyType:       client.ResourceTypeURL,
			testKeyTargetURL:  testResourceExposeURL,
			testKeyStatus:     client.StatusActive,
		}}, "", false)
	})

	_, _, async := inv.invokeAdminAsync("resource expose url:"+testResourceExposeURL+" as:$"+testResourceExposeChannelAlias, testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "URL resource is now available as `$handbook`") {
		t.Fatalf("async reply = %q", async)
	}
	got, found, err := h.cfg.AdminStore.LookupChannelAlias(context.Background(), testAdminTeamID, "C_test", testResourceExposeChannelAlias)
	if err != nil {
		t.Fatalf("LookupChannelAlias: %v", err)
	}
	if !found || got != testResourceExposeID {
		t.Fatalf("channel alias = (%q, %v), want (%q, true)", got, found, testResourceExposeID)
	}
}

func TestHandleResourceExpose_NonAdminDoesNotLookupOrBind(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedNonAdmin(t)
	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	inv := newAdminSlashInvoker(t, h)
	ts.failOnAdminMutation(t, "non-admin resource expose should not bind")
	var resourceLookups atomic.Int32
	ts.addCustomer(http.MethodGet, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		resourceLookups.Add(1)
		writeResourceListFixture(t, w, nil, "", false)
	})

	_, reply := inv.invokeAdmin("resource expose $"+testResourceExposeAlias, testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, "admin-only") {
		t.Fatalf("reply = %q, want admin denial", reply)
	}
	if resourceLookups.Load() != 0 {
		t.Fatalf("resource lookup reached for non-admin")
	}
}

func TestHandleResourceExpose_DuplicateChannelAliasRefusesOverwrite(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.seedPolicyAliasBindings(t, testAdminTeamID, "C_test", map[string]string{testResourceExposeAlias: testResourceExposeExistingID})
	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	inv := newAdminSlashInvoker(t, h)

	ts.addCustomer(http.MethodGet, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{{
			testKeyResourceID: testResourceExposeID,
			testKeyType:       client.ResourceTypeURL,
			fAttrAlias:        testResourceExposeAlias,
			testKeyTargetURL:  testResourceExposeURL,
			testKeyStatus:     client.StatusActive,
		}}, "", false)
	})

	_, _, async := inv.invokeAdminAsync("resource expose $"+testResourceExposeAlias, testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "Alias `$docs` is already bound in this channel") {
		t.Fatalf("async reply = %q", async)
	}
	got, found, err := h.cfg.AdminStore.LookupChannelAlias(context.Background(), testAdminTeamID, "C_test", testResourceExposeAlias)
	if err != nil {
		t.Fatalf("LookupChannelAlias: %v", err)
	}
	if !found || got != testResourceExposeExistingID {
		t.Fatalf("channel alias = (%q, %v), want existing binding", got, found)
	}
}

func TestHandleResourceExpose_IgnoresTunnelAlias(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	inv := newAdminSlashInvoker(t, h)

	ts.addCustomer(http.MethodGet, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{{
			testKeyResourceID: "r_tunnel_docs",
			testKeyType:       client.ResourceTypeTunnel,
			testKeySlug:       "docs-tunnel",
			fAttrAlias:        testResourceExposeAlias,
			testKeyStatus:     client.StatusActive,
		}}, "", false)
	})

	_, _, async := inv.invokeAdminAsync("resource expose $"+testResourceExposeAlias, testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "No active URL resource `$docs` was found") {
		t.Fatalf("async reply = %q", async)
	}
	if _, found, err := h.cfg.AdminStore.LookupChannelAlias(context.Background(), testAdminTeamID, "C_test", testResourceExposeAlias); err != nil {
		t.Fatalf("LookupChannelAlias: %v", err)
	} else if found {
		t.Fatalf("tunnel alias should not have been exposed as a URL resource")
	}
}

func TestHandleResourceExpose_TargetURLNotFoundMentionsExactMatch(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	inv := newAdminSlashInvoker(t, h)

	ts.addCustomer(http.MethodGet, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{{
			testKeyResourceID: testResourceExposeID,
			testKeyType:       client.ResourceTypeURL,
			testKeyTargetURL:  testResourceExposeURL + "/",
			testKeyStatus:     client.StatusActive,
		}}, "", false)
	})

	_, _, async := inv.invokeAdminAsync("resource expose url:"+testResourceExposeURL+" as:$"+testResourceExposeChannelAlias, testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "exact target URL") || !strings.Contains(async, "match the dashboard URL exactly") {
		t.Fatalf("async reply = %q", async)
	}
	if _, found, err := h.cfg.AdminStore.LookupChannelAlias(context.Background(), testAdminTeamID, "C_test", testResourceExposeChannelAlias); err != nil {
		t.Fatalf("LookupChannelAlias: %v", err)
	} else if found {
		t.Fatalf("near-match URL should not have been exposed")
	}
}
