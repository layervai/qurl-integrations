package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/layervai/qurl-integrations/apps/slack/internal/agent"
	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
	"github.com/layervai/qurl-integrations/shared/auth"
	"github.com/layervai/qurl-integrations/shared/client"
)

const (
	testAgentCreatedURLResourceID = "r_agent_url"
	testAgentGetResourceID        = "r_2"
	testAgentGetQURLLink          = "https://qurl.link/agent-confirm-get"
	testDashboardQURLsPath        = "/v1/resources/r_dashboard/qurls"
)

// qurlBackendServer is an httptest qURL API returning canned resources + quota.
// /v1/resources honors the ?slug= filter so the ResolveToken slug branch can be
// exercised.
func qurlBackendServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/resources/"+testAgentGetResourceID+"/qurls":
			// Agent-confirm get success-path tests need the same resource-scoped mint
			// endpoint getWork calls after resolveTokenForGet authorizes `$staging`.
			var input map[string]any
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Errorf("decode resource mint body: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if got, _ := input["one_time_use"].(bool); !got {
				t.Errorf("one_time_use = %v, want true", input["one_time_use"])
			}
			if got, _ := input["expires_in"].(string); got != resourceLinkExpiry {
				t.Errorf("expires_in = %q, want %q", input["expires_in"], resourceLinkExpiry)
			}
			if got, _ := input["session_duration"].(string); got != resourceSessionDuration {
				t.Errorf("session_duration = %q, want %q", input["session_duration"], resourceSessionDuration)
			}
			if got, _ := input["max_sessions"].(float64); int(got) != resourceMaxSessions {
				t.Errorf("max_sessions = %v, want %d", input["max_sessions"], resourceMaxSessions)
			}
			respondQURLEnvelope(t, w, map[string]any{
				testKeyResourceID: testAgentGetResourceID,
				"qurl_link":       testAgentGetQURLLink,
				"qurl_site":       "https://" + testAgentGetResourceID + ".qurl.site",
			})
		case r.Method == http.MethodPost && r.URL.Path == testResourcesPath:
			// Agent-confirm tests use this server to pin the URL upsert shape:
			// qurl-service owns target-URL idempotency, while Slack binds the
			// channel alias separately after the resource comes back.
			var input client.CreateResourceInput
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Errorf("decode create resource body: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if input.Type != client.ResourceTypeURL {
				t.Errorf("resource type = %q, want %q", input.Type, client.ResourceTypeURL)
			}
			if input.TargetURL == "" {
				t.Errorf("target_url must be present for agent URL protection")
			}
			if input.Alias != "" {
				t.Errorf("agent URL upsert must not send a resource alias; Slack binds the channel alias separately, got %q", input.Alias)
			}
			respondQURLEnvelope(t, w, map[string]any{
				testKeyResourceID: testAgentCreatedURLResourceID,
				testKeyTargetURL:  input.TargetURL,
				testKeyType:       client.ResourceTypeURL,
				testKeyStatus:     client.StatusActive,
			})
		case r.URL.Path == testResourcesPath && r.URL.Query().Get("slug") == "staging":
			// Alias resolution filters for active tunnel resources.
			_, _ = w.Write([]byte(`{"data":[{"resource_id":"r_2","slug":"staging","type":"tunnel","status":"active","description":"Staging dashboard"}]}`))
		case r.URL.Path == testResourcesPath && r.URL.Query().Get("slug") == "ghost":
			_, _ = w.Write([]byte(`{"data":[]}`))
		case r.URL.Path == testResourcesPath:
			// r_url carries a target_url so the protect-url confirm path can match it by
			// exact URL. It's in no channel's allowed set, so channel-scoped list views
			// still filter it out (the list/get tests below stay unaffected).
			_, _ = w.Write([]byte(`{"data":[` +
				`{"resource_id":"r_1","alias":"oncall","type":"url","description":"On-call dash"},` +
				`{"resource_id":"r_url","alias":"handbook","type":"url","target_url":"https://docs.example.com/handbook","description":"Handbook"},` +
				`{"resource_id":"r_9","slug":"secret","type":"tunnel","description":"Other channel"}]}`))
		case r.URL.Path == "/v1/quota":
			_, _ = w.Write([]byte(`{"data":{"plan":"pro","usage":{"active_qurls":3,"qurls_created":10}}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newBackendUnderTest(t *testing.T, withClient bool) (*agentBackend, *slackdata.Store) {
	t.Helper()
	names := defaultTestTableNames()
	// One channel_policies row: alias `oncall`→r_1, allowed set {r_1, r_2}.
	row := map[string]ddbtypes.AttributeValue{
		"slack_team_id":    &ddbtypes.AttributeValueMemberS{Value: "T1"},
		"slack_channel_id": &ddbtypes.AttributeValueMemberS{Value: "C1"},
		"alias_bindings": &ddbtypes.AttributeValueMemberM{Value: map[string]ddbtypes.AttributeValue{
			"oncall": &ddbtypes.AttributeValueMemberS{Value: "r_1"},
		}},
		"allowed_resource_ids": &ddbtypes.AttributeValueMemberSS{Value: []string{"r_1", "r_2"}},
	}
	fake := newFakeDDB(t, names, map[string][]map[string]ddbtypes.AttributeValue{
		names.channelPolicy: {row},
	})
	store := &slackdata.Store{
		Client:                fake,
		WorkspaceMappingsName: names.workspace,
		ChannelPoliciesName:   names.channelPolicy,
		Now:                   func() time.Time { return fixedNow },
	}
	b := &agentBackend{store: store, log: slog.Default()}
	if withClient {
		srv := qurlBackendServer(t)
		c := client.New(srv.URL, "k")
		b.authClient = func(context.Context, string) (*client.Client, error) { return c, nil }
	}
	return b, store
}

func backendTC() *agent.TurnContext {
	return &agent.TurnContext{TeamID: "T1", ChannelID: "C1", UserID: "U1"}
}

func TestAgentBackend_ListAliases(t *testing.T) {
	b, _ := newBackendUnderTest(t, false)
	out, err := b.ListAliases(context.Background(), backendTC())
	if err != nil {
		t.Fatalf("ListAliases: %v", err)
	}
	if !strings.Contains(out, "$oncall") {
		t.Fatalf("aliases output = %q, want $oncall", out)
	}
	// The opaque resource id the alias binds to ($oncall→r_1) must never reach the
	// model/user. Match the exact fixture id, not the bare "r_" substring (which collides
	// with legitimate content like a "user_data" description).
	if strings.Contains(out, "r_1") {
		t.Fatalf("aliases output leaked an internal resource id: %q", out)
	}
}

func TestAgentBackend_ListResources_ScopedToChannel(t *testing.T) {
	b, _ := newBackendUnderTest(t, true)
	out, err := b.ListResources(context.Background(), backendTC())
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	// r_1 ($oncall) is in the channel's allowed set; r_9 ($secret, "Other channel") is not
	// and must not leak. Resources are identified by $alias/$slug — the opaque id is gone.
	if !strings.Contains(out, "$oncall") {
		t.Fatalf("expected the in-scope resource ($oncall) in output: %q", out)
	}
	if strings.Contains(out, "Other channel") || strings.Contains(out, "$secret") {
		t.Fatalf("a resource outside the channel scope leaked: %q", out)
	}
	// The in-scope resource's id (r_1) must not be rendered. Exact fixture id, not bare "r_".
	if strings.Contains(out, "r_1") {
		t.Fatalf("list output leaked an internal resource id: %q", out)
	}
}

func TestAgentBackend_ListResources_PaginatesPastFirstPage(t *testing.T) {
	// allowed = {r_1, r_2}; r_2 only appears on page 2. The listing is
	// workspace-wide, so filtering only page 1 would silently drop r_2.
	b, _ := newBackendUnderTest(t, false)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("cursor") == "" {
			_, _ = w.Write([]byte(`{"data":[{"resource_id":"r_1","alias":"oncall","type":"url"},{"resource_id":"r_9","slug":"sneaky","type":"tunnel"}],"meta":{"has_more":true,"next_cursor":"c2"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"resource_id":"r_2","slug":"staging","type":"tunnel"}]}`))
	}))
	t.Cleanup(srv.Close)
	c := client.New(srv.URL, "k")
	b.authClient = func(context.Context, string) (*client.Client, error) { return c, nil }

	out, err := b.ListResources(context.Background(), backendTC())
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	// r_1 ($oncall, page 1) and r_2 ($staging, page 2) are both reachable; finding $staging
	// proves pagination didn't stop at page 1.
	if !strings.Contains(out, "$oncall") || !strings.Contains(out, "$staging") {
		t.Fatalf("pagination dropped a reachable resource on page 2: %q", out)
	}
	if strings.Contains(out, "$sneaky") {
		t.Fatalf("out-of-scope resource leaked: %q", out)
	}
	// Neither reachable resource's id (r_1, r_2) may be rendered. Exact fixture ids.
	if strings.Contains(out, "r_1") || strings.Contains(out, "r_2") {
		t.Fatalf("list output leaked an internal resource id: %q", out)
	}
}

func TestAgentBackend_ListResources_ShowsChannelAliasForUnaliasedResource(t *testing.T) {
	// The real agent-protected-URL shape: the resource carries NO intrinsic alias and no
	// slug (urls have none) — its handle is the channel-alias binding (Slack-side). The
	// list must surface that channel alias via the join, not a handle-less `- (url)`.
	names := defaultTestTableNames()
	row := map[string]ddbtypes.AttributeValue{
		"slack_team_id":    &ddbtypes.AttributeValueMemberS{Value: "T1"},
		"slack_channel_id": &ddbtypes.AttributeValueMemberS{Value: "C1"},
		"alias_bindings": &ddbtypes.AttributeValueMemberM{Value: map[string]ddbtypes.AttributeValue{
			"deploydash": &ddbtypes.AttributeValueMemberS{Value: "r_url2"},
		}},
		"allowed_resource_ids": &ddbtypes.AttributeValueMemberSS{Value: []string{"r_url2"}},
	}
	store := &slackdata.Store{
		Client:                newFakeDDB(t, names, map[string][]map[string]ddbtypes.AttributeValue{names.channelPolicy: {row}}),
		WorkspaceMappingsName: names.workspace,
		ChannelPoliciesName:   names.channelPolicy,
		Now:                   func() time.Time { return fixedNow },
	}
	b := &agentBackend{store: store, log: slog.Default()}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// r_url2: a url resource with no alias, no slug — named only by the channel binding.
		_, _ = w.Write([]byte(`{"data":[{"resource_id":"r_url2","type":"url","description":"Deploy dashboard","target_url":"https://deploy.example.com"}]}`))
	}))
	t.Cleanup(srv.Close)
	c := client.New(srv.URL, "k")
	b.authClient = func(context.Context, string) (*client.Client, error) { return c, nil }

	out, err := b.ListResources(context.Background(), backendTC())
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	// Without the join this line would be a handle-less `- Deploy dashboard (url)`.
	if !strings.Contains(out, "$deploydash") {
		t.Fatalf("list must surface the channel alias for an unaliased resource, got %q", out)
	}
	if strings.Contains(out, "r_url2") {
		t.Fatalf("list leaked the resource id: %q", out)
	}
}

func TestAgentBackend_ListResources_PicksSmallestAliasOnMultipleBindings(t *testing.T) {
	// alias_bindings is map[alias]rid, so several aliases can bind one resource.
	// GetChannelPolicy builds its slice by ranging that Go map (randomized order), so the
	// label must be chosen deterministically — the lexicographically smallest — or it would
	// flip turn-to-turn for a multi-bound resource.
	names := defaultTestTableNames()
	row := map[string]ddbtypes.AttributeValue{
		"slack_team_id":    &ddbtypes.AttributeValueMemberS{Value: "T1"},
		"slack_channel_id": &ddbtypes.AttributeValueMemberS{Value: "C1"},
		"alias_bindings": &ddbtypes.AttributeValueMemberM{Value: map[string]ddbtypes.AttributeValue{
			"zzz": &ddbtypes.AttributeValueMemberS{Value: "r_url2"},
			"aaa": &ddbtypes.AttributeValueMemberS{Value: "r_url2"},
		}},
		"allowed_resource_ids": &ddbtypes.AttributeValueMemberSS{Value: []string{"r_url2"}},
	}
	store := &slackdata.Store{
		Client:                newFakeDDB(t, names, map[string][]map[string]ddbtypes.AttributeValue{names.channelPolicy: {row}}),
		WorkspaceMappingsName: names.workspace,
		ChannelPoliciesName:   names.channelPolicy,
		Now:                   func() time.Time { return fixedNow },
	}
	b := &agentBackend{store: store, log: slog.Default()}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"resource_id":"r_url2","type":"url","description":"Deploy dashboard","target_url":"https://deploy.example.com"}]}`))
	}))
	t.Cleanup(srv.Close)
	c := client.New(srv.URL, "k")
	b.authClient = func(context.Context, string) (*client.Client, error) { return c, nil }

	out, err := b.ListResources(context.Background(), backendTC())
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	if !strings.Contains(out, "$aaa") || strings.Contains(out, "$zzz") {
		t.Fatalf("multi-bound resource must take the smallest alias ($aaa), got %q", out)
	}
}

