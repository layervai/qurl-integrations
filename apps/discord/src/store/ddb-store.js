// store/ddb-store — DynamoDB backend
//
// Implements the Store contract (see `src/store/contract.js`) against
// the 11 DynamoDB tables provisioned by `modules/qurl-bot-ddb/`
// in the infra repo. Selected at boot when `STORE_TYPE=ddb`.
//
// Table naming: tables carry the env in their name
// (`qurl-bot-discord-<env>-<kebab-name>`). The env prefix is
// supplied via `DDB_TABLE_PREFIX` (default `qurl-bot-discord-prod-`).
// The bot task role's IAM policy must allow data-plane verbs on the
// specific table ARNs + GSI ARNs — scoping happens infra-side, not
// here.
//
// Encryption parity with SqliteStore: the three sensitive fields
// (`guild_configs.qurl_api_key`, `orphaned_oauth_tokens.access_token`,
// `qurl_send_configs.attachment_url`) are envelope-encrypted at the
// app layer via `utils/crypto.encrypt`. Ciphertext is stored as a
// regular `S` string. DDB server-side encryption (AWS-managed
// aws/dynamodb CMK, configured at the table level) is defense-in-
// depth — the primary encryption is the app-layer envelope.
//
// Composite key encoding — two tables flatten a SQLite UNIQUE into
// a string hash key to preserve dedup semantics under
// `ConditionExpression: attribute_not_exists(pk)`:
//
//   - `contributions.contribution_id = "<repo>#<pr_number>"`
//     Uniqueness invariant: neither component contains `#`
//     (GitHub owner/name slug disallows it; PR numbers are
//     integers).
//
//   - `milestones.milestone_id = "<repo-or-sentinel>#<type>#<value>"`
//     Uniqueness invariant: real repo values always contain `/`
//     (owner/name GitHub slug) so no real repo can equal the
//     `__NONE__` sentinel used for account-wide milestones.
//
// Cross-process streak + badge writes: `recordContribution` chains
// into `updateStreak` and badge-award helpers just like the SQLite
// impl. DDB doesn't support atomic read-modify-write on different
// items, so the streak update is a best-effort follow-up — a
// concurrent second recordContribution call for the same user could
// race the streak counter. Acceptable at projected write volume;
// revisit with `TransactWriteItems` if contention ever surfaces.

const crypto = require('crypto');
const {
  DynamoDBClient,
} = require('@aws-sdk/client-dynamodb');
const {
  DynamoDBDocumentClient,
  GetCommand,
  PutCommand,
  DeleteCommand,
  UpdateCommand,
  QueryCommand,
  ScanCommand,
  BatchWriteCommand,
} = require('@aws-sdk/lib-dynamodb');

const { encrypt, decrypt } = require('../utils/crypto');
const config = require('../config');
const logger = require('../logger');

