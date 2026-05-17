// Pillar 3 chaos validation — RESUME-fail (watchdog retries exhausted).
//
// The watchdog's unit test (gateway-connection-watchdog.test.js, the
// "step() — exhaustion path" describe block) already covers the
// exhaustion-exit branch at high fidelity with `releaseLock` and
// `deleteOwnRow` mocked as `jest.fn()`. What this chaos test adds is
// the COMPOSITION-level guarantee: the watchdog's failure-exit path
// drives the REAL gateway-lock + REAL peer-heartbeat primitives, and
// we observe DDB-level row deletion. A future refactor that
// disconnects the watchdog's releaseLock wiring from the real lock
// primitive (e.g., passing a no-op closure) would pass the unit test
// but leave the lock row stuck until TTL — this test catches that.
//
// Design-doc acceptance criterion (zero-downtime-design.md lines 607-612):
//   At maxAttempts (5), the watchdog "releases the lock and exit(1)
//   so ECS replaces the task". This test pins both effects landing
//   on the actual DDB row state, not just the spy call counts.
//
// What this test does NOT cover (out of scope vs the unit test):
//   - backoff timing (sleep is injected as a no-op stub here; the
//     unit test pins 200/400/800/1600 ms)
//   - per-attempt logging shape
//   - hung releaseLock with Promise.race ceiling (unit test pins this
//     with a hand-stubbed releaseLock; replicating with real DDB
//     would mean a DDB call that never resolves, which is at odds
//     with mockClient's call-and-respond model)
//   - the leader's `isConnecting` true case (covered by leader unit
//     tests; not a chaos scenario)

const { DeleteCommand } = require('@aws-sdk/lib-dynamodb');

const { createGatewayLock } = require('../src/gateway-lock');
const { createPeerHeartbeat } = require('../src/gateway-peer-heartbeat');
const { createConnectionWatchdog } = require('../src/gateway-connection-watchdog');
const {
  setupChaosDdb, makeChaosLogger, makeCcfe,
  LOCK_TABLE, HEARTBEAT_TABLE, SHARD_ID,
  INSTANCE_A, INSTANCE_B, HOLDER_A,
  assertNoUnexpectedTableCalls,
} = require('./helpers/chaos-ddb');

