// Unit tests for src/gateway-lock.js — Pillar 3 leader-election
// lock primitive. Pins the five load-bearing contracts:
//
//   1. Conditional-write IS the lock (TTL is janitor only).
//   2. TTL writer shape is epoch SECONDS, never milliseconds.
//   3. instance_id + version CAS guards renew/transfer against a
//      stale-process-vs-peer-took-over race.
//   4. Release uses DeleteItem (re-arms attribute_not_exists for
//      clean handoff), not UpdateItem REMOVE.
//   5. Acquire uses PutItem (single round-trip for the post-Delete
//      row-absent case; idempotent on cond fail).
//
// Each contract has a real failure mode the test would catch in
// production: a writer that sends ms-as-N TTL would leave rows
// alive forever; a missing version CAS would let a zombie task
// renew a lock peer has taken; etc.

const { mockClient } = require('aws-sdk-client-mock');
const {
  DynamoDBDocumentClient,
  PutCommand,
  GetCommand,
  UpdateCommand,
  DeleteCommand,
} = require('@aws-sdk/lib-dynamodb');
const { DynamoDBClient } = require('@aws-sdk/client-dynamodb');

const {
  createGatewayLock,
  DEFAULT_TTL_SECONDS,
} = require('../src/gateway-lock');

// Synthesize a ConditionalCheckFailedException matching the AWS SDK
// shape so the lock module's `err.name === 'ConditionalCheckFailedException'`
// branches hit. Real DDB returns this; aws-sdk-client-mock's
// `.rejects(err)` carries the .name through faithfully.
function ccfe() {
  const err = new Error('The conditional request failed');
  err.name = 'ConditionalCheckFailedException';
  return err;
}

function makeLock({ clock, ttlSeconds, instanceId = 'inst-A', lockHolder = 'task-A/inst-A' } = {}) {
  const rawClient = new DynamoDBClient({});
  const docClient = DynamoDBDocumentClient.from(rawClient);
  const ddbMock = mockClient(docClient);
  const logger = {
    info: jest.fn(), warn: jest.fn(), error: jest.fn(), debug: jest.fn(),
  };
  const lock = createGatewayLock({
    ddbClient: docClient,
    tableName: 'test-gateway-lock',
    shardId: '0:1',
    instanceId,
    lockHolder,
    logger,
    clock,
    ttlSeconds,
  });
  return { lock, ddbMock, logger };
}

describe('createGatewayLock — factory validation', () => {
  it('throws when required args are missing', () => {
    expect(() => createGatewayLock()).toThrow(/ddbClient is required/);
    expect(() => createGatewayLock({ ddbClient: {} })).toThrow(/tableName is required/);
    expect(() => createGatewayLock({ ddbClient: {}, tableName: 't' }))
      .toThrow(/shardId is required/);
    expect(() => createGatewayLock({ ddbClient: {}, tableName: 't', shardId: '0:1' }))
      .toThrow(/instanceId is required/);
    expect(() => createGatewayLock({ ddbClient: {}, tableName: 't', shardId: '0:1', instanceId: 'i' }))
      .toThrow(/lockHolder is required/);
    expect(() => createGatewayLock({
      ddbClient: {}, tableName: 't', shardId: '0:1', instanceId: 'i', lockHolder: 'h',
    })).toThrow(/logger is required/);
  });
});

