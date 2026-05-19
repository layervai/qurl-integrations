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

const express = require('express');
const config = require('../config');
const db = require('../store');
const logger = require('../logger');
const { AUDIT_EVENTS, QURL_WEBHOOK_EVENTS } = require('../constants');
const { createBadSigLimiter, verifyHmacSha256 } = require('../utils/webhook-hardening');

const router = express.Router();

const SIGNATURE_HEADER = 'qurl-signature';
// Lowercase only by design: Node's crypto.digest('hex') and Go's
// hex.EncodeToString both emit lowercase. A future cross-language
// emitter defaulting to uppercase hex would silently 401 every
// webhook — make the regex flag the regression instead of the /i flag
// silently absorbing it.
const SIGNATURE_PATTERN = /^[0-9a-f]{64}$/;

const badSigLimiter = createBadSigLimiter({ max: 30, windowMs: 60_000 });

function verifySignature(req) {
  const signature = req.headers[SIGNATURE_HEADER];
  if (!signature) {
    logger.warn('qURL webhook missing QURL-Signature header');
    return false;
  }
  if (typeof signature !== 'string' || !SIGNATURE_PATTERN.test(signature)) {
    logger.warn('qURL webhook signature has unexpected format', {
      length: typeof signature === 'string' ? signature.length : 0,
    });
    return false;
  }
  if (!req.rawBody) {
    logger.error('qURL webhook middleware did not populate rawBody — check server.js middleware ordering. Signature verification is BLOCKED until fixed.');
    return false;
  }
  return verifyHmacSha256(req.rawBody, config.QURL_WEBHOOK_SECRET, signature);
}

router.post('/qurl', async (req, res) => {
  const ip = req.ip || 'unknown';
  if (badSigLimiter.shouldThrottle(ip)) {
    logger.warn('qURL webhook rate limit exceeded (bad signatures)', { ip });
    logger.audit(AUDIT_EVENTS.QURL_WEBHOOK_RATE_LIMITED, {});
    return res.status(429).json({ error: 'Too many invalid webhook attempts' });
  }

  // 503 vs 401: 503 says "receiver is up but unconfigured" (set
  // QURL_WEBHOOK_SECRET in SSM); 401 says "real signature mismatch."
  if (!config.QURL_WEBHOOK_SECRET) {
    return res.status(503).json({ error: 'Webhook receiver not configured' });
  }

  if (!verifySignature(req)) {
    const n = badSigLimiter.recordBadSig(ip);
    logger.warn('Invalid qURL webhook signature', { ip, totalInWindow: n });
    logger.audit(AUDIT_EVENTS.QURL_WEBHOOK_SIGNATURE_INVALID, { total_in_window: n });
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
  if (!Number.isSafeInteger(data.access_count) || data.access_count < 0) {
    logger.warn('qURL webhook access_count not a non-negative integer', { qurl_id: data.qurl_id, access_count: data.access_count });
    return res.status(200).json({ status: 'invalid-payload' });
  }
  const accessCount = data.access_count;

  // Strict equality — Boolean(data.consumed) would coerce the string
  // "false" to true. Pinning to the boolean catches a future emit-side
  // regression that JSON-encodes the field as a string.
  const consumed = data.consumed === true;

  try {
    const result = await db.recordQurlView({
      qurlId: data.qurl_id,
      accessCount,
      consumed,
      eventId,
    });
    logger.info('qURL view recorded', { qurl_id: data.qurl_id, access_count: accessCount, consumed, result });
    logger.audit(AUDIT_EVENTS.QURL_WEBHOOK_RECEIVED, { result });
    return res.status(200).json({ status: result });
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
module.exports = router;
