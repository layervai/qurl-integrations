// Shared constants for the OpenNHP Discord bot

// Embed colors (Discord uses hex integers)
const COLORS = {
  PRIMARY: 0x3498DB,      // Blue - general info
  SUCCESS: 0x2ECC71,      // Green - success states
  WARNING: 0xF39C12,      // Orange - warnings
  ERROR: 0xE74C3C,        // Red - errors
  PURPLE: 0x9B59B6,       // Purple - stats/contributions
  GOLD: 0xF1C40F,         // Gold - achievements/milestones
  GITHUB_GREEN: 0x238636, // GitHub green - issues/PRs
  GITHUB_PURPLE: 0x6e5494,// GitHub purple - commits
  QURL_BRAND: 0x00d4ff,   // Cyan - qURL delivery embeds
};

// qURL resource types
const RESOURCE_TYPES = {
  FILE: 'file',
  MAPS: 'maps',
};

// Trust signals surfaced on the per-recipient DM embed (sender provenance
// + "is this real?" verify path). The destination domain in the footer
// mirrors the host of the minted qURL — keep in lockstep with whatever
// host qurl-service returns from POST /v1/qurls. The landing URL is the
// public brand page first-time recipients can hit to verify qURL is a
// real service before clicking Step Through.
//
// LANDING_URL is intentionally brand-canonical — it stays pinned to
// the layerv.ai/qurl page even when a tenant brands their minted-link
// host via `branded_domain`. The verify path is "is qURL itself
// real?", not "is this tenant real?"; routing through the canonical
// brand page is the correct trust signal regardless of which host
// served the link.
//
// TODO(branded-domain) — tracked: layervai/qurl-integrations#383
// DESTINATION_DOMAIN is a brand-default literal. qurl-service already
// supports a `branded_domain` concept (tenant-custom hosts on minted
// links — see qurl-service api.gen.go). Discord doesn't consume that
// field today, but the moment it does, the footer "opens qurl.link"
// becomes factually wrong while Step Through opens e.g.
// `door.acme.com`. When qurl-integrations starts honoring
// branded_domain in the minted-link response, derive the footer text
// from link.qurlLink's host rather than this literal.
const TRUST = {
  LANDING_URL: 'https://layerv.ai/qurl/',
  DESTINATION_DOMAIN: 'qurl.link',
};

// DM delivery status values
const DM_STATUS = {
  SENT: 'sent',
  FAILED: 'failed',
  PENDING: 'pending',
};

// Role colors (for auto-creation)
const ROLE_COLORS = {
  CONTRIBUTOR: 0x3498DB,       // Blue
  ACTIVE_CONTRIBUTOR: 0x2ECC71, // Green
  CORE_CONTRIBUTOR: 0x9B59B6,   // Purple
  CHAMPION: 0xF1C40F,           // Gold
};

// Timeouts (in milliseconds)
const TIMEOUTS = {
  BUTTON_INTERACTION: 60000,  // 1 minute
  DEFER_REPLY: 3000,          // 3 seconds
  QURL_REVOKE_WINDOW: 900000, // 15 minutes - button stays active, /qurl revoke works forever
};

// Limits
const LIMITS = {
  EMBED_DESCRIPTION: 4096,
  EMBED_FIELD_VALUE: 1024,
  RECENT_CONTRIBUTIONS: 3,
  LEADERBOARD_SIZE: 10,
  TOP_REPOS: 5,
  MAX_LABELS_DISPLAY: 5,
  RELEASE_NOTES_TRUNCATE: 500,
};

// Maximum attachment size the bot will accept. Shared between commands.js
// (user-facing validation) and connector.js (CDN download + streaming cap).
// Keep in sync with Discord's own 25MB attachment limit.
const MAX_FILE_SIZE = 25 * 1024 * 1024;

// Cap on concurrent link-status monitors. Each monitor fires setInterval
// up to 1 hour; a burst of sends could otherwise stack dozens of timers.
const MAX_CONCURRENT_MONITORS = 50;

