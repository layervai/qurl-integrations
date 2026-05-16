const { loadGatewayHmacSecret } = require('../src/gateway-hmac-secret-loader');

const VALID_HEX_A = 'a'.repeat(64);
const VALID_HEX_B = 'b'.repeat(64);

describe('loadGatewayHmacSecret', () => {
  describe('happy path', () => {
    it('parses {current, previous} and returns both', () => {
      const raw = JSON.stringify({ current: VALID_HEX_A, previous: VALID_HEX_B });
      expect(loadGatewayHmacSecret(raw)).toEqual({
        current: VALID_HEX_A,
        previous: VALID_HEX_B,
      });
    });

    it('parses {current} alone (no rotation window) — previous normalizes to null', () => {
      const raw = JSON.stringify({ current: VALID_HEX_A });
      expect(loadGatewayHmacSecret(raw)).toEqual({
        current: VALID_HEX_A,
        previous: null,
      });
    });

    it('accepts uppercase hex (case-insensitive)', () => {
      const upper = 'A'.repeat(64);
      const result = loadGatewayHmacSecret(JSON.stringify({ current: upper }));
      expect(result.current).toBe(upper);
    });

    it('treats `previous: null` the same as missing — single-key mode', () => {
      const raw = JSON.stringify({ current: VALID_HEX_A, previous: null });
      expect(loadGatewayHmacSecret(raw).previous).toBeNull();
    });
  });

  describe('shape validation', () => {
    it('rejects undefined env var', () => {
      expect(() => loadGatewayHmacSecret(undefined)).toThrow(/empty or missing/);
    });

    it('rejects empty string env var', () => {
      expect(() => loadGatewayHmacSecret('')).toThrow(/empty or missing/);
    });

    it('rejects non-string input (defense-in-depth for a future caller mistake)', () => {
      expect(() => loadGatewayHmacSecret(42)).toThrow(/empty or missing/);
      expect(() => loadGatewayHmacSecret(null)).toThrow(/empty or missing/);
    });

    it('rejects malformed JSON', () => {
      expect(() => loadGatewayHmacSecret('not json')).toThrow(/not valid JSON/);
      expect(() => loadGatewayHmacSecret('{"unterminated')).toThrow(/not valid JSON/);
    });

    it('rejects a JSON array', () => {
      expect(() => loadGatewayHmacSecret('[1,2,3]')).toThrow(/must decode to a JSON object/);
    });

    it('rejects a JSON primitive (number / null / true)', () => {
      expect(() => loadGatewayHmacSecret('42')).toThrow(/must decode to a JSON object/);
      expect(() => loadGatewayHmacSecret('null')).toThrow(/must decode to a JSON object/);
      expect(() => loadGatewayHmacSecret('true')).toThrow(/must decode to a JSON object/);
    });
  });

  describe('current validation', () => {
    it('rejects missing current', () => {
      expect(() => loadGatewayHmacSecret('{}')).toThrow(/current must be a 64-char hex/);
    });

    it('rejects current as non-string', () => {
      expect(() => loadGatewayHmacSecret(JSON.stringify({ current: 42 })))
        .toThrow(/current must be a 64-char hex/);
    });

    it('rejects current with wrong length (63 chars)', () => {
      const short = 'a'.repeat(63);
      expect(() => loadGatewayHmacSecret(JSON.stringify({ current: short })))
        .toThrow(/current must be a 64-char hex/);
    });

    it('rejects current with wrong length (65 chars)', () => {
      const long = 'a'.repeat(65);
      expect(() => loadGatewayHmacSecret(JSON.stringify({ current: long })))
        .toThrow(/current must be a 64-char hex/);
    });

    it('rejects current with non-hex chars', () => {
      const bad = 'g'.repeat(64);
      expect(() => loadGatewayHmacSecret(JSON.stringify({ current: bad })))
        .toThrow(/current must be a 64-char hex/);
    });

    it('rejects current with mid-string non-hex char', () => {
      const bad = `${'a'.repeat(63)}z`;
      expect(() => loadGatewayHmacSecret(JSON.stringify({ current: bad })))
        .toThrow(/current must be a 64-char hex/);
    });
  });

  describe('previous validation', () => {
    it('rejects previous as non-string non-null', () => {
      expect(() => loadGatewayHmacSecret(JSON.stringify({ current: VALID_HEX_A, previous: 42 })))
        .toThrow(/previous, if present, must be a 64-char hex/);
    });

    it('rejects previous with wrong length', () => {
      const short = 'b'.repeat(60);
      expect(() => loadGatewayHmacSecret(JSON.stringify({ current: VALID_HEX_A, previous: short })))
        .toThrow(/previous, if present, must be a 64-char hex/);
    });

    it('rejects previous with non-hex chars', () => {
      const bad = 'z'.repeat(64);
      expect(() => loadGatewayHmacSecret(JSON.stringify({ current: VALID_HEX_A, previous: bad })))
        .toThrow(/previous, if present, must be a 64-char hex/);
    });

    it('rejects previous = empty string (would silently disable dual-accept)', () => {
      // An empty-string `previous` would parse as truthy-falsy in
      // gateway-hmac's `if (!matched && secrets.previous)` gate,
      // silently disabling dual-accept during a rotation. The loader
      // is the chokepoint that catches this misconfig at boot.
      expect(() => loadGatewayHmacSecret(JSON.stringify({ current: VALID_HEX_A, previous: '' })))
        .toThrow(/previous, if present, must be a 64-char hex/);
    });
  });

  describe('error shape', () => {
    it('attaches code GATEWAY_HMAC_SECRET_MALFORMED on every throw', () => {
      try {
        loadGatewayHmacSecret('not json');
        throw new Error('expected throw');
      } catch (err) {
        expect(err.code).toBe('GATEWAY_HMAC_SECRET_MALFORMED');
      }
    });

    it('does not echo the raw secret in the JSON-parse-failure path', () => {
      // The secret is the env var contents — never log it verbatim.
      // We mention "not valid JSON" + the JSON.parse error message
      // (which discloses the parse position, not the bytes).
      const raw = 'SECRET_SHOULD_NOT_APPEAR_HERE';
      try {
        loadGatewayHmacSecret(raw);
        throw new Error('expected throw');
      } catch (err) {
        expect(err.message).not.toContain('SECRET_SHOULD_NOT_APPEAR_HERE');
      }
    });
  });
});