describe('Pillar 3 chaos — RESUME-fail (watchdog exhausts retries)', () => {
  const now = 1_700_000_000_000;
  const clock = () => now;

  it('5 consecutive connect() failures → DDB lock row deleted + heartbeat row deleted + exit(1)', async () => {
    const nowSeconds = Math.floor(now / 1000);
    // A holds the lock (version=3, lease alive). Heartbeat rows for
    // both A (the dying replica) and B (a peer that won't be reached
    // before exit anyway). Seeded so the heartbeat-delete on A is
    // observable as a row-count delta vs an irrelevant peer.
    const { docClient, ddbMock, state } = setupChaosDdb({
      initialLockRow: {
        shard_id: SHARD_ID, instance_id: INSTANCE_A, lock_holder: HOLDER_A,
        version: 3, expires_at: nowSeconds + 6,
      },
      initialPeerRows: [
        {
          instance_id: INSTANCE_A, ip: '10.0.0.10', port: 7800,
          shard_id: SHARD_ID, updated_at: nowSeconds, expires_at: nowSeconds + 60,
          lock_holder: HOLDER_A,
        },
        {
          instance_id: INSTANCE_B, ip: '10.0.0.20', port: 7800,
          shard_id: SHARD_ID, updated_at: nowSeconds, expires_at: nowSeconds + 60,
        },
      ],
    });

    const logger = makeChaosLogger();
    const lock = createGatewayLock({
      ddbClient: docClient, tableName: LOCK_TABLE, shardId: SHARD_ID,
      instanceId: INSTANCE_A, lockHolder: HOLDER_A, logger, clock,
    });
    // Synthesize prior acquisition (version cursor at 3) so the
    // watchdog's releaseLock CAS-Delete has a non-null currentVersion
    // to consume. Mirrors the post-tick state we'd see in production.
    lock.adoptLockFromHandoff(3);

    const heartbeat = createPeerHeartbeat({
      ddbClient: docClient, tableName: HEARTBEAT_TABLE,
      instanceId: INSTANCE_A, ip: '10.0.0.10', port: 7800,
      shardId: SHARD_ID, lockHolder: HOLDER_A, logger, clock,
    });

    // Persistent-fail manager — every connect() rejects. Mirrors the
    // production failure mode where Discord gateway endpoint
    // resolution / TLS / WS upgrade fails consecutively (e.g., during
    // a region-wide DNS outage). isConnected stays false throughout.
    const manager = {
      isConnected: jest.fn(() => false),
      connect: jest.fn().mockRejectedValue(new Error('econnrefused')),
    };

    const exit = jest.fn();

    const watchdog = createConnectionWatchdog({
      manager,
      isHoldingLock: () => true,
      isConnecting: () => false,
      releaseLock: () => lock.releaseLock(),
      deleteOwnRow: () => heartbeat.deleteOwnRow(),
      logger,
      maxAttempts: 5,
      // No-op sleep so backoff doesn't wall-clock the test. Backoff
      // timing is covered by the unit test (200/400/800/1600 ms ladder).
      sleep: jest.fn(async () => {}),
      exit,
    });

    // Drive 5 failed step() iterations — equivalent to 5 watchdog
    // ticks in production. After the 5th, the exhaustion-exit branch
    // fires.
    for (let i = 0; i < 5; i += 1) {
      // eslint-disable-next-line no-await-in-loop
      await watchdog._stepForTest();
    }

    // ── Real-DDB-level assertions ──

    // (1) The lock row in DDB has been DELETED. This is the strongest
    //     signal that the cold-fallback floor (~6 s TTL lapse) is
    //     skipped on the watchdog-exit path — a peer can acquire
    //     immediately because the row is gone, not just expired.
    expect(state.lockRow).toBeNull();
    // Confirm via DDB call inspection: a DeleteCommand against the
    // lock table landed exactly once. `commandCalls`' second arg is
    // a per-input filter — the same shape mockClient uses for routing.
    const lockDeletes = ddbMock.commandCalls(DeleteCommand, { TableName: LOCK_TABLE });
    expect(lockDeletes).toHaveLength(1);
    // CAS guard MUST be present — without it, a peer that already
    // cold-acquired would have its row clobbered.
    expect(lockDeletes[0].args[0].input.ConditionExpression).toContain('instance_id = :self');

    // (2) The heartbeat row for A has been deleted. Closes the peer-
    //     discovery window immediately so a future replacement's
    //     listFreshPeers stops returning the dead row.
    const remainingHeartbeats = state.peerRows.map((r) => r.instance_id);
    expect(remainingHeartbeats).not.toContain(INSTANCE_A);
    expect(remainingHeartbeats).toContain(INSTANCE_B); // sanity: didn't nuke the wrong row.

    // (3) Exit fired with code 1 — ECS replaces the task.
    expect(exit).toHaveBeenCalledWith(1);
    expect(exit).toHaveBeenCalledTimes(1);

    // (4) Five connect() attempts occurred — the ladder ran to
    //     completion, not short-circuited.
    expect(manager.connect).toHaveBeenCalledTimes(5);

    // Forbidden-table guard: the watchdog's exhaustion-exit path is
    // gateway-tier and must NOT touch flow_state (or any other table
    // outside the lock/heartbeat allowlist).
    assertNoUnexpectedTableCalls(ddbMock);
  });

  it('lock-table DeleteCommand mocked to throw CCFE → exit(1) still fires (defensive)', async () => {
    // Pathological case: the watchdog's releaseLock call hits a CAS
    // failure (peer already cold-acquired during the failure ladder).
    // gateway-lock returns { released: false } and logs a warn; the
    // watchdog catches no throw, proceeds to deleteOwnRow, then exits.
    // Pinning this so a future refactor that promotes the
    // releaseLock-returns-false outcome to a throw doesn't silently
    // skip the heartbeat-row delete + exit(1) — the surviving
    // failover slot would stay held with no live gateway.
    const nowSeconds = Math.floor(now / 1000);
    const { docClient, ddbMock, state } = setupChaosDdb({
      initialLockRow: {
        shard_id: SHARD_ID, instance_id: INSTANCE_A, lock_holder: HOLDER_A,
        version: 3, expires_at: nowSeconds + 6,
      },
      initialPeerRows: [{
        instance_id: INSTANCE_A, ip: '10.0.0.10', port: 7800,
        shard_id: SHARD_ID, updated_at: nowSeconds, expires_at: nowSeconds + 60,
        lock_holder: HOLDER_A,
      }],
    });

    const logger = makeChaosLogger();
    const lock = createGatewayLock({
      ddbClient: docClient, tableName: LOCK_TABLE, shardId: SHARD_ID,
      instanceId: INSTANCE_A, lockHolder: HOLDER_A, logger, clock,
    });
    lock.adoptLockFromHandoff(3);
    // Override DeleteCommand for the lock table to throw a CAS error
    // — simulates a peer cold-acquiring during the failure ladder.
    ddbMock.on(DeleteCommand, { TableName: LOCK_TABLE }).callsFake(() => { throw makeCcfe(); });

    const heartbeat = createPeerHeartbeat({
      ddbClient: docClient, tableName: HEARTBEAT_TABLE,
      instanceId: INSTANCE_A, ip: '10.0.0.10', port: 7800,
      shardId: SHARD_ID, lockHolder: HOLDER_A, logger, clock,
    });

    const manager = {
      isConnected: jest.fn(() => false),
      connect: jest.fn().mockRejectedValue(new Error('econnrefused')),
    };
    const exit = jest.fn();

    const watchdog = createConnectionWatchdog({
      manager,
      isHoldingLock: () => true,
      isConnecting: () => false,
      releaseLock: () => lock.releaseLock(),
      deleteOwnRow: () => heartbeat.deleteOwnRow(),
      // maxAttempts=3 (vs 5 in test #1) because this test pins the
      // exhaustion-branch outcome under CAS-failure, not the ladder
      // depth. The 200/400/800/1600 ms ladder is covered by the
      // watchdog's unit test; 3 attempts is enough to enter the
      // exhaustion exit and faster than the 5-attempt setup.
      logger, maxAttempts: 3,
      sleep: jest.fn(async () => {}),
      exit,
    });

    for (let i = 0; i < 3; i += 1) {
      // eslint-disable-next-line no-await-in-loop
      await watchdog._stepForTest();
    }

    // Lock-row Delete CAS failed → row stays in DDB. But heartbeat
    // row deleted, exit(1) fired. The system is still recoverable
    // (TTL reaps the row inside 6 s); the assertion is that
    // exit(1) and heartbeat-cleanup both fired despite the lock
    // delete CAS failure.
    expect(state.lockRow).not.toBeNull();
    // Pin the row's instance_id — recovery story is "TTL reaps the
    // row inside 6 s" (the lock is still held by A in DDB; only the
    // TTL lapse will free it). A future refactor that succeeds the
    // delete via a different path (without throwing) would set
    // state.lockRow=null and silently pass the .not.toBeNull check.
    expect(state.lockRow.instance_id).toBe(INSTANCE_A);
    // Heartbeat row for A is gone. Mirror test #1's map+not.toContain
    // shape so both heartbeat-row assertions read identically.
    const remainingHeartbeats = state.peerRows.map((r) => r.instance_id);
    expect(remainingHeartbeats).not.toContain(INSTANCE_A);
    expect(exit).toHaveBeenCalledWith(1);
    // gateway-lock.releaseLock logs `release CAS failed (peer took
    // over)` at warn on CCFE — pinning the log so a future regression
    // that silently swallows the warning (and lose the operational
    // signal) is caught here.
    expect(logger.warn).toHaveBeenCalledWith(
      expect.stringContaining('release CAS failed'),
      expect.any(Object),
    );

    // Forbidden-table guard: same invariant as test #1 — even on the
    // CAS-failure branch the watchdog must not write outside the
    // lock/heartbeat allowlist.
    assertNoUnexpectedTableCalls(ddbMock);
  });
});
