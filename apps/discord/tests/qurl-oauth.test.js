
process.env.AUTH0_DOMAIN = 'layerv-test.auth0.com';
process.env.AUTH0_CLIENT_ID = 'test-client-id';
process.env.AUTH0_CLIENT_SECRET = 'test-client-secret';
process.env.AUTH0_AUDIENCE = 'https://api.layerv.test';
process.env.QURL_ENDPOINT = 'http://localhost:9999';
process.env.PRIVATE_UPLOAD_QURL = 'qurl://private-upload';
process.env.BASE_URL = 'http://localhost:3000';
process.env.MAP_COMMAND_ENABLED = 'false';
process.env.KEY_ENCRYPTION_KEY = '1'.repeat(64);
process.env.GUILD_ID = '123456789012345678';
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
  getGuildConfig: jest.fn().mockResolvedValue({ guild_id: 'guild-1', configured_by: 'admin-2' }),
  getPendingLink: jest.fn(),
  consumePendingLink: jest.fn(),
}));

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
const { app } = require('../src/server');
const db = require('../src/store');
const discord = require('../src/discord');
const { signQurlOAuthState } = require('../src/utils/qurl-oauth-state');
const {
  QURL_OAUTH_SESSION_COOKIE,
  QURL_OAUTH_PKCE_COOKIE,
} = require('../src/utils/oauth-cookies');
const { pkceChallengeForVerifier } = require('../src/utils/oauth-pkce');
const { rateLimitStore } = require('../src/utils/oauth-rate-limit');
const { clearedCookieHeader, cookieValue } = require('./helpers/cookies');

const originalFetch = globalThis.fetch;
const TEST_PKCE_VERIFIER = 'a'.repeat(43);

function cookieFor(state, codeVerifier = TEST_PKCE_VERIFIER) {
  return `${QURL_OAUTH_SESSION_COOKIE}=${encodeURIComponent(state)}; `
    + `${QURL_OAUTH_PKCE_COOKIE}=${encodeURIComponent(codeVerifier)}`;
}

function expectQurlOAuthCookiesCleared(res) {
  for (const name of [QURL_OAUTH_SESSION_COOKIE, QURL_OAUTH_PKCE_COOKIE]) {
    const clearCookie = clearedCookieHeader(res.headers['set-cookie'], name);
    expect(clearCookie).toBeDefined();
    expect(clearCookie).toMatch(/Path=\/oauth\/qurl(?:;|$)/);
  }
}

beforeEach(() => {
  jest.clearAllMocks();
  rateLimitStore.clear();
  globalThis.fetch = originalFetch;
});