// Badge taxonomy — same values as SqliteStore so callers that
// compare across envs see identical enum values.
const BADGE_TYPES = {
  FIRST_PR: 'first_pr',
  FIRST_ISSUE: 'first_issue',
  DOCS_HERO: 'docs_hero',
  BUG_HUNTER: 'bug_hunter',
  ON_FIRE: 'on_fire',
  STREAK_MASTER: 'streak_master',
  MULTI_REPO: 'multi_repo',
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

// Table name mapping. Map keys (SQL-era snake_case names) match the
// outputs.tf `table_names` shape in `modules/qurl-bot-ddb/` so
// consumers look up by the same semantic key.
const TABLE_PREFIX = process.env.DDB_TABLE_PREFIX || 'qurl-bot-discord-prod-';
const TABLES = Object.freeze({
  github_links: `${TABLE_PREFIX}github-links`,
  pending_links: `${TABLE_PREFIX}pending-links`,
  contributions: `${TABLE_PREFIX}contributions`,
  badges: `${TABLE_PREFIX}badges`,
  streaks: `${TABLE_PREFIX}streaks`,
  milestones: `${TABLE_PREFIX}milestones`,
  weekly_stats: `${TABLE_PREFIX}weekly-stats`,
  qurl_sends: `${TABLE_PREFIX}qurl-sends`,
  qurl_send_configs: `${TABLE_PREFIX}qurl-send-configs`,
  orphaned_oauth_tokens: `${TABLE_PREFIX}orphaned-oauth-tokens`,
  guild_configs: `${TABLE_PREFIX}guild-configs`,
});

// Allow tests to inject a pre-configured mock client via
// `process.env.DDB_TEST_ENDPOINT` (used by integration tests against
// a local DynamoDB) or a stubbed DocumentClient. Production path
// constructs the real client once at module load.
const rawClient = new DynamoDBClient({
  region: process.env.AWS_REGION || 'us-east-2',
  ...(process.env.DDB_TEST_ENDPOINT ? { endpoint: process.env.DDB_TEST_ENDPOINT } : {}),
});
const ddb = DynamoDBDocumentClient.from(rawClient, {
  marshallOptions: {
    // Remove undefined values (default is to throw), matching
    // SQLite's "NULL goes in, NULL comes out" tolerance for
    // optional columns the bot doesn't always populate.
    removeUndefinedValues: true,
    // Preserve `""` as a distinct value (default behavior is
    // fine — DDB v3 SDK handles this correctly). Noted for
    // clarity.
  },
});

// ── Helpers ──

const nowIso = () => new Date().toISOString();

// Sentinel for milestones' nullable `repo` column — guaranteed
// distinct from any real GitHub slug because real slugs always
// contain `/`.
const NONE_SENTINEL = '__NONE__';

function contributionId(repo, prNumber) {
  return `${repo}#${prNumber}`;
}

function milestoneId(type, value, repo) {
  return `${repo || NONE_SENTINEL}#${type}#${value}`;
}

// sha-256 hex of a string — used as the PK for orphaned_oauth_tokens
// so we don't index the non-deterministic AES-GCM ciphertext.
function sha256Hex(str) {
  return crypto.createHash('sha256').update(str).digest('hex');
}

function getMonthString(date = new Date()) {
  const d = new Date(date);
  return `${d.getUTCFullYear()}-${String(d.getUTCMonth() + 1).padStart(2, '0')}`;
}

function getPreviousMonthString(date = new Date()) {
  const d = new Date(date);
  d.setUTCDate(1);
  d.setUTCMonth(d.getUTCMonth() - 1);
  return getMonthString(d);
}

// ── Pending OAuth state tokens ──

async function createPendingLink(state, discordId) {
  const minsMs = Math.trunc(Number(config.PENDING_LINK_EXPIRY_MINUTES)) * 60 * 1000;
  const expiresAt = Math.floor((Date.now() + minsMs) / 1000); // epoch seconds for DDB TTL
  await ddb.send(new PutCommand({
    TableName: TABLES.pending_links,
    Item: {
      state,
      discord_id: discordId,
      created_at: nowIso(),
      expires_at: expiresAt,
    },
  }));
  logger.debug('Created pending link', { discordId, state: state.substring(0, 8) + '...' });
}

async function getPendingLink(state) {
  const res = await ddb.send(new GetCommand({
    TableName: TABLES.pending_links,
    Key: { state },
  }));
  return res.Item ? { discord_id: res.Item.discord_id } : undefined;
}

async function deletePendingLink(state) {
  await ddb.send(new DeleteCommand({
    TableName: TABLES.pending_links,
    Key: { state },
  }));
}

// Atomic consume via ReturnValues=ALL_OLD: the delete returns the
// pre-delete row in one round-trip, closing the TOCTOU window in
// the OAuth callback handler.
async function consumePendingLink(state) {
  const res = await ddb.send(new DeleteCommand({
    TableName: TABLES.pending_links,
    Key: { state },
    ReturnValues: 'ALL_OLD',
  }));
  return res.Attributes ? { discord_id: res.Attributes.discord_id } : undefined;
}

// ── GitHub-Discord account links ──

async function createLink(discordId, githubUsername) {
  const now = nowIso();
  await ddb.send(new PutCommand({
    TableName: TABLES.github_links,
    Item: {
      discord_id: discordId,
      github_username: githubUsername.toLowerCase(),
      linked_at: now,
      updated_at: now,
    },
  }));
  logger.info('Created/updated GitHub link', { discordId, github: githubUsername });
}

async function getLinkByDiscord(discordId) {
  const res = await ddb.send(new GetCommand({
    TableName: TABLES.github_links,
    Key: { discord_id: discordId },
  }));
  return res.Item || undefined;
}

async function getLinkedDiscordIds() {
  // Scan + project only the PK. At projected scale (< 10k rows total)
  // the full scan is acceptable; the caller uses this to pre-filter a
  // guild-wide sync, not per-request. Revisit if contributions table
  // sizing grows past 50k and this query shows up in flame graphs.
  const ids = new Set();
  let ExclusiveStartKey;
  do {
    const res = await ddb.send(new ScanCommand({
      TableName: TABLES.github_links,
      ProjectionExpression: 'discord_id',
      ExclusiveStartKey,
    }));
    for (const item of res.Items || []) ids.add(item.discord_id);
    ExclusiveStartKey = res.LastEvaluatedKey;
  } while (ExclusiveStartKey);
  return ids;
}

async function getLinkByGithub(githubUsername) {
  const res = await ddb.send(new QueryCommand({
    TableName: TABLES.github_links,
    IndexName: 'github_username-index',
    KeyConditionExpression: 'github_username = :u',
    ExpressionAttributeValues: { ':u': githubUsername.toLowerCase() },
    Limit: 1,
  }));
  if (!res.Items || res.Items.length === 0) return undefined;
  // GSI is KEYS_ONLY — hop back to the base table for the full item.
  const { discord_id } = res.Items[0];
  return getLinkByDiscord(discord_id);
}

async function deleteLink(discordId) {
  const res = await ddb.send(new DeleteCommand({
    TableName: TABLES.github_links,
    Key: { discord_id: discordId },
    ReturnValues: 'ALL_OLD',
  }));
  const deleted = !!res.Attributes;
  logger.info('Deleted GitHub link', { discordId, deleted });
  // Match SqliteStore's `result.changes` shape so callers can
  // continue to check `result.changes > 0`.
  return { changes: deleted ? 1 : 0 };
}

async function forceLink(discordId, githubUsername) {
  return createLink(discordId, githubUsername);
}

// ── Contributions (merged PRs) ──

async function recordContribution(discordId, githubUsername, prNumber, repo, prTitle = null) {
  const id = contributionId(repo, prNumber);
  try {
    await ddb.send(new PutCommand({
      TableName: TABLES.contributions,
      Item: {
        contribution_id: id,
        discord_id: discordId,
        github_username: githubUsername.toLowerCase(),
        pr_number: prNumber,
        repo,
        pr_title: prTitle,
        merged_at: nowIso(),
      },
      ConditionExpression: 'attribute_not_exists(contribution_id)',
    }));
    logger.info('Recorded contribution', { discordId, github: githubUsername, pr: prNumber, repo });
    // Best-effort streak update; mirrors SqliteStore's fire-and-await.
    await updateStreak(discordId);
    return true;
  } catch (err) {
    if (err.name === 'ConditionalCheckFailedException') {
      logger.debug('Contribution already exists', { pr: prNumber, repo });
      return false;
    }
    logger.error('Failed to record contribution', { error: err.message, pr: prNumber, repo });
    return false;
  }
}

async function getContributions(discordId, limit = 50) {
  const res = await ddb.send(new QueryCommand({
    TableName: TABLES.contributions,
    IndexName: 'discord_id-merged_at-index',
    KeyConditionExpression: 'discord_id = :d',
    ExpressionAttributeValues: { ':d': discordId },
    ScanIndexForward: false, // newest first
    Limit: limit,
  }));
  return res.Items || [];
}

async function getAllContributions(limit = 100) {
  // No natural partition key for "newest N across all users" — full
  // scan, sorted client-side. Acceptable at projected volume.
  const items = [];
  let ExclusiveStartKey;
  do {
    const res = await ddb.send(new ScanCommand({
      TableName: TABLES.contributions,
      ExclusiveStartKey,
    }));
    items.push(...(res.Items || []));
    ExclusiveStartKey = res.LastEvaluatedKey;
    if (items.length >= limit * 3) break; // cheap cap — 3x headroom for the sort
  } while (ExclusiveStartKey);
  items.sort((a, b) => (b.merged_at || '').localeCompare(a.merged_at || ''));
  return items.slice(0, limit);
}

async function getContributionCount(discordId) {
  const res = await ddb.send(new QueryCommand({
    TableName: TABLES.contributions,
    IndexName: 'discord_id-merged_at-index',
    KeyConditionExpression: 'discord_id = :d',
    ExpressionAttributeValues: { ':d': discordId },
    Select: 'COUNT',
  }));
  return res.Count || 0;
}

async function getWeeklyContributions(discordId) {
  const sevenDaysAgo = new Date(Date.now() - 7 * 24 * 60 * 60 * 1000).toISOString();
  const res = await ddb.send(new QueryCommand({
    TableName: TABLES.contributions,
    IndexName: 'discord_id-merged_at-index',
    KeyConditionExpression: 'discord_id = :d AND merged_at >= :t',
    ExpressionAttributeValues: { ':d': discordId, ':t': sevenDaysAgo },
    ScanIndexForward: false,
  }));
  return res.Items || [];
}

async function getMonthlyContributions(discordId) {
  const now = new Date();
  const monthStart = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), 1)).toISOString();
  const res = await ddb.send(new QueryCommand({
    TableName: TABLES.contributions,
    IndexName: 'discord_id-merged_at-index',
    KeyConditionExpression: 'discord_id = :d AND merged_at >= :t',
    ExpressionAttributeValues: { ':d': discordId, ':t': monthStart },
    ScanIndexForward: false,
  }));
  return res.Items || [];
}

