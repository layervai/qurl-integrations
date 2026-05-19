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
  if (!eventId) {
    logger.warn('qURL webhook missing body.id (per-event replay key)');
    return res.status(200).json({ status: 'invalid-payload' });
  }

  if (eventType !== QURL_WEBHOOK_EVENTS.ACCESSED) {
    logger.debug('qURL webhook event ignored', { type: eventType });
    return res.status(200).json({ status: 'ignored', type: eventType });
  }

  if (!data || typeof data.qurl_id !== 'string' || !data.qurl_id) {
    logger.warn('qURL webhook qurl.accessed missing qurl_id', { data });
    return res.status(200).json({ status: 'invalid-payload' });
  }

  const accessCount = Number(data.access_count);
  if (!Number.isFinite(accessCount) || accessCount < 0) {
    logger.warn('qURL webhook access_count not a non-negative number', { qurl_id: data.qurl_id, access_count: data.access_count });
    return res.status(200).json({ status: 'invalid-payload' });
  }

  try {
    const result = await db.recordQurlView({
      qurlId: data.qurl_id,
      accessCount,
      consumed: Boolean(data.consumed),
      eventId,
    });
    logger.info('qURL view recorded', { qurl_id: data.qurl_id, access_count: accessCount, consumed: Boolean(data.consumed), result });
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
