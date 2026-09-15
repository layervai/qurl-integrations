
const { mockClient } = require('aws-sdk-client-mock');
const {
  DynamoDBDocumentClient,
  PutCommand,
  GetCommand,
  DeleteCommand,
} = require('@aws-sdk/lib-dynamodb');
const { DynamoDBClient } = require('@aws-sdk/client-dynamodb');

const {
  createGatewaySessionStore,
  DEFAULT_WRITE_THROTTLE_MS,
} = require('../src/gateway-session-store');

function makeStore({ clock, writeThrottleMs } = {}) {
  const rawClient = new DynamoDBClient({});
  const docClient = DynamoDBDocumentClient.from(rawClient);
  const ddbMock = mockClient(docClient);
  const logger = {
    info: jest.fn(),
    warn: jest.fn(),
    error: jest.fn(),
    debug: jest.fn(),
  };
  const store = createGatewaySessionStore({
    ddbClient: docClient,
    tableName: 'test-gateway-session',
    shardId: '0:1',
    logger,
    clock,
    writeThrottleMs,
  });
  return { store, ddbMock, logger };
}

function sessionInfo({ sessionId = 'sess-abc', resumeURL = 'wss://r.discord/abc', sequence = 1 } = {}) {
  return { sessionId, resumeURL, sequence };
}

describe('createGatewaySessionStore — factory validation', () => {
  it('throws when required args are missing', () => {
    expect(() => createGatewaySessionStore()).toThrow(/ddbClient is required/);
    expect(() => createGatewaySessionStore({ ddbClient: {} })).toThrow(/tableName is required/);
    expect(() => createGatewaySessionStore({ ddbClient: {}, tableName: 't' }))
      .toThrow(/shardId is required/);
    expect(() => createGatewaySessionStore({ ddbClient: {}, tableName: 't', shardId: '0:1' }))
      .toThrow(/logger is required/);
  });
});

describe('hydrate', () => {
  it('returns null and logs cold-start when no row exists', async () => {
    const { store, ddbMock, logger } = makeStore();
    ddbMock.on(GetCommand).resolves({ Item: undefined });

    const result = await store.hydrate();

    expect(result).toBeNull();
    expect(store._getMirrorForTest()).toBeNull();
    expect(logger.info).toHaveBeenCalledWith(
      expect.stringMatching(/cold start/i),
    );
  });

  it('parses a well-formed row into mirror and returns it', async () => {
    const { store, ddbMock } = makeStore();
    ddbMock.on(GetCommand).resolves({
      Item: {
        shard_id: '0:1',
        session_id: 'sess-xyz',
        resume_url: 'wss://r.discord/xyz',
        sequence: 42,
        updated_at: 1700000000000,
      },
    });

    const result = await store.hydrate();

    expect(result).toEqual({
      sessionId: 'sess-xyz',
      resumeURL: 'wss://r.discord/xyz',
      sequence: 42,
    });
    expect(store._getMirrorForTest()).toEqual(result);
  });

  it('treats malformed rows as cold start (does not throw or leak session credentials)', async () => {
    const { store, ddbMock, logger } = makeStore();
    ddbMock.on(GetCommand).resolves({
      Item: {
        shard_id: '0:1',
        session_id: 'sess-leaky-secret',
        resume_url: 'wss://r.discord/leaky-resume-url',
        sequence: 'not-a-number',
      },
    });

    const result = await store.hydrate();

    expect(result).toBeNull();
    expect(store._getMirrorForTest()).toBeNull();
    const warnCall = logger.warn.mock.calls.find(
      (call) => /malformed/i.test(call[0]),
    );
    expect(warnCall).toBeDefined();
    const payload = warnCall[1];
    expect(payload).toEqual(expect.objectContaining({
      types: expect.objectContaining({
        session_id: 'string',
        resume_url: 'string',
        sequence: 'string',
      }),
    }));
    const serialized = JSON.stringify(payload);
    expect(serialized).not.toMatch(/sess-leaky-secret/);
    expect(serialized).not.toMatch(/leaky-resume-url/);
  });

  it('treats DDB read errors as cold start (does not throw)', async () => {
    const { store, ddbMock, logger } = makeStore();
    ddbMock.on(GetCommand).rejects(new Error('DDB unavailable'));

    const result = await store.hydrate();

    expect(result).toBeNull();
    expect(store._getMirrorForTest()).toBeNull();
    expect(logger.warn).toHaveBeenCalledWith(
      expect.stringMatching(/hydrate failed/i),
      expect.objectContaining({ error: 'DDB unavailable' }),
    );
  });
});

