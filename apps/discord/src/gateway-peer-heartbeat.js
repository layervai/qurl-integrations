// DDB-backed per-replica heartbeat for Pillar 3 standby discovery.
// `gateway_lock` answers "who holds the lock"; this table answers
// "where do I reach the *other* replica to hand off to" at SIGTERM.
// The active replica scans this table, filters by freshness, picks
// the row whose `instance_id != self`, and POSTs the push-handoff
// HMAC body to `http://<ip>:<port>/control/yours`.
//
// Factory `createPeerHeartbeat` returns a per-replica instance. Tests
// run isolated.
//
// ── Load-bearing contracts ──
//
// 1. Freshness filter is the correctness primitive, NOT DDB TTL.
//    DDB TTL deletion is asynchronous (AWS publishes "typically within
//    a few days"). A row visible past `expires_at` would cause the
//    active to POST to a dead peer's IP and miss the handoff window.
//    The application MUST filter peer rows by
//    `updated_at > now - freshnessWindowSeconds` at READ time.
//    Six seconds because the writer cadence is 2 s — three missed
//    heartbeats = peer presumed dead. Do NOT treat TTL absence as
//    a correctness signal; do NOT tighten the TTL to 6 s expecting
//    it to substitute for the freshness filter (deletion lag would
//    still leave stale rows visible).
//
// 2. Single PutItem per renewal — `updated_at` AND `expires_at`
//    written together. Splitting them into separate ops creates a
//    window where a partial write leaves the freshness signal and
//    the TTL out of sync. One write per renewal.
//
// 3. TTL writer shape: epoch SECONDS, not milliseconds. Same
//    convention as flow_state / gateway_lock — DDB TTL only
//    understands seconds-since-epoch, ms would land ~50,000 years
//    in the future to the reaper. `expires_at = floor(clock()/1000)
//    + ttlSeconds`. TTL is 10× the freshness window (60 s default
//    vs 6 s freshness) — long enough that a transient DDB write
//    hiccup doesn't reap a live row.
//
// 4. Scan is the correct access pattern for this table. The single
//    read use case is "find my peer" — one scan, sub-second on a
//    table with at most a handful of rows. ConsistentRead is NOT
//    needed (eventually-consistent reads have sub-second replication
//    lag, well inside the 6 s freshness window, and ConsistentRead
//    doubles RCU cost for no correctness benefit). If a future
//    sharded topology pushes peer row count past ~100, revisit
//    with a GSI on `shard_id` AND a `LastEvaluatedKey` pagination
//    loop (the current call assumes the entire result fits in one
//    1 MB ScanCommand response — fine at 2 replicas, broken at
//    ~1000+).
//
//    Client-side filter vs DDB FilterExpression: we apply the
//    freshness / shard / self filters in Node, not as a
//    FilterExpression on the Scan. FilterExpressions still consume
//    RCU for filtered-out rows (post-Scan, pre-response — no
//    cost-side win), and the network-byte savings are negligible
//    at this row count. If row count ever grows past ~1000 the
//    network bytes start to matter; revisit then.

const net = require('node:net');
const {
  PutCommand,
  ScanCommand,
  DeleteCommand,
} = require('@aws-sdk/lib-dynamodb');

const DEFAULT_FRESHNESS_WINDOW_SECONDS = 6;
const DEFAULT_TTL_SECONDS = 60;

