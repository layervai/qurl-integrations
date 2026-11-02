// shortcut: budgets are per process; use a shared store if cross-replica abuse requires one global user budget.
const BURST = 10;
const WINDOW_MS = 10_000;
const TOKEN_MS = WINDOW_MS / BURST;
const MAX_USERS = 10_000;
const autocompleteBuckets = new Map();

function takeAutocompleteToken(userId) {
  if (typeof userId !== 'string' || userId.length === 0 || userId.length > 64) return false;
  const now = Date.now();
  const previous = autocompleteBuckets.get(userId);
  if (!previous && autocompleteBuckets.size >= MAX_USERS) {
    const [oldestId, oldest] = autocompleteBuckets.entries().next().value;
    if (now - oldest.updatedAt < WINDOW_MS) return false;
    autocompleteBuckets.delete(oldestId);
  }
  const creditMs = previous
    ? Math.min(WINDOW_MS, previous.creditMs + Math.max(0, now - previous.updatedAt))
    : WINDOW_MS;
  const allowed = creditMs >= TOKEN_MS;
  // Refresh insertion order without discarding an active user's accumulated budget.
  autocompleteBuckets.delete(userId);
  autocompleteBuckets.set(userId, { creditMs: allowed ? creditMs - TOKEN_MS : creditMs, updatedAt: Math.max(now, previous?.updatedAt ?? now) });
  return allowed;
}

module.exports = { takeAutocompleteToken, autocompleteBuckets };
