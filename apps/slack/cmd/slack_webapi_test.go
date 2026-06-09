package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/layervai/qurl-integrations/apps/slack/internal"
	"github.com/layervai/qurl-integrations/shared/auth"
)

const (
	testWorkspaceSlackBotToken        = "xoxb-123456789012345678901234567890"
	testRotatedWorkspaceSlackBotToken = "xoxb-223456789012345678901234567890"
)

type fakeSlackBotTokenProvider struct {
	token string
	err   error
	calls atomic.Int64
}

func (f *fakeSlackBotTokenProvider) SlackBotToken(context.Context, string) (string, error) {
	f.calls.Add(1)
	return f.token, f.err
}

type blockingSlackBotTokenProvider struct {
	token   string
	started chan struct{}
	unblock chan struct{}
	once    sync.Once
	calls   atomic.Int64
}

func (f *blockingSlackBotTokenProvider) SlackBotToken(context.Context, string) (string, error) {
	f.calls.Add(1)
	f.once.Do(func() { close(f.started) })
	<-f.unblock
	return f.token, nil
}

type capturingBlockingSlackBotTokenProvider struct {
	token   string
	started chan struct{}
	unblock chan struct{}
	once    sync.Once
	calls   atomic.Int64
}

func (f *capturingBlockingSlackBotTokenProvider) SlackBotToken(context.Context, string) (string, error) {
	f.calls.Add(1)
	token := f.token
	f.once.Do(func() { close(f.started) })
	<-f.unblock
	return token, nil
}

type panicOnceSlackBotTokenProvider struct {
	calls atomic.Int64
}

func (f *panicOnceSlackBotTokenProvider) SlackBotToken(context.Context, string) (string, error) {
	if f.calls.Add(1) == 1 {
		panic("simulated token lookup panic")
	}
	return testWorkspaceSlackBotToken, nil
}

