// Unit tests for src/gateway-session-store.js — Pillar 2 DDB-backed
// session store. Pins the four load-bearing contracts the module
// header enumerates:
//
//   1. In-memory mirror (retrieveSessionInfo never hits DDB after
//      hydrate).
//   2. Null-clear respected (mirror set to null AND DDB row deleted).
//   3. Write throttling (READY/sessionId-change writes immediately;
//      sequence-only updates within throttle window defer).
//   4. Final flush on SIGTERM (cancels pending timer; persists mirror).
//
// Failure of any of these contracts produced a real incident class:
//   - Mirror miss → infinite RESUME-reject loop (spike's first run).
//   - Null-clear miss → same infinite loop on the next process boot.
//   - Throttle miss → DDB write burn at dispatch rate ($50/mo+ at
//     interaction-storm volume).
//   - Final-flush miss → stale-sequence RESUME after every restart,
//     observable as "Discord replays the last few seconds of events."

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

// Helper to build the standard test store + mocks. Each test calls
// this fresh so state isolation is automatic (vs reusing a top-level
// store across tests, which would leak mirror state).
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

// Realistic SessionInfo shape that @discordjs/ws would hand to
// updateSessionInfo. The three fields (sessionId, resumeURL,
// sequence) are the ones the design-doc DDB row carries; any
// other fields @discordjs/ws may add are dropped on write (the
// store doesn't pass info through verbatim).
function sessionInfo({ sessionId = 'sess-abc', resumeURL = 'wss://r.discord/abc', sequence = 1 } = {}) {
  return { sessionId, resumeURL, sequence };
}

