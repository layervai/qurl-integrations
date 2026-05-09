// Canary exec endpoint — exercised by the canary runner Lambda
// (qurl-integrations-infra/qurl-bot-discord/terraform/canary-exec.tf)
// to verify the bot's qURL send pipeline end-to-end every 3 hours.
//
// Auth is HMAC-SHA256 over `<unix_seconds>.<raw_body>` with the
// CANARY_SHARED_SECRET (SSM SecureString, mirrored on the Lambda).
// 5-minute timestamp window guards against replay. The endpoint is
// disabled (503) when the secret isn't configured — both for local
// dev safety and for the bot's first deploy before the canary infra
// exists.
//
// Body shape (all fields optional for back-compat):
//   { test: "send_file" | "send_location", recipient_user_id: "<snowflake>" }
//
// When `test` + `recipient_user_id` are both present, the canary:
//   1. Builds a synthetic resource shaped like the named test
//      (small text buffer for send_file, location JSON for send_location).
//   2. Uploads to the connector (reUploadBuffer / uploadJsonToConnector).
//   3. Mints a single short-TTL qURL via mintLinks.
//   4. Sends a clearly-labeled "[Canary probe]" DM to the recipient.
//
// Empty body falls through to the legacy back-end-only probe shape
// (synthetic location upload + mint, no DM). The legacy path stays
// because the EventBridge → Lambda canary on infra side may run
// before the bot extension has shipped — undifferentiated test runs
// are better than 503s during the rollout window.
//
// Response includes per-step status so the Lambda's per-test pass/fail
// metric can attribute failures to the correct subsystem:
//   { ok, step: "upload"|"mint"|"dm"|null, latency_ms, dm_status, ... }

const express = require('express');
const crypto = require('crypto');
const { EmbedBuilder } = require('discord.js');
const config = require('../config');
const logger = require('../logger');
const { reUploadBuffer, uploadJsonToConnector, mintLinks } = require('../connector');
const { sendDM } = require('../discord');
const { COLORS } = require('../constants');

const router = express.Router();

// 5-minute replay window. Tighter than the OAuth/webhook windows (which
// are 5 min too) because the runner-to-bot path is synchronous and
// shouldn't see clock skew above a few seconds.
const TIMESTAMP_TOLERANCE_SECONDS = 300;

// Allowed `test` values. The Lambda iterates `SCENARIOS_JSON`; this
// set is the bot-side guardrail so a typo'd scenario in the Lambda
// env returns a clear 400 instead of being silently mistreated as
// the legacy back-end-only path.
const VALID_TESTS = new Set(['send_file', 'send_location']);

// Discord snowflake validation: 17-20 digits. Same shape the
// terraform `canary_recipient_user_ids` validation uses.
const SNOWFLAKE_RE = /^[0-9]{17,20}$/;

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

// Build a clearly-labeled canary DM payload. Intentionally NOT using
// `buildDeliveryPayload` from commands.js — that helper is gated
// behind `_test` (non-production only). The canary's purpose is to
// exercise client.users.fetch → user.send; embed shape doesn't need
// to match `/qurl send` exactly. Recipients know this is a probe.
function buildCanaryDmPayload({ test, qurlLink, resourceId, expiresAt }) {
  const expiresUnix = Math.floor(new Date(expiresAt).getTime() / 1000);
  const embed = new EmbedBuilder()
    .setColor(COLORS.QURL_BRAND)
    .setTitle(`[Canary probe] ${test}`)
    .setDescription(`Synthetic test from the canary-exec probe — confirms the qURL pipeline (connector upload → mint → DM) is healthy.\n\n[qURL link](${qurlLink}) (resource_id: \`${resourceId}\`, expires <t:${expiresUnix}:R>)`)
    .setTimestamp();
  return { embeds: [embed] };
}

