// Unit tests for src/gateway-peer-heartbeat.js — Pillar 3 standby
// discovery. Pins the four load-bearing contracts:
//
//   1. Freshness filter is the correctness primitive, NOT DDB TTL.
//   2. Single PutItem per renewal — updated_at + expires_at together.
//   3. TTL writer shape is epoch SECONDS, never milliseconds.
//   4. Scan + filter is the correct access pattern (consistency at
//      6s freshness; small row count).

const { mockClient } = require('aws-sdk-client-mock');
const {
  DynamoDBDocumentClient,
  PutCommand,
  ScanCommand,
  DeleteCommand,
} = require('@aws-sdk/lib-dynamodb');
const { DynamoDBClient } = require('@aws-sdk/client-dynamodb');

const {
  createPeerHeartbeat,
  DEFAULT_FRESHNESS_WINDOW_SECONDS,
  DEFAULT_TTL_SECONDS,
} = require('../src/gateway-peer-heartbeat');

function makeHeartbeat({
  clock, freshnessWindowSeconds, ttlSeconds,
  instanceId = 'inst-A', ip = '10.0.1.5', port = 9876, shardId = '0:1',
} = {}) {
  const rawClient = new DynamoDBClient({});
  const docClient = DynamoDBDocumentClient.from(rawClient);
  const ddbMock = mockClient(docClient);
  const logger = {
    info: jest.fn(), warn: jest.fn(), error: jest.fn(), debug: jest.fn(),
  };
  const heartbeat = createPeerHeartbeat({
    ddbClient: docClient,
    tableName: 'test-gateway-peer-heartbeat',
    instanceId, ip, port, shardId, logger, clock,
    freshnessWindowSeconds, ttlSeconds,
  });
  return { heartbeat, ddbMock, logger };
}

describe('createPeerHeartbeat — factory validation', () => {
  it('throws when required args are missing', () => {
    expect(() => createPeerHeartbeat()).toThrow(/ddbClient is required/);
    expect(() => createPeerHeartbeat({ ddbClient: {} })).toThrow(/tableName is required/);
    expect(() => createPeerHeartbeat({ ddbClient: {}, tableName: 't' }))
      .toThrow(/instanceId is required/);
    expect(() => createPeerHeartbeat({
      ddbClient: {}, tableName: 't', instanceId: 'i',
    })).toThrow(/ip is required/);
    expect(() => createPeerHeartbeat({
      ddbClient: {}, tableName: 't', instanceId: 'i', ip: '10.0.0.1',
    })).toThrow(/port \(number\) is required/);
    expect(() => createPeerHeartbeat({
      ddbClient: {}, tableName: 't', instanceId: 'i', ip: '10.0.0.1', port: 9876,
    })).toThrow(/shardId is required/);
    expect(() => createPeerHeartbeat({
      ddbClient: {}, tableName: 't', instanceId: 'i', ip: '10.0.0.1', port: 9876, shardId: '0:1',
    })).toThrow(/logger is required/);
  });

  it('rejects a non-number port (defends against string-from-env bug)', () => {
    // The control-channel port comes from config (env). A bug that
    // forgot to parseInt would surface as a string "9876" which would
    // serialize into DDB as an S-type, and DDB readers expecting N
    // would skip it. Catch at construction.
    expect(() => createPeerHeartbeat({
      ddbClient: {}, tableName: 't', instanceId: 'i', ip: '10.0.0.1',
      port: '9876', shardId: '0:1', logger: {},
    })).toThrow(/port \(number\) is required/);
  });
});

