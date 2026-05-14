/**
 * Tests for src/utils/time.js — expiry parsing + self-destruct preset
 * helpers. Critical because a bad expiry value used to throw RangeError
 * inside new Date(...).toISOString() mid-send.
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
  formatSelfDestructLabel,
  selfDestructSelectValueToSeconds,
  isLegitimateSelfDestructSelectValue,
  SELF_DESTRUCT_PRESETS,
  SELF_DESTRUCT_NO_TIMER_VALUE,
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
    it('exposes the 7 user-curated durations in ascending order', () => {
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
      // Ascending so the dropdown reads naturally short→long.
      for (let i = 1; i < seconds.length; i++) {
        expect(seconds[i]).toBeGreaterThan(seconds[i - 1]);
      }
    });

    it('SELF_DESTRUCT_NO_TIMER_VALUE is distinct from any preset value', () => {
      // The dropdown's "No timer" option uses this sentinel as its value.
      // It must not collide with any String(preset.seconds) so that
      // selfDestructSelectValueToSeconds can disambiguate.
      const presetValues = new Set(SELF_DESTRUCT_PRESETS.map((p) => String(p.seconds)));
      expect(presetValues.has(SELF_DESTRUCT_NO_TIMER_VALUE)).toBe(false);
    });
  });

  describe('selfDestructSelectValueToSeconds', () => {
    it('returns null for the no-timer sentinel', () => {
      expect(selfDestructSelectValueToSeconds(SELF_DESTRUCT_NO_TIMER_VALUE)).toBeNull();
    });

    it('returns the preset seconds for every preset value', () => {
      for (const preset of SELF_DESTRUCT_PRESETS) {
        expect(selfDestructSelectValueToSeconds(String(preset.seconds))).toBe(preset.seconds);
      }
    });

    it('returns null for unexpected values (forged interaction / option drift)', () => {
      // Defense — the option set is fixed by the form, but a forged
      // component interaction or a future drift in the option values
      // shouldn't accidentally become a half-set timer.
      for (const v of ['', '0', '2', '60', '7200', 'abc', '0x1', null, undefined]) {
        expect(selfDestructSelectValueToSeconds(v)).toBeNull();
      }
    });
  });

  describe('isLegitimateSelfDestructSelectValue', () => {
    // Predicate used by the form-side reject-vs-apply gate. Differs
    // from `selfDestructSelectValueToSeconds` (which returns null for
    // BOTH legitimate "no timer" AND forged values) by returning
    // `true` only for the closed legitimate set.
    it('true for the no-timer sentinel', () => {
      expect(isLegitimateSelfDestructSelectValue(SELF_DESTRUCT_NO_TIMER_VALUE)).toBe(true);
    });

    it('true for every preset seconds value (stringified)', () => {
      for (const preset of SELF_DESTRUCT_PRESETS) {
        expect(isLegitimateSelfDestructSelectValue(String(preset.seconds))).toBe(true);
      }
    });

    it('false for forged / unexpected values', () => {
      for (const v of ['', '0', '2', '60', '7200', 'abc', '0x1', null, undefined]) {
        expect(isLegitimateSelfDestructSelectValue(v)).toBe(false);
      }
    });

    it('false for numeric preset (must be the stringified form)', () => {
      // The select carries values as strings ('0.5', '1', ...). Direct
      // numeric input would forge past `selfDestructSelectValueToSeconds`
      // if String equality wasn't strict — this test pins the gate.
      for (const preset of SELF_DESTRUCT_PRESETS) {
        expect(isLegitimateSelfDestructSelectValue(preset.seconds)).toBe(false);
      }
    });
  });

  describe('formatSelfDestructLabel', () => {
    it('renders each preset seconds value as the matching label', () => {
      for (const preset of SELF_DESTRUCT_PRESETS) {
        expect(formatSelfDestructLabel(preset.seconds)).toBe(preset.label);
      }
    });

    it('falls back to a compact "Ns" rendering for off-preset values', () => {
      // Unreachable through the dropdown today (the option set is the
      // 7 presets) but defends against a future caller feeding in stored
      // off-preset state — never silently substitute a different preset.
      expect(formatSelfDestructLabel(2)).toBe('2s');
      expect(formatSelfDestructLabel(0.75)).toBe('0.75s');
    });

    it('renders "(invalid)" for non-finite stored values (corrupted DB row)', () => {
      // Defense against findPresetBySeconds receiving NaN/Infinity from a
      // backfilled row — never substitute a preset, never throw, never
      // surface the literal "NaNs"/"Infinitys" strings to the user.
      expect(formatSelfDestructLabel(NaN)).toBe('(invalid)');
      expect(formatSelfDestructLabel(Infinity)).toBe('(invalid)');
      expect(formatSelfDestructLabel(-Infinity)).toBe('(invalid)');
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
