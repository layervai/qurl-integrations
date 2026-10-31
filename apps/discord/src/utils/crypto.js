const crypto = require('crypto');
const logger = require('../logger');

// AES-256-GCM envelope encryption for secrets at rest.
//
// Format: `enc:v1:<iv-hex>:<tag-hex>:<ct-hex>` — versioned so we can rotate
// algorithm later; plaintext strings without this prefix are returned as-is
// so a deployment can roll out without a schema-wide migration first (next
// write re-encrypts).
//
// Key sourced from KEY_ENCRYPTION_KEY (base64-encoded, 32 bytes after decode).
// encrypt requires the key. encryptOrPlaintext explicitly permits the
// development fallback for operator-managed configuration.
//
// KEY ROTATION — IMPORTANT:
// Current and optional previous keys are cached on first use. Rotation needs
// a rolling restart. Keep KEY_ENCRYPTION_KEY_PREV until old ciphertext has
// been re-encrypted; new writes always use KEY_ENCRYPTION_KEY.
// Re-encrypt guild_configs.qurl_api_key and .webhook_secret, plus
// qurl_send_configs.attachment_url and .interaction_token before removing PREV.

const PREFIX = 'enc:v1:';
const ALGO = 'aes-256-gcm';

let cachedKey = null;
let cachedPrevKey = null;
function parseKey(raw, label) {
  if (!raw) return null;
  const buf = Buffer.from(raw, 'base64');
  if (buf.length !== 32 || buf.toString('base64') !== raw.trim()) {
    const err = new Error(
      `${label} is malformed. Expected base64-encoded 32 bytes. ` +
      'Generate with: node -e "console.log(require(\'crypto\').randomBytes(32).toString(\'base64\'))"'
    );
    logger.error(err.message);
    throw err;
  }
  return buf;
}
function getKey() {
  if (cachedKey !== null) return cachedKey;
  const buf = parseKey(process.env.KEY_ENCRYPTION_KEY, 'KEY_ENCRYPTION_KEY');
  cachedKey = buf === null ? false : buf;
  return cachedKey;
}
// Optional previous key for zero-downtime rotation: decrypt tries the
// current key first, then falls back to KEY_ENCRYPTION_KEY_PREV if the
// auth-tag verification fails. Encrypt ALWAYS uses the current key —
// next write re-encrypts stale rows under the new key. Operators keep
// PREV set through a rolling deploy, then remove it once DB is fully
// re-encrypted. See migration notes at the top of this file.
function getPrevKey() {
  if (cachedPrevKey !== null) return cachedPrevKey;
  const buf = parseKey(process.env.KEY_ENCRYPTION_KEY_PREV, 'KEY_ENCRYPTION_KEY_PREV');
  cachedPrevKey = buf === null ? false : buf;
  return cachedPrevKey;
}

let plaintextWarned = false;
function encryptOrPlaintext(plaintext) {
  if (plaintext == null) return plaintext;
  const key = getKey();
  if (!key) {
    // Warn once per process so dev installs without KEY_ENCRYPTION_KEY
    // don't silently store secrets in plaintext. index.js fails boot
    // when NODE_ENV=production OR when the qURL OAuth setup flow is
    // configured, so this branch only runs in dev/test environments
    // without OAuth wiring. For live third-party credentials, callers
    // MUST use encrypt() instead.
    if (!plaintextWarned) {
      plaintextWarned = true;
      logger.warn('KEY_ENCRYPTION_KEY is not set — secrets are being stored in PLAINTEXT. Generate a key with `node -e "console.log(require(\'crypto\').randomBytes(32).toString(\'base64\'))"` and set KEY_ENCRYPTION_KEY in the environment before using in any shared deployment.');
    }
    return plaintext;
  }
  return encryptWithKey(key, plaintext);
}

