/**
 * Tests for generateState/verifyStateBinding in commands.js — OAuth state
 * HMAC binding to discord user id (round 28 defense-in-depth).
 */

jest.mock('../src/config', () => ({
  DATABASE_PATH: ':memory:',
  GITHUB_CLIENT_ID: 'client',
  GITHUB_CLIENT_SECRET: 'test-client-secret',
  ALLOWED_GITHUB_ORGS: ['opennhp'],
  QURL_SEND_MAX_RECIPIENTS: 10,
  PENDING_LINK_EXPIRY_MINUTES: 10,
  BASE_URL: 'http://localhost:3000',
  NODE_ENV: 'test',
}));

jest.mock('../src/logger', () => ({
  info: jest.fn(), warn: jest.fn(), error: jest.fn(), debug: jest.fn(),
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
function makeState(discordId, secret = 'test-client-secret') {
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
});
