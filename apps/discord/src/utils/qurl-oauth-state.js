// State token for the qURL OAuth flow (/qurl setup → Auth0 → mint API key).
//
// Distinct from the GitHub OAuth state in commands.js because the binding
// shape is different: GitHub state binds discord_id only; qURL state binds
// guild_id + discord_user_id + nonce + expiry. Sharing OAUTH_STATE_SECRET
// with GitHub OAuth is fine — the differing payloads + a `kind` field make
// cross-purpose forgery (replay a GitHub state as a qURL state, or vice
// versa) impossible: the `kind` is HMAC-covered, so flipping it invalidates
// the signature.
//
// Format:
//   state = base64url(JSON({k: 'qurl-oauth', g: guildId, u: discordUserId,
//                           n: nonce, e: expirySec})) + '.' + sigHex
//   sig   = HMAC-SHA256(stateSecret(), payload).hex
//
// 5-minute TTL. After Auth0 redirects back, the callback re-verifies the
// signature, parses the payload, checks `kind === 'qurl-oauth'`, and asserts
// expiry. Tampering across the boundary fails.
//
// Replay protection note: within the 5-minute TTL the same signed state
// CAN be presented to /callback multiple times by the same browser
// session — there's no consumed-nonce store. The practical impact is
// bounded because Auth0's `code` parameter is single-use and short-lived
// (so the second presentation needs a fresh Auth0 grant), and a re-mint
// on the bot side is an idempotent upsert in guild_configs. The signed
// state is NOT a one-shot token; it's an integrity envelope. PR copy and
// route comments avoid the "single-use" framing for that reason.
const crypto = require('crypto');
const config = require('../config');
const logger = require('../logger');
const {
  collectOAuthFlowStateSecrets,
  collectStateSecrets,
} = require('./oauth-state-secrets');

const STATE_KIND = 'qurl-oauth';
const STATE_TTL_SECONDS = 5 * 60;

let _warnedFallback = false;
let _warnedShortLegacy = false;
const _testFallbackSecret = crypto.randomBytes(32).toString('hex');

// Signing uses the first secret below; verification tries every configured
// secret in order so already-clicked links survive the migration window:
//   1. QURL_OAUTH_STATE_SECRET — flow-dedicated, lets ops rotate the
//      qURL OAuth signer without invalidating in-flight GitHub OAuth
//      links. Preferred going forward.
//   2. OAUTH_STATE_SECRET     — legacy shared secret (qURL + GitHub).
//      kind-binding on the state token already prevents cross-purpose
//      forgery, so sharing was secure; the precedence here is purely
//      operational hygiene per PR #177 review (issue #184).
//   3. GITHUB_CLIENT_SECRET   — last-ditch fallback for old/dev env-parity;
//      production boot requires #1 or #2 and will not start on this fallback.
//   4. Test fallback          — per-process random secret for jest only.
//
// Rotation playbook: provision QURL_OAUTH_STATE_SECRET in SSM while leaving
// OAUTH_STATE_SECRET present, deploy, wait longer than STATE_TTL_SECONDS, then
// replace OAUTH_STATE_SECRET with the SSM PLACEHOLDER sentinel. Verification
// accepts both configured keys during that window; after the legacy key is
// disabled it stops validating. If rotating both GitHub and qURL OAuth state
// together, pace legacy removal by the longer GitHub pending-link window.
// Minimum acceptable secret length — per round-9 #4. 32 chars is the
// floor for an HMAC-SHA256 secret with adequate entropy (matches the
// `0`.repeat(64) test fixture's order of magnitude, well below the
// 128-char hex secrets ops actually provisions). A 4-char accidental
// value would HMAC just fine with no security; reject upfront.
function warnShortLegacySecret(label, length) {
  if (!_warnedShortLegacy) {
    logger.warn(
      `Ignoring ${label} for qURL OAuth state: secret is too short `
      + `(got ${length}) while a dedicated secret is active.`
    );
    _warnedShortLegacy = true;
  }
}