func TestAgentBackend_ResolveToken(t *testing.T) {
	b, _ := newBackendUnderTest(t, true)
	ctx := context.Background()

	// Match the exact bound fixture ids (alias→r_1, slug→r_2), not the bare "r_" substring.
	alias, err := b.ResolveToken(ctx, backendTC(), "$oncall")
	if err != nil || !strings.Contains(alias, "$oncall") || strings.Contains(alias, "r_1") {
		t.Fatalf("alias resolve must confirm $oncall without leaking a resource id: %q err=%v", alias, err)
	}
	slug, err := b.ResolveToken(ctx, backendTC(), "staging")
	if err != nil || !strings.Contains(slug, "$staging") || strings.Contains(slug, "r_2") {
		t.Fatalf("slug resolve must name $staging without leaking a resource id: %q err=%v", slug, err)
	}
	ghost, err := b.ResolveToken(ctx, backendTC(), "ghost")
	if err != nil || !strings.Contains(ghost, "doesn't resolve") {
		t.Fatalf("ghost resolve: %q err=%v", ghost, err)
	}
}

func TestAgentBackend_InspectToken(t *testing.T) {
	names := defaultTestTableNames()
	row := map[string]ddbtypes.AttributeValue{
		"slack_team_id":    &ddbtypes.AttributeValueMemberS{Value: "T1"},
		"slack_channel_id": &ddbtypes.AttributeValueMemberS{Value: "C1"},
		"alias_bindings": &ddbtypes.AttributeValueMemberM{Value: map[string]ddbtypes.AttributeValue{
			"dashboard": &ddbtypes.AttributeValueMemberS{Value: "r_dashboard"},
		}},
		"allowed_resource_ids": &ddbtypes.AttributeValueMemberSS{Value: []string{"r_dashboard"}},
	}
	store := &slackdata.Store{
		Client:                newFakeDDB(t, names, map[string][]map[string]ddbtypes.AttributeValue{names.channelPolicy: {row}}),
		WorkspaceMappingsName: names.workspace,
		ChannelPoliciesName:   names.channelPolicy,
		Now:                   func() time.Time { return fixedNow },
	}
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><html><head><title>Team Dashboard</title><meta name="description" content="Operational dashboards and release status."></head><body><main><h1>Dashboard</h1><p>Shows incidents, deploy health, and release readiness.</p></main></body></html>`))
	}))
	t.Cleanup(page.Close)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == testResourcesPath:
			_, _ = w.Write([]byte(`{"data":[{"resource_id":"r_dashboard","type":"url","description":"Production dashboard"}]}`))
		case r.Method == http.MethodPost && r.URL.Path == testDashboardQURLsPath:
			respondQURLEnvelope(t, w, map[string]any{
				testKeyResourceID: "r_dashboard",
				"qurl_link":       page.URL,
				"qurl_site":       page.URL,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(api.Close)

	b := &agentBackend{
		authClient:                    func(context.Context, string) (*client.Client, error) { return client.New(api.URL, "k"), nil },
		store:                         store,
		log:                           slog.Default(),
		allowInspectableLoopbackHosts: true,
	}
	out, err := b.InspectToken(context.Background(), backendTC(), "$dashboard")
	if err != nil {
		t.Fatalf("InspectToken: %v", err)
	}
	for _, want := range []string{"$dashboard", "Team Dashboard", "Operational dashboards and release status.", "Dashboard"} {
		if !strings.Contains(out, want) {
			t.Fatalf("inspect output missing %q: %q", want, out)
		}
	}
	if strings.Contains(out, "Shows incidents, deploy health, and release readiness.") {
		t.Fatalf("inspect output must not expose document body text: %q", out)
	}
	if strings.Contains(out, page.URL) || strings.Contains(out, "r_dashboard") {
		t.Fatalf("inspect output leaked a qurl link or resource id: %q", out)
	}
}

func TestAgentBackend_InspectToken_EscapesUntrustedPageContent(t *testing.T) {
	names := defaultTestTableNames()
	row := map[string]ddbtypes.AttributeValue{
		"slack_team_id":    &ddbtypes.AttributeValueMemberS{Value: "T1"},
		"slack_channel_id": &ddbtypes.AttributeValueMemberS{Value: "C1"},
		"alias_bindings": &ddbtypes.AttributeValueMemberM{Value: map[string]ddbtypes.AttributeValue{
			"dashboard": &ddbtypes.AttributeValueMemberS{Value: "r_dashboard"},
		}},
		"allowed_resource_ids": &ddbtypes.AttributeValueMemberSS{Value: []string{"r_dashboard"}},
	}
	store := &slackdata.Store{
		Client:                newFakeDDB(t, names, map[string][]map[string]ddbtypes.AttributeValue{names.channelPolicy: {row}}),
		WorkspaceMappingsName: names.workspace,
		ChannelPoliciesName:   names.channelPolicy,
		Now:                   func() time.Time { return fixedNow },
	}
	// Hostile page: title/description/heading carry mrkdwn control chars and Slack
	// broadcast mentions. The summary posts straight to the channel, so these MUST be
	// escaped — otherwise a page could spoof the card or @-broadcast the channel.
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><html><head><title>Pwn &lt;!channel&gt; *bold*</title><meta name="description" content="click &lt;https://evil.example|here&gt;"></head><body><main><h1>&lt;!here&gt; run this</h1></main></body></html>`))
	}))
	t.Cleanup(page.Close)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == testResourcesPath:
			_, _ = w.Write([]byte(`{"data":[{"resource_id":"r_dashboard","type":"url","description":"Production dashboard"}]}`))
		case r.Method == http.MethodPost && r.URL.Path == testDashboardQURLsPath:
			respondQURLEnvelope(t, w, map[string]any{
				testKeyResourceID: "r_dashboard",
				"qurl_link":       page.URL,
				"qurl_site":       page.URL,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(api.Close)

	b := &agentBackend{
		authClient:                    func(context.Context, string) (*client.Client, error) { return client.New(api.URL, "k"), nil },
		store:                         store,
		log:                           slog.Default(),
		allowInspectableLoopbackHosts: true,
	}
	out, err := b.InspectToken(context.Background(), backendTC(), "$dashboard")
	if err != nil {
		t.Fatalf("InspectToken: %v", err)
	}
	// Raw mrkdwn / broadcast controls from the page must NOT survive into the card.
	for _, banned := range []string{"<!channel>", "<!here>", "<https://evil.example|here>", "*bold*"} {
		if strings.Contains(out, banned) {
			t.Fatalf("unescaped untrusted content %q leaked into the card: %q", banned, out)
		}
	}
	// ...they should appear in their escaped form instead.
	if !strings.Contains(out, "&lt;!channel&gt;") {
		t.Fatalf("expected the hostile title to be mrkdwn-escaped, got %q", out)
	}
}

func TestAgentBackend_InspectToken_StripsHTMLNoiseAndTruncatesSummaryFields(t *testing.T) {
	names := defaultTestTableNames()
	row := map[string]ddbtypes.AttributeValue{
		"slack_team_id":    &ddbtypes.AttributeValueMemberS{Value: "T1"},
		"slack_channel_id": &ddbtypes.AttributeValueMemberS{Value: "C1"},
		"alias_bindings": &ddbtypes.AttributeValueMemberM{Value: map[string]ddbtypes.AttributeValue{
			"dashboard": &ddbtypes.AttributeValueMemberS{Value: "r_dashboard"},
		}},
		"allowed_resource_ids": &ddbtypes.AttributeValueMemberSS{Value: []string{"r_dashboard"}},
	}
	store := &slackdata.Store{
		Client:                newFakeDDB(t, names, map[string][]map[string]ddbtypes.AttributeValue{names.channelPolicy: {row}}),
		WorkspaceMappingsName: names.workspace,
		ChannelPoliciesName:   names.channelPolicy,
		Now:                   func() time.Time { return fixedNow },
	}
	const injectedTitle = "Injected Script Title"
	const injectedHeading = "Injected Script Heading"
	realTitle := "Team Dashboard " + strings.Repeat("alpha ", 40)
	realMeta := "Operational dashboards and release status. " + strings.Repeat("beta ", 60)
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><html><head><script>document.write("<title>` + injectedTitle + `</title><h1>` + injectedHeading + `</h1>")</script><title>` + realTitle + `</title><meta name="description" content="` + realMeta + `"></head><body><main><h1>Dashboard</h1></main></body></html>`))
	}))
	t.Cleanup(page.Close)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == testResourcesPath:
			_, _ = w.Write([]byte(`{"data":[{"resource_id":"r_dashboard","type":"url","description":"Production dashboard"}]}`))
		case r.Method == http.MethodPost && r.URL.Path == testDashboardQURLsPath:
			respondQURLEnvelope(t, w, map[string]any{
				testKeyResourceID: "r_dashboard",
				"qurl_link":       page.URL,
				"qurl_site":       page.URL,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(api.Close)

	b := &agentBackend{
		authClient:                    func(context.Context, string) (*client.Client, error) { return client.New(api.URL, "k"), nil },
		store:                         store,
		log:                           slog.Default(),
		allowInspectableLoopbackHosts: true,
	}
	out, err := b.InspectToken(context.Background(), backendTC(), "$dashboard")
	if err != nil {
		t.Fatalf("InspectToken: %v", err)
	}
	wantTitle := truncateRunes(normalizeInspectedText(realTitle), agentInspectTitleMaxRunes)
	wantMeta := truncateRunes(normalizeInspectedText(realMeta), agentInspectMetaMaxRunes)
	for _, want := range []string{wantTitle, wantMeta, "Dashboard"} {
		if !strings.Contains(out, want) {
			t.Fatalf("inspect output missing %q: %q", want, out)
		}
	}
	for _, unwanted := range []string{injectedTitle, injectedHeading, realTitle, realMeta} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("inspect output leaked unbounded or noisy HTML content %q: %q", unwanted, out)
		}
	}
}

func TestAgentBackend_InspectToken_RejectsAuthPage(t *testing.T) {
	names := defaultTestTableNames()
	row := map[string]ddbtypes.AttributeValue{
		"slack_team_id":    &ddbtypes.AttributeValueMemberS{Value: "T1"},
		"slack_channel_id": &ddbtypes.AttributeValueMemberS{Value: "C1"},
		"alias_bindings": &ddbtypes.AttributeValueMemberM{Value: map[string]ddbtypes.AttributeValue{
			"dashboard": &ddbtypes.AttributeValueMemberS{Value: "r_dashboard"},
		}},
		"allowed_resource_ids": &ddbtypes.AttributeValueMemberSS{Value: []string{"r_dashboard"}},
	}
	store := &slackdata.Store{
		Client:                newFakeDDB(t, names, map[string][]map[string]ddbtypes.AttributeValue{names.channelPolicy: {row}}),
		WorkspaceMappingsName: names.workspace,
		ChannelPoliciesName:   names.channelPolicy,
		Now:                   func() time.Time { return fixedNow },
	}
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><html><head><title>Sign in</title></head><body><form><input type="email" name="email"><input type="password" name="password"></form></body></html>`))
	}))
	t.Cleanup(page.Close)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == testResourcesPath:
			_, _ = w.Write([]byte(`{"data":[{"resource_id":"r_dashboard","type":"url","description":"Production dashboard"}]}`))
		case r.Method == http.MethodPost && r.URL.Path == testDashboardQURLsPath:
			respondQURLEnvelope(t, w, map[string]any{
				testKeyResourceID: "r_dashboard",
				"qurl_link":       page.URL,
				"qurl_site":       page.URL,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(api.Close)

	b := &agentBackend{
		authClient:                    func(context.Context, string) (*client.Client, error) { return client.New(api.URL, "k"), nil },
		store:                         store,
		log:                           slog.Default(),
		allowInspectableLoopbackHosts: true,
	}
	out, err := b.InspectToken(context.Background(), backendTC(), "$dashboard")
	if err != nil {
		t.Fatalf("InspectToken: %v", err)
	}
	if !strings.Contains(out, "couldn't read its page content right now") {
		t.Fatalf("inspect auth-page fallback = %q", out)
	}
}

func TestAgentBackend_InspectToken_JSONReturnsGenericFallback(t *testing.T) {
	names := defaultTestTableNames()
	row := map[string]ddbtypes.AttributeValue{
		"slack_team_id":    &ddbtypes.AttributeValueMemberS{Value: "T1"},
		"slack_channel_id": &ddbtypes.AttributeValueMemberS{Value: "C1"},
		"alias_bindings": &ddbtypes.AttributeValueMemberM{Value: map[string]ddbtypes.AttributeValue{
			"dashboard": &ddbtypes.AttributeValueMemberS{Value: "r_dashboard"},
		}},
		"allowed_resource_ids": &ddbtypes.AttributeValueMemberSS{Value: []string{"r_dashboard"}},
	}
	store := &slackdata.Store{
		Client:                newFakeDDB(t, names, map[string][]map[string]ddbtypes.AttributeValue{names.channelPolicy: {row}}),
		WorkspaceMappingsName: names.workspace,
		ChannelPoliciesName:   names.channelPolicy,
		Now:                   func() time.Time { return fixedNow },
	}
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"title":"Dashboard API","status":"ok"}`))
	}))
	t.Cleanup(page.Close)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == testResourcesPath:
			_, _ = w.Write([]byte(`{"data":[{"resource_id":"r_dashboard","type":"url","description":"Production dashboard"}]}`))
		case r.Method == http.MethodPost && r.URL.Path == testDashboardQURLsPath:
			respondQURLEnvelope(t, w, map[string]any{
				testKeyResourceID: "r_dashboard",
				"qurl_link":       page.URL,
				"qurl_site":       page.URL,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(api.Close)

	b := &agentBackend{
		authClient:                    func(context.Context, string) (*client.Client, error) { return client.New(api.URL, "k"), nil },
		store:                         store,
		log:                           slog.Default(),
		allowInspectableLoopbackHosts: true,
	}
	out, err := b.InspectToken(context.Background(), backendTC(), "$dashboard")
	if err != nil {
		t.Fatalf("InspectToken: %v", err)
	}
	if !strings.Contains(out, "couldn't read its page content right now") {
		t.Fatalf("inspect JSON fallback = %q", out)
	}
	if strings.Contains(out, "Protected resource for `$dashboard`") {
		t.Fatalf("inspect JSON output must not be treated as a protected resource: %q", out)
	}
}

func TestInspectAllowedEntryHost_RejectsLoopbackWithoutOverride(t *testing.T) {
	qurlLink, err := url.Parse("http://127.0.0.1:8080/inspect")
	if err != nil {
		t.Fatalf("parse qurl link: %v", err)
	}
	qurlSite, err := url.Parse("http://127.0.0.1:9090/site")
	if err != nil {
		t.Fatalf("parse qurl site: %v", err)
	}
	if err := inspectAllowedEntryHost(qurlLink, qurlSite, false); err == nil {
		t.Fatal("inspectAllowedEntryHost allowed loopback without test override")
	}
}

func TestAgentBackend_InspectToken_PDFReturnsProtectedResource(t *testing.T) {
	names := defaultTestTableNames()
	row := map[string]ddbtypes.AttributeValue{
		"slack_team_id":    &ddbtypes.AttributeValueMemberS{Value: "T1"},
		"slack_channel_id": &ddbtypes.AttributeValueMemberS{Value: "C1"},
		"alias_bindings": &ddbtypes.AttributeValueMemberM{Value: map[string]ddbtypes.AttributeValue{
			"runbook": &ddbtypes.AttributeValueMemberS{Value: "r_runbook"},
		}},
		"allowed_resource_ids": &ddbtypes.AttributeValueMemberSS{Value: []string{"r_runbook"}},
	}
	store := &slackdata.Store{
		Client:                newFakeDDB(t, names, map[string][]map[string]ddbtypes.AttributeValue{names.channelPolicy: {row}}),
		WorkspaceMappingsName: names.workspace,
		ChannelPoliciesName:   names.channelPolicy,
		Now:                   func() time.Time { return fixedNow },
	}
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.Header().Set("Content-Disposition", `attachment; filename="runbook.pdf"`)
		_, _ = w.Write([]byte("%PDF-1.4 fake pdf body"))
	}))
	t.Cleanup(page.Close)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == testResourcesPath:
			_, _ = w.Write([]byte(`{"data":[{"resource_id":"r_runbook","type":"url","description":"Operations runbook"}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/resources/r_runbook/qurls":
			respondQURLEnvelope(t, w, map[string]any{
				testKeyResourceID: "r_runbook",
				"qurl_link":       page.URL,
				"qurl_site":       page.URL,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(api.Close)

	b := &agentBackend{
		authClient:                    func(context.Context, string) (*client.Client, error) { return client.New(api.URL, "k"), nil },
		store:                         store,
		log:                           slog.Default(),
		allowInspectableLoopbackHosts: true,
	}
	out, err := b.InspectToken(context.Background(), backendTC(), "$runbook")
	if err != nil {
		t.Fatalf("InspectToken: %v", err)
	}
	for _, want := range []string{"Protected resource for `$runbook`", "Operations runbook", "application/pdf", "no website summary is available"} {
		if !strings.Contains(out, want) {
			t.Fatalf("pdf inspect output missing %q: %q", want, out)
		}
	}
	if strings.Contains(out, "couldn't read its page content right now") {
		t.Fatalf("pdf inspect output should identify a protected resource instead of generic fallback: %q", out)
	}
}

func TestAgentBackend_InspectToken_DownloadMimeReturnsProtectedResource(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		want        string
	}{
		{
			name:        "docx",
			contentType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
			want:        "wordprocessingml.document",
		},
		{
			name:        "zip",
			contentType: "application/zip",
			want:        "application/zip",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			names := defaultTestTableNames()
			row := map[string]ddbtypes.AttributeValue{
				"slack_team_id":    &ddbtypes.AttributeValueMemberS{Value: "T1"},
				"slack_channel_id": &ddbtypes.AttributeValueMemberS{Value: "C1"},
				"alias_bindings": &ddbtypes.AttributeValueMemberM{Value: map[string]ddbtypes.AttributeValue{
					"download": &ddbtypes.AttributeValueMemberS{Value: "r_download"},
				}},
				"allowed_resource_ids": &ddbtypes.AttributeValueMemberSS{Value: []string{"r_download"}},
			}
			store := &slackdata.Store{
				Client:                newFakeDDB(t, names, map[string][]map[string]ddbtypes.AttributeValue{names.channelPolicy: {row}}),
				WorkspaceMappingsName: names.workspace,
				ChannelPoliciesName:   names.channelPolicy,
				Now:                   func() time.Time { return fixedNow },
			}
			page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				_, _ = w.Write([]byte("download body"))
			}))
			t.Cleanup(page.Close)
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && r.URL.Path == testResourcesPath:
					_, _ = w.Write([]byte(`{"data":[{"resource_id":"r_download","type":"url","description":"Download resource"}]}`))
				case r.Method == http.MethodPost && r.URL.Path == "/v1/resources/r_download/qurls":
					respondQURLEnvelope(t, w, map[string]any{
						testKeyResourceID: "r_download",
						"qurl_link":       page.URL,
						"qurl_site":       page.URL,
					})
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(api.Close)

			b := &agentBackend{
				authClient:                    func(context.Context, string) (*client.Client, error) { return client.New(api.URL, "k"), nil },
				store:                         store,
				log:                           slog.Default(),
				allowInspectableLoopbackHosts: true,
			}
			out, err := b.InspectToken(context.Background(), backendTC(), "$download")
			if err != nil {
				t.Fatalf("InspectToken: %v", err)
			}
			if !strings.Contains(out, "Protected resource for `$download`") || !strings.Contains(out, tc.want) {
				t.Fatalf("inspect download output missing protected-resource signal: %q", out)
			}
			if strings.Contains(out, "couldn't read its page content right now") {
				t.Fatalf("inspect download output fell back to generic failure: %q", out)
			}
		})
	}
}

