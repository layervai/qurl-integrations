// Background job: retry revocation of GitHub OAuth tokens that failed their
// initial `finally`-block revoke. Runs every hour. On success (or GitHub
// 404 "already revoked"), the row is deleted; on failure, it's left for the
// next sweep. cleanupOrphanedTokens in database.js purges anything older
// than 7 days regardless, so a GitHub API outage doesn't leak tokens
// forever — GitHub's own session TTL still applies on their side.

const crypto = require('crypto');
const config = require('./config');
const db = require('./database');
const logger = require('./logger');

const SWEEP_INTERVAL_MS = 60 * 60 * 1000; // 1 hour
const BATCH = 20;

async function revokeOne(accessToken) {
  const resp = await fetch(`https://api.github.com/applications/${config.GITHUB_CLIENT_ID}/token`, {
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
  return resp.ok || resp.status === 404;
}

async function sweepOnce() {
  if (!config.GITHUB_CLIENT_ID || !config.GITHUB_CLIENT_SECRET) return;
  let rows;
  try { rows = db.listOrphanedTokens(BATCH); } catch (err) {
    logger.error('Orphan token sweep: listOrphanedTokens failed', { error: err.message });
    return;
  }
  if (rows.length === 0) return;
  let revoked = 0;
  for (let i = 0; i < rows.length; i++) {
    const { id, accessToken } = rows[i];
    try {
      if (await revokeOne(accessToken)) {
        db.deleteOrphanedToken(id);
        revoked++;
      }
    } catch (err) {
      const tokenHash8 = crypto.createHash('sha256').update(accessToken || '').digest('hex').slice(0, 8);
      logger.warn('Orphan token retry-revoke failed (will retry next sweep)', {
        id, tokenHash8, error: err.message,
      });
    }
    // 100ms between calls — GitHub's secondary rate limit kicks in around
    // 100 requests/minute on the /applications/.../token endpoint, so 20
    // calls at 10/sec is well below. Also means one rejection doesn't
    // burn the whole batch in <200ms.
    if (i < rows.length - 1) await new Promise(r => setTimeout(r, 100));
  }
  if (revoked > 0) {
    logger.info(`Orphan token sweep: revoked ${revoked}/${rows.length}`);
  }
}

function startOrphanTokenSweeper() {
  // First sweep 5 min after boot so we don't hit GitHub during a cold start.
  const kick = setTimeout(() => {
    sweepOnce().catch(err => logger.error('Orphan sweep crash', { error: err.message }));
    const interval = setInterval(() => {
      sweepOnce().catch(err => logger.error('Orphan sweep crash', { error: err.message }));
    }, SWEEP_INTERVAL_MS);
    interval.unref();
  }, 5 * 60 * 1000);
  kick.unref();
}

module.exports = { startOrphanTokenSweeper, sweepOnce };