function createPeerHeartbeat({
  ddbClient,
  tableName,
  instanceId,
  ip,
  port,
  shardId,
  // Optional. When set, written as `lock_holder` on the heartbeat
  // row so the active's SIGTERM `transferLock(target, targetHolder)`
  // call has a real holder string for the peer instead of inventing
  // a placeholder. Pure operational-debug metadata; lock correctness
  // doesn't depend on its value.
  lockHolder,
  logger,
  clock = () => Date.now(),
  freshnessWindowSeconds = DEFAULT_FRESHNESS_WINDOW_SECONDS,
  ttlSeconds = DEFAULT_TTL_SECONDS,
} = {}) {
  if (!ddbClient) throw new Error('createPeerHeartbeat: ddbClient is required');
  if (!tableName) throw new Error('createPeerHeartbeat: tableName is required');
  if (!instanceId) throw new Error('createPeerHeartbeat: instanceId is required');
  // Validate as a parseable IPv4/IPv6 literal rather than just
  // truthy. A misconfig that env-stringifies an undefined (passing
  // the string `"undefined"`) would otherwise write a row whose
  // `ip` field looks valid to DDB but is unreachable from the
  // active's POST. `net.isIP` returns 0 for non-IPs, 4/6 otherwise.
  if (!ip || net.isIP(ip) === 0) {
    throw new Error('createPeerHeartbeat: ip (IPv4 or IPv6 literal) is required');
  }
  // Validate port as a TCP port (positive integer, 1-65535) rather
  // than just `typeof === 'number'` — the latter accepts NaN, 0,
  // negatives, fractionals, and >65535 (all of which would produce
  // an unreachable peer entry that's invalid even before DDB sees it).
  if (!Number.isInteger(port) || port <= 0 || port > 65535) {
    throw new Error('createPeerHeartbeat: port (integer 1-65535) is required');
  }
  if (!shardId) throw new Error('createPeerHeartbeat: shardId is required');
  if (!logger) throw new Error('createPeerHeartbeat: logger is required');

  function nowSeconds() {
    return Math.floor(clock() / 1000);
  }

  // Write the heartbeat row. Called every ~2 s from the leader
  // coordinator alongside `gateway-lock.renewLock` (regardless of
  // whether THIS replica holds the lock — both active and standby
  // heartbeat continuously so each can find the other). Idempotent
  // PutItem; no CAS — heartbeats from this replica should always
  // win against any earlier write. Throws on transport errors; the
  // caller decides whether a missed heartbeat is fatal (it isn't —
  // the freshness window absorbs up to three misses).
  async function writeHeartbeat() {
    const now = nowSeconds();
    const item = {
      instance_id: instanceId,
      ip,
      port,
      shard_id: shardId,
      updated_at: now,
      expires_at: now + ttlSeconds,
    };
    // Only write lock_holder if provided. DDB rejects undefined
    // attribute values and back-compat callers may not pass it.
    if (lockHolder) item.lock_holder = lockHolder;
    await ddbClient.send(new PutCommand({
      TableName: tableName,
      Item: item,
    }));
  }

  // Scan-and-filter for peers eligible to receive a push-handoff.
  // Excludes self by `instance_id`, filters by
  // `updated_at > now - freshnessWindowSeconds`, AND filters by
  // `shard_id` so a future sharded topology routes correctly. Sorts
  // freshest-first — the caller takes the head of the list. Returns
  // [] if no fresh peer exists (caller falls through to the
  // cold-fallback path).
  async function listFreshPeers() {
    const cutoff = nowSeconds() - freshnessWindowSeconds;
    const result = await ddbClient.send(new ScanCommand({
      TableName: tableName,
    }));
    const items = result.Items ?? [];
    return items
      .filter((row) => row.instance_id !== instanceId)
      .filter((row) => row.shard_id === shardId)
      .filter((row) => typeof row.updated_at === 'number' && row.updated_at > cutoff)
      // Defense-in-depth: skip any row missing a parseable IPv4/IPv6
      // `ip` or a valid TCP `port`. The write path already validates
      // both, but a corrupt or pre-validator row would otherwise reach
      // the SIGTERM handoff path. The control-client validates again
      // at POST time and would throw — but the cost of catching a bad
      // row here is one filter pass, and the win is that the caller
      // moves on to the next-freshest peer instead of bailing out
      // with `pushHandoff: peerIp required` and losing the handoff.
      .filter((row) => typeof row.ip === 'string' && net.isIP(row.ip) !== 0)
      .filter((row) => Number.isInteger(row.port) && row.port > 0 && row.port <= 65535)
      .sort((a, b) => b.updated_at - a.updated_at);
  }

  // Best-effort delete of this replica's own row at clean shutdown.
  // The freshness filter at read time keeps stale rows invisible
  // anyway, so this is a courtesy that closes the discovery window
  // immediately rather than waiting up to `freshnessWindowSeconds`.
  // Logs but doesn't throw on failure.
  async function deleteOwnRow() {
    try {
      await ddbClient.send(new DeleteCommand({
        TableName: tableName,
        Key: { instance_id: instanceId },
      }));
      logger.debug('gateway-peer-heartbeat: deleted own row', { instanceId });
    } catch (err) {
      logger.warn('gateway-peer-heartbeat: delete own row failed', {
        instanceId, error: err.message,
      });
    }
  }

  return {
    writeHeartbeat,
    listFreshPeers,
    deleteOwnRow,
  };
}

module.exports = {
  createPeerHeartbeat,
  DEFAULT_FRESHNESS_WINDOW_SECONDS,
  DEFAULT_TTL_SECONDS,
};
