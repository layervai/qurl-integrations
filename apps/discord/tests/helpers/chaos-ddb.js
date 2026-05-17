// Shared DDB scaffolding for Pillar 3 chaos tests
// (deploy-during-flow.chaos.test.js, resume-fail.chaos.test.js).
//
// Each chaos test composes the REAL `createGatewayLock` +
// `createPeerHeartbeat` primitives against a `mockClient(docClient)`
// and exercises a SIGTERM-class scenario. The setup boilerplate
// (table-routed mock handlers + in-memory row state so subsequent
// reads see prior writes) is identical between files; this helper
// owns it.
//
// Why an in-memory `state` map vs static `.resolves({})`: the
// transferLock → readCurrentHolder sequence inside `pushHandoff`
// expects the row's `instance_id` to reflect the transferred owner.
// Static resolves would always return the seeded row, masking
// transfer regressions.
//
// Scope limit: only the lock + heartbeat tables are wired. The
// flow-state table is intentionally UN-routed so any write to it
// surfaces as an uncaught throw, catching the gateway-tier-touches-
// flow-state regression on the spot. Callers running tests that
// legitimately need additional tables should extend the mock at
// call-site, not in this helper.

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

const LOCK_TABLE = 'test-gateway-lock';
const HEARTBEAT_TABLE = 'test-gateway-peer-heartbeat';

// Stable identifiers shared by the chaos tests. HOLDER_A/B are
// opaque (lock/heartbeat treat lock_holder as unparsed debug metadata).
const SHARD_ID = '0:1';
const INSTANCE_A = 'inst-A';
const INSTANCE_B = 'inst-B';
const HOLDER_A = 'task-arn:.../inst-A';
const HOLDER_B = 'task-arn:.../inst-B';

function makeChaosLogger() {
  // Mirrors src/logger.js's exported methods so a future chaos
  // composition that invokes logger.audit doesn't throw on an
  // undefined method.
  return {
    info: jest.fn(), warn: jest.fn(), error: jest.fn(),
    debug: jest.fn(), audit: jest.fn(),
  };
}

function makeCcfe() {
  const err = new Error('conditional check failed');
  err.name = 'ConditionalCheckFailedException';
  return err;
}

// Build a docClient + mockClient + mutable in-memory `state` map
// representing the lock + heartbeat tables. Seeds the state with
// the initial rows from the caller and wires Put/Get/Update/Delete/
// Scan handlers that read and write through `state` so a CAS-by-
// version pattern is visible to subsequent reads.
function setupChaosDdb({ initialLockRow = null, initialPeerRows = [] } = {}) {
  const rawClient = new DynamoDBClient({});
  const docClient = DynamoDBDocumentClient.from(rawClient);
  const ddbMock = mockClient(docClient);

  const state = {
    lockRow: initialLockRow ? { ...initialLockRow } : null,
    peerRows: initialPeerRows.map((r) => ({ ...r })),
  };

  // CAS guard for the lock row. Mirrors what production DDB enforces
  // on gateway-lock's `instance_id = :self [AND version = :expected]`
  // condition shape (renew/transfer use both; release uses :self only).
  // Without these checks a regression that ships a corrupted :expected
  // or :self would silently write through the mock when production
  // would reject — defeating the whole point of composing real
  // primitives. `requireSelf` is on for Update/Delete (a stripped-CAS
  // regression should fail here too); `checkVersion` is on for Update
  // only since Delete (releaseLock) doesn't guard on version.
  function assertLockCas(cmd, { requireSelf = true, checkVersion = false } = {}) {
    if (!state.lockRow) throw makeCcfe();
    const v = cmd.ExpressionAttributeValues || {};
    if (requireSelf && v[':self'] === undefined) throw makeCcfe();
    if (v[':self'] !== undefined && v[':self'] !== state.lockRow.instance_id) {
      throw makeCcfe();
    }
    if (checkVersion && v[':expected'] !== undefined && v[':expected'] !== state.lockRow.version) {
      throw makeCcfe();
    }
  }

  // ── Lock table ──
  ddbMock.on(PutCommand, { TableName: LOCK_TABLE }).callsFake((cmd) => {
    state.lockRow = { ...cmd.Item };
    return {};
  });
  ddbMock.on(UpdateCommand, { TableName: LOCK_TABLE }).callsFake((cmd) => {
    assertLockCas(cmd, { requireSelf: true, checkVersion: true });
    // Renew writes version + expires_at; transfer also flips
    // instance_id + lock_holder. Apply whichever fields the caller's
    // ExpressionAttributeValues include — matches gateway-lock.js's
    // SET expression without re-implementing the parser.
    const v = cmd.ExpressionAttributeValues || {};
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

  // ── Heartbeat table ──
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

module.exports = {
  setupChaosDdb,
  makeChaosLogger,
  LOCK_TABLE,
  HEARTBEAT_TABLE,
  SHARD_ID,
  INSTANCE_A,
  INSTANCE_B,
  HOLDER_A,
  HOLDER_B,
  makeCcfe,
};
