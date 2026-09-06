
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
  lockHolder,
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
    instanceId, ip, port, shardId, logger, clock, lockHolder,
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
    })).toThrow(/ip \(IPv4 or IPv6 literal\) is required/);
    expect(() => createPeerHeartbeat({
      ddbClient: {}, tableName: 't', instanceId: 'i', ip: '10.0.0.1',
    })).toThrow(/port \(integer 1-65535\) is required/);
    expect(() => createPeerHeartbeat({
      ddbClient: {}, tableName: 't', instanceId: 'i', ip: '10.0.0.1', port: 9876,
    })).toThrow(/shardId is required/);
    expect(() => createPeerHeartbeat({
      ddbClient: {}, tableName: 't', instanceId: 'i', ip: '10.0.0.1', port: 9876, shardId: '0:1',
    })).toThrow(/logger is required/);
  });

  it('rejects invalid IPs (literal "undefined" from env-stringification, garbage strings, IPs masked as identifiers)', () => {
    const base = {
      ddbClient: {}, tableName: 't', instanceId: 'i', port: 9876,
      shardId: '0:1', logger: {},
    };
    expect(() => createPeerHeartbeat({ ...base, ip: 'undefined' }))
      .toThrow(/ip \(IPv4 or IPv6 literal\) is required/);
    expect(() => createPeerHeartbeat({ ...base, ip: 'not.an.ip' }))
      .toThrow(/ip \(IPv4 or IPv6 literal\) is required/);
    expect(() => createPeerHeartbeat({ ...base, ip: '999.999.999.999' }))
      .toThrow(/ip \(IPv4 or IPv6 literal\) is required/);
    expect(() => createPeerHeartbeat({ ...base, ip: 'fe80::1::2' })) // malformed v6
      .toThrow(/ip \(IPv4 or IPv6 literal\) is required/);
  });

  it('accepts both IPv4 and IPv6 literals', () => {
    const base = {
      ddbClient: {}, tableName: 't', instanceId: 'i', port: 9876,
      shardId: '0:1', logger: {},
    };
    expect(() => createPeerHeartbeat({ ...base, ip: '10.0.0.1' })).not.toThrow();
    expect(() => createPeerHeartbeat({ ...base, ip: 'fe80::1' })).not.toThrow();
    expect(() => createPeerHeartbeat({ ...base, ip: '::1' })).not.toThrow();
  });

  it('rejects non-string or empty-string lockHolder when provided', () => {
    const base = {
      ddbClient: {}, tableName: 't', instanceId: 'i', ip: '10.0.0.1',
      port: 9876, shardId: '0:1', logger: {},
    };
    expect(() => createPeerHeartbeat({ ...base, lockHolder: '' }))
      .toThrow(/lockHolder must be a non-empty string when provided/);
    expect(() => createPeerHeartbeat({ ...base, lockHolder: 42 }))
      .toThrow(/lockHolder must be a non-empty string when provided/);
    expect(() => createPeerHeartbeat({ ...base, lockHolder: true }))
      .toThrow(/lockHolder must be a non-empty string when provided/);
    expect(() => createPeerHeartbeat({ ...base, lockHolder: {} }))
      .toThrow(/lockHolder must be a non-empty string when provided/);
    expect(() => createPeerHeartbeat({ ...base })).not.toThrow();
    expect(() => createPeerHeartbeat({ ...base, lockHolder: undefined })).not.toThrow();
    expect(() => createPeerHeartbeat({ ...base, lockHolder: 'task-arn:..../inst-A' })).not.toThrow();
  });

  it('rejects invalid ports (string, NaN, 0, negative, >65535, fractional)', () => {
    const base = {
      ddbClient: {}, tableName: 't', instanceId: 'i', ip: '10.0.0.1',
      shardId: '0:1', logger: {},
    };
    expect(() => createPeerHeartbeat({ ...base, port: '9876' }))
      .toThrow(/port \(integer 1-65535\) is required/);
    expect(() => createPeerHeartbeat({ ...base, port: NaN }))
      .toThrow(/port \(integer 1-65535\) is required/);
    expect(() => createPeerHeartbeat({ ...base, port: 0 }))
      .toThrow(/port \(integer 1-65535\) is required/);
    expect(() => createPeerHeartbeat({ ...base, port: -1 }))
      .toThrow(/port \(integer 1-65535\) is required/);
    expect(() => createPeerHeartbeat({ ...base, port: 65536 }))
      .toThrow(/port \(integer 1-65535\) is required/);
    expect(() => createPeerHeartbeat({ ...base, port: 9876.5 }))
      .toThrow(/port \(integer 1-65535\) is required/);
  });
});

