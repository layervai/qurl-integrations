const logger = require('../logger');

const EXPIRY_UNITS = { m: 60, h: 3600, d: 86400 };
// Cap the expiry at 30 days in ms. An arbitrarily large numeric component in
// the expiry string (e.g. "99999999999d") would otherwise overflow Number
// arithmetic and make `new Date(...).toISOString()` throw RangeError.
const MAX_EXPIRY_MS = 30 * 86400 * 1000;
const DEFAULT_EXPIRY_MS = 86400000;

function parseExpiryMs(expiresIn) {
  const match = String(expiresIn ?? '').match(/^(\d{1,6})([mhd])$/);
  if (!match) return null;
  const ms = Number(match[1]) * EXPIRY_UNITS[match[2]] * 1000;
  if (!Number.isFinite(ms) || ms <= 0) return null;
  return Math.min(ms, MAX_EXPIRY_MS);
}

function expiryToISO(expiresIn) {
  const ms = parseExpiryMs(expiresIn);
  if (ms === null) {
    logger.warn('expiryToISO: invalid expiry format, defaulting to 24h', { expiresIn });
    return new Date(Date.now() + DEFAULT_EXPIRY_MS).toISOString();
  }
  return new Date(Date.now() + ms).toISOString();
}

function expiryToMs(expiresIn) {
  const ms = parseExpiryMs(expiresIn);
  return ms === null ? DEFAULT_EXPIRY_MS : ms;
}

// Self-destruct timer parsing — bot-side validation that mirrors the
// connector's `viewer_ttl_seconds` contract (qurl-s3-connector PR #477):
// 0.5 to 3600 seconds, fractional accepted. Out-of-range / NaN / Inf /
// non-numeric all map to a parse error so the caller can render an
// inline modal validation message rather than silently dropping the value.
//
// Internally named "self-destruct" (matches the user-facing modal label).
// The wire field forwarded to the connector is `viewer_ttl_seconds`.
const SELF_DESTRUCT_MIN_SECONDS = 0.5;
const SELF_DESTRUCT_MAX_SECONDS = 3600;

function parseSelfDestructSeconds(raw) {
  const trimmed = String(raw ?? '').trim();
  if (trimmed === '') return { seconds: null, error: null };
  // Length cap mirrors the connector — bounds CPU on hostile input
  // before parseFloat. Max legal `"3600.0"` is 6 chars; 32 is generous.
  if (trimmed.length > 32) {
    return { seconds: null, error: 'Value is too long.' };
  }
  // strconv.ParseFloat accepts hex-float (`0x1p3`) per Go spec; the
  // connector rejects it (PR #477). JS Number() does NOT accept hex
  // floats, but reject the prefix explicitly so the bot's contract
  // mirrors the connector's whether or not Number() is the parser.
  const prefix = trimmed.replace(/^[+-]/, '');
  if (/^0x/i.test(prefix)) {
    return { seconds: null, error: 'Use a decimal number, not hex.' };
  }
  const n = Number(trimmed);
  if (!Number.isFinite(n) || n <= 0) {
    return { seconds: null, error: 'Enter a positive number of seconds.' };
  }
  if (n < SELF_DESTRUCT_MIN_SECONDS) {
    return { seconds: null, error: `Minimum is ${SELF_DESTRUCT_MIN_SECONDS} seconds.` };
  }
  // Above-max is intentionally clamped (matches connector's silent clamp).
  // Returning the clamped value keeps the persisted state honest with
  // what the renderer will actually enforce.
  return { seconds: Math.min(n, SELF_DESTRUCT_MAX_SECONDS), error: null };
}

module.exports = {
  expiryToISO,
  expiryToMs,
  parseSelfDestructSeconds,
  SELF_DESTRUCT_MIN_SECONDS,
  SELF_DESTRUCT_MAX_SECONDS,
};
