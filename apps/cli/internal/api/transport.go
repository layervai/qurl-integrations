package qurlapi

import (
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
	// maxAttempts bounds the 429 retry loop: one initial attempt plus two
	// retries.
	maxAttempts = 3
	// maxRetryAfter caps how long a Retry-After header can make the CLI
	// wait per retry; anything longer surfaces the 429 instead of hanging.
	maxRetryAfter = 15 * time.Second
	// drainLimit bounds how much of a discarded retry response is read for
	// connection reuse.
	drainLimit = 512 << 10
)

// transport decorates every request — SDK-issued and direct alike — with the
// CLI's headers and bounded 429 retry. It implements qurl.HTTPDoer.
type transport struct {
	next         *http.Client
	userAgent    string
	newRequestID func() string
	sleep        func(time.Duration)
	verbose      func(format string, args ...any)
}

func newTransport(cfg *Config) *transport {
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				// Surface 3xx to the caller instead of forwarding the
				// Authorization header to a different URL.
				return http.ErrUseLastResponse
			},
		}
	}
	sleep := cfg.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	newRequestID := cfg.NewRequestID
	if newRequestID == nil {
		newRequestID = randomRequestID
	}
	return &transport{
		next:         httpClient,
		userAgent:    "qurl-cli/" + cfg.Version,
		newRequestID: newRequestID,
		sleep:        sleep,
		verbose:      cfg.Verbose,
	}
}

// Do sends req with the CLI headers set, retrying bounded times on 429. The
// X-Request-Id stays constant across retries of one logical request so the
// service can correlate them.
func (t *transport) Do(req *http.Request) (*http.Response, error) {
	req.Header.Set("User-Agent", t.userAgent)
	req.Header.Set("X-Request-Id", t.newRequestID())

	for attempt := 1; ; attempt++ {
		t.verbosef("> %s %s", req.Method, req.URL.Path)
		resp, err := t.next.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusTooManyRequests || attempt >= maxAttempts {
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
		t.verbosef("< HTTP 429, retrying in %s", wait)
		if req.Context().Err() != nil {
			return nil, req.Context().Err()
		}
		t.sleep(wait)
		if err := req.Context().Err(); err != nil {
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
func retryDelay(resp *http.Response, attempt int) time.Duration {
	if v := strings.TrimSpace(resp.Header.Get("Retry-After")); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
			d := time.Duration(secs) * time.Second
			if d > maxRetryAfter {
				return maxRetryAfter
			}
			return d
		}
	}
	return time.Duration(attempt) * 500 * time.Millisecond
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
