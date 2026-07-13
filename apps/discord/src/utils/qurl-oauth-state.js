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
//   sig   = HMAC-SHA256(state secret, payload).hex
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
const { createStateSigner } = require('./oauth-state');

const STATE_KIND = 'qurl-oauth';
const STATE_TTL_SECONDS = 5 * 60;

// Secret precedence (highest first): QURL_OAUTH_STATE_SECRET — flow-
// dedicated, lets ops rotate the qURL OAuth signer without invalidating
// in-flight GitHub OAuth links (preferred going forward) — then
// OAUTH_STATE_SECRET, the legacy shared secret (qURL + GitHub).
// kind-binding on the state token already prevents cross-purpose
// forgery, so sharing was secure; the precedence here is purely
// operational hygiene per PR #177 review (issue #184). The
// GITHUB_CLIENT_SECRET last-ditch fallback, the 32-char minimum secret
// length, the jest-only random fallback, and the rotation playbook all
// live in the shared signer — see utils/oauth-state.js.
const qurlOAuthStateSigner = createStateSigner({
  flowLabel: 'qURL OAuth state',
  secretConfigKeys: ['QURL_OAUTH_STATE_SECRET', 'OAUTH_STATE_SECRET'],
});

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
  return `${encoded}.${qurlOAuthStateSigner.sign(encoded)}`;
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
  // The shared verify folds a comparison-shape failure (the old
  // 'sig_compare_threw' reason) into `false` — unreachable anyway now
  // that the charset regex above guarantees equal-length hex buffers.
  if (!qurlOAuthStateSigner.verify(encoded, sig)) {
    return { ok: false, reason: 'sig_mismatch' };
  }

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
