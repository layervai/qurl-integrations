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
const { EXPIRY_PREFIX_PRESENT, EXPIRY_PREFIX_PAST } = require('./constants');

const SWEEP_INTERVAL_MS = 60 * 1000; // 1 minute
const BATCH = 50;

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

    // Dedupe by dm_message_id so we issue one Discord API edit per unique
    // DM. Multiple links to the same recipient via /qurl add recipients
    // share a single message_id; we still want to mark all sibling rows
    // edited (markDMExpiredLabelEditedByMessageId handles that bulk update).
    const seen = new Set();
    let edited = 0;
    let permanentFails = 0;
    for (const row of rows) {
      if (seen.has(row.dm_message_id)) continue;
      seen.add(row.dm_message_id);

      try {
        const ok = await discord.editDMToPastTense(
          row.dm_channel_id,
          row.dm_message_id,
          EXPIRY_PREFIX_PRESENT,
          EXPIRY_PREFIX_PAST,
        );
        // ok === true  → edit landed (or was already past-tense). Mark all
        //                sibling rows so we don't re-attempt next sweep.
        // ok === false → permanent Discord failure (DM/channel gone, bot
        //                blocked). Same marker — there's nothing to retry.
        // throw        → transient. Don't mark; next sweep retries.
        db.markDMExpiredLabelEditedByMessageId(row.dm_message_id);
        if (ok) edited++;
        else permanentFails++;
      } catch (err) {
        logger.warn('Expired-DM-label edit failed (will retry next sweep)', {
          sendId: row.send_id,
          recipientId: row.recipient_discord_id,
          channelId: row.dm_channel_id,
          messageId: row.dm_message_id,
          error: err.message,
        });
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
        uniqueMessages: seen.size,
        edited,
        permanentFails,
      });
    } else if (edited > 0) {
      logger.info('Expired-DM-label sweep complete', {
        processedRows: rows.length,
        uniqueMessages: seen.size,
        edited,
      });
    }
  } finally {
    sweeping = false;
  }
}

function startExpiredDMLabelSweeper() {
  // First sweep 30s after boot — short delay so a bot restart still
  // closes the tense gap quickly without colliding with Discord login
  // and the initial guild-cache hydration.
  const kick = setTimeout(() => {
    sweepOnce().catch(err => logger.error('Expired-DM-label sweep crash', { error: err.message }));
    const interval = setInterval(() => {
      sweepOnce().catch(err => logger.error('Expired-DM-label sweep crash', { error: err.message }));
    }, SWEEP_INTERVAL_MS);
    interval.unref();
  }, 30 * 1000);
  kick.unref();
}

module.exports = { startExpiredDMLabelSweeper, sweepOnce };