describe('acquireLock', () => {
  it('writes the row with PutItem (not UpdateItem) and the documented condition expression', async () => {
    // PutItem-vs-UpdateItem matters: the post-Delete row-absent case
    // is a single round-trip with PutItem; UpdateItem would need a
    // follow-up to write the full row. Contract 5.
    const { lock, ddbMock } = makeLock({ clock: () => 1_700_000_000_000 });
    ddbMock.on(PutCommand).resolves({});

    const result = await lock.acquireLock();

    expect(result).toEqual({ acquired: true, version: 1 });
    const putCalls = ddbMock.commandCalls(PutCommand);
    expect(putCalls).toHaveLength(1);
    expect(putCalls[0].args[0].input.ConditionExpression).toBe(
      'attribute_not_exists(lock_holder) ' +
      'OR attribute_not_exists(expires_at) ' +
      'OR expires_at < :now'
    );
    // No UpdateItem on the cold acquire path.
    expect(ddbMock.commandCalls(UpdateCommand)).toHaveLength(0);
  });

  it('writes expires_at as epoch SECONDS (not milliseconds)', async () => {
    // Contract 2 — DDB TTL only understands seconds-since-epoch. A
    // ms-encoded value would land ~50,000 years in the future to the
    // reaper and rows would live forever, breaking lock recovery.
    const { lock, ddbMock } = makeLock({ clock: () => 1_700_000_000_000, ttlSeconds: 6 });
    ddbMock.on(PutCommand).resolves({});

    await lock.acquireLock();

    const item = ddbMock.commandCalls(PutCommand)[0].args[0].input.Item;
    expect(item.expires_at).toBe(1_700_000_006); // ms→s + 6s TTL
    expect(item.expires_at).toBeLessThan(2_000_000_000); // sanity: not ms
  });

  it('returns acquired:false (not a throw) on ConditionalCheckFailedException', async () => {
    // Peer holds the lock — we observe a cond fail and treat it as
    // a soft "not yours yet" signal. Caller retries on the next
    // heartbeat cycle.
    const { lock, ddbMock, logger } = makeLock({ clock: () => 1_700_000_000_000 });
    ddbMock.on(PutCommand).rejects(ccfe());

    const result = await lock.acquireLock();

    expect(result).toEqual({ acquired: false });
    expect(logger.debug).toHaveBeenCalledWith(
      'gateway-lock: acquire failed (peer holds live lease)',
      expect.objectContaining({ shardId: '0:1' }),
    );
  });

  it('propagates non-CCFE errors (transport failure → caller decides retry)', async () => {
    // A throughput-exceeded or network blip is NOT "peer holds the
    // lock." We want it to surface so the leader loop logs and
    // retries on the next tick rather than silently thinking we
    // acquired.
    const { lock, ddbMock } = makeLock({ clock: () => 1_700_000_000_000 });
    const transportErr = new Error('ThroughputExceededException');
    transportErr.name = 'ThroughputExceededException';
    ddbMock.on(PutCommand).rejects(transportErr);

    await expect(lock.acquireLock()).rejects.toThrow(/ThroughputExceededException/);
  });

  it('treats expires_at === :now as still-live (strict < boundary)', async () => {
    // The cond uses strict `expires_at < :now`. A peer whose lease
    // expires at exactly `:now` should NOT be takeable yet — DDB
    // rejects the cond and we return acquired:false. Pins the
    // boundary against a refactor to `<=` that would shave 1s off
    // the steady-state cold-fallback floor at the cost of a clock-
    // skew race against the legitimate holder.
    const { lock, ddbMock } = makeLock({ clock: () => 1_700_000_000_000 });
    ddbMock.on(PutCommand).rejects(ccfe()); // simulate DDB rejecting because expires_at == :now

    const result = await lock.acquireLock();

    expect(result).toEqual({ acquired: false });
    // Pin that we're checking the right condition by inspecting the
    // request payload — the cond text must say `<`, not `<=`.
    const condText = ddbMock.commandCalls(PutCommand)[0].args[0].input.ConditionExpression;
    expect(condText).toContain('expires_at < :now');
    expect(condText).not.toContain('expires_at <= :now');
  });

  it('re-acquire while already holding is a soft no-op (DDB cond fails on live lease)', async () => {
    // A caller bug that double-calls acquireLock without a release
    // in between should NOT be treated as a renewal (which would
    // require a CAS on the existing instance_id/version). The cond
    // `expires_at < :now` fails against our own live lease, DDB
    // rejects, we return acquired:false. The local `currentVersion`
    // stays at the first acquire's value — important so the next
    // renewLock CAS still works.
    const { lock, ddbMock } = makeLock({ clock: () => 1_700_000_000_000 });
    ddbMock.on(PutCommand)
      .resolvesOnce({}) // first acquire wins
      .rejects(ccfe()); // second acquire hits cond fail

    const first = await lock.acquireLock();
    expect(first).toEqual({ acquired: true, version: 1 });

    const second = await lock.acquireLock();
    expect(second).toEqual({ acquired: false });
    // Cursor unchanged — the first acquire's version=1 is still valid.
    expect(lock._getVersionForTest()).toBe(1);
  });

  it('embeds expires_at < :now in the cond — :now is the caller wall clock at acquire time', async () => {
    // The lock primitive's correctness over a clock-skewed peer
    // depends on `:now` being self-evaluated (we trust our own clock
    // for the freshness check; the `version` CAS catches misuse). If
    // a refactor moved :now to a stored value, a stuck process
    // could observe an old `:now` and re-acquire wrongly.
    let nowMs = 1_700_000_000_000;
    const { lock, ddbMock } = makeLock({ clock: () => nowMs });
    ddbMock.on(PutCommand).resolves({});

    await lock.acquireLock();
    expect(ddbMock.commandCalls(PutCommand)[0].args[0].input.ExpressionAttributeValues[':now'])
      .toBe(1_700_000_000);

    nowMs = 1_700_000_010_000; // 10s later
    await lock.acquireLock();
    expect(ddbMock.commandCalls(PutCommand)[1].args[0].input.ExpressionAttributeValues[':now'])
      .toBe(1_700_000_010);
  });
});

