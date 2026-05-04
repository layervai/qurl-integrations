// Tests for src/routes/qurl-oauth.js — the OAuth-redirect setup flow that
// replaces the API-key-paste modal in /qurl setup. Covers:
//   - /start: 400 invalid-state, 302 redirect to Auth0, cookie set
//   - /callback: 400 invalid state / missing cookie / cookie mismatch,
//     502 Auth0 token-exchange failure, 502 qurl-service mint failure,
//     200 happy path with mint + persist + DM, 500 + orphan-key cleanup
//   - 503 not-configured response when AUTH0_* env vars are unset
//     (separate describe — uses jest.isolateModules to load the router
//     with the env temporarily wiped).

// Auth0 + state-secret env vars must be set BEFORE requiring the modules
// that read them at load time. SAME OAUTH_STATE_SECRET as
// qurl-oauth-state.test.js so cross-test ordering doesn't matter (per
// PR #177 review on env-var leakage).
process.env.OAUTH_STATE_SECRET = '0'.repeat(64);
process.env.AUTH0_DOMAIN = 'layerv-test.auth0.com';
process.env.AUTH0_CLIENT_ID = 'test-client-id';
process.env.AUTH0_CLIENT_SECRET = 'test-client-secret';
process.env.AUTH0_AUDIENCE = 'https://api.layerv.test';
process.env.QURL_ENDPOINT = 'http://localhost:9999';
process.env.BASE_URL = 'http://localhost:3000';
// KEY_ENCRYPTION_KEY required for the persist-time guard added in PR #177
// review round 2; matches the legacy modal-paste path's existing check.
process.env.KEY_ENCRYPTION_KEY = '1'.repeat(64);
// /qurl-oauth router mounts always; OpenNHP is a different gate
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

// commands.js still requires verifyStateBinding for the GitHub OAuth route;
// stub it to avoid pulling in the full command tree at module load.
jest.mock('../src/commands', () => ({
  verifyStateBinding: jest.fn().mockReturnValue(true),
  handleCommand: jest.fn(),
  commands: [],
  registerCommands: jest.fn(),
}));

const request = require('supertest');
const { app } = require('../src/server');
const db = require('../src/store');
const discord = require('../src/discord');
const { signQurlOAuthState } = require('../src/utils/qurl-oauth-state');

const originalFetch = globalThis.fetch;

