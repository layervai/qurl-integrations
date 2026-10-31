// shortcut: budgets are per process; use a shared store if cross-replica abuse requires one global user budget.
const BURST = 10;
const WINDOW_MS = 10_000;
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
  const tokens = previous
    ? Math.min(BURST, previous.tokens + Math.max(0, now - previous.updatedAt) * BURST / WINDOW_MS)
    : BURST;
  const allowed = tokens >= 1;
  // Refresh insertion order without discarding an active user's accumulated budget.
  autocompleteBuckets.delete(userId);
  autocompleteBuckets.set(userId, { tokens: allowed ? tokens - 1 : tokens, updatedAt: Math.max(now, previous?.updatedAt ?? now) });
  return allowed;
}

module.exports = { takeAutocompleteToken, autocompleteBuckets };