// Discord's `GET /guilds/{id}/members` page cap (and the maximum value
// accepted by the `limit` query param).
const DISCORD_MEMBERS_PAGE_SIZE = 1000;
// Safety bound on the @everyone prewarm pagination loop. Exceeding
// Discord's per-guild ceiling (~1M) means an upstream bug is returning
// steady-state full pages without advancing the `after` cursor.
const PREWARM_MAX_PAGES = 1000;

// Fraction of `effectiveGuildMemberCount` the cache must reach for
// `/unlinked` to consider its member set complete. Below this we
// surface a degraded-API message instead of reporting "all linked" —
// mid-pagination failures (e.g. 429 on page 6 of 12) otherwise leave
// a non-empty but incomplete cache that would silent-false-positive.
// 0.9 absorbs `approximateMemberCount` drift in either direction
// without letting a substantive shortfall through.
const UNLINKED_CACHE_COMPLETENESS_THRESHOLD = 0.9;

// GitHub event actions we care about
const GITHUB_ACTIONS = {
  PR_MERGED: 'closed',  // with merged=true
  ISSUE_LABELED: 'labeled',
  ISSUE_OPENED: 'opened',
  RELEASE_PUBLISHED: 'published',
  STAR_CREATED: 'created',
};

// Good first issue label patterns
const GOOD_FIRST_ISSUE_PATTERNS = [
  'good first issue',
  'good-first-issue',
  'beginner',
  'help wanted',
];

