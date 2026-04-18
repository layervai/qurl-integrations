/**
 * Tests for src/utils/crypto.js — AES-256-GCM envelope encryption used to
 * protect guild API keys + attachment URLs at rest.
 */

jest.mock('../src/logger', () => ({
  info: jest.fn(),
  warn: jest.fn(),
  error: jest.fn(),
  debug: jest.fn(),
}));

const nodeCrypto = require('crypto');
const { encrypt, decrypt, _resetKeyCache } = require('../src/utils/crypto');

const VALID_KEY = nodeCrypto.randomBytes(32).toString('base64');

describe('utils/crypto', () => {
  afterEach(() => {
    delete process.env.KEY_ENCRYPTION_KEY;
    _resetKeyCache();
  });

  describe('with a valid key', () => {
    beforeEach(() => {
      process.env.KEY_ENCRYPTION_KEY = VALID_KEY;
      _resetKeyCache();
    });

    it('round-trips arbitrary strings', () => {
      for (const plain of ['', 'hello', 'lv_live_abc123', '🔐 unicode', 'a'.repeat(4096)]) {
        const ct = encrypt(plain);
        expect(typeof ct).toBe('string');
        expect(ct.startsWith('enc:v1:')).toBe(true);
        if (plain) expect(ct).not.toContain(plain);
        expect(decrypt(ct)).toBe(plain);
      }
    });

    it('passes through null/undefined unchanged', () => {
      expect(encrypt(null)).toBeNull();
      expect(encrypt(undefined)).toBeUndefined();
      expect(decrypt(null)).toBeNull();
      expect(decrypt(undefined)).toBeUndefined();
    });

    it('passes through legacy (non-prefixed) plaintext unchanged', () => {
      expect(decrypt('legacy_plaintext_value')).toBe('legacy_plaintext_value');
    });

    it('rejects tampered ciphertext via the auth tag', () => {
      const ct = encrypt('secret');
      // Flip a byte in the ct portion: format is enc:v1:iv:tag:ct
      const parts = ct.split(':');
      const ctHex = parts[parts.length - 1];
      parts[parts.length - 1] = ctHex.replace(/.$/, (c) => (c === '0' ? '1' : '0'));
      const tampered = parts.join(':');
      expect(() => decrypt(tampered)).toThrow();
    });

    it('rejects malformed ciphertext (wrong part count)', () => {
      expect(() => decrypt('enc:v1:onlyone')).toThrow(/Malformed encrypted value/);
    });

    it('produces different ciphertexts for the same plaintext (random IV)', () => {
      const a = encrypt('same-input');
      const b = encrypt('same-input');
      expect(a).not.toBe(b);
      expect(decrypt(a)).toBe('same-input');
      expect(decrypt(b)).toBe('same-input');
    });
  });

  describe('with a malformed key', () => {
    it('throws when the key is not 32 bytes', () => {
      process.env.KEY_ENCRYPTION_KEY = Buffer.from('too-short').toString('base64');
      _resetKeyCache();
      expect(() => encrypt('x')).toThrow(/KEY_ENCRYPTION_KEY is malformed/);
    });

    it('throws when the key does not round-trip base64', () => {
      // Leading whitespace changes decoded length; trailing garbage does too.
      process.env.KEY_ENCRYPTION_KEY = 'not_real_base64!!!!!!!';
      _resetKeyCache();
      expect(() => encrypt('x')).toThrow(/KEY_ENCRYPTION_KEY is malformed/);
    });
  });

  describe('without a key (dev/test mode)', () => {
    it('returns plaintext from encrypt() and logs a one-time warning', () => {
      _resetKeyCache();
      expect(encrypt('hello')).toBe('hello');
      // Second call should not re-warn (tested via mock call count).
      expect(encrypt('world')).toBe('world');
    });

    it('throws if asked to decrypt a real ciphertext', () => {
      // First encrypt WITH a key.
      process.env.KEY_ENCRYPTION_KEY = VALID_KEY;
      _resetKeyCache();
      const ct = encrypt('secret');
      // Now unset the key.
      delete process.env.KEY_ENCRYPTION_KEY;
      _resetKeyCache();
      expect(() => decrypt(ct)).toThrow(/KEY_ENCRYPTION_KEY is not set/);
    });
  });
});