async function getUniqueRepos(discordId) {
  const items = await getContributions(discordId, 10000);
  return [...new Set(items.map(r => r.repo))];
}

async function getLastWeekContributions() {
  const sevenDaysAgo = new Date(Date.now() - 7 * 24 * 60 * 60 * 1000).toISOString();
  const items = [];
  let ExclusiveStartKey;
  do {
    const res = await ddb.send(new ScanCommand({
      TableName: TABLES.contributions,
      FilterExpression: 'merged_at >= :t',
      ExpressionAttributeValues: { ':t': sevenDaysAgo },
      ExclusiveStartKey,
    }));
    items.push(...(res.Items || []));
    ExclusiveStartKey = res.LastEvaluatedKey;
  } while (ExclusiveStartKey);
  items.sort((a, b) => (b.merged_at || '').localeCompare(a.merged_at || ''));
  return items;
}

async function getNewContributorsThisWeek() {
  // "Users whose FIRST ever contribution was in the last 7 days."
  // SQL did this with a GROUP BY + HAVING. DDB: fetch all
  // contributions (scoped by users who appeared recently is
  // already a narrow set), compute per-user MIN(merged_at),
  // filter to the window.
  const sevenDaysAgo = new Date(Date.now() - 7 * 24 * 60 * 60 * 1000).toISOString();
  const all = [];
  let ExclusiveStartKey;
  do {
    const res = await ddb.send(new ScanCommand({
      TableName: TABLES.contributions,
      ExclusiveStartKey,
    }));
    all.push(...(res.Items || []));
    ExclusiveStartKey = res.LastEvaluatedKey;
  } while (ExclusiveStartKey);
  const firstByUser = new Map();
  for (const it of all) {
    const prev = firstByUser.get(it.discord_id);
    if (!prev || (it.merged_at || '') < prev.first_contribution) {
      firstByUser.set(it.discord_id, {
        discord_id: it.discord_id,
        github_username: it.github_username,
        first_contribution: it.merged_at,
      });
    }
  }
  return [...firstByUser.values()].filter(r => (r.first_contribution || '') >= sevenDaysAgo);
}

