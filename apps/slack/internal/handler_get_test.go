package internal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/shared/client"
)

const (
	testAPIErrorCodeRateLimited = "rate_limited"
	testKeyMeta                 = "meta"
	testKeyRequestID            = "request_id"
)

// writeCreateFixture writes a POST /v1/qurls success envelope.
func writeCreateFixture(t *testing.T, w http.ResponseWriter, link, resourceID string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	body := map[string]any{
		testKeyData: map[string]any{
			testKeyResourceID: resourceID,
			"qurl_link":       link,
		},
	}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Fatalf("encode: %v", err)
	}
}

// writeAPIError writes an RFC-7807-shaped error envelope at the
// given status code.
func writeAPIError(t *testing.T, w http.ResponseWriter, status int, code, title string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body := map[string]any{
		testKeyError: map[string]any{
			"status":     status,
			"code":       code,
			testKeyTitle: title,
		},
	}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Fatalf("encode error: %v", err)
	}
}

// TestHandleGet_HappyPath fences the canonical /qurl get flow:
// channel-scoped alias resolution → rate-limit OK → mint → channel
// ephemeral reply carrying the qURL link.
func TestHandleGet_HappyPath(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "prod-db", []string{testResourceIDFix})
	ts.addCustomer("POST", mintByTestResourcePath, func(w http.ResponseWriter, _ *http.Request) {
		writeCreateFixture(t, w, "https://qurl.link/abc", testResourceIDFix)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	status, ack, async := inv.invokeAdminAsync("get $prod-db", testAdminTeamID, testAdminUserID)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if ack != ackWorkingOnIt {
		t.Errorf("ack = %q, want %q", ack, ackWorkingOnIt)
	}
	if !strings.Contains(async, "https://qurl.link/abc") {
		t.Errorf("async reply missing link: %q", async)
	}
}

// TestHandleGet_AliasNotFound fences the no-binding path on a COLD
// channel (no channel_policies row → empty allow-set): when the
// channel's alias_bindings map has no entry for the requested alias
// (no row, missing map, or missing key), getWork surfaces the
// "not configured for this channel" copy that points the user at
// their Slack admin, and never reaches the mint.
//
// Post-#534, an empty allow-set short-circuits the slug/alias fallback
// BEFORE the upstream GET /v1/resources hop (see resolveTokenForGet's
// cost note): both fallbacks would be gated out by the empty set anyway,
// so the hop is pure waste and an unmetered probe surface. The registered
// GET /v1/resources handler therefore asserts ZERO hits — the message is
// produced from the DDB allow-set read alone. (The warm-channel slug
// fallback, where the hop DOES run, is fenced by
// TestHandleGet_DollarSlugNotAllowedNonAdmin.)
func TestHandleGet_AliasNotFound(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	var mintHits atomic.Int32
	ts.addCustomer("POST", "/v1/qurls", func(w http.ResponseWriter, _ *http.Request) {
		mintHits.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	var listHits atomic.Int32
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		listHits.Add(1)
		writeResourceListFixture(t, w, []map[string]any{}, "", false)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("get $missing", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "`$missing` is not configured for this channel") {
		t.Errorf("async reply missing not-configured message: %q", async)
	}
	if !strings.Contains(async, "contact your Slack admin") {
		t.Errorf("async reply missing admin-contact fallback: %q", async)
	}
	if listHits.Load() != 0 {
		t.Errorf("upstream GET /v1/resources reached on a cold channel (hits = %d) — #534 cold-channel short-circuit regressed", listHits.Load())
	}
	if mintHits.Load() != 0 {
		t.Errorf("mint reached despite alias-not-found (hits = %d)", mintHits.Load())
	}
}

// TestHandleGet_ChannelRejectionsShareCopyModuloToken fences the soft
// anti-enumeration posture from issue #540: a user must not be able to tell
// "this token does not resolve" from "this slug exists but is not exposed in
// this channel" by a changed verb in the rejection copy. The sibling
// TestHandleGet_DollarSlugNotAllowedNonAdmin fences the blocked-slug path
// alone; this test pins byte-for-byte parity against the cold missing-token
// path.
func TestHandleGet_ChannelRejectionsShareCopyModuloToken(t *testing.T) {
	normalizeToken := func(reply, token string) string {
		t.Helper()
		quotedToken := "`$" + token + "`"
		if !strings.Contains(reply, quotedToken) {
			t.Fatalf("reply %q did not echo token %q", reply, quotedToken)
		}
		return strings.ReplaceAll(reply, quotedToken, "`$<token>`")
	}

	missingTS := newAdminTestServers(t)
	missingTS.seedNonAdmin(t)
	var missingListHits atomic.Int32
	missingTS.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		missingListHits.Add(1)
		writeResourceListFixture(t, w, []map[string]any{}, "", false)
	})
	missingH := newAdminTestHandler(t, missingTS)
	_, _, missingReply := newAdminSlashInvoker(t, missingH).invokeAdminAsync("get $missing", testAdminTeamID, testAdminUserID)

	blockedTS := newAdminTestServers(t)
	blockedTS.seedNonAdmin(t)
	blockedTS.seedPolicySet(t, testAdminTeamID, "C_test", "", []string{"r_other_alloc"})
	var blockedListHits atomic.Int32
	blockedTS.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		blockedListHits.Add(1)
		writeTunnelSlugResourceFixture(t, w)
	})
	var mintHits atomic.Int32
	blockedTS.addCustomer("POST", mintByTestResourcePath, func(w http.ResponseWriter, _ *http.Request) {
		mintHits.Add(1)
		writeCreateFixture(t, w, "https://qurl.link/should-not", testResourceIDFix)
	})
	blockedH := newAdminTestHandler(t, blockedTS)
	_, _, blockedReply := newAdminSlashInvoker(t, blockedH).invokeAdminAsync("get $"+testTunnelSlug, testAdminTeamID, testAdminUserID)

	if missingListHits.Load() != 0 {
		t.Fatalf("missing-token branch reached upstream GET /v1/resources despite cold-channel short-circuit (hits = %d)", missingListHits.Load())
	}
	if blockedListHits.Load() == 0 {
		t.Fatal("blocked-slug branch did not reach upstream slug lookup; warm-channel seed broke")
	}
	if mintHits.Load() != 0 {
		t.Fatalf("mint reached despite blocked slug (hits = %d)", mintHits.Load())
	}
	missingNormalized := normalizeToken(missingReply, "missing")
	blockedNormalized := normalizeToken(blockedReply, testTunnelSlug)
	if blockedNormalized != missingNormalized {
		t.Fatalf("channel rejection copy drifted:\nmissing: %q\nblocked: %q", missingNormalized, blockedNormalized)
	}
}

