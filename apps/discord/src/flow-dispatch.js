// flow-dispatch — routes Discord component / modal interactions to
// DDB-backed flow handlers (see docs/zero-downtime-design.md,
// Pillar 1).
//
// Component / modal interactions arrive on the same `interactionCreate`
// event as slash commands but follow an entirely different lifecycle:
// they're keyed off a customId rather than a command name, and they
// must resolve a DDB flow row before acting. Branching inside
// handleCommand would mix two state machines (slash dispatch vs.
// flow-resume) under one error-handling envelope. Split modules
// keep each surface single-purpose.
//
// Handlers register their prefix at load-time (see registerFlow
// callsites in commands.js + future flow modules). Routing-table-as-
// single-source-of-truth means a `handlers.has(prefix)` check is the
// only thing standing between an unknown customId and a silent drop.
//
// Trust model for customId routing:
//
//   The customId on a component event is REPLAYABLE CLIENT INPUT —
//   Discord puts whatever was set on the original message into the
//   interaction payload verbatim. It is fine to use as a routing key
//   (the worst case is a typo'd prefix → no match → silent drop, which
//   is harmless), but it MUST NOT be used as an identity or
//   authorization signal. The flow_id is derived ENTIRELY from the
//   trusted interaction context (user.id + channelId + shard), not from
//   anything inside customId. A user can't spoof another user's flow
//   by editing a customId.

const { buildFlowId } = require('./flow-id');
const { loadFlow } = require('./flow-state');
const { SHARD_ID } = require('./config');
const logger = require('./logger');

// Module-state routing table. Registered at the bottom of commands.js
// (and future setup/send handler modules) via `registerFlow`. Module-
// state is fine here: the registration runs once at require-time, and
// the table is read-only after boot. No concurrency hazard.
const handlers = new Map();

// Register a flow handler. `prefix` is matched against the full
// customId (exact match, not startsWith — see customId rationale at
// callsite). `expectedStage` is what `loadFlow(flow_id).stage` must
// equal for the handler to fire; mismatches yield a "superseded"
// user-visible reply.
//
// Throws on duplicate registration — silently overwriting a handler
// would mask a real bug (two modules claiming the same customId).
function registerFlow(prefix, { expectedStage, handler }) {
  if (typeof prefix !== 'string' || prefix.length === 0) {
    throw new TypeError('flow-dispatch.registerFlow: prefix must be a non-empty string');
  }
  if (typeof expectedStage !== 'string' || expectedStage.length === 0) {
    throw new TypeError('flow-dispatch.registerFlow: expectedStage must be a non-empty string');
  }
  if (typeof handler !== 'function') {
    throw new TypeError('flow-dispatch.registerFlow: handler must be a function');
  }
  if (handlers.has(prefix)) {
    throw new Error(`flow-dispatch.registerFlow: prefix ${JSON.stringify(prefix)} is already registered`);
  }
  handlers.set(prefix, { expectedStage, handler });
}

// Derive the canonical flow_id for an interaction from its trusted
// context. Used by BOTH sides of every flow: the command handler
// (creating the flow row) and the dispatcher (loading the flow row).
// Single source of truth — if these two callsites computed flow_id
// differently the OCC contract would silently fail.
//
// DM context: interaction.guildId is null. Namespace DM flows under
// `dm:<user_id>` so they can't collide with a real guild snowflake
// (which are pure numerics — the `dm:` prefix is unambiguous). The
// channel_id is still the DM channel ID, so two parallel DM flows for
// the same user in different channels (theoretically possible if the
// user opens a group DM) get distinct flow_ids.
function flowIdForInteraction(interaction) {
  const guild_id = interaction.guildId ?? `dm:${interaction.user.id}`;
  return buildFlowId({
    shard_id: SHARD_ID,
    guild_id,
    channel_id: interaction.channelId,
    user_id: interaction.user.id,
  });
}

// User-visible reply when a component fires but its flow row is
// missing (TTL'd, superseded, or never created). Phrased to be
// recoverable — the user just runs the command again. Kept as a
// module-level constant so the dispatcher test can assert on it
// without re-stating wording.
const SUPERSEDED_MSG = 'This action was superseded — run the command again.';

