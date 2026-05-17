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
// Handlers register their customId at load-time (see registerFlow
// callsites in commands.js + future flow modules). Routing-table-as-
// single-source-of-truth means a `handlers.has(customId)` check is
// the only thing standing between an unknown customId and a silent
// drop. Match is EXACT — `handlers.get(customId)`, not startsWith.
// If a future flow needs a nonce'd suffix (e.g. `setup_modal:<id>`),
// extend the lookup here; do not let callers pass partial routing
// keys.
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

// stage → user-visible message map. Populated alongside the routing
// table by `registerFlow`'s optional `siblingMessage`. Each entry
// describes the REGISTERING handler's own stage — i.e. "if a caller
// found a surviving row at this stage, here is the actionable thing
// to tell the user." Co-locating this with the routing registration
// closes the forgot-to-add-an-entry footgun that an external
// SIBLING_FLOW_MESSAGES map (the pre-#274 shape) carried.
const siblingMessages = new Map();

// Register a flow handler. `customId` is matched exactly against
// `interaction.customId` (not startsWith — see module header).
// `expectedStage` is what `loadFlow(flow_id).stage` must equal for
// the handler to fire; mismatches yield a "superseded" reply.
//
// `siblingMessage` (optional) is the user-visible string a DIFFERENT
// flow's supersede peek should surface when it finds a surviving
// row at THIS stage. Co-located with the routing registration so
// adding a new two-stage flow doesn't require updating a parallel
// map (the pre-#274 shape had that map in commands.js, easy to
// miss when adding a new flow). When omitted, peers that find a
// row at this stage fall through to their generic "could not start"
// wording — appropriate for stages that are too short-lived or too
// sibling-irrelevant to merit a dedicated message.
//
// Throws on duplicate registration — silently overwriting a handler
// would mask a real bug (two modules claiming the same customId).
function registerFlow(customId, { expectedStage, handler, siblingMessage }) {
  if (typeof customId !== 'string' || customId.length === 0) {
    throw new TypeError('flow-dispatch.registerFlow: customId must be a non-empty string');
  }
  if (typeof expectedStage !== 'string' || expectedStage.length === 0) {
    throw new TypeError('flow-dispatch.registerFlow: expectedStage must be a non-empty string');
  }
  if (typeof handler !== 'function') {
    throw new TypeError('flow-dispatch.registerFlow: handler must be a function');
  }
  if (siblingMessage !== undefined && (typeof siblingMessage !== 'string' || siblingMessage.length === 0)) {
    throw new TypeError('flow-dispatch.registerFlow: siblingMessage must be a non-empty string when provided');
  }
  if (handlers.has(customId)) {
    throw new Error(`flow-dispatch.registerFlow: customId ${JSON.stringify(customId)} is already registered`);
  }
  handlers.set(customId, { expectedStage, handler });
  // The same `expectedStage` could be registered by multiple
  // customIds (e.g. a flow with parallel input components both at
  // the same stage). Last-wins is fine because the message
  // describes the stage, not the customId — but a TYPE mismatch
  // (one registration sets siblingMessage, another omits it)
  // would silently flip the lookup behavior depending on
  // registration order. Reject inconsistency upfront.
  if (siblingMessage !== undefined) {
    const existing = siblingMessages.get(expectedStage);
    if (existing !== undefined && existing !== siblingMessage) {
      throw new Error(`flow-dispatch.registerFlow: stage ${JSON.stringify(expectedStage)} already has a different siblingMessage registered`);
    }
    siblingMessages.set(expectedStage, siblingMessage);
  }
}

// Look up the actionable user-visible message for a stage. Returns
// null when no flow registered a siblingMessage for that stage —
// the caller (a different flow's supersede peek) should then fall
// through to generic "could not start" wording.
//
// Pure reverse-lookup over the registry — there is no module-level
// state to maintain across registerFlow calls beyond what
// registerFlow itself wrote.
function siblingMessageForStage(stage) {
  if (typeof stage !== 'string' || stage.length === 0) return null;
  return siblingMessages.get(stage) ?? null;
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
    await supersededRoutingFailureReply(interaction);
    return;
  }

  let row;
  try {
    row = await loadFlow(flow_id);
  } catch (err) {
    logger.error('flow-dispatch: loadFlow failed', {
      customId, flow_id, error: err.message,
    });
    await supersededRoutingFailureReply(interaction);
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
    await supersededRoutingFailureReply(interaction);
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

// Routing-failure reply: the dispatcher has decided the flow row is
// gone or in the wrong stage and the registered handler will NOT run.
// For MessageComponent interactions, replace the stale card via
// `update` instead of sending a fresh ephemeral — `interaction.reply`
// would leave the source card's buttons live, so each repeated click
// (e.g. Cancel on a confirm card whose flow row TTL'd out) stacks
// another ephemeral with a reply-quote of the source card. Editing
// the source message also clears the buttons so the user can't keep
// firing dead-flow interactions.
//
// Falls back to `safeReply` (ephemeral reply / followUp) for:
//   - Modal submits — the `update` API on a ModalSubmit only edits
//     the source message when the modal was opened from a component
//     click, and even then the modal's submit ack model is divergent
//     enough that the simpler ephemeral reply is the right shape.
//   - Already-acked interactions — defend against a future reorder
//     where this helper is reached after a handler-internal defer.
//   - `update` failures — most commonly Unknown Message (10008) if
//     the user dismissed the source ephemeral between rendering and
//     clicking; the interaction token is still live so a fresh reply
//     is the right recovery.
async function supersededRoutingFailureReply(interaction) {
  // `isMessageComponent` is guaranteed to exist — `index.js` only
  // routes here when `isMessageComponent() || isModalSubmit()` is
  // true (interactionCreate listener), and discord.js declares both
  // methods on the base Interaction class.
  if (interaction.isMessageComponent() && !interaction.replied && !interaction.deferred) {
    try {
      await interaction.update({ content: SUPERSEDED_MSG, components: [] });
      return;
    } catch (err) {
      logger.warn('flow-dispatch: update failed on routing failure, falling back to reply', {
        error: err.message,
      });
      // Fall through to safeReply. `update` failures leave the
      // interaction unacked (Discord rejects the update without
      // consuming the token on Unknown Message), so the followup
      // reply still has a valid token to spend.
    }
  }
  await safeReply(interaction, SUPERSEDED_MSG);
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
  siblingMessageForStage,
  flowIdForInteraction,
  // Best-effort reply helper that picks followUp vs reply based on
  // interaction.replied/deferred state. Part of the handler-author
  // contract (not just dispatch-internal): handlers reaching a reply
  // point with ambiguous ack state — e.g. `showModal` that may have
  // partially acked before throwing — should call safeReply instead
  // of `interaction.reply().catch(...)` to avoid silently swallowing
  // an InteractionAlreadyReplied. See handleSetupButton's rollback
  // path for the canonical consumer.
  safeReply,
  handleFlowInteraction,
  // Exported for tests so they can assert the supersede wording
  // without duplicating the string.
  SUPERSEDED_MSG,
};
