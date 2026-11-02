package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
	"github.com/layervai/qurl-integrations/shared/auth"
	"github.com/layervai/qurl-integrations/shared/client"
)

func TestResolveChannelTokenPolicy(t *testing.T) {
	tunnel := func(id, slug, status string) client.Resource {
		return client.Resource{ResourceID: id, Slug: slug, Type: client.ResourceTypeTunnel, Status: status}
	}
	url := func(id, alias string) client.Resource {
		return client.Resource{ResourceID: id, Alias: alias, Type: client.ResourceTypeURL}
	}
	for _, tc := range []struct {
		name            string
		binding         string
		allowed         []string
		slugs, aliases  []client.Resource
		wantID, wantVia string
		wantErr         error
		wantCalls       []string
	}{
		{name: "bound alias wins without metadata or API", binding: "bound", wantID: "bound", wantVia: channelTokenAlias},
		{name: "legacy binding refuses before fallback", binding: "https://legacy.example.test", wantErr: errChannelTokenLegacy},
		{name: "empty channel spends no upstream call"},
		{name: "slug wins before resource alias", allowed: []string{"a", "b"}, slugs: []client.Resource{tunnel("a", "token", client.StatusActive)}, aliases: []client.Resource{url("b", "token")}, wantID: "a", wantVia: channelTokenSlug, wantCalls: []string{channelTokenSlug}},
		{name: "wrong type does not make a slug", allowed: []string{"a"}, slugs: []client.Resource{{ResourceID: "a", Slug: "token", Type: client.ResourceTypeURL}}, wantCalls: []string{channelTokenSlug, channelTokenResourceAlias}},
		{name: "wrong slug refuses", allowed: []string{"a"}, slugs: []client.Resource{tunnel("a", "different", client.StatusActive)}, wantCalls: []string{channelTokenSlug, channelTokenResourceAlias}},
		{name: "revoked slug refuses", allowed: []string{"a"}, slugs: []client.Resource{tunnel("a", "token", client.StatusRevoked)}, wantCalls: []string{channelTokenSlug, channelTokenResourceAlias}},
		{name: "out of channel slug is not a match", allowed: []string{"a"}, slugs: []client.Resource{tunnel("secret", "token", client.StatusActive)}, wantCalls: []string{channelTokenSlug, channelTokenResourceAlias}},
		{name: "out of channel slug permits in channel alias", allowed: []string{"a"}, slugs: []client.Resource{tunnel("secret", "token", client.StatusActive)}, aliases: []client.Resource{url("a", "token")}, wantID: "a", wantVia: channelTokenResourceAlias, wantCalls: []string{channelTokenSlug, channelTokenResourceAlias}},
		{name: "only authorized slug counts", allowed: []string{"a"}, slugs: []client.Resource{tunnel("secret", "token", client.StatusActive), tunnel("a", "token", client.StatusActive)}, wantID: "a", wantVia: channelTokenSlug, wantCalls: []string{channelTokenSlug}},
		{name: "ambiguous slugs refuse", allowed: []string{"a", "b"}, slugs: []client.Resource{tunnel("a", "token", client.StatusActive), tunnel("b", "token", client.StatusActive)}, wantErr: errChannelTokenAmbiguous, wantCalls: []string{channelTokenSlug}},
		{name: "URL resource alias", allowed: []string{"a"}, aliases: []client.Resource{url("a", "token")}, wantID: "a", wantVia: channelTokenResourceAlias, wantCalls: []string{channelTokenSlug, channelTokenResourceAlias}},
		{name: "legacy URL metadata remains supported", allowed: []string{"a"}, aliases: []client.Resource{{ResourceID: "a", Alias: "token", TargetURL: "https://example.test"}}, wantID: "a", wantVia: channelTokenResourceAlias, wantCalls: []string{channelTokenSlug, channelTokenResourceAlias}},
		{name: "revoked alias refuses", allowed: []string{"a"}, aliases: []client.Resource{{ResourceID: "a", Alias: "token", Type: client.ResourceTypeURL, Status: client.StatusRevoked}}, wantCalls: []string{channelTokenSlug, channelTokenResourceAlias}},
		{name: "out of channel alias refuses", allowed: []string{"a"}, aliases: []client.Resource{url("secret", "token")}, wantCalls: []string{channelTokenSlug, channelTokenResourceAlias}},
		{name: "ambiguous URL aliases refuse", allowed: []string{"a", "b"}, aliases: []client.Resource{url("a", "token"), url("b", "token")}, wantErr: errChannelTokenAmbiguous, wantCalls: []string{channelTokenSlug, channelTokenResourceAlias}},
		{name: "duplicate rows are not ambiguous", allowed: []string{"a"}, aliases: []client.Resource{url("a", "token"), url("a", "token")}, wantID: "a", wantVia: channelTokenResourceAlias, wantCalls: []string{channelTokenSlug, channelTokenResourceAlias}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			scopeCalls := 0
			got, err := resolveChannelToken("token", channelTokenSource{
				alias: func() (*channelTokenResource, error) {
					if tc.binding == "" {
						return nil, errChannelTokenNotFound
					}
					return &channelTokenResource{ResourceID: tc.binding}, nil
				},
				allowed: func() (map[string]struct{}, error) {
					scopeCalls++
					out := map[string]struct{}{}
					for _, id := range tc.allowed {
						out[id] = struct{}{}
					}
					return out, nil
				},
				candidates: func(via string, _ map[string]struct{}) ([]client.Resource, error) {
					calls = append(calls, via)
					if via == channelTokenSlug {
						return tc.slugs, nil
					}
					return tc.aliases, nil
				},
			})
			wantErr := tc.wantErr
			if tc.wantID == "" && wantErr == nil {
				wantErr = errChannelTokenNotFound
			}
			if !errors.Is(err, wantErr) {
				t.Fatalf("error=%v want=%v", err, wantErr)
			}
			if tc.wantID == "" {
				if got != nil {
					t.Fatalf("unexpected resolution: %+v", got)
				}
			} else if got == nil || got.ResourceID != tc.wantID || got.Via != tc.wantVia {
				t.Fatalf("resolution=%+v, want id=%s via=%s", got, tc.wantID, tc.wantVia)
			}
			if !reflect.DeepEqual(calls, tc.wantCalls) {
				t.Fatalf("upstream calls=%v want=%v", calls, tc.wantCalls)
			}
			if tc.binding != "" && scopeCalls != 0 {
				t.Fatalf("bound alias read an unnecessary allow-set")
			}
		})
	}
}