describe('retrieveSessionInfo — in-memory mirror contract', () => {
  it('returns null before hydrate runs', () => {
    const { store } = makeStore();
    expect(store.retrieveSessionInfo('0:1')).toBeNull();
  });

  it('returns hydrated mirror without a fresh DDB read', async () => {
    const { store, ddbMock } = makeStore();
    ddbMock.on(GetCommand).resolves({
      Item: {
        shard_id: '0:1',
        session_id: 'sess-1',
        resume_url: 'wss://r/1',
        sequence: 10,
      },
    });
    await store.hydrate();

    for (let i = 0; i < 100; i++) {
      store.retrieveSessionInfo('0:1');
    }

    expect(ddbMock.commandCalls(GetCommand)).toHaveLength(1);
  });
});

describe('updateSessionInfo — null-clear contract', () => {
  it('clears mirror, cancels pending flush, and issues DDB delete', async () => {
    let now = 1_000_000;
    const clock = () => now;
    const { store, ddbMock } = makeStore({ clock, writeThrottleMs: 1000 });

    ddbMock.on(PutCommand).resolves({});
    ddbMock.on(DeleteCommand).resolves({});

    await store.updateSessionInfo('0:1', sessionInfo({ sessionId: 'sess-A', sequence: 1 }));
    expect(store._getMirrorForTest()).not.toBeNull();

    now += 100;
    await store.updateSessionInfo('0:1', sessionInfo({ sessionId: 'sess-A', sequence: 2 }));

    await store.updateSessionInfo('0:1', null);

    expect(store._getMirrorForTest()).toBeNull();
    expect(ddbMock.commandCalls(DeleteCommand)).toHaveLength(1);

    now += 2000;
    await new Promise((r) => setImmediate(r));
    expect(ddbMock.commandCalls(PutCommand)).toHaveLength(1); // only the initial prime
  });

  it('after null-clear, retrieveSessionInfo returns null (the contract)', async () => {
    const { store, ddbMock } = makeStore();
    ddbMock.on(PutCommand).resolves({});
    ddbMock.on(DeleteCommand).resolves({});

    await store.updateSessionInfo('0:1', sessionInfo({ sessionId: 'sess-A' }));
    expect(store.retrieveSessionInfo('0:1')).not.toBeNull();

    await store.updateSessionInfo('0:1', null);
    expect(store.retrieveSessionInfo('0:1')).toBeNull();
  });

  it('logs but does not throw when DDB delete fails', async () => {
    const { store, ddbMock, logger } = makeStore();
    ddbMock.on(DeleteCommand).rejects(new Error('throughput exceeded'));

    store.updateSessionInfo('0:1', null);
    await store._awaitInFlightForTest();
    expect(logger.warn).toHaveBeenCalledWith(
      expect.stringMatching(/null-clear delete failed/i),
      expect.objectContaining({ error: 'throughput exceeded' }),
    );
  });
});

describe('updateSessionInfo — immediate-write path (sessionId change)', () => {
  it('writes immediately when sessionId changes (READY-fresh-session)', async () => {
    const { store, ddbMock } = makeStore();
    ddbMock.on(PutCommand).resolves({});

    await store.updateSessionInfo('0:1', sessionInfo({ sessionId: 'sess-A', sequence: 5 }));
    expect(ddbMock.commandCalls(PutCommand)).toHaveLength(1);

    await store.updateSessionInfo('0:1', sessionInfo({ sessionId: 'sess-B', sequence: 6 }));
    expect(ddbMock.commandCalls(PutCommand)).toHaveLength(2);
  });

  it('persists the @discordjs/ws shape as DDB snake_case columns', async () => {
    const { store, ddbMock } = makeStore({ clock: () => 1_700_000_000_000 });
    ddbMock.on(PutCommand).resolves({});

    await store.updateSessionInfo('0:1', sessionInfo({
      sessionId: 'sess-Z',
      resumeURL: 'wss://r.discord/z',
      sequence: 99,
    }));

    const calls = ddbMock.commandCalls(PutCommand);
    expect(calls[0].args[0].input).toEqual({
      TableName: 'test-gateway-session',
      Item: {
        shard_id: '0:1',
        session_id: 'sess-Z',
        resume_url: 'wss://r.discord/z',
        sequence: 99,
        updated_at: 1_700_000_000_000,
      },
    });
  });
});

