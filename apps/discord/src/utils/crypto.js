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
  // Buffer.from silently discards non-base64 chars and accepts wrong-length
  // input, so verify both the decoded length AND that re-encoding round-trips.
  // If the env var was mis-pasted we want a loud boot-time failure, not a
  // silent misconfiguration that only surfaces under load.
  if (buf.length !== 32 || buf.toString('base64') !== raw.trim()) {
    const err = new Error(
      'KEY_ENCRYPTION_KEY is malformed. Expected base64-encoded 32 bytes. ' +
      'Generate with: node -e "console.log(require(\'crypto\').randomBytes(32).toString(\'base64\'))"'
    );
    logger.error(err.message);
    throw err;
  }
  cachedKey = buf;
  return buf;
}

let plaintextWarned = false;
function encrypt(plaintext) {
  if (plaintext == null) return plaintext;
  const key = getKey();
  if (!key) {
    // Warn once per process so staging/preview environments without
    // KEY_ENCRYPTION_KEY don't silently store secrets in plaintext.
    // index.js fails boot in NODE_ENV=production so this branch only
    // runs in dev/test/staging.
    if (!plaintextWarned) {
      plaintextWarned = true;
      logger.warn('KEY_ENCRYPTION_KEY is not set — secrets are being stored in PLAINTEXT. Generate a key with `node -e "console.log(require(\'crypto\').randomBytes(32).toString(\'base64\'))"` and set KEY_ENCRYPTION_KEY in the environment before using in any shared deployment.');
    }
    return plaintext;
  }
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
  const decipher = crypto.createDecipheriv(ALGO, key, Buffer.from(ivHex, 'hex'));
  decipher.setAuthTag(Buffer.from(tagHex, 'hex'));
  const pt = Buffer.concat([decipher.update(Buffer.from(ctHex, 'hex')), decipher.final()]);
  return pt.toString('utf8');
}

// Reset cache — test-only; lets jest env var changes take effect.
function _resetKeyCache() { cachedKey = null; plaintextWarned = false; }

module.exports = { encrypt, decrypt, _resetKeyCache };