// Canonical event names emitted via logger.audit(). The CloudWatch metric
// filters at qurl-integrations-infra/qurl-bot-discord/terraform/main.tf
// pattern-match these strings, so a typo at a call site silently disables
// the metric. Always import from here rather than passing literal strings.
// Adding a new event: add the constant here, the call site, AND the
// terraform filter (in the same merge train, since the filter is a no-op
// without the emission and vice versa).
//
// Scope: this set covers events the qURL service cannot see — transport-
// layer (DM dispatch), bulk-revoke outcomes (per-link API calls happen
// at the service but the all-or-partial-success tally lives here), and
// upload-to-connector results. Mint counts (`mint_success` / `mint_failed`)
// are intentionally NOT here — they belong at the qURL service layer
// with an `agent` dimension fed by an X-QURL-Agent header so every
// integration (Discord, Slack, Teams, CLI, web/portal) gets them for
// free without each re-implementing emission. Tracked separately; see
// Justin's review comment on qurl-integrations-infra#309.
//
// SECRETS CONTRACT: callers SHOULD pass only non-sensitive meta values
// from a small, pre-vetted vocabulary: `send_id`, `kind`, `count`,
// `expires_in`, `api_code`, `success`, `total`. logger.audit()
// defends in two layers if a caller still slips a secret-shaped key
// in (top-level OR nested):
//   1. The value is redacted to '[REDACTED]' in the emitted payload.
//   2. A CloudWatch-visible `console.error` line names the offending
//      key so the call site is grep-able from the dashboard.
// Sibling keys are unaffected, so legitimate dimensions still flow.
// Detection uses an EXACT-MATCH set (AUDIT_SECRET_KEYS in logger.js),
// not the top-level logger's substring-based REDACT_SUBSTRINGS — that
// way `tokens_minted` / `token_count` / similar legitimate dimensions
// don't trigger false-positive redactions. The substring approach
// would have made the redaction unsafe; exact-match makes it safe.
const AUDIT_EVENTS = {
  // UPLOAD_SUCCESS fires after upload + mintLinksInBatches + sufficiency
  // check all succeed — i.e. when the send is fully prepared and ready
  // to dispatch. It's not just the connector POST. Name is kept literal
  // ("upload_success") for back-compat with the upload_count terraform
  // filter; semantically closer to "prepare_success" or "links_ready".
  //
  // Emitted EXACTLY ONCE per send, even when handleAddRecipients runs
  // both file and location prep paths. The meta `kind` field carries
  // the composition: 'file' | 'location' | 'mixed'. Collapsing to one
  // event prevents UploadCount from double-counting mixed sends if the
  // CloudWatch filter doesn't dimension on kind. /qurl send and
  // /qurl map are mutually exclusive so 'mixed' only ever shows up
  // from handleAddRecipients on a sendConfig that has both file +
  // location.
  UPLOAD_SUCCESS: 'upload_success',
  DISPATCH_SENT: 'dispatch_sent',
  DISPATCH_FAILED: 'dispatch_failed',
  // DISPATCH_SENT_NO_REFS fires when sendDM resolved ok:true but the
  // channelId / messageId came back missing — the audit-vs-DDB
  // divergence persistDispatchResult records as `failed`. Distinct
  // from DISPATCH_SENT so the dashboard can reconcile CloudWatch
  // `dispatch_sent` count with DDB `count(dm_status='sent')` without
  // a mystery gap. Should always read zero — if it lights up, the
  // discord.js user.send() response shape has changed.
  DISPATCH_SENT_NO_REFS: 'dispatch_sent_no_refs',
  // DISPATCH_PERSIST_FAILED fires when sendDM succeeded but the
  // bookkeeping write to qurl_sends threw (DDB outage, throttle,
  // ValidationException, etc.). The DM is real; the dispatch loop
  // continues to report the recipient as delivered. This event is
  // a canary for an oncall-relevant DDB issue separate from the
  // dispatch-success-rate signal — should always read zero.
  DISPATCH_PERSIST_FAILED: 'dispatch_persist_failed',
  // REVOKE_SUCCESS fires when at least one per-link delete succeeded;
  // REVOKE_FAILED fires when every per-link delete threw (success === 0
  // && total > 0). When total === 0 (nothing to revoke — already-revoked
  // or unknown sendId) neither event fires. Splitting the two stops a
  // dashboard from counting all-failed revokes as successes.
  REVOKE_SUCCESS: 'revoke_success',
  REVOKE_FAILED: 'revoke_failed',

  // Emitted by gateway-health.js on every /health response that
  // returns 503. Carries `reason: 'not_ready' | 'sampler_threw'`
  // so the dashboard can split a clean WebSocket disconnect from a
  // readiness-closure bug under load. The wget probe runs every
  // 30 s, so a real wedge produces this event at probe cadence —
  // the paired CloudWatch metric filter at qurl-integrations-infra
  // qurl-bot-discord/terraform/monitoring.tf counts these so an
  // alarm can fire on >N unhealthy responses in a window.
  GATEWAY_HEALTH_UNHEALTHY: 'gateway_health_unhealthy',

  // Phase 1 monitoring events — emitted by the gateway role only.
  // Paired with terraform filters in qurl-integrations-infra
  // qurl-bot-discord/terraform/monitoring.tf.

  // Single emission per ChatInputCommand interaction. handleCommand
  // early-returns on autocomplete / non-chat-input (modal submits,
  // buttons) WITHOUT emitting — those flows have their own contracts.
  // If discord.js ever routes a new interaction type through the
  // same handler, the metric undercounts until either the early-
  // return or this comment is updated. `success: true|false`,
  // `handler_duration_ms` (handler entry → metric emit; not edge-to-ACK
  // — see commands.js comment), and `failure_type` ('ack_timeout' |
  // 'handler_error' | 'unknown_command' | 'reply_failed' | null)
  // carry every dimension Phase 1 alarms need. Low-cardinality only —
  // command_name is bounded by registered slash commands.
  //
  // failure_type precedence: when execute() throws AND a follow-up
  // reply also throws, handler_error wins (the original execute
  // failure is the more meaningful signal); only ack_timeout is
  // allowed to override. Asymmetric vs. the stale-registration path
  // which DOES tag reply_failed because there's no prior execute.
  // See commands.js handleCommand for the precedence table.
  INTERACTION_HANDLED: 'interaction_handled',

  // Positive-signal heartbeat. Emitted every 30 s when the composite
  // readiness check passes (client.isReady() && ws.ping > 0 &&
  // ack_age_ms < 60000). Missing emissions = wedge.
  GATEWAY_HEARTBEAT: 'gateway_heartbeat_healthy',

  // Negative-signal heartbeat. Emitted every 30 s when the composite
  // readiness check FAILS (post-#210: !is_ready || ping_ms <= 0 ||
  // ack_age_ms >= 60_000). Pairs with the healthy event so a real WS
  // wedge surfaces as a metric. `activity_age_ms` rides along on both
  // emissions for dashboarding only — it is NOT part of the gating
  // predicate (#210 backed out the false-positive on idle bots). Don't
  // alarm on activity_age_ms; alarm on missing healthy emissions.
  GATEWAY_HEARTBEAT_UNHEALTHY: 'gateway_heartbeat_unhealthy',

  // Bot added/removed from a guild. Single emission on the
  // guildCreate / guildDelete event. `guild_id` is in the payload
  // for log-grep / forensic queries; it MUST NOT be promoted to a
  // CloudWatch metric dimension at the terraform-filter layer —
  // per-guild dimensioning explodes metric cost ($0.30/metric/guild)
  // and is high-cardinality unbounded as installs grow. See the
  // monitoring.tf filter for guild_install — it counts events as a
  // flat metric.
  GUILD_INSTALL: 'guild_install',
  GUILD_UNINSTALL: 'guild_uninstall',

  // Periodic gauge of `client.guilds.cache.size`. Emitted every 60 s.
  ACTIVE_GUILD_COUNT: 'active_guild_count',

  // ── Event-shipper Pillar 1: flow_state observability ──
  //
  // Names reserved by the event-shipper observability Phase 1.0 PR;
  // emissions wired by the state-machine harness PR in
  // apps/discord/src/flow-state.js. The paired CloudWatch metric filters
  // land in qurl-integrations-infra (separate PR) once the harness is
  // producing events in sandbox. Three-stage rollout — names reserved →
  // emissions wired → metric filters lit — keeps each layer reviewable
  // in isolation.
  //
  // The "flow_state" table is created by qurl-integrations-infra#504; the
  // SLI these events feed is "Flow continuity" (design doc § SLI / SLO
  // definitions, target 99.99% over 1-day windows).
  //
  // SLI computation contract:
  //   total_flows           = count(FLOW_CREATED)
  //   completed_flows       = count(FLOW_DELETED)
  //   silently_dropped_flows = total_flows - completed_flows
  //                          = count(FLOW_CREATED) - count(FLOW_DELETED)
  // Every FLOW_CREATED MUST be followed by either an explicit FLOW_DELETED
  // (terminal stage, abort, etc.) OR a TTL reap. The reap path emits no
  // event by design (DDB TTL is asynchronous, so a "deleted by TTL"
  // signal would itself require a separate sweeper). Computing
  // silently_dropped via "created minus deleted" sidesteps the async-reap
  // problem AND captures all unclean drops (crashes, hangs, partial
  // process restart between transitions).

  // Single emission per createFlow() call. Marks the start of a flow's
  // lifecycle for the "Flow continuity" SLI's denominator.
  // Payload fields:
  //   - `stage`:    initial stage the flow enters at (e.g.
  //                 'awaiting_button'). LOW-cardinality — safe to
  //                 dimension on. Bounded by the state-machine's
  //                 stage enum.
  //   - `shard_id`: e.g. '0:1' (single-shard today); generalizes to
  //                 'k:n' post-sharding. LOW-cardinality — safe to
  //                 dimension on.
  //   - `flow_id`:  HIGH-cardinality (composite encoding of shard /
  //                 guild / channel / user). Forensic-query field
  //                 ONLY — same trap as `guild_id` / `path` above:
  //                 do NOT promote to a CloudWatch metric dimension
  //                 at the terraform-filter layer. Use Logs Insights
  //                 for per-flow drilldown during an incident.
  FLOW_CREATED: 'flow_created',

  // Emitted on every transitionFlow() call — the workhorse event for
  // the state machine's observability. The paired CloudWatch metric
  // filter materializes
  // `qurl_bot_flow_transition_total{stage_from,stage_to,result,terminal}`
  // (design doc § Pillar 1).
  // Payload fields:
  //   - `stage_from`: stage the flow was in before the transition.
  //                   LOW-cardinality — safe to dimension on.
  //   - `stage_to`:   stage the flow advances to. LOW-cardinality —
  //                   safe to dimension on.
  //   - `result`:     'success' | 'conflict' | 'not_found' | 'error'.
  //                   LOW-cardinality — safe to dimension on. The
  //                   conflict bucket counts OCC-loser races (DDB
  //                   ConditionalCheckFailedException) — the
  //                   correctness primitive that gates concurrent
  //                   worker advances. SUSTAINED conflict > 0 indicates
  //                   spurious dispatch; conflict at low rate is the
  //                   expected at-least-once-delivery behavior.
  //   - `terminal`:   bool. When true, this transition ends the flow
  //                   (the next call will be deleteFlow). Lets the
  //                   metric math identify clean completions vs.
  //                   mid-flow transitions without enumerating every
  //                   terminal stage in the filter (terminal set
  //                   varies per flow type — revoke is shorter than
  //                   send). LOW-cardinality (boolean) — safe to
  //                   dimension on AND included in the materialized
  //                   metric's dimension list above. FORCED TO false
  //                   on non-success results (not_found, conflict,
  //                   error) — the transition didn't advance, so
  //                   nothing terminal happened, so the audit is
  //                   honest by construction. Consumers can still
  //                   slice `count_by(terminal=true)` safely.
  //   - `extended`:   bool. True iff the transition GENUINELY
  //                   extended the row's expires_at (the new value
  //                   is strictly greater than the prior). A
  //                   set_expires_at that shortens, equals, or
  //                   leaves the value untouched emits false —
  //                   so `count_by(extended=true)` is a faithful
  //                   "this transition bumped the deadline forward"
  //                   count and not a "set_expires_at was passed"
  //                   count. Also false on non-success transitions
  //                   (nothing extended) and on rows whose prior
  //                   expires_at was missing/corrupted (no honest
  //                   baseline to extend FROM). LOW-cardinality
  //                   (boolean) — safe to dimension on. Forensic-
  //                   only today; not currently used as a metric
  //                   filter dimension.
  //   - `version`:    integer. On success: the row's NEW version
  //                   after the OCC bump. On non-success (conflict,
  //                   not_found, error): the version the caller
  //                   expected (i.e., `expectedVersion`). Lets a
  //                   forensic query correlate retries by attempt
  //                   identity ("which attempt won version 5?")
  //                   without needing the live row — important
  //                   because already-deleted flows can't be
  //                   JOINed against the live table. LOW-cardinality
  //                   per-flow (bounded by transitions-per-flow,
  //                   ~10 in practice); NOT a metric dimension —
  //                   forensic-only field.
  //   - `flow_id`:    HIGH-cardinality. Forensic-query ONLY — same
  //                   posture as FLOW_CREATED.
  FLOW_TRANSITION: 'flow_transition',

  // Emitted on every explicit deleteFlow() call — flow numerator for
  // the "Flow continuity" SLI (completed_flows). TTL-reaped flows do
  // NOT emit this event; the SLI math relies on that asymmetry to
  // identify silent drops (FLOW_CREATED count minus FLOW_DELETED
  // count). If a future change adds a "delete-on-TTL-reap" sweeper,
  // it MUST emit a distinct event (e.g. FLOW_REAPED) — not
  // FLOW_DELETED — to preserve the SLI math.
  // Payload fields:
  //   - `stage`:  stage the flow was in at deletion. LOW-cardinality
  //               — safe to dimension on. For terminal completions,
  //               equals the terminal stage; for aborts, equals
  //               whatever the flow was awaiting when aborted.
  //   - `reason`: 'terminal' | 'abort' | 'admin_cleanup'.
  //               LOW-cardinality — safe to dimension on. Splits
  //               clean completions ('terminal') from user/operator
  //               aborts ('abort') from operator-driven cleanup
  //               ('admin_cleanup'). 'terminal' should dominate; a
  //               sustained 'abort' rate indicates a UX problem.
  //   - `flow_id`: HIGH-cardinality. Forensic-query ONLY.
  FLOW_DELETED: 'flow_deleted',

  // Emitted when a request to a dependency returns 401 or 403 — catches
  // rotation drift (qurl-service API key, GitHub App token, etc.) that
  // client.isReady() can't see.
  // Payload fields:
  //   - `dependency`: 'qurl_service' (extensible to GitHub / Auth0
  //                   / etc. as future dependencies are instrumented).
  //                   LOW-cardinality — safe to dimension on.
  //   - `status`: numeric 401 | 403. LOW-cardinality — safe to
  //               dimension on.
  //   - `method`: HTTP verb (GET, POST, PUT, DELETE). LOW-cardinality
  //               — safe to dimension on.
  //   - `path`: HIGH-cardinality (carries resource IDs like
  //             `/qurls/abc123def`). Forensic-query field ONLY —
  //             do NOT promote to a CloudWatch metric dimension at
  //             the terraform-filter layer. Same trap as `guild_id`
  //             above: per-resource dimensioning would explode
  //             metric cost. Dimension on `dependency + method +
  //             status` instead; use Logs Insights to drill down
  //             to specific paths during an incident.
  //
  // The paired CloudWatch metric filter counts these so an alarm
  // can fire on >N auth failures in a window — catches token-
  // rotation drift before users see cascading errors. Reactive
  // design: only fires on actual dependency calls (no periodic
  // probing). For an idle bot with no real users, the metric
  // stays zero — but for an idle bot the rotation also doesn't
  // matter until someone tries to use it.
  DEPENDENCY_AUTH_FAILURE: 'dependency_auth_failure',

  // qURL webhook receiver — feeds CloudWatch metric filters +
  // alarms managed in the deploying organization's infrastructure
  // (separate from this repo). Flat counters only: do NOT promote
  // qurl_id/resource_id to dimensions (unbounded).
  QURL_WEBHOOK_RECEIVED: 'qurl_webhook_received',
  // Sustained rate = SSM secret drift OR attacker probing.
  QURL_WEBHOOK_SIGNATURE_INVALID: 'qurl_webhook_signature_invalid',
  // Legit traffic NEVER triggers — sustained rate = sustained attacker.
  QURL_WEBHOOK_RATE_LIMITED: 'qurl_webhook_rate_limited',
  // Sustained rate = qurl_views table hot.
  QURL_WEBHOOK_STORE_ERROR: 'qurl_webhook_store_error',

  // Subscription registry (BYOK per-guild webhooks). The registry is a
  // process-local Map<owner_id, …> primed from guild_configs at boot and
  // refreshed every 30s. Sustained rate of UNPRIMED = boot stall OR DDB
  // throttling on the priming scan. Sustained rate of UNKNOWN_OWNER after
  // priming = a real auth0 owner is hitting the receiver without ever
  // having linked a key (attacker probing) OR an upstream contract drift
  // (qurl-service emitting events under a different owner_id than the one
  // that registered the subscription).
  QURL_WEBHOOK_CACHE_MISS_UNPRIMED: 'qurl_webhook_cache_miss_unprimed',
  QURL_WEBHOOK_CACHE_MISS_UNKNOWN_OWNER: 'qurl_webhook_cache_miss_unknown_owner',
  // Refresh failure. After N consecutive failures the registry stays
  // unprimed (or stale) — receiver responds 503; qurl-service retries.
  QURL_WEBHOOK_CACHE_REFRESH_FAIL: 'qurl_webhook_cache_refresh_fail',
  // Lifecycle. SUBSCRIPTION_DELETE_FAILED fires when DELETE returns 401
  // (key revoked) or 404 (already gone) — both are swallowed so guild
  // unlink doesn't fail user-facing on stale qurl-service state.
  // REGISTER_FAILED fires on every linkGuildWebhookSubscription failure
  // branch so CloudWatch alarms can fire on the right side of the
  // binary (success has its own event below).
  QURL_WEBHOOK_SUBSCRIPTION_REGISTERED: 'qurl_webhook_subscription_registered',
  QURL_WEBHOOK_SUBSCRIPTION_REGISTER_FAILED: 'qurl_webhook_subscription_register_failed',
  QURL_WEBHOOK_SUBSCRIPTION_DELETED: 'qurl_webhook_subscription_deleted',
  QURL_WEBHOOK_SUBSCRIPTION_DELETE_FAILED: 'qurl_webhook_subscription_delete_failed',
};

