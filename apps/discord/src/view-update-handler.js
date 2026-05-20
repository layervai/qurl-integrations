// Render path for view-update push (feat #60). Extracted from
// `monitorLinkStatus` in commands.js so the state matrix can be unit-
// tested without spinning up a full monitor closure (discord.js
// interaction, db, setInterval, etc.).
//
// The factory takes accessor/mutator closures because the monitor's
// state (stopped, viewed, allDone, interaction) is closure-mutable
// inside `monitorLinkStatus`. A flat state-object snapshot would race
// the polling tick's mutations. Same accessor shape as the existing
// `monitorLinkStatus` runTick path.
//
// Contract — the returned handler:
//   1. No-ops when `isStopped()`, `isViewCounterDegraded()`, or
//      `update.accessCount <= 0`.
//   2. No-ops when the qurl_id has already flipped to `status: 'opened'`
//      (same idempotency guard runTick uses; protects against double-
//      increment when push + poll race).
//   3. On a genuine pending → opened transition: mutates `linkStatus`,
//      bumps `viewed` via setViewed, and fires safeEdit with the new
//      status message + buttonRow (or empty components when all viewed).
//   4. Calls `onAllDone()` when viewed === expectedCount so the
//      caller can flip `allDone = true` + clearInterval(timer).
//
// Tests against this factory pin the load-bearing state-matrix paths:
//   - stopped early return
//   - viewCounterDegraded early return
//   - accessCount <= 0 early return
//   - idempotency on status === 'opened'
//   - viewed mutation + safeEdit call shape
//   - onAllDone fires on the transition that takes pending to 0
//
// Render failures are caught + logged (NOT thrown). safeEdit is the
// only async surface here; the consumer's pollLoop has no way to
// recover from a render throw anyway, so swallow + log.

function createHandleViewUpdate({
  sendId,
  linkStatus,
  getButtonRow,
  isStopped,
  isViewCounterDegraded,
  hasInteraction,
  getViewed,
  setViewed,
  getExpectedCount,
  buildStatusMsg,
  safeEdit,
  onAllDone,
  logger,
}) {
  return function handleViewUpdate(update, qurlId) {
    if (isStopped()) return;
    if (isViewCounterDegraded()) return;
    if (!(update && update.accessCount > 0)) return;
    const current = linkStatus.get(qurlId);
    if (!current || current.status === 'opened') return;
    linkStatus.set(qurlId, { ...current, status: 'opened' });
    setViewed(getViewed() + 1);
    // Asymmetry vs runTick (commands.js): the poll path's
    // `if (!interaction) return` GATES before its views loop, so it
    // never bumps `viewed` on a nulled-interaction tick. The push
    // path here bumps first so the in-memory counter stays consistent
    // with the eventual DDB state. Unreachable in practice today —
    // `interaction` is only nulled by stop(), after which isStopped()
    // gates above — but the order matters if a future refactor adds
    // a token-expiry path that nulls interaction without going
    // through stop(). Either-shape behavior is documented; do not
    // "fix" the asymmetry without verifying the polling-path branch
    // too. Same applies to onAllDone below.
    if (!hasInteraction()) return;
    const pending = Math.max(0, getExpectedCount() - getViewed());
    // getButtonRow() is a getter (vs. capturing the value) because
    // the monitor reassigns buttonRow to null in stop(); a snapshot
    // at factory-call time would pin a stale reference past stop().
    safeEdit({
      content: buildStatusMsg(),
      components: pending > 0 ? [getButtonRow()] : [],
    }).catch((err) => {
      logger.warn('view-update render failed', {
        sendId,
        qurl_id: qurlId,
        error: err.message,
      });
    });
    if (pending === 0) onAllDone();
  };
}

module.exports = {
  createHandleViewUpdate,
};