// Run a single canary scenario end-to-end. Returns { ok, step, ... }
// where `step` names the failure point (upload | mint | dm) so the
// Lambda's metric attribution is specific.
async function runScenario({ test, recipientUserId, apiKey }) {
  const startedAt = Date.now();

  // Synthetic resource — shape differs by test so each scenario
  // exercises a slightly different connector code path. send_file
  // uses reUploadBuffer (raw bytes via multipart); send_location
  // uses uploadJsonToConnector (JSON body, location-specific path).
  let upload;
  try {
    if (test === 'send_file') {
      const fileBuffer = Buffer.from(`canary probe @ ${new Date().toISOString()}\n`, 'utf8');
      upload = await reUploadBuffer(fileBuffer, 'canary-probe.txt', 'text/plain', apiKey);
    } else {
      // send_location
      const probePayload = {
        type: 'google-map',
        url: 'https://maps.app.goo.gl/canary-probe',
        name: 'canary',
      };
      upload = await uploadJsonToConnector(probePayload, 'canary-location.json', apiKey);
    }
  } catch (err) {
    return {
      ok: false, step: 'upload', error: 'upload_threw',
      reason: err?.message, apiCode: err?.apiCode,
      latency_ms: Date.now() - startedAt,
    };
  }
  if (!upload?.resource_id) {
    return {
      ok: false, step: 'upload', error: 'upload_no_resource_id',
      latency_ms: Date.now() - startedAt,
    };
  }

  // 60-second TTL on canary links — short window keeps the qURL
  // backend's bookkeeping costs negligible.
  const expiresAt = new Date(Date.now() + 60_000).toISOString();
  let minted;
  try {
    minted = await mintLinks(upload.resource_id, expiresAt, 1, apiKey);
  } catch (err) {
    return {
      ok: false, step: 'mint', error: 'mint_threw',
      reason: err?.message, apiCode: err?.apiCode,
      latency_ms: Date.now() - startedAt,
      resource_id: upload.resource_id,
    };
  }
  const link = minted?.[0]?.qurl_link;
  if (!link) {
    return {
      ok: false, step: 'mint', error: 'no_link_in_mint_response',
      latency_ms: Date.now() - startedAt,
      resource_id: upload.resource_id,
    };
  }

  // DM the recipient. sendDM swallows + logs internally and returns
  // boolean — propagate that so the Lambda metric attributes a DM
  // delivery regression to the dm step specifically.
  const dmOk = await sendDM(recipientUserId, buildCanaryDmPayload({
    test, qurlLink: link, resourceId: upload.resource_id, expiresAt,
  }));

  let linkHost;
  try { linkHost = new URL(link).host; } catch { linkHost = 'invalid-url'; }

  if (!dmOk) {
    return {
      ok: false, step: 'dm', error: 'dm_failed',
      latency_ms: Date.now() - startedAt,
      resource_id: upload.resource_id,
      link_host: linkHost,
    };
  }

  return {
    ok: true, step: null,
    latency_ms: Date.now() - startedAt,
    resource_id: upload.resource_id,
    link_host: linkHost,
    dm_status: 'sent',
  };
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

  // Body is JSON-parsed by express.json() in server.js. req.body is
  // {} when the request body is empty or non-JSON.
  const body = req.body || {};
  const test = typeof body.test === 'string' ? body.test : null;
  const recipientUserId = typeof body.recipient_user_id === 'string' ? body.recipient_user_id : null;

  // Differentiated path: scenario + recipient → upload → mint → DM.
  if (test || recipientUserId) {
    if (!test || !VALID_TESTS.has(test)) {
      return res.status(400).json({
        ok: false, error: 'invalid_test',
        valid: Array.from(VALID_TESTS),
        latency_ms: Date.now() - startedAt,
      });
    }
    if (!recipientUserId || !SNOWFLAKE_RE.test(recipientUserId)) {
      return res.status(400).json({
        ok: false, error: 'invalid_recipient_user_id',
        latency_ms: Date.now() - startedAt,
      });
    }

    try {
      const result = await runScenario({ test, recipientUserId, apiKey });
      // Echo the test name + recipient so logs grep cleanly and the
      // Lambda's per-test attribution is unambiguous.
      const status = result.ok ? 200 : 500;
      return res.status(status).json({ ...result, test, recipient_user_id: recipientUserId });
    } catch (err) {
      logger.warn('Canary scenario threw', {
        test, recipient_user_id: recipientUserId,
        error: err?.message,
        latency_ms: Date.now() - startedAt,
      });
      return res.status(500).json({
        ok: false, step: null, error: 'scenario_threw',
        reason: err?.message,
        latency_ms: Date.now() - startedAt,
        test, recipient_user_id: recipientUserId,
      });
    }
  }

  // Legacy back-end-only path. Empty body falls through here —
  // exercises connector + mint without DM. Kept for back-compat
  // with any pre-extension Lambda or operator-manual probe
  // (`curl ... /canary/exec` with no body).
  //
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

    // 60-second TTL on the canary link.
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
