/**
 * Tests for generateState/verifyStateBinding in commands.js — OAuth state
 * HMAC binding to discord user id (round 28 defense-in-depth).
 */

jest.mock('../src/config', () => ({
  GITHUB_CLIENT_ID: 'client',
  GITHUB_CLIENT_SECRET: 'c'.repeat(64),
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
function makeState(discordId, secret = 'c'.repeat(64)) {
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

  describe('secret precedence (#184)', () => {
    let savedGitHubStateSecret;
    let savedQurlStateSecret;
    let savedSharedStateSecret;

    beforeEach(() => {
      savedGitHubStateSecret = process.env.GITHUB_OAUTH_STATE_SECRET;
      savedQurlStateSecret = process.env.QURL_OAUTH_STATE_SECRET;
      savedSharedStateSecret = process.env.OAUTH_STATE_SECRET;
    });

    afterEach(() => {
      if (savedGitHubStateSecret === undefined) delete process.env.GITHUB_OAUTH_STATE_SECRET;
      else process.env.GITHUB_OAUTH_STATE_SECRET = savedGitHubStateSecret;
      if (savedQurlStateSecret === undefined) delete process.env.QURL_OAUTH_STATE_SECRET;
      else process.env.QURL_OAUTH_STATE_SECRET = savedQurlStateSecret;
      if (savedSharedStateSecret === undefined) delete process.env.OAUTH_STATE_SECRET;
      else process.env.OAUTH_STATE_SECRET = savedSharedStateSecret;
    });

    it('signs GitHub OAuth state with GITHUB_OAUTH_STATE_SECRET when set', () => {
      process.env.GITHUB_OAUTH_STATE_SECRET = 'g'.repeat(64);
      process.env.OAUTH_STATE_SECRET = 's'.repeat(64);

      const state = makeState('12345', process.env.GITHUB_OAUTH_STATE_SECRET);
      expect(verifyStateBinding(state, '12345')).toBe(true);
      expect(verifyStateBinding(makeState('12345', 'c'.repeat(64)), '12345')).toBe(false);
    });

    it('accepts OAUTH_STATE_SECRET during cutover and rejects it after removal', () => {
      process.env.GITHUB_OAUTH_STATE_SECRET = 'g'.repeat(64);
      process.env.OAUTH_STATE_SECRET = 's'.repeat(64);
      const legacyState = makeState('12345', process.env.OAUTH_STATE_SECRET);

      expect(verifyStateBinding(legacyState, '12345')).toBe(true);
      delete process.env.OAUTH_STATE_SECRET;
      expect(verifyStateBinding(legacyState, '12345')).toBe(false);
    });

    it('falls back to OAUTH_STATE_SECRET before the GitHub-specific secret exists', () => {
      delete process.env.GITHUB_OAUTH_STATE_SECRET;
      process.env.OAUTH_STATE_SECRET = 's'.repeat(64);

      const state = makeState('12345', process.env.OAUTH_STATE_SECRET);
      expect(verifyStateBinding(state, '12345')).toBe(true);
    });

    it('treats the Terraform SSM placeholder as unset', () => {
      process.env.GITHUB_OAUTH_STATE_SECRET = 'PLACEHOLDER';
      process.env.OAUTH_STATE_SECRET = 's'.repeat(64);

      const legacyState = makeState('12345', process.env.OAUTH_STATE_SECRET);
      const placeholderState = makeState('12345', process.env.GITHUB_OAUTH_STATE_SECRET);

      expect(verifyStateBinding(legacyState, '12345')).toBe(true);
      expect(verifyStateBinding(placeholderState, '12345')).toBe(false);
    });

    it('trims GitHub OAuth state secrets read from env', () => {
      process.env.GITHUB_OAUTH_STATE_SECRET = `  ${'g'.repeat(64)}  `;
      delete process.env.OAUTH_STATE_SECRET;

      const state = makeState('12345', 'g'.repeat(64));
      expect(verifyStateBinding(state, '12345')).toBe(true);
    });

    it('rejects short GitHub OAuth state secrets instead of using them', () => {
      process.env.GITHUB_OAUTH_STATE_SECRET = 'short';
      delete process.env.OAUTH_STATE_SECRET;

      const shortState = makeState('12345', process.env.GITHUB_OAUTH_STATE_SECRET);
      expect(verifyStateBinding(shortState, '12345')).toBe(false);
    });

    it('ignores a short legacy secret when a dedicated GitHub secret is active', () => {
      process.env.GITHUB_OAUTH_STATE_SECRET = 'g'.repeat(64);
      process.env.OAUTH_STATE_SECRET = 'short';

      const primaryState = makeState('12345', process.env.GITHUB_OAUTH_STATE_SECRET);
      const legacyState = makeState('12345', process.env.OAUTH_STATE_SECRET);

      expect(verifyStateBinding(primaryState, '12345')).toBe(true);
      expect(verifyStateBinding(legacyState, '12345')).toBe(false);
    });

    it('does not accept a GitHub-format state signed with the qURL state secret', () => {
      process.env.GITHUB_OAUTH_STATE_SECRET = 'g'.repeat(64);
      process.env.QURL_OAUTH_STATE_SECRET = 'q'.repeat(64);

      const qurlSignedState = makeState('12345', process.env.QURL_OAUTH_STATE_SECRET);
      expect(verifyStateBinding(qurlSignedState, '12345')).toBe(false);
    });
  });
});
