package internal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/shared/auth"
	"github.com/layervai/qurl-integrations/shared/client"
)

type testRoundTripFunc func(*http.Request) (*http.Response, error)

func (f testRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type trackingResponseBody struct {
	reader *strings.Reader
	sawEOF atomic.Bool
	closed atomic.Bool
}

func (b *trackingResponseBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	if errors.Is(err, io.EOF) {
		b.sawEOF.Store(true)
	}
	return n, err
}

func (b *trackingResponseBody) Close() error {
	b.closed.Store(true)
	return nil
}

// responseURLRecorder is an httptest.Server that captures every
// response_url POST it receives. Tests use it to assert the body the
// async worker delivered after the synchronous ack.
type responseURLRecorder struct {
	URL string

	mu    sync.Mutex
	posts []map[string]string
}

// posts returns a snapshot of the captured payloads.
func (r *responseURLRecorder) Posts() []map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.posts)
}

func newResponseURLRecorder(t *testing.T) *responseURLRecorder {
	t.Helper()
	rec := &responseURLRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		rec.mu.Lock()
		rec.posts = append(rec.posts, body)
		rec.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	rec.URL = srv.URL
	return rec
}

func TestPostResponseBodyDrainsOversizedErrorBody(t *testing.T) {
	t.Parallel()
	body := &trackingResponseBody{reader: strings.NewReader(strings.Repeat("x", 4097))}
	h := &Handler{
		responseURLClient: &http.Client{Transport: testRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusBadRequest,
				Header:     make(http.Header),
				Body:       body,
			}, nil
		})},
		validateResponseURLFn: url.Parse,
	}

	ok := h.postResponseBody(slog.New(slog.NewTextHandler(io.Discard, nil)), "http://slack.test/response", []byte(`{"text":"test"}`))

	if ok {
		t.Fatal("postResponseBody returned true for HTTP 400")
	}
	if !body.sawEOF.Load() {
		t.Fatal("oversized response_url body was not drained to EOF")
	}
	if !body.closed.Load() {
		t.Fatal("oversized response_url body was not closed")
	}
}

// TestReplaceOriginalResponsePayload is the regression guard for the prod
// `no_text` 500: delete_original is unsupported for slash commands, so the
// wizard cleanup MUST replace (carry text) rather than delete. This asserts the
// posted body shape — it fails against the old `{"delete_original": true}`
// payload, which had no `text` and is exactly what Slack rejected.
func TestReplaceOriginalResponsePayload(t *testing.T) {
	t.Parallel()

	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	h := &Handler{
		baseCtx:               context.Background(),
		responseURLClient:     srv.Client(),
		validateResponseURLFn: url.Parse,
	}

	const msg = ":white_check_mark: Opened the form."
	if !h.replaceOriginalResponse(slog.New(slog.NewTextHandler(io.Discard, nil)), srv.URL, msg) {
		t.Fatal("replaceOriginalResponse returned false for HTTP 200")
	}

	var payload map[string]any
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatalf("response_url body is not JSON: %v (body=%s)", err, gotBody)
	}
	if _, ok := payload["delete_original"]; ok {
		t.Errorf("payload must not contain delete_original (unsupported for slash commands); got %v", payload)
	}
	if got, _ := payload["replace_original"].(bool); !got {
		t.Errorf("replace_original = %v, want true", payload["replace_original"])
	}
	if got, _ := payload[respFieldText].(string); got != msg {
		t.Errorf("%s = %q, want %q", respFieldText, payload[respFieldText], msg)
	}
}

func TestReplaceOriginalResponseRetryHonorsBaseContextCancellation(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"slack temporarily unavailable"}`))
	}))
	t.Cleanup(srv.Close)

	baseCtx, cancel := context.WithCancel(context.Background())
	cancel()
	h := &Handler{
		baseCtx:               baseCtx,
		responseURLClient:     srv.Client(),
		validateResponseURLFn: url.Parse,
	}

	ok := h.replaceOriginalResponse(slog.New(slog.NewTextHandler(io.Discard, nil)), srv.URL, "msg")

	if ok {
		t.Fatal("replaceOriginalResponse returned true for HTTP 500")
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("response_url hits = %d, want 1 because canceled baseCtx skips retry", got)
	}
}

func TestPostResponseWithRetrySkipsPermanentHTTPFailure(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_blocks"}`))
	}))
	t.Cleanup(srv.Close)

	h := &Handler{
		baseCtx:               context.Background(),
		responseURLClient:     srv.Client(),
		validateResponseURLFn: url.Parse,
	}

	ok := h.postResponseWithRetry(slog.New(slog.NewTextHandler(io.Discard, nil)), srv.URL, "msg", "test_permanent")

	if ok {
		t.Fatal("postResponseWithRetry returned true for HTTP 400")
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("response_url hits = %d, want 1 because HTTP 400 is permanent", got)
	}
}