func TestSlackOpenViewFuncPostsViewsOpenPayload(t *testing.T) {
	t.Parallel()
	var gotAuth string
	var gotUA string
	var gotContentType string
	var gotBody struct {
		TriggerID string          `json:"trigger_id"`
		View      json.RawMessage `json:"view"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		gotContentType = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	err := newSlackOpenViewFunc("xoxb-test", "qurl-slack/test", srv.URL)(context.Background(), "T_test", "trigger_test", []byte(`{"type":"modal"}`))
	if err != nil {
		t.Fatalf("views.open: %v", err)
	}
	if gotAuth != "Bearer xoxb-test" {
		t.Fatalf("Authorization = %q, want Bearer token", gotAuth)
	}
	if gotUA != "qurl-slack/test" {
		t.Fatalf("User-Agent = %q, want qurl-slack/test", gotUA)
	}
	if gotContentType != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", gotContentType)
	}
	if gotBody.TriggerID != "trigger_test" {
		t.Fatalf("trigger_id = %q, want trigger_test", gotBody.TriggerID)
	}
	if string(gotBody.View) != `{"type":"modal"}` {
		t.Fatalf("view = %s, want raw modal JSON", string(gotBody.View))
	}
}

func TestSlackOpenViewFuncUsesWorkspaceTokenLookup(t *testing.T) {
	t.Parallel()
	var gotTeam string
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	openView := newSlackOpenViewFuncWithTokenLookup(func(_ context.Context, teamID string) (string, error) {
		gotTeam = teamID
		return "xoxb-workspace-token", nil
	}, "qurl-slack/test", srv.URL, nil)
	if err := openView(context.Background(), "T_lookup", "trigger_test", []byte(`{"type":"modal"}`)); err != nil {
		t.Fatalf("views.open: %v", err)
	}
	if gotTeam != "T_lookup" {
		t.Fatalf("lookup teamID = %q, want T_lookup", gotTeam)
	}
	if gotAuth != "Bearer xoxb-workspace-token" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
}

func TestWorkspaceSlackTokenLookupCachesDDBToken(t *testing.T) {
	provider := &fakeSlackBotTokenProvider{token: testWorkspaceSlackBotToken}
	now := time.Unix(1800000000, 0)
	lookup, _ := newWorkspaceSlackTokenLookupWithInvalidation(provider, "", time.Minute, func() time.Time {
		return now
	})

	for i := 0; i < 2; i++ {
		token, err := lookup(context.Background(), "T_cache")
		if err != nil {
			t.Fatalf("lookup %d: %v", i, err)
		}
		if token != testWorkspaceSlackBotToken {
			t.Fatalf("token = %q, want cached workspace token", token)
		}
	}
	if calls := provider.calls.Load(); calls != 1 {
		t.Fatalf("provider calls before TTL = %d, want 1", calls)
	}

	now = now.Add(time.Minute + time.Second)
	if _, err := lookup(context.Background(), "T_cache"); err != nil {
		t.Fatalf("lookup after TTL: %v", err)
	}
	if calls := provider.calls.Load(); calls != 2 {
		t.Fatalf("provider calls after TTL = %d, want 2", calls)
	}
}

func TestWorkspaceSlackTokenLookupFallsBackWhenUnset(t *testing.T) {
	provider := &fakeSlackBotTokenProvider{err: auth.ErrSlackBotTokenNotConfigured}
	now := time.Unix(1800000000, 0)
	lookup, _ := newWorkspaceSlackTokenLookupWithInvalidation(provider, "xoxb-fallback", time.Minute, func() time.Time {
		return now
	})

	for i := 0; i < 2; i++ {
		token, err := lookup(context.Background(), "T_legacy")
		if err != nil {
			t.Fatalf("lookup fallback %d: %v", i, err)
		}
		if token != "xoxb-fallback" {
			t.Fatalf("token = %q, want fallback", token)
		}
	}
	if calls := provider.calls.Load(); calls != 1 {
		t.Fatalf("provider calls before negative TTL = %d, want 1", calls)
	}

	now = now.Add(slackWorkspaceTokenNegativeCacheTTL + time.Second)
	if _, err := lookup(context.Background(), "T_legacy"); err != nil {
		t.Fatalf("lookup fallback after negative TTL: %v", err)
	}
	if calls := provider.calls.Load(); calls != 2 {
		t.Fatalf("provider calls after negative TTL = %d, want 2", calls)
	}
}

func TestWorkspaceSlackTokenLookupInvalidationPurgesCache(t *testing.T) {
	provider := &fakeSlackBotTokenProvider{token: testWorkspaceSlackBotToken}
	lookup, purge := newWorkspaceSlackTokenLookupWithInvalidation(provider, "", time.Minute, nil)

	token, err := lookup(context.Background(), "T_rotate")
	if err != nil {
		t.Fatalf("first lookup: %v", err)
	}
	if token != testWorkspaceSlackBotToken {
		t.Fatalf("token = %q, want initial token", token)
	}
	provider.token = testRotatedWorkspaceSlackBotToken
	token, err = lookup(context.Background(), "T_rotate")
	if err != nil {
		t.Fatalf("cached lookup: %v", err)
	}
	if token != testWorkspaceSlackBotToken {
		t.Fatalf("token = %q, want cached initial token", token)
	}

	purge("T_rotate")
	token, err = lookup(context.Background(), "T_rotate")
	if err != nil {
		t.Fatalf("lookup after purge: %v", err)
	}
	if token != testRotatedWorkspaceSlackBotToken {
		t.Fatalf("token = %q, want rotated token", token)
	}
	if calls := provider.calls.Load(); calls != 2 {
		t.Fatalf("provider calls = %d, want 2", calls)
	}
}

func TestWorkspaceSlackTokenLookupPurgeDetachesStaleInFlight(t *testing.T) {
	provider := &capturingBlockingSlackBotTokenProvider{
		token:   testWorkspaceSlackBotToken,
		started: make(chan struct{}),
		unblock: make(chan struct{}),
	}
	var unblockOnce sync.Once
	t.Cleanup(func() {
		unblockOnce.Do(func() { close(provider.unblock) })
	})
	lookup, purge := newWorkspaceSlackTokenLookupWithInvalidation(provider, "", time.Minute, nil)
	first := make(chan struct {
		token string
		err   error
	}, 1)

	go func() {
		token, err := lookup(context.Background(), "T_race")
		first <- struct {
			token string
			err   error
		}{token: token, err: err}
	}()

	select {
	case <-provider.started:
	case <-time.After(5 * time.Second):
		t.Fatal("provider lookup did not start")
	}
	provider.token = testRotatedWorkspaceSlackBotToken
	purge("T_race")
	unblockOnce.Do(func() { close(provider.unblock) })

	got := <-first
	if got.err != nil {
		t.Fatalf("first lookup: %v", got.err)
	}
	if got.token != testWorkspaceSlackBotToken {
		t.Fatalf("first token = %q, want stale in-flight token", got.token)
	}

	token, err := lookup(context.Background(), "T_race")
	if err != nil {
		t.Fatalf("lookup after stale in-flight finishes: %v", err)
	}
	if token != testRotatedWorkspaceSlackBotToken {
		t.Fatalf("token after purge = %q, want rotated token", token)
	}
	if calls := provider.calls.Load(); calls != 2 {
		t.Fatalf("provider calls = %d, want 2", calls)
	}
}

func TestWorkspaceSlackTokenLookupCollapsesConcurrentMisses(t *testing.T) {
	provider := &blockingSlackBotTokenProvider{
		token:   testWorkspaceSlackBotToken,
		started: make(chan struct{}),
		unblock: make(chan struct{}),
	}
	var unblockOnce sync.Once
	t.Cleanup(func() {
		unblockOnce.Do(func() { close(provider.unblock) })
	})
	lookup, _ := newWorkspaceSlackTokenLookupWithInvalidation(provider, "", time.Minute, nil)

	const callers = 8
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			defer wg.Done()
			token, err := lookup(context.Background(), "T_singleflight")
			if err != nil {
				errs <- err
				return
			}
			if token != testWorkspaceSlackBotToken {
				errs <- errors.New("unexpected token")
			}
		}()
	}

	select {
	case <-provider.started:
	case <-time.After(5 * time.Second):
		t.Fatal("provider lookup did not start")
	}
	unblockOnce.Do(func() { close(provider.unblock) })
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("lookup error: %v", err)
		}
	}
	if calls := provider.calls.Load(); calls != 1 {
		t.Fatalf("provider calls = %d, want 1", calls)
	}
}

func TestWorkspaceSlackTokenLookupReleasesInFlightOnPanic(t *testing.T) {
	provider := &panicOnceSlackBotTokenProvider{}
	lookup, _ := newWorkspaceSlackTokenLookupWithInvalidation(provider, "", time.Minute, nil)

	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("first lookup should panic")
			}
		}()
		_, _ = lookup(context.Background(), "T_panic")
	}()

	token, err := lookup(context.Background(), "T_panic")
	if err != nil {
		t.Fatalf("second lookup should start after panic cleanup: %v", err)
	}
	if token != testWorkspaceSlackBotToken {
		t.Fatalf("token = %q, want workspace token", token)
	}
	if calls := provider.calls.Load(); calls != 2 {
		t.Fatalf("provider calls = %d, want 2", calls)
	}
}

func TestWorkspaceSlackTokenLookupCacheSweepsExpiredEntries(t *testing.T) {
	at := time.Unix(1800000000, 0)
	cache := &workspaceSlackTokenLookupCache{
		positive: map[string]cachedSlackBotToken{
			"T_expired": {token: testWorkspaceSlackBotToken, expiresAt: at.Add(-time.Second)},
			"T_fresh":   {token: testWorkspaceSlackBotToken, expiresAt: at.Add(time.Minute)},
		},
		negative: map[string]time.Time{
			"T_negative_expired": at.Add(-time.Second),
			"T_negative_fresh":   at.Add(time.Minute),
		},
		inFlight: map[string]*workspaceSlackTokenLookupCall{},
		fallbackWarned: map[string]struct{}{
			"T_negative_expired": {},
			"T_negative_fresh":   {},
		},
	}

	start := cache.getOrStart("T_new", time.Minute, at)
	if !start.owner {
		t.Fatal("new team should start a provider lookup")
	}
	if _, ok := cache.positive["T_expired"]; ok {
		t.Fatal("expired positive cache entry was not swept")
	}
	if _, ok := cache.negative["T_negative_expired"]; ok {
		t.Fatal("expired negative cache entry was not swept")
	}
	if _, ok := cache.positive["T_fresh"]; !ok {
		t.Fatal("fresh positive cache entry should remain")
	}
	if _, ok := cache.negative["T_negative_fresh"]; !ok {
		t.Fatal("fresh negative cache entry should remain")
	}
	if _, ok := cache.fallbackWarned["T_negative_expired"]; ok {
		t.Fatal("expired negative cache entry should clear fallback warning state")
	}
	if _, ok := cache.fallbackWarned["T_negative_fresh"]; !ok {
		t.Fatal("fresh negative cache entry should keep fallback warning state")
	}
}

func TestSlackOpenViewFuncLookupErrorSkipsRequest(t *testing.T) {
	t.Parallel()
	var called atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called.Store(true)
	}))
	t.Cleanup(srv.Close)

	openView := newSlackOpenViewFuncWithTokenLookup(func(context.Context, string) (string, error) {
		return "", errors.New("missing workspace token")
	}, "qurl-slack/test", srv.URL, nil)
	err := openView(context.Background(), "T_missing", "trigger_test", []byte(`{"type":"modal"}`))
	if err == nil || !strings.Contains(err.Error(), "token lookup") {
		t.Fatalf("error = %v, want token lookup error", err)
	}
	if called.Load() {
		t.Fatal("views.open request should not be sent when token lookup fails")
	}
}

func TestSlackOpenViewFuncDefaultsUserAgent(t *testing.T) {
	t.Parallel()
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	err := newSlackOpenViewFunc("xoxb-test", "", srv.URL)(context.Background(), "T_test", "trigger_test", []byte(`{"type":"modal"}`))
	if err != nil {
		t.Fatalf("views.open: %v", err)
	}
	if gotUA != defaultSlackAPIUserAgent {
		t.Fatalf("User-Agent = %q, want %q", gotUA, defaultSlackAPIUserAgent)
	}
}

func TestSlackOpenViewFuncSurfacesSlackError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"error":"invalid_trigger"}`))
	}))
	t.Cleanup(srv.Close)

	err := newSlackOpenViewFunc("xoxb-test", "", srv.URL)(context.Background(), "T_test", "trigger_test", []byte(`{"type":"modal"}`))
	if !errors.Is(err, internal.ErrSlackTriggerExpired) || !strings.Contains(err.Error(), "invalid_trigger") {
		t.Fatalf("error = %v, want trigger-expired sentinel wrapping invalid_trigger", err)
	}
}

