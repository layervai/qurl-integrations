// End-to-end integration test for the Pillar 2 stack: gateway-ws-shim
// composed with gateway-session-store, driven through the full
// hydrate → start → READY → INTERACTION_CREATE → SIGTERM lifecycle.
//
// Unit tests in tests/gateway-session-store.test.js and tests/
// gateway-ws-shim.test.js cover each module's contracts in isolation.
// This test pins the composition: the shim's callback wiring lands in
// the store correctly, READY detection flips isReady AND persists,
// final-flush on stop writes the latest sequence (without calling
// manager.destroy()).
//
// Real-world failure mode this test would catch: a refactor renaming
// the store's `updateSessionInfo` signature without updating the
// shim's wiring would silently break cross-process RESUME in
// production (in-process unit tests both still pass; the integration
// surface fails). One test here covers the contract that no unit
// test alone can.

const { EventEmitter } = require('node:events');
const { mockClient } = require('aws-sdk-client-mock');
const { DynamoDBClient } = require('@aws-sdk/client-dynamodb');
const {
  DynamoDBDocumentClient,
  PutCommand,
  GetCommand,
  DeleteCommand,
} = require('@aws-sdk/lib-dynamodb');
const { WebSocketShardEvents } = require('@discordjs/ws');

const { createGatewaySessionStore } = require('../src/gateway-session-store');
const { createGatewayWsShim } = require('../src/gateway-ws-shim');

// FakeWebSocketManager — replays the shape @discordjs/ws emits when
// driven by real Discord traffic. Tests construct one via the
// `WebSocketManagerCtor` injection seam on createGatewayWsShim.
function makeFakeManagerCtor() {
  const instances = [];
  function FakeManager(args) {
    const inst = new EventEmitter();
    inst._args = args;
    inst.connect = jest.fn().mockResolvedValue(undefined);
    inst.destroy = jest.fn().mockResolvedValue(undefined);
    instances.push(inst);
    return inst;
  }
  return { FakeManager, instances };
}

// FakeREST — the shim lazy-constructs a REST instance when none is
// injected. Production uses @discordjs/rest; we stub so the test
// doesn't depend on the real REST surface.
function makeFakeRESTCtor() {
  function FakeREST() {
    const inst = { token: null };
    inst.setToken = jest.fn().mockImplementation((t) => { inst.token = t; return inst; });
    return inst;
  }
  return { FakeREST };
}

function makeLogger() {
  return {
    info: jest.fn(),
    warn: jest.fn(),
    error: jest.fn(),
    debug: jest.fn(),
  };
}

