package internal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"runtime/debug"
	"strconv"
	"strings"

	"github.com/layervai/qurl-integrations/shared/client"
)

// Slack slash-command form-field names. Slack POSTs slash-command
// bodies as application/x-www-form-urlencoded; these are the keys the
// handler reads off the parsed values. Centralizing keeps a typo on
// one read site (which silently yields "") from going un-fenced.
//
// fieldResponseURL in particular is treated as opaque — slashing into
// it would risk SSRF if the signature gate ever broke, so postResponse
// validates the scheme and host before dialing.
const (
	fieldResponseURL = "response_url"
	fieldTeamID      = "team_id"
	fieldUserID      = "user_id"
	fieldChannelID   = "channel_id"
	fieldCommand     = "command"
	fieldText        = "text"
	fieldTriggerID   = "trigger_id"
)

// slackResponseURLHost is Slack's webhook ingress for slash-command
// follow-ups. Pinning the host before dialing is defense-in-depth: a
// supply-chain compromise that flipped some Slack-supplied URL would
// otherwise let attackers turn this binary into an SSRF emitter.
// The host string is the only Slack-controlled identifier the Slack
// docs guarantee for this surface.
const slackResponseURLHost = "hooks.slack.com"

// runAsync acks the request synchronously with ackWorkingOnIt and runs
// `work` in a bounded-pool goroutine. Returns after the ack is written.
//
// Concurrency contract:
//   - sem reservation must succeed before wg.Add and ack — otherwise a
//     burst that overflows the pool would still consume the user's
//     "Working on it…" expectation. Saturation gets ackBusy instead.
//   - wg.Add MUST happen on the request goroutine (before `go`). Adding
//     inside the spawned goroutine races with Wait() during shutdown.
//   - The goroutine context derives from h.baseCtx, NOT r.Context() —
//     r.Context() cancels as soon as ServeHTTP returns, which is
//     immediately after the ack here.
func (h *Handler) runAsync(w http.ResponseWriter, command string, values url.Values, work func(ctx context.Context, log *slog.Logger)) {
	// Bind the per-request log scope once. Every emission below this
	// point inherits team/channel/trigger attribution. The form-field
	// values are user-controlled but slog's JSON handler escapes
	// control bytes in attribute values — same log-injection posture
	// as the request-path slog sites in handler.go.
	log := slog.With(
		"command", command,
		"team_id", values.Get(fieldTeamID),
		"channel_id", values.Get(fieldChannelID),
		"trigger_id", values.Get(fieldTriggerID),
	)

	select {
	case h.sem <- struct{}{}:
	default:
		log.Warn("async pool saturated — dropping request")
		respondSlack(w, ackBusy)
		return
	}

	h.wg.Add(1)
	go func() {
		// Defer LIFO is load-bearing here: on panic, the recover
		// runs FIRST (innermost), absorbs the panic, then sem
		// release and wg.Done run unwinding outward. Reordering
		// these — putting recover above the sem release, say —
		// would silently leak a slot and a wg counter on every
		// panic. The ctx cancel is innermost-of-innermost so
		// children of that ctx see cancellation before the worker
		// frame returns.
		defer h.wg.Done()
		defer func() { <-h.sem }()
		defer func() {
			if rec := recover(); rec != nil {
				// A panicking goroutine in a long-lived process is
				// disqualifying — log + stack so the cause is in
				// CloudWatch, then swallow so the deferred sem release
				// and wg.Done still run.
				log.Error("panic in async slash-command worker", "recover", rec, "stack", string(debug.Stack()))
			}
		}()

		ctx, cancel := context.WithTimeout(h.baseCtx, asyncWorkTimeout)
		defer cancel()
		work(ctx, log)
	}()

	respondSlack(w, ackWorkingOnIt)
}