// Entry point — wired from index.js's interactionCreate listener.
// Contract: returns a Promise that resolves once the dispatch is
// complete (or short-circuits for unknown customId). Throws are NOT
// expected to propagate — every internal failure path either replies
// to the user or logs and exits. The caller (index.js listener) does
// not have a meaningful retry path.
async function handleFlowInteraction(interaction) {
  const customId = interaction.customId;
  if (typeof customId !== 'string' || customId.length === 0) {
    // Discord ought to always send a customId for component/modal
    // events, but guard so a malformed payload doesn't crash the
    // process. Quiet drop — the user would see Discord's own
    // "interaction failed" within 3 s.
    return;
  }

  const route = handlers.get(customId);
  if (!route) {
    // Unknown customId — most likely a stale component from a previous
    // deploy that used a different prefix scheme. Silent drop; the
    // user's interaction will time out client-side with Discord's
    // generic "interaction failed" notice.
    logger.debug('flow-dispatch: unknown customId, dropping', { customId });
    return;
  }

  let flow_id;
  try {
    flow_id = flowIdForInteraction(interaction);
  } catch (err) {
    // flowIdForInteraction calls buildFlowId, which throws on missing/
    // malformed components. Shouldn't happen for real Discord events,
    // but log + reply rather than letting the throw escape.
    logger.warn('flow-dispatch: failed to derive flow_id from interaction', {
      customId, error: err.message,
    });
    await safeReply(interaction, SUPERSEDED_MSG);
    return;
  }

  let row;
  try {
    row = await loadFlow(flow_id);
  } catch (err) {
    logger.error('flow-dispatch: loadFlow failed', {
      customId, flow_id, error: err.message,
    });
    await safeReply(interaction, SUPERSEDED_MSG);
    return;
  }

  if (!row || row.stage !== route.expectedStage) {
    // Either the flow is gone (TTL'd, deleted, never existed) or it
    // advanced to a stage that doesn't match this customId. Both
    // collapse to the same user-visible recovery ("run again"); the
    // distinction matters only for forensic logs.
    logger.debug('flow-dispatch: flow not in expected stage', {
      customId, flow_id,
      stage: row ? row.stage : null,
      expected_stage: route.expectedStage,
    });
    await safeReply(interaction, SUPERSEDED_MSG);
    return;
  }

  // Universal safety net. A handler may have ALREADY committed an
  // irreversible flow_state side effect (deleteFlow-first ordering,
  // a transitionFlow on an OCC path) before throwing — letting the
  // throw escape would surface as a process-level unhandledRejection
  // with no user-visible reply, making the action look silently
  // broken. Catch here so every handler shares the same safety net
  // rather than re-implementing it.
  //
  // `row` is passed alongside `flow_id` so handlers that need
  // `row.version` for `transitionFlow`'s OCC parameter don't have
  // to re-issue loadFlow. handleRevokeSelect ignores it today
  // (single-shot stage); PR 6's setup-modal handler will consume it.
  try {
    await route.handler(interaction, { flow_id, row });
  } catch (err) {
    logger.error('flow-dispatch: handler threw', {
      customId, flow_id, error: err.message, stack: err.stack,
    });
    await safeReply(
      interaction,
      'Something went wrong — please run the command again.',
    );
  }
}

// Best-effort reply that swallows Discord errors. The dispatcher may
// arrive at the reply path AFTER another code path (a Discord retry,
// say) already acked — `interaction.reply` would throw
// `InteractionAlreadyReplied` then. Logging at warn is enough; there
// is no recovery.
async function safeReply(interaction, content) {
  try {
    if (interaction.replied || interaction.deferred) {
      await interaction.followUp({ content, ephemeral: true });
    } else {
      await interaction.reply({ content, ephemeral: true });
    }
  } catch (err) {
    logger.warn('flow-dispatch: reply failed', { error: err.message });
  }
}

module.exports = {
  registerFlow,
  flowIdForInteraction,
  handleFlowInteraction,
  // Exported for tests so they can assert the supersede wording
  // without duplicating the string.
  SUPERSEDED_MSG,
};
