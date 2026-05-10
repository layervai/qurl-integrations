/**
 * Tests for src/utils/time.js — expiry parsing. Critical because a bad value
 * used to throw RangeError inside new Date(...).toISOString() mid-send.
 */

jest.mock('../src/logger', () => ({
  info: jest.fn(),
  warn: jest.fn(),
  error: jest.fn(),
  debug: jest.fn(),
  audit: jest.fn(),
}));

const {
  expiryToISO,
  expiryToMs,
  parseSelfDestructSeconds,
  SELF_DESTRUCT_MIN_SECONDS,
  SELF_DESTRUCT_MAX_SECONDS,
} = require('../src/utils/time');

describe('utils/time', () => {
  describe('expiryToMs', () => {
    it('parses minutes, hours, days', () => {
      expect(expiryToMs('30m')).toBe(30 * 60 * 1000);
      expect(expiryToMs('24h')).toBe(24 * 3600 * 1000);
      expect(expiryToMs('7d')).toBe(7 * 86400 * 1000);
    });

    it('caps at MAX_EXPIRY_MS (30 days)', () => {
      expect(expiryToMs('999d')).toBe(30 * 86400 * 1000);
    });

    it('rejects overflow / non-numeric with the 24h default', () => {
      const DEFAULT = 86400000;
      expect(expiryToMs('99999999999d')).toBe(DEFAULT); // regex rejects >6 digits
      expect(expiryToMs('')).toBe(DEFAULT);
      expect(expiryToMs('abc')).toBe(DEFAULT);
      expect(expiryToMs('1x')).toBe(DEFAULT);
      expect(expiryToMs(null)).toBe(DEFAULT);
      expect(expiryToMs(undefined)).toBe(DEFAULT);
      expect(expiryToMs('0d')).toBe(DEFAULT); // zero-valued rejected
    });
  });

  describe('parseSelfDestructSeconds', () => {
    // Mirrors the connector's parseExpireAfterMs codomain (PR #477):
    // 0.5–3600 inclusive accepted (above-max clamps), everything else
    // returns an error string the modal handler renders inline.
    it('treats absent / empty / whitespace as no-timer (no error)', () => {
      for (const v of [undefined, null, '', ' ', '   ', '\t', ' \n\t ']) {
        const r = parseSelfDestructSeconds(v);
        expect(r).toEqual({ seconds: null, error: null });
      }
    });

    it('accepts the floor exactly', () => {
      expect(parseSelfDestructSeconds('0.5')).toEqual({ seconds: 0.5, error: null });
    });

    it('accepts integers and decimals in range', () => {
      expect(parseSelfDestructSeconds('1')).toEqual({ seconds: 1, error: null });
      expect(parseSelfDestructSeconds('30')).toEqual({ seconds: 30, error: null });
      expect(parseSelfDestructSeconds('0.7')).toEqual({ seconds: 0.7, error: null });
      expect(parseSelfDestructSeconds('3600')).toEqual({ seconds: 3600, error: null });
    });

    it('clamps above-max silently (matches connector)', () => {
      expect(parseSelfDestructSeconds('3601')).toEqual({ seconds: 3600, error: null });
      expect(parseSelfDestructSeconds('100000')).toEqual({ seconds: 3600, error: null });
    });

    it('rejects sub-floor with a floor-specific message', () => {
      const r = parseSelfDestructSeconds('0.4');
      expect(r.seconds).toBeNull();
      expect(r.error).toMatch(/0\.5/);
    });

    it('rejects zero / negative as non-positive', () => {
      for (const v of ['0', '-5', '-0.5']) {
        const r = parseSelfDestructSeconds(v);
        expect(r.seconds).toBeNull();
        expect(r.error).toMatch(/positive/i);
      }
    });

    it('rejects non-numeric / NaN / Infinity', () => {
      for (const v of ['abc', 'NaN', 'Infinity', '-Infinity']) {
        const r = parseSelfDestructSeconds(v);
        expect(r.seconds).toBeNull();
        // Different error text per class is fine — pin only the rejection.
        expect(r.error).toBeTruthy();
      }
    });

    it('rejects hex prefix even though Number() does not accept hex floats', () => {
      // Symmetric with the connector's parseExpireAfterMs (Go strconv
      // accepts `0x1p3`). The bot's contract should reject the same
      // inputs even though JS Number() would already reject — pinned
      // so a future swap to a different parser doesn't regress.
      for (const v of ['0x1p3', '+0x1', '-0x1']) {
        const r = parseSelfDestructSeconds(v);
        expect(r.seconds).toBeNull();
        expect(r.error).toMatch(/hex/i);
      }
    });

    it('rejects oversized input (DoS bound)', () => {
      const huge = '9'.repeat(33);
      const r = parseSelfDestructSeconds(huge);
      expect(r.seconds).toBeNull();
      expect(r.error).toMatch(/too long/i);
    });

    it('exports MIN/MAX constants for callers building UI labels', () => {
      expect(SELF_DESTRUCT_MIN_SECONDS).toBe(0.5);
      expect(SELF_DESTRUCT_MAX_SECONDS).toBe(3600);
    });
  });

  describe('expiryToISO', () => {
    it('returns an ISO timestamp strictly in the future', () => {
      const now = Date.now();
      const iso = expiryToISO('1h');
      const parsed = Date.parse(iso);
      expect(parsed).toBeGreaterThan(now);
      expect(parsed - now).toBeGreaterThanOrEqual(3600_000 - 100);
      expect(parsed - now).toBeLessThanOrEqual(3600_000 + 100);
    });

    it('never throws RangeError on pathological input', () => {
      expect(() => expiryToISO('99999999999d')).not.toThrow();
      expect(() => expiryToISO('not-a-duration')).not.toThrow();
      expect(() => expiryToISO(null)).not.toThrow();
    });
  });
});
