const sendSlash = require('./qurl-send-slash');
const {
  SELF_DESTRUCT_NO_TIMER_CHOICE,
  CONFIRM_USER_SELECT_CUSTOM_ID,
  SEND_STAGE_AWAITING_CONFIRM,
  handleConfirmUserSelect,
  CONFIRM_SEND_CUSTOM_ID,
  handleConfirmSendClick,
  CONFIRM_CANCEL_CUSTOM_ID,
  handleConfirmCancelClick,
  CONFIRM_EXPIRY_SELECT_CUSTOM_ID,
  handleConfirmExpirySelect,
  CONFIRM_SELF_DESTRUCT_SELECT_CUSTOM_ID,
  handleConfirmSelfDestructSelect,
  CONFIRM_NOTE_BUTTON_CUSTOM_ID,
  handleConfirmNoteButton,
  CONFIRM_NOTE_MODAL_CUSTOM_ID,
  handleConfirmNoteModal,
  CONFIRM_VOICE_EVERYONE_BUTTON_CUSTOM_ID,
  handleConfirmVoiceEveryone,
  CONFIRM_PICK_MANUAL_BUTTON_CUSTOM_ID,
  handleConfirmPickManual,
  CONFIRM_EVERYONE_BUTTON_CUSTOM_ID,
  handleConfirmEveryone,
  logSafeApiDetail,
  isGoogleMapsURL,
  sanitizeMessage,
  isAllowedFileType,
  isOnCooldown,
  setCooldown,
  clearCooldown,
  revokingSendLocks,
  batchSettled,
  sendCooldowns,
  renderConfirmCardRows,
  everyoneCountMemo,
  computeEveryoneDisplayCount,
  handleAddRecipients,
  buildDeliveryPayload,
  buildDeliveryEmbed,
  buildStepThroughButton,
  buildTrustButton,
  packBulkDeliveryComponents,
  buildRevokedDMPayload,
  persistDispatchResult,
  resolveSenderAlias,
  resolveGuildProvenance,
  safeUrlHost,
  monitorLinkStatus,
  revokeAllLinks,
  renderRevokeMsg,
  renderSendConfirm,
  mintLinksInBatches,
  cleanupFreshMintedLinks,
  activeMonitors,
  executeSendPipeline,
  largeSendThreshold,
  LARGE_SEND_RECIPIENT_FLOOR,
  handleQurlSend,
  handleQurlMap,
  resolveRecipientUsers,
  partitionRecipients,
  resolveMentionableSelection,
  resolveRoleNames,
  selfDestructOptionToSeconds,
  renderRecipientWarnings,
  renderConfirmCardContent,
  parseLocationInput,
  resolveLocation,
  RESOLVE_REASON,
  safeDecodeURIComponent,
  RECIPIENT_MODE_PICKER,
  RECIPIENT_MODE_VOICE,
  RECIPIENT_MODE_EVERYONE,
  normalizeRecipientMode,
  SEND_FLOW_TTL_SECONDS,
  classifyMintFailure,
  evictOldest,
  COOLDOWNS_MAX,
  logIgnoredDiscordErr,
  ALREADY_REVOKING_SEND_MSG,
  REVOKE_SELECT_RESULT_WAIT_MS,
  REVOKE_RUNNING_MSG,
  revokeRenderFallback,
  EXPIRY_CHOICES,
  PERSONAL_MESSAGE_INPUT_MAX,
  capUtf16Units,
} = sendSlash;

const {
  MessageFlags,
  SlashCommandBuilder,
  PermissionFlagsBits,
  ActionRowBuilder,
  ButtonBuilder,
  ButtonStyle,
  StringSelectMenuBuilder,
  ModalBuilder,
  TextInputBuilder,
  TextInputStyle,
} = require('discord.js');
const {
  Routes,
  ApplicationIntegrationType,
  InteractionContextType,
} = require('discord-api-types/v10');
const {
  takeAutocompleteToken,
} = require('./utils/autocomplete-rate-limit');
const config = require('./config');
const db = require('./store');
const logger = require('./logger');
const {
  MAX_FILE_SIZE,
  AUDIT_EVENTS,
  SETUP_VIA,
  GUILD_WEBHOOK_PENDING_NOTICE,
} = require('./constants');
const {
  expiryToISO,
  SELF_DESTRUCT_PRESETS,
  settlesWithin,
} = require('./utils/time');
const {
  signQurlOAuthState,
} = require('./utils/qurl-oauth-state');
const {
  getIdentity,
} = require('./qurl');
const {
  qurlApiErrorStatus,
} = require('./utils/qurl-errors');
const {
  detectWatermark,
  isAllowedSourceUrl,
} = require('./connector');
const {
  deleteFlow,
  transitionFlow,
  supersedeOrCreate,
} = require('./flow-state');
const {
  linkGuildWebhookSubscriptionForSetup,
} = require('./guild-webhook-link');
const { prepareGuildOwnerBinding } = require('./guild-owner');
const {
  flowIdForInteraction,
  registerFlow,
  safeReply,
  siblingMessageForStage,
} = require('./flow-dispatch');
const {
  isUnsupportedQurlContext,
  UNSUPPORTED_CONTEXT_MSG,
} = require('./interaction-context');
const {
  searchPlaces,
  encodePlaceIdSentinel,
  PLACE_ID_SHAPE_RE,
} = require('./places');

// renderViewCounter lives in its own leaf module so the cross-replica
// webhook fast-path (routes/qurl-webhook.js) can import the SAME pure
// renderer without pulling commands.js (+ discord.js) into the HTTP
// receiver's require graph. The monitor's buildStatusMsg() below + the
// fast-path both call it, so the confirmation body stays byte-identical
// across replicas. Still re-exported via _test (see module.exports) so
// the byte-identity unit test reads it where it always did.
const {
  renderViewCounter,
} = require('./view-counter-render');



const {
  sanitizeFilename,
  escapeDiscordMarkdown,
  sanitizeDisplayNamePlain,
  sanitizeContentLabel,
} = require('./utils/sanitize');

// ── /qurl detect cooldown (#1101) ──
//
// AUTHORITATIVE abuse enforcement for the deanonymization oracle lives on
// the CONNECTOR: it guild-scopes every lookup via the X-Guild-Id header and
// rate-limits per guild (a 429 we honor by KEEPING this cooldown to back
// off). This in-process Map is best-effort DEFENSE-IN-DEPTH — it's not
// shared across shards/instances, so a multi-instance deploy could let a
// determined caller exceed the per-process budget. It still dampens the
// common single-instance abuse case and gives the connector room to apply
// the real limit.
//
// SEPARATE Map from sendCooldowns, and keyed on a COMPOSITE `${guildId}:
// ${userId}` rather than userId alone. Two deliberate differences from the
// send bucket:
//   1. Distinct bucket — a user's detect throttle must not lock out their
//      /qurl send (and vice-versa); these are unrelated rate limits.
//   2. Per-(guild,user) — /qurl detect attribution is guild-scoped, so the
//      bot-side throttle is too. The same user probing two guilds gets two
//      independent budgets; a shared userId key would let one guild's
//      activity throttle another's.
//   3. Separate WINDOW — keyed on config.QURL_DETECT_COOLDOWN_MS, which
//      DEFAULTS to QURL_SEND_COOLDOWN_MS (no behavior change) but is
//      independently tunable. Detect (a deanonymization oracle) and send
//      are unrelated operations; coupling their windows would let a future
//      send-cadence change silently re-tune the oracle throttle.
// Eviction/cap structure mirrors sendCooldowns: it shares the SAME
// evictOldest helper + COOLDOWNS_MAX cap + the 60s time-sweep, so the Map
// can't grow unbounded under a snowflake-spraying attacker and the two
// buckets can't drift.
const detectCooldowns = new Map();

function detectCooldownKey(guildId, userId) {
  return `${guildId}:${userId}`;
}

function isOnDetectCooldown(guildId, userId) {
  const last = detectCooldowns.get(detectCooldownKey(guildId, userId));
  if (!last) return false;
  return Date.now() - last < config.QURL_DETECT_COOLDOWN_MS;
}

function setDetectCooldown(guildId, userId) {
  const key = detectCooldownKey(guildId, userId);
  detectCooldowns.delete(key);
  detectCooldowns.set(key, Date.now());
  evictOldest(detectCooldowns, COOLDOWNS_MAX);
}

function clearDetectCooldown(guildId, userId) {
  detectCooldowns.delete(detectCooldownKey(guildId, userId));
}
// Progress copy for the /qurl revoke select; it stays actionable if the process
// restarts before the result edit lands.
const REVOKE_SELECT_PROGRESS_MSG = "Revoking links... this can take a few minutes. If this message hasn't updated in 15 minutes, run `/qurl revoke` again.";

// --- /qurl revoke handler ---

// Discord caps StringSelectMenuOption label at 100 chars and description
// at 100 chars. Truncating to 99 + `…` leaves one for safety and keeps the
// rendering predictable across Discord clients.
// https://discord.com/developers/docs/interactions/message-components#select-menu-object-select-option-structure
const SELECT_MENU_FIELD_MAX = 100;

function truncate(s, max = SELECT_MENU_FIELD_MAX) {
  if (!s) return '';
  return s.length <= max ? s : `${s.slice(0, max - 1)}…`;
}

// Render the /qurl revoke dropdown label from a row returned by
// db.getRecentSends. Prefers the human-identifiable bit (filename or
// location name) over the previous abstract `${resource_type} to N users`.
// Falls back gracefully for legacy rows that predate qurl_send_configs
// being populated (LEFT JOIN produces null fields there).
//
// `channelName` is passed in by the caller (handleRevoke) from
// `interaction.guild?.channels.cache.get(s.channel_id)?.name`. Discord
// mention syntax (`<#id>`) does NOT render inside StringSelectMenu
// option labels — passing the name explicitly is the only way to avoid
// showing a raw snowflake to the user.
function formatRevokeLabel(s, channelName) {
  const recipients = s.recipient_count === 1 ? '1 person' : `${s.recipient_count} people`;
  const channelRef = s.target_type === 'user'
    ? 'DM'
    : (channelName ? `#${channelName}` : `${s.target_type} channel`);

  if (s.resource_type === 'file') {
    const name = s.attachment_name || 'file';
    return truncate(`📎 ${name} — ${recipients} (${channelRef})`);
  }
  if (s.resource_type === 'location') {
    const name = s.location_name || 'location';
    return truncate(`📍 ${name} — ${recipients} (${channelRef})`);
  }
  // Unknown resource type — keep the old abstract shape so we never
  // render a bare empty label.
  return truncate(`${s.resource_type} — ${recipients} (${channelRef})`);
}

function formatRevokeDescription(s) {
  const when = new Date(s.created_at).toLocaleString();
  const delivery = `${s.delivered_count}/${s.recipient_count} delivered`;
  const expiry = `requested expiry ${s.expires_in}`;
  const retry = s.revocation_pending ? 'Retry · ' : '';
  const base = `${retry}${when} · ${delivery} · ${expiry}`;
  // If there's space left, append a truncated message preview so users
  // can disambiguate sends with the same filename but different notes.
  if (s.personal_message) {
    const remaining = SELECT_MENU_FIELD_MAX - base.length - 4; // ` · "…"` overhead
    if (remaining > 8) {
      return truncate(`${base} · "${truncate(s.personal_message, remaining)}"`);
    }
  }
  return truncate(base);
}

// Single non-terminal stage between createFlow and deleteFlow.
// Stage name is the source of truth for the dispatcher routing
// table (registerFlow at the bottom of the file) — changing it is
// a single edit.
const REVOKE_STAGE_AWAITING_SELECT = 'awaiting_revoke_select';

// customId for the select menu. PREFIX-ONLY — no nonce, no flow_id
// encoded. Concurrent revokes deliberately drop: the second call
// supersedes the first via deleteFlow + createFlow. See
// flow-dispatch's trust-model comment on why customId is unsafe as
// an identity signal.
const REVOKE_SELECT_CUSTOM_ID = 'qurl_revoke_select';

// TTL for the awaiting-select flow row, in seconds. Matches the
// pre-existing 60-second click window for the menu. DDB TTL reap is
// asynchronous (~48 h) but loadFlow filters expired rows
// synchronously, so the UX cuts off at 60 s on the dot.
const REVOKE_FLOW_TTL_SECONDS = 60;

async function handleRevoke(interaction, apiKey) {
  if (!apiKey) {
    return interaction.reply({ content: 'qURL API key is not configured.', flags: MessageFlags.Ephemeral });
  }
  await interaction.deferReply({ flags: MessageFlags.Ephemeral });

  if (config.isMultiTenant && !interaction.guildId) {
    return interaction.editReply({ content: 'Use this command in a server.' });
  }
  const recentSends = config.isMultiTenant
    ? await db.getRecentSends(interaction.user.id, 5, interaction.guildId)
    : await db.getRecentSends(interaction.user.id, 5);

  if (recentSends.length === 0) {
    return interaction.editReply({ content: 'No recent sends to revoke.' });
  }

  // Open the flow row BEFORE rendering the menu. If the harness
  // fails (DDB outage, IAM regression), we want the user to see an
  // error immediately rather than a clickable menu whose selection
  // would then 404 against the dispatcher.
  //
  // Supersede semantics: a user with an existing in-flight revoke
  // flow who runs `/qurl revoke` again gets the old flow torn down
  // and a fresh menu. The orphan menu in the old message becomes a
  // no-op (its selection event will hit the dispatcher, miss on
  // loadFlow, and surface the "superseded — run again" message).
  // This is a deliberate exception to the design-doc rule that
  // "a user with an existing non-expired flow in a given channel
  // cannot start a second flow there" — for revoke specifically,
  // the menu is a stateless listing and the second invocation is
  // idempotent, so blocking it would just confuse users who can't
  // remember whether they cancelled the prior menu.
  //
  // supersedeOrCreate (flow-state.js) encapsulates the create →
  // load → version-gated delete → retry sequence shared by all
  // two-stage flows; the only flow-specific shape here is the
  // sibling-flow disambiguation when a non-revoke row owns the
  // flow_id.
  const flow_id = flowIdForInteraction(interaction);
  let supersede;
  try {
    supersede = await supersedeOrCreate({
      flow_id,
      stage: REVOKE_STAGE_AWAITING_SELECT,
      payload: null,
      ttl_seconds: REVOKE_FLOW_TTL_SECONDS,
    });
  } catch (err) {
    // DDB throttle / IAM blip — surface as recoverable rather than
    // letting it propagate to the generic "error executing command"
    // reply. supersedeOrCreate internally already retried once, so
    // throws here are real (not transient OCC churn).
    logger.warn('handleRevoke: supersedeOrCreate threw', {
      flow_id, error: err && err.message,
    });
    return interaction.editReply({
      content: 'Could not start a revoke session — please try again.',
    });
  }
  if (!supersede.created) {
    // A sibling flow owns this flow_id (or we lost two OCC races
    // in a row). Look up the sibling-message via the dispatcher's
    // registry — known sibling stage (setup-modal etc.) gets
    // actionable wording naming the command the user should
    // finish or cancel first; an unknown or vanished surviving
    // row falls through to the generic "try again."
    logger.warn('handleRevoke: supersedeOrCreate did not claim slot', {
      flow_id, surviving_stage: supersede.surviving?.stage ?? null,
    });
    const siblingMsg = siblingMessageForStage(supersede.surviving?.stage);
    if (siblingMsg) {
      return interaction.editReply({ content: siblingMsg });
    }
    return interaction.editReply({
      content: 'Could not start a revoke session — please try again.',
    });
  }

  const select = new StringSelectMenuBuilder()
    .setCustomId(REVOKE_SELECT_CUSTOM_ID)
    .setPlaceholder('Select a send to revoke')
    .addOptions(recentSends.map(s => {
      // StringSelectMenu labels are plain text — <#id> mention syntax
      // does NOT resolve. Look up the channel name from the guild cache
      // so users see `#general` instead of `#1123456789012345678`.
      // Cache hit is cheap; if cache is cold (DM context, bot just
      // restarted) the formatter falls through to a generic label.
      const channelName = s.channel_id
        ? interaction.guild?.channels.cache.get(s.channel_id)?.name ?? null
        : null;
      return {
        label: formatRevokeLabel(s, channelName),
        description: formatRevokeDescription(s),
        value: s.send_id,
      };
    }));

  const row = new ActionRowBuilder().addComponents(select);
  await interaction.editReply({
    content: 'Select a send to revoke all its links:',
    components: [row],
  });
}