describe('updateSessionInfo — throttle path', () => {
  it('defers writes within throttle window; one flush at boundary', async () => {
    let now = 1_000_000;
    const clock = () => now;
    const { store, ddbMock } = makeStore({ clock, writeThrottleMs: 1000 });

    ddbMock.on(PutCommand).resolves({});

    await store.updateSessionInfo('0:1', sessionInfo({ sessionId: 'sess-A', sequence: 1 }));
    expect(ddbMock.commandCalls(PutCommand)).toHaveLength(1);

    now += 100;
    await store.updateSessionInfo('0:1', sessionInfo({ sessionId: 'sess-A', sequence: 2 }));
    now += 100;
    await store.updateSessionInfo('0:1', sessionInfo({ sessionId: 'sess-A', sequence: 3 }));
    now += 100;
    await store.updateSessionInfo('0:1', sessionInfo({ sessionId: 'sess-A', sequence: 4 }));
    expect(ddbMock.commandCalls(PutCommand)).toHaveLength(1);

    expect(store._getMirrorForTest()).toEqual(expect.objectContaining({ sequence: 4 }));
  });

  it('issues exactly one deferred write after rapid updates', async () => {
    jest.useFakeTimers();
    let now = 1_000_000;
    const clock = () => now;
    const { store, ddbMock } = makeStore({ clock, writeThrottleMs: 1000 });

    ddbMock.on(PutCommand).resolves({});

    await store.updateSessionInfo('0:1', sessionInfo({ sessionId: 'sess-A', sequence: 1 }));
    now += 100;
    await store.updateSessionInfo('0:1', sessionInfo({ sessionId: 'sess-A', sequence: 2 }));
    now += 100;
    await store.updateSessionInfo('0:1', sessionInfo({ sessionId: 'sess-A', sequence: 3 }));
    now += 100;
    await store.updateSessionInfo('0:1', sessionInfo({ sessionId: 'sess-A', sequence: 4 }));

    now += 1000;
    await jest.advanceTimersByTimeAsync(1000);
    const puts = ddbMock.commandCalls(PutCommand);
    expect(puts).toHaveLength(2);
    expect(puts[1].args[0].input.Item.sequence).toBe(4);

    jest.useRealTimers();
  });

  it('logs but does not throw when a fire-and-forget write fails', async () => {
    jest.useFakeTimers();
    let now = 1_000_000;
    const clock = () => now;
    const { store, ddbMock, logger } = makeStore({ clock, writeThrottleMs: 1000 });

    ddbMock.on(PutCommand)
      .resolvesOnce({})
      .rejects(new Error('throttling'));

    store.updateSessionInfo('0:1', sessionInfo({ sessionId: 'sess-A', sequence: 1 }));
    now += 100;
    store.updateSessionInfo('0:1', sessionInfo({ sessionId: 'sess-A', sequence: 2 }));
    now += 1000;
    await jest.advanceTimersByTimeAsync(1000);
    await store._awaitInFlightForTest();

    expect(logger.warn).toHaveBeenCalledWith(
      expect.stringMatching(/write failed/i),
      expect.objectContaining({ error: 'throttling' }),
    );
    jest.useRealTimers();
  });
});