func TestWaitForResponseURLRetryReturnsImmediatelyWhenCanceled(t *testing.T) {
	t.Parallel()

	baseCtx, cancel := context.WithCancel(context.Background())
	cancel()
	h := &Handler{baseCtx: baseCtx}

	done := make(chan bool, 1)
	go func() {
		done <- h.waitForResponseURLRetry()
	}()

	select {
	case got := <-done:
		if got {
			t.Fatal("waitForResponseURLRetry returned true for canceled baseCtx")
		}
	case <-time.After(responseURLRetryDelay / 2):
		t.Fatal("waitForResponseURLRetry slept despite canceled baseCtx")
	}
}

// getTokenCommandBody builds a /slack/commands form payload for a
// `/qurl get $<alias>` mint. The infra tests in this file fence the
// async machinery (ack latency, idempotency-key derivation, pool
// saturation, orphan-ack on shutdown) rather than the resolution
// surface, so they mint through the channel-alias token form
// ([getTestAlias] → [getTestResourceID]) seeded into the handler's
// AdminStore via [seedGetAliasBinding]. Raw URLs are no longer
// mintable through Slack (parseGet rejects them), so the token form
// is the only vehicle that reaches the mint pipeline.
//
// response_url is parameterized so a test can wire it at the recorder
// it controls. team_id is parameterized so per-test idempotency-key
// fixtures stay distinct; channel_id / user_id are pinned to
// [getTokenCommandTestChannelID] / [getTokenCommandTestUserID] so
// assertions that re-derive the expected IdempotencyKey from
// `(team, channel, user, trigger)` reference the same constants the
// body carries — a typo on one side would otherwise fail the test for
// the wrong reason. The seeded alias binding is keyed on the same
// channel, so a test that mints with a non-default channel would also
// have to reseed.
const (
	getTokenCommandTestChannelID = "C123"
	getTokenCommandTestUserID    = "U_test"

	// getTestAlias is the channel alias the infra tests mint through.
	// getTestResourceID is its bound resource_id — the mint then lands
	// at POST /v1/resources/<getTestResourceID>/qurls, which the bare
	// httptest qURL servers in this file (not path-routed) answer.
	getTestAlias      = "tunnel-a"
	getTestResourceID = "r_proc_test"
)

func getTokenCommandBody(teamID, triggerID, responseURL string) string {
	return url.Values{
		fieldCommand:     {testSlashCmd},
		fieldText:        {"get $" + getTestAlias},
		fieldTeamID:      {teamID},
		fieldChannelID:   {getTokenCommandTestChannelID},
		fieldUserID:      {getTokenCommandTestUserID},
		fieldTriggerID:   {triggerID},
		fieldResponseURL: {responseURL},
	}.Encode()
}

// seedGetAliasBinding wires an AdminStore onto h (a fakeDDB-backed
// slackdata.Store) and seeds a single channel alias binding
// ([getTestAlias] → [getTestResourceID]) on (teamID,
// [getTokenCommandTestChannelID]) so the token-form `/qurl get` issued
// by [getTokenCommandBody] resolves via LookupChannelAlias and mints
// against the resource-scoped endpoint. Used by the async-infra tests
// in this file, which construct their *Handler with a bespoke Config
// (custom pool size, retry, or BaseContext) and so can't share
// newAdminTestHandler. The rate-limit gate is a stubbed always-allow
// (slackdata.CheckRateLimit), so no rate-limit seed is needed.
func seedGetAliasBinding(t *testing.T, h *Handler, teamID string) {
	t.Helper()
	names := defaultTestTableNames()
	ddb := newFakeDDB(t, names, nil)
	ddb.seedItem(t, names.channelPolicy, seedChannelPolicyAliasBindings(
		teamID, getTokenCommandTestChannelID, map[string]string{getTestAlias: getTestResourceID},
	))
	h.cfg.AdminStore = newStoreFromFake(t, ddb, names, nil)
}

// waitFor polls until cond returns true or the deadline elapses. Used
// to gate on async post-ack work without hard sleeps. The 10ms tick
// keeps the success path snappy while the timeout argument bounds the
// failure path; callers should pass a value that absorbs race-detector
// load on a busy CI runner.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}

