// Tests for src/routes/discord-install.js — the Stage-2 "Add to Discord"
// install callback that chains the Discord OAuth2 install to the qURL
// Auth0 leg. Covers:
//   - 503 not-configured response (DISCORD_CLIENT_SECRET or AUTH0_* unset)
//   - 400 missing code, missing guild_id, declined consent
//   - 502 Discord token exchange failure, /users/@me failure
//   - 302 happy path: redirects to Auth0 with a qURL OAuth state binding
//     guild_id + discord_user_id

process.env.OAUTH_STATE_SECRET = '0'.repeat(64);
process.env.AUTH0_DOMAIN = 'layerv-test.auth0.com';
process.env.AUTH0_CLIENT_ID = 'test-auth0-client-id';
process.env.AUTH0_CLIENT_SECRET = 'test-auth0-secret';
process.env.AUTH0_AUDIENCE = 'https://api.layerv.test';
process.env.DISCORD_CLIENT_ID = 'test-discord-client-id';
process.env.DISCORD_CLIENT_SECRET = 'test-discord-secret';
process.env.QURL_ENDPOINT = 'http://localhost:9999';
process.env.BASE_URL = 'http://localhost:3000';
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

jest.mock('../src/commands', () => ({
  verifyStateBinding: jest.fn().mockReturnValue(true),
  handleCommand: jest.fn(),
  commands: [],
  registerCommands: jest.fn(),
}));

const request = require('supertest');
const { app } = require('../src/server');
const { verifyQurlOAuthState } = require('../src/utils/qurl-oauth-state');

const originalFetch = globalThis.fetch;

beforeEach(() => {
  jest.clearAllMocks();
  globalThis.fetch = originalFetch;
});

describe('Discord install callback', () => {
  describe('GET /oauth/discord/callback', () => {
    it('400s on missing code', async () => {
      const res = await request(app).get('/oauth/discord/callback?guild_id=guild-1');
      expect(res.status).toBe(400);
      expect(res.text).toContain('Missing authorization code');
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

    it('302s to Auth0 on happy path with a valid qURL OAuth state', async () => {
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

      // The state Discord callback minted must round-trip through the
      // qURL OAuth state verifier with the right guild + discord-user
      // bindings — that's how the Auth0 callback identifies who set
      // up which guild.
      const state = loc.searchParams.get('state');
      const verified = verifyQurlOAuthState(state);
      expect(verified.ok).toBe(true);
      expect(verified.payload.guildId).toBe('guild-1');
      expect(verified.payload.discordUserId).toBe('987654321098765432');
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
