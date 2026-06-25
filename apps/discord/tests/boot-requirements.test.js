/**
 * Unit tests for src/boot-requirements.js. These lists drive the
 * fail-fast behavior of index.js — a regression here would either boot
 * prod with missing secrets or die on a spurious false-positive, so the
 * exact set per mode is pinned down here.
 *
 * NOTE: the helpers are parameterized on `isOpenNHPActive`, not
 * `isMultiTenant`. Single-guild-plain (GUILD_ID set, flag off) behaves
 * identically to multi-tenant here — neither mounts /auth or /webhook,
 * so neither needs GITHUB_* / BASE_URL.
 */

const {
  bootRequired,
  prodRequired,
  missingBootKeys,
  missingProdKeys,
  missingKekRequiredKeys,
  baseUrlHttpsProblem,
  missingEventShipperKeys,
  unsupportedRoleShipperCombo,
  unsupportedRoleResumeCombo,
  unsupportedRoleHotStandbyCombo,
  missingHotStandbyKeys,
  invalidHotStandbyValues,
  shouldRegisterInteractionListener,
  missingMapCommandKeys,
  GOOGLE_MAPS_API_KEY_PLACEHOLDER_SENTINEL,
  VALID_PROCESS_ROLES,
  resolveProcessRole,
} = require('../src/boot-requirements');

describe('bootRequired', () => {
  it('OpenNHP-active mode demands DISCORD_TOKEN + GITHUB_* (but not GUILD_ID or BASE_URL — those are enforced upstream)', () => {
    expect(bootRequired(true).sort()).toEqual([
      'DISCORD_TOKEN', 'GITHUB_CLIENT_ID',
      'GITHUB_CLIENT_SECRET', 'GITHUB_WEBHOOK_SECRET',
    ]);
  });

  it('non-OpenNHP modes (multi-tenant OR single-guild-plain) demand only DISCORD_TOKEN', () => {
    expect(bootRequired(false)).toEqual(['DISCORD_TOKEN']);
  });
});

describe('prodRequired', () => {
  it('OpenNHP prod requires QURL_API_KEY (global fallback)', () => {
    expect(prodRequired(true).sort()).toEqual([
      'KEY_ENCRYPTION_KEY', 'METRICS_TOKEN', 'QURL_API_KEY',
    ]);
  });

  it('non-OpenNHP prod omits QURL_API_KEY (per-guild via /qurl setup)', () => {
    expect(prodRequired(false).sort()).toEqual([
      'KEY_ENCRYPTION_KEY', 'METRICS_TOKEN',
    ]);
  });
});

describe('missingBootKeys', () => {
  it('returns empty when every boot key is present', () => {
    const cfg = {
      DISCORD_TOKEN: 't', GITHUB_CLIENT_ID: 'x', GITHUB_CLIENT_SECRET: 'x',
      GITHUB_WEBHOOK_SECRET: 'x', GUILD_ID: '123', BASE_URL: 'https://h',
    };
    expect(missingBootKeys(cfg, true)).toEqual([]);
    expect(missingBootKeys(cfg, false)).toEqual([]);
  });

  it('surfaces exact missing keys (not just a count) in OpenNHP mode', () => {
    const cfg = { DISCORD_TOKEN: 't' };
    expect(missingBootKeys(cfg, true).sort()).toEqual([
      'GITHUB_CLIENT_ID', 'GITHUB_CLIENT_SECRET', 'GITHUB_WEBHOOK_SECRET',
    ]);
  });

  it('only DISCORD_TOKEN missing triggers fail in non-OpenNHP mode', () => {
    expect(missingBootKeys({}, false)).toEqual(['DISCORD_TOKEN']);
  });

  it('does not flag OpenNHP-only keys as missing in non-OpenNHP mode', () => {
    // Single-guild-plain: GUILD_ID may be set but GITHUB_* unset — boot should not fail.
    expect(missingBootKeys({ DISCORD_TOKEN: 't', GUILD_ID: '123' }, false)).toEqual([]);
    // Multi-tenant: GUILD_ID unset, nothing else set — boot should not fail as long as DISCORD_TOKEN is there.
    expect(missingBootKeys({ DISCORD_TOKEN: 't' }, false)).toEqual([]);
  });

  it('treats empty strings as missing (not just undefined)', () => {
    const cfg = {
      DISCORD_TOKEN: '', GITHUB_CLIENT_ID: '', GITHUB_CLIENT_SECRET: 'x',
      GITHUB_WEBHOOK_SECRET: 'x',
    };
    expect(missingBootKeys(cfg, true).sort()).toEqual([
      'DISCORD_TOKEN', 'GITHUB_CLIENT_ID',
    ]);
  });
});

