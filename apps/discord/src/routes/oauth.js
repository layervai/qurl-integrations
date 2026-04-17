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
    // Search for merged PRs by this user in allowed orgs. GitHub's search API
    // returns at most 100 per page and caps `total_count` at 1000; paginate up
    // to 5 pages (500 PRs) per org so prolific contributors don't have
    // badges/contributions silently truncated.
    const MAX_PAGES = 5;
    for (const org of config.ALLOWED_GITHUB_ORGS) {
      for (let page = 1; page <= MAX_PAGES; page++) {
        const searchQuery = `type:pr author:${githubUsername} org:${org} is:merged`;
        const searchUrl = `https://api.github.com/search/issues?q=${encodeURIComponent(searchQuery)}&per_page=100&page=${page}`;

        const response = await fetch(searchUrl, {
          headers: {
            'Authorization': `Bearer ${accessToken}`,
            'Accept': 'application/vnd.github.v3+json',
            'User-Agent': 'OpenNHP-Bot',
          },
          signal: AbortSignal.timeout(30000),
        });

        if (!response.ok) {
          const remaining = parseInt(response.headers.get('x-ratelimit-remaining') || '-1', 10);
          const retryAfter = response.headers.get('retry-after');
          logger.warn(`Failed to search PRs for ${githubUsername} in ${org} page ${page}`, {
            status: response.status, remaining, retryAfter,
          });
          if (response.status === 403 || response.status === 429 || remaining === 0) {
            return {
              count: contributions.length,
              newBadges: [],
              error: `GitHub rate limit hit (status ${response.status}); historical results incomplete`,
            };
          }
          break; // stop paginating this org, move on to the next
        }

        const data = await response.json();
        const items = data.items || [];

        for (const pr of items) {
          const repoMatch = pr.repository_url?.match(/repos\/(.+)$/);
          const repo = repoMatch ? repoMatch[1] : 'unknown';
          contributions.push({
            prNumber: pr.number,
            repo,
            title: pr.title,
            url: pr.html_url,
            mergedAt: pr.closed_at,
          });
        }
        // Stop paginating when the page is partial — nothing more to fetch.
        if (items.length < 100) break;
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

    // Check for badges. Aggregate badges (count/streak/unique-repo) only
    // need to be checked once; title-based badges (Docs Hero, Bug Hunter)
    // short-circuit on the first match via hasBadge(), so iterate distinct
    // (title, repo) pairs instead of every contribution to avoid N*4 queries.
    const newBadges = [];
    const seenTitleKeys = new Set();
    for (const contrib of contributions) {
      const key = `${contrib.title}\x00${contrib.repo}`;
      if (seenTitleKeys.has(key)) continue;
      seenTitleKeys.add(key);
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

// Simple in-memory rate limiter with periodic eviction
const rateLimitStore = new Map();

// Node.js single-threaded: no true data race, but eviction could theoretically
// interleave with a request's filter→set. Acceptable for in-memory rate limiting.

// Evict stale entries every 5 minutes to prevent unbounded growth
setInterval(() => {
  const cutoff = Date.now() - config.RATE_LIMIT_WINDOW_MS * 2;
  for (const [ip, requests] of rateLimitStore) {
    const recent = requests.filter(t => t > cutoff);
    if (recent.length === 0) rateLimitStore.delete(ip);
    else rateLimitStore.set(ip, recent);
  }
}, 5 * 60 * 1000).unref();

// Absolute cap on how many timestamps we keep per IP so an abusive IP can't
// grow its array unboundedly between eviction sweeps.
const MAX_REQUESTS_PER_IP = Math.max(config.RATE_LIMIT_MAX_REQUESTS * 4, 100);

function rateLimit(req, res, next) {
  const ip = req.ip || 'unknown'; // req.ip uses x-forwarded-for via 'trust proxy' (server.js)
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
  // Trim the per-IP array to MAX_REQUESTS_PER_IP so one IP can't accumulate
  // thousands of timestamps between sweeps. Keep the most recent entries.
  if (requests.length > MAX_REQUESTS_PER_IP) {
    requests.splice(0, requests.length - MAX_REQUESTS_PER_IP);
  }
  rateLimitStore.set(ip, requests);
  if (rateLimitStore.size >= 10000) {
    const oldest = rateLimitStore.keys().next().value;
    rateLimitStore.delete(oldest);
  }
  next();
}

// Start GitHub OAuth flow
router.get('/github', rateLimit, (req, res) => {
  const { state } = req.query;

  if (!state || !/^[a-f0-9]{32}$/.test(state)) {
    return res.status(400).send(renderPage({
      title: 'Invalid Link',
      icon: '❌',
      heading: 'Invalid Link',
      message: 'This link is invalid. Please use the /link command in Discord to get a valid link.',
      type: 'error',
    }));
  }

  const pending = db.getPendingLink(state);
  if (!pending) {
    return res.status(400).send(renderPage({
      title: 'Link Expired',
      icon: '⏰',
      heading: 'Link Expired',
      message: 'This link has expired or was already used. Please use the /link command in Discord to get a new link.',
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
      // Cap to 200 chars: escapeHtml already prevents XSS, but an attacker
      // could craft a very long error_description to push phishing text.
      message: (error_description || 'You denied the authorization request.').slice(0, 200),
      subtext: 'You can try again anytime with /link in Discord.',
      type: 'error',
    }));
  }

  if (!code || !state || !/^[a-f0-9]{32}$/.test(state)) {
    return res.status(400).send(renderPage({
      title: 'Invalid Request',
      icon: '❌',
      heading: 'Invalid Request',
      message: 'Missing required parameters. Please use /link in Discord to try again.',
      type: 'error',
    }));
  }

  // Atomic DELETE ... RETURNING closes the TOCTOU window: a second concurrent
  // request with the same state gets no row back and is rejected.
  const pending = db.consumePendingLink(state);
  if (!pending) {
    return res.status(400).send(renderPage({
      title: 'Link Expired',
      icon: '⏰',
      heading: 'Session Expired',
      message: 'Your session has expired. Please use /link in Discord to start over.',
      subtext: `Sessions expire after ${config.PENDING_LINK_EXPIRY_MINUTES} minutes for security.`,
      type: 'warning',
    }));
  }

  let accessToken = null;
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
    accessToken = tokenData.access_token || null;

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
    // deletePendingLink already called above (TOCTOU prevention)

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
  } finally {
    // Revoke the GitHub OAuth token. Await the call and retry once with
    // backoff; leaving a read:user token alive on GitHub would be a real
    // credential leak.
    if (accessToken) {
      const revokeOnce = () => fetch(`https://api.github.com/applications/${config.GITHUB_CLIENT_ID}/token`, {
        method: 'DELETE',
        headers: {
          'Authorization': 'Basic ' + Buffer.from(`${config.GITHUB_CLIENT_ID}:${config.GITHUB_CLIENT_SECRET}`).toString('base64'),
          'Accept': 'application/json',
          'Content-Type': 'application/json',
          'User-Agent': 'OpenNHP-Bot',
        },
        body: JSON.stringify({ access_token: accessToken }),
        signal: AbortSignal.timeout(5000),
      });
      let lastErr = null;
      for (let attempt = 0; attempt < 2; attempt++) {
        try {
          const resp = await revokeOnce();
          if (resp.ok || resp.status === 404) { lastErr = null; break; }
          lastErr = new Error(`revoke returned ${resp.status}`);
        } catch (err) {
          lastErr = err;
        }
        if (attempt === 0) await new Promise(r => setTimeout(r, 1000));
      }
      if (lastErr) {
        // Page oncall: the user's GitHub token is orphaned-alive after 2 attempts.
        logger.error('Failed to revoke GitHub OAuth token after retries', { error: lastErr.message });
      }
    }
  }
});

module.exports = router;
