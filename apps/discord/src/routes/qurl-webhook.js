// qURL webhook receiver — POST /webhooks/qurl
//
// Wire contract (qurl-service/internal/domain/webhook.go::WebhookEvent +
// SignPayload):
//   header  `QURL-Signature`  bare hex HMAC-SHA256 (NO `sha256=` prefix —
//                             that's GitHub's shape)
//   body    {id, type, data:{qurl_id, resource_id, access_count, consumed},
//            owner_id, timestamp, api_version}
//
// `src_ip` and `user_agent` are stripped server-side for type=transit
// resources (connector-owned privacy boundary). Discord bot sends are
// always transit.
//
// Multi-secret routing (BYOK view counter): inbound events carry the
// qurl-service auth0 `owner_id` in the body envelope. The bot looks
// the secret up by owner_id in the in-process subscription registry
// BEFORE HMAC verification. That's safe because the lookup itself
// grants no trust — HMAC verification still gates every accepted
// event. Industry-standard pattern (Stripe, GitHub, Linear all do
// this); the only alternative is making qurl-service include a
// QURL-Webhook-Id header, which is a 2-repo coordination we don't
// need for a problem the bot can solve itself.

const express = require('express');
const db = require('../store');
const logger = require('../logger');
const { AUDIT_EVENTS, QURL_WEBHOOK_EVENTS } = require('../constants');
const { createBadSigLimiter, verifyHmacSha256 } = require('../utils/webhook-hardening');
const subs = require('../webhook-subscriptions');
const viewUpdatePublisher = require('../view-update-publisher');

const router = express.Router();

const SIGNATURE_HEADER = 'qurl-signature';
// Lowercase only by design: Node's crypto.digest('hex') and Go's
// hex.EncodeToString both emit lowercase. A future cross-language
// emitter defaulting to uppercase hex would silently 401 every
// webhook — make the regex flag the regression instead of the /i flag
// silently absorbing it.
const SIGNATURE_PATTERN = /^[0-9a-f]{64}$/;

const badSigLimiter = createBadSigLimiter({ max: 30, windowMs: 60_000 });

// Reason codes returned from verifyAndResolve. Caller maps them to
// HTTP status + audit event + rate-limiter interaction in one place
// so the policy table stays grep-able.
const VERIFY_RESULTS = {
  OK: 'ok',
  RAW_BODY_MISSING: 'raw_body_missing',         // middleware bug; should never happen
  SIG_HEADER_MISSING: 'sig_header_missing',
  SIG_HEADER_MALFORMED: 'sig_header_malformed',
  OWNER_ID_MISSING: 'owner_id_missing',         // parse failed OR field absent
  CACHE_UNPRIMED: 'cache_unprimed',             // 503 — registry hasn't completed first scan
  OWNER_UNKNOWN: 'owner_unknown',               // 401 — registry primed, owner not registered
  SIG_INVALID: 'sig_invalid',                   // HMAC mismatch on real secret
};