describe('qurl-oauth routes', () => {
  describe('GET /oauth/qurl/start', () => {
    it('redirects to Auth0 authorize URL with the right params on valid state', async () => {
      const state = signQurlOAuthState('guild-1', 'admin-2');
      const res = await request(app).get(`/oauth/qurl/start?state=${encodeURIComponent(state)}`);
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
      expect(loc.searchParams.get('prompt')).toBe('consent');
      expect(loc.searchParams.get('state')).toBe(state);
      expect(loc.searchParams.get('redirect_uri')).toBe('http://localhost:3000/oauth/qurl/callback');

      const codeVerifier = cookieValue(res.headers['set-cookie'], QURL_OAUTH_PKCE_COOKIE);
      expect(codeVerifier).not.toBeNull();
      expect(loc.searchParams.get('code_challenge_method')).toBe('S256');
      expect(loc.searchParams.get('code_challenge')).toBe(pkceChallengeForVerifier(codeVerifier));
      expect(loc.searchParams.get('code_challenge')).not.toBe(codeVerifier);
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
      expect(cookieHeader).toMatch(/Path=\/oauth\/qurl(?:;|\s|$)/);
      expect(cookieHeader).toContain(encodeURIComponent(state));
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
        const { signQurlOAuthState: sign } = require('../src/utils/qurl-oauth-state');
        const state = sign('guild-1', 'admin-2');
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

    it('omits prompt=consent on first install (no prior guild config)', async () => {
      db.getGuildConfig.mockResolvedValueOnce(undefined);
      const state = signQurlOAuthState('guild-fresh', 'admin-fresh');
      const res = await request(app).get(`/oauth/qurl/start?state=${encodeURIComponent(state)}`);
      expect(res.status).toBe(302);
      const loc = new URL(res.headers.location);
      expect(loc.searchParams.get('prompt')).toBeNull();
    });

    it('falls back to prompt=consent when getGuildConfig throws (DDB blip)', async () => {
      db.getGuildConfig.mockRejectedValueOnce(new Error('DDB throttled'));
      const state = signQurlOAuthState('guild-1', 'admin-2');
      const res = await request(app).get(`/oauth/qurl/start?state=${encodeURIComponent(state)}`);
      expect(res.status).toBe(302);
      const loc = new URL(res.headers.location);
      expect(loc.searchParams.get('prompt')).toBe('consent');
    });
  });

  describe('GET /oauth/qurl/callback', () => {
    it('503s and clears cookies when KEY_ENCRYPTION_KEY is unset', async () => {
      const saved = process.env.KEY_ENCRYPTION_KEY;
      delete process.env.KEY_ENCRYPTION_KEY;
      try {
        const state = signQurlOAuthState('guild-1', 'admin-2');
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
      const state = signQurlOAuthState('guild-1', 'admin-2');
      const res = await request(app).get(`/oauth/qurl/callback?state=${encodeURIComponent(state)}`)
        .set('Cookie', cookieFor(state));
      expect(res.status).toBe(400);
      expect(res.text).toContain('Missing authorization code');
      expectQurlOAuthCookiesCleared(res);
    });

    it('400s on Auth0 error param (admin declined consent)', async () => {
      const state = signQurlOAuthState('guild-1', 'admin-2');
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
      const state = signQurlOAuthState('guild-1', 'admin-2');
      const res = await request(app)
        .get(`/oauth/qurl/callback?code=auth0-code&state=${encodeURIComponent(state)}`)
        .set('Cookie', `${QURL_OAUTH_SESSION_COOKIE}=${encodeURIComponent(state)}`);
      expect(res.status).toBe(400);
      expect(res.text).toMatch(/could not be completed/i);
      expectQurlOAuthCookiesCleared(res);
    });

    it('400s on missing CSRF cookie (leaked URL opened in different browser)', async () => {
      const state = signQurlOAuthState('guild-1', 'admin-2');
      const res = await request(app).get(
        `/oauth/qurl/callback?code=auth0-code&state=${encodeURIComponent(state)}`,
      );
      expect(res.status).toBe(400);
      expect(res.text).toMatch(/same browser tab/i);
      expectQurlOAuthCookiesCleared(res);
    });

    it('cookie value URL-decodes to the same state used in the timingSafeEqual compare (round-9 #8 follow-up)', async () => {
      const state = signQurlOAuthState('guild-1', 'admin-2');
      expect(encodeURIComponent(state)).toBe(state);
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({ access_token: 'jwt-xyz' }),
        })
        .mockResolvedValueOnce({
          ok: true, status: 201,
          json: () => Promise.resolve({
            binding_id: 'eib_12345678901',
            api_key: { key_id: 'key_123456789012', plaintext: 'lv_live_abc', key_prefix: 'lv_live_a' },
          }),
        });
      const res = await request(app)
        .get(`/oauth/qurl/callback?code=auth0-code&state=${encodeURIComponent(state)}`)
        .set('Cookie', cookieFor(state));
      expect(res.status).toBe(200);
      expect(res.text).toContain('qURL is connected');
    });

    it('400s on cookie/state mismatch (cookie from a different state)', async () => {
      const stateA = signQurlOAuthState('guild-1', 'admin-2');
      const stateB = signQurlOAuthState('guild-1', 'admin-2'); // different nonce → different state
      const res = await request(app).get(
        `/oauth/qurl/callback?code=auth0-code&state=${encodeURIComponent(stateA)}`,
      ).set('Cookie', cookieFor(stateB));
      expect(res.status).toBe(400);
      expectQurlOAuthCookiesCleared(res);
    });

    it('502s when Auth0 token exchange fails', async () => {
      const state = signQurlOAuthState('guild-1', 'admin-2');
      globalThis.fetch = jest.fn().mockResolvedValueOnce({
        ok: false,
        status: 401,
        text: () => Promise.resolve('unauthorized client'),
      });
      const res = await request(app).get(
        `/oauth/qurl/callback?code=auth0-code&state=${encodeURIComponent(state)}`,
      ).set('Cookie', cookieFor(state));
      expect(res.status).toBe(502);
      expect(res.text).toContain('Authorization failed');
      expect(db.setGuildApiKey).not.toHaveBeenCalled();
    });

    it('sends the PKCE verifier cookie on the Auth0 token exchange', async () => {
      const state = signQurlOAuthState('guild-1', 'admin-2');
      const codeVerifier = 'b'.repeat(43);
      const fetchSpy = jest.fn()
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({ access_token: 'jwt-xyz', token_type: 'Bearer', expires_in: 3600 }),
        })
        .mockResolvedValueOnce({
          ok: true, status: 201,
          json: () => Promise.resolve({
            binding_id: 'eib_12345678901',
            api_key: { key_id: 'key_123456789012', plaintext: 'lv_live_abc', key_prefix: 'lv_live_a' },
          }),
        });
      globalThis.fetch = fetchSpy;
      const res = await request(app).get(
        `/oauth/qurl/callback?code=auth0-code&state=${encodeURIComponent(state)}`,
      ).set('Cookie', cookieFor(state, codeVerifier));
      expect(res.status).toBe(200);
      const tokenBody = new URLSearchParams(fetchSpy.mock.calls[0][1].body.toString());
      expect(tokenBody.get('code_verifier')).toBe(codeVerifier);
    });

    it('502s when qurl-service mint fails', async () => {
      const state = signQurlOAuthState('guild-1', 'admin-2');
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({ access_token: 'jwt-xyz', token_type: 'Bearer', expires_in: 3600 }),
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

    it('429s with key-limit-specific copy when qurl-service returns api_key_limit, does not persist or DM', async () => {
      const state = signQurlOAuthState('guild-1', 'admin-2');
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({ access_token: 'jwt-xyz', token_type: 'Bearer', expires_in: 3600 }),
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
    });

    it('falls through to generic 502 when qurl-service returns valid JSON without error.code', async () => {
      const state = signQurlOAuthState('guild-1', 'admin-2');
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({ access_token: 'jwt-xyz', token_type: 'Bearer', expires_in: 3600 }),
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

    it('200s on happy path: mints key, persists, DMs admin, renders success with binding readout', async () => {
      const state = signQurlOAuthState('guild-1', 'admin-2');
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
            binding_id: 'eib_12345678901', provider: 'discord', external_id: 'guild-1',
            api_key: { key_id: 'key_123456789012', plaintext: 'lv_live_abc123', key_prefix: 'lv_live_abc1' },
          }),
        });
      const fetchMock = globalThis.fetch;
      const res = await request(app).get(
        `/oauth/qurl/callback?code=auth0-code&state=${encodeURIComponent(state)}`,
      ).set('Cookie', cookieFor(state));
      expect(res.status).toBe(200);
      expect(res.text).toContain('qURL is connected');
      expect(res.text).toContain('/qurl send is ready');
      expect(res.text).not.toContain('/qurl map');
      expect(db.setGuildApiKey).toHaveBeenCalledWith('guild-1', 'lv_live_abc123', 'admin-2', {
        keyId: 'key_123456789012', bindingId: 'eib_12345678901',
      });
      expect(discord.sendDM).toHaveBeenCalledTimes(1);
      expect(discord.sendDM.mock.calls[0][0]).toBe('admin-2');
      expect(discord.sendDM.mock.calls[0][1]).toContain('qURL is connected');
      expect(discord.sendDM.mock.calls[0][1]).toContain('`/qurl send`');
      expect(discord.sendDM.mock.calls[0][1]).not.toContain('/qurl map');

      const mintCall = fetchMock.mock.calls.find(([url]) => String(url).endsWith('/v1/external-identity-bindings'));
      const body = JSON.parse(mintCall[1].body);
      expect(body).toEqual({ provider: 'discord', external_id: 'guild-1', display_name: 'Discord guild guild-1' });
      expect(mintCall[1].headers['Idempotency-Key']).toMatch(/^[0-9a-f-]{36}$/);

      expect(res.text).toMatch(/<dt>Discord guild<\/dt>\s*<dd>guild-1<\/dd>/);
      expect(res.text).toMatch(/<dt>qURL account<\/dt>\s*<dd>alice@layerv\.test<\/dd>/);
      expect(res.text).toMatch(/<dt>API key prefix<\/dt>\s*<dd>lv_live_abc1<\/dd>/);
    });

    it('retries an exact private binding create after a service 5xx', async () => {
      const state = signQurlOAuthState('guild-1', 'admin-2');
      const cancelBody = jest.fn().mockResolvedValue(undefined);
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({ access_token: 'jwt-xyz' }),
        })
        .mockResolvedValueOnce({ ok: false, status: 500, body: { cancel: cancelBody } })
        .mockResolvedValueOnce({
          ok: true, status: 201,
          json: () => Promise.resolve({
            binding_id: 'eib_12345678901',
            api_key: { key_id: 'key_123456789012', plaintext: 'lv_live_abc123', key_prefix: 'lv_live_abc1' },
          }),
        });

      const res = await request(app).get(
        `/oauth/qurl/callback?code=auth0-code&state=${encodeURIComponent(state)}`,
      ).set('Cookie', cookieFor(state));

      expect(res.status).toBe(200);
      const createCalls = globalThis.fetch.mock.calls.filter(([url]) => String(url).endsWith('/v1/external-identity-bindings'));
      expect(createCalls).toHaveLength(2);
      expect(createCalls[1][1].body).toBe(createCalls[0][1].body);
      expect(createCalls[1][1].headers['Idempotency-Key']).toBe(createCalls[0][1].headers['Idempotency-Key']);
      expect(cancelBody).toHaveBeenCalledTimes(1);
      expect(db.setGuildApiKey).toHaveBeenCalledWith('guild-1', 'lv_live_abc123', 'admin-2', {
        keyId: 'key_123456789012', bindingId: 'eib_12345678901',
      });
    });

    it('retries a private binding DELETE after 5xx and accepts terminal 204', async () => {
      const state = signQurlOAuthState('guild-1', 'admin-2');
      const fetchSpy = jest.fn()
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({ access_token: 'jwt-xyz' }),
        })
        .mockResolvedValueOnce({
          ok: true, status: 201,
          json: () => Promise.resolve({
            binding_id: 'eib_12345678901',
            api_key: { key_id: 'key_123456789012', plaintext: 'lv_live_abc123', key_prefix: 'lv_live_abc1' },
          }),
        })
        .mockResolvedValueOnce({ ok: false, status: 503 })
        .mockResolvedValueOnce({ ok: true, status: 204 });
      globalThis.fetch = fetchSpy;
      db.setGuildApiKey.mockRejectedValueOnce(new Error('DDB throttled'));
      const res = await request(app).get(
        `/oauth/qurl/callback?code=auth0-code&state=${encodeURIComponent(state)}`,
      ).set('Cookie', cookieFor(state));
      expect(res.status).toBe(500);
      expect(res.text).toContain('provisioned but not stored');
      const deleteCalls = fetchSpy.mock.calls.filter(([, init]) => init?.method === 'DELETE');
      expect(deleteCalls).toHaveLength(2);
      expect(deleteCalls.map(([url]) => url)).toEqual([
        'http://localhost:9999/v1/external-identity-bindings/eib_12345678901',
        'http://localhost:9999/v1/external-identity-bindings/eib_12345678901',
      ]);
      expect(deleteCalls[1][1].headers).toEqual(deleteCalls[0][1].headers);
    });

    it('awaits one exact private-binding DELETE retry after a transport-unknown result', async () => {
      const state = signQurlOAuthState('guild-1', 'admin-2');
      const timeout = jest.spyOn(AbortSignal, 'timeout');
      const fetchSpy = jest.fn()
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({ access_token: 'jwt-xyz' }),
        })
        .mockResolvedValueOnce({
          ok: true, status: 201,
          json: () => Promise.resolve({
            binding_id: 'eib_12345678901',
            api_key: { key_id: 'key_123456789012', plaintext: 'lv_live_abc123', key_prefix: 'lv_live_abc1' },
          }),
        })
        .mockRejectedValueOnce(new Error('connection reset after send'))
        .mockResolvedValueOnce({ ok: false, status: 404 });
      globalThis.fetch = fetchSpy;
      db.setGuildApiKey.mockRejectedValueOnce(new Error('DDB throttled'));

      try {
        const res = await request(app).get(
          `/oauth/qurl/callback?code=auth0-code&state=${encodeURIComponent(state)}`,
        )
          .set('Cookie', cookieFor(state))
          .set('X-Forwarded-For', '198.51.100.17');

        expect(res.status).toBe(500);
        const deleteCalls = fetchSpy.mock.calls.filter(([, init]) => init?.method === 'DELETE');
        expect(deleteCalls).toHaveLength(2);
        expect(deleteCalls[1][0]).toBe(deleteCalls[0][0]);
        expect(deleteCalls[1][1].headers).toEqual(deleteCalls[0][1].headers);
        expect(timeout).toHaveBeenCalledWith(5000);
      } finally {
        timeout.mockRestore();
      }
    });

    it('renders success without qURL-account-email line when id_token verification fails', async () => {
      const { verifyAuth0IdToken } = require('../src/utils/auth0-jwks');
      verifyAuth0IdToken.mockResolvedValueOnce({ ok: false, reason: 'ERR_JWS_SIGNATURE_VERIFICATION_FAILED' });
      const state = signQurlOAuthState('guild-1', 'admin-2');
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({ access_token: 'jwt-xyz', id_token: 'tampered.jwt.sig' }),
        })
        .mockResolvedValueOnce({
          ok: true, status: 201,
          json: () => Promise.resolve({
            binding_id: 'eib_12345678901',
            api_key: { key_id: 'key_123456789012', plaintext: 'lv_live_abc', key_prefix: 'lv_live_a' },
          }),
        });
      const res = await request(app).get(
        `/oauth/qurl/callback?code=auth0-code&state=${encodeURIComponent(state)}`,
      ).set('Cookie', cookieFor(state));
      expect(res.status).toBe(200);
      expect(res.text).toContain('qURL is connected');
      expect(res.text).toMatch(/<dt>Discord guild<\/dt>\s*<dd>guild-1<\/dd>/);
      expect(res.text).toMatch(/<dt>API key prefix<\/dt>\s*<dd>lv_live_a<\/dd>/);
      expect(res.text).not.toMatch(/<dt>qURL account<\/dt>/);
    });

    it('renders success without qURL-account-email line when id_token field is absent from the Auth0 response', async () => {
      const state = signQurlOAuthState('guild-1', 'admin-2');
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({ access_token: 'jwt-xyz' }),
        })
        .mockResolvedValueOnce({
          ok: true, status: 201,
          json: () => Promise.resolve({
            binding_id: 'eib_12345678901',
            api_key: { key_id: 'key_123456789012', plaintext: 'lv_live_abc', key_prefix: 'lv_live_a' },
          }),
        });
      const res = await request(app).get(
        `/oauth/qurl/callback?code=auth0-code&state=${encodeURIComponent(state)}`,
      ).set('Cookie', cookieFor(state));
      expect(res.status).toBe(200);
      expect(res.text).toContain('qURL is connected');
      expect(res.text).not.toMatch(/<dt>qURL account<\/dt>/);
    });

    it('502s when qurl-service mint response has api_key but missing key_id (orphan-cleanup contract)', async () => {
      const state = signQurlOAuthState('guild-1', 'admin-2');
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({ access_token: 'jwt-xyz' }),
        })
        .mockResolvedValueOnce({
          ok: true, status: 201,
          json: () => Promise.resolve({
            binding_id: 'eib_12345678901',
            api_key: { plaintext: 'lv_live_abc', key_prefix: 'lv_live_a' },
          }),
        });
      const res = await request(app).get(
        `/oauth/qurl/callback?code=auth0-code&state=${encodeURIComponent(state)}`,
      ).set('Cookie', cookieFor(state));
      expect(res.status).toBe(502);
      expect(res.text).toContain('Could not provision qURL key');
      expect(db.setGuildApiKey).not.toHaveBeenCalled();
    });

    it('clears the qurl_setup_session and PKCE cookies on successful callback (one-shot binding)', async () => {
      const state = signQurlOAuthState('guild-1', 'admin-2');
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({ access_token: 'jwt-xyz' }),
        })
        .mockResolvedValueOnce({
          ok: true, status: 201,
          json: () => Promise.resolve({
            binding_id: 'eib_12345678901',
            api_key: { key_id: 'key_123456789012', plaintext: 'lv_live_abc', key_prefix: 'lv_live_a' },
          }),
        });
      const res = await request(app).get(
        `/oauth/qurl/callback?code=auth0-code&state=${encodeURIComponent(state)}`,
      ).set('Cookie', cookieFor(state));
      expect(res.status).toBe(200);
      expectQurlOAuthCookiesCleared(res);
    });
  });
});