describe('flushFinal', () => {
  it('cancels pending flush and writes the mirror state', async () => {
    jest.useFakeTimers();
    let now = 1_000_000;
    const clock = () => now;
    const { store, ddbMock } = makeStore({ clock, writeThrottleMs: 1000 });

    ddbMock.on(PutCommand).resolves({});

    await store.updateSessionInfo('0:1', sessionInfo({ sessionId: 'sess-A', sequence: 1 }));
    now += 100;
    await store.updateSessionInfo('0:1', sessionInfo({ sessionId: 'sess-A', sequence: 99 }));

    await store.flushFinal();
    const puts = ddbMock.commandCalls(PutCommand);
    expect(puts).toHaveLength(2);
    expect(puts[1].args[0].input.Item.sequence).toBe(99);

    now += 2000;
    await jest.advanceTimersByTimeAsync(2000);
    expect(ddbMock.commandCalls(PutCommand)).toHaveLength(2);

    jest.useRealTimers();
  });

  it('is a no-op when mirror is null (already cleared)', async () => {
    const { store, ddbMock } = makeStore();
    ddbMock.on(PutCommand).resolves({});

    await store.flushFinal();
    expect(ddbMock.commandCalls(PutCommand)).toHaveLength(0);
  });

  it('is idempotent — a second call does not re-write the mirror', async () => {
    const { store, ddbMock } = makeStore();
    ddbMock.on(PutCommand).resolves({});

    store.updateSessionInfo('0:1', sessionInfo({ sessionId: 'sess-A', sequence: 1 }));
    await store.flushFinal();
    const firstCount = ddbMock.commandCalls(PutCommand).length;

    await store.flushFinal();
    await store.flushFinal();
    expect(ddbMock.commandCalls(PutCommand)).toHaveLength(firstCount);
  });

  it('awaits every in-flight fire-and-forget write before exit (not just the most recent)', async () => {
    let resolveFirstWrite;
    let secondWriteFired = false;

    const rawClient = new DynamoDBClient({});
    const docClient = DynamoDBDocumentClient.from(rawClient);
    const ddbMock = mockClient(docClient);
    ddbMock.on(PutCommand)
      .callsFakeOnce(() => new Promise((resolve) => { resolveFirstWrite = resolve; }))
      .callsFake(() => { secondWriteFired = true; return Promise.resolve({}); });

    const logger = {
      info: jest.fn(), warn: jest.fn(), error: jest.fn(), debug: jest.fn(),
    };
    const store = createGatewaySessionStore({
      ddbClient: docClient,
      tableName: 't',
      shardId: '0:1',
      logger,
    });

    store.updateSessionInfo('0:1', { sessionId: 'sess-A', resumeURL: 'wss://r/a', sequence: 1 });
    store.updateSessionInfo('0:1', { sessionId: 'sess-B', resumeURL: 'wss://r/b', sequence: 2 });
    expect(secondWriteFired).toBe(true);

    const flushPromise = store.flushFinal();
    let flushSettled = false;
    flushPromise.then(() => { flushSettled = true; });

    await new Promise((r) => setImmediate(r));
    expect(flushSettled).toBe(false);

    resolveFirstWrite({});
    await flushPromise;
    expect(flushSettled).toBe(true);
  });

  it('awaits an in-flight null-clear delete chased by a fresh-session put', async () => {
    let resolveDelete;
    let putFired = false;

    const rawClient = new DynamoDBClient({});
    const docClient = DynamoDBDocumentClient.from(rawClient);
    const ddbMock = mockClient(docClient);
    ddbMock.on(DeleteCommand).callsFake(() => new Promise((resolve) => { resolveDelete = resolve; }));
    ddbMock.on(PutCommand).callsFake(() => { putFired = true; return Promise.resolve({}); });

    const logger = {
      info: jest.fn(), warn: jest.fn(), error: jest.fn(), debug: jest.fn(),
    };
    const store = createGatewaySessionStore({
      ddbClient: docClient, tableName: 't', shardId: '0:1', logger,
    });

    store.updateSessionInfo('0:1', sessionInfo({ sessionId: 'sess-A', sequence: 1 }));
    store.updateSessionInfo('0:1', null);
    expect(ddbMock.commandCalls(DeleteCommand)).toHaveLength(1);
    store.updateSessionInfo('0:1', sessionInfo({ sessionId: 'sess-B', sequence: 1 }));
    expect(putFired).toBe(true);

    const flushPromise = store.flushFinal();
    let flushSettled = false;
    flushPromise.then(() => { flushSettled = true; });
    await new Promise((r) => setImmediate(r));
    expect(flushSettled).toBe(false);

    resolveDelete({});
    await flushPromise;
    expect(flushSettled).toBe(true);
  });

  it('after flushFinal, subsequent updateSessionInfo calls are dropped', async () => {
    const { store, ddbMock } = makeStore();
    ddbMock.on(PutCommand).resolves({});

    await store.updateSessionInfo('0:1', sessionInfo({ sessionId: 'sess-A', sequence: 1 }));
    await store.flushFinal();
    expect(ddbMock.commandCalls(PutCommand)).toHaveLength(2); // prime + flushFinal

    await store.updateSessionInfo('0:1', sessionInfo({ sessionId: 'sess-B', sequence: 2 }));
    expect(ddbMock.commandCalls(PutCommand)).toHaveLength(2);
  });
});

describe('stop()', () => {
  it('cancels pending flush without writing', async () => {
    jest.useFakeTimers();
    let now = 1_000_000;
    const clock = () => now;
    const { store, ddbMock } = makeStore({ clock, writeThrottleMs: 1000 });

    ddbMock.on(PutCommand).resolves({});

    await store.updateSessionInfo('0:1', sessionInfo({ sessionId: 'sess-A', sequence: 1 }));
    now += 100;
    await store.updateSessionInfo('0:1', sessionInfo({ sessionId: 'sess-A', sequence: 99 }));

    store.stop();
    now += 2000;
    await jest.advanceTimersByTimeAsync(2000);
    expect(ddbMock.commandCalls(PutCommand)).toHaveLength(1);

    jest.useRealTimers();
  });
});

describe('throttle default', () => {
  it('exposes DEFAULT_WRITE_THROTTLE_MS at 1000', () => {
    expect(DEFAULT_WRITE_THROTTLE_MS).toBe(1000);
  });
});
