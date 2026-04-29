// Canary exec endpoint — exercised by the canary runner ECS service to
// verify the bot's *back-end* health: connector reachability, qURL API
// auth, mint-link round-trip latency. Discord Gateway readiness is
// covered by the existing /health probe + LB target-group; the front-
// half interactive state machine is NOT exercised here (it requires a
// real Discord client to drive component clicks, which the HTTP-only
// canary deliberately can't do).
//
// Auth is HMAC-SHA256 over `<unix_seconds>.<raw_body>` with the
// CANARY_SHARED_SECRET (SSM SecureString, mirrored on the runner).
// 5-minute timestamp window guards against replay. The endpoint is
// disabled (503) when the secret isn't configured — both for local
// dev safety and for the bot's first deploy before the runner exists.
//
// The exec mints exactly ONE token against a synthetic location
// resource. Side effects: a single ephemeral connector resource +
// one mint API call per probe. No DM, no DB write, no channel post —
// the canary is a back-end health probe, not a synthetic /qurl send.

const express = require('express');
const crypto = require('crypto');
const config = require('../config');
const logger = require('../logger');
const { uploadJsonToConnector, mintLinks } = require('../connector');

const router = express.Router();

// 5-minute replay window. Tighter than the OAuth/webhook windows (which
// are 5 min too) because the runner-to-bot path is synchronous and
// shouldn't see clock skew above a few seconds.
const TIMESTAMP_TOLERANCE_SECONDS = 300;

function verifyCanarySignature(req, res, next) {
  const secret = config.CANARY_SHARED_SECRET;
  if (!secret) {
    // Don't leak that the secret is unset to unauthenticated callers
    // beyond the 503 itself — log it once at boot, not per request.
    return res.status(503).json({ ok: false, error: 'canary_disabled' });
  }

  const sig = req.header('X-Canary-Signature');
  const ts = req.header('X-Canary-Timestamp');
  if (!sig || !ts) {
    return res.status(401).json({ ok: false, error: 'missing_signature' });
  }

  const tsInt = parseInt(ts, 10);
  if (!Number.isFinite(tsInt)) {
    return res.status(401).json({ ok: false, error: 'bad_timestamp' });
  }
  const drift = Math.abs(Math.floor(Date.now() / 1000) - tsInt);
  if (drift > TIMESTAMP_TOLERANCE_SECONDS) {
    return res.status(401).json({ ok: false, error: 'expired_timestamp' });
  }

  // verify() middleware (mounted in server.js) populated req.rawBody as
  // a Buffer. If it's missing, the route was misconfigured — refuse
  // rather than computing a signature over `undefined`.
  if (!Buffer.isBuffer(req.rawBody)) {
    logger.error('Canary route: req.rawBody missing — verify middleware not registered for this path');
    return res.status(500).json({ ok: false, error: 'server_misconfigured' });
  }

  const expected = crypto.createHmac('sha256', secret)
    .update(`${tsInt}.${req.rawBody.toString('utf8')}`)
    .digest('hex');

  let valid = false;
  try {
    const sigBuf = Buffer.from(sig, 'hex');
    const expBuf = Buffer.from(expected, 'hex');
    if (sigBuf.length === expBuf.length) {
      valid = crypto.timingSafeEqual(sigBuf, expBuf);
    }
  } catch {
    valid = false;
  }

  if (!valid) {
    return res.status(401).json({ ok: false, error: 'bad_signature' });
  }

  next();
}

router.post('/exec', verifyCanarySignature, async (req, res) => {
  const startedAt = Date.now();
  const apiKey = config.QURL_API_KEY;
  if (!apiKey) {
    // Multi-tenant deployments don't set a global QURL_API_KEY — the
    // canary is meaningful only on single-tenant prod where the bot
    // has its own key. Fail closed.
    return res.status(503).json({ ok: false, error: 'no_api_key', latency_ms: Date.now() - startedAt });
  }

  // Synthetic resource: a tiny location payload. Picked location
  // (not file) because it doesn't touch the Discord CDN download
  // path — the canary is testing the bot↔qURL hop, and the file
  // path's downloadAndUpload hits cdn.discordapp.com which is its
  // own dependency cone. Location goes straight from the bot to
  // the connector via uploadJsonToConnector, isolating the failure
  // surface to the qURL stack.
  const probePayload = {
    type: 'google-map',
    url: 'https://maps.app.goo.gl/canary-probe',
    name: 'canary',
  };

  try {
    const upload = await uploadJsonToConnector(probePayload, 'canary.json', apiKey);
    if (!upload?.resource_id) {
      return res.status(500).json({
        ok: false, error: 'upload_no_resource_id',
        latency_ms: Date.now() - startedAt,
      });
    }

    // 60-second TTL on the canary link. Short window keeps the
    // qURL backend's bookkeeping costs negligible even with the
    // canary running 5x/min.
    const expiresAt = new Date(Date.now() + 60_000).toISOString();
    const minted = await mintLinks(upload.resource_id, expiresAt, 1, apiKey);

    const link = minted?.[0]?.qurl_link;
    if (!link) {
      return res.status(500).json({
        ok: false, error: 'no_link_in_mint_response',
        latency_ms: Date.now() - startedAt,
      });
    }

    let linkHost;
    try { linkHost = new URL(link).host; } catch { linkHost = 'invalid-url'; }

    return res.json({
      ok: true,
      latency_ms: Date.now() - startedAt,
      // Echo the host but not the full link — the link itself is a
      // single-use credential, no point ever logging it. Host is
      // useful for verifying we're hitting the right qURL pool.
      link_host: linkHost,
      resource_id: upload.resource_id,
    });
  } catch (err) {
    // logger.warn (not error) — canary failures are expected during
    // outages and shouldn't pollute the error log on top of whatever
    // is already firing.
    logger.warn('Canary exec failed', {
      error: err?.message,
      apiCode: err?.apiCode,
      latency_ms: Date.now() - startedAt,
    });
    return res.status(500).json({
      ok: false,
      error: 'exec_failed',
      reason: err?.message,
      apiCode: err?.apiCode,
      latency_ms: Date.now() - startedAt,
    });
  }
});

module.exports = router;