describe('writeHeartbeat', () => {
  it('writes updated_at AND expires_at in the SAME PutItem (contract 2)', async () => {
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

  it('writes lock_holder when supplied, omits when absent', async () => {
    const { heartbeat: withHolder, ddbMock: m1 } = makeHeartbeat({
      clock: () => 1_700_000_000_000,
      lockHolder: 'task-arn:.../inst-A',
    });
    m1.on(PutCommand).resolves({});
    await withHolder.writeHeartbeat();
    expect(m1.commandCalls(PutCommand)[0].args[0].input.Item.lock_holder)
      .toBe('task-arn:.../inst-A');

    const { heartbeat: noHolder, ddbMock: m2 } = makeHeartbeat({
      clock: () => 1_700_000_000_000,
    });
    m2.on(PutCommand).resolves({});
    await noHolder.writeHeartbeat();
    expect(m2.commandCalls(PutCommand)[0].args[0].input.Item)
      .not.toHaveProperty('lock_holder');
  });

  it('uses no condition expression — idempotent overwrite is the desired shape', async () => {
    const { heartbeat, ddbMock } = makeHeartbeat({
      clock: () => 1_700_000_000_000,
    });
    ddbMock.on(PutCommand).resolves({});

    await heartbeat.writeHeartbeat();

    const putInput = ddbMock.commandCalls(PutCommand)[0].args[0].input;
    expect(putInput.ConditionExpression).toBeUndefined();
  });

  it('throws on transport error (caller decides whether a missed beat is fatal)', async () => {
    const { heartbeat, ddbMock } = makeHeartbeat({
      clock: () => 1_700_000_000_000,
    });
    ddbMock.on(PutCommand).rejects(new Error('throughput exceeded'));

    await expect(heartbeat.writeHeartbeat()).rejects.toThrow(/throughput exceeded/);
  });
});

describe('listFreshPeers', () => {
  it('returns peers whose updated_at is within the freshness window', async () => {
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
    const { heartbeat, ddbMock } = makeHeartbeat({
      clock: () => 1_700_000_010_000,
    });
    ddbMock.on(ScanCommand).resolves({}); // no Items key at all

    expect(await heartbeat.listFreshPeers()).toEqual([]);
  });

  it('cutoff is computed from caller-wall-clock, not stored value', async () => {
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
    const { heartbeat, ddbMock } = makeHeartbeat();
    ddbMock.on(DeleteCommand).resolves({});

    await heartbeat.deleteOwnRow();

    const deleteCalls = ddbMock.commandCalls(DeleteCommand);
    expect(deleteCalls).toHaveLength(1);
    expect(deleteCalls[0].args[0].input.Key).toEqual({ instance_id: 'inst-A' });
    expect(deleteCalls[0].args[0].input.ConditionExpression).toBeUndefined();
  });

  it('logs but does not throw on transport error (SIGTERM path must stay clean)', async () => {
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
    expect(DEFAULT_FRESHNESS_WINDOW_SECONDS).toBe(6);
    expect(DEFAULT_TTL_SECONDS).toBe(60);
    expect(DEFAULT_TTL_SECONDS).toBe(DEFAULT_FRESHNESS_WINDOW_SECONDS * 10);
  });
});
