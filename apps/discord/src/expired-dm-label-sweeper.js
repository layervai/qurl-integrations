// Background job: edit recipient DMs from "Portal closes <t:N:R>" to
// "Portal closed <t:N:R>" once expires_at_unix has passed. Discord's
// <t:N:R> markdown auto-renders "5 hours ago" client-side, but the
// surrounding literal verb stays present-tense unless we rewrite the
// embed. The sweep finds qurl_sends rows where the link expired AND we
// haven't already past-tense-edited that DM, then issues a single edit
// per unique dm_message_id (one consolidated DM can carry multiple
// links / multiple qurl_sends rows — one edit settles them all via
// markDMExpiredLabelEditedByMessageId).
//
// 1-minute sweep interval is the worst-case latency a recipient sees
// between "in 0 seconds" and "Portal closed N seconds ago" — within the
// natural rounding boundary of Discord's relative-time renderer (which
// shows "now" / "1 minute ago" buckets), so a tighter interval wouldn't
// improve the user-visible experience.

const db = require('./database');
const logger = require('./logger');
const discord = require('./discord');

const SWEEP_INTERVAL_MS = 60 * 1000; // 1 minute
const BATCH = 50;
// Edit concurrency. Discord per-channel/user edit rate limits are
// generous (5+ req/s sustained); CONCURRENCY=5 drains a 50-row backlog
// in ~3s instead of ~15s sequential, while leaving headroom under the
// global rate limit. Mirrors `batchSettled` in commands.js without
// importing it (keeps the sweeper self-contained).
const CONCURRENCY = 5;

// Re-entrancy guard. With a 60s interval and batch=50 sequential Discord
// edits per sweep, a Discord 5xx outage can stretch a sweep past 60s; the
// next setInterval tick would otherwise launch a second sweep that fetches
// the same rows (neither has marked anything yet) and double-edits each
// message. Idempotent (the second edit no-ops on already-past-tense embeds)
// but doubles API traffic and log noise. Module-scoped flag wins because
// `setInterval` doesn't await its callback.
let sweeping = false;

