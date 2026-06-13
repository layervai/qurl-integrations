package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"time"

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
	fieldResponseURL  = "response_url"
	fieldTeamID       = "team_id"
	fieldTeamDomain   = "team_domain"
	fieldUserID       = "user_id"
	fieldUserName     = "user_name"
	fieldChannelID    = "channel_id"
	fieldChannelName  = "channel_name"
	fieldCommand      = "command"
	fieldText         = "text"
	fieldTriggerID    = "trigger_id"
	fieldEnterpriseID = "enterprise_id"
)

// slackResponseURLHost is Slack's webhook ingress for slash-command
// follow-ups. Pinning the host before dialing is defense-in-depth: a
// supply-chain compromise that flipped some Slack-supplied URL would
// otherwise let attackers turn this binary into an SSRF emitter.
// The host string is the only Slack-controlled identifier the Slack
// docs guarantee for this surface.
const slackResponseURLHost = "hooks.slack.com"

const responseURLRetryDelay = 250 * time.Millisecond

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
		"enterprise_id", values.Get(fieldEnterpriseID),
		"channel_id", values.Get(fieldChannelID),
		"trigger_id", values.Get(fieldTriggerID),
	)

	if !h.startAsyncWorker(log, work) {
		respondSlack(w, ackBusy)
		return
	}

	respondSlack(w, ackWorkingOnIt)
}

// startAsyncWorker runs async slash-command or interaction work through the
// same bounded pool, shutdown drain, timeout, and panic recovery. The caller
// owns the Slack ack shape because slash commands and modal submissions have
// different response contracts. Bounded by [asyncWorkTimeout] (slash-command
// budget); see [Handler.startAsyncWorkerWithTimeout] for callers that need more.
func (h *Handler) startAsyncWorker(log *slog.Logger, work func(ctx context.Context, log *slog.Logger)) bool {
	return h.startAsyncWorkerWithTimeout(log, asyncWorkTimeout, work)
}

// startAsyncWorkerWithTimeout is startAsyncWorker with a caller-chosen per-work
// deadline. A conversation-mode turn makes several Anthropic round-trips and so
// needs a larger budget than the slash-command default, which was sized for a
// couple of qURL API calls.
func (h *Handler) startAsyncWorkerWithTimeout(log *slog.Logger, timeout time.Duration, work func(ctx context.Context, log *slog.Logger)) bool {
	if h.runOnPool(h.sem, log, timeout, work) {
		return true
	}
	log.Warn("async pool saturated — dropping request")
	return false
}

func (h *Handler) startAsyncWorkerWithTail(log *slog.Logger, work func(ctx context.Context, log *slog.Logger) func()) bool {
	if h.runOnPoolWithTail(h.sem, log, asyncWorkTimeout, work) {
		return true
	}
	log.Warn("async pool saturated — dropping request")
	return false
}

// runOnPool acquires a non-blocking slot on sem and runs work in a wg-tracked,
// panic-recovered, timeout-bounded goroutine off h.baseCtx. It returns false WITHOUT
// running if sem is full, leaving the saturation log to the caller so each pool reports
// its own context. The shared turn pool (h.sem) goes through here; channel follow-ups use
// runAgentFollowupPipeline so their short gate slot can be released before the long turn.
func (h *Handler) runOnPool(sem chan struct{}, log *slog.Logger, timeout time.Duration, work func(ctx context.Context, log *slog.Logger)) bool {
	select {
	case sem <- struct{}{}:
	default:
		return false
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
		defer func() { <-sem }()
		defer func() {
			if rec := recover(); rec != nil {
				// A panicking goroutine in a long-lived process is
				// disqualifying — log + stack so the cause is in
				// CloudWatch, then swallow so the deferred sem release
				// and wg.Done still run.
				log.Error("panic in async worker", "recover", rec, "stack", string(debug.Stack()))
			}
		}()

		ctx, cancel := context.WithTimeout(h.baseCtx, timeout)
		defer cancel()
		work(ctx, log)
	}()
	return true
}

