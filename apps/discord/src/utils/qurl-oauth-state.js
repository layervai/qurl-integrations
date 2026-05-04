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

const STATE_KIND = 'qurl-oauth';
const STATE_TTL_SECONDS = 5 * 60;

let _warnedFallback = false;
const _testFallbackSecret = crypto.randomBytes(32).toString('hex');

// Mirrors stateSecret() in commands.js: prefer dedicated OAUTH_STATE_SECRET
// so a compromised AUTH0_CLIENT_SECRET / GITHUB_CLIENT_SECRET can be rotated
// independently. Falls back to GITHUB_CLIENT_SECRET for env-parity with the
// existing GitHub OAuth flow; throws hard outside Jest if neither is set.
function stateSecret() {
  const dedicated = process.env.OAUTH_STATE_SECRET;
  if (dedicated) return dedicated;
  if (!config.GITHUB_CLIENT_SECRET) {
    const inTestHarness = process.env.NODE_ENV === 'test'
      && (process.env.JEST_WORKER_ID || process.env.CI === 'true');
    if (!inTestHarness) {
      throw new Error('Refusing to mint qURL OAuth state: OAUTH_STATE_SECRET or GITHUB_CLIENT_SECRET must be set.');
    }
    if (!_warnedFallback) {
      // eslint-disable-next-line no-console
      console.warn('qURL OAuth state HMAC using per-process random test fallback — set OAUTH_STATE_SECRET or GITHUB_CLIENT_SECRET');
      _warnedFallback = true;
    }
    return _testFallbackSecret;
  }
  return config.GITHUB_CLIENT_SECRET;
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
  const expected = crypto.createHmac('sha256', stateSecret())
    .update(encoded)
    .digest('hex');
  let sigOk = false;
  try {
    sigOk = crypto.timingSafeEqual(Buffer.from(sig, 'hex'), Buffer.from(expected, 'hex'));
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