describe('renewLock', () => {
  it('uses UpdateItem with the CAS guard on instance_id + version', async () => {
    // Contract 3 — version is the fencing token. Without `version =
    // :expected`, a stale process whose lease expired and was re-
    // acquired by a peer could accidentally succeed on a delayed
    // renew (clock skew + a peer that took the cond branch). The
    // CAS guard makes the renewer fail loud rather than silently
    // overwrite a peer's row.
    const { lock, ddbMock } = makeLock({ clock: () => 1_700_000_000_000 });
    ddbMock.on(PutCommand).resolves({});
    ddbMock.on(UpdateCommand).resolves({});

    await lock.acquireLock(); // version → 1
    const result = await lock.renewLock(); // version → 2

    expect(result).toEqual({ renewed: true, version: 2 });
    const updateCall = ddbMock.commandCalls(UpdateCommand)[0].args[0].input;
    expect(updateCall.ConditionExpression).toBe(
      'instance_id = :self AND version = :expected'
    );
    expect(updateCall.ExpressionAttributeValues[':self']).toBe('inst-A');
    expect(updateCall.ExpressionAttributeValues[':expected']).toBe(1);
    expect(updateCall.ExpressionAttributeValues[':next']).toBe(2);
    // renew updates version + expires_at only — lock_holder is
    // unchanged because we already hold the lock. Including it
    // would waste a WCU byte per renew. Pin against accidental
    // reintroduction.
    expect(updateCall.UpdateExpression).toBe('SET version = :next, expires_at = :exp');
    expect(updateCall.UpdateExpression).not.toMatch(/lock_holder/);
    expect(updateCall.ExpressionAttributeValues[':holder']).toBeUndefined();
  });

  it('returns renewed:false and clears version on CAS fail (lock lost)', async () => {
    // The peer took over (clock skew + we tarried, or we got network-
    // partitioned past the lease). The renew CAS fails; we treat this
    // as "you no longer hold the lock" so the leader-coordinator can
    // tear down its WS rather than keep trying to renew a lock it
    // doesn't own.
    const { lock, ddbMock, logger } = makeLock({ clock: () => 1_700_000_000_000 });
    ddbMock.on(PutCommand).resolves({});
    ddbMock.on(UpdateCommand).rejects(ccfe());

    await lock.acquireLock();
    const result = await lock.renewLock();

    expect(result).toEqual({ renewed: false });
    expect(lock._getVersionForTest()).toBe(null);
    expect(logger.warn).toHaveBeenCalledWith(
      'gateway-lock: renew CAS failed — lock lost',
      expect.objectContaining({ shardId: '0:1', expectedVersion: 1 }),
    );
  });

  it('returns renewed:false and warns if called before acquire', async () => {
    // Defensive: a caller bug that calls renew without acquire would
    // otherwise sit in an unreachable cond-fail branch. Surface it
    // as a warn log so it shows up in observability.
    const { lock, logger } = makeLock();
    const result = await lock.renewLock();

    expect(result).toEqual({ renewed: false });
    expect(logger.warn).toHaveBeenCalledWith(
      'gateway-lock: renew called without prior acquire',
      expect.objectContaining({ shardId: '0:1' }),
    );
  });

  it('writes expires_at as epoch seconds on every renewal', async () => {
    // Same TTL writer-shape contract as acquire — every write must
    // be seconds, not ms.
    let nowMs = 1_700_000_000_000;
    const { lock, ddbMock } = makeLock({ clock: () => nowMs, ttlSeconds: 6 });
    ddbMock.on(PutCommand).resolves({});
    ddbMock.on(UpdateCommand).resolves({});

    await lock.acquireLock();
    nowMs += 2000;
    await lock.renewLock();

    const renewExp = ddbMock.commandCalls(UpdateCommand)[0].args[0].input
      .ExpressionAttributeValues[':exp'];
    expect(renewExp).toBe(1_700_000_008); // (1_700_000_000_000 + 2000)ms→s + 6
  });

  it('uses the latest version as :expected across consecutive renews', async () => {
    // Pins the version cursor advancing. Without this, a refactor
    // that forgot to update `currentVersion` would cause the second
    // renew to use stale :expected=1 instead of :expected=2, and
    // the CAS would fail spuriously.
    const { lock, ddbMock } = makeLock({ clock: () => 1_700_000_000_000 });
    ddbMock.on(PutCommand).resolves({});
    ddbMock.on(UpdateCommand).resolves({});

    await lock.acquireLock(); // v1
    await lock.renewLock(); // v2
    await lock.renewLock(); // v3

    expect(ddbMock.commandCalls(UpdateCommand)[1].args[0].input
      .ExpressionAttributeValues[':expected']).toBe(2);
    expect(ddbMock.commandCalls(UpdateCommand)[1].args[0].input
      .ExpressionAttributeValues[':next']).toBe(3);
  });
});

