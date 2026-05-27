package internal

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
	"github.com/layervai/qurl-integrations/shared/client"
)

const (
	testTunnelSlug       = "prod-dashboard"
	testTunnelResourceID = "r_prod_dash01"
	testTunnelInstallCmd = "tunnel install " + testTunnelSlug
)

func freezeTunnelBootstrapNow(t *testing.T, now time.Time) {
	t.Helper()
	previous := tunnelBootstrapNow
	tunnelBootstrapNow = func() time.Time { return now }
	t.Cleanup(func() { tunnelBootstrapNow = previous })
}

func TestParseTunnelInstall(t *testing.T) {
	cases := []struct {
		name      string
		text      string
		wantErr   bool
		wantSlug  string
		wantAlias string
		wantPort  int
	}{
		{name: "minimal", text: testTunnelInstallCmd, wantSlug: testTunnelSlug, wantAlias: testTunnelSlug, wantPort: defaultTunnelLocalPort},
		{name: "port and alias", text: testTunnelInstallCmd + " port:9090 alias:$dash", wantSlug: testTunnelSlug, wantAlias: "dash", wantPort: 9090},
		{name: "alias without sigil", text: testTunnelInstallCmd + " alias:dash", wantSlug: testTunnelSlug, wantAlias: "dash", wantPort: defaultTunnelLocalPort},
		{name: "bad slug uppercase", text: "tunnel install Prod", wantErr: true},
		{name: "bad port", text: testTunnelInstallCmd + " port:70000", wantErr: true},
		{name: "unknown option", text: testTunnelInstallCmd + " mode:fast", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, msg := parseTunnelInstall(tc.text)
			if tc.wantErr {
				if msg == "" {
					t.Fatalf("expected rejection, got %+v", got)
				}
				return
			}
			if msg != "" {
				t.Fatalf("unexpected rejection: %s", msg)
			}
			if got.Slug != tc.wantSlug || got.Alias != tc.wantAlias || got.LocalPort != tc.wantPort {
				t.Errorf("got %+v, want slug=%q alias=%q port=%d", got, tc.wantSlug, tc.wantAlias, tc.wantPort)
			}
		})
	}
}

