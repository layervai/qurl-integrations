// GitHub OAuth routes
const express = require('express');
const config = require('../config');
const db = require('../database');
const logger = require('../logger');
const { renderPage } = require('../templates/page');
const { sendDM, assignContributorRole, notifyBadgeEarned } = require('../discord');

const router = express.Router();

// Check for historical contributions after linking
async function checkHistoricalContributions(discordId, githubUsername, accessToken) {
  const contributions = [];

  try {
    // Search for merged PRs by this user in allowed orgs
    for (const org of config.ALLOWED_GITHUB_ORGS) {
      const searchQuery = `type:pr author:${githubUsername} org:${org} is:merged`;
      const searchUrl = `https://api.github.com/search/issues?q=${encodeURIComponent(searchQuery)}&per_page=100`;

      const response = await fetch(searchUrl, {
        headers: {
          'Authorization': `Bearer ${accessToken}`,
          'Accept': 'application/vnd.github.v3+json',
          'User-Agent': 'OpenNHP-Bot',
        },
        signal: AbortSignal.timeout(30000),
      });

      if (!response.ok) {
        logger.warn(`Failed to search PRs for ${githubUsername} in ${org}`, { status: response.status });
        continue;
      }

      const data = await response.json();

      for (const pr of data.items || []) {
        // Extract repo name from repository_url
        const repoMatch = pr.repository_url?.match(/repos\/(.+)$/);
        const repo = repoMatch ? repoMatch[1] : 'unknown';

        contributions.push({
          prNumber: pr.number,
          repo: repo,
          title: pr.title,
          url: pr.html_url,
          mergedAt: pr.closed_at,
        });
      }
    }

    if (contributions.length === 0) {
      logger.info(`No historical contributions found for @${githubUsername}`);
      return { count: 0, newBadges: [] };
    }

    logger.info(`Found ${contributions.length} historical contribution(s) for @${githubUsername}`);

    // Record each contribution (db handles duplicates)
    let newCount = 0;
    for (const contrib of contributions) {
      const recorded = db.recordContribution(
        discordId,
        githubUsername,
        contrib.prNumber,
        contrib.repo,
        contrib.title
      );
      if (recorded) newCount++;
    }

    // Assign contributor role
    if (contributions.length > 0) {
      await assignContributorRole(
        discordId,
        contributions[0].prNumber,
        contributions[0].repo,
        githubUsername
      );
    }

    // Check for badges
    const newBadges = [];
    for (const contrib of contributions) {
      const badges = db.checkAndAwardBadges(discordId, contrib.title, contrib.repo);
      newBadges.push(...badges);
    }

    // Dedupe badges
    const uniqueBadges = [...new Set(newBadges)];

    if (uniqueBadges.length > 0) {
      await notifyBadgeEarned(discordId, uniqueBadges);
    }

    return { count: contributions.length, newCount, newBadges: uniqueBadges };

  } catch (error) {
    logger.error('Error checking historical contributions', { error: error.message, githubUsername });
    return { count: 0, newBadges: [], error: error.message };
  }
}

// Simple in-memory rate limiter
const rateLimitStore = new Map();

function rateLimit(req, res, next) {
  const ip = req.ip || req.headers['x-forwarded-for'] || 'unknown';
  const now = Date.now();
  const windowStart = now - config.RATE_LIMIT_WINDOW_MS;

  const requests = (rateLimitStore.get(ip) || []).filter(time => time > windowStart);

  if (requests.length >= config.RATE_LIMIT_MAX_REQUESTS) {
    logger.warn('Rate limit exceeded', { ip });
    return res.status(429).send(renderPage({
      title: 'Too Many Requests',
      icon: '⏳',
      heading: 'Slow Down!',
      message: 'You\'ve made too many requests. Please wait a moment and try again.',
      type: 'warning',
    }));
  }

  requests.push(now);
  rateLimitStore.set(ip, requests);
  next();
}

// Start GitHub OAuth flow
router.get('/github', rateLimit, (req, res) => {
  const { state } = req.query;

  if (!state) {
    return res.status(400).send(renderPage({
      title: 'Missing State',
      icon: '❌',
      heading: 'Missing State Parameter',
      message: 'This link is invalid. Please use the <strong>/link</strong> command in Discord to get a valid link.',
      type: 'error',
    }));
  }

  const pending = db.getPendingLink(state);
  if (!pending) {
    return res.status(400).send(renderPage({
      title: 'Link Expired',
      icon: '⏰',
      heading: 'Link Expired',
      message: 'This link has expired or was already used. Please use the <strong>/link</strong> command in Discord to get a new link.',
      subtext: `Links expire after ${config.PENDING_LINK_EXPIRY_MINUTES} minutes for security.`,
      type: 'warning',
    }));
  }

  const params = new URLSearchParams({
    client_id: config.GITHUB_CLIENT_ID,
    redirect_uri: `${config.BASE_URL}/auth/github/callback`,
    scope: 'read:user',
    state: state,
  });

  res.redirect(`https://github.com/login/oauth/authorize?${params}`);
});