describe('transferLock', () => {
  it('atomically rewrites instance_id + lock_holder + version in one UpdateItem', async () => {
    // The whole point of transferLock vs release-then-acquire is no
    // lock-released-but-not-acquired-yet window. One atomic
    // UpdateItem with the CAS guard on `self` instance_id swaps
    // ownership cleanly.
    const { lock, ddbMock } = makeLock({ clock: () => 1_700_000_000_000 });
    ddbMock.on(PutCommand).resolves({});
    ddbMock.on(UpdateCommand).resolves({});

    await lock.acquireLock();
    const result = await lock.transferLock('inst-B', 'task-B/inst-B');

    expect(result).toEqual({ transferred: true, version: 2 });
    const updateInput = ddbMock.commandCalls(UpdateCommand)[0].args[0].input;
    expect(updateInput.ConditionExpression).toBe(
      'instance_id = :self AND version = :expected'
    );
    expect(updateInput.ExpressionAttributeValues[':self']).toBe('inst-A');
    expect(updateInput.ExpressionAttributeValues[':peer']).toBe('inst-B');
    expect(updateInput.ExpressionAttributeValues[':peerHolder']).toBe('task-B/inst-B');
    expect(updateInput.ExpressionAttributeValues[':next']).toBe(2);
  });

  it('clears the local version cursor on success (this process no longer holds the lock)', async () => {
    // After transferLock, this process must stop heartbeating and
    // tear down the WS. The version-null state is the local signal:
    // subsequent renew/release calls become no-ops, and a future
    // re-acquire would correctly start from version=1.
    const { lock, ddbMock } = makeLock({ clock: () => 1_700_000_000_000 });
    ddbMock.on(PutCommand).resolves({});
    ddbMock.on(UpdateCommand).resolves({});

    await lock.acquireLock();
    await lock.transferLock('inst-B', 'task-B/inst-B');

    expect(lock._getVersionForTest()).toBe(null);
  });

  it('returns transferred:false on CAS fail (caller falls through to clean exit)', async () => {
    // Edge case the design doc names explicitly: version moved
    // underneath us (peer crashed and the replacement acquired via
    // the lease-lapse path) OR we don't actually hold the lock. The
    // active falls through to a clean exit; the new peer reaches
    // steady state via the ~7 s cold-fallback floor.
    const { lock, ddbMock, logger } = makeLock({ clock: () => 1_700_000_000_000 });
    ddbMock.on(PutCommand).resolves({});
    ddbMock.on(UpdateCommand).rejects(ccfe());

    await lock.acquireLock();
    const result = await lock.transferLock('inst-B', 'task-B/inst-B');

    expect(result).toEqual({ transferred: false });
    expect(logger.warn).toHaveBeenCalledWith(
      'gateway-lock: transfer CAS failed',
      expect.objectContaining({ shardId: '0:1', expectedVersion: 1 }),
    );
  });

  it('returns transferred:false and warns if called before acquire', async () => {
    const { lock, logger } = makeLock();
    const result = await lock.transferLock('inst-B', 'task-B/inst-B');

    expect(result).toEqual({ transferred: false });
    expect(logger.warn).toHaveBeenCalledWith(
      'gateway-lock: transfer called without prior acquire',
      expect.anything(),
    );
  });

  it('rejects self-handoff (target === self) as no-op with warn', async () => {
    // A caller bug — e.g., peer-discovery accidentally returns our
    // own row — that called transferLock(self → self) would otherwise
    // bump the version counter while keeping ownership, churning
    // DDB for nothing. Reject at the API boundary so the failure
    // surfaces as a warn log rather than silent state churn.
    const { lock, ddbMock, logger } = makeLock({ clock: () => 1_700_000_000_000 });
    ddbMock.on(PutCommand).resolves({});
    ddbMock.on(UpdateCommand).resolves({});

    await lock.acquireLock();
    const result = await lock.transferLock('inst-A', 'task-A/inst-A'); // same as constructor instanceId

    expect(result).toEqual({ transferred: false });
    expect(ddbMock.commandCalls(UpdateCommand)).toHaveLength(0); // no DDB write
    expect(lock._getVersionForTest()).toBe(1); // cursor unchanged
    expect(logger.warn).toHaveBeenCalledWith(
      'gateway-lock: transferLock called with self as target (no-op)',
      expect.anything(),
    );
  });
});

