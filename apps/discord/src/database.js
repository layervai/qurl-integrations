const Database = require('better-sqlite3');
const path = require('path');
const fs = require('fs');
const config = require('./config');
const logger = require('./logger');
const { DM_STATUS } = require('./constants');

// Ensure data directory exists
const dbDir = path.dirname(config.DATABASE_PATH);
if (!fs.existsSync(dbDir)) {
  fs.mkdirSync(dbDir, { recursive: true });
}

const db = new Database(config.DATABASE_PATH);
db.pragma('journal_mode = WAL');

// Initialize tables
db.exec(`
  CREATE TABLE IF NOT EXISTS github_links (
    discord_id TEXT PRIMARY KEY,
    github_username TEXT NOT NULL UNIQUE,
    linked_at TEXT DEFAULT CURRENT_TIMESTAMP,
    updated_at TEXT DEFAULT CURRENT_TIMESTAMP
  );

  CREATE TABLE IF NOT EXISTS pending_links (
    state TEXT PRIMARY KEY,
    discord_id TEXT NOT NULL,
    created_at TEXT DEFAULT CURRENT_TIMESTAMP
  );

  CREATE TABLE IF NOT EXISTS contributions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    discord_id TEXT NOT NULL,
    github_username TEXT NOT NULL,
    pr_number INTEGER NOT NULL,
    repo TEXT NOT NULL,
    pr_title TEXT,
    merged_at TEXT DEFAULT CURRENT_TIMESTAMP
  );

  -- Badges earned by users
  CREATE TABLE IF NOT EXISTS badges (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    discord_id TEXT NOT NULL,
    badge_type TEXT NOT NULL,
    earned_at TEXT DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(discord_id, badge_type)
  );

  -- Contribution streaks
  CREATE TABLE IF NOT EXISTS streaks (
    discord_id TEXT PRIMARY KEY,
    current_streak INTEGER DEFAULT 0,
    longest_streak INTEGER DEFAULT 0,
    last_contribution_week TEXT,
    updated_at TEXT DEFAULT CURRENT_TIMESTAMP
  );

  -- Announced milestones (to avoid duplicate announcements)
  CREATE TABLE IF NOT EXISTS milestones (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    milestone_type TEXT NOT NULL,
    milestone_value INTEGER NOT NULL,
    repo TEXT,
    announced_at TEXT DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(milestone_type, milestone_value, repo)
  );

  -- Weekly digest data
  CREATE TABLE IF NOT EXISTS weekly_stats (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    week_start TEXT NOT NULL,
    prs_merged INTEGER DEFAULT 0,
    new_contributors INTEGER DEFAULT 0,
    data TEXT,
    created_at TEXT DEFAULT CURRENT_TIMESTAMP
  );

  CREATE INDEX IF NOT EXISTS idx_github_username ON github_links(github_username);
  CREATE INDEX IF NOT EXISTS idx_contributions_discord ON contributions(discord_id);
  CREATE INDEX IF NOT EXISTS idx_contributions_merged ON contributions(merged_at);
  CREATE UNIQUE INDEX IF NOT EXISTS idx_contributions_unique ON contributions(repo, pr_number);
  CREATE INDEX IF NOT EXISTS idx_badges_discord ON badges(discord_id);

  -- QURL send tracking
  CREATE TABLE IF NOT EXISTS qurl_sends (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    send_id TEXT NOT NULL,
    sender_discord_id TEXT NOT NULL,
    recipient_discord_id TEXT NOT NULL,
    resource_id TEXT NOT NULL,
    resource_type TEXT NOT NULL,
    qurl_link TEXT NOT NULL,
    expires_in TEXT,
    channel_id TEXT,
    target_type TEXT NOT NULL,
    dm_status TEXT DEFAULT 'pending',
    created_at TEXT DEFAULT CURRENT_TIMESTAMP
  );

  CREATE INDEX IF NOT EXISTS idx_qurl_sends_sender ON qurl_sends(sender_discord_id);
  CREATE INDEX IF NOT EXISTS idx_qurl_sends_send_id ON qurl_sends(send_id);
  CREATE INDEX IF NOT EXISTS idx_qurl_sends_created ON qurl_sends(created_at);

  -- QURL send configuration (one row per send, used for "Add Recipients")
  CREATE TABLE IF NOT EXISTS qurl_send_configs (
    send_id TEXT PRIMARY KEY,
    sender_discord_id TEXT NOT NULL,
    resource_type TEXT NOT NULL,
    connector_resource_id TEXT,
    actual_url TEXT,
    expires_in TEXT NOT NULL,
    personal_message TEXT,
    location_name TEXT,
    attachment_name TEXT,
    created_at TEXT DEFAULT CURRENT_TIMESTAMP
  );
`);