func TestTunnelInstallCreatesResourceBindsAliasAndMintsBootstrapKey(t *testing.T) {
	now := time.Date(2026, 5, 27, 4, 30, 0, 0, time.UTC)
	freezeTunnelBootstrapNow(t, now)

	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	var resourceBody map[string]any
	var apiKeyBody map[string]any
	var idempotencyKey string
	ts.addCustomer(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&resourceBody); err != nil {
			t.Fatalf("decode resource body: %v", err)
		}
		respondQURLEnvelope(t, w, map[string]any{
			testKeyResourceID:   testTunnelResourceID,
			testKeyType:         client.ResourceTypeTunnel,
			testKeySlug:         testTunnelSlug,
			testKeyStatus:       client.StatusActive,
			"knock_resource_id": "qurl-tunnel-server",
		})
	})
	ts.addCustomer(http.MethodPost, "/v1/api-keys", func(w http.ResponseWriter, r *http.Request) {
		idempotencyKey = r.Header.Get(client.HeaderIdempotencyKey)
		if err := json.NewDecoder(r.Body).Decode(&apiKeyBody); err != nil {
			t.Fatalf("decode api key body: %v", err)
		}
		respondQURLEnvelope(t, w, map[string]any{
			"key_id":      "key_tunnel_bootstrap",
			"api_key":     "lv_live_test_bootstrap",
			"name":        "Slack tunnel bootstrap " + testTunnelSlug,
			"scopes":      []string{"qurl:agent", "qurl:write"},
			testKeyStatus: client.StatusActive,
			"purpose":     client.APIKeyPurposeTunnelBootstrap,
			"tunnel_slug": testTunnelSlug,
			"expires_at":  "2026-05-28T00:00:00Z",
		})
	})

	h := newAdminTestHandler(t, ts)
	h.cfg.TunnelImage = "ghcr.io/layervai/qurl-reverse-tunnel-client:v-test"
	h.SetAliasStore(h.cfg.AdminStore)
	inv := newAdminSlashInvoker(t, h)
	status, ack := inv.invokeAdmin(testTunnelInstallCmd+" port:9090", testAdminTeamID, testAdminUserID)
	var asyncEnvelope map[string]string
	if err := json.Unmarshal(inv.captured.waitForBody(t, 2*time.Second), &asyncEnvelope); err != nil {
		t.Fatalf("unmarshal tunnel install response_url body: %v", err)
	}
	async := asyncEnvelope[respFieldText]

	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(ack, "Working") {
		t.Fatalf("ack = %q, want async working copy", ack)
	}
	if asyncEnvelope[respFieldResponseType] != respTypeEphemeral {
		t.Fatalf("response_url response_type = %q, want ephemeral", asyncEnvelope[respFieldResponseType])
	}
	if resourceBody[testKeyType] != client.ResourceTypeTunnel || resourceBody[testKeySlug] != testTunnelSlug || resourceBody["find_or_create"] != true {
		t.Errorf("resource body = %+v, want tunnel find-or-create slug", resourceBody)
	}
	if apiKeyBody["purpose"] != client.APIKeyPurposeTunnelBootstrap || apiKeyBody["tunnel_slug"] != testTunnelSlug || apiKeyBody["expires_in"] != tunnelBootstrapTTL {
		t.Errorf("api key body = %+v, want constrained tunnel bootstrap key", apiKeyBody)
	}
	if idempotencyKey == "" {
		t.Error("Idempotency-Key header was empty")
	}
	wantIdempotencyKey := tunnelBootstrapIdempotencyKey(testAdminTeamID, "C_test", testAdminUserID, testTunnelSlug, now)
	if idempotencyKey != wantIdempotencyKey {
		t.Fatalf("Idempotency-Key = %q, want %q", idempotencyKey, wantIdempotencyKey)
	}
	for _, want := range []string{
		"Tunnel `" + testTunnelSlug + "` is ready.",
		"lv_live_test_bootstrap",
		"QURL_TUNNEL_SLUG=" + testTunnelSlug,
		"local_port: 9090",
		"WEB_CONTAINER=YOUR_WEB_CONTAINER_NAME",
		`--network "container:${WEB_CONTAINER}"`,
		"ghcr.io/layervai/qurl-reverse-tunnel-client:v-test",
		"expires at 2026-05-28T00:00:00Z",
		"Delete this Slack message once the sidecar is running",
		"/qurl get $" + testTunnelSlug,
	} {
		if !strings.Contains(async, want) {
			t.Errorf("async reply missing %q:\n%s", want, async)
		}
	}
	for _, forbidden := range []string{"connect.layerv", "proxy.layerv", "frps-", "<web-container>"} {
		if strings.Contains(async, forbidden) {
			t.Errorf("async reply leaked %q:\n%s", forbidden, async)
		}
	}
	gotRID, found, err := h.cfg.AdminStore.LookupChannelAlias(context.Background(), testAdminTeamID, "C_test", testTunnelSlug)
	if err != nil {
		t.Fatalf("LookupChannelAlias: %v", err)
	}
	if !found || gotRID != testTunnelResourceID {
		t.Fatalf("alias lookup = (%q, %v), want (%s, true)", gotRID, found, testTunnelResourceID)
	}
}