// Default sensitive-value encryption fails closed in every environment.
// Only operator-managed local configuration may opt into encryptOrPlaintext.
function encrypt(plaintext) {
  // Null/undefined pass through unchanged, intentionally matching
  // encryptOrPlaintext(). Callers that should never see a null at this point
  // gate required values upstream;
  // tightening to a throw here would break that contract.
  if (plaintext == null) return plaintext;
  const key = getKey();
  if (!key) {
    throw new Error('KEY_ENCRYPTION_KEY is required to persist this value but is not set. Refusing to store sensitive data in plaintext.');
  }
  return encryptWithKey(key, plaintext);
}

function encryptWithKey(key, plaintext) {
  const iv = crypto.randomBytes(12);
  const cipher = crypto.createCipheriv(ALGO, key, iv);
  const ct = Buffer.concat([cipher.update(String(plaintext), 'utf8'), cipher.final()]);
  const tag = cipher.getAuthTag();
  return `${PREFIX}${iv.toString('hex')}:${tag.toString('hex')}:${ct.toString('hex')}`;
}

function decrypt(value) {
  if (value == null || typeof value !== 'string') return value;
  if (!value.startsWith(PREFIX)) return value; // legacy plaintext row
  const key = getKey();
  if (!key) {
    throw new Error('KEY_ENCRYPTION_KEY is not set but an encrypted value was read');
  }
  const parts = value.slice(PREFIX.length).split(':');
  if (parts.length !== 3) throw new Error('Malformed encrypted value');
  const [ivHex, tagHex, ctHex] = parts;
  // AES-256-GCM: iv MUST be 12 bytes (24 hex) and auth tag MUST be 16 bytes
  // (32 hex). Validate before Buffer.from — that call silently discards
  // non-hex chars, which would let tampered input through the setAuthTag
  // API with a wrong-length buffer and surface a confusing error elsewhere.
  if (!/^[0-9a-f]{24}$/.test(ivHex)) throw new Error('Malformed encrypted value: bad iv');
  if (!/^[0-9a-f]{32}$/.test(tagHex)) throw new Error('Malformed encrypted value: bad tag');
  if (!/^[0-9a-f]*$/.test(ctHex)) throw new Error('Malformed encrypted value: bad ciphertext');
  const ivBuf = Buffer.from(ivHex, 'hex');
  const tagBuf = Buffer.from(tagHex, 'hex');
  const ctBuf = Buffer.from(ctHex, 'hex');
  // Try current key first; on auth-tag mismatch, fall back to KEY_ENCRYPTION_KEY_PREV
  // if set. Enables zero-downtime rotation — operators deploy with both keys,
  // re-encrypt rows on next write, then remove PREV.
  const tryDecipher = (k) => {
    const d = crypto.createDecipheriv(ALGO, k, ivBuf);
    d.setAuthTag(tagBuf);
    return Buffer.concat([d.update(ctBuf), d.final()]).toString('utf8');
  };
  try {
    return tryDecipher(key);
  } catch (err) {
    const prev = getPrevKey();
    if (prev) {
      try { return tryDecipher(prev); } catch { /* fall through to original err */ }
    }
    throw err;
  }
}

// Validate both write and fallback-read keys before serving traffic.
function smokeTestKeys() {
  const probe = 'boot-smoke';
  if (decrypt(encrypt(probe)) !== probe) throw new Error('current-key round-trip mismatch');
  const previous = getPrevKey();
  if (previous && decrypt(encryptWithKey(previous, probe)) !== probe) {
    throw new Error('previous-key round-trip mismatch');
  }
}

// Reset cache — test-only; lets jest env var changes take effect. Exported
// conditionally so a production caller that accidentally imports it can't
// null out the encryption key at runtime (which would break the next
// encrypt call mid-request).
function _resetKeyCache() { cachedKey = null; cachedPrevKey = null; plaintextWarned = false; }

const exports_ = { encryptOrPlaintext, encrypt, decrypt, smokeTestKeys };
if (process.env.NODE_ENV !== 'production') {
  exports_._resetKeyCache = _resetKeyCache;
}
module.exports = exports_;
