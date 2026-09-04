// Tests for src/routes/discord-install.js — the Stage-2 "Add to Discord"
// install callback that chains the Discord OAuth2 install to the qURL
// Auth0 leg. Covers:
//   - 503 not-configured response (DISCORD_CLIENT_SECRET or AUTH0_* unset)
//   - 400 missing code, declined consent, mismatched guild hint
//   - 502 Discord token exchange failure, /users/@me failure
//   - 302 happy path: redirects to Auth0 with a qURL OAuth state binding
//     guild_id + discord_user_id

// OAUTH_STATE_SECRET is pinned globally in tests/setup-env.js.
// KEY_ENCRYPTION_KEY required for the fail-fast guard added in PR #177
// review round 3; matches the legacy modal-paste path's existing check.
process.env.KEY_ENCRYPTION_KEY = '1'.repeat(64);
process.env.AUTH0_DOMAIN = 'layerv-test.auth0.com';
process.env.AUTH0_CLIENT_ID = 'test-auth0-client-id';
process.env.AUTH0_CLIENT_SECRET = 'test-auth0-secret';
process.env.AUTH0_AUDIENCE = 'https://api.layerv.test';
process.env.AUTH0_EMAIL_CONNECTION = ' email ';
process.env.DISCORD_CLIENT_ID = '234567890123456789';
// Real SSM writes can accidentally carry surrounding whitespace. Config must
// normalize it once so the Discord token exchange receives the actual secret.
process.env.DISCORD_CLIENT_SECRET = ' test-discord-secret\n';
process.env.QURL_ENDPOINT = 'http://localhost:9999';
process.env.BASE_URL = 'http://localhost:3000';
process.env.GUILD_ID = '123456789012345678';
// Trust proxy so the Secure-cookie test can simulate ALB-fronted prod
// via X-Forwarded-Proto: https (server.js reads TRUST_PROXY at module
// load — must be set BEFORE require('../src/server') below).
process.env.TRUST_PROXY = '1';

jest.mock('../src/discord', () => ({
  sendDM: jest.fn().mockResolvedValue(true),
  assignContributorRole: jest.fn(),
  notifyPRMerge: jest.fn(),
  notifyBadgeEarned: jest.fn(),
}));

jest.mock('../src/store', () => ({
  setGuildApiKey: jest.fn().mockResolvedValue(undefined),
  getGuildApiKey: jest.fn(),
  getPendingLink: jest.fn(),
  consumePendingLink: jest.fn(),
}));

jest.mock('../src/commands', () => ({
  verifyStateBinding: jest.fn().mockReturnValue(true),
  handleCommand: jest.fn(),
  commands: [],
  registerCommands: jest.fn(),
}));

const request = require('supertest');
const { app } = require('../src/server');
const config = require('../src/config');
const {
  signQurlOAuthState,
  verifyQurlOAuthState,
} = require('../src/utils/qurl-oauth-state');
const {
  QURL_OAUTH_SESSION_COOKIE,
  QURL_OAUTH_PKCE_COOKIE,
  DISCORD_INSTALL_SESSION_COOKIE,
} = require('../src/utils/oauth-cookies');
const { pkceChallengeForVerifier } = require('../src/utils/oauth-pkce');
const { rateLimitStore } = require('../src/utils/oauth-rate-limit');
const logger = require('../src/logger');
const { clearedCookieHeader, cookieValue } = require('./helpers/cookies');

const originalFetch = globalThis.fetch;
const DISCORD_INSTALL_STATE = 'a'.repeat(43);
const REQUIRED_DISCORD_PERMISSION_BITS = [10n, 11n, 14n, 31n];
const REQUIRED_DISCORD_PERMISSIONS = REQUIRED_DISCORD_PERMISSION_BITS
  .reduce((permissions, bit) => permissions | (1n << bit), 0n)
  .toString();

function discordCallback(query, { cookieState = DISCORD_INSTALL_STATE } = {}) {
  const separator = query.includes('?') ? '&' : '?';
  const test = request(app).get(`${query}${separator}state=${DISCORD_INSTALL_STATE}`);
  if (cookieState !== null) {
    test.set('Cookie', `${DISCORD_INSTALL_SESSION_COOKIE}=${cookieState}`);
  }
  return test;
}

function mockSuccessfulDiscordIdentity() {
  globalThis.fetch = jest.fn()
    .mockResolvedValueOnce({
      ok: true, status: 200,
      json: () => Promise.resolve({
        access_token: 'disc-token',
        token_type: 'Bearer',
        guild: { id: '123456789012345678' },
      }),
    })
    .mockResolvedValueOnce({
      ok: true, status: 200,
      json: () => Promise.resolve({ id: '987654321098765432', username: 'admin' }),
    });
}

function extractStyleNonce(res) {
  const csp = res.headers['content-security-policy'];
  expect(csp).toBeDefined();
  expect(csp).not.toContain('unsafe-inline');

  const nonceMatch = csp.match(/style-src 'nonce-([A-Za-z0-9_-]+)'/);
  expect(nonceMatch).not.toBeNull();
  return nonceMatch[1];
}

beforeEach(() => {
  jest.clearAllMocks();
  rateLimitStore.clear();
  globalThis.fetch = originalFetch;
});

afterEach(() => {
  // Ensure a failed assertion cannot leave a logger spy installed for the
  // remaining cases in this file.
  jest.restoreAllMocks();
});

