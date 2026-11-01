
process.env.AUTH0_DOMAIN = 'layerv-test.auth0.com';
process.env.AUTH0_CLIENT_ID = 'test-client-id';
process.env.AUTH0_CLIENT_SECRET = 'test-client-secret';
process.env.AUTH0_AUDIENCE = 'https://api.layerv.test';
process.env.QURL_ENDPOINT = 'http://localhost:9999';
process.env.BASE_URL = 'http://localhost:3000';
process.env.MAP_COMMAND_ENABLED = 'false';
process.env.KEY_ENCRYPTION_KEY = '1'.repeat(64);
process.env.GUILD_ID = '123456789012345678';
process.env.TRUST_PROXY = '1';

jest.mock('../src/discord-rest', () => ({
  ...jest.requireActual('../src/discord-rest'), getGuildSetupInstallation: jest.fn(),
}));

jest.mock('../src/discord', () => ({
  sendDM: jest.fn().mockResolvedValue(true),
  assignContributorRole: jest.fn(),
  notifyPRMerge: jest.fn(),
  notifyBadgeEarned: jest.fn(),
}));

jest.mock('../src/logger', () => ({
  info: jest.fn(), warn: jest.fn(), error: jest.fn(), debug: jest.fn(), audit: jest.fn(),
}));

jest.mock('../src/store', () => ({
  setGuildApiKey: jest.fn().mockResolvedValue(undefined),
  getGuildConfigWithApiKey: jest.fn().mockResolvedValue(undefined),
  getGuildApiKey: jest.fn(),
  getGuildConfig: jest.fn(),
  getPendingLink: jest.fn(),
  consumePendingLink: jest.fn(),
}));

jest.mock('../src/guild-webhook-link', () => ({
  linkGuildWebhookSubscriptionForSetup: jest.fn().mockResolvedValue(true),
}));

// commands.js still requires verifyStateBinding for the GitHub OAuth route;
// stub it to avoid pulling in the full command tree at module load.
jest.mock('../src/commands', () => ({
  verifyStateBinding: jest.fn().mockReturnValue(true),
  handleCommand: jest.fn(),
  commands: [],
  registerCommands: jest.fn(),
}));

jest.mock('../src/utils/auth0-jwks', () => ({
  verifyAuth0IdToken: jest.fn().mockResolvedValue({
    ok: true, payload: { email: 'alice@layerv.test', sub: 'auth0|abc' },
  }),
}));

const request = require('supertest');
const crypto = require('crypto');
const { app } = require('../src/server');
const config = require('../src/config');
const db = require('../src/store');
const discord = require('../src/discord');
const logger = require('../src/logger');
const guildWebhookLink = require('../src/guild-webhook-link');
const { AUDIT_EVENTS, GUILD_WEBHOOK_PENDING_NOTICE } = require('../src/constants');
const { signQurlOAuthState, signQurlOAuthRedirectState, verifyQurlOAuthState } = require('../src/utils/qurl-oauth-state');
const {
  QURL_OAUTH_SESSION_COOKIE,
  QURL_OAUTH_PKCE_COOKIE,
} = require('../src/utils/oauth-cookies');
const { pkceChallengeForVerifier } = require('../src/utils/oauth-pkce');
const { clearedCookieHeader, cookieValue } = require('./helpers/cookies');
const { rateLimitStore } = require('../src/utils/oauth-rate-limit');

function signCallbackState(guildId, userId) {
  return signQurlOAuthRedirectState(signQurlOAuthState(guildId, userId));
}

const originalFetch = globalThis.fetch;
const TEST_PKCE_VERIFIER = 'a'.repeat(43);
const SUBJECT_FINGERPRINT_SECRET = process.env.QURL_OAUTH_STATE_SECRET
  || process.env.OAUTH_STATE_SECRET;
const SUBJECT_FINGERPRINT_KEY = crypto.hkdfSync(
  'sha256',
  SUBJECT_FINGERPRINT_SECRET,
  Buffer.alloc(0),
  'qurl-account-fingerprint:v1',
  32,
);
const AUTH0_ABC_FINGERPRINT = crypto.createHmac('sha256', SUBJECT_FINGERPRINT_KEY)
  .update('qurl-account-subject:auth0|abc')
  .digest('hex');
const SUBJECT_FINGERPRINT_KEY_EPOCH = crypto.createHmac('sha256', SUBJECT_FINGERPRINT_KEY)
  .update('qurl-account-fingerprint-key-epoch:v1')
  .digest('hex')
  .slice(0, 12);

function cookieFor(state, codeVerifier = TEST_PKCE_VERIFIER) {
  return `${QURL_OAUTH_SESSION_COOKIE}=${encodeURIComponent(state)}; `
    + `${QURL_OAUTH_PKCE_COOKIE}=${encodeURIComponent(codeVerifier)}`;
}

function expectQurlOAuthCookiesCleared(res) {
  for (const name of [QURL_OAUTH_SESSION_COOKIE, QURL_OAUTH_PKCE_COOKIE]) {
    const clearCookie = clearedCookieHeader(res.headers['set-cookie'], name);
    expect(clearCookie).toBeDefined();
    expect(clearCookie).toMatch(/Path=\/(?:;|$)/);
  }
}

beforeEach(() => {
  jest.clearAllMocks();
  config.ENABLE_CREDENTIAL_LIFECYCLE = false;
  require('../src/discord-rest').getGuildSetupInstallation.mockReset();
  rateLimitStore.clear();
  globalThis.fetch = originalFetch;
});