func TestSlackOpenViewFuncSurfacesRateLimit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name           string
		handler        http.HandlerFunc
		wantRetryAfter string
	}{
		{
			name:           "http 429",
			wantRetryAfter: "2",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "2")
				w.WriteHeader(http.StatusTooManyRequests)
			},
		},
		{
			name:           "json ratelimited",
			wantRetryAfter: "3",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "3")
				_, _ = w.Write([]byte(`{"ok":false,"error":"ratelimited"}`))
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(tc.handler)
			t.Cleanup(srv.Close)

			err := newSlackOpenViewFunc("xoxb-test", "", srv.URL)(context.Background(), "T_test", "trigger_test", []byte(`{"type":"modal"}`))
			if !errors.Is(err, internal.ErrSlackRateLimited) {
				t.Fatalf("error = %v, want rate-limited sentinel", err)
			}
			if got := internal.SlackRateLimitRetryAfter(err); got != tc.wantRetryAfter {
				t.Fatalf("Retry-After = %q, want %q", got, tc.wantRetryAfter)
			}
		})
	}
}

func TestSlackOpenViewFuncRejectsInvalidViewJSON(t *testing.T) {
	t.Parallel()

	for _, raw := range [][]byte{
		nil,
		[]byte(``),
		[]byte(`   `),
		[]byte(`not-json`),
	} {
		err := newSlackOpenViewFunc("xoxb-test", "", "https://slack.invalid/views.open")(context.Background(), "T_test", "trigger_test", raw)
		if err == nil || !strings.Contains(err.Error(), "invalid view JSON") {
			t.Fatalf("input %q error = %v, want invalid view JSON", raw, err)
		}
	}
}

