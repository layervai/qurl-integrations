// Canary exec endpoint — synthetic-monitor probe driven by the
// EventBridge Lambda in qurl-integrations-infra. Auth is the
// qURL/NHP knock; reaching the route means the caller already
// resolved a valid qURL targeting `${BOT_URL}/canary/exec`, so the
// firewall hole IS the auth. X-Canary-Timestamp + 5-min replay
// window are belt-and-suspenders.

const express = require('express');
const { EmbedBuilder } = require('discord.js');
const config = require('../config');
const logger = require('../logger');
const { reUploadBuffer, uploadJsonToConnector, mintLinks } = require('../connector');
const { sendDM } = require('../discord');
const { COLORS } = require('../constants');

const router = express.Router();

// Matches qurl-service's NHP token-resolution window so operators
// inspecting both layers see the same number. Replay defenses past
// the window: qurl-service one-time-use (load-bearing — relaxing
// that contract widens this window in practice), 4 KB body cap,
// recipient allowlist.
const TIMESTAMP_TOLERANCE_SECONDS = 300;

const VALID_TESTS = new Set(['send_file', 'send_location']);
const SNOWFLAKE_RE = /^[0-9]{17,20}$/;

function verifyCanaryTimestamp(req, res, next) {
  // ip is observe-only (logged, never gated) — under NHP it confirms
  // the runner; without NHP, a 401 stream from arbitrary IPs is the
  // first triage signal of a misconfigured mount.
  const ip = req.ip || 'unknown';
  const ts = req.header('X-Canary-Timestamp');
  if (!ts) {
    logger.warn('Canary timestamp rejected', { reason: 'missing_timestamp', ip });
    return res.status(401).json({ ok: false, error: 'missing_timestamp' });
  }
  // Strict shape — `parseInt('1234567890extra', 10)` returns
  // `1234567890`, so a digit-prefixed garbage string would slip past
  // a permissive isFinite check. Lock the contract to "10-digit Unix
  // epoch seconds" (covers everything until ~year 2286) before
  // trusting it for a drift comparison.
  if (!/^[0-9]{10}$/.test(ts)) {
    logger.warn('Canary timestamp rejected', { reason: 'bad_timestamp', ip });
    return res.status(401).json({ ok: false, error: 'bad_timestamp' });
  }
  const tsInt = parseInt(ts, 10);
  const drift = Math.floor(Date.now() / 1000) - tsInt;
  if (Math.abs(drift) > TIMESTAMP_TOLERANCE_SECONDS) {
    logger.warn('Canary timestamp rejected', { reason: 'expired_timestamp', drift_seconds: drift, ip });
    return res.status(401).json({ ok: false, error: 'expired_timestamp' });
  }
  next();
}

// Standalone embed (not commands.js's `buildDeliveryPayload`, which
// is gated to non-production). Embed shape doesn't need to match
// `/qurl send` — recipients know this is a probe.
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
  // Unreachable today — connector.js throws on missing resource_id.
  // Kept against a future refactor to a return-shape contract.
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

router.post('/exec', verifyCanaryTimestamp, async (req, res) => {
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
    // Allowlist is the last DM-blast defense if the qURL/NHP gate
    // is ever bypassed. Empty list → 503 (server config); mismatch
    // → 403 (auth OK, principal not authorized).
    const allowlist = config.CANARY_RECIPIENT_USER_IDS;
    if (!Array.isArray(allowlist) || allowlist.length === 0) {
      return res.status(503).json({
        ok: false, error: 'canary_recipients_unconfigured',
        latency_ms: Date.now() - startedAt,
      });
    }
    if (!allowlist.includes(recipientUserId)) {
      return res.status(403).json({
        ok: false, error: 'recipient_not_allowed',
        latency_ms: Date.now() - startedAt,
      });
    }

    try {
      const result = await runScenario({ test, recipientUserId, apiKey });
      // Step-level metrics emit even when runScenario doesn't throw —
      // log so on-call has a correlatable entry per CloudWatch alarm.
      if (!result.ok) {
        logger.warn('Canary scenario failed', {
          test, recipient_user_id: recipientUserId,
          step: result.step, error: result.error,
          reason: result.reason, apiCode: result.apiCode,
          latency_ms: result.latency_ms,
        });
      }
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

  // Legacy empty-body path — connector + mint, no DM. Pre-extension
  // Lambdas still hit this; drop once every fielded Lambda sends the
  // differentiated body. Location payload (not file) keeps this off
  // the Discord CDN download path so the canary isolates the
  // bot↔qURL hop.
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
      // Single-use credential — host only, never the full link.
      link_host: linkHost,
      resource_id: upload.resource_id,
    });
  } catch (err) {
    // warn not error — expected during outages; don't pollute the
    // error log alongside whatever's already firing.
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