function stateSecrets() {
  // If a dedicated secret is present but too short, fail closed instead of
  // silently falling back; the production boot guard catches that before serve.
  const secrets = collectOAuthFlowStateSecrets({
    primaryEnvName: 'QURL_OAUTH_STATE_SECRET',
    errorPrefix: 'Refusing to mint qURL OAuth state',
    warnShortOptional: warnShortLegacySecret,
  });
  if (secrets.length > 0) return secrets;

  if (!config.GITHUB_CLIENT_SECRET) {
    const inTestHarness = process.env.NODE_ENV === 'test'
      && (process.env.JEST_WORKER_ID || process.env.CI === 'true');
    if (!inTestHarness) {
      throw new Error(
        'Refusing to mint qURL OAuth state: QURL_OAUTH_STATE_SECRET, OAUTH_STATE_SECRET, '
        + 'or GITHUB_CLIENT_SECRET must be set.'
      );
    }
    if (!_warnedFallback) {
      logger.warn('qURL OAuth state HMAC using per-process random test fallback — set QURL_OAUTH_STATE_SECRET');
      _warnedFallback = true;
    }
    return [_testFallbackSecret];
  }
  // GITHUB_CLIENT_SECRET fallback is also length-checked — Auth0 /
  // GitHub provision 32+ char client secrets by default, but a manual
  // /placeholder env on a misconfigured dev box would slip past
  // otherwise.
  return collectStateSecrets([
    { value: config.GITHUB_CLIENT_SECRET, label: 'GITHUB_CLIENT_SECRET fallback' },
  ], {
    errorPrefix: 'Refusing to mint qURL OAuth state',
  });
}

function stateSecret() {
  return stateSecrets()[0];
}

function b64urlEncode(buf) {
  return Buffer.from(buf).toString('base64')
    .replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

function b64urlDecode(str) {
  // Re-pad to a multiple of 4 — base64url-encoded payloads strip trailing '='.
  const padded = str + '='.repeat((4 - (str.length % 4)) % 4);
  return Buffer.from(padded.replace(/-/g, '+').replace(/_/g, '/'), 'base64');
}

function signQurlOAuthState(guildId, discordUserId) {
  if (typeof guildId !== 'string' || !guildId) {
    throw new TypeError('signQurlOAuthState: guildId must be a non-empty string');
  }
  if (typeof discordUserId !== 'string' || !discordUserId) {
    throw new TypeError('signQurlOAuthState: discordUserId must be a non-empty string');
  }
  const nonce = crypto.randomBytes(16).toString('hex');
  const expirySec = Math.floor(Date.now() / 1000) + STATE_TTL_SECONDS;
  const payload = {
    k: STATE_KIND,
    g: guildId,
    u: discordUserId,
    n: nonce,
    e: expirySec,
  };
  const encoded = b64urlEncode(JSON.stringify(payload));
  // By design this throws on unusable signing config; production boot validates
  // state secrets before serving, so a sign-time throw is a dev/test signal.
  const sig = crypto.createHmac('sha256', stateSecret())
    .update(encoded)
    .digest('hex');
  return `${encoded}.${sig}`;
}

// Returns { ok: true, payload } on success, or { ok: false, reason } on
// failure. Reasons are intentionally coarse-grained on the wire ("invalid",
// "expired") so a probing attacker can't distinguish "wrong format" from
// "wrong signature" — caller logs the granular reason for triage.
function verifyQurlOAuthState(state) {
  if (typeof state !== 'string') return { ok: false, reason: 'not_a_string' };
  const parts = state.split('.');
  if (parts.length !== 2) return { ok: false, reason: 'malformed_parts' };
  const [encoded, sig] = parts;
  if (!/^[A-Za-z0-9_-]+$/.test(encoded) || !/^[0-9a-f]{64}$/.test(sig)) {
    return { ok: false, reason: 'malformed_chars' };
  }
  const sigBuf = Buffer.from(sig, 'hex');
  let secrets;
  try {
    secrets = stateSecrets();
  } catch {
    return { ok: false, reason: 'config_error' };
  }
  let sigOk;
  try {
    sigOk = false;
    for (const secret of secrets) {
      const expected = crypto.createHmac('sha256', secret)
        .update(encoded)
        .digest('hex');
      if (crypto.timingSafeEqual(sigBuf, Buffer.from(expected, 'hex'))) sigOk = true;
    }
  } catch {
    return { ok: false, reason: 'sig_compare_threw' };
  }
  if (!sigOk) return { ok: false, reason: 'sig_mismatch' };

  let payload;
  try {
    payload = JSON.parse(b64urlDecode(encoded).toString('utf8'));
  } catch {
    return { ok: false, reason: 'payload_unparseable' };
  }
  if (payload?.k !== STATE_KIND) return { ok: false, reason: 'wrong_kind' };
  if (typeof payload.g !== 'string' || !payload.g) return { ok: false, reason: 'no_guild_id' };
  if (typeof payload.u !== 'string' || !payload.u) return { ok: false, reason: 'no_user_id' };
  if (typeof payload.e !== 'number') return { ok: false, reason: 'no_expiry' };
  if (Math.floor(Date.now() / 1000) >= payload.e) return { ok: false, reason: 'expired' };
  return {
    ok: true,
    payload: { guildId: payload.g, discordUserId: payload.u, nonce: payload.n, expirySec: payload.e },
  };
}

module.exports = {
  signQurlOAuthState,
  verifyQurlOAuthState,
  STATE_KIND,
  STATE_TTL_SECONDS,
};