// `reason: 'terminal'` reflects the FLOW lifecycle, not the qURL
// API call's success. The state machine terminated when the user
// picked an item; downstream revoke outcome is captured by its
// own `revoke_success` / `revoke_failed` audit events.
//
// `deleteFlow` is the at-most-once dedup primitive — only the
// worker whose conditional delete returns `{ deleted: true }`
// proceeds. A duplicate event (SQS at-least-once redelivery in
// the future worker tier, or a Discord double-dispatch today) sees
// `{ deleted: false }` and exits.
async function handleRevokeSelect(interaction, { flow_id }) {
  // Parallelize the dedup primitive (deleteFlow) with the apiKey
  // resolution. Safe here because getGuildApiKey is an idempotent
  // DDB read with zero side effects — the dedup loser short-circuits
  // before the qURL API call, so a duplicate event never multiplies
  // the parallel call's cost.
  //
  // Rule for future handlers in PR 6/7/8/9: parallelize the dedup
  // primitive ONLY with idempotent / free reads. Do NOT parallelize
  // with a paid side-effect call (a Google Places lookup, a recipient
  // resolution that hits an external API, a connector upload) —
  // at-least-once redelivery would then bill or trigger the side
  // effect once per duplicate, defeating the dedup gate's purpose.
  const [guildApiKey, deleteResult] = await Promise.all([
    interaction.guildId ? db.getGuildApiKey(interaction.guildId) : null,
    deleteFlow(flow_id, {
      stage: REVOKE_STAGE_AWAITING_SELECT,
      reason: 'terminal',
    }),
  ]);
  const apiKey = guildApiKey || config.QURL_API_KEY;

  if (!deleteResult.deleted) {
    // Another worker already completed (or admin_cleanup'd) this flow.
    // Reply rather than update — `interaction.update` would mutate
    // the original ephemeral message, but a duplicate event by
    // definition means the original was already updated by the
    // winning worker. Ephemeral reply preserves the user-visible
    // first result.
    return interaction.reply({
      content: 'This revoke was already processed.',
      flags: MessageFlags.Ephemeral,
    }).catch(logIgnoredDiscordErr);
  }

  if (!apiKey) {
    return interaction.update({
      content: 'qURL API key is no longer configured for this server.',
      components: [],
    }).catch(logIgnoredDiscordErr);
  }

  const sendId = interaction.values[0];
  const revokeLockKey = `${interaction.user.id}:${sendId}`;
  if (revokingSendLocks.has(revokeLockKey)) {
    return interaction.update({ content: ALREADY_REVOKING_SEND_MSG, components: [] }).catch(logIgnoredDiscordErr);
  }
  // Claim before acknowledging: another select/button must not start a fan-out.
  revokingSendLocks.add(revokeLockKey);
  // Child revoke can outlast the 3s component-response window (up to 65s per
  // connector chunk of ten children, plus the SDK fallback budget), so
  // acknowledge with a progress update and edit the message when the revoke
  // settles. A failed ack still revokes: the user asked for it and the barrier
  // makes a repeat safe; only the result message is lost.
  await interaction.update({ content: REVOKE_SELECT_PROGRESS_MSG, components: [] }).catch((err) => {
    logger.warn('Revoke select acknowledgement failed; revoking without a result message', {
      sendId, error: err?.message,
    });
  });
  let revoked;
  let revoking;
  try {
    revoking = revokeAllLinks(sendId, interaction.user.id, apiKey, resolveSenderAlias(interaction))
      .finally(() => revokingSendLocks.delete(revokeLockKey));
    // Leave the result edit inside Discord's 15-minute interaction window: a
    // very large send can outlast it, so say so and let the revoke finish.
    if (!await settlesWithin(revoking, REVOKE_SELECT_RESULT_WAIT_MS)) {
      logger.warn('Revoke select still running at its result budget', { sendId });
      await interaction.editReply({
        content: REVOKE_RUNNING_MSG,
        components: [],
      }).catch(logIgnoredDiscordErr);
      return;
    }
    revoked = await revoking;
  } catch (err) {
    if (!revoking) revokingSendLocks.delete(revokeLockKey);
    // The select menu is already gone; replace the progress text with one
    // actionable message and stop here so the dispatcher does not post twice.
    logger.error('Revoke select failed', { sendId, error: err?.message });
    await interaction.editReply({
      content: 'Could not complete revocation. Run `/qurl revoke` to retry.',
      components: [],
    }).catch(logIgnoredDiscordErr);
    return;
  }

  if (!revoked.barrierEstablished) {
    await interaction.editReply({
      content: 'Could not verify this send for revocation. It may already be revoked or unavailable; run `/qurl revoke` to refresh.',
      components: [],
    }).catch(logIgnoredDiscordErr);
    return;
  }

  // Slash-command path lacks the in-scope `recipients` array needed
  // to resolve names → no "Revoked for: …" line here. Operators
  // wanting names should use the inline button after a send.
  await interaction.editReply({
    content: safeRevokeHeader(sendId, revoked.success, revoked.total, revoked.finalizationFailed),
    components: [],
  }).catch(logIgnoredDiscordErr);
}

// /qurl-admin setup — legacy modal-paste path conversion. See
// docs/zero-downtime-design.md Pillar 1 for the two-stage shape:
//
//   /qurl-admin setup (slash)          createFlow(awaiting_setup_button)
//       → reply with [Configure qURL] button
//   button click (dispatcher)    transitionFlow → awaiting_setup_modal
//       → showModal()                              (set_expires_at extends TTL)
//   modal submit (dispatcher)    validate + persist + deleteFlow(terminal)
//       → editReply(success)
//
// The OAuth path (when AUTH0_* is configured) is NOT flow-state-
// backed — its persistence is the signed state token + the
// pending_links DDB table, and there's no `await*` in process to
// convert.
const SETUP_STAGE_AWAITING_BUTTON = 'awaiting_setup_button';
const SETUP_STAGE_AWAITING_MODAL = 'awaiting_setup_modal';
const SETUP_BUTTON_CUSTOM_ID = 'qurl_setup_button';
const SETUP_MODAL_CUSTOM_ID = 'qurl_setup_modal';
const SETUP_MODAL_FIELD_API_KEY = 'api_key';
const SETUP_AUTH_POLICY_UNAVAILABLE_MSG =
  '❌ **qURL setup is temporarily unavailable.**\n\n'
  + 'The bot operator must correct an invalid authentication setting in the deployment.';

function rejectSetupForInvalidAuthPolicy(interaction) {
  // warn, not error: server.js already raised the one actionable error-level
  // signal at boot; this fires per user interaction while the deploy is broken.
  logger.warn('Refusing /qurl-admin setup: AUTH0_EMAIL_CONNECTION was rejected at boot');
  return interaction.reply({
    content: SETUP_AUTH_POLICY_UNAVAILABLE_MSG,
    flags: MessageFlags.Ephemeral,
  });
}

// Two-stage TTL budget. The button-stage window covers "click the
// button" — short enough that an abandoned button is naturally
// superseded, long enough that a mobile admin who app-switches to
// a password manager doesn't come back to an expired button. The
// modal-stage window is the real budget for finding and pasting
// the key.
const SETUP_BUTTON_TTL_SECONDS = 120;
const SETUP_MODAL_TTL_SECONDS = 300;

// TODO(upstream-rebrand): regex + setMaxLength below mirror the
// upstream qurl-service key-format spec. A future rebrand or
// JWT-shaped key format must update both in lockstep — grep with
// `git grep TODO(upstream-rebrand)` lands every mirroring site.
const SETUP_API_KEY_REGEX = /^lv_(live|test)_[A-Za-z0-9_-]{20,}$/;
// Length floor + ceiling for the modal's TextInput, paired with
// SETUP_API_KEY_REGEX above. The floor (28) derives from
// `lv_live_`-prefix (8 chars) + the regex suffix floor of 20 chars;
// the ceiling (64) is a defense-in-depth cap for pathological
// pastes (Discord's default is 4000). Every member of the trio
// must move in lockstep — a future JWT-shaped key format that
// exceeds the ceiling would silently truncate at the modal and
// surface as a misleading "Invalid API key format" reply. Keeping
// the constants adjacent makes the lockstep impossible to update
// independently.
const SETUP_API_KEY_MIN_LENGTH = 28;
const SETUP_API_KEY_MAX_LENGTH = 64;

// User-visible success message on successful setup. Exported via
// _test so test assertions read the production string instead of
// hardcoding a copy that drifts.
const SETUP_SUCCESS_MSG =
  '✅ **qURL is now configured for this server!**\n\n'
  + (config.MAP_COMMAND_ENABLED
    ? 'Your team can use `/qurl send` and `/qurl map` to share files and locations securely.\n'
    : 'Your team can use `/qurl send` to share files securely.\n')
  + 'All qURL usage will be billed to your API key.';

// Button-stage handler. Routed by flow-dispatch when the admin
// clicks the [Configure qURL] button rendered by handleSetup.
//
// Sequence: transitionFlow first (it's the OCC primitive that
// rejects concurrent button clicks), then showModal as the ACK on
// the success branch. `showModal` MUST be the interaction's first
// response — we cannot deferReply ahead of it — so the DDB round-
// trip happens inside Discord's 3 s ACK window. UpdateItem latency
// is typically 50–200 ms; if a DDB outage pushes it close to the
// wall, the user sees Discord's "interaction failed" notice and
// can re-click. Acceptable tradeoff vs. losing OCC dedup.
//
// `set_expires_at` extends the row's TTL from the 120s button window
// (SETUP_BUTTON_TTL_SECONDS) to the 300s modal window
// (SETUP_MODAL_TTL_SECONDS), giving the admin a fresh budget from
// the moment the modal opens. This trips the `extended: true` audit
// flag on the FLOW_TRANSITION event — correct: the deadline really
// was extended.
async function handleSetupButton(interaction, { flow_id, row }) {
  if (config.isAuth0EmailConnectionRejected) {
    return rejectSetupForInvalidAuthPolicy(interaction);
  }

  const result = await transitionFlow(flow_id, row.version, {
    stage_to: SETUP_STAGE_AWAITING_MODAL,
    terminal: false,
    set_expires_at: Math.floor(Date.now() / 1000) + SETUP_MODAL_TTL_SECONDS,
  });

  // Early-return branches below use `interaction.reply` directly
  // (not safeReply): we haven't deferReply'd, so replied/deferred
  // are false on entry. Matches handleRevokeSelect's shape.
  if (result.result === 'conflict') {
    // Two concurrent button clicks. The OCC loser must NOT call
    // showModal — the winner will (or already did), and a duplicate
    // showModal on the same parent interaction surfaces as a
    // confusing Discord error.
    return interaction.reply({
      content: 'Another setup attempt is in progress — finish or close it, then retry.',
      flags: MessageFlags.Ephemeral,
    }).catch(logIgnoredDiscordErr);
  }

  if (result.result === 'not_found') {
    // Flow row vanished between the dispatcher's loadFlow and the
    // transitionFlow Update (TTL reap or a concurrent
    // admin_cleanup). Tell the user to rerun.
    return interaction.reply({
      content: 'This setup session expired or was replaced — please run `/qurl-admin setup` again.',
      flags: MessageFlags.Ephemeral,
    }).catch(logIgnoredDiscordErr);
  }

  // Success — show the modal. It becomes the interaction's ACK.
  // (Non-CCFE transitionFlow failures throw and propagate to the
  // dispatcher's safety net; see flow-state.js for the contract.)
  const modal = new ModalBuilder()
    .setCustomId(SETUP_MODAL_CUSTOM_ID)
    .setTitle('Configure qURL');
  const keyInput = new TextInputBuilder()
    .setCustomId(SETUP_MODAL_FIELD_API_KEY)
    .setLabel('qURL API Key')
    .setPlaceholder('lv_live_your_key_here')
    .setStyle(TextInputStyle.Short)
    .setRequired(true)
    // TODO(upstream-rebrand): min+max mirror upstream qurl-service's
    // key-format bounds. Both constants live next to SETUP_API_KEY_REGEX
    // so the lockstep trio is impossible to update independently.
    .setMinLength(SETUP_API_KEY_MIN_LENGTH)
    .setMaxLength(SETUP_API_KEY_MAX_LENGTH);
  modal.addComponents(new ActionRowBuilder().addComponents(keyInput));

  // Roll back the OCC transition if showModal throws. Without the
  // delete, the row sits at awaiting_setup_modal until TTL and the
  // admin's /qurl-admin setup rerun trips the supersede peek into the
  // "you already have a modal open" branch. If the rollback itself
  // also fails (DDB region outage), the error logs at error-level
  // — admin recovery is the SETUP_MODAL_TTL_SECONDS (5 min) wait.
  //
  // expectedVersion gates the rollback delete on the exact version
  // we just wrote. If a concurrent /qurl-admin setup rerun's
  // supersedeOrCreate ALSO observed this row and won the version
  // race first, this delete fails (deleted:false) — correct: the
  // row that exists at flow_id is now the new caller's, not ours
  // to clean up. Without the version gate, two simultaneous
  // rollbacks across two clients could each "succeed" against
  // each other's rows.
  //
  // No spurious error log if a concurrent supersede ALSO cleared
  // this row first — CCFE in deleteFlow returns `{deleted: false}`
  // rather than throwing (flow-state.js), so the .catch below never
  // fires on that path.
  try {
    await interaction.showModal(modal);
  } catch (err) {
    // warn-level: the dominant cause is Discord token expiry /
    // transient REST blips that are fully recoverable by user
    // rerun. error-level is reserved for the rollback-also-failed
    // branch below, which is the genuine "this admin is stuck for
    // up to SETUP_MODAL_TTL_SECONDS" condition that warrants paging.
    logger.warn('handleSetupButton: showModal failed after transitionFlow committed', {
      flow_id, error: err && err.message,
    });
    await deleteFlow(flow_id, {
      stage: SETUP_STAGE_AWAITING_MODAL,
      reason: 'abort',
      // The version that landed after our successful transitionFlow
      // — anything else means a concurrent supersede already
      // advanced the row past us, in which case we don't own the
      // rollback anymore.
      expectedVersion: result.version,
    }).catch((rollbackErr) => {
      // `metric: 'setup_rollback_failed'` is the stable selector
      // for the CloudWatch alarm filter. Keying on the message
      // string instead would break the moment a copy tweak lands.
      logger.error('handleSetupButton: rollback deleteFlow failed', {
        flow_id,
        error: rollbackErr && rollbackErr.message,
        metric: 'setup_rollback_failed',
      });
    });
    // The "run /qurl-admin setup again" guidance is accurate when the
    // rollback succeeded (clean state, rerun works). If the rollback
    // ALSO failed (rare double-DDB-failure logged just above), the
    // rerun will hit the "you already have a modal open" branch
    // until SETUP_MODAL_TTL_SECONDS elapses — the admin's recovery
    // is just to wait. Distinguishing the two cases in the message
    // would require reading the rollback outcome into a variable
    // (the .catch swallows it), and the wait-vs-retry distinction
    // is marginal UX in an already-rare error path; keep the
    // simpler wording.
    //
    // Use safeReply (followUp vs reply based on interaction state)
    // rather than a bare .reply.catch — showModal may have partially
    // acked before throwing, leaving interaction.replied=true; a
    // bare .reply would then throw InteractionAlreadyReplied and
    // silently swallow, leaving the admin with no feedback.
    await safeReply(
      interaction,
      'Could not open the configuration form — please run `/qurl-admin setup` again.',
    );
  }
}