// TestHandleGet_UnknownSlugColdChannelNoUpstreamHop is the direct
// regression fence for #534: the unmetered cold-channel probe surface.
//
// On a cold channel (workspace seeded but NO channel_policies row, so an
// empty allow-set), repeated unknown-slug gets — `get $typo1`, `$typo2`,
// `$typo3` — from one user must each be answered from the DDB allow-set
// read ALONE, firing ZERO upstream GET /v1/resources hops. Before the fix
// each miss spent one upstream slug lookup against the workspace API key
// AHEAD of the per-user rate-limit gate, so a fat-fingering (or hostile)
// user could fan out unmetered probes. We assert the counter stays at 0
// across all three, the not-configured copy is returned each time, and the
// mint route is never hit.
//
// Complements TestHandleGet_AliasNotFound (single cold-channel miss) by
// pinning the *fan-out* case the issue describes, and stands opposite
// TestHandleGet_DollarSlugNotAllowedNonAdmin, which proves the hop DOES
// still run for a WARM channel (non-empty allow-set) — i.e. the
// short-circuit is scoped strictly to the empty set.
func TestHandleGet_UnknownSlugColdChannelNoUpstreamHop(t *testing.T) {
	ts := newAdminTestServers(t)
	// Cold channel: seed the workspace (so AdminStore is usable and the
	// caller resolves) but NO channel policy/exposure → empty allow-set.
	// seedNonAdmin (vs TestHandleGet_AliasNotFound's seedAdmin) is the
	// deliberate pick: the allow-set gate is channel-scoped with no admin
	// bypass (allowedResourceIDsForGet doc), so the more security-relevant
	// caller to pin against the cold-channel probe surface is a non-admin.
	// The two tests together show both an admin and a non-admin caller hit
	// the same short-circuit.
	ts.seedNonAdmin(t)
	var listHits atomic.Int32
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		listHits.Add(1)
		writeResourceListFixture(t, w, []map[string]any{}, "", false)
	})
	var mintHits atomic.Int32
	ts.addCustomerPrefix("POST", "/v1/resources/", func(w http.ResponseWriter, _ *http.Request) {
		mintHits.Add(1)
		writeCreateFixture(t, w, "https://qurl.link/should-not", testResourceIDFix)
	})
	h := newAdminTestHandler(t, ts)

	// A fresh invoker per call: each spins its own response_url capture
	// (waitForBody returns the FIRST recorded body, so a reused invoker
	// would read back call #1's reply for every call). The shared handler
	// and httptest servers keep listHits/mintHits accumulating across all
	// three — which is exactly what the fan-out assertion below checks.
	for _, typo := range []string{"typo1", "typo2", "typo3"} {
		inv := newAdminSlashInvoker(t, h)
		_, _, async := inv.invokeAdminAsync("get $"+typo, testAdminTeamID, testAdminUserID)
		if !strings.Contains(async, "`$"+typo+"` is not configured for this channel") {
			t.Errorf("get $%s: async reply missing not-configured copy: %q", typo, async)
		}
	}
	if listHits.Load() != 0 {
		t.Errorf("cold-channel unknown-slug gets hit the upstream GET /v1/resources %d time(s) — #534 unmetered probe surface regressed", listHits.Load())
	}
	if mintHits.Load() != 0 {
		t.Errorf("mint reached on cold-channel unknown-slug gets (hits = %d)", mintHits.Load())
	}
}

// TestHandleGet_MintTunnelDisabled fences the 403/tunnel_disabled
// mint error → user-facing "Protected resources are not yet enabled"
// reply.
func TestHandleGet_MintTunnelDisabled(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "prod-db", []string{testResourceIDFix})
	ts.addCustomer("POST", mintByTestResourcePath, func(w http.ResponseWriter, _ *http.Request) {
		writeAPIError(t, w, http.StatusForbidden, "tunnel_disabled", "Forbidden")
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("get $prod-db", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "Protected resources are not yet enabled") {
		t.Errorf("async reply missing tunnel-disabled message: %q", async)
	}
}

// TestHandleGet_MintRateLimit fences the 429 mint error with a
// retry-after header plus opaque request ID support handle.
func TestHandleGet_MintRateLimit(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "prod-db", []string{testResourceIDFix})
	ts.addCustomer("POST", mintByTestResourcePath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		body := map[string]any{
			testKeyError: map[string]any{
				"status":     http.StatusTooManyRequests,
				"code":       testAPIErrorCodeRateLimited,
				testKeyTitle: "Too Many Requests from internal API",
			},
			testKeyMeta: map[string]any{testKeyRequestID: "req_get_rate"},
		}
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Fatalf("encode error: %v", err)
		}
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("get $prod-db", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "Rate limit hit") {
		t.Errorf("async reply missing rate-limit message: %q", async)
	}
	if !strings.Contains(async, "30s") {
		t.Errorf("async reply missing 30s retry hint: %q", async)
	}
	if !strings.Contains(async, "req_get_rate") {
		t.Errorf("async reply missing request ID: %q", async)
	}
	if strings.Contains(async, "internal API") {
		t.Errorf("async reply leaked upstream rate-limit title: %q", async)
	}
}

func TestHandleGet_AdminStoreRateLimitDenialShowsRetryHint(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "prod-db", []string{testResourceIDFix})
	var mintHits atomic.Int32
	ts.addCustomer("POST", mintByTestResourcePath, func(w http.ResponseWriter, _ *http.Request) {
		mintHits.Add(1)
		writeCreateFixture(t, w, "https://qurl.link/abc", testResourceIDFix)
	})
	h := newAdminTestHandler(t, ts)
	h.cfg.AdminStore.Now = func() time.Time {
		return time.Date(2026, 6, 17, 12, 42, 0, 0, time.UTC)
	}
	inv := newAdminSlashInvoker(t, h)
	for i := 0; i < 30; i++ {
		allowed, retry, err := h.cfg.AdminStore.CheckRateLimit(context.Background(), testAdminUserID, testAdminTeamID)
		if err != nil {
			t.Fatalf("prefill rate limit %d: %v", i+1, err)
		}
		if !allowed || retry != 0 {
			t.Fatalf("prefill rate limit %d allowed=%v retry=%s, want allowed/no retry", i+1, allowed, retry)
		}
	}
	_, _, async := inv.invokeAdminAsync("get $prod-db", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "Rate limit hit") {
		t.Fatalf("denied async reply missing rate-limit copy: %q", async)
	}
	if !strings.Contains(async, "Try again in 18m") {
		t.Fatalf("denied async reply missing retry-after hint: %q", async)
	}
	if got := mintHits.Load(); got != 0 {
		t.Fatalf("mint hits = %d, want denied request not to reach qurl-service", got)
	}
}

