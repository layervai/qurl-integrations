const crypto = require('crypto');
const config = require('../config');

const STATE_KIND = 'discord-install';
// TODO(upstream-contract): keep this bot-side ceiling in sync with the
// layerv.ai marketing page that mints Discord install state tokens.
const STATE_MAX_TTL_SECONDS = 10 * 60;
const STATE_TTL_SKEW_SECONDS = 30;

function b64urlDecode(str) {
  const padded = str + '='.repeat((4 - (str.length % 4)) % 4);
  return Buffer.from(padded.replace(/-/g, '+').replace(/_/g, '/'), 'base64');
}

function verifyMarketingInstallState(state, nowSec = Math.floor(Date.now() / 1000)) {
  if (typeof state !== 'string' || !state) {
    return { ok: false, reason: 'missing' };
  }
  const secret = config.DISCORD_INSTALL_STATE_SECRET;
  if (!secret) {
    return { ok: false, reason: 'secret_unset' };
  }
  if (secret.length < config.DISCORD_INSTALL_STATE_SECRET_MIN_CHARS) {
    return { ok: false, reason: 'secret_too_short' };
  }

  const dot = state.lastIndexOf('.');
  if (dot <= 0 || dot === state.length - 1) {
    return { ok: false, reason: 'malformed' };
  }
  const encoded = state.slice(0, dot);
  const sigHex = state.slice(dot + 1);
  if (!/^[a-f0-9]{64}$/i.test(sigHex)) {
    return { ok: false, reason: 'malformed' };
  }

  // TODO(upstream-contract): the layerv.ai marketing minter must use
  // this 64-char hex string as the raw HMAC key, not hex-decode it
  // before signing, or signatures will diverge.
  const expected = crypto.createHmac('sha256', secret).update(encoded).digest('hex');
  if (!crypto.timingSafeEqual(Buffer.from(sigHex, 'hex'), Buffer.from(expected, 'hex'))) {
    return { ok: false, reason: 'signature' };
  }

  let payload;
  try {
    payload = JSON.parse(b64urlDecode(encoded).toString('utf8'));
  } catch {
    return { ok: false, reason: 'payload' };
  }

  if (!payload || payload.k !== STATE_KIND) {
    return { ok: false, reason: 'kind' };
  }
  // `n` is reserved for a future seen-nonce cache. Today the signed
  // state is reusable until expiry; replay prevention comes from the
  // short TTL plus the optional required-state rollout flag.
  if (!Number.isInteger(payload.e)) {
    return { ok: false, reason: 'expiry_missing' };
  }
  if (payload.e + STATE_TTL_SKEW_SECONDS < nowSec) {
    return { ok: false, reason: 'expired' };
  }
  if (payload.e - nowSec > STATE_MAX_TTL_SECONDS + STATE_TTL_SKEW_SECONDS) {
    return { ok: false, reason: 'expiry_too_far' };
  }

  return { ok: true, payload };
}

module.exports = {
  STATE_MAX_TTL_SECONDS,
  verifyMarketingInstallState,
};