// Modal-submit handler. Routed by flow-dispatch when the admin
// submits the modal opened by handleSetupButton. The flow row's
// stage is already validated (`awaiting_setup_modal`) by the
// dispatcher.
//
// `deleteFlow` runs BEFORE the qURL API validation so the dedup
// gate fires on the OCC primitive — a duplicate modal submit
// (Discord retry, double-click) sees `deleted: false` and exits
// without burning a qURL API key validation call. Same ordering
// rationale as handleRevokeSelect.
async function handleSetupModal(interaction, { flow_id }) {
  if (config.isAuth0EmailConnectionRejected) {
    return rejectSetupForInvalidAuthPolicy(interaction);
  }

  // Modal-stage flow_state delete is terminal — the user committed
  // the form, the flow has lifecycled out.
  const { deleted } = await deleteFlow(flow_id, {
    stage: SETUP_STAGE_AWAITING_MODAL,
    reason: 'terminal',
  });
  if (!deleted) {
    // `deleted: false` collapses three real causes: TTL'd between
    // modal open and submit, concurrent admin_cleanup, and a duplicate
    // submit from Discord retry. The TTL case is the most plausible
    // (admin walks away mid-paste past the 300 s budget), so the
    // wording covers both "expired" and "already processed."
    return interaction.reply({
      content: 'This setup session has expired or was already processed — run `/qurl-admin setup` again.',
      flags: MessageFlags.Ephemeral,
    }).catch(logIgnoredDiscordErr);
  }

  // Structured-log fields shared across every validation failure
  // branch. A guild that hits "Invalid API key" 50 times in a row
  // (admin keeps re-pasting the wrong key, or a credential rotation
  // broke them) needs to surface in ops dashboards — that's only
  // possible if every failure logs guild_id + configured_by.
  // Bare reply paths (no .catch on the malformed-key `interaction.reply`
  // and the validation-failure `interaction.editReply` branches below)
  // are intentional: the flow row is already deleted by this point,
  // so a Discord throw propagates to the dispatcher's safety net
  // which replies "run again" — which IS the correct UX for these
  // branches (admin really should rerun). Contrast the success-
  // path .catch a few lines down, where "run again" would mislead
  // because the key was already persisted.
  //
  // Exception: the `!deleted` early-return above DOES .catch — the
  // dedup-loser path means another worker already replied to the
  // user, and a second "run again" from the safety net would
  // visually duplicate. The bare-reply rule applies only to the
  // post-deleteFlow validation branches.
  const logFields = {
    guild_id: interaction.guildId,
    configured_by: interaction.user.id,
  };

  // `String(...)` coerces any non-string return shape (a hypothetical
  // Discord SDK contract change) into a string before .trim(). A
  // null/undefined coerces to 'null'/'undefined', which the regex
  // format check below cleanly rejects as malformed — the defense's
  // job is just to keep .trim() from throwing, not to validate.
  // The flow row is already deleted by this point, so an uncaught
  // throw here would surface as "Something went wrong" AFTER the
  // row is gone — asymmetrically expensive vs. this one-token wrap.
  const submittedKey = String(
    interaction.fields.getTextInputValue(SETUP_MODAL_FIELD_API_KEY),
  ).trim();
  // Probable client-side truncation: the key landed exactly at the
  // setMaxLength ceiling. The regex check below will likely reject
  // (current `lv_*` keys are well under 64 chars), but if it ever
  // accepts a 64-char value the warn surfaces the truncation
  // candidate to ops — an upstream key-format change that exceeds
  // the cap would otherwise look like a guild-side "Invalid API
  // key format" loop with no trail back to this constant.
  if (submittedKey.length === SETUP_API_KEY_MAX_LENGTH) {
    logger.warn('validate-key probable truncation (key landed at SETUP_API_KEY_MAX_LENGTH)', {
      ...logFields,
      key_length: submittedKey.length,
    });
  }
  if (!SETUP_API_KEY_REGEX.test(submittedKey)) {
    logger.warn('validate-key rejected (bad format)', logFields);
    return interaction.reply({
      content: 'Invalid API key format. Keys start with `lv_live_` or `lv_test_` and are at least 28 characters. Run `/qurl-admin setup` again to retry.',
      flags: MessageFlags.Ephemeral,
    });
  }

  await interaction.deferReply({ flags: MessageFlags.Ephemeral });
  let identity;
  try {
    identity = await getIdentity(submittedKey, interaction.guildId);
  } catch (err) {
    const status = qurlApiErrorStatus(err);
    logger.warn('Submitted guild key validation failed', { ...logFields, status, errorType: err?.name });
    if (status === 401 || status === 403) {
      return interaction.editReply({ content: '❌ **Invalid API key.** Double-check your key at **https://layerv.ai**.' });
    }
    return interaction.editReply({ content: status
      ? `❌ **qURL API error** (${status}). Try again later.`
      : '❌ **Could not validate key.** Please try again in a moment.' });
  }

  try {
    const binding = await prepareGuildOwnerBinding(interaction.guildId, identity.owner_id);
    await db.setGuildApiKey(interaction.guildId, submittedKey, interaction.user.id, SETUP_VIA.PASTE, binding);
  } catch (err) {
    const status = qurlApiErrorStatus(err);
    const changed = err?.code === 'GUILD_SETUP_CHANGED' || err?.name === 'ConditionalCheckFailedException';
    const conflict = err?.code === 'GUILD_OWNER_CONFLICT' || status === 401 || status === 403;
    const log = changed || conflict ? logger.warn : logger.error;
    log('Guild key owner verification refused', { ...logFields, code: err?.code, status, errorType: err?.name });
    if (changed) {
      return interaction.editReply({ content: '❌ **Server setup changed.** Another setup changed this server. Run `/qurl setup` again with the existing qURL owner account.' });
    }
    return interaction.editReply({ content: conflict
      ? '❌ **Server ownership could not be verified.** Use a key from the existing qURL owner account. If its saved key is no longer valid or ownership must change, contact the bot operator.'
      : '❌ **Setup is temporarily unavailable.** The existing server connection is unchanged. Try again shortly.' });
  }
  logger.info('Guild API key configured', logFields);

  const webhookLinked = await linkGuildWebhookSubscriptionForSetup({
    guildId: interaction.guildId,
    apiKey: submittedKey,
    via: SETUP_VIA.PASTE,
    configuredBy: interaction.user.id,
  });

  // Swallow Discord errors on the post-persist editReply. If the
  // editReply throws AFTER setGuildApiKey commits, letting it
  // propagate would fire the dispatcher's universal safety-net
  // reply ("Something went wrong — please run the command again"),
  // which is actively wrong: the key IS saved. Admin can confirm
  // with /qurl-admin status; the saved-but-no-confirmation case is rare
  // and recoverable, the misleading-error case is not.
  return interaction.editReply({
    content: webhookLinked ? SETUP_SUCCESS_MSG : `${SETUP_SUCCESS_MSG}\n${GUILD_WEBHOOK_PENDING_NOTICE}`,
  }).catch(logIgnoredDiscordErr);
}

// ─────────────────────────────────────────────────────────────
// /qurl send + /qurl map — slash-driven sends.
//
// All-options-up-front slash commands that render an in-channel
// ephemeral confirm card. The flow:
//
//   /qurl send recipients:<text> attachment:<file> [expires-in]
//                                                  [self-destruct]
//                                                  [personal-message]
//   /qurl map  recipients:<text> location:<text>   [location-name]
//                                                  [expires-in]
//                                                  [self-destruct]
//                                                  [personal-message]
//
// After parsing/validating the slash options, the bot renders an
// in-channel ephemeral confirm card with the resolved recipient list
// (or a UserSelectMenu when `recipients:` was omitted) plus Send /
// Cancel buttons. The confirm card is flow_state-backed so the Send
// dedup gate fires on `deleteFlow` (consistent with /qurl revoke,
// /qurl-admin setup).
// ─────────────────────────────────────────────────────────────

const {
  MAX_SLASH_OPTION_LENGTH: RECIPIENTS_SLASH_MAX_LENGTH,
} = require('./recipient-parser');

// Subcommands that require the guild API key resolution + cooldown gate.
// Single-source allowlist: adding a new send-style subcommand only
// requires touching this set, not the dispatcher fall-through.
//
// `'map'` joins the set only when MAP_COMMAND_ENABLED is on. The
// conditional removal is load-bearing — the gate runs BEFORE the
// dispatch in execute() below, so if `'map'` stayed in the set with
// the flag off, a stale-client /qurl map in an unconfigured guild
// would hit "qURL is not configured for this server" instead of
// QURL_MAP_DISABLED_REPLY.
const API_KEY_GATED_SUBCOMMANDS = new Set(
  config.MAP_COMMAND_ENABLED ? ['send', 'map', 'revoke'] : ['send', 'revoke'],
);

// User-facing reply for stale /qurl map submissions when the toggle
// is off. Reached when a Discord client routes a cached `map`
// submission after the registration has dropped it. Copy mirrors
// the flag-off branch of SETUP_SUCCESS_MSG so both surfaces describe
// the same remaining capability.
const QURL_MAP_DISABLED_REPLY = '❌ `/qurl map` is currently disabled. Use `/qurl send` to share files securely.';

// User-facing reply for stale /qurl detect submissions when DETECT_COMMAND_ENABLED
// is off — a Discord client routing a cached `detect` submission after the
// registration dropped it. Mirrors QURL_MAP_DISABLED_REPLY.
const QURL_DETECT_DISABLED_REPLY = '❌ `/qurl detect` is currently disabled.';
const SELF_DESTRUCT_CHOICES = [
  { name: 'No timer (default)', value: SELF_DESTRUCT_NO_TIMER_CHOICE },
  ...SELF_DESTRUCT_PRESETS.map((p) => ({ name: p.label, value: String(p.seconds) })),
];

// ──────────────────────────────────────────────────────────────────────
// /qurl detect — watermark attribution (#1101)
// ──────────────────────────────────────────────────────────────────────

// Shared no-match reply. BOTH the "no mark at all" branch and the
// "mark found but no same-guild row" branch (the cross-guild defense-in-depth
// filter) MUST emit BYTE-IDENTICAL copy. If they differed, a requester could
// distinguish "no watermark" from "watermarked, but for another guild" — and
// that distinction IS the cross-guild signal the second-layer filter exists
// to contain (it only matters if the connector's X-Guild-Id scope regresses,
// which is exactly when this filter is load-bearing). One constant = the two
// paths can't drift apart. (The ambiguous >1-row reply is deliberately
// distinct — that's a same-guild duplicate, not a cross-guild leak.)
const DETECT_NO_MATCH_MSG = '🔍 No qURL watermark found in this image (for this server).';

// Server-staff permissions that grant /qurl detect standing — the PM "admins/mods" tier, layered
// on top of the original-sender tier (see the handler's ACCESS MODEL). Deliberately TIGHT and
// governance-oriented, not a broad enumeration of every mod bit:
//   - ManageGuild — this bot's own admin gate (matches /qurl-admin status + setup); an Administrator
//     passes it automatically via PermissionsBitField.has().
//   - BanMembers — the moderator tier that handles the PM's governance examples (abuse reports,
//     DMCA, ban appeals).
// Excludes broader content perms (e.g. ManageMessages) on purpose: ship tight, loosen if real
// demand surfaces (a one-line change). A caller with NONE of these who is also not the original
// sender of the qURL has no standing and gets the byte-identical no-match reply.
const DETECT_STAFF_PERMISSIONS = [
  PermissionFlagsBits.ManageGuild,
  PermissionFlagsBits.BanMembers,
];

