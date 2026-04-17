const logger = require('../logger');

const EXPIRY_UNITS = { m: 60, h: 3600, d: 86400 };

function expiryToISO(expiresIn) {
  const match = expiresIn.match(/^(\d+)([mhd])$/);
  if (!match) {
    logger.warn('expiryToISO: invalid expiry format, defaulting to 24h', { expiresIn });
    return new Date(Date.now() + 86400000).toISOString();
  }
  const ms = parseInt(match[1]) * EXPIRY_UNITS[match[2]] * 1000;
  return new Date(Date.now() + ms).toISOString();
}

function expiryToMs(expiresIn) {
  const match = expiresIn.match(/^(\d+)([mhd])$/);
  if (!match) return 86400000;
  return parseInt(match[1]) * EXPIRY_UNITS[match[2]] * 1000;
}

module.exports = { expiryToISO, expiryToMs };