func TestSlackOpenViewFuncRejectsNonObjectViewJSON(t *testing.T) {
	t.Parallel()

	for _, raw := range [][]byte{
		[]byte(`null`),
		[]byte(`[1,2]`),
		[]byte(`"str"`),
		[]byte(`42`),
	} {
		err := newSlackOpenViewFunc("xoxb-test", "", "https://slack.invalid/views.open")(context.Background(), "T_test", "trigger_test", raw)
		if err == nil || !strings.Contains(err.Error(), "invalid view JSON") {
			t.Fatalf("input %s error = %v, want invalid view JSON", raw, err)
		}
	}
}

func TestSlackOpenViewFuncSurfacesHTTPError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)

	err := newSlackOpenViewFunc("xoxb-test", "", srv.URL)(context.Background(), "T_test", "trigger_test", []byte(`{"type":"modal"}`))
	if err == nil || !strings.Contains(err.Error(), "HTTP 502") {
		t.Fatalf("error = %v, want HTTP 502", err)
	}
}

func TestSlackOpenViewFuncSurfacesRedirectAsHTTPError(t *testing.T) {
	t.Parallel()

	err := slackOpenViewResponseError(http.StatusFound, http.Header{}, []byte(`<a href="/elsewhere">Found</a>`))
	if err == nil || !strings.Contains(err.Error(), "HTTP 302") {
		t.Fatalf("error = %v, want HTTP 302 redirect error", err)
	}
	if strings.Contains(err.Error(), "response JSON") {
		t.Fatalf("error = %v, want redirect handled before JSON parse", err)
	}
}