// TestHandle_AckIsFastUnderSlowAPI fences the load-bearing UX promise:
// no matter how slow the qURL upstream is, the handler must ack within
// Slack's 3s budget — a 50ms cap leaves room for ~60x slowdown before
// the timeout starts to bite.
func TestHandle_AckIsFastUnderSlowAPI(t *testing.T) {
	slowSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Far longer than the 50ms ack budget — synchronous code would
		// exceed it; the async path returns the ack first and hits the
		// upstream off the request goroutine.
		time.Sleep(500 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"resource_id":"r1","qurl_link":"https://qurl.link/x"}}`))
	}))
	t.Cleanup(slowSrv.Close)

	t.Setenv("QURL_API_KEY", "test-key")

	rec := newResponseURLRecorder(t)
	h := newTestHandler(t, slowSrv)
	seedGetAliasBinding(t, h, "T123")

	body := getTokenCommandBody("T123", "trig-fast", rec.URL)

	start := time.Now()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, body))
	elapsed := time.Since(start)

	if elapsed > 50*time.Millisecond {
		t.Errorf("ack took %v, want ≤50ms", elapsed)
	}
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	var ack map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &ack); err != nil {
		t.Fatalf("unmarshal ack: %v", err)
	}
	if ack["text"] != ackWorkingOnIt {
		t.Errorf("ack text = %q, want %q", ack["text"], ackWorkingOnIt)
	}
}

// TestHandle_AsyncGetPostsResultToResponseURL fences the round-trip:
// after the ack, the worker POSTs the qURL link via response_url.
func TestHandle_AsyncGetPostsResultToResponseURL(t *testing.T) {
	qurlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"resource_id":"r_abc","qurl_link":"https://qurl.link/at_token"}}`))
	}))
	t.Cleanup(qurlSrv.Close)

	t.Setenv("QURL_API_KEY", "test-key")

	rec := newResponseURLRecorder(t)
	h := newTestHandler(t, qurlSrv)
	seedGetAliasBinding(t, h, "T123")

	body := getTokenCommandBody("T123", "trig-create", rec.URL)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, body))

	h.Wait() // drain the async worker before asserting on what it posted.

	posts := rec.Posts()
	if len(posts) != 1 {
		t.Fatalf("response_url POST count = %d, want 1; posts=%v", len(posts), posts)
	}
	got := posts[0]
	if got["response_type"] != "ephemeral" {
		t.Errorf("response_url response_type = %q, want ephemeral", got["response_type"])
	}
	if !strings.Contains(got["text"], "https://qurl.link/at_token") {
		t.Errorf("response_url text missing qURL link: %q", got["text"])
	}
}

// TestHandle_AsyncGetSurfacesIdempotencyKey fences the dedup contract:
// every /qurl get <url> with a fixed team_id+trigger_id MUST carry the
// same Idempotency-Key. Slack's 3s ack timeout is below typical qURL
// API latency under load, so Slack-side retries are real — the qURL
// service uses this header to fold them into a single resource
// creation.
func TestHandle_AsyncGetSurfacesIdempotencyKey(t *testing.T) {
	var seenKey string
	var keyMu sync.Mutex
	qurlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k := r.Header.Get(client.HeaderIdempotencyKey)
		keyMu.Lock()
		seenKey = k
		keyMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"resource_id":"r1","qurl_link":"https://qurl.link/x"}}`))
	}))
	t.Cleanup(qurlSrv.Close)

	t.Setenv("QURL_API_KEY", "test-key")

	rec := newResponseURLRecorder(t)
	h := newTestHandler(t, qurlSrv)
	seedGetAliasBinding(t, h, "T999")

	body := getTokenCommandBody("T999", "trig-dedup", rec.URL)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, body))
	h.Wait()

	keyMu.Lock()
	got := seenKey
	keyMu.Unlock()
	if got == "" {
		t.Fatal("upstream saw no Idempotency-Key header")
	}
	want := IdempotencyKey("T999", getTokenCommandTestChannelID, getTokenCommandTestUserID, "trig-dedup")
	if got != want {
		t.Errorf("Idempotency-Key = %q, want %q", got, want)
	}
}

// TestHandle_ConcurrentGetSharesIdempotencyKey is the load-bearing
// dedup integration test: 100 concurrent /qurl get <url> requests with
// the same trigger_id must all carry the same Idempotency-Key. Anything
// less is a regression that re-introduces duplicate qURLs under
// Slack's retry storm.
func TestHandle_ConcurrentGetSharesIdempotencyKey(t *testing.T) {
	const concurrency = 100

	var keys sync.Map
	var hits atomic.Int32
	qurlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if k := r.Header.Get(client.HeaderIdempotencyKey); k != "" {
			keys.LoadOrStore(k, struct{}{})
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"resource_id":"r1","qurl_link":"https://qurl.link/x"}}`))
	}))
	t.Cleanup(qurlSrv.Close)

	t.Setenv("QURL_API_KEY", "test-key")

	rec := newResponseURLRecorder(t)
	// Pool capacity bumped so all concurrency requests are dispatched
	// (the dedup property is what's under test, not back-pressure).
	h := NewHandler(Config{
		AuthProvider:       &auth.EnvProvider{EnvVar: "QURL_API_KEY"},
		SlackSigningSecret: testSigningSecret,
		MaxConcurrentAsync: concurrency,
		NewClient: func(apiKey string) *client.Client {
			return client.New(qurlSrv.URL, apiKey)
		},
	})
	h.now = func() time.Time { return fixedNow }
	h.validateResponseURLFn = url.Parse
	seedGetAliasBinding(t, h, "T-concurrent")
	t.Cleanup(h.Wait)

	body := getTokenCommandBody("T-concurrent", "trig-shared", rec.URL)
	// Sign once on the test goroutine. Doing it inside each spawned
	// goroutine would mean 100 redundant HMAC computations and would
	// expose us to t.Helper / t.Fatalf inside non-test goroutines —
	// which leaves wg.Wait blocked on a runtime.Goexit'd worker.
	sig, ts := signSlackBody(t, body)

	var wg sync.WaitGroup
	for range concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := httptest.NewRequest(http.MethodPost, "/slack/commands", strings.NewReader(body))
			r.Header.Set(headerSlackSignature, sig)
			r.Header.Set(headerSlackTimestamp, ts)
			h.ServeHTTP(httptest.NewRecorder(), r)
		}()
	}
	wg.Wait()
	h.Wait()

	if got := hits.Load(); got != concurrency {
		t.Errorf("upstream hits = %d, want %d (pool was sized for full dispatch)", got, concurrency)
	}
	var uniqueKeys int
	keys.Range(func(_, _ any) bool { uniqueKeys++; return true })
	if uniqueKeys != 1 {
		t.Errorf("upstream observed %d unique Idempotency-Keys across %d requests, want 1", uniqueKeys, concurrency)
	}
}