// ── Aggregate stats and leaderboard ──

async function getStats() {
  const [links, contribs] = await Promise.all([
    ddb.send(new ScanCommand({ TableName: TABLES.github_links, Select: 'COUNT' })),
    scanAll(TABLES.contributions),
  ]);
  const contributors = new Set();
  const repoCounts = new Map();
  for (const c of contribs) {
    contributors.add(c.discord_id);
    repoCounts.set(c.repo, (repoCounts.get(c.repo) || 0) + 1);
  }
  const byRepo = [...repoCounts.entries()]
    .map(([repo, count]) => ({ repo, count }))
    .sort((a, b) => b.count - a.count);
  return {
    linkedUsers: links.Count || 0,
    totalContributions: contribs.length,
    uniqueContributors: contributors.size,
    byRepo,
  };
}

async function getTopContributors(limit = 10) {
  const contribs = await scanAll(TABLES.contributions);
  const counts = new Map();
  for (const c of contribs) counts.set(c.discord_id, (counts.get(c.discord_id) || 0) + 1);
  return [...counts.entries()]
    .map(([discord_id, count]) => ({ discord_id, count }))
    .sort((a, b) => b.count - a.count)
    .slice(0, limit);
}

async function scanAll(TableName) {
  const items = [];
  let ExclusiveStartKey;
  do {
    const res = await ddb.send(new ScanCommand({ TableName, ExclusiveStartKey }));
    items.push(...(res.Items || []));
    ExclusiveStartKey = res.LastEvaluatedKey;
  } while (ExclusiveStartKey);
  return items;
}

// ── Badges ──

async function awardBadge(discordId, badgeType) {
  try {
    await ddb.send(new PutCommand({
      TableName: TABLES.badges,
      Item: {
        discord_id: discordId,
        badge_type: badgeType,
        earned_at: nowIso(),
      },
      ConditionExpression: 'attribute_not_exists(discord_id) AND attribute_not_exists(badge_type)',
    }));
    logger.info('Badge awarded', { discordId, badge: badgeType });
    return true;
  } catch (err) {
    if (err.name === 'ConditionalCheckFailedException') return false; // already had it
    logger.error('Error awarding badge', { error: err.message });
    return false;
  }
}

async function getBadges(discordId) {
  const res = await ddb.send(new QueryCommand({
    TableName: TABLES.badges,
    KeyConditionExpression: 'discord_id = :d',
    ExpressionAttributeValues: { ':d': discordId },
  }));
  return (res.Items || [])
    .map(i => ({ badge_type: i.badge_type, earned_at: i.earned_at }))
    .sort((a, b) => (a.earned_at || '').localeCompare(b.earned_at || ''));
}

async function hasBadge(discordId, badgeType) {
  const res = await ddb.send(new GetCommand({
    TableName: TABLES.badges,
    Key: { discord_id: discordId, badge_type: badgeType },
  }));
  return !!res.Item;
}

