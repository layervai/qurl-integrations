jest.mock('../src/config', () => ({
  ENABLE_EVENT_SHIPPER: true,
  QURL_BOT_EVENTS_QUEUE_URL: 'https://sqs.us-east-2.amazonaws.com/123/events',
  QURL_BOT_DRAIN_DEADLINE_MS: 3000,
}));
jest.mock('../src/logger', () => ({ info: jest.fn(), warn: jest.fn(), error: jest.fn(), debug: jest.fn() }));

const publisher = require('../src/event-publisher');
const { createGatewaySessionStore } = require('../src/gateway-session-store');
const { WebSocketShard, WebSocketShardEvents } = require('@discordjs/ws');
const logger = require('../src/logger');
const tick = () => new Promise(setImmediate);
const info = { sessionId: 'test-session', resumeURL: 'wss://gateway.invalid', sequence: 10, shardId: 0, shardCount: 1 };

afterEach(() => publisher._test._resetStateForTest());

test('the SDK session update before dispatch cannot checkpoint an unpublished message', async () => {
  let accepted;
  const send = new Promise((resolve) => { accepted = resolve; });
  publisher._test._setSqsClientForTest({ send: jest.fn(() => send) });
  publisher.start();
  const ddbClient = { send: jest.fn().mockResolvedValue({}) };
  const store = createGatewaySessionStore({
    ddbClient, tableName: 'test-session', shardId: '0:1', logger,
    writeThrottleMs: 0,
    beforePersist: () => publisher.waitForPublishes(),
  });
  try {
    store.updateSessionInfo(0, { ...info, sequence: 9 });
    await tick();
    ddbClient.send.mockClear();
    const shard = new WebSocketShard({ options: {}, retrieveSessionInfo: store.retrieveSessionInfo,
      updateSessionInfo: store.updateSessionInfo }, 0);
    shard.on(WebSocketShardEvents.Dispatch, ({ data }) => publisher.publish(data));
    // TODO(upstream-contract): drive the pinned SDK's actual update-before-Dispatch path.
    await shard.onMessage(JSON.stringify({ op: 0, t: 'INTERACTION_CREATE', s: 10, d: { id: 'test-interaction' } }), false);
    await tick();
    expect(ddbClient.send.mock.calls.length).toBe(0);
    accepted({ MessageId: 'accepted-message' });
    await tick();
    expect(ddbClient.send).toHaveBeenCalledTimes(1);
    expect(ddbClient.send.mock.calls[0][0].input.Item.sequence).toBe(10);
  } finally {
    accepted({ MessageId: 'accepted-message' });
    await publisher.stop();
    await store.flushFinal();
  }
});

test.each(['reject', 'throw'])('a %s failure keeps the old checkpoint despite a later successful publish', async (mode) => {
  const send = jest.fn(() => {
    if (mode === 'throw') throw new Error('queue unavailable');
    return Promise.reject(new Error('queue unavailable'));
  });
  const onFailure = jest.fn();
  publisher._test._setSqsClientForTest({ send });
  publisher.start({ onFailure });
  const ddbClient = { send: jest.fn().mockResolvedValue({}) };
  const store = createGatewaySessionStore({ ddbClient, tableName: 'test-session', shardId: '0:1', logger,
    writeThrottleMs: 0, beforePersist: () => publisher.waitForPublishes() });
  try {
    store.updateSessionInfo(0, { ...info, sequence: 9 });
    await tick();
    ddbClient.send.mockClear();
    store.updateSessionInfo(0, info);
    publisher.publish({ op: 0, t: 'INTERACTION_CREATE', s: 10, d: { id: 'failed' } });
    await tick();
    expect(ddbClient.send.mock.calls.length).toBe(0);
    expect(onFailure).toHaveBeenCalledTimes(1);
    expect(store.retrieveSessionInfo(0).sequence).toBe(10);
    store.rewindToDurableCheckpoint();
    expect(store.retrieveSessionInfo(0).sequence).toBe(9);
    send.mockResolvedValue({ MessageId: 'later-message' });
    store.updateSessionInfo(0, { ...info, sequence: 11 });
    publisher.publish({ op: 0, t: 'INTERACTION_CREATE', s: 11, d: { id: 'later' } });
    await tick();
    await expect(publisher.waitForPublishes()).rejects.toThrow('unconfirmed');
    expect(ddbClient.send.mock.calls.length).toBe(0);
  } finally {
    await publisher.stop();
    await store.flushFinal();
  }
  expect(ddbClient.send.mock.calls.length).toBe(0);
});

test('a pending send has a bounded checkpoint wait and can later complete', async () => {
  let accepted;
  publisher._test._setSqsClientForTest({ send: () => new Promise((resolve) => { accepted = resolve; }) });
  publisher._test._setDrainDeadlineForTest(5);
  publisher.start();
  publisher.publish({ op: 0, t: 'INTERACTION_CREATE', s: 10, d: { id: 'pending' } });
  try {
    await expect(publisher.waitForPublishes()).rejects.toThrow('unconfirmed');
    accepted({ MessageId: 'eventually-accepted' });
    await expect(publisher.waitForPublishes()).resolves.toBeUndefined();
  } finally {
    accepted({ MessageId: 'eventually-accepted' });
    await publisher.stop();
  }
});

test('a dispatch dropped before startup cannot be checkpointed', async () => {
  publisher.publish({ op: 0, t: 'INTERACTION_CREATE', s: 10, d: { id: 'not-published' } });
  await expect(publisher.waitForPublishes()).rejects.toThrow('unconfirmed');
});

test('an explicit invalid-session clear is not resurrected by reconnect', async () => {
  const ddbClient = { send: jest.fn().mockResolvedValue({}) };
  const store = createGatewaySessionStore({ ddbClient, tableName: 'test-session', shardId: '0:1', logger,
    beforePersist: async () => {} });
  store.updateSessionInfo(0, info);
  await tick();
  ddbClient.send.mockRejectedValue(new Error('delete unavailable'));
  store.updateSessionInfo(0, null);
  await tick();
  store.rewindToDurableCheckpoint();
  expect(store.retrieveSessionInfo(0)).toBeNull();
  await store.flushFinal();
});

test('a hydrated checkpoint remains the reconnect floor when new publication is unconfirmed', async () => {
  const row = { shard_id: '0:1', session_id: info.sessionId, resume_url: info.resumeURL,
    sequence: 9, session_shard_id: 0, session_shard_count: 1, updated_at: Date.now() };
  const ddbClient = { send: jest.fn().mockResolvedValue({ Item: row }) };
  const store = createGatewaySessionStore({ ddbClient, tableName: 'test-session', shardId: '0:1', logger,
    beforePersist: async () => { throw new Error('publication unconfirmed'); } });
  await store.hydrate();
  expect(store.retrieveSessionInfo(0).sequence).toBe(9);
  store.updateSessionInfo(0, info);
  await tick();
  expect(store.retrieveSessionInfo(0).sequence).toBe(10);
  store.rewindToDurableCheckpoint();
  expect(store.retrieveSessionInfo(0).sequence).toBe(9);
  await store.flushFinal();
});