describe('Discord install callback', () => {
  describe('GET /oauth/discord/install', () => {
    it('redirects to the complete Discord authorization-code install flow', async () => {
      const res = await request(app).get('/oauth/discord/install');

      expect(res.status).toBe(302);
      const loc = new URL(res.headers.location);
      expect(loc.origin).toBe('https://discord.com');
      expect(loc.pathname).toBe('/oauth2/authorize');
      expect(loc.searchParams.get('client_id')).toBe('234567890123456789');
      expect(REQUIRED_DISCORD_PERMISSIONS).toBe('2147503104');
      expect(loc.searchParams.get('permissions')).toBe(REQUIRED_DISCORD_PERMISSIONS);
      expect(loc.searchParams.get('integration_type')).toBe('0');
      expect(loc.searchParams.get('response_type')).toBe('code');
      expect(loc.searchParams.get('redirect_uri')).toBe(
        'http://localhost:3000/oauth/discord/callback',
      );
      expect(new Set(loc.searchParams.get('scope').split(' '))).toEqual(
        new Set(['identify', 'bot', 'applications.commands']),
      );
      const state = loc.searchParams.get('state');
      expect(state).toMatch(/^[A-Za-z0-9_-]{43}$/);
      expect(cookieValue(res.headers['set-cookie'], DISCORD_INSTALL_SESSION_COOKIE)).toBe(state);
      const cookieHeader = Array.isArray(res.headers['set-cookie'])
        ? res.headers['set-cookie'].join('\n')
        : res.headers['set-cookie'];
      expect(cookieHeader).toMatch(/HttpOnly/i);
      expect(cookieHeader).toMatch(/SameSite=Lax/i);
      expect(cookieHeader).toMatch(/Max-Age=600/i);
      expect(DISCORD_INSTALL_SESSION_COOKIE).toMatch(/^__Host-/);
      expect(cookieHeader).toMatch(/Secure/i);
      expect(cookieHeader).toMatch(/Path=\/(?:;|\s|$)/);
      expect(res.headers['cache-control']).toBe('no-store');
      expect(cookieValue(res.headers['set-cookie'], QURL_OAUTH_SESSION_COOKIE)).toBeNull();
      expect(cookieValue(res.headers['set-cookie'], QURL_OAUTH_PKCE_COOKIE)).toBeNull();
    });

    it('mints a fresh install state for every request', async () => {
      const first = await request(app).get('/oauth/discord/install');
      const second = await request(app).get('/oauth/discord/install');

      expect(new URL(first.headers.location).searchParams.get('state'))
        .not.toBe(new URL(second.headers.location).searchParams.get('state'));
    });

    it('refuses to begin an install when encryption-at-rest is not configured', async () => {
      const saved = process.env.KEY_ENCRYPTION_KEY;
      const errorSpy = jest.spyOn(logger, 'error').mockImplementation(() => {});
      delete process.env.KEY_ENCRYPTION_KEY;
      try {
        const res = await request(app).get('/oauth/discord/install');

        expect(res.status).toBe(503);
        expect(res.text).toContain('Nothing was installed');
        expect(res.text).not.toContain('KEY_ENCRYPTION_KEY');
        expect(cookieValue(
          res.headers['set-cookie'],
          DISCORD_INSTALL_SESSION_COOKIE,
        )).toBeNull();
        expect(errorSpy).toHaveBeenCalledWith(
          'Refusing /oauth/discord/install: KEY_ENCRYPTION_KEY is not set',
        );
      } finally {
        process.env.KEY_ENCRYPTION_KEY = saved;
        errorSpy.mockRestore();
      }
    });

    it('sets Secure on the host-prefixed install-session cookie even on local HTTP', async () => {
      const res = await request(app).get('/oauth/discord/install');

      const cookieHeader = Array.isArray(res.headers['set-cookie'])
        ? res.headers['set-cookie'].join('\n')
        : res.headers['set-cookie'];
      expect(cookieHeader).toMatch(/Secure/i);
    });

    it('keeps a bounded OAuth rate limiter mounted on the public install entrypoint', async () => {
      for (let i = 0; i < config.RATE_LIMIT_MAX_REQUESTS; i++) {
        // Sequential requests make the expected bucket consumption explicit.
        // Each response is a local redirect; no Discord network call occurs.
        // eslint-disable-next-line no-await-in-loop
        const res = await request(app).get('/oauth/discord/install');
        expect(res.status).toBe(302);
      }

      const throttled = await request(app).get('/oauth/discord/install');
      expect(throttled.status).toBe(429);
      expect(throttled.text).toContain('Slow Down');
    });

    it('does not let install-page traffic consume the in-flight callback budget', async () => {
      for (let i = 0; i < config.RATE_LIMIT_MAX_REQUESTS; i++) {
        // eslint-disable-next-line no-await-in-loop
        await request(app).get('/oauth/discord/install');
      }
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({
            access_token: 'disc-token', guild: { id: '123456789012345678' },
          }),
        })
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({ id: '987654321098765432' }),
        });

      const callback = await discordCallback(
        '/oauth/discord/callback?code=ok-code&guild_id=123456789012345678',
      );

      expect(callback.status).toBe(302);
      expect(globalThis.fetch).toHaveBeenCalledTimes(2);
      // Separate logical buckets must still occupy one bounded entry per IP.
      expect(rateLimitStore.size).toBe(1);
    });
  });

  describe('GET /oauth/discord/callback', () => {
    it('rejects missing or mismatched install-session state before Discord token exchange', async () => {
      globalThis.fetch = jest.fn();

      const missing = await request(app)
        .get('/oauth/discord/callback?code=ok-code&guild_id=123456789012345678');
      const mismatch = await discordCallback(
        '/oauth/discord/callback?code=ok-code&guild_id=123456789012345678',
        { cookieState: 'b'.repeat(43) },
      );
      const empty = await request(app)
        .get('/oauth/discord/callback?code=ok-code&guild_id=123456789012345678&state=')
        .set('Cookie', `${DISCORD_INSTALL_SESSION_COOKIE}=`);
      const duplicateState = await request(app)
        .get(`/oauth/discord/callback?code=ok-code&guild_id=123456789012345678&state=${DISCORD_INSTALL_STATE}&state=${DISCORD_INSTALL_STATE}`)
        .set('Cookie', `${DISCORD_INSTALL_SESSION_COOKIE}=${DISCORD_INSTALL_STATE}`);

      for (const res of [missing, mismatch, empty, duplicateState]) {
        expect(res.status).toBe(400);
        expect(res.text).toContain('Invalid install link');
        expect(res.text.indexOf('/qurl setup')).toBeLessThan(
          res.text.indexOf('install session is invalid or expired'),
        );
        expect(clearedCookieHeader(
          res.headers['set-cookie'],
          DISCORD_INSTALL_SESSION_COOKIE,
        )).toBeUndefined();
        expect(cookieValue(res.headers['set-cookie'], QURL_OAUTH_SESSION_COOKIE)).toBeNull();
        expect(cookieValue(res.headers['set-cookie'], QURL_OAUTH_PKCE_COOKIE)).toBeNull();
      }
      expect(globalThis.fetch).not.toHaveBeenCalled();
    });

    it('rejects duplicate install-session cookies before Discord token exchange', async () => {
      globalThis.fetch = jest.fn();
      const res = await request(app)
        .get(`/oauth/discord/callback?code=ok-code&guild_id=123456789012345678&state=${DISCORD_INSTALL_STATE}`)
        .set('Cookie', `${DISCORD_INSTALL_SESSION_COOKIE}=${DISCORD_INSTALL_STATE}; ${DISCORD_INSTALL_SESSION_COOKIE}=${DISCORD_INSTALL_STATE}`);

      expect(res.status).toBe(400);
      expect(res.text).toContain('invalid or expired');
      expect(globalThis.fetch).not.toHaveBeenCalled();
    });

    it('logs only whether a rejected callback carried an install cookie', async () => {
      const warn = jest.spyOn(logger, 'warn').mockImplementation(() => {});

      await request(app)
        .get(`/oauth/discord/callback?code=ok-code&state=${DISCORD_INSTALL_STATE}`);
      await discordCallback('/oauth/discord/callback?code=ok-code', {
        cookieState: 'b'.repeat(43),
      });

      expect(warn).toHaveBeenNthCalledWith(
        1,
        'Discord install callback rejected invalid session state',
        expect.objectContaining({ hasCookie: false }),
      );
      expect(warn).toHaveBeenNthCalledWith(
        2,
        'Discord install callback rejected invalid session state',
        expect.objectContaining({ hasCookie: true }),
      );
      expect(JSON.stringify(warn.mock.calls)).not.toContain('b'.repeat(43));
    });

    it('validates state before returning a generic KEY_ENCRYPTION_KEY 503', async () => {
      // With Require OAuth2 Code Grant enabled, Discord does not finish
      // installing the bot until the code exchange succeeds. Failing here
      // saves the code from being burned on a doomed flow. The service cannot
      // observe Discord's external Code Grant setting, so recovery must cover
      // both the installed and not-yet-installed states.
      const saved = process.env.KEY_ENCRYPTION_KEY;
      const errorSpy = jest.spyOn(logger, 'error').mockImplementation(() => {});
      delete process.env.KEY_ENCRYPTION_KEY;
      try {
        const invalid = await request(app)
          .get('/oauth/discord/callback?code=ok-code&guild_id=123456789012345678');
        const res = await discordCallback('/oauth/discord/callback?code=ok-code&guild_id=123456789012345678');

        expect(invalid.status).toBe(400);
        expect(invalid.text).toContain('Invalid install link');
        expect(res.status).toBe(503);
        expect(res.text).toMatch(/not configured/i);
        expect(res.text).toContain('may already be in your server');
        expect(res.text).toContain('/qurl setup');
        expect(res.text).not.toContain('KEY_ENCRYPTION_KEY');
        expect(clearedCookieHeader(
          res.headers['set-cookie'],
          DISCORD_INSTALL_SESSION_COOKIE,
        )).toBeUndefined();
        expect(errorSpy).toHaveBeenCalledWith(
          'Refusing /oauth/discord/callback: KEY_ENCRYPTION_KEY is not set',
        );
      } finally {
        process.env.KEY_ENCRYPTION_KEY = saved;
        errorSpy.mockRestore();
      }
    });

    it('400s on missing code', async () => {
      const res = await discordCallback('/oauth/discord/callback?guild_id=123456789012345678');
      expect(res.status).toBe(400);
      expect(res.text).toContain('Missing authorization code');
      expect(clearedCookieHeader(
        res.headers['set-cookie'],
        DISCORD_INSTALL_SESSION_COOKIE,
      )).toBeUndefined();
    });

    it('uses one CSP nonce in the HTTP header and style tag', async () => {
      const res = await discordCallback('/oauth/discord/callback?guild_id=123456789012345678');
      expect(res.status).toBe(400);

      const nonce = extractStyleNonce(res);
      // 16 random bytes encoded as unpadded base64url.
      expect(nonce).toHaveLength(22);

      expect(res.text).toContain(`<style nonce="${nonce}">`);
      expect(res.text).not.toContain('Content-Security-Policy');
      expect(res.text).not.toContain('unsafe-inline');
    });

    it('generates a fresh CSP nonce for each response', async () => {
      const first = await discordCallback('/oauth/discord/callback?guild_id=123456789012345678');
      const second = await discordCallback('/oauth/discord/callback?guild_id=123456789012345678');

      expect(extractStyleNonce(first)).not.toBe(extractStyleNonce(second));
    });

    it('accepts an absent advisory guild_id when the token response identifies the installed guild', async () => {
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({
            access_token: 'disc-token', token_type: 'Bearer', guild: { id: '123456789012345678' },
          }),
        })
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({ id: '987654321098765432' }),
        });

      const res = await discordCallback('/oauth/discord/callback?code=disc-code');

      expect(res.status).toBe(302);
      expect(verifyQurlOAuthState(new URL(res.headers.location).searchParams.get('state')).payload.guildId)
        .toBe('123456789012345678');
      expect(clearedCookieHeader(
        res.headers['set-cookie'],
        DISCORD_INSTALL_SESSION_COOKIE,
      )).toBeDefined();
    });

    it('400s on Discord error param (admin declined consent)', async () => {
      const res = await discordCallback(
        '/oauth/discord/callback?error=access_denied&error_description=user+declined&guild_id=123456789012345678',
      );
      expect(res.status).toBe(400);
      expect(res.text).toContain('Authorization declined');
      expect(clearedCookieHeader(
        res.headers['set-cookie'],
        DISCORD_INSTALL_SESSION_COOKIE,
      )).toBeUndefined();
    });

    it('502s when Discord token exchange fails', async () => {
      const reflectedCode = 'reflected-oauth-code-must-not-be-logged';
      const errorSpy = jest.spyOn(logger, 'error').mockImplementation(() => {});
      globalThis.fetch = jest.fn().mockResolvedValueOnce({
        ok: false, status: 401, text: () => Promise.resolve(reflectedCode),
      });
      const res = await discordCallback('/oauth/discord/callback?code=bad-code&guild_id=123456789012345678');
      expect(res.status).toBe(502);
      expect(res.text).toContain('Authorization failed');
      expect(JSON.stringify(errorSpy.mock.calls)).not.toContain(reflectedCode);
    });

    it('502s when Discord token response omits the installed guild', async () => {
      globalThis.fetch = jest.fn().mockResolvedValueOnce({
        ok: true, status: 200,
        json: () => Promise.resolve({ access_token: 'disc-token', token_type: 'Bearer' }),
      });

      const res = await discordCallback('/oauth/discord/callback?code=ok-code');

      expect(res.status).toBe(502);
      expect(res.text).toContain('Discord returned an unexpected response');
      expect(globalThis.fetch).toHaveBeenCalledTimes(1);
    });

    it.each([
      ['a non-string guild ID', 123],
      ['a malformed string guild ID', 'guild-not-a-snowflake'],
    ])('502s when Discord token response returns %s', async (_label, guildId) => {
      globalThis.fetch = jest.fn().mockResolvedValueOnce({
        ok: true, status: 200,
        json: () => Promise.resolve({
          access_token: 'disc-token', token_type: 'Bearer', guild: { id: guildId },
        }),
      });

      const res = await discordCallback('/oauth/discord/callback?code=ok-code&guild_id=123');

      expect(res.status).toBe(502);
      expect(res.text).toContain('Discord returned an unexpected response');
      expect(globalThis.fetch).toHaveBeenCalledTimes(1);
    });

    it('rejects a browser guild hint that differs from Discord token response', async () => {
      globalThis.fetch = jest.fn().mockResolvedValueOnce({
        ok: true, status: 200,
        json: () => Promise.resolve({
          access_token: 'disc-token',
          token_type: 'Bearer',
          guild: { id: '345678901234567890' },
        }),
      });

      const res = await discordCallback('/oauth/discord/callback?code=ok-code&guild_id=456789012345678901');

      expect(res.status).toBe(400);
      expect(res.text).toContain('selected server did not match');
      expect(globalThis.fetch).toHaveBeenCalledTimes(1);
      expect(clearedCookieHeader(
        res.headers['set-cookie'],
        DISCORD_INSTALL_SESSION_COOKIE,
      )).toBeDefined();
    });

    it('warns but continues when Discord reports a different granted permission bitfield', async () => {
      const warnSpy = jest.spyOn(logger, 'warn').mockImplementation(() => {});
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({
            access_token: 'disc-token', guild: { id: '123456789012345678' },
          }),
        })
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({ id: '987654321098765432' }),
        });

      const res = await discordCallback(
        '/oauth/discord/callback?code=ok-code&guild_id=123456789012345678&permissions=1024',
      );

      expect(res.status).toBe(302);
      expect(warnSpy).toHaveBeenCalledWith(
        'Discord install callback reported different bot permissions',
        expect.objectContaining({
          grantedPermissions: '1024',
          requestedPermissions: '2147503104',
        }),
      );
      warnSpy.mockRestore();
    });

    it('does not warn when Discord reports the exact requested permission bitfield', async () => {
      const warnSpy = jest.spyOn(logger, 'warn').mockImplementation(() => {});
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({
            access_token: 'disc-token', guild: { id: '123456789012345678' },
          }),
        })
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({ id: '987654321098765432' }),
        });

      const res = await discordCallback(
        `/oauth/discord/callback?code=ok-code&guild_id=123456789012345678&permissions=${REQUIRED_DISCORD_PERMISSIONS}`,
      );

      expect(res.status).toBe(302);
      expect(warnSpy).not.toHaveBeenCalledWith(
        'Discord install callback reported different bot permissions',
        expect.anything(),
      );
      warnSpy.mockRestore();
    });

    it('does not warn when Discord reports a superset of the requested permissions', async () => {
      const warnSpy = jest.spyOn(logger, 'warn').mockImplementation(() => {});
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({
            access_token: 'disc-token', guild: { id: '123456789012345678' },
          }),
        })
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({ id: '987654321098765432' }),
        });
      // Bit 80 keeps this above the old 20-digit sanitizer cap and proves the
      // decimal-string/BigInt path remains forward-compatible with new bits.
      const grantedSuperset = (
        BigInt(REQUIRED_DISCORD_PERMISSIONS) | (1n << 80n)
      ).toString();

      const res = await discordCallback(
        `/oauth/discord/callback?code=ok-code&guild_id=123456789012345678&permissions=${grantedSuperset}`,
      );

      expect(res.status).toBe(302);
      expect(warnSpy).not.toHaveBeenCalledWith(
        'Discord install callback reported different bot permissions',
        expect.anything(),
      );
      warnSpy.mockRestore();
    });

    it('does not copy malformed callback permissions into operator logs', async () => {
      const warnSpy = jest.spyOn(logger, 'warn').mockImplementation(() => {});
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({
            access_token: 'disc-token', guild: { id: '123456789012345678' },
          }),
        })
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({ id: '987654321098765432' }),
        });

      const res = await discordCallback(
        '/oauth/discord/callback?code=ok-code&guild_id=123456789012345678&permissions=not-a-bitfield',
      );

      expect(res.status).toBe(302);
      expect(warnSpy).toHaveBeenCalledWith(
        'Discord install callback reported different bot permissions',
        expect.objectContaining({
          grantedPermissions: '<malformed>',
          requestedPermissions: REQUIRED_DISCORD_PERMISSIONS,
        }),
      );
      warnSpy.mockRestore();
    });

    it('502s when Discord /users/@me fails after successful token exchange', async () => {
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({
            access_token: 'disc-token', token_type: 'Bearer', guild: { id: '123456789012345678' },
          }),
        })
        .mockResolvedValueOnce({
          ok: false, status: 500,
          text: () => Promise.resolve('Discord API error'),
        });
      const res = await discordCallback('/oauth/discord/callback?code=ok-code&guild_id=123456789012345678');
      expect(res.status).toBe(502);
      expect(res.text).toContain('Could not identify the installing user');
    });

    it('302s to Auth0 on happy path with a valid qURL OAuth state and sets the CSRF cookie', async () => {
      mockSuccessfulDiscordIdentity();
      const res = await discordCallback('/oauth/discord/callback?code=ok-code&guild_id=123456789012345678');
      expect(res.status).toBe(302);
      expect(clearedCookieHeader(
        res.headers['set-cookie'],
        DISCORD_INSTALL_SESSION_COOKIE,
      )).toBeDefined();
      const loc = new URL(res.headers.location);
      expect(loc.host).toBe('layerv-test.auth0.com');
      expect(loc.pathname).toBe('/authorize');
      expect(loc.searchParams.get('client_id')).toBe('test-auth0-client-id');
      expect(loc.searchParams.get('redirect_uri')).toBe('http://localhost:3000/oauth/qurl/callback');
      // Auth0 scope must NOT include offline_access (refresh tokens not
      // stored/used; dropped per PR #177 review item 5).
      expect(loc.searchParams.get('scope')).not.toContain('offline_access');
      // Both halves are load-bearing on every Auth0 entry path: `login`
      // asks Auth0 not to reuse its ambient session, and `consent` lets a
      // setup re-run mint a new key.
      expect(loc.searchParams.get('prompt')).toBe('login consent');
      expect(loc.searchParams.get('connection')).toBe('email');

      // The state Discord callback minted must round-trip through the
      // qURL OAuth state verifier with the right guild + discord-user
      // bindings — that's how the Auth0 callback identifies who set
      // up which guild.
      const state = loc.searchParams.get('state');
      const verified = verifyQurlOAuthState(state);
      expect(verified.ok).toBe(true);
      expect(verified.payload.guildId).toBe('123456789012345678');
      expect(verified.payload.discordUserId).toBe('987654321098765432');

      const codeVerifier = cookieValue(res.headers['set-cookie'], QURL_OAUTH_PKCE_COOKIE);
      expect(codeVerifier).not.toBeNull();
      expect(loc.searchParams.get('code_challenge_method')).toBe('S256');
      expect(loc.searchParams.get('code_challenge')).toBe(pkceChallengeForVerifier(codeVerifier));
      expect(loc.searchParams.get('code_challenge')).not.toBe(codeVerifier);

      // Cookie binding — Stage-2 chain must set the same `qurl_setup_session`
      // cookie that /oauth/qurl/start sets — Stage-2 sets it at the
      // discord-install handler so the chained /oauth/qurl/callback
      // sees it. Path narrowed to /oauth/qurl per round-9 item #2 —
      // the only reader is /oauth/qurl/callback so the broader /oauth
      // was unnecessary scope.
      const setCookie = res.headers['set-cookie'];
      expect(setCookie).toBeDefined();
      const cookieHeader = Array.isArray(setCookie) ? setCookie.join('\n') : setCookie;
      expect(cookieHeader).toMatch(new RegExp(`${QURL_OAUTH_SESSION_COOKIE}=`));
      expect(cookieHeader).toMatch(new RegExp(`${QURL_OAUTH_PKCE_COOKIE}=`));
      expect(cookieHeader).toMatch(/HttpOnly/i);
      expect(cookieHeader).toMatch(/SameSite=Lax/i);
      expect(cookieHeader).toMatch(/Path=\/oauth\/qurl(?:;|\s|$)/);
      expect(cookieHeader).toContain(encodeURIComponent(state));
    });

    it('asks the browser to clear the install-session cookie after a valid callback', async () => {
      const install = await request(app).get('/oauth/discord/install');
      const state = new URL(install.headers.location).searchParams.get('state');
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({
            access_token: 'disc-token', guild: { id: '123456789012345678' },
          }),
        })
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({ id: '987654321098765432' }),
        });
      const callback = `/oauth/discord/callback?code=ok-code&guild_id=123456789012345678&state=${state}`;

      const first = await request(app)
        .get(callback)
        .set('Cookie', `${DISCORD_INSTALL_SESSION_COOKIE}=${state}`);

      expect(first.status).toBe(302);
      expect(clearedCookieHeader(
        first.headers['set-cookie'],
        DISCORD_INSTALL_SESSION_COOKIE,
      )).toMatch(/Secure/i);
      expect(globalThis.fetch).toHaveBeenCalledTimes(2);
    });

    it('keeps every non-ephemeral Auth0 authorize parameter identical across both entry paths', async () => {
      mockSuccessfulDiscordIdentity();
      const installRes = await discordCallback(
        '/oauth/discord/callback?code=ok-code&guild_id=123456789012345678',
      );
      const setupState = signQurlOAuthState(
        '123456789012345678',
        '987654321098765432',
      );
      const setupRes = await request(app)
        .get(`/oauth/qurl/start?state=${encodeURIComponent(setupState)}`);

      expect(installRes.status).toBe(302);
      expect(setupRes.status).toBe(302);
      const stableParams = (location) => [...new URL(location).searchParams.entries()]
        .filter(([key]) => !['state', 'code_challenge'].includes(key))
        .sort(([left], [right]) => left.localeCompare(right));
      expect(stableParams(installRes.headers.location))
        .toEqual(stableParams(setupRes.headers.location));
    });

    it('omits the connection pin on Add to Discord when the setting is unset', async () => {
      // The app and route capture this config module object at load time; the
      // shared builder reads its current property value on every request.
      const configuredConnection = config.AUTH0_EMAIL_CONNECTION;
      config.AUTH0_EMAIL_CONNECTION = '';
      try {
        mockSuccessfulDiscordIdentity();
        const res = await discordCallback(
          '/oauth/discord/callback?code=ok-code&guild_id=123456789012345678',
        );

        expect(res.status).toBe(302);
        const loc = new URL(res.headers.location);
        expect(loc.searchParams.get('prompt')).toBe('login consent');
        expect(loc.searchParams.get('connection')).toBeNull();
      } finally {
        config.AUTH0_EMAIL_CONNECTION = configuredConnection;
      }
    });

    it('sets Secure flag on the cookie when behind a proxy that sets X-Forwarded-Proto: https', async () => {
      // Defense vs trust-proxy regression: server.js sets `trust proxy`
      // so req.protocol reflects X-Forwarded-Proto from the ALB. Flipping
      // that off would silently downgrade prod cookies to insecure. Pin
      // the wire-level shape here.
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({
            access_token: 'disc-token', guild: { id: '123456789012345678' },
          }),
        })
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({ id: '987654321098765432' }),
        });
      const res = await discordCallback('/oauth/discord/callback?code=ok-code&guild_id=123456789012345678')
        .set('X-Forwarded-Proto', 'https');
      expect(res.status).toBe(302);
      const cookieHeader = (Array.isArray(res.headers['set-cookie']) ? res.headers['set-cookie'].join('\n') : res.headers['set-cookie']) || '';
      expect(cookieHeader).toMatch(/Secure/);
    });

    it('still forces fresh login and consent when Discord omits the advisory guild hint', async () => {
      // Re-installs may omit the callback hint. Stage 2 must still use the
      // authoritative token-response guild and keep the shared fresh-login
      // plus consent policy.
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({
            access_token: 'disc-token', guild: { id: '123456789012345678' },
          }),
        })
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({ id: '987654321098765432' }),
        });
      const res = await discordCallback('/oauth/discord/callback?code=ok-code');
      expect(res.status).toBe(302);
      const loc = new URL(res.headers.location);
      expect(loc.searchParams.get('prompt')).toBe('login consent');
    });

    it('cookie set at /oauth/discord/callback rides through to /oauth/qurl/callback (round-trip pin per round-9 #8)', async () => {
      // Round-9 #8 closed: the previous tests inspected the Set-Cookie
      // header but didn't actually replay the cookie back on the qurl
      // callback. Path=/oauth/qurl on the cookie + request URL
      // /oauth/qurl/callback is the prefix-match the browser uses when
      // deciding to send the cookie back; pin it end-to-end so a
      // future path narrowing/widening can't silently break Stage-2.
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({
            access_token: 'disc-token', guild: { id: '123456789012345678' },
          }),
        })
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({ id: '987654321098765432' }),
        });
      const stage2 = await discordCallback('/oauth/discord/callback?code=ok-code&guild_id=123456789012345678');
      expect(stage2.status).toBe(302);
      const setCookie = Array.isArray(stage2.headers['set-cookie'])
        ? stage2.headers['set-cookie'].join('\n')
        : stage2.headers['set-cookie'] || '';
      const sessionCookieValue = cookieValue(setCookie, QURL_OAUTH_SESSION_COOKIE);
      const pkceCookieValue = cookieValue(setCookie, QURL_OAUTH_PKCE_COOKIE);
      expect(sessionCookieValue).not.toBeNull();
      expect(pkceCookieValue).not.toBeNull();
      const stateFromRedirect = new URL(stage2.headers.location).searchParams.get('state');
      // The cookie value IS the state token (double-submit pattern).
      expect(sessionCookieValue).toBe(stateFromRedirect);

      // Replay the cookie on /oauth/qurl/callback — the browser would
      // do this because Path=/oauth/qurl matches the request path.
      // Stub Auth0 + qurl-service so the chained callback can reach
      // the cookie/state CSRF check.
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({ access_token: 'jwt-xyz' }),
        })
        .mockResolvedValueOnce({
          ok: true, status: 201,
          json: () => Promise.resolve({ data: { key_id: 'key-1', api_key: 'lv_live_abc', key_prefix: 'lv_live_a' } }),
        });
      const stage1Callback = await request(app)
        .get(`/oauth/qurl/callback?code=auth0-code&state=${encodeURIComponent(stateFromRedirect)}`)
        .set('Cookie', `${QURL_OAUTH_SESSION_COOKIE}=${encodeURIComponent(sessionCookieValue)}; `
          + `${QURL_OAUTH_PKCE_COOKIE}=${encodeURIComponent(pkceCookieValue)}`);
      expect(stage1Callback.status).toBe(200);
      const tokenBody = new URLSearchParams(globalThis.fetch.mock.calls[0][1].body.toString());
      expect(tokenBody.get('code_verifier')).toBe(pkceCookieValue);
      // Reaching the success page proves the cookie/state CSRF check
      // passed — i.e., the cookie minted on /oauth/discord/callback
      // would actually travel to /oauth/qurl/callback in a real
      // browser (path attribute does its job).
      expect(stage1Callback.text).toContain('qURL is connected');
    });

    it('uses the right Discord token-exchange body shape (form-urlencoded with client creds)', async () => {
      const fetchSpy = jest.fn()
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({
            access_token: 'disc-token', guild: { id: '123456789012345678' },
          }),
        })
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({ id: '111' }),
        });
      globalThis.fetch = fetchSpy;
      await discordCallback('/oauth/discord/callback?code=ok-code&guild_id=123456789012345678');
      expect(fetchSpy).toHaveBeenCalled();
      const tokenCall = fetchSpy.mock.calls[0];
      expect(tokenCall[0]).toBe('https://discord.com/api/oauth2/token');
      expect(tokenCall[1].method).toBe('POST');
      expect(tokenCall[1].headers['Content-Type']).toBe('application/x-www-form-urlencoded');
      const bodyParams = new URLSearchParams(tokenCall[1].body.toString());
      expect(bodyParams.get('client_id')).toBe('234567890123456789');
      expect(bodyParams.get('client_secret')).toBe('test-discord-secret');
      expect(bodyParams.get('grant_type')).toBe('authorization_code');
      expect(bodyParams.get('code')).toBe('ok-code');
      expect(bodyParams.get('redirect_uri')).toBe('http://localhost:3000/oauth/discord/callback');
    });
  });
});