describe('writeHeartbeat', () => {
  it('writes updated_at AND expires_at in the SAME PutItem (contract 2)', async () => {
    // Splitting these into two ops creates a window where the
    // freshness signal (updated_at) and the TTL marker (expires_at)
    // disagree — a partial write could leave a row visible past its
    // intended reap, or reap a row whose freshness is still good.
    // Single PutItem keeps them lock-step.
    const { heartbeat, ddbMock } = makeHeartbeat({
      clock: () => 1_700_000_000_000, ttlSeconds: 60,
    });
    ddbMock.on(PutCommand).resolves({});

    await heartbeat.writeHeartbeat();

    const putCalls = ddbMock.commandCalls(PutCommand);
    expect(putCalls).toHaveLength(1);
    const item = putCalls[0].args[0].input.Item;
    expect(item.updated_at).toBe(1_700_000_000);
    expect(item.expires_at).toBe(1_700_000_060); // updated_at + ttl
  });

  it('writes expires_at as epoch SECONDS (not milliseconds)', async () => {
    // Contract 3 — same TTL writer shape as gateway-session-store /
    // gateway-lock. Pinning this catches a refactor that introduces
    // a `clock()` change (returns seconds instead of ms) without
    // updating the divide-by-1000.
    const { heartbeat, ddbMock } = makeHeartbeat({
      clock: () => 1_700_000_000_000,
    });
    ddbMock.on(PutCommand).resolves({});

    await heartbeat.writeHeartbeat();

    const item = ddbMock.commandCalls(PutCommand)[0].args[0].input.Item;
    expect(item.expires_at).toBeLessThan(2_000_000_000); // sanity: not ms
    expect(item.updated_at).toBeLessThan(2_000_000_000);
  });

  it('persists ip, port, shard_id alongside the timestamps', async () => {
    // The handoff path POSTs to http://<ip>:<port>/control/yours.
    // Missing any of these on the row makes the peer unreachable
    // even after a fresh write.
    const { heartbeat, ddbMock } = makeHeartbeat({
      clock: () => 1_700_000_000_000,
    });
    ddbMock.on(PutCommand).resolves({});

    await heartbeat.writeHeartbeat();

    const item = ddbMock.commandCalls(PutCommand)[0].args[0].input.Item;
    expect(item).toMatchObject({
      instance_id: 'inst-A',
      ip: '10.0.1.5',
      port: 9876,
      shard_id: '0:1',
    });
  });

  it('uses no condition expression — idempotent overwrite is the desired shape', async () => {
    // Heartbeats from THIS replica should always win against any
    // earlier write. No CAS, no version. A retry of a same-second
    // heartbeat just overwrites the previous one with identical
    // fields — harmless.
    const { heartbeat, ddbMock } = makeHeartbeat({
      clock: () => 1_700_000_000_000,
    });
    ddbMock.on(PutCommand).resolves({});

    await heartbeat.writeHeartbeat();

    const putInput = ddbMock.commandCalls(PutCommand)[0].args[0].input;
    expect(putInput.ConditionExpression).toBeUndefined();
  });

  it('throws on transport error (caller decides whether a missed beat is fatal)', async () => {
    // A single missed heartbeat is fine — the freshness window
    // absorbs three. The leader coordinator catches this and
    // continues. The module's contract is "throw on transport
    // failure so the caller can log + count" rather than
    // silently swallow.
    const { heartbeat, ddbMock } = makeHeartbeat({
      clock: () => 1_700_000_000_000,
    });
    ddbMock.on(PutCommand).rejects(new Error('throughput exceeded'));

    await expect(heartbeat.writeHeartbeat()).rejects.toThrow(/throughput exceeded/);
  });
});