describe('missingProdKeys', () => {
  it('returns empty when every prod key is set in env', () => {
    const env = { METRICS_TOKEN: 'x', QURL_API_KEY: 'x', KEY_ENCRYPTION_KEY: 'x' };
    expect(missingProdKeys(env, true)).toEqual([]);
    expect(missingProdKeys(env, false)).toEqual([]);
  });

  it('does not demand QURL_API_KEY in non-OpenNHP prod', () => {
    const env = { METRICS_TOKEN: 'x', KEY_ENCRYPTION_KEY: 'x' };
    expect(missingProdKeys(env, false)).toEqual([]);
    // But it IS required in OpenNHP mode
    expect(missingProdKeys(env, true)).toEqual(['QURL_API_KEY']);
  });

  it('surfaces missing encryption key loudly — no silent fallback possible', () => {
    const env = { METRICS_TOKEN: 'x' };
    expect(missingProdKeys(env, false)).toEqual(['KEY_ENCRYPTION_KEY']);
    expect(missingProdKeys(env, true).sort()).toEqual([
      'KEY_ENCRYPTION_KEY', 'QURL_API_KEY',
    ]);
  });
});

describe('missingKekRequiredKeys', () => {
  it('returns empty when GITHUB_CLIENT_SECRET is unset (no token-issuing surface, no KEK demand)', () => {
    expect(missingKekRequiredKeys({})).toEqual([]);
    // Empty string also counts as unset — same as missingBootKeys's robustness.
    expect(missingKekRequiredKeys({ GITHUB_CLIENT_SECRET: '' })).toEqual([]);
  });

  it('flags KEY_ENCRYPTION_KEY when GITHUB_CLIENT_SECRET is set without KEK', () => {
    expect(missingKekRequiredKeys({ GITHUB_CLIENT_SECRET: 'x' })).toEqual(['KEY_ENCRYPTION_KEY']);
    // Empty-string KEK counts as missing (matches the
    // boot-requirements `!env[k]` falsy treatment elsewhere).
    expect(
      missingKekRequiredKeys({ GITHUB_CLIENT_SECRET: 'x', KEY_ENCRYPTION_KEY: '' })
    ).toEqual(['KEY_ENCRYPTION_KEY']);
  });

  it('returns empty when both are set, regardless of NODE_ENV', () => {
    // Independent of NODE_ENV by design — staging/preview deploys with
    // a real GitHub client secret must satisfy this gate even though
    // missingProdKeys does not run for them.
    expect(
      missingKekRequiredKeys({ GITHUB_CLIENT_SECRET: 'x', KEY_ENCRYPTION_KEY: 'k' })
    ).toEqual([]);
  });
});