// Frozen so a stray `AUDIT_EVENTS.UPLOAD_SUCCESS = 'oops'` mutation at
// runtime can't silently break a CloudWatch metric (the literal string
// would still work but the filter would stop matching). The other
// constant objects in this file aren't frozen, but AUDIT_EVENTS is the
// only one whose mutation is undetectable by tests.
Object.freeze(AUDIT_EVENTS);

// Wire-protocol event-type strings for qURL webhook payloads.
// Pinned as constants so a typo in the receiver's type check (or in a
// test asserting against the wire shape) fails to import rather than
// silently matching nothing. Mirrors qurl-service WebhookEventType.
const QURL_WEBHOOK_EVENTS = Object.freeze({
  ACCESSED: 'qurl.accessed',
});

// Discord gateway dispatch event names (the `t` field on op=0 frames).
// Today only INTERACTION_CREATE is published to the worker tier;
// MESSAGE_CREATE etc. are reserved for future event-class expansion.
// Centralized here so the publisher (event-publisher.js, filters on
// publish) and the consumer (event-consumer.js, validates the
// envelope's eventType) can't drift — Discord's wire-protocol string
// is the only value either side ever uses, and a typo on one side
// would silently drop every dispatch.
//
// Frozen for the same reason AUDIT_EVENTS is: a runtime mutation
// would leave the literal string in transit unaffected but break
// the other tier's check.
const GATEWAY_DISPATCH_TYPES = Object.freeze({
  INTERACTION_CREATE: 'INTERACTION_CREATE',
});