func TestSlackOpenViewFuncSurfacesEmptyRedirectBody(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv.Close)

	err := newSlackOpenViewFunc("xoxb-test", "", srv.URL)(context.Background(), "T_test", "trigger_test", []byte(`{"type":"modal"}`))
	if err == nil || err.Error() != "views.open returned HTTP 307" {
		t.Fatalf("error = %v, want bare HTTP 307", err)
	}
}

func TestSlackOpenViewFuncCapsHTTPErrorBodySnippet(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(strings.Repeat("<html>gateway</html>", 40)))
	}))
	t.Cleanup(srv.Close)

	err := newSlackOpenViewFunc("xoxb-test", "", srv.URL)(context.Background(), "T_test", "trigger_test", []byte(`{"type":"modal"}`))
	if err == nil || !strings.Contains(err.Error(), "HTTP 502") {
		t.Fatalf("error = %v, want HTTP 502", err)
	}
	maxErrLen := len("views.open returned HTTP 502: ") + slackAPIMaxErrorSnippetBytes + len("...")
	if len(err.Error()) > maxErrLen {
		t.Fatalf("error length = %d, want <= %d; error = %q", len(err.Error()), maxErrLen, err.Error())
	}
}

func TestSlackOpenViewFuncMakesHTTPErrorBodySnippetPrintable(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<script>\x00alert(1)</script>"))
	}))
	t.Cleanup(srv.Close)

	err := newSlackOpenViewFunc("xoxb-test", "", srv.URL)(context.Background(), "T_test", "trigger_test", []byte(`{"type":"modal"}`))
	if err == nil || !strings.Contains(err.Error(), "<script>?alert(1)</script>") {
		t.Fatalf("error = %v, want printable body snippet", err)
	}
	if strings.Contains(err.Error(), "\x00") {
		t.Fatalf("error = %q, want control characters replaced", err.Error())
	}
}

func TestSlackOpenViewBodySnippetTruncatesOnUTF8Boundary(t *testing.T) {
	t.Parallel()

	got := slackAPIBodySnippet([]byte(strings.Repeat("\U0001F9EA", 100)))

	if !utf8.ValidString(got) {
		t.Fatalf("snippet is not valid UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("snippet = %q, want truncation suffix", got)
	}
	if len(got) > slackAPIMaxErrorSnippetBytes {
		t.Fatalf("snippet length = %d, want <= %d", len(got), slackAPIMaxErrorSnippetBytes)
	}
}

func TestSlackOpenViewBodySnippetRepairsMalformedUTF8(t *testing.T) {
	t.Parallel()

	raw := append(bytes.Repeat([]byte{0x80}, slackAPIMaxErrorSnippetBytes+10), []byte("tail")...)
	got := slackAPIBodySnippet(raw)

	if !utf8.ValidString(got) {
		t.Fatalf("snippet is not valid UTF-8: %q", got)
	}
	if got != "?tail" {
		t.Fatalf("snippet = %q, want repaired malformed bytes", got)
	}
}

func TestSlackOpenViewFuncSurfacesMalformedJSON(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`not json`))
	}))
	t.Cleanup(srv.Close)

	err := newSlackOpenViewFunc("xoxb-test", "", srv.URL)(context.Background(), "T_test", "trigger_test", []byte(`{"type":"modal"}`))
	if err == nil || !strings.Contains(err.Error(), "response JSON") {
		t.Fatalf("error = %v, want response JSON", err)
	}
	if !strings.Contains(err.Error(), "not json") {
		t.Fatalf("error = %v, want bounded body snippet", err)
	}
}

