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
const { __TABLE_NAME: FLOW_STATE_TABLE_NAME } = require('../../src/flow-state');

// Bound for microtask-yield polling loops (`while (!condition) await
// setImmediate(...)`). Used by chaos tests that wait for an
// observable DDB state mutation to land. Set generously above the
// observed need (≤ 3 hops with mocked DDB); the diagnostic throw on
// overflow turns a future flake into a clear failure message.
const MAX_MICROTASK_YIELDS = 50;

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
  // Match the real AWS error shape so any future branch on
  // err.$metadata.httpStatusCode (e.g. distinguishing 4xx vs 5xx)
  // behaves the same against the mock as against prod DDB.
  err.$metadata = { httpStatusCode: 400 };
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
  // The known set of `:`-keys this mock recognizes. If gateway-lock's
  // SET expression ever renames one (e.g. :peer → :newOwner), the
  // mock would silently no-op the state mutation and downstream
  // assertions would fail with "the row never flipped" instead of a
  // clear "your rename broke the mock contract" error. Throw on any
  // unknown key to surface drift at the point of breakage.
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
    // Renew writes version + expires_at; transfer also flips
    // instance_id + lock_holder.
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

// Pull every TableName an SDK command targets, including the nested
// shapes (BatchGet/BatchWrite use `RequestItems`; TransactGet/
// TransactWrite use `TransactItems`). Single-item shapes (Put/Get/
// Update/Delete/Scan/Query) carry `input.TableName` directly. Returns
// a flat string[] so callers can filter against an allowlist.
function tableNamesTargeted(cmdInput) {
  if (!cmdInput) return [];
  if (cmdInput.TableName) return [cmdInput.TableName];
  if (cmdInput.RequestItems) return Object.keys(cmdInput.RequestItems);
  if (Array.isArray(cmdInput.TransactItems)) {
    return cmdInput.TransactItems
      .map((entry) => {
        // Includes Get for TransactGet (read-side) so the allowlist
        // gate doesn't miss it if a future read-side refactor lands.
        const op = entry.Put || entry.Update || entry.Delete || entry.ConditionCheck || entry.Get;
        return op?.TableName;
      })
      .filter(Boolean);
  }
  return [];
}

// Post-hoc inspection: every DDB command issued during the test must
// have a TableName in the allowlist. Unrouted commands in mockClient
// silently resolve, so a future refactor that writes to e.g.
// qurl_bot_flow_state from the gateway-tier SIGTERM path would not
// throw at runtime — this assertion is the catch. Walks every shape
// (single-item, BatchGet/Write RequestItems, TransactGet/Write
// TransactItems) so the regression surface is exhaustive. Used by
// both deploy-during-flow and resume-fail chaos tests (both
// gateway-tier paths that must not touch flow_state).
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
  // Belt-and-suspenders: if a future change adds flow_state to
  // `allowed` (mistakenly or otherwise), the throw above wouldn't
  // fire — this expect catches that drift specifically, since the
  // flow_state-on-gateway prohibition is the primary regression
  // target these chaos tests exist to protect.
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