func TestResolveChannelTokenFailuresDoNotFallThrough(t *testing.T) {
	cause := errors.New("lookup unavailable")
	for _, stage := range []string{"binding", "scope", channelTokenSlug, channelTokenResourceAlias} {
		t.Run(stage, func(t *testing.T) {
			calls := 0
			got, err := resolveChannelToken("token", channelTokenSource{
				alias: func() (*channelTokenResource, error) {
					if stage == "binding" {
						return nil, cause
					}
					return nil, errChannelTokenNotFound
				},
				allowed: func() (map[string]struct{}, error) {
					if stage == "scope" {
						return nil, cause
					}
					return map[string]struct{}{"a": {}}, nil
				},
				candidates: func(via string, _ map[string]struct{}) ([]client.Resource, error) {
					calls++
					if via == stage {
						return nil, cause
					}
					return []client.Resource{}, nil
				},
			})
			if got != nil || !errors.Is(err, cause) {
				t.Fatalf("lookup failure was hidden: result=%+v err=%v", got, err)
			}
			maxCalls := 0
			if stage == channelTokenSlug {
				maxCalls = 1
			}
			if stage == channelTokenResourceAlias {
				maxCalls = 2
			}
			if calls != maxCalls {
				t.Fatalf("lookup fell through after failure: calls=%d want=%d", calls, maxCalls)
			}
		})
	}
}