func (h *Handler) runOnPoolWithTail(sem chan struct{}, log *slog.Logger, timeout time.Duration, work func(ctx context.Context, log *slog.Logger) func()) bool {
	select {
	case sem <- struct{}{}:
	default:
		return false
	}

	h.wg.Add(1)
	go func() {
		defer h.wg.Done()

		ctx, cancel := context.WithTimeout(h.baseCtx, timeout)
		var tail func()
		func() {
			defer cancel()
			defer func() {
				if rec := recover(); rec != nil {
					log.Error("panic in async worker", "recover", rec, "stack", string(debug.Stack()))
				}
			}()
			tail = work(ctx, log)
		}()
		// Release the bounded worker slot before the tail runs, but keep the tail
		// in the same wg-tracked goroutine so shutdown waits for best-effort cleanup
		// such as audit writes. Tails intentionally run outside the semaphore: a
		// saturated pool can have cap(sem) bodies plus cap(sem) tails in flight, so
		// tails must stay cheap or separately bounded.
		<-sem

		if tail == nil {
			return
		}
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					log.Error("panic in async worker tail", "recover", rec, "stack", string(debug.Stack()))
				}
			}()
			tail()
		}()
	}()
	return true
}

// sanitizeAPIError builds a user-safe message from a (possibly non-nil)
// qURL API error. The full *client.APIError is logged at the call site;
// what reaches Slack is bounded to caller-owned static copy plus the
// opaque RequestID support handle. Upstream Title and Detail stay out of
// Slack because either can drift into service names, DB errors, or other
// operator-grade text.
func sanitizeAPIError(err error, prefix string) string {
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) {
		return prefix + "."
	}
	return appendSlackReference(prefix, apiErr.RequestID) + "."
}

func appendSlackReference(message, requestID string) string {
	if requestID == "" {
		return message
	}
	return fmt.Sprintf("%s (Reference: `%s`)", message, requestID)
}

func rateLimitMessage(retry time.Duration, requestID string) string {
	return appendSlackReference("Rate limit hit", requestID) + ". Try again in " + humanizeRetry(retry) + "."
}

func withRequestIDAttr(requestID string, attrs ...any) []any {
	if requestID == "" {
		return attrs
	}
	out := make([]any, 0, len(attrs)+2)
	out = append(out, "request_id", requestID)
	return append(out, attrs...)
}

// postResponse POSTs an ephemeral follow-up to Slack's response_url.
// Errors are logged, never retried. The bool tells sensitive callers whether
// they should add extra audit context after a failed delivery.
// Ordinary command handlers intentionally ignore the bool; callers that minted
// sensitive material should branch on false and log the relevant audit fields.
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
func (h *Handler) postResponse(log *slog.Logger, responseURL, text string) bool {
	body, err := responseURLTextBody(text)
	if err != nil {
		// json.Marshal of a map[string]string can't fail in practice; log
		// and bail rather than POSTing a half-baked body.
		log.Error("marshal response_url payload failed", "error", err)
		return false
	}
	return h.postResponseBody(log, responseURL, body)
}

// postResponseWithRetry is the opt-in shape for follow-ups where a false
// delivery negative changes security behavior. Ordinary responses stay
// single-attempt so non-idempotent Slack blips do not duplicate user-visible
// messages; sensitive callers such as tunnel install delivery retry once before
// treating delivery as unconfirmed and revoking freshly minted material.
func (h *Handler) postResponseWithRetry(log *slog.Logger, responseURL, text, operation string) bool {
	body, err := responseURLTextBody(text)
	if err != nil {
		log.Error("marshal response_url payload failed", "error", err)
		return false
	}
	return h.postResponseBodyWithRetry(log, responseURL, body, operation)
}

func responseURLTextBody(text string) ([]byte, error) {
	return json.Marshal(map[string]string{
		respFieldResponseType: respTypeEphemeral,
		respFieldText:         text,
	})
}

// postResponseBlocks POSTs an ephemeral Block Kit follow-up to Slack's
// response_url. fallbackText is the plain-text rendering Slack shows in
// notifications and to clients that can't render blocks — Slack treats a
// blocks message's `text` as the accessibility/notification fallback, so
// it MUST still carry the full listing. Same SSRF-fenced delivery,
// single-attempt-with-logging posture as [Handler.postResponse].
func (h *Handler) postResponseBlocks(log *slog.Logger, responseURL, fallbackText string, blocks []any) bool {
	body, err := json.Marshal(map[string]any{
		respFieldResponseType: respTypeEphemeral,
		respFieldText:         fallbackText,
		blockKitFieldBlocks:   blocks,
	})
	if err != nil {
		log.Error("marshal response_url blocks payload failed", "error", err)
		return false
	}
	return h.postResponseBody(log, responseURL, body)
}

func (h *Handler) postErrorResponse(log *slog.Logger, responseURL, message string, replaceOriginal bool) bool {
	body, err := ErrorResponse(message, replaceOriginal)
	if err != nil {
		log.Error("marshal response_url error payload failed", "error", err)
		return false
	}
	return h.postResponseBody(log, responseURL, body)
}

