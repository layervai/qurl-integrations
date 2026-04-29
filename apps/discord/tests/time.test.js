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

const { expiryToISO, expiryToMs } = require('../src/utils/time');

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
