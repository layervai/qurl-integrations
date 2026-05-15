/**
 * Integration test: producer → SQS envelope → consumer round-trip.
 *
 * The unit tests in tests/event-publisher.test.js and
 * tests/event-consumer.test.js cover each side in isolation. They
 * do NOT catch envelope-field drift: if the producer emits
 * `eventType: 'INTERACTION_CREATE'` and the consumer reads
 * `parsed.event_type`, both unit suites pass and only sandbox soak
 * surfaces the mismatch. This file plugs that gap by wiring the two
 * sides together against an in-memory SQS substitute
 * (aws-sdk-client-mock) and asserting the consumer dispatches the
 * exact payload the producer published.
 *
 * What this test does NOT cover (still requires sandbox soak / a
 * real queue):
 *   - SQS Standard's at-least-once delivery semantics
 *   - Long-poll timing and visibility-timeout accuracy
 *   - Discord's real interaction-token TTL behavior
 *   - Cross-host clock skew driving the e2e latency metric
 *
 * Both mocks share one queue URL string so the assertions stay
 * semantically coupled (a producer-side QueueUrl change without a
 * consumer-side change would surface as a test failure here).
 */

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

// Inject the same SQSClient into both modules so SendMessage and the
// downstream processMessage(...) route through the shared mock.
function withMockedSqs(fn) {
  const client = new SQSClient({ region: 'us-east-2' });
  eventPublisher._test._setSqsClientForTest(client);
  eventConsumer._test._setSqsClientForTest(client);
  return fn();
}

function makeRawInteractionPacket({ sequence = 1, data } = {}) {
  // discord.js's `raw` event delivers GatewayDispatchPayload-shaped
  // packets: { op: 0, t: <type>, s: <sequence>, d: <payload> }.
  return {
    op: 0,
    t: 'INTERACTION_CREATE',
    s: sequence,
    d: data,
  };
}

function makeStubDiscordClient() {
  // Mirrors tests/event-consumer.test.js's stub — only the surface
  // the consumer reaches into (client.actions.InteractionCreate.handle).
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

    // Capture exactly what the publisher emitted on the wire.
    const sends = sqsMock.commandCalls(SendMessageCommand);
    expect(sends).toHaveLength(1);
    const sentInput = sends[0].args[0].input;
    expect(sentInput.QueueUrl).toBe(TEST_QUEUE_URL);

    // Feed the publisher's body to the consumer as if SQS delivered
    // it. Pins the wire contract: a field-name change on either side
    // (eventType ↔ event_type, data ↔ payload, etc.) breaks here.
    const consumerMessage = {
      Body: sentInput.MessageBody,
      ReceiptHandle: 'rh-integration-1',
      MessageId: 'sqs-integration-1',
    };
    const client = makeStubDiscordClient();
    await withMockedSqs(() => eventConsumer._test.processMessage(client, consumerMessage));

    // Consumer dispatched with EXACTLY the data the producer sent.
    expect(client.actions.InteractionCreate.handle).toHaveBeenCalledTimes(1);
    expect(client.actions.InteractionCreate.handle).toHaveBeenCalledWith(interactionData);

    // And it deleted the message (DeleteMessage is the consumer's
    // success terminal).
    const deletes = sqsMock.commandCalls(DeleteMessageCommand);
    expect(deletes).toHaveLength(1);
    expect(deletes[0].args[0].input).toMatchObject({
      QueueUrl: TEST_QUEUE_URL,
      ReceiptHandle: 'rh-integration-1',
    });
  });

  test('event_id format matches consumer LRU dedup expectations', async () => {
    // Producer emits `${SHARD_ID}:${packet.s}`; consumer's recordSeen
    // treats that as an opaque string key. Pin that producer's shape
    // populates the LRU correctly — a future producer-side reformat
    // would silently break dup detection without this test.
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

    // Round-trip the SAME envelope through the consumer twice.
    // First processMessage populates the LRU; second should observe
    // the dup. Verifies the producer's event_id key is the SAME shape
    // the consumer's recordSeen expects.
    const consumerMessage = {
      Body: JSON.stringify(body),
      ReceiptHandle: 'rh-dup-1',
      MessageId: 'm-dup-1',
    };
    const client = makeStubDiscordClient();
    await withMockedSqs(() => eventConsumer._test.processMessage(client, consumerMessage));
    expect(eventConsumer._test.seenEventIds.has('0:7777777')).toBe(true);

    // Now re-process the same envelope — recordSeen returns true →
    // the dup-debug log fires (consumer module-level behavior).
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
    // Pin the e2e-metric wire contract end-to-end. A producer change
    // that renames published_at_ms or stops setting it would surface
    // here as a missing structured-log field on the consumer side.
    sqsMock.on(SendMessageCommand).resolves({ MessageId: 'sqs-e2e' });
    sqsMock.on(DeleteMessageCommand).resolves({});

    eventPublisher.start();
    // Fix Date.now so the e2e delta is predictable across the
    // producer-call and consumer-call observations of the wall clock.
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

      // Advance the worker-side clock by 42 ms before processMessage
      // reads Date.now() to compute the e2e delta.
      dateSpy.mockReturnValue(fixedNow + 42);
      const client = makeStubDiscordClient();
      await withMockedSqs(() => eventConsumer._test.processMessage(client, {
        Body: JSON.stringify(body),
        ReceiptHandle: 'rh-e2e',
        MessageId: 'sqs-e2e',
      }));

      const logger = require('../src/logger');
      expect(logger.info).toHaveBeenCalledWith(
        expect.stringContaining('dispatched'),
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
    // Negative contract: ensures the consumer side wouldn't even
    // SEE a HEARTBEAT_ACK / PRESENCE_UPDATE because the producer
    // filters before SendMessage. Belt-and-suspenders against a
    // future relaxation of the producer filter — the consumer's
    // unhandled-eventType branch would still log+delete, but that's
    // wasted SQS traffic the round-trip should never carry.
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
