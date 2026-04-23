const Database = require('better-sqlite3');
const { encrypt, decrypt } = require('./utils/crypto');
const path = require('path');
const fs = require('fs');
const config = require('./config');
const logger = require('./logger');

// Ensure data directory exists
const dbDir = path.dirname(config.DATABASE_PATH);
if (!fs.existsSync(dbDir)) {
  fs.mkdirSync(dbDir, { recursive: true });
}

const db = new Database(config.DATABASE_PATH);
db.pragma('journal_mode = WAL');
// Retry for up to 5s on writer contention (Express + Discord events can
// race) before surfacing SQLITE_BUSY to the caller.
db.pragma('busy_timeout = 5000');

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
  -- NOTE: last_contribution_week despite the name stores a MONTH string
  -- in YYYY-MM format (see getMonthString). The column was carried over
  -- from an earlier weekly-streak design; renaming requires a data-rewrite
  -- migration. Treat it as "last_contribution_month" in code.
  CREATE TABLE IF NOT EXISTS streaks (
    discord_id TEXT PRIMARY KEY,
    current_streak INTEGER DEFAULT 0,
    longest_streak INTEGER DEFAULT 0,
    last_contribution_week TEXT,  -- actually YYYY-MM
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
  CREATE INDEX IF NOT EXISTS idx_pending_links_created ON pending_links(created_at);
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
    attachment_content_type TEXT,
    attachment_url TEXT,
    created_at TEXT DEFAULT CURRENT_TIMESTAMP
  );

  -- Tokens that failed the OAuth revoke step; background sweep attempts to
  -- retire these. Store the token + timestamp so oncall can alert on a
  -- rising count (= GitHub revoke API is unhappy or we have a real leak).
  CREATE TABLE IF NOT EXISTS orphaned_oauth_tokens (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    access_token TEXT NOT NULL,
    recorded_at TEXT DEFAULT CURRENT_TIMESTAMP
  );

  CREATE TABLE IF NOT EXISTS guild_configs (
    guild_id TEXT PRIMARY KEY,
    qurl_api_key TEXT NOT NULL,
    configured_by TEXT NOT NULL,
    configured_at TEXT DEFAULT CURRENT_TIMESTAMP,
    updated_at TEXT DEFAULT CURRENT_TIMESTAMP
  );
