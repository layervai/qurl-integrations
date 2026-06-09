package internal

import (
	"bytes"
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
	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
	"github.com/layervai/qurl-integrations/shared/client"
)

// qurlBackendServer is an httptest qURL API returning canned resources + quota.
// /v1/resources honors the ?slug= filter so the ResolveToken slug branch can be
// exercised.
func qurlBackendServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == testResourcesPath && r.URL.Query().Get("slug") == "staging":
			_, _ = w.Write([]byte(`{"data":[{"resource_id":"r_2","slug":"staging","type":"tunnel","description":"Staging dashboard"}]}`))
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
	if !strings.Contains(out, "$oncall") || !strings.Contains(out, "r_1") {
		t.Fatalf("aliases output = %q", out)
	}
}

func TestAgentBackend_ListResources_ScopedToChannel(t *testing.T) {
	b, _ := newBackendUnderTest(t, true)
	out, err := b.ListResources(context.Background(), backendTC())
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	// r_1 is in the channel's allowed set; r_9 is not and must not leak.
	if !strings.Contains(out, "r_1") {
		t.Fatalf("expected r_1 in output: %q", out)
	}
	if strings.Contains(out, "r_9") || strings.Contains(out, "Other channel") {
		t.Fatalf("a resource outside the channel scope leaked: %q", out)
	}
}

func TestAgentBackend_ListResources_PaginatesPastFirstPage(t *testing.T) {
	// allowed = {r_1, r_2}; r_2 only appears on page 2. The listing is
	// workspace-wide, so filtering only page 1 would silently drop r_2.
	b, _ := newBackendUnderTest(t, false)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("cursor") == "" {
			_, _ = w.Write([]byte(`{"data":[{"resource_id":"r_1","alias":"oncall","type":"url"},{"resource_id":"r_9","type":"tunnel"}],"meta":{"has_more":true,"next_cursor":"c2"}}`))
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
	if !strings.Contains(out, "r_1") || !strings.Contains(out, "r_2") {
		t.Fatalf("pagination dropped a reachable resource on page 2: %q", out)
	}
	if strings.Contains(out, "r_9") {
		t.Fatalf("out-of-scope resource leaked: %q", out)
	}
}

func TestAgentBackend_ResolveToken(t *testing.T) {
	b, _ := newBackendUnderTest(t, true)
	ctx := context.Background()

	alias, err := b.ResolveToken(ctx, backendTC(), "$oncall")
	if err != nil || !strings.Contains(alias, "r_1") {
		t.Fatalf("alias resolve: %q err=%v", alias, err)
	}
	slug, err := b.ResolveToken(ctx, backendTC(), "staging")
	if err != nil || !strings.Contains(slug, "r_2") {
		t.Fatalf("slug resolve: %q err=%v", slug, err)
	}
	ghost, err := b.ResolveToken(ctx, backendTC(), "ghost")
	if err != nil || !strings.Contains(ghost, "doesn't resolve") {
		t.Fatalf("ghost resolve: %q err=%v", ghost, err)
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

func TestAgentBackend_NilStoreIsGraceful(t *testing.T) {
	b := &agentBackend{}
	for _, fn := range []func() (string, error){
		func() (string, error) { return b.ListResources(context.Background(), backendTC()) },
		func() (string, error) { return b.ListAliases(context.Background(), backendTC()) },
		func() (string, error) { return b.ResolveToken(context.Background(), backendTC(), "x") },
	} {
		out, err := fn()
		if err != nil || out != agentBackendUnconfigured {
			t.Fatalf("nil store should be graceful, got %q err=%v", out, err)
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
	got := formatResourceLine(&client.Resource{ResourceID: "r_1", Alias: "oncall", Type: "url", Description: "Dash"})
	if !strings.Contains(got, "$oncall") || !strings.Contains(got, "Dash") || !strings.Contains(got, "url") || !strings.Contains(got, "r_1") {
		t.Fatalf("format = %q", got)
	}
	// Slug fallback when no alias.
	got = formatResourceLine(&client.Resource{ResourceID: "r_2", Slug: "staging", Type: "tunnel"})
	if !strings.Contains(got, "$staging") {
		t.Fatalf("slug fallback = %q", got)
	}
}