describe('qurl-oauth — not configured (AUTH0_* env unset)', () => {
  it('returns 503 with a "not configured" page on /start', async () => {
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
        expectQurlOAuthCookiesCleared(callback);
        expect(start.text).not.toMatch(/AUTH0_[A-Z_]+/);
        expect(start.text).not.toMatch(/DISCORD_CLIENT_SECRET/);
        expect(start.text).not.toMatch(/Reason:/i);
        expect(callback.text).not.toMatch(/AUTH0_[A-Z_]+/);
        expect(callback.text).not.toMatch(/DISCORD_CLIENT_SECRET/);
        expect(callback.text).not.toMatch(/Reason:/i);
      });
    } finally {
      Object.assign(process.env, saved);
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
        const { signQurlOAuthState: sign } = require('../src/utils/qurl-oauth-state');
        const state = sign('guild-1', 'admin-2');
        globalThis.fetch = jest.fn()
          .mockResolvedValueOnce({
            ok: true, status: 200,
            json: () => Promise.resolve({ access_token: 'jwt-xyz' }),
          })
          .mockResolvedValueOnce({
            ok: true, status: 201,
            json: () => Promise.resolve({
              binding_id: 'eib_12345678901',
              api_key: { key_id: 'key_123456789012', plaintext: 'lv_live_abc', key_prefix: 'lv_live_a' },
            }),
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

describe('qurl-oauth — public upload path', () => {
  it('keeps the existing API-key endpoint while private upload is disabled', async () => {
    const savedPrivateUploadQurl = process.env.PRIVATE_UPLOAD_QURL;
    delete process.env.PRIVATE_UPLOAD_QURL;
    try {
      await jest.isolateModulesAsync(async () => {
        const freshStore = {
          setGuildApiKey: jest.fn().mockResolvedValue(undefined),
          getGuildApiKey: jest.fn(),
          getGuildConfig: jest.fn().mockResolvedValue({ guild_id: 'guild-1', configured_by: 'admin-2' }),
          getPendingLink: jest.fn(),
          consumePendingLink: jest.fn(),
        };
        jest.doMock('../src/discord', () => ({
          sendDM: jest.fn().mockResolvedValue(true),
          assignContributorRole: jest.fn(),
          notifyPRMerge: jest.fn(),
          notifyBadgeEarned: jest.fn(),
        }));
        jest.doMock('../src/store', () => freshStore);
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
        const { signQurlOAuthState: sign } = require('../src/utils/qurl-oauth-state');
        const state = sign('guild-1', 'admin-2');
        globalThis.fetch = jest.fn()
          .mockResolvedValueOnce({
            ok: true, status: 200,
            json: () => Promise.resolve({ access_token: 'jwt-xyz' }),
          })
          .mockResolvedValueOnce({
            ok: true, status: 201,
            json: () => Promise.resolve({
              data: { key_id: 'key-public-1', api_key: 'lv_live_public', key_prefix: 'lv_live_pub' },
            }),
          });

        const res = await supertest(freshApp)
          .get(`/oauth/qurl/callback?code=auth0-code&state=${encodeURIComponent(state)}`)
          .set('Cookie', cookieFor(state));

        expect(res.status).toBe(200);
        expect(globalThis.fetch.mock.calls[1][0]).toBe('http://localhost:9999/v1/api-keys');
        expect(JSON.parse(globalThis.fetch.mock.calls[1][1].body)).toEqual({
          kind: 'api_key', name: 'Discord guild guild-1', scopes: ['qurl:write', 'qurl:read'],
        });
        expect(globalThis.fetch.mock.calls[1][1].headers).not.toHaveProperty('Idempotency-Key');
      });
    } finally {
      process.env.PRIVATE_UPLOAD_QURL = savedPrivateUploadQurl;
      globalThis.fetch = originalFetch;
    }
  });

  it('retries a public API-key DELETE after 5xx and accepts terminal 404', async () => {
    const router = require('../src/routes/qurl-oauth');
    globalThis.fetch = jest.fn()
      .mockResolvedValueOnce({ ok: false, status: 500 })
      .mockResolvedValueOnce({ ok: false, status: 404 });

    await router.__testExports.deleteOrphanCredential({
      accessToken: 'jwt-xyz',
      keyId: 'key-public-1',
      guildId: 'guild-1',
    });

    expect(globalThis.fetch).toHaveBeenCalledTimes(2);
    expect(globalThis.fetch.mock.calls.map(([url]) => url)).toEqual([
      'http://localhost:9999/v1/api-keys/key-public-1',
      'http://localhost:9999/v1/api-keys/key-public-1',
    ]);
    expect(globalThis.fetch.mock.calls[0][1]).toEqual(expect.objectContaining({
      method: 'DELETE',
      headers: expect.objectContaining({ Authorization: 'Bearer jwt-xyz' }),
    }));
  });
});
