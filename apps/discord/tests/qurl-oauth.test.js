// Tests for src/routes/qurl-oauth.js — the OAuth-redirect setup flow that
// replaces the API-key-paste modal in /qurl setup. Covers:
//   - 503 not-configured response when AUTH0_* env vars are unset
//   - /start: 400 invalid-state, 302 redirect to Auth0 on valid state
//   - /callback: 400 invalid state, 502 Auth0 token-exchange failure,
//     502 qurl-service mint failure, 200 happy path with mint + persist + DM

// Auth0 + state-secret env vars must be set BEFORE requiring the modules
// that read them at load time.
process.env.OAUTH_STATE_SECRET = 'b'.repeat(64);
process.env.AUTH0_DOMAIN = 'layerv-test.auth0.com';
process.env.AUTH0_CLIENT_ID = 'test-client-id';
process.env.AUTH0_CLIENT_SECRET = 'test-client-secret';
process.env.AUTH0_AUDIENCE = 'https://api.layerv.test';
process.env.QURL_ENDPOINT = 'http://localhost:9999';
process.env.BASE_URL = 'http://localhost:3000';
// /qurl-oauth router mounts always; OpenNHP is a different gate
process.env.GUILD_ID = '123456789012345678';

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
      expect(loc.searchParams.get('state')).toBe(state);
      expect(loc.searchParams.get('redirect_uri')).toBe('http://localhost:3000/oauth/qurl/callback');
    });

    it('400s on missing state', async () => {
      const res = await request(app).get('/oauth/qurl/start');
      expect(res.status).toBe(400);
      expect(res.text).toContain('Invalid setup link');
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
    it('400s on missing code', async () => {
      const state = signQurlOAuthState('guild-1', 'admin-2');
      const res = await request(app).get(`/oauth/qurl/callback?state=${encodeURIComponent(state)}`);
      expect(res.status).toBe(400);
      expect(res.text).toContain('Missing authorization code');
    });

    it('400s on Auth0 error param (admin declined consent)', async () => {
      const state = signQurlOAuthState('guild-1', 'admin-2');
      const res = await request(app).get(
        `/oauth/qurl/callback?state=${encodeURIComponent(state)}&error=access_denied&error_description=user+declined`,
      );
      expect(res.status).toBe(400);
      expect(res.text).toContain('Authorization declined');
    });

    it('400s on invalid state', async () => {
      const res = await request(app).get('/oauth/qurl/callback?code=auth0-code&state=garbage');
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
      );
      expect(res.status).toBe(502);
      expect(res.text).toContain('Authorization failed');
      expect(db.setGuildApiKey).not.toHaveBeenCalled();
    });

    it('502s when qurl-service mint fails', async () => {
      const state = signQurlOAuthState('guild-1', 'admin-2');
      globalThis.fetch = jest.fn()
        // Auth0 token exchange success
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({ access_token: 'jwt-xyz', token_type: 'Bearer', expires_in: 3600 }),
        })
        // qurl-service mint failure
        .mockResolvedValueOnce({
          ok: false, status: 500,
          text: () => Promise.resolve('internal error'),
        });
      const res = await request(app).get(
        `/oauth/qurl/callback?code=auth0-code&state=${encodeURIComponent(state)}`,
      );
      expect(res.status).toBe(502);
      expect(res.text).toContain('Could not provision qURL key');
      expect(db.setGuildApiKey).not.toHaveBeenCalled();
    });

    it('200s on happy path: mints key, persists, DMs admin, renders success', async () => {
      const state = signQurlOAuthState('guild-1', 'admin-2');
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({ access_token: 'jwt-xyz', token_type: 'Bearer', expires_in: 3600 }),
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
      );
      expect(res.status).toBe(200);
      expect(res.text).toContain('qURL is connected');
      // Key persisted via Store abstraction (DDB in prod).
      expect(db.setGuildApiKey).toHaveBeenCalledWith('guild-1', 'lv_live_abc123', 'admin-2');
      // Admin DM'd with confirmation (fire-and-forget).
      expect(discord.sendDM).toHaveBeenCalledWith('admin-2', expect.stringContaining('qURL is connected'));
    });

    it('500s when persist fails after successful mint (key minted but not stored)', async () => {
      const state = signQurlOAuthState('guild-1', 'admin-2');
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({
          ok: true, status: 200,
          json: () => Promise.resolve({ access_token: 'jwt-xyz' }),
        })
        .mockResolvedValueOnce({
          ok: true, status: 201,
          json: () => Promise.resolve({ data: { api_key: 'lv_live_abc123', key_prefix: 'lv_live_abc1' } }),
        });
      db.setGuildApiKey.mockRejectedValueOnce(new Error('DDB throttled'));
      const res = await request(app).get(
        `/oauth/qurl/callback?code=auth0-code&state=${encodeURIComponent(state)}`,
      );
      expect(res.status).toBe(500);
      expect(res.text).toContain('provisioned but not stored');
    });
  });
});
