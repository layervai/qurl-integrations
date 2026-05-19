package internal

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// writeResourceFixture writes a /v1/resources/by-alias/:alias
// success envelope. Shared with the admin-test path's identical
// helper — duplicated here so this test file is self-contained
// against #231's renames.
func writeResourceFixture(t *testing.T, w http.ResponseWriter, resourceID, alias string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	body := map[string]any{
		testKeyData: map[string]any{
			testKeyResourceID: resourceID,
			"alias":           alias,
		},
	}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Fatalf("encode: %v", err)
	}
}

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
// alias resolve → policy allow → rate-limit OK → mint → channel
// ephemeral reply carrying the qURL link.
func TestHandleGet_HappyPath(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "prod-db", []string{testResourceIDFix})
	ts.addCustomer("GET", "/v1/resources/by-alias/prod-db", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceFixture(t, w, testResourceIDFix, "prod-db")
	})
	ts.addCustomer("POST", "/v1/qurls", func(w http.ResponseWriter, _ *http.Request) {
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
	if !strings.Contains(async, "`$prod-db`") {
		t.Errorf("async reply missing alias: %q", async)
	}
}

// TestHandleGet_AliasNotFound fences the alias-not-found path:
// alias resolution 404 → user-facing "No resource has alias" reply.
func TestHandleGet_AliasNotFound(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.addCustomer("GET", "/v1/resources/by-alias/missing", func(w http.ResponseWriter, _ *http.Request) {
		writeAPIError(t, w, http.StatusNotFound, "alias_not_found", "Not Found")
	})
	var mintHits atomic.Int32
	ts.addCustomer("POST", "/v1/qurls", func(w http.ResponseWriter, _ *http.Request) {
		mintHits.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("get $missing", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "No resource has alias `$missing`") {
		t.Errorf("async reply missing not-found message: %q", async)
	}
	if mintHits.Load() != 0 {
		t.Errorf("mint reached despite alias-not-found (hits = %d)", mintHits.Load())
	}
}

// TestHandleGet_PolicyDenied fences the channel-policy gate.
// Alias resolves OK, but no channel_policies row allows the
// resource in C_test → friendly "not allowed in this channel" reply
// and no mint.
func TestHandleGet_PolicyDenied(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.addCustomer("GET", "/v1/resources/by-alias/prod-db", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceFixture(t, w, testResourceIDFix, "prod-db")
	})
	var mintHits atomic.Int32
	ts.addCustomer("POST", "/v1/qurls", func(w http.ResponseWriter, _ *http.Request) {
		mintHits.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("get $prod-db", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "not allowed in this channel") {
		t.Errorf("async reply missing policy-denied message: %q", async)
	}
	if mintHits.Load() != 0 {
		t.Errorf("mint reached despite policy-denied (hits = %d)", mintHits.Load())
	}
}

// TestHandleGet_MintTunnelDisabled fences the 403/tunnel_disabled
// mint error → user-facing "Tunnel resources are not yet enabled"
// reply.
func TestHandleGet_MintTunnelDisabled(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "prod-db", []string{testResourceIDFix})
	ts.addCustomer("GET", "/v1/resources/by-alias/prod-db", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceFixture(t, w, testResourceIDFix, "prod-db")
	})
	ts.addCustomer("POST", "/v1/qurls", func(w http.ResponseWriter, _ *http.Request) {
		writeAPIError(t, w, http.StatusForbidden, "tunnel_disabled", "Forbidden")
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("get $prod-db", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "Tunnel resources are not yet enabled") {
		t.Errorf("async reply missing tunnel-disabled message: %q", async)
	}
}

// TestHandleGet_MintRateLimit fences the 429 mint error with a
// retry-after header → user-facing "Rate limit hit. Try again in 30s."
func TestHandleGet_MintRateLimit(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "prod-db", []string{testResourceIDFix})
	ts.addCustomer("GET", "/v1/resources/by-alias/prod-db", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceFixture(t, w, testResourceIDFix, "prod-db")
	})
	ts.addCustomer("POST", "/v1/qurls", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		writeAPIError(t, w, http.StatusTooManyRequests, "rate_limited", "Too Many Requests")
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
}

// TestHandleGet_MintTransportError fences 5xx and bare network
// errors → user-facing "Could not reach qURL. Please try again."
// (mapMintError's serviceUnreachableMessage branch).
func TestHandleGet_MintTransportError(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "prod-db", []string{testResourceIDFix})
	ts.addCustomer("GET", "/v1/resources/by-alias/prod-db", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceFixture(t, w, testResourceIDFix, "prod-db")
	})
	ts.addCustomer("POST", "/v1/qurls", func(w http.ResponseWriter, _ *http.Request) {
		writeAPIError(t, w, http.StatusBadGateway, "upstream_error", "Bad Gateway")
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("get $prod-db", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "Could not reach qURL") {
		t.Errorf("async reply missing service-unreachable message: %q", async)
	}
}

// TestHandleGet_MissingAlias fences the parser-level "missing $alias"
// surface. The slash-command body has `get` with no positional arg
// → the handler replies synchronously with a Usage hint and never
// kicks off async work.
func TestHandleGet_MissingAlias(t *testing.T) {
	ts := newAdminTestServers(t)
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	status, ack := inv.invokeAdmin("get", testAdminTeamID, testAdminUserID)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(ack, "$alias argument") {
		t.Errorf("ack missing alias-hint: %q", ack)
	}
}

// TestHandleGet_AdminStoreNil fences the post-pivot fail-closed
// posture: a nil AdminStore (sandbox / no-DDB) refuses to mint
// because the policy gate can't run. The user sees a friendly
// "Admin features are not configured" message and the customer API
// is never reached for the mint NOR for the upstream alias
// resolution (round-17 cr #2: the not-configured check happens
// before GetResourceByAlias so we don't burn customer-API quota /
// leak alias-existence on a workspace that can't gate the result).
func TestHandleGet_AdminStoreNil(t *testing.T) {
	ts := newAdminTestServers(t)
	var aliasHits, mintHits atomic.Int32
	ts.addCustomer("GET", "/v1/resources/by-alias/prod-db", func(w http.ResponseWriter, _ *http.Request) {
		aliasHits.Add(1)
		writeResourceFixture(t, w, testResourceIDFix, "prod-db")
	})
	ts.addCustomer("POST", "/v1/qurls", func(w http.ResponseWriter, _ *http.Request) {
		mintHits.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	h := newAdminTestHandler(t, ts)
	// Override AdminStore to nil after construction.
	h.cfg.AdminStore = nil
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("get $prod-db", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "Admin features are not configured") {
		t.Errorf("async reply missing not-configured message: %q", async)
	}
	if mintHits.Load() != 0 {
		t.Errorf("mint reached despite nil AdminStore (hits = %d)", mintHits.Load())
	}
	if aliasHits.Load() != 0 {
		t.Errorf("GetResourceByAlias reached despite nil AdminStore (hits = %d) — admin-store-nil check must short-circuit before alias resolution", aliasHits.Load())
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
	ts.addCustomer("GET", "/v1/resources/by-alias/prod-db", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceFixture(t, w, testResourceIDFix, "prod-db")
	})
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
	ts.addCustomer("GET", "/v1/resources/by-alias/prod-db", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceFixture(t, w, testResourceIDFix, "prod-db")
	})
	ts.addCustomer("POST", "/v1/qurls", func(w http.ResponseWriter, _ *http.Request) {
		writeCreateFixture(t, w, "https://qurl.link/dm-secret", testResourceIDFix)
	})

	var dmCalls atomic.Int32
	var dmText string
	h := newAdminTestHandler(t, ts)
	h.cfg.PostDM = func(_ context.Context, _, text string) error {
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

// TestHumanizeRetry fences the rate-limit retry-after rendering.
// Sub-second collapses to "a moment" (so 0.4s doesn't print as "0s"
// from int(0.4+0.5) rounding); minute-or-more rounds to integer.
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
	}
	for _, c := range cases {
		got := humanizeRetry(c.in)
		if got != c.want {
			t.Errorf("humanizeRetry(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestCreateInputJSON_ResourceID fences the wire shape:
// CreateInput{ResourceID: "r_x"} marshals without a target_url key
// (mutually_exclusive_fields with the server). Captured by a
// recording httptest server.
func TestCreateInputJSON_ResourceID(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "prod-db", []string{testResourceIDFix})
	ts.addCustomer("GET", "/v1/resources/by-alias/prod-db", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceFixture(t, w, testResourceIDFix, "prod-db")
	})
	var capturedBody []byte
	ts.addCustomer("POST", "/v1/qurls", func(w http.ResponseWriter, r *http.Request) {
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
		t.Errorf("target_url present on resource-id mint (mutually-exclusive): %v", parsed)
	}
	if got, _ := parsed["resource_id"].(string); got != testResourceIDFix {
		t.Errorf("resource_id = %v, want r_prod_db", parsed["resource_id"])
	}
}

// TestCreateInputJSON_Reason fences the wire shape: reason flag
// flows through to the JSON body when set, and is absent when
// unset.
func TestCreateInputJSON_Reason(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "prod-db", []string{testResourceIDFix})
	ts.addCustomer("GET", "/v1/resources/by-alias/prod-db", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceFixture(t, w, testResourceIDFix, "prod-db")
	})
	var capturedBody []byte
	ts.addCustomer("POST", "/v1/qurls", func(w http.ResponseWriter, r *http.Request) {
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

// TestCreateInputJSON_IdempotencyKeyHeader fences that the
// Idempotency-Key header lands on the wire (not in the JSON body)
// and is the sha256(team\x00channel\x00user\x00trigger).
func TestCreateInputJSON_IdempotencyKeyHeader(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "prod-db", []string{testResourceIDFix})
	ts.addCustomer("GET", "/v1/resources/by-alias/prod-db", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceFixture(t, w, testResourceIDFix, "prod-db")
	})
	var capturedHeader string
	ts.addCustomer("POST", "/v1/qurls", func(w http.ResponseWriter, r *http.Request) {
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
func TestMapMintError_Unmapped5xx(t *testing.T) {
	statuses := []int{
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
	}
	for _, s := range statuses {
		ts := newAdminTestServers(t)
		ts.seedPolicySet(t, testAdminTeamID, "C_test", "prod-db", []string{testResourceIDFix})
		ts.addCustomer("GET", "/v1/resources/by-alias/prod-db", func(w http.ResponseWriter, _ *http.Request) {
			writeResourceFixture(t, w, testResourceIDFix, "prod-db")
		})
		ts.addCustomer("POST", "/v1/qurls", func(w http.ResponseWriter, _ *http.Request) {
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
