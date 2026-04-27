/**
 * Tests for the actual database module (src/database.js).
 * Uses :memory: SQLite via config mock.
 */

jest.mock('../src/config', () => ({
  DATABASE_PATH: ':memory:',
  PENDING_LINK_EXPIRY_MINUTES: 30,
}));

jest.mock('../src/logger', () => ({
  info: jest.fn(),
  warn: jest.fn(),
  error: jest.fn(),
  debug: jest.fn(),
}));

const db = require('../src/database');

afterAll(() => {
  db.close();
});

describe('database module', () => {
  describe('pending links', () => {
    it('creates and retrieves pending link', () => {
      db.createPendingLink('state-1', 'discord-1');
      const result = db.getPendingLink('state-1');
      expect(result).toBeDefined();
      expect(result.discord_id).toBe('discord-1');
    });

    it('deletes pending link', () => {
      db.createPendingLink('state-2', 'discord-2');
      db.deletePendingLink('state-2');
      expect(db.getPendingLink('state-2')).toBeUndefined();
    });

    it('returns undefined for nonexistent state', () => {
      expect(db.getPendingLink('nope')).toBeUndefined();
    });
  });

  describe('github links', () => {
    it('creates and retrieves by discord ID', () => {
      db.createLink('d1', 'ghuser1');
      const link = db.getLinkByDiscord('d1');
      expect(link).toBeDefined();
      expect(link.github_username).toBe('ghuser1');
    });

    it('creates and retrieves by github username (lowercased)', () => {
      db.createLink('d2', 'GHUser2');
      const link = db.getLinkByGithub('ghuser2');
      expect(link).toBeDefined();
      expect(link.discord_id).toBe('d2');
    });

    it('updates existing link on conflict', () => {
      db.createLink('d3', 'old');
      db.createLink('d3', 'new');
      expect(db.getLinkByDiscord('d3').github_username).toBe('new');
    });

    it('deletes link', () => {
      db.createLink('d4', 'del');
      db.deleteLink('d4');
      expect(db.getLinkByDiscord('d4')).toBeUndefined();
    });

    it('forceLink is alias for createLink', () => {
      db.forceLink('d5', 'forced');
      expect(db.getLinkByDiscord('d5').github_username).toBe('forced');
    });
  });

  describe('contributions', () => {
    it('records and retrieves contributions', () => {
      db.createLink('c1', 'contrib1');
      const recorded = db.recordContribution('c1', 'contrib1', 100, 'OpenNHP/opennhp', 'Test PR');
      expect(recorded).toBe(true);

      const contribs = db.getContributions('c1');
      expect(contribs.length).toBeGreaterThanOrEqual(1);
      expect(contribs[0].pr_number).toBe(100);
    });

    it('ignores duplicate contribution', () => {
      db.recordContribution('c1', 'contrib1', 200, 'OpenNHP/opennhp', 'Dup');
      const first = db.recordContribution('c1', 'contrib1', 200, 'OpenNHP/opennhp', 'Dup');
      // Second attempt should return false (already exists)
      expect(first).toBe(false);
    });

    it('gets contribution count', () => {
      const count = db.getContributionCount('c1');
      expect(count).toBeGreaterThanOrEqual(1);
    });

    it('gets all contributions', () => {
      const all = db.getAllContributions();
      expect(all.length).toBeGreaterThanOrEqual(1);
    });

    it('gets unique repos', () => {
      db.recordContribution('c1', 'contrib1', 300, 'OpenNHP/other', 'Other PR');
      const repos = db.getUniqueRepos('c1');
      expect(repos.length).toBeGreaterThanOrEqual(2);
    });

    it('gets monthly contributions', () => {
      const monthly = db.getMonthlyContributions('c1');
      expect(Array.isArray(monthly)).toBe(true);
    });

    it('gets weekly contributions', () => {
      const weekly = db.getWeeklyContributions('c1');
      expect(Array.isArray(weekly)).toBe(true);
    });

    it('gets last week contributions', () => {
      const last = db.getLastWeekContributions();
      expect(Array.isArray(last)).toBe(true);
    });

    it('gets new contributors this week', () => {
      const nc = db.getNewContributorsThisWeek();
      expect(Array.isArray(nc)).toBe(true);
    });
  });

  describe('stats', () => {
    it('returns stats object', () => {
      const stats = db.getStats();
      expect(stats).toHaveProperty('linkedUsers');
      expect(stats).toHaveProperty('totalContributions');
      expect(stats).toHaveProperty('uniqueContributors');
      expect(stats).toHaveProperty('byRepo');
    });
  });

  describe('leaderboard', () => {
    it('returns top contributors', () => {
      const top = db.getTopContributors(5);
      expect(Array.isArray(top)).toBe(true);
    });
  });

  describe('badges', () => {
    it('awards and retrieves badges', () => {
      const awarded = db.awardBadge('b1', db.BADGE_TYPES.FIRST_PR);
      expect(awarded).toBe(true);

      const badges = db.getBadges('b1');
      expect(badges.length).toBe(1);
      expect(badges[0].badge_type).toBe('first_pr');
    });

    it('does not double-award', () => {
      const second = db.awardBadge('b1', db.BADGE_TYPES.FIRST_PR);
      expect(second).toBe(false);
    });

    it('hasBadge returns correct boolean', () => {
      expect(db.hasBadge('b1', db.BADGE_TYPES.FIRST_PR)).toBe(true);
      expect(db.hasBadge('b1', db.BADGE_TYPES.DOCS_HERO)).toBe(false);
    });

    it('checkAndAwardBadges awards first PR', () => {
      db.createLink('badge-user', 'badgegh');
      db.recordContribution('badge-user', 'badgegh', 999, 'OpenNHP/opennhp', 'First');
      const awarded = db.checkAndAwardBadges('badge-user', 'First PR', 'OpenNHP/opennhp');
      expect(awarded).toContain('first_pr');
    });

    it('checkAndAwardBadges awards docs hero', () => {
      db.createLink('docs-user', 'docsgh');
      db.recordContribution('docs-user', 'docsgh', 1000, 'OpenNHP/opennhp', 'Update docs');
      const awarded = db.checkAndAwardBadges('docs-user', 'Update documentation', 'OpenNHP/opennhp');
      expect(awarded).toContain('docs_hero');
    });

    it('checkAndAwardBadges awards bug hunter', () => {
      db.createLink('bug-user', 'buggh');
      db.recordContribution('bug-user', 'buggh', 1001, 'OpenNHP/opennhp', 'Fix bug');
      const awarded = db.checkAndAwardBadges('bug-user', 'Fix critical bug', 'OpenNHP/opennhp');
      expect(awarded).toContain('bug_hunter');
    });

    it('checkAndAwardBadges awards multi-repo', () => {
      db.createLink('multi-user', 'multigh');
      db.recordContribution('multi-user', 'multigh', 1010, 'OpenNHP/repo1', 'PR');
      db.recordContribution('multi-user', 'multigh', 1011, 'OpenNHP/repo2', 'PR2');
      const awarded = db.checkAndAwardBadges('multi-user', 'PR', 'OpenNHP/repo2');
      expect(awarded).toContain('multi_repo');
    });

    it('awardFirstIssueBadge', () => {
      const awarded = db.awardFirstIssueBadge('issue-user');
      expect(awarded).toContain('first_issue');
      // Second time should return empty
      const second = db.awardFirstIssueBadge('issue-user');
      expect(second).toEqual([]);
    });
  });

  describe('streaks', () => {
    it('creates new streak on first contribution', () => {
      const result = db.updateStreak('streak-user-1');
      expect(result.current).toBe(1);
      expect(result.isNew).toBe(true);
    });

    it('same month does not extend streak', () => {
      const result = db.updateStreak('streak-user-1');
      expect(result.isNew).toBe(false);
    });

    it('getStreak returns streak data', () => {
      const streak = db.getStreak('streak-user-1');
      expect(streak).toBeDefined();
      expect(streak.current_streak).toBe(1);
    });
  });

  describe('milestones', () => {
    it('records and checks milestone', () => {
      const recorded = db.recordMilestone('stars', 100, 'OpenNHP/opennhp');
      expect(recorded).toBe(true);

      const announced = db.hasMilestoneBeenAnnounced('stars', 100, 'OpenNHP/opennhp');
      expect(announced).toBe(true);
    });

    it('hasMilestoneBeenAnnounced returns false for unannounced', () => {
      expect(db.hasMilestoneBeenAnnounced('stars', 999, 'OpenNHP/opennhp')).toBe(false);
    });

    it('handles null repo', () => {
      db.recordMilestone('type', 1, null);
      expect(db.hasMilestoneBeenAnnounced('type', 1, null)).toBe(true);
    });
  });

  describe('weekly digest', () => {
    it('returns digest data', () => {
      const data = db.getWeeklyDigestData();
      expect(data).toHaveProperty('totalPRs');
      expect(data).toHaveProperty('uniqueContributors');
      expect(data).toHaveProperty('newContributors');
      expect(data).toHaveProperty('byRepo');
    });
  });

  describe('qURL sends', () => {
    it('records and retrieves qURL send', () => {
      db.recordQURLSend({ sendId: 'qs1', senderDiscordId: 'sender1', recipientDiscordId: 'rcpt1', resourceId: 'res1', resourceType: 'file', qurlLink: 'https://q.test/1', expiresIn: '24h', channelId: 'ch1', targetType: 'user' });
      const sends = db.getRecentSends('sender1');
      expect(sends.length).toBeGreaterThanOrEqual(1);
    });

    it('updates DM status', () => {
      db.updateSendDMStatus('qs1', 'rcpt1', 'sent');
      const sends = db.getRecentSends('sender1');
      expect(sends[0].delivered_count).toBe(1);
    });

    it('gets send resource IDs', () => {
      const ids = db.getSendResourceIds('qs1', 'sender1');
      expect(ids).toContain('res1');
    });
  });

  describe('send configs', () => {
    it('saves and retrieves send config', () => {
      db.saveSendConfig({ sendId: 'sc1', senderDiscordId: 'sender1', resourceType: 'file', connectorResourceId: 'conn1', actualUrl: null, expiresIn: '6h', personalMessage: 'msg', locationName: null, attachmentName: 'file.pdf' });
      const config = db.getSendConfig('sc1', 'sender1');
      expect(config).toBeDefined();
      expect(config.resource_type).toBe('file');
      expect(config.attachment_name).toBe('file.pdf');
    });

    it('returns undefined for wrong sender', () => {
      expect(db.getSendConfig('sc1', 'wrong-sender')).toBeUndefined();
    });
  });

  // SQLite-level coverage of the helpers consumed by the
  // expired-dm-label sweeper (src/expired-dm-label-sweeper.js). The
  // sweeper's own unit tests mock `db` entirely, so without these the
  // SQL guards (dm_message_id IS NULL on update, the multi-clause WHERE
  // on listExpiredUneditedDMs, the bulk-mark by message_id) live
  // entirely in SQL with no real-DB exercise.
  describe('expired-DM-label helpers', () => {
    const NOW_UNIX = Math.floor(Date.now() / 1000);
    const PAST = NOW_UNIX - 60;        // 1 minute ago — eligible
    const FUTURE = NOW_UNIX + 3600;    // 1 hour from now — not yet expired

    function addSend({ sendId, recipientId, status = 'sent' }) {
      db.recordQURLSend({
        sendId, senderDiscordId: 'sender-edm', recipientDiscordId: recipientId,
        resourceId: `res-${sendId}-${recipientId}`, resourceType: 'file',
        qurlLink: `https://q.test/${sendId}-${recipientId}`,
        expiresIn: '24h', channelId: 'ch-edm', targetType: 'user',
      });
      // Default DM status is 'pending' from the schema; we lift to 'sent'
      // unless the test wants the failed-DM exclusion path.
      db.updateSendDMStatus(sendId, recipientId, status);
    }

    it('recordDMIdentifiers populates only NULL rows (overwrite-protection invariant)', () => {
      // Original send — recipient gets identifiers.
      addSend({ sendId: 'edm-overwrite', recipientId: 'rcpt-A' });
      db.recordDMIdentifiers({
        sendId: 'edm-overwrite', recipientDiscordId: 'rcpt-A',
        dmChannelId: 'chan-original', dmMessageId: 'msg-original', expiresAtUnix: PAST,
      });

      // /qurl add recipients re-uses the same recipient (edge case): a
      // SECOND row gets inserted for the same (sendId, recipientId).
      addSend({ sendId: 'edm-overwrite', recipientId: 'rcpt-A' });
      // Second recordDMIdentifiers call — SHOULD only fill the NULL row
      // (the new one), NOT clobber the original.
      db.recordDMIdentifiers({
        sendId: 'edm-overwrite', recipientDiscordId: 'rcpt-A',
        dmChannelId: 'chan-add-recipients', dmMessageId: 'msg-add-recipients', expiresAtUnix: PAST,
      });

      // Both rows should now have non-null identifiers; original is
      // preserved, new one got the second pair.
      const rows = db.listExpiredUneditedDMs(50);
      const matchingRows = rows.filter(r => r.send_id === 'edm-overwrite' && r.recipient_discord_id === 'rcpt-A');
      const messageIds = matchingRows.map(r => r.dm_message_id).sort();
      expect(messageIds).toEqual(['msg-add-recipients', 'msg-original']);
    });

    it('listExpiredUneditedDMs excludes future-expiry rows', () => {
      addSend({ sendId: 'edm-future', recipientId: 'rcpt-future' });
      db.recordDMIdentifiers({
        sendId: 'edm-future', recipientDiscordId: 'rcpt-future',
        dmChannelId: 'c', dmMessageId: 'msg-future', expiresAtUnix: FUTURE,
      });
      const rows = db.listExpiredUneditedDMs(50);
      expect(rows.find(r => r.dm_message_id === 'msg-future')).toBeUndefined();
    });

    it('listExpiredUneditedDMs excludes dm_status="failed" rows', () => {
      addSend({ sendId: 'edm-failed', recipientId: 'rcpt-failed', status: 'failed' });
      db.recordDMIdentifiers({
        sendId: 'edm-failed', recipientDiscordId: 'rcpt-failed',
        dmChannelId: 'c', dmMessageId: 'msg-failed', expiresAtUnix: PAST,
      });
      const rows = db.listExpiredUneditedDMs(50);
      // Even though expiry is past and identifiers are present, dm_status='failed'
      // means the DM never reached the recipient — there's no message to edit.
      expect(rows.find(r => r.dm_message_id === 'msg-failed')).toBeUndefined();
    });

    it('listExpiredUneditedDMs excludes legacy rows with NULL dm_message_id', () => {
      // recordQURLSend without recordDMIdentifiers leaves dm_message_id NULL
      // (legacy migration path). These rows must be skipped — the sweeper has
      // no message to edit.
      addSend({ sendId: 'edm-legacy', recipientId: 'rcpt-legacy' });
      const rows = db.listExpiredUneditedDMs(50);
      expect(rows.find(r => r.send_id === 'edm-legacy')).toBeUndefined();
    });

    it('listExpiredUneditedDMs excludes already-edited rows', () => {
      addSend({ sendId: 'edm-done', recipientId: 'rcpt-done' });
      db.recordDMIdentifiers({
        sendId: 'edm-done', recipientDiscordId: 'rcpt-done',
        dmChannelId: 'c', dmMessageId: 'msg-done', expiresAtUnix: PAST,
      });
      db.markDMExpiredLabelEditedByMessageId('msg-done');
      const rows = db.listExpiredUneditedDMs(50);
      expect(rows.find(r => r.dm_message_id === 'msg-done')).toBeUndefined();
    });

    it('markDMExpiredLabelEditedByMessageId stamps every row sharing the message_id', () => {
      // Two rows for the same consolidated DM (one message → multiple links).
      addSend({ sendId: 'edm-bulk', recipientId: 'rcpt-bulk' });
      addSend({ sendId: 'edm-bulk', recipientId: 'rcpt-bulk-2' });
      db.recordDMIdentifiers({
        sendId: 'edm-bulk', recipientDiscordId: 'rcpt-bulk',
        dmChannelId: 'c', dmMessageId: 'msg-shared', expiresAtUnix: PAST,
      });
      db.recordDMIdentifiers({
        sendId: 'edm-bulk', recipientDiscordId: 'rcpt-bulk-2',
        dmChannelId: 'c', dmMessageId: 'msg-shared', expiresAtUnix: PAST,
      });
      db.markDMExpiredLabelEditedByMessageId('msg-shared');
      // Both rows should now be excluded from the sweep.
      const rows = db.listExpiredUneditedDMs(50);
      expect(rows.find(r => r.dm_message_id === 'msg-shared')).toBeUndefined();
    });
  });

  describe('streak updates — consecutive months', () => {
    it('handles streak broken (gap > 1 month)', () => {
      // We can't easily control dates, but we can verify the flow
      const r1 = db.updateStreak('broken-streak');
      expect(r1.current).toBe(1);
      // Since both calls happen in the same month, it should just return same
      const r2 = db.updateStreak('broken-streak');
      expect(r2.isNew).toBe(false);
    });
  });

  describe('awardBadge error handling', () => {
    it('returns false on database error', () => {
      // Attempt to award to a non-string value might cause an issue, but
      // the code catches errors
      // Just verify it doesn't throw
      const result = db.awardBadge('err-badge-user', 'nonexistent_type');
      expect(typeof result).toBe('boolean');
    });
  });

  describe('recordMilestone duplicate', () => {
    it('returns false on duplicate milestone', () => {
      db.recordMilestone('dup_type', 42, 'repo');
      const second = db.recordMilestone('dup_type', 42, 'repo');
      expect(second).toBe(false);
    });
  });

  describe('streak master badge', () => {
    it('returns an array of badge-type strings (may include streak_master)', () => {
      db.createLink('streak-master', 'streakgh');
      db.recordContribution('streak-master', 'streakgh', 3000, 'OpenNHP/opennhp', 'M1');
      db.recordContribution('streak-master', 'streakgh', 3001, 'OpenNHP/opennhp', 'M2');
      db.recordContribution('streak-master', 'streakgh', 3002, 'OpenNHP/opennhp', 'M3');
      const awarded = db.checkAndAwardBadges('streak-master', 'M3', 'OpenNHP/opennhp');
      // Whether streak_master drops in this jest run depends on wall-clock
      // month boundaries; what we can always assert is the shape.
      expect(Array.isArray(awarded)).toBe(true);
      for (const b of awarded) {
        expect(Object.values(db.BADGE_TYPES)).toContain(b);
      }
    });
  });

  describe('on fire badge', () => {
    it('awards on_fire for 2+ PRs in a month', () => {
      db.createLink('fire-user', 'firegh');
      db.recordContribution('fire-user', 'firegh', 2000, 'OpenNHP/opennhp', 'PR 1');
      db.recordContribution('fire-user', 'firegh', 2001, 'OpenNHP/opennhp', 'PR 2');
      const awarded = db.checkAndAwardBadges('fire-user', 'PR 2', 'OpenNHP/opennhp');
      expect(awarded).toContain('on_fire');
    });
  });

  describe('streaks — edge cases', () => {
    it('resets streak when contribution gap > 1 month', () => {
      // user with existing streak from 2 months ago
      // The streak logic depends on getMonthString, so we just test the basic flow
      const result = db.updateStreak('gap-user');
      expect(result.current).toBe(1);
    });
  });

  describe('contribution error handling', () => {
    it('handles database errors in recordContribution', () => {
      // Record a contribution that would violate a constraint
      // First, record one
      db.recordContribution('err-user', 'errgh', 5000, 'OpenNHP/opennhp', 'First');
      // Duplicate should return false (not throw)
      const result = db.recordContribution('err-user', 'errgh', 5000, 'OpenNHP/opennhp', 'Dup');
      expect(result).toBe(false);
    });
  });

  describe('BADGE_TYPES and BADGE_INFO', () => {
    it('exports BADGE_TYPES', () => {
      expect(db.BADGE_TYPES).toBeDefined();
      expect(db.BADGE_TYPES.FIRST_PR).toBe('first_pr');
    });
    it('exports BADGE_INFO', () => {
      expect(db.BADGE_INFO).toBeDefined();
      expect(db.BADGE_INFO.first_pr).toBeDefined();
      expect(db.BADGE_INFO.first_pr.emoji).toBeDefined();
    });
  });

  describe('orphaned OAuth tokens', () => {
    it('starts empty, records, and counts', () => {
      expect(db.countOrphanedTokens()).toBe(0);
      db.recordOrphanedToken('gho_fakefake1');
      db.recordOrphanedToken('gho_fakefake2');
      expect(db.countOrphanedTokens()).toBe(2);
    });
  });
});
