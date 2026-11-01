jest.mock('../src/config', () => ({
  ENABLE_EVENT_SHIPPER: true, ENABLE_DISPATCH_PROBE: true,
  QURL_BOT_EVENTS_QUEUE_URL: 'https://sqs.us-east-2.amazonaws.com/123/fixture-events',
  QURL_BOT_MAX_INFLIGHT_HANDLERS: 100, QURL_BOT_DRAIN_DEADLINE_MS: 3000,
}));
jest.mock('../src/logger', () => ({ info: jest.fn(), warn: jest.fn(), error: jest.fn(), debug: jest.fn(), audit: jest.fn() }));
jest.mock('../src/dispatch-probe', () => ({ ...jest.requireActual('../src/dispatch-probe'), getRuntimeProbeStore: jest.fn() }));
const { Client, GatewayIntentBits } = require('discord.js');
const { mockClient } = require('aws-sdk-client-mock');
const { SQSClient, SendMessageCommand, DeleteMessageCommand, ReceiveMessageCommand } = require('@aws-sdk/client-sqs');
const probes = require('../src/dispatch-probe');
const publisher = require('../src/event-publisher');
const consumer = require('../src/event-consumer');
const sqs = mockClient(SQSClient);
const originalRegion = process.env.AWS_REGION;
beforeEach(() => {
  process.env.AWS_REGION = 'us-east-2'; sqs.reset();
  publisher._test._resetStateForTest(); consumer._test._resetStateForTest();
});
afterEach(() => jest.restoreAllMocks());
afterAll(() => { if (originalRegion === undefined) delete process.env.AWS_REGION; else process.env.AWS_REGION = originalRegion; });

test('actual publisher envelope crosses actual consumer and real SDK before completion', async () => {
  let time = 0, row;
  jest.spyOn(Date, 'now').mockImplementation(() => time);
  const store = {
    prepare: async data => { row = { ...data }; }, read: jest.fn(async () => row),
    acknowledge: jest.fn(async (_origin, nonce) => { if (nonce === row.nonce) row.ack_at = time; return true; }),
  };
  probes.getRuntimeProbeStore.mockReturnValue(store);
  const client = new Client({ intents: [GatewayIntentBits.Guilds, GatewayIntentBits.GuildMembers] });
  const guildId = '123456789012345678', userId = '234567890123456789';
  client.guilds._add({ id: guildId, name: 'fixture', unavailable: false });
  const logger = { audit: jest.fn() };
  const probe = probes.createDispatchProbe({ store, client, logger, now: () => time,
    transport: { isReady: () => true, getTarget: async () => ({ guildId, userId }), request: async () => {} },
    publish: publisher.publishDispatchProbe,
  });
  sqs.on(SendMessageCommand).resolves({ MessageId: 'fixture' }); sqs.on(DeleteMessageCommand).resolves({});
  publisher.start();
  publisher._test._setSqsClientForTest(new SQSClient({ region: 'us-east-2' }));
  consumer._test._setSqsClientForTest(new SQSClient({ region: 'us-east-2' }));
  time = 60_000; await probe.tick();
  const packet = { op: 0, t: 'GUILD_MEMBERS_CHUNK', s: 44, d: {
    guild_id: guildId, nonce: row.nonce, chunk_index: 0, chunk_count: 1,
    members: [{ user: { id: userId, username: 'fixture', discriminator: '0', bot: true }, roles: [] }],
  } };
  // Ambient member chunks stay off the queue, including discord.js raw's shard argument.
  publisher.publish(packet, 7);
  expect(sqs.commandCalls(SendMessageCommand)).toHaveLength(0);
  probe.capture(packet);
  await publisher.stop();
  const body = sqs.commandCalls(SendMessageCommand)[0].args[0].input.MessageBody;
  const envelope = JSON.parse(body);
  expect(envelope).toMatchObject({ eventType: 'GUILD_MEMBERS_CHUNK', probe_origin: row.origin, data: packet.d });
  let observed = false; client.on('guildMembersChunk', () => { observed = true; });
  await consumer._test.processMessage(client, { Body: body, ReceiptHandle: 'fixture-receipt' });
  expect(observed).toBe(true); expect(store.acknowledge).toHaveBeenCalledTimes(1);
  expect(sqs.commandCalls(DeleteMessageCommand)).toHaveLength(1);
  await probe.tick(); expect(logger.audit).toHaveBeenCalledWith('gateway_dispatch_probe_ok', { latency_ms: 0 });
  // A storage outage is not terminal: leave the SQS message for redelivery.
  store.read.mockRejectedValueOnce(new Error('fixture DDB unavailable'));
  sqs.on(ReceiveMessageCommand).resolves({ Messages: [{ Body: body, ReceiptHandle: 'retry-receipt' }] });
  consumer._test._setStopControllerForTest();
  await consumer._test.pollOnce(client);
  expect(require('../src/logger').error).toHaveBeenCalledWith(
    expect.stringContaining('processing or deletion failed'), expect.objectContaining({ error: 'fixture DDB unavailable' }),
  );
  expect(sqs.commandCalls(DeleteMessageCommand)).toHaveLength(1);
  probe.stop(); await client.destroy();
});

test('worker flag off deletes an unknown probe envelope without processing it', async () => {
  const config = require('../src/config'); config.ENABLE_DISPATCH_PROBE = false;
  try {
    sqs.on(DeleteMessageCommand).resolves({});
    consumer._test._setSqsClientForTest(new SQSClient({ region: 'us-east-2' }));
    probes.getRuntimeProbeStore.mockClear();
    await consumer._test.processMessage({}, { Body: JSON.stringify({ eventType: 'GUILD_MEMBERS_CHUNK' }), ReceiptHandle: 'old-worker' });
    expect(probes.getRuntimeProbeStore).not.toHaveBeenCalled();
    expect(sqs.commandCalls(DeleteMessageCommand)).toHaveLength(1);
  } finally { config.ENABLE_DISPATCH_PROBE = true; }
});
