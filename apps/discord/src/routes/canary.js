// Canary exec endpoint — synthetic-monitor probe driven by the
// EventBridge Lambda in qurl-integrations-infra#493. Auth is the
// NHP knock at the network layer (Lambda calls /nhp/internal/knock
// directly; AC opens iptables hole for the Lambda's egress IP
// within OpenTime). No HMAC or timestamp at this layer — NHP's
// per-knock OpenTime subsumes replay defense.
//
// Body shape: `{test, recipient_user_id}` required. Upload → mint
// → DM the recipient with a "[Canary probe]" embed. Step-level
// status returned so the Lambda's per-test metric attributes
// failures to the right subsystem (upload | mint | dm).
//
// Upstream error detail (reason, apiCode) stays in logs only —
// never echoed in HTTP responses, since reflecting upstream
// internals to an arbitrary caller would leak connector internals
// if NHP ever fails open.

const express = require('express');
const { EmbedBuilder } = require('discord.js');
const config = require('../config');
const logger = require('../logger');
const { reUploadBuffer, uploadJsonToConnector, mintLinks } = require('../connector');
const { sendDM } = require('../discord');
const { COLORS } = require('../constants');

const router = express.Router();

const VALID_TESTS = new Set(['send_file', 'send_location']);
const SNOWFLAKE_RE = /^[0-9]{17,20}$/;

// Standalone embed (not commands.js's `buildDeliveryPayload`, which
// is gated to non-production). Embed shape doesn't need to match
// `/qurl send` — recipients know this is a probe.
function buildCanaryDmPayload({ test, qurlLink, resourceId, expiresAt }) {
  // Guard against `expiresAt` being undefined/garbage from a future
  // caller-shape change — `Math.floor(NaN)` would render `<t:NaN:R>`
  // in the embed otherwise. `null` makes Discord drop the relative
  // timestamp gracefully.
  const t = new Date(expiresAt).getTime();
  const expiresUnix = Number.isFinite(t) ? Math.floor(t / 1000) : null;
  const expiresFragment = expiresUnix === null ? '' : `, expires <t:${expiresUnix}:R>`;
  const embed = new EmbedBuilder()
    .setColor(COLORS.QURL_BRAND)
    .setTitle(`[Canary probe] ${test}`)
    .setDescription(`Synthetic test from the canary-exec probe — confirms the qURL pipeline (connector upload → mint → DM) is healthy.\n\n[qURL link](${qurlLink}) (resource_id: \`${resourceId}\`${expiresFragment})`)
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
    } else if (test === 'send_location') {
      const probePayload = {
        type: 'google-map',
        url: 'https://maps.app.goo.gl/canary-probe',
        name: 'canary',
      };
      upload = await uploadJsonToConnector(probePayload, 'canary-location.json', apiKey);
    } else {
      // Defense in depth: VALID_TESTS upstream already gates this.
      // Explicit default catches the "added to VALID_TESTS, forgot
      // to wire runScenario" refactor — fail fast with a named
      // error instead of silently falling through to the location
      // path. step: null because no upload was attempted — keeps
      // the Lambda's per-step metric attribution honest.
      return {
        ok: false, step: null, error: 'unhandled_test',
        latency_ms: Date.now() - startedAt,
      };
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

router.post('/exec', async (req, res) => {
  const startedAt = Date.now();
  const apiKey = config.QURL_API_KEY;
  if (!apiKey) {
    // Multi-tenant deployments don't set a global QURL_API_KEY —
    // canary is single-tenant-prod-only. Fail closed.
    return res.status(503).json({ ok: false, error: 'no_api_key', latency_ms: Date.now() - startedAt });
  }

  const body = req.body || {};
  const test = typeof body.test === 'string' ? body.test : null;
  const recipientUserId = typeof body.recipient_user_id === 'string' ? body.recipient_user_id : null;

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
  // Allowlist is the last DM-blast defense if the NHP gate is ever
  // bypassed. Empty list → 503 (server config); mismatch → 403.
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
    if (!result.ok) {
      // Upstream reason + apiCode go to logs only — never echoed in
      // the response body. The Lambda is internal, but if NHP ever
      // fails open, reflecting upstream error detail to an arbitrary
      // caller would leak connector internals.
      logger.warn('Canary scenario failed', {
        test, recipient_user_id: recipientUserId,
        step: result.step, error: result.error,
        reason: result.reason, apiCode: result.apiCode,
        latency_ms: result.latency_ms,
      });
    }
    // Strip reason + apiCode before responding — same rationale.
    // eslint-disable-next-line no-unused-vars
    const { reason, apiCode, ...safe } = result;
    const status = result.ok ? 200 : 500;
    return res.status(status).json({ ...safe, test, recipient_user_id: recipientUserId });
  } catch (err) {
    logger.warn('Canary scenario threw', {
      test, recipient_user_id: recipientUserId,
      error: err?.message,
      latency_ms: Date.now() - startedAt,
    });
    return res.status(500).json({
      ok: false, step: null, error: 'scenario_threw',
      latency_ms: Date.now() - startedAt,
      test, recipient_user_id: recipientUserId,
    });
  }
});

module.exports = router;