func TestAgentResolveUsesSharedLegacyAndResourceAliasPolicy(t *testing.T) {
	t.Run("legacy binding refuses without a client", func(t *testing.T) {
		b, store := newBackendUnderTest(t, false)
		if err := store.BindChannelAlias(context.Background(), "T1", "C1", "legacy", "https://legacy.example.test"); err != nil {
			t.Fatal(err)
		}
		got, err := b.ResolveToken(context.Background(), backendTC(), "$legacy")
		if err != nil || got != legacyAliasBindingMessage("legacy") {
			t.Fatalf("legacy guidance=%q err=%v", got, err)
		}
	})
	t.Run("resource alias works without channel alias binding", func(t *testing.T) {
		b, store := newBackendUnderTest(t, true)
		if err := store.UnbindChannelAlias(context.Background(), "T1", "C1", "oncall"); err != nil {
			t.Fatal(err)
		}
		got, err := b.ResolveToken(context.Background(), backendTC(), "$oncall")
		if err != nil || !strings.Contains(got, "protected resource") || strings.Contains(got, "r_1") {
			t.Fatalf("resource alias result=%q err=%v", got, err)
		}
	})
}

func TestAgentTokenAliasLookupHasOnePageBudget(t *testing.T) {
	for _, complete := range []bool{false, true} {
		t.Run(strconv.FormatBool(complete), func(t *testing.T) {
			b, store := newBackendUnderTest(t, false)
			if complete {
				if err := store.BindChannelAlias(context.Background(), "T1", "C1", "legacy", "https://legacy.example.test"); err != nil {
					t.Fatal(err)
				}
			}
			var pages atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodPost {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				if r.URL.Query().Get("slug") != "" {
					_, _ = w.Write([]byte(`{"data":[]}`))
					return
				}
				pages.Add(1)
				if r.URL.Query().Get("cursor") != "" {
					t.Error("alias lookup paginated")
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				rows := `{"resource_id":"r_1","type":"url","alias":"report"}`
				if complete {
					rows += `,{"resource_id":"r_2","type":"tunnel","slug":"other","status":"active"}`
				}
				_, _ = w.Write([]byte(`{"data":[` + rows + `],"meta":{"has_more":true,"next_cursor":"unread"}}`))
			}))
			t.Cleanup(server.Close)
			b.authClient = func(context.Context, string) (*client.Client, error) { return client.New(server.URL, "key"), nil }
			got, err := b.ResolveToken(context.Background(), backendTC(), "report")
			if err != nil {
				t.Fatal(err)
			}
			if complete {
				if !strings.Contains(got, "protected resource") {
					t.Fatalf("complete channel page did not resolve: %q", got)
				}
			} else if got != channelTokenIncompleteMessage {
				t.Fatalf("partial channel page claimed certainty: %q", got)
			}
			if pages.Load() != 1 {
				t.Fatalf("alias reads=%d, want one", pages.Load())
			}
		})
	}
}

func TestAgentTokenClientFailureIsLoggedOnce(t *testing.T) {
	for _, cause := range []error{errors.New("credential store unavailable"), auth.ErrWorkspaceNotConfigured} {
		t.Run(cause.Error(), func(t *testing.T) {
			b, _ := newBackendUnderTest(t, false)
			var logs bytes.Buffer
			b.log = slog.New(slog.NewTextHandler(&logs, nil))
			b.authClient = func(context.Context, string) (*client.Client, error) { return nil, cause }
			got, err := b.ResolveToken(context.Background(), backendTC(), "unbound")
			count := bytes.Count(logs.Bytes(), []byte("agent backend read failed"))
			if errors.Is(cause, auth.ErrWorkspaceNotConfigured) {
				if err != nil || got != workspaceNotSetupMessage(commandUser) || count != 0 {
					t.Fatalf("setup nudge changed: message=%q err=%v logs=%d", got, err, count)
				}
			} else if !errors.Is(err, cause) || count != 1 || strings.Count(err.Error(), "resolve token:") != 1 {
				t.Fatalf("client failure duplicated or hidden: err=%v logs=%d", err, count)
			}
		})
	}
}

