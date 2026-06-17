// Tests for src/routes/discord-install.js — the Stage-2 "Add to Discord"
// install callback that chains the Discord OAuth2 install to the qURL
// Auth0 leg. Covers:
//   - 503 not-configured response (DISCORD_CLIENT_SECRET or AUTH0_* unset)
//   - 400 missing code, missing guild_id, declined consent
//   - 502 Discord token exchange failure, /users/@me failure
//   - 302 happy path: redirects to Auth0 with a qURL OAuth state binding
//     guild_id + discord_user_id

process.env.OAUTH_STATE_SECRET = '0'.repeat(64);
// KEY_ENCRYPTION_KEY required for the fail-fast guard added in PR #177
// review round 3; matches the legacy modal-paste path's existing check.
process.env.KEY_ENCRYPTION_KEY = '1'.repeat(64);
process.env.AUTH0_DOMAIN = 'layerv-test.auth0.com';
process.env.AUTH0_CLIENT_ID = 'test-auth0-client-id';
process.env.AUTH0_CLIENT_SECRET = 'test-auth0-secret';
process.env.AUTH0_AUDIENCE = 'https://api.layerv.test';
process.env.DISCORD_CLIENT_ID = 'test-discord-client-id';
process.env.DISCORD_CLIENT_SECRET = 'test-discord-secret';
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
  // Default: no prior config — Stage 2 is normally a first install.
  // Re-install path (prior `configured_by` present → prompt=consent set
  // on the chained Auth0 redirect) gets its own test below.
  getGuildConfig: jest.fn().mockResolvedValue(undefined),
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
const db = require('../src/store');
const { verifyQurlOAuthState } = require('../src/utils/qurl-oauth-state');
const {
  QURL_OAUTH_SESSION_COOKIE,
  QURL_OAUTH_PKCE_COOKIE,
} = require('../src/utils/oauth-cookies');
const { pkceChallengeForVerifier } = require('../src/utils/oauth-pkce');

const originalFetch = globalThis.fetch;

function extractStyleNonce(res) {
  const csp = res.headers['content-security-policy'];
  expect(csp).toBeDefined();
  expect(csp).not.toContain('unsafe-inline');

  const nonceMatch = csp.match(/style-src 'nonce-([A-Za-z0-9_-]+)'/);
  expect(nonceMatch).not.toBeNull();
  return nonceMatch[1];
}

function cookieValue(setCookie, name) {
  const header = Array.isArray(setCookie) ? setCookie.join('\n') : setCookie || '';
  const match = header.match(new RegExp(`${name}=([^;\\n]+)`));
  return match ? decodeURIComponent(match[1]) : null;
}

beforeEach(() => {
  jest.clearAllMocks();
  globalThis.fetch = originalFetch;
});