function verifyAndResolve(req) {
  if (!req.rawBody) {
    logger.error('qURL webhook middleware did not populate rawBody — check server.js middleware ordering. Signature verification is BLOCKED until fixed.');
    return { result: VERIFY_RESULTS.RAW_BODY_MISSING };
  }

  const signature = req.headers[SIGNATURE_HEADER];
  if (!signature) {
    logger.warn('qURL webhook missing QURL-Signature header');
    return { result: VERIFY_RESULTS.SIG_HEADER_MISSING };
  }
  if (typeof signature !== 'string' || !SIGNATURE_PATTERN.test(signature)) {
    logger.warn('qURL webhook signature has unexpected format', {
      length: typeof signature === 'string' ? signature.length : 0,
    });
    return { result: VERIFY_RESULTS.SIG_HEADER_MALFORMED };
  }

  // Pre-HMAC owner_id read. req.body is already parsed by the
  // rawBodyJson middleware in server.js (1mb cap on rawBody bounds
  // any JSON.parse risk). The parser also stashed req.rawBody, which
  // is what we HMAC. We treat req.body.owner_id as untrusted routing
  // input — lookup grants no trust, HMAC is what grants trust.
  // A malformed body never reaches us (express.json middleware would
  // have already 400'd), but a body that parsed-successfully but
  // lacks owner_id is possible and must produce 401, not 5xx.
  const ownerId = req.body && typeof req.body.owner_id === 'string' ? req.body.owner_id : null;
  if (!ownerId) {
    logger.warn('qURL webhook missing or non-string body.owner_id');
    return { result: VERIFY_RESULTS.OWNER_ID_MISSING };
  }

  const secret = subs.getSecretForOwner(ownerId);
  if (!secret) {
    if (!subs.isPrimed()) return { result: VERIFY_RESULTS.CACHE_UNPRIMED, ownerId };
    return { result: VERIFY_RESULTS.OWNER_UNKNOWN, ownerId };
  }

  if (!verifyHmacSha256(req.rawBody, secret, signature)) {
    return { result: VERIFY_RESULTS.SIG_INVALID, ownerId };
  }
  return { result: VERIFY_RESULTS.OK, ownerId };
}