async function checkAndAwardBadges(discordId, prTitle, repo) {
  const awarded = [];
  const count = await getContributionCount(discordId);

  if (count === 1 && await awardBadge(discordId, BADGE_TYPES.FIRST_PR)) {
    awarded.push(BADGE_TYPES.FIRST_PR);
  }

  const isDocsPR = /doc|readme|guide|tutorial|example/i.test(prTitle) || /doc|example|demo/i.test(repo);
  if (isDocsPR && !(await hasBadge(discordId, BADGE_TYPES.DOCS_HERO))) {
    if (await awardBadge(discordId, BADGE_TYPES.DOCS_HERO)) awarded.push(BADGE_TYPES.DOCS_HERO);
  }

  const isBugFix = /fix|bug|issue|patch|resolve/i.test(prTitle);
  if (isBugFix && !(await hasBadge(discordId, BADGE_TYPES.BUG_HUNTER))) {
    if (await awardBadge(discordId, BADGE_TYPES.BUG_HUNTER)) awarded.push(BADGE_TYPES.BUG_HUNTER);
  }

  const monthlyPRs = await getMonthlyContributions(discordId);
  if (monthlyPRs.length >= 2 && !(await hasBadge(discordId, BADGE_TYPES.ON_FIRE))) {
    if (await awardBadge(discordId, BADGE_TYPES.ON_FIRE)) awarded.push(BADGE_TYPES.ON_FIRE);
  }

  const streak = await getStreak(discordId);
  if (streak && streak.current_streak >= 3 && !(await hasBadge(discordId, BADGE_TYPES.STREAK_MASTER))) {
    if (await awardBadge(discordId, BADGE_TYPES.STREAK_MASTER)) awarded.push(BADGE_TYPES.STREAK_MASTER);
  }

  const uniqueRepos = await getUniqueRepos(discordId);
  if (uniqueRepos.length >= 2 && !(await hasBadge(discordId, BADGE_TYPES.MULTI_REPO))) {
    if (await awardBadge(discordId, BADGE_TYPES.MULTI_REPO)) awarded.push(BADGE_TYPES.MULTI_REPO);
  }

  return awarded;
}

async function awardFirstIssueBadge(discordId) {
  if (!(await hasBadge(discordId, BADGE_TYPES.FIRST_ISSUE))) {
    if (await awardBadge(discordId, BADGE_TYPES.FIRST_ISSUE)) return [BADGE_TYPES.FIRST_ISSUE];
  }
  return [];
}

// ── Streaks ──

async function getStreak(discordId) {
  const res = await ddb.send(new GetCommand({
    TableName: TABLES.streaks,
    Key: { discord_id: discordId },
  }));
  return res.Item;
}

async function updateStreak(discordId) {
  const currentMonth = getMonthString();
  const existing = await getStreak(discordId);

  if (!existing) {
    await ddb.send(new PutCommand({
      TableName: TABLES.streaks,
      Item: {
        discord_id: discordId,
        current_streak: 1,
        longest_streak: 1,
        last_contribution_week: currentMonth,
        updated_at: nowIso(),
      },
    }));
    return { current: 1, longest: 1, isNew: true };
  }

  if (existing.last_contribution_week === currentMonth) {
    return { current: existing.current_streak, longest: existing.longest_streak, isNew: false };
  }

  const lastMonth = getPreviousMonthString();
  const newStreak = existing.last_contribution_week === lastMonth ? existing.current_streak + 1 : 1;
  const newLongest = Math.max(newStreak, existing.longest_streak);

  await ddb.send(new UpdateCommand({
    TableName: TABLES.streaks,
    Key: { discord_id: discordId },
    UpdateExpression: 'SET current_streak = :c, longest_streak = :l, last_contribution_week = :w, updated_at = :u',
    ExpressionAttributeValues: {
      ':c': newStreak,
      ':l': newLongest,
      ':w': currentMonth,
      ':u': nowIso(),
    },
  }));
  return { current: newStreak, longest: newLongest, isNew: newStreak > existing.current_streak };
}

// ── Announcement milestones ──

async function hasMilestoneBeenAnnounced(type, value, repo = null) {
  const id = milestoneId(type, value, repo);
  const res = await ddb.send(new GetCommand({
    TableName: TABLES.milestones,
    Key: { milestone_id: id },
  }));
  return !!res.Item;
}

async function recordMilestone(type, value, repo = null) {
  const id = milestoneId(type, value, repo);
  try {
    await ddb.send(new PutCommand({
      TableName: TABLES.milestones,
      Item: {
        milestone_id: id,
        milestone_type: type,
        milestone_value: value,
        repo: repo || null,
        announced_at: nowIso(),
      },
      ConditionExpression: 'attribute_not_exists(milestone_id)',
    }));
    return true;
  } catch (err) {
    if (err.name === 'ConditionalCheckFailedException') return false;
    logger.error('recordMilestone failed', { type, value, repo, error: err.message });
    return false;
  }
}

// ── Weekly digest ──