describe('listFreshPeers', () => {
  it('returns peers whose updated_at is within the freshness window', async () => {
    // Contract 1 — freshness filter at read time. A row past
    // `updated_at + freshnessWindowSeconds` must be invisible to
    // the active even if DDB TTL hasn't reaped it.
    const { heartbeat, ddbMock } = makeHeartbeat({
      clock: () => 1_700_000_010_000, // now = 1700000010
      freshnessWindowSeconds: 6,
    });
    ddbMock.on(ScanCommand).resolves({
      Items: [
        { instance_id: 'inst-A', shard_id: '0:1', updated_at: 1_700_000_009, ip: '10.0.0.1', port: 9876 }, // self — excluded
        { instance_id: 'inst-B', shard_id: '0:1', updated_at: 1_700_000_008, ip: '10.0.0.2', port: 9876 }, // fresh (2s ago)
        { instance_id: 'inst-C', shard_id: '0:1', updated_at: 1_700_000_003, ip: '10.0.0.3', port: 9876 }, // stale (7s ago)
        { instance_id: 'inst-D', shard_id: '0:1', updated_at: 1_700_000_005, ip: '10.0.0.4', port: 9876 }, // fresh edge (5s ago)
      ],
    });

    const peers = await heartbeat.listFreshPeers();
    const ids = peers.map((p) => p.instance_id);
    expect(ids).toEqual(['inst-B', 'inst-D']); // self excluded; inst-C stale; freshest first
  });

  it('excludes the active replica\'s own row by instance_id', async () => {
    // Even if our own heartbeat is fresh (it always is — we wrote
    // it 2s ago), we don't push-handoff to ourselves.
    const { heartbeat, ddbMock } = makeHeartbeat({
      clock: () => 1_700_000_010_000, freshnessWindowSeconds: 6,
    });
    ddbMock.on(ScanCommand).resolves({
      Items: [
        { instance_id: 'inst-A', shard_id: '0:1', updated_at: 1_700_000_009, ip: '10.0.0.1', port: 9876 },
      ],
    });

    expect(await heartbeat.listFreshPeers()).toEqual([]);
  });

  it('filters by shard_id so a future sharded topology routes correctly', async () => {
    // Today every replica carries `"0:1"` so this is a no-op, but
    // the filter must be in place for the sharded future — otherwise
    // shard 0's active would scan shard 5's standby and POST to
    // the wrong target.
    const { heartbeat, ddbMock } = makeHeartbeat({
      clock: () => 1_700_000_010_000, freshnessWindowSeconds: 6,
    });
    ddbMock.on(ScanCommand).resolves({
      Items: [
        { instance_id: 'inst-B', shard_id: '0:1', updated_at: 1_700_000_009, ip: '10.0.0.2', port: 9876 },
        { instance_id: 'inst-C', shard_id: '5:8', updated_at: 1_700_000_009, ip: '10.0.0.3', port: 9876 },
      ],
    });

    const peers = await heartbeat.listFreshPeers();
    expect(peers.map((p) => p.instance_id)).toEqual(['inst-B']);
  });

  it('sorts freshest-first so the caller takes the head of the list', async () => {
    // Multiple fresh peers (a transient overlap during deploy) —
    // the active picks the most recently heartbeating one because
    // it's the most likely to still be alive when the POST lands.
    const { heartbeat, ddbMock } = makeHeartbeat({
      clock: () => 1_700_000_010_000, freshnessWindowSeconds: 6,
    });
    ddbMock.on(ScanCommand).resolves({
      Items: [
        { instance_id: 'inst-B', shard_id: '0:1', updated_at: 1_700_000_006, ip: '10.0.0.2', port: 9876 },
        { instance_id: 'inst-C', shard_id: '0:1', updated_at: 1_700_000_009, ip: '10.0.0.3', port: 9876 },
        { instance_id: 'inst-D', shard_id: '0:1', updated_at: 1_700_000_008, ip: '10.0.0.4', port: 9876 },
      ],
    });

    const peers = await heartbeat.listFreshPeers();
    expect(peers.map((p) => p.instance_id)).toEqual(['inst-C', 'inst-D', 'inst-B']);
  });

  it('drops rows whose updated_at is missing or non-numeric (defensive against partial writes)', async () => {
    // A corrupted row from a manual operator write, or a partial
    // failure in some future updater, should not surface as a
    // peer candidate. Same defense shape as gateway-session-store's
    // malformed-row hydrate path.
    const { heartbeat, ddbMock } = makeHeartbeat({
      clock: () => 1_700_000_010_000, freshnessWindowSeconds: 6,
    });
    ddbMock.on(ScanCommand).resolves({
      Items: [
        { instance_id: 'inst-B', shard_id: '0:1', updated_at: 1_700_000_009, ip: '10.0.0.2', port: 9876 },
        { instance_id: 'inst-C', shard_id: '0:1', ip: '10.0.0.3', port: 9876 }, // missing updated_at
        { instance_id: 'inst-D', shard_id: '0:1', updated_at: 'recently', ip: '10.0.0.4', port: 9876 }, // string
      ],
    });

    const peers = await heartbeat.listFreshPeers();
    expect(peers.map((p) => p.instance_id)).toEqual(['inst-B']);
  });

  it('returns [] when the scan returns no Items (empty table cold start)', async () => {
    // Standby hasn't booted yet. The active falls through to the
    // cold-fallback path (~7 s floor).
    const { heartbeat, ddbMock } = makeHeartbeat({
      clock: () => 1_700_000_010_000,
    });
    ddbMock.on(ScanCommand).resolves({}); // no Items key at all

    expect(await heartbeat.listFreshPeers()).toEqual([]);
  });

  it('cutoff is computed from caller-wall-clock, not stored value', async () => {
    // Symmetric to the lock module's `:now` contract — the freshness
    // boundary depends on OUR clock. A refactor that cached `cutoff`
    // somewhere would let the boundary drift and stale peers
    // re-appear.
    let nowMs = 1_700_000_010_000;
    const { heartbeat, ddbMock } = makeHeartbeat({
      clock: () => nowMs, freshnessWindowSeconds: 6,
    });
    ddbMock.on(ScanCommand).resolves({
      Items: [
        { instance_id: 'inst-B', shard_id: '0:1', updated_at: 1_700_000_005, ip: '10.0.0.2', port: 9876 },
      ],
    });

    expect(await heartbeat.listFreshPeers()).toHaveLength(1); // 5s ago: still fresh

    nowMs = 1_700_000_020_000; // 15s later
    expect(await heartbeat.listFreshPeers()).toEqual([]); // 15s ago: stale
  });
});

