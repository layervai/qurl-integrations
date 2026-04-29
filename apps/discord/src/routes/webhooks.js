// GitHub webhook routes
const express = require('express');
const crypto = require('crypto');
const { EmbedBuilder } = require('discord.js');
const config = require('../config');
const db = require('../store');
const logger = require('../logger');
const { COLORS, GOOD_FIRST_ISSUE_PATTERNS } = require('../constants');
const { escapeDiscordMarkdown } = require('../utils/sanitize');
// Short alias — GitHub webhook payloads put user-controlled text everywhere
// (PR title, issue title, commit message, branch name, label). We render
// those inside Discord embeds, which render markdown including [text](url)
// links, so every field below goes through `md()` before interpolation.
const md = escapeDiscordMarkdown;

// Only https://github.com/ URLs are valid for EmbedBuilder.setURL here. A
// crafted payload with a non-GitHub or non-https scheme would otherwise
// produce a clickable link pointing anywhere. Returns null on mismatch so
// the caller can omit setURL entirely.
function safeGithubUrl(url) {
  if (typeof url !== 'string') return null;
  return url.startsWith('https://github.com/') ? url : null;
}
const {
  assignContributorRole,
  notifyPRMerge,
  notifyBadgeEarned,
  postGoodFirstIssue,
  postReleaseAnnouncement,
  postStarMilestone,
  postToGitHubFeed,
} = require('../discord');

const router = express.Router();

// Verify GitHub webhook signature
function verifySignature(req) {
  const signature = req.headers['x-hub-signature-256'];

  if (!config.GITHUB_WEBHOOK_SECRET) {
    logger.error('GITHUB_WEBHOOK_SECRET not configured - rejecting webhook');
    return false;
  }

  if (!signature) {
    logger.warn('Webhook request missing signature');
    return false;
  }
  // Validate the header is exactly `sha256=<64 lowercase hex>` before it
  // reaches timingSafeEqual. An attacker can't pick the digest, but a
  // malformed header (wrong length, non-hex) should be rejected early
  // rather than producing unequal-length buffers inside the try/catch.
  if (typeof signature !== 'string' || !/^sha256=[0-9a-f]{64}$/.test(signature)) {
    // Don't log the (attacker-controlled) signature prefix — only shape data.
    logger.warn('Webhook signature has unexpected format', {
      length: typeof signature === 'string' ? signature.length : 0,
      hasPrefix: typeof signature === 'string' && signature.startsWith('sha256='),
    });
    return false;
  }
  // Defensive: if middleware ordering ever changes or a request arrives with
  // a non-JSON content type, rawBody may be absent. hmac.update(undefined)
  // throws TypeError, which is outside the try/catch below. Logged at error
  // level so oncall catches a silent misconfiguration quickly — legit GitHub
  // traffic always has a JSON body and hits the /webhook express.json parser.
  if (!req.rawBody) {
    logger.error('Webhook middleware did not populate rawBody — check server.js middleware ordering. Signature verification is BLOCKED until fixed.');
    return false;
  }

  const hmac = crypto.createHmac('sha256', config.GITHUB_WEBHOOK_SECRET);
  const digest = 'sha256=' + hmac.update(req.rawBody).digest('hex');

  try {
    return crypto.timingSafeEqual(Buffer.from(signature), Buffer.from(digest));
  } catch {
    return false;
  }
}

// Check if repo belongs to allowed organization
function isAllowedRepo(repoFullName) {
  const [org] = repoFullName.split('/');
  return config.ALLOWED_GITHUB_ORGS.includes(org.toLowerCase());
}

// Check if PR is from an automated bot (dependabot, renovate, etc.)
function isAutomatedBot(prUser) {
  if (!prUser) return false;
  const botNames = ['dependabot', 'renovate', 'snyk-bot', 'whitesource-renovate'];
  const username = prUser.login?.toLowerCase() || '';
  return prUser.type === 'Bot' || botNames.some(bot => username.includes(bot));
}