// GitHub OAuth callback
router.get('/github/callback', rateLimit, async (req, res) => {
  const { code, state, error, error_description } = req.query;

  if (error) {
    logger.warn('GitHub OAuth denied', { error, error_description });
    return res.status(400).send(renderPage({
      title: 'Authorization Denied',
      icon: '🚫',
      heading: 'Authorization Denied',
      message: error_description || 'You denied the authorization request.',
      subtext: 'You can try again anytime with /link in Discord.',
      type: 'error',
    }));
  }

  if (!code || !state) {
    return res.status(400).send(renderPage({
      title: 'Invalid Request',
      icon: '❌',
      heading: 'Invalid Request',
      message: 'Missing required parameters. Please use <strong>/link</strong> in Discord to try again.',
      type: 'error',
    }));
  }

  const pending = db.getPendingLink(state);
  if (!pending) {
    return res.status(400).send(renderPage({
      title: 'Link Expired',
      icon: '⏰',
      heading: 'Session Expired',
      message: 'Your session has expired. Please use <strong>/link</strong> in Discord to start over.',
      subtext: `Sessions expire after ${config.PENDING_LINK_EXPIRY_MINUTES} minutes for security.`,
      type: 'warning',
    }));
  }

  try {
    // Exchange code for access token
    const tokenResponse = await fetch('https://github.com/login/oauth/access_token', {
      method: 'POST',
      headers: {
        'Accept': 'application/json',
        'Content-Type': 'application/json',
      },
      signal: AbortSignal.timeout(30000),
      body: JSON.stringify({
        client_id: config.GITHUB_CLIENT_ID,
        client_secret: config.GITHUB_CLIENT_SECRET,
        code: code,
      }),
    });

    const tokenData = await tokenResponse.json();

    if (tokenData.error) {
      logger.error('GitHub OAuth error', { error: tokenData.error_description });
      return res.status(400).send(renderPage({
        title: 'GitHub Error',
        icon: '❌',
        heading: 'GitHub Error',
        message: tokenData.error_description || 'An error occurred with GitHub authentication.',
        subtext: 'Please try again with /link in Discord.',
        type: 'error',
      }));
    }

    // Get user info
    const userResponse = await fetch('https://api.github.com/user', {
      headers: {
        'Authorization': `Bearer ${tokenData.access_token}`,
        'Accept': 'application/json',
        'User-Agent': 'OpenNHP-Bot',
      },
      signal: AbortSignal.timeout(30000),
    });

    const userData = await userResponse.json();

    if (!userData.login) {
      return res.status(400).send(renderPage({
        title: 'Failed',
        icon: '❌',
        heading: 'Failed to Get User Info',
        message: 'We couldn\'t retrieve your GitHub profile. Please try again.',
        type: 'error',
      }));
    }

    db.createLink(pending.discord_id, userData.login);
    db.deletePendingLink(state);

    logger.info(`Linked Discord ${pending.discord_id} to GitHub @${userData.login}`);

    // Check for historical contributions (async, don't block response)
    const historicalCheck = checkHistoricalContributions(
      pending.discord_id,
      userData.login,
      tokenData.access_token
    );

    // Send initial DM
    let dmMessage = `✅ **GitHub account linked!**\n\nYou're now linked to GitHub **@${userData.login}**.`;

    // Wait for historical check to complete for better UX
    let historical = { count: 0, newBadges: [] };
    try {
      historical = await historicalCheck;
    } catch (err) {
      logger.error('Historical contributions check failed', { error: err.message, discordId: pending.discord_id });
    }

    if (historical.count > 0) {
      dmMessage += `\n\n🎉 **Found ${historical.count} past contribution(s)!**\nYou've been credited for your previous merged PRs to OpenNHP repos.`;
      if (historical.newBadges?.length > 0) {
        dmMessage += `\n\n🏅 **Badges earned:** ${historical.newBadges.join(', ')}`;
      }
    } else {
      dmMessage += `\n\nWhen your PRs to OpenNHP repos are merged, you'll automatically receive the **@Contributor** role!`;
    }

    await sendDM(pending.discord_id, dmMessage);

    // Build response message
    let responseSubtext = 'When your PRs are merged, you\'ll automatically receive the @Contributor role!';
    if (historical.count > 0) {
      responseSubtext = `Found ${historical.count} past contribution(s)! You've been credited and assigned roles.`;
    }

    res.send(renderPage({
      title: 'Success',
      icon: '✅',
      heading: 'Linked Successfully!',
      message: `Your Discord account is now linked to GitHub @${userData.login}`,
      subtext: responseSubtext,
      type: 'success',
      showDiscordButton: true,
    }));

  } catch (error) {
    logger.error('OAuth callback error', { error: error.message });
    res.status(500).send(renderPage({
      title: 'Error',
      icon: '💥',
      heading: 'Something Went Wrong',
      message: 'An unexpected error occurred during authentication.',
      subtext: 'Please try again with /link in Discord. If the problem persists, contact a moderator.',
      type: 'error',
    }));
  }
});

module.exports = router;
