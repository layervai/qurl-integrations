package qurlapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// defaultHTTPClientTimeout bounds each HTTP attempt when an embedding
	// client does not provide an explicit nonzero timeout.
	defaultHTTPClientTimeout = 30 * time.Second
	// maxAttempts bounds the transient retry loop: one initial attempt plus
	// two retries.
	maxAttempts = 3
	// maxRetryAfter caps each Retry-After wait before the CLI retries a
	// replayable transient 429 or idempotent 503 response.
	maxRetryAfter        = 15 * time.Second
	maxRetryAfterSeconds = int(maxRetryAfter / time.Second)
	// drainLimit bounds how much of a discarded retry response is read for
	// connection reuse.
	drainLimit = 512 << 10
)

// transport decorates every request — SDK-issued and direct alike — with the
// CLI's headers and bounded transient retry. A 429 is retryable only for an
// allowlisted read-like request or when the caller supplied an Idempotency-Key.
// A 503 requires the key. It implements qurl.HTTPDoer.
type transport struct {
	next         *http.Client
	userAgent    string
	newRequestID func() string
	sleep        func(time.Duration)
	verbose      func(format string, args ...any)
}

type requestRetryIntentKey struct{}

func withRequestRetryIntent(req *http.Request, allowRetry bool) *http.Request {
	return req.WithContext(context.WithValue(req.Context(), requestRetryIntentKey{}, allowRetry))
}

func newTransport(cfg *Config) *transport {
	// Copy an injected client so the CLI can enforce its credential boundary
	// without mutating caller-owned test or embedding state. Redirect refusal
	// applies to every client, not only the default: net/http can otherwise
	// forward an Authorization header to a Location selected by the server.
	httpClient := &http.Client{Timeout: defaultHTTPClientTimeout}
	if cfg.HTTPClient != nil {
		*httpClient = *cfg.HTTPClient
		if httpClient.Timeout == 0 {
			httpClient.Timeout = defaultHTTPClientTimeout
		}
	}
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	newRequestID := cfg.NewRequestID
	if newRequestID == nil {
		newRequestID = randomRequestID
	}
	return &transport{
		next:         httpClient,
		userAgent:    "qurl-cli/" + cfg.Version,
		newRequestID: newRequestID,
		// nil means the context-aware timer path in backoff; tests inject a
		// recorder.
		sleep:   cfg.Sleep,
		verbose: cfg.Verbose,
	}
}

// Do sends req with the CLI headers set, retrying bounded transient responses.
// Direct CLI REST calls carry their explicit retry intent in the request
// context because qurl-go's registered HTTPDoer exposes only Do. SDK-issued
// requests have no marker and default fail-closed: only read-only methods or
// requests with an Idempotency-Key can retry. The
// X-Request-Id stays constant across retries of one logical request so the
// service can correlate them.
func (t *transport) Do(req *http.Request) (*http.Response, error) {
	// TODO(upstream-contract): qurl-go's registered HTTPDoer exposes only Do,
	// not DoOnce. Keep carrying the caller's explicit intent through the
	// request context until the upstream interface can express it directly.
	allowRetry, explicit := req.Context().Value(requestRetryIntentKey{}).(bool)
	if !explicit {
		allowRetry = retrySafeRequest(req)
	}
	return t.do(req, allowRetry)
}

// DoOnce applies the same headers and redaction as Do but never replays the
// request, including after a 429. It is reserved for writes whose service
// contract has no idempotency key.
func (t *transport) DoOnce(req *http.Request) (*http.Response, error) {
	return t.do(req, false)
}