func TestHandleGet_AdminStoreRateLimitErrorFailsClosed(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "prod-db", []string{testResourceIDFix})
	ts.ddb.SetUpdateItemErr(ts.tableNames.channelPolicy, errors.New("ddb down"))
	var mintHits atomic.Int32
	ts.addCustomer("POST", mintByTestResourcePath, func(w http.ResponseWriter, _ *http.Request) {
		mintHits.Add(1)
		writeCreateFixture(t, w, "https://qurl.link/should-not", testResourceIDFix)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("get $prod-db", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, commonGetMintFailedMessage) {
		t.Fatalf("async reply = %q, want generic mint failure", async)
	}
	if strings.Contains(async, "Rate limit hit") {
		t.Fatalf("async reply = %q, want DDB-error path not quota-denied copy", async)
	}
	if got := mintHits.Load(); got != 0 {
		t.Fatalf("mint hits = %d, want rate-limit store error not to reach qurl-service", got)
	}
}

// TestHandleGet_MintTransportError fences 5xx and bare network
// errors → user-facing "Could not reach qURL. Please try again."
// (mapMintError's serviceUnreachableMessage branch).
func TestHandleGet_MintTransportError(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "prod-db", []string{testResourceIDFix})
	ts.addCustomer("POST", mintByTestResourcePath, func(w http.ResponseWriter, _ *http.Request) {
		writeAPIError(t, w, http.StatusBadGateway, "upstream_error", "Bad Gateway")
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("get $prod-db", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "Could not reach qURL") {
		t.Errorf("async reply missing service-unreachable message: %q", async)
	}
}

// TestHandleGet_MissingAlias fences the bare-`get` surface. The
// slash-command body has `get` with no positional arg → the handler
// replies synchronously with the `$<id>|$<alias>` Usage hint (the
// getUsageMessage copy) and never kicks off async work.
func TestHandleGet_MissingAlias(t *testing.T) {
	ts := newAdminTestServers(t)
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	status, ack := inv.invokeAdmin("get", testAdminTeamID, testAdminUserID)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(ack, "$id|$alias") {
		t.Errorf("ack missing get usage hint: %q", ack)
	}
	if !strings.Contains(ack, "/qurl list") {
		t.Errorf("ack missing /qurl list pointer: %q", ack)
	}
}

// TestHandleGet_LoneSigil fences the `get $` surface — a lone `$` with no
// slug/alias body. parseAliasToken returns ErrEmptyResource for the bare
// sigil, so `get $` reaches the same getUsageMessage hint as bare `get` but
// via a DISTINCT parse path (the ErrEmptyResource branch, not the no-arg
// one), and likewise never kicks off async work.
func TestHandleGet_LoneSigil(t *testing.T) {
	ts := newAdminTestServers(t)
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	status, ack := inv.invokeAdmin("get $", testAdminTeamID, testAdminUserID)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(ack, "$id|$alias") {
		t.Errorf("ack missing get usage hint: %q", ack)
	}
	if !strings.Contains(ack, "/qurl list") {
		t.Errorf("ack missing /qurl list pointer: %q", ack)
	}
}

// TestHandleGet_AdminStoreNil fences the fail-closed posture when
// AdminStore is nil (sandbox / no-DDB deployment) and the user
// requested the alias form: the channel-scoped lookup can't run, so
// the user sees the "qURL admin features are not yet configured"
// message that routes them to a workspace admin. The customer API is
// never reached for the mint.
func TestHandleGet_AdminStoreNil(t *testing.T) {
	ts := newAdminTestServers(t)
	var mintHits atomic.Int32
	ts.addCustomer("POST", "/v1/qurls", func(w http.ResponseWriter, _ *http.Request) {
		mintHits.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	h := newAdminTestHandler(t, ts)
	// Override AdminStore to nil after construction.
	h.cfg.AdminStore = nil
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("get $prod-db", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "qURL admin features are not yet configured") {
		t.Errorf("async reply missing not-configured message: %q", async)
	}
	if !strings.Contains(async, "contact your Slack admin") {
		t.Errorf("async reply missing admin-contact fallback: %q", async)
	}
	if mintHits.Load() != 0 {
		t.Errorf("mint reached despite nil AdminStore (hits = %d)", mintHits.Load())
	}
}

// TestHandleGet_URLRejected fences that raw URLs are no longer mintable
// through Slack: `/qurl get <url>` is rejected at parse with a friendly
// pointer to slugs/aliases (the synchronous ack, since the parser fails
// before runAsync), and never reaches the mint.
func TestHandleGet_URLRejected(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	var mintHits atomic.Int32
	ts.addCustomer("POST", "/v1/qurls", func(w http.ResponseWriter, _ *http.Request) {
		mintHits.Add(1)
		writeCreateFixture(t, w, "https://qurl.link/should-not", testResourceIDFix)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	status, ack := inv.invokeAdmin("get https://example.com", testAdminTeamID, testAdminUserID)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(ack, "raw URLs aren't supported") {
		t.Errorf("ack missing URL-rejection copy: %q", ack)
	}
	if mintHits.Load() != 0 {
		t.Errorf("mint reached on a rejected URL (hits = %d)", mintHits.Load())
	}
}

// TestHandleGet_URLResourceAliasMints fences the restored URL-resource path:
// `/qurl list` can render a URL resource's Alias as `$docs`, and pasting that
// token into `/qurl get $docs` resolves the existing resource only if it is
// exposed in this channel. The mint uses the resource-scoped endpoint with the
// same short-lived `/qurl get` policy as tunnel resources.
func TestHandleGet_URLResourceAliasMints(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedChannelExposure(t, testAdminTeamID, "C_test", testListResIDURLDocs)
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{{
			testKeyResourceID:  testListResIDURLDocs,
			testKeyType:        client.ResourceTypeURL,
			fAttrAlias:         testListAliasDocs,
			testKeyTargetURL:   testListURLDocs,
			testKeyStatus:      client.StatusActive,
			testKeyDescription: "Docs portal",
		}}, "", false)
	})
	var capturedBody []byte
	ts.addCustomer("POST", "/v1/resources/"+testListResIDURLDocs+"/qurls", func(w http.ResponseWriter, r *http.Request) {
		var err error
		capturedBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read mint body: %v", err)
		}
		writeCreateFixture(t, w, "https://qurl.link/url-docs", testListResIDURLDocs)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("get $"+testListAliasDocs, testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "https://qurl.link/url-docs") {
		t.Errorf("async reply missing URL resource qURL: %q", async)
	}
	if !strings.Contains(async, "(one-time use · link expires in "+resourceLinkExpiryHuman+")") {
		t.Errorf("async reply missing one-time-use/expiry note: %q", async)
	}

	var parsed map[string]any
	if err := json.Unmarshal(capturedBody, &parsed); err != nil {
		t.Fatalf("unmarshal captured body: %v body=%s", err, capturedBody)
	}
	if got, _ := parsed["one_time_use"].(bool); !got {
		t.Errorf("one_time_use = %v, want true", parsed["one_time_use"])
	}
	for _, absent := range []string{"target_url", "resource_id"} {
		if _, ok := parsed[absent]; ok {
			t.Errorf("%s should be absent from URL resource mint body: %v", absent, parsed)
		}
	}
	if got, _ := parsed["expires_in"].(string); got != resourceLinkExpiry {
		t.Errorf("expires_in = %q, want %q", parsed["expires_in"], resourceLinkExpiry)
	}
	if got, _ := parsed["session_duration"].(string); got != resourceSessionDuration {
		t.Errorf("session_duration = %q, want %q", parsed["session_duration"], resourceSessionDuration)
	}
	if got, _ := parsed["max_sessions"].(float64); int(got) != resourceMaxSessions {
		t.Errorf("max_sessions = %v, want %d", parsed["max_sessions"], resourceMaxSessions)
	}
}

// TestHandleGet_URLChannelAliasMints keeps the channel-alias path cheap and
// broad: if an admin binds `$docs` directly to a URL resource ID, `/qurl get
// $docs` mints that resource without requiring a pre-mint resource-list lookup.
func TestHandleGet_URLChannelAliasMints(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedPolicyAliasBindings(t, testAdminTeamID, "C_test", map[string]string{testListAliasDocs: testListResIDURLDocs})
	var capturedBody []byte
	ts.addCustomer("POST", "/v1/resources/"+testListResIDURLDocs+"/qurls", func(w http.ResponseWriter, r *http.Request) {
		var err error
		capturedBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read mint body: %v", err)
		}
		writeCreateFixture(t, w, "https://qurl.link/channel-docs", testListResIDURLDocs)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("get $"+testListAliasDocs, testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "https://qurl.link/channel-docs") {
		t.Errorf("async reply missing URL channel-alias qURL: %q", async)
	}

	var parsed map[string]any
	if err := json.Unmarshal(capturedBody, &parsed); err != nil {
		t.Fatalf("unmarshal captured body: %v body=%s", err, capturedBody)
	}
	if got, _ := parsed["expires_in"].(string); got != resourceLinkExpiry {
		t.Errorf("expires_in = %q, want %q", parsed["expires_in"], resourceLinkExpiry)
	}
}

// TestHandleGet_URLAliasWinsWhenTunnelSlugCollisionIsNotAllowed covers a
// mixed-resource collision: a URL resource alias visible in this channel has
// the same token as a tunnel slug elsewhere. The unallowed tunnel must not
// block the URL alias round-trip advertised by `/qurl list`.
func TestHandleGet_URLAliasWinsWhenTunnelSlugCollisionIsNotAllowed(t *testing.T) {
	const (
		token         = "shared"
		urlResourceID = "r_url_shared1"
		tunnelID      = "r_tunnel_shadow"
	)
	ts := newAdminTestServers(t)
	ts.seedChannelExposure(t, testAdminTeamID, "C_test", urlResourceID)
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: tunnelID, testKeyType: client.ResourceTypeTunnel, testKeySlug: token, testKeyStatus: client.StatusActive},
			{testKeyResourceID: urlResourceID, testKeyType: client.ResourceTypeURL, fAttrAlias: token, testKeyTargetURL: "https://shared.example.com", testKeyStatus: client.StatusActive},
		}, "", false)
	})
	var tunnelMintHits atomic.Int32
	ts.addCustomer("POST", "/v1/resources/"+tunnelID+"/qurls", func(w http.ResponseWriter, _ *http.Request) {
		tunnelMintHits.Add(1)
		writeCreateFixture(t, w, "https://qurl.link/shadow", tunnelID)
	})
	ts.addCustomer("POST", "/v1/resources/"+urlResourceID+"/qurls", func(w http.ResponseWriter, _ *http.Request) {
		writeCreateFixture(t, w, "https://qurl.link/url-shared", urlResourceID)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("get $"+token, testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "https://qurl.link/url-shared") {
		t.Errorf("async reply missing URL alias qURL despite slug collision: %q", async)
	}
	if tunnelMintHits.Load() != 0 {
		t.Errorf("unallowed colliding tunnel slug was minted (hits = %d)", tunnelMintHits.Load())
	}
}

// TestHandleGet_ResourceAliasSkipsUnallowedDuplicate covers the alias
// shadowing edge: if two listed resources share an alias and the first one in
// page order is not exposed in this channel, `/qurl get $alias` must continue
// scanning for a later exposed match rather than reporting "not configured."
func TestHandleGet_ResourceAliasSkipsUnallowedDuplicate(t *testing.T) {
	const (
		shadowID  = "r_url_docs_shadow"
		allowedID = "r_url_docs_ok"
	)
	ts := newAdminTestServers(t)
	ts.seedChannelExposure(t, testAdminTeamID, "C_test", allowedID)
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: shadowID, testKeyType: client.ResourceTypeURL, fAttrAlias: testListAliasDocs, testKeyTargetURL: "https://shadow.example.com", testKeyStatus: client.StatusActive},
			{testKeyResourceID: allowedID, testKeyType: client.ResourceTypeURL, fAttrAlias: testListAliasDocs, testKeyTargetURL: testListURLDocs, testKeyStatus: client.StatusActive},
		}, "", false)
	})
	var shadowMintHits atomic.Int32
	ts.addCustomer("POST", "/v1/resources/"+shadowID+"/qurls", func(w http.ResponseWriter, _ *http.Request) {
		shadowMintHits.Add(1)
		writeCreateFixture(t, w, "https://qurl.link/shadow-docs", shadowID)
	})
	ts.addCustomer("POST", "/v1/resources/"+allowedID+"/qurls", func(w http.ResponseWriter, _ *http.Request) {
		writeCreateFixture(t, w, "https://qurl.link/allowed-docs", allowedID)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("get $"+testListAliasDocs, testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "https://qurl.link/allowed-docs") {
		t.Errorf("async reply missing later allowed duplicate-alias qURL: %q", async)
	}
	if shadowMintHits.Load() != 0 {
		t.Errorf("unallowed duplicate alias resource was minted (hits = %d)", shadowMintHits.Load())
	}
}

// TestHandleGet_ResourceAliasDuplicateAllowedRefusesAmbiguousMint keeps
// duplicate aliases fail-closed when more than one matching resource is exposed
// to the same channel. A slash token cannot disambiguate those rows, and the
// list path must not make an arbitrary page-order choice on click.
func TestHandleGet_ResourceAliasDuplicateAllowedRefusesAmbiguousMint(t *testing.T) {
	const (
		firstID  = "r_url_docs_first"
		secondID = "r_url_docs_second"
	)
	ts := newAdminTestServers(t)
	ts.seedChannelExposure(t, testAdminTeamID, "C_test", firstID, secondID)
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: firstID, testKeyType: client.ResourceTypeURL, fAttrAlias: testListAliasDocs, testKeyTargetURL: testListURLFirst, testKeyStatus: client.StatusActive},
			{testKeyResourceID: secondID, testKeyType: client.ResourceTypeURL, fAttrAlias: testListAliasDocs, testKeyTargetURL: testListURLSecond, testKeyStatus: client.StatusActive},
		}, "", false)
	})
	var mintHits atomic.Int32
	ts.addCustomerPrefix("POST", "/v1/resources/", func(w http.ResponseWriter, _ *http.Request) {
		mintHits.Add(1)
		writeCreateFixture(t, w, "https://qurl.link/unexpected", firstID)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("get $"+testListAliasDocs, testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, ambiguousResourceAliasMessage(testListAliasDocs)) {
		t.Errorf("async reply should explain ambiguous duplicate alias: %q", async)
	}
	if mintHits.Load() != 0 {
		t.Errorf("ambiguous duplicate alias attempted mint (hits = %d)", mintHits.Load())
	}
}

// TestHandleGet_ResourceAliasNotAllowedLooksNotConfigured fences the
// non-enumerating failure shape for resource-alias fallback: finding a listed
// alias that is not protected in this channel collapses to the same user copy as
// a missing token and never attempts a mint.
func TestHandleGet_ResourceAliasNotAllowedLooksNotConfigured(t *testing.T) {
	ts := newAdminTestServers(t)
	// Expose an UNRELATED resource so the channel is "warm" (non-empty
	// allow-set). Without this, the cold-channel short-circuit (#534) returns
	// the not-configured copy before the listed alias is ever resolved, so
	// this test would pass without exercising the resolve-then-reject branch
	// it documents. With the set non-empty, the resource-alias fallback runs,
	// resolves the listed alias, and rejects it because its resource_id is
	// not in the allow-set.
	ts.seedChannelExposure(t, testAdminTeamID, "C_test", "r_other_alloc")
	var listHits atomic.Int32
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		listHits.Add(1)
		writeResourceListFixture(t, w, []map[string]any{{
			testKeyResourceID: testListResIDURLDocs,
			testKeyType:       client.ResourceTypeURL,
			fAttrAlias:        testListAliasDocs,
			testKeyTargetURL:  testListURLDocs,
			testKeyStatus:     client.StatusActive,
		}}, "", false)
	})
	var mintHits atomic.Int32
	ts.addCustomerPrefix("POST", "/v1/resources/", func(w http.ResponseWriter, _ *http.Request) {
		mintHits.Add(1)
		writeCreateFixture(t, w, "https://qurl.link/unexpected", testListResIDURLDocs)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("get $"+testListAliasDocs, testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, noResourceForAliasMessage(testListAliasDocs)) {
		t.Errorf("async reply should use not-configured copy for unallowed resource alias: %q", async)
	}
	// The message above is BYTE-IDENTICAL to the cold-channel short-circuit's
	// (#534), so it alone can't tell whether the resolve-then-reject branch ran
	// or the warm seed silently broke and we fell through the short-circuit.
	// Pin that the alias fallback actually reached upstream: a non-empty
	// allow-set must let GET /v1/resources fire at least once.
	if listHits.Load() == 0 {
		t.Errorf("resource-alias fallback never hit upstream GET /v1/resources — warm-channel seed broke and the test silently regressed to the cold-channel short-circuit")
	}
	if mintHits.Load() != 0 {
		t.Errorf("unallowed resource alias attempted mint (hits = %d)", mintHits.Load())
	}
}

// TestHandleGet_ResourceAliasFallbackIgnoresTunnelAlias keeps the restored
// resource-alias fallback scoped to URL resources. A tunnel's hidden Alias
// field is not the token `/qurl list` advertises when the tunnel has a slug, so
// `/qurl get $alias` should not mint it through the URL fallback.
func TestHandleGet_ResourceAliasFallbackIgnoresTunnelAlias(t *testing.T) {
	const tunnelID = "r_tunnel_hidden_alias"
	ts := newAdminTestServers(t)
	ts.seedChannelExposure(t, testAdminTeamID, "C_test", tunnelID)
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{{
			testKeyResourceID: tunnelID,
			testKeyType:       client.ResourceTypeTunnel,
			testKeySlug:       testListSlugOpsTunnel,
			fAttrAlias:        testListAliasDocs,
			testKeyStatus:     client.StatusActive,
		}}, "", false)
	})
	var mintHits atomic.Int32
	ts.addCustomerPrefix("POST", "/v1/resources/", func(w http.ResponseWriter, _ *http.Request) {
		mintHits.Add(1)
		writeCreateFixture(t, w, "https://qurl.link/unexpected", tunnelID)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("get $"+testListAliasDocs, testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, noResourceForAliasMessage(testListAliasDocs)) {
		t.Errorf("async reply should use not-configured copy for hidden tunnel alias: %q", async)
	}
	if mintHits.Load() != 0 {
		t.Errorf("hidden tunnel alias attempted mint (hits = %d)", mintHits.Load())
	}
}

// TestHandleGet_ResourceIDRejected fences the friendly `$r_<id>` redirect:
// a user pasting a resource-id token (which pre-tunnels-only `/qurl list`
// surfaced) gets the resource-id-specific copy pointing them at the `$slug`,
// NOT the generic invalid-alias error, and the mint is never reached.
func TestHandleGet_ResourceIDRejected(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	var mintHits atomic.Int32
	ts.addCustomer("POST", "/v1/qurls", func(w http.ResponseWriter, _ *http.Request) {
		mintHits.Add(1)
		writeCreateFixture(t, w, "https://qurl.link/should-not", testResourceIDFix)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	status, ack := inv.invokeAdmin("get $r_abc123def01", testAdminTeamID, testAdminUserID)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(ack, "not an internal `r_...` identifier") {
		t.Errorf("ack missing resource-id-rejection copy: %q", ack)
	}
	if mintHits.Load() != 0 {
		t.Errorf("mint reached on a rejected resource id (hits = %d)", mintHits.Load())
	}
}

// TestHandleGet_LegacyURLBindingRefused fences the read-side guard for
// bindings created by the pre-tunnels-only `/qurl set-alias`, which
// stored a raw URL verbatim in alias_bindings. Those rows survive this
// PR; resolving one would hand a URL to the mint call and surface as the
// generic retry error, stranding the user. Instead the resolver detects
// the non-resource-id shape and replies with an actionable "ask an admin
// to re-bind" message, and the mint is never reached.
func TestHandleGet_LegacyURLBindingRefused(t *testing.T) {
	ts := newAdminTestServers(t)
	// A raw URL is the canonical pre-tunnels-only binding value; testAliasURL
	// is reused (vs a fresh literal) only to stay under goconst's threshold.
	ts.seedPolicyAliasBindings(t, testAdminTeamID, "C_test", map[string]string{"legacy": testAliasURL})
	var mintHits atomic.Int32
	// Narrow to the resource-mint family (where a regressed guard would
	// send the would-be mint) rather than a blanket /v1/ catch-all.
	ts.addCustomerPrefix("POST", "/v1/resources/", func(w http.ResponseWriter, _ *http.Request) {
		mintHits.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("get $legacy", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "no longer supported") {
		t.Errorf("async reply missing legacy-binding copy: %q", async)
	}
	if !strings.Contains(async, "re-point it at a resource") {
		t.Errorf("async reply missing re-bind instruction: %q", async)
	}
	if mintHits.Load() != 0 {
		t.Errorf("mint reached despite a legacy URL binding (hits = %d)", mintHits.Load())
	}
}

// TestGetWork_EmptyAliasRefusesToMint locks the unreachable
// empty-alias guard in getWork: a Command that parseGet cannot
// actually produce (neither an alias nor a resource-id token) must hit
// the "refuse to mint" guard and return the distinct internal-error
// copy — NOT fall through to an unauthenticated mint. Drives the guard
// directly since the parser guarantees it can't be reached end-to-end.
func TestGetWork_EmptyAliasRefusesToMint(t *testing.T) {
	t.Parallel()
	h := &Handler{}
	args := getWorkArgs{
		// Alias == "" (a shape parseGet can't produce), so getWork's
		// empty-alias guard fires instead of resolving and minting.
		cmd:       &Command{Subcommand: SubcmdGet},
		teamID:    "T1",
		channelID: "C1",
		userID:    "U1",
	}
	reply, err := h.getWork(context.Background(), slogTestLogger(t), &args)
	if reply != "" {
		t.Errorf("reply = %q, want empty on the refuse-to-mint path", reply)
	}
	var ue *userError
	if !errors.As(err, &ue) {
		t.Fatalf("err = %v (%T), want *userError", err, err)
	}
	if ue.msg != unexpectedGetShapeMessage {
		t.Errorf("err msg = %q, want unexpectedGetShapeMessage (%q)", ue.msg, unexpectedGetShapeMessage)
	}
}

// TestHandleGet_DMVariantRefusedWhenPostDMNil fences the privacy-
// preserving refusal: dm:true asks for the link in a DM (so it does
// NOT leak into channel history). When PostDM is not wired we
// refuse the mint with a user-facing "DM is not configured" copy —
// silently posting the link in-channel would violate the user's
// explicit intent. The mint is NOT burned (no POST /v1/qurls).
func TestHandleGet_DMVariantRefusedWhenPostDMNil(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "prod-db", []string{testResourceIDFix})
	var mintCalls atomic.Int32
	ts.addCustomer("POST", "/v1/qurls", func(w http.ResponseWriter, _ *http.Request) {
		mintCalls.Add(1)
		writeCreateFixture(t, w, "https://qurl.link/should-not-be-minted", testResourceIDFix)
	})
	h := newAdminTestHandler(t, ts)
	// PostDM is nil by default.
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("get $prod-db dm:true", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "DM delivery is not configured") {
		t.Errorf("async reply missing DM-not-configured refusal: %q", async)
	}
	if strings.Contains(async, "https://qurl.link/should-not-be-minted") {
		t.Errorf("async response leaked the link into channel despite dm:true privacy intent: %q", async)
	}
	if mintCalls.Load() != 0 {
		t.Errorf("mint was burned (POST /v1/qurls calls = %d) despite refusal at the dm:true gate; the user paid a quota for a request we couldn't honor", mintCalls.Load())
	}
}

// TestHandleGet_DMVariantPostDMSuccess fences the dm:true happy path:
// the link goes to PostDM, and the channel ephemeral confirms with
// the :incoming_envelope: copy. No link in the channel surface.
func TestHandleGet_DMVariantPostDMSuccess(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "prod-db", []string{testResourceIDFix})
	ts.addCustomer("POST", mintByTestResourcePath, func(w http.ResponseWriter, _ *http.Request) {
		writeCreateFixture(t, w, "https://qurl.link/dm-secret", testResourceIDFix)
	})

	var dmCalls atomic.Int32
	var dmText string
	h := newAdminTestHandler(t, ts)
	h.cfg.PostDM = func(_ context.Context, _, _, _, text string) error {
		dmCalls.Add(1)
		dmText = text
		return nil
	}
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("get $prod-db dm:true", testAdminTeamID, testAdminUserID)
	if dmCalls.Load() != 1 {
		t.Errorf("PostDM calls = %d, want 1", dmCalls.Load())
	}
	if !strings.Contains(dmText, "https://qurl.link/dm-secret") {
		t.Errorf("DM text missing link: %q", dmText)
	}
	if !strings.Contains(async, ":incoming_envelope:") {
		t.Errorf("async reply missing DM-sent confirmation: %q", async)
	}
	if strings.Contains(async, "https://qurl.link/dm-secret") {
		t.Errorf("link leaked to channel ephemeral on dm:true: %q", async)
	}
}

func TestHandleGet_DMVariantMissingScopeMentionsSlackReinstall(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "prod-db", []string{testResourceIDFix})
	ts.addCustomer("POST", mintByTestResourcePath, func(w http.ResponseWriter, _ *http.Request) {
		writeCreateFixture(t, w, "https://qurl.link/dm-scope", testResourceIDFix)
	})

	h := newAdminTestHandler(t, ts)
	h.SetSlackInstallURL("https://slack-bot.example/oauth/slack/install")
	h.cfg.PostDM = func(context.Context, string, string, string, string) error {
		return fmt.Errorf("chat.postMessage: %w", ErrSlackMissingScope)
	}
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("get $prod-db dm:true", testAdminTeamID, testAdminUserID)
	for _, want := range []string{
		"Could not DM you the link",
		"latest qURL Slack app install",
		"<https://slack-bot.example/oauth/slack/install|the qURL Slack install link>",
		"re-run the command",
	} {
		if !strings.Contains(async, want) {
			t.Fatalf("async reply = %q, missing %q", async, want)
		}
	}
	if strings.Contains(async, "https://qurl.link/dm-scope") {
		t.Fatalf("async reply leaked dm:true link after DM failure: %q", async)
	}
}

// TestResourceLinkExpiryConstsInSync is a tripwire: resourceLinkExpiry (the wire
// value sent as expires_in) and resourceLinkExpiryHuman (the Slack reply copy)
// are hand-maintained and must describe the same window. Without this, a bump
// to one (e.g. "1m"→"5m") that forgets the other would silently diverge the
// user-facing copy from the actual admit window. Pin the current pair; whoever
// changes the window updates both consts AND this assertion. (cr #561.)
func TestResourceLinkExpiryConstsInSync(t *testing.T) {
	if resourceLinkExpiry != "1m" || resourceLinkExpiryHuman != "1 minute" {
		t.Errorf("link-expiry consts drifted: wire=%q human=%q — update BOTH the consts and this assertion together",
			resourceLinkExpiry, resourceLinkExpiryHuman)
	}
}

// TestHandleGet_DMRidesOneTimeSuffix fences that the
// "(one-time use · link expires in 1 minute)" suffix rides into the DM
// payload (not the channel reply) on dm:true. That suffix is the
// unconditional default for `/qurl get`, so it's always present; this guards
// against a future refactor that reorders the suffix-append and DM-dispatch
// branches in getWork.
func TestHandleGet_DMRidesOneTimeSuffix(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "prod-db", []string{testResourceIDFix})
	ts.addCustomer("POST", mintByTestResourcePath, func(w http.ResponseWriter, _ *http.Request) {
		writeCreateFixture(t, w, "https://qurl.link/dm-once", testResourceIDFix)
	})

	var dmText string
	h := newAdminTestHandler(t, ts)
	h.cfg.PostDM = func(_ context.Context, _, _, _, text string) error {
		dmText = text
		return nil
	}
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("get $prod-db dm:true", testAdminTeamID, testAdminUserID)

	wantSuffix := "(one-time use · link expires in " + resourceLinkExpiryHuman + ")"
	if !strings.HasSuffix(strings.TrimSpace(dmText), wantSuffix) {
		t.Errorf("DM payload missing one-time-use/expiry suffix %q: %q", wantSuffix, dmText)
	}
	if !strings.Contains(dmText, "https://qurl.link/dm-once") {
		t.Errorf("DM payload missing link: %q", dmText)
	}
	if strings.Contains(async, "one-time use") {
		t.Errorf("one-time-use suffix leaked to channel ephemeral on dm:true: %q", async)
	}
	if strings.Contains(async, "https://qurl.link/dm-once") {
		t.Errorf("link leaked to channel ephemeral on dm:true: %q", async)
	}
}

// TestHumanizeRetry fences the rate-limit retry-after rendering.
// Sub-second collapses to "a moment" (so 0.4s doesn't print as "0s"
// from int(0.4+0.5) rounding); minute-or-more rounds to the nearest minute
// and switches to hours when that reads better.
func TestHumanizeRetry(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, humanFallbackMoment},
		{-1 * time.Second, humanFallbackMoment},
		{500 * time.Millisecond, humanFallbackMoment},
		{900 * time.Millisecond, humanFallbackMoment},
		{1 * time.Second, "1s"},
		{30 * time.Second, "30s"},
		{59 * time.Second, "59s"},
		// 59.5s rounds half-up to 60s in the seconds branch — that's
		// the boundary the round-16 cr flagged. Rolls over to "1m"
		// instead of leaking a "60s" reading that contradicts the
		// minutes-branch shape (humanizeRetry must never print ≥60s).
		{59500 * time.Millisecond, "1m"},
		{60 * time.Second, "1m"},
		{2 * time.Minute, "2m"},
		{60 * time.Minute, "1h"},
		{61 * time.Minute, "1h1m"},
		{90 * time.Minute, "1h30m"},
		{119*time.Minute + 31*time.Second, "2h"},
	}
	for _, c := range cases {
		got := humanizeRetry(c.in)
		if got != c.want {
			t.Errorf("humanizeRetry(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestCreateInputJSON_ResourceID fences the wire shape for the
// alias-form mint: the bot calls POST /v1/resources/{id}/qurls, the
// resource id rides in the path, and the body carries neither
// target_url nor resource_id (both repeat the path id and would
// trip the server's exclusivity rules).
func TestCreateInputJSON_ResourceID(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "prod-db", []string{testResourceIDFix})
	var capturedBody []byte
	ts.addCustomer("POST", mintByTestResourcePath, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		capturedBody = b
		writeCreateFixture(t, w, "https://qurl.link/abc", testResourceIDFix)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	inv.invokeAdminAsync("get $prod-db", testAdminTeamID, testAdminUserID)

	var parsed map[string]any
	if err := json.Unmarshal(capturedBody, &parsed); err != nil {
		t.Fatalf("unmarshal captured body: %v body=%s", err, capturedBody)
	}
	if _, ok := parsed["target_url"]; ok {
		t.Errorf("target_url present on alias-form mint body (path-bound id, target_url is rejected): %v", parsed)
	}
	if _, ok := parsed["resource_id"]; ok {
		t.Errorf("resource_id present on alias-form mint body (rides in URL path): %v", parsed)
	}
}

// TestCreateInputJSON_Reason fences the wire shape: reason flag
// flows through to the JSON body when set, and is absent when
// unset. Alias-form mint, so the path is the resource-scoped
// endpoint.
func TestCreateInputJSON_Reason(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "prod-db", []string{testResourceIDFix})
	var capturedBody []byte
	ts.addCustomer("POST", mintByTestResourcePath, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		capturedBody = b
		writeCreateFixture(t, w, "https://qurl.link/abc", testResourceIDFix)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	inv.invokeAdminAsync(`get $prod-db reason:"incident #123"`, testAdminTeamID, testAdminUserID)

	var parsed map[string]any
	if err := json.Unmarshal(capturedBody, &parsed); err != nil {
		t.Fatalf("unmarshal captured body: %v body=%s", err, capturedBody)
	}
	if got, _ := parsed["reason"].(string); got != "incident #123" {
		t.Errorf("reason = %v, want %q", parsed["reason"], "incident #123")
	}
}

// TestCreateInputJSON_OneTimeDefault fences that one-time use is the
// unconditional default for `/qurl get`: with no flag at all, the wire
// body carries `one_time_use: true` and the async reply carries the
// "(one-time use · link expires in 1 minute)" suffix. There is no `once`
// flag — every `/qurl get` link burns on first redemption.
func TestCreateInputJSON_OneTimeDefault(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "prod-db", []string{testResourceIDFix})
	var capturedBody []byte
	ts.addCustomer("POST", mintByTestResourcePath, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		capturedBody = b
		writeCreateFixture(t, w, "https://qurl.link/abc", testResourceIDFix)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("get $prod-db", testAdminTeamID, testAdminUserID)

	var parsed map[string]any
	if err := json.Unmarshal(capturedBody, &parsed); err != nil {
		t.Fatalf("unmarshal captured body: %v body=%s", err, capturedBody)
	}
	if got, _ := parsed["one_time_use"].(bool); !got {
		t.Errorf("one_time_use = %v, want true (one-time use is the unconditional default)", parsed["one_time_use"])
	}
	if !strings.HasSuffix(strings.TrimSpace(async), "(one-time use · link expires in "+resourceLinkExpiryHuman+")") {
		t.Errorf("async reply missing one-time-use/expiry suffix: %q", async)
	}
	// The tight admit window MUST be surfaced at the point of sharing so a
	// late click isn't a silent dead link (cr #561).
	if !strings.Contains(async, "link expires in "+resourceLinkExpiryHuman) {
		t.Errorf("async reply does not surface the link-expiry window %q: %q", resourceLinkExpiryHuman, async)
	}
}

// TestCreateInputJSON_ResourceSessionLimits fences that every `/qurl get`
// mint carries the resource access limits on the wire: a 1-minute link
// expiry, a 1-hour session duration, and a single concurrent session.
// Enforcement is server-side; this only fences that the bot sets the policy
// and that the resource-scoped body carries all three.
func TestCreateInputJSON_ResourceSessionLimits(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "prod-db", []string{testResourceIDFix})
	var capturedBody []byte
	ts.addCustomer("POST", mintByTestResourcePath, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		capturedBody = b
		writeCreateFixture(t, w, "https://qurl.link/abc", testResourceIDFix)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	inv.invokeAdminAsync("get $prod-db", testAdminTeamID, testAdminUserID)

	var parsed map[string]any
	if err := json.Unmarshal(capturedBody, &parsed); err != nil {
		t.Fatalf("unmarshal captured body: %v body=%s", err, capturedBody)
	}
	if got, _ := parsed["expires_in"].(string); got != resourceLinkExpiry {
		t.Errorf("expires_in = %q, want %q", parsed["expires_in"], resourceLinkExpiry)
	}
	if got, _ := parsed["session_duration"].(string); got != resourceSessionDuration {
		t.Errorf("session_duration = %q, want %q", parsed["session_duration"], resourceSessionDuration)
	}
	// JSON numbers decode to float64 through map[string]any.
	if got, _ := parsed["max_sessions"].(float64); int(got) != resourceMaxSessions {
		t.Errorf("max_sessions = %v, want %d", parsed["max_sessions"], resourceMaxSessions)
	}
}

// TestCreateInputJSON_IdempotencyKeyHeader fences that the
// Idempotency-Key header lands on the wire (not in the JSON body)
// and is the sha256(team\x00channel\x00user\x00trigger). Alias-form,
// so the request lands at the resource-scoped endpoint.
func TestCreateInputJSON_IdempotencyKeyHeader(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "prod-db", []string{testResourceIDFix})
	var capturedHeader string
	ts.addCustomer("POST", mintByTestResourcePath, func(w http.ResponseWriter, r *http.Request) {
		capturedHeader = r.Header.Get("Idempotency-Key")
		writeCreateFixture(t, w, "https://qurl.link/abc", testResourceIDFix)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	inv.invokeAdminAsync("get $prod-db", testAdminTeamID, testAdminUserID)

	want := IdempotencyKey(testAdminTeamID, "C_test", testAdminUserID, "trigger_test")
	if capturedHeader != want {
		t.Errorf("Idempotency-Key header = %q, want %q", capturedHeader, want)
	}
	if len(capturedHeader) != 64 {
		t.Errorf("Idempotency-Key length = %d, want 64 (sha256 hex)", len(capturedHeader))
	}
}

// TestMapMintError_Unmapped5xx fences the catch-all transport-class
// branch: 503 + 504 + bare network errors all surface
// serviceUnreachableMessage and never the generic "Failed to mint".
// Alias-form, so the upstream serves 5xx at the resource-scoped
// endpoint.
func TestMapMintError_Unmapped5xx(t *testing.T) {
	statuses := []int{
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
	}
	for _, s := range statuses {
		ts := newAdminTestServers(t)
		ts.seedPolicySet(t, testAdminTeamID, "C_test", "prod-db", []string{testResourceIDFix})
		ts.addCustomer("POST", mintByTestResourcePath, func(w http.ResponseWriter, _ *http.Request) {
			writeAPIError(t, w, s, "upstream_error", "Upstream Error")
		})
		h := newAdminTestHandler(t, ts)
		inv := newAdminSlashInvoker(t, h)

		_, _, async := inv.invokeAdminAsync("get $prod-db", testAdminTeamID, testAdminUserID)
		if !strings.Contains(async, "Could not reach qURL") {
			t.Errorf("status %d: async reply missing service-unreachable: %q", s, async)
		}
	}
}

// addTunnelSlugResource registers a GET /v1/resources handler that
// resolves testTunnelSlug → testResourceIDFix as an active tunnel — the
// upstream half of the `/qurl get $<slug>` slug-fallback path.
func addTunnelSlugResource(t *testing.T, ts *adminTestServers) {
	t.Helper()
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeTunnelSlugResourceFixture(t, w)
	})
}

func writeTunnelSlugResourceFixture(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	writeResourceListFixture(t, w, []map[string]any{{
		testKeyResourceID: testResourceIDFix,
		testKeyType:       client.ResourceTypeTunnel,
		testKeySlug:       testTunnelSlug,
		testKeyStatus:     client.StatusActive,
	}}, "", false)
}

// TestHandleGet_DollarSlugAllowedSetNonAdmin fences the list→get
// round-trip for a tunnel that reaches a non-admin's /qurl list via
// allowed_resource_ids only — no alias_binding in this channel (e.g. a
// tunnel installed in another channel, then granted here). /qurl list
// renders `$<slug>`; pasting it into /qurl get must mint. The token
// misses the alias-binding lookup, falls back to slug resolution, and
// the resolved resource_id passes the channel allow-set gate.
func TestHandleGet_DollarSlugAllowedSetNonAdmin(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedNonAdmin(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "", []string{testResourceIDFix})
	addTunnelSlugResource(t, ts)
	ts.addCustomer("POST", mintByTestResourcePath, func(w http.ResponseWriter, _ *http.Request) {
		writeCreateFixture(t, w, "https://qurl.link/by-slug", testResourceIDFix)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("get $"+testTunnelSlug, testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "https://qurl.link/by-slug") {
		t.Errorf("slug round-trip failed — async reply missing link: %q", async)
	}
}

// TestHandleGet_DollarSlugMintsAfterAliasBound is the regression fence
// for "couldn't /qurl get $<slug> after set-alias $x $<slug>": an admin
// binds an alias (`$dash`) to a tunnel slug, which puts the tunnel's
// resource_id in the channel allow-set via alias_bindings.values().
// Getting by the SLUG (whose name is NOT itself a bound alias) must still
// mint — the lookup misses the binding, falls back to slug resolution,
// and the resolved resource_id passes the allow-set gate precisely
// because the alias binding put it there. Uses a non-admin so the mint
// proves the allow-set path (an admin would bypass it).
func TestHandleGet_DollarSlugMintsAfterAliasBound(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedNonAdmin(t)
	// `set-alias $dash $<slug>` binds `dash` → the tunnel's resource_id;
	// the slug name itself is NOT a bound alias.
	ts.seedPolicyAliasBindings(t, testAdminTeamID, "C_test", map[string]string{testTunnelAliasDash: testResourceIDFix})
	addTunnelSlugResource(t, ts)
	ts.addCustomer("POST", mintByTestResourcePath, func(w http.ResponseWriter, _ *http.Request) {
		writeCreateFixture(t, w, "https://qurl.link/slug-after-alias", testResourceIDFix)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("get $"+testTunnelSlug, testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "https://qurl.link/slug-after-alias") {
		t.Errorf("get-by-slug after aliasing the slug failed to mint: %q", async)
	}
}

// TestHandleGet_DollarSlugNotAllowedNonAdmin fences the slug-fallback
// authorization gate AND its anti-enumeration posture: a non-admin
// pasting a tunnel slug whose resource isn't in the channel allow-set
// sees the SAME "not configured for this channel" copy as a
// non-existent slug — not a distinct "not allowed" — so the wire text
// can't be used to enumerate tunnel slugs that exist elsewhere in the
// workspace. The resolved r_<id> never leaks and the mint never runs.
func TestHandleGet_DollarSlugNotAllowedNonAdmin(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedNonAdmin(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "", []string{"r_other_alloc"})
	addTunnelSlugResource(t, ts)
	var mintHits atomic.Int32
	ts.addCustomer("POST", mintByTestResourcePath, func(w http.ResponseWriter, _ *http.Request) {
		mintHits.Add(1)
		writeCreateFixture(t, w, "https://qurl.link/should-not", testResourceIDFix)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("get $"+testTunnelSlug, testAdminTeamID, testAdminUserID)
	// Collapsed to the not-found copy — see resolveTokenForGet.
	if !strings.Contains(async, "`$"+testTunnelSlug+"` is not configured for this channel") {
		t.Errorf("async reply missing anti-enumeration not-configured copy: %q", async)
	}
	if strings.Contains(async, "is not allowed in this channel") {
		t.Errorf("not-allowed copy leaked the slug-exists-elsewhere distinction: %q", async)
	}
	if strings.Contains(async, testResourceIDFix) {
		t.Errorf("resolved resource_id leaked in rejection message: %q", async)
	}
	if mintHits.Load() != 0 {
		t.Errorf("mint reached despite slug not in allow-set (hits = %d)", mintHits.Load())
	}
}

// TestHandleGet_DollarTokenBindingWinsOverSlug fences the resolution
// precedence in resolveTokenForGet: when a channel alias_binding exists
// for the token, it is authoritative and the tunnel-slug fallback is
// NOT consulted — even if a different tunnel happens to carry the same
// name as its slug. Pins the ordering against a future refactor that
// might flip it (which would let a same-named slug shadow an admin's
// explicit binding).
func TestHandleGet_DollarTokenBindingWinsOverSlug(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedNonAdmin(t)
	// Bind the token to testResourceIDFix in this channel.
	ts.seedPolicyAliasBindings(t, testAdminTeamID, "C_test", map[string]string{
		testTunnelSlug: testResourceIDFix,
	})
	// A slug lookup, if (wrongly) consulted, would resolve to a DIFFERENT
	// resource — its mint must never be hit.
	var slugMintHits atomic.Int32
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{{
			testKeyResourceID: "r_shadow_tun",
			testKeyType:       client.ResourceTypeTunnel,
			testKeySlug:       testTunnelSlug,
			testKeyStatus:     client.StatusActive,
		}}, "", false)
	})
	ts.addCustomer("POST", "/v1/resources/r_shadow_tun/qurls", func(w http.ResponseWriter, _ *http.Request) {
		slugMintHits.Add(1)
		writeCreateFixture(t, w, "https://qurl.link/SHADOW", "r_shadow_tun")
	})
	ts.addCustomer("POST", mintByTestResourcePath, func(w http.ResponseWriter, _ *http.Request) {
		writeCreateFixture(t, w, "https://qurl.link/from-binding", testResourceIDFix)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("get $"+testTunnelSlug, testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "https://qurl.link/from-binding") {
		t.Errorf("binding did not win — async reply missing binding link: %q", async)
	}
	if slugMintHits.Load() != 0 {
		t.Errorf("slug fallback was consulted despite a live binding (shadow mint hits = %d)", slugMintHits.Load())
	}
}

// TestHandleGet_DollarSlugAdminAlsoChannelScoped is the security regression
// fence for "even an admin can't /qurl get a tunnel from a channel it isn't
// protected in". The former admin bypass (admins could mint any slug from any
// channel, because /qurl list was workspace-wide) is gone: list, alias, and
// mint now share one channel-scoped definition. An admin minting `$<slug>` in a
// channel where the resource has no alias binding and no allow-set entry is
// refused with the same anti-enumeration "not configured for this channel" copy
// a non-admin gets, and the mint never runs — but the admin DOES mint once the
// tunnel is protected in the channel.
func TestHandleGet_DollarSlugAdminAlsoChannelScoped(t *testing.T) {
	t.Run("blocked when not exposed in this channel", func(t *testing.T) {
		ts := newAdminTestServers(t)
		ts.seedAdmin(t) // the caller is a workspace admin
		// Protect an UNRELATED resource so the channel is "warm" (non-empty
		// allow-set): the slug fallback must actually run and be rejected by
		// the allow-set gate, which is what proves there's no admin bypass at
		// the gate. Without this, the cold-channel short-circuit (#534)
		// returns not-configured before the slug is ever resolved, so the
		// admin-bypass property would go unexercised.
		ts.seedChannelExposure(t, testAdminTeamID, "C_test", "r_other_alloc")
		// Inlined slug resource (vs addTunnelSlugResource) so listHits can pin
		// that the slug fallback actually reached upstream — see the assertion
		// below.
		var listHits atomic.Int32
		ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
			listHits.Add(1)
			writeResourceListFixture(t, w, []map[string]any{{
				testKeyResourceID: testResourceIDFix,
				testKeyType:       client.ResourceTypeTunnel,
				testKeySlug:       testTunnelSlug,
				testKeyStatus:     client.StatusActive,
			}}, "", false)
		})
		var mintHits atomic.Int32
		ts.addCustomer("POST", mintByTestResourcePath, func(w http.ResponseWriter, _ *http.Request) {
			mintHits.Add(1)
			writeCreateFixture(t, w, "https://qurl.link/should-not", testResourceIDFix)
		})
		h := newAdminTestHandler(t, ts)
		inv := newAdminSlashInvoker(t, h)

		_, _, async := inv.invokeAdminAsync("get $"+testTunnelSlug, testAdminTeamID, testAdminUserID)
		if !strings.Contains(async, "`$"+testTunnelSlug+"` is not configured for this channel") {
			t.Errorf("admin was not channel-scoped — missing not-configured copy: %q", async)
		}
		// This not-configured copy is BYTE-IDENTICAL to the cold-channel
		// short-circuit's (#534). Without the assertion below, a broken warm
		// seed would fall through the short-circuit and still match the copy —
		// leaving the admin-no-bypass-at-the-gate property unexercised. Pin
		// that the slug fallback actually ran and was rejected BY THE GATE: a
		// non-empty allow-set must let the slug lookup hit upstream.
		if listHits.Load() == 0 {
			t.Errorf("slug fallback never hit upstream GET /v1/resources — warm-channel seed broke and the admin-no-bypass property went unexercised (regressed to the cold-channel short-circuit)")
		}
		if mintHits.Load() != 0 {
			t.Errorf("admin minted a tunnel not exposed in this channel (hits = %d) — the bypass is back", mintHits.Load())
		}
	})

	t.Run("mints when exposed in this channel", func(t *testing.T) {
		ts := newAdminTestServers(t)
		ts.seedAdmin(t)
		// Expose the slug's resource to C_test (allow-set, no alias) so the
		// slug-fallback gate passes for the admin exactly as for anyone else.
		ts.seedChannelExposure(t, testAdminTeamID, "C_test", testResourceIDFix)
		addTunnelSlugResource(t, ts)
		ts.addCustomer("POST", mintByTestResourcePath, func(w http.ResponseWriter, _ *http.Request) {
			writeCreateFixture(t, w, "https://qurl.link/admin-slug", testResourceIDFix)
		})
		h := newAdminTestHandler(t, ts)
		inv := newAdminSlashInvoker(t, h)

		_, _, async := inv.invokeAdminAsync("get $"+testTunnelSlug, testAdminTeamID, testAdminUserID)
		if !strings.Contains(async, "https://qurl.link/admin-slug") {
			t.Errorf("admin should mint a tunnel exposed in this channel: %q", async)
		}
	})
}
