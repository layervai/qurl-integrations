// DDB-backed per-shard leader-election lock for Pillar 3's hot-standby
// gateway pair. Exactly one of the two `bot_gateway` replicas holds the
// lock for a given shard at any time; the non-holder boots fully (DDB
// clients open, control-channel bound, `/health` returning 200) but
// skips `WebSocketManager.connect()` so Discord only sees one active
// WS per shard.
//
// Factory `createGatewayLock` returns a per-shard instance with its own
// version cursor. Tests run isolated; a future multi-shard caller can
// construct one per shard.
//
// ── Concurrency: single-caller only ──
//
// `currentVersion` is closure state mutated by acquireLock / renewLock
// / transferLock / adoptLockFromHandoff. JS's single-threaded event
// loop makes each read-then-write atomic at the await boundary, but
// two concurrent renewLock invocations (e.g., a scheduled tick
// overlapping a watchdog-triggered renew) would both read version
// `N`, both attempt to write `N+1`, and the second's CAS fails —
// clearing `currentVersion=null` even though the lock is still held.
// The leader-coordinator in PR 13b.2 must serialize all mutator
// calls (one outstanding renew at a time; transfer/release run only
// from the SIGTERM path which has the loop stopped). The diagnostic
// `readCurrentHolder` is the only safe overlap.
//
// ── Release semantics ──
//
// `releaseLock` is intended for the no-peer SIGTERM path (active dies
// with no standby to hand to). Both transport errors and CAS failures
// clear `currentVersion=null` so the caller's state doesn't outlive
// the lock. This is correct on the SIGTERM path (process is exiting)
// but would be over-aggressive on a voluntary-step-down path — a
// future watchdog-driven release would need to distinguish "lock is
// gone" from "DDB transient error, retry" rather than zero the
// cursor on either. Today's only caller is gracefulShutdown.
//
// ── Load-bearing contracts ──
//
// 1. Conditional-write is the lock primitive, NOT DDB TTL. The TTL
//    attribute (`expires_at`) is a background janitor that reaps
//    long-decommissioned shard rows. Lock correctness lives in the
//    `ConditionExpression` on every acquire/renew/transfer — DDB TTL
//    deletion is asynchronous (AWS publishes "typically within a few
//    days") and a row with `now > expires_at` that hasn't been reaped
//    is still returned by GetItem.
//
// 2. TTL writer shape: epoch SECONDS, not milliseconds. DDB TTL only
//    understands seconds-since-epoch; a ms-encoded value would land
//    ~50,000 years in the future to the reaper, leaving rows alive
//    forever. Matches the convention on `flow_state` and
//    `gateway_session`. ALL writers (acquire, renew, transfer) MUST
//    pass `Math.floor(clock() / 1000) + ttlSeconds`.
//
// 3. `instance_id` + `version` are the CAS guard. Renew/transfer use
//    `ConditionExpression = "instance_id = :self AND version =
//    :expected"`. A stale process whose lease expired and was re-
//    acquired by a peer cannot accidentally succeed on a delayed
//    renew — the version moved underneath it. This is the fencing-
//    token primitive (cf. Martin Kleppmann's "How to do distributed
//    locking") that makes the TTL clock-skew tolerant: even a clock-
//    skewed peer that thinks the lock has expired cannot actually
//    take it while the legitimate holder is still heartbeating;
//    their write hits a ConditionalCheckFailedException.
//
//    Scope note: `version` is scoped to the CURRENT lock tenancy.
//    Each `acquireLock` after a release / lapse / transfer resets
//    the counter to 1; it is NOT a globally monotonic fencing token
//    in the Kleppmann-treats-it-as-external sense. The internal-CAS
//    use is sound because every CAS pairs `version` with `instance_id`,
//    and `instance_id` is unique per process. If any downstream
//    system ever consumes `version` as an EXTERNAL fencing token
//    (e.g., a separate storage system that wants "is this fence
//    number ahead of the last one we saw"), the per-acquire reset
//    breaks that assumption and the consumer would need to pair
//    `version` with `instance_id` or read the holder row.
//
// 4. Release uses `DeleteItem`, not `UpdateItem REMOVE lock_holder`.
//    A REMOVE would leave `expires_at` populated, so a peer's next
//    acquire couldn't take the `attribute_not_exists(lock_holder)`
//    branch and would have to wait for the (now-stale) `expires_at`
//    to lapse — adding lease-duration latency to a clean handoff.
//    DeleteItem re-arms the `attribute_not_exists` branch so the
//    peer acquires immediately. Crash-without-release falls back to
//    lease expiry (~6 s) which is the design's worst-case floor.
//
// 5. acquire uses `PutItem`, not `UpdateItem`. PutItem cleanly handles
//    the post-DeleteItem "row absent" case in one round-trip
//    (UpdateItem would need a follow-up to write the full row); it
//    also makes acquire idempotent on ConditionalCheckFailedException.
//    Caveat: PutItem replaces the entire item, so any future non-CAS-
//    managed attribute (e.g., a diagnostic `last_acquired_at`) MUST
//    be included in every acquire's PutItem payload or moved to a
//    sibling table — otherwise acquire silently wipes it.