//
// A user uploads an image; the bot reads the invisible meta-seal watermark
// via the connector's /api/detect, resolves it to the qurl_id it was minted
// for, and replies (EPHEMERALLY — it's sensitive) with the original
// RECIPIENT's Discord handle + a % match. SCOPED to the requester's guild:
// an image watermarked in guild A must never attribute in guild B.
//
// ACCESS MODEL (PM decision, #1101 — "privacy is the north star"): detect REVEALS a recipient
// only to someone with STANDING to investigate —
//   (a) the ORIGINAL SENDER of that qURL (their distribution, their property right), or
//   (b) server STAFF (ManageGuild / BanMembers — see DETECT_STAFF_PERMISSIONS) for governance
//       (abuse reports, DMCA, ban appeals, where the sender may be absent or uninvolved).
// A general member has NO standing and gets the BYTE-IDENTICAL no-match reply — they learn
// nothing, not even that the image is a watermarked qURL (a partial result + "ask an admin" would
// just train users to route around the privacy model). The standing check runs BEFORE the
// ambiguity/reveal branches so EVERY no-standing outcome collapses to ONE reply. The permission
// set is tight by design and a one-line, reversible change if real demand surfaces.
//
// RESIDUAL (timing side-channel): the byte-identical reply closes the CONTENT channel but not a
// TIMING one — no-mark returns BEFORE the DDB lookup, no-standing AFTER it, and a matched reveal
// after an extra users.fetch, so reply latency weakly clusters fast/medium/slow. A determined
// prober could use that to distinguish "no mark" from "marked, no standing". Accepted as a residual,
// NOT equalized with constant-time work: it's a weak oracle (ephemeral replies, network jitter, the
// per-(guild,user) cooldown + the connector's per-guild throttle all blunt it) and the connector's
// guild scope stays the authoritative boundary regardless.
//
// SCOPE (threat model): the standing gate is a bot-side UX/governance layer, NOT a server-enforced
// authorization boundary. The connector's /api/detect enforces only guild-scope; a holder of the
// guild's qURL API key (admin-tier) could call /api/detect directly and bypass THIS gate (guild-
// scope still holds — never cross-tenant). Closing that gap needs a connector-side per-route auth
// factor (tracked in infra repo #1170); until then, standing is enforced here.
//
// This is a deanonymization oracle by construction. The guards, in order:
//   - guild-only (DM rejects, no oracle outside a guild).
//   - per-(guild,user) cooldown (detectCooldowns) — best-effort
//     defense-in-depth against scraping the recipient↔image mapping. The
//     AUTHORITATIVE rate limit is the connector's per-guild throttle (this
//     in-process Map isn't shared across instances); see the detectCooldowns
//     header for the full enforcement model.
//   - SSRF + size + image-type validation on the attachment, identical to
//     the guards handleQurlSend applies to the same Discord-CDN fetch.
//   - TWO layers of guild scoping: the connector scopes via X-Guild-Id,
//     AND we re-filter the returned rows on guild_id here (defense-in-depth
//     — if the connector contract ever regresses, the leak still can't
//     cross guilds because this filter fails closed).
//   - STANDING gate (the ACCESS MODEL above): reveal only to the original
//     sender or to staff; no-standing callers get the byte-identical no-match
//     reply, checked BEFORE the ambiguity/reveal branches.
//   - audit the attribution outcomes + abuse signals (matched / no_match /
//     ambiguous / rejected / unconfigured / rate_limited / no_standing — see
//     AUDIT_EVENTS.QURL_DETECT); recipient id NEVER logged (audit access is
//     broader than the ephemeral reply). Honest operational failures (CDN
//     download, connector 5xx/network) log at warn/error, not audit.
//
// Reply is always ephemeral. Cooldown rationale per-branch mirrors
// handleQurlSend: honest user errors (missing/non-image/oversize attachment,
// client-schema desync) CLEAR the cooldown so retry is immediate; the SSRF
// probe KEEPS it (probing the allow-list is an abuse signal).
async function handleQurlDetect(interaction) {
  // DM rejection first — no cooldown burned on a guild-only command
  // attempted from DMs. Same `interaction.guildId`-only signal as
  // handleQurlSend (see its comment for the http-only-mode caveat).
  if (!interaction.guildId) {
    return interaction.reply({
      content: 'This command can only be used in a server, not in DMs.',
      flags: MessageFlags.Ephemeral,
    });
  }

  // Cooldown gate at entry — per (guild, user). Set BEFORE input
  // validation so a malformed attachment still throttles (so spamming
  // invalid inputs can't sidestep the bot-side throttle; the connector's
  // per-guild rate limit is the authoritative backstop).
  if (isOnDetectCooldown(interaction.guildId, interaction.user.id)) {
    return interaction.reply({
      content: 'Please wait before running detect again.',
      flags: MessageFlags.Ephemeral,
    });
  }
  setDetectCooldown(interaction.guildId, interaction.user.id);

  // Required-option lookup. `getAttachment(name, true)` throws on a
  // missing option; Discord enforces required server-side, so a hit is
  // almost always a client/schema desync during a redeploy window — clear
  // the cooldown so retry is immediate once the deploy stabilizes (same
  // rationale as handleQurlSend's required-option catch).
  let attachment;
  try {
    attachment = interaction.options.getAttachment('image', true);
  } catch (err) {
    logger.warn('handleQurlDetect: required image option missing', {
      user_id: interaction.user.id, error: err && err.message,
    });
    clearDetectCooldown(interaction.guildId, interaction.user.id);
    return interaction.reply({
      content: '❌ The `image:` option is required. Re-run with an image attached.',
      flags: MessageFlags.Ephemeral,
    });
  }
  if (!attachment || typeof attachment.url !== 'string') {
    clearDetectCooldown(interaction.guildId, interaction.user.id);
    return interaction.reply({
      content: '❌ Attachment is missing or malformed.',
      flags: MessageFlags.Ephemeral,
    });
  }
  // SSRF gate — the attachment.url is fetched below; re-validate it
  // against the Discord-CDN allow-list exactly like handleQurlSend. This
  // is the ONE rejection that KEEPS the cooldown — probing the allow-list
  // is an abuse signal, not an honest user error.
  if (!isAllowedSourceUrl(attachment.url)) {
    logger.warn('handleQurlDetect: attachment.url failed SSRF gate', {
      user_id: interaction.user.id, host: safeUrlHost(attachment.url),
    });
    // Audit the SSRF probe — it's the STRONGEST abuse signal this handler
    // sees (it's the one rejection that KEEPS the cooldown), so it belongs
    // in the audit trail, not just logger.warn. No recipient id: a
    // rejection never resolves one.
    logger.audit(AUDIT_EVENTS.QURL_DETECT, {
      result: 'rejected',
      guild_id: interaction.guildId,
      requester_id: interaction.user.id,
    });
    return interaction.reply({
      content: '❌ Attachment source not allowed. Upload the image via Discord, not a linked URL.',
      flags: MessageFlags.Ephemeral,
    });
  }
  // Image-only: the connector's neural detector reads pixels. A non-image
  // (PDF, zip, etc.) can't carry the meta-seal mark, so reject early
  // rather than spend an inference round-trip. Honest user error → clear
  // cooldown.
  if (typeof attachment.contentType !== 'string' || !attachment.contentType.startsWith('image/')) {
    clearDetectCooldown(interaction.guildId, interaction.user.id);
    return interaction.reply({
      content: `❌ Detect needs an image. \`${escapeDiscordMarkdown(String(attachment.contentType || 'unknown'))}\` isn't supported.`,
      flags: MessageFlags.Ephemeral,
    });
  }
  // Size cap before buffering — guard against pulling a 25MB+ blob into
  // memory. Honest user error → clear cooldown. `typeof === 'number'`
  // guard: a missing/undefined attachment.size would make a bare
  // `size > MAX` read false and skip this pre-check — harmless here only
  // because the realized-buffer cap after the fetch (below) is the real
  // backstop, but make the intent explicit rather than relying on
  // `undefined > N` semantics.
  if (typeof attachment.size === 'number' && attachment.size > MAX_FILE_SIZE) {
    clearDetectCooldown(interaction.guildId, interaction.user.id);
    return interaction.reply({
      content: `❌ Image too large (${Math.round(attachment.size / 1024 / 1024)}MB). Maximum is ${Math.round(MAX_FILE_SIZE / 1024 / 1024)}MB.`,
      flags: MessageFlags.Ephemeral,
    });
  }

  // Defer EARLY (ephemeral) — everything past here is network-bound (CDN
  // fetch + neural-net inference ~seconds + a recipient users.fetch), which
  // would blow Discord's 3-second ACK deadline on a plain reply. After
  // deferReply, all user-visible output is editReply.
  await interaction.deferReply({ flags: MessageFlags.Ephemeral });

  // The bot credential mints the guild-scoped capability. No customer key
  // is sent to the detect service.
  if (!config.QURL_API_KEY) {
    // Detect stays outside API_KEY_GATED_SUBCOMMANDS: no guild setup is needed.
    // Keep the cooldown for operator configuration failures; user retries cannot fix them.
    logger.audit(AUDIT_EVENTS.QURL_DETECT, {
      result: 'unconfigured',
      guild_id: interaction.guildId,
      requester_id: interaction.user.id,
    });
    return interaction.editReply({
      content: '❌ **Watermark detection is unavailable.** The bot operator must configure the detect credential.',
    });
  }

  // Download the image bytes from Discord CDN. This is a SEPARATE failure
  // domain from the connector detect below — a download failure (expired
  // signed URL, timeout, DNS, network) is honest + re-uploadable and must
  // NOT be reported as "detection unavailable" (detection was never
  // reached). Its own try/catch keeps the two error surfaces distinct, and
  // both the non-ok and the throw paths CLEAR the cooldown (honest, and the
  // reply tells the user to re-upload, which the cooldown would block).
  let bytes;
  try {
    const r = await fetch(attachment.url, { signal: AbortSignal.timeout(30000) });
    if (!r.ok) {
      // CDN URLs are signed + expire (~24h). A non-ok fetch is almost always
      // an expired URL on a re-shared old image, not our bug.
      logger.warn('handleQurlDetect: CDN fetch failed', {
        user_id: interaction.user.id, status: r.status,
      });
      clearDetectCooldown(interaction.guildId, interaction.user.id);
      return interaction.editReply({
        content: '❌ Could not download that image (the Discord link may have expired). Re-upload and try again.',
      });
    }
    bytes = Buffer.from(await r.arrayBuffer());
  } catch (err) {
    // Fetch threw — AbortSignal 30s timeout, DNS, connection reset, etc.
    // Honest download failure (NOT a detection failure), so same copy +
    // cooldown-clear as the non-ok branch above.
    logger.warn('handleQurlDetect: CDN fetch threw', {
      user_id: interaction.user.id, error: err && err.message,
    });
    clearDetectCooldown(interaction.guildId, interaction.user.id);
    return interaction.editReply({
      content: '❌ Could not download that image (re-upload and try again).',
    });
  }

  // Realized-buffer size guard (closes the TOCTOU vs. the attachment.size
  // pre-check): re-check the ACTUAL downloaded byte count. SSRF already
  // pins the source to the Discord CDN, so this is defense-in-depth against
  // a lying/absent Content-Length or a metadata/payload mismatch. Honest
  // oversize → clear cooldown, same as the pre-fetch size branch.
  if (bytes.length > MAX_FILE_SIZE) {
    logger.warn('handleQurlDetect: realized buffer exceeds cap', {
      user_id: interaction.user.id, bytes: bytes.length,
    });
    clearDetectCooldown(interaction.guildId, interaction.user.id);
    return interaction.editReply({
      content: `❌ Image too large (${Math.round(bytes.length / 1024 / 1024)}MB). Maximum is ${Math.round(MAX_FILE_SIZE / 1024 / 1024)}MB.`,
    });
  }

  // POST the bytes to /api/detect.
  let result;
  try {
    result = await detectWatermark(bytes, {
      guildId: interaction.guildId,
      contentType: attachment.contentType,
    });
  } catch (err) {
    // detectWatermark (the CONNECTOR POST — the CDN download is handled in
    // its own try/catch above) throws (with .status, set from
    // response.status in connector.js's throwConnectorError) on
    // 401/400/429/5xx and on a connector-side network failure. Body-free
    // message by contract — log server-side, surface a generic ephemeral
    // error. Don't leak err.message (it may echo connector detail) into the
    // reply.
    //
    // Cooldown policy: a transient failure shouldn't throttle an honest
    // user for the full window, so CLEAR the cooldown — EXCEPT on a 429
    // (connector rate-limited this guild), where KEEPING the cooldown is
    // the correct back-off. (429 is cleanly distinguishable via err.status;
    // connector network throws / 5xx / 4xx-other all clear.)
    const rateLimited = err && err.status === 429;
    logger.error('handleQurlDetect: detect failed', {
      user_id: interaction.user.id,
      guild_id: interaction.guildId,
      status: err && err.status,
      rate_limited: rateLimited,
      error: err && err.message,
    });
    if (rateLimited) {
      // A connector 429 means this guild is spamming detect into the
      // connector's per-guild limiter — an abuse signal, and the one detect
      // failure that KEEPS the cooldown. Mirror the SSRF-probe reasoning and
      // put it in the audit trail (not just logger.error) so the
      // throttled-spam pattern is queryable. No recipient id — a throttled
      // call never resolves one.
      logger.audit(AUDIT_EVENTS.QURL_DETECT, {
        result: 'rate_limited',
        guild_id: interaction.guildId,
        requester_id: interaction.user.id,
      });
    } else {
      clearDetectCooldown(interaction.guildId, interaction.user.id);
    }
    return interaction.editReply({
      content: rateLimited
        ? '❌ Watermark detection is busy right now (rate-limited). Please wait a moment and try again.'
        : '❌ Watermark detection is unavailable right now. Please try again in a moment.',
    });
  }

  // No mark, OR the connector found a mark but no SAME-GUILD match
  // (detected=false collapses both — qurl_id/match_pct null). Audit the
  // no-match outcome, then a non-committal ephemeral (don't reveal whether
  // a mark exists in some OTHER guild — that itself would leak cross-guild
  // signal).
  if (!result.detected || !result.qurl_id) {
    logger.audit(AUDIT_EVENTS.QURL_DETECT, {
      result: 'no_match',
      guild_id: interaction.guildId,
      requester_id: interaction.user.id,
    });
    return interaction.editReply({
      // Shared constant — MUST match the cross-guild-filtered branch below
      // byte-for-byte. See DETECT_NO_MATCH_MSG.
      content: DETECT_NO_MATCH_MSG,
    });
  }

  // Connector says detected + gave a qurl_id. Look up the recipient
  // row(s). DEFENSE-IN-DEPTH guild filter: the connector already
  // guild-scopes via X-Guild-Id, but re-filter on guild_id here so a
  // connector-side contract regression can't cross guilds — this filter
  // fails closed (a row missing guild_id, or carrying a different one, is
  // dropped). findSendsByQurlId returns 0–1 rows in steady state
  // (one qurl_id per recipient by write-path invariant, capped at Limit:2).
  const sends = await db.findSendsByQurlId(result.qurl_id);
  const sameGuild = (sends || []).filter((s) => s.guild_id === interaction.guildId);
  if (sameGuild.length === 0) {
    // Connector matched but no local same-guild row — treat as no match
    // (the local row may have aged out, or the connector's scope and ours
    // diverged). BYTE-IDENTICAL copy to the no-mark branch above (shared
    // DETECT_NO_MATCH_MSG) so a requester can't tell "no watermark" from
    // "watermarked for another guild" — that distinction is the cross-guild
    // signal this filter exists to contain.
    logger.audit(AUDIT_EVENTS.QURL_DETECT, {
      result: 'no_match',
      guild_id: interaction.guildId,
      requester_id: interaction.user.id,
      qurl_id: result.qurl_id,
    });
    return interaction.editReply({
      content: DETECT_NO_MATCH_MSG,
    });
  }

  // STANDING gate (ACCESS MODEL): reveal only to the original SENDER of this qURL or to server
  // STAFF. Runs BEFORE the ambiguity/reveal branches ON PURPOSE — every no-standing outcome (no
  // mark, marked-for-another-guild, ambiguous, no-standing) must collapse to the SAME
  // byte-identical reply, or a prober could distinguish "this is a marked qURL" from "no mark".
  // Standing is checked with .some() over the rows so the sender check still holds in the
  // degenerate length-2 (ambiguous) case.
  const isStaff = DETECT_STAFF_PERMISSIONS.some(
    (p) => interaction.memberPermissions?.has(p) === true,
  );
  const isSender = sameGuild.some((s) => s.sender_discord_id === interaction.user.id);
  if (!isStaff && !isSender) {
    // No standing → audit the truth (operators see it) but reply byte-identical to no-match (the
    // caller learns nothing). KEEP the cooldown — a no-standing probe is sensitive, not an honest
    // input error, so it must not reset the throttle.
    logger.audit(AUDIT_EVENTS.QURL_DETECT, {
      result: 'no_standing',
      guild_id: interaction.guildId,
      requester_id: interaction.user.id,
      qurl_id: result.qurl_id,
    });
    return interaction.editReply({
      content: DETECT_NO_MATCH_MSG,
    });
  }

  if (sameGuild.length > 1) {
    // Ambiguous: >1 same-guild row for one qurl_id violates the
    // one-qurl_id-per-recipient write invariant (a write-path regression
    // landed a duplicate). Mirror the qurl.expired handler's ambiguous-skip
    // — do NOT attribute to one row arbitrarily (it could finger the wrong
    // recipient). Surface a non-committal reply + audit so an operator can
    // investigate. findSendsByQurlId's Limit:2 means length is exactly 2
    // here; the audit's qurl_id is the investigation handle.
    logger.audit(AUDIT_EVENTS.QURL_DETECT, {
      result: 'ambiguous',
      guild_id: interaction.guildId,
      requester_id: interaction.user.id,
      qurl_id: result.qurl_id,
    });
    return interaction.editReply({
      content: '🔍 Couldn\'t determine a single recipient for this image. Please contact a server admin.',
    });
  }

  const send = sameGuild[0];
  // How this caller earned standing — 'sender' (their own qURL) or 'staff' (governance). Recorded
  // in the match audit for forensics; the user-facing reveal is identical either way.
  const grantBasis = isSender ? 'sender' : 'staff';
  const matchPct = typeof result.match_pct === 'number' ? Math.round(result.match_pct) : null;

  // Audit the MATCH — qurl_id + match_pct + confidence, but NEVER the
  // resolved recipient id. Audit logs are broader-access than the ephemeral
  // reply; logging the unmasked recipient would re-leak the deanonymization
  // the ephemeral is there to contain. confidence (0–1) lives HERE, not in
  // the user reply — see the reply below. match_pct is logged RAW
  // (result.match_pct, not the display-rounded matchPct) so forensics keeps
  // the connector's exact value, not the value the user happened to see.
  logger.audit(AUDIT_EVENTS.QURL_DETECT, {
    result: 'matched',
    guild_id: interaction.guildId,
    requester_id: interaction.user.id,
    qurl_id: result.qurl_id,
    match_pct: result.match_pct,
    confidence: result.confidence,
    grant_basis: grantBasis,
  });

  // Resolve the recipient's handle. Best-effort: if the user can't be
  // fetched (left Discord, API blip), still surface the attribution with
  // the raw mention (Discord renders <@id> client-side) rather than fail
  // the whole detect.
  let username = null;
  try {
    const user = await interaction.client.users.fetch(send.recipient_discord_id);
    username = user && user.username;
  } catch (err) {
    logger.warn('handleQurlDetect: recipient user fetch failed', {
      user_id: interaction.user.id, error: err && err.message,
    });
  }

  const id = send.recipient_discord_id;
  const who = username ? `**${escapeDiscordMarkdown(username)}** (<@${id}>)` : `<@${id}>`;
  // match_pct (0–100) is the headline the user asked for. confidence (0–1)
  // is deliberately OMITTED from the reply — mixing a 0–100 and a 0–1 scale
  // confused readers, and an absent confidence rendered a bare "0". It stays
  // in the match audit above for operators.
  //
  // Suppress the "% match" suffix unless match_pct is a POSITIVE number:
  // a detected match always has match_pct > 0, so a null/0/garbled value
  // is a malformed connector response, not a real 0% — render the bare
  // attribution rather than a confusing "0% match". (The audit above keeps
  // the raw value for forensics.) The > 0 test is on the ROUNDED value, which
  // is safe because the connector FLOORS a detected match_pct at 75 (Hamming
  // path, distance <= max) or 100 (exact hit) — so a value that rounds to 0 is
  // genuinely malformed, never a real sub-1% match being dropped.
  const pctText = (typeof matchPct === 'number' && matchPct > 0) ? ` — ${matchPct}% match` : '';
  return interaction.editReply({
    content: `🔍 This image was watermarked for ${who}${pctText}.`,
    // The reply embeds the recipient's <@id> mention. Ephemeral replies
    // don't ping in practice, but suppress mention-resolution explicitly so
    // "the deanonymized recipient is NEVER pinged" is a hard guarantee, not
    // a reliance on Discord ephemeral behavior. parse:[] = resolve nothing.
    allowedMentions: { parse: [] },
  });
}