async function getWeeklyDigestData() {
  const [lastWeekPRs, newContributors] = await Promise.all([
    getLastWeekContributions(),
    getNewContributorsThisWeek(),
  ]);
  const byRepo = {};
  for (const pr of lastWeekPRs) {
    if (!byRepo[pr.repo]) byRepo[pr.repo] = [];
    byRepo[pr.repo].push(pr);
  }
  const uniqueContributors = [...new Set(lastWeekPRs.map(pr => pr.discord_id))];
  return {
    totalPRs: lastWeekPRs.length,
    uniqueContributors: uniqueContributors.length,
    newContributors,
    byRepo,
    prs: lastWeekPRs.slice(0, 10),
  };
}

// ── QURL sends ──

async function recordQURLSend({
  sendId, senderDiscordId, recipientDiscordId, resourceId, resourceType,
  qurlLink, expiresIn, channelId, targetType,
}) {
  await ddb.send(new PutCommand({
    TableName: TABLES.qurl_sends,
    Item: {
      send_id: sendId,
      recipient_discord_id: recipientDiscordId,
      sender_discord_id: senderDiscordId,
      resource_id: resourceId,
      resource_type: resourceType,
      qurl_link: qurlLink,
      expires_in: expiresIn,
      channel_id: channelId,
      target_type: targetType,
      dm_status: 'pending',
      created_at: nowIso(),
    },
  }));
}

async function recordQURLSendBatch(sends) {
  if (!sends || sends.length === 0) return;
  // DDB BatchWriteItem caps at 25 items per request.
  const CHUNK = 25;
  const now = nowIso();
  for (let i = 0; i < sends.length; i += CHUNK) {
    const slice = sends.slice(i, i + CHUNK);
    await ddb.send(new BatchWriteCommand({
      RequestItems: {
        [TABLES.qurl_sends]: slice.map(s => ({
          PutRequest: {
            Item: {
              send_id: s.sendId,
              recipient_discord_id: s.recipientDiscordId,
              sender_discord_id: s.senderDiscordId,
              resource_id: s.resourceId,
              resource_type: s.resourceType,
              qurl_link: s.qurlLink,
              expires_in: s.expiresIn,
              channel_id: s.channelId,
              target_type: s.targetType,
              dm_status: 'pending',
              created_at: now,
            },
          },
        })),
      },
    }));
  }
}

async function updateSendDMStatus(sendId, recipientDiscordId, status) {
  await ddb.send(new UpdateCommand({
    TableName: TABLES.qurl_sends,
    Key: { send_id: sendId, recipient_discord_id: recipientDiscordId },
    UpdateExpression: 'SET dm_status = :s',
    ExpressionAttributeValues: { ':s': status },
  }));
}

async function getRecentSends(senderDiscordId, limit = 10) {
  // SQL did a LEFT JOIN on qurl_send_configs + GROUP BY send_id to
  // produce one row per send with per-send metadata. DDB: query the
  // sender's recent send_ids via the GSI, then fetch each send's
  // config + recipient-count in parallel. N+1-ish but capped at `limit`
  // so it's bounded Promise.all fanout at ~10 getItem + 10 query calls.
  const sendsRes = await ddb.send(new QueryCommand({
    TableName: TABLES.qurl_sends,
    IndexName: 'sender_discord_id-created_at-index',
    KeyConditionExpression: 'sender_discord_id = :s',
    ExpressionAttributeValues: { ':s': senderDiscordId },
    ScanIndexForward: false,
    Limit: limit * 3, // Overfetch — we'll filter out revoked then trim to `limit`.
  }));

  // Group by send_id (one row per recipient in the base table).
  const bySendId = new Map();
  for (const row of sendsRes.Items || []) {
    const existing = bySendId.get(row.send_id);
    if (!existing || (row.created_at || '') > (existing.created_at || '')) {
      bySendId.set(row.send_id, row);
    }
    const entry = bySendId.get(row.send_id);
    entry._recipient_count = (entry._recipient_count || 0) + 1;
    entry._delivered_count = (entry._delivered_count || 0) + (row.dm_status === 'sent' ? 1 : 0);
  }

  // Fetch config for each send_id in parallel.
  const sendIds = [...bySendId.keys()];
  const configs = await Promise.all(
    sendIds.map(id => ddb.send(new GetCommand({ TableName: TABLES.qurl_send_configs, Key: { send_id: id } })))
  );
  const configBySendId = new Map();
  for (let i = 0; i < sendIds.length; i++) {
    configBySendId.set(sendIds[i], configs[i].Item);
  }

  // Build result matching SqliteStore shape; filter out already-revoked
  // sends (config.revoked_at is non-null).
  const rows = [];
  for (const sendId of sendIds) {
    const base = bySendId.get(sendId);
    const cfg = configBySendId.get(sendId);
    if (cfg && cfg.revoked_at) continue;
    rows.push({
      send_id: sendId,
      resource_type: base.resource_type,
      target_type: base.target_type,
      channel_id: base.channel_id,
      expires_in: base.expires_in,
      created_at: base.created_at,
      recipient_count: base._recipient_count,
      delivered_count: base._delivered_count,
      attachment_name: cfg?.attachment_name,
      location_name: cfg?.location_name,
      personal_message: cfg?.personal_message,
    });
  }
  rows.sort((a, b) => (b.created_at || '').localeCompare(a.created_at || ''));
  return rows.slice(0, limit);
}

