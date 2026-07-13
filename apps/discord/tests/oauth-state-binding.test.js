/**
 * Tests for generateState/verifyStateBinding in commands.js — OAuth state
 * HMAC binding to discord user id (round 28 defense-in-depth).
 *
 * The mocked config deliberately omits OAUTH_STATE_SECRET, so the shared
 * signer (src/utils/oauth-state.js) resolves the GITHUB_CLIENT_SECRET
 * fallback — deterministic regardless of what other suites in the same
 * worker leave in process.env. The fixture must clear the signer's
 * 32-char MIN_STATE_SECRET_LENGTH floor, which now applies to the
 * GitHub OAuth flow too (it used to accept any length).
 */

const mockGithubClientSecret = 'test-client-secret-0123456789abcdef';

jest.mock('../src/config', () => ({
  GITHUB_CLIENT_ID: 'client',
  GITHUB_CLIENT_SECRET: mockGithubClientSecret,
  ALLOWED_GITHUB_ORGS: ['opennhp'],
  QURL_SEND_MAX_RECIPIENTS: 10,
  PENDING_LINK_EXPIRY_MINUTES: 10,
  BASE_URL: 'http://localhost:3000',
  NODE_ENV: 'test',
}));

jest.mock('../src/logger', () => ({
  info: jest.fn(), warn: jest.fn(), error: jest.fn(), debug: jest.fn(), audit: jest.fn(),
}));

jest.mock('../src/discord', () => ({
  client: { on: jest.fn(), login: jest.fn() },
  sendDM: jest.fn(), assignContributorRole: jest.fn(),
  notifyBadgeEarned: jest.fn(), notifyPRMerge: jest.fn(),
}));

const { verifyStateBinding } = require('../src/commands');
const crypto = require('crypto');

// Re-implement generateState locally so we can sign test states without
// going through the full /link command path.
function makeState(discordId, secret = mockGithubClientSecret) {
  const nonce = crypto.randomBytes(16).toString('hex');
  const sig = crypto.createHmac('sha256', secret)
    .update(`${discordId}:${nonce}`).digest('hex');
  return `${nonce}.${sig}`;
}

describe('verifyStateBinding', () => {
  it('accepts a state signed for the correct discord id', () => {
    const state = makeState('12345');
    expect(verifyStateBinding(state, '12345')).toBe(true);
  });

  it('rejects a state signed for a different discord id', () => {
    const state = makeState('12345');
    expect(verifyStateBinding(state, '67890')).toBe(false);
  });

  it('rejects malformed state (no dot)', () => {
    expect(verifyStateBinding('abcdef', '12345')).toBe(false);
  });

  it('rejects state with non-hex nonce', () => {
    expect(verifyStateBinding('zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz.' + 'a'.repeat(64), '12345')).toBe(false);
  });

  it('rejects state with wrong-length signature', () => {
    const nonce = crypto.randomBytes(16).toString('hex');
    expect(verifyStateBinding(`${nonce}.abc`, '12345')).toBe(false);
  });

  it('rejects non-string state', () => {
    expect(verifyStateBinding(null, '12345')).toBe(false);
    expect(verifyStateBinding(undefined, '12345')).toBe(false);
    expect(verifyStateBinding(123, '12345')).toBe(false);
  });

  it('rejects state signed with a different secret', () => {
    const state = makeState('12345', 'other-secret');
    expect(verifyStateBinding(state, '12345')).toBe(false);
  });

  it('throws (not false) on a well-formed state when the resolved secret is under the floor', () => {
    // Pins the headline behavior change at the surface the /auth
    // callback route actually calls (routes/oauth.js): a sub-32-char
    // secret makes verifyStateBinding THROW once the state passes the
    // format gates — the callback's own try/catch renders its 500
    // page. Previously it silently verified against the short secret.
    // The signer resolves config lazily per call, so mutating the
    // mocked config object here is observed.
    const config = require('../src/config');
    const saved = config.GITHUB_CLIENT_SECRET;
    config.GITHUB_CLIENT_SECRET = 'shrt';
    try {
      const state = makeState('12345', 'shrt');
      expect(() => verifyStateBinding(state, '12345')).toThrow(
        /Refusing to mint OAuth state: GITHUB_CLIENT_SECRET is shorter than 32 chars/,
      );
    } finally {
      config.GITHUB_CLIENT_SECRET = saved;
    }
  });
});
