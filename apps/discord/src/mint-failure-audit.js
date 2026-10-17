const logger = require('./logger');
const { AUDIT_EVENTS } = require('./constants');

// classifyMintFailure maps a thrown error from the upload + mint phase
// of /qurl send into a small enum of reason strings. Used as the
// `reason` field on the QURL_SEND_CREATE_LINK_FAILURE audit event
// (qurl-integrations#276) so the CloudWatch metric filter can split
// failures by class — 4xx (likely client/contract problem) vs 5xx
// (likely upstream outage) vs timeout (likely network/cold-start).
//
// Don't add per-API-code branches here — that explodes the metric
// cardinality. New reasons should map to one of the existing
// categories or earn a new top-level category with a clear
// dimensional purpose (e.g. a future "rate_limit" if we add upstream
// 429 handling).
function classifyMintFailure(error) {
  if (!error) return 'unknown';
  // AbortError is ambiguous: undici/fetch raises it on deadline-fired
  // aborts AND on user-cancellations. error.cause is the only reliable
  // disambiguator — bucket as `timeout` only when cause corroborates a
  // timeout signal; otherwise fall through to `unknown` so a future
  // cancel-button adoption doesn't silently mis-bucket cancellations
  // as timeouts.
  if (error.name === 'AbortError') {
    const causeStr = String((error.cause && (error.cause.message || error.cause)) || '');
    // Word-anchored to drop strange concatenations like "rtimeout" or
    // "timeouted" — but it does NOT defend against "not due to timeout"
    // because "timeout" is still a complete word in that string. The
    // false-positive class for negation-containing causes is acceptable
    // because real timeout-driven aborts in undici raise TimeoutError
    // (handled by the name/code branch below) rather than AbortError; this
    // branch defends against libraries that use controller.abort('timeout')
    // as their deadline signal, where the cause is the deliberate marker.
    // This returns BEFORE the status check below — an abort's error.status
    // (if it somehow carried one) is not meaningful, so abort disambiguation
    // deliberately takes priority over status classification.
    if (/\btimed?\s*out\b/i.test(causeStr)) return 'timeout';
    return 'unknown';
  }
  // libuv socket codes + undici/fetch TimeoutError DOMException — all
  // unambiguous deadline-shaped failures. The message-regex branch was
  // removed: `/timeout/i.test(error.message)` over-matched
  // any string containing the substring "timeout" (e.g. "not a timeout
  // related error") and a future upstream message like "completed
  // after timeout retry" would mis-bucket. The name/code paths above
  // cover every real shape we've seen.
  if (error.code === 'ETIMEDOUT' || error.code === 'ECONNABORTED' ||
      error.name === 'TimeoutError') {
    return 'timeout';
  }
  // The connector answered 200 but its own upstream qURL create failed; name
  // that hop instead of bucketing on the upstream status. Gated on the local
  // redaction marker only assertUploadResult sets, never on apiCode alone:
  // other paths parse apiCode out of response bodies, so a 5xx body claiming
  // `qurl_creation_failed` must still bucket as upstream_5xx.
  if (error.apiDetailRedacted === true && error.apiCode === 'qurl_creation_failed') {
    return 'upstream_create_failed';
  }
  const status = error.status ?? 0;
  if (status >= 500 && status < 600) return 'upstream_5xx';
  if (status >= 400 && status < 500) return 'upstream_4xx';
  return 'unknown';
}

// emitMintFailureAudit centralizes the QURL_SEND_CREATE_LINK_FAILURE
// emission contract so the three catch sites (initial /qurl send +
// addRecipients file branch + addRecipients location branch) cannot
// drift on the skip rule or field shape. Adding a fourth call site
// goes through here too — owns both the contract and the cardinality
// discipline in one place. See #276.
function emitMintFailureAudit(error, { sendId, kind, synthetic = false }) {
  if (error && error.apiCode === 'quota_exceeded') return;
  logger.audit(AUDIT_EVENTS.QURL_SEND_CREATE_LINK_FAILURE, {
    send_id: sendId,
    reason: classifyMintFailure(error),
    // `?? null` (not `|| null`) preserves an empty-string `apiCode` —
    // `|| null` would collapse it to null and lose a forensic dimension.
    // (Status 0 doesn't apply: native fetch throws on transport errors
    // rather than returning a 0-status response.)
    api_code: error?.apiCode ?? null,
    status_code: error?.status ?? null,
    kind,
    ...(synthetic ? { synthetic: true } : {}),
  });
}

module.exports = { classifyMintFailure, emitMintFailureAudit };
