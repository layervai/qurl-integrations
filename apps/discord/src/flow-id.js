// flow-id — canonical parse/build for the shard-aware composite key
// used by `flow_state.flow_id` (see qurl-integrations-infra
// modules/qurl-bot-ddb flow_state table).
//
// Format: `<shard_id>#<guild_id>#<channel_id>#<user_id>`
//
// Asymmetric separator rules — `shard_id` may contain `:` (the
// canonical shard encoding is `k:n`, e.g. `0:1` for single-shard,
// `3:8` for shard 3 of 8); `:` is therefore NOT used as the
// flow_id-level separator. The flow-level separator is `#`, which
// is FORBIDDEN inside every component (including shard_id). This
// keeps split-on-first-`#` parseable AND lets shard_id retain its
// natural `k:n` shape.
//
// Why centralize this in one module instead of inlining the join /
// split at every callsite (handler, flow-state, future leader-
// election lookup, audit-log forensic queries):
//
//   - A handler computing `flow_id = `${shard}#${guild}#${chan}#${user}``
//     and a worker computing the same on its side will drift the
//     first time someone changes the separator convention. The
//     OCC-on-flow_id contract relies on byte-identical keys across
//     both sides; the canonical build/parse pair removes the drift
//     surface.
//   - Validation lives in one place. A `guild_id` containing `#`
//     (impossible per Discord's snowflake encoding today, but cheap
//     to defend against if Discord ever ships a non-numeric guild
//     identifier) would silently produce a malformed flow_id and a
//     parseFlowId() at the other end would yield wrong fields. Reject
//     at build time, not at deserialization time.

const SEP = '#';

// Snowflake-ish components MUST NOT contain `#`. We intentionally do
// NOT check the colon — `:` is legal inside `shard_id` and irrelevant
// for the other components.
function assertNoSep(value, fieldName) {
  if (typeof value !== 'string' || value.length === 0) {
    throw new TypeError(`flow-id: ${fieldName} must be a non-empty string (got ${typeof value === 'string' ? 'empty string' : typeof value})`);
  }
  if (value.includes(SEP)) {
    throw new TypeError(`flow-id: ${fieldName} must not contain '${SEP}' (got ${JSON.stringify(value)}); the '#' character is reserved as the flow_id component separator`);
  }
}

// Build a flow_id from its four components. Order is fixed:
// shard → guild → channel → user. Callers should treat the returned
// string as opaque; do not parse it inline — use parseFlowId().
function buildFlowId({ shard_id, guild_id, channel_id, user_id }) {
  assertNoSep(shard_id, 'shard_id');
  assertNoSep(guild_id, 'guild_id');
  assertNoSep(channel_id, 'channel_id');
  assertNoSep(user_id, 'user_id');
  return `${shard_id}${SEP}${guild_id}${SEP}${channel_id}${SEP}${user_id}`;
}

// Inverse of buildFlowId. Returns `null` for any malformed input
// (wrong type, wrong shape, empty component) so callers can decide
// whether to log + skip or throw. Throwing on a malformed flow_id
// pulled from DDB would break the audit/forensic path — a
// best-effort parse + null sentinel preserves that path.
//
// Split is exact: exactly 3 `#` separators produces 4 components.
// More or fewer is rejected (returns null).
function parseFlowId(flow_id) {
  if (typeof flow_id !== 'string' || flow_id.length === 0) return null;
  const parts = flow_id.split(SEP);
  if (parts.length !== 4) return null;
  const [shard_id, guild_id, channel_id, user_id] = parts;
  if (!shard_id || !guild_id || !channel_id || !user_id) return null;
  return { shard_id, guild_id, channel_id, user_id };
}

module.exports = { buildFlowId, parseFlowId };