`);

// Add attachment-reupload columns to an existing qurl_send_configs table.
// CREATE TABLE IF NOT EXISTS does not alter existing schemas; these are
// idempotent ALTER TABLE ADD COLUMN calls that let handleAddRecipients
// re-download the file via Discord CDN on follow-up sends.
//
// SAFE_ALTERS is a static map from column name → complete ALTER TABLE
// statement. No template interpolation, no regex validation — a future
// contributor who needs to add a column writes the full statement literally,
// so SQL-injection risk from this block is structurally zero.
const SAFE_ALTERS = {
  attachment_content_type: 'ALTER TABLE qurl_send_configs ADD COLUMN attachment_content_type TEXT',
  attachment_url: 'ALTER TABLE qurl_send_configs ADD COLUMN attachment_url TEXT',
  // Timestamp of when the sender hit /qurl revoke on this send. NULL ⇒ still
  // revocable. Used to hide already-revoked sends from the /qurl revoke
  // dropdown so users don't re-select a no-op and see "0/0 links revoked".
  revoked_at: 'ALTER TABLE qurl_send_configs ADD COLUMN revoked_at TEXT',
};
for (const [col, sql] of Object.entries(SAFE_ALTERS)) {
  try {
    db.exec(sql);
  } catch (err) {
    // "duplicate column name" is expected on second run; everything else bubbles up.
    if (!String(err.message).includes('duplicate column')) {
      logger.error('SAFE_ALTERS exec failed', { column: col, error: err.message });
      throw err;
    }
  }
}

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

// Get month string (YYYY-MM format) in UTC - used for monthly streak tracking.
// Using UTC instead of local time guarantees contributions are bucketed by the
// same month boundary regardless of container timezone (ECS images default to
// UTC today but that's not contractually guaranteed).
function getMonthString(date = new Date()) {
  const d = new Date(date);
  return `${d.getUTCFullYear()}-${String(d.getUTCMonth() + 1).padStart(2, '0')}`;
}

// Get previous month string in UTC. Anchor the date to day 1 (UTC) before
// decrementing so March 31 rolls back to Feb 1 (the month we want) rather
// than Mar 3 (which is what setMonth does when the target month has fewer days).
function getPreviousMonthString(date = new Date()) {
  const d = new Date(date);
  d.setUTCDate(1);
  d.setUTCMonth(d.getUTCMonth() - 1);
  return getMonthString(d);
}

// Clean up expired pending links.
//
// PENDING_LINK_EXPIRY_MINUTES is validated as a positive integer at boot
// (see config.intEnv + index.js), but defense-in-depth: pre-format the
// modifier string in JS and bind the whole literal as a parameter. That
// way the '||' SQL concatenation pattern is eliminated — the bound value
// is now the complete datetime modifier, so a future change to the env
// validation can't open a SQL-injection path into this table.
function cleanupExpiredPendingLinks() {
  const mins = Math.trunc(Number(config.PENDING_LINK_EXPIRY_MINUTES));
  if (!Number.isInteger(mins) || mins <= 0) {
    throw new Error(`cleanupExpiredPendingLinks: invalid PENDING_LINK_EXPIRY_MINUTES: ${config.PENDING_LINK_EXPIRY_MINUTES}`);
  }
  const modifier = `-${mins} minutes`;
  const result = db.prepare(`
    DELETE FROM pending_links
    WHERE datetime(created_at) < datetime('now', ?)
  `).run(modifier);

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

// Purge orphaned OAuth tokens older than ORPHAN_TOKEN_RETENTION_DAYS (default
// 7 in production, 1 in dev/staging to narrow the plaintext exposure window
// — though they're encrypted at rest, shorter retention is still preferable).
// The tokens have `read:user` scope and GitHub expires them on its own TTL,
// so after this window they're effectively dead; the sweeper keeps retrying
// live revocation up to the cutoff.
const ORPHAN_RETENTION_DAYS = (() => {
  const fromEnv = parseInt(process.env.ORPHAN_TOKEN_RETENTION_DAYS, 10);
  if (Number.isInteger(fromEnv) && fromEnv > 0 && fromEnv <= 30) return fromEnv;
  return process.env.NODE_ENV === 'production' ? 7 : 1;
})();
function cleanupOrphanedTokens() {
  const modifier = `-${ORPHAN_RETENTION_DAYS} days`;
  const result = db.prepare(`
    DELETE FROM orphaned_oauth_tokens
    WHERE datetime(recorded_at) < datetime('now', ?)
  `).run(modifier);
  if (result.changes > 0) {
    logger.info(`Cleaned up ${result.changes} orphaned oauth tokens (>${ORPHAN_RETENTION_DAYS}d old)`);
  }
}
cleanupOrphanedTokens();
const orphanedTokenCleanupInterval = setInterval(cleanupOrphanedTokens, 24 * 60 * 60 * 1000);
orphanedTokenCleanupInterval.unref();

const dbModule = {
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

  // Atomic consume: returns the pending_links row and deletes it in one SQL
  // statement. Closes the TOCTOU window in the OAuth callback where a
  // concurrent request with the same state could pass a separate check-then-
  // delete pair.
  consumePendingLink(state) {
    const stmt = db.prepare('DELETE FROM pending_links WHERE state = ? RETURNING discord_id');
    return stmt.get(state);
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

  getLinkedDiscordIds() {
    const stmt = db.prepare('SELECT discord_id FROM github_links');
    return new Set(stmt.all().map(r => r.discord_id));
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
    return dbModule.createLink(discordId, githubUsername);
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
        dbModule.updateStreak(discordId);
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
    const count = dbModule.getContributionCount(discordId);

    // First PR badge
    if (count === 1) {
      if (dbModule.awardBadge(discordId, BADGE_TYPES.FIRST_PR)) {
        awarded.push(BADGE_TYPES.FIRST_PR);
      }
    }

    // Docs Hero - check if PR title or repo suggests docs
    const isDocsPR = /doc|readme|guide|tutorial|example/i.test(prTitle) ||
                     /doc|example|demo/i.test(repo);
    if (isDocsPR && !dbModule.hasBadge(discordId, BADGE_TYPES.DOCS_HERO)) {
      if (dbModule.awardBadge(discordId, BADGE_TYPES.DOCS_HERO)) {
        awarded.push(BADGE_TYPES.DOCS_HERO);
      }
    }

    // Bug Hunter - check if PR title suggests bug fix
    const isBugFix = /fix|bug|issue|patch|resolve/i.test(prTitle);
    if (isBugFix && !dbModule.hasBadge(discordId, BADGE_TYPES.BUG_HUNTER)) {
      if (dbModule.awardBadge(discordId, BADGE_TYPES.BUG_HUNTER)) {
        awarded.push(BADGE_TYPES.BUG_HUNTER);
      }
    }

    // On Fire - 2+ PRs this month (adjusted for realistic contribution cadence)
    const monthlyPRs = dbModule.getMonthlyContributions(discordId);
    if (monthlyPRs.length >= 2 && !dbModule.hasBadge(discordId, BADGE_TYPES.ON_FIRE)) {
      if (dbModule.awardBadge(discordId, BADGE_TYPES.ON_FIRE)) {
        awarded.push(BADGE_TYPES.ON_FIRE);
      }
    }

    // Streak Master - 3 consecutive months (adjusted from 4 weeks)
    const streak = dbModule.getStreak(discordId);
    if (streak && streak.current_streak >= 3 && !dbModule.hasBadge(discordId, BADGE_TYPES.STREAK_MASTER)) {
      if (dbModule.awardBadge(discordId, BADGE_TYPES.STREAK_MASTER)) {
        awarded.push(BADGE_TYPES.STREAK_MASTER);
      }
    }

    // Multi-Repo - contributed to 2+ different repos
    const uniqueRepos = dbModule.getUniqueRepos(discordId);
    if (uniqueRepos.length >= 2 && !dbModule.hasBadge(discordId, BADGE_TYPES.MULTI_REPO)) {
      if (dbModule.awardBadge(discordId, BADGE_TYPES.MULTI_REPO)) {
        awarded.push(BADGE_TYPES.MULTI_REPO);
      }
    }

    return awarded;
  },

  // Award badge for opening an issue (called from webhook handler)
  awardFirstIssueBadge(discordId) {
    if (!dbModule.hasBadge(discordId, BADGE_TYPES.FIRST_ISSUE)) {
      if (dbModule.awardBadge(discordId, BADGE_TYPES.FIRST_ISSUE)) {
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
    const existing = dbModule.getStreak(discordId);

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
    } catch (err) {
      // Log so a disk-full / corruption failure is distinguishable from the
      // INSERT-OR-IGNORE duplicate case (which returns false by design).
      logger.error('recordMilestone failed', { type, value, repo, error: err.message });
      return false;
    }
  },

  // === WEEKLY DIGEST ===

  getWeeklyDigestData() {
    const lastWeekPRs = dbModule.getLastWeekContributions();
    const newContributors = dbModule.getNewContributorsThisWeek();

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
    // LEFT JOIN on qurl_send_configs so we can:
    // 1. Filter out already-revoked sends. With LEFT JOIN, rows that
    //    have no config match (legacy sends predating qurl_send_configs)
    //    get c.revoked_at = NULL and are preserved — so the single
    //    `c.revoked_at IS NULL` check covers BOTH "unrevoked" AND "no
    //    config row at all". markSendRevoked below backfills a minimal
    //    config row when called against a legacy send, so the filter
    //    stays honest across both cases.
    // 2. Surface filename / location / message snippet in the /qurl revoke
    //    dropdown so users can identify WHICH send they're about to revoke.
    const stmt = db.prepare(`
      SELECT s.send_id, s.resource_type, s.target_type, s.channel_id, s.expires_in, s.created_at,
             COUNT(*) as recipient_count,
             SUM(CASE WHEN s.dm_status = 'sent' THEN 1 ELSE 0 END) as delivered_count,
             c.attachment_name, c.location_name, c.personal_message
      FROM qurl_sends s
      LEFT JOIN qurl_send_configs c ON c.send_id = s.send_id
      WHERE s.sender_discord_id = ?
        AND c.revoked_at IS NULL
      GROUP BY s.send_id
      ORDER BY s.created_at DESC
      LIMIT ?
    `);
    return stmt.all(senderDiscordId, limit);
  },

  markSendRevoked(sendId, senderDiscordId) {
    // Scoped by senderDiscordId so one user can't mark another user's
    // send as revoked — defense-in-depth; the revoke handler is already
    // gated on ownership upstream.
    //
    // Two cases:
    // (a) Normal path: a qurl_send_configs row exists for this send.
    //     UPDATE flips revoked_at; a second revoke is a no-op because
    //     of the `revoked_at IS NULL` guard.
    // (b) Legacy path: the send predates qurl_send_configs being
    //     populated, so no config row exists. Without this fallback,
    //     markSendRevoked silently updates zero rows and the send
    //     reappears in the /qurl revoke dropdown on the next invocation
    //     — the exact failure mode this fix targets. Insert a minimal
    //     config row (NOT NULL columns: send_id, sender_discord_id,
    //     resource_type, expires_in) pulled from the qurl_sends row,
    //     with only the revocation marker populated.
    const updateStmt = db.prepare(`
      UPDATE qurl_send_configs
      SET revoked_at = CURRENT_TIMESTAMP
      WHERE send_id = ? AND sender_discord_id = ? AND revoked_at IS NULL
    `);
    const result = updateStmt.run(sendId, senderDiscordId);
    if (result.changes > 0) return;

    const legacyStmt = db.prepare(`
      SELECT resource_type, expires_in FROM qurl_sends
      WHERE send_id = ? AND sender_discord_id = ? LIMIT 1
    `);
    const legacy = legacyStmt.get(sendId, senderDiscordId);
    if (!legacy) return; // nothing to mark — caller targeted a non-existent send

    const insertStmt = db.prepare(`
      INSERT OR IGNORE INTO qurl_send_configs
        (send_id, sender_discord_id, resource_type, expires_in, revoked_at)
      VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)
    `);
    insertStmt.run(sendId, senderDiscordId, legacy.resource_type, legacy.expires_in || '24h');
  },

  saveSendConfig({ sendId, senderDiscordId, resourceType, connectorResourceId, actualUrl, expiresIn, personalMessage, locationName, attachmentName, attachmentContentType, attachmentUrl }) {
    const stmt = db.prepare(`
      INSERT OR REPLACE INTO qurl_send_configs (send_id, sender_discord_id, resource_type, connector_resource_id, actual_url, expires_in, personal_message, location_name, attachment_name, attachment_content_type, attachment_url)
      VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
    `);
    // Encrypt the Discord CDN URL at rest for consistency with the other
    // secrets in this table. Discord signs these URLs with a short TTL, but
    // an EFS leak inside the window could still be used to re-fetch the file.
    const encryptedUrl = attachmentUrl ? encrypt(attachmentUrl) : null;
    stmt.run(sendId, senderDiscordId, resourceType, connectorResourceId, actualUrl, expiresIn, personalMessage, locationName, attachmentName, attachmentContentType ?? null, encryptedUrl);
  },

  getSendConfig(sendId, senderDiscordId) {
    const stmt = db.prepare('SELECT * FROM qurl_send_configs WHERE send_id = ? AND sender_discord_id = ?');
    const row = stmt.get(sendId, senderDiscordId);
    if (!row) return row;
    // Return a copy with the attachment URL decrypted. Legacy plaintext rows
    // pass through untouched via utils/crypto.decrypt.
    return row.attachment_url
      ? { ...row, attachment_url: decrypt(row.attachment_url) }
      : row;
  },

  getSendResourceIds(sendId, senderDiscordId) {
    const stmt = db.prepare('SELECT DISTINCT resource_id FROM qurl_sends WHERE send_id = ? AND sender_discord_id = ?');
    return stmt.all(sendId, senderDiscordId).map(r => r.resource_id);
  },

  // ── Guild config (BYOK API keys) ──

  // Encrypted at rest via KEY_ENCRYPTION_KEY (AES-256-GCM). Rows written
  // before encryption was enabled are returned as plaintext and re-encrypted
  // on next write (utils/crypto.decrypt passes non-prefixed values through).
  getGuildApiKey(guildId) {
    const stmt = db.prepare('SELECT qurl_api_key FROM guild_configs WHERE guild_id = ?');
    const row = stmt.get(guildId);
    return row ? decrypt(row.qurl_api_key) : null;
  },

  setGuildApiKey(guildId, apiKey, configuredBy) {
    const stmt = db.prepare(`
      INSERT INTO guild_configs (guild_id, qurl_api_key, configured_by, updated_at)
      VALUES (?, ?, ?, CURRENT_TIMESTAMP)
      ON CONFLICT(guild_id) DO UPDATE SET
        qurl_api_key = excluded.qurl_api_key,
        configured_by = excluded.configured_by,
        updated_at = CURRENT_TIMESTAMP
    `);
    stmt.run(guildId, encrypt(apiKey), configuredBy);
  },

  recordOrphanedToken(accessToken) {
    const stmt = db.prepare('INSERT INTO orphaned_oauth_tokens (access_token) VALUES (?)');
    stmt.run(encrypt(accessToken));
  },

  countOrphanedTokens() {
    const row = db.prepare('SELECT COUNT(*) AS c FROM orphaned_oauth_tokens').get();
    return row ? row.c : 0;
  },

  // Return up to `limit` orphaned rows (oldest first). Access tokens are
  // returned as encrypted blobs so the caller can decrypt one at a time and
  // keep only a single plaintext in memory at once — narrows the window for
  // memory-dump exposure vs. decrypting the whole batch up front.
  listOrphanedTokens(limit = 50) {
    const stmt = db.prepare('SELECT id, access_token FROM orphaned_oauth_tokens ORDER BY recorded_at ASC LIMIT ?');
    return stmt.all(limit).map(r => ({ id: r.id, encryptedAccessToken: r.access_token }));
  },

  // Callers hold one plaintext token only for the duration of a single
  // revoke call, then let it drop out of scope.
  decryptOrphanedToken(encryptedAccessToken) {
    return decrypt(encryptedAccessToken);
  },

  deleteOrphanedToken(id) {
    db.prepare('DELETE FROM orphaned_oauth_tokens WHERE id = ?').run(id);
  },

  removeGuildApiKey(guildId) {
    const stmt = db.prepare('DELETE FROM guild_configs WHERE guild_id = ?');
    return stmt.run(guildId).changes > 0;
  },

  getGuildConfig(guildId) {
    const stmt = db.prepare('SELECT * FROM guild_configs WHERE guild_id = ?');
    const row = stmt.get(guildId);
    if (!row) return row;
    // STRIP the decrypted key from the returned object so any caller that
    // logs the row (debug dumps, accidental JSON.stringify) can't leak the
    // plaintext tenant API key. Callers that need the key MUST use the
    // getGuildApiKey accessor below, which is explicit about returning
    // secret material and is never passed to a logger.
    const { qurl_api_key, ...safe } = row;
    void qurl_api_key; // explicit discard so lint/readers know it's dropped
    return safe;
  },

  // Returns the decrypted guild key. ONLY for use at the last-mile QURL
  // API call — never log or render this value. Callers that need to show
  // the admin that a key is configured should use getGuildConfig + compute
  // a sha256 fingerprint (see /qurl status in commands.js).
  getGuildConfigWithApiKey(guildId) {
    const stmt = db.prepare('SELECT * FROM guild_configs WHERE guild_id = ?');
    const row = stmt.get(guildId);
    if (!row) return row;
    return { ...row, qurl_api_key: row.qurl_api_key ? decrypt(row.qurl_api_key) : row.qurl_api_key };
  },

  // Close database (for graceful shutdown)
  close() {
    clearInterval(cleanupInterval);
    clearInterval(sendsCleanupInterval);
    clearInterval(orphanedTokenCleanupInterval);
    db.close();
    logger.info('Database closed');
  },
};

module.exports = dbModule;
