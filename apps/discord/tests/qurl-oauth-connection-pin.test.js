// The AUTH0_EMAIL_CONNECTION pin on the /qurl setup authorize redirect.
// Lives apart from qurl-oauth.test.js because config.js reads the env at
// require time: the router has to be loaded with the variable set.

process.env.AUTH0_DOMAIN = 'layerv-test.auth0.com';
process.env.AUTH0_CLIENT_ID = 'test-client-id';
process.env.AUTH0_CLIENT_SECRET = 'test-client-secret';
process.env.AUTH0_AUDIENCE = 'https://api.layerv.test';
process.env.QURL_ENDPOINT = 'http://localhost:9999';
process.env.BASE_URL = 'http://localhost:3000';
process.env.KEY_ENCRYPTION_KEY = '1'.repeat(64);
process.env.GUILD_ID = '123456789012345678';
// Surrounding whitespace is an easy SSM/tfvars slip; config.js trims it.
process.env.AUTH0_EMAIL_CONNECTION = ' email ';

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
const { signQurlOAuthState } = require('../src/utils/qurl-oauth-state');

describe('GET /oauth/qurl/start — AUTH0_EMAIL_CONNECTION pin', () => {
  it('pins the trimmed connection alongside the forced login + consent', async () => {
    const state = signQurlOAuthState('guild-1', 'admin-2');
    const res = await request(app).get(`/oauth/qurl/start?state=${encodeURIComponent(state)}`);
    expect(res.status).toBe(302);
    const loc = new URL(res.headers.location);
    expect(loc.searchParams.get('connection')).toBe('email');
    expect(loc.searchParams.get('prompt')).toBe('login consent');
  });
});
