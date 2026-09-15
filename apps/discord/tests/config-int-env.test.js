
const { captureFreshConfig } = require('./helpers/fresh-config');
describe('config.intEnv — strictInteger + minPositive (QURL_BOT_MAX_INFLIGHT_HANDLERS)', () => {
  test.each([
    ['100abc', 'trailing garbage'],
    ['1.5', 'non-integer float'],
    ['Infinity', 'infinity literal'],
    ['NaN', 'NaN literal'],
    ['abc', 'non-numeric'],
    [' ', 'whitespace only'],
  ])('rejects %p (%s) and falls back to default 100 with warn', (raw) => {
    captureFreshConfig({ QURL_BOT_MAX_INFLIGHT_HANDLERS: raw }, (cfg, warns) => {
      expect(cfg.QURL_BOT_MAX_INFLIGHT_HANDLERS).toBe(100);
      expect(warns.some((w) => w.includes('QURL_BOT_MAX_INFLIGHT_HANDLERS') && w.includes('rejected'))).toBe(true);
    });
  });

  test.each([
    ['-5', 'negative'],
    ['0', 'zero'],
  ])('rejects %p (%s, fails minPositive) and falls back to default 100 with warn', (raw) => {
    captureFreshConfig({ QURL_BOT_MAX_INFLIGHT_HANDLERS: raw }, (cfg, warns) => {
      expect(cfg.QURL_BOT_MAX_INFLIGHT_HANDLERS).toBe(100);
      expect(warns.some((w) => w.includes('QURL_BOT_MAX_INFLIGHT_HANDLERS') && w.includes('rejected'))).toBe(true);
    });
  });

  test.each([
    ['1', 1],
    ['50', 50],
    ['100', 100],
    ['10000', 10000],
  ])('accepts %p as %i with no warning', (raw, expected) => {
    captureFreshConfig({ QURL_BOT_MAX_INFLIGHT_HANDLERS: raw }, (cfg, warns) => {
      expect(cfg.QURL_BOT_MAX_INFLIGHT_HANDLERS).toBe(expected);
      expect(warns.filter((w) => w.includes('QURL_BOT_MAX_INFLIGHT_HANDLERS'))).toHaveLength(0);
    });
  });

  test('unset env var resolves to default 100 without warning', () => {
    captureFreshConfig({ QURL_BOT_MAX_INFLIGHT_HANDLERS: undefined }, (cfg, warns) => {
      expect(cfg.QURL_BOT_MAX_INFLIGHT_HANDLERS).toBe(100);
      expect(warns.filter((w) => w.includes('QURL_BOT_MAX_INFLIGHT_HANDLERS'))).toHaveLength(0);
    });
  });

  test('empty string treated as unset (no warning)', () => {
    captureFreshConfig({ QURL_BOT_MAX_INFLIGHT_HANDLERS: '' }, (cfg, warns) => {
      expect(cfg.QURL_BOT_MAX_INFLIGHT_HANDLERS).toBe(100);
      expect(warns.filter((w) => w.includes('QURL_BOT_MAX_INFLIGHT_HANDLERS'))).toHaveLength(0);
    });
  });
});

describe('config.intEnv — strictInteger + min + max (QURL_BOT_DRAIN_DEADLINE_MS)', () => {
  test.each([
    ['99', 'just below the floor'],
    ['8001', 'just above the ceiling'],
    ['50000', 'order of magnitude over'],
    ['1', 'far below floor'],
  ])('out-of-range %p (%s) falls back to default 3000 with "out of range" warn', (raw) => {
    captureFreshConfig({ QURL_BOT_DRAIN_DEADLINE_MS: raw }, (cfg, warns) => {
      expect(cfg.QURL_BOT_DRAIN_DEADLINE_MS).toBe(3000);
      expect(warns.some((w) => w.includes('QURL_BOT_DRAIN_DEADLINE_MS') && w.includes('out of range'))).toBe(true);
    });
  });

  test.each([
    ['100', 100], // exact floor
    ['1500', 1500],
    ['3000', 3000],
    ['8000', 8000], // exact ceiling
  ])('in-range %p accepted as %i', (raw, expected) => {
    captureFreshConfig({ QURL_BOT_DRAIN_DEADLINE_MS: raw }, (cfg, warns) => {
      expect(cfg.QURL_BOT_DRAIN_DEADLINE_MS).toBe(expected);
      expect(warns.filter((w) => w.includes('QURL_BOT_DRAIN_DEADLINE_MS'))).toHaveLength(0);
    });
  });

  test.each([
    ['100abc', 'trailing garbage'],
    ['1.5', 'non-integer'],
    ['abc', 'non-numeric'],
  ])('non-integer %p (%s) rejected before range check', (raw) => {
    captureFreshConfig({ QURL_BOT_DRAIN_DEADLINE_MS: raw }, (cfg, warns) => {
      expect(cfg.QURL_BOT_DRAIN_DEADLINE_MS).toBe(3000);
      const drainWarns = warns.filter((w) => w.includes('QURL_BOT_DRAIN_DEADLINE_MS'));
      expect(drainWarns.some((w) => w.includes('rejected'))).toBe(true);
      expect(drainWarns.some((w) => w.includes('out of range'))).toBe(false);
    });
  });

  test('unset env var resolves to default 3000 without warning', () => {
    captureFreshConfig({ QURL_BOT_DRAIN_DEADLINE_MS: undefined }, (cfg, warns) => {
      expect(cfg.QURL_BOT_DRAIN_DEADLINE_MS).toBe(3000);
      expect(warns.filter((w) => w.includes('QURL_BOT_DRAIN_DEADLINE_MS'))).toHaveLength(0);
    });
  });
});