describe('deleteOwnRow', () => {
  it('issues DeleteItem keyed by self instance_id (best-effort, no CAS)', async () => {
    // Called at clean shutdown to close the discovery window
    // immediately rather than waiting for the freshness boundary.
    // No CAS — this row is ours by construction (PK).
    const { heartbeat, ddbMock } = makeHeartbeat();
    ddbMock.on(DeleteCommand).resolves({});

    await heartbeat.deleteOwnRow();

    const deleteCalls = ddbMock.commandCalls(DeleteCommand);
    expect(deleteCalls).toHaveLength(1);
    expect(deleteCalls[0].args[0].input.Key).toEqual({ instance_id: 'inst-A' });
    expect(deleteCalls[0].args[0].input.ConditionExpression).toBeUndefined();
  });

  it('logs but does not throw on transport error (SIGTERM path must stay clean)', async () => {
    // Called from gracefulShutdown. A throw would unwind into the
    // shutdown handler and mask the cleaner exit path. The freshness
    // filter is the actual safety net — this delete is hygiene.
    const { heartbeat, ddbMock, logger } = makeHeartbeat();
    ddbMock.on(DeleteCommand).rejects(new Error('network blip'));

    await expect(heartbeat.deleteOwnRow()).resolves.toBeUndefined();
    expect(logger.warn).toHaveBeenCalledWith(
      'gateway-peer-heartbeat: delete own row failed',
      expect.objectContaining({ instanceId: 'inst-A', error: 'network blip' }),
    );
  });
});

describe('default constants', () => {
  it('exports the documented freshness window (6s) and TTL (60s, 10x the window)', () => {
    // Pinning these matters: 6 s freshness is what bounds the
    // "three missed heartbeats = dead" semantic; 60 s TTL is 10×
    // the window per the table comment so a transient write hiccup
    // doesn't reap a live row.
    expect(DEFAULT_FRESHNESS_WINDOW_SECONDS).toBe(6);
    expect(DEFAULT_TTL_SECONDS).toBe(60);
    expect(DEFAULT_TTL_SECONDS).toBe(DEFAULT_FRESHNESS_WINDOW_SECONDS * 10);
  });
});
