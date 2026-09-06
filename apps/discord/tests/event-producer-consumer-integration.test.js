
const TEST_QUEUE_URL = 'https://sqs.us-east-2.amazonaws.com/123/qurl-bot-events-integration';

jest.mock('../src/config', () => ({
  ENABLE_EVENT_SHIPPER: true,
  QURL_BOT_EVENTS_QUEUE_URL: TEST_QUEUE_URL,
  QURL_BOT_MAX_INFLIGHT_HANDLERS: 100,
  QURL_BOT_DRAIN_DEADLINE_MS: 3000,
}));

jest.mock('../src/logger', () => ({
  info: jest.fn(),
  warn: jest.fn(),
  error: jest.fn(),
  debug: jest.fn(),
  audit: jest.fn(),
}));

const { mockClient } = require('aws-sdk-client-mock');
const {
  SQSClient,
  SendMessageCommand,
  DeleteMessageCommand,
} = require('@aws-sdk/client-sqs');

const sqsMock = mockClient(SQSClient);

const eventPublisher = require('../src/event-publisher');
const eventConsumer = require('../src/event-consumer');

beforeEach(() => {
  sqsMock.reset();
  jest.clearAllMocks();
  eventPublisher._test._resetStateForTest();
  eventConsumer._test._resetStateForTest();
});

function withMockedSqs(fn) {
  const client = new SQSClient({ region: 'us-east-2' });
  eventPublisher._test._setSqsClientForTest(client);
  eventConsumer._test._setSqsClientForTest(client);
  return fn();
}

function makeRawInteractionPacket({ sequence = 1, data } = {}) {
  return {
    op: 0,
    t: 'INTERACTION_CREATE',
    s: sequence,
    d: data,
  };
}

function makeStubDiscordClient() {
  return {
    actions: {
      InteractionCreate: {
        handle: jest.fn(),
      },
    },
  };
}

function flushMicro() {
  return new Promise((resolve) => setImmediate(resolve));
}

describe('producer → consumer envelope round-trip', () => {
  test('publisher SendMessage body parses cleanly into consumer dispatch with identical data', async () => {
    sqsMock.on(SendMessageCommand).resolves({ MessageId: 'sqs-integration-1' });
    sqsMock.on(DeleteMessageCommand).resolves({});

    const interactionData = {
      type: 2,
      data: { type: 1, name: 'qurl', options: [{ name: 'file', type: 1 }] },
      id: 'i-integration-1',
      token: 'opaque-token',
      guild_id: '111',
      channel_id: '222',
      member: { user: { id: '333' }, permissions: '8' },
      version: 1,
    };

    eventPublisher.start();
    withMockedSqs(() => {
      eventPublisher.publish(makeRawInteractionPacket({ sequence: 42, data: interactionData }));
    });
    await flushMicro();

    const sends = sqsMock.commandCalls(SendMessageCommand);
    expect(sends).toHaveLength(1);
    const sentInput = sends[0].args[0].input;
    expect(sentInput.QueueUrl).toBe(TEST_QUEUE_URL);

    const consumerMessage = {
      Body: sentInput.MessageBody,
      ReceiptHandle: 'rh-integration-1',
      MessageId: 'sqs-integration-1',
    };
    const client = makeStubDiscordClient();
    await withMockedSqs(() => eventConsumer._test.processMessage(client, consumerMessage));

    expect(client.actions.InteractionCreate.handle).toHaveBeenCalledTimes(1);
    expect(client.actions.InteractionCreate.handle).toHaveBeenCalledWith(interactionData);

    const deletes = sqsMock.commandCalls(DeleteMessageCommand);
    expect(deletes).toHaveLength(1);
    expect(deletes[0].args[0].input).toMatchObject({
      QueueUrl: TEST_QUEUE_URL,
      ReceiptHandle: 'rh-integration-1',
    });
  });

  test('event_id format matches consumer LRU dedup expectations', async () => {
    sqsMock.on(SendMessageCommand).resolves({ MessageId: 'sqs-event-id' });
    sqsMock.on(DeleteMessageCommand).resolves({});

    eventPublisher.start();
    withMockedSqs(() => {
      eventPublisher.publish(makeRawInteractionPacket({
        sequence: 7777777,
        data: { type: 2, data: { name: 'qurl' }, id: 'i-eid' },
      }));
    });
    await flushMicro();

    const body = JSON.parse(sqsMock.commandCalls(SendMessageCommand)[0].args[0].input.MessageBody);
    expect(body.event_id).toBe('0:7777777');

    const consumerMessage = {
      Body: JSON.stringify(body),
      ReceiptHandle: 'rh-dup-1',
      MessageId: 'm-dup-1',
    };
    const client = makeStubDiscordClient();
    await withMockedSqs(() => eventConsumer._test.processMessage(client, consumerMessage));
    expect(eventConsumer._test.seenEventIds.has('0:7777777')).toBe(true);

    const dupMessage = {
      Body: JSON.stringify(body),
      ReceiptHandle: 'rh-dup-2',
      MessageId: 'm-dup-2',
    };
    await withMockedSqs(() => eventConsumer._test.processMessage(client, dupMessage));
    const logger = require('../src/logger');
    expect(logger.debug).toHaveBeenCalledWith(
      expect.stringContaining('event_id seen recently'),
      expect.objectContaining({ eventId: '0:7777777' }),
    );
  });

  test('producer published_at_ms drives consumer e2e latency log', async () => {
    sqsMock.on(SendMessageCommand).resolves({ MessageId: 'sqs-e2e' });
    sqsMock.on(DeleteMessageCommand).resolves({});

    eventPublisher.start();
    const fixedNow = 1_700_000_000_000;
    const dateSpy = jest.spyOn(Date, 'now').mockReturnValue(fixedNow);
    try {
      withMockedSqs(() => {
        eventPublisher.publish(makeRawInteractionPacket({
          sequence: 99,
          data: { type: 2, data: { name: 'qurl' }, id: 'i-e2e' },
        }));
      });
      await flushMicro();

      const body = JSON.parse(sqsMock.commandCalls(SendMessageCommand)[0].args[0].input.MessageBody);
      expect(body.published_at_ms).toBe(fixedNow);

      dateSpy.mockReturnValue(fixedNow + 42);
      const client = makeStubDiscordClient();
      await withMockedSqs(() => eventConsumer._test.processMessage(client, {
        Body: JSON.stringify(body),
        ReceiptHandle: 'rh-e2e',
        MessageId: 'sqs-e2e',
      }));

      const logger = require('../src/logger');
      expect(logger.info).toHaveBeenCalledWith(
        expect.stringContaining('received'),
        expect.objectContaining({
          qurl_bot_event_e2e_ms: 42,
          eventId: '0:99',
          shardId: '0',
        }),
      );
    } finally {
      dateSpy.mockRestore();
    }
  });

  test('non-INTERACTION_CREATE dispatches from producer never reach consumer', async () => {
    sqsMock.on(SendMessageCommand).resolves({ MessageId: 'never-fires' });
    eventPublisher.start();
    withMockedSqs(() => {
      eventPublisher.publish({ op: 11, t: null, s: null, d: null }); // HEARTBEAT_ACK
      eventPublisher.publish({ op: 0, t: 'GUILD_CREATE', s: 1, d: {} });
      eventPublisher.publish({ op: 0, t: 'MESSAGE_CREATE', s: 2, d: {} });
      eventPublisher.publish({ op: 0, t: 'PRESENCE_UPDATE', s: 3, d: {} });
    });
    await flushMicro();
    expect(sqsMock.commandCalls(SendMessageCommand)).toHaveLength(0);
  });
});