// TestHandle_AsyncGetSurfaces5xxCorrelationHandle fences the 5xx
// surface on /qurl get: upstream Title and Detail stay out of Slack,
// while the opaque RequestID remains so users have a handle to share
// with support. The retry-friendly "Please try again." is preserved so
// the disposition is unchanged.
func TestHandle_AsyncGetSurfaces5xxCorrelationHandle(t *testing.T) {
	const titleText = "Internal Server Error"
	qurlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		// `detail` carries a representative server-side internal — a
		// regression that surfaced this string to users would leak
		// implementation details (here: a synthetic stack hint).
		_, _ = fmt.Fprintf(w, `{"error":{"type":"about:blank","title":%q,"status":500,"detail":"db: connection to internal-host:5432 refused (stack: ...)","code":"SECRET_LEAK_HOOK"},"meta":{"request_id":"req_abc123"}}`, titleText)
	}))
	t.Cleanup(qurlSrv.Close)

	t.Setenv("QURL_API_KEY", "test-key")

	rec := newResponseURLRecorder(t)
	// Build the handler from a Config that already disables retries so
	// a 500 doesn't push the test toward the 30s retry budget. Cleaner
	// than mutating cfg.NewClient post-construction.
	h := NewHandler(Config{
		AuthProvider:       &auth.EnvProvider{EnvVar: "QURL_API_KEY"},
		SlackSigningSecret: testSigningSecret,
		NewClient: func(apiKey string) *client.Client {
			return client.New(qurlSrv.URL, apiKey, client.WithRetry(0))
		},
	})
	h.now = func() time.Time { return fixedNow }
	h.validateResponseURLFn = url.Parse
	seedGetAliasBinding(t, h, "T123")
	t.Cleanup(h.Wait)

	body := getTokenCommandBody("T123", "trig-err", rec.URL)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, body))
	h.Wait()

	posts := rec.Posts()
	if len(posts) != 1 {
		t.Fatalf("response_url POST count = %d, want 1", len(posts))
	}
	text := posts[0]["text"]

	// Detail MUST NOT appear — it can carry internal hostnames / DB
	// error strings. The leak check is the security-critical
	// assertion here; everything else is UX confirmation.
	for _, leak := range []string{"internal-host", "stack", "SECRET_LEAK_HOOK"} {
		if strings.Contains(text, leak) {
			t.Errorf("Detail leak on 5xx: response contains %q: %q", leak, text)
		}
	}
	// Upstream Title is suppressed along with Detail. Even though it
	// is usually bounded short text, it is service-owned copy and can
	// drift into operator-grade internals under regressions.
	if strings.Contains(text, titleText) {
		t.Errorf("Title leak on 5xx: response contains %q: %q", titleText, text)
	}
	if !strings.Contains(text, "req_abc123") {
		t.Errorf("expected RequestID req_abc123 in 5xx reply for support correlation, got: %q", text)
	}
	if !strings.Contains(text, "Could not reach qURL") {
		t.Errorf("expected service-unreachable copy on 5xx, got: %q", text)
	}
	if !strings.Contains(text, "Please try again") {
		t.Errorf("expected retry hint on 5xx, got: %q", text)
	}
}

