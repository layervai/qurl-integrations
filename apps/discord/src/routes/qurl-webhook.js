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

// Replace control chars (0x00-0x1f + DEL) with '?' so attacker-
// controlled strings can't spoof field separators or inject newlines
// in line-oriented log shippers. The control-regex IS the point here.
// eslint-disable-next-line no-control-regex
const CONTROL_CHARS = /[\x00-\x1f\x7f]/g;
function sanitizeForLog(s) {
  return s.replace(CONTROL_CHARS, '?');
}

const badSigLimiter = createBadSigLimiter({ max: 30, windowMs: 60_000 });
// Looser limiter for OWNER_UNKNOWN — operational drift is the
// expected failure mode (not an attacker brute-forcing HMAC), but
// without ANY ceiling an attacker could flood the receiver with
// signed-shaped requests carrying bogus owner_id and never trip a
// rate limit. Threshold is 5× the HMAC limiter to avoid throttling
// during a real registry-rebuild incident.
//
// CROSS-TENANT FAIRNESS TRADE-OFF: keyed on IP alone. qurl-service
// egresses from a small NAT/proxy pool so a single guild whose
// webhook_owner_id has drifted vs. qurl-service's signing identity
// can burn 150/min on the shared source IP and 429 other guilds'
// legitimate events. Acceptable at low BYOK count today (≤10 in
// prod); at scale this should key on (ip, owner_id) or skip per-IP
// limiting for OWNER_UNKNOWN entirely.
const unknownOwnerLimiter = createBadSigLimiter({ max: 150, windowMs: 60_000 });

// Reason codes returned from verifyAndResolve. Caller maps them to
// HTTP status + audit event + rate-limiter interaction in one place
// so the policy table stays grep-able. Frozen for parity with
// LINK_RESULTS — typo in a string value would silently break the
// downstream switch.
const VERIFY_RESULTS = Object.freeze({
  OK: 'ok',
  RAW_BODY_MISSING: 'raw_body_missing',         // middleware bug; should never happen
  SIG_HEADER_MISSING: 'sig_header_missing',
  SIG_HEADER_MALFORMED: 'sig_header_malformed',
  OWNER_ID_MISSING: 'owner_id_missing',         // parse failed OR field absent
  CACHE_UNPRIMED: 'cache_unprimed',             // 503 — registry hasn't completed first scan
  OWNER_UNKNOWN: 'owner_unknown',               // 401 — registry primed, owner not registered
  SIG_INVALID: 'sig_invalid',                   // HMAC mismatch on real secret
});

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

  // SECURITY: pre-HMAC parse depth/size MUST stay bounded by the
  // rawBodyJson middleware cap (server.js, `limit: '1mb'`). Loosening
  // that cap or adding extended type coercion here widens the
  // pre-trust window — req.body is attacker-controlled JSON until
  // the HMAC check below succeeds.
  // The lookup itself grants no trust: a forged owner_id either
  // misses the cache (→ 401) or hits a real secret the attacker
  // can't forge against (→ 401).
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
  // OR-coupling note: an IP that already burned the HMAC limiter
  // (30/min) will get 429'd here even if its current request would
  // have been OWNER_UNKNOWN (separately budgeted at 150/min). That's
  // acceptable — the IP is already flagged as suspicious — so the
  // looser unknown-owner threshold protects legitimate operational
  // drift from OTHER IPs, not from the same already-bad-actor IP.
  if (badSigLimiter.shouldThrottle(ip) || unknownOwnerLimiter.shouldThrottle(ip)) {
    logger.warn('qURL webhook rate limit exceeded', { ip });
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
    // Intentionally NOT counted toward unknownOwnerLimiter — the
    // unprimed window is one-shot at boot (primed never flips back to
    // false), so attacker exploitation here is bounded to the
    // cold-start window. qurl-service retries 503 so the event isn't
    // lost.
    logger.audit(AUDIT_EVENTS.QURL_WEBHOOK_CACHE_MISS_UNPRIMED, {});
    return res.status(503).json({ error: 'Webhook receiver warming up' });
  }
  if (result === VERIFY_RESULTS.RAW_BODY_MISSING) {
    // Middleware bug — 503 so qurl-service retries while we investigate.
    return res.status(503).json({ error: 'Webhook receiver misconfigured' });
  }

  // Two limiters, two threat models:
  //   - HMAC failures count toward badSigLimiter (attacker brute-
  //     forcing the secret).
  //   - OWNER_UNKNOWN / OWNER_ID_MISSING count toward unknownOwnerLimiter
  //     (operational drift OR attacker probing with garbage owner_id;
  //     looser threshold so a real registry-rebuild burst doesn't
  //     throttle legitimate traffic on the same source IP).
  if (result !== VERIFY_RESULTS.OK) {
    const isHmacFailure = result === VERIFY_RESULTS.SIG_INVALID
      || result === VERIFY_RESULTS.SIG_HEADER_MISSING
      || result === VERIFY_RESULTS.SIG_HEADER_MALFORMED;
    const isUnknownOwner = result === VERIFY_RESULTS.OWNER_UNKNOWN
      || result === VERIFY_RESULTS.OWNER_ID_MISSING;
    let totalInWindow = null;
    if (isHmacFailure) totalInWindow = badSigLimiter.recordBadSig(ip);
    else if (isUnknownOwner) totalInWindow = unknownOwnerLimiter.recordBadSig(ip);
    // Audit-event split mirrors the threat-model split: OWNER_* are
    // operational/payload-shape signal (route both to the same
    // unknown-owner dashboard line); SIG_* are HMAC-failure signal.
    const auditEvent = isUnknownOwner
      ? AUDIT_EVENTS.QURL_WEBHOOK_CACHE_MISS_UNKNOWN_OWNER
      : AUDIT_EVENTS.QURL_WEBHOOK_SIGNATURE_INVALID;
    // Truncate + strip non-printables on the attacker-controlled
    // ownerId before logging. Mega-string blows up log volume; control
    // chars (\n, \r, escape sequences) confuse line-oriented log
    // parsers and can spoof field separators in some shippers.
    // 64 chars is well over an auth0 sub.
    const safeOwnerId = typeof ownerId === 'string'
      ? sanitizeForLog(ownerId.slice(0, 64))
      : ownerId;
    logger.warn('qURL webhook verification failed', { ip, totalInWindow, result, ownerId: safeOwnerId });
    logger.audit(auditEvent, { total_in_window: totalInWindow, result });
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
  unknownOwnerLimiter.stopSweep();
};
module.exports = router;