// Badge types (thresholds adjusted for realistic open source contribution cadence)
const BADGE_TYPES = {
  FIRST_PR: 'first_pr',           // 🌱 First PR
  FIRST_ISSUE: 'first_issue',     // 💡 First Issue (opened an issue)
  DOCS_HERO: 'docs_hero',         // 📚 Docs Hero (docs PRs)
  BUG_HUNTER: 'bug_hunter',       // 🐛 Bug Hunter (bug fixes)
  ON_FIRE: 'on_fire',             // 🔥 On Fire (2+ PRs in a month)
  STREAK_MASTER: 'streak_master', // 🎯 Streak Master (3 consecutive months)
  MULTI_REPO: 'multi_repo',       // 🌐 Multi-Repo (contributed to 2+ repos)
};

const BADGE_INFO = {
  [BADGE_TYPES.FIRST_PR]: { emoji: '🌱', name: 'First PR', description: 'Merged your first PR' },
  [BADGE_TYPES.FIRST_ISSUE]: { emoji: '💡', name: 'First Issue', description: 'Opened your first issue' },
  [BADGE_TYPES.DOCS_HERO]: { emoji: '📚', name: 'Docs Hero', description: 'Contributed to documentation' },
  [BADGE_TYPES.BUG_HUNTER]: { emoji: '🐛', name: 'Bug Hunter', description: 'Fixed a bug' },
  [BADGE_TYPES.ON_FIRE]: { emoji: '🔥', name: 'On Fire', description: '2+ PRs in one month' },
  [BADGE_TYPES.STREAK_MASTER]: { emoji: '🎯', name: 'Streak Master', description: '3 consecutive months of contributions' },
  [BADGE_TYPES.MULTI_REPO]: { emoji: '🌐', name: 'Multi-Repo', description: 'Contributed to multiple repositories' },
};

