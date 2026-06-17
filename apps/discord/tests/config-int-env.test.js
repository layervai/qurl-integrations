/**
 * Unit tests for config.intEnv (apps/discord/src/config.js).
 *
 * intEnv is the shared env-var-int parser used by every module that
 * needs to read a tunable integer from the environment (event-consumer's
 * QURL_BOT_MAX_INFLIGHT_HANDLERS, event-consumer + event-publisher's
 * QURL_BOT_DRAIN_DEADLINE_MS, qurl-file-map's recipient caps, etc.).
 * A regression in this helper would silently mistune every consumer,
 * so pin every branch.
 *
 * The tests work by setting process.env, re-requiring config inside
 * jest.isolateModules to capture a fresh value, and asserting on the
 * resolved number plus the captured console.warn output. Direct
 * function-export isn't available because intEnv is closure-private
 * to config.js, but the resolved exports (e.g. QURL_BOT_DRAIN_DEADLINE_MS)
 * expose the full path under each scenario.
 */

function captureFreshConfig(envOverrides, run) {
  jest.isolateModules(() => {
    const prevValues = {};
    const origConsoleWarn = console.warn;
    const warns = [];
    console.warn = (...args) => warns.push(args.join(' '));
    try {
      for (const [key, value] of Object.entries(envOverrides)) {
        prevValues[key] = process.env[key];
        if (value === undefined) {
          delete process.env[key];
        } else {
          process.env[key] = value;
        }
      }
      const fresh = require('../src/config');
      run(fresh, warns);
    } finally {
      console.warn = origConsoleWarn;
      for (const [key, prev] of Object.entries(prevValues)) {
        if (prev === undefined) {
          delete process.env[key];
        } else {
          process.env[key] = prev;
        }
      }
    }
  });
}

describe('config — Discord install state rollout', () => {
  test('trims DISCORD_INSTALL_STATE_SECRET and parses required-state flag literally', () => {
    const secret = '2'.repeat(64);
    captureFreshConfig({
      DISCORD_INSTALL_STATE_SECRET: `  ${secret}\n`,
      DISCORD_INSTALL_STATE_REQUIRED: 'true',
    }, (cfg) => {
      expect(cfg.DISCORD_INSTALL_STATE_SECRET).toBe(secret);
      expect(cfg.DISCORD_INSTALL_STATE_SECRET_MIN_CHARS).toBe(64);
      expect(cfg.DISCORD_INSTALL_STATE_REQUIRED).toBe(true);
    });
  });

  test('keeps required-state flag off for typo values', () => {
    captureFreshConfig({ DISCORD_INSTALL_STATE_REQUIRED: 'TRUE' }, (cfg) => {
      expect(cfg.DISCORD_INSTALL_STATE_REQUIRED).toBe(false);
    });
  });
});

describe('config.intEnv — strictInteger + minPositive (QURL_BOT_MAX_INFLIGHT_HANDLERS)', () => {
  // QURL_BOT_MAX_INFLIGHT_HANDLERS is the canonical strictInteger +
  // minPositive caller. Trailing-garbage rejection is load-bearing:
  // an operator who types "100abc" into SSM should see the boot warn
  // rather than silently get cap=100 (parseInt's lenient behavior).
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
      // No "rejected" / "out of range" — the only warns acceptable at
      // boot are unrelated config logs (GUILD_ID parsing, etc.).
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
    // SSM-templated params sometimes seed empty strings — should NOT
    // false-positive the warn path that real bad values trigger.
    captureFreshConfig({ QURL_BOT_MAX_INFLIGHT_HANDLERS: '' }, (cfg, warns) => {
      expect(cfg.QURL_BOT_MAX_INFLIGHT_HANDLERS).toBe(100);
      expect(warns.filter((w) => w.includes('QURL_BOT_MAX_INFLIGHT_HANDLERS'))).toHaveLength(0);
    });
  });
});

describe('config.intEnv — strictInteger + min + max (QURL_BOT_DRAIN_DEADLINE_MS)', () => {
  // Drain deadline is range-clamped: too-large pushes past
  // gracefulShutdown's 10s budget, too-small is operationally a
  // disabled-drain knob (the unset-env path already provides that).
  // Range bounds [100, 8000] are documented in config.js + .env.example.
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
      // The "rejected" warn fires from the strictInteger path, NOT
      // "out of range" — order matters for the operator-facing log.
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
  // The original intEnv shape (still used by PORT, RATE_LIMIT_*,
  // PENDING_LINK_EXPIRY_MINUTES, etc.) is lenient — parseInt accepts
  // trailing garbage. Pin the back-compat contract so a future
  // refactor doesn't accidentally tighten these and break existing
  // deploys that happen to have whitespace-suffixed env values.
  test('PORT accepts "3000" → 3000', () => {
    captureFreshConfig({ PORT: '3000' }, (cfg) => {
      expect(cfg.PORT).toBe(3000);
    });
  });

  test('PORT lenient-parses "8080abc" → 8080 (no strictInteger flag)', () => {
    // Documents the lenient behavior — NOT a recommendation. New
    // tunables should pass strictInteger: true. Existing tunables
    // are pinned for back-compat.
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

// #1101 — the /qurl detect throttle. Defaults to QURL_SEND_COOLDOWN_MS so an
// unset knob is current behavior, but is independently tunable (detect is a
// deanonymization oracle; coupling its window to send cadence would let a
// send-cadence change silently re-tune the oracle).
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
        // Rejected non-positive → default (which is the resolved send value).
        expect(cfg.QURL_DETECT_COOLDOWN_MS).toBe(30000);
        expect(warns.some((w) => w.includes('QURL_DETECT_COOLDOWN_MS') && w.includes('must be > 0'))).toBe(true);
      },
    );
  });
});
