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

module.exports = { expiryToISO, expiryToMs };