describe('qurl-oauth routes', () => {
  it.each(['/start', '/callback'])('rate-limits %s before any authorization HTTP request', async (path) => {
    const previous = config.RATE_LIMIT_MAX_REQUESTS;
    config.RATE_LIMIT_MAX_REQUESTS = 1;
    globalThis.fetch = jest.fn();
    try {
      expect((await request(app).get(`/oauth/qurl${path}`)).status).toBe(400);
      expect((await request(app).get(`/oauth/qurl${path}`)).status).toBe(429);
      expect(globalThis.fetch).not.toHaveBeenCalled();
    } finally {
      config.RATE_LIMIT_MAX_REQUESTS = previous;
    }
  });

  describe('GET /oauth/qurl/start', () => {
    it('redirects to Auth0 authorize URL with the right params on valid state', async () => {
      const state = signQurlOAuthState('guild-1', 'admin-2');
      const configuredConnection = config.AUTH0_EMAIL_CONNECTION;
      config.AUTH0_EMAIL_CONNECTION = 'email';
      let res;
      try {
        res = await request(app)
          .get(`/oauth/qurl/start?state=${encodeURIComponent(state)}`);
      } finally {
        config.AUTH0_EMAIL_CONNECTION = configuredConnection;
      }
      expect(res.status).toBe(302);
      const loc = new URL(res.headers.location);
      expect(loc.host).toBe('layerv-test.auth0.com');
      expect(loc.pathname).toBe('/authorize');
      expect(loc.searchParams.get('response_type')).toBe('code');
      expect(loc.searchParams.get('client_id')).toBe('test-client-id');
      expect(loc.searchParams.get('audience')).toBe('https://api.layerv.test');
      expect(loc.searchParams.get('scope')).toContain('qurl:write');
      expect(loc.searchParams.get('scope')).toContain('qurl:read');
      expect(loc.searchParams.get('scope')).not.toContain('offline_access');
      // `login` asks Auth0 not to reuse its ambient session; `consent` is
      // load-bearing for key rotation — re-running /qurl-admin setup must actually
      // re-prompt. Pin both so a future refactor can't drop either.
      expect(loc.searchParams.get('prompt')).toBe('login consent');
      expect(loc.searchParams.get('connection')).toBe('email');
      expect(loc.searchParams.get('state')).not.toBe(state);
      expect(verifyQurlOAuthState(loc.searchParams.get('state')).payload).toMatchObject({ guildId: 'guild-1', discordUserId: 'admin-2' });
      expect(loc.searchParams.get('redirect_uri')).toBe('http://localhost:3000/oauth/qurl/callback');

      const codeVerifier = cookieValue(res.headers['set-cookie'], QURL_OAUTH_PKCE_COOKIE);
      expect(codeVerifier).not.toBeNull();
      expect(loc.searchParams.get('code_challenge_method')).toBe('S256');
      expect(loc.searchParams.get('code_challenge')).toBe(pkceChallengeForVerifier(codeVerifier));
      expect(loc.searchParams.get('code_challenge')).not.toBe(codeVerifier);
    });

    it('refreshes the sign-in clock without extending the setup link lifetime', async () => {
      const began = Date.now();
      const clock = jest.spyOn(Date, 'now').mockReturnValue(began);
      try {
        const state = signQurlOAuthState('guild-1', 'admin-2');
        const expiry = verifyQurlOAuthState(state).payload.expirySec;
        clock.mockReturnValue(began + 60_000);
        const first = await request(app).get(`/oauth/qurl/start?state=${encodeURIComponent(state)}`);
        const renewed = new URL(first.headers.location).searchParams.get('state');
        expect(verifyQurlOAuthState(renewed).payload).toMatchObject({
          expirySec: expiry, authStartedAt: Math.floor((began + 60_000) / 1000),
        });
        clock.mockReturnValue(began + 120_000);
        const second = await request(app).get(`/oauth/qurl/start?state=${encodeURIComponent(renewed)}`);
        const twice = new URL(second.headers.location).searchParams.get('state');
        expect(verifyQurlOAuthState(twice).payload.expirySec).toBe(expiry);
        clock.mockReturnValue((expiry + 1) * 1000);
        expect((await request(app).get(`/oauth/qurl/start?state=${encodeURIComponent(twice)}`)).status).toBe(400);
      } finally {
        clock.mockRestore();
      }
    });

    it('shows an expired-link response if the clock crosses expiry before re-signing', async () => {
      const began = Date.now();
      const state = signQurlOAuthState('guild-1', 'admin-2');
      const expiry = verifyQurlOAuthState(state).payload.expirySec * 1000;
      let checks = 0;
      const clock = jest.spyOn(Date, 'now').mockImplementation(() => {
        checks += 1;
        return checks < 3 ? began : expiry;
      });
      try {
        expect((await request(app).get(`/oauth/qurl/start?state=${encodeURIComponent(state)}`)).status).toBe(400);
      } finally {
        clock.mockRestore();
      }
    });

    it('sets Secure flag on the cookie when behind a proxy that sets X-Forwarded-Proto: https', async () => {
      const state = signQurlOAuthState('guild-1', 'admin-2');
      const res = await request(app)
        .get(`/oauth/qurl/start?state=${encodeURIComponent(state)}`)
        .set('X-Forwarded-Proto', 'https');
      expect(res.status).toBe(302);
      const cookieHeader = (Array.isArray(res.headers['set-cookie']) ? res.headers['set-cookie'].join('\n') : res.headers['set-cookie']) || '';
      expect(cookieHeader).toMatch(/Secure/);
    });

    it('sets a HttpOnly session cookie binding the browser to this state (CSRF guard)', async () => {
      const state = signQurlOAuthState('guild-1', 'admin-2');
      const res = await request(app).get(`/oauth/qurl/start?state=${encodeURIComponent(state)}`);
      expect(res.status).toBe(302);
      const setCookie = res.headers['set-cookie'];
      expect(setCookie).toBeDefined();
      const cookieHeader = Array.isArray(setCookie) ? setCookie.join('\n') : setCookie;
      expect(cookieHeader).toMatch(/qurl_setup_session=/);
      expect(cookieHeader).toMatch(/HttpOnly/i);
      expect(cookieHeader).toMatch(/SameSite=Lax/i);
      expect(cookieHeader).toMatch(/Path=\/(?:;|\s|$)/);
      expect(cookieHeader).toContain(encodeURIComponent(new URL(res.headers.location).searchParams.get('state')));
    });

    it('400s on missing state', async () => {
      const res = await request(app).get('/oauth/qurl/start');
      expect(res.status).toBe(400);
      expect(res.text).toContain('Invalid setup link');
    });

    it('503s when KEY_ENCRYPTION_KEY is unset (fail-fast before Auth0 round-trip)', async () => {
      const saved = process.env.KEY_ENCRYPTION_KEY;
      delete process.env.KEY_ENCRYPTION_KEY;
      try {
        const { signQurlOAuthState: sign, signQurlOAuthRedirectState: redirect } = require('../src/utils/qurl-oauth-state');
        const state = redirect(sign('guild-1', 'admin-2'));
        const res = await request(app).get(`/oauth/qurl/start?state=${encodeURIComponent(state)}`);
        expect(res.status).toBe(503);
        expect(res.text).toMatch(/qURL setup not provisioned|encryption-at-rest/i);
      } finally {
        process.env.KEY_ENCRYPTION_KEY = saved;
      }
    });

    it('emits Cache-Control: no-store on every /oauth/qurl/* response (router-level pin)', async () => {
      const state = signQurlOAuthState('guild-1', 'admin-2');
      const start = await request(app).get(`/oauth/qurl/start?state=${encodeURIComponent(state)}`);
      expect(start.headers['cache-control']).toBe('no-store');
      const startBad = await request(app).get('/oauth/qurl/start');
      expect(startBad.headers['cache-control']).toBe('no-store');
    });

    it('rejects array-shaped state query (?state=a&state=b) via singleStringParam', async () => {
      const res = await request(app).get('/oauth/qurl/start?state=alpha&state=beta');
      expect(res.status).toBe(400);
      expect(res.text).toContain('Invalid setup link');
    });

    it('400s on tampered state', async () => {
      const state = signQurlOAuthState('guild-1', 'admin-2');
      const tampered = state.slice(0, -1) + (state.slice(-1) === '0' ? '1' : '0');
      const res = await request(app).get(`/oauth/qurl/start?state=${encodeURIComponent(tampered)}`);
      expect(res.status).toBe(400);
    });

    it('forces login + consent without reading guild config', async () => {
      const state = signQurlOAuthState('guild-fresh', 'admin-fresh');
      const res = await request(app).get(`/oauth/qurl/start?state=${encodeURIComponent(state)}`);
      expect(res.status).toBe(302);
      const loc = new URL(res.headers.location);
      expect(loc.searchParams.get('prompt')).toBe('login consent');
      expect(loc.searchParams.get('connection')).toBeNull();
      expect(db.getGuildConfig).not.toHaveBeenCalled();
    });
  });

  describe('GET /oauth/qurl/callback', () => {
    it('503s and clears cookies when KEY_ENCRYPTION_KEY is unset', async () => {
      const saved = process.env.KEY_ENCRYPTION_KEY;
      delete process.env.KEY_ENCRYPTION_KEY;
      try {
        const state = signCallbackState('guild-1', 'admin-2');
        const res = await request(app)
          .get(`/oauth/qurl/callback?code=auth0-code&state=${encodeURIComponent(state)}`)
          .set('Cookie', cookieFor(state));
        expect(res.status).toBe(503);
        expect(res.text).toMatch(/qURL setup not provisioned|encryption-at-rest/i);
        expectQurlOAuthCookiesCleared(res);
      } finally {
        process.env.KEY_ENCRYPTION_KEY = saved;
      }
    });

    it('400s on missing code', async () => {
      const state = signCallbackState('guild-1', 'admin-2');
      const res = await request(app).get(`/oauth/qurl/callback?state=${encodeURIComponent(state)}`)
        .set('Cookie', cookieFor(state));
      expect(res.status).toBe(400);
      expect(res.text).toContain('Missing authorization code');
      expectQurlOAuthCookiesCleared(res);
    });

    it('400s on Auth0 error param (admin declined consent)', async () => {
      const state = signCallbackState('guild-1', 'admin-2');
      const res = await request(app).get(
        `/oauth/qurl/callback?state=${encodeURIComponent(state)}&error=access_denied&error_description=user+declined`,
      ).set('Cookie', cookieFor(state));
      expect(res.status).toBe(400);
      expect(res.text).toContain('Authorization declined');
      expectQurlOAuthCookiesCleared(res);
    });

    it('400s on invalid state', async () => {
      const res = await request(app).get('/oauth/qurl/callback?code=auth0-code&state=garbage');
      expect(res.status).toBe(400);
      expectQurlOAuthCookiesCleared(res);
    });

    it('400s on missing PKCE verifier cookie', async () => {
      const state = signCallbackState('guild-1', 'admin-2');
      const res = await request(app)
        .get(`/oauth/qurl/callback?code=auth0-code&state=${encodeURIComponent(state)}`)
        .set('Cookie', `${QURL_OAUTH_SESSION_COOKIE}=${encodeURIComponent(state)}`);
      expect(res.status).toBe(400);
      expect(res.text).toMatch(/could not be completed/i);
      expectQurlOAuthCookiesCleared(res);
    });

    it('400s on missing CSRF cookie (leaked URL opened in different browser)', async () => {
      const state = signCallbackState('guild-1', 'admin-2');
      const res = await request(app).get(
        `/oauth/qurl/callback?code=auth0-code&state=${encodeURIComponent(state)}`,
      );
      expect(res.status).toBe(400);
      expect(res.text).toMatch(/same browser tab/i);
      expectQurlOAuthCookiesCleared(res);
    });

    it('rejects unprefixed cookies that a sibling domain could plant', async () => {
      const state = signCallbackState('guild-1', 'admin-2');
      const res = await request(app)
        .get(`/oauth/qurl/callback?code=auth0-code&state=${encodeURIComponent(state)}`)
        .set('Cookie', `qurl_setup_session=${encodeURIComponent(state)}; qurl_setup_pkce=${TEST_PKCE_VERIFIER}`);
      expect(res.status).toBe(400);
      expectQurlOAuthCookiesCleared(res);
    });

    it('cookie value URL-decodes to the same state used in the timingSafeEqual compare (round-9 #8 follow-up)', async () => {
      const state = signCallbackState('guild-1', 'admin-2');
      expect(encodeURIComponent(state)).toBe(state);
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({ access_token: 'jwt-xyz', id_token: 'verified.jwt.sig' }),
        })
        .mockResolvedValueOnce({
          ok: true, status: 201,
          json: () => Promise.resolve({ data: { key_id: 'key-1', api_key: 'lv_live_abc', key_prefix: 'lv_live_a' } }),
        });
      const res = await request(app)
        .get(`/oauth/qurl/callback?code=auth0-code&state=${encodeURIComponent(state)}`)
        .set('Cookie', cookieFor(state));
      expect(res.status).toBe(200);
      expect(res.text).toContain('qURL is connected');
    });

    it('400s on cookie/state mismatch (cookie from a different state)', async () => {
      const stateA = signCallbackState('guild-1', 'admin-2');
      const stateB = signCallbackState('guild-1', 'admin-2'); // different nonce → different state
      const res = await request(app).get(
        `/oauth/qurl/callback?code=auth0-code&state=${encodeURIComponent(stateA)}`,
      ).set('Cookie', cookieFor(stateB));
      expect(res.status).toBe(400);
      expectQurlOAuthCookiesCleared(res);
    });

    it('502s when Auth0 token exchange fails', async () => {
      const state = signCallbackState('guild-1', 'admin-2');
      globalThis.fetch = jest.fn().mockResolvedValueOnce({
        ok: false,
        status: 401,
        text: () => Promise.resolve('unauthorized client: reflected-secret'),
      });
      const res = await request(app).get(
        `/oauth/qurl/callback?code=auth0-code&state=${encodeURIComponent(state)}`,
      ).set('Cookie', cookieFor(state));
      expect(res.status).toBe(502);
      expect(res.text).toContain('Authorization failed');
      expect(logger.error).toHaveBeenCalledWith('Auth0 token exchange failed', { status: 401 });
      expect(JSON.stringify(logger.error.mock.calls)).not.toContain('reflected-secret');
      expect(db.setGuildApiKey).not.toHaveBeenCalled();
    });

    it('sends the PKCE verifier cookie on the Auth0 token exchange', async () => {
      const state = signCallbackState('guild-1', 'admin-2');
      const codeVerifier = 'b'.repeat(43);
      const fetchSpy = jest.fn()
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({ access_token: 'jwt-xyz', id_token: 'verified.jwt.sig', token_type: 'Bearer', expires_in: 3600 }),
        })
        .mockResolvedValueOnce({
          ok: true, status: 201,
          json: () => Promise.resolve({ data: { key_id: 'key-1', api_key: 'lv_live_abc', key_prefix: 'lv_live_a' } }),
        });
      globalThis.fetch = fetchSpy;
      const res = await request(app).get(
        `/oauth/qurl/callback?code=auth0-code&state=${encodeURIComponent(state)}`,
      ).set('Cookie', cookieFor(state, codeVerifier));
      expect(res.status).toBe(200);
      const tokenBody = new URLSearchParams(fetchSpy.mock.calls[0][1].body.toString());
      expect(tokenBody.get('code_verifier')).toBe(codeVerifier);
      expect(tokenBody.get('redirect_uri')).toBe('http://localhost:3000/oauth/qurl/callback');
    });

    it('keeps an infrastructure failure separate from an owner mismatch', async () => {
      db.getGuildConfigWithApiKey.mockRejectedValueOnce(new Error('DDB unavailable'));
      const state = signCallbackState('guild-1', 'admin-2');
      globalThis.fetch = jest.fn().mockResolvedValueOnce({
        ok: true, json: async () => ({ access_token: 'jwt-xyz', id_token: 'verified.jwt.sig' }),
      });
      const res = await request(app).get(`/oauth/qurl/callback?code=code&state=${encodeURIComponent(state)}`)
        .set('Cookie', cookieFor(state));
      expect(res.status).toBe(503);
      expect(res.text).toContain('temporarily unavailable');
      expect(globalThis.fetch).toHaveBeenCalledTimes(1);
      expect(db.setGuildApiKey).not.toHaveBeenCalled();
    });

    it('refuses an initial setup token that was never issued for a redirect', async () => {
      const state = signQurlOAuthState('guild-1', 'admin-2');
      globalThis.fetch = jest.fn();
      const res = await request(app).get(`/oauth/qurl/callback?code=code&state=${encodeURIComponent(state)}`)
        .set('Cookie', cookieFor(state));
      expect(res.status).toBe(400);
      expect(globalThis.fetch).not.toHaveBeenCalled();
    });

    it('migrates a legacy saved key through the real service identity reader', async () => {
      db.getGuildConfigWithApiKey.mockResolvedValueOnce({ qurl_api_key: 'legacy-saved-key' });
      const state = signCallbackState('guild-1', 'admin-2');
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({ ok: true, json: async () => ({ access_token: 'jwt-xyz', id_token: 'verified.jwt.sig' }) })
        .mockResolvedValueOnce({ ok: true, json: async () => ({ data: {
          owner_id: 'auth0|abc', auth_type: 'api_key', api_key: { key_id: 'legacy-key-id', scopes: ['qurl:read', 'qurl:write'] },
        } }) })
        .mockResolvedValueOnce({ ok: true, json: async () => ({ data: { api_key: 'new-saved-key', key_id: 'new-key-id' } }) });
      const res = await request(app).get(`/oauth/qurl/callback?code=code&state=${encodeURIComponent(state)}`)
        .set('Cookie', cookieFor(state));
      expect(res.status).toBe(200);
      expect(globalThis.fetch.mock.calls[1][0]).toBe('http://localhost:9999/v1/me');
      expect(globalThis.fetch.mock.calls[1][1].headers.Authorization).toBe('Bearer legacy-saved-key');
      expect(db.setGuildApiKey).toHaveBeenCalledWith('guild-1', 'new-saved-key', 'admin-2', 'oauth', {
        ownerId: 'auth0|abc', expectedApiKey: 'legacy-saved-key', keyId: 'new-key-id', keyKind: 'dedicated',
      });
    });

    it('rejects a different stored owner before minting a key', async () => {
      db.getGuildConfigWithApiKey.mockResolvedValueOnce({ qurl_owner_id: 'another-owner', qurl_api_key: 'saved' });
      const state = signCallbackState('guild-1', 'admin-2');
      globalThis.fetch = jest.fn().mockResolvedValueOnce({
        ok: true, json: async () => ({ access_token: 'jwt-xyz', id_token: 'verified.jwt.sig' }),
      });
      const res = await request(app).get(`/oauth/qurl/callback?code=code&state=${encodeURIComponent(state)}`)
        .set('Cookie', cookieFor(state));
      expect(res.status).toBe(409);
      expect(globalThis.fetch).toHaveBeenCalledTimes(1);
      expect(db.setGuildApiKey).not.toHaveBeenCalled();
    });

    it('502s when qurl-service mint fails', async () => {
      const state = signCallbackState('guild-1', 'admin-2');
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({ access_token: 'jwt-xyz', id_token: 'verified.jwt.sig', token_type: 'Bearer', expires_in: 3600 }),
        })
        .mockResolvedValueOnce({
          ok: false, status: 500,
          text: () => Promise.resolve('internal error'),
        });
      const res = await request(app).get(
        `/oauth/qurl/callback?code=auth0-code&state=${encodeURIComponent(state)}`,
      ).set('Cookie', cookieFor(state));
      expect(res.status).toBe(502);
      expect(res.text).toContain('Could not provision qURL key');
      expect(db.setGuildApiKey).not.toHaveBeenCalled();
    });

    it.each([
      [403, 'email_verification_required', 'Verify your qURL account email'],
      [403, 'account_frozen', 'Your qURL account is frozen'],
      [403, 'scope_delegation_denied', 'does not permit the required qURL permissions'],
      [401, 'invalid_audience', 'bot operator must correct the Auth0 audience'],
      [401, 'insufficient_scope', 'Sign in again and grant qURL read and write access'],
    ])('shows a safe next step for mint failure %s/%s', async (status, code, expected) => {
      const state = signCallbackState('guild-1', 'admin-2');
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({ ok: true, json: async () => ({ access_token: 'jwt-xyz', id_token: 'verified.jwt.sig' }) })
        .mockResolvedValueOnce({ ok: false, status, text: async () => JSON.stringify({ error: { code, detail: 'PRIVATE upstream detail' } }) });
      const res = await request(app).get(`/oauth/qurl/callback?code=auth0-code&state=${encodeURIComponent(state)}`)
        .set('Cookie', cookieFor(state));
      expect(res.status).toBe(status);
      expect(res.text).toContain(expected);
      expect(res.text).not.toContain('PRIVATE upstream detail');
      expect(db.setGuildApiKey).not.toHaveBeenCalled();
      expect(discord.sendDM).not.toHaveBeenCalled();
    });

    it('rejects a former server admin before minting a key even after successful Auth0 login', async () => {
      config.ENABLE_CREDENTIAL_LIFECYCLE = true;
      require('../src/discord-rest').getGuildSetupInstallation.mockResolvedValue({ ok: false, reason: 'not_admin' });
      const state = signCallbackState('guild-1', 'admin-2');
      globalThis.fetch = jest.fn().mockResolvedValueOnce({ ok: true,
        json: async () => ({ access_token: 'jwt', id_token: 'verified.jwt.sig' }) });
      const res = await request(app).get(`/oauth/qurl/callback?code=grant&state=${encodeURIComponent(state)}`)
        .set('Cookie', cookieFor(state));
      expect(res.status).toBe(403);
      expect(res.text).toContain('Manage Server permission');
      expect(globalThis.fetch).toHaveBeenCalledTimes(1);
      expect(db.setGuildApiKey).not.toHaveBeenCalled();
    });

    it('does not revoke a minted key when its persistence outcome is uncertain', async () => {
      const state = signCallbackState('guild-1', 'admin-2');
      db.setGuildApiKey.mockRejectedValueOnce(Object.assign(new Error('confirmation unavailable'), {
        code: 'GUILD_SETUP_UNCONFIRMED',
      }));
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({ ok: true, json: async () => ({ access_token: 'jwt', id_token: 'verified.jwt.sig' }) })
        .mockResolvedValueOnce({ ok: true, json: async () => ({ data: { key_id: 'new-id', api_key: 'new-secret' } }) });
      const res = await request(app).get(`/oauth/qurl/callback?code=grant&state=${encodeURIComponent(state)}`)
        .set('Cookie', cookieFor(state));
      expect(res.status).toBe(503);
      expect(res.text).toContain('before starting setup again');
      expect(res.text).not.toContain('new-secret');
      expect(globalThis.fetch).toHaveBeenCalledTimes(2);
      expect(discord.sendDM).not.toHaveBeenCalled();
    });

    it('fresh grant on the same state cannot replay a key revoked after persistence failed', async () => {
      const state = signCallbackState('guild-1', 'admin-2');
      const cached = new Map();
      const activeKeys = new Set();
      globalThis.fetch = jest.fn(async (url, options) => {
        if (String(url).endsWith('/oauth/token')) return { ok: true, json: async () => ({ access_token: 'jwt-xyz', id_token: 'verified.jwt.sig' }) };
        if (options.method === 'DELETE') {
          activeKeys.delete(String(url).split('/').at(-1));
          return { ok: true };
        }
        const key = options.headers['Idempotency-Key'];
        if (!cached.has(key)) {
          const keyId = `key-${cached.size + 1}`;
          cached.set(key, { data: { api_key: `secret-${keyId}`, key_id: keyId } });
          activeKeys.add(keyId);
        }
        return { ok: true, json: async () => cached.get(key) };
      });
      db.setGuildApiKey.mockRejectedValueOnce(new Error('storage failed'));
      const first = await request(app).get(`/oauth/qurl/callback?code=grant-one&state=${encodeURIComponent(state)}`).set('Cookie', cookieFor(state));
      expect(first.status).toBe(500);
      expect(activeKeys.size).toBe(0);
      const second = await request(app).get(`/oauth/qurl/callback?code=grant-two&state=${encodeURIComponent(state)}`).set('Cookie', cookieFor(state));
      expect(second.status).toBe(200);
      expect(cached.size).toBe(2);
      expect(db.setGuildApiKey.mock.calls.at(-1)[1]).toBe('secret-key-2');
      expect(activeKeys.has('key-2')).toBe(true);
    });

    it('does not trust a known mint code with the wrong HTTP status', async () => {
      const state = signCallbackState('guild-1', 'admin-2');
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({ ok: true, json: async () => ({ access_token: 'jwt-xyz', id_token: 'verified.jwt.sig' }) })
        .mockResolvedValueOnce({ ok: false, status: 401, text: async () => JSON.stringify({ error: { code: 'email_verification_required' } }) });
      const res = await request(app).get(`/oauth/qurl/callback?code=auth0-code&state=${encodeURIComponent(state)}`).set('Cookie', cookieFor(state));
      expect(res.status).toBe(502);
      expect(res.text).toContain('Could not provision qURL key');
      expect(db.setGuildApiKey).not.toHaveBeenCalled();
    });

    it('429s with key-limit-specific copy when qurl-service returns api_key_limit, does not persist or DM', async () => {
      const state = signCallbackState('guild-1', 'admin-2');
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({ access_token: 'jwt-xyz', id_token: 'verified.jwt.sig', token_type: 'Bearer', expires_in: 3600 }),
        })
        .mockResolvedValueOnce({
          ok: false, status: 403,
          text: () => Promise.resolve(JSON.stringify({
            error: {
              type: 'https://docs.layerv.ai/problems/api_key_limit',
              title: 'API Key Limit Exceeded',
              status: 403,
              code: 'api_key_limit',
              detail: 'You have reached the maximum number of API keys (3) for your plan.',
            },
          })),
        });
      const res = await request(app).get(
        `/oauth/qurl/callback?code=auth0-code&state=${encodeURIComponent(state)}`,
      ).set('Cookie', cookieFor(state));
      expect(res.status).toBe(429);
      expect(res.text).toContain('qURL API key limit reached');
      expect(res.text).toContain('Delete an unused key');
      expect(db.setGuildApiKey).not.toHaveBeenCalled();
      expect(discord.sendDM).not.toHaveBeenCalled();
      expect(logger.audit).not.toHaveBeenCalledWith(
        AUDIT_EVENTS.QURL_GUILD_KEY_CONFIGURED,
        expect.anything(),
      );
    });

    it('falls through to generic 502 when qurl-service returns valid JSON without error.code', async () => {
      const state = signCallbackState('guild-1', 'admin-2');
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({ access_token: 'jwt-xyz', id_token: 'verified.jwt.sig', token_type: 'Bearer', expires_in: 3600 }),
        })
        .mockResolvedValueOnce({
          ok: false, status: 403,
          text: () => Promise.resolve(JSON.stringify({
            error: { title: 'Forbidden', status: 403, detail: 'no code field' },
          })),
        });
      const res = await request(app).get(
        `/oauth/qurl/callback?code=auth0-code&state=${encodeURIComponent(state)}`,
      ).set('Cookie', cookieFor(state));
      expect(res.status).toBe(502);
      expect(res.text).toContain('Could not provision qURL key');
      expect(db.setGuildApiKey).not.toHaveBeenCalled();
    });

    it.each([true, false])('persists setup and reports confirmed or pending view counting (linked=%s)', async (webhookLinked) => {
      guildWebhookLink.linkGuildWebhookSubscriptionForSetup.mockResolvedValueOnce(webhookLinked);
      const state = signCallbackState('guild-1', 'admin-2');
      const idTokenPayload = Buffer.from(JSON.stringify({ email: 'alice@layerv.test', sub: 'auth0|abc' })).toString('base64')
        .replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
      const idToken = `header.${idTokenPayload}.sig`;
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({ access_token: 'jwt-xyz', id_token: idToken, token_type: 'Bearer', expires_in: 3600 }),
        })
        .mockResolvedValueOnce({
          ok: true, status: 201,
          json: () => Promise.resolve({
            data: {
              key_id: 'key-123', api_key: 'lv_live_abc123', key_prefix: 'lv_live_abc1',
              name: 'Discord guild guild-1', status: 'active',
            },
          }),
        });
      const fetchMock = globalThis.fetch;
      const res = await request(app).get(
        `/oauth/qurl/callback?code=auth0-code&state=${encodeURIComponent(state)}`,
      ).set('Cookie', cookieFor(state));
      expect(res.status).toBe(200);
      expect(res.text).toContain('qURL is connected');
      const dm = discord.sendDM.mock.calls[0][1];
      if (webhookLinked) {
        expect(res.text).not.toContain(GUILD_WEBHOOK_PENDING_NOTICE);
        expect(dm).not.toContain(GUILD_WEBHOOK_PENDING_NOTICE);
      } else {
        expect(res.text).toContain(GUILD_WEBHOOK_PENDING_NOTICE);
        expect(dm).toContain(GUILD_WEBHOOK_PENDING_NOTICE);
      }
      expect(res.text).toContain('/qurl send is ready');
      expect(res.text).not.toContain('/qurl map');
      expect(db.setGuildApiKey).toHaveBeenCalledWith('guild-1', 'lv_live_abc123', 'admin-2', 'oauth', { ownerId: 'auth0|abc', expectedApiKey: null, keyId: 'key-123', keyKind: 'dedicated' });
      expect(logger.audit).toHaveBeenCalledWith(AUDIT_EVENTS.QURL_GUILD_KEY_CONFIGURED, {
        guild_id: 'guild-1',
        configured_by: 'admin-2',
        key_prefix: 'lv_live_abc1',
        qurl_account_subject_fingerprint: AUTH0_ABC_FINGERPRINT,
        qurl_account_fingerprint_key_epoch: SUBJECT_FINGERPRINT_KEY_EPOCH,
      });
      expect(discord.sendDM).toHaveBeenCalledTimes(1);
      expect(discord.sendDM.mock.calls[0][0]).toBe('admin-2');
      expect(discord.sendDM.mock.calls[0][1]).toContain('qURL is connected');
      expect(discord.sendDM.mock.calls[0][1]).toContain('`/qurl send`');
      expect(discord.sendDM.mock.calls[0][1]).not.toContain('/qurl map');

      const mintCall = fetchMock.mock.calls.find(([url]) => String(url).endsWith('/v1/api-keys'));
      const body = JSON.parse(mintCall[1].body);
      const statePayload = JSON.parse(Buffer.from(state.split('.')[0], 'base64url').toString());
      expect(mintCall[1].headers['Idempotency-Key']).toBe(crypto.createHmac('sha256', statePayload.n).update('auth0-code').digest('hex'));
      expect(require('../src/utils/auth0-jwks').verifyAuth0IdToken).toHaveBeenCalledWith(expect.any(String), { nonce: statePayload.n, startedAt: statePayload.a });
      expect(mintCall[1].headers['Idempotency-Key']).toMatch(/^[a-f0-9]{64}$/);
      expect(body.kind).toBe('api_key');
      expect(body).not.toHaveProperty('key_type');

      expect(res.text).toMatch(/<dt>Discord guild<\/dt>\s*<dd>guild-1<\/dd>/);
      expect(res.text).toMatch(/<dt>qURL account<\/dt>\s*<dd>alice@layerv\.test<\/dd>/);
      expect(res.text).toMatch(/<dt>API key prefix<\/dt>\s*<dd>lv_live_abc1<\/dd>/);
    });

    it('degrades audit fingerprint failures without breaking a successful setup', async () => {
      const { verifyAuth0IdToken } = require('../src/utils/auth0-jwks');
      const state = signCallbackState('guild-1', 'admin-2');
      const configuredStateSecret = config.QURL_OAUTH_STATE_SECRET;
      verifyAuth0IdToken.mockImplementationOnce(async () => {
        // State has already verified when id_token verification runs. Break
        // the lazy secret resolver only for the later audit computation.
        config.QURL_OAUTH_STATE_SECRET = 'short';
        return {
          ok: true,
          payload: { email: 'alice@layerv.test', sub: 'auth0|abc' },
        };
      });
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({ access_token: 'jwt-xyz', id_token: 'verified.jwt.sig' }),
        })
        .mockResolvedValueOnce({
          ok: true, status: 201,
          json: () => Promise.resolve({
            data: { key_id: 'key-123', api_key: 'lv_live_abc', key_prefix: 'lv_live_a' },
          }),
        });

      let res;
      try {
        res = await request(app).get(
          `/oauth/qurl/callback?code=auth0-code&state=${encodeURIComponent(state)}`,
        ).set('Cookie', cookieFor(state));
      } finally {
        if (configuredStateSecret === undefined) {
          delete config.QURL_OAUTH_STATE_SECRET;
        } else {
          config.QURL_OAUTH_STATE_SECRET = configuredStateSecret;
        }
      }

      expect(res.status).toBe(200);
      expect(db.setGuildApiKey).toHaveBeenCalledWith(
        'guild-1',
        'lv_live_abc',
        'admin-2',
        'oauth',
        { ownerId: 'auth0|abc', expectedApiKey: null, keyId: 'key-123', keyKind: 'dedicated' },
      );
      expect(logger.audit).toHaveBeenCalledWith(
        AUDIT_EVENTS.QURL_GUILD_KEY_CONFIGURED,
        expect.objectContaining({
          qurl_account_subject_fingerprint: null,
          qurl_account_fingerprint_key_epoch: null,
        }),
      );
      expect(logger.warn).toHaveBeenCalledWith(
        'qURL OAuth audit fingerprint unavailable (setup continues)',
        expect.objectContaining({ guildId: 'guild-1' }),
      );
    });

    it('degrades audit emission failures after persistence and still finishes setup', async () => {
      const state = signCallbackState('guild-1', 'admin-2');
      logger.audit.mockImplementationOnce(() => {
        throw new Error('audit sink unavailable');
      });
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({ access_token: 'jwt-xyz', id_token: 'verified.jwt.sig' }),
        })
        .mockResolvedValueOnce({
          ok: true, status: 201,
          json: () => Promise.resolve({
            data: { key_id: 'key-123', api_key: 'lv_live_abc', key_prefix: 'lv_live_a' },
          }),
        });

      const res = await request(app).get(
        `/oauth/qurl/callback?code=auth0-code&state=${encodeURIComponent(state)}`,
      ).set('Cookie', cookieFor(state));

      expect(res.status).toBe(200);
      expect(res.text).toContain('qURL is connected');
      expect(db.setGuildApiKey).toHaveBeenCalledWith('guild-1', 'lv_live_abc', 'admin-2', 'oauth', { ownerId: 'auth0|abc', expectedApiKey: null, keyId: 'key-123', keyKind: 'dedicated' });
      expect(discord.sendDM).toHaveBeenCalledWith(
        'admin-2', expect.stringContaining('qURL is connected'),
      );
      expect(guildWebhookLink.linkGuildWebhookSubscriptionForSetup)
        .toHaveBeenCalledWith({
          guildId: 'guild-1',
          apiKey: 'lv_live_abc',
          via: 'oauth',
          configuredBy: 'admin-2',
        });
      expect(logger.warn).toHaveBeenCalledWith(
        'qURL OAuth audit emission failed (setup continues)',
        { error: 'audit sink unavailable', guildId: 'guild-1' },
      );
    });

    it.each([
      [new Error('DDB throttled'), 500],
      [Object.assign(new Error('conditional write lost'), { name: 'ConditionalCheckFailedException' }), 409],
      [Object.assign(new Error('saved key changed'), { code: 'GUILD_SETUP_CHANGED' }), 409],
      [Object.assign(new Error('owner changed'), { code: 'GUILD_OWNER_CONFLICT' }), 409],
    ])('cleans up an orphan after a failed key write: %s', async (persistError, expectedStatus) => {
      const state = signCallbackState('guild-1', 'admin-2');
      let resolveDeleteFired;
      const deleteFired = new Promise((resolve) => { resolveDeleteFired = resolve; });
      const fetchSpy = jest.fn()
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({ access_token: 'jwt-xyz', id_token: 'verified.jwt.sig' }),
        })
        .mockResolvedValueOnce({
          ok: true, status: 201,
          json: () => Promise.resolve({ data: { key_id: 'key-orphan-1', api_key: 'lv_live_abc123', key_prefix: 'lv_live_abc1' } }),
        })
        .mockImplementationOnce(async () => {
          resolveDeleteFired();
          return { ok: true, status: 204, text: () => Promise.resolve('') };
        });
      globalThis.fetch = fetchSpy;
      db.setGuildApiKey.mockRejectedValueOnce(persistError);
      const res = await request(app).get(
        `/oauth/qurl/callback?code=auth0-code&state=${encodeURIComponent(state)}`,
      ).set('Cookie', cookieFor(state));
      expect(res.status).toBe(expectedStatus);
      expect(res.text).toContain(expectedStatus === 500 ? 'provisioned but not stored' : 'Server setup changed');
      await deleteFired;
      const deleteCall = fetchSpy.mock.calls.find((c) => typeof c[1]?.method === 'string' && c[1].method === 'DELETE');
      expect(deleteCall).toBeDefined();
      expect(deleteCall[0]).toContain('/v1/api-keys/key-orphan-1');
      expect(logger.audit).not.toHaveBeenCalledWith(
        AUDIT_EVENTS.QURL_GUILD_KEY_CONFIGURED,
        expect.anything(),
      );
    });

    it.each(['ERR_JWS_SIGNATURE_VERIFICATION_FAILED', 'nonce_mismatch'])('refuses setup before minting for an unverified identity: %s', async reason => {
      const { verifyAuth0IdToken } = require('../src/utils/auth0-jwks');
      verifyAuth0IdToken.mockResolvedValueOnce({ ok: false, reason });
      const state = signCallbackState('guild-1', 'admin-2');
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({ access_token: 'jwt-xyz', id_token: 'tampered.jwt.sig' }),
        })
        .mockResolvedValueOnce({
          ok: true, status: 201,
          json: () => Promise.resolve({ data: { key_id: 'key-123', api_key: 'lv_live_abc', key_prefix: 'lv_live_a' } }),
        });
      const res = await request(app).get(
        `/oauth/qurl/callback?code=auth0-code&state=${encodeURIComponent(state)}`,
      ).set('Cookie', cookieFor(state));
      expect(res.status).toBe(403);
      expect(globalThis.fetch).toHaveBeenCalledTimes(1);
      expect(db.setGuildApiKey).not.toHaveBeenCalled();
    });

    it('refuses setup before minting when the Auth0 response has no id_token', async () => {
      const state = signCallbackState('guild-1', 'admin-2');
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({ access_token: 'jwt-xyz' }),
        })
        .mockResolvedValueOnce({
          ok: true, status: 201,
          json: () => Promise.resolve({ data: { key_id: 'key-123', api_key: 'lv_live_abc', key_prefix: 'lv_live_a' } }),
        });
      const res = await request(app).get(
        `/oauth/qurl/callback?code=auth0-code&state=${encodeURIComponent(state)}`,
      ).set('Cookie', cookieFor(state));
      expect(res.status).toBe(403);
      expect(globalThis.fetch).toHaveBeenCalledTimes(1);
      expect(db.setGuildApiKey).not.toHaveBeenCalled();
    });

    it('502s when qurl-service mint response has api_key but missing key_id (orphan-cleanup contract)', async () => {
      const state = signCallbackState('guild-1', 'admin-2');
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({ access_token: 'jwt-xyz', id_token: 'verified.jwt.sig' }),
        })
        .mockResolvedValueOnce({
          ok: true, status: 201,
          json: () => Promise.resolve({ data: { api_key: 'lv_live_abc', key_prefix: 'lv_live_a' } }),
        });
      const res = await request(app).get(
        `/oauth/qurl/callback?code=auth0-code&state=${encodeURIComponent(state)}`,
      ).set('Cookie', cookieFor(state));
      expect(res.status).toBe(502);
      expect(res.text).toContain('Could not provision qURL key');
      expect(db.setGuildApiKey).not.toHaveBeenCalled();
    });

    it('clears the qurl_setup_session and PKCE cookies on successful callback (one-shot binding)', async () => {
      const state = signCallbackState('guild-1', 'admin-2');
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({ access_token: 'jwt-xyz', id_token: 'verified.jwt.sig' }),
        })
        .mockResolvedValueOnce({
          ok: true, status: 201,
          json: () => Promise.resolve({ data: { key_id: 'key-1', api_key: 'lv_live_abc', key_prefix: 'lv_live_a' } }),
        });
      const res = await request(app).get(
        `/oauth/qurl/callback?code=auth0-code&state=${encodeURIComponent(state)}`,
      ).set('Cookie', cookieFor(state));
      expect(res.status).toBe(200);
      expectQurlOAuthCookiesCleared(res);
    });
  });
});

