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
const { EXPIRY_PREFIX_PRESENT, EXPIRY_PREFIX_PAST } = require('./commands');

const SWEEP_INTERVAL_MS = 60 * 1000; // 1 minute
const BATCH = 50;

async function sweepOnce() {
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
        messageId: row.dm_message_id,
        error: err.message,
      });
    }
  }

  if (edited > 0 || permanentFails > 0) {
    logger.info('Expired-DM-label sweep complete', {
      processedRows: rows.length,
      uniqueMessages: seen.size,
      edited,
      permanentFails,
    });
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