// Pure rendering / wording logic lives in `./revoke-render` so the
// e2e smoke test can require it without pulling in `discord.js`.
const {
  REVOKE_TRUNC_LIMIT,
  buildRevokeHeader,
} = require('./revoke-render');

function safeRevokeHeader(sendId, success, total, finalizationFailed = false) {
  try {
    return buildRevokeHeader(success, total, { finalizationFailed });
  } catch (err) {
    return revokeRenderFallback(sendId, success, total, err);
  }
}

// Time-based sweep every 60s (was 5min). With high user counts the Map can
// creep between the size-based 10k-threshold eviction, so a more aggressive
// proactive sweep keeps steady-state memory tight. An entry is safe to drop
// only once it's older than the SAME window its is-on-cooldown gate uses —
// otherwise the sweep would evict a still-active entry and silently end the
// cooldown early.
//
// detectCooldowns rides the SAME interval but is swept against
// QURL_DETECT_COOLDOWN_MS (the window isOnDetectCooldown gates on), NOT the
// send window — these can differ (the round-4 decoupling exists precisely so
// detect can be a STRICTER, longer window than send). Sweeping detect at the
// send window would truncate a longer detect cooldown back to the send
// window, defeating the decoupling.
// Named (not an inline arrow) so the windows-differ regression — a detect
// entry swept at the SEND window when DETECT > SEND — is directly testable
// via the _test export, without driving the module-level setInterval.
function sweepCooldowns() {
  const now = Date.now();
  for (const [k, v] of sendCooldowns) {
    if (now - v > config.QURL_SEND_COOLDOWN_MS) sendCooldowns.delete(k);
  }
  for (const [k, v] of detectCooldowns) {
    if (now - v > config.QURL_DETECT_COOLDOWN_MS) detectCooldowns.delete(k);
  }
}
setInterval(sweepCooldowns, 60 * 1000).unref();

// Split so map can insert location-name between recipients and delivery options.
function addSendRecipients(sub) {
  return sub
    .addStringOption(opt =>
      opt.setName('recipients')
        .setDescription('Users to send to — paste @mentions. Leave blank to pick from a menu.')
        .setRequired(false)
        .setMaxLength(RECIPIENTS_SLASH_MAX_LENGTH)
    );
}

function addSendDeliveryOptions(sub) {
  return sub
    .addStringOption(opt =>
      opt.setName('expires-in')
        .setDescription('How long the qURL links stay valid (default: 24 hours)')
        .setRequired(false)
        .addChoices(...EXPIRY_CHOICES)
    )
    .addStringOption(opt =>
      opt.setName('self-destruct')
        .setDescription('Optional countdown after first open (default: no timer)')
        .setRequired(false)
        .addChoices(...SELF_DESTRUCT_CHOICES)
    )
    .addStringOption(opt =>
      opt.setName('personal-message')
        .setDescription('Optional note included in each recipient\'s DM')
        .setRequired(false)
        .setMaxLength(PERSONAL_MESSAGE_INPUT_MAX)
    );
}

function buildQurlCommandData(cfg) {
  const builder = new SlashCommandBuilder()
    .setName('qurl')
    .setDescription('Share resources securely via qURL')
    .addSubcommand(sub => {
      sub.setName('send')
        .setDescription('Share a file via one-time qURL links')
        .addAttachmentOption(opt =>
          opt.setName('attachment')
            .setDescription('The file to share')
            .setRequired(true)
        );
      addSendRecipients(sub);
      return addSendDeliveryOptions(sub);
    });
  // /qurl detect — watermark attribution (#1101). Gated behind
  // DETECT_COMMAND_ENABLED (default OFF), like /qurl map: the connector
  // /api/detect backend 503/404s until the watermark stack is ACTIVATED (a
  // separate gated step AFTER the connector deploys), so detect stays DARK
  // until an operator flips the flag at activation — no visible-but-failing
  // command in the interim. The smoke test (e2e/tests/discord-commands.smoke.test.ts)
  // reads the same flag and asserts detect present-when-enabled / absent-when-off.
  if (cfg.DETECT_COMMAND_ENABLED) {
    builder.addSubcommand(sub =>
      sub.setName('detect')
        .setDescription('Find which recipient an image was watermarked for (this server)')
        .addAttachmentOption(opt =>
          opt.setName('image')
            .setDescription('The image to check for a qURL watermark')
            .setRequired(true)
        )
    );
  }
  if (cfg.MAP_COMMAND_ENABLED) {
    builder.addSubcommand(sub => {
      sub.setName('map')
        .setDescription('Share a Google Maps location via one-time qURL links')
        .addStringOption(opt =>
          opt.setName('location')
            .setDescription('Google Maps URL, or a place / address to search')
            .setRequired(true)
            // Picking a suggestion sends a `qurl_place:<id>` sentinel;
            // free text is resolved server-side via resolveLocation.
            .setAutocomplete(true)
            // 500 chars covers full Google Maps URLs (which can be
            // 200-400 chars after place params + coordinates) plus
            // headroom; not tied to the recipients-parser cap because
            // location is a single URL/address, not a token list.
            .setMaxLength(500)
        );
      addSendRecipients(sub)
        .addStringOption(opt =>
          opt.setName('location-name')
            .setDescription('Override the label shown to recipients (defaults to URL/address)')
            .setRequired(false)
            .setMaxLength(256)
        );
      return addSendDeliveryOptions(sub);
    });
  }
  builder.addSubcommand(sub =>
    sub.setName('revoke')
      .setDescription('Revoke links from a previous send')
  );
  builder.addSubcommand(sub =>
    sub.setName('help')
      .setDescription('Show qURL bot help')
  );
  return builder;
}