// Separate describe — exercises both not-configured 503 paths. Uses
// jest.isolateModules so the route's derived configuration is evaluated
// against each temporary env shape. Without isolateModules, changing env
// after `config.js` has been required would leave its cached flags unchanged.
describe('qurl-oauth — not configured', () => {
  it.each([
    ['AUTH0_* is unset', {
      AUTH0_DOMAIN: undefined,
      AUTH0_CLIENT_ID: undefined,
      AUTH0_CLIENT_SECRET: undefined,
      AUTH0_AUDIENCE: undefined,
      AUTH0_EMAIL_CONNECTION: undefined,
    }, /has not been registered yet/],
    ['AUTH0_EMAIL_CONNECTION is rejected', {
      AUTH0_EMAIL_CONNECTION: 'email!',
    }, /must correct an invalid authentication setting/],
  ])('returns 503 from /start and /callback when %s', async (_label, overrides, remediation) => {
    const saved = {
      AUTH0_DOMAIN: process.env.AUTH0_DOMAIN,
      AUTH0_CLIENT_ID: process.env.AUTH0_CLIENT_ID,
      AUTH0_CLIENT_SECRET: process.env.AUTH0_CLIENT_SECRET,
      AUTH0_AUDIENCE: process.env.AUTH0_AUDIENCE,
      AUTH0_EMAIL_CONNECTION: process.env.AUTH0_EMAIL_CONNECTION,
    };
    for (const [key, value] of Object.entries(overrides)) {
      if (value === undefined) delete process.env[key];
      else process.env[key] = value;
    }
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
        // Re-require everything against the freshened module cache.
        // eslint-disable-next-line global-require
        const supertest = require('supertest');
        // eslint-disable-next-line global-require
        const { app: freshApp } = require('../src/server');
        const start = await supertest(freshApp).get('/oauth/qurl/start?state=anything');
        expect(start.status).toBe(503);
        expect(start.text).toMatch(/not configured/i);
        const callback = await supertest(freshApp)
          .get('/oauth/qurl/callback?code=auth0-code&state=anything')
          .set('Cookie', cookieFor('anything'));
        expect(callback.status).toBe(503);
        expect(callback.text).toMatch(/not configured/i);
        expect(callback.text).toMatch(remediation);
        expectQurlOAuthCookiesCleared(callback);
        expect(start.text).not.toMatch(/AUTH0_[A-Z_]+/);
        expect(start.text).not.toMatch(/DISCORD_CLIENT_SECRET/);
        expect(start.text).not.toMatch(/Reason:/i);
        expect(callback.text).not.toMatch(/AUTH0_[A-Z_]+/);
        expect(callback.text).not.toMatch(/DISCORD_CLIENT_SECRET/);
        expect(callback.text).not.toMatch(/Reason:/i);
      });
    } finally {
      // Restore env so subsequent tests run against the configured router.
      for (const [key, value] of Object.entries(saved)) {
        if (value === undefined) delete process.env[key];
        else process.env[key] = value;
      }
    }
  });
});