func TestTunnelInstallRejectsMissingPlaintextBootstrapKey(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.addCustomer(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		respondQURLEnvelope(t, w, map[string]any{
			testKeyResourceID: testTunnelResourceID,
			testKeyType:       client.ResourceTypeTunnel,
			testKeySlug:       testTunnelSlug,
			testKeyStatus:     client.StatusActive,
		})
	})
	ts.addCustomer(http.MethodPost, "/v1/api-keys", func(w http.ResponseWriter, _ *http.Request) {
		respondQURLEnvelope(t, w, map[string]any{
			"key_id":      "key_tunnel_bootstrap",
			"api_key":     "",
			"purpose":     client.APIKeyPurposeTunnelBootstrap,
			"tunnel_slug": testTunnelSlug,
		})
	})

	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	_, _, async := newAdminSlashInvoker(t, h).invokeAdminAsync(testTunnelInstallCmd, testAdminTeamID, testAdminUserID)

	if !strings.Contains(async, "did not return a bootstrap key") {
		t.Fatalf("async reply = %q, want missing-plaintext copy", async)
	}
}

func TestTunnelInstallRetryRemintsWhenAliasAlreadyMatches(t *testing.T) {
	now := time.Date(2026, 5, 27, 4, 30, 0, 0, time.UTC)
	freezeTunnelBootstrapNow(t, now)

	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	var apiKeyHits int
	var idempotencyKeys []string
	ts.addCustomer(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		respondQURLEnvelope(t, w, map[string]any{
			testKeyResourceID: testTunnelResourceID,
			testKeyType:       client.ResourceTypeTunnel,
			testKeySlug:       testTunnelSlug,
			testKeyStatus:     client.StatusActive,
		})
	})
	ts.addCustomer(http.MethodPost, "/v1/api-keys", func(w http.ResponseWriter, r *http.Request) {
		idempotencyKeys = append(idempotencyKeys, r.Header.Get(client.HeaderIdempotencyKey))
		apiKeyHits++
		if apiKeyHits == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"title":"boom","status":500}}`))
			return
		}
		respondQURLEnvelope(t, w, map[string]any{
			"key_id":      "key_tunnel_bootstrap",
			"api_key":     "lv_live_retry_bootstrap",
			"purpose":     client.APIKeyPurposeTunnelBootstrap,
			"tunnel_slug": testTunnelSlug,
			"expires_at":  "2026-05-28T00:00:00Z",
		})
	})

	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)

	_, _, first := newAdminSlashInvoker(t, h).invokeAdminAsync(testTunnelInstallCmd, testAdminTeamID, testAdminUserID)
	if !strings.Contains(first, "Failed to mint") {
		t.Fatalf("first async reply = %q, want mint failure", first)
	}
	_, _, second := newAdminSlashInvoker(t, h).invokeAdminAsync(testTunnelInstallCmd, testAdminTeamID, testAdminUserID)
	if !strings.Contains(second, "lv_live_retry_bootstrap") || !strings.Contains(second, "already bound") {
		t.Fatalf("second async reply = %q, want successful remint against existing alias", second)
	}
	if apiKeyHits != 2 {
		t.Fatalf("api key hits = %d, want 2", apiKeyHits)
	}
	if len(idempotencyKeys) != 2 || idempotencyKeys[0] == "" || idempotencyKeys[0] != idempotencyKeys[1] {
		t.Fatalf("idempotency keys = %v, want same non-empty retry key", idempotencyKeys)
	}
	nextHourKey := tunnelBootstrapIdempotencyKey(testAdminTeamID, "C_test", testAdminUserID, testTunnelSlug, now.Add(time.Hour))
	if nextHourKey == idempotencyKeys[0] {
		t.Fatal("next-hour tunnel bootstrap idempotency key matched current-hour key")
	}
}

func TestEnsureTunnelAliasRecoversConcurrentSameResourceBind(t *testing.T) {
	ts := newAdminTestServers(t)
	store := newStoreFromFake(t, ts.ddb, ts.tableNames, nil)
	h := NewHandler(Config{AdminStore: store})
	h.aliasStore = raceBindAliasStore{store: store}

	status, err := h.ensureTunnelAlias(context.Background(), testAdminTeamID, "C_test", testTunnelSlug, testTunnelResourceID)
	if err != nil {
		t.Fatalf("ensureTunnelAlias: %v", err)
	}
	if !strings.Contains(status, "already bound") {
		t.Fatalf("status = %q, want already-bound recovery copy", status)
	}
}

func TestTunnelInstallRefusesExistingDifferentAliasBeforeMintingKey(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.seedPolicyAliasBindings(t, testAdminTeamID, "C_test", map[string]string{testTunnelSlug: "r_other"})

	var apiKeyHits int
	ts.addCustomer(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		respondQURLEnvelope(t, w, map[string]any{
			testKeyResourceID: testTunnelResourceID,
			testKeyType:       client.ResourceTypeTunnel,
			testKeySlug:       testTunnelSlug,
			testKeyStatus:     client.StatusActive,
		})
	})
	ts.addCustomer(http.MethodPost, "/v1/api-keys", func(w http.ResponseWriter, _ *http.Request) {
		apiKeyHits++
		w.WriteHeader(http.StatusInternalServerError)
	})

	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	_, _, async := newAdminSlashInvoker(t, h).invokeAdminAsync(testTunnelInstallCmd, testAdminTeamID, testAdminUserID)

	if !strings.Contains(async, "already bound") {
		t.Fatalf("async reply = %q, want already-bound refusal", async)
	}
	if apiKeyHits != 0 {
		t.Fatalf("api key route hit %d times; bootstrap key must not be minted when alias bind fails", apiKeyHits)
	}
}

type raceBindAliasStore struct {
	store *slackdata.Store
}

func (r raceBindAliasStore) BindChannelAlias(ctx context.Context, teamID, channelID, aliasName, resourceID string) error {
	if err := r.store.BindChannelAlias(ctx, teamID, channelID, aliasName, resourceID); err != nil {
		return err
	}
	return slackdata.ErrAliasAlreadyBound
}

func (r raceBindAliasStore) UnbindChannelAlias(ctx context.Context, teamID, channelID, aliasName string) error {
	return r.store.UnbindChannelAlias(ctx, teamID, channelID, aliasName)
}

func TestValidateBootstrapAPIKeyForShell(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		key     string
		wantErr bool
	}{
		{name: "current live-ish shape", key: "lv_live_-Pe7wofxOWsLBlOL1hwPPV491dqNJ4zHbNuRvQUaRHQ"},
		{name: "base64 characters allowed", key: "abc+/def=="},
		{name: "colon allowed", key: "prefix:payload.signature"},
		{name: "empty rejected", key: "", wantErr: true},
		{name: "quote rejected", key: "abc'def", wantErr: true},
		{name: "newline rejected", key: "abc\ndef", wantErr: true},
		{name: "del rejected", key: "abc\x7fdef", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateBootstrapAPIKeyForShell(tc.key)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateBootstrapAPIKeyForShell(%q) err=%v, wantErr=%v", tc.key, err, tc.wantErr)
			}
		})
	}
}

func TestShellSingleQuote(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want string
	}{
		{in: "", want: "''"},
		{in: "plain", want: "'plain'"},
		{in: "$HOME && rm -rf /", want: "'$HOME && rm -rf /'"},
		{in: "a'b", want: `'a'"'"'b'`},
		{in: "line\nbreak", want: "'line\nbreak'"},
	}
	for _, tc := range cases {
		if got := shellSingleQuote(tc.in); got != tc.want {
			t.Errorf("shellSingleQuote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSlackCodeBlockPanicsOnNestedFence(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("slackCodeBlock did not panic on nested fence")
		}
	}()
	_ = slackCodeBlock("sh", "echo before\n```inner\n```")
}

func TestSlackCodeBlock(t *testing.T) {
	t.Parallel()
	got := slackCodeBlock("sh", "echo ok")
	if strings.Count(got, "```") != 2 {
		t.Fatalf("slackCodeBlock fences = %q, want one opening and one closing fence", got)
	}
}

func respondQURLEnvelope(t *testing.T, w http.ResponseWriter, data any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"data": data,
		"meta": map[string]string{"request_id": "req_test"},
	}); err != nil {
		t.Fatalf("encode qurl envelope: %v", err)
	}
}