describe('config.intEnv — lenient mode (parseInt fallback, no strictInteger)', () => {
  test('PORT accepts "3000" → 3000', () => {
    captureFreshConfig({ PORT: '3000' }, (cfg) => {
      expect(cfg.PORT).toBe(3000);
    });
  });

  test('PORT lenient-parses "8080abc" → 8080 (no strictInteger flag)', () => {
    captureFreshConfig({ PORT: '8080abc' }, (cfg) => {
      expect(cfg.PORT).toBe(8080);
    });
  });

  test('PORT unset → default 3000', () => {
    captureFreshConfig({ PORT: undefined }, (cfg) => {
      expect(cfg.PORT).toBe(3000);
    });
  });

  test('QURL_SEND_MAX_RECIPIENTS lenient + minPositive: "0" → default 20000 with warn', () => {
    captureFreshConfig({ QURL_SEND_MAX_RECIPIENTS: '0' }, (cfg, warns) => {
      expect(cfg.QURL_SEND_MAX_RECIPIENTS).toBe(20000);
      expect(warns.some((w) => w.includes('QURL_SEND_MAX_RECIPIENTS') && w.includes('must be > 0'))).toBe(true);
    });
  });
});

describe('config — QURL_DETECT_COOLDOWN_MS (defaults to send, decoupled)', () => {
  test('unset → defaults to the send cooldown (no behavior change)', () => {
    captureFreshConfig(
      { QURL_DETECT_COOLDOWN_MS: undefined, QURL_SEND_COOLDOWN_MS: undefined },
      (cfg) => {
        expect(cfg.QURL_DETECT_COOLDOWN_MS).toBe(cfg.QURL_SEND_COOLDOWN_MS);
        expect(cfg.QURL_DETECT_COOLDOWN_MS).toBe(30000); // the send default
      },
    );
  });

  test('unset detect + overridden send → tracks the send override', () => {
    captureFreshConfig(
      { QURL_DETECT_COOLDOWN_MS: undefined, QURL_SEND_COOLDOWN_MS: '45000' },
      (cfg) => {
        expect(cfg.QURL_SEND_COOLDOWN_MS).toBe(45000);
        expect(cfg.QURL_DETECT_COOLDOWN_MS).toBe(45000);
      },
    );
  });

  test('explicit detect override → decoupled from send', () => {
    captureFreshConfig(
      { QURL_DETECT_COOLDOWN_MS: '90000', QURL_SEND_COOLDOWN_MS: '30000' },
      (cfg) => {
        expect(cfg.QURL_SEND_COOLDOWN_MS).toBe(30000);
        expect(cfg.QURL_DETECT_COOLDOWN_MS).toBe(90000);
      },
    );
  });

  test('minPositive: "0" → falls back to the send value with a warn', () => {
    captureFreshConfig(
      { QURL_DETECT_COOLDOWN_MS: '0', QURL_SEND_COOLDOWN_MS: '30000' },
      (cfg, warns) => {
        expect(cfg.QURL_DETECT_COOLDOWN_MS).toBe(30000);
        expect(warns.some((w) => w.includes('QURL_DETECT_COOLDOWN_MS') && w.includes('must be > 0'))).toBe(true);
      },
    );
  });
});

describe('config — QURL_VIEW_COUNTER_COALESCE_MS (sub-second only)', () => {
  test('unset → defaults to the largest sub-second window', () => {
    captureFreshConfig({ QURL_VIEW_COUNTER_COALESCE_MS: undefined }, (cfg, warns) => {
      expect(cfg.QURL_VIEW_COUNTER_COALESCE_MS).toBe(900);
      expect(warns.filter((w) => w.includes('QURL_VIEW_COUNTER_COALESCE_MS'))).toHaveLength(0);
    });
  });

  test('accepts an in-range sub-second override', () => {
    captureFreshConfig({ QURL_VIEW_COUNTER_COALESCE_MS: '500' }, (cfg, warns) => {
      expect(cfg.QURL_VIEW_COUNTER_COALESCE_MS).toBe(500);
      expect(warns.filter((w) => w.includes('QURL_VIEW_COUNTER_COALESCE_MS'))).toHaveLength(0);
    });
  });

  test('rejects over-900ms override back to default 900 with warn', () => {
    captureFreshConfig({ QURL_VIEW_COUNTER_COALESCE_MS: '1500' }, (cfg, warns) => {
      expect(cfg.QURL_VIEW_COUNTER_COALESCE_MS).toBe(900);
      expect(warns.some((w) => (
        w.includes('QURL_VIEW_COUNTER_COALESCE_MS')
        && w.includes('out of range <= 900')
      ))).toBe(true);
    });
  });
});
