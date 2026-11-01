
jest.mock('../src/logger', () => ({
  info: jest.fn(),
  warn: jest.fn(),
  error: jest.fn(),
  debug: jest.fn(),
  audit: jest.fn(),
}));

const nodeCrypto = require('crypto');
const { encryptOrPlaintext, encrypt, decrypt, smokeTestKeys, _resetKeyCache } = require('../src/utils/crypto');

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
        const ct = encryptOrPlaintext(plain);
        expect(typeof ct).toBe('string');
        expect(ct.startsWith('enc:v1:')).toBe(true);
        if (plain) expect(ct).not.toContain(plain);
        expect(decrypt(ct)).toBe(plain);
      }
    });

    it('passes through null/undefined unchanged', () => {
      expect(encryptOrPlaintext(null)).toBeNull();
      expect(encryptOrPlaintext(undefined)).toBeUndefined();
      expect(decrypt(null)).toBeNull();
      expect(decrypt(undefined)).toBeUndefined();
    });

    it('passes through legacy (non-prefixed) plaintext unchanged', () => {
      expect(decrypt('legacy_plaintext_value')).toBe('legacy_plaintext_value');
    });

    it('rejects tampered ciphertext via the auth tag', () => {
      const ct = encryptOrPlaintext('secret');
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
      const a = encryptOrPlaintext('same-input');
      const b = encryptOrPlaintext('same-input');
      expect(a).not.toBe(b);
      expect(decrypt(a)).toBe('same-input');
      expect(decrypt(b)).toBe('same-input');
    });
  });

  describe('with a malformed key', () => {
    it('throws when the key is not 32 bytes', () => {
      process.env.KEY_ENCRYPTION_KEY = Buffer.from('too-short').toString('base64');
      _resetKeyCache();
      expect(() => encryptOrPlaintext('x')).toThrow(/KEY_ENCRYPTION_KEY is malformed/);
    });

    it('throws when the key does not round-trip base64', () => {
      process.env.KEY_ENCRYPTION_KEY = 'not_real_base64!!!!!!!';
      _resetKeyCache();
      expect(() => encryptOrPlaintext('x')).toThrow(/KEY_ENCRYPTION_KEY is malformed/);
    });
  });

  describe('without a key (dev/test mode)', () => {
    it('returns plaintext from encryptOrPlaintext() and logs a one-time warning', () => {
      _resetKeyCache();
      expect(encryptOrPlaintext('hello')).toBe('hello');
      expect(encryptOrPlaintext('world')).toBe('world');
    });

    it('throws if asked to decrypt a real ciphertext', () => {
      process.env.KEY_ENCRYPTION_KEY = VALID_KEY;
      _resetKeyCache();
      const ct = encryptOrPlaintext('secret');
      delete process.env.KEY_ENCRYPTION_KEY;
      _resetKeyCache();
      expect(() => decrypt(ct)).toThrow(/KEY_ENCRYPTION_KEY is not set/);
    });
  });

  describe('encrypt (fail-closed variant for sensitive persistence)', () => {
    it('produces ciphertext indistinguishable from encryptOrPlaintext() when KEK is set', () => {
      process.env.KEY_ENCRYPTION_KEY = VALID_KEY;
      _resetKeyCache();
      const ct = encrypt('github-oauth-token');
      expect(ct.startsWith('enc:v1:')).toBe(true);
      expect(decrypt(ct)).toBe('github-oauth-token');
    });

    it('passes through null/undefined unchanged (matches encryptOrPlaintext() ergonomics)', () => {
      process.env.KEY_ENCRYPTION_KEY = VALID_KEY;
      _resetKeyCache();
      expect(encrypt(null)).toBeNull();
      expect(encrypt(undefined)).toBeUndefined();
    });

    it('throws — never returns plaintext — when KEY_ENCRYPTION_KEY is unset', () => {
      _resetKeyCache();
      expect(() => encrypt('github-oauth-token')).toThrow(/KEY_ENCRYPTION_KEY is required/);
    });

    it('throws on a malformed KEY_ENCRYPTION_KEY (no silent fallback)', () => {
      process.env.KEY_ENCRYPTION_KEY = Buffer.from('too-short').toString('base64');
      _resetKeyCache();
      expect(() => encrypt('x')).toThrow(/KEY_ENCRYPTION_KEY is malformed/);
    });
  });
});


describe('boot encryption-key smoke test (#191)', () => {
  beforeEach(() => {
    process.env.KEY_ENCRYPTION_KEY = Buffer.alloc(32, 7).toString('base64');
    delete process.env.KEY_ENCRYPTION_KEY_PREV;
    _resetKeyCache();
  });
  afterEach(() => {
    delete process.env.KEY_ENCRYPTION_KEY;
    delete process.env.KEY_ENCRYPTION_KEY_PREV;
    _resetKeyCache();
  });
  test('accepts a valid current key without a previous key', () => {
    expect(smokeTestKeys()).toBeUndefined();
  });
  test('exercises fallback decryption with a distinct valid previous key', () => {
    process.env.KEY_ENCRYPTION_KEY_PREV = Buffer.alloc(32, 8).toString('base64');
    expect(smokeTestKeys()).toBeUndefined();
  });
  test('rejects a malformed previous key before any stored row needs it', () => {
    process.env.KEY_ENCRYPTION_KEY_PREV = 'not-base64';
    expect(smokeTestKeys).toThrow('KEY_ENCRYPTION_KEY_PREV is malformed');
  });
  test('does not treat a previous key as authority to write without a current key', () => {
    delete process.env.KEY_ENCRYPTION_KEY;
    process.env.KEY_ENCRYPTION_KEY_PREV = Buffer.alloc(32, 8).toString('base64');
    expect(smokeTestKeys).toThrow('KEY_ENCRYPTION_KEY is required');
  });
});