// Get month string (YYYY-MM format) - used for monthly streak tracking
function getMonthString(date = new Date()) {
  const d = new Date(date);
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}`;
}

// Get previous month string
function getPreviousMonthString(date = new Date()) {
  const d = new Date(date);
  d.setMonth(d.getMonth() - 1);
  return getMonthString(d);
}

// Clean up expired pending links
function cleanupExpiredPendingLinks() {
  const result = db.prepare(`
    DELETE FROM pending_links
    WHERE datetime(created_at) < datetime('now', '-' || ? || ' minutes')
  `).run(config.PENDING_LINK_EXPIRY_MINUTES);

  if (result.changes > 0) {
    logger.info(`Cleaned up ${result.changes} expired pending links`);
  }
}

// Run cleanup on startup
cleanupExpiredPendingLinks();

// Periodic cleanup every 10 minutes
const cleanupInterval = setInterval(cleanupExpiredPendingLinks, 10 * 60 * 1000);
cleanupInterval.unref();

// Clean up old qurl_sends and qurl_send_configs (older than 30 days)
function cleanupOldSends() {
  const sends = db.prepare(`
    DELETE FROM qurl_sends
    WHERE datetime(created_at) < datetime('now', '-30 days')
  `).run();

  const configs = db.prepare(`
    DELETE FROM qurl_send_configs
    WHERE datetime(created_at) < datetime('now', '-30 days')
  `).run();

  const total = sends.changes + configs.changes;
  if (total > 0) {
    logger.info(`Cleaned up ${sends.changes} qurl_sends + ${configs.changes} qurl_send_configs rows`);
  }
}

cleanupOldSends();
const sendsCleanupInterval = setInterval(cleanupOldSends, 24 * 60 * 60 * 1000);
sendsCleanupInterval.unref();

module.exports = {
  BADGE_TYPES,
  BADGE_INFO,

  // Pending links (for OAuth state)
  createPendingLink(state, discordId) {
    const stmt = db.prepare('INSERT OR REPLACE INTO pending_links (state, discord_id) VALUES (?, ?)');
    stmt.run(state, discordId);
    logger.debug('Created pending link', { discordId, state: state.substring(0, 8) + '...' });
  },

  getPendingLink(state) {
    const stmt = db.prepare('SELECT discord_id FROM pending_links WHERE state = ?');
    return stmt.get(state);
  },

  deletePendingLink(state) {
    const stmt = db.prepare('DELETE FROM pending_links WHERE state = ?');
    stmt.run(state);
  },

  // GitHub links
  createLink(discordId, githubUsername) {
    const stmt = db.prepare(`
      INSERT INTO github_links (discord_id, github_username, updated_at)
      VALUES (?, ?, CURRENT_TIMESTAMP)
      ON CONFLICT(discord_id) DO UPDATE SET
        github_username = excluded.github_username,
        updated_at = CURRENT_TIMESTAMP
    `);
    stmt.run(discordId, githubUsername.toLowerCase());
    logger.info('Created/updated GitHub link', { discordId, github: githubUsername });
  },

  getLinkByDiscord(discordId) {
    const stmt = db.prepare('SELECT * FROM github_links WHERE discord_id = ?');
    return stmt.get(discordId);
  },

  getLinkByGithub(githubUsername) {
    const stmt = db.prepare('SELECT * FROM github_links WHERE github_username = ?');
    return stmt.get(githubUsername.toLowerCase());
  },

  deleteLink(discordId) {
    const stmt = db.prepare('DELETE FROM github_links WHERE discord_id = ?');
    const result = stmt.run(discordId);
    logger.info('Deleted GitHub link', { discordId, deleted: result.changes > 0 });
    return result;
  },

  // Admin: force link a user
  forceLink(discordId, githubUsername) {
    return this.createLink(discordId, githubUsername);
  },

  // Contributions
  recordContribution(discordId, githubUsername, prNumber, repo, prTitle = null) {
    try {
      const stmt = db.prepare(
        'INSERT OR IGNORE INTO contributions (discord_id, github_username, pr_number, repo, pr_title) VALUES (?, ?, ?, ?, ?)'
      );
      const result = stmt.run(discordId, githubUsername.toLowerCase(), prNumber, repo, prTitle);

      if (result.changes > 0) {
        logger.info('Recorded contribution', { discordId, github: githubUsername, pr: prNumber, repo });
        // Update streak only for new contributions
        this.updateStreak(discordId);
        return true;
      } else {
        logger.debug('Contribution already exists', { pr: prNumber, repo });
        return false;
      }
    } catch (error) {
      logger.error('Failed to record contribution', { error: error.message, pr: prNumber, repo });
      return false;
    }
  },

  getContributions(discordId, limit = 50) {
    const stmt = db.prepare('SELECT * FROM contributions WHERE discord_id = ? ORDER BY merged_at DESC LIMIT ?');
    return stmt.all(discordId, limit);
  },

  getAllContributions(limit = 100) {
    const stmt = db.prepare('SELECT * FROM contributions ORDER BY merged_at DESC LIMIT ?');
    return stmt.all(limit);
  },

  getContributionCount(discordId) {
    const stmt = db.prepare('SELECT COUNT(*) as count FROM contributions WHERE discord_id = ?');
    return stmt.get(discordId).count;
  },

  // Get contributions for the current week (legacy)
  getWeeklyContributions(discordId) {
    const stmt = db.prepare(`
      SELECT * FROM contributions
      WHERE discord_id = ?
      AND date(merged_at) >= date('now', 'weekday 0', '-7 days')
      ORDER BY merged_at DESC
    `);
    return stmt.all(discordId);
  },

  // Get contributions for the current month
  getMonthlyContributions(discordId) {
    const stmt = db.prepare(`
      SELECT * FROM contributions
      WHERE discord_id = ?
      AND strftime('%Y-%m', merged_at) = strftime('%Y-%m', 'now')
      ORDER BY merged_at DESC
    `);
    return stmt.all(discordId);
  },

  // Get unique repos a user has contributed to
  getUniqueRepos(discordId) {
    const stmt = db.prepare(`
      SELECT DISTINCT repo FROM contributions WHERE discord_id = ?
    `);
    return stmt.all(discordId).map(r => r.repo);
  },

  // Get all contributions from the past week (for digest)
  getLastWeekContributions() {
    const stmt = db.prepare(`
      SELECT * FROM contributions
      WHERE date(merged_at) >= date('now', '-7 days')
      ORDER BY merged_at DESC
    `);
    return stmt.all();
  },

  // Get new contributors from the past week
  getNewContributorsThisWeek() {
    const stmt = db.prepare(`
      SELECT DISTINCT c.discord_id, c.github_username, MIN(c.merged_at) as first_contribution
      FROM contributions c
      WHERE c.discord_id IN (
        SELECT discord_id FROM contributions
        GROUP BY discord_id
        HAVING MIN(merged_at) >= date('now', '-7 days')
      )
      GROUP BY c.discord_id
    `);
    return stmt.all();
  },

  // Stats
  getStats() {
    const links = db.prepare('SELECT COUNT(*) as count FROM github_links').get();
    const contributions = db.prepare('SELECT COUNT(*) as count FROM contributions').get();
    const uniqueContributors = db.prepare('SELECT COUNT(DISTINCT discord_id) as count FROM contributions').get();
    const repoStats = db.prepare(`
      SELECT repo, COUNT(*) as count
      FROM contributions
      GROUP BY repo
      ORDER BY count DESC
    `).all();

    return {
      linkedUsers: links.count,
      totalContributions: contributions.count,
      uniqueContributors: uniqueContributors.count,
      byRepo: repoStats,
    };
  },

  // Leaderboard
  getTopContributors(limit = 10) {
    const stmt = db.prepare(`
      SELECT discord_id, COUNT(*) as count
      FROM contributions
      GROUP BY discord_id
      ORDER BY count DESC
      LIMIT ?
    `);
    return stmt.all(limit);
  },

  // === BADGES ===

  awardBadge(discordId, badgeType) {
    try {
      const stmt = db.prepare('INSERT OR IGNORE INTO badges (discord_id, badge_type) VALUES (?, ?)');
      const result = stmt.run(discordId, badgeType);
      if (result.changes > 0) {
        logger.info('Badge awarded', { discordId, badge: badgeType });
        return true;
      }
      return false; // Already had badge
    } catch (error) {
      logger.error('Error awarding badge', { error: error.message });
      return false;
    }
  },

  getBadges(discordId) {
    const stmt = db.prepare('SELECT badge_type, earned_at FROM badges WHERE discord_id = ? ORDER BY earned_at');
    return stmt.all(discordId);
  },

  hasBadge(discordId, badgeType) {
    const stmt = db.prepare('SELECT 1 FROM badges WHERE discord_id = ? AND badge_type = ?');
    return !!stmt.get(discordId, badgeType);
  },

  // Check and award badges based on contribution
  checkAndAwardBadges(discordId, prTitle, repo) {
    const awarded = [];
    const count = this.getContributionCount(discordId);

    // First PR badge
    if (count === 1) {
      if (this.awardBadge(discordId, BADGE_TYPES.FIRST_PR)) {
        awarded.push(BADGE_TYPES.FIRST_PR);
      }
    }

    // Docs Hero - check if PR title or repo suggests docs
    const isDocsPR = /doc|readme|guide|tutorial|example/i.test(prTitle) ||
                     /doc|example|demo/i.test(repo);
    if (isDocsPR && !this.hasBadge(discordId, BADGE_TYPES.DOCS_HERO)) {
      if (this.awardBadge(discordId, BADGE_TYPES.DOCS_HERO)) {
        awarded.push(BADGE_TYPES.DOCS_HERO);
      }
    }

    // Bug Hunter - check if PR title suggests bug fix
    const isBugFix = /fix|bug|issue|patch|resolve/i.test(prTitle);
    if (isBugFix && !this.hasBadge(discordId, BADGE_TYPES.BUG_HUNTER)) {
      if (this.awardBadge(discordId, BADGE_TYPES.BUG_HUNTER)) {
        awarded.push(BADGE_TYPES.BUG_HUNTER);
      }
    }

    // On Fire - 2+ PRs this month (adjusted for realistic contribution cadence)
    const monthlyPRs = this.getMonthlyContributions(discordId);
    if (monthlyPRs.length >= 2 && !this.hasBadge(discordId, BADGE_TYPES.ON_FIRE)) {
      if (this.awardBadge(discordId, BADGE_TYPES.ON_FIRE)) {
        awarded.push(BADGE_TYPES.ON_FIRE);
      }
    }

    // Streak Master - 3 consecutive months (adjusted from 4 weeks)
    const streak = this.getStreak(discordId);
    if (streak && streak.current_streak >= 3 && !this.hasBadge(discordId, BADGE_TYPES.STREAK_MASTER)) {
      if (this.awardBadge(discordId, BADGE_TYPES.STREAK_MASTER)) {
        awarded.push(BADGE_TYPES.STREAK_MASTER);
      }
    }

    // Multi-Repo - contributed to 2+ different repos
    const uniqueRepos = this.getUniqueRepos(discordId);
    if (uniqueRepos.length >= 2 && !this.hasBadge(discordId, BADGE_TYPES.MULTI_REPO)) {
      if (this.awardBadge(discordId, BADGE_TYPES.MULTI_REPO)) {
        awarded.push(BADGE_TYPES.MULTI_REPO);
      }
    }

    return awarded;
  },

  // Award badge for opening an issue (called from webhook handler)
  awardFirstIssueBadge(discordId) {
    if (!this.hasBadge(discordId, BADGE_TYPES.FIRST_ISSUE)) {
      if (this.awardBadge(discordId, BADGE_TYPES.FIRST_ISSUE)) {
        return [BADGE_TYPES.FIRST_ISSUE];
      }
    }
    return [];
  },

  // === STREAKS ===

  getStreak(discordId) {
    const stmt = db.prepare('SELECT * FROM streaks WHERE discord_id = ?');
    return stmt.get(discordId);
  },

  updateStreak(discordId) {
    // Using monthly streaks for realistic open source contribution cadence
    const currentMonth = getMonthString();
    const existing = this.getStreak(discordId);

    if (!existing) {
      // First contribution ever
      db.prepare(`
        INSERT INTO streaks (discord_id, current_streak, longest_streak, last_contribution_week)
        VALUES (?, 1, 1, ?)
      `).run(discordId, currentMonth);
      return { current: 1, longest: 1, isNew: true };
    }

    if (existing.last_contribution_week === currentMonth) {
      // Already contributed this month
      return { current: existing.current_streak, longest: existing.longest_streak, isNew: false };
    }

    // Check if this is consecutive (last month)
    const lastMonth = getPreviousMonthString();
    let newStreak;

    if (existing.last_contribution_week === lastMonth) {
      // Consecutive month - extend streak
      newStreak = existing.current_streak + 1;
    } else {
      // Streak broken - reset to 1
      newStreak = 1;
    }

    const newLongest = Math.max(newStreak, existing.longest_streak);

    db.prepare(`
      UPDATE streaks
      SET current_streak = ?, longest_streak = ?, last_contribution_week = ?, updated_at = CURRENT_TIMESTAMP
      WHERE discord_id = ?
    `).run(newStreak, newLongest, currentMonth, discordId);

    return { current: newStreak, longest: newLongest, isNew: newStreak > existing.current_streak };
  },

  // === MILESTONES ===

  hasMilestoneBeenAnnounced(type, value, repo = null) {
    const stmt = db.prepare('SELECT 1 FROM milestones WHERE milestone_type = ? AND milestone_value = ? AND (repo = ? OR (repo IS NULL AND ? IS NULL))');
    return !!stmt.get(type, value, repo, repo);
  },

  recordMilestone(type, value, repo = null) {
    try {
      const stmt = db.prepare('INSERT OR IGNORE INTO milestones (milestone_type, milestone_value, repo) VALUES (?, ?, ?)');
      return stmt.run(type, value, repo).changes > 0;
    } catch {
      return false;
    }
  },

  // === WEEKLY DIGEST ===

  getWeeklyDigestData() {
    const lastWeekPRs = this.getLastWeekContributions();
    const newContributors = this.getNewContributorsThisWeek();

    // Group PRs by repo
    const byRepo = {};
    for (const pr of lastWeekPRs) {
      if (!byRepo[pr.repo]) byRepo[pr.repo] = [];
      byRepo[pr.repo].push(pr);
    }

    // Get unique contributors this week
    const uniqueContributors = [...new Set(lastWeekPRs.map(pr => pr.discord_id))];

    return {
      totalPRs: lastWeekPRs.length,
      uniqueContributors: uniqueContributors.length,
      newContributors: newContributors,
      byRepo,
      prs: lastWeekPRs.slice(0, 10), // Top 10 for display
    };
  },

  // --- QURL SENDS ---

  recordQURLSend({ sendId, senderDiscordId, recipientDiscordId, resourceId, resourceType, qurlLink, expiresIn, channelId, targetType }) {
    const stmt = db.prepare(`
      INSERT INTO qurl_sends (send_id, sender_discord_id, recipient_discord_id, resource_id, resource_type, qurl_link, expires_in, channel_id, target_type)
      VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
    `);
    stmt.run(sendId, senderDiscordId, recipientDiscordId, resourceId, resourceType, qurlLink, expiresIn, channelId, targetType);
  },

  recordQURLSendBatch(sends) {
    const stmt = db.prepare(`
      INSERT INTO qurl_sends (send_id, sender_discord_id, recipient_discord_id, resource_id, resource_type, qurl_link, expires_in, channel_id, target_type)
      VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
    `);
    const insertMany = db.transaction((items) => {
      for (const s of items) {
        stmt.run(s.sendId, s.senderDiscordId, s.recipientDiscordId, s.resourceId, s.resourceType, s.qurlLink, s.expiresIn, s.channelId, s.targetType);
      }
    });
    insertMany(sends);
  },

  updateSendDMStatus(sendId, recipientDiscordId, status) {
    const stmt = db.prepare('UPDATE qurl_sends SET dm_status = ? WHERE send_id = ? AND recipient_discord_id = ?');
    stmt.run(status, sendId, recipientDiscordId);
  },

  getRecentSends(senderDiscordId, limit = 10) {
    const stmt = db.prepare(`
      SELECT send_id, resource_type, target_type, channel_id, expires_in, created_at,
             COUNT(*) as recipient_count,
             -- DM_STATUS.SENT is a compile-time constant ('sent'), safe for SQL interpolation
             SUM(CASE WHEN dm_status = '${DM_STATUS.SENT}' THEN 1 ELSE 0 END) as delivered_count
      FROM qurl_sends
      WHERE sender_discord_id = ?
      GROUP BY send_id
      ORDER BY created_at DESC
      LIMIT ?
    `);
    return stmt.all(senderDiscordId, limit);
  },

  saveSendConfig({ sendId, senderDiscordId, resourceType, connectorResourceId, actualUrl, expiresIn, personalMessage, locationName, attachmentName }) {
    const stmt = db.prepare(`
      INSERT OR REPLACE INTO qurl_send_configs (send_id, sender_discord_id, resource_type, connector_resource_id, actual_url, expires_in, personal_message, location_name, attachment_name)
      VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
    `);
    stmt.run(sendId, senderDiscordId, resourceType, connectorResourceId, actualUrl, expiresIn, personalMessage, locationName, attachmentName);
  },

  getSendConfig(sendId, senderDiscordId) {
    const stmt = db.prepare('SELECT * FROM qurl_send_configs WHERE send_id = ? AND sender_discord_id = ?');
    return stmt.get(sendId, senderDiscordId);
  },

  getSendResourceIds(sendId, senderDiscordId) {
    const stmt = db.prepare('SELECT DISTINCT resource_id FROM qurl_sends WHERE send_id = ? AND sender_discord_id = ?');
    return stmt.all(sendId, senderDiscordId).map(r => r.resource_id);
  },

  // Close database (for graceful shutdown)
  close() {
    clearInterval(cleanupInterval);
    clearInterval(sendsCleanupInterval);
    db.close();
    logger.info('Database closed');
  },
};
