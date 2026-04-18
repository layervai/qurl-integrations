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
// Returns the input unchanged when the key is unset so tests + dev installs
// work without ceremony; production boot validation should require it.

const PREFIX = 'enc:v1:';
const ALGO = 'aes-256-gcm';

let cachedKey = null;
function getKey() {
  if (cachedKey !== null) return cachedKey;
  const raw = process.env.KEY_ENCRYPTION_KEY;
  if (!raw) {
    cachedKey = false;
    return false;
  }
  const buf = Buffer.from(raw, 'base64');
  if (buf.length !== 32) {
    logger.error('KEY_ENCRYPTION_KEY must be 32 bytes (base64-encoded)');
    cachedKey = false;
    return false;
  }
  cachedKey = buf;
  return buf;
}

function encrypt(plaintext) {
  if (plaintext == null) return plaintext;
  const key = getKey();
  if (!key) return plaintext; // no key → store plaintext (dev/tests)
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
  const decipher = crypto.createDecipheriv(ALGO, key, Buffer.from(ivHex, 'hex'));
  decipher.setAuthTag(Buffer.from(tagHex, 'hex'));
  const pt = Buffer.concat([decipher.update(Buffer.from(ctHex, 'hex')), decipher.final()]);
  return pt.toString('utf8');
}

// Reset cache — test-only; lets jest env var changes take effect.
function _resetKeyCache() { cachedKey = null; }

module.exports = { encrypt, decrypt, _resetKeyCache };
