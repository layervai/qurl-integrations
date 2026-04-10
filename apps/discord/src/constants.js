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
  QURL_REVOKE_WINDOW: 900000, // 15 minutes — button stays active, /qurl revoke works forever
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
  ROLE_COLORS,
  TIMEOUTS,
  LIMITS,
  GITHUB_ACTIONS,
  GOOD_FIRST_ISSUE_PATTERNS,
};