async function markSendRevoked(sendId, senderDiscordId) {
  // Mirror SqliteStore's two-branch logic:
  // (a) Normal path: config row exists — flip revoked_at.
  // (b) Legacy path: config row doesn't exist — insert minimal row.
  // Scoped to senderDiscordId for defense-in-depth.
  const cfgRes = await ddb.send(new GetCommand({
    TableName: TABLES.qurl_send_configs,
    Key: { send_id: sendId },
  }));
  if (cfgRes.Item) {
    if (cfgRes.Item.sender_discord_id !== senderDiscordId) return; // ownership check
    if (cfgRes.Item.revoked_at) return; // idempotent
    await ddb.send(new UpdateCommand({
      TableName: TABLES.qurl_send_configs,
      Key: { send_id: sendId },
      UpdateExpression: 'SET revoked_at = :t',
      ExpressionAttributeValues: { ':t': nowIso() },
      ConditionExpression: 'sender_discord_id = :s AND attribute_not_exists(revoked_at)',
      ExpressionAttributeNames: {},
    }));
    return;
  }

  // Legacy — look up qurl_sends row, insert minimal config with revoked_at.
  const legacyRes = await ddb.send(new QueryCommand({
    TableName: TABLES.qurl_sends,
    KeyConditionExpression: 'send_id = :sid',
    FilterExpression: 'sender_discord_id = :s',
    ExpressionAttributeValues: { ':sid': sendId, ':s': senderDiscordId },
    Limit: 1,
  }));
  if (!legacyRes.Items || legacyRes.Items.length === 0) return;
  const legacy = legacyRes.Items[0];
  await ddb.send(new PutCommand({
    TableName: TABLES.qurl_send_configs,
    Item: {
      send_id: sendId,
      sender_discord_id: senderDiscordId,
      resource_type: legacy.resource_type,
      expires_in: legacy.expires_in || '24h',
      revoked_at: nowIso(),
      created_at: nowIso(),
    },
    ConditionExpression: 'attribute_not_exists(send_id)',
  })).catch(err => {
    if (err.name === 'ConditionalCheckFailedException') return; // raced with a concurrent markSendRevoked
    throw err;
  });
}

async function saveSendConfig({
  sendId, senderDiscordId, resourceType, connectorResourceId, actualUrl,
  expiresIn, personalMessage, locationName, attachmentName,
  attachmentContentType, attachmentUrl,
}) {
  const encryptedUrl = attachmentUrl ? encrypt(attachmentUrl) : null;
  await ddb.send(new PutCommand({
    TableName: TABLES.qurl_send_configs,
    Item: {
      send_id: sendId,
      sender_discord_id: senderDiscordId,
      resource_type: resourceType,
      connector_resource_id: connectorResourceId,
      actual_url: actualUrl,
      expires_in: expiresIn,
      personal_message: personalMessage,
      location_name: locationName,
      attachment_name: attachmentName,
      attachment_content_type: attachmentContentType ?? null,
      attachment_url: encryptedUrl,
      created_at: nowIso(),
    },
  }));
}

async function getSendConfig(sendId, senderDiscordId) {
  const res = await ddb.send(new GetCommand({
    TableName: TABLES.qurl_send_configs,
    Key: { send_id: sendId },
  }));
  const row = res.Item;
  if (!row) return row;
  if (row.sender_discord_id !== senderDiscordId) return undefined; // ownership check
  return row.attachment_url
    ? { ...row, attachment_url: decrypt(row.attachment_url) }
    : row;
}

async function getSendResourceIds(sendId, senderDiscordId) {
  const res = await ddb.send(new QueryCommand({
    TableName: TABLES.qurl_sends,
    KeyConditionExpression: 'send_id = :sid',
    FilterExpression: 'sender_discord_id = :s',
    ExpressionAttributeValues: { ':sid': sendId, ':s': senderDiscordId },
  }));
  const ids = new Set();
  for (const item of res.Items || []) ids.add(item.resource_id);
  return [...ids];
}

// ── Guild (BYOK) API keys ──

async function getGuildApiKey(guildId) {
  const res = await ddb.send(new GetCommand({
    TableName: TABLES.guild_configs,
    Key: { guild_id: guildId },
  }));
  return res.Item ? decrypt(res.Item.qurl_api_key) : null;
}

