// store/contract — the Store interface
//
// The bot's data layer is reached through a single `Store` object that
// every consumer gets via `require('./store')`. The contract below lists
// every method a Store backend MUST implement. Runtime assertion at boot
// (see `assertStoreShape`) fails fast in a real (non-Jest) boot if a
// backend drops a method — so a missing implementation surfaces as a
// clear boot-time error, not as `TypeError: store.xMethod is not a
// function` deep in a request path. Under Jest the boot-time assertion
// is intentionally skipped so partial `jest.mock('../src/database', …)`
// stubs keep working; the invariant is instead enforced by
// `tests/store-contract.test.js`, which calls `assertStoreShape`
// directly against both a complete fixture AND the real default
// backend AND a `child_process.spawnSync` of the real boot path.
//
// Backend lifecycle: a Store may keep synchronous or asynchronous
// implementations as long as it preserves the method names and return
// shapes. Today the SQLite backend is synchronous (better-sqlite3 is
// sync). When a Promise-returning backend lands, flip the contract to
// async atomically (contract + every call site in one change) so the
// sync→async migration is a single reviewable flag-day rather than
// two entangled concerns.
//
// Scope of `assertStoreShape`: name-level only. A backend that exports
// `createPendingLink` with a wrong arity / wrong return shape passes
// this check happily, then crashes at the first call site. Parameter
// and return-shape parity between backends is the test suite's job
// (run the same behavioral tests against each backend's :memory: or
// mock instance). Particularly relevant for future sync→async flips:
// the assertion won't catch "method still exists but now returns a
// Promise" drift — behavioral tests will.
//
// Adding / removing a method:
//   1. Update `STORE_METHODS` below.
//   2. Add the implementation to every concrete backend.
//   3. The `assertStoreShape` test in
//      `tests/store-contract.test.js` will start failing until both
//      sides of the change land in the same PR — intentional lockstep.

const STORE_METHODS = Object.freeze([
  // Pending OAuth state tokens
  'createPendingLink',
  'getPendingLink',
  'deletePendingLink',
  'consumePendingLink',

  // GitHub-Discord account links
  'createLink',
  'getLinkByDiscord',
  'getLinkedDiscordIds',
  'getLinkByGithub',
  'deleteLink',
  'forceLink',

  // Contributions (merged PRs)
  'recordContribution',
  'getContributions',
  'getAllContributions',
  'getContributionCount',
  'getWeeklyContributions',
  'getMonthlyContributions',
  'getUniqueRepos',
  'getLastWeekContributions',
  'getNewContributorsThisWeek',

  // Aggregate stats and leaderboard
  'getStats',
  'getTopContributors',

  // Badges
  'awardBadge',
  'getBadges',
  'hasBadge',
  'checkAndAwardBadges',
  'awardFirstIssueBadge',

  // Contribution streaks
  'getStreak',
  'updateStreak',

  // Announcement-milestone dedup
  'hasMilestoneBeenAnnounced',
  'recordMilestone',

  // Weekly digest
  'getWeeklyDigestData',

  // QURL sends
  'recordQURLSend',
  'recordQURLSendBatch',
  'updateSendDMStatus',
  'getRecentSends',
  'markSendRevoked',
  'saveSendConfig',
  'getSendConfig',
  'getSendResourceIds',

  // Guild (BYOK) API keys
  'getGuildApiKey',
  'setGuildApiKey',
  'removeGuildApiKey',
  'getGuildConfig',
  'getGuildConfigWithApiKey',

  // Orphaned OAuth tokens (background revoke-retry queue)
  'recordOrphanedToken',
  'countOrphanedTokens',
  'listOrphanedTokens',
  'decryptOrphanedToken',
  'deleteOrphanedToken',

  // Lifecycle
  'close',
]);

// Constants surfaced on the Store object (not methods). Backends must
// export these as own-properties so callers that reach for
// `store.BADGE_TYPES` / `store.BADGE_INFO` don't get `undefined` under
// a minimal backend.
const STORE_CONSTANTS = Object.freeze([
  'BADGE_TYPES',
  'BADGE_INFO',
]);

/**
 * Verifies a Store backend implements every method and surfaces every
 * constant the bot code depends on. Called once at boot (see
 * `store/index.js`); intentionally throws rather than returning a
 * boolean so the bot refuses to start with a half-built backend.
 *
 * @param {object} store    The backend object to check.
 * @param {string} backend  Backend name — used in the thrown message
 *                          so a failure clearly names the offending
 *                          backend, not the generic "Store" interface.
 * @throws {Error}          When any method or constant is missing.
 */
function assertStoreShape(store, backend) {
  if (!store || typeof store !== 'object') {
    throw new Error(`Store backend '${backend}' is not an object (got ${typeof store}).`);
  }
  const missingMethods = STORE_METHODS.filter(m => typeof store[m] !== 'function');
  const missingConstants = STORE_CONSTANTS.filter(c => store[c] === undefined);
  if (missingMethods.length === 0 && missingConstants.length === 0) {
    return;
  }
  const parts = [];
  if (missingMethods.length > 0) {
    parts.push(`missing methods: ${missingMethods.join(', ')}`);
  }
  if (missingConstants.length > 0) {
    parts.push(`missing constants: ${missingConstants.join(', ')}`);
  }
  throw new Error(`Store backend '${backend}' is incomplete — ${parts.join('; ')}. Check that the backend module exports every item listed in src/store/contract.js's STORE_METHODS + STORE_CONSTANTS.`);
}

module.exports = { STORE_METHODS, STORE_CONSTANTS, assertStoreShape };