func TestInspectPartialSnapshotCannotMintGuessedAlias(t *testing.T) {
	for _, rows := range [][]client.Resource{nil, {{ResourceID: "r_1", Type: client.ResourceTypeURL, Alias: "report"}}} {
		t.Run(strconv.Itoa(len(rows)), func(t *testing.T) {
			b, _ := newBackendUnderTest(t, false)
			b.resourcesOnce.Do(func() { b.resources = rows; b.resourcesPartial = true })
			var batches, mints atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost && r.URL.Path == "/v1/resources/batch-get" {
					batches.Add(1)
					writeMetadataFixture(t, w, []client.Resource{{ResourceID: "r_1", Type: client.ResourceTypeURL, Alias: "report"}, {ResourceID: "r_2", Type: client.ResourceTypeURL, Alias: "report"}})
					return
				}
				mints.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
			}))
			t.Cleanup(server.Close)
			b.authClient = func(context.Context, string) (*client.Client, error) { return client.New(server.URL, "key"), nil }
			got, summarized, err := b.InspectToken(context.Background(), backendTC(), "report")
			if err != nil || summarized || !strings.Contains(got, "matches multiple resources") || mints.Load() != 0 || batches.Load() != 1 {
				t.Fatalf("partial alias guessed instead of checked: message=%q summarized=%t err=%v mints=%d metadata=%d", got, summarized, err, mints.Load(), batches.Load())
			}
		})
	}
	bound, msg, err := resolveInspectableResource("bound", []slackdata.PolicyEntry{{Alias: "bound", ResourceID: "r_bound"}}, nil, nil, nil)
	if err != nil || msg != "" || bound == nil || bound.ResourceID != "r_bound" {
		t.Fatalf("binding blocked by missing metadata: resolved=%+v message=%q", bound, msg)
	}
}

func TestAgentTokenMetadataHandlesDeletedAndLegacyBindings(t *testing.T) {
	b, store := newBackendUnderTest(t, false)
	if err := store.BindChannelAlias(context.Background(), "T1", "C1", "legacy", "https://legacy.example.test"); err != nil {
		t.Fatal(err)
	}
	var batches, catalog atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/resources/batch-get" {
			batches.Add(1)
			var input struct {
				ResourceIDs []string `json:"resource_ids"`
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if !reflect.DeepEqual(input.ResourceIDs, []string{"r_1", "r_2"}) {
				t.Errorf("metadata IDs=%v", input.ResourceIDs)
			}
			// r_2 was deleted. Its absence is authoritative, not an incomplete catalog.
			writeMetadataFixture(t, w, []client.Resource{{ResourceID: "r_1", Type: client.ResourceTypeURL, Alias: "report"}})
			return
		}
		if r.URL.Query().Get("slug") == "" {
			catalog.Add(1)
		}
		writeResourceListFixture(t, w, nil, "", false)
	}))
	t.Cleanup(server.Close)
	b.authClient = func(context.Context, string) (*client.Client, error) { return client.New(server.URL, "key"), nil }
	got, err := b.ResolveToken(context.Background(), backendTC(), "report")
	if err != nil || !strings.Contains(got, "protected resource") || batches.Load() != 1 || catalog.Load() != 0 {
		t.Fatalf("metadata did not resolve around legacy/deleted IDs: message=%q err=%v metadata=%d catalog=%d", got, err, batches.Load(), catalog.Load())
	}
}

