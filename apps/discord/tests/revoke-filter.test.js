/**
 * Integration test for the `/qurl revoke` dropdown filter.
 *
 * The headline bug this PR fixes — "revoked sends stay visible in the
 * dropdown" — is invisible to the existing unit tests because they
 * mock `db.getRecentSends` directly. This test hits the real
 * better-sqlite3 layer (in-memory via `DATABASE_PATH=:memory:`) to
 * verify the actual SQL: `markSendRevoked` flips the state, and the
 * next `getRecentSends` call filters the revoked send out.
 *
 * Without this coverage, a future edit that breaks the LEFT JOIN or
 * the `c.revoked_at IS NULL` predicate in getRecentSends would go
 * undetected until it regressed in prod.
 */

jest.mock('../src/config', () => ({
  DATABASE_PATH: ':memory:',
  // Supplied to keep the require graph from exploding on missing env
  // vars — database.js reads PENDING_LINK_EXPIRY_MINUTES during its
  // startup `cleanupExpiredPendingLinks` call.
  PENDING_LINK_EXPIRY_MINUTES: 30,
  QURL_API_KEY: 'x',
  QURL_ENDPOINT: 'https://api.test.local',
  GUILD_ID: 'guild-1',
  isMultiTenant: false,
  isOpenNHPActive: false,
  ENABLE_OPENNHP_FEATURES: false,
}));

jest.mock('../src/logger', () => ({
  info: jest.fn(),
  warn: jest.fn(),
  error: jest.fn(),
  debug: jest.fn(),
  audit: jest.fn(),}));

const db = require('../src/database');

function seedSend({ sendId, senderDiscordId, resourceType = 'file', recipients = 1, withConfig = true }) {
  for (let i = 0; i < recipients; i++) {
    db.recordQURLSend({
      sendId,
      senderDiscordId,
      recipientDiscordId: `recipient-${sendId}-${i}`,
      resourceId: `res-${sendId}-${i}`,
      resourceType,
      qurlLink: `https://q.test/${sendId}-${i}`,
      expiresIn: '24h',
      channelId: 'channel-1',
      targetType: 'channel',
    });
  }
  if (withConfig) {
    db.saveSendConfig({
      sendId,
      senderDiscordId,
      resourceType,
      connectorResourceId: null,
      actualUrl: null,
      expiresIn: '24h',
      personalMessage: null,
      locationName: null,
      attachmentName: `${sendId}.pdf`,
      attachmentContentType: 'application/pdf',
      attachmentUrl: null,
    });
  }
}

describe('/qurl revoke dropdown filter (integration)', () => {
  const sender = 'sender-1';

  it('excludes sends marked revoked from getRecentSends, keeps unrevoked ones', () => {
    seedSend({ sendId: 'keep-me', senderDiscordId: sender });
    seedSend({ sendId: 'revoke-me', senderDiscordId: sender });

    // Sanity: both visible before revocation.
    const beforeIds = db.getRecentSends(sender, 10).map(s => s.send_id).sort();
    expect(beforeIds).toEqual(['keep-me', 'revoke-me']);

    db.markSendRevoked('revoke-me', sender);

    const afterIds = db.getRecentSends(sender, 10).map(s => s.send_id);
    expect(afterIds).toEqual(['keep-me']);
  });

  it('markSendRevoked is idempotent — second call is a no-op', () => {
    seedSend({ sendId: 'double-revoke', senderDiscordId: sender });
    db.markSendRevoked('double-revoke', sender);
    expect(() => db.markSendRevoked('double-revoke', sender)).not.toThrow();
    const afterIds = db.getRecentSends(sender, 10).map(s => s.send_id);
    expect(afterIds).not.toContain('double-revoke');
  });

  it('markSendRevoked only affects the send owner', () => {
    seedSend({ sendId: 'cross-user', senderDiscordId: sender });
    db.markSendRevoked('cross-user', 'different-sender');
    const afterIds = db.getRecentSends(sender, 10).map(s => s.send_id);
    expect(afterIds).toContain('cross-user');
  });

  it('backfills a config row for legacy sends (qurl_sends without qurl_send_configs)', () => {
    // Legacy path: the failure class flagged in code review —
    // markSendRevoked without a fallback UPDATE'd 0 rows and the send
    // reappeared in the dropdown on re-revoke.
    seedSend({ sendId: 'legacy', senderDiscordId: sender, withConfig: false });

    const beforeIds = db.getRecentSends(sender, 10).map(s => s.send_id);
    expect(beforeIds).toContain('legacy');

    db.markSendRevoked('legacy', sender);

    const afterIds = db.getRecentSends(sender, 10).map(s => s.send_id);
    expect(afterIds).not.toContain('legacy');
  });
});