async function sweepOnce() {
  if (sweeping) {
    // A prior sweep is still in flight (Discord slowness, large backlog).
    // Skip this tick — the in-flight sweep will eventually mark its rows
    // edited; the next tick after it finishes will pick up anything new.
    logger.warn('Expired-DM-label sweep skipped (prior sweep still in flight)');
    return;
  }
  sweeping = true;
  try {
    let rows;
    try {
      rows = db.listExpiredUneditedDMs(BATCH);
    } catch (err) {
      logger.error('Expired-DM-label sweep: list failed', { error: err.message });
      return;
    }
    if (rows.length === 0) return;

    // Dedupe by dm_message_id BEFORE edit so a single consolidated DM
    // (from /qurl add recipients carrying multiple links → multiple
    // qurl_sends rows) gets exactly one Discord API edit. The bulk-mark
    // by message_id below handles all sibling rows in one UPDATE.
    // Computed up-front so the parallel batch below doesn't race the
    // dedup Set.
    const uniqueByMessageId = new Map();
    for (const row of rows) {
      if (!uniqueByMessageId.has(row.dm_message_id)) {
        uniqueByMessageId.set(row.dm_message_id, row);
      }
    }
    const uniqueRows = [...uniqueByMessageId.values()];

    let edited = 0;
    let permanentFails = 0;
    for (let i = 0; i < uniqueRows.length; i += CONCURRENCY) {
      const batch = uniqueRows.slice(i, i + CONCURRENCY);
      // Catch the mark separately from the edit so the warn log can
      // distinguish "edit failed" from "edit succeeded but DB mark
      // failed" — the latter is self-healing (next sweep finds the
      // embed already past-tense and idempotently re-marks via the
      // already-past branch in editDMToPastTense), but lumping it into
      // the same log line as a real edit failure inflates the
      // apparent edit-failure rate on dashboards.
      const results = await Promise.allSettled(batch.map(async (row) => {
        const ok = await discord.editDMToPastTense(row.dm_channel_id, row.dm_message_id);
        try {
          db.markDMExpiredLabelEditedByMessageId(row.dm_message_id);
        } catch (markErr) {
          logger.warn('Expired-DM-label edit succeeded but DB mark failed (will be re-swept and idempotently marked)', {
            sendId: row.send_id,
            recipientId: row.recipient_discord_id,
            channelId: row.dm_channel_id,
            messageId: row.dm_message_id,
            error: markErr.message,
          });
          // Still return `ok` — the Discord edit DID land. Next sweep
          // finds the embed already past-tense (alreadyPast branch)
          // and re-marks. Counting this as edited keeps dashboard
          // numbers accurate; the warn above is the operational signal.
        }
        return ok;
      }));
      for (let j = 0; j < results.length; j++) {
        const r = results[j];
        const row = batch[j];
        if (r.status === 'fulfilled') {
          if (r.value) edited++;
          else permanentFails++;
        } else {
          // Only the editDMToPastTense throw reaches this branch — the
          // mark-failure path above catches its own error and returns ok.
          logger.warn('Expired-DM-label edit failed (will retry next sweep)', {
            sendId: row.send_id,
            recipientId: row.recipient_discord_id,
            channelId: row.dm_channel_id,
            messageId: row.dm_message_id,
            error: r.reason?.message ?? String(r.reason),
          });
        }
      }
    }

    // Hitting the BATCH ceiling means there are more expired-but-unedited
    // rows than one sweep can process — backlog likely from Discord
    // outage, bot downtime, or a sudden spike in /qurl send. Log warn so
    // dashboards can alert if the saturation persists across sweeps.
    if (rows.length === BATCH) {
      logger.warn('Expired-DM-label sweep hit batch ceiling — backlog likely', {
        processedRows: rows.length,
        batch: BATCH,
      });
    }

    // Permanent failures (DM gone, bot blocked) shouldn't fold into the
    // happy-path info line — a sudden spike in `permanentFails` is a real
    // operational signal (e.g. a region-wide Discord auth bug, or an embed
    // shape regression making editDMToPastTense return false-positive).
    // Separate warn keeps the signal scannable in log dashboards.
    if (permanentFails > 0) {
      logger.warn('Expired-DM-label sweep recorded permanent failures', {
        processedRows: rows.length,
        uniqueMessages: uniqueRows.length,
        edited,
        permanentFails,
      });
    } else if (edited > 0) {
      logger.info('Expired-DM-label sweep complete', {
        processedRows: rows.length,
        uniqueMessages: uniqueRows.length,
        edited,
      });
    }
  } finally {
    sweeping = false;
  }
}

function startExpiredDMLabelSweeper() {
  // First sweep 60s after boot — covers the worst case where
  // `client.login(...)` itself burns its full 30s deadline before the
  // Discord client is ready. A 30s kick that races a 30s login window
  // would have the first sweep fire against an unready client; the
  // edits would throw and re-sweep next minute (self-healing) but it's
  // a footgun. 60s clears the login deadline + leaves headroom for
  // initial guild-cache hydration. Cost: at most 30s of "Portal closes
  // X seconds ago" tense lag on the very first sweep after boot.
  //
  // Idempotency note: if `editDMToPastTense` lands the Discord edit but
  // `markDMExpiredLabelEditedByMessageId` then throws (DB transient),
  // the row stays unmarked → re-swept → editDMToPastTense finds the
  // embed already past-tense → returns true silently → marked. The
  // already-past branch in editDMToPastTense is the explicit safety
  // net for this gap.
  const kick = setTimeout(() => {
    sweepOnce().catch(err => logger.error('Expired-DM-label sweep crash', { error: err.message }));
    const interval = setInterval(() => {
      sweepOnce().catch(err => logger.error('Expired-DM-label sweep crash', { error: err.message }));
    }, SWEEP_INTERVAL_MS);
    interval.unref();
  }, 60 * 1000);
  kick.unref();
}

module.exports = { startExpiredDMLabelSweeper, sweepOnce };