func TestInspectPartialSnapshotKeepsKnownSlug(t *testing.T) {
	for _, observed := range []bool{true, false} {
		t.Run(strconv.FormatBool(observed), func(t *testing.T) {
			b, _ := newBackendUnderTest(t, false)
			b.resourcesOnce.Do(func() {
				if observed {
					b.resources = []client.Resource{{ResourceID: "r_2", Type: client.ResourceTypeTunnel, Status: client.StatusActive, Slug: "staging"}}
				}
				b.resourcesPartial = true
			})
			page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				_, _ = w.Write([]byte("<html><title>Staging overview</title><body>Ready</body></html>"))
			}))
			t.Cleanup(page.Close)
			var mints, other, batches atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost && r.URL.Path == "/v1/resources/batch-get" {
					batches.Add(1)
					writeMetadataFixture(t, w, []client.Resource{{ResourceID: "r_2", Type: client.ResourceTypeTunnel, Status: client.StatusActive, Slug: "staging"}})
					return
				}
				if r.Method == http.MethodPost && r.URL.Path == "/v1/resources/r_2/qurls" {
					mints.Add(1)
					respondQURLEnvelope(t, w, map[string]any{"resource_id": "r_2", "qurl_link": page.URL, "qurl_site": page.URL})
					return
				}
				other.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
			}))
			t.Cleanup(server.Close)
			b.allowInspectableLoopbackHosts = true
			b.authClient = func(context.Context, string) (*client.Client, error) { return client.New(server.URL, "key"), nil }
			got, summarized, err := b.InspectToken(context.Background(), backendTC(), "staging")
			if err != nil || !summarized || !strings.Contains(got, "Staging overview") || mints.Load() != 1 || other.Load() != 0 {
				t.Fatalf("known unique slug was blocked or rescanned: message=%q summarized=%t err=%v mints=%d other=%d", got, summarized, err, mints.Load(), other.Load())
			}
			expectedBatches := int32(1)
			if observed {
				expectedBatches = 0
			}
			if batches.Load() != expectedBatches {
				t.Fatalf("metadata reads=%d want=%d", batches.Load(), expectedBatches)
			}
		})
	}
}

func TestHandleGet_PartialResourceAliasesCannotMint(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedChannelExposure(t, testAdminTeamID, "C_test", "r_visible", "r_unseen")
	var pages, mints atomic.Int32
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("slug") != "" {
			writeResourceListFixture(t, w, nil, "", false)
			return
		}
		pages.Add(1)
		if r.URL.Query().Get("cursor") != "" {
			t.Error("alias lookup exceeded one page")
		}
		writeResourceListFixture(t, w, []map[string]any{{testKeyResourceID: "r_visible", testKeyType: client.ResourceTypeURL, fAttrAlias: "docs", testKeyStatus: client.StatusActive}}, "unread", true)
	})
	ts.addCustomerPrefix("POST", "/v1/resources/", func(w http.ResponseWriter, _ *http.Request) {
		mints.Add(1)
		writeCreateFixture(t, w, "https://qurl.link/unexpected", "r_visible")
	})
	h := newAdminTestHandler(t, ts)
	_, _, reply := newAdminSlashInvoker(t, h).invokeAdminAsync("get $docs", testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, channelTokenIncompleteMessage) || mints.Load() != 0 || pages.Load() != 1 {
		t.Fatalf("partial alias was minted or misreported: message=%q mints=%d pages=%d", reply, mints.Load(), pages.Load())
	}
}