// TestHandle_PoolSaturationDropsWithBusyAck fences back-pressure: when
// the bounded async pool is full, further requests get ackBusy
// instantly rather than queueing or unbounded-spawning. Two concurrent
// long-running workers fill the pool; the third request must drop.
func TestHandle_PoolSaturationDropsWithBusyAck(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	// Failure path: a t.Fatalf before the explicit releaseAll below
	// would otherwise leave the parked worker goroutines blocked,
	// hanging h.Wait() in t.Cleanup. sync.Once makes the explicit
	// close + the cleanup safe to both call.
	t.Cleanup(releaseAll)
	qurlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"resource_id":"r1"}}`))
	}))
	t.Cleanup(qurlSrv.Close)

	t.Setenv("QURL_API_KEY", "test-key")

	rec := newResponseURLRecorder(t)
	h := NewHandler(Config{
		AuthProvider:       &auth.EnvProvider{EnvVar: "QURL_API_KEY"},
		SlackSigningSecret: testSigningSecret,
		MaxConcurrentAsync: 2,
		NewClient: func(apiKey string) *client.Client {
			return client.New(qurlSrv.URL, apiKey, client.WithRetry(0))
		},
	})
	h.now = func() time.Time { return fixedNow }
	h.validateResponseURLFn = url.Parse
	seedGetAliasBinding(t, h, "T123")
	t.Cleanup(h.Wait)

	send := func(triggerID string) string {
		body := getTokenCommandBody("T123", triggerID, rec.URL)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, body))
		var ack map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &ack); err != nil {
			t.Fatalf("unmarshal ack: %v", err)
		}
		return ack["text"]
	}

	first := send("trig-1")
	second := send("trig-2")
	// Wait until both workers are actually parked in the qURL handler;
	// otherwise the third send could race ahead while a slot is still
	// unclaimed.
	waitFor(t, 2*time.Second, func() bool {
		// Two slots taken means a third reservation will fail. 2s
		// gives the race detector + a busy CI runner enough headroom.
		return len(h.sem) == 2
	})
	third := send("trig-3")

	if first != ackWorkingOnIt || second != ackWorkingOnIt {
		t.Errorf("first/second acks should be working-on-it; got %q / %q", first, second)
	}
	if third != ackBusy {
		t.Errorf("third (saturated) ack = %q, want %q", third, ackBusy)
	}

	// Release the parked workers so h.Wait() drains.
	releaseAll()
}

// TestHandle_PanicInAsyncWorkRecovers fences the panic-recovery defer
// in runAsync. A panicking auth.Provider must not crash the process or
// leak a wg slot — the goroutine returns cleanly via the recover, sem
// releases, and h.Wait drains.
func TestHandle_PanicInAsyncWorkRecovers(t *testing.T) {
	qurlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(qurlSrv.Close)

	rec := newResponseURLRecorder(t)
	h := NewHandler(Config{
		AuthProvider:       panickingProvider{},
		SlackSigningSecret: testSigningSecret,
		NewClient: func(apiKey string) *client.Client {
			return client.New(qurlSrv.URL, apiKey, client.WithRetry(0))
		},
	})
	h.now = func() time.Time { return fixedNow }
	h.validateResponseURLFn = url.Parse
	// Seed the alias so the worker resolves the token, passes the rate-limit
	// gate, and reaches authenticatedClient, where panickingProvider.APIKey
	// panics. Without the seed the worker would fail closed at LookupChannelAlias
	// (before the rate-limit gate) and never exercise the recover defer.
	seedGetAliasBinding(t, h, "T123")

	body := getTokenCommandBody("T123", "trig-panic", rec.URL)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, body))

	// h.Wait() blocks if the panic wasn't recovered cleanly (wg.Done
	// would leak); the test would hang on a regression that broke
	// the recover path.
	h.Wait()

	// Sem slot must be released too.
	if got := len(h.sem); got != 0 {
		t.Errorf("sem leak after panic: len(sem) = %d, want 0", got)
	}
}

// panickingProvider implements auth.Provider but panics on APIKey, used
// to exercise runAsync's panic-recovery defer.
type panickingProvider struct{}

func (panickingProvider) APIKey(_ context.Context, _ string) (string, error) {
	panic("provider panic for test")
}

func (panickingProvider) SupportsDeleteAPIKey() bool {
	// True is deliberate: this fake should stay on mutable-provider code paths
	// if a test reaches uninstall, while still panicking for panic-recovery tests.
	return true
}

func (panickingProvider) DeleteAPIKey(_ context.Context, _ string) error {
	panic("provider panic for test")
}

// TestValidateResponseURL fences the production validator: only HTTPS
// to hooks.slack.com is accepted. Anything else is a refusal — even a
// signature-passing request can't pivot the bot into an SSRF emitter.
// Successful returns must also have Scheme/Host pinned to the literal
// constants (the SSRF-sanitization contract CodeQL relies on).
func TestValidateResponseURL(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{name: "valid Slack hooks URL", raw: "https://hooks.slack.com/commands/T1/abc/xyz", wantErr: false},
		{name: "valid Slack hooks URL with explicit 443", raw: "https://hooks.slack.com:443/commands/T1/abc/xyz", wantErr: false},
		{name: "valid Slack hooks URL with mixed-case host", raw: "https://Hooks.Slack.Com/commands/T1/abc/xyz", wantErr: false},
		{name: "rejects http (no TLS)", raw: "http://hooks.slack.com/commands/T1/abc/xyz", wantErr: true},
		{name: "rejects non-Slack host", raw: "https://attacker.example/commands/T1/abc/xyz", wantErr: true},
		{name: "rejects subdomain trick", raw: "https://hooks.slack.com.attacker.example/path", wantErr: true},
		{name: "rejects embedded userinfo", raw: "https://user:pw@hooks.slack.com/commands/T1/abc/xyz", wantErr: true},
		{name: "rejects empty URL", raw: "", wantErr: true},
		{name: "rejects relative path", raw: "/oops", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, err := validateResponseURL(tc.raw)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateResponseURL(%q) err = %v, wantErr = %v", tc.raw, err, tc.wantErr)
			}
			if !tc.wantErr {
				// On success, Scheme and Host must be the pinned
				// literal constants (NOT propagated from input).
				// This is the CodeQL-recognizable sanitization
				// pattern; a regression that returned the parsed
				// URL unchanged would re-introduce the SSRF flow.
				if u.Scheme != "https" {
					t.Errorf("validated URL Scheme = %q, want %q", u.Scheme, "https")
				}
				if u.Host != slackResponseURLHost {
					t.Errorf("validated URL Host = %q, want %q", u.Host, slackResponseURLHost)
				}
			}
		})
	}
}

// TestResponseURLClient_RefusesRedirects fences the redirect-refusal
// posture on the default response_url client: a 30x from
// hooks.slack.com to any other host must NOT be followed. Without the
// CheckRedirect override, Go's default would silently follow up to 10
// redirects, defeating the validateResponseURL host pin.
func TestResponseURLClient_RefusesRedirects(t *testing.T) {
	h := NewHandler(Config{
		AuthProvider:       &auth.EnvProvider{EnvVar: "QURL_API_KEY"},
		SlackSigningSecret: testSigningSecret,
		NewClient:          func(string) *client.Client { return nil },
	})

	var followCount atomic.Int32
	var redirectTo string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		followCount.Add(1)
		// Always 302 to ourselves — without redirect-refusal the
		// follower would loop until Go's 10-redirect cap, accumulating
		// follow hits.
		http.Redirect(w, r, redirectTo, http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	redirectTo = srv.URL

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, http.NoBody)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := h.responseURLClient.Do(req)
	if err != nil {
		t.Fatalf("client returned error instead of surfacing 302: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	if resp.StatusCode != http.StatusFound {
		t.Errorf("status = %d, want 302 (caller sees the redirect, doesn't follow)", resp.StatusCode)
	}
	if got := followCount.Load(); got != 1 {
		t.Errorf("server hit count = %d, want 1 (any >1 means a redirect was followed)", got)
	}
}

// TestSanitizeAPIError fences the user-facing message contract for a
// range of qURL API error shapes. A regression that surfaced upstream
// Title/Detail or dropped RequestID would change the
// security/observability posture.
func TestSanitizeAPIError(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		prefix  string
		wantSub []string
		notSub  []string
	}{
		{
			name: "APIError with title and request id",
			err: &client.APIError{
				StatusCode: 500,
				Title:      "Internal Server Error",
				Detail:     "db: leaked-internal-host failed",
				RequestID:  "req_xyz",
			},
			prefix:  "Failed to create qURL",
			wantSub: []string{"Failed to create qURL", "req_xyz"},
			notSub:  []string{"Internal Server Error", "leaked-internal-host"},
		},
		{
			name: "APIError without title",
			err: &client.APIError{
				StatusCode: 500,
				Detail:     "internal error",
				RequestID:  "req_abc",
			},
			prefix:  "Failed to create qURL",
			wantSub: []string{"Failed to create qURL", "req_abc"},
			notSub:  []string{"internal error"},
		},
		{
			name:    "non-APIError falls back to prefix only",
			err:     errors.New("some opaque transport error"),
			prefix:  "Failed to list qURLs",
			wantSub: []string{"Failed to list qURLs"},
			notSub:  []string{"opaque transport"},
		},
		{
			name: "APIError title-only still falls back to static copy",
			err: &client.APIError{
				StatusCode: 500,
				Title:      "Internal Server Error.",
			},
			prefix:  "Failed to create qURL",
			wantSub: []string{"Failed to create qURL."},
			notSub:  []string{"Internal Server Error", ".."},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeAPIError(tc.err, tc.prefix)
			for _, s := range tc.wantSub {
				if !strings.Contains(got, s) {
					t.Errorf("sanitizeAPIError missing %q in %q", s, got)
				}
			}
			for _, s := range tc.notSub {
				if strings.Contains(got, s) {
					t.Errorf("sanitizeAPIError leaked %q in %q", s, got)
				}
			}
		})
	}
}

func newContextBlockingQURLServer(t *testing.T) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() {
		close(release)
		srv.CloseClientConnections()
		srv.Close()
	})
	return srv
}

// TestHandle_AsyncWorkObservesBaseContextCancellation fences the
// shutdown-cancellation contract: when h.baseCtx is canceled, an
// in-flight worker exits promptly rather than blocking shutdown.
func TestHandle_AsyncWorkObservesBaseContextCancellation(t *testing.T) {
	// Block until request cancellation; test cleanup also has an explicit
	// release so httptest.Server.Close cannot hang if the transport delays
	// propagating cancellation to r.Context().
	qurlSrv := newContextBlockingQURLServer(t)

	t.Setenv("QURL_API_KEY", "test-key")

	baseCtx, baseCancel := context.WithCancel(context.Background())
	rec := newResponseURLRecorder(t)
	h := NewHandler(Config{
		AuthProvider:       &auth.EnvProvider{EnvVar: "QURL_API_KEY"},
		SlackSigningSecret: testSigningSecret,
		BaseContext:        baseCtx,
		NewClient: func(apiKey string) *client.Client {
			return client.New(qurlSrv.URL, apiKey, client.WithRetry(0))
		},
	})
	h.now = func() time.Time { return fixedNow }
	h.validateResponseURLFn = url.Parse
	seedGetAliasBinding(t, h, "T123")

	body := getTokenCommandBody("T123", "trig-cancel", rec.URL)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, body))

	// Worker is parked in qURL upstream. Cancel baseCtx; worker
	// context fires; httpClient.Do returns; goroutine exits.
	baseCancel()

	done := make(chan struct{})
	go func() {
		h.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("h.Wait did not return within 2s after baseCtx cancellation")
	}
}

// TestHandle_OrphanAckPreventionOnBaseContextCancel fences the
// orphan-ack contract: when baseCtx cancels mid-flight (SIGTERM
// reaches the worker after the qURL call has started), the failure
// follow-up MUST still reach response_url. postResponse therefore uses
// a context decoupled from the worker's, scoped to responseURLTimeout.
func TestHandle_OrphanAckPreventionOnBaseContextCancel(t *testing.T) {
	// qURL upstream blocks on r.Context().Done() so the only exit is
	// cancellation. After baseCancel, c.Create returns ctx.Canceled —
	// processCreate then sanitizes and calls postResponse. If
	// postResponse used the worker ctx, that POST would fail too
	// (orphan ack); with a fresh context it should succeed.
	qurlSrv := newContextBlockingQURLServer(t)

	t.Setenv("QURL_API_KEY", "test-key")

	baseCtx, baseCancel := context.WithCancel(context.Background())
	rec := newResponseURLRecorder(t)
	h := NewHandler(Config{
		AuthProvider:       &auth.EnvProvider{EnvVar: "QURL_API_KEY"},
		SlackSigningSecret: testSigningSecret,
		BaseContext:        baseCtx,
		NewClient: func(apiKey string) *client.Client {
			return client.New(qurlSrv.URL, apiKey, client.WithRetry(0))
		},
	})
	h.now = func() time.Time { return fixedNow }
	h.validateResponseURLFn = url.Parse
	seedGetAliasBinding(t, h, "T123")

	body := getTokenCommandBody("T123", "trig-orphan", rec.URL)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, body))

	// Cancel baseCtx mid-flight (simulates SIGTERM). The worker's
	// qURL call returns ctx.Canceled, processCreate calls
	// postResponse with the failure, and that POST must land.
	baseCancel()
	h.Wait()

	posts := rec.Posts()
	if len(posts) != 1 {
		t.Fatalf("response_url POST count = %d, want 1 (failure follow-up must reach Slack on shutdown)", len(posts))
	}
	if !strings.Contains(posts[0]["text"], "Could not reach qURL") {
		t.Errorf("expected service-unreachable failure text in follow-up, got: %q", posts[0]["text"])
	}
}

// TestHandler_WaitTimeout fences the bounded-drain contract: if a
// worker outlives the budget, WaitTimeout returns false rather than
// blocking the caller indefinitely. This is the cheap insurance against
// a future regression where a worker stops honoring its ctx.
func TestHandler_WaitTimeout(t *testing.T) {
	// qURL upstream blocks indefinitely on a non-ctx-honoring channel,
	// simulating a worker that ignored ctx.
	block := make(chan struct{})
	qurlSrv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		<-block
	}))
	t.Cleanup(qurlSrv.Close)

	t.Setenv("QURL_API_KEY", "test-key")

	rec := newResponseURLRecorder(t)
	h := newTestHandler(t, qurlSrv)
	seedGetAliasBinding(t, h, "T123")
	// Register the block-release AFTER newTestHandler so LIFO runs it
	// BEFORE newTestHandler's t.Cleanup(h.Wait) — otherwise h.Wait
	// blocks on the parked worker and t.Cleanup deadlocks.
	t.Cleanup(func() { close(block) })

	body := getTokenCommandBody("T123", "trig-waittimeout", rec.URL)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, body))

	if drained := h.WaitTimeout(50 * time.Millisecond); drained {
		t.Errorf("WaitTimeout returned true with worker still parked; want false")
	}
}

// TestHandler_WaitTimeout_DrainsOnSuccess fences the success path:
// when workers complete inside the budget, WaitTimeout returns true.
func TestHandler_WaitTimeout_DrainsOnSuccess(t *testing.T) {
	qurlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"resource_id":"r1","qurl_link":"https://qurl.link/x"}}`))
	}))
	t.Cleanup(qurlSrv.Close)

	t.Setenv("QURL_API_KEY", "test-key")

	rec := newResponseURLRecorder(t)
	h := newTestHandler(t, qurlSrv)
	seedGetAliasBinding(t, h, "T123")

	body := getTokenCommandBody("T123", "trig-waittimeout-ok", rec.URL)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, body))

	if drained := h.WaitTimeout(2 * time.Second); !drained {
		t.Errorf("WaitTimeout returned false on a fast-completing worker; want true")
	}
}