// Command definitions
const commands = [
  {
    // NOTE: adding/removing/renaming a `/qurl` subcommand? Update the
    // expected-set assertion in
    // `e2e/tests/discord-commands.smoke.test.ts` too — the smoke test
    // checks the subcommand names and, when map is enabled, that send and map
    // register identical shared options.
    data: buildQurlCommandData(config),
    // Shared admin handlers also accept guarded, unregistered cached aliases.
    async execute(interaction) {
      const sub = interaction.options.getSubcommand();

      // /qurl-admin setup — admin-only, configure API key for this server.
      //
      // Default flow (when AUTH0_* env vars are configured): replies with
      // a one-shot OAuth-redirect link. Admin clicks → Auth0 sign-in +
      // consent → server-side callback mints a guild-scoped API key on
      // qurl-service (admin-owned, billed to the admin's qURL account)
      // and persists it via the Store abstraction. NO API key paste.
      //
      // Fallback flow (AUTH0_* unset, e.g. early sandbox before Justin
      // registers the Auth0 app): reverts to the legacy modal-paste UX
      // so the bot stays usable until OAuth is wired end-to-end. The
      // fallback can be removed in a follow-up once the OAuth path is
      // live in prod.
      if (sub === 'setup') {
        if (!interaction.guildId) {
          return interaction.reply({ content: 'This command can only be used in a server, not in DMs.', flags: MessageFlags.Ephemeral });
        }
        if (!interaction.memberPermissions?.has(PermissionFlagsBits.ManageGuild)) {
          return interaction.reply({ content: 'Only server administrators can configure qURL.', flags: MessageFlags.Ephemeral });
        }

        // A malformed explicit connection policy must not silently downgrade
        // account binding to the legacy API-key paste flow. Keep unrelated
        // gateway/send operations available, but make setup fail closed until
        // the operator corrects the deployment value.
        if (config.isAuth0EmailConnectionRejected) {
          return rejectSetupForInvalidAuthPolicy(interaction);
        }

        // OAuth path — preferred when configured. After the rejected-policy
        // return above this equals isQurlOAuthConfigured; it stays the
        // setup-availability flag so the two cannot be split by a later edit.
        if (config.isQurlSetupAvailable) {
          // Fail-fast on encryption-at-rest BEFORE minting the OAuth
          // setup link — otherwise the admin clicks through, completes
          // the full Auth0 dance, and only then sees the 503 from
          // /oauth/qurl/start. Same actionable message as the legacy
          // modal-paste branch below; round-9.6 #4 surface symmetry.
          if (!process.env.KEY_ENCRYPTION_KEY) {
            logger.error('Refusing /qurl-admin setup (OAuth path): KEY_ENCRYPTION_KEY is not set');
            return interaction.reply({
              content: '❌ **qURL is not ready to accept setup on this server.**\n\n'
                + 'The bot operator needs to set `KEY_ENCRYPTION_KEY` (encryption-at-rest) before '
                + '/qurl-admin setup can store keys safely. Ask them to check the deployment env.',
              flags: MessageFlags.Ephemeral,
            });
          }
          const state = signQurlOAuthState(interaction.guildId, interaction.user.id);
          const startUrl = `${config.BASE_URL}/oauth/qurl/start?state=${encodeURIComponent(state)}`;
          return interaction.reply({
            content: '🔐 **Connect qURL to this server**\n\n'
              + `**[Click here to authorize qURL](${startUrl})**\n\n`
              + '_Open in this browser; the link expires in 15 minutes._',
            flags: MessageFlags.Ephemeral,
          });
        }

        // Legacy modal-paste fallback. Refuse to accept a guild API key
        // unless encryption-at-rest is configured. Falling through to the
        // crypto module's plaintext fallback would silently store a
        // billing-sensitive secret on disk.
        if (!process.env.KEY_ENCRYPTION_KEY) {
          logger.error('Refusing /qurl-admin setup: KEY_ENCRYPTION_KEY is not set');
          return interaction.reply({
            content: '❌ **qURL is not ready to accept API keys on this server.**\n\n' +
              'The bot operator needs to set `KEY_ENCRYPTION_KEY` (encryption-at-rest) before '
              + '/qurl-admin setup can store keys safely. Ask them to check the deployment env.',
            flags: MessageFlags.Ephemeral,
          });
        }

        // Open the flow row + render the button. The actual modal is
        // shown on button-click by handleSetupButton (see
        // dispatcher-side handlers above), which lets a deploy
        // between command and modal still resume cleanly — the
        // button is a persistent message component, not an
        // in-process Promise.
        //
        // Supersede semantics: supersedeOrCreate's version-gated
        // deleteFlow makes a second /qurl-admin setup a no-op when the
        // prior flow is mid-modal (the surviving peek returns the
        // awaiting_setup_modal row unchanged, and we surface the
        // "modal already open" wording from its registered
        // siblingMessage). When the prior flow is still pre-modal
        // — i.e. the admin walked away from the button — the
        // claim succeeds and they get a fresh button.
        await interaction.deferReply({ flags: MessageFlags.Ephemeral });
        const setupFlowId = flowIdForInteraction(interaction);
        let setupSupersede;
        try {
          setupSupersede = await supersedeOrCreate({
            flow_id: setupFlowId,
            stage: SETUP_STAGE_AWAITING_BUTTON,
            payload: null,
            ttl_seconds: SETUP_BUTTON_TTL_SECONDS,
          });
        } catch (err) {
          logger.warn('handleSetup: supersedeOrCreate threw', {
            flow_id: setupFlowId, error: err && err.message,
          });
          return interaction.editReply({
            content: 'Could not start a setup session — please try again.',
          });
        }
        if (!setupSupersede.created) {
          // Did not claim the slot. Three distinct branches:
          //   (a) surviving.stage === awaiting_setup_modal → admin
          //       has an in-flight modal; tell them to finish or
          //       wait for it to expire.
          //   (b) surviving is a sibling flow's row (e.g. revoke
          //       menu open) → surface its registered sibling
          //       message.
          //   (c) surviving is null OR an unregistered stage →
          //       generic "try again" fallback.
          logger.warn('handleSetup: supersedeOrCreate did not claim slot', {
            flow_id: setupFlowId,
            surviving_stage: setupSupersede.surviving?.stage ?? null,
          });
          const siblingMsg = siblingMessageForStage(setupSupersede.surviving?.stage);
          if (siblingMsg) {
            return interaction.editReply({ content: siblingMsg });
          }
          return interaction.editReply({
            content: 'Could not start a setup session — please try again.',
          });
        }

        const setupButton = new ButtonBuilder()
          .setCustomId(SETUP_BUTTON_CUSTOM_ID)
          .setLabel('Configure qURL')
          .setStyle(ButtonStyle.Primary);
        return interaction.editReply({
          content: '🔐 **Connect qURL to this server**\n\n'
            + 'Click below to open the API-key form. Inputs are NOT recorded in Discord audit logs.',
          components: [new ActionRowBuilder().addComponents(setupButton)],
        });
      }

      // /qurl-admin status — verify the stored key. Gate behind ManageGuild because
      // the response discloses the key's prefix, granted scopes and provenance,
      // and each run spends a KMS decrypt plus one upstream qURL API call.
      // No cooldown, accepted deliberately: the same ManageGuild admin can
      // already drive the uncooled per-submit validation call in the setup
      // modal above, so this adds no new abuse class, and the send/detect
      // buckets are kept separate by design (a status check must never lock
      // out a send).
      if (sub === 'status') {
        if (!interaction.memberPermissions?.has(PermissionFlagsBits.ManageGuild)) {
          return interaction.reply({
            content: '❌ Only server administrators can view qURL configuration status.',
            flags: MessageFlags.Ephemeral,
          });
        }
        await interaction.deferReply({ flags: MessageFlags.Ephemeral });
        let guildConfig;
        try {
          guildConfig = await db.getGuildConfig(interaction.guildId);
        } catch {
          logger.warn('qURL status configuration read failed', {
            guild_id: interaction.guildId,
          });
          return interaction.editReply({
            content: '⚠️ **The qURL status check could not be completed.**\n'
              + 'Please try `/qurl-admin status` again later.',
          });
        }
        if (guildConfig) {
          // Start the independent key/service work before checking guild
          // membership. Catch inside the promise so an early rejection cannot
          // become unhandled while members.fetch is still pending.
          const identityResultPromise = (async () => {
            let failureStage = 'key_store';
            try {
              // Keep the decrypted key out of the longer-lived config object,
              // even though the explicit accessor costs a second DDB read.
              const apiKey = await db.getGuildApiKey(interaction.guildId);
              if (!apiKey) return { keyUnavailable: true };
              failureStage = 'qurl_service';
              return { identity: await getIdentity(apiKey, interaction.guildId) };
            } catch (error) {
              return { error, failureStage, identityFailed: true };
            }
          })();

          // Stored values are normally a Discord snowflake and an ISO timestamp;
          // escape Markdown and bound their source form so a corrupt row cannot
          // inject content or crowd out the key verdict/offboarding guidance.
          const sanitizedConfiguredBy = sanitizeContentLabel(guildConfig.configured_by, 64);
          const configuredByDisplay = sanitizedConfiguredBy || 'unknown';
          const updatedAtDisplay = sanitizeContentLabel(guildConfig.updated_at, 64) || 'unknown';
          const configuredByReference = sanitizedConfiguredBy
            ? `<@${configuredByDisplay}>`
            : configuredByDisplay;

          // #185 admin-offboarding nudge: the qURL key is owned by the
          // admin who ran setup (Auth0 sub claim); usage bills to their
          // qURL account even after they leave. Surface a passive notice
          // so a remaining ManageGuild admin knows to rerun setup to
          // take over billing. Best-effort — a Discord API blip just
          // omits the notice rather than failing the whole status read.
          let originalAdminLeftNotice = '';
          if (sanitizedConfiguredBy) {
            try {
              // fetch takes the raw snowflake; the sanitized form only gates
              // an empty/all-stripped row.
              await interaction.guild.members.fetch(guildConfig.configured_by);
            } catch (err) {
              // discord.js throws DiscordAPIError code 10007 ("Unknown
              // Member") when the user is no longer in the guild. Any
              // other error (rate limit, transient) → skip the nudge
              // silently rather than mis-flag a present admin.
              if (err?.code === 10007) {
                originalAdminLeftNotice =
                  '\n\n⚠️ The admin who originally ran `/qurl-admin setup` (<@' +
                  configuredByDisplay + '>) has left this server. ' +
                  'qURL usage continues to bill to their layerv.ai account. ' +
                  'A current `ManageGuild` admin can run `/qurl-admin setup` again to take over billing.';
              }
            }
          }
          const configurationDetails =
            `Configured by: ${configuredByReference}\n` +
            `Last updated: ${updatedAtDisplay}` +
            originalAdminLeftNotice;

          const {
            identity,
            error,
            keyUnavailable,
            failureStage,
            identityFailed,
          } = await identityResultPromise;
          const reconnectCopy = 'Re-run `/qurl-admin setup` to connect a valid key.\n\n';
          // Every outcome renders as `<verdict copy> + configurationDetails`;
          // only the verdict differs, so build it here and share one exit.
          const STATUS_SCOPE_DISPLAY_MAX = 10;
          const STATUS_CONTENT_MAX = 2000;
          let verdict;
          if (keyUnavailable) {
            logger.warn('qURL status key unavailable', { guild_id: interaction.guildId });
            verdict = '❌ **The stored qURL key is unavailable.**\n\n' + reconnectCopy;
          } else if (identityFailed) {
            const status = qurlApiErrorStatus(error);
            logger.warn('qURL status identity check failed', {
              guild_id: interaction.guildId,
              status,
              code: error?.code ?? null,
              error_name: error?.name ?? null,
              failure_stage: failureStage,
            });
            // Same 401/403 → "invalid key" policy the legacy setup validator
            // applies at the top of this file; #1370 tracks consolidating them.
            verdict = failureStage === 'qurl_service' && (status === 401 || status === 403)
              ? '❌ **The stored qURL key is revoked or invalid.**\n\n' + reconnectCopy
              : '⚠️ **Stored qURL configuration found, but the key check could not be completed.**\n'
                + 'Please try `/qurl-admin status` again later.\n\n';
          } else {
            // Shows the service-issued `key_prefix` where this handler used to
            // show a sha256 fingerprint of the stored key. That earlier decision
            // rejected echoing key bytes; it is superseded because /v1/me
            // designates `key_prefix` as the non-secret display identifier
            // (the CLI identity model does the same), the OAuth setup success
            // DM already shows this exact prefix to the same admin, and #1360
            // asks for the prefix so the admin can match the two. The
            // brute-force concern does not apply: the prefix is a namespace
            // plus a few leading bytes of a >=28-char key, and it only reaches
            // the ManageGuild-gated ephemeral reply. Trade-off: a fingerprint
            // answered "same key as last setup?"; two keys can share a prefix.
            // TODO(upstream-contract): keep this aligned with /v1/me.
            //
            // Inline-code content renders backslashes literally, so strip
            // backticks instead of applying general Markdown escaping. The
            // plain display-name sanitizer also strips controls and codepoint-
            // caps each value; the slice below bounds how many scopes render.
            const sanitizeIdentityValue = (value) =>
              sanitizeDisplayNamePlain(value, { fallback: '' }).replace(/`/g, '');
            // getIdentity enforces this shape; default anyway so a contract
            // drift lands in the verdict machinery, not the generic catch-all.
            const { key_prefix: rawKeyPrefix, scopes: allScopes = [] } = identity?.api_key ?? {};
            // TODO(upstream-contract): 12 mirrors qurl-service's
            // APIKeyDisplayPrefixLength (the issued prefix is exactly 12 bytes),
            // so a service prefix renders whole and matches the setup DM; a
            // wider upstream field is capped rather than echoed.
            const keyPrefix = capUtf16Units(sanitizeIdentityValue(rawKeyPrefix), 12) || 'unknown';
            const shownScopes = allScopes.slice(0, STATUS_SCOPE_DISPLAY_MAX)
              .map(scope => `\`${sanitizeIdentityValue(scope) || 'unnamed'}\``);
            const renderHealthyVerdict = (shownScopeCount) => {
              const renderedScopes = shownScopes.slice(0, shownScopeCount);
              const omittedScopeCount = allScopes.length - renderedScopes.length;
              let scopes = renderedScopes.join(', ');
              if (omittedScopeCount > 0) {
                scopes += scopes
                  ? `, _+${omittedScopeCount} more_`
                  : `_${omittedScopeCount} scopes omitted_`;
              }
              if (!scopes) scopes = '_none_';
              return `✅ **qURL is configured**\n` +
                `Key prefix: \`${keyPrefix}\`\n` +
                `Scopes: ${scopes}\n`;
            };
            verdict = renderHealthyVerdict(shownScopes.length);
            // Keep Discord's 2,000-UTF-16-unit content limit an invariant of
            // the code rather than of a copy budget spread across the fields
            // above. Drop whole scope entries instead of slicing Markdown in
            // the middle of an inline-code span or UTF-16 surrogate pair. The
            // field caps keep the zero-scope render under the limit.
            for (let n = shownScopes.length; n > 0 && (verdict + configurationDetails).length > STATUS_CONTENT_MAX; n -= 1) {
              verdict = renderHealthyVerdict(n - 1);
            }
          }
          return interaction.editReply({
            content: verdict + configurationDetails,
            allowedMentions: { parse: [] },
          });
        }
        // Branch the not-configured copy on the active setup flow so
        // the instructions match what /qurl-admin setup actually accepts —
        // OAuth-redirect (no api_key arg) vs legacy modal-paste.
        // Pre-OAuth this hardcoded `setup api_key:lv_live_your_key_here`,
        // which never matched the modal flow either; fixed in round-9.6
        // alongside the OAuth-redirect path documentation.
        let notConfiguredCopy;
        if (config.isAuth0EmailConnectionRejected) {
          notConfiguredCopy = SETUP_AUTH_POLICY_UNAVAILABLE_MSG;
        } else if (config.isQurlSetupAvailable) {
          notConfiguredCopy = '❌ **qURL is not configured for this server.**\n\n'
            + 'Run `/qurl-admin setup` to connect — you\'ll be redirected to layerv.ai to authorize, '
            + 'and the bot will mint an API key bound to your server. Only server administrators can run setup.';
        } else {
          notConfiguredCopy = '❌ **qURL is not configured for this server.**\n\n'
            + '1. Sign up at **https://layerv.ai** to get your API key\n'
            + '2. Run `/qurl-admin setup` and paste the key into the modal\n\n'
            + 'Only server administrators can run setup.';
        }
        return interaction.editReply({
          content: notConfiguredCopy,
        });
      }

      // Stale-client rename reply — `/qurl file` was renamed to
      // `/qurl send`. Discord clients cache slash registrations and
      // can submit the old name for up to ~1h after a global rollout
      // (near-instant on guild-scoped registration). Caught here
      // BEFORE API_KEY_GATED_SUBCOMMANDS so unconfigured guilds get
      // the rename hint instead of "qURL is not configured".
      if (sub === 'file') {
        logger.debug('qurl_file_renamed_reply: stale-client /qurl file submission caught by rename gate', {
          user_id: interaction.user?.id,
          guild_id: interaction.guildId,
        });
        return interaction.reply({
          content: '⚠️ `/qurl file` has been renamed to `/qurl send`. Please use `/qurl send` instead — your Discord client may take up to an hour to refresh the command list.',
          flags: MessageFlags.Ephemeral,
        });
      }

      // Gate: require guild API key for send/map/revoke (the set
      // in API_KEY_GATED_SUBCOMMANDS, hoisted to module scope).
      //
      // For /qurl send + /qurl map this read is a fail-fast presence
      // check — the resolved value is intentionally NOT threaded
      // through to the back-half. handleConfirmSendClick re-fetches at
      // Send-click time so a key rotation during the 3-minute confirm-
      // card window is honored. Threading resolvedApiKey through here
      // would break that rotation-safety property: a future optimization
      // pass that "saves the redundant DDB read" would silently
      // re-introduce stale-key dispatches. The handleQurlSlashSend
      // docstring documents the contract explicitly.
      let resolvedApiKey = null;
      if (API_KEY_GATED_SUBCOMMANDS.has(sub)) {
        const guildApiKey = interaction.guildId ? await db.getGuildApiKey(interaction.guildId) : null;
        if (!guildApiKey && !config.QURL_API_KEY) {
          return interaction.reply({
            content: '❌ **qURL is not configured for this server.**\n\n' +
              'A server admin needs to run `/qurl-admin setup` first.\n' +
              'Sign up at **https://layerv.ai** to get your API key.',
            flags: MessageFlags.Ephemeral,
          });
        }
        resolvedApiKey = guildApiKey || config.QURL_API_KEY;
      }

      // /qurl send and /qurl map deliberately don't accept the
      // dispatcher-resolved apiKey — handleConfirmSendClick re-fetches
      // at Send time so a mid-flow rotation still uses the live key.
      // The dispatcher's API_KEY_GATED_SUBCOMMANDS gate above is the
      // fail-fast presence check.
      if (sub === 'send') return handleQurlSend(interaction);
      // /qurl detect resolves its own API key inside the handler (BYOK →
      // global fallback) — deliberately NOT in API_KEY_GATED_SUBCOMMANDS,
      // so it dispatches here without the dispatcher-level key gate.
      if (sub === 'detect') {
        if (!config.DETECT_COMMAND_ENABLED) {
          // Stale client with a cached command def that still lists `detect`
          // after the toggle was off — expected post-deploy traffic, not an
          // error (same cache-TTL story as /qurl map below). Enforcing the flag
          // HERE too means a stale detect can't reveal a recipient even if the
          // backend is live but the flag is off.
          logger.debug('qurl_detect_disabled_reply: stale-client /qurl detect submission caught by toggle gate', {
            user_id: interaction.user?.id,
            guild_id: interaction.guildId,
          });
          return interaction.reply({ content: QURL_DETECT_DISABLED_REPLY, flags: MessageFlags.Ephemeral });
        }
        return handleQurlDetect(interaction);
      }
      if (sub === 'map') {
        if (!config.MAP_COMMAND_ENABLED) {
          // Debug (not warn) — expected post-deploy traffic from
          // clients with cached command defs, not an error. Cache
          // TTL depends on registration mode: up to 1h on global
          // (multi-tenant) registration, near-instant on guild-
          // scoped (single-GUILD_ID). Grep this line to see decay.
          logger.debug('qurl_map_disabled_reply: stale-client /qurl map submission caught by toggle gate', {
            user_id: interaction.user?.id,
            guild_id: interaction.guildId,
          });
          return interaction.reply({ content: QURL_MAP_DISABLED_REPLY, flags: MessageFlags.Ephemeral });
        }
        return handleQurlMap(interaction);
      }
      if (sub === 'revoke') return handleRevoke(interaction, resolvedApiKey);
      if (sub === 'help') {
        // Section order: user-facing flow first (Getting started → How it
        // works), then admin-only setup (now the OAuth-redirect flow per
        // PR #177), then glossary (Terms), then operational caveat
        // The "Setting up" section pivots on whether qURL OAuth is
        // configured. When unset (sandbox before Auth0 secrets land), keep
        // the legacy API-key wording so help matches what /qurl-admin setup does.
        // The Add to Discord entrypoint has an additional Discord client-
        // secret dependency, so advertise it only when the full customer
        // install flow is ready.
        let oauthSetupSection;
        if (config.isAuth0EmailConnectionRejected) {
          oauthSetupSection = SETUP_AUTH_POLICY_UNAVAILABLE_MSG + '\n\n';
        } else if (config.isQurlSetupAvailable) {
          oauthSetupSection = '**Setting up (for Admins):**\n'
            + '  `/qurl-admin setup` — connect qURL via OAuth (admin only). Click the link, sign in to layerv.ai, consent. No API key paste.\n'
            + '  `/qurl-admin status` — verify the saved key and show its prefix and scopes (admin only)\n\n';
        } else {
          oauthSetupSection = '**Setting up (for Admins):**\n'
            + '  `/qurl-admin setup` — configure your API key (admin only)\n'
            + '  `/qurl-admin status` — verify the saved key and show its prefix and scopes (admin only)\n\n';
        }
        const discordInstallSection = config.isDiscordInstallConfigured
          ? '_Adding the bot to a new server?_ Use the "Add to Discord" link on **https://layerv.ai** — '
            + 'it walks you through server selection, permissions consent, and qURL connection in one click chain.\n\n'
          : '';
        // `cmd` is used in two spots (Terms + Large servers); a
        // single token keeps them in lockstep across copy edits.
        const mapCopy = config.MAP_COMMAND_ENABLED
          ? {
            sectionVerb: 'Share resources',
            bullet: '  `/qurl map` — share a Google Maps location via one-time qURL links\n',
            runLine: '\t1. Run `/qurl send` (attach a file) or `/qurl map` (paste a Google Maps URL or address)\n',
            resource: 'the file or location',
            cmd: '`/qurl send` or `/qurl map`',
          }
          : {
            sectionVerb: 'Share files',
            bullet: '',
            runLine: '\t1. Run `/qurl send` (attach a file)\n',
            resource: 'the file',
            cmd: '`/qurl send`',
          };
        return interaction.reply({
          content: '**qURL — Help**\n\n' +
            `**Getting started — ${mapCopy.sectionVerb} securely via one-time links:**\n` +
            '  `/qurl send` — share a file with users via one-time qURL links\n' +
            mapCopy.bullet +
            '  `/qurl revoke` — revoke links from a previous send\n' +
            '  `/qurl help` — show this message\n\n' +
            '**How it works:**\n' +
            // Leading tab on "1." keeps Discord's markdown parser from
            // treating it as the start of an ordered list (which would
            // renumber it relative to the subsequent lines and visually
            // misalign "1." with "2.", "3.", "4."). The tab indent now
            // matches the two-space indent below, but bypasses the list
            // auto-formatter.
            mapCopy.runLine +
            '  2. Optionally `recipients:@a @b @role` (up to 25 users via @mentions or role expansion) — '
              + 'leave blank to pick from a menu (up to 10 at a time)\n' +
            '  3. Confirm the card, then click **Send**\n' +
            '  4. Recipients get a one-time link by DM that self-destructs on first access (or when the expiry elapses)\n\n' +
            oauthSetupSection +
            discordInstallSection +
            `**Terms:** a *protected resource* is ${mapCopy.resource} you're sharing. ` +
            'A *qurl* (or *access link*) is the single-use URL that delivers it. ' +
            `You create a qurl for a protected resource each time you run ${mapCopy.cmd}.\n\n` +
            `**Large servers (~1000+ members):** ${mapCopy.cmd} with role @mentions ` +
            'may skip members the bot has not yet cached locally (the bot fetches members lazily, ' +
            'and very large servers may not be fully populated). ' +
            'If you need to reach a specific person for sure, use an explicit user @mention instead of a role.\n\n' +
            'Learn more at **https://layerv.ai**.',
          flags: MessageFlags.Ephemeral,
        });
      }
    },
  },
  {
    data: new SlashCommandBuilder()
      .setName('qurl-admin')
      .setDescription('Manage qURL setup and configuration status')
      .setDefaultMemberPermissions(PermissionFlagsBits.ManageGuild)
      .addSubcommand(sub => sub.setName('setup').setDescription('Configure qURL for this server'))
      .addSubcommand(sub => sub.setName('status').setDescription("Verify the saved qURL key, prefix, and scopes")),
    async execute(interaction) {
      const sub = interaction.options.getSubcommand();
      if (sub !== 'setup' && sub !== 'status') {
        return interaction.reply({ content: 'Unknown qURL admin command.', flags: MessageFlags.Ephemeral });
      }
      // Reuse the same runtime permission gates as cached setup/status aliases.
      return commands.find(command => command.data.name === 'qurl').execute(interaction);
    },
  },
];