// Structured-log `kind` tags used to correlate failures across the
// async-boundary trio: the gateway-WS-driven unhandledRejection
// handler in index.js, the worker-tier dispatch handler rejection
// path in event-consumer.js (trackDispatch's .catch), and the
// publish-failure path in event-publisher.js. All three emit the
// same `kind: 'unhandledRejection'` tag so a single CloudWatch
// query — filtering on the structured field — finds every site
// without grepping message text or maintaining per-site filter
// rules. Centralizing the literal here makes the contract
// explicit and lets a future tag addition (LOG_KIND_AUDIT, etc.)
// follow the same pattern.
//
// Frozen — see AUDIT_EVENTS for the rationale. A mutation here
// would silently make one site stop matching the CloudWatch
// alarm filter the other two sites still emit.
const LOG_KINDS = Object.freeze({
  UNHANDLED_REJECTION: 'unhandledRejection',
  // Separate kind for view-update publish/dispatch failures (feat #60).
  // Decoupled from UNHANDLED_REJECTION so CloudWatch alarm filters
  // targeting interaction-loss (event-shipper + global unhandled-
  // rejection paths) don't page on view-update failures — those are
  // covered by the polling-tick fallback at the render layer.
  VIEW_UPDATE_PUBLISH_FAIL: 'viewUpdatePublishFail',
});

module.exports = {
  COLORS,
  RESOURCE_TYPES,
  DM_STATUS,
  ROLE_COLORS,
  TIMEOUTS,
  LIMITS,
  MAX_FILE_SIZE,
  MAX_CONCURRENT_MONITORS,
  DISCORD_MEMBERS_PAGE_SIZE,
  PREWARM_MAX_PAGES,
  UNLINKED_CACHE_COMPLETENESS_THRESHOLD,
  GITHUB_ACTIONS,
  GOOD_FIRST_ISSUE_PATTERNS,
  AUDIT_EVENTS,
  QURL_WEBHOOK_EVENTS,
  TRUST,
  GATEWAY_DISPATCH_TYPES,
  LOG_KINDS,
};
