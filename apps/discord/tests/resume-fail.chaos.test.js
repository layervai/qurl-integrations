
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
    lock.adoptLockFromHandoff(3);

    const heartbeat = createPeerHeartbeat({
      ddbClient: docClient, tableName: HEARTBEAT_TABLE,
      instanceId: INSTANCE_A, ip: '10.0.0.10', port: 7800,
      shardId: SHARD_ID, lockHolder: HOLDER_A, logger, clock,
    });

    const manager = {
      isConnected: jest.fn(() => false),
      isRecovering: jest.fn(() => false),
      connect: jest.fn().mockRejectedValue(new Error('econnrefused')),
    };

    const exit = jest.fn();

    const watchdog = createConnectionWatchdog({
      manager,
      isHoldingLock: () => true,
      isConnecting: () => false,
      readCurrentHolder: () => lock.readCurrentHolder(),
      selfInstanceId: INSTANCE_A,
      releaseLock: () => lock.releaseLock(),
      deleteOwnRow: () => heartbeat.deleteOwnRow(),
      logger,
      maxAttempts: 5,
      sleep: jest.fn(async () => {}),
      exit,
    });

    for (let i = 0; i < 5; i += 1) {
      // eslint-disable-next-line no-await-in-loop
      await watchdog._stepForTest();
    }

    expect(state.lockRow).toBeNull();
    const lockDeletes = ddbMock.commandCalls(DeleteCommand, { TableName: LOCK_TABLE });
    expect(lockDeletes).toHaveLength(1);
    expect(lockDeletes[0].args[0].input.ConditionExpression).toContain('instance_id = :self');

    const remainingHeartbeats = state.peerRows.map((r) => r.instance_id);
    expect(remainingHeartbeats).not.toContain(INSTANCE_A);
    expect(remainingHeartbeats).toContain(INSTANCE_B); // sanity: didn't nuke the wrong row.

    expect(exit).toHaveBeenCalledWith(1);
    expect(exit).toHaveBeenCalledTimes(1);

    expect(manager.connect).toHaveBeenCalledTimes(5);

    assertNoUnexpectedTableCalls(ddbMock);
  });

  it('lock-table DeleteCommand mocked to throw CCFE → exit(1) still fires (defensive)', async () => {
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
    ddbMock.on(DeleteCommand, { TableName: LOCK_TABLE }).callsFake(() => { throw makeCcfe(); });

    const heartbeat = createPeerHeartbeat({
      ddbClient: docClient, tableName: HEARTBEAT_TABLE,
      instanceId: INSTANCE_A, ip: '10.0.0.10', port: 7800,
      shardId: SHARD_ID, lockHolder: HOLDER_A, logger, clock,
    });

    const manager = {
      isConnected: jest.fn(() => false),
      isRecovering: jest.fn(() => false),
      connect: jest.fn().mockRejectedValue(new Error('econnrefused')),
    };
    const exit = jest.fn();

    const watchdog = createConnectionWatchdog({
      manager,
      isHoldingLock: () => true,
      isConnecting: () => false,
      readCurrentHolder: () => lock.readCurrentHolder(),
      selfInstanceId: INSTANCE_A,
      releaseLock: () => lock.releaseLock(),
      deleteOwnRow: () => heartbeat.deleteOwnRow(),
      logger, maxAttempts: 3,
      sleep: jest.fn(async () => {}),
      exit,
    });

    for (let i = 0; i < 3; i += 1) {
      // eslint-disable-next-line no-await-in-loop
      await watchdog._stepForTest();
    }

    expect(state.lockRow).not.toBeNull();
    expect(state.lockRow.instance_id).toBe(INSTANCE_A);
    const remainingHeartbeats = state.peerRows.map((r) => r.instance_id);
    expect(remainingHeartbeats).not.toContain(INSTANCE_A);
    expect(exit).toHaveBeenCalledWith(1);
    expect(logger.warn).toHaveBeenCalledWith(
      expect.stringContaining('release CAS failed'),
      expect.any(Object),
    );

    assertNoUnexpectedTableCalls(ddbMock);
  });
});