describe('adoptLockFromHandoff', () => {
  it('seeds currentVersion so the new holder\'s next renewLock CAS passes', async () => {
    // The load-bearing case: PR 13b.2's control-channel handler
    // receives an HMAC-verified push, the active calls transferLock
    // (DDB row now shows this replica as holder, version=v), and
    // this call synchronizes the local cursor with that DDB state.
    // Without it, the first renewLock hits the currentVersion===null
    // guard and returns renewed:false — the standby would think it
    // lost the lock it just received.
    const { lock, ddbMock, logger } = makeLock({ clock: () => 1_700_000_000_000 });
    ddbMock.on(UpdateCommand).resolves({});

    lock.adoptLockFromHandoff(7); // active's prior version was 6, transferLock bumped to 7

    expect(lock._getVersionForTest()).toBe(7);
    expect(logger.info).toHaveBeenCalledWith(
      'gateway-lock: adopted from handoff',
      expect.objectContaining({ shardId: '0:1', instanceId: 'inst-A', version: 7 }),
    );

    // The next renewLock uses the adopted version as :expected.
    await lock.renewLock();
    const updateCall = ddbMock.commandCalls(UpdateCommand)[0].args[0].input;
    expect(updateCall.ExpressionAttributeValues[':expected']).toBe(7);
    expect(updateCall.ExpressionAttributeValues[':next']).toBe(8);
  });

  it('does NOT write to DDB (caller is responsible for the prior transferLock)', async () => {
    // Pure local-state-sync. The active's transferLock already
    // wrote DDB; this call is just the new holder catching up its
    // in-memory cursor.
    const { lock, ddbMock } = makeLock();
    lock.adoptLockFromHandoff(5);

    expect(ddbMock.commandCalls(PutCommand)).toHaveLength(0);
    expect(ddbMock.commandCalls(UpdateCommand)).toHaveLength(0);
  });

  it('throws on non-positive-integer version (defends against null/undefined/0/string from a malformed HMAC body)', () => {
    // The HMAC body upstream carries the version as JSON-decoded number.
    // A buggy upstream (or a forged body that passed the HMAC check
    // somehow) could pass garbage. Fail loud so the standby's handler
    // bubbles a clear error to the active rather than silently
    // adopting an invalid cursor.
    const { lock } = makeLock();
    expect(() => lock.adoptLockFromHandoff(null)).toThrow(/positive integer version/);
    expect(() => lock.adoptLockFromHandoff(undefined)).toThrow(/positive integer version/);
    expect(() => lock.adoptLockFromHandoff(0)).toThrow(/positive integer version/);
    expect(() => lock.adoptLockFromHandoff(-1)).toThrow(/positive integer version/);
    expect(() => lock.adoptLockFromHandoff(1.5)).toThrow(/positive integer version/);
    expect(() => lock.adoptLockFromHandoff('5')).toThrow(/positive integer version/);
  });

  it('is safe to call multiple times — cursor just re-anchors', async () => {
    // Idempotency lets the control-channel handler retry-on-error
    // without worrying about double-anchor side effects.
    const { lock } = makeLock();
    lock.adoptLockFromHandoff(5);
    lock.adoptLockFromHandoff(5);
    lock.adoptLockFromHandoff(7);
    expect(lock._getVersionForTest()).toBe(7);
  });
});

