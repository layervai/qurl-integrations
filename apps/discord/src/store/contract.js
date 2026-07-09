// store/contract — the Store interface
//
// The bot's data layer is reached through a single `Store` object that
// every consumer gets via `require('./store')`. The contract below lists
// every method a Store backend MUST implement. Runtime assertion at boot
// (see `assertStoreShape`) fails fast in a real (non-Jest) boot if a
// backend drops a method — so a missing implementation surfaces as a
// clear boot-time error, not as `TypeError: store.xMethod is not a
// function` deep in a request path. Under Jest the boot-time assertion
// is intentionally skipped so partial `jest.mock('../src/store', …)`
// stubs keep working; the invariant is instead enforced by
// `tests/store-contract.test.js`, which calls `assertStoreShape`
// directly against both a complete fixture AND the real default
// backend AND a `child_process.spawnSync` of the real boot path.
//
// Backend lifecycle: every Store *method* (everything in `STORE_METHODS`
// below) returns a Promise. ddb-store (the only supported backend) is
// async-native. If a sync backend is ever re-added, it must still
// expose Promise-returning methods so callers can `await` uniformly
// across backends. The `STORE_CONSTANTS` block (e.g. `BADGE_TYPES`,
// `BADGE_INFO`) is exempt — those are plain-value exports, not
// methods, and `assertStoreShape` honors the distinction.
//
// Scope of `assertStoreShape`: name-level only. A backend that exports
// `createPendingLink` with a wrong arity / wrong return shape passes
// this check happily, then crashes at the first call site. Parameter
// and return-shape parity between backends is the test suite's job
// (run the same behavioral tests against each backend's mock instance).
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
  'markSendDMDelivered',
  'getRecentSends',
  'markSendRevoked',
  'isSendRevoked',
  'saveSendConfig',
  'getSendConfig',
  'getSendResourceIds',
  'getSendItems',
  'findSendsByQurlId',
  'markExpiredDMEdited',
  'clearExpiredDMEdited',
  'markConsumedDMEdited',
  'clearConsumedDMEdited',

  // View-counter confirmation render state (cross-replica fast-path).
  // saveSendConfirmState persists the render-state fields AFTER the
  // initial editReply (separate from saveSendConfig, which runs earlier
  // before the token/baseMsg exist); the rest are the read + mutate
  // surface PR-B's webhook fast-path drives.
  'saveSendConfirmState',
  'getSendRenderState',
  'incrementSendViewedCount',
  'getSendViewedCount',
  'getSendRenderedCount',
  'tryAdvanceRenderedCount',
  'touchRenderedAt',
  'tryClaimRenderAttempt',
  'markConfirmTerminal',

  // QURL views (webhook-fed view counter)
  'recordQurlView',
  'getQurlViews',

  // Guild (BYOK) API keys
  'getGuildApiKey',
  'setGuildApiKey',
  // Raw delete — see ddb-store.js's defensive comment. No production
  // caller today. A future /qurl unlink admin command MUST tear down
  // the qurl-service subscription BEFORE invoking this, otherwise it
  // leaks an orphan webhook on qurl-service. See the link-side
  // pattern in guild-webhook-link.js::linkGuildWebhookSubscription.
  '_removeGuildApiKeyRaw',
  'getGuildConfig',
  'getGuildConfigWithApiKey',

  // Per-guild qurl-service webhook subscriptions (BYOK view counter)
  'setGuildWebhookSubscription',
  'clearGuildWebhookSubscription',
  'listGuildSubscriptionsByOwner',
  'scanGuildSubscriptions',
  'propagateGuildWebhookSubscription',

  // Orphaned OAuth tokens (background revoke-retry queue)
  'recordOrphanedToken',
  'countOrphanedTokens',
  'listOrphanedTokens',
  'decryptOrphanedToken',
  'deleteOrphanedToken',

  // Lifecycle
  'close',
  // Cheap "is the data layer functional" probe — called by /health
  // at LB-cadence (10–30s typical). Backends must keep this O(1):
  // ddb-store uses a single GetItem on a sentinel key. NEVER use
  // this for aggregation / scan work; /metrics is the right home
  // for that.
  'healthCheck',
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