// replaceOriginalResponse swaps the slash-command's ephemeral "Working on it…"
// ack for a final message once a wizard modal has opened. Slack does NOT support
// delete_original for slash commands — it ignores the field and, because the POST
// then carries no text, rejects it with `no_text` (HTTP 500); an ephemeral ack
// also can't be deleted. So wizard flows REPLACE the ack rather than delete it.
// The always-present `text` field is what keeps the POST off the `no_text` path,
// and replace_original updates the spinner in place instead of stacking a second
// ephemeral. Same mechanism the error paths already use via ErrorResponse.
func (h *Handler) replaceOriginalResponse(log *slog.Logger, responseURL, message string) bool {
	body, err := json.Marshal(map[string]any{
		respFieldResponseType:    respTypeEphemeral,
		respFieldReplaceOriginal: true,
		respFieldText:            message,
	})
	if err != nil {
		log.Error("marshal response_url replace payload failed", "error", err)
		return false
	}
	if h.postResponseBody(log, responseURL, body) {
		return true
	}
	// Retry only this replace: a stale "Working on it" ack is uniquely
	// confusing after a modal opens, while ordinary async replies are safer as
	// single-attempt deliveries with explicit failure logging.
	return h.postResponseBodyRetryAfterFailure(log, responseURL, body, "replace_original")
}

func (h *Handler) postResponseBodyWithRetry(log *slog.Logger, responseURL string, body []byte, operation string) bool {
	result := h.postResponseBodyResult(log, responseURL, body)
	if result == responseURLDeliveryConfirmed {
		return true
	}
	if result == responseURLDeliveryPermanentFailure {
		return false
	}
	return h.postResponseBodyRetryAfterFailure(log, responseURL, body, operation)
}

func (h *Handler) postResponseBodyRetryAfterFailure(log *slog.Logger, responseURL string, body []byte, operation string) bool {
	log.Warn("response_url delivery failed; retrying once", "operation", operation)
	if !h.waitForResponseURLRetry() {
		log.Warn("response_url delivery retry skipped because handler is shutting down", "operation", operation)
		return false
	}
	return h.postResponseBodyResult(log, responseURL, body) == responseURLDeliveryConfirmed
}

func (h *Handler) waitForResponseURLRetry() bool {
	ctx := h.baseCtx
	if ctx == nil {
		ctx = context.Background()
	}
	timer := time.NewTimer(responseURLRetryDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (h *Handler) postResponseBody(log *slog.Logger, responseURL string, body []byte) bool {
	return h.postResponseBodyResult(log, responseURL, body) == responseURLDeliveryConfirmed
}

type responseURLDeliveryResult int

const (
	responseURLDeliveryConfirmed responseURLDeliveryResult = iota
	responseURLDeliveryRetryableFailure
	responseURLDeliveryPermanentFailure
)

func (h *Handler) postResponseBodyResult(log *slog.Logger, responseURL string, body []byte) responseURLDeliveryResult {
	if responseURL == "" {
		log.Warn("missing response_url — async result has nowhere to go")
		return responseURLDeliveryPermanentFailure
	}
	target, err := h.validateResponseURLFn(responseURL)
	if err != nil {
		log.Warn("invalid response_url — refusing to dial", "error", err)
		return responseURLDeliveryPermanentFailure
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
		return responseURLDeliveryPermanentFailure
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.responseURLClient.Do(req)
	if err != nil {
		log.Error("response_url POST failed", "error", err)
		return responseURLDeliveryRetryableFailure
	}
	defer func() { _ = resp.Body.Close() }()

	// Read up to a small cap of the body so a 4xx from Slack
	// (`invalid_url`, `channel_not_found`, etc.) is visible in the
	// warn log rather than swallowed. If the LimitReader actually
	// hit the cap, drain the remainder so Go's transport can recycle
	// the connection — without that, keep-alive degrades silently if
	// Slack ever returns a body larger than the cap.
	const respBodyCap = 4096
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, respBodyCap+1))
	if len(respBody) > respBodyCap {
		_, _ = io.Copy(io.Discard, resp.Body)
		// Keep the log body bounded at respBodyCap bytes; the extra byte was
		// read only to detect overflow.
		respBody = respBody[:respBodyCap]
	}
	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
		log.Warn("response_url returned retryable non-2xx", "status", resp.StatusCode, "body", string(respBody))
		return responseURLDeliveryRetryableFailure
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Warn("response_url returned non-2xx", "status", resp.StatusCode, "body", string(respBody))
		return responseURLDeliveryPermanentFailure
	}
	return responseURLDeliveryConfirmed
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
