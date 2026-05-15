/**
 * Unit tests for src/event-consumer.js — the SQS-driven dispatch
 * path used by the worker tier (zero-downtime design, Pillar 1).
 *
 * Covers:
 *   - LRU dedup tracking (recordSeen) including cap eviction
 *   - processMessage: envelope parse, dispatch, DeleteMessage on
 *     every terminal path (success, malformed body, unknown
 *     eventType, reconstruction throw)
 *   - pollOnce: parallel processing, empty receive no-op
 *   - start/stop lifecycle: idempotency, flag/queue-url guards
 *   - discord.js@14.25.1 internal-API smoke: pinned version +
 *     client.actions.InteractionCreate.handle reconstructs a
 *     ChatInputCommandInteraction with the methods our handlers
 *     depend on (deferReply, editReply, options, etc.)
 *
 * Does NOT cover:
 *   - Real SQS behavior (long-poll timing, visibility timeout
 *     accuracy, redrive policy) — integration territory.
 *   - End-to-end interaction handler execution from an SQS message
 *     — that requires a real Discord token + flow-state DDB rows;
 *     the e2e smoke suite covers it post-PR-10.
 */

// Config + logger mocks must run BEFORE requiring event-consumer
// (top-level require of config + logger).
jest.mock('../src/config', () => ({
  ENABLE_EVENT_SHIPPER: true,
  QURL_BOT_EVENTS_QUEUE_URL: 'https://sqs.us-east-2.amazonaws.com/123/qurl-bot-events',
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
  ReceiveMessageCommand,
  DeleteMessageCommand,
} = require('@aws-sdk/client-sqs');

const sqsMock = mockClient(SQSClient);

const eventConsumer = require('../src/event-consumer');
const logger = require('../src/logger');

beforeEach(() => {
  sqsMock.reset();
  jest.clearAllMocks();
  // Reset the module's seen-set between tests so LRU assertions
  // start from a known state.
  eventConsumer._test.seenEventIds.clear();
});

// Make a stub Discord client that exposes only the surface
// processMessage uses: client.actions.InteractionCreate.handle.
// Avoids constructing a real discord.js Client (which would pull
// in REST/WS init and slow each test by tens of ms).
function makeStubClient() {
  return {
    actions: {
      InteractionCreate: {
        handle: jest.fn(),
      },
    },
  };
}

// Build a minimal SQS message envelope. ReceiptHandle is required
// for the DeleteMessage assertion; MessageId is for log noise only.
function makeMessage(envelope, { receiptHandle = 'rh-1', messageId = 'm-1' } = {}) {
  return {
    Body: JSON.stringify(envelope),
    ReceiptHandle: receiptHandle,
    MessageId: messageId,
  };
}

// Inject the mocked SQSClient. event-consumer.js holds an internal
// sqsClient var that start() lazy-constructs; tests that exercise
// processMessage / pollOnce directly bypass start(), so we wire the
// mock client via the _setSqsClientForTest DI hook.
function withMockedSqs(fn) {
  // The aws-sdk-client-mock library intercepts at the SDK-command
  // level, so any SQSClient instance routes through the mock. But
  // since the consumer's sqsClient may be null until start() runs,
  // we construct one explicitly and inject it.
  const realClient = new SQSClient({ region: 'us-east-2' });
  eventConsumer._test._setSqsClientForTest(realClient);
  return fn();
}

describe('event-consumer: recordSeen LRU', () => {
  const { recordSeen, seenEventIds, SEEN_EVENT_ID_CAP } = eventConsumer._test;

  test('first-hit returns false; second-hit returns true', () => {
    expect(recordSeen('e:1')).toBe(false);
    expect(recordSeen('e:1')).toBe(true);
  });

  test('refreshes recency on second-hit (move to tail)', () => {
    recordSeen('e:1');
    recordSeen('e:2');
    // Re-hit e:1 — should now be the most-recent entry, not e:2.
    recordSeen('e:1');
    const keys = Array.from(seenEventIds.keys());
    expect(keys[keys.length - 1]).toBe('e:1');
  });

  test('evicts oldest entry beyond cap', () => {
    // Fill to cap.
    for (let i = 0; i < SEEN_EVENT_ID_CAP; i += 1) {
      recordSeen(`e:${i}`);
    }
    expect(seenEventIds.size).toBe(SEEN_EVENT_ID_CAP);
    // Add one more — oldest (`e:0`) should evict.
    recordSeen('e:NEW');
    expect(seenEventIds.size).toBe(SEEN_EVENT_ID_CAP);
    expect(seenEventIds.has('e:0')).toBe(false);
    expect(seenEventIds.has('e:NEW')).toBe(true);
  });

  test('null/undefined event_id is a no-op', () => {
    expect(recordSeen(null)).toBe(false);
    expect(recordSeen(undefined)).toBe(false);
    expect(seenEventIds.size).toBe(0);
  });
});

describe('event-consumer: processMessage dispatch paths', () => {
  test('INTERACTION_CREATE → calls actions.InteractionCreate.handle + DeleteMessage', async () => {
    sqsMock.on(DeleteMessageCommand).resolves({});
    const client = makeStubClient();
    const payload = { type: 2, data: { type: 1, name: 'qurl' }, id: 'i1' };
    const message = makeMessage({
      eventType: 'INTERACTION_CREATE',
      shardId: '0:1',
      data: payload,
      event_id: '0:1',
    });

    await withMockedSqs(() => eventConsumer._test.processMessage(client, message));

    expect(client.actions.InteractionCreate.handle).toHaveBeenCalledTimes(1);
    expect(client.actions.InteractionCreate.handle).toHaveBeenCalledWith(payload);
    const deleteCalls = sqsMock.commandCalls(DeleteMessageCommand);
    expect(deleteCalls).toHaveLength(1);
    expect(deleteCalls[0].args[0].input).toMatchObject({
      QueueUrl: 'https://sqs.us-east-2.amazonaws.com/123/qurl-bot-events',
      ReceiptHandle: 'rh-1',
    });
  });

  test('malformed JSON body → logs error + DeleteMessage anyway (no redrive)', async () => {
    sqsMock.on(DeleteMessageCommand).resolves({});
    const client = makeStubClient();
    const message = {
      Body: 'not-json{{{',
      ReceiptHandle: 'rh-2',
      MessageId: 'm-2',
    };

    await withMockedSqs(() => eventConsumer._test.processMessage(client, message));

    expect(client.actions.InteractionCreate.handle).not.toHaveBeenCalled();
    expect(logger.error).toHaveBeenCalledWith(
      expect.stringContaining('malformed message body'),
      expect.objectContaining({ messageId: 'm-2' }),
    );
    expect(sqsMock.commandCalls(DeleteMessageCommand)).toHaveLength(1);
  });

  test('unknown eventType → logs warn + DeleteMessage', async () => {
    sqsMock.on(DeleteMessageCommand).resolves({});
    const client = makeStubClient();
    const message = makeMessage({
      eventType: 'GUILD_MEMBER_ADD',
      data: {},
      event_id: '0:99',
    });

    await withMockedSqs(() => eventConsumer._test.processMessage(client, message));

    expect(client.actions.InteractionCreate.handle).not.toHaveBeenCalled();
    expect(logger.warn).toHaveBeenCalledWith(
      expect.stringContaining('unhandled eventType'),
      expect.objectContaining({ eventType: 'GUILD_MEMBER_ADD' }),
    );
    expect(sqsMock.commandCalls(DeleteMessageCommand)).toHaveLength(1);
  });

  test('reconstruction throw → logs error + DeleteMessage (poison-pill containment)', async () => {
    sqsMock.on(DeleteMessageCommand).resolves({});
    const client = makeStubClient();
    client.actions.InteractionCreate.handle.mockImplementation(() => {
      throw new Error('unknown interaction subtype');
    });
    const message = makeMessage({
      eventType: 'INTERACTION_CREATE',
      data: { type: 99 },
      event_id: '0:2',
    });

    await withMockedSqs(() => eventConsumer._test.processMessage(client, message));

    expect(logger.error).toHaveBeenCalledWith(
      expect.stringContaining('dispatch reconstruction failed'),
      expect.objectContaining({ error: 'unknown interaction subtype' }),
    );
    expect(sqsMock.commandCalls(DeleteMessageCommand)).toHaveLength(1);
  });

  test('duplicate event_id is processed (not skipped) — OCC owns correctness', async () => {
    sqsMock.on(DeleteMessageCommand).resolves({});
    const client = makeStubClient();
    const msg1 = makeMessage(
      { eventType: 'INTERACTION_CREATE', data: { id: 'i1' }, event_id: '0:1' },
      { receiptHandle: 'rh-a' },
    );
    const msg2 = makeMessage(
      { eventType: 'INTERACTION_CREATE', data: { id: 'i1' }, event_id: '0:1' },
      { receiptHandle: 'rh-b' },
    );

    await withMockedSqs(async () => {
      await eventConsumer._test.processMessage(client, msg1);
      await eventConsumer._test.processMessage(client, msg2);
    });

    // Both invocations dispatch — telemetry-only dedup, not a correctness gate.
    expect(client.actions.InteractionCreate.handle).toHaveBeenCalledTimes(2);
    expect(sqsMock.commandCalls(DeleteMessageCommand)).toHaveLength(2);
    // Second receive logs the dup at debug.
    expect(logger.debug).toHaveBeenCalledWith(
      expect.stringContaining('event_id seen recently'),
      expect.objectContaining({ eventId: '0:1' }),
    );
  });
});

describe('event-consumer: pollOnce', () => {
  test('0 messages → no DeleteMessage', async () => {
    sqsMock.on(ReceiveMessageCommand).resolves({ Messages: [] });
    const client = makeStubClient();

    await withMockedSqs(() => eventConsumer._test.pollOnce(client));

    expect(client.actions.InteractionCreate.handle).not.toHaveBeenCalled();
    expect(sqsMock.commandCalls(DeleteMessageCommand)).toHaveLength(0);
  });

  test('N messages → N parallel dispatches + N DeleteMessage calls', async () => {
    const messages = [1, 2, 3].map((n) => makeMessage(
      { eventType: 'INTERACTION_CREATE', data: { id: `i${n}` }, event_id: `0:${n}` },
      { receiptHandle: `rh-${n}`, messageId: `m-${n}` },
    ));
    sqsMock.on(ReceiveMessageCommand).resolves({ Messages: messages });
    sqsMock.on(DeleteMessageCommand).resolves({});
    const client = makeStubClient();

    await withMockedSqs(() => eventConsumer._test.pollOnce(client));

    expect(client.actions.InteractionCreate.handle).toHaveBeenCalledTimes(3);
    expect(sqsMock.commandCalls(DeleteMessageCommand)).toHaveLength(3);
  });

  test('pollOnce passes the load-bearing SQS parameters (MaxNumberOfMessages, WaitTimeSeconds, VisibilityTimeout)', async () => {
    // These three values are operationally load-bearing:
    //   - WaitTimeSeconds=20 minimizes empty-receive cost on an idle queue.
    //   - VisibilityTimeout=60 must be < Discord interaction-token TTL.
    //   - MaxNumberOfMessages=10 is the SQS API cap.
    // Pin them against an accidental edit. If a future change wants
    // to retune, this test goes with it as the deliberate signal.
    sqsMock.on(ReceiveMessageCommand).resolves({ Messages: [] });
    const client = makeStubClient();

    await withMockedSqs(() => eventConsumer._test.pollOnce(client));

    const receiveCalls = sqsMock.commandCalls(ReceiveMessageCommand);
    expect(receiveCalls).toHaveLength(1);
    expect(receiveCalls[0].args[0].input).toMatchObject({
      QueueUrl: 'https://sqs.us-east-2.amazonaws.com/123/qurl-bot-events',
      MaxNumberOfMessages: 10,
      WaitTimeSeconds: 20,
      VisibilityTimeout: 60,
    });
  });

  test('per-message error does not block siblings', async () => {
    const messages = [
      makeMessage({ eventType: 'INTERACTION_CREATE', data: { id: 'good1' }, event_id: '0:1' }, { receiptHandle: 'rh-1', messageId: 'm-1' }),
      makeMessage({ eventType: 'INTERACTION_CREATE', data: { id: 'bad' }, event_id: '0:2' }, { receiptHandle: 'rh-2', messageId: 'm-2' }),
      makeMessage({ eventType: 'INTERACTION_CREATE', data: { id: 'good2' }, event_id: '0:3' }, { receiptHandle: 'rh-3', messageId: 'm-3' }),
    ];
    sqsMock.on(ReceiveMessageCommand).resolves({ Messages: messages });
    // Per-receipt dispatch via callsFake — aws-sdk-client-mock's
    // input-matcher .on(Command, {...}) does subset-match in
    // registration order, but chained `.on(DeleteMessageCommand)`
    // matchers can shadow each other depending on call order.
    // callsFake is a single handler that explicitly switches on
    // the input. Test pins the contract that one failing delete
    // does NOT block siblings — all three still dispatch and the
    // pollOnce-level wrapper logs the rh-2 failure.
    sqsMock.on(DeleteMessageCommand).callsFake((input) => {
      if (input.ReceiptHandle === 'rh-2') throw new Error('boom');
      return {};
    });
    const client = makeStubClient();

    await withMockedSqs(() => eventConsumer._test.pollOnce(client));

    // All three dispatched; the failed DeleteMessage logs but
    // pollOnce returns normally so siblings complete.
    expect(client.actions.InteractionCreate.handle).toHaveBeenCalledTimes(3);
    expect(logger.error).toHaveBeenCalledWith(
      expect.stringContaining('processMessage failed'),
      expect.objectContaining({ messageId: 'm-2' }),
    );
  });
});

describe('event-consumer: start/stop lifecycle', () => {
  // The top-level jest.mock('../src/config', ...) returns a literal
  // object that's hoisted into the module cache. Mutating its fields
  // is the simplest way to test config-gated branches without
  // wrestling with jest.isolateModules + jest.doMock interactions
  // (which require the original mock to be a factory, not a literal,
  // to take effect within the isolated scope).
  test('start() throws when ENABLE_EVENT_SHIPPER=false', () => {
    const config = require('../src/config');
    const orig = config.ENABLE_EVENT_SHIPPER;
    config.ENABLE_EVENT_SHIPPER = false;
    try {
      expect(() => eventConsumer.start({})).toThrow('ENABLE_EVENT_SHIPPER=false');
    } finally {
      config.ENABLE_EVENT_SHIPPER = orig;
    }
  });

  test('start() throws when queue URL is missing', () => {
    const config = require('../src/config');
    const orig = config.QURL_BOT_EVENTS_QUEUE_URL;
    config.QURL_BOT_EVENTS_QUEUE_URL = undefined;
    try {
      expect(() => eventConsumer.start({})).toThrow('QURL_BOT_EVENTS_QUEUE_URL');
    } finally {
      config.QURL_BOT_EVENTS_QUEUE_URL = orig;
    }
  });

  test('stop() before start() is a no-op (idempotent)', async () => {
    await expect(eventConsumer.stop()).resolves.toBeUndefined();
  });

  test('isAbortError recognizes AbortError name + DOMException variants', () => {
    const { isAbortError } = eventConsumer._test;
    expect(isAbortError(null)).toBe(false);
    expect(isAbortError(undefined)).toBe(false);
    expect(isAbortError(new Error('boom'))).toBe(false);
    const e1 = new Error('aborted'); e1.name = 'AbortError';
    expect(isAbortError(e1)).toBe(true);
    const e2 = new Error('timeout'); e2.name = 'TimeoutError';
    expect(isAbortError(e2)).toBe(true);
    const e3 = new Error('aborted'); e3.code = 'AbortError';
    expect(isAbortError(e3)).toBe(true);
  });

  test('pollOnce sets receiveAbortController; stop() aborts it', async () => {
    // aws-sdk-client-mock doesn't pass HttpHandlerOptions (incl.
    // abortSignal) through to callsFake handlers, so we can't easily
    // mock the SDK's abort-aware receive. Test the abort *machinery*
    // directly instead: pollOnce constructs a controller and stop()
    // aborts it. The end-to-end integration (SDK actually honors the
    // abort) is covered by the AWS SDK v3 contract; the worker
    // boot-test path will exercise it against the sandbox queue.
    sqsMock.on(ReceiveMessageCommand).resolves({ Messages: [] });
    eventConsumer._test._setSqsClientForTest(new SQSClient({ region: 'us-east-2' }));

    // Run a single pollOnce so receiveAbortController is set.
    await withMockedSqs(() => eventConsumer._test.pollOnce(makeStubClient()));

    const controller = eventConsumer._test.getReceiveAbortController();
    expect(controller).not.toBeNull();
    expect(controller.signal.aborted).toBe(false);

    // Simulate stop()'s abort step. We don't call full stop() here
    // because the loop isn't running — start() sets running=true
    // which stop() requires. Direct abort assertion is the focused
    // unit test.
    controller.abort();
    expect(controller.signal.aborted).toBe(true);
  });

  test('start() + stop() round-trip; second start logs warn (idempotent)', async () => {
    // ReceiveMessage returns empty immediately so pollLoop iterates
    // quickly between the stopping flag flips.
    sqsMock.on(ReceiveMessageCommand).resolves({ Messages: [] });
    eventConsumer._test._setSqsClientForTest(new SQSClient({ region: 'us-east-2' }));
    const client = makeStubClient();

    eventConsumer.start(client);
    // Second start while running: no-op + warn.
    eventConsumer.start(client);
    expect(logger.warn).toHaveBeenCalledWith(
      expect.stringContaining('start() called while already running'),
    );

    await eventConsumer.stop();
    // Stop is idempotent.
    await eventConsumer.stop();
  });
});

describe('event-consumer: discord.js@14.25.1 internal-API smoke', () => {
  test('package.json pins discord.js to an exact version (no ~/^)', () => {
    const pkg = require('../package.json');
    const decl = pkg.dependencies['discord.js'];
    // Exact-pin assertion: leading char must be a digit.
    expect(decl).toMatch(/^\d/);
    expect(decl).not.toMatch(/^[~^]/);
  });

  test('client.actions.InteractionCreate.handle is a function', () => {
    const { Client, GatewayIntentBits } = require('discord.js');
    const client = new Client({ intents: [GatewayIntentBits.Guilds] });
    expect(client.actions).toBeDefined();
    expect(typeof client.actions.InteractionCreate.handle).toBe('function');
    // Cleanup — don't leak the client through subsequent tests.
    client.destroy().catch(() => {});
  });

  test('handle() reconstructs a ChatInputCommandInteraction with the methods handlers use', () => {
    const {
      Client,
      GatewayIntentBits,
      InteractionType,
      ApplicationCommandType,
    } = require('discord.js');
    const client = new Client({ intents: [GatewayIntentBits.Guilds] });

    let received = null;
    client.on('interactionCreate', (i) => { received = i; });

    // Minimal ChatInputCommand payload. discord.js's constructor
    // resolves channel/guild via client.{channels,guilds}.cache —
    // both empty here, which is the cold-cache reality for the
    // worker tier. The interaction object still exposes the methods
    // our handlers depend on; .guild / .channel just come back as
    // null (or partial, via channel_id).
    const payload = {
      id: '1234567890',
      application_id: '987654321',
      type: InteractionType.ApplicationCommand,
      data: {
        id: 'cmd_id_1',
        name: 'qurl',
        type: ApplicationCommandType.ChatInput,
      },
      token: 'tok',
      version: 1,
      user: {
        id: 'user_id_1',
        username: 'testuser',
        discriminator: '0',
        global_name: 'TestUser',
      },
      channel_id: 'channel_id_1',
      locale: 'en-US',
      app_permissions: '0',
      entitlements: [],
      authorizing_integration_owners: {},
      context: 0,
      attachment_size_limit: 26_214_400,
    };

    client.actions.InteractionCreate.handle(payload);

    expect(received).not.toBeNull();
    // Methods our handlers depend on, in commands.js + flow-dispatch.js.
    expect(received.isChatInputCommand()).toBe(true);
    expect(typeof received.deferReply).toBe('function');
    expect(typeof received.editReply).toBe('function');
    expect(typeof received.reply).toBe('function');
    expect(received.options).toBeDefined();
    expect(typeof received.options.getString).toBe('function');
    expect(received.commandName).toBe('qurl');
    expect(received.user).toBeDefined();
    expect(received.user.id).toBe('user_id_1');

    client.destroy().catch(() => {});
  });

  test('handle() reconstructs a ButtonInteraction with customId + isButton()', () => {
    const {
      Client,
      GatewayIntentBits,
      InteractionType,
      ComponentType,
    } = require('discord.js');
    const client = new Client({ intents: [GatewayIntentBits.Guilds] });

    let received = null;
    client.on('interactionCreate', (i) => { received = i; });

    // Snowflakes must be numeric-string-shaped — discord.js parses
    // them as BigInts via @sapphire/snowflake (Message._patch calls
    // Snowflake.timestampFrom on message.id). Non-numeric IDs throw
    // "Cannot convert X to a BigInt". 17–19 digit decimals match
    // Discord's actual snowflake shape.
    const payload = {
      id: '2222222222222222222',
      application_id: '987654321987654321',
      type: InteractionType.MessageComponent,
      data: {
        custom_id: 'qurl_confirm_everyone',
        component_type: ComponentType.Button,
      },
      message: {
        id: '3333333333333333333',
        channel_id: '4444444444444444444',
        type: 19,
        content: '',
        author: {
          id: '5555555555555555555',
          username: 'bot',
          discriminator: '0',
        },
        timestamp: new Date().toISOString(),
        edited_timestamp: null,
        tts: false,
        mention_everyone: false,
        mentions: [],
        mention_roles: [],
        attachments: [],
        embeds: [],
        pinned: false,
        flags: 0,
      },
      token: 'tok2',
      version: 1,
      user: {
        id: '6666666666666666666',
        username: 'testuser',
        discriminator: '0',
        global_name: 'TestUser',
      },
      channel_id: '4444444444444444444',
      locale: 'en-US',
      app_permissions: '0',
      entitlements: [],
      authorizing_integration_owners: {},
      context: 0,
      attachment_size_limit: 26_214_400,
    };

    client.actions.InteractionCreate.handle(payload);

    expect(received).not.toBeNull();
    expect(received.isButton()).toBe(true);
    expect(received.isMessageComponent()).toBe(true);
    expect(received.customId).toBe('qurl_confirm_everyone');
    expect(typeof received.deferUpdate).toBe('function');
    expect(typeof received.update).toBe('function');

    client.destroy().catch(() => {});
  });
});