// TODO(upstream-contract): Discord keeps global and guild command namespaces separate.
// Clear only an explicitly inactive scope; never enumerate joined guilds at boot.
async function purgeStaleCommandNamespace({ rest, route, scope }) {
  try {
    const existing = await rest.get(route);
    if (!Array.isArray(existing)) throw new Error('Command namespace response must be an array');
    if (existing.length === 0) return;
    await rest.put(route, { body: [] });
    logger.info('Purged stale command namespace', { scope, purged_count: existing.length });
  } catch (error) {
    logger.warn('Could not purge stale command namespace', { scope, error: error.message });
  }
}

// Register the active scope before cleaning inactive scopes. REST + appId work
// for both the legacy Client and the cache-free gateway shim.
async function registerCommands({ rest, appId }) {
  const commandData = commands.map(cmd => {
    const data = cmd.data.toJSON();
    // Deprecated by Discord in favour of `contexts`; never send both.
    // Omitting the legacy field also avoids relying on builder defaults.
    delete data.dm_permission;
    // These fields are global-command-only in Discord's API. Pin them
    // explicitly so Developer Portal defaults cannot expose the server-
    // scoped qURL workflows through user installs or DM contexts.
    if (!config.GUILD_ID) {
      data.integration_types = [ApplicationIntegrationType.GuildInstall];
      data.contexts = [InteractionContextType.Guild];
    } else {
      delete data.integration_types;
      delete data.contexts;
    }
    return data;
  });

  try {
    if (config.GUILD_ID) {
      // Guild-scoped registration: commands appear instantly in just this
      // guild. Used by single-guild deployments where fast command
      // iteration matters more than appearing in other guilds.
      logger.info(`Registering ${commands.length} slash commands to guild ${config.GUILD_ID}...`);
      await rest.put(Routes.applicationGuildCommands(appId, config.GUILD_ID), { body: commandData });
    } else {
      // Global registration: commands appear in every guild the bot joins.
      // Discord caches global commands for up to 1 hour, so newly-added
      // commands may take that long to propagate. Used for multi-tenant
      // deployments (customers invite the bot to their own servers).
      logger.info(`Registering ${commands.length} slash commands globally (multi-tenant mode): ${commands.map(c => c.data.name).join(', ')}`);
      await rest.put(Routes.applicationCommands(appId), { body: commandData });
    }
    if (config.GUILD_ID && config.PREVIOUS_COMMAND_SCOPE === 'global') {
      await purgeStaleCommandNamespace({ rest, route: Routes.applicationCommands(appId), scope: 'global' });
    }
    if (config.PREVIOUS_COMMAND_SCOPE && config.PREVIOUS_COMMAND_SCOPE !== 'global' && config.PREVIOUS_COMMAND_SCOPE !== config.GUILD_ID) {
      await purgeStaleCommandNamespace({
        rest, route: Routes.applicationGuildCommands(appId, config.PREVIOUS_COMMAND_SCOPE), scope: 'previous_guild',
      });
    }
    logger.info('Slash commands registered.');
  } catch (error) {
    logger.error('Failed to register commands', { error: error.message });
  }
}

// Discord's 3 s interaction-acknowledgement deadline surfaces as a
// REST 10062 / "Unknown interaction" error from discord.js when the bot
// tries to reply after the token has expired. Detect both the numeric
// code (preferred — discord.js DiscordAPIError preserves it) and the
// message text (fallback for wrapped-error shapes that drop .code).
// The regex accepts an optional "Class: " or "Class[code]: " prefix
// common to wrapped shapes — e.g. `RESTJSONError: Unknown interaction`
// or discord.js's own `DiscordAPIError[10062]: Unknown interaction` —
// but rejects trailing content like "Unknown interaction type X" to
// avoid misclassification.
const ACK_TIMEOUT_MSG_RE = /^(?:[A-Za-z][A-Za-z0-9]*(?:\[\d+\])?: )?Unknown interaction$/;
function isAckTimeoutError(err) {
  if (!err) return false;
  if (err.code === 10062) return true;
  return typeof err.message === 'string' && ACK_TIMEOUT_MSG_RE.test(err.message);
}

// Below this length, autocomplete suggestions are noise (single-letter
// prefixes match thousands of places) and the per-keystroke Places
// cost isn't justified.
const AUTOCOMPLETE_MIN_QUERY_LENGTH = 2;

// Sampled-warn cadence for autocomplete failures. The catch logs at
// `debug` per-call to avoid keystroke-rate log spam during a Places
// outage; this counter emits one `warn` per AUTOCOMPLETE_FAILURE_LOG_BURST
// failures so SRE has a coarse signal that autocomplete is degraded
// (vs. no traffic) without flooding logs. resolveLocation's send-time
// `warn` carries the load-bearing signal — this is secondary visibility.
const AUTOCOMPLETE_FAILURE_LOG_BURST = 50;
let autocompleteFailureBurst = 0;

// Discord caps each choice's `name` (label) and `value` (handler input)
// at 100 chars. Labels (name + address) often need truncation; values
// (`qurl_place:<placeId>`) almost never do, but we drop pathological
// ones so one bad result doesn't fail the whole response.
const AUTOCOMPLETE_CHOICE_NAME_MAX = 100;
const AUTOCOMPLETE_CHOICE_VALUE_MAX = 100;
const AUTOCOMPLETE_MAX_CHOICES = 25;

// Per Discord's contract, MUST respond within 3 s. Two-layer error
// handling: the inner try catches Places I/O failures (ticks the
// sampled SRE counter; that's its intent — "Places is degraded"); the
// outer try catches anything else (early-return respond([]) throws on
// expired token, etc.) without ticking the counter, since those aren't
// Places-degradation signals.
async function handleAutocomplete(interaction) {
  try {
    // `return await` (not bare `return`) so a rejected `respond([])`
    // promise is caught by the outer try/catch instead of leaking out
    // of the async function unhandled. Each early-return is the
    // contract handler — surface the rejection through the outer
    // recovery path instead of propagating up to the dispatch caller.
    if (interaction.commandName !== 'qurl') {
      return await interaction.respond([]);
    }
    // Reject DM and user-install-only autocomplete — handleQurlMap
    // rejects those contexts at submit time, but Discord could still
    // deliver autocomplete while global registration changes propagate.
    // Without this guard the invalid interaction would burn the operator's
    // global GOOGLE_MAPS_API_KEY quota for a send that's about to fail.
    if (isUnsupportedQurlContext(interaction)) {
      return await interaction.respond([]);
    }
    const subcommand = interaction.options.getSubcommand(false);
    const focused = interaction.options.getFocused(true);
    if (subcommand !== 'map' || focused?.name !== 'location') {
      return await interaction.respond([]);
    }
    // Feature-flag gate. When MAP_COMMAND_ENABLED is off the map
    // subcommand isn't registered, so Discord shouldn't deliver an
    // autocomplete for it — but a stale client racing the registration
    // roll could. Short-circuit before searchPlaces so we don't burn
    // operator GOOGLE_MAPS_API_KEY quota on a submit that will be
    // rejected by the dispatcher anyway.
    if (!config.MAP_COMMAND_ENABLED) {
      return await interaction.respond([]);
    }
    const rawQuery = (focused.value || '').trim();
    if (rawQuery.length < AUTOCOMPLETE_MIN_QUERY_LENGTH) {
      return await interaction.respond([]);
    }
    // A pasted URL is already a stable identifier — parseLocationInput
    // passes it through verbatim and suggestions would just clutter.
    if (/^https?:\/\//i.test(rawQuery)) {
      return await interaction.respond([]);
    }

    const userId = interaction.user?.id;
    if (typeof userId !== 'string' || userId.length === 0 || userId.length > 64) return await interaction.respond([]);

    let results;
    try {
      results = await searchPlaces(rawQuery, () => takeAutocompleteToken(userId));
    } catch (err) {
      // Per-call log at debug so keystroke-rate failures don't spam.
      logger.debug('autocomplete handler failed', {
        command: interaction.commandName,
        error: err && err.message,
      });
      // Sampled SRE signal: emit one warn per BURST failures so a
      // Places outage is visible (vs. silent autocomplete-only degrade).
      if (++autocompleteFailureBurst >= AUTOCOMPLETE_FAILURE_LOG_BURST) {
        logger.warn('autocomplete handler failure burst', {
          count: autocompleteFailureBurst,
          error: err && err.message,
        });
        autocompleteFailureBurst = 0;
      }
      return await interaction.respond([]);
    }

    const choices = [];
    for (const p of results) {
      if (choices.length >= AUTOCOMPLETE_MAX_CHOICES) break;
      // Skip dud entries early instead of relying on submit-time decode
      // to reject them — saves the user a "place no longer available"
      // error on a malformed Google response.
      if (!PLACE_ID_SHAPE_RE.test(p.placeId)) {
        // Debug, not warn: would fire per-keystroke during a Google
        // place_id format drift, drowning logs. Operator can grep for
        // this when investigating "why is my dropdown empty?" — that's
        // the upstream-shape-drift signal.
        logger.debug('autocomplete: dropped prediction (place_id failed shape check)', {
          place_id: p.placeId,
        });
        continue;
      }
      // Places marks `main_text` and `description` as optional; if both
      // are missing, searchPlaces returns `name: undefined` and the
      // label would render as the literal string "undefined". Discord
      // also rejects empty/whitespace names, so just skip.
      if (!p.name) continue;
      const value = encodePlaceIdSentinel(p.placeId);
      if (value.length > AUTOCOMPLETE_CHOICE_VALUE_MAX) continue;
      const label = p.address ? `${p.name} — ${p.address}` : p.name;
      // Discord validates name length in UTF-16 code units, not code
      // points, so we cap by `.length`. Back off by 1 if the boundary
      // would leave a lone high surrogate so we don't ship a
      // half-emoji (which Discord renders as tofu). Deliberately NOT
      // `safeCodepointSlice` — that helper counts codepoints, which
      // can ship a string whose `.length` > 100 for emoji-heavy
      // labels (each surrogate pair is 2 UTF-16 units but 1 codepoint).
      // Always-check (not just on truncation): a label that's exactly
      // 100 UTF-16 units AND ends with a lone high surrogate would
      // otherwise slip through the fast path.
      let end = Math.min(label.length, AUTOCOMPLETE_CHOICE_NAME_MAX);
      if (end > 0) {
        const lastUnit = label.charCodeAt(end - 1);
        if (lastUnit >= 0xD800 && lastUnit <= 0xDBFF) end -= 1;
      }
      const name = end === label.length ? label : label.slice(0, end);
      choices.push({ name, value });
    }
    return await interaction.respond(choices);
  } catch (err) {
    // Non-Places-I/O failure (respond() on expired token, getSubcommand
    // throw on malformed interaction, etc.). Doesn't advance the burst
    // counter — those aren't Places-degradation signals.
    logger.debug('autocomplete handler unhandled', {
      command: interaction.commandName,
      error: err && err.message,
    });
    try { await interaction.respond([]); } catch { /* ignore */ }
    return undefined;
  }
}

// Handle command interactions
async function handleCommand(interaction) {
  // Autocomplete fires per-keystroke and must respond within 3s. Route
  // it off the chat-input path so the autocomplete handler doesn't drag
  // in the metrics + mode-flip-defense overhead that handleQurlSlashSend
  // and friends need.
  if (interaction.isAutocomplete()) return handleAutocomplete(interaction);

  if (!interaction.isChatInputCommand()) return;

  // handler_duration_ms is wall-clock from entry to emit, NOT user-
  // perceived ACK latency — for commands that defer + run a background
  // task, this captures the whole operation. Edge-to-ACK is Phase 2.
  // hrtime.bigint() instead of Date.now() so an NTP step backward
  // can't produce a negative duration.
  const handlerStart = process.hrtime.bigint();
  const commandName = interaction.commandName;
  const emitInteractionMetric = (success, failureType) => {
    // Number(ns) / 1_000_000 (NOT BigInt division then Number) so a
    // sub-millisecond handler reports a fractional ms instead of
    // truncating to 0.
    const handler_duration_ms = Number(process.hrtime.bigint() - handlerStart) / 1_000_000;
    logger.audit(AUDIT_EVENTS.INTERACTION_HANDLED, {
      command_name: commandName,
      success,
      failure_type: failureType,
      handler_duration_ms,
    });
  };

  // Global command updates can take up to an hour to propagate. Reject any
  // stale DM or user-install-only invocation centrally so no subcommand can
  // bypass the guild-install product boundary during that window. A user-
  // installed command may still be invoked inside a guild, so guildId alone
  // is not sufficient; inspect Discord's authorizing-integration mapping too.
  if (isUnsupportedQurlContext(interaction)) {
    try {
      await interaction.reply({
        content: UNSUPPORTED_CONTEXT_MSG,
        flags: MessageFlags.Ephemeral,
      });
      emitInteractionMetric(false, 'unsupported_context');
    } catch (err) {
      logger.warn('Failed to reject command in unsupported context', {
        command: interaction.commandName, error: err.message,
      });
      emitInteractionMetric(false, isAckTimeoutError(err) ? 'ack_timeout' : 'reply_failed');
    }
    return;
  }

  // Defense-in-depth against stale registrations: a guild served by an
  // older deploy may still list commands this build no longer ships
  // (the GitHub account-linking and contributor-role commands, removed
  // in #1026), because Discord's guild and global namespaces don't
  // purge each other on a new .set() call. registerCommands cleans inactive
  // scopes at boot, but Discord's cache can lag. Look the handler
  // up in the shipped set so a stale registration can't dispatch into a
  // path that no longer exists.
  const command = commands.find(cmd => cmd.data.name === interaction.commandName);
  if (!command) {
    // The interaction is for a command we know exists globally (Discord
    // only dispatches registered commands to us) but is not in the
    // currently-active set — so it's a stale guild-scoped registration
    // from a previous deploy in a different mode. Acknowledge the
    // interaction so the user sees a clear "no longer available" reply
    // instead of Discord's 3-second "This interaction failed" timeout.
    // Defensive: wrap in try/catch since the interaction may already
    // have been responded to by a race elsewhere.
    try {
      await interaction.reply({
        content: 'This command is no longer available in this server.',
        flags: MessageFlags.Ephemeral,
      });
      emitInteractionMetric(false, 'unknown_command');
    } catch (err) {
      logger.warn('Failed to reply to stale command interaction', {
        command: interaction.commandName, error: err.message,
      });
      // Reply throw within Discord's 3 s window surfaces as the user-
      // visible "did not respond" dialog. Tag it distinctly so the
      // ack_timeout alarm catches it without being washed out by other
      // failures.
      emitInteractionMetric(false, isAckTimeoutError(err) ? 'ack_timeout' : 'reply_failed');
    }
    return;
  }

  try {
    await command.execute(interaction);
    emitInteractionMetric(true, null);
  } catch (error) {
    logger.error(`Error executing ${interaction.commandName}`, { error: error.message });
    // Any throw after setCooldown (e.g. deferReply failure, modal timeout path
    // that bubbles) would leave the user in a 30s block for an action that
    // never completed. clearCooldown is idempotent and a no-op for commands
    // that don't use it, so call unconditionally.
    clearCooldown(interaction.user.id);
    const reply = {
      content: 'There was an error executing this command.',
      flags: MessageFlags.Ephemeral,
    };

    // failureType reflects the FIRST failure observed. If the follow-up
    // reply also fails for a non-ack reason, keep the original
    // handler_error — execute() is the more meaningful signal. Only
    // ack_timeout on the reply may override (user-visible "did not
    // respond"). Asymmetric vs. the stale-registration path above
    // (which tags reply_failed) because there's no prior execute() there.
    let failureType = isAckTimeoutError(error) ? 'ack_timeout' : 'handler_error';
    try {
      if (interaction.replied || interaction.deferred) {
        await interaction.followUp(reply);
      } else {
        await interaction.reply(reply);
      }
    } catch (replyError) {
      logger.error('Failed to send error response', { error: replyError.message });
      if (isAckTimeoutError(replyError)) failureType = 'ack_timeout';
    }
    emitInteractionMetric(false, failureType);
  }
}