// Per-IP counter of failed-signature attempts so an attacker can't burn CPU
// by firing unlimited invalid webhooks. Legitimate GitHub traffic (valid
// HMAC) is never throttled. Swept every 5 minutes.
// SCALING: single-instance only. Move to Redis if the bot runs horizontally.
const BAD_SIG_WINDOW_MS = 60_000;
const BAD_SIG_MAX = 30;
const badSigAttempts = new Map(); // ip -> number[]  (timestamps)
setInterval(() => {
  const cutoff = Date.now() - BAD_SIG_WINDOW_MS * 2;
  for (const [ip, times] of badSigAttempts) {
    const recent = times.filter(t => t > cutoff);
    if (recent.length === 0) badSigAttempts.delete(ip);
    else badSigAttempts.set(ip, recent);
  }
}, 5 * 60 * 1000).unref();

// Hard cap per-IP array to prevent a single abusive IP from growing its
// timestamp list unboundedly between sweeps.
const BAD_SIG_PER_IP_CAP = BAD_SIG_MAX * 4;

function recordBadSig(ip) {
  const now = Date.now();
  let list = (badSigAttempts.get(ip) || []).filter(t => t > now - BAD_SIG_WINDOW_MS);
  list.push(now);
  if (list.length > BAD_SIG_PER_IP_CAP) {
    list = list.slice(-BAD_SIG_PER_IP_CAP);
  }
  if (badSigAttempts.size > 10_000) {
    // Same 10%-drop strategy as oauth.js rateLimitStore — single-entry
    // eviction can't keep up with a distributed flood of unique IPs.
    const dropCount = Math.max(1, Math.floor(badSigAttempts.size / 10));
    const it = badSigAttempts.keys();
    for (let i = 0; i < dropCount; i++) {
      const k = it.next().value;
      if (k === undefined) break;
      badSigAttempts.delete(k);
    }
  }
  badSigAttempts.set(ip, list);
  return list.length;
}

// Main webhook handler
router.post('/github', async (req, res) => {
  const ip = req.ip || 'unknown';
  const existing = (badSigAttempts.get(ip) || []).filter(t => t > Date.now() - BAD_SIG_WINDOW_MS);
  if (existing.length >= BAD_SIG_MAX) {
    logger.warn('Webhook rate limit exceeded (bad signatures)', { ip, recentFailures: existing.length });
    return res.status(429).json({ error: 'Too many invalid webhook attempts' });
  }

  if (!verifySignature(req)) {
    const n = recordBadSig(ip);
    logger.warn('Invalid webhook signature', { ip, totalInWindow: n });
    return res.status(401).json({ error: 'Invalid signature' });
  }

  const event = req.headers['x-github-event'];
  const payload = req.body;
  const repo = payload.repository?.full_name;

  logger.info(`Received GitHub event: ${event}`, { repo, action: payload.action });

  // Ping is the only event GitHub sends without a repository — let it through
  // so the webhook health check works.
  if (event === 'ping') {
    return res.status(200).send('OK - ping');
  }

  // Require a repository and verify it's on the allowlist. Rejects forged
  // events from repos that happen to share the same webhook secret, and
  // guards downstream handlers that dereference payload.repository.full_name.
  if (!repo || !isAllowedRepo(repo)) {
    logger.warn(`Rejecting webhook from non-allowed or missing repo`, { repo: repo || '(none)', event });
    return res.status(200).send('OK - ignored');
  }

  try {
    switch (event) {
      case 'pull_request':
        await handlePullRequest(payload);
        break;
      case 'issues':
        await handleIssue(payload);
        break;
      case 'release':
        await handleRelease(payload);
        break;
      case 'star':
        await handleStar(payload);
        break;
      case 'push':
      case 'create':
      case 'delete':
        await handleActivityFeed(event, payload);
        break;
      default:
        logger.debug(`Unhandled event type: ${event}`);
    }
  } catch (error) {
    // Return 500 so GitHub retries: 2xx = "delivered successfully, don't
    // retry", which would silently drop events on transient DB/Discord errors.
    logger.error(`Error handling ${event} webhook`, { error: error.message });
    return res.status(500).send('Internal error');
  }

  res.status(200).send('OK');
});