describe('baseUrlHttpsProblem', () => {
  // Mirrors the shape index.js passes: a parsed config object (BASE_URL is
  // already defaulted to http://localhost:3000 in config.js when unset),
  // plus the caller-computed `baseUrlExplicitlySet` boolean. Default cfg is
  // a plain non-consuming deploy with BASE_URL unset (the localhost
  // fallback); each test overrides the mode flags / BASE_URL it exercises.
  const LOCALHOST = 'http://localhost:3000'; // config.js BASE_URL default
  function cfg(overrides = {}) {
    return {
      isQurlOAuthConfigured: false,
      BASE_URL: LOCALHOST,
      ...overrides,
    };
  }

  it('accepts a bare https:// BASE_URL origin (the good prod case)', () => {
    const HTTPS = 'https://bot.example.com';
    expect(baseUrlHttpsProblem(cfg({ BASE_URL: HTTPS }), true)).toBeNull();
    expect(baseUrlHttpsProblem(cfg({ isQurlOAuthConfigured: true, BASE_URL: HTTPS }), true)).toBeNull();
  });

  it('accepts an uppercase HTTPS:// scheme (URL scheme is case-insensitive)', () => {
    // The parse-based check normalizes the scheme, so a valid HTTPS:// origin
    // isn't falsely rejected at boot (the pre-#619 prefix check was case-sensitive).
    expect(
      baseUrlHttpsProblem(cfg({ isQurlOAuthConfigured: true, BASE_URL: 'HTTPS://bot.example.com' }), true),
    ).toBeNull();
  });

  it('rejects a host-less "https://" BASE_URL (would build a broken redirect)', () => {
    // new URL('https://') throws — a scheme with no host can't be a usable
    // redirect base, so a consuming deploy must still fail fast rather than
    // pass a prefix check.
    const msg = baseUrlHttpsProblem(cfg({ isQurlOAuthConfigured: true, BASE_URL: 'https://' }), true);
    expect(msg).not.toBeNull();
    expect(msg).toContain('https://');
  });

  it('rejects qURL OAuth configured + BASE_URL with path/query/fragment/userinfo', () => {
    for (const bad of [
      'https://bot.example.com/prefix',
      'https://bot.example.com?debug=true',
      'https://bot.example.com#callback',
    ]) {
      const msg = baseUrlHttpsProblem(cfg({ isQurlOAuthConfigured: true, BASE_URL: bad }), true);
      expect(msg).not.toBeNull();
      expect(msg).toContain('public bare https:// origin');
      expect(msg).toContain(bad);
    }
  });

  it('redacts BASE_URL userinfo from boot errors', () => {
    const msg = baseUrlHttpsProblem(
      cfg({ isQurlOAuthConfigured: true, BASE_URL: 'https://user:pass@bot.example.com' }),
      true,
    );
    expect(msg).not.toBeNull();
    expect(msg).toContain('public bare https:// origin');
    expect(msg).toContain('https://bot.example.com/');
    expect(msg).not.toContain('user:pass');
  });

  it('rejects qURL OAuth configured + local-only BASE_URL host literals', () => {
    for (const bad of [
      'https://localhost',
      'https://bot.localhost',
      'https://127.0.0.1',
      'https://10.0.3.4',
      'https://172.16.0.2',
      'https://192.168.1.20',
      'https://[::1]',
    ]) {
      const msg = baseUrlHttpsProblem(cfg({ isQurlOAuthConfigured: true, BASE_URL: bad }), true);
      expect(msg).not.toBeNull();
      expect(msg).toContain('public bare https:// origin');
      expect(msg).toContain(bad);
    }
  });

  // The #619 headline regression: a deploy with the qURL OAuth setup flow
  // configured (AUTH0_* set) but BASE_URL left unset silently falls back to
  // localhost and dead-ends /qurl setup at the OAuth redirect. Boot must
  // reject — fail-fast at deploy, not at setup time.
  it('rejects qURL OAuth configured + BASE_URL unset (localhost fallback)', () => {
    const msg = baseUrlHttpsProblem(
      cfg({ isQurlOAuthConfigured: true, BASE_URL: LOCALHOST }),
      false, // not explicitly set — fell back to the localhost default
    );
    expect(msg).not.toBeNull();
    expect(msg).toMatch(/BASE_URL/);
    expect(msg).toMatch(/https:\/\//);
    expect(msg).toMatch(/qURL/);
    // Echoes the offending value so an operator pasting the log line sees it.
    expect(msg).toContain(LOCALHOST);
  });

  // The non-consuming false-positive guard: a plain single-guild / multi-
  // tenant deploy with no OAuth surface ignores BASE_URL, so the localhost
  // default (unset, not explicitly set) must NOT fail boot.
  it('does not false-positive a non-consuming deploy with BASE_URL unset', () => {
    expect(baseUrlHttpsProblem(cfg({ BASE_URL: LOCALHOST }), false)).toBeNull();
  });

  // ...but a stale explicit http:// value in a non-consuming deploy is still
  // rejected — the original canary, in case a future code path re-enables use.
  it('rejects a stale explicit http:// BASE_URL even when no surface consumes it', () => {
    const msg = baseUrlHttpsProblem(cfg({ BASE_URL: 'http://stale.example.com' }), true);
    expect(msg).not.toBeNull();
    expect(msg).toContain('http://stale.example.com');
  });

  // The two non-https rejections carry distinct messages: the consuming-
  // surface message explains why BASE_URL matters (operator remediation);
  // the non-consuming path keeps the terse legacy canary message. Pin the
  // distinction so a refactor can't collapse them and strip the guidance.
  it('uses the OAuth-aware message for consuming surfaces and the terse canary otherwise', () => {
    const consuming = baseUrlHttpsProblem(cfg({ isQurlOAuthConfigured: true, BASE_URL: LOCALHOST }), false);
    const canary = baseUrlHttpsProblem(cfg({ BASE_URL: 'http://x.example.com' }), true);
    expect(consuming).toMatch(/OAuth redirect/);
    expect(canary).not.toMatch(/OAuth redirect/);
    expect(canary).toMatch(/BASE_URL must use https:\/\/ in production/);
  });
});

describe('missingEventShipperKeys', () => {
  it('returns empty when the flag is unset (event-shipper path inactive)', () => {
    expect(missingEventShipperKeys({})).toEqual([]);
    expect(missingEventShipperKeys({ ENABLE_EVENT_SHIPPER: false })).toEqual([]);
    // Even with a missing queue URL — the flag is the gate, not the URL.
    expect(
      missingEventShipperKeys({ ENABLE_EVENT_SHIPPER: false, QURL_BOT_EVENTS_QUEUE_URL: undefined })
    ).toEqual([]);
  });

  it('flags QURL_BOT_EVENTS_QUEUE_URL when flag is on without a URL', () => {
    expect(
      missingEventShipperKeys({ ENABLE_EVENT_SHIPPER: true })
    ).toEqual(['QURL_BOT_EVENTS_QUEUE_URL']);
    // Empty string counts as missing — matches the `!env[k]` falsy
    // treatment elsewhere in this module.
    expect(
      missingEventShipperKeys({ ENABLE_EVENT_SHIPPER: true, QURL_BOT_EVENTS_QUEUE_URL: '' })
    ).toEqual(['QURL_BOT_EVENTS_QUEUE_URL']);
  });

  it('returns empty when both are set', () => {
    expect(
      missingEventShipperKeys({
        ENABLE_EVENT_SHIPPER: true,
        QURL_BOT_EVENTS_QUEUE_URL: 'https://sqs.us-east-2.amazonaws.com/123/qurl-bot-events',
      }),
    ).toEqual([]);
  });
});

describe('missingViewUpdatePushKeys', () => {
  const { missingViewUpdatePushKeys } = require('../src/boot-requirements');

  it('returns empty when the flag is unset (view-update-push path inactive)', () => {
    expect(missingViewUpdatePushKeys({})).toEqual([]);
    expect(missingViewUpdatePushKeys({ ENABLE_VIEW_UPDATE_PUSH: false })).toEqual([]);
    expect(
      missingViewUpdatePushKeys({ ENABLE_VIEW_UPDATE_PUSH: false, QURL_BOT_VIEW_UPDATES_QUEUE_URL: undefined })
    ).toEqual([]);
  });

  it('flags QURL_BOT_VIEW_UPDATES_QUEUE_URL when flag is on without a URL', () => {
    expect(
      missingViewUpdatePushKeys({ ENABLE_VIEW_UPDATE_PUSH: true })
    ).toEqual(['QURL_BOT_VIEW_UPDATES_QUEUE_URL']);
    expect(
      missingViewUpdatePushKeys({ ENABLE_VIEW_UPDATE_PUSH: true, QURL_BOT_VIEW_UPDATES_QUEUE_URL: '' })
    ).toEqual(['QURL_BOT_VIEW_UPDATES_QUEUE_URL']);
  });

  it('returns empty when both are set', () => {
    expect(
      missingViewUpdatePushKeys({
        ENABLE_VIEW_UPDATE_PUSH: true,
        QURL_BOT_VIEW_UPDATES_QUEUE_URL: 'https://sqs.us-east-2.amazonaws.com/123/qurl-bot-view-updates',
      }),
    ).toEqual([]);
  });

  // Pin the asymmetric "URL set without the flag" case. During the
  // flag-flip rollout, infra changes may land the URL on the task
  // def BEFORE the flag flips — the boot check must treat that as
  // a harmlessly-ignored config.
  it('returns empty when URL is set but flag is off (mid-rollout shape)', () => {
    expect(
      missingViewUpdatePushKeys({
        ENABLE_VIEW_UPDATE_PUSH: false,
        QURL_BOT_VIEW_UPDATES_QUEUE_URL: 'https://sqs.us-east-2.amazonaws.com/123/qurl-bot-view-updates',
      }),
    ).toEqual([]);
  });
});

// Pin the contract that combined-mode + view-update-push
// is intentionally accepted (no analog of `unsupportedRoleShipperCombo`).
// Locks the design so a copy-paste-from-shipper refactor doesn't silently
// add a rejection.
describe('combined-mode + view-update-push (no equivalent of unsupportedRoleShipperCombo)', () => {
  it('boot-requirements exports no view-update combined-mode rejector', () => {
    // The shipper has unsupportedRoleShipperCombo for combined-mode
    // rejection. The view-update-push intentionally has no such
    // helper — the registry's silent-drop-on-miss + status==='opened'
    // idempotency guard make combined-mode safe.
    const bootReq = require('../src/boot-requirements');
    expect(bootReq.unsupportedRoleShipperCombo).toBeDefined();
    expect(bootReq.unsupportedRoleViewUpdatePushCombo).toBeUndefined();
  });
});

describe('unsupportedRoleShipperCombo', () => {
  it('rejects combined + flag-on with operator-facing remediation', () => {
    const msg = unsupportedRoleShipperCombo('combined', true);
    expect(msg).not.toBeNull();
    // Pin the message contract so a wording drift can't silently
    // strip the remediation hint operators rely on to fix the deploy.
    expect(msg).toMatch(/PROCESS_ROLE=combined/);
    expect(msg).toMatch(/ENABLE_EVENT_SHIPPER=true/);
    expect(msg).toMatch(/PROCESS_ROLE=gateway/);
    expect(msg).toMatch(/PROCESS_ROLE=http/);
  });

  it.each([
    ['gateway', true],
    ['http', true],
    ['combined', false],
    ['gateway', false],
    ['http', false],
  ])('returns null for supported combination role=%s shipper=%s', (role, shipperEnabled) => {
    expect(unsupportedRoleShipperCombo(role, shipperEnabled)).toBeNull();
  });
});

describe('unsupportedRoleResumeCombo', () => {
  it('returns null when resume=false regardless of other inputs', () => {
    // Flag-off is the legacy path — every (role, shipper, storeType)
    // combination is supported (or rejected by
    // unsupportedRoleShipperCombo). Pin every input as null so a
    // future shape change can't accidentally start rejecting legacy
    // deploys.
    for (const role of ['combined', 'gateway', 'http']) {
      for (const shipper of [true, false]) {
        for (const storeType of ['sqlite', 'ddb']) {
          expect(unsupportedRoleResumeCombo(role, false, shipper, storeType)).toBeNull();
        }
      }
    }
  });

  it('rejects resume=true with combined role and surfaces shim/Client conflict', () => {
    // combined+resume is rejected ahead of every other check because
    // combined mode is the higher-order failure. Pin the message
    // contract so a future wording drift can't strip the operator
    // remediation hint.
    const msg = unsupportedRoleResumeCombo('combined', true, true, 'ddb');
    expect(msg).not.toBeNull();
    expect(msg).toMatch(/PROCESS_ROLE=combined/);
    expect(msg).toMatch(/ENABLE_GATEWAY_RESUME=true/);
    expect(msg).toMatch(/PROCESS_ROLE=gateway/);
    expect(msg).toMatch(/PROCESS_ROLE=http/);
    // combined-mode check is sequenced first so it dominates the
    // shipper/store-type rejections.
    expect(unsupportedRoleResumeCombo('combined', true, false, 'sqlite')).toBe(msg);
  });

  it('rejects resume=true with shipper=false on supported roles', () => {
    for (const role of ['gateway', 'http']) {
      const msg = unsupportedRoleResumeCombo(role, true, false, 'ddb');
      expect(msg).not.toBeNull();
      expect(msg).toMatch(/ENABLE_GATEWAY_RESUME=true requires ENABLE_EVENT_SHIPPER=true/);
      // Role-neutral framing: the rejection should not mention
      // gateway-tier-specific implementation details (the shim is
      // never constructed on http, so an http operator reading the
      // message wouldn't see "the shim replaces Client" verbiage).
      expect(msg).not.toMatch(/replaces discord\.js Client/);
      expect(msg).not.toMatch(/@discordjs\/ws/);
    }
  });

  it('rejects resume=true with non-ddb storeType (would lose state across processes)', () => {
    // A non-ddb backend (no such backend is supported today after the
    // SQLite removal; this branch is defense-in-depth for a future
    // backend addition) lacks the cross-process visibility the next
    // ECS task needs. Without rejecting at boot, the bot would
    // silently IDENTIFY on every restart — mimicking flag-off
    // behavior and burning Discord's per-bot IDENTIFY budget.
    // We pass the literal string 'sqlite' here because it surfaces
    // the most realistic regression — an operator carrying over an
    // env file from before the DDB-only world.
    //
    // Unreachable-in-real-boot caveat: `src/store/index.js`'s
    // validator throws on any non-`ddb` STORE_TYPE before this
    // function ever sees the value (config.STORE_TYPE === 'ddb' is
    // the only outcome of a successful boot). The test exercises
    // the function in isolation so the defense-in-depth message
    // contract stays pinned for a hypothetical future backend
    // addition that updates VALID_BACKENDS without thinking through
    // the cross-process resume semantics. Do not "simplify" by
    // deleting this test — it's intentionally dead-defense coverage.
    const msg = unsupportedRoleResumeCombo('gateway', true, true, 'sqlite');
    expect(msg).not.toBeNull();
    expect(msg).toMatch(/STORE_TYPE=ddb/);
    expect(msg).toMatch(/gateway-session/);
    expect(msg).toMatch(/'sqlite'/);
  });

  it('returns null for the production-shape path (gateway + shipper + ddb)', () => {
    expect(unsupportedRoleResumeCombo('gateway', true, true, 'ddb')).toBeNull();
    // http tier accepts resume=true as a no-op so the operator can
    // ship one task-def with uniform env across both tiers.
    expect(unsupportedRoleResumeCombo('http', true, true, 'ddb')).toBeNull();
  });
});

describe('shouldRegisterInteractionListener', () => {
  // The full 3 roles × 2 flag states truth table. Combined + flag-on
  // is rejected at boot (unsupportedRoleShipperCombo), so its
  // intended-behavior is "unreachable in production." We still pin
  // the predicate's output for that input so a future caller that
  // bypasses the boot guard sees a coherent value.
  //
  // The mapping is derived via resolveProcessRole semantics:
  //   combined → isGateway=true, isHttp=true
  //   gateway  → isGateway=true, isHttp=false
  //   http     → isGateway=false, isHttp=true
  test.each([
    // [role,       flag,  expected, rationale]
    ['combined',   false, true,  'legacy in-process; local listener handles dispatch'],
    ['combined',   true,  true,  'unreachable in prod (boot reject); predicate output coherent'],
    ['gateway',    false, true,  'legacy in-process gateway tier (single-process deploy)'],
    ['gateway',    true,  false, 'gateway tier publishes to SQS; local listener disconnected'],
    ['http',       false, false, 'no gateway WS + no SQS consumer; listener would never fire'],
    ['http',       true,  true,  'worker tier; SQS consumer re-emits, listener routes'],
  ])('role=%s flag=%s → %s (%s)', (role, eventShipperEnabled, expected) => {
    const { isGateway, isHttp } = resolveProcessRole(role);
    expect(shouldRegisterInteractionListener({ isGateway, isHttp, eventShipperEnabled })).toBe(expected);
  });

  it('is a pure function (no side effects, same input → same output)', () => {
    // Predicate must be referentially transparent — a side effect
    // would couple boot to non-determinism and undermine the
    // unit-testability that motivated the lift.
    const args = { isGateway: true, isHttp: false, eventShipperEnabled: true };
    const first = shouldRegisterInteractionListener(args);
    const second = shouldRegisterInteractionListener(args);
    expect(first).toBe(second);
  });
});

describe('missingMapCommandKeys', () => {
  it('returns empty when the flag is off — Maps key state is irrelevant', () => {
    expect(missingMapCommandKeys({})).toEqual([]);
    expect(missingMapCommandKeys({ MAP_COMMAND_ENABLED: false })).toEqual([]);
    // Even with a missing or PLACEHOLDER key — the toggle is the gate.
    // No /qurl map registration means no Places call possible.
    expect(
      missingMapCommandKeys({ MAP_COMMAND_ENABLED: false, GOOGLE_MAPS_API_KEY: '' }),
    ).toEqual([]);
    expect(
      missingMapCommandKeys({
        MAP_COMMAND_ENABLED: false,
        GOOGLE_MAPS_API_KEY: GOOGLE_MAPS_API_KEY_PLACEHOLDER_SENTINEL,
      }),
    ).toEqual([]);
  });

  it('flags GOOGLE_MAPS_API_KEY when toggle is on but the key is missing', () => {
    expect(
      missingMapCommandKeys({ MAP_COMMAND_ENABLED: true }),
    ).toEqual(['GOOGLE_MAPS_API_KEY']);
    expect(
      missingMapCommandKeys({ MAP_COMMAND_ENABLED: true, GOOGLE_MAPS_API_KEY: '' }),
    ).toEqual(['GOOGLE_MAPS_API_KEY']);
  });

  it('flags GOOGLE_MAPS_API_KEY when toggle is on but the key is still the PLACEHOLDER sentinel', () => {
    // The exact regression that triggered the toggle PR: the SSM
    // parameter shipped as the literal "PLACEHOLDER" value in both
    // sandbox + prod accounts. Without this branch, an operator
    // flipping the toggle without seeding the SSM secret would boot
    // successfully and fail at the first /qurl map invocation.
    expect(
      missingMapCommandKeys({
        MAP_COMMAND_ENABLED: true,
        GOOGLE_MAPS_API_KEY: GOOGLE_MAPS_API_KEY_PLACEHOLDER_SENTINEL,
      }),
    ).toEqual(['GOOGLE_MAPS_API_KEY']);
  });

  it('returns empty when toggle is on AND the key is a real value', () => {
    expect(
      missingMapCommandKeys({
        MAP_COMMAND_ENABLED: true,
        GOOGLE_MAPS_API_KEY: 'AIzaSyA-real-looking-key-1234567890',
      }),
    ).toEqual([]);
  });
});

describe('resolveProcessRole', () => {
  it('VALID_PROCESS_ROLES is the canonical set in stable order (combined first as the default)', () => {
    expect(VALID_PROCESS_ROLES).toEqual(['combined', 'gateway', 'http']);
    expect(Object.isFrozen(VALID_PROCESS_ROLES)).toBe(true);
  });

  it.each([
    ['combined', { role: 'combined', isGateway: true, isHttp: true }],
    ['gateway', { role: 'gateway', isGateway: true, isHttp: false }],
    ['http', { role: 'http', isGateway: false, isHttp: true }],
  ])('resolves %s to expected role flags', (input, expected) => {
    expect(resolveProcessRole(input)).toEqual(expected);
  });

  it.each([undefined, null, '', '   ', '\t'])(
    'falls back to combined for unset / whitespace-only value (%p)',
    (input) => {
      expect(resolveProcessRole(input)).toEqual({
        role: 'combined', isGateway: true, isHttp: true,
      });
    }
  );

  it('trims surrounding whitespace before validating', () => {
    expect(resolveProcessRole('  http  ')).toEqual({
      role: 'http', isGateway: false, isHttp: true,
    });
  });

  it('throws on unknown role with INVALID_PROCESS_ROLE code (so index.js can exit(1))', () => {
    let caught;
    try {
      resolveProcessRole('gatewayy');
    } catch (e) {
      caught = e;
    }
    expect(caught).toBeInstanceOf(Error);
    expect(caught.code).toBe('INVALID_PROCESS_ROLE');
    // Message names the bad value AND lists the valid ones — operators
    // pasting the log line into a ticket should see both immediately.
    expect(caught.message).toMatch(/'gatewayy'/);
    expect(caught.message).toMatch(/combined, gateway, http/);
  });

  it('rejects case-variant roles (no silent normalization)', () => {
    // SQLITE-vs-sqlite parity: an env-templating bug that produces
    // 'GATEWAY' should fail loud, not silently coerce.
    expect(() => resolveProcessRole('GATEWAY')).toThrow(/GATEWAY/);
    expect(() => resolveProcessRole('Combined')).toThrow(/Combined/);
  });
});

describe('unsupportedRoleHotStandbyCombo', () => {
  it('returns null when hot-standby=false regardless of other inputs', () => {
    for (const role of ['combined', 'gateway', 'http']) {
      for (const resume of [true, false]) {
        expect(unsupportedRoleHotStandbyCombo(role, false, resume)).toBeNull();
      }
    }
  });

  it('rejects hot-standby=true on combined role with operator-facing remediation', () => {
    const msg = unsupportedRoleHotStandbyCombo('combined', true, true);
    expect(msg).not.toBeNull();
    expect(msg).toMatch(/ENABLE_GATEWAY_HOT_STANDBY=true/);
    expect(msg).toMatch(/PROCESS_ROLE=gateway/);
    // Names the actual role so an operator pasting the log line into
    // a ticket sees their misconfiguration immediately.
    expect(msg).toMatch(/'combined'/);
  });

  it('rejects hot-standby=true on http role', () => {
    const msg = unsupportedRoleHotStandbyCombo('http', true, true);
    expect(msg).not.toBeNull();
    expect(msg).toMatch(/'http'/);
    expect(msg).toMatch(/no manager to hand off/);
  });

  it('rejects hot-standby=true with resume=false (would session-flap)', () => {
    // The push-handoff path adopts the outgoing leader's
    // session_id+sequence on the standby — without RESUME, the
    // standby would IDENTIFY against the same token and Discord
    // would flap the session identity.
    const msg = unsupportedRoleHotStandbyCombo('gateway', true, false);
    expect(msg).not.toBeNull();
    expect(msg).toMatch(/requires ENABLE_GATEWAY_RESUME=true/);
    expect(msg).toMatch(/flap the session/);
  });

  it('returns null on the supported combo (gateway + resume + hot-standby)', () => {
    expect(unsupportedRoleHotStandbyCombo('gateway', true, true)).toBeNull();
  });

  it('sequences role check before resume check (operator sees the dominant fix first)', () => {
    // combined+hot-standby+resume-off is doubly broken. The role
    // check fires first because PROCESS_ROLE is the higher-order
    // misconfig — fixing the role + leaving resume off would leave
    // the second rejection still firing; fixing resume + leaving
    // combined would re-fire the role rejection. Sequencing the
    // role check first means the first redeploy lands the higher-
    // order fix.
    const msg = unsupportedRoleHotStandbyCombo('combined', true, false);
    expect(msg).toMatch(/PROCESS_ROLE=gateway/);
    expect(msg).not.toMatch(/ENABLE_GATEWAY_RESUME=true/);
  });
});

describe('missingHotStandbyKeys', () => {
  function cfg(overrides = {}) {
    return {
      ENABLE_GATEWAY_HOT_STANDBY: true,
      INSTANCE_ID: 'task-abc-123',
      INSTANCE_IP: '10.0.1.42',
      hasGatewayHandoffHmac: true,
      ...overrides,
    };
  }

  it('returns empty when the flag is off (no requirements)', () => {
    expect(missingHotStandbyKeys({
      ENABLE_GATEWAY_HOT_STANDBY: false,
      // Everything else unset — must still be empty.
    })).toEqual([]);
  });

  it('returns empty when every required key is present', () => {
    expect(missingHotStandbyKeys(cfg())).toEqual([]);
  });

  it('surfaces missing INSTANCE_ID', () => {
    expect(missingHotStandbyKeys(cfg({ INSTANCE_ID: undefined }))).toEqual(['INSTANCE_ID']);
  });

  it('surfaces missing INSTANCE_IP', () => {
    expect(missingHotStandbyKeys(cfg({ INSTANCE_IP: null }))).toEqual(['INSTANCE_IP']);
  });

  it('surfaces missing GATEWAY_HANDOFF_HMAC (via hasGatewayHandoffHmac flag)', () => {
    // The presence check reads `hasGatewayHandoffHmac` (boolean) rather
    // than the raw secret string — see config.js's
    // `takeGatewayHandoffHmac` for the heap-dump security rationale.
    expect(missingHotStandbyKeys(cfg({ hasGatewayHandoffHmac: false })))
      .toEqual(['GATEWAY_HANDOFF_HMAC']);
  });

  it('returns every missing key (not just the first) for one-shot remediation', () => {
    // Order is the function's natural push order (INSTANCE_ID,
    // INSTANCE_IP, GATEWAY_HANDOFF_HMAC); pinning it documents that
    // contract so a refactor that reorders the pushes flags the
    // operator-facing log-message ordering as a change.
    const missing = missingHotStandbyKeys(cfg({
      INSTANCE_ID: undefined,
      INSTANCE_IP: undefined,
      hasGatewayHandoffHmac: false,
    }));
    expect(missing).toEqual(['INSTANCE_ID', 'INSTANCE_IP', 'GATEWAY_HANDOFF_HMAC']);
  });
});

describe('invalidHotStandbyValues', () => {
  function cfg(overrides = {}) {
    return {
      ENABLE_GATEWAY_HOT_STANDBY: true,
      INSTANCE_ID: 'task-abc-123',
      INSTANCE_IP: '10.0.1.42',
      ...overrides,
    };
  }

  it('returns empty when the flag is off (no shape requirements)', () => {
    expect(invalidHotStandbyValues({
      ENABLE_GATEWAY_HOT_STANDBY: false,
      INSTANCE_ID: '${LITERALLY_UNRESOLVED}',
      INSTANCE_IP: 'not-an-ip',
    })).toEqual([]);
  });

  it('returns empty when both values are well-shaped', () => {
    expect(invalidHotStandbyValues(cfg())).toEqual([]);
  });

  it('flags unsubstituted template literal in INSTANCE_ID — env-override paste footgun', () => {
    // The specific scenario: an operator sets
    // `INSTANCE_ID=${ECS_TASK_ARN}` as an env override (e.g. pasted
    // from a runbook) and the surrounding shell fails to expand it.
    // Without this check the literal `${ECS_TASK_ARN}` would key the
    // lock + heartbeat rows and every replica would think it owns
    // the same identifier.
    const problems = invalidHotStandbyValues(cfg({ INSTANCE_ID: '${ECS_TASK_ARN}' }));
    expect(problems).toHaveLength(1);
    expect(problems[0]).toMatch(/INSTANCE_ID looks like an unsubstituted template literal/);
    expect(problems[0]).toContain('${ECS_TASK_ARN}');
  });

  it('flags non-IPv4 INSTANCE_IP (string)', () => {
    const problems = invalidHotStandbyValues(cfg({ INSTANCE_IP: 'not-an-ip' }));
    expect(problems).toHaveLength(1);
    expect(problems[0]).toMatch(/INSTANCE_IP must be a valid IPv4 address/);
    expect(problems[0]).toContain("'not-an-ip'");
  });

  it('flags out-of-range octets in INSTANCE_IP (10.0.0.999)', () => {
    const problems = invalidHotStandbyValues(cfg({ INSTANCE_IP: '10.0.0.999' }));
    expect(problems).toHaveLength(1);
    expect(problems[0]).toContain('10.0.0.999');
  });

  it('flags IPv6 INSTANCE_IP (out of scope for Pillar 3)', () => {
    const problems = invalidHotStandbyValues(cfg({ INSTANCE_IP: '::1' }));
    expect(problems).toHaveLength(1);
    expect(problems[0]).toMatch(/must be a valid IPv4/);
  });

  it('flags link-local INSTANCE_IP (paste-error from ECS metadata endpoint URL)', () => {
    // Mirrors deriveInstanceIp's link-local filter — without this,
    // an operator pasting `169.254.172.2` (the ECS task metadata
    // endpoint) into INSTANCE_IP would slip past the shape check
    // and re-introduce the Pillar 3 push-handoff bug through the
    // env override path.
    const problems = invalidHotStandbyValues(cfg({ INSTANCE_IP: '169.254.172.2' }));
    expect(problems).toHaveLength(1);
    expect(problems[0]).toMatch(/link-local/);
    expect(problems[0]).toContain("'169.254.172.2'");
  });

  it('flags leading-zero IPv4 octets (octal-parse hazard under some resolvers)', () => {
    // `01.02.03.04` parses as octal under glibc's `inet_aton` and a
    // handful of resolvers — a typo'd "010.0.0.1" would resolve as
    // 8.0.0.1, silently routing the control channel to a wrong host.
    // The closed-door fix is to require canonical no-leading-zero
    // octets, which the ECS task-def injection always produces.
    for (const ip of ['01.0.0.1', '10.01.0.1', '10.0.01.1', '10.0.0.01']) {
      const problems = invalidHotStandbyValues(cfg({ INSTANCE_IP: ip }));
      expect(problems).toHaveLength(1);
      expect(problems[0]).toMatch(/must be a valid IPv4/);
    }
  });

  it('accepts every octet boundary (0, 9, 10, 99, 100, 199, 200, 249, 255)', () => {
    const ips = ['0.0.0.0', '255.255.255.255', '10.99.100.249', '1.9.199.200'];
    for (const ip of ips) {
      expect(invalidHotStandbyValues(cfg({ INSTANCE_IP: ip }))).toEqual([]);
    }
  });

  it('reports every problem on a single call (one-shot operator remediation)', () => {
    const problems = invalidHotStandbyValues(cfg({
      INSTANCE_ID: '${ECS_TASK_ARN}',
      INSTANCE_IP: '999.999.999.999',
    }));
    expect(problems).toHaveLength(2);
  });

  it('does not trip on present-but-empty INSTANCE_IP (the missingHotStandbyKeys check catches that)', () => {
    // Separation of concerns: missing-key checks are presence-only,
    // shape checks only run on present values. An empty string is
    // "missing" from the perspective of the upstream check and is
    // skipped here to avoid duplicate operator-facing messages.
    expect(invalidHotStandbyValues(cfg({ INSTANCE_IP: '' }))).toEqual([]);
    expect(invalidHotStandbyValues(cfg({ INSTANCE_ID: '' }))).toEqual([]);
  });
});
