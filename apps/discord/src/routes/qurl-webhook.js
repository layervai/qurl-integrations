// qURL webhook receiver
//
// Inbound endpoint for qurl-service's `qurl.accessed` event. Writes the
// view count + consumed flag to the qurl_views DDB table; the live
// monitor's setInterval reads from that table (see commands.js
// monitorLinkStatus). DDB is the multi-instance bridge — a webhook
// can land on instance A while the originating monitor's setInterval
// runs on instance B, so we never reach for an in-process EventEmitter.
//
// Wire contract pinned by qurl-service:
//   header  `QURL-Signature`  bare hex HMAC-SHA256 over raw body (no
//                             `sha256=` prefix — different shape from
//                             GitHub's signature scheme; do not copy
//                             routes/webhooks.js' prefix-check verbatim)
//   body    `{event, data:{qurl_id, resource_id, access_count, consumed}}`
//
// `src_ip` and `user_agent` are stripped server-side for type=transit
// resources (the connector-owned privacy boundary). Discord bot sends
// are always transit, so we don't even attempt to read those fields.
//
// Secret-unset behavior: route still mounts, but every request is
// rejected with 503. A boot WARN surfaces the misconfig before traffic
// arrives — operator sees "webhook receiver mounted in unconfigured
// mode" in startup logs.

const express = require('express');
const crypto = require('crypto');
const config = require('../config');
const db = require('../store');
const logger = require('../logger');

const router = express.Router();

// Bare hex digest (no `sha256=` prefix). qurl-service sends raw hex —
// peer is `internal/domain/webhook.go::HeaderSignature` (qurl-service
// repo). Length is fixed at 64 lowercase hex chars; reject anything
// else before timingSafeEqual so a malformed header can't produce
// unequal-length buffers inside the try/catch.
function verifySignature(req) {
  const signature = req.headers['qurl-signature'];

  if (!config.QURL_WEBHOOK_SECRET) {
    // Logged at error level so oncall catches the misconfig immediately.
    // The boot-time WARN in server.js is the first signal; this is the
    // per-request follow-up if traffic arrives before the secret is set.
    logger.error('QURL_WEBHOOK_SECRET not configured — rejecting qURL webhook');
    return false;
  }

  if (!signature) {
    logger.warn('qURL webhook missing QURL-Signature header');
    return false;
  }
  if (typeof signature !== 'string' || !/^[0-9a-f]{64}$/.test(signature)) {
    logger.warn('qURL webhook signature has unexpected format', {
      length: typeof signature === 'string' ? signature.length : 0,
    });
    return false;
  }
  if (!req.rawBody) {
    logger.error('qURL webhook middleware did not populate rawBody — check server.js middleware ordering. Signature verification is BLOCKED until fixed.');
    return false;
  }

  const hmac = crypto.createHmac('sha256', config.QURL_WEBHOOK_SECRET);
  const digest = hmac.update(req.rawBody).digest('hex');

  try {
    return crypto.timingSafeEqual(Buffer.from(signature), Buffer.from(digest));
  } catch {
    return false;
  }
}

// Per-IP failed-sig counter — mirrors routes/webhooks.js shape so the
// two webhook surfaces have parallel hardening. SCALING: single-instance
// only; move to Redis when the bot runs horizontally.
const BAD_SIG_WINDOW_MS = 60_000;
const BAD_SIG_MAX = 30;
const BAD_SIG_PER_IP_CAP = BAD_SIG_MAX * 4;
const badSigAttempts = new Map();

const badSigSweep = setInterval(() => {
  const cutoff = Date.now() - BAD_SIG_WINDOW_MS * 2;
  for (const [ip, times] of badSigAttempts) {
    const recent = times.filter(t => t > cutoff);
    if (recent.length === 0) badSigAttempts.delete(ip);
    else badSigAttempts.set(ip, recent);
  }
}, 5 * 60 * 1000);
badSigSweep.unref();

function recordBadSig(ip) {
  const now = Date.now();
  let list = (badSigAttempts.get(ip) || []).filter(t => t > now - BAD_SIG_WINDOW_MS);
  list.push(now);
  if (list.length > BAD_SIG_PER_IP_CAP) list = list.slice(-BAD_SIG_PER_IP_CAP);
  if (badSigAttempts.size > 10_000) {
    const dropCount = Math.max(1, Math.floor(badSigAttempts.size / 10));
    const it = badSigAttempts.keys();
    for (let i = 0; i < dropCount; i++) {
      const k = it.next().value;
      if (k === undefined) break;
      badSigAttempts.delete(k);
    }
  }
  badSigAttempts.set(ip, list);
  return list.length;
}

router.post('/qurl', async (req, res) => {
  const ip = req.ip || 'unknown';
  const existing = (badSigAttempts.get(ip) || []).filter(t => t > Date.now() - BAD_SIG_WINDOW_MS);
  if (existing.length >= BAD_SIG_MAX) {
    logger.warn('qURL webhook rate limit exceeded (bad signatures)', { ip, recentFailures: existing.length });
    return res.status(429).json({ error: 'Too many invalid webhook attempts' });
  }

  // Secret-unset path returns 503 (not 401) — the discriminator matters
  // for oncall: 503 says "receiver is up but unconfigured"; 401 says
  // "real signature mismatch." Without it, an operator forgetting to
  // set the SSM param after first deploy would see the same status as
  // a genuine wire-shape regression.
  if (!config.QURL_WEBHOOK_SECRET) {
    return res.status(503).json({ error: 'Webhook receiver not configured' });
  }

  if (!verifySignature(req)) {
    const n = recordBadSig(ip);
    logger.warn('Invalid qURL webhook signature', { ip, totalInWindow: n });
    return res.status(401).json({ error: 'Invalid signature' });
  }

  const event = req.body?.event;
  const data = req.body?.data;
  // Replay protection key — peer is qurl-service's per-delivery event_id.
  // We also fall back to message_id (older payload shape) so a
  // qurl-service rev that hasn't standardized the field name yet still
  // gets dedup. If BOTH are absent the payload predates webhook
  // hardening; record under a synthesized key derived from the body so
  // we still get SOME replay protection (won't dedup across distinct
  // events, but a literal redelivery hashes to the same key).
  const eventId = req.body?.event_id || req.body?.message_id ||
    crypto.createHash('sha256').update(req.rawBody).digest('hex');

  // Only qurl.accessed drives the view counter today. Other event types
  // (e.g. qurl.created, qurl.revoked) may arrive if the subscription is
  // broader than necessary — silently 200 them so qurl-service doesn't
  // retry on the bot's account.
  if (event !== 'qurl.accessed') {
    logger.debug('qURL webhook event ignored', { event });
    return res.status(200).json({ status: 'ignored', event });
  }

  if (!data || typeof data.qurl_id !== 'string' || !data.qurl_id) {
    logger.warn('qURL webhook qurl.accessed missing qurl_id', { data });
    // 200 — the event was authentic, the payload was bad. Returning 4xx
    // here would make qurl-service retry, but no amount of retry will
    // fix a malformed payload. Log + drop.
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
    return res.status(200).json({ status: result });
  } catch (err) {
    // 5xx so qurl-service retries — transient DDB throttle should not
    // silently drop a view event.
    logger.error('Failed to record qURL view', { error: err.message, qurl_id: data.qurl_id });
    return res.status(500).json({ error: 'Internal error' });
  }
});

// Export sweep handle so server.js stopIntervals() can clear it on
// graceful shutdown — symmetric with the metricsSweepInterval pattern.
module.exports = router;
module.exports.stopIntervals = function stopIntervals() {
  clearInterval(badSigSweep);
};