// TestHandler_WaitTimeout_ZeroBudgetNoWorkers locks the shutdown edge
// case where lameduck + HTTP drain consume the full platform budget. A
// zero remaining budget should not log a bogus async-drain timeout when
// no workers are registered.
func TestHandler_WaitTimeout_ZeroBudgetNoWorkers(t *testing.T) {
	h := &Handler{}

	if drained := h.WaitTimeout(0); !drained {
		t.Errorf("WaitTimeout(0) returned false with no workers; want true")
	}
}

// TestHandler_WaitTimeout_ZeroBudgetWithWorker verifies the zero-budget
// fast path still reports timeout when a registered worker is actually
// pending.
func TestHandler_WaitTimeout_ZeroBudgetWithWorker(t *testing.T) {
	h := &Handler{}
	block := make(chan struct{})
	registered := make(chan struct{})

	h.Go(func() {
		close(registered)
		<-block
	})
	<-registered
	t.Cleanup(func() {
		close(block)
		h.Wait()
	})

	if drained := h.WaitTimeout(0); drained {
		t.Errorf("WaitTimeout(0) returned true with worker still parked; want false")
	}
}

// TestHandle_PostResponseRefusesRedirectsEndToEnd is the end-to-end
// counterpart to TestResponseURLClient_RefusesRedirects: it goes
// through processCreate → postResponse so a regression that wired the
// request through a non-default *http.Client at the call site is
// caught here even if the client unit test still passed.
func TestHandle_PostResponseRefusesRedirectsEndToEnd(t *testing.T) {
	qurlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"resource_id":"r1","qurl_link":"https://qurl.link/x"}}`))
	}))
	t.Cleanup(qurlSrv.Close)

	t.Setenv("QURL_API_KEY", "test-key")

	// Evil host — should never see traffic if redirect-refusal works.
	var evilHits atomic.Int32
	evilSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		evilHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(evilSrv.Close)

	// response_url server returns 302 to the evil host.
	var responseURLHits atomic.Int32
	responseSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		responseURLHits.Add(1)
		http.Redirect(w, r, evilSrv.URL, http.StatusFound)
	}))
	t.Cleanup(responseSrv.Close)

	h := newTestHandler(t, qurlSrv)
	seedGetAliasBinding(t, h, "T123")
	body := getTokenCommandBody("T123", "trig-redir-e2e", responseSrv.URL)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, body))
	h.Wait()

	if got := responseURLHits.Load(); got != 1 {
		t.Errorf("response_url server hit count = %d, want 1 (initial POST)", got)
	}
	if got := evilHits.Load(); got != 0 {
		t.Errorf("evil server hit count = %d, want 0 (redirect must be refused)", got)
	}
}

// TestHandle_ListExactMatchOnly fences the tightened "/qurl list"
// matcher: the looser HasPrefix(text, "list") form matched `listing`,
// `lists`, `list-foo` (silently routing them to processListResources)
// AND `list extra args` (which processListResources ignores). Now
// only the bare token reaches processListResources — anything else
// falls through to the unknown-subcommand branch.
func TestHandle_ListExactMatchOnly(t *testing.T) {
	cases := []string{
		"listing",
		"lists",
		"list-foo",
		"list foo bar", // trailing tokens — list takes no args
	}
	for _, text := range cases {
		t.Run(text, func(t *testing.T) {
			srv, hits := countingQURLServer(t)
			h := newTestHandler(t, srv)
			body := url.Values{
				"command": {"/qurl"},
				"text":    {text},
				"team_id": {"T123"},
			}.Encode()

			w := httptest.NewRecorder()
			h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, body))

			var ack map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &ack); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if !strings.Contains(ack["text"], "Unknown subcommand") {
				t.Errorf("expected 'Unknown subcommand' help nudge for %q, got: %q", text, ack["text"])
			}
			if got := hits.Load(); got != 0 {
				t.Errorf("upstream qURL hits for %q: got %d, want 0", text, got)
			}
		})
	}
}