const {
  PutCommand,
  GetCommand,
  UpdateCommand,
  DeleteCommand,
} = require('@aws-sdk/lib-dynamodb');

// Lease duration in seconds. 6 s sets the cold-fallback floor at
// ~6 s + RESUME RTT ≈ 7 s. Paired with the 2 s renew cadence elsewhere
// — three missed renewals = lock becomes acquirable by a peer.
const DEFAULT_TTL_SECONDS = 6;

function createGatewayLock({
  ddbClient,
  tableName,
  shardId,
  instanceId,
  lockHolder,
  logger,
  // Injected for deterministic tests. Production uses Date.now.
  clock = () => Date.now(),
  ttlSeconds = DEFAULT_TTL_SECONDS,
} = {}) {
  if (!ddbClient) throw new Error('createGatewayLock: ddbClient is required');
  if (!tableName) throw new Error('createGatewayLock: tableName is required');
  if (!shardId) throw new Error('createGatewayLock: shardId is required');
  if (!instanceId) throw new Error('createGatewayLock: instanceId is required');
  if (!lockHolder) throw new Error('createGatewayLock: lockHolder is required');
  if (!logger) throw new Error('createGatewayLock: logger is required');

  // Tracks the version cursor across renew/transfer ops. Set on
  // successful acquire/renew (we always know what version we just
  // wrote); used as :expected on the next CAS. null when we don't
  // hold the lock.
  let currentVersion = null;

  function nowSeconds() {
    return Math.floor(clock() / 1000);
  }

  // Acquire the lock for this instance. Returns { acquired: true,
  // version } on success, { acquired: false } when a peer holds a
  // live lease. Throws on transport errors (caller decides retry).
  async function acquireLock() {
    const now = nowSeconds();
    const newVersion = 1;
    try {
      await ddbClient.send(new PutCommand({
        TableName: tableName,
        Item: {
          shard_id: shardId,
          lock_holder: lockHolder,
          instance_id: instanceId,
          version: newVersion,
          expires_at: now + ttlSeconds,
        },
        ConditionExpression:
          'attribute_not_exists(lock_holder) ' +
          'OR attribute_not_exists(expires_at) ' +
          'OR expires_at < :now',
        ExpressionAttributeValues: { ':now': now },
      }));
      currentVersion = newVersion;
      logger.info('gateway-lock: acquired', {
        shardId, instanceId, version: newVersion, ttlSeconds,
      });
      return { acquired: true, version: newVersion };
    } catch (err) {
      if (err.name === 'ConditionalCheckFailedException') {
        logger.debug('gateway-lock: acquire failed (peer holds live lease)', {
          shardId, instanceId,
        });
        return { acquired: false };
      }
      throw err;
    }
  }

  // Renew the lease. Returns { renewed: true, version } on success,
  // { renewed: false } if the CAS guard fails (we lost the lock, or a
  // version-collision race). Caller should treat renewed=false as
  // "you no longer hold the lock" and stop the WS / release downstream
  // resources.
  async function renewLock() {
    if (currentVersion === null) {
      logger.warn('gateway-lock: renew called without prior acquire', {
        shardId, instanceId,
      });
      return { renewed: false };
    }
    const now = nowSeconds();
    const nextVersion = currentVersion + 1;
    try {
      // No `lock_holder = :holder` in SET — we already hold the
      // lock, so lock_holder is unchanged. Including it would
      // burn ~1 WCU byte per renew with no semantic effect.
      // (acquireLock writes the full row; transferLock writes
      // the new holder. Both code paths that need to set
      // lock_holder do so explicitly.)
      await ddbClient.send(new UpdateCommand({
        TableName: tableName,
        Key: { shard_id: shardId },
        UpdateExpression: 'SET version = :next, expires_at = :exp',
        ConditionExpression: 'instance_id = :self AND version = :expected',
        ExpressionAttributeValues: {
          ':next': nextVersion,
          ':exp': now + ttlSeconds,
          ':self': instanceId,
          ':expected': currentVersion,
        },
      }));
      currentVersion = nextVersion;
      return { renewed: true, version: nextVersion };
    } catch (err) {
      if (err.name === 'ConditionalCheckFailedException') {
        logger.warn('gateway-lock: renew CAS failed — lock lost', {
          shardId, instanceId, expectedVersion: currentVersion,
        });
        currentVersion = null;
        return { renewed: false };
      }
      throw err;
    }
  }

  // Atomic ownership transfer to a peer. Used by the SIGTERM push-
  // handoff path: the active hands the lock over in one DDB op, with
  // no lock-released-but-not-acquired-yet window. Returns { transferred:
  // true, version } on success. On CAS failure (we don't hold the lock,
  // or version moved), returns { transferred: false } and the caller
  // should fall through to a clean exit — the peer will acquire via
  // the cold-fallback path.
  async function transferLock(targetInstanceId, targetLockHolder) {
    if (currentVersion === null) {
      logger.warn('gateway-lock: transfer called without prior acquire', {
        shardId, instanceId,
      });
      return { transferred: false };
    }
    if (targetInstanceId === instanceId) {
      // Self-handoff is meaningless and would still bump the version
      // (the CAS would succeed, since we're a holder). Reject at the
      // API boundary so a caller bug (e.g., a peer-discovery lookup
      // that accidentally returns our own row) doesn't silently churn
      // the version counter.
      logger.warn('gateway-lock: transferLock called with self as target (no-op)', {
        shardId, instanceId,
      });
      return { transferred: false };
    }
    const now = nowSeconds();
    const nextVersion = currentVersion + 1;
    try {
      await ddbClient.send(new UpdateCommand({
        TableName: tableName,
        Key: { shard_id: shardId },
        UpdateExpression:
          'SET instance_id = :peer, lock_holder = :peerHolder, ' +
          'version = :next, expires_at = :exp',
        ConditionExpression: 'instance_id = :self AND version = :expected',
        ExpressionAttributeValues: {
          ':peer': targetInstanceId,
          ':peerHolder': targetLockHolder,
          ':next': nextVersion,
          ':exp': now + ttlSeconds,
          ':self': instanceId,
          ':expected': currentVersion,
        },
      }));
      logger.info('gateway-lock: transferred', {
        shardId, fromInstanceId: instanceId, toInstanceId: targetInstanceId,
        version: nextVersion,
      });
      currentVersion = null;
      return { transferred: true, version: nextVersion };
    } catch (err) {
      if (err.name === 'ConditionalCheckFailedException') {
        logger.warn('gateway-lock: transfer CAS failed', {
          shardId, instanceId, expectedVersion: currentVersion,
        });
        return { transferred: false };
      }
      throw err;
    }
  }

  // Best-effort release. Used by the no-peer SIGTERM path (active dies
  // with no standby to hand to) so a future replacement task can
  // acquire immediately rather than wait for the ~6 s lease lapse.
  // The CAS guard on `instance_id` prevents us from deleting a row
  // owned by a peer that took over while we were tearing down. Logs
  // but doesn't throw — the worst case is the lease lapses naturally.
  async function releaseLock() {
    if (currentVersion === null) {
      logger.debug('gateway-lock: release called without prior acquire (no-op)', {
        shardId, instanceId,
      });
      return { released: false };
    }
    try {
      await ddbClient.send(new DeleteCommand({
        TableName: tableName,
        Key: { shard_id: shardId },
        ConditionExpression: 'instance_id = :self',
        ExpressionAttributeValues: { ':self': instanceId },
      }));
      logger.info('gateway-lock: released', { shardId, instanceId });
      currentVersion = null;
      return { released: true };
    } catch (err) {
      if (err.name === 'ConditionalCheckFailedException') {
        logger.warn('gateway-lock: release CAS failed (peer took over)', {
          shardId, instanceId,
        });
        currentVersion = null;
        return { released: false };
      }
      logger.error('gateway-lock: release failed', {
        shardId, instanceId, error: err.message,
      });
      currentVersion = null;
      return { released: false };
    }
  }

  // Bootstrap the local version cursor after receiving a lock via
  // cross-process handoff. The producer of this call is PR 13b.2's
  // control-channel handler: after HMAC-verifying a `POST /control/yours`
  // body and observing that the active's `transferLock` succeeded
  // (DDB row now shows this replica as `instance_id`, version=`v`),
  // the standby must seed its own gateway-lock instance's version
  // cursor with `v` so the next `renewLock` finds a non-null
  // `currentVersion` and passes its CAS guard.
  //
  // No DDB write. The active's transferLock already wrote the row;
  // this call just synchronizes local state with what already
  // exists in DDB. Safe to call multiple times — the cursor just
  // re-anchors.
  function adoptLockFromHandoff(versionAfterTransfer) {
    if (!Number.isInteger(versionAfterTransfer) || versionAfterTransfer < 1) {
      throw new Error(
        `gateway-lock: adoptLockFromHandoff requires a positive integer version (got ${versionAfterTransfer})`
      );
    }
    currentVersion = versionAfterTransfer;
    logger.info('gateway-lock: adopted from handoff', {
      shardId, instanceId, version: versionAfterTransfer,
    });
  }

  // Read the current holder row for diagnostics (/health, debug logs).
  // NOT a correctness primitive — the conditional writes above are
  // the lock contract. Returns the row or null if absent.
  async function readCurrentHolder() {
    const result = await ddbClient.send(new GetCommand({
      TableName: tableName,
      Key: { shard_id: shardId },
    }));
    return result.Item ?? null;
  }

  return {
    acquireLock,
    renewLock,
    transferLock,
    adoptLockFromHandoff,
    releaseLock,
    readCurrentHolder,
    // Inspection seam for tests. Production callers track version
    // through acquire/renew/transfer return values instead.
    _getVersionForTest() {
      return currentVersion;
    },
  };
}

module.exports = {
  createGatewayLock,
  DEFAULT_TTL_SECONDS,
};