describe('releaseLock', () => {
  it('issues DeleteItem (not UpdateItem REMOVE) so the next acquire takes attribute_not_exists', async () => {
    // Contract 4 — DeleteItem re-arms the attribute_not_exists branch
    // on the cond expression, so a peer's next acquire is immediate.
    // UpdateItem REMOVE would leave expires_at populated, forcing
    // the peer to wait for it to lapse.
    const { lock, ddbMock } = makeLock({ clock: () => 1_700_000_000_000 });
    ddbMock.on(PutCommand).resolves({});
    ddbMock.on(DeleteCommand).resolves({});

    await lock.acquireLock();
    const result = await lock.releaseLock();

    expect(result).toEqual({ released: true });
    const deleteInput = ddbMock.commandCalls(DeleteCommand)[0].args[0].input;
    expect(deleteInput.Key).toEqual({ shard_id: '0:1' });
    expect(deleteInput.ConditionExpression).toBe('instance_id = :self');
    expect(deleteInput.ExpressionAttributeValues[':self']).toBe('inst-A');
    // No REMOVE-style UpdateItem.
    expect(ddbMock.commandCalls(UpdateCommand)).toHaveLength(0);
  });

  it('treats CAS fail as released:false (a peer took over while we were tearing down)', async () => {
    // The CAS guard on instance_id prevents us from deleting a row
    // a peer now owns. If this fired during a busy handoff window
    // (transfer succeeded; the peer then re-acquired with a new
    // version), our release CAS would fail and we want a soft
    // released:false rather than an error — the lock is gone from
    // our perspective, which is what we wanted.
    const { lock, ddbMock, logger } = makeLock({ clock: () => 1_700_000_000_000 });
    ddbMock.on(PutCommand).resolves({});
    ddbMock.on(DeleteCommand).rejects(ccfe());

    await lock.acquireLock();
    const result = await lock.releaseLock();

    expect(result).toEqual({ released: false });
    expect(lock._getVersionForTest()).toBe(null);
    expect(logger.warn).toHaveBeenCalledWith(
      'gateway-lock: release CAS failed (peer took over)',
      expect.anything(),
    );
  });

  it('best-effort on transport errors (logs error, returns released:false, does not throw)', async () => {
    // Release is on the SIGTERM teardown path. We must not throw —
    // the process is exiting and a throw would mask the cleaner
    // released:false signal. The fallback is the lease lapse, which
    // is the design's documented worst-case path.
    const { lock, ddbMock, logger } = makeLock({ clock: () => 1_700_000_000_000 });
    ddbMock.on(PutCommand).resolves({});
    const transportErr = new Error('NetworkingError');
    transportErr.name = 'NetworkingError';
    ddbMock.on(DeleteCommand).rejects(transportErr);

    await lock.acquireLock();
    const result = await lock.releaseLock();

    expect(result).toEqual({ released: false });
    expect(lock._getVersionForTest()).toBe(null);
    expect(logger.error).toHaveBeenCalledWith(
      'gateway-lock: release failed',
      expect.objectContaining({ error: 'NetworkingError' }),
    );
  });

  it('is a no-op when called before acquire', async () => {
    // The control-channel watchdog may call release on a path where
    // acquire never succeeded. We should NOT attempt a DDB delete
    // (it would do nothing anyway, but spending the call costs PPR
    // read+write).
    const { lock, ddbMock } = makeLock();
    const result = await lock.releaseLock();

    expect(result).toEqual({ released: false });
    expect(ddbMock.commandCalls(DeleteCommand)).toHaveLength(0);
  });
});

describe('readCurrentHolder', () => {
  it('returns the row for diagnostic / health reads', async () => {
    // Used by /health endpoint output and debug logs. Not in the
    // lock-correctness path.
    const { lock, ddbMock } = makeLock();
    ddbMock.on(GetCommand).resolves({
      Item: {
        shard_id: '0:1', lock_holder: 'task-A/inst-A', instance_id: 'inst-A',
        version: 3, expires_at: 1_700_000_006,
      },
    });

    const result = await lock.readCurrentHolder();
    expect(result.lock_holder).toBe('task-A/inst-A');
    expect(result.version).toBe(3);
  });

  it('returns null when the row is absent', async () => {
    const { lock, ddbMock } = makeLock();
    ddbMock.on(GetCommand).resolves({ Item: undefined });

    expect(await lock.readCurrentHolder()).toBeNull();
  });
});

describe('default TTL', () => {
  it('exports the documented 6 second lease', () => {
    // Pinning this constant matters: the cold-fallback floor math
    // (~6 s + RESUME RTT ≈ 7 s) lives in the design doc. A silent
    // bump to 15 s would push the floor out and invalidate the
    // SLO arithmetic.
    expect(DEFAULT_TTL_SECONDS).toBe(6);
  });
});
