/**
 * Unit tests for src/boot-requirements.js. These lists drive the
 * fail-fast behavior of index.js — a regression here would either boot
 * prod with missing secrets or die on a spurious false-positive, so the
 * exact set per mode is pinned down here.
 */

const {
  bootRequired,
  prodRequired,
  missingBootKeys,
  missingProdKeys,
} = require('../src/boot-requirements');

describe('bootRequired', () => {
  it('single-guild mode demands the OpenNHP-linking env vars', () => {
    expect(bootRequired(false).sort()).toEqual([
      'BASE_URL', 'DISCORD_TOKEN', 'GITHUB_CLIENT_ID',
      'GITHUB_CLIENT_SECRET', 'GITHUB_WEBHOOK_SECRET', 'GUILD_ID',
    ]);
  });

  it('multi-tenant mode demands only DISCORD_TOKEN', () => {
    expect(bootRequired(true)).toEqual(['DISCORD_TOKEN']);
  });
});

describe('prodRequired', () => {
  it('single-guild prod requires QURL_API_KEY (global fallback)', () => {
    expect(prodRequired(false).sort()).toEqual([
      'KEY_ENCRYPTION_KEY', 'METRICS_TOKEN', 'QURL_API_KEY',
    ]);
  });

  it('multi-tenant prod omits QURL_API_KEY (per-guild via /qurl setup)', () => {
    expect(prodRequired(true).sort()).toEqual([
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
    expect(missingBootKeys(cfg, false)).toEqual([]);
    expect(missingBootKeys(cfg, true)).toEqual([]);
  });

  it('surfaces exact missing keys (not just a count)', () => {
    const cfg = { DISCORD_TOKEN: 't', GUILD_ID: '123' };
    expect(missingBootKeys(cfg, false).sort()).toEqual([
      'BASE_URL', 'GITHUB_CLIENT_ID', 'GITHUB_CLIENT_SECRET', 'GITHUB_WEBHOOK_SECRET',
    ]);
  });

  it('treats multi-tenant-only missing DISCORD_TOKEN correctly', () => {
    expect(missingBootKeys({}, true)).toEqual(['DISCORD_TOKEN']);
  });

  it('does not flag OpenNHP-only keys as missing in multi-tenant mode', () => {
    expect(missingBootKeys({ DISCORD_TOKEN: 't' }, true)).toEqual([]);
  });

  it('treats empty strings as missing (not just undefined)', () => {
    const cfg = {
      DISCORD_TOKEN: '', GITHUB_CLIENT_ID: '', GITHUB_CLIENT_SECRET: 'x',
      GITHUB_WEBHOOK_SECRET: 'x', GUILD_ID: '123', BASE_URL: 'https://h',
    };
    expect(missingBootKeys(cfg, false).sort()).toEqual([
      'DISCORD_TOKEN', 'GITHUB_CLIENT_ID',
    ]);
  });
});

describe('missingProdKeys', () => {
  it('returns empty when every prod key is set in env', () => {
    const env = { METRICS_TOKEN: 'x', QURL_API_KEY: 'x', KEY_ENCRYPTION_KEY: 'x' };
    expect(missingProdKeys(env, false)).toEqual([]);
    expect(missingProdKeys(env, true)).toEqual([]);
  });

  it('does not demand QURL_API_KEY in multi-tenant prod', () => {
    const env = { METRICS_TOKEN: 'x', KEY_ENCRYPTION_KEY: 'x' };
    expect(missingProdKeys(env, true)).toEqual([]);
    // But it IS required in single-guild
    expect(missingProdKeys(env, false)).toEqual(['QURL_API_KEY']);
  });

  it('surfaces missing encryption key loudly — no silent fallback possible', () => {
    const env = { METRICS_TOKEN: 'x' };
    expect(missingProdKeys(env, true)).toEqual(['KEY_ENCRYPTION_KEY']);
    expect(missingProdKeys(env, false).sort()).toEqual([
      'KEY_ENCRYPTION_KEY', 'QURL_API_KEY',
    ]);
  });
});