async function setGuildApiKey(guildId, apiKey, configuredBy) {
  const now = nowIso();
  // PutItem replaces the item atomically. SQL did ON CONFLICT upsert;
  // DDB PutItem without ConditionExpression is upsert semantics.
  await ddb.send(new PutCommand({
    TableName: TABLES.guild_configs,
    Item: {
      guild_id: guildId,
      qurl_api_key: encrypt(apiKey),
      configured_by: configuredBy,
      configured_at: now,
      updated_at: now,
    },
  }));
}

async function removeGuildApiKey(guildId) {
  const res = await ddb.send(new DeleteCommand({
    TableName: TABLES.guild_configs,
    Key: { guild_id: guildId },
    ReturnValues: 'ALL_OLD',
  }));
  return !!res.Attributes;
}

async function getGuildConfig(guildId) {
  const res = await ddb.send(new GetCommand({
    TableName: TABLES.guild_configs,
    Key: { guild_id: guildId },
  }));
  const row = res.Item;
  if (!row) return row;
  // Strip the api key before returning — callers that need it use
  // getGuildConfigWithApiKey explicitly.
  const { qurl_api_key, ...safe } = row;
  void qurl_api_key;
  return safe;
}

async function getGuildConfigWithApiKey(guildId) {
  const res = await ddb.send(new GetCommand({
    TableName: TABLES.guild_configs,
    Key: { guild_id: guildId },
  }));
  const row = res.Item;
  if (!row) return row;
  return { ...row, qurl_api_key: row.qurl_api_key ? decrypt(row.qurl_api_key) : row.qurl_api_key };
}

// ── Orphaned OAuth tokens ──

async function recordOrphanedToken(accessToken) {
  const ttlDays = parseInt(process.env.ORPHAN_TOKEN_RETENTION_DAYS, 10) || (process.env.NODE_ENV === 'production' ? 7 : 1);
  const expiresAt = Math.floor((Date.now() + ttlDays * 24 * 60 * 60 * 1000) / 1000);
  await ddb.send(new PutCommand({
    TableName: TABLES.orphaned_oauth_tokens,
    Item: {
      token_hash: sha256Hex(accessToken),
      access_token: encrypt(accessToken),
      recorded_at: nowIso(),
      expires_at: expiresAt,
    },
  }));
}

async function countOrphanedTokens() {
  const res = await ddb.send(new ScanCommand({
    TableName: TABLES.orphaned_oauth_tokens,
    Select: 'COUNT',
  }));
  return res.Count || 0;
}

async function listOrphanedTokens(limit = 50) {
  const res = await ddb.send(new ScanCommand({
    TableName: TABLES.orphaned_oauth_tokens,
    Limit: limit,
  }));
  return (res.Items || [])
    .sort((a, b) => (a.recorded_at || '').localeCompare(b.recorded_at || ''))
    .map(r => ({ id: r.token_hash, encryptedAccessToken: r.access_token }));
}

// Caller passes the encrypted blob from listOrphanedTokens; we
// decrypt one at a time so only a single plaintext is in memory.
function decryptOrphanedToken(encryptedAccessToken) {
  return decrypt(encryptedAccessToken);
}

async function deleteOrphanedToken(id) {
  await ddb.send(new DeleteCommand({
    TableName: TABLES.orphaned_oauth_tokens,
    Key: { token_hash: id },
  }));
}

// ── Lifecycle ──

async function close() {
  // DDB client has no persistent connection to close; AWS SDK v3
  // manages sockets internally via keep-alive. The method exists
  // for Store contract parity with SqliteStore.close().
  logger.info('DDB store closed (no-op)');
}

module.exports = {
  BADGE_TYPES,
  BADGE_INFO,
  // Pending links
  createPendingLink, getPendingLink, deletePendingLink, consumePendingLink,
  // Links
  createLink, getLinkByDiscord, getLinkedDiscordIds, getLinkByGithub, deleteLink, forceLink,
  // Contributions
  recordContribution, getContributions, getAllContributions, getContributionCount,
  getWeeklyContributions, getMonthlyContributions, getUniqueRepos,
  getLastWeekContributions, getNewContributorsThisWeek,
  // Stats
  getStats, getTopContributors,
  // Badges
  awardBadge, getBadges, hasBadge, checkAndAwardBadges, awardFirstIssueBadge,
  // Streaks
  getStreak, updateStreak,
  // Milestones
  hasMilestoneBeenAnnounced, recordMilestone,
  // Weekly digest
  getWeeklyDigestData,
  // QURL sends
  recordQURLSend, recordQURLSendBatch, updateSendDMStatus, getRecentSends, markSendRevoked,
  saveSendConfig, getSendConfig, getSendResourceIds,
  // Guild configs
  getGuildApiKey, setGuildApiKey, removeGuildApiKey, getGuildConfig, getGuildConfigWithApiKey,
  // Orphaned tokens
  recordOrphanedToken, countOrphanedTokens, listOrphanedTokens,
  decryptOrphanedToken, deleteOrphanedToken,
  // Lifecycle
  close,
};