router.post('/qurl', async (req, res) => {
  const ip = req.ip || 'unknown';
  if (badSigLimiter.shouldThrottle(ip)) {
    logger.warn('qURL webhook rate limit exceeded (bad signatures)', { ip });
    logger.audit(AUDIT_EVENTS.QURL_WEBHOOK_RATE_LIMITED, {});
    return res.status(429).json({ error: 'Too many invalid webhook attempts' });
  }

  const { result, ownerId } = verifyAndResolve(req);

  // 503 is the only retriable failure mode — qurl-service retries 503
  // (1+2+4+8+16=31s backoff, 5 attempts) but NOT 401. The cache-
  // unprimed case happens at cold start and during sibling-replica
  // lag after a peer's setGuildApiKey; treating it as 401 would
  // silently drop a guild's first views post-link.
  if (result === VERIFY_RESULTS.CACHE_UNPRIMED) {
    logger.audit(AUDIT_EVENTS.QURL_WEBHOOK_CACHE_MISS_UNPRIMED, {});
    return res.status(503).json({ error: 'Webhook receiver warming up' });
  }
  if (result === VERIFY_RESULTS.RAW_BODY_MISSING) {
    // Middleware bug — 503 so qurl-service retries while we investigate.
    return res.status(503).json({ error: 'Webhook receiver misconfigured' });
  }

  // Every other failure shape is 401 + rate-limiter increment + audit.
  // OWNER_UNKNOWN-after-primed gets a distinct audit so a real auth0
  // owner probing the receiver shows up on a different dashboard line
  // than HMAC-mismatch traffic (different threat models).
  if (result !== VERIFY_RESULTS.OK) {
    const n = badSigLimiter.recordBadSig(ip);
    const auditEvent = result === VERIFY_RESULTS.OWNER_UNKNOWN
      ? AUDIT_EVENTS.QURL_WEBHOOK_CACHE_MISS_UNKNOWN_OWNER
      : AUDIT_EVENTS.QURL_WEBHOOK_SIGNATURE_INVALID;
    logger.warn('qURL webhook verification failed', { ip, totalInWindow: n, result, ownerId });
    logger.audit(auditEvent, { total_in_window: n, result });
    return res.status(401).json({ error: 'Invalid signature' });
  }

  const eventType = req.body?.type;
  const data = req.body?.data;
  const eventId = req.body?.id;
  // typeof guard (not just falsy): a payload with `id: { weird: true }`
  // would otherwise pass through DDB's UpdateExpression and persist a
  // non-scalar replay key — silent corruption of dedup semantics.
  if (typeof eventId !== 'string' || !eventId) {
    logger.warn('qURL webhook missing or non-string body.id (per-event replay key)');
    return res.status(200).json({ status: 'invalid-payload' });
  }

  // TODO(freshness): the signature pins payload integrity but not
  // freshness. A captured event with a never-seen `id` and a
  // sufficiently-advanced access_count would still be accepted long
  // after capture. qurl-service includes a signed `timestamp` field;
  // adding a `now() - body.timestamp > N` rejection would close the
  // replay window. Out of scope for this PR (qurl-service is the only
  // valid emitter today).

  if (eventType !== QURL_WEBHOOK_EVENTS.ACCESSED) {
    logger.debug('qURL webhook event ignored', { type: eventType });
    return res.status(200).json({ status: 'ignored', type: eventType });
  }

  if (!data || typeof data.qurl_id !== 'string' || !data.qurl_id) {
    // Limit logged fields — `data` is post-trust but unbounded in shape
    // and may include attacker-influenced src_ip/user_agent for non-
    // transit resources. Log only the keys the pipeline actually reads.
    logger.warn('qURL webhook qurl.accessed missing qurl_id', { qurl_id: data?.qurl_id, resource_id: data?.resource_id });
    return res.status(200).json({ status: 'invalid-payload' });
  }

  // Strict integer gate — qurl-service emits access_count as Go int64
  // (`WebhookEvent.Data['access_count']`), so a non-integer or a value
  // past 2^53 here would be a wire-shape regression. isSafeInteger
  // (NOT just isFinite + >= 0) catches the float case + the unsafe-
  // int case in one check. Also blocks Number(null)===0 from slipping
  // through, which would otherwise cost a DDB write per such event.
  // Strict positive integer — qurl.accessed events always carry
  // access_count >= 1 by contract. Reject 0 here (vs accepting 0 +
  // writing to DDB + publisher dropping at SQS layer) to avoid the
  // asymmetric log pair where the webhook returns "recorded" and the
  // view-update-publisher then warns "invalid accessCount" on the
  // same event. One source of truth at the wire boundary.
  if (!Number.isSafeInteger(data.access_count) || data.access_count <= 0) {
    logger.warn('qURL webhook access_count not a positive integer', { qurl_id: data.qurl_id, access_count: data.access_count });
    return res.status(200).json({ status: 'invalid-payload' });
  }
  const accessCount = data.access_count;

  // Strict equality — Boolean(data.consumed) would coerce the string
  // "false" to true. Pinning to the boolean catches a future emit-side
  // regression that JSON-encodes the field as a string.
  const consumed = data.consumed === true;

  try {
    const dbResult = await db.recordQurlView({
      qurlId: data.qurl_id,
      accessCount,
      consumed,
      eventId,
    });
    logger.info('qURL view recorded', { qurl_id: data.qurl_id, access_count: accessCount, consumed, result: dbResult });
    logger.audit(AUDIT_EVENTS.QURL_WEBHOOK_RECEIVED, { result: dbResult });
    // Sub-second view counter (feat #60): publish to SQS only on
    // result === 'recorded' (a real new view, not a per-event dedup
    // replay). Fire-and-log — the polling fallback in
    // monitorLinkStatus catches anything the publisher drops. No
    // await: the HTTP response must come back to qurl-service
    // promptly so it doesn't retry on its own.
    if (dbResult === 'recorded') {
      viewUpdatePublisher.publish({
        qurlId: data.qurl_id,
        accessCount,
        eventId,
      });
    }
    return res.status(200).json({ status: dbResult });
  } catch (err) {
    // 5xx so qurl-service retries — transient DDB throttle should not
    // silently drop a view event.
    logger.error('Failed to record qURL view', { error: err.message, qurl_id: data.qurl_id });
    logger.audit(AUDIT_EVENTS.QURL_WEBHOOK_STORE_ERROR, { error_type: err.name || 'unknown' });
    return res.status(500).json({ error: 'Internal error' });
  }
});

router.stopIntervals = function stopIntervals() {
  badSigLimiter.stopSweep();
};
// Exposed for tests that want to assert against the policy table
// without rebuilding the conditional ladder. NOT part of any external
// contract.
router._VERIFY_RESULTS = VERIFY_RESULTS;
module.exports = router;