// processCreate runs the asynchronous /qurl create work: resolve the
// per-workspace API key, call the qURL API with an idempotency key, and
// POST the result back via response_url.
//
// Idempotency-Key construction: sha256("slack:" + team_id + ":" +
// trigger_id) hex-encoded — 64 ASCII bytes, well under the 256-byte
// header cap, and reveals no PII on transit. Slack guarantees a unique
// trigger_id per click; matching keys come from Slack's own retry path
// (3s ack timeout exceeded), so collisions are exactly the dedup target.
func (h *Handler) processCreate(ctx context.Context, log *slog.Logger, values url.Values, targetURL string) {
	responseURL := values.Get(fieldResponseURL)
	teamID := values.Get(fieldTeamID)
	triggerID := values.Get(fieldTriggerID)

	c, err := h.authenticatedClient(ctx, teamID)
	if err != nil {
		log.Error("failed to get API key", "error", err)
		h.postResponse(log, responseURL, authErrorMessage(err))
		return
	}

	result, err := c.Create(ctx, client.CreateInput{
		TargetURL:      targetURL,
		IdempotencyKey: idempotencyKeyForCreate(teamID, triggerID),
	})
	if err != nil {
		log.Error("failed to create qURL", "error", err, "target_url", targetURL)
		h.postResponse(log, responseURL, sanitizeAPIError(err, "Failed to create qURL"))
		return
	}

	h.postResponse(log, responseURL, fmt.Sprintf("qURL created!\n*Link:* %s\n*Target:* %s", result.QURLLink, targetURL))
}

// processList runs the asynchronous /qurl list work: page through the
// most recent qURLs and POST a formatted summary back via response_url.
func (h *Handler) processList(ctx context.Context, log *slog.Logger, values url.Values) {
	responseURL := values.Get(fieldResponseURL)
	teamID := values.Get(fieldTeamID)

	c, err := h.authenticatedClient(ctx, teamID)
	if err != nil {
		log.Error("failed to get API key", "error", err)
		h.postResponse(log, responseURL, authErrorMessage(err))
		return
	}

	result, err := c.List(ctx, client.ListInput{Limit: 5})
	if err != nil {
		log.Error("failed to list qURLs", "error", err)
		h.postResponse(log, responseURL, sanitizeAPIError(err, "Failed to list qURLs"))
		return
	}

	h.postResponse(log, responseURL, formatListMessage(result.QURLs))
}

// idempotencyKeyForCreate hashes the workspace + trigger so the qURL
// service dedupes Slack-side retries.
//
// Inputs are length-framed before hashing so a future rev of Slack's
// ID format that introduced a colon couldn't collide distinct (team,
// trigger) pairs into the same key. Today's IDs (T-prefixed alphanum
// teams; UUID-shaped trigger IDs) don't contain colons, but pinning
// the contract via length-framing is cheap and removes the assumption.
func idempotencyKeyForCreate(teamID, triggerID string) string {
	pre := "slack:" + strconv.Itoa(len(teamID)) + ":" + teamID +
		":" + strconv.Itoa(len(triggerID)) + ":" + triggerID
	sum := sha256.Sum256([]byte(pre))
	return hex.EncodeToString(sum[:])
}

// sanitizeAPIError builds a user-safe message from a (possibly non-nil)
// qURL API error. The full *client.APIError is logged at the call site;
// what reaches Slack is bounded to:
//
//   - `prefix` (caller-supplied static text)
//   - `apiErr.Title` — qURL service or net/http standard text, stable
//     and not a leak surface
//   - `apiErr.RequestID` — operator handle for support correlation
//
// Notably absent: `apiErr.Detail`, which can carry server-side internals
// (DB error strings, internal hostnames, stack-trace fragments via the
// non-RFC-7807 fallback path in shared/client).
func sanitizeAPIError(err error, prefix string) string {
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) {
		return prefix + "."
	}
	msg := prefix
	if apiErr.Title != "" {
		// Trim a trailing period from Title so the appended one
		// below doesn't double-punctuate (some upstream servers
		// emit "Internal Server Error." — surfacing that verbatim
		// reads as "...Server Error..").
		msg = prefix + ": " + strings.TrimRight(apiErr.Title, ".")
	}
	if apiErr.RequestID != "" {
		msg += fmt.Sprintf(" (Reference: `%s`)", apiErr.RequestID)
	}
	return msg + "."
}

// formatListMessage renders /qurl list results as Slack mrkdwn.
func formatListMessage(qurls []client.QURL) string {
	if len(qurls) == 0 {
		return "No qURLs found."
	}
	lines := make([]string, 0, len(qurls))
	for i := range qurls {
		q := &qurls[i]
		line := fmt.Sprintf("• `%s` → %s [%s]", q.ResourceID, q.TargetURL, q.Status)
		if q.Description != "" {
			line = fmt.Sprintf("• *%s* — `%s` → %s [%s]", q.Description, q.ResourceID, q.TargetURL, q.Status)
		}
		lines = append(lines, line)
	}
	return "*Recent qURLs:*\n" + strings.Join(lines, "\n")
}

