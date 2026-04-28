// store/ddb-store — DynamoDB backend
//
// Implements the Store contract (see `src/store/contract.js`) against
// the 11 DynamoDB tables provisioned by `modules/qurl-bot-ddb/`
// in the infra repo. Selected at boot when `STORE_TYPE=ddb`.
//
// Table naming: tables carry the env in their name
// (`qurl-bot-discord-<env>-<kebab-name>`). The env prefix is
// supplied via `DDB_TABLE_PREFIX` and is REQUIRED — there is no
// default. A previous version defaulted to `qurl-bot-discord-prod-`,
// which meant a developer running the bot locally with `STORE_TYPE=
// ddb` and any AWS creds in their shell would silently hit prod
// tables. Failing fast on unset prefix is a one-time onboarding
// inconvenience (set the env in the deployment template) traded for
// eliminating that footgun. The bot task role's IAM policy must
// allow data-plane verbs on the specific table ARNs + GSI ARNs —
// scoping happens infra-side, not here.
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
//
// DDB_TABLE_PREFIX is required (no default). See header comment for
// the prod-footgun rationale. Whitespace-only values are treated as
// unset to defend against buggy container templating that emits
// `DDB_TABLE_PREFIX=` with no value — same shape as the STORE_TYPE
// normalization in `store/index.js`.
const TABLE_PREFIX = (process.env.DDB_TABLE_PREFIX ?? '').trim();
if (!TABLE_PREFIX) {
  throw new Error('DDB_TABLE_PREFIX is required when STORE_TYPE=ddb. Set it to the env-specific prefix (e.g. `qurl-bot-discord-sandbox-` for sandbox, `qurl-bot-discord-prod-` for prod) in the deployment template.');
}
// Trailing-dash check. The prefix is concatenated directly with each
// table's kebab-case suffix (`${TABLE_PREFIX}github-links`), so a
// missing trailing dash silently produces malformed names like
// `qurl-bot-discord-prodgithub-links` and the bot's first DDB call
// returns ResourceNotFoundException — clear at the first call but
// confusing in CloudWatch logs (looks like a permission or naming
// issue, not a config typo). Catch it at boot so the failure points
// directly at the env var.
if (!TABLE_PREFIX.endsWith('-')) {
  throw new Error(`DDB_TABLE_PREFIX must end with '-' (got '${TABLE_PREFIX}'). The prefix is concatenated with kebab-case table suffixes; a missing dash produces malformed names like '${TABLE_PREFIX}github-links'. Add the trailing '-' in the deployment template.`);
}
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

// AWS_REGION is required (no fallback). Same shape of footgun the
// DDB_TABLE_PREFIX guard above closes: a developer with stray AWS
// creds + DDB_TABLE_PREFIX=qurl-bot-discord-prod- + unset
// AWS_REGION would silently land in whichever region the SDK
// defaults to (us-east-2 today via the prior `|| 'us-east-2'`
// fallback), which is presumably the prod region. Catch at boot.
// Whitespace-only treated as unset, mirroring the prefix guard.
const AWS_REGION = (process.env.AWS_REGION ?? '').trim();
if (!AWS_REGION) {
  throw new Error('AWS_REGION is required when STORE_TYPE=ddb. Set it to the env-specific region (e.g. `us-east-2` for sandbox, the matching prod region for prod) in the deployment template.');
}

