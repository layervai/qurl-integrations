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
  formatSelfDestructSegment,
  formatSessionDurationSeconds,
  isPositiveFinite,
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

  describe('formatSelfDestructSegment', () => {
    it('renders each preset as "Self-destruct: <label>"', () => {
      for (const preset of SELF_DESTRUCT_PRESETS) {
        expect(formatSelfDestructSegment(preset.seconds))
          .toBe(`Self-destruct: ${preset.label}`);
      }
    });

    it('renders "Self-destruct: off" when no timer is set', () => {
      // The post-send confirm header always shows a self-destruct
      // segment for visual alignment — null/undefined map to the same
      // "off" sentinel as the form-side "No timer" dropdown option.
      expect(formatSelfDestructSegment(null)).toBe('Self-destruct: off');
      expect(formatSelfDestructSegment(undefined)).toBe('Self-destruct: off');
    });

    it('renders "Self-destruct: off" for non-finite / non-positive values', () => {
      // Same defense as formatSelfDestructLabel — a corrupted DB row
      // surfacing NaN/Infinity, or a 0 / negative value, falls through
      // to the "off" sentinel rather than leaking "(invalid)" or "0s".
      expect(formatSelfDestructSegment(NaN)).toBe('Self-destruct: off');
      expect(formatSelfDestructSegment(Infinity)).toBe('Self-destruct: off');
      expect(formatSelfDestructSegment(0)).toBe('Self-destruct: off');
      expect(formatSelfDestructSegment(-5)).toBe('Self-destruct: off');
    });
  });

  // formatSessionDurationSeconds is the connector→qurl-service ABI
  // formatter for the bot's `session_duration` wire field. Tested here
  // alongside its only legitimate input source (SELF_DESTRUCT_PRESETS)
  // so a future preset change forces a co-located test update.
  describe('formatSessionDurationSeconds', () => {
    it('every preset maps to "Ns" with whole-seconds floor', () => {
      // Round-trip the full preset set so adding a new preset forces a
      // decision about how this formatter handles it (and updates the
      // test if behavior changes).
      const expected = {
        0.5: '1s', // qurl-service MinSessionDuration floor
        1: '1s',
        5: '5s',
        30: '30s',
        300: '300s',
        1800: '1800s',
        3600: '3600s',
      };
      for (const preset of SELF_DESTRUCT_PRESETS) {
        expect(formatSessionDurationSeconds(preset.seconds))
          .toBe(expected[preset.seconds]);
      }
    });

    it('clamps the 0.5s preset to "1s" (MinSessionDuration floor)', () => {
      // qurl-service rejects sub-second session_duration via
      // MinSessionDuration = 1 * time.Second. The 0.5s preset's
      // fileviewer-side 500ms canvas blank still fires; only the L7
      // session window floors at 1s.
      expect(formatSessionDurationSeconds(0.5)).toBe('1s');
    });

    it('ceils fractional values > 1 (defensive — presets are integer ≥1)', () => {
      expect(formatSessionDurationSeconds(2.3)).toBe('3s');
      expect(formatSessionDurationSeconds(1.0001)).toBe('2s');
    });

    it('returns null for null / undefined / non-finite / non-numeric / ≤0', () => {
      // Mirrors the appendViewerTtl defensive contract on the sibling
      // upload wire field (connector.js:appendViewerTtl) so the bot
      // can never put "NaNs" / "Infinitys" on the wire and turn a
      // recoverable upstream-input mistake into a confusing 400 from
      // qurl-service::validateSessionDuration.
      const cases = [null, undefined, NaN, Infinity, -Infinity, '30', '0.5', true, false, {}, [], 0, -1, -0.5];
      for (const v of cases) {
        expect(formatSessionDurationSeconds(v)).toBeNull();
      }
    });
  });

  // isPositiveFinite is the shared "valid positive numeric
  // seconds/count/TTL" gate replacing 11 inline `Number.isFinite(x)
  // && x > 0` sites. Tested here directly (in addition to integration
  // coverage at each call site) so the contract lives co-located with
  // the formatters that share the gate.
  describe('isPositiveFinite', () => {
    it('returns true for positive finite numbers', () => {
      const cases = [0.5, 1, 5, 30, 1.0001, 1e308, Number.MAX_SAFE_INTEGER];
      for (const v of cases) {
        expect(isPositiveFinite(v)).toBe(true);
      }
    });

    it('returns false for null / undefined / NaN / ±Infinity', () => {
      const cases = [null, undefined, NaN, Infinity, -Infinity];
      for (const v of cases) {
        expect(isPositiveFinite(v)).toBe(false);
      }
    });

    it('returns false for zero and negative finite numbers', () => {
      const cases = [0, -0, -1, -0.5, -1e308, Number.MIN_SAFE_INTEGER];
      for (const v of cases) {
        expect(isPositiveFinite(v)).toBe(false);
      }
    });

    it('returns false for non-number types (no Number coercion)', () => {
      // Number.isFinite (strict variant, used internally) rejects
      // string/boolean/object/array without coercion. This is
      // load-bearing: the global isFinite() would coerce '0x1' → 1.
      const cases = ['1', '0.5', '30s', true, false, {}, [], () => 1];
      for (const v of cases) {
        expect(isPositiveFinite(v)).toBe(false);
      }
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