func TestSlackOpenViewFuncSurfacesHTMLSuccessAsMalformedJSON(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html>not slack json</html>`))
	}))
	t.Cleanup(srv.Close)

	err := newSlackOpenViewFunc("xoxb-test", "", srv.URL)(context.Background(), "T_test", "trigger_test", []byte(`{"type":"modal"}`))
	if err == nil || !strings.Contains(err.Error(), "response JSON") {
		t.Fatalf("error = %v, want response JSON for HTTP 200 HTML body", err)
	}
	if !strings.Contains(err.Error(), "<html>not slack json</html>") {
		t.Fatalf("error = %v, want HTML body snippet", err)
	}
}

func TestSlackOpenViewFuncSurfacesEmptyResponseBody(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	err := newSlackOpenViewFunc("xoxb-test", "", srv.URL)(context.Background(), "T_test", "trigger_test", []byte(`{"type":"modal"}`))
	if err == nil || !strings.Contains(err.Error(), "empty response body") {
		t.Fatalf("error = %v, want empty response body", err)
	}
}

func TestSlackOpenViewFuncSurfacesNotOKFallback(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false}`))
	}))
	t.Cleanup(srv.Close)

	err := newSlackOpenViewFunc("xoxb-test", "", srv.URL)(context.Background(), "T_test", "trigger_test", []byte(`{"type":"modal"}`))
	if err == nil || !strings.Contains(err.Error(), "not_ok") {
		t.Fatalf("error = %v, want not_ok fallback", err)
	}
}

func TestSlackOpenViewFuncMakesSlackErrorCodePrintable(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("{\"ok\":false,\"error\":\"bad\\ncode\"}"))
	}))
	t.Cleanup(srv.Close)

	err := newSlackOpenViewFunc("xoxb-test", "", srv.URL)(context.Background(), "T_test", "trigger_test", []byte(`{"type":"modal"}`))
	if err == nil || !strings.Contains(err.Error(), "bad code") {
		t.Fatalf("error = %v, want printable Slack error code", err)
	}
	if strings.Contains(err.Error(), "\n") {
		t.Fatalf("error = %q, want newline normalized", err.Error())
	}
}

func TestSlackOpenViewFuncAcceptsLargeSuccessfulViewEcho(t *testing.T) {
	t.Parallel()
	successBody, err := json.Marshal(map[string]any{
		"ok": true,
		"view": map[string]any{
			"id":               "V_test",
			"team_id":          "T_test",
			"private_metadata": strings.Repeat("m", 1024),
			"state":            strings.Repeat("s", 8192),
		},
	})
	if err != nil {
		t.Fatalf("marshal success body: %v", err)
	}
	if len(successBody) <= 4096 {
		t.Fatalf("test body length = %d, want larger than old 4096-byte cap", len(successBody))
	}
	if len(successBody) > slackViewsOpenResponseBodyLimit {
		t.Fatalf("test body length = %d, want within views.open cap", len(successBody))
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(successBody)
	}))
	t.Cleanup(srv.Close)

	err = newSlackOpenViewFunc("xoxb-test", "", srv.URL)(context.Background(), "T_test", "trigger_test", []byte(`{"type":"modal"}`))
	if err != nil {
		t.Fatalf("views.open: %v", err)
	}
}