// Allow tests to inject a pre-configured mock client via
// `process.env.DDB_TEST_ENDPOINT` (used by integration tests against
// a local DynamoDB) or a stubbed DocumentClient. Production path
// constructs the real client once at module load.
const rawClient = new DynamoDBClient({
  region: AWS_REGION,
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
  // Preserve the original `linked_at` across re-links — SQLite's
  // `ON CONFLICT(discord_id) DO UPDATE SET github_username = excluded…`
  // only touched github_username + updated_at, so the "when did I
  // first link" timestamp stayed stable. A naive PutItem would
  // overwrite linked_at on every re-link, breaking any analytics
  // on first-link cohort age. UpdateExpression with
  // `if_not_exists(linked_at, :now)` keeps the SQLite behavior:
  // set linked_at only on first write; always update
  // github_username + updated_at.
  await ddb.send(new UpdateCommand({
    TableName: TABLES.github_links,
    Key: { discord_id: discordId },
    UpdateExpression: 'SET github_username = :g, updated_at = :now, linked_at = if_not_exists(linked_at, :now)',
    ExpressionAttributeValues: {
      ':g': githubUsername.toLowerCase(),
      ':now': now,
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
  // Tri-state return: 'recorded' | 'duplicate' | 'failed'.
  // Why three states instead of boolean: callers want to act
  // differently on each. Webhook handler skips role+badges on both
  // dup AND failure (preserves the "no role without persisted
  // credit" invariant — but failure is the case where GitHub will
  // re-deliver, dup is steady-state). Historical-backfill loop in
  // routes/oauth.js wants to count NEW contributions only, AND
  // surface transient failures so the user-visible message doesn't
  // silently undercount during onboarding. The boolean shape
  // collapsed the second case into "no-op."
  //
  // Split into two try blocks so a streak-update failure doesn't
  // mask a successful contribution insert. DDB's two-round-trip
  // streak flow (Get + Put/Update) has a higher base failure rate
  // than SQLite's same-transaction write; collapsing the catches
  // would mean a transient streak timeout causes the caller to
  // skip badge evaluation for a contribution that actually landed.
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
  } catch (err) {
    if (err.name === 'ConditionalCheckFailedException') {
      // Dedup: a webhook redelivery for the same (repo, pr_number)
      // hits this branch. Streak update is intentionally NOT
      // attempted on this path — if the streak failed during the
      // ORIGINAL recordContribution call, it logged loudly there
      // and the next contribution will pick the streak back up via
      // updateStreak's reconciliation. Retrying streak on every
      // dedup'd redelivery would amplify load with no correctness
      // win.
      logger.debug('Contribution already exists', { pr: prNumber, repo });
      return 'duplicate';
    }
    logger.error('Failed to record contribution', { error: err.message, pr: prNumber, repo });
    return 'failed';
  }

  try {
    await updateStreak(discordId);
  } catch (err) {
    // Don't fail the caller — contribution IS recorded. Log so an
    // operator notices sustained streak failures (would indicate a
    // streak-table IAM / capacity issue, not a correctness bug).
    logger.error('Streak update failed after recordContribution succeeded', {
      discordId, pr: prNumber, repo, error: err.message,
    });
  }
  return 'recorded';
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
  // scan, sorted client-side. DDB Scan returns items in internal-hash
  // order (not chronological), so an early-termination cap could miss
  // the newest rows if they happen to sit late in the scan order.
  // At projected volume (< 10k rows) a full scan is acceptable; if
  // contributions ever grow past ~50k, introduce a dedicated "newest
  // across users" GSI (pk=__all__, sk=merged_at) or an aggregated
  // counter row and query that instead.
  const items = await scanAll(TABLES.contributions);
  items.sort((a, b) => (b.merged_at || '').localeCompare(a.merged_at || ''));
  return items.slice(0, limit);
}

async function getContributionCount(discordId) {
  // GSI queries are eventually consistent — ConsistentRead isn't
  // supported on GSIs. A `Query` issued immediately after a base-
  // table `PutItem` (see recordContribution → checkAndAwardBadges
  // chain) can return a count of 0 when it should be 1, because
  // the GSI hasn't propagated the new row yet. That's a permanent
  // miss for the FIRST_PR badge — on the next contribution the
  // count is 2 and the `count === 1` check never fires again.
  //
  // Mitigation: short retry loop. DDB GSI lag under normal load is
  // typically <50ms; retry with backoff covers the tail. Only
  // re-issues on count=0 to avoid noise for users with established
  // histories (where count=0 is the correct answer for a
  // never-contributor).
  //
  // Caller contract: this is intended to be called RIGHT AFTER a
  // contribution insert (the checkAndAwardBadges chain), where
  // count=0 is almost always GSI lag rather than ground truth. A
  // future caller probing "does this user have any history?" for
  // an unlinked Discord user pays the full ~350ms before getting
  // back the correct 0. If you add a "true zero is expected"
  // caller, take the retry loop out for that call site (or push
  // it behind a `justWrote` flag) — don't make every legitimate-
  // zero query eat the GSI-lag budget.
  const MAX_RETRIES = 3;
  for (let attempt = 0; attempt <= MAX_RETRIES; attempt++) {
    const res = await ddb.send(new QueryCommand({
      TableName: TABLES.contributions,
      IndexName: 'discord_id-merged_at-index',
      KeyConditionExpression: 'discord_id = :d',
      ExpressionAttributeValues: { ':d': discordId },
      Select: 'COUNT',
    }));
    const count = res.Count || 0;
    if (count > 0 || attempt === MAX_RETRIES) return count;
    // Backoff: 50ms, 100ms, 200ms. Total worst case ~350ms before
    // giving up and returning 0.
    await new Promise(resolve => setTimeout(resolve, 50 * Math.pow(2, attempt)));
  }
  return 0;
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
  // Pagination + projection. Why not getContributions(discordId, 10000)?
  //   1. DDB Query response is capped at 1MB per page regardless of
  //      the requested Limit, with LastEvaluatedKey set when more
  //      pages exist. A user with many contributions (or longer
  //      pr_title / repo strings) hits the cap before 10k rows and
  //      getContributions silently truncates — getUniqueRepos
  //      would then miss MULTI_REPO badge eligibility.
  //   2. We only need `repo` to dedup; pulling the full row is
  //      wasted RCU + bytes. ProjectionExpression cuts the read
  //      cost on a path that fires from checkAndAwardBadges on
  //      every contribution insert.
  const repos = new Set();
  let ExclusiveStartKey;
  do {
    const res = await ddb.send(new QueryCommand({
      TableName: TABLES.contributions,
      IndexName: 'discord_id-merged_at-index',
      KeyConditionExpression: 'discord_id = :d',
      ExpressionAttributeValues: { ':d': discordId },
      ProjectionExpression: 'repo',
      ExclusiveStartKey,
    }));
    for (const item of res.Items || []) repos.add(item.repo);
    ExclusiveStartKey = res.LastEvaluatedKey;
  } while (ExclusiveStartKey);
  return [...repos];
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
  const [linkedUsers, contribs] = await Promise.all([
    countAll(TABLES.github_links),
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
    linkedUsers,
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

// Paginated `Select: COUNT` scan. DDB Scan with Select=COUNT is
// STILL subject to the 1MB read cap — `res.Count` is the count for
// the current page only, with `LastEvaluatedKey` set when more
// pages exist. A naive single-call ScanCommand silently undercounts
// once the table size pushes past ~1MB worth of items, with no
// error signal. Accumulate Count across pages until LEK is empty.
async function countAll(TableName) {
  let total = 0;
  let ExclusiveStartKey;
  do {
    const res = await ddb.send(new ScanCommand({ TableName, Select: 'COUNT', ExclusiveStartKey }));
    total += res.Count || 0;
    ExclusiveStartKey = res.LastEvaluatedKey;
  } while (ExclusiveStartKey);
  return total;
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

  // FIRST_PR award: idempotent on `count >= 1 && !hasBadge(FIRST_PR)`
  // rather than `count === 1`. Two reasons the strict-equality check
  // could permanently miss the badge on DDB:
  //   1. Webhook redelivery: GitHub re-sends a PR-merged webhook
  //      after 4xx/5xx; recordContribution dedups via CCFE on the
  //      base table, but if a SECOND legitimate PR landed in the
  //      same window the GSI may already show count=2 for the
  //      first user's first-ever contribution.
  //   2. GSI lag inversion: the retry loop in getContributionCount
  //      papers over count=0, but if writes for two contributions
  //      hit the GSI between the retry's reads, count can jump
  //      directly from 0 → 2 and skip count=1.
  // Idempotent form: hasBadge is the source of truth (badge table
  // has its own composite-PK dedup), so awardBadge can fire on any
  // count≥1 and the condition self-clears after the first award.
  // SQLite has the same race window in theory but a much narrower
  // one — flagging here so a future SQLite cleanup follows the
  // same shape.
  if (count >= 1 && !(await hasBadge(discordId, BADGE_TYPES.FIRST_PR))) {
    if (await awardBadge(discordId, BADGE_TYPES.FIRST_PR)) awarded.push(BADGE_TYPES.FIRST_PR);
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
  // Public Store contract: eventually-consistent GetItem, matching
  // SqliteStore's signature. updateStreak's CCFE-recurse path needs
  // a ConsistentRead but issues that GetCommand inline (see comment
  // there) so this function stays uniform across backends — no
  // backend-specific options leaking onto the contract.
  const res = await ddb.send(new GetCommand({
    TableName: TABLES.streaks,
    Key: { discord_id: discordId },
  }));
  return res.Item;
}

async function updateStreak(discordId, { _afterRace = false } = {}) {
  const currentMonth = getMonthString();
  // First call: eventually-consistent via getStreak (cheaper, and
  // any staleness here just means we'll attempt the conditional
  // Put and either succeed or take the CCFE → recurse path).
  // Recurse re-entry: ConsistentRead inline so we KNOW the
  // concurrent winner's row is visible. We don't widen getStreak's
  // signature for this backend-specific concern; the inline
  // GetCommand keeps the post-race reconciliation visible at its
  // call site.
  let existing;
  if (_afterRace) {
    const res = await ddb.send(new GetCommand({
      TableName: TABLES.streaks,
      Key: { discord_id: discordId },
      ConsistentRead: true,
    }));
    existing = res.Item;
  } else {
    existing = await getStreak(discordId);
  }

  if (!existing) {
    // First-write race: two concurrent recordContribution calls for
    // a brand-new contributor BOTH see existing == null and BOTH
    // PutItem. Without a condition the second clobbers the first
    // (still streak=1 here, so the data outcome matches; but the
    // second writer's attribution race-loses silently). Guard with
    // attribute_not_exists; on CCFE fall through to the update path
    // — by definition a streak row now exists, so re-reading it
    // (ConsistentRead, since the concurrent winner JUST committed)
    // and running the same-month / consecutive-month / break logic
    // produces the correct end state. Cheap fix that closes the
    // insert-branch race without TransactWriteItems.
    if (_afterRace) {
      // Defense-in-depth: a ConsistentRead after CCFE that STILL
      // returns undefined would mean DDB lost the concurrent
      // winner's write, which violates DDB's strong-consistency
      // contract. Rather than infinite-recurse, throw loud.
      throw new Error(`updateStreak: streak row for ${discordId} still missing after CCFE + ConsistentRead — DDB consistency violation or a bug above this layer.`);
    }
    try {
      await ddb.send(new PutCommand({
        TableName: TABLES.streaks,
        Item: {
          discord_id: discordId,
          current_streak: 1,
          longest_streak: 1,
          last_contribution_week: currentMonth,
          updated_at: nowIso(),
        },
        ConditionExpression: 'attribute_not_exists(discord_id)',
      }));
      return { current: 1, longest: 1, isNew: true };
    } catch (err) {
      if (err.name !== 'ConditionalCheckFailedException') throw err;
      // Concurrent writer beat us to the insert. Recurse with
      // _afterRace=true so getStreak does a ConsistentRead — the
      // row is durably present per CCFE, so eventual consistency
      // could still return undefined for a few ms and re-enter
      // this branch. ConsistentRead closes that hole.
      return updateStreak(discordId, { _afterRace: true });
    }
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
  // DDB BatchWriteItem caps at 25 items per request. A batch can come
  // back with `UnprocessedItems` populated — DDB returns items that
  // were throttled, rejected on the 4MB payload limit, or hit a
  // transient 5xx. The caller MUST retry those with exponential
  // backoff; silent-drop would leak recipients (the row never lands
  // in qurl_sends, so `updateSendDMStatus` against the missing
  // composite key later becomes a no-op and the user sees a missing
  // delivery). SQLite used `db.transaction` which was atomic; DDB
  // has no equivalent so the retry loop IS the durability guarantee.
  const CHUNK = 25;
  const MAX_RETRIES = 5;
  // Single `now` shared across every chunk + every retry of this
  // batch. INTENTIONAL parity with SQLite's transactional insert —
  // every recipient of one send shares one `created_at`, and
  // downstream queries (getRecentSends groups by send_id and uses
  // created_at as the GSI sort key) rely on that. Don't "fix" this
  // to nowIso()-per-chunk: a slow batch under throttle/retry would
  // produce a 100-recipient send with a 2s spread of created_at
  // values, fragmenting the GSI ordering and making a single send
  // look like several distinct events to consumers.
  const now = nowIso();
  for (let i = 0; i < sends.length; i += CHUNK) {
    const slice = sends.slice(i, i + CHUNK);
    let requestItems = {
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
    };
    for (let attempt = 0; attempt <= MAX_RETRIES; attempt++) {
      const res = await ddb.send(new BatchWriteCommand({ RequestItems: requestItems }));
      const unprocessed = res.UnprocessedItems || {};
      const remaining = unprocessed[TABLES.qurl_sends];
      if (!remaining || remaining.length === 0) break;
      if (attempt === MAX_RETRIES) {
        // All attempts exhausted — fail loud rather than silently
        // dropping recipients. Caller's retry policy (workflow-level)
        // decides whether to re-invoke the whole /qurl send command.
        throw new Error(`recordQURLSendBatch: ${remaining.length} unprocessed items after ${MAX_RETRIES + 1} attempts`);
      }
      // Exponential backoff: 50ms, 100ms, 200ms, 400ms, 800ms.
      // `.unref()`-equivalent for Promise sleep isn't needed; the
      // await keeps the event loop moving.
      await new Promise(resolve => setTimeout(resolve, 50 * Math.pow(2, attempt)));
      requestItems = unprocessed;
    }
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
  // sender's recent send_ids via the GSI to build the UNIQUE set of
  // recent sends (limit*3 overfetch for revoked-filter headroom),
  // then do a per-send Query to compute the accurate
  // recipient_count / delivered_count (the GSI result may truncate
  // if one send has many recipients). Then Promise.all the config
  // fetches. Bounded fanout at ~10 queries + 10 gets.
  // Paginate the GSI until we've collected `limit * 2` unique
  // send_ids (2x for revoked-filter headroom) OR the GSI is
  // exhausted. Fixed-Limit overfetch would starve older sends if a
  // single recent send has many recipients — their rows fill the
  // window and push older distinct sends past the limit. Paginating
  // until `limit * 2` unique send_ids collected avoids that
  // starvation. Capped at 10 pagination rounds to bound worst-case
  // cost when a sender has hundreds of recent sends.
  // GSI projection requirement: this Query reads `resource_type`,
  // `target_type`, `channel_id`, `expires_in`, `created_at` directly
  // off the returned rows (see metaBySendId usage below). That only
  // works because `sender_discord_id-created_at-index` is declared
  // with `projection_type = "ALL"` in the qurl-bot-ddb terraform
  // module (modules/qurl-bot-ddb/main.tf — search for the GSI block
  // on the `qurl_sends` table). If that ever narrows to KEYS_ONLY
  // or INCLUDE without these attrs, those fields silently become
  // `undefined` at runtime — unit tests won't catch it because the
  // mock returns full rows. Cross-repo invariant; flag both sides
  // if you change either.
  const metaBySendId = new Map();
  const sendIdOrder = [];
  let ExclusiveStartKey;
  const MAX_GSI_PAGES = 10;
  const TARGET_UNIQUE_SENDS = limit * 2;
  // Per-page GSI Limit: caps RCU on the hot path. Without it, a
  // sender whose latest send fanned out to 1000 recipients reads
  // ALL 1000 rows (1MB / 8KB-ish per row → up to ~125 RCU
  // amortized per page) every time the user runs /qurl history,
  // even though the function only needs `TARGET_UNIQUE_SENDS`
  // distinct send_ids. 100 is a comfortable upper bound on
  // recipients-per-typical-send while still letting up-to-100
  // distinct sends land in a single page on senders who use the
  // command for many small sends. Pagination already handles the
  // case where one page isn't enough.
  const GSI_PAGE_LIMIT = 100;
  for (let page = 0; page < MAX_GSI_PAGES && sendIdOrder.length < TARGET_UNIQUE_SENDS; page++) {
    const sendsRes = await ddb.send(new QueryCommand({
      TableName: TABLES.qurl_sends,
      IndexName: 'sender_discord_id-created_at-index',
      KeyConditionExpression: 'sender_discord_id = :s',
      ExpressionAttributeValues: { ':s': senderDiscordId },
      ScanIndexForward: false,
      Limit: GSI_PAGE_LIMIT,
      ExclusiveStartKey,
    }));
    for (const row of sendsRes.Items || []) {
      const existing = metaBySendId.get(row.send_id);
      if (!existing) {
        sendIdOrder.push(row.send_id);
        metaBySendId.set(row.send_id, row);
      } else if ((row.created_at || '') > (existing.created_at || '')) {
        metaBySendId.set(row.send_id, row);
      }
    }
    ExclusiveStartKey = sendsRes.LastEvaluatedKey;
    if (!ExclusiveStartKey) break;
  }

  // Parallel fetch: per-send recipient query + per-send config
  // getItem. Query on the BASE table (pk=send_id) returns every
  // recipient for that send, so counts are exact.
  const [allRecipients, allConfigs] = await Promise.all([
    Promise.all(sendIdOrder.map(id => ddb.send(new QueryCommand({
      TableName: TABLES.qurl_sends,
      KeyConditionExpression: 'send_id = :sid',
      ExpressionAttributeValues: { ':sid': id },
    })))),
    Promise.all(sendIdOrder.map(id => ddb.send(new GetCommand({
      TableName: TABLES.qurl_send_configs,
      Key: { send_id: id },
    })))),
  ]);

  const rows = [];
  for (let i = 0; i < sendIdOrder.length; i++) {
    const sendId = sendIdOrder[i];
    const meta = metaBySendId.get(sendId);
    const recipients = allRecipients[i].Items || [];
    const cfg = allConfigs[i].Item;
    if (cfg && cfg.revoked_at) continue;
    const recipientCount = recipients.length;
    const deliveredCount = recipients.filter(r => r.dm_status === 'sent').length;
    rows.push({
      send_id: sendId,
      resource_type: meta.resource_type,
      target_type: meta.target_type,
      channel_id: meta.channel_id,
      expires_in: meta.expires_in,
      created_at: meta.created_at,
      recipient_count: recipientCount,
      delivered_count: deliveredCount,
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
    try {
      await ddb.send(new UpdateCommand({
        TableName: TABLES.qurl_send_configs,
        Key: { send_id: sendId },
        UpdateExpression: 'SET revoked_at = :t',
        ExpressionAttributeValues: {
          ':t': nowIso(),
          ':s': senderDiscordId,
        },
        ConditionExpression: 'sender_discord_id = :s AND attribute_not_exists(revoked_at)',
      }));
    } catch (err) {
      // Swallow CCFE — a concurrent revoke already flipped
      // revoked_at (or a racing caller raced ownership swapping,
      // which the condition also rejects). Either way the end
      // state is "send is revoked" so no action needed. Any other
      // error propagates.
      if (err.name !== 'ConditionalCheckFailedException') throw err;
    }
    return;
  }

  // Legacy — look up qurl_sends row, insert minimal config with revoked_at.
  // No `Limit: 1`: DDB applies Limit BEFORE FilterExpression, so the
  // first server-side row gets read and the filter on
  // sender_discord_id is then applied to that single row. Today every
  // recipient row of a given send_id shares one sender_discord_id (per
  // recordQURLSend / recordQURLSendBatch invariants), so a Limit:1
  // would be safe — but a future migration / manual repair / partial
  // write that violated the invariant would silently miss the
  // matching row and the revoke would no-op. Dropping the Limit makes
  // the lookup robust regardless of the partition layout: the
  // partition is bounded by recipient count anyway, and the match-
  // first-then-stop happens client-side via the .find() below.
  const legacyRes = await ddb.send(new QueryCommand({
    TableName: TABLES.qurl_sends,
    KeyConditionExpression: 'send_id = :sid',
    FilterExpression: 'sender_discord_id = :s',
    ExpressionAttributeValues: { ':sid': sendId, ':s': senderDiscordId },
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
  // SQLite's `ON CONFLICT(guild_id) DO UPDATE SET qurl_api_key=…,
  // configured_by=…, updated_at=…` deliberately preserved
  // `configured_at` across re-keys — the field tracks "when was
  // this guild FIRST configured" and downstream cohort analytics
  // depend on that semantic. A naive PutItem rewrites the whole
  // row, including resetting configured_at. Mirror createLink's
  // shape: UpdateCommand with `if_not_exists(configured_at, :u)`
  // so first-write sets it and re-keys leave it alone.
  await ddb.send(new UpdateCommand({
    TableName: TABLES.guild_configs,
    Key: { guild_id: guildId },
    UpdateExpression: 'SET qurl_api_key = :k, configured_by = :b, updated_at = :u, configured_at = if_not_exists(configured_at, :u)',
    ExpressionAttributeValues: {
      ':k': encrypt(apiKey),
      ':b': configuredBy,
      ':u': now,
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
  // getGuildConfigWithApiKey explicitly. Underscore-prefix on the
  // dropped field is the idiomatic "intentionally unused" marker
  // for eslint's no-unused-vars rule (configured to allow leading
  // `_`); avoids the `void` workaround.
  const { qurl_api_key: _drop, ...safe } = row;
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
  // Dedup on token_hash. A retry / replay of the same plaintext
  // (e.g. a flaky revoke path that re-enqueues) would otherwise
  // overwrite the existing row and push expires_at 7 days
  // further out — silently extending the credential's lifetime
  // in the queue beyond the operator-stated retention. CCFE on
  // the second insert is the correct shape: same row already
  // exists, no-op. Mirrors the createLink / setGuildApiKey
  // first-write-wins pattern.
  try {
    await ddb.send(new PutCommand({
      TableName: TABLES.orphaned_oauth_tokens,
      Item: {
        token_hash: sha256Hex(accessToken),
        access_token: encrypt(accessToken),
        recorded_at: nowIso(),
        expires_at: expiresAt,
      },
      ConditionExpression: 'attribute_not_exists(token_hash)',
    }));
  } catch (err) {
    if (err.name !== 'ConditionalCheckFailedException') throw err;
    // Token already in queue; original recorded_at + expires_at
    // are preserved. Idempotent caller-visible behavior.
  }
}

async function countOrphanedTokens() {
  return countAll(TABLES.orphaned_oauth_tokens);
}

async function listOrphanedTokens(limit = 50) {
  // Why scanAll instead of `ScanCommand({ Limit })`: DDB applies
  // Scan's Limit BEFORE returning items, so a `Limit: 50` Scan
  // returns the first 50 items in DDB internal-hash order, then
  // we sort those 50. The actually-oldest tokens may never appear
  // in that arbitrary subset — and the sweeper depends on
  // oldest-first to retry tokens before the 7-day TTL purges
  // them. SqliteStore returns true oldest-N via
  // `ORDER BY recorded_at ASC LIMIT ?`; matching that requires a
  // full-table read here. Bounded by ORPHAN_TOKEN_RETENTION_DAYS
  // × write rate; per the module-header projection this stays
  // small. If the queue ever grows past a few thousand, swap in a
  // `pk='__all__', sk=recorded_at` GSI and Query it.
  const items = await scanAll(TABLES.orphaned_oauth_tokens);
  items.sort((a, b) => (a.recorded_at || '').localeCompare(b.recorded_at || ''));
  return items.slice(0, limit).map(r => ({ id: r.token_hash, encryptedAccessToken: r.access_token }));
}

// Caller passes the encrypted blob from listOrphanedTokens; we
// decrypt one at a time so only a single plaintext is in memory.
// Async to match the rest of the Store contract even though the
// body is sync CPU work — callers `await` uniformly and a future
// backend that needs async decrypt (e.g. KMS GenerateDataKey) can
// drop in without call-site churn.
async function decryptOrphanedToken(encryptedAccessToken) {
  return decrypt(encryptedAccessToken);
}

async function deleteOrphanedToken(id) {
  await ddb.send(new DeleteCommand({
    TableName: TABLES.orphaned_oauth_tokens,
    Key: { token_hash: id },
  }));
}

// ── Lifecycle ──

// Cheap "is the data layer functional" probe for /health. Single
// GetItem against a sentinel key on the smallest table (pending_links
// — short TTL, low cardinality) verifies SDK init, network path,
// IAM, and that the table exists, in one ~1-RCU round-trip. Throws
// if any of those are broken so the orchestrator replaces the
// container.
//
// Why not Scan/getStats(): /health is hit at LB cadence (10–30s).
// SqliteStore's old getStats() probe was constant-time aggregation
// against indexed COUNT(*); the DDB equivalent would be a paginated
// full-table Scan. Using getStats() at health-check cadence would
// scale RCU cost with table size and amplify cost-per-instance in
// any fleet. /metrics keeps the full aggregation — that's the right
// home for it.
async function healthCheck() {
  await ddb.send(new GetCommand({
    TableName: TABLES.pending_links,
    Key: { state: '__healthcheck__' },
  }));
  return { ok: true };
}

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
  close, healthCheck,
};