// Handle pull_request events
async function handlePullRequest(payload) {
  if (payload.action !== 'closed' || !payload.pull_request?.merged) {
    if (payload.action === 'opened') {
      const pr = payload.pull_request;
      const embed = new EmbedBuilder()
        .setColor(COLORS.GITHUB_GREEN)
        .setTitle(`🔀 PR Opened: #${pr.number}`)
        .setDescription(`**${md(pr.title)}**`)
        .addFields(
          { name: 'Author', value: `@${md(pr.user.login)}`, inline: true },
          { name: 'Repo', value: md(payload.repository.full_name), inline: true }
        )
        .setURL(safeGithubUrl(pr.html_url) || undefined)
        .setTimestamp();
      await postToGitHubFeed(embed);
    }
    return;
  }

  const pr = payload.pull_request;
  const repo = payload.repository.full_name;
  const githubUsername = pr.user.login;
  const prNumber = pr.number;
  const prTitle = pr.title;
  const prUrl = pr.html_url;

  logger.info(`PR #${prNumber} merged by @${githubUsername} in ${repo}`);

  // Post to activity feed
  const mergeEmbed = new EmbedBuilder()
    .setColor(0x8957e5)
    .setTitle(`✅ PR Merged: #${prNumber}`)
    .setDescription(`**${md(prTitle)}**`)
    .addFields(
      { name: 'Author', value: `@${md(githubUsername)}`, inline: true },
      { name: 'Repo', value: md(repo), inline: true }
    )
    .setURL(safeGithubUrl(prUrl) || undefined)
    .setTimestamp();
  await postToGitHubFeed(mergeEmbed);

  const link = db.getLinkByGithub(githubUsername);

  if (link) {
    // Record the contribution BEFORE assignContributorRole — the role-assign
    // path reads getContributionCount, so if we called it first a first-time
    // contributor would still read a count of 0 and never get the role.
    // recordContribution returns false on transient DB error; if the row
    // didn't land we skip the role-assign + badge flow so the user gets a
    // consistent state (role assigned ↔ contribution recorded) instead of
    // a dangling role with no persisted credit.
    const recorded = db.recordContribution(link.discord_id, githubUsername, prNumber, repo, prTitle);
    if (!recorded) {
      logger.error('recordContribution failed — skipping role assign + badges', {
        discord_id: link.discord_id, githubUsername, prNumber, repo,
      });
      return;
    }
    const result = await assignContributorRole(link.discord_id, prNumber, repo, githubUsername);

    const newBadges = db.checkAndAwardBadges(link.discord_id, prTitle, repo);
    if (newBadges.length > 0) {
      await notifyBadgeEarned(link.discord_id, newBadges);
    }

    if (result.success) {
      logger.info(`Assigned Contributor role for PR #${prNumber}`);
    } else {
      logger.debug(`Role not assigned: ${result.reason}`);
    }
  } else if (!isAutomatedBot(pr.user)) {
    await notifyPRMerge(prNumber, repo, githubUsername, prTitle, prUrl);
  } else {
    logger.debug(`Skipping #general notification for automated bot: @${githubUsername}`);
  }
}

// Handle issues events
async function handleIssue(payload) {
  const issue = payload.issue;
  const repo = payload.repository.full_name;
  const labels = issue.labels?.map(l => l.name) || [];
  const githubUsername = issue.user?.login;

  if (payload.action === 'opened' || payload.action === 'labeled') {
    const isGoodFirstIssue = labels.some(l =>
      GOOD_FIRST_ISSUE_PATTERNS.some(pattern => l.toLowerCase().includes(pattern))
    );

    if (isGoodFirstIssue) {
      await postGoodFirstIssue(repo, issue.number, issue.title, issue.html_url, labels);
    }
  }

  if (payload.action === 'opened' && githubUsername) {
    const link = db.getLinkByGithub(githubUsername);
    if (link) {
      const awarded = db.awardFirstIssueBadge(link.discord_id);
      if (awarded.length > 0) {
        await notifyBadgeEarned(link.discord_id, awarded);
      }
    }
  }

  if (payload.action === 'opened') {
    const embed = new EmbedBuilder()
      .setColor(COLORS.GITHUB_GREEN)
      .setTitle(`📝 Issue Opened: #${issue.number}`)
      .setDescription(`**${md(issue.title)}**`)
      .addFields(
        { name: 'Author', value: `@${md(githubUsername || 'unknown')}`, inline: true },
        { name: 'Repo', value: md(repo), inline: true }
      )
      .setURL(safeGithubUrl(issue.html_url) || undefined)
      .setTimestamp();

    if (labels.length > 0) {
      embed.addFields({ name: 'Labels', value: labels.slice(0, 5).map(l => `\`${md(l)}\``).join(' '), inline: false });
    }

    await postToGitHubFeed(embed);
  }
}