describe('qurl-oauth — MAP_COMMAND_ENABLED=true', () => {
  it('advertises both enabled commands on the success page and in the admin DM', async () => {
    const savedMapCommandEnabled = process.env.MAP_COMMAND_ENABLED;
    process.env.MAP_COMMAND_ENABLED = 'true';
    try {
      await jest.isolateModulesAsync(async () => {
        jest.doMock('../src/discord', () => ({
          sendDM: jest.fn().mockResolvedValue(true),
          assignContributorRole: jest.fn(),
          notifyPRMerge: jest.fn(),
          notifyBadgeEarned: jest.fn(),
        }));
        jest.doMock('../src/store', () => ({
          setGuildApiKey: jest.fn().mockResolvedValue(undefined),
          getGuildConfigWithApiKey: jest.fn().mockResolvedValue(undefined),
          getGuildApiKey: jest.fn(),
          getGuildConfig: jest.fn().mockResolvedValue({ guild_id: 'guild-1', configured_by: 'admin-2' }),
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
        // eslint-disable-next-line global-require
        const freshDiscord = require('../src/discord');
        // eslint-disable-next-line global-require
        const { signQurlOAuthState: sign, signQurlOAuthRedirectState: redirect } = require('../src/utils/qurl-oauth-state');
        const state = redirect(sign('guild-1', 'admin-2'));
        globalThis.fetch = jest.fn()
          .mockResolvedValueOnce({
            ok: true, status: 200,
            json: () => Promise.resolve({ access_token: 'jwt-xyz', id_token: 'verified.jwt.sig' }),
          })
          .mockResolvedValueOnce({
            ok: true, status: 201,
            json: () => Promise.resolve({ data: { key_id: 'key-1', api_key: 'lv_live_abc', key_prefix: 'lv_live_a' } }),
          });

        const res = await supertest(freshApp)
          .get(`/oauth/qurl/callback?code=auth0-code&state=${encodeURIComponent(state)}`)
          .set('Cookie', cookieFor(state));

        expect(res.status).toBe(200);
        expect(res.text).toContain('/qurl send and /qurl map are ready');
        expect(freshDiscord.sendDM).toHaveBeenCalledTimes(1);
        expect(freshDiscord.sendDM.mock.calls[0][0]).toBe('admin-2');
        expect(freshDiscord.sendDM.mock.calls[0][1])
          .toContain('`/qurl send` and `/qurl map`');
      });
    } finally {
      process.env.MAP_COMMAND_ENABLED = savedMapCommandEnabled;
      globalThis.fetch = originalFetch;
    }
  });
});
