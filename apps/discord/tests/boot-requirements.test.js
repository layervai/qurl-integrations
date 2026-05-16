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
  missingEventShipperKeys,
  unsupportedRoleShipperCombo,
  unsupportedRoleResumeCombo,
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
        for (const storeType of ['sqlite', 'ddb', undefined]) {
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
      expect(msg).toMatch(/@discordjs\/ws/);
    }
  });

  it('rejects resume=true with storeType!=ddb (sqlite would lose state across processes)', () => {
    // Default sqlite backend writes a local file that the next ECS
    // task can't see. Without rejecting at boot, the bot would
    // silently IDENTIFY on every restart — mimicking flag-off
    // behavior and burning Discord's per-bot IDENTIFY budget.
    for (const storeType of ['sqlite', undefined, '']) {
      const msg = unsupportedRoleResumeCombo('gateway', true, true, storeType);
      expect(msg).not.toBeNull();
      expect(msg).toMatch(/STORE_TYPE=ddb/);
      expect(msg).toMatch(/gateway-session/);
    }
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
