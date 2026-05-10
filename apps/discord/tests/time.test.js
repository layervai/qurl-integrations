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
  formatSelfDestructLabel,
  SELF_DESTRUCT_PRESETS,
  SELF_DESTRUCT_MIN_SECONDS,
  SELF_DESTRUCT_MAX_SECONDS,
  SELF_DESTRUCT_OPTIONS_TEXT,
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

  describe('SELF_DESTRUCT_PRESETS', () => {
    it('exposes the 7 user-specified durations in ascending order', () => {
      const labels = SELF_DESTRUCT_PRESETS.map((p) => p.label);
      expect(labels).toEqual([
        '1/2 second',
        '1 second',
        '5 seconds',
        '30 seconds',
        '5 minutes',
        '30 minutes',
        '1 hour',
      ]);
      const seconds = SELF_DESTRUCT_PRESETS.map((p) => p.seconds);
      expect(seconds).toEqual([0.5, 1, 5, 30, 300, 1800, 3600]);
      // Ascending so the modal placeholder reads naturally short→long.
      for (let i = 1; i < seconds.length; i++) {
        expect(seconds[i]).toBeGreaterThan(seconds[i - 1]);
      }
    });

    it('MIN/MAX track the first and last preset', () => {
      expect(SELF_DESTRUCT_MIN_SECONDS).toBe(0.5);
      expect(SELF_DESTRUCT_MAX_SECONDS).toBe(3600);
    });

    it('OPTIONS_TEXT is the comma-joined label list (used in the modal placeholder)', () => {
      expect(SELF_DESTRUCT_OPTIONS_TEXT).toBe(
        '1/2 second, 1 second, 5 seconds, 30 seconds, 5 minutes, 30 minutes, 1 hour'
      );
    });
  });

  describe('parseSelfDestructSeconds', () => {
    it('treats absent / empty / whitespace as no-timer (no error)', () => {
      for (const v of [undefined, null, '', ' ', '   ', '\t', ' \n\t ']) {
        const r = parseSelfDestructSeconds(v);
        expect(r).toEqual({ seconds: null, error: null });
      }
    });

    it('accepts every preset by its friendly label', () => {
      for (const preset of SELF_DESTRUCT_PRESETS) {
        expect(parseSelfDestructSeconds(preset.label)).toEqual({
          seconds: preset.seconds,
          error: null,
        });
      }
    });

    it('accepts every preset by its raw seconds value', () => {
      for (const preset of SELF_DESTRUCT_PRESETS) {
        expect(parseSelfDestructSeconds(String(preset.seconds))).toEqual({
          seconds: preset.seconds,
          error: null,
        });
      }
    });

    it('is case- and whitespace-insensitive on labels', () => {
      expect(parseSelfDestructSeconds('5 MINUTES').seconds).toBe(300);
      expect(parseSelfDestructSeconds('  5   minutes  ').seconds).toBe(300);
      expect(parseSelfDestructSeconds('1 Hour').seconds).toBe(3600);
      expect(parseSelfDestructSeconds('1/2 SECOND').seconds).toBe(0.5);
    });

    it('rejects values outside the preset set with the option list', () => {
      for (const v of ['2', '7', '60', '120', '0.7', '3600.5', '7200']) {
        const r = parseSelfDestructSeconds(v);
        expect(r.seconds).toBeNull();
        expect(r.error).toContain('1/2 second');
        expect(r.error).toContain('1 hour');
      }
    });

    it('rejects non-preset friendly phrasings', () => {
      // Plausible-looking inputs that aren't in the preset set must fail
      // — the modal placeholder is the source of truth; partial matches
      // would be misleading (e.g., "2 minutes" silently becoming 5).
      for (const v of ['2 minutes', '10 seconds', '15 min', 'forever', '5 mins']) {
        const r = parseSelfDestructSeconds(v);
        expect(r.seconds).toBeNull();
        expect(r.error).toBeTruthy();
      }
    });

    it('rejects non-numeric / NaN / Infinity / hex', () => {
      for (const v of ['abc', 'NaN', 'Infinity', '-Infinity', '0x1p3', '+0x1', '-0x1']) {
        const r = parseSelfDestructSeconds(v);
        expect(r.seconds).toBeNull();
        expect(r.error).toBeTruthy();
      }
    });

    it('rejects oversized input (DoS bound)', () => {
      const huge = '9'.repeat(33);
      const r = parseSelfDestructSeconds(huge);
      expect(r.seconds).toBeNull();
      expect(r.error).toMatch(/too long/i);
    });
  });

  describe('formatSelfDestructLabel', () => {
    it('renders each preset seconds value as the matching label', () => {
      for (const preset of SELF_DESTRUCT_PRESETS) {
        expect(formatSelfDestructLabel(preset.seconds)).toBe(preset.label);
      }
    });

    it('falls back to a compact "Ns" rendering for off-preset values', () => {
      // Unreachable through the modal today (the parser blocks off-preset
      // input) but defends against a future caller feeding in stored
      // off-preset state — never silently substitute a different preset.
      expect(formatSelfDestructLabel(2)).toBe('2s');
      expect(formatSelfDestructLabel(0.75)).toBe('0.75s');
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
