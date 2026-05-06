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
  // CloudWatch filter doesn't dimension on kind. handleSend's branches
  // are mutually exclusive so 'mixed' only ever shows up from
  // handleAddRecipients on a sendConfig that has both file + location.
  UPLOAD_SUCCESS: 'upload_success',
  DISPATCH_SENT: 'dispatch_sent',
  DISPATCH_FAILED: 'dispatch_failed',
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
};

// Frozen so a stray `AUDIT_EVENTS.UPLOAD_SUCCESS = 'oops'` mutation at
// runtime can't silently break a CloudWatch metric (the literal string
// would still work but the filter would stop matching). The other
// constant objects in this file aren't frozen, but AUDIT_EVENTS is the
// only one whose mutation is undetectable by tests.
Object.freeze(AUDIT_EVENTS);

module.exports = {
  COLORS,
  RESOURCE_TYPES,
  DM_STATUS,
  ROLE_COLORS,
  TIMEOUTS,
  LIMITS,
  MAX_FILE_SIZE,
  MAX_CONCURRENT_MONITORS,
  GITHUB_ACTIONS,
  GOOD_FIRST_ISSUE_PATTERNS,
  AUDIT_EVENTS,
};