describe('Discord install callback', () => {
  describe('GET /oauth/discord/callback', () => {
    it('503s when KEY_ENCRYPTION_KEY is unset (fail-fast before Discord token exchange)', async () => {
      // Bot is in the server already (Discord install ran), but the
      // chained Auth0 leg can't safely proceed without encryption-at-
      // rest configured. Failing here saves the Discord code from
      // being burned on a doomed flow.
      const saved = process.env.KEY_ENCRYPTION_KEY;
      delete process.env.KEY_ENCRYPTION_KEY;
      try {
        const res = await request(app).get('/oauth/discord/callback?code=ok-code&guild_id=guild-1');
        expect(res.status).toBe(503);
        expect(res.text).toMatch(/encryption-at-rest|KEY_ENCRYPTION_KEY/i);
      } finally {
        process.env.KEY_ENCRYPTION_KEY = saved;
      }
    });

    it('400s on missing code', async () => {
      const res = await request(app).get('/oauth/discord/callback?guild_id=guild-1');
      expect(res.status).toBe(400);
      expect(res.text).toContain('Missing authorization code');
    });

    it('uses one CSP nonce in the HTTP header and style tag', async () => {
      const res = await request(app).get('/oauth/discord/callback?guild_id=guild-1');
      expect(res.status).toBe(400);

      const nonce = extractStyleNonce(res);
      // 16 random bytes encoded as unpadded base64url.
      expect(nonce).toHaveLength(22);

      expect(res.text).toContain(`<style nonce="${nonce}">`);
      expect(res.text).not.toContain('Content-Security-Policy');
      expect(res.text).not.toContain('unsafe-inline');
    });

    it('generates a fresh CSP nonce for each response', async () => {
      const first = await request(app).get('/oauth/discord/callback?guild_id=guild-1');
      const second = await request(app).get('/oauth/discord/callback?guild_id=guild-1');

      expect(extractStyleNonce(first)).not.toBe(extractStyleNonce(second));
    });

    it('400s on missing guild_id (admin abandoned mid-install)', async () => {
      const res = await request(app).get('/oauth/discord/callback?code=disc-code');
      expect(res.status).toBe(400);
      expect(res.text).toContain('Bot install incomplete');
    });

    it('400s on Discord error param (admin declined consent)', async () => {
      const res = await request(app).get(
        '/oauth/discord/callback?error=access_denied&error_description=user+declined&guild_id=guild-1',
      );
      expect(res.status).toBe(400);
      expect(res.text).toContain('Authorization declined');
    });

    it('502s when Discord token exchange fails', async () => {
      globalThis.fetch = jest.fn().mockResolvedValueOnce({
        ok: false, status: 401, text: () => Promise.resolve('invalid_grant'),
      });
      const res = await request(app).get('/oauth/discord/callback?code=bad-code&guild_id=guild-1');
      expect(res.status).toBe(502);
      expect(res.text).toContain('Authorization failed');
    });

    it('502s when Discord /users/@me fails after successful token exchange', async () => {
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({ access_token: 'disc-token', token_type: 'Bearer' }),
        })
        .mockResolvedValueOnce({
          ok: false, status: 500,
          text: () => Promise.resolve('Discord API error'),
        });
      const res = await request(app).get('/oauth/discord/callback?code=ok-code&guild_id=guild-1');
      expect(res.status).toBe(502);
      expect(res.text).toContain('Could not identify the installing user');
    });

    it('302s to Auth0 on happy path with a valid qURL OAuth state and sets the CSRF cookie', async () => {
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({ access_token: 'disc-token', token_type: 'Bearer' }),
        })
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({ id: '987654321098765432', username: 'admin' }),
        });
      const res = await request(app).get('/oauth/discord/callback?code=ok-code&guild_id=guild-1');
      expect(res.status).toBe(302);
      const loc = new URL(res.headers.location);
      expect(loc.host).toBe('layerv-test.auth0.com');
      expect(loc.pathname).toBe('/authorize');
      expect(loc.searchParams.get('client_id')).toBe('test-auth0-client-id');
      expect(loc.searchParams.get('redirect_uri')).toBe('http://localhost:3000/oauth/qurl/callback');
      // Auth0 scope must NOT include offline_access (refresh tokens not
      // stored/used; dropped per PR #177 review item 5).
      expect(loc.searchParams.get('scope')).not.toContain('offline_access');
      // Round-9 item #1: Stage-2 ALWAYS sets prompt=consent (independent
      // of first-install vs re-install). Stage-2 is the URL-forwarding
      // attack surface (forwarded /oauth/discord/callback → confused
      // deputy); the explicit consent screen is one extra defense
      // gate before the qURL key is bound to the admin's account.
      expect(loc.searchParams.get('prompt')).toBe('consent');

      // The state Discord callback minted must round-trip through the
      // qURL OAuth state verifier with the right guild + discord-user
      // bindings — that's how the Auth0 callback identifies who set
      // up which guild.
      const state = loc.searchParams.get('state');
      const verified = verifyQurlOAuthState(state);
      expect(verified.ok).toBe(true);
      expect(verified.payload.guildId).toBe('guild-1');
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

    it('sets Secure flag on the cookie when behind a proxy that sets X-Forwarded-Proto: https', async () => {
      // Defense vs trust-proxy regression: server.js sets `trust proxy`
      // so req.protocol reflects X-Forwarded-Proto from the ALB. Flipping
      // that off would silently downgrade prod cookies to insecure. Pin
      // the wire-level shape here.
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({ access_token: 'disc-token' }),
        })
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({ id: '987654321098765432' }),
        });
      const res = await request(app)
        .get('/oauth/discord/callback?code=ok-code&guild_id=guild-1')
        .set('X-Forwarded-Proto', 'https');
      expect(res.status).toBe(302);
      const cookieHeader = (Array.isArray(res.headers['set-cookie']) ? res.headers['set-cookie'].join('\n') : res.headers['set-cookie']) || '';
      expect(cookieHeader).toMatch(/Secure/);
    });

    it('still sets prompt=consent on re-install (guild already has a configured_by) — Stage-2 always prompts', async () => {
      // Round-9 item #1 says always-on for Stage-2; this test pins
      // the re-install branch produces the same redirect shape as
      // first-install. Mock returns a prior config row to exercise
      // the previouslyConfigured=true code path.
      db.getGuildConfig.mockResolvedValueOnce({ guild_id: 'guild-1', configured_by: '111' });
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({ access_token: 'disc-token' }),
        })
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({ id: '987654321098765432' }),
        });
      const res = await request(app).get('/oauth/discord/callback?code=ok-code&guild_id=guild-1');
      expect(res.status).toBe(302);
      const loc = new URL(res.headers.location);
      expect(loc.searchParams.get('prompt')).toBe('consent');
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
          json: () => Promise.resolve({ access_token: 'disc-token' }),
        })
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({ id: '987654321098765432' }),
        });
      const stage2 = await request(app).get('/oauth/discord/callback?code=ok-code&guild_id=guild-1');
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
          json: () => Promise.resolve({ access_token: 'disc-token' }),
        })
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({ id: '111' }),
        });
      globalThis.fetch = fetchSpy;
      await request(app).get('/oauth/discord/callback?code=ok-code&guild_id=guild-1');
      expect(fetchSpy).toHaveBeenCalled();
      const tokenCall = fetchSpy.mock.calls[0];
      expect(tokenCall[0]).toBe('https://discord.com/api/oauth2/token');
      expect(tokenCall[1].method).toBe('POST');
      expect(tokenCall[1].headers['Content-Type']).toBe('application/x-www-form-urlencoded');
      const bodyParams = new URLSearchParams(tokenCall[1].body.toString());
      expect(bodyParams.get('client_id')).toBe('test-discord-client-id');
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
        const res = await supertest(freshApp).get('/oauth/discord/callback?code=ok-code&guild_id=guild-1');
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
      });
    } finally {
      Object.assign(process.env, saved);
    }
  });

  it('returns 503 with the DISCORD_CLIENT_SECRET-unset reason when Auth0 is set but Discord secret is missing', async () => {
    const saved = process.env.DISCORD_CLIENT_SECRET;
    delete process.env.DISCORD_CLIENT_SECRET;
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
        const res = await supertest(freshApp).get('/oauth/discord/callback?code=ok-code&guild_id=guild-1');
        expect(res.status).toBe(503);
        // C.4: generic copy on the wire; reason logged only.
        expect(res.text).toMatch(/not configured/i);
        expect(res.text).not.toMatch(/AUTH0_[A-Z_]+/);
        expect(res.text).not.toMatch(/DISCORD_CLIENT_SECRET/);
        expect(res.text).not.toMatch(/Reason:/i);
      });
    } finally {
      process.env.DISCORD_CLIENT_SECRET = saved;
    }
  });
});