// Wire flow handlers into the dispatcher at module-load time.
// flow-dispatch's `registerFlow` is the single source of truth for
// the customId → handler routing table; doing this here (rather than
// in index.js) co-locates the registration with the handler
// implementation so future readers don't have to grep two modules to
// understand the routing.
// Each registerFlow co-locates two things at the same site: the
// customId → handler binding (for dispatch) and the stage →
// siblingMessage binding (for cross-flow supersede disambiguation).
// A future flow that adds a new awaiting_* stage SHOULD register
// its siblingMessage here so peer flows' supersede peeks can
// surface actionable wording instead of falling through to the
// generic "try again." Stages without a registered message (e.g.
// short-lived ones nobody else would race against) are fine to
// omit — peers will fall through to the generic copy.
registerFlow(REVOKE_SELECT_CUSTOM_ID, {
  expectedStage: REVOKE_STAGE_AWAITING_SELECT,
  handler: handleRevokeSelect,
  siblingMessage: 'You have a `/qurl revoke` menu open in this channel — finish or cancel it first.',
});
registerFlow(SETUP_BUTTON_CUSTOM_ID, {
  expectedStage: SETUP_STAGE_AWAITING_BUTTON,
  handler: handleSetupButton,
  // Cross-flow collision wording: a /qurl revoke supersede whose
  // peek finds a row at awaiting_setup_button surfaces this — its
  // surviving.stage !== awaiting_revoke_select so it doesn't claim,
  // but a generic "try again" wouldn't tell the admin what to do.
  // (A same-flow-type /qurl-admin setup rerun never hits this path: its
  // own supersedeOrCreate matches the button stage and version-
  // gated-deletes the prior row in one round-trip.)
  siblingMessage: 'You have a `/qurl-admin setup` button waiting in this channel — click it or wait for it to expire.',
});
registerFlow(SETUP_MODAL_CUSTOM_ID, {
  expectedStage: SETUP_STAGE_AWAITING_MODAL,
  handler: handleSetupModal,
  siblingMessage: 'You already have a `/qurl-admin setup` modal open — finish that one, or wait for it to expire.',
});

// /qurl send + /qurl map confirm-card components. All three customIds
// share the same expectedStage — they're three component types
// (button + user-select + button) attached to the same confirm-card
// message, all routed by stage. siblingMessage is registered on the
// FIRST customId only; flow-dispatch rejects mismatched re-registrations
// of the same stage so we don't repeat it.
registerFlow(CONFIRM_USER_SELECT_CUSTOM_ID, {
  expectedStage: SEND_STAGE_AWAITING_CONFIRM,
  handler: handleConfirmUserSelect,
  siblingMessage: 'You have a `/qurl send` or `/qurl map` confirm card open in this channel — finish or cancel it first.',
});
// siblingMessage intentionally omitted on the SEND + CANCEL custom-
// id registrations below — flow-dispatch's `siblingMessages` map is
// keyed by stage (not by customId), so the message registered on
// USER_SELECT above is reachable from any of the three customIds
// at SEND_STAGE_AWAITING_CONFIRM. The "siblingMessage keyed by stage"
// test in qurl-send-helpers.test.js pins this contract.
registerFlow(CONFIRM_SEND_CUSTOM_ID, {
  expectedStage: SEND_STAGE_AWAITING_CONFIRM,
  handler: handleConfirmSendClick,
});
registerFlow(CONFIRM_CANCEL_CUSTOM_ID, {
  metadataOnly: true,
  expectedStage: SEND_STAGE_AWAITING_CONFIRM,
  handler: handleConfirmCancelClick,
});
// Confirm-card menus + note button/modal. All share the same
// expectedStage as the original three customIds above; siblingMessage
// is keyed by stage (registered on USER_SELECT only), so no
// re-registration here. Each handler reads `row.payload` from the
// dispatcher's loadFlow and uses `expectedVersion: row.version` on
// transitionFlow for the picker-vs-menu race.
registerFlow(CONFIRM_EXPIRY_SELECT_CUSTOM_ID, {
  expectedStage: SEND_STAGE_AWAITING_CONFIRM,
  handler: handleConfirmExpirySelect,
});
registerFlow(CONFIRM_SELF_DESTRUCT_SELECT_CUSTOM_ID, {
  expectedStage: SEND_STAGE_AWAITING_CONFIRM,
  handler: handleConfirmSelfDestructSelect,
});
registerFlow(CONFIRM_NOTE_BUTTON_CUSTOM_ID, {
  metadataOnly: true,
  expectedStage: SEND_STAGE_AWAITING_CONFIRM,
  handler: handleConfirmNoteButton,
});
registerFlow(CONFIRM_NOTE_MODAL_CUSTOM_ID, {
  expectedStage: SEND_STAGE_AWAITING_CONFIRM,
  handler: handleConfirmNoteModal,
});
// Voice-everyone button — only present on the confirm card when the
// slash command was invoked from a voice / stage-voice channel.
// Same stage + siblingMessage-keyed-by-stage contract as the other
// CONFIRM_* registrations above.
registerFlow(CONFIRM_VOICE_EVERYONE_BUTTON_CUSTOM_ID, {
  expectedStage: SEND_STAGE_AWAITING_CONFIRM,
  handler: handleConfirmVoiceEveryone,
});
// "Pick people instead" — voice/everyone-mode → picker-mode toggle.
// Same stage contract as the other CONFIRM_* registrations.
registerFlow(CONFIRM_PICK_MANUAL_BUTTON_CUSTOM_ID, {
  expectedStage: SEND_STAGE_AWAITING_CONFIRM,
  handler: handleConfirmPickManual,
});
// @everyone button — only rendered when sender has MENTION_EVERYONE
// and recipientMode is PICKER. Same stage + siblingMessage-keyed-by-
// stage contract as the other CONFIRM_* registrations.
registerFlow(CONFIRM_EVERYONE_BUTTON_CUSTOM_ID, {
  expectedStage: SEND_STAGE_AWAITING_CONFIRM,
  handler: handleConfirmEveryone,
});

module.exports = {
  commands,
  registerCommands,
  handleCommand,
  // Exported for the dispatcher's unit tests. Production code reaches
  // these via flow-dispatch's registry, not these exports.
  handleRevokeSelect,
  handleSetupButton,
  handleSetupModal,
  handleConfirmUserSelect,
  handleConfirmSendClick,
  handleConfirmCancelClick,
  handleConfirmExpirySelect,
  handleConfirmSelfDestructSelect,
  handleConfirmNoteButton,
  handleConfirmNoteModal,
  handleConfirmVoiceEveryone,
  handleConfirmPickManual,
  handleConfirmEveryone,
  // _test is only exported in non-production so live state (sendCooldowns)
  // and internal handlers can't leak into prod consumers. Tests run with
  // NODE_ENV=test (jest's default); production deploys set NODE_ENV=production.
  ...(process.env.NODE_ENV !== 'production' && {
    _test: {
      buildQurlCommandData,
      logSafeApiDetail,
      isGoogleMapsURL,
      sanitizeFilename,
      sanitizeMessage,
      isAllowedFileType,
      isOnCooldown,
      setCooldown,
      clearCooldown,
      revokingSendLocks,
      batchSettled,
      expiryToISO,
      sendCooldowns,
      // Renderer exposed so tests can pin the bottom-row button
      // composition directly (e.g., the DM-context @everyone-gate
      // assertion) without driving through handleQurlSend's entry path.
      renderConfirmCardRows,
      // Memo for the @everyone non-bot count is module-scoped. Tests
      // that reuse a guild reference across cases could otherwise get
      // surprising memo hits; exposing the WeakMap lets a beforeEach
      // hook clear specific entries. Production callers never need
      // this — the memo busts on cache.size/memberCount changes.
      _everyoneCountMemo: everyoneCountMemo,
      computeEveryoneDisplayCount,
      handleAddRecipients,
      buildDeliveryPayload,
      buildDeliveryEmbed,
      buildStepThroughButton,
      buildTrustButton,
      packBulkDeliveryComponents,
      buildRevokedDMPayload,
      persistDispatchResult,
      resolveSenderAlias,
      resolveGuildProvenance,
      safeUrlHost,
      // Back-half functions exposed for direct unit testing. Without these
      // hooks, coverage of the polling/revoke/add-recipients code paths
      // can only be reached via full /qurl send + /qurl map integration
      // tests, which require mocking the entire state-machine front-half
      // before the back-half even runs. Direct exposure means each
      // function gets a focused spec without that setup overhead.
      monitorLinkStatus,
      revokeAllLinks,
      renderRevokeMsg,
      safeRevokeHeader,
      renderSendConfirm,
      // Pure view-counter render, re-exported (defined in
      // ./view-counter-render) so the wording/floor contract is pinned
      // directly (degraded→baseMsg, normal→counter line, pending floors
      // at 0) rather than only via the monitor closure. The webhook
      // fast-path imports the same module function; the unit test is the
      // byte-identity anchor both render sites lean on.
      renderViewCounter,
      REVOKE_TRUNC_LIMIT,
      mintLinksInBatches,
      // Fresh-mint cleanup's defensive unidentified-id branch is unreachable
      // through validated mint output, so pin it directly.
      cleanupFreshMintedLinks,
      activeMonitors,
      // The top-level back-half driver. Exported here so PR 7b's
      // tests (and the follow-up direct unit spec in #278) can pin
      // its contract against a constructed param object — without
      // re-driving the full /qurl send + /qurl map confirm-card flow.
      executeSendPipeline,
      // Test-only file-concurrency hooks. The slot counter is module-
      // private (live state) and exposing a setter lets the cap branch
      // be tested without a parallel-send harness.
      getActiveFileSends: sendSlash.getActiveFileSends,
      setActiveFileSends: sendSlash.setActiveFileSends,
      // The ACK_TIMEOUT_MSG_RE fallback shape drives the failure_type
      // alarm — table-driven tests pin every shape so a silent regex
      // breakage can't slip through.
      isAckTimeoutError,
      // largeSendThreshold formula has non-trivial math + a
      // degenerate-cap guard; exposed for direct unit testing
      // rather than asserting via log-spy.
      largeSendThreshold,
      LARGE_SEND_RECIPIENT_FLOOR,
      // Setup-flow constants — exposed so tests assert against
      // production values, not stale duplicates that would silently
      // pass when the constants are tuned. The regex export lets
      // future tests assert format shape directly rather than via
      // VALID_KEY round-tripping.
      SETUP_BUTTON_TTL_SECONDS,
      SETUP_MODAL_TTL_SECONDS,
      SETUP_API_KEY_REGEX,
      SETUP_API_KEY_MIN_LENGTH,
      SETUP_API_KEY_MAX_LENGTH,
      SETUP_SUCCESS_MSG,
      // /qurl send + /qurl map handlers + flow constants. Tests pin
      // against production values via these exports rather than
      // re-stating the strings.
      handleQurlSend,
      handleQurlMap,
      // /qurl detect handler + its per-(guild,user) cooldown surface
      // (#1101). The cooldown Map + predicates are exposed so tests pin
      // the abuse-guard contract (composite key, separate bucket from
      // sendCooldowns) directly, like the send cooldowns above.
      handleQurlDetect,
      detectCooldowns,
      isOnDetectCooldown,
      setDetectCooldown,
      clearDetectCooldown,
      // The 60s time-sweep, exposed so the windows-differ regression (detect
      // entry must NOT be swept at the send window when DETECT > SEND) is
      // testable directly. DETECT_NO_MATCH_MSG is exposed so tests pin that
      // the two no-match branches share byte-identical copy.
      sweepCooldowns,
      DETECT_NO_MATCH_MSG,
      resolveRecipientUsers,
      partitionRecipients,
      resolveMentionableSelection,
      resolveRoleNames,
      selfDestructOptionToSeconds,
      renderRecipientWarnings,
      renderConfirmCardContent,
      parseLocationInput,
      resolveLocation,
      RESOLVE_REASON,
      // Disabled-state reply for stale /qurl map submissions. Exported
      // so flag-off tests pin against the production string.
      QURL_MAP_DISABLED_REPLY,
      QURL_DETECT_DISABLED_REPLY,
      handleAutocomplete,
      // Test-only reset: the autocomplete-failure burst counter is
      // module-level state that accumulates across tests within a
      // file unless explicitly cleared.
      _resetAutocompleteFailureBurst: () => { autocompleteFailureBurst = 0; },
      AUTOCOMPLETE_FAILURE_LOG_BURST,
      safeDecodeURIComponent,
      SEND_STAGE_AWAITING_CONFIRM,
      // Every confirm-card customId is exported so the contract test
      // in qurl-send-helpers.test.js can pin every wire value — a typo
      // in any of these silently breaks routing for in-flight confirm
      // cards, so they need to be test-asserted.
      CONFIRM_USER_SELECT_CUSTOM_ID,
      CONFIRM_SEND_CUSTOM_ID,
      CONFIRM_CANCEL_CUSTOM_ID,
      CONFIRM_EXPIRY_SELECT_CUSTOM_ID,
      CONFIRM_SELF_DESTRUCT_SELECT_CUSTOM_ID,
      CONFIRM_NOTE_BUTTON_CUSTOM_ID,
      CONFIRM_NOTE_MODAL_CUSTOM_ID,
      CONFIRM_VOICE_EVERYONE_BUTTON_CUSTOM_ID,
      CONFIRM_PICK_MANUAL_BUTTON_CUSTOM_ID,
      RECIPIENT_MODE_PICKER,
      RECIPIENT_MODE_VOICE,
      RECIPIENT_MODE_EVERYONE,
      normalizeRecipientMode,
      SEND_FLOW_TTL_SECONDS,
      SELF_DESTRUCT_NO_TIMER_CHOICE,
      // classifyMintFailure: exposed so a unit test pins the
      // reason-category contract (qurl-integrations#276). The
      // cardinality-discipline comment on the helper says "don't
      // add per-API-code branches" — without a test, that's just
      // a docstring waiting to drift.
      classifyMintFailure,
    },
  }),
};