func (t *transport) do(req *http.Request, allowRetry bool) (*http.Response, error) {
	req.Header.Set("User-Agent", t.userAgent)
	req.Header.Set("X-Request-Id", t.newRequestID())

	for attempt := 1; ; attempt++ {
		t.verbosef("> %s %s", req.Method, req.URL.Path)
		resp, err := t.next.Do(req)
		if err != nil {
			return nil, err
		}
		if !allowRetry || !retryableResponse(req, resp) || attempt >= maxAttempts {
			t.verbosef("< HTTP %d", resp.StatusCode)
			return resp, nil
		}
		replay, ok := replayableBody(req)
		if !ok {
			t.verbosef("< HTTP %d", resp.StatusCode)
			return resp, nil
		}
		wait := retryDelay(resp, attempt)
		discardResponse(resp)
		t.verbosef("< HTTP %d, retrying in %s", resp.StatusCode, wait)
		if err := t.backoff(req.Context(), wait); err != nil {
			return nil, err
		}
		if replay != nil {
			body, err := replay()
			if err != nil {
				return nil, err
			}
			req.Body = body
		}
	}
}

func retryableResponse(req *http.Request, resp *http.Response) bool {
	if resp.StatusCode == http.StatusTooManyRequests {
		return retrySafeRequest(req)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		return false
	}
	switch req.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	default:
		return strings.TrimSpace(req.Header.Get("Idempotency-Key")) != ""
	}
}

func retrySafeRequest(req *http.Request) bool {
	if strings.TrimSpace(req.Header.Get("Idempotency-Key")) != "" {
		return true
	}
	switch req.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	case http.MethodPost:
		// Resolve mints a short-lived link but does not change the resource.
		// qurl-go owns this request and cannot carry our private context marker,
		// so keep the one reviewed SDK write-like route explicit here. A 503
		// still requires an Idempotency-Key in retryableResponse.
		resource, ok := strings.CutPrefix(req.URL.EscapedPath(), "/v1/resources/")
		if !ok {
			return false
		}
		resource, ok = strings.CutSuffix(resource, "/resolve")
		return ok && resource != "" && !strings.Contains(resource, "/")
	default:
		return false
	}
}

// backoff waits out one retry delay, context-aware: cancellation during the
// wait returns promptly with the context's error instead of finishing the
// sleep. The injected recorder (tests) is honored when present; the context
// is still consulted on both sides of it so a canceled invocation never
// proceeds to another attempt.
func (t *transport) backoff(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if t.sleep != nil {
		t.sleep(d)
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// replayableBody reports whether req can be re-sent, returning the body
// factory for requests that carry one. Requests without a body always can;
// requests with a body only when the standard library recorded GetBody.
func replayableBody(req *http.Request) (func() (io.ReadCloser, error), bool) {
	if req.Body == nil || req.Body == http.NoBody {
		return nil, true
	}
	if req.GetBody == nil {
		return nil, false
	}
	return req.GetBody, true
}

// retryDelay honors a parseable Retry-After seconds value (capped) and falls
// back to a small linear backoff otherwise.
// TODO(upstream-contract): qURL API responses use the delta-seconds form. If
// the service adopts HTTP-date, update parseRetryAfterSeconds and its tests.
func retryDelay(resp *http.Response, attempt int) time.Duration {
	if secs, ok := parseRetryAfterSeconds(resp.Header.Get("Retry-After")); ok && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	return time.Duration(attempt) * 500 * time.Millisecond
}

// parseRetryAfterSeconds is the one Retry-After policy for transport waits and
// user-facing errors. It accepts the service's delta-seconds form and caps the
// value so the CLI never waits or tells a user to wait longer than it will.
func parseRetryAfterSeconds(value string) (int, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	seconds, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, false
	}
	// Keep the narrowing conversion on an explicitly bounded branch. Besides
	// making this safe on 32-bit Windows, this lets static analysis verify that
	// an arbitrary uint64 from the service can never overflow int.
	if seconds >= uint64(maxRetryAfterSeconds) {
		return maxRetryAfterSeconds, true
	}
	return int(seconds), true
}

func discardResponse(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, drainLimit))
	_ = resp.Body.Close()
}

func (t *transport) verbosef(format string, args ...any) {
	if t.verbose == nil {
		return
	}
	t.verbose("%s", Redact(fmt.Sprintf(format, args...)))
}

// randomRequestID mints an X-Request-Id: 16 random bytes, hex-encoded.
func randomRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "qurl-cli-request"
	}
	return hex.EncodeToString(b[:])
}
