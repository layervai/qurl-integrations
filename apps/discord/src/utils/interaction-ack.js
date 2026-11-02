const logger = require('../logger');

// A component interaction owns its flow mutation only after Discord accepts
// its acknowledgement. SQS delivery and Gateway reconnects are at-least-once:
// a duplicate interaction loses the one-shot deferUpdate race with Discord.
// Continuing after that failure lets the unacknowledged copy mutate DDB, then
// makes the acknowledged copy lose OCC and replace a valid card with the
// "superseded" error. Stop at the acknowledgement boundary for every remote
// or unknown error, not only Discord 10062/40060 responses: a transport failure
// has an unknown outcome, so continuing could let an unacknowledged copy mutate
// state. The one exception is discord.js's local InteractionAlreadyReplied
// error when this same interaction object records that it already acknowledged.
// If the sole delivery has a transport failure, Discord keeps its retry/toast
// UX and the user can click again. Preserving state is the safer outcome.
async function deferUpdateOrStop(interaction, flowId) {
  try {
    await interaction.deferUpdate();
    return true;
  } catch (err) {
    // discord.js throws this locally when this interaction object already
    // completed an acknowledgement. That means this copy still owns the
    // action. Reconstructed duplicate deliveries use separate objects whose
    // flags start false, so their rejected REST acknowledgement still stops.
    if (err?.code === 'InteractionAlreadyReplied'
        && (interaction.deferred || interaction.replied)) {
      logger.debug('Interaction was already acknowledged by this handler; continuing', {
        flow_id: flowId,
        custom_id: interaction.customId ?? null,
      });
      return true;
    }
    logger.warn('Interaction acknowledgement failed — stopping before state change', {
      flow_id: flowId,
      custom_id: interaction.customId ?? null,
      error_code: err?.code ?? err?.rawError?.code ?? null,
      status_code: err?.status ?? null,
      error: err?.message,
    });
    return false;
  }
}

module.exports = { deferUpdateOrStop };