beforeEach(() => {
  jest.clearAllMocks();
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
      // scopes include qurl:write + qurl:read; the state echoes back so the
      // callback can re-verify the same binding.
      expect(loc.searchParams.get('scope')).toContain('qurl:write');
      expect(loc.searchParams.get('scope')).toContain('qurl:read');
      // offline_access dropped per PR #177 review — no refresh-token use.
      expect(loc.searchParams.get('scope')).not.toContain('offline_access');
      // prompt=consent is load-bearing for key rotation — re-running
      // /qurl setup must actually re-prompt. Pin it here so a future
      // refactor doesn't silently drop it.
      expect(loc.searchParams.get('prompt')).toBe('consent');
      expect(loc.searchParams.get('state')).toBe(state);
      expect(loc.searchParams.get('redirect_uri')).toBe('http://localhost:3000/oauth/qurl/callback');
    });

    it('sets Secure flag on the cookie when behind a proxy that sets X-Forwarded-Proto: https', async () => {
      // Defense vs trust-proxy regression: server.js sets `trust proxy`
      // so req.protocol reflects X-Forwarded-Proto from the ALB. Flipping
      // that off would silently downgrade prod cookies to insecure. Pin
      // the wire-level shape here.
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
      // Cookie path /oauth (not /oauth/qurl) so it's also visible at
      // /oauth/discord/callback for the Stage-2 chain.
      expect(cookieHeader).toMatch(/Path=\/oauth(?:;|\s|$)/);
      // Cookie value is the state itself (double-submit pattern); the
      // callback re-checks cookie === query.state.
      expect(cookieHeader).toContain(encodeURIComponent(state));
    });

    it('400s on missing state', async () => {
      const res = await request(app).get('/oauth/qurl/start');
      expect(res.status).toBe(400);
      expect(res.text).toContain('Invalid setup link');
    });

    it('503s when KEY_ENCRYPTION_KEY is unset (fail-fast before Auth0 round-trip)', async () => {
      // Pre-Auth0 guard — refusing here keeps the admin from completing
      // the full sign-in + consent dance only to fail at the persist
      // step. Mirrors the legacy modal-paste path's pre-modal guard.
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

    it('400s on tampered state', async () => {
      const state = signQurlOAuthState('guild-1', 'admin-2');
      // Flip the last char of the sig.
      const tampered = state.slice(0, -1) + (state.slice(-1) === '0' ? '1' : '0');
      const res = await request(app).get(`/oauth/qurl/start?state=${encodeURIComponent(tampered)}`);
      expect(res.status).toBe(400);
    });
  });

  describe('GET /oauth/qurl/callback', () => {
    // Helper: build the Cookie header value the double-submit CSRF check
    // expects on /callback. /start sets `qurl_setup_session=<state>`
    // (URL-encoded so `.` and `=` survive); the verifier reads it back
    // via decodeURIComponent. Tests skip the /start round-trip and set
    // the cookie directly to keep each callback test independent.
    const cookieFor = (state) => `qurl_setup_session=${encodeURIComponent(state)}`;

    it('400s on missing code', async () => {
      const state = signQurlOAuthState('guild-1', 'admin-2');
      const res = await request(app).get(`/oauth/qurl/callback?state=${encodeURIComponent(state)}`)
        .set('Cookie', cookieFor(state));
      expect(res.status).toBe(400);
      expect(res.text).toContain('Missing authorization code');
    });

    it('400s on Auth0 error param (admin declined consent)', async () => {
      const state = signQurlOAuthState('guild-1', 'admin-2');
      const res = await request(app).get(
        `/oauth/qurl/callback?state=${encodeURIComponent(state)}&error=access_denied&error_description=user+declined`,
      ).set('Cookie', cookieFor(state));
      expect(res.status).toBe(400);
      expect(res.text).toContain('Authorization declined');
    });

    it('400s on invalid state', async () => {
      const res = await request(app).get('/oauth/qurl/callback?code=auth0-code&state=garbage');
      expect(res.status).toBe(400);
    });

    it('400s on missing CSRF cookie (leaked URL opened in different browser)', async () => {
      const state = signQurlOAuthState('guild-1', 'admin-2');
      // No .set('Cookie', ...) — simulates a leaked URL opened in a fresh
      // browser session. Must reject BEFORE any Auth0 token exchange.
      const res = await request(app).get(
        `/oauth/qurl/callback?code=auth0-code&state=${encodeURIComponent(state)}`,
      );
      expect(res.status).toBe(400);
      expect(res.text).toMatch(/same browser tab/i);
    });

    it('400s on cookie/state mismatch (cookie from a different state)', async () => {
      const stateA = signQurlOAuthState('guild-1', 'admin-2');
      const stateB = signQurlOAuthState('guild-1', 'admin-2'); // different nonce → different state
      const res = await request(app).get(
        `/oauth/qurl/callback?code=auth0-code&state=${encodeURIComponent(stateA)}`,
      ).set('Cookie', cookieFor(stateB));
      expect(res.status).toBe(400);
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

    it('200s on happy path: mints key, persists, DMs admin, renders success with binding readout', async () => {
      const state = signQurlOAuthState('guild-1', 'admin-2');
      // id_token with an `email` claim — base64url-encoded JSON, no
      // signature verification needed for a display-only readout (token
      // came from Auth0 over TLS in the same response).
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
      const res = await request(app).get(
        `/oauth/qurl/callback?code=auth0-code&state=${encodeURIComponent(state)}`,
      ).set('Cookie', cookieFor(state));
      expect(res.status).toBe(200);
      expect(res.text).toContain('qURL is connected');
      expect(db.setGuildApiKey).toHaveBeenCalledWith('guild-1', 'lv_live_abc123', 'admin-2');
      expect(discord.sendDM).toHaveBeenCalledWith('admin-2', expect.stringContaining('qURL is connected'));

      // Confused-deputy mitigation: the success page must surface the
      // bound (guild_id, qURL email, key prefix) tuple as readable
      // text — NOT escaped HTML angle brackets. This is the load-
      // bearing visual cue the admin uses to spot a mismatched
      // binding before closing the tab.
      expect(res.text).toContain('Discord guild: guild-1');
      expect(res.text).toContain('qURL account: alice@layerv.test');
      expect(res.text).toContain('API key prefix: lv_live_abc1');
      // Belt-and-suspenders: assert NO literal escaped HTML lands in
      // the subtext (e.g. `&lt;code&gt;...&lt;/code&gt;`) — that would
      // mean we accidentally double-escaped the binding values.
      expect(res.text).not.toMatch(/&lt;code&gt;/);
    });

    it('500s when persist fails after successful mint, and best-effort deletes the orphan key', async () => {
      const state = signQurlOAuthState('guild-1', 'admin-2');
      const fetchSpy = jest.fn()
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({ access_token: 'jwt-xyz' }),
        })
        .mockResolvedValueOnce({
          ok: true, status: 201,
          json: () => Promise.resolve({ data: { key_id: 'key-orphan-1', api_key: 'lv_live_abc123', key_prefix: 'lv_live_abc1' } }),
        })
        // Best-effort orphan delete
        .mockResolvedValueOnce({ ok: true, status: 204, text: () => Promise.resolve('') });
      globalThis.fetch = fetchSpy;
      db.setGuildApiKey.mockRejectedValueOnce(new Error('DDB throttled'));
      const res = await request(app).get(
        `/oauth/qurl/callback?code=auth0-code&state=${encodeURIComponent(state)}`,
      ).set('Cookie', cookieFor(state));
      expect(res.status).toBe(500);
      expect(res.text).toContain('provisioned but not stored');
      // .then() of the fire-and-forget delete may settle after the
      // response goes out; await one event-loop turn so the assertion
      // sees the third fetch call.
      await new Promise((resolve) => setImmediate(resolve));
      const deleteCall = fetchSpy.mock.calls.find((c) => typeof c[1]?.method === 'string' && c[1].method === 'DELETE');
      expect(deleteCall).toBeDefined();
      expect(deleteCall[0]).toContain('/v1/api-keys/key-orphan-1');
    });
  });
});

// Separate describe — exercises the not-configured 503 path. Uses
// jest.isolateModules with the AUTH0_* env vars temporarily wiped so
// the route's `config.isQurlOAuthConfigured` evaluates false on this
// branch only. Without isolateModules, unsetting env after `config.js`
// has been required wouldn't change the cached `isQurlOAuthConfigured`.
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
        // Re-mock dependencies inside the isolate so the freshly-loaded
        // server.js + router pick them up.
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
        const res = await supertest(freshApp).get('/oauth/qurl/start?state=anything');
        expect(res.status).toBe(503);
        expect(res.text).toMatch(/not configured/i);
      });
    } finally {
      // Restore env so subsequent tests run against the configured router.
      Object.assign(process.env, saved);
    }
  });
});
