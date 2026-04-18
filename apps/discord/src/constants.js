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
  QURL_BRAND: 0x00d4ff,   // Cyan - QURL delivery embeds
};

// QURL resource types
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
};
