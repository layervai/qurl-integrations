package internal

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/shared/client"
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
// channel-scoped alias lookup → rate-limit OK → mint → channel
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

// TestHandleGet_AliasNotFound fences the no-binding path: when the
// channel's alias_bindings map has no entry for the requested alias
// (no row, missing map, or missing key), getWork surfaces the
// "not configured for this channel" copy that points the user at
// their Slack admin, and never reaches the mint.
func TestHandleGet_AliasNotFound(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	var mintHits atomic.Int32
	ts.addCustomer("POST", "/v1/qurls", func(w http.ResponseWriter, _ *http.Request) {
		mintHits.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	// On an alias-binding miss, getWork falls back to a tunnel-slug
	// lookup (GET /v1/resources?slug=…). `$missing` is neither a binding
	// nor a live tunnel slug, so the resources listing returns empty and
	// the user sees the "not configured for this channel" copy.
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
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
	if mintHits.Load() != 0 {
		t.Errorf("mint reached despite alias-not-found (hits = %d)", mintHits.Load())
	}
}

// TestHandleGet_MintTunnelDisabled fences the 403/tunnel_disabled
// mint error → user-facing "Tunnel resources are not yet enabled"
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
	if !strings.Contains(async, "Tunnel resources are not yet enabled") {
		t.Errorf("async reply missing tunnel-disabled message: %q", async)
	}
}

// TestHandleGet_MintRateLimit fences the 429 mint error with a
// retry-after header → user-facing "Rate limit hit. Try again in 30s."
func TestHandleGet_MintRateLimit(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "prod-db", []string{testResourceIDFix})
	ts.addCustomer("POST", mintByTestResourcePath, func(w http.ResponseWriter, _ *http.Request) {
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
	if !strings.Contains(ack, "not a resource ID") {
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
	if !strings.Contains(async, "re-point it at a tunnel") {
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
	reply, err := h.getWork(context.Background(), slogTestLogger(t), args)
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

// TestHandleGet_DMRidesOneTimeSuffix fences that the (one-time use)
// suffix rides into the DM payload (not the channel reply) on dm:true.
// One-time use is the unconditional default for `/qurl get`, so the
// suffix is always present; this guards against a future refactor that
// reorders the suffix-append and DM-dispatch branches in getWork.
func TestHandleGet_DMRidesOneTimeSuffix(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "prod-db", []string{testResourceIDFix})
	ts.addCustomer("POST", mintByTestResourcePath, func(w http.ResponseWriter, _ *http.Request) {
		writeCreateFixture(t, w, "https://qurl.link/dm-once", testResourceIDFix)
	})

	var dmText string
	h := newAdminTestHandler(t, ts)
	h.cfg.PostDM = func(_ context.Context, _, text string) error {
		dmText = text
		return nil
	}
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("get $prod-db dm:true", testAdminTeamID, testAdminUserID)

	if !strings.HasSuffix(strings.TrimSpace(dmText), "(one-time use)") {
		t.Errorf("DM payload missing one-time-use suffix: %q", dmText)
	}
	if !strings.Contains(dmText, "https://qurl.link/dm-once") {
		t.Errorf("DM payload missing link: %q", dmText)
	}
	if strings.Contains(async, "(one-time use)") {
		t.Errorf("one-time-use suffix leaked to channel ephemeral on dm:true: %q", async)
	}
	if strings.Contains(async, "https://qurl.link/dm-once") {
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
// `(one-time use)` suffix. There is no `once` flag — every `/qurl get`
// link burns on first redemption.
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
	if !strings.HasSuffix(strings.TrimSpace(async), "(one-time use)") {
		t.Errorf("async reply missing one-time-use suffix: %q", async)
	}
}

// TestCreateInputJSON_TunnelSessionLimits fences that every `/qurl get`
// mint carries the tunnel access limits on the wire: a 1-minute link
// expiry, a 1-hour session duration, and a single concurrent session.
// `/qurl get` is tunnel-only, so these bound a shared tunnel link to one
// short-lived viewer. Enforcement is server-side; this only fences that the
// bot sets the policy (and that the resource-scoped body carries all three).
func TestCreateInputJSON_TunnelSessionLimits(t *testing.T) {
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
	if got, _ := parsed["expires_in"].(string); got != tunnelLinkExpiry {
		t.Errorf("expires_in = %q, want %q", parsed["expires_in"], tunnelLinkExpiry)
	}
	if got, _ := parsed["session_duration"].(string); got != tunnelSessionDuration {
		t.Errorf("session_duration = %q, want %q", parsed["session_duration"], tunnelSessionDuration)
	}
	// JSON numbers decode to float64 through map[string]any.
	if got, _ := parsed["max_sessions"].(float64); int(got) != tunnelMaxSessions {
		t.Errorf("max_sessions = %v, want %d", parsed["max_sessions"], tunnelMaxSessions)
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
		writeResourceListFixture(t, w, []map[string]any{{
			testKeyResourceID: testResourceIDFix,
			testKeyType:       client.ResourceTypeTunnel,
			testKeySlug:       testTunnelSlug,
			testKeyStatus:     client.StatusActive,
		}}, "", false)
	})
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

// TestHandleGet_DollarSlugAdminBypassesAllowedSet fences the admin
// round-trip: a workspace admin sees every tunnel in /qurl list
// (unfiltered) and can mint its `$<slug>` even with no alias_binding
// and no allow-set entry in the current channel.
func TestHandleGet_DollarSlugAdminBypassesAllowedSet(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	addTunnelSlugResource(t, ts)
	ts.addCustomer("POST", mintByTestResourcePath, func(w http.ResponseWriter, _ *http.Request) {
		writeCreateFixture(t, w, "https://qurl.link/admin-slug", testResourceIDFix)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("get $"+testTunnelSlug, testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "https://qurl.link/admin-slug") {
		t.Errorf("admin slug round-trip failed: %q", async)
	}
}
