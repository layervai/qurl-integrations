
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
    expect(ddbMock.commandCalls(UpdateCommand)).toHaveLength(0);
  });

  it('writes expires_at as epoch SECONDS (not milliseconds)', async () => {
    const { lock, ddbMock } = makeLock({ clock: () => 1_700_000_000_000, ttlSeconds: 6 });
    ddbMock.on(PutCommand).resolves({});

    await lock.acquireLock();

    const item = ddbMock.commandCalls(PutCommand)[0].args[0].input.Item;
    expect(item.expires_at).toBe(1_700_000_006); // ms→s + 6s TTL
    expect(item.expires_at).toBeLessThan(2_000_000_000); // sanity: not ms
  });

  it('returns acquired:false (not a throw) on ConditionalCheckFailedException', async () => {
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
    const { lock, ddbMock } = makeLock({ clock: () => 1_700_000_000_000 });
    const transportErr = new Error('ThroughputExceededException');
    transportErr.name = 'ThroughputExceededException';
    ddbMock.on(PutCommand).rejects(transportErr);

    await expect(lock.acquireLock()).rejects.toThrow(/ThroughputExceededException/);
  });

  it('treats expires_at === :now as still-live (strict < boundary)', async () => {
    const { lock, ddbMock } = makeLock({ clock: () => 1_700_000_000_000 });
    ddbMock.on(PutCommand).rejects(ccfe()); // simulate DDB rejecting because expires_at == :now

    const result = await lock.acquireLock();

    expect(result).toEqual({ acquired: false });
    const condText = ddbMock.commandCalls(PutCommand)[0].args[0].input.ConditionExpression;
    expect(condText).toContain('expires_at < :now');
    expect(condText).not.toContain('expires_at <= :now');
  });

  it('re-acquire while already holding is a soft no-op (DDB cond fails on live lease)', async () => {
    const { lock, ddbMock } = makeLock({ clock: () => 1_700_000_000_000 });
    ddbMock.on(PutCommand)
      .resolvesOnce({}) // first acquire wins
      .rejects(ccfe()); // second acquire hits cond fail

    const first = await lock.acquireLock();
    expect(first).toEqual({ acquired: true, version: 1 });

    const second = await lock.acquireLock();
    expect(second).toEqual({ acquired: false });
    expect(lock._getVersionForTest()).toBe(1);
  });

  it('embeds expires_at < :now in the cond — :now is the caller wall clock at acquire time', async () => {
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
    expect(updateCall.UpdateExpression).toBe('SET version = :next, expires_at = :exp');
    expect(updateCall.UpdateExpression).not.toMatch(/lock_holder/);
    expect(updateCall.ExpressionAttributeValues[':holder']).toBeUndefined();
  });

  it('returns renewed:false and clears version on CAS fail (lock lost)', async () => {
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
    const { lock, logger } = makeLock();
    const result = await lock.renewLock();

    expect(result).toEqual({ renewed: false });
    expect(logger.warn).toHaveBeenCalledWith(
      'gateway-lock: renew called without prior acquire',
      expect.objectContaining({ shardId: '0:1' }),
    );
  });

  it('writes expires_at as epoch seconds on every renewal', async () => {
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
    const { lock, ddbMock } = makeLock({ clock: () => 1_700_000_000_000 });
    ddbMock.on(PutCommand).resolves({});
    ddbMock.on(UpdateCommand).resolves({});

    await lock.acquireLock();
    await lock.transferLock('inst-B', 'task-B/inst-B');

    expect(lock._getVersionForTest()).toBe(null);
  });

  it('returns transferred:false on CAS fail (caller falls through to clean exit)', async () => {
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
    const { lock, ddbMock, logger } = makeLock({ clock: () => 1_700_000_000_000 });
    ddbMock.on(UpdateCommand).resolves({});

    lock.adoptLockFromHandoff(7); // active's prior version was 6, transferLock bumped to 7

    expect(lock._getVersionForTest()).toBe(7);
    expect(logger.info).toHaveBeenCalledWith(
      'gateway-lock: adopted from handoff',
      expect.objectContaining({ shardId: '0:1', instanceId: 'inst-A', version: 7 }),
    );

    await lock.renewLock();
    const updateCall = ddbMock.commandCalls(UpdateCommand)[0].args[0].input;
    expect(updateCall.ExpressionAttributeValues[':expected']).toBe(7);
    expect(updateCall.ExpressionAttributeValues[':next']).toBe(8);
  });

  it('does NOT write to DDB (caller is responsible for the prior transferLock)', async () => {
    const { lock, ddbMock } = makeLock();
    lock.adoptLockFromHandoff(5);

    expect(ddbMock.commandCalls(PutCommand)).toHaveLength(0);
    expect(ddbMock.commandCalls(UpdateCommand)).toHaveLength(0);
  });

  it('throws on non-positive-integer version (defends against null/undefined/0/string from a malformed HMAC body)', () => {
    const { lock } = makeLock();
    expect(() => lock.adoptLockFromHandoff(null)).toThrow(/positive integer version/);
    expect(() => lock.adoptLockFromHandoff(undefined)).toThrow(/positive integer version/);
    expect(() => lock.adoptLockFromHandoff(0)).toThrow(/positive integer version/);
    expect(() => lock.adoptLockFromHandoff(-1)).toThrow(/positive integer version/);
    expect(() => lock.adoptLockFromHandoff(1.5)).toThrow(/positive integer version/);
    expect(() => lock.adoptLockFromHandoff('5')).toThrow(/positive integer version/);
  });

  it('is safe to call multiple times — cursor just re-anchors', async () => {
    const { lock } = makeLock();
    lock.adoptLockFromHandoff(5);
    lock.adoptLockFromHandoff(5);
    lock.adoptLockFromHandoff(7);
    expect(lock._getVersionForTest()).toBe(7);
  });
});

describe('releaseLock', () => {
  it('issues DeleteItem (not UpdateItem REMOVE) so the next acquire takes attribute_not_exists', async () => {
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
    expect(ddbMock.commandCalls(UpdateCommand)).toHaveLength(0);
  });

  it('treats CAS fail as released:false (a peer took over while we were tearing down)', async () => {
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
    const { lock, ddbMock } = makeLock();
    const result = await lock.releaseLock();

    expect(result).toEqual({ released: false });
    expect(ddbMock.commandCalls(DeleteCommand)).toHaveLength(0);
  });
});

describe('readCurrentHolder', () => {
  it('exposes the immutable instance identity used in holder rows', () => {
    const { lock } = makeLock();
    expect(lock.instanceId).toBe('inst-A');
  });

  it('returns the row for diagnostic / health reads', async () => {
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
    expect(ddbMock.commandCalls(GetCommand)[0].args[0].input).toEqual(
      expect.objectContaining({ ConsistentRead: true }),
    );
  });

  it('returns null when the row is absent', async () => {
    const { lock, ddbMock } = makeLock();
    ddbMock.on(GetCommand).resolves({ Item: undefined });

    expect(await lock.readCurrentHolder()).toBeNull();
  });
});

describe('default TTL', () => {
  it('exports the documented 6 second lease', () => {
    expect(DEFAULT_TTL_SECONDS).toBe(6);
  });
});