// Separate describe — exercises the not-configured 503 paths that
// `isDiscordInstallConfigured` gates. Uses jest.isolateModulesAsync so
// the env-var unsetting on this branch doesn't leak into the
// configured-flow describe above (it's already past). Mirrors the
// equivalent suite in tests/qurl-oauth.test.js for AUTH0_* unset.
describe('discord-install — not configured', () => {
  it('returns 503 with the AUTH0-unset reason when Auth0 env is missing', async () => {
    const saved = {
      AUTH0_DOMAIN: process.env.AUTH0_DOMAIN,
      AUTH0_CLIENT_ID: process.env.AUTH0_CLIENT_ID,
      AUTH0_CLIENT_SECRET: process.env.AUTH0_CLIENT_SECRET,
      AUTH0_AUDIENCE: process.env.AUTH0_AUDIENCE,
    };
    delete process.env.AUTH0_DOMAIN;
    delete process.env.AUTH0_CLIENT_ID;
    delete process.env.AUTH0_CLIENT_SECRET;
    delete process.env.AUTH0_AUDIENCE;
    try {
      await jest.isolateModulesAsync(async () => {
        jest.doMock('../src/discord', () => ({
          sendDM: jest.fn().mockResolvedValue(true),
          assignContributorRole: jest.fn(),
          notifyPRMerge: jest.fn(),
          notifyBadgeEarned: jest.fn(),
        }));
        jest.doMock('../src/store', () => ({
          setGuildApiKey: jest.fn(),
          getGuildApiKey: jest.fn(),
          getPendingLink: jest.fn(),
          consumePendingLink: jest.fn(),
        }));
        jest.doMock('../src/commands', () => ({
          verifyStateBinding: jest.fn().mockReturnValue(true),
          handleCommand: jest.fn(),
          commands: [],
          registerCommands: jest.fn(),
        }));
        // eslint-disable-next-line global-require
        const supertest = require('supertest');
        // eslint-disable-next-line global-require
        const { app: freshApp } = require('../src/server');
        const responses = await Promise.all([
          supertest(freshApp).get('/oauth/discord/install'),
          supertest(freshApp).get('/oauth/discord/callback?code=ok-code&guild_id=123456789012345678'),
        ]);
        for (const res of responses) {
          expect(res.status).toBe(503);
          // Generic "not configured" copy on the wire (C.4); the env-var
          // reason is logged but MUST NOT appear in the rendered HTML —
          // echoing it would tell a probing attacker which secret an
          // operator hasn't shipped yet. Env-var-shaped strings + the
          // legacy "Reason:" prefix are the leak surfaces; the literal
          // word "Auth0" alone is the user-visible service name and OK.
          expect(res.text).toMatch(/not configured/i);
          expect(res.text).not.toMatch(/AUTH0_[A-Z_]+/);
          expect(res.text).not.toMatch(/DISCORD_CLIENT_SECRET/);
          expect(res.text).not.toMatch(/Reason:/i);
          expect(cookieValue(
            res.headers['set-cookie'],
            DISCORD_INSTALL_SESSION_COOKIE,
          )).toBeNull();
        }
        expect(responses[0].text).toContain('Nothing was installed');
        expect(responses[1].text).toContain('may already be in your server');
      });
    } finally {
      Object.assign(process.env, saved);
    }
  });

  it.each(['DISCORD_CLIENT_ID', 'DISCORD_CLIENT_SECRET'])(
    'returns 503 when Auth0 is set but %s is missing',
    async (missingKey) => {
      const saved = process.env[missingKey];
      delete process.env[missingKey];
      try {
        await jest.isolateModulesAsync(async () => {
          jest.doMock('../src/discord', () => ({
            sendDM: jest.fn().mockResolvedValue(true),
            assignContributorRole: jest.fn(),
            notifyPRMerge: jest.fn(),
            notifyBadgeEarned: jest.fn(),
          }));
          jest.doMock('../src/store', () => ({
            setGuildApiKey: jest.fn(),
            getGuildApiKey: jest.fn(),
            getPendingLink: jest.fn(),
            consumePendingLink: jest.fn(),
          }));
          jest.doMock('../src/commands', () => ({
            verifyStateBinding: jest.fn().mockReturnValue(true),
            handleCommand: jest.fn(),
            commands: [],
            registerCommands: jest.fn(),
          }));
          // eslint-disable-next-line global-require
          const supertest = require('supertest');
          // eslint-disable-next-line global-require
          const { app: freshApp } = require('../src/server');
          const responses = await Promise.all([
            supertest(freshApp).get('/oauth/discord/install'),
            supertest(freshApp).get('/oauth/discord/callback?code=ok-code&guild_id=123456789012345678'),
          ]);
          for (const res of responses) {
            expect(res.status).toBe(503);
            // C.4: generic copy on the wire; reason logged only.
            expect(res.text).toMatch(/not configured/i);
            expect(res.text).not.toMatch(/AUTH0_[A-Z_]+/);
            expect(res.text).not.toMatch(/DISCORD_CLIENT_ID/);
            expect(res.text).not.toMatch(/DISCORD_CLIENT_SECRET/);
            expect(res.text).not.toMatch(/Reason:/i);
            expect(cookieValue(
              res.headers['set-cookie'],
              DISCORD_INSTALL_SESSION_COOKIE,
            )).toBeNull();
          }
        });
      } finally {
        process.env[missingKey] = saved;
      }
    },
  );

  it.each(['PLACEHOLDER', ' PLACEHOLDER ', 'PLACEHOLDER\n'])(
    'returns 503 while DISCORD_CLIENT_SECRET is the infrastructure placeholder (%j)',
    async (placeholder) => {
      const saved = process.env.DISCORD_CLIENT_SECRET;
      process.env.DISCORD_CLIENT_SECRET = placeholder;
      try {
        await jest.isolateModulesAsync(async () => {
          jest.doMock('../src/discord', () => ({
            sendDM: jest.fn().mockResolvedValue(true),
            assignContributorRole: jest.fn(),
            notifyPRMerge: jest.fn(),
            notifyBadgeEarned: jest.fn(),
          }));
          jest.doMock('../src/store', () => ({
            setGuildApiKey: jest.fn(),
            getGuildApiKey: jest.fn(),
            getPendingLink: jest.fn(),
            consumePendingLink: jest.fn(),
          }));
          jest.doMock('../src/commands', () => ({
            verifyStateBinding: jest.fn().mockReturnValue(true),
            handleCommand: jest.fn(),
            commands: [],
            registerCommands: jest.fn(),
          }));
          // eslint-disable-next-line global-require
          const supertest = require('supertest');
          // eslint-disable-next-line global-require
          const { app: freshApp } = require('../src/server');

          const res = await supertest(freshApp).get('/oauth/discord/install');

          expect(res.status).toBe(503);
          expect(res.text).toMatch(/not configured/i);
          expect(res.text).not.toContain('PLACEHOLDER');
        });
      } finally {
        process.env.DISCORD_CLIENT_SECRET = saved;
      }
    },
  );

  it('normalizes whitespace around a valid Discord client ID', async () => {
    const saved = process.env.DISCORD_CLIENT_ID;
    process.env.DISCORD_CLIENT_ID = ' 234567890123456789\n';
    try {
      await jest.isolateModulesAsync(async () => {
        // eslint-disable-next-line global-require
        const freshConfig = require('../src/config');
        expect(freshConfig.isDiscordInstallConfigured).toBe(true);
        expect(freshConfig.DISCORD_CLIENT_ID).toBe('234567890123456789');
      });
    } finally {
      process.env.DISCORD_CLIENT_ID = saved;
    }
  });

  it('returns 503 when a non-local HTTP BASE_URL cannot retain the Secure install cookie', async () => {
    const saved = process.env.BASE_URL;
    process.env.BASE_URL = 'http://staging.example.com';
    try {
      await jest.isolateModulesAsync(async () => {
        jest.doMock('../src/discord', () => ({
          sendDM: jest.fn().mockResolvedValue(true),
          assignContributorRole: jest.fn(),
          notifyPRMerge: jest.fn(),
          notifyBadgeEarned: jest.fn(),
        }));
        jest.doMock('../src/store', () => ({
          setGuildApiKey: jest.fn(),
          getGuildApiKey: jest.fn(),
          getPendingLink: jest.fn(),
          consumePendingLink: jest.fn(),
        }));
        jest.doMock('../src/commands', () => ({
          verifyStateBinding: jest.fn().mockReturnValue(true),
          handleCommand: jest.fn(),
          commands: [],
          registerCommands: jest.fn(),
        }));
        // eslint-disable-next-line global-require
        const freshConfig = require('../src/config');
        expect(freshConfig.discordInstallNotConfiguredReason)
          .toBe('BASE_URL cannot retain the Secure install cookie');
        // eslint-disable-next-line global-require
        const supertest = require('supertest');
        // eslint-disable-next-line global-require
        const { app: freshApp } = require('../src/server');

        const res = await supertest(freshApp).get('/oauth/discord/install');

        expect(res.status).toBe(503);
        expect(cookieValue(res.headers['set-cookie'], DISCORD_INSTALL_SESSION_COOKIE)).toBeNull();
        expect(res.text).not.toContain('BASE_URL');
      });
    } finally {
      process.env.BASE_URL = saved;
    }
  });

  it.each([
    ['PLACEHOLDER', 'DISCORD_CLIENT_ID is the SSM placeholder'],
    [' PLACEHOLDER ', 'DISCORD_CLIENT_ID is the SSM placeholder'],
    ['PLACEHOLDER\n', 'DISCORD_CLIENT_ID is the SSM placeholder'],
    ['test-discord-client-id', 'DISCORD_CLIENT_ID is not a valid Discord snowflake'],
  ])(
    'returns 503 while DISCORD_CLIENT_ID is invalid (%j)',
    async (invalidClientId, expectedReason) => {
      const saved = process.env.DISCORD_CLIENT_ID;
      process.env.DISCORD_CLIENT_ID = invalidClientId;
      try {
        await jest.isolateModulesAsync(async () => {
          jest.doMock('../src/discord', () => ({
            sendDM: jest.fn().mockResolvedValue(true),
            assignContributorRole: jest.fn(),
            notifyPRMerge: jest.fn(),
            notifyBadgeEarned: jest.fn(),
          }));
          jest.doMock('../src/store', () => ({
            setGuildApiKey: jest.fn(),
            getGuildApiKey: jest.fn(),
            getPendingLink: jest.fn(),
            consumePendingLink: jest.fn(),
          }));
          jest.doMock('../src/commands', () => ({
            verifyStateBinding: jest.fn().mockReturnValue(true),
            handleCommand: jest.fn(),
            commands: [],
            registerCommands: jest.fn(),
          }));
          // eslint-disable-next-line global-require
          const freshConfig = require('../src/config');
          expect(freshConfig.discordInstallNotConfiguredReason)
            .toBe(expectedReason);
          expect(freshConfig.DISCORD_CLIENT_ID).toBeNull();
          // eslint-disable-next-line global-require
          const supertest = require('supertest');
          // eslint-disable-next-line global-require
          const { app: freshApp } = require('../src/server');

          const res = await supertest(freshApp).get('/oauth/discord/install');

          expect(res.status).toBe(503);
          expect(res.text).toMatch(/not configured/i);
          expect(res.text).not.toContain(invalidClientId.trim());
        });
      } finally {
        process.env.DISCORD_CLIENT_ID = saved;
      }
    },
  );
});
