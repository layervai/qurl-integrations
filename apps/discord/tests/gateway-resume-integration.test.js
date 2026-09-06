
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

function makeFakeManagerCtor() {
  const instances = [];
  function FakeManager(args) {
    const inst = new EventEmitter();
    inst._constructorArgs = args;
    inst.connect = jest.fn().mockResolvedValue(undefined);
    inst.destroy = jest.fn().mockResolvedValue(undefined);
    instances.push(inst);
    return inst;
  }
  return { FakeManager, instances };
}

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

    const rawClient = new DynamoDBClient({});
    const docClient = DynamoDBDocumentClient.from(rawClient);
    const ddbMock = mockClient(docClient);
    ddbMock.on(GetCommand).resolves({ Item: undefined }); // cold start
    ddbMock.on(PutCommand).resolves({});
    ddbMock.on(DeleteCommand).resolves({});

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

    const hydrated = await shim.hydrate();
    expect(hydrated).toBeNull();
    expect(shim.isReady()).toBe(false);

    await shim.start();
    expect(instances).toHaveLength(1);
    const mgr = instances[0];

    const publisher = jest.fn();
    shim.onDispatch(({ data }) => publisher(data));

    const { retrieveSessionInfo, updateSessionInfo } = mgr._constructorArgs;

    expect(retrieveSessionInfo('0:1')).toBeNull();
    expect(shim._getIdentifyAttemptsForTest()).toBe(1);

    updateSessionInfo('0:1', {
      sessionId: 'sess-fresh',
      resumeURL: 'wss://resume.discord.gg/?v=10&encoding=json',
      sequence: 1,
    });
    expect(ddbMock.commandCalls(PutCommand)).toHaveLength(1);

    mgr.emit(WebSocketShardEvents.Dispatch, {
      data: { op: 0, t: 'READY', s: 1, d: { application: { id: '999000111222333444' } } },
      shardId: 0,
    });
    expect(shim.isReady()).toBe(true);
    expect(shim.getAppId()).toBe('999000111222333444');
    expect(publisher).toHaveBeenLastCalledWith(expect.objectContaining({ t: 'READY' }));

    now += 100;
    updateSessionInfo('0:1', {
      sessionId: 'sess-fresh',
      resumeURL: 'wss://resume.discord.gg/?v=10&encoding=json',
      sequence: 2,
    });
    mgr.emit(WebSocketShardEvents.Dispatch, {
      data: { op: 0, t: 'INTERACTION_CREATE', s: 2, d: { id: 'interaction-1' } },
      shardId: 0,
    });
    expect(publisher).toHaveBeenLastCalledWith(expect.objectContaining({ t: 'INTERACTION_CREATE' }));
    expect(ddbMock.commandCalls(PutCommand)).toHaveLength(1);

    now += 100;
    updateSessionInfo('0:1', {
      sessionId: 'sess-fresh', resumeURL: 'wss://r/', sequence: 3,
    });
    now += 100;
    updateSessionInfo('0:1', {
      sessionId: 'sess-fresh', resumeURL: 'wss://r/', sequence: 42,
    });
    expect(ddbMock.commandCalls(PutCommand)).toHaveLength(1); // still just the initial.
    expect(store._getMirrorForTest()).toEqual(expect.objectContaining({ sequence: 42 }));

    await shim.stop();

    expect(mgr.destroy).not.toHaveBeenCalled();

    const puts = ddbMock.commandCalls(PutCommand);
    expect(puts).toHaveLength(2);
    expect(puts[1].args[0].input.Item.sequence).toBe(42);
    expect(puts[1].args[0].input.Item.session_id).toBe('sess-fresh');

    jest.useRealTimers();
  });

  it('warm start → hydrate returns persisted session → RESUME path', async () => {
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
    const mgr = instances[0];
    const { retrieveSessionInfo } = mgr._constructorArgs;

    expect(retrieveSessionInfo('0:1')).toEqual({
      sessionId: 'sess-prior',
      resumeURL: 'wss://r.discord/prior',
      sequence: 42,
    });
    expect(shim._getIdentifyAttemptsForTest()).toBe(0);
    expect(shim.isReady()).toBe(false);

    mgr.emit(WebSocketShardEvents.Dispatch, {
      data: { op: 0, t: 'RESUMED', s: 43, d: {} },
      shardId: 0,
    });
    expect(shim.isReady()).toBe(true);

    await shim.stop();
  });

  it('Discord rejects RESUME → updateSessionInfo(null) → next retrieve returns null (no infinite loop)', async () => {
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
    const { retrieveSessionInfo, updateSessionInfo } = instances[0]._constructorArgs;

    expect(retrieveSessionInfo('0:1')).not.toBeNull();

    updateSessionInfo('0:1', null);

    expect(retrieveSessionInfo('0:1')).toBeNull();

    expect(ddbMock.commandCalls(DeleteCommand)).toHaveLength(1);

    await shim.stop({ flushFinal: false });
  });

  it('registerCommands fires only on the first READY per process, not on RESUMED or a later READY', async () => {
    const rawClient = new DynamoDBClient({});
    const docClient = DynamoDBDocumentClient.from(rawClient);
    const ddbMock = mockClient(docClient);
    ddbMock.on(GetCommand).resolves({ Item: undefined });
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
    const mgr = instances[0];

    const registerCommandsCalls = [];
    let commandsRegistered = false;
    shim.onDispatch(({ data }) => {
      if (data?.t === 'READY' && !commandsRegistered) {
        const appId = shim.getAppId();
        if (appId) {
          commandsRegistered = true;
          registerCommandsCalls.push({ appId });
        }
      }
    });

    mgr.emit(WebSocketShardEvents.Dispatch, {
      data: { t: 'READY', d: { application: { id: 'app-1' }, session_id: 's1', resume_gateway_url: 'wss://r1/' } },
      shardId: '0:1',
    });
    expect(registerCommandsCalls).toHaveLength(1);
    expect(registerCommandsCalls[0]).toEqual({ appId: 'app-1' });

    mgr.emit(WebSocketShardEvents.Dispatch, {
      data: { t: 'RESUMED', d: {} },
      shardId: '0:1',
    });
    expect(registerCommandsCalls).toHaveLength(1);

    mgr.emit(WebSocketShardEvents.Dispatch, {
      data: { t: 'READY', d: { application: { id: 'app-1' }, session_id: 's2', resume_gateway_url: 'wss://r2/' } },
      shardId: '0:1',
    });
    expect(registerCommandsCalls).toHaveLength(1);

    await shim.stop({ flushFinal: false });
  });
});
