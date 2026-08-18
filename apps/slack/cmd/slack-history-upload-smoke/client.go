// The Slack Web API transport for this command: one read method, one retry, and the
// response envelope every read shares. The no-redirect policy that slacksmoke.NewHTTPClient
// sets is what several comments here depend on — a bearer token rides on every one of
// these requests.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slacksmoke"
)

type slackResponseStatus struct {
	OK       bool   `json:"ok"`
	Error    string `json:"error"`
	Needed   string `json:"needed"`
	Provided string `json:"provided"`
}

// slackResponse is what get decodes into. Embedding slackResponseStatus satisfies it,
// so a new read method cannot define a response type that forgets the ok/error check —
// the compiler refuses it, where the previous version re-parsed the whole body a second
// time to enforce the same thing at runtime. On a 4 MB history page that second parse
// walked every message object again to read four top-level fields.
type slackResponse interface {
	status() slackResponseStatus
}

func (s slackResponseStatus) status() slackResponseStatus { return s }

type slackResponseMetadata struct {
	NextCursor string `json:"next_cursor"`
}

// slackMessagesResponse keeps the messages raw so observeMessage can see the whole
// object, including whether the `files` key was present at all.
type slackMessagesResponse struct {
	slackResponseStatus
	Messages []json.RawMessage     `json:"messages"`
	Metadata slackResponseMetadata `json:"response_metadata"`
}

type slackClient struct {
	token      string
	baseURL    string
	userAgent  string
	httpClient *http.Client
	// sleep is the 429 wait, injected so tests do not spend real time in it.
	sleep func(context.Context, time.Duration) error
}

// get issues one read and decodes it into out, retrying once when Slack rate-limits.
// The retry is here rather than left to the operator because a tier-3 method read
// across a couple of dozen conversations will hit 429 on a busy workspace, and a scan
// that dies there measures nothing.
func (c *slackClient) get(ctx context.Context, method string, params url.Values, out slackResponse) error {
	retryAfter, err := c.getOnce(ctx, method, params, out)
	if err == nil || retryAfter <= 0 {
		return err
	}
	if retryAfter > maxRateLimitWait {
		return fmt.Errorf("%s: rate limited, Retry-After %s exceeds the %s cap", method, retryAfter, maxRateLimitWait)
	}
	if sleepErr := c.wait(ctx, retryAfter); sleepErr != nil {
		return sleepErr
	}
	_, err = c.getOnce(ctx, method, params, out)
	return err
}

func (c *slackClient) wait(ctx context.Context, d time.Duration) error {
	if c.sleep != nil {
		return c.sleep(ctx, d)
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

// getOnce returns the Retry-After duration alongside the error when Slack rate-limits,
// and zero otherwise, so get can tell a retryable refusal from a terminal one.
func (c *slackClient) getOnce(ctx context.Context, method string, params url.Values, out slackResponse) (time.Duration, error) {
	endpoint := c.baseURL + "/" + method
	if encoded := params.Encode(); encoded != "" {
		endpoint += "?" + encoded
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return 0, fmt.Errorf("%s request build: %w", method, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%s request: %w", method, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusTooManyRequests {
		slacksmoke.DrainResponseBody(resp.Body, maxSlackResponseBytes)
		return retryAfterDelay(resp.Header.Get("Retry-After")), fmt.Errorf("%s: rate limited", method)
	}
	if resp.StatusCode >= 300 {
		slacksmoke.DrainResponseBody(resp.Body, maxSlackResponseBytes)
		// Location is carried for 3xx because redirects are surfaced rather than followed
		// (see slacksmoke.NewHTTPClient), and "returned HTTP 302" alone hides an SSO portal.
		if location := sanitizeReportText(resp.Header.Get("Location")); location != "" {
			return 0, fmt.Errorf("%s returned HTTP %d redirecting to %s (not followed)", method, resp.StatusCode, location)
		}
		return 0, fmt.Errorf("%s returned HTTP %d", method, resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxSlackResponseBytes+1))
	if err != nil {
		return 0, fmt.Errorf("%s response read: %w", method, err)
	}
	if len(raw) > maxSlackResponseBytes {
		slacksmoke.DrainResponseBody(resp.Body, maxSlackResponseBytes)
		return 0, fmt.Errorf("%s response exceeded %d bytes", method, maxSlackResponseBytes)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		// The status, content type and length are not user content, and without them an
		// HTML SSO page from a corporate proxy is indistinguishable from a Slack outage.
		// The body itself stays out: quoting it would be the obvious content leak.
		return 0, fmt.Errorf("%s response is not JSON (HTTP %d, content-type %q, %d bytes)",
			method, resp.StatusCode, sanitizeReportText(resp.Header.Get("Content-Type")), len(raw))
	}
	return slackStatusError(method, resp.Header, out.status())
}

// slackStatusError turns the decoded envelope into an error, and reports a retry delay
// alongside it for the one refusal that is worth retrying.
//
// TODO(upstream-contract): Slack answers some rate limits with HTTP 200 and
// {"ok":false,"error":"ratelimited"} rather than a 429 status.
// slackWebAPIResponseFieldsError in slack_webapi.go maps that code to
// internal.NewSlackRateLimitError and its comment calls the branch load-bearing;
// retrying only on the status would die where production backs off. If Slack renames
// the code, this scan resumes failing hard on a shape production still retries.
func slackStatusError(method string, header http.Header, status slackResponseStatus) (time.Duration, error) {
	if status.OK {
		return 0, nil
	}
	reason := sanitizeReportText(status.Error)
	if reason == "" {
		reason = "not_ok"
	}
	if reason == "ratelimited" {
		return retryAfterDelay(header.Get("Retry-After")), fmt.Errorf("%s: %s", method, reason)
	}
	if needed := sanitizeReportText(status.Needed); needed != "" {
		return 0, fmt.Errorf("%s: %s (needed %s, provided %s)", method, reason, needed, sanitizeReportText(status.Provided))
	}
	return 0, fmt.Errorf("%s: %s", method, reason)
}

// retryAfterDelay reads Slack's Retry-After. Deliberately NOT named parseRetryAfter:
// shared/client has a function by that name whose default is the opposite of this
// one's — it returns 0 for a missing or garbled header, while a scan that cannot
// read the header should still pause rather than hammer the endpoint.
func retryAfterDelay(header string) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(header))
	if err != nil || seconds < 0 {
		return time.Second
	}
	// A literal "Retry-After: 0" still gets the floor: a zero wait would retry into the
	// same limiter immediately.
	if seconds == 0 {
		return time.Second
	}
	return time.Duration(seconds) * time.Second
}
