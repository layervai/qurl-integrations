jest.mock('../src/config', () => ({
  ENABLE_EVENT_SHIPPER: true, ENABLE_CREDENTIAL_LIFECYCLE: true,
  QURL_BOT_EVENTS_QUEUE_URL: 'https://sqs.us-east-2.amazonaws.com/123/events',
  QURL_BOT_DRAIN_DEADLINE_MS: 3000,
}));
jest.mock('../src/logger', () => ({ info: jest.fn(), warn: jest.fn(), error: jest.fn(), debug: jest.fn() }));
jest.mock('../src/store', () => ({ noteGuildInstallation: jest.fn(), enqueueGuildRemoval: jest.fn() }));
const publisher = require('../src/event-publisher');
const db = require('../src/store');
const logger = require('../src/logger');
const { createGatewaySessionStore } = require('../src/gateway-session-store');
const { WebSocketShard, WebSocketShardEvents } = require('@discordjs/ws');
const tick = () => new Promise(setImmediate);
const joinA = '2026-10-01T00:00:00.000000000Z';
const joinB = '2026-10-02T00:00:00.000000000Z';
let send;
beforeEach(() => {
  jest.resetAllMocks();
  publisher._test._resetStateForTest();
  db.noteGuildInstallation.mockResolvedValue(true);
  db.enqueueGuildRemoval.mockResolvedValue('durable-job');
  send = jest.fn().mockResolvedValue({ MessageId: 'accepted' });
  publisher._test._setSqsClientForTest({ send });
  publisher.start();
});
afterEach(async () => { await publisher.stop(); publisher._test._resetStateForTest(); });
const create = (joinedAt, s) => publisher.publish({ op: 0, t: 'GUILD_CREATE', s, d: { id: 'g', joined_at: joinedAt } });
const remove = (s, unavailable) => publisher.publish({ op: 0, t: 'GUILD_DELETE', s, d: { id: 'g', unavailable } });

test('installation persistence precedes forwarding an interaction from that guild', async () => {
  let accept;
  db.noteGuildInstallation.mockReturnValueOnce(new Promise(resolve => { accept = resolve; }));
  create(joinA, 8);
  publisher.publish({ op: 0, t: 'INTERACTION_CREATE', s: 9, d: { id: 'i', guild_id: 'g' } });
  await tick();
  expect(send).not.toHaveBeenCalled();
  accept(true);
  await publisher.waitForPublishes();
  expect(send).toHaveBeenCalledTimes(1);
});

test('the pinned SDK cannot checkpoint a removal until its job is durable', async () => {
  create(joinA, 8);
  await publisher.waitForPublishes();
  let accept;
  db.enqueueGuildRemoval.mockReturnValueOnce(new Promise(resolve => { accept = resolve; }));
  const ddbClient = { send: jest.fn().mockResolvedValue({}) };
  const store = createGatewaySessionStore({ ddbClient, tableName: 'test-session', shardId: '0:1', logger,
    writeThrottleMs: 0, beforePersist: () => publisher.waitForPublishes() });
  const info = { sessionId: 'session', resumeURL: 'wss://gateway.invalid', sequence: 9, shardId: 0, shardCount: 1 };
  store.updateSessionInfo(0, info);
  await tick();
  ddbClient.send.mockClear();
  const shard = new WebSocketShard({ options: {}, retrieveSessionInfo: store.retrieveSessionInfo,
    updateSessionInfo: store.updateSessionInfo }, 0);
  shard.on(WebSocketShardEvents.Dispatch, ({ data }) => publisher.publish(data));
  try {
    await shard.onMessage(JSON.stringify({ op: 0, t: 'GUILD_DELETE', s: 10, d: { id: 'g' } }), false);
    await tick();
    expect(ddbClient.send).not.toHaveBeenCalled();
    expect(db.enqueueGuildRemoval).toHaveBeenCalledWith(expect.objectContaining({ joinedAt: joinA }));
    accept('durable-job');
    await tick();
    expect(ddbClient.send.mock.calls[0][0].input.Item.sequence).toBe(10);
    expect(send).not.toHaveBeenCalled();
  } finally { accept('durable-job'); await store.flushFinal(); }
});

test('temporary unavailability neither queues cleanup nor discards its installation', async () => {
  create(joinA, 8);
  remove(9, true);
  await publisher.waitForPublishes();
  expect(db.enqueueGuildRemoval).not.toHaveBeenCalled();
  remove(10);
  await publisher.waitForPublishes();
  expect(db.enqueueGuildRemoval).toHaveBeenCalledWith(expect.objectContaining({ joinedAt: joinA }));
});

test('an older delete replay cannot mark or replace a newer cached installation', async () => {
  create(joinA, 8);
  create(joinB, 11);
  remove(10);
  create(joinA, 8);
  remove(12);
  await publisher.waitForPublishes();
  expect(db.enqueueGuildRemoval.mock.calls.map(([job]) => job.joinedAt)).toEqual([null, joinB]);
});

test('cold resume without an installation anchor queues reconciliation without guessing', async () => {
  remove(10);
  await publisher.waitForPublishes();
  expect(db.enqueueGuildRemoval).toHaveBeenCalledWith(expect.objectContaining({ joinedAt: null }));
});

test('a failed membership write blocks later interaction publication and checkpoint progress', async () => {
  const onFailure = jest.fn();
  await publisher.stop();
  publisher.start({ onFailure });
  db.noteGuildInstallation.mockRejectedValueOnce(new Error('DDB unavailable'));
  create(joinA, 8);
  await tick();
  publisher.publish({ op: 0, t: 'INTERACTION_CREATE', s: 9, d: { id: 'i', guild_id: 'g' } });
  await expect(publisher.waitForPublishes()).rejects.toThrow('unconfirmed');
  expect(send).not.toHaveBeenCalled();
  expect(onFailure).toHaveBeenCalledTimes(1);
});
