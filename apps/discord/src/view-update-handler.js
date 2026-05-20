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
    // Number.isSafeInteger guard mirrors the consumer's parse-time
    // gate. `"3" > 0` would coerce truthy; tightening to strict
    // integer keeps the handler boundary symmetric with the wire
    // boundary so a stringly-typed regression can't slip through.
    if (!(update && Number.isSafeInteger(update.accessCount) && update.accessCount > 0)) return;
    const current = linkStatus.get(qurlId);
    if (!current || current.status === 'opened') return;
    linkStatus.set(qurlId, { ...current, status: 'opened' });
    setViewed(getViewed() + 1);
    // Invariant: push path mutates linkStatus + viewed BEFORE the
    // hasInteraction gate (vs runTick's gate-before-loop in
    // commands.js). Keeps the in-memory counter consistent with the
    // eventual DDB state regardless of render reachability.
    const pending = Math.max(0, getExpectedCount() - getViewed());
    // onAllDone is interaction-independent (clearInterval +
    // allDone=true), so fire it before the hasInteraction gate.
    if (pending === 0) onAllDone();
    if (!hasInteraction()) return;
    // getButtonRow() is a getter (vs. capturing the value) because
    // the monitor reassigns buttonRow to null in stop(); a snapshot
    // at factory-call time would pin a stale reference past stop().
    // .catch is defense-in-depth — `safeEdit` in commands.js already
    // swallows the Discord-side rejection via `logIgnoredDiscordErr`,
    // so this catch only fires if `logIgnoredDiscordErr` itself
    // throws (effectively never). Promise.resolve wrap hardens
    // against a future refactor that switches safeEdit to a sync-
    // return shape — without it, a sync-return safeEdit would make
    // `.catch` throw at runtime.
    Promise.resolve(safeEdit({
      content: buildStatusMsg(),
      components: pending > 0 ? [getButtonRow()] : [],
    })).catch((err) => {
      logger.warn('view-update render failed', {
        sendId,
        qurl_id: qurlId,
        error: err.message,
      });
    });
  };
}

module.exports = {
  createHandleViewUpdate,
};
