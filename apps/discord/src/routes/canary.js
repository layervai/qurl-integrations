// Canary exec endpoint — exercised by the canary runner Lambda
// (qurl-integrations-infra/qurl-bot-discord/terraform/canary-exec.tf)
// to verify the bot's qURL send pipeline end-to-end every 3 hours.
//
// Auth is the qURL/NHP knock. The Lambda mints a single-use qURL
// targeting this endpoint and calls qurl-service /v1/resolve, which
// triggers an NHP knock that opens the bot's firewall to the
// Lambda's egress IP. By the time a request reaches the route,
// the caller has already proved possession of a valid qURL access
// token for `${BOT_URL}/canary/exec`. No HMAC.
//
// The 5-minute X-Canary-Timestamp window is kept as belt-and-
// suspenders against replay of a captured-and-resolved request
// past the qURL's lifetime.
//
// Body shape — empty OR both fields together. Partial body
// (one of the two) is rejected with 400 invalid_test:
//   {} → legacy back-end-only path (no DM)
//   { test: "send_file" | "send_location", recipient_user_id: "<snowflake>" }
//      → differentiated path: upload → mint → DM
//
// When `test` + `recipient_user_id` are both present, the canary:
//   1. Builds a synthetic resource shaped like the named test —
//      small text Buffer for send_file (raw-bytes connector path),
//      `{type, url, name}` location JSON for send_location (the JSON
//      connector path).
//   2. Uploads to the connector (reUploadBuffer / uploadJsonToConnector).
//   3. Mints a single 60s-TTL qURL via mintLinks.
//   4. DMs the recipient via sendDM with a clearly-labeled
//      "[Canary probe]" embed.
//
// Empty body falls through to the legacy back-end-only path. Kept
// for any operator-manual probe (curl with no body) and during the
// rollout window before the EventBridge Lambda starts sending the
// differentiated body.
//
// Response includes per-step status so the Lambda's per-test pass/fail
// metric can attribute failures to the correct subsystem:
//   { ok, step: "upload"|"mint"|"dm"|null, latency_ms, dm_status, ... }
//
// Recipient allowlist: the differentiated path rejects any
// `recipient_user_id` not in `config.CANARY_RECIPIENT_USER_IDS`
// with 403 recipient_not_allowed. Defense-in-depth — if the qURL
// auth path is ever bypassed, an attacker still can't DM arbitrary
// users. Allowlist mirrors the recipient list terraform passes
// into the Lambda's RECIPIENT_USER_IDS_JSON env.

const express = require('express');
const { EmbedBuilder } = require('discord.js');
const config = require('../config');
const logger = require('../logger');
const { reUploadBuffer, uploadJsonToConnector, mintLinks } = require('../connector');
const { sendDM } = require('../discord');
const { COLORS } = require('../constants');

const router = express.Router();

// 5-minute replay window — matches the qurl-service / NHP standard
// for token-resolution time, so an operator inspecting both layers
// sees the same number. The runner-to-bot path is synchronous and
// shouldn't see clock skew above a few seconds; the wider window
// absorbs Lambda cold-start + qURL mint + resolve overhead without
// forcing a tighter bound that would reject legit traffic during
// NHP slowness.
//
// Replay-trade note: a captured valid request CAN be replayed within
// the 5-min window (each replay mints a new connector resource +
// runs through to a DM). Acceptable surface here because:
//   - 4 KB body cap bounds replay payload size
//   - qURL one-time-use semantics make re-resolving the same token
//     impossible past the first resolve, so a captured request can
//     only be replayed against a still-open NHP firewall hole
//   - allowlist on recipient_user_id bounds DM blast radius
//   - canary scenarios are idempotent (the audit row tags the run-id
//     and a duplicate qURL-mint is functionally a no-op)
const TIMESTAMP_TOLERANCE_SECONDS = 300;

// Allowed `test` values. The Lambda iterates `SCENARIOS_JSON`; this
// set is the bot-side guardrail so a typo'd scenario in the Lambda
// env returns a clear 400 instead of being silently mistreated as
// the legacy back-end-only path.
const VALID_TESTS = new Set(['send_file', 'send_location']);

// Discord snowflake validation: 17-20 digits. Same shape the
// terraform `canary_recipient_user_ids` validation uses.
const SNOWFLAKE_RE = /^[0-9]{17,20}$/;

function verifyCanaryTimestamp(req, res, next) {
  // Pre-auth 401s deliberately omit `latency_ms` — every other failure
  // path on this route includes it for triage, but those are post-auth
  // and triggered by the trusted Lambda. Echoing latency_ms on
  // unauthenticated rejections leaks (microsecond-scale) timing info
  // for no operational benefit; the warn line below is the on-call
  // signal instead.
  const ts = req.header('X-Canary-Timestamp');
  if (!ts) {
    logger.warn('Canary timestamp rejected', { reason: 'missing_timestamp' });
    return res.status(401).json({ ok: false, error: 'missing_timestamp' });
  }
  // Strict shape — `parseInt('1234567890extra', 10)` returns
  // `1234567890`, so a digit-prefixed garbage string would slip past
  // a permissive isFinite check. Lock the contract to "Unix epoch
  // seconds, 10 digits today, 11 around the year ~2286" before
  // trusting it for a drift comparison.
  if (!/^[0-9]{10,11}$/.test(ts)) {
    logger.warn('Canary timestamp rejected', { reason: 'bad_timestamp' });
    return res.status(401).json({ ok: false, error: 'bad_timestamp' });
  }
  const tsInt = parseInt(ts, 10);
  const drift = Math.floor(Date.now() / 1000) - tsInt;
  if (Math.abs(drift) > TIMESTAMP_TOLERANCE_SECONDS) {
    logger.warn('Canary timestamp rejected', { reason: 'expired_timestamp', drift_seconds: drift });
    return res.status(401).json({ ok: false, error: 'expired_timestamp' });
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
  // Belt-and-suspenders: connector.js's reUploadBuffer +
  // uploadJsonToConnector both throw on missing resource_id today
  // (`Connector JSON upload returned no resource_id` etc.), so this
  // branch is unreachable against the real client. Kept so a future
  // connector refactor that switches to a return-shape contract
  // doesn't silently degrade the canary into a "no link minted, no
  // alarm" state.
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
    // Allowlist enforcement — defense-in-depth against an attacker
    // who somehow gets past the qURL/NHP knock. Empty allowlist →
    // 503 (server config state, not a client error — matches the
    // no_api_key shape). Mismatched recipient → 403 (request is
    // well-formed, authentication succeeded, but the principal is
    // not authorized to act on this target — textbook 403).
    // Response body still names the error code; the allowlist
    // itself is never exposed.
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
      // Add a structured warn line on any non-OK result so an on-call
      // who clicks through from a CloudWatch alarm finds a
      // correlatable log entry. Without this, `step: 'upload'` /
      // 'mint' / 'dm' failures emit metrics but no log — outer
      // catch only fires if runScenario throws, which is rare.
      if (!result.ok) {
        logger.warn('Canary scenario failed', {
          test, recipient_user_id: recipientUserId,
          step: result.step, error: result.error,
          reason: result.reason, apiCode: result.apiCode,
          latency_ms: result.latency_ms,
        });
      }
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
  // exercises connector + mint without DM. Reachable now only by a
  // qURL/NHP-knocked caller that posts an empty body (no operator
  // can curl this directly anymore — the route requires a recently-
  // resolved qURL). Kept for back-compat with any pre-extension
  // Lambda still in flight; once every fielded Lambda sends the
  // differentiated body, the next reader can drop this branch.
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