// postResponse POSTs an ephemeral follow-up to Slack's response_url.
// Errors are logged, never re-raised — the user will retry the command
// if the follow-up never arrives, which is preferable to retrying our
// own POST and risking a duplicate-message storm at Slack.
//
// Validates scheme+host before dialing so a malformed (or attacker-
// controlled, in the event of a signature-gate bypass) URL can't make
// the bot a generic SSRF emitter.
//
// Note: the worker's ctx is intentionally NOT used for the HTTP
// request. On SIGTERM the worker ctx is canceled, which has already
// failed the qURL call upstream — we still owe the user the failure
// follow-up. Deriving from context.Background() with a tight
// responseURLTimeout lets the follow-up land before Fargate's hard
// kill while still bounding the goroutine's lifetime.
// handler.Wait()/WaitTimeout in main blocks process exit.
func (h *Handler) postResponse(log *slog.Logger, responseURL, text string) {
	if responseURL == "" {
		log.Warn("missing response_url — async result has nowhere to go")
		return
	}
	target, err := h.validateResponseURLFn(responseURL)
	if err != nil {
		log.Warn("invalid response_url — refusing to dial", "error", err)
		return
	}

	body, err := json.Marshal(map[string]string{
		respFieldResponseType: respTypeEphemeral,
		respFieldText:         text,
	})
	if err != nil {
		// json.Marshal of a map[string]string can't fail in practice; log
		// and bail rather than POSTing a half-baked body.
		log.Error("marshal response_url payload failed", "error", err)
		return
	}

	deliverCtx, cancel := context.WithTimeout(context.Background(), responseURLTimeout)
	defer cancel()

	// Dial the URL the validator returned, NOT the original
	// responseURL string. The production validator constructs the
	// returned URL with Scheme and Host pinned to literal-string
	// constants, so the network destination is statically determined
	// and CodeQL's go/request-forgery taint analysis is satisfied
	// (validateResponseURLFn is the sanitizer; target is the safe
	// post-sanitizer value).
	req, err := http.NewRequestWithContext(deliverCtx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		log.Error("build response_url request failed", "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.responseURLClient.Do(req)
	if err != nil {
		log.Error("response_url POST failed", "error", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	// Read up to a small cap of the body so a 4xx from Slack
	// (`invalid_url`, `channel_not_found`, etc.) is visible in the
	// warn log rather than swallowed. If the LimitReader actually
	// hit the cap, drain the remainder so Go's transport can recycle
	// the connection — without that, keep-alive degrades silently if
	// Slack ever returns a body larger than the cap.
	const respBodyCap = 4096
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, respBodyCap))
	if len(respBody) == respBodyCap {
		_, _ = io.Copy(io.Discard, resp.Body)
	}
	if resp.StatusCode >= 400 {
		log.Warn("response_url returned non-2xx", "status", resp.StatusCode, "body", string(respBody))
	}
}

// validateResponseURL fences the response_url POST destination to
// hooks.slack.com over HTTPS. The Slack request signature already
// authenticates the form body, but a defense-in-depth host check means
// an attacker who finds a way past the signature gate (or a future
// regression there) still can't pivot the bot into an arbitrary-host
// SSRF emitter.
//
// On success, returns a *url.URL whose Scheme and Host are set to
// literal-string constants (NOT propagated from the parsed input).
// This is load-bearing for SSRF taint analysis: the dial target's
// network destination is statically determined, so CodeQL's
// go/request-forgery query treats this function as the sanitizer.
// The Path/RawQuery still flow from the input but those don't change
// the network destination.
func validateResponseURL(rawURL string) (*url.URL, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse response_url: %w", err)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("response_url scheme %q is not https", u.Scheme)
	}
	// Reject embedded userinfo (https://x:y@hooks.slack.com/...). Slack
	// will never send those; rejecting tightens the canonical-shape
	// contract and removes a footgun on the SSRF fence.
	if u.User != nil {
		return nil, errors.New("response_url contains userinfo")
	}
	// Hostname strips any port suffix; Slack doesn't send explicit ports
	// today, but treating ports as transparent matches user intent and
	// dodges a future regression where a port appears.
	// EqualFold is RFC 3986-correct: DNS hostnames are case-insensitive,
	// so a Hooks.Slack.Com URL that resolves identically must validate.
	if !strings.EqualFold(u.Hostname(), slackResponseURLHost) {
		return nil, fmt.Errorf("response_url host %q is not %s", u.Hostname(), slackResponseURLHost)
	}
	return &url.URL{
		Scheme:   "https",
		Host:     slackResponseURLHost,
		Path:     u.Path,
		RawPath:  u.RawPath,
		RawQuery: u.RawQuery,
	}, nil
}