describe('Pillar 2 integration — shim + store full lifecycle', () => {
  it('cold start → READY → INTERACTION_CREATE → SIGTERM persists final sequence', async () => {
    jest.useFakeTimers();
    let now = 1_700_000_000_000;
    const clock = () => now;

    // ── DDB setup ──
    const rawClient = new DynamoDBClient({});
    const docClient = DynamoDBDocumentClient.from(rawClient);
    const ddbMock = mockClient(docClient);
    ddbMock.on(GetCommand).resolves({ Item: undefined }); // cold start
    ddbMock.on(PutCommand).resolves({});
    ddbMock.on(DeleteCommand).resolves({});

    // ── Shim setup ──
    const logger = makeLogger();
    const store = createGatewaySessionStore({
      ddbClient: docClient,
      tableName: 'qurl-bot-discord-test-gateway-session',
      shardId: '0:1',
      logger,
      clock,
      writeThrottleMs: 1000,
    });
    const { FakeManager, instances } = makeFakeManagerCtor();
    const { FakeREST } = makeFakeRESTCtor();
    const shim = createGatewayWsShim({
      token: 'test-token',
      intents: 1,
      store,
      logger,
      WebSocketManagerCtor: FakeManager,
      RESTCtor: FakeREST,
    });

    // ── Lifecycle ──
    // 1. Hydrate (cold start — no persisted session).
    const hydrated = await shim.hydrate();
    expect(hydrated).toBeNull();
    expect(shim.isReady()).toBe(false);

    // 2. Start — shim wires callbacks into the (fake) manager. With
    //    mirror still null, retrieveSessionInfo will return null
    //    once (IDENTIFY budget burns one); after that the throw
    //    would surface. We don't trigger that path here because
    //    the cold-start cycle has the library invoke retrieve
    //    exactly once on the way to IDENTIFY → READY.
    await shim.start();
    expect(instances).toHaveLength(1);
    const mgr = instances[0];

    // Subscribe a fake event-publisher to verify dispatch fan-out.
    const publisher = jest.fn();
    shim.onDispatch(({ data }) => publisher(data));

    // 3. Fake the IDENTIFY path: @discordjs/ws calls retrieveSessionInfo,
    //    sees null, identifies, then on READY calls
    //    updateSessionInfo(shardId, sessionInfo). Simulate by directly
    //    calling the wired callback (the manager's `_args`).
    const { retrieveSessionInfo, updateSessionInfo } = mgr._args;

    expect(retrieveSessionInfo('0:1')).toBeNull();
    // IDENTIFY pending (budget=1/1 — one IDENTIFY is OK on cold start).
    expect(shim._getIdentifyAttemptsForTest()).toBe(1);

    // READY arrives — updateSessionInfo fires with the new session.
    await updateSessionInfo('0:1', {
      sessionId: 'sess-fresh',
      resumeURL: 'wss://resume.discord.gg/?v=10&encoding=json',
      sequence: 1,
    });
    // First write is immediate (sessionId change from null → real).
    expect(ddbMock.commandCalls(PutCommand)).toHaveLength(1);

    // 4. Dispatch the READY event so isReady flips and the publisher fan-out fires.
    mgr.emit(WebSocketShardEvents.Dispatch, {
      data: { op: 0, t: 'READY', s: 1, d: { application: { id: '999000111222333444' } } },
      shardId: 0,
    });
    expect(shim.isReady()).toBe(true);
    expect(shim.getAppId()).toBe('999000111222333444');
    expect(publisher).toHaveBeenLastCalledWith(expect.objectContaining({ t: 'READY' }));

    // 5. INTERACTION_CREATE: updateSessionInfo fires with bumped
    //    sequence; publisher receives the dispatch.
    now += 100;
    await updateSessionInfo('0:1', {
      sessionId: 'sess-fresh',
      resumeURL: 'wss://resume.discord.gg/?v=10&encoding=json',
      sequence: 2,
    });
    mgr.emit(WebSocketShardEvents.Dispatch, {
      data: { op: 0, t: 'INTERACTION_CREATE', s: 2, d: { id: 'interaction-1' } },
      shardId: 0,
    });
    expect(publisher).toHaveBeenLastCalledWith(expect.objectContaining({ t: 'INTERACTION_CREATE' }));
    // Throttled (within 1 s window, same sessionId) — no new Put yet.
    expect(ddbMock.commandCalls(PutCommand)).toHaveLength(1);

    // 6. More dispatches inside the throttle window — mirror updates,
    //    DDB writes deferred.
    now += 100;
    await updateSessionInfo('0:1', {
      sessionId: 'sess-fresh', resumeURL: 'wss://r/', sequence: 3,
    });
    now += 100;
    await updateSessionInfo('0:1', {
      sessionId: 'sess-fresh', resumeURL: 'wss://r/', sequence: 42,
    });
    expect(ddbMock.commandCalls(PutCommand)).toHaveLength(1); // still just the initial.
    expect(store._getMirrorForTest()).toEqual(expect.objectContaining({ sequence: 42 }));

    // 7. SIGTERM: flush + stop. The final-flush MUST persist the
    //    latest sequence (42) without calling manager.destroy().
    //    Without flushFinal, the deferred write would land later
    //    (or get cancelled), and the next process's hydrate() would
    //    see sequence=1 — Discord would replay all events between
    //    seq 1 and the truth at exit time.
    await shim.stop();

    // manager.destroy MUST NOT have been called — the load-bearing
    // SIGTERM contract that keeps Discord's 60 s resume buffer alive.
    expect(mgr.destroy).not.toHaveBeenCalled();

    // Final-flush wrote the latest mirror state (sequence 42).
    const puts = ddbMock.commandCalls(PutCommand);
    expect(puts).toHaveLength(2);
    expect(puts[1].args[0].input.Item.sequence).toBe(42);
    expect(puts[1].args[0].input.Item.session_id).toBe('sess-fresh');

    jest.useRealTimers();
  });

  it('warm start → hydrate returns persisted session → RESUME path', async () => {
    // Simulates the next-process boot after a SIGTERM. The DDB row
    // exists from the prior process; hydrate populates the mirror;
    // retrieveSessionInfo returns the row instead of null — so
    // @discordjs/ws issues RESUME (op 6) rather than IDENTIFY (op 2)
    // and the IDENTIFY budget stays at zero.
    const rawClient = new DynamoDBClient({});
    const docClient = DynamoDBDocumentClient.from(rawClient);
    const ddbMock = mockClient(docClient);
    ddbMock.on(GetCommand).resolves({
      Item: {
        shard_id: '0:1',
        session_id: 'sess-prior',
        resume_url: 'wss://r.discord/prior',
        sequence: 42,
        updated_at: 1_700_000_000_000,
      },
    });
    ddbMock.on(PutCommand).resolves({});

    const logger = makeLogger();
    const store = createGatewaySessionStore({
      ddbClient: docClient,
      tableName: 't',
      shardId: '0:1',
      logger,
    });
    const { FakeManager, instances } = makeFakeManagerCtor();
    const shim = createGatewayWsShim({
      token: 't',
      intents: 1,
      store,
      logger,
      WebSocketManagerCtor: FakeManager,
      RESTCtor: makeFakeRESTCtor().FakeREST,
    });

    const hydrated = await shim.hydrate();
    expect(hydrated).toEqual({
      sessionId: 'sess-prior',
      resumeURL: 'wss://r.discord/prior',
      sequence: 42,
    });

    await shim.start();
    const { retrieveSessionInfo } = instances[0]._args;

    // Critical: retrieveSessionInfo returns the persisted row.
    // @discordjs/ws sees a non-null SessionInfo and chooses RESUME.
    // The IDENTIFY counter stays zero — exactly the win Pillar 2
    // unlocks (no IDENTIFY budget burn on a restart).
    expect(retrieveSessionInfo('0:1')).toEqual({
      sessionId: 'sess-prior',
      resumeURL: 'wss://r.discord/prior',
      sequence: 42,
    });
    expect(shim._getIdentifyAttemptsForTest()).toBe(0);

    await shim.stop();
  });

  it('Discord rejects RESUME → updateSessionInfo(null) → next retrieve returns null (no infinite loop)', async () => {
    // The spike's first sandbox run encountered this: if the mirror
    // is not honored, the standby loops forever. Discord rejects
    // the RESUME, library calls updateSessionInfo(null), library
    // calls retrieveSessionInfo again, our store hands back the
    // stale session, Discord rejects again, repeat every ~200 ms.
    // The fix is mirror-respect-null; this test pins it across the
    // composed surface.
    const rawClient = new DynamoDBClient({});
    const docClient = DynamoDBDocumentClient.from(rawClient);
    const ddbMock = mockClient(docClient);
    ddbMock.on(GetCommand).resolves({
      Item: {
        shard_id: '0:1', session_id: 'sess-X', resume_url: 'wss://r/', sequence: 1,
      },
    });
    ddbMock.on(PutCommand).resolves({});
    ddbMock.on(DeleteCommand).resolves({});

    const logger = makeLogger();
    const store = createGatewaySessionStore({
      ddbClient: docClient, tableName: 't', shardId: '0:1', logger,
    });
    const { FakeManager, instances } = makeFakeManagerCtor();
    const shim = createGatewayWsShim({
      token: 't', intents: 1, store, logger,
      WebSocketManagerCtor: FakeManager,
      RESTCtor: makeFakeRESTCtor().FakeREST,
    });

    await shim.hydrate();
    await shim.start();
    const { retrieveSessionInfo, updateSessionInfo } = instances[0]._args;

    // 1. Mirror holds the persisted session; first retrieve returns it.
    expect(retrieveSessionInfo('0:1')).not.toBeNull();

    // 2. Discord rejects RESUME — library calls updateSessionInfo(null).
    await updateSessionInfo('0:1', null);

    // 3. Library calls retrieveSessionInfo again — MUST return null
    //    to break the loop. If this returned the stored session
    //    (mirror miss), we'd be in the infinite-loop hazard.
    expect(retrieveSessionInfo('0:1')).toBeNull();

    // 4. DDB Delete was issued so the NEXT process's hydrate() also
    //    sees null (cross-process consistency, not just in-process).
    expect(ddbMock.commandCalls(DeleteCommand)).toHaveLength(1);

    await shim.stop({ flushFinal: false });
  });
});