func TestAgentBackend_InspectToken_AllowsQURLLinkRedirectToQURLSite(t *testing.T) {
	names := defaultTestTableNames()
	row := map[string]ddbtypes.AttributeValue{
		"slack_team_id":    &ddbtypes.AttributeValueMemberS{Value: "T1"},
		"slack_channel_id": &ddbtypes.AttributeValueMemberS{Value: "C1"},
		"alias_bindings": &ddbtypes.AttributeValueMemberM{Value: map[string]ddbtypes.AttributeValue{
			"dashboard": &ddbtypes.AttributeValueMemberS{Value: "r_dashboard"},
		}},
		"allowed_resource_ids": &ddbtypes.AttributeValueMemberSS{Value: []string{"r_dashboard"}},
	}
	store := &slackdata.Store{
		Client:                newFakeDDB(t, names, map[string][]map[string]ddbtypes.AttributeValue{names.channelPolicy: {row}}),
		WorkspaceMappingsName: names.workspace,
		ChannelPoliciesName:   names.channelPolicy,
		Now:                   func() time.Time { return fixedNow },
	}
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><html><head><title>Team Dashboard</title><meta name="description" content="Operational dashboards and release status."></head><body><main><h1>Dashboard</h1></main></body></html>`))
	}))
	t.Cleanup(site.Close)
	link := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, site.URL, http.StatusFound)
	}))
	t.Cleanup(link.Close)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == testResourcesPath:
			_, _ = w.Write([]byte(`{"data":[{"resource_id":"r_dashboard","type":"url","description":"Production dashboard"}]}`))
		case r.Method == http.MethodPost && r.URL.Path == testDashboardQURLsPath:
			respondQURLEnvelope(t, w, map[string]any{
				testKeyResourceID: "r_dashboard",
				"qurl_link":       link.URL,
				"qurl_site":       site.URL,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(api.Close)

	b := &agentBackend{
		authClient:                    func(context.Context, string) (*client.Client, error) { return client.New(api.URL, "k"), nil },
		store:                         store,
		log:                           slog.Default(),
		allowInspectableLoopbackHosts: true,
	}
	out, err := b.InspectToken(context.Background(), backendTC(), "$dashboard")
	if err != nil {
		t.Fatalf("InspectToken: %v", err)
	}
	for _, want := range []string{"$dashboard", "Team Dashboard", "Operational dashboards and release status."} {
		if !strings.Contains(out, want) {
			t.Fatalf("redirected inspect output missing %q: %q", want, out)
		}
	}
}

func TestAgentBackend_InspectToken_RejectsCrossHostRedirect(t *testing.T) {
	names := defaultTestTableNames()
	row := map[string]ddbtypes.AttributeValue{
		"slack_team_id":    &ddbtypes.AttributeValueMemberS{Value: "T1"},
		"slack_channel_id": &ddbtypes.AttributeValueMemberS{Value: "C1"},
		"alias_bindings": &ddbtypes.AttributeValueMemberM{Value: map[string]ddbtypes.AttributeValue{
			"dashboard": &ddbtypes.AttributeValueMemberS{Value: "r_dashboard"},
		}},
		"allowed_resource_ids": &ddbtypes.AttributeValueMemberSS{Value: []string{"r_dashboard"}},
	}
	store := &slackdata.Store{
		Client:                newFakeDDB(t, names, map[string][]map[string]ddbtypes.AttributeValue{names.channelPolicy: {row}}),
		WorkspaceMappingsName: names.workspace,
		ChannelPoliciesName:   names.channelPolicy,
		Now:                   func() time.Time { return fixedNow },
	}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><html><head><title>Secret</title></head><body><p>Cross-host content</p></body></html>`))
	}))
	t.Cleanup(target.Close)
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	t.Cleanup(page.Close)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == testResourcesPath:
			_, _ = w.Write([]byte(`{"data":[{"resource_id":"r_dashboard","type":"url","description":"Production dashboard"}]}`))
		case r.Method == http.MethodPost && r.URL.Path == testDashboardQURLsPath:
			respondQURLEnvelope(t, w, map[string]any{
				testKeyResourceID: "r_dashboard",
				"qurl_link":       page.URL,
				"qurl_site":       page.URL,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(api.Close)

	b := &agentBackend{
		authClient:                    func(context.Context, string) (*client.Client, error) { return client.New(api.URL, "k"), nil },
		store:                         store,
		log:                           slog.Default(),
		allowInspectableLoopbackHosts: true,
	}
	out, err := b.InspectToken(context.Background(), backendTC(), "$dashboard")
	if err != nil {
		t.Fatalf("InspectToken: %v", err)
	}
	if !strings.Contains(out, "couldn't read its page content right now") {
		t.Fatalf("inspect cross-host redirect fallback = %q", out)
	}
	if strings.Contains(out, "Cross-host content") {
		t.Fatalf("inspect output must not follow cross-host redirects: %q", out)
	}
}

func TestAgentBackend_InspectToken_AllowsProtectedAuthDocumentation(t *testing.T) {
	names := defaultTestTableNames()
	row := map[string]ddbtypes.AttributeValue{
		"slack_team_id":    &ddbtypes.AttributeValueMemberS{Value: "T1"},
		"slack_channel_id": &ddbtypes.AttributeValueMemberS{Value: "C1"},
		"alias_bindings": &ddbtypes.AttributeValueMemberM{Value: map[string]ddbtypes.AttributeValue{
			"login-guide": &ddbtypes.AttributeValueMemberS{Value: "r_login_guide"},
		}},
		"allowed_resource_ids": &ddbtypes.AttributeValueMemberSS{Value: []string{"r_login_guide"}},
	}
	store := &slackdata.Store{
		Client:                newFakeDDB(t, names, map[string][]map[string]ddbtypes.AttributeValue{names.channelPolicy: {row}}),
		WorkspaceMappingsName: names.workspace,
		ChannelPoliciesName:   names.channelPolicy,
		Now:                   func() time.Time { return fixedNow },
	}
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><html><head><title>SSO Login Guide</title><meta name="description" content="How to configure single sign-on for the portal."></head><body><main><h1>SSO Login Guide</h1><h2>Prerequisites</h2><p>This guide explains login prerequisites, redirect URLs, and troubleshooting steps.</p></main></body></html>`))
	}))
	t.Cleanup(page.Close)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == testResourcesPath:
			_, _ = w.Write([]byte(`{"data":[{"resource_id":"r_login_guide","type":"url","description":"Protected login guide"}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/resources/r_login_guide/qurls":
			respondQURLEnvelope(t, w, map[string]any{
				testKeyResourceID: "r_login_guide",
				"qurl_link":       page.URL,
				"qurl_site":       page.URL,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(api.Close)

	b := &agentBackend{
		authClient:                    func(context.Context, string) (*client.Client, error) { return client.New(api.URL, "k"), nil },
		store:                         store,
		log:                           slog.Default(),
		allowInspectableLoopbackHosts: true,
	}
	out, err := b.InspectToken(context.Background(), backendTC(), "$login-guide")
	if err != nil {
		t.Fatalf("InspectToken: %v", err)
	}
	for _, want := range []string{"$login-guide", "SSO Login Guide", "How to configure single sign-on for the portal.", "Prerequisites"} {
		if !strings.Contains(out, want) {
			t.Fatalf("inspect output missing %q: %q", want, out)
		}
	}
	if strings.Contains(out, "This guide explains login prerequisites, redirect URLs, and troubleshooting steps.") {
		t.Fatalf("inspect output must not expose protected document body text: %q", out)
	}
}

func TestAgentBackend_InspectToken_RejectsUnexpectedInitialLinkHost(t *testing.T) {
	names := defaultTestTableNames()
	row := map[string]ddbtypes.AttributeValue{
		"slack_team_id":    &ddbtypes.AttributeValueMemberS{Value: "T1"},
		"slack_channel_id": &ddbtypes.AttributeValueMemberS{Value: "C1"},
		"alias_bindings": &ddbtypes.AttributeValueMemberM{Value: map[string]ddbtypes.AttributeValue{
			"dashboard": &ddbtypes.AttributeValueMemberS{Value: "r_dashboard"},
		}},
		"allowed_resource_ids": &ddbtypes.AttributeValueMemberSS{Value: []string{"r_dashboard"}},
	}
	store := &slackdata.Store{
		Client:                newFakeDDB(t, names, map[string][]map[string]ddbtypes.AttributeValue{names.channelPolicy: {row}}),
		WorkspaceMappingsName: names.workspace,
		ChannelPoliciesName:   names.channelPolicy,
		Now:                   func() time.Time { return fixedNow },
	}
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><html><head><title>Team Dashboard</title></head><body><main><h1>Dashboard</h1></main></body></html>`))
	}))
	t.Cleanup(page.Close)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == testResourcesPath:
			_, _ = w.Write([]byte(`{"data":[{"resource_id":"r_dashboard","type":"url","description":"Production dashboard"}]}`))
		case r.Method == http.MethodPost && r.URL.Path == testDashboardQURLsPath:
			respondQURLEnvelope(t, w, map[string]any{
				testKeyResourceID: "r_dashboard",
				"qurl_link":       "https://evil.example/summary",
				"qurl_site":       page.URL,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(api.Close)

	var fetched atomic.Bool
	b := &agentBackend{
		authClient:                    func(context.Context, string) (*client.Client, error) { return client.New(api.URL, "k"), nil },
		store:                         store,
		log:                           slog.Default(),
		allowInspectableLoopbackHosts: true,
		fetchClient: &http.Client{
			Transport: testRoundTripFunc(func(*http.Request) (*http.Response, error) {
				fetched.Store(true)
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`<!doctype html><html><head><title>evil</title></head><body>evil</body></html>`)),
				}, nil
			}),
		},
	}
	out, err := b.InspectToken(context.Background(), backendTC(), "$dashboard")
	if err != nil {
		t.Fatalf("InspectToken: %v", err)
	}
	if fetched.Load() {
		t.Fatalf("inspect should not fetch an unexpected initial qurl_link host")
	}
	if !strings.Contains(out, "couldn't read its page content right now") {
		t.Fatalf("inspect unexpected-host fallback = %q", out)
	}
}

func TestAgentBackend_Quota(t *testing.T) {
	b, _ := newBackendUnderTest(t, true)
	out, err := b.Quota(context.Background(), backendTC())
	if err != nil {
		t.Fatalf("Quota: %v", err)
	}
	if !strings.Contains(out, "pro") || !strings.Contains(out, "3") {
		t.Fatalf("quota output = %q", out)
	}
}

func TestAgentBackend_ChannelScopeMemoizedPerTurn(t *testing.T) {
	// The backend is built once per turn and reused across the model's tool
	// calls; the channel scope is invariant within a turn, so it must be read
	// once, not on every list/resolve.
	b, store := newBackendUnderTest(t, false)
	fake, ok := store.Client.(*fakeDDB)
	if !ok {
		t.Fatalf("expected the internal fakeDDB")
	}
	cp := defaultTestTableNames().channelPolicy
	var gets int
	fake.SetGetItemHook(func(table string, _ map[string]string) {
		if table == cp {
			gets++
		}
	})
	ctx := context.Background()
	for range 3 {
		if _, err := b.channelAllowed(ctx, backendTC()); err != nil {
			t.Fatalf("channelAllowed: %v", err)
		}
	}
	if gets != 1 {
		t.Fatalf("channel scope read %d times, want 1 (memoized)", gets)
	}
}

func TestAgentBackend_ResourceScanMemoizedPerTurn(t *testing.T) {
	// list_resources may be called several times in one turn; the channel's
	// reachable set is invariant within a (read-only) turn, so the workspace scan
	// must run once and be reused — not re-paged on every call.
	b, _ := newBackendUnderTest(t, false) // allowed = {r_1, r_2}
	var gets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		gets.Add(1)
		w.Header().Set("Content-Type", "application/json")
		// Both reachable ids on a single page (no has_more) → one GET per scan.
		_, _ = w.Write([]byte(`{"data":[{"resource_id":"r_1","alias":"oncall","type":"url"},{"resource_id":"r_2","slug":"staging","type":"tunnel"}]}`))
	}))
	t.Cleanup(srv.Close)
	c := client.New(srv.URL, "k")
	b.authClient = func(context.Context, string) (*client.Client, error) { return c, nil }

	ctx := context.Background()
	for range 3 {
		if _, err := b.ListResources(ctx, backendTC()); err != nil {
			t.Fatalf("ListResources: %v", err)
		}
	}
	if g := gets.Load(); g != 1 {
		t.Fatalf("workspace resource list fetched %d times across 3 calls, want 1 (memoized)", g)
	}
}

func TestAgentBackend_StalePolicyScanMemoized(t *testing.T) {
	// A stale channel_policies id (r_2 here is absent workspace-side) keeps
	// len(found) below len(allowed), so collectChannelResources' early-stop can't
	// fire and the scan pages to the end. The per-turn memo bounds that cost: the
	// stale-policy scan is paid once per turn, not re-paged on every list_resources.
	b, _ := newBackendUnderTest(t, false) // allowed = {r_1, r_2}
	var gets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("cursor") == "" {
			// Page 1: r_1 reachable, plus has_more. r_2 never appears (stale).
			_, _ = w.Write([]byte(`{"data":[{"resource_id":"r_1","alias":"oncall","type":"url"}],"meta":{"has_more":true,"next_cursor":"c2"}}`))
			return
		}
		// Page 2: nothing reachable, no has_more → scan ends here (2 GETs/scan).
		_, _ = w.Write([]byte(`{"data":[{"resource_id":"r_9","type":"tunnel"}]}`))
	}))
	t.Cleanup(srv.Close)
	c := client.New(srv.URL, "k")
	b.authClient = func(context.Context, string) (*client.Client, error) { return c, nil }

	ctx := context.Background()
	for range 3 {
		out, err := b.ListResources(ctx, backendTC())
		if err != nil {
			t.Fatalf("ListResources: %v", err)
		}
		if !strings.Contains(out, "$oncall") {
			t.Fatalf("reachable resource missing from scan: %q", out)
		}
	}
	// Without the memo this stale-policy scan re-pages on every call (2 × 3 = 6);
	// the memo bounds it to a single 2-page scan for the turn.
	if g := gets.Load(); g != 2 {
		t.Fatalf("stale-policy scan fetched %d pages across 3 calls, want 2 (one memoized scan)", g)
	}
}

func TestAgentBackend_NilStoreIsGraceful(t *testing.T) {
	b := &agentBackend{}
	for _, fn := range []func() (string, error){
		func() (string, error) { return b.ListResources(context.Background(), backendTC()) },
		func() (string, error) { return b.ListAliases(context.Background(), backendTC()) },
		func() (string, error) { return b.ResolveToken(context.Background(), backendTC(), "x") },
		func() (string, error) { return b.InspectToken(context.Background(), backendTC(), "x") },
	} {
		out, err := fn()
		if err != nil || out != agentBackendUnconfigured {
			t.Fatalf("nil store should be graceful, got %q err=%v", out, err)
		}
	}
}

func TestAgentBackend_UnboundWorkspaceNudgesToSetup(t *testing.T) {
	// When the workspace isn't connected to qURL, a tool that needs the client must
	// return the actionable "/qurl setup <email>" nudge as content (so the model
	// relays it) rather than a generic model-safe error — matching the slash path.
	b, _ := newBackendUnderTest(t, false)
	b.authClient = func(context.Context, string) (*client.Client, error) {
		return nil, auth.ErrWorkspaceNotConfigured
	}
	for name, fn := range map[string]func() (string, error){
		// ListResources needs the client only when the channel has an allowed set, so
		// resolve a token (its slug branch always reaches the client) and read quota.
		"ResolveToken": func() (string, error) { return b.ResolveToken(context.Background(), backendTC(), "staging") },
		"InspectToken": func() (string, error) { return b.InspectToken(context.Background(), backendTC(), "staging") },
		"Quota":        func() (string, error) { return b.Quota(context.Background(), backendTC()) },
	} {
		out, err := fn()
		if err != nil {
			t.Fatalf("%s: unbound workspace must not error, got %v", name, err)
		}
		if out != workspaceNotSetupMessage {
			t.Fatalf("%s: want the setup nudge, got %q", name, out)
		}
	}
}

func TestAgentBackend_LogsReadError(t *testing.T) {
	// A backend read failure is collapsed to a generic string for the model, so
	// the real error must be logged for operators.
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	names := defaultTestTableNames()
	fake := newFakeDDB(t, names, nil)
	fake.SetGetItemErr(names.channelPolicy, errors.New("ddb exploded"))
	store := &slackdata.Store{Client: fake, WorkspaceMappingsName: names.workspace, ChannelPoliciesName: names.channelPolicy, Now: func() time.Time { return fixedNow }}
	b := &agentBackend{store: store, log: logger}

	out, err := b.ListAliases(context.Background(), backendTC())
	if err == nil || out != "" {
		t.Fatalf("expected an error, got out=%q err=%v", out, err)
	}
	if !strings.Contains(buf.String(), "agent backend read failed") || !strings.Contains(buf.String(), "ddb exploded") {
		t.Fatalf("backend error was not logged: %s", buf.String())
	}
}

func TestFormatResourceLine(t *testing.T) {
	// No channel alias → falls back to the resource's intrinsic alias.
	got := formatResourceLine(&client.Resource{ResourceID: "r_1", Alias: "oncall", Type: "url", Description: "Dash"}, "")
	if !strings.Contains(got, "$oncall") || !strings.Contains(got, "Dash") || !strings.Contains(got, "url") {
		t.Fatalf("format = %q, want $oncall/Dash/url", got)
	}
	// The opaque internal resource id must NOT be rendered — the model echoes this verbatim.
	if strings.Contains(got, "r_1") {
		t.Fatalf("format leaked the internal resource id: %q", got)
	}
	// Slug fallback when no alias; still no id.
	got = formatResourceLine(&client.Resource{ResourceID: "r_2", Slug: "staging", Type: client.ResourceTypeTunnel}, "")
	if !strings.Contains(got, "$staging") || strings.Contains(got, "r_2") {
		t.Fatalf("slug fallback = %q, want $staging without the id", got)
	}
	// The channel alias wins, and names a resource that has NEITHER intrinsic alias NOR
	// slug (the agent-protected-URL shape) — which would otherwise render handle-less.
	got = formatResourceLine(&client.Resource{ResourceID: "r_url2", Type: "url", Description: "Deploy"}, "deploydash")
	if !strings.Contains(got, "$deploydash") || strings.Contains(got, "r_url2") {
		t.Fatalf("channel-alias precedence = %q, want $deploydash without the id", got)
	}
}
