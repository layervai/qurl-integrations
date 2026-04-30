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
// SECRETS CONTRACT: logger.audit() bypasses the redact() pass on meta
// (key-name redaction would blank legitimate dimensions like
// `tokens_minted`). Callers MUST therefore pass only non-sensitive meta
// values from a small, pre-vetted vocabulary: `send_id`, `kind`,
// `count`, `expires_in`, `api_code`, `success`, `total`. Never pass
// API keys, tokens, OAuth state, secrets, raw user input, or anything
// whose value should not appear verbatim in CloudWatch. logger.audit()
// emits a warn-level error if a meta key exactly matches one of a
// small set of known secret-bearer names (`auth_token`, `api_key`,
// `password`, etc. — see AUDIT_SECRET_KEYS in logger.js), but the
// warn is defense-in-depth — the canonical contract enforcement is
// the call-site review of every new audit emission.
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
