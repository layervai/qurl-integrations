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

// Returns { ok, status } so the caller can distinguish retryable rate
// limits (429/403) from permanent failures (401/500) and apply backoff.
async function revokeOneDetailed(accessToken) {
  const resp = await fetch(`https://api.github.com/applications/${config.GITHUB_CLIENT_ID}/token`, {
    method: 'DELETE',
    headers: {
      'Authorization': 'Basic ' + Buffer.from(`${config.GITHUB_CLIENT_ID}:${config.GITHUB_CLIENT_SECRET}`).toString('base64'),
      'Accept': 'application/json',
      'Content-Type': 'application/json',
      'User-Agent': 'qurl-discord-bot/1.0',
    },
    body: JSON.stringify({ access_token: accessToken }),
    signal: AbortSignal.timeout(5000),
  });
  return { ok: resp.ok || resp.status === 404, status: resp.status };
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
  let backoffMs = 100;
  for (let i = 0; i < rows.length; i++) {
    const { id, encryptedAccessToken } = rows[i];
    // Decrypt inside the loop so only one plaintext token is in memory at
    // a time. Whole-batch decryption would widen the memory-dump window.
    let accessToken;
    try { accessToken = db.decryptOrphanedToken(encryptedAccessToken); } catch (err) {
      logger.warn('Orphan token decrypt failed (will retry next sweep)', { id, error: err.message });
      continue;
    }
    try {
      const result = await revokeOneDetailed(accessToken);
      if (result.ok) {
        db.deleteOrphanedToken(id);
        revoked++;
        backoffMs = 100; // reset on any success
      } else if (result.status === 429 || result.status === 403) {
        // Exponential backoff when GitHub secondary rate-limits us. Cap at
        // 60s so a single sweep never stalls beyond the interval window.
        // Abort the rest of the batch — we'll retry on the next hourly sweep.
        logger.warn('Orphan sweep hit GitHub rate limit, aborting batch', {
          id, status: result.status, backoffMs,
        });
        backoffMs = Math.min(backoffMs * 2, 60_000);
        await new Promise(r => setTimeout(r, backoffMs));
        accessToken = null;
        break;
      }
    } catch (err) {
      // Only log a real token hash. If accessToken was already nulled out
      // (e.g. rate-limit abort path above set it null before break), emit
      // a sentinel instead of a misleading "hash of empty string".
      const tokenHash8 = accessToken
        ? crypto.createHash('sha256').update(accessToken).digest('hex').slice(0, 8)
        : '(already-released)';
      logger.warn('Orphan token retry-revoke failed (will retry next sweep)', {
        id, tokenHash8, error: err.message,
      });
    } finally {
      // Shorten the plaintext memory window — the next iteration decrypts
      // a fresh token, so this one should not linger in scope under a
      // long sleep or error path.
      accessToken = null;
    }
    if (i < rows.length - 1) await new Promise(r => setTimeout(r, backoffMs));
  }
  if (revoked > 0) {
    logger.info(`Orphan token sweep: revoked ${revoked}/${rows.length}`);
  }
  // Log the remaining queue depth at error level so monitoring thresholds
  // can page oncall on a rising trend — accumulating orphans means GitHub
  // is persistently rejecting revokes, which is the signal we want alerts on.
  let remaining = 0;
  try { remaining = db.countOrphanedTokens(); } catch { /* non-fatal */ }
  if (remaining > 0) {
    logger.error('Orphan token queue has residual entries', {
      remaining, sweepProcessed: rows.length, sweepRevoked: revoked,
    });
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