describe('createGatewaySessionStore — factory validation', () => {
  it('throws when required args are missing', () => {
    // Pin every required-arg name so a refactor renaming one fails
    // loudly rather than silently dropping a guard.
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

    // Mirror is hydrated with the @discordjs/ws-facing shape
    // (sessionId/resumeURL/sequence camelCase) — distinct from
    // the snake_case DDB column names.
    expect(result).toEqual({
      sessionId: 'sess-xyz',
      resumeURL: 'wss://r.discord/xyz',
      sequence: 42,
    });
    expect(store._getMirrorForTest()).toEqual(result);
  });

  it('treats malformed rows as cold start (does not throw or leak session credentials)', async () => {
    // Production bot MUST boot even if the DDB row is corrupted.
    // The recovery path is IDENTIFY-from-scratch, which the
    // mirror=null state surfaces to @discordjs/ws on the first
    // retrieveSessionInfo call.
    //
    // Security: the warn log must NOT include the actual session_id
    // or resume_url values — those are session-bound credentials
    // reachable from CloudWatch. Pin the type-signature-only shape.
    const { store, ddbMock, logger } = makeStore();
    ddbMock.on(GetCommand).resolves({
      Item: {
        shard_id: '0:1',
        session_id: 'sess-leaky-secret',
        resume_url: 'wss://r.discord/leaky-resume-url',
        // sequence is missing/non-number to trigger the malformed branch
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
    // Type signature is logged...
    expect(payload).toEqual(expect.objectContaining({
      types: expect.objectContaining({
        session_id: 'string',
        resume_url: 'string',
        sequence: 'string',
      }),
    }));
    // ...but the actual credential values are NOT in the payload.
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

    // Call retrieveSessionInfo 100 times — DDB Get count stays at 1
    // (the single hydrate). Pinning this rules out a regression
    // where a future refactor reads DDB inside retrieveSessionInfo
    // (which would re-introduce the infinite-loop hazard the
    // spike documented).
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

    // Prime mirror with a session via the immediate-write path.
    await store.updateSessionInfo('0:1', sessionInfo({ sessionId: 'sess-A', sequence: 1 }));
    expect(store._getMirrorForTest()).not.toBeNull();

    // Issue a sequence-only update while still inside the throttle
    // window so a deferred flush gets scheduled.
    now += 100;
    await store.updateSessionInfo('0:1', sessionInfo({ sessionId: 'sess-A', sequence: 2 }));

    // Now null-clear — mirror flips to null, DDB delete fires,
    // and the pending timer is cancelled (no second Put after the
    // null-clear settles).
    await store.updateSessionInfo('0:1', null);

    expect(store._getMirrorForTest()).toBeNull();
    expect(ddbMock.commandCalls(DeleteCommand)).toHaveLength(1);

    // Advance past the throttle window and yield to the event loop
    // — if the pending timer wasn't cancelled, a second Put would
    // fire here. We assert it did NOT.
    now += 2000;
    await new Promise((r) => setImmediate(r));
    expect(ddbMock.commandCalls(PutCommand)).toHaveLength(1); // only the initial prime
  });

  it('after null-clear, retrieveSessionInfo returns null (the contract)', async () => {
    // This is the contract the spike's first sandbox run discovered:
    // if retrieveSessionInfo returns the old session post-null-clear,
    // Discord rejects RESUME, library calls updateSessionInfo(null)
    // again, infinite loop.
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

    // updateSessionInfo returns synchronously; the DDB delete is
    // fire-and-forget. Settle the in-flight promise via the test
    // seam before asserting on the warn log.
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

    // First call: lastWrittenSessionId=null, change detected, write fires.
    await store.updateSessionInfo('0:1', sessionInfo({ sessionId: 'sess-A', sequence: 5 }));
    expect(ddbMock.commandCalls(PutCommand)).toHaveLength(1);

    // Second call with same sessionId but different sequence: throttle
    // path (no immediate write, since throttle hasn't expired). We
    // assert this in the throttle test below; here we focus on the
    // sessionId-change path.
    await store.updateSessionInfo('0:1', sessionInfo({ sessionId: 'sess-B', sequence: 6 }));
    // sessionId changed → immediate write again.
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

    // Pin the column-name contract — a rename to camelCase on the
    // DDB side would diverge from infra (modules/qurl-bot-ddb's
    // schema uses snake_case shard_id PK + the attribute names
    // here as the bot's payload).
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

    // Prime with the first update — immediate write because
    // lastWriteAt=0 → throttleExpired=true.
    await store.updateSessionInfo('0:1', sessionInfo({ sessionId: 'sess-A', sequence: 1 }));
    expect(ddbMock.commandCalls(PutCommand)).toHaveLength(1);

    // Rapid sequence updates inside the throttle window — no new
    // writes should fire synchronously.
    now += 100;
    await store.updateSessionInfo('0:1', sessionInfo({ sessionId: 'sess-A', sequence: 2 }));
    now += 100;
    await store.updateSessionInfo('0:1', sessionInfo({ sessionId: 'sess-A', sequence: 3 }));
    now += 100;
    await store.updateSessionInfo('0:1', sessionInfo({ sessionId: 'sess-A', sequence: 4 }));
    expect(ddbMock.commandCalls(PutCommand)).toHaveLength(1);

    // Mirror reflects the latest sequence even though DDB hasn't
    // caught up — pins the "mirror is fresh, DDB lags" contract.
    // Deferred-flush firing is covered by the explicit-fake-timer
    // test immediately below.
    expect(store._getMirrorForTest()).toEqual(expect.objectContaining({ sequence: 4 }));
  });

  it('issues exactly one deferred write after rapid updates', async () => {
    jest.useFakeTimers();
    let now = 1_000_000;
    const clock = () => now;
    const { store, ddbMock } = makeStore({ clock, writeThrottleMs: 1000 });

    ddbMock.on(PutCommand).resolves({});

    // Prime: immediate write (throttleExpired=true at lastWriteAt=0).
    await store.updateSessionInfo('0:1', sessionInfo({ sessionId: 'sess-A', sequence: 1 }));
    // Burst of in-window updates.
    now += 100;
    await store.updateSessionInfo('0:1', sessionInfo({ sessionId: 'sess-A', sequence: 2 }));
    now += 100;
    await store.updateSessionInfo('0:1', sessionInfo({ sessionId: 'sess-A', sequence: 3 }));
    now += 100;
    await store.updateSessionInfo('0:1', sessionInfo({ sessionId: 'sess-A', sequence: 4 }));

    // Advance fake timers past the throttle boundary. The deferred
    // flush should fire once with the latest sequence (4).
    now += 1000;
    await jest.advanceTimersByTimeAsync(1000);
    // One additional Put beyond the prime — total 2.
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

    // First Put succeeds (the prime); second Put (deferred fire-
    // and-forget after the throttle elapses) fails.
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

    // Prime + schedule a deferred flush.
    await store.updateSessionInfo('0:1', sessionInfo({ sessionId: 'sess-A', sequence: 1 }));
    now += 100;
    await store.updateSessionInfo('0:1', sessionInfo({ sessionId: 'sess-A', sequence: 99 }));

    // flushFinal cancels the timer AND writes synchronously.
    await store.flushFinal();
    const puts = ddbMock.commandCalls(PutCommand);
    // Prime + flushFinal = 2 puts total. The most recent carries
    // the latest sequence (99).
    expect(puts).toHaveLength(2);
    expect(puts[1].args[0].input.Item.sequence).toBe(99);

    // Advance past the original throttle boundary — the cancelled
    // deferred flush must NOT fire (would be a 3rd Put).
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

  it('awaits every in-flight fire-and-forget write before exit (not just the most recent)', async () => {
    // Without Promise.allSettled across the full set, a SIGTERM
    // that lands between a null-clear delete and a fresh-session
    // put could exit while the earlier delete is still in flight.
    // The Set-based tracker keeps all outstanding writes pending
    // until flushFinal settles them.
    let resolveFirstWrite;
    let secondWriteFired = false;

    const rawClient = new DynamoDBClient({});
    const docClient = DynamoDBDocumentClient.from(rawClient);
    const ddbMock = mockClient(docClient);
    // First Put blocks until we resolve it; second Put resolves
    // immediately. Without the Set tracker, awaiting only the
    // second would lose the first's settlement signal.
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

    // Fire two writes back-to-back (sessionId change → immediate
    // writes both times).
    store.updateSessionInfo('0:1', { sessionId: 'sess-A', resumeURL: 'wss://r/a', sequence: 1 });
    store.updateSessionInfo('0:1', { sessionId: 'sess-B', resumeURL: 'wss://r/b', sequence: 2 });
    expect(secondWriteFired).toBe(true);

    // Kick off flushFinal — it must NOT resolve until the still-
    // pending first write completes.
    const flushPromise = store.flushFinal();
    let flushSettled = false;
    flushPromise.then(() => { flushSettled = true; });

    // Drain the microtask queue — flush is still pending on the
    // blocked write.
    await new Promise((r) => setImmediate(r));
    expect(flushSettled).toBe(false);

    // Now unblock and verify flush completes.
    resolveFirstWrite({});
    await flushPromise;
    expect(flushSettled).toBe(true);
  });

  it('awaits an in-flight null-clear delete chased by a fresh-session put', async () => {
    // The other "awaits every in-flight write" test exercises
    // two consecutive puts. This one pins the more operationally
    // relevant shape: a RESUME rejection (updateSessionInfo(null)
    // → Delete) immediately followed by a fresh IDENTIFY's READY
    // (updateSessionInfo({...}) → Put). Both writes have to settle
    // before flushFinal returns, otherwise SIGTERM could exit
    // mid-delete and the next process would hydrate a row that
    // should have been cleared.
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

    // Prime mirror with a session, then null-clear (Delete starts,
    // blocked on resolveDelete).
    store.updateSessionInfo('0:1', sessionInfo({ sessionId: 'sess-A', sequence: 1 }));
    store.updateSessionInfo('0:1', null);
    expect(ddbMock.commandCalls(DeleteCommand)).toHaveLength(1);
    // Fresh IDENTIFY's READY fires while Delete is still in-flight.
    store.updateSessionInfo('0:1', sessionInfo({ sessionId: 'sess-B', sequence: 1 }));
    expect(putFired).toBe(true);

    // flushFinal awaits BOTH the in-flight Delete AND the in-flight
    // Put. It can't resolve until we unblock the Delete.
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
    // SIGTERM lands; flushFinal runs and sets stopped=true. A late
    // dispatch arriving on the way out shouldn't trigger another
    // DDB write — the process is exiting, the next process will
    // pick up state from the row we just wrote.
    const { store, ddbMock } = makeStore();
    ddbMock.on(PutCommand).resolves({});

    await store.updateSessionInfo('0:1', sessionInfo({ sessionId: 'sess-A', sequence: 1 }));
    await store.flushFinal();
    expect(ddbMock.commandCalls(PutCommand)).toHaveLength(2); // prime + flushFinal

    await store.updateSessionInfo('0:1', sessionInfo({ sessionId: 'sess-B', sequence: 2 }));
    // No additional Put — stopped=true short-circuits.
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
    // Only the prime — stop() neither wrote NOR fired the deferred.
    expect(ddbMock.commandCalls(PutCommand)).toHaveLength(1);

    jest.useRealTimers();
  });
});

describe('throttle default', () => {
  it('exposes DEFAULT_WRITE_THROTTLE_MS at 1000', () => {
    // Pin the design-doc-derived value. Changing it would be a
    // visible operational decision (cost vs cross-process lag);
    // pin so the change requires a test update.
    expect(DEFAULT_WRITE_THROTTLE_MS).toBe(1000);
  });
});