func TestResourceMetadataLater404DoesNotFallBackToCatalog(t *testing.T) {
	allowed := make(map[string]struct{}, client.ResourceBatchGetMaxIDs+1)
	for i := 0; i <= client.ResourceBatchGetMaxIDs; i++ {
		allowed["r_meta_"+strconv.Itoa(i)] = struct{}{}
	}
	var batches, catalog atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			catalog.Add(1)
			writeResourceListFixture(t, w, nil, "", false)
			return
		}
		if batches.Add(1) > 1 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var input struct {
			ResourceIDs []string `json:"resource_ids"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		rows := make([]client.Resource, 0, len(input.ResourceIDs))
		for _, id := range input.ResourceIDs {
			rows = append(rows, client.Resource{ResourceID: id, Type: client.ResourceTypeTunnel})
		}
		writeMetadataFixture(t, w, rows)
	}))
	t.Cleanup(server.Close)
	rows, err := (&Handler{}).fetchAllowedResources(context.Background(), slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), client.New(server.URL, "key", client.WithRetry(0)), allowed)
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotFound || rows != nil || batches.Load() != 2 || catalog.Load() != 0 {
		t.Fatalf("later failure was weakened into fallback: rows=%d err=%v batches=%d catalog=%d", len(rows), err, batches.Load(), catalog.Load())
	}
}

func TestInspectMetadataFailureIsHardFailure(t *testing.T) {
	b, _ := newBackendUnderTest(t, false)
	b.resourcesOnce.Do(func() {
		b.resources = []client.Resource{{ResourceID: "r_1", Type: client.ResourceTypeURL, Alias: "report"}}
		b.resourcesPartial = true
	})
	var logs bytes.Buffer
	b.log = slog.New(slog.NewTextHandler(&logs, nil))
	var batches, other atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/resources/batch-get" {
			batches.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		other.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(server.Close)
	b.authClient = func(context.Context, string) (*client.Client, error) {
		return client.New(server.URL, "key", client.WithRetry(0)), nil
	}
	got, summarized, err := b.InspectToken(context.Background(), backendTC(), "report")
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusInternalServerError || got != "" || summarized || batches.Load() != 1 || other.Load() != 0 || bytes.Count(logs.Bytes(), []byte("agent backend read failed")) != 1 {
		t.Fatalf("upstream failure hidden or duplicated: message=%q summarized=%t err=%v metadata=%d other=%d", got, summarized, err, batches.Load(), other.Load())
	}
}

func TestHandleGet_MetadataAliasResolvesWithoutCatalog(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedChannelExposure(t, testAdminTeamID, "C_test", "r_visible", "r_deleted")
	var batches, catalog, mints atomic.Int32
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("slug") == "" {
			catalog.Add(1)
		}
		writeResourceListFixture(t, w, nil, "", false)
	})
	ts.addCustomer("POST", "/v1/resources/batch-get", func(w http.ResponseWriter, r *http.Request) {
		batches.Add(1)
		var input struct {
			ResourceIDs []string `json:"resource_ids"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if !reflect.DeepEqual(input.ResourceIDs, []string{"r_deleted", "r_visible"}) {
			t.Errorf("metadata IDs=%v", input.ResourceIDs)
		}
		writeMetadataFixture(t, w, []client.Resource{{ResourceID: "r_visible", Type: client.ResourceTypeURL, Status: client.StatusActive, Alias: "docs"}})
	})
	ts.addCustomer("POST", "/v1/resources/r_visible/qurls", func(w http.ResponseWriter, _ *http.Request) {
		mints.Add(1)
		writeCreateFixture(t, w, "https://qurl.link/metadata-docs", "r_visible")
	})
	h := newAdminTestHandler(t, ts)
	if err := h.cfg.AdminStore.BindChannelAlias(context.Background(), testAdminTeamID, "C_test", "legacy", "https://legacy.example.test"); err != nil {
		t.Fatal(err)
	}
	_, _, reply := newAdminSlashInvoker(t, h).invokeAdminAsync("get $docs", testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, "https://qurl.link/metadata-docs") || batches.Load() != 1 || catalog.Load() != 0 || mints.Load() != 1 {
		t.Fatalf("metadata alias did not mint: message=%q metadata=%d catalog=%d mints=%d", reply, batches.Load(), catalog.Load(), mints.Load())
	}
}
