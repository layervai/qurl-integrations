
const { mockClient } = require('aws-sdk-client-mock');
const {
  DynamoDBDocumentClient,
  PutCommand,
  GetCommand,
  UpdateCommand,
  DeleteCommand,
  ScanCommand,
} = require('@aws-sdk/lib-dynamodb');
const { DynamoDBClient } = require('@aws-sdk/client-dynamodb');
const { __TABLE_NAME: FLOW_STATE_TABLE_NAME } = require('../../src/flow-state');

const MAX_MICROTASK_YIELDS = 50;

const LOCK_TABLE = 'test-gateway-lock';
const HEARTBEAT_TABLE = 'test-gateway-peer-heartbeat';

const SHARD_ID = '0:1';
const INSTANCE_A = 'inst-A';
const INSTANCE_B = 'inst-B';
const HOLDER_A = 'task-arn:.../inst-A';
const HOLDER_B = 'task-arn:.../inst-B';

function makeChaosLogger() {
  return {
    info: jest.fn(), warn: jest.fn(), error: jest.fn(),
    debug: jest.fn(), audit: jest.fn(),
  };
}

function makeCcfe() {
  const err = new Error('conditional check failed');
  err.name = 'ConditionalCheckFailedException';
  err.$metadata = { httpStatusCode: 400 };
  return err;
}

function setupChaosDdb({ initialLockRow = null, initialPeerRows = [] } = {}) {
  const rawClient = new DynamoDBClient({});
  const docClient = DynamoDBDocumentClient.from(rawClient);
  const ddbMock = mockClient(docClient);

  const state = {
    lockRow: initialLockRow ? { ...initialLockRow } : null,
    peerRows: initialPeerRows.map((r) => ({ ...r })),
  };

  function assertLockCas(cmd, { requireSelf = true, checkVersion = false } = {}) {
    if (!state.lockRow) throw makeCcfe();
    if (requireSelf && !(cmd.ConditionExpression && cmd.ConditionExpression.includes('instance_id = :self'))) {
      throw makeCcfe();
    }
    const v = cmd.ExpressionAttributeValues || {};
    if (requireSelf && v[':self'] === undefined) throw makeCcfe();
    if (v[':self'] !== undefined && v[':self'] !== state.lockRow.instance_id) {
      throw makeCcfe();
    }
    if (checkVersion && v[':expected'] !== undefined && v[':expected'] !== state.lockRow.version) {
      throw makeCcfe();
    }
  }

  ddbMock.on(PutCommand, { TableName: LOCK_TABLE }).callsFake((cmd) => {
    state.lockRow = { ...cmd.Item };
    return {};
  });
  const KNOWN_UPDATE_KEYS = new Set([
    ':self', ':expected',                        // CAS guards
    ':next', ':exp',                             // renew
    ':peer', ':peerHolder',                      // transfer
  ]);
  ddbMock.on(UpdateCommand, { TableName: LOCK_TABLE }).callsFake((cmd) => {
    assertLockCas(cmd, { requireSelf: true, checkVersion: true });
    const v = cmd.ExpressionAttributeValues || {};
    for (const key of Object.keys(v)) {
      if (!KNOWN_UPDATE_KEYS.has(key)) {
        throw new Error(
          `chaos-ddb: UpdateCommand on ${LOCK_TABLE} carried unknown ExpressionAttributeValues key "${key}". ` +
          `If gateway-lock.js added or renamed an expression key, update KNOWN_UPDATE_KEYS + the SET-apply switch below.`
        );
      }
    }
    if (v[':peer'] !== undefined) {
      state.lockRow.instance_id = v[':peer'];
      state.lockRow.lock_holder = v[':peerHolder'];
    }
    if (v[':next'] !== undefined) state.lockRow.version = v[':next'];
    if (v[':exp'] !== undefined) state.lockRow.expires_at = v[':exp'];
    return {};
  });
  ddbMock.on(DeleteCommand, { TableName: LOCK_TABLE }).callsFake((cmd) => {
    assertLockCas(cmd, { requireSelf: true, checkVersion: false });
    state.lockRow = null;
    return {};
  });
  ddbMock.on(GetCommand, { TableName: LOCK_TABLE }).callsFake(() => ({
    Item: state.lockRow ?? undefined,
  }));

  ddbMock.on(PutCommand, { TableName: HEARTBEAT_TABLE }).callsFake((cmd) => {
    const idx = state.peerRows.findIndex((r) => r.instance_id === cmd.Item.instance_id);
    if (idx >= 0) state.peerRows[idx] = { ...cmd.Item };
    else state.peerRows.push({ ...cmd.Item });
    return {};
  });
  ddbMock.on(ScanCommand, { TableName: HEARTBEAT_TABLE }).callsFake(() => ({
    Items: state.peerRows.slice(),
  }));
  ddbMock.on(DeleteCommand, { TableName: HEARTBEAT_TABLE }).callsFake((cmd) => {
    state.peerRows = state.peerRows.filter((r) => r.instance_id !== cmd.Key.instance_id);
    return {};
  });

  return { docClient, ddbMock, state };
}

function tableNamesTargeted(cmdInput) {
  if (!cmdInput) return [];
  if (cmdInput.TableName) return [cmdInput.TableName];
  if (cmdInput.RequestItems) return Object.keys(cmdInput.RequestItems);
  if (Array.isArray(cmdInput.TransactItems)) {
    return cmdInput.TransactItems
      .map((entry) => {
        const op = entry.Put || entry.Update || entry.Delete || entry.ConditionCheck || entry.Get;
        return op?.TableName;
      })
      .filter(Boolean);
  }
  return [];
}

function assertNoUnexpectedTableCalls(ddbMock) {
  const allowed = new Set([LOCK_TABLE, HEARTBEAT_TABLE]);
  const allTables = ddbMock.calls()
    .flatMap((c) => tableNamesTargeted(c.args[0]?.input));
  const offenders = allTables.filter((t) => !allowed.has(t));
  if (offenders.length > 0) {
    throw new Error(
      `chaos: gateway-tier path wrote to forbidden tables: ${[...new Set(offenders)].join(', ')}. ` +
      `Allowed: ${[...allowed].join(', ')}. ` +
      `If this test starts failing because the gateway tier legitimately needs ` +
      `another table, add it here AND update the relevant source module's header ` +
      `to document the new write surface.`
    );
  }
  expect(allTables.filter((t) => t === FLOW_STATE_TABLE_NAME)).toHaveLength(0);
}

module.exports = {
  setupChaosDdb,
  makeChaosLogger,
  LOCK_TABLE,
  HEARTBEAT_TABLE,
  FLOW_STATE_TABLE_NAME,
  SHARD_ID,
  INSTANCE_A,
  INSTANCE_B,
  HOLDER_A,
  HOLDER_B,
  MAX_MICROTASK_YIELDS,
  makeCcfe,
  tableNamesTargeted,
  assertNoUnexpectedTableCalls,
};
