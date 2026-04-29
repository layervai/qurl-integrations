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