// Handle release events
async function handleRelease(payload) {
  if (payload.action !== 'published') return;

  const release = payload.release;
  const repo = payload.repository.full_name;

  await postReleaseAnnouncement(repo, release.tag_name, release.name, release.html_url, release.body);

  const embed = new EmbedBuilder()
    .setColor(COLORS.PRIMARY)
    .setTitle(`🚀 Release: ${md(release.tag_name)}`)
    .setDescription(`**${md(release.name || release.tag_name)}**`)
    .addFields({ name: 'Repo', value: md(repo), inline: true })
    .setURL(safeGithubUrl(release.html_url) || undefined)
    .setTimestamp();
  await postToGitHubFeed(embed);
}

// Handle star events
async function handleStar(payload) {
  if (payload.action !== 'created') return;

  const repo = payload.repository.full_name;
  const stars = payload.repository.stargazers_count;
  const repoUrl = payload.repository.html_url;

  // Iterate in descending order to find the highest applicable milestone
  const milestonesDesc = [...config.STAR_MILESTONES].sort((a, b) => b - a);
  for (const milestone of milestonesDesc) {
    if (stars >= milestone && !db.hasMilestoneBeenAnnounced('stars', milestone, repo)) {
      if (db.recordMilestone('stars', milestone, repo)) {
        await postStarMilestone(repo, milestone, repoUrl);
        break;
      }
    }
  }
}

// Handle activity feed events
async function handleActivityFeed(event, payload) {
  const repo = payload.repository?.full_name;
  if (!repo) return;

  let embed;

  switch (event) {
    case 'push': {
      const commits = payload.commits || [];
      if (commits.length === 0) return;

      const branch = payload.ref?.replace('refs/heads/', '') || 'unknown';
      const pusher = payload.pusher?.name || 'Unknown';

      embed = new EmbedBuilder()
        .setColor(COLORS.GITHUB_PURPLE)
        .setTitle(`📤 ${commits.length} commit(s) pushed to ${md(branch)}`)
        .setDescription(
          commits.slice(0, 3).map(c => `• \`${c.id.substring(0, 7)}\` ${md(c.message.split('\n')[0].substring(0, 50))}`).join('\n') +
          (commits.length > 3 ? `\n... and ${commits.length - 3} more` : '')
        )
        .addFields(
          { name: 'Pusher', value: `@${md(pusher)}`, inline: true },
          { name: 'Repo', value: md(repo), inline: true }
        )
        .setTimestamp();
      break;
    }

    case 'create':
      if (payload.ref_type === 'branch') {
        embed = new EmbedBuilder()
          .setColor(COLORS.GITHUB_GREEN)
          .setTitle(`🌿 Branch Created: ${md(payload.ref)}`)
          .addFields({ name: 'Repo', value: md(repo), inline: true })
          .setTimestamp();
      } else if (payload.ref_type === 'tag') {
        embed = new EmbedBuilder()
          .setColor(COLORS.PRIMARY)
          .setTitle(`🏷️ Tag Created: ${md(payload.ref)}`)
          .addFields({ name: 'Repo', value: md(repo), inline: true })
          .setTimestamp();
      }
      break;

    case 'delete':
      if (payload.ref_type === 'branch') {
        embed = new EmbedBuilder()
          .setColor(COLORS.ERROR)
          .setTitle(`🗑️ Branch Deleted: ${md(payload.ref)}`)
          .addFields({ name: 'Repo', value: md(repo), inline: true })
          .setTimestamp();
      }
      break;
  }

  if (embed) {
    await postToGitHubFeed(embed);
  }
}

module.exports = router;