func TestSlackOpenViewFuncAcceptsResponseAtBodyLimit(t *testing.T) {
	t.Parallel()
	paddingLen := slackViewsOpenResponseBodyLimit - len(`{"ok":true,"padding":""}`)
	if paddingLen <= 0 {
		t.Fatalf("invalid slackViewsOpenResponseBodyLimit: %d", slackViewsOpenResponseBodyLimit)
	}
	successBody := `{"ok":true,"padding":"` + strings.Repeat("x", paddingLen) + `"}`
	if len(successBody) != slackViewsOpenResponseBodyLimit {
		t.Fatalf("test body length = %d, want %d", len(successBody), slackViewsOpenResponseBodyLimit)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(successBody))
	}))
	t.Cleanup(srv.Close)

	err := newSlackOpenViewFunc("xoxb-test", "", srv.URL)(context.Background(), "T_test", "trigger_test", []byte(`{"type":"modal"}`))
	if err != nil {
		t.Fatalf("views.open exactly at body limit: %v", err)
	}
}

func TestSlackOpenViewFuncSurfacesOversizedResponse(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", slackViewsOpenResponseBodyLimit+1)))
	}))
	t.Cleanup(srv.Close)

	err := newSlackOpenViewFunc("xoxb-test", "", srv.URL)(context.Background(), "T_test", "trigger_test", []byte(`{"type":"modal"}`))
	if err == nil || !strings.Contains(err.Error(), "exceeded 65536 bytes") {
		t.Fatalf("error = %v, want oversized response", err)
	}
}

func TestSlackOpenViewFuncRefusesRedirects(t *testing.T) {
	t.Parallel()
	var redirected atomic.Bool
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirected.Store(true)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(redirectTarget.Close)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectTarget.URL, http.StatusFound)
	}))
	t.Cleanup(redirector.Close)

	err := newSlackOpenViewFunc("xoxb-test", "", redirector.URL)(context.Background(), "T_test", "trigger_test", []byte(`{"type":"modal"}`))
	if err == nil {
		t.Fatal("views.open followed redirect and returned nil error")
	}
	if redirected.Load() {
		t.Fatal("views.open followed redirect target")
	}
}

func TestSlackOpenViewFuncDrainsAndClosesOversizedResponse(t *testing.T) {
	t.Parallel()
	body := &trackingReadCloser{reader: strings.NewReader(strings.Repeat("x", slackViewsOpenResponseBodyLimit+1024))}
	httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       body,
		}, nil
	})}

	err := newSlackOpenViewFuncWithClient("xoxb-test", "", "https://slack.test/views.open", httpClient)(context.Background(), "T_test", "trigger_test", []byte(`{"type":"modal"}`))
	if err == nil || !strings.Contains(err.Error(), "exceeded 65536 bytes") {
		t.Fatalf("error = %v, want oversized response", err)
	}
	if !body.sawEOF.Load() {
		t.Fatal("oversized response body was not drained to EOF")
	}
	if !body.closed.Load() {
		t.Fatal("oversized response body was not closed")
	}
}

func TestSlackOpenViewFuncReadsAndClosesSuccessfulResponse(t *testing.T) {
	t.Parallel()
	body := &trackingReadCloser{reader: strings.NewReader(`{"ok":true}`)}
	httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       body,
		}, nil
	})}

	err := newSlackOpenViewFuncWithClient("xoxb-test", "", "https://slack.test/views.open", httpClient)(context.Background(), "T_test", "trigger_test", []byte(`{"type":"modal"}`))
	if err != nil {
		t.Fatalf("views.open: %v", err)
	}
	if !body.sawEOF.Load() {
		t.Fatal("successful response body was not read to EOF")
	}
	if !body.closed.Load() {
		t.Fatal("successful response body was not closed")
	}
}

func TestSlackOpenViewFuncPropagatesContextCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		cancel()
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}

	err := newSlackOpenViewFuncWithClient("xoxb-test", "", "https://slack.test/views.open", httpClient)(ctx, "T_test", "trigger_test", []byte(`{"type":"modal"}`))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestPrintableLogSnippetNormalizesUnicodeLineSeparators(t *testing.T) {
	t.Parallel()

	got := printableLogSnippet("alpha\u2028beta\u2029gamma")

	if got != "alpha beta gamma" {
		t.Fatalf("printableLogSnippet = %q, want Unicode separators normalized", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type trackingReadCloser struct {
	reader *strings.Reader
	sawEOF atomic.Bool
	closed atomic.Bool
}

func (b *trackingReadCloser) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	if errors.Is(err, io.EOF) {
		b.sawEOF.Store(true)
	}
	return n, err
}

func (b *trackingReadCloser) Close() error {
	b.closed.Store(true)
	return nil
}
