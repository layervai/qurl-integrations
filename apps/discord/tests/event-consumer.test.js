
jest.mock('../src/config', () => ({
  ENABLE_EVENT_SHIPPER: true,
  QURL_BOT_EVENTS_QUEUE_URL: 'https://sqs.us-east-2.amazonaws.com/123/qurl-bot-events',
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
  ReceiveMessageCommand,
  DeleteMessageCommand,
} = require('@aws-sdk/client-sqs');

const sqsMock = mockClient(SQSClient);

const eventConsumer = require('../src/event-consumer');
const logger = require('../src/logger');

beforeEach(() => {
  sqsMock.reset();
  jest.clearAllMocks();
  eventConsumer._test._resetStateForTest();
});

function makeStubClient() {
  return {
    actions: {
      InteractionCreate: {
        handle: jest.fn(),
      },
    },
  };
}

function makeMessage(envelope, { receiptHandle = 'rh-1', messageId = 'm-1' } = {}) {
  return {
    Body: JSON.stringify(envelope),
    ReceiptHandle: receiptHandle,
    MessageId: messageId,
  };
}

function withMockedSqs(fn) {
  const realClient = new SQSClient({ region: 'us-east-2' });
  eventConsumer._test._setSqsClientForTest(realClient);
  if (!eventConsumer._test.getStopController()) {
    eventConsumer._test._setStopControllerForTest();
  }
  return fn();
}

function withWorkerDispatch(fn) {
  eventConsumer._test._setWorkerDispatchingForTest(true);
  try {
    return fn();
  } finally {
    eventConsumer._test._setWorkerDispatchingForTest(false);
  }
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
    recordSeen('e:1');
    const keys = Array.from(seenEventIds.keys());
    expect(keys[keys.length - 1]).toBe('e:1');
  });

  test('evicts oldest entry beyond cap', () => {
    for (let i = 0; i < SEEN_EVENT_ID_CAP; i += 1) {
      recordSeen(`e:${i}`);
    }
    expect(seenEventIds.size).toBe(SEEN_EVENT_ID_CAP);
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

  test('whitespace-only event_id is rejected (does NOT pollute LRU)', () => {
    expect(recordSeen(' ')).toBe(false);
    expect(recordSeen('   ')).toBe(false);
    expect(recordSeen('\t')).toBe(false);
    expect(recordSeen('')).toBe(false);
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

  test.each([
    ['null', 'null'],
    ['number', '42'],
    ['string', '"hello"'],
    ['array', '[1,2,3]'],
  ])('non-object envelope (%s) → logs error + DeleteMessage', async (label, body) => {
    sqsMock.on(DeleteMessageCommand).resolves({});
    const client = makeStubClient();
    const message = { Body: body, ReceiptHandle: `rh-${label}`, MessageId: `m-${label}` };

    await withMockedSqs(() => eventConsumer._test.processMessage(client, message));

    expect(client.actions.InteractionCreate.handle).not.toHaveBeenCalled();
    expect(logger.error).toHaveBeenCalledWith(
      expect.stringContaining('envelope is not a JSON object'),
      expect.objectContaining({ messageId: `m-${label}` }),
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

  test('seenEventIds IS populated when dispatch reconstruction throws', async () => {
    sqsMock.on(DeleteMessageCommand).resolves({});
    const client = makeStubClient();
    client.actions.InteractionCreate.handle.mockImplementation(() => {
      throw new Error('unknown subtype');
    });
    const seen = eventConsumer._test.seenEventIds;

    await withMockedSqs(() => eventConsumer._test.processMessage(client, makeMessage({
      eventType: 'INTERACTION_CREATE',
      data: { type: 99 },
      event_id: '0:throw',
    })));

    expect(seen.has('0:throw')).toBe(true);

    await withMockedSqs(() => eventConsumer._test.processMessage(client, makeMessage(
      { eventType: 'INTERACTION_CREATE', data: { type: 99 }, event_id: '0:throw' },
      { receiptHandle: 'rh-dup' },
    )));

    expect(logger.debug).toHaveBeenCalledWith(
      expect.stringContaining('event_id seen recently'),
      expect.objectContaining({ eventId: '0:throw', dispatchOk: false }),
    );
  });

  test('dup-debug log carries dispatchOk: true on the success-after-dup path', async () => {
    sqsMock.on(DeleteMessageCommand).resolves({});
    const client = makeStubClient();
    const ok = { eventType: 'INTERACTION_CREATE', data: { id: 'i' }, event_id: '0:ok' };
    await withMockedSqs(async () => {
      await eventConsumer._test.processMessage(client, makeMessage(ok, { receiptHandle: 'rh-a' }));
      await eventConsumer._test.processMessage(client, makeMessage(ok, { receiptHandle: 'rh-b' }));
    });
    expect(logger.debug).toHaveBeenCalledWith(
      expect.stringContaining('event_id seen recently'),
      expect.objectContaining({ eventId: '0:ok', dispatchOk: true }),
    );
  });

  test('missing event_id: warn rate-limited to once per cooldown window, then re-armed', async () => {
    sqsMock.on(DeleteMessageCommand).resolves({});
    const client = makeStubClient();
    const message1 = makeMessage(
      { eventType: 'INTERACTION_CREATE', data: { id: 'no-eid-1' } /* event_id missing */ },
      { messageId: 'm-no-eid-1' },
    );
    const message2 = makeMessage(
      { eventType: 'INTERACTION_CREATE', data: { id: 'no-eid-2' } },
      { messageId: 'm-no-eid-2', receiptHandle: 'rh-2' },
    );

    await withMockedSqs(async () => {
      await eventConsumer._test.processMessage(client, message1);
      await eventConsumer._test.processMessage(client, message2);
    });

    expect(client.actions.InteractionCreate.handle).toHaveBeenCalledTimes(2);
    expect(logger.warn).toHaveBeenCalledWith(
      expect.stringContaining('envelope missing valid event_id'),
      expect.objectContaining({ messageId: 'm-no-eid-1', eventType: 'INTERACTION_CREATE', eventIdType: 'undefined' }),
    );
    expect(logger.debug).toHaveBeenCalledWith(
      expect.stringContaining('envelope missing valid event_id'),
      expect.objectContaining({ messageId: 'm-no-eid-2' }),
    );
    const warnCalls = logger.warn.mock.calls.filter(
      ([msg]) => typeof msg === 'string' && msg.includes('envelope missing valid event_id'),
    );
    expect(warnCalls).toHaveLength(1);

    const dateSpy = jest.spyOn(Date, 'now').mockReturnValue(Date.now() + 60 * 60 * 1000 + 1);
    try {
      const message3 = makeMessage(
        { eventType: 'INTERACTION_CREATE', data: { id: 'no-eid-3' } },
        { messageId: 'm-no-eid-3', receiptHandle: 'rh-3' },
      );
      await withMockedSqs(() => eventConsumer._test.processMessage(client, message3));
      const reArmedWarns = logger.warn.mock.calls.filter(
        ([msg]) => typeof msg === 'string' && msg.includes('envelope missing valid event_id'),
      );
      expect(reArmedWarns).toHaveLength(2);
    } finally {
      dateSpy.mockRestore();
    }
  });

  test('envelope with published_at_ms → logs qurl_bot_event_e2e_ms', async () => {
    sqsMock.on(DeleteMessageCommand).resolves({});
    const client = makeStubClient();
    const fixedNow = 1_000_000_000;
    const dateSpy = jest.spyOn(Date, 'now').mockReturnValue(fixedNow);
    try {
      const message = makeMessage({
        eventType: 'INTERACTION_CREATE',
        shardId: '0',
        data: { type: 2, data: { name: 'qurl' }, id: 'i-e2e' },
        event_id: '0:42',
        published_at_ms: fixedNow - 25,
      });
      await withMockedSqs(() => eventConsumer._test.processMessage(client, message));
      expect(logger.info).toHaveBeenCalledWith(
        expect.stringContaining('received'),
        expect.objectContaining({
          qurl_bot_event_e2e_ms: 25,
          eventId: '0:42',
          shardId: '0',
        }),
      );
    } finally {
      dateSpy.mockRestore();
    }
  });

  test('envelope missing published_at_ms → logs debug + skips e2e metric', async () => {
    sqsMock.on(DeleteMessageCommand).resolves({});
    const client = makeStubClient();
    const message = makeMessage({
      eventType: 'INTERACTION_CREATE',
      shardId: '0',
      data: { type: 2, data: { name: 'qurl' }, id: 'i-no-ts' },
      event_id: '0:43',
    });
    await withMockedSqs(() => eventConsumer._test.processMessage(client, message));
    expect(logger.debug).toHaveBeenCalledWith(
      expect.stringContaining('missing published_at_ms'),
      expect.objectContaining({ publishedAtMsType: 'undefined' }),
    );
    expect(logger.info).not.toHaveBeenCalledWith(
      expect.stringContaining('received'),
      expect.objectContaining({ qurl_bot_event_e2e_ms: expect.anything() }),
    );
  });

  test.each([
    ['string', '1700000000000'],
    ['null', null],
  ])('envelope with non-number published_at_ms (%s) → debug skip, no e2e log', async (label, value) => {
    sqsMock.on(DeleteMessageCommand).resolves({});
    const client = makeStubClient();
    const message = makeMessage({
      eventType: 'INTERACTION_CREATE',
      shardId: '0',
      data: { type: 2, data: { name: 'qurl' }, id: `i-${label}` },
      event_id: `0:${label}`,
      published_at_ms: value,
    });
    await withMockedSqs(() => eventConsumer._test.processMessage(client, message));
    expect(logger.info).not.toHaveBeenCalledWith(
      expect.stringContaining('received'),
      expect.objectContaining({ qurl_bot_event_e2e_ms: expect.anything() }),
    );
  });

  test('oversized message body short-circuits before JSON.parse + still deletes', async () => {
    sqsMock.on(DeleteMessageCommand).resolves({});
    const client = makeStubClient();
    const message = {
      Body: 'x'.repeat(250 * 1024), // 250 KB > 200 KB cap (ASCII: bytes == chars)
      ReceiptHandle: 'rh-oversize',
      MessageId: 'm-oversize',
    };

    await withMockedSqs(() => eventConsumer._test.processMessage(client, message));

    expect(client.actions.InteractionCreate.handle).not.toHaveBeenCalled();
    expect(logger.error).toHaveBeenCalledWith(
      expect.stringContaining('message body exceeds size cap'),
      expect.objectContaining({ messageId: 'm-oversize', cap: expect.any(Number) }),
    );
    expect(sqsMock.commandCalls(DeleteMessageCommand)).toHaveLength(1);
  });

  test('multi-byte payload measured in bytes, not chars (caps correctly)', async () => {
    sqsMock.on(DeleteMessageCommand).resolves({});
    const client = makeStubClient();
    const message = {
      Body: '日'.repeat(110_000),
      ReceiptHandle: 'rh-multibyte',
      MessageId: 'm-multibyte',
    };
    expect(message.Body.length).toBeLessThan(200 * 1024); // would have slipped under a char-based cap
    expect(Buffer.byteLength(message.Body, 'utf8')).toBeGreaterThan(200 * 1024); // but exceeds the byte-based cap

    await withMockedSqs(() => eventConsumer._test.processMessage(client, message));

    expect(client.actions.InteractionCreate.handle).not.toHaveBeenCalled();
    expect(logger.error).toHaveBeenCalledWith(
      expect.stringContaining('message body exceeds size cap'),
      expect.objectContaining({
        messageId: 'm-multibyte',
        bodyBytes: 330_000,
      }),
    );
  });

  test('seenEventIds not populated on malformed body or unknown eventType', async () => {
    sqsMock.on(DeleteMessageCommand).resolves({});
    const client = makeStubClient();
    const seen = eventConsumer._test.seenEventIds;

    await withMockedSqs(async () => {
      await eventConsumer._test.processMessage(client, {
        Body: 'not-json{{{',
        ReceiptHandle: 'rh-mal',
        MessageId: 'm-mal',
      });
      await eventConsumer._test.processMessage(client, {
        Body: JSON.stringify({ eventType: 'GUILD_MEMBER_ADD', event_id: '0:99' }),
        ReceiptHandle: 'rh-unk',
        MessageId: 'm-unk',
      });
      await eventConsumer._test.processMessage(client, {
        Body: 'null',
        ReceiptHandle: 'rh-null',
        MessageId: 'm-null',
      });
    });

    expect(seen.size).toBe(0);
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

    expect(client.actions.InteractionCreate.handle).toHaveBeenCalledTimes(2);
    expect(sqsMock.commandCalls(DeleteMessageCommand)).toHaveLength(2);
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
    sqsMock.on(DeleteMessageCommand).callsFake((input) => {
      if (input.ReceiptHandle === 'rh-2') throw new Error('boom');
      return {};
    });
    const client = makeStubClient();

    await withMockedSqs(() => eventConsumer._test.pollOnce(client));

    expect(client.actions.InteractionCreate.handle).toHaveBeenCalledTimes(3);
    expect(logger.error).toHaveBeenCalledWith(
      expect.stringContaining('DeleteMessage failed'),
      expect.objectContaining({ messageId: 'm-2' }),
    );
  });
});

describe('event-consumer: data: undefined envelope shape', () => {
  test('INTERACTION_CREATE with missing data → reconstruction throws → still deletes', async () => {
    sqsMock.on(DeleteMessageCommand).resolves({});
    const client = makeStubClient();
    client.actions.InteractionCreate.handle.mockImplementation((data) => {
      if (!data) throw new TypeError("Cannot read properties of undefined (reading 'type')");
    });
    const message = makeMessage({
      eventType: 'INTERACTION_CREATE',
      event_id: '0:7',
    });

    await withMockedSqs(() => eventConsumer._test.processMessage(client, message));

    expect(logger.error).toHaveBeenCalledWith(
      expect.stringContaining('dispatch reconstruction failed'),
      expect.objectContaining({ eventId: '0:7' }),
    );
    expect(sqsMock.commandCalls(DeleteMessageCommand)).toHaveLength(1);
  });
});

describe('event-consumer: abort silently exits pollLoop', () => {
  test('AbortError from pollOnce produces no error log + no backoff', async () => {
    const setIntervalSpy = jest.spyOn(global, 'setInterval');
    const baselineIntervals = setIntervalSpy.mock.calls.filter(([, ms]) => ms === 50).length;

    let receiveCount = 0;
    sqsMock.on(ReceiveMessageCommand).callsFake(() => {
      receiveCount += 1;
      if (receiveCount === 2) {
        eventConsumer.stop();
      }
      const err = new Error('Request aborted');
      err.name = 'AbortError';
      throw err;
    });
    eventConsumer._test._setSqsClientForTest(new SQSClient({ region: 'us-east-2' }));

    eventConsumer.start(makeStubClient());
    await new Promise((r) => setTimeout(r, 50));

    expect(receiveCount).toBe(2);
    expect(logger.error).not.toHaveBeenCalledWith(
      expect.stringContaining('poll iteration failed'),
      expect.anything(),
    );
    const finalIntervals = setIntervalSpy.mock.calls.filter(([, ms]) => ms === 50).length;
    expect(finalIntervals).toBe(baselineIntervals);

    setIntervalSpy.mockRestore();
  });
});

describe('event-consumer: abortableSleep (AbortSignal-driven)', () => {

  test('timeout-wins-race path resolves without any setInterval polling tick', async () => {
    const setIntervalSpy = jest.spyOn(global, 'setInterval');
    try {
      eventConsumer._test._setStopControllerForTest();
      await eventConsumer._test.abortableSleep(30);
      const ticks50ms = setIntervalSpy.mock.calls.filter(([, ms]) => ms === 50);
      expect(ticks50ms).toHaveLength(0);
    } finally {
      setIntervalSpy.mockRestore();
    }
  });

  test('abort()-wins-race path resolves on a microtask after abort fires', async () => {
    eventConsumer._test._setStopControllerForTest();
    const start = Date.now();
    const sleepPromise = eventConsumer._test.abortableSleep(10_000);
    await Promise.resolve();
    eventConsumer._test.getStopController().abort();
    await sleepPromise;
    const elapsed = Date.now() - start;
    expect(elapsed).toBeLessThan(100);
  });

  test('already-aborted signal: abortableSleep resolves immediately (fast path)', async () => {
    eventConsumer._test._setStopControllerForTest();
    eventConsumer._test.getStopController().abort();
    const start = Date.now();
    await eventConsumer._test.abortableSleep(5_000);
    const elapsed = Date.now() - start;
    expect(elapsed).toBeLessThan(100);
  });

  test('null stopController: abortableSleep degrades to a pure setTimeout (defensive)', async () => {
    eventConsumer._test._resetStateForTest();
    expect(eventConsumer._test.getStopController()).toBeNull();
    const start = Date.now();
    await eventConsumer._test.abortableSleep(25);
    const elapsed = Date.now() - start;
    expect(elapsed).toBeGreaterThanOrEqual(20);
    expect(elapsed).toBeLessThan(500);
  });
});

describe('event-consumer: pollLoop error backoff', () => {
  test('pollLoop catches ReceiveMessage errors, logs, sleeps, then continues', async () => {
    let receiveCount = 0;
    sqsMock.on(ReceiveMessageCommand).callsFake(() => {
      receiveCount += 1;
      if (receiveCount === 1) throw new Error('AWS throttling');
      return { Messages: [] };
    });
    eventConsumer._test._setSqsClientForTest(new SQSClient({ region: 'us-east-2' }));
    const client = makeStubClient();

    eventConsumer.start(client);
    await new Promise((r) => setImmediate(r));

    const startTime = Date.now();
    await eventConsumer.stop();
    const elapsedMs = Date.now() - startTime;

    expect(logger.error).toHaveBeenCalledWith(
      expect.stringContaining('poll iteration failed'),
      expect.objectContaining({ error: 'AWS throttling' }),
    );
    expect(elapsedMs).toBeLessThan(100);
  });

  test('pollLoop exits on permanent AWS error (QueueDoesNotExist) — fatal log + process.exit(1)', async () => {
    const exitSpy = jest.spyOn(process, 'exit').mockImplementation(() => undefined);
    sqsMock.on(ReceiveMessageCommand).callsFake(() => {
      const err = new Error('Specified queue does not exist');
      err.name = 'QueueDoesNotExist';
      throw err;
    });
    eventConsumer._test._setSqsClientForTest(new SQSClient({ region: 'us-east-2' }));
    const client = makeStubClient();

    try {
      eventConsumer.start(client);
      await new Promise((r) => setImmediate(r));

      expect(exitSpy).toHaveBeenCalledWith(1);
      expect(exitSpy).toHaveBeenCalledTimes(1);
      expect(logger.error).toHaveBeenCalledWith(
        expect.stringContaining('permanent AWS failure'),
        expect.objectContaining({ code: 'QueueDoesNotExist' }),
      );
      expect(logger.error).not.toHaveBeenCalledWith(
        expect.stringContaining('poll iteration failed'),
        expect.anything(),
      );
      expect(eventConsumer._test.isRunning()).toBe(true);
      await eventConsumer.stop();
      expect(eventConsumer._test.isRunning()).toBe(false);
    } finally {
      exitSpy.mockRestore();
    }
  });

  test('pollLoop exits on err.cause-wrapped permanent error (SDK wrapping)', async () => {
    const exitSpy = jest.spyOn(process, 'exit').mockImplementation(() => undefined);
    sqsMock.on(ReceiveMessageCommand).callsFake(() => {
      const inner = Object.assign(new Error('access denied'), {
        name: 'AccessDeniedException',
      });
      const outer = new Error('SDK wrapper');
      outer.cause = inner;
      throw outer;
    });
    eventConsumer._test._setSqsClientForTest(new SQSClient({ region: 'us-east-2' }));
    const client = makeStubClient();

    try {
      eventConsumer.start(client);
      await new Promise((r) => setImmediate(r));

      expect(exitSpy).toHaveBeenCalledTimes(1);
      expect(logger.error).toHaveBeenCalledWith(
        expect.stringContaining('permanent AWS failure'),
        expect.objectContaining({ code: 'AccessDeniedException' }),
      );
      await eventConsumer.stop();
    } finally {
      exitSpy.mockRestore();
    }
  });

  test('onFatal callback routes permanent-error path away from process.exit', async () => {
    const exitSpy = jest.spyOn(process, 'exit').mockImplementation(() => undefined);
    const onFatal = jest.fn();
    sqsMock.on(ReceiveMessageCommand).callsFake(() => {
      const err = new Error('q gone');
      err.name = 'QueueDoesNotExist';
      throw err;
    });
    eventConsumer._test._setSqsClientForTest(new SQSClient({ region: 'us-east-2' }));
    const client = makeStubClient();

    try {
      eventConsumer.start(client, { onFatal });
      await new Promise((r) => setImmediate(r));

      expect(onFatal).toHaveBeenCalledTimes(1);
      expect(exitSpy).not.toHaveBeenCalled();
      await eventConsumer.stop();
    } finally {
      exitSpy.mockRestore();
    }
  });

  test('onFatal throw falls back to process.exit(1)', async () => {
    const exitSpy = jest.spyOn(process, 'exit').mockImplementation(() => undefined);
    const onFatal = jest.fn(() => { throw new Error('shutdown failed'); });
    sqsMock.on(ReceiveMessageCommand).callsFake(() => {
      const err = new Error('q gone');
      err.name = 'QueueDoesNotExist';
      throw err;
    });
    eventConsumer._test._setSqsClientForTest(new SQSClient({ region: 'us-east-2' }));
    const client = makeStubClient();

    try {
      eventConsumer.start(client, { onFatal });
      await new Promise((r) => setImmediate(r));

      expect(onFatal).toHaveBeenCalledTimes(1);
      expect(exitSpy).toHaveBeenCalledWith(1);
      expect(logger.error).toHaveBeenCalledWith(
        expect.stringContaining('onFatal threw, falling back to process.exit'),
        expect.objectContaining({ error: 'shutdown failed' }),
      );
      await eventConsumer.stop();
    } finally {
      exitSpy.mockRestore();
    }
  });

  test('onFatal async rejection falls back to process.exit(1)', async () => {
    const exitSpy = jest.spyOn(process, 'exit').mockImplementation(() => undefined);
    const onFatal = jest.fn(() => Promise.reject(new Error('async shutdown failed')));
    sqsMock.on(ReceiveMessageCommand).callsFake(() => {
      const err = new Error('q gone');
      err.name = 'QueueDoesNotExist';
      throw err;
    });
    eventConsumer._test._setSqsClientForTest(new SQSClient({ region: 'us-east-2' }));
    const client = makeStubClient();

    try {
      eventConsumer.start(client, { onFatal });
      await new Promise((r) => setImmediate(r));
      await new Promise((r) => setImmediate(r));

      expect(onFatal).toHaveBeenCalledTimes(1);
      expect(exitSpy).toHaveBeenCalledWith(1);
      expect(logger.error).toHaveBeenCalledWith(
        expect.stringContaining('onFatal rejected, falling back to process.exit'),
        expect.objectContaining({ error: 'async shutdown failed' }),
      );
      await eventConsumer.stop();
    } finally {
      exitSpy.mockRestore();
    }
  });
});

describe('event-consumer: start/stop lifecycle', () => {
  test('start() throws when client.actions.InteractionCreate.handle is missing', () => {
    expect(() => eventConsumer.start({})).toThrow('discord.js internal-API drift');
    expect(() => eventConsumer.start({ actions: {} })).toThrow('discord.js internal-API drift');
    expect(() => eventConsumer.start({ actions: { InteractionCreate: {} } })).toThrow('discord.js internal-API drift');
    expect(() => eventConsumer.start(null)).toThrow('discord.js internal-API drift');
  });

  test('start() throws when ENABLE_EVENT_SHIPPER=false', () => {
    const config = require('../src/config');
    const orig = config.ENABLE_EVENT_SHIPPER;
    config.ENABLE_EVENT_SHIPPER = false;
    try {
      expect(() => eventConsumer.start(makeStubClient())).toThrow('ENABLE_EVENT_SHIPPER=false');
    } finally {
      config.ENABLE_EVENT_SHIPPER = orig;
    }
  });

  test('start() throws when queue URL is missing', () => {
    const config = require('../src/config');
    const orig = config.QURL_BOT_EVENTS_QUEUE_URL;
    config.QURL_BOT_EVENTS_QUEUE_URL = undefined;
    try {
      expect(() => eventConsumer.start(makeStubClient())).toThrow('QURL_BOT_EVENTS_QUEUE_URL');
    } finally {
      config.QURL_BOT_EVENTS_QUEUE_URL = orig;
    }
  });

  test('stop() before start() is a no-op (idempotent)', async () => {
    await expect(eventConsumer.stop()).resolves.toBeUndefined();
  });

  test('isAbortError recognizes AbortError shapes but NOT TimeoutError', () => {
    const { isAbortError } = eventConsumer._test;
    expect(isAbortError(null)).toBe(false);
    expect(isAbortError(undefined)).toBe(false);
    expect(isAbortError(new Error('boom'))).toBe(false);
    const e1 = new Error('aborted'); e1.name = 'AbortError';
    expect(isAbortError(e1)).toBe(true);
    const e2 = new Error('aborted'); e2.code = 'AbortError';
    expect(isAbortError(e2)).toBe(true);
    const e3 = new Error('aborted'); e3.code = 'ABORT_ERR';
    expect(isAbortError(e3)).toBe(true);
    const e4 = new Error('canceled'); e4.name = 'CanceledError';
    expect(isAbortError(e4)).toBe(true);
    const e5 = new Error('Request failed'); e5.cause = { name: 'AbortError' };
    expect(isAbortError(e5)).toBe(true);
    const e6 = new Error('Request failed'); e6.cause = { name: 'CanceledError' };
    expect(isAbortError(e6)).toBe(true);
    const e7 = new Error('outer');
    e7.cause = { name: 'Wrapper', cause: { name: 'AbortError' } };
    expect(isAbortError(e7)).toBe(true);
    const e8 = new Error('timeout'); e8.name = 'TimeoutError';
    expect(isAbortError(e8)).toBe(false);
    const e9 = new Error('Request failed'); e9.cause = { name: 'TimeoutError' };
    expect(isAbortError(e9)).toBe(false);
    const e10 = new Error('cyclic');
    e10.cause = e10;
    expect(isAbortError(e10)).toBe(false);
  });

  test('permanentAwsErrorCode recognizes misconfig shapes, returns null for transient', () => {
    const { permanentAwsErrorCode } = eventConsumer._test;
    expect(permanentAwsErrorCode(null)).toBeNull();
    expect(permanentAwsErrorCode(undefined)).toBeNull();
    expect(permanentAwsErrorCode(new Error('boom'))).toBeNull();

    const queueGone = Object.assign(new Error('q gone'), { name: 'QueueDoesNotExist' });
    expect(permanentAwsErrorCode(queueGone)).toBe('QueueDoesNotExist');
    const accessDenied = Object.assign(new Error('AD'), { name: 'AccessDeniedException' });
    expect(permanentAwsErrorCode(accessDenied)).toBe('AccessDeniedException');

    const v2NonExistent = Object.assign(new Error('q gone'), {
      Code: 'AWS.SimpleQueueService.NonExistentQueue',
    });
    expect(permanentAwsErrorCode(v2NonExistent)).toBe('AWS.SimpleQueueService.NonExistentQueue');
    const invalidUrl = Object.assign(new Error('bad url'), { code: 'InvalidQueueUrl' });
    expect(permanentAwsErrorCode(invalidUrl)).toBe('InvalidQueueUrl');

    const badRegion = Object.assign(new Error('endpoint'), { name: 'UnknownEndpoint' });
    expect(permanentAwsErrorCode(badRegion)).toBe('UnknownEndpoint');

    const badCreds = Object.assign(new Error('creds'), { name: 'UnrecognizedClientException' });
    expect(permanentAwsErrorCode(badCreds)).toBe('UnrecognizedClientException');

    const wrapped = Object.assign(new Error('wrap'), {
      cause: { name: 'QueueDoesNotExist' },
    });
    expect(permanentAwsErrorCode(wrapped)).toBe('QueueDoesNotExist');

    const throttle = Object.assign(new Error('rate'), { name: 'ThrottlingException' });
    expect(permanentAwsErrorCode(throttle)).toBeNull();
    const networkErr = Object.assign(new Error('econnreset'), { code: 'ECONNRESET' });
    expect(permanentAwsErrorCode(networkErr)).toBeNull();
    const timeoutErr = Object.assign(new Error('t/o'), { name: 'TimeoutError' });
    expect(permanentAwsErrorCode(timeoutErr)).toBeNull();

    const cyclic = new Error('cyclic');
    cyclic.cause = cyclic;
    expect(permanentAwsErrorCode(cyclic)).toBeNull();
  });

  test('pollOnce passes the stopController.signal in the SDK send options', async () => {
    sqsMock.on(ReceiveMessageCommand).resolves({ Messages: [] });
    const realClient = new SQSClient({ region: 'us-east-2' });
    const sendSpy = jest.spyOn(realClient, 'send');
    eventConsumer._test._setSqsClientForTest(realClient);
    eventConsumer._test._setStopControllerForTest();

    await eventConsumer._test.pollOnce(makeStubClient());

    expect(sendSpy).toHaveBeenCalled();
    const receiveCall = sendSpy.mock.calls.find(
      ([cmd]) => cmd instanceof ReceiveMessageCommand,
    );
    expect(receiveCall).toBeDefined();
    const [, options] = receiveCall;
    expect(options).toBeDefined();
    expect(options.abortSignal).toBeDefined();
    expect(options.abortSignal.aborted).toBe(false);
    expect(options.abortSignal).toBe(eventConsumer._test.getStopController().signal);

    sendSpy.mockRestore();
  });

  test('stopController persists across pollOnce iterations (single lifetime controller, not per-iteration)', async () => {
    sqsMock.on(ReceiveMessageCommand).resolves({ Messages: [] });
    eventConsumer._test._setSqsClientForTest(new SQSClient({ region: 'us-east-2' }));
    eventConsumer._test._setStopControllerForTest();

    await withMockedSqs(() => eventConsumer._test.pollOnce(makeStubClient()));
    const ctrlAfterFirst = eventConsumer._test.getStopController();
    expect(ctrlAfterFirst).not.toBeNull();
    expect(ctrlAfterFirst.signal.aborted).toBe(false);

    await withMockedSqs(() => eventConsumer._test.pollOnce(makeStubClient()));
    const ctrlAfterSecond = eventConsumer._test.getStopController();
    expect(ctrlAfterSecond).toBe(ctrlAfterFirst);

    ctrlAfterFirst.abort();
    expect(ctrlAfterFirst.signal.aborted).toBe(true);
  });

  test('start() + stop() round-trip; second start logs warn (idempotent)', async () => {
    sqsMock.on(ReceiveMessageCommand).resolves({ Messages: [] });
    eventConsumer._test._setSqsClientForTest(new SQSClient({ region: 'us-east-2' }));
    const client = makeStubClient();

    eventConsumer.start(client);
    eventConsumer.start(client);
    expect(logger.warn).toHaveBeenCalledWith(
      expect.stringContaining('start() called while already running'),
    );

    await eventConsumer.stop();
    await eventConsumer.stop();
  });

  test('stop() drains in-flight handlers before resolving (settled within deadline)', async () => {
    sqsMock.on(ReceiveMessageCommand).resolves({ Messages: [] });
    eventConsumer._test._setSqsClientForTest(new SQSClient({ region: 'us-east-2' }));
    eventConsumer._test._setDrainDeadlineForTest(500);
    const client = makeStubClient();
    eventConsumer.start(client);

    const resolvers = [];
    withWorkerDispatch(() => {
      for (let i = 0; i < 2; i += 1) {
        let resolve;
        const p = new Promise((r) => { resolve = r; });
        resolvers.push(resolve);
        eventConsumer.trackDispatch(p);
      }
    });
    expect(eventConsumer._test.getInFlightCount()).toBe(2);

    setTimeout(() => resolvers.forEach((r) => r('done')), 20);
    await eventConsumer.stop();

    expect(logger.info).toHaveBeenCalledWith(
      'Event consumer: drain complete',
      expect.objectContaining({ count: 2 }),
    );
    expect(eventConsumer._test.getInFlightCount()).toBe(0);
  });

  test('stop() returns within deadline when handlers do not settle', async () => {
    sqsMock.on(ReceiveMessageCommand).resolves({ Messages: [] });
    eventConsumer._test._setSqsClientForTest(new SQSClient({ region: 'us-east-2' }));
    eventConsumer._test._setDrainDeadlineForTest(50);

    const client = makeStubClient();
    eventConsumer.start(client);

    withWorkerDispatch(() => {
      eventConsumer.trackDispatch(new Promise(() => {}));
    });
    expect(eventConsumer._test.getInFlightCount()).toBe(1);

    const start = Date.now();
    await eventConsumer.stop();
    const elapsed = Date.now() - start;

    expect(logger.warn).toHaveBeenCalledWith(
      'Event consumer: drain deadline elapsed, proceeding with handlers still in-flight',
      expect.objectContaining({ unsettled: 1, settled: 0 }),
    );
    expect(elapsed).toBeLessThan(50 * 20);
  });

  test('getDrainDeadlineMs reflects live value after _setDrainDeadlineForTest', () => {
    expect(eventConsumer._test.getDrainDeadlineMs()).toBe(3000);
    eventConsumer._test._setDrainDeadlineForTest(50);
    expect(eventConsumer._test.getDrainDeadlineMs()).toBe(50);
  });

  test('stop() drains even when loopPromise rejects (loop crash does not skip drain)', async () => {
    sqsMock.on(ReceiveMessageCommand).resolves({ Messages: [] });
    eventConsumer._test._setSqsClientForTest(new SQSClient({ region: 'us-east-2' }));
    eventConsumer._test._setDrainDeadlineForTest(500);
    const client = makeStubClient();
    eventConsumer.start(client);

    eventConsumer._test._setLoopPromiseForTest(Promise.reject(new Error('loop crashed')));

    const resolvers = [];
    withWorkerDispatch(() => {
      let resolve;
      const p = new Promise((r) => { resolve = r; });
      resolvers.push(resolve);
      eventConsumer.trackDispatch(p);
    });
    expect(eventConsumer._test.getInFlightCount()).toBe(1);

    setTimeout(() => resolvers.forEach((r) => r('done')), 20);
    await eventConsumer.stop();

    expect(logger.error).toHaveBeenCalledWith(
      'Event consumer: error during stop',
      expect.objectContaining({ error: 'loop crashed' }),
    );
    expect(logger.info).toHaveBeenCalledWith(
      'Event consumer: drain complete',
      expect.objectContaining({ count: 1 }),
    );
  });

  test('stop() skips drain branch when no handlers are in-flight (no spurious logs)', async () => {
    sqsMock.on(ReceiveMessageCommand).resolves({ Messages: [] });
    eventConsumer._test._setSqsClientForTest(new SQSClient({ region: 'us-east-2' }));
    const client = makeStubClient();
    eventConsumer.start(client);
    expect(eventConsumer._test.getInFlightCount()).toBe(0);

    await eventConsumer.stop();

    expect(logger.info).not.toHaveBeenCalledWith(
      'Event consumer: draining in-flight handlers',
      expect.anything(),
    );
    expect(logger.info).not.toHaveBeenCalledWith(
      'Event consumer: drain complete',
      expect.anything(),
    );
    expect(logger.warn).not.toHaveBeenCalledWith(
      'Event consumer: drain deadline elapsed, proceeding with handlers still in-flight',
      expect.anything(),
    );
  });
});

describe('event-consumer: backpressure (in-flight handler cap)', () => {

  async function flushMicrotasks(n = 3) {
    for (let i = 0; i < n; i += 1) {
      // eslint-disable-next-line no-await-in-loop -- sequential by design
      await new Promise((r) => setImmediate(r));
    }
  }

  test('trackDispatch is a no-op when isWorkerDispatch is false (gateway mode)', () => {
    expect(eventConsumer._test.isWorkerDispatching()).toBe(false);
    expect(eventConsumer._test.getInFlightCount()).toBe(0);
    eventConsumer.trackDispatch(Promise.resolve('ignored'));
    expect(eventConsumer._test.getInFlightCount()).toBe(0);
  });

  test('trackDispatch handles non-promise inputs without throwing', () => {
    eventConsumer.trackDispatch(undefined);
    eventConsumer.trackDispatch(null);
    eventConsumer.trackDispatch('not a promise');
    eventConsumer.trackDispatch({ then: 'not-a-fn' });
    expect(eventConsumer._test.getInFlightCount()).toBe(0);
  });

  test('processMessage sets isWorkerDispatch true around handle() and clears in finally', async () => {
    sqsMock.on(DeleteMessageCommand).resolves({});
    const client = makeStubClient();
    let observedDuringHandle = false;
    client.actions.InteractionCreate.handle.mockImplementation(() => {
      observedDuringHandle = eventConsumer._test.isWorkerDispatching();
    });

    await withMockedSqs(() => eventConsumer._test.processMessage(client, makeMessage({
      eventType: 'INTERACTION_CREATE',
      data: { id: 'i1' },
      event_id: '0:1',
    })));

    expect(observedDuringHandle).toBe(true);
    expect(eventConsumer._test.isWorkerDispatching()).toBe(false);
  });

  test('processMessage clears isWorkerDispatch even when handle() throws', async () => {
    sqsMock.on(DeleteMessageCommand).resolves({});
    const client = makeStubClient();
    client.actions.InteractionCreate.handle.mockImplementation(() => {
      throw new Error('reconstruction failed');
    });

    await withMockedSqs(() => eventConsumer._test.processMessage(client, makeMessage({
      eventType: 'INTERACTION_CREATE',
      data: { type: 99 },
      event_id: '0:err',
    })));

    expect(eventConsumer._test.isWorkerDispatching()).toBe(false);
    expect(eventConsumer._test.getInFlightCount()).toBe(0);
  });

  test('trackDispatch increments then decrements on promise resolve', async () => {
    let resolveHandler;
    const handlerPromise = new Promise((r) => { resolveHandler = r; });

    withWorkerDispatch(() => eventConsumer.trackDispatch(handlerPromise));

    expect(eventConsumer._test.getInFlightCount()).toBe(1);
    resolveHandler('done');
    await handlerPromise;
    await flushMicrotasks();
    expect(eventConsumer._test.getInFlightCount()).toBe(0);
  });

  test('trackDispatch decrements on promise rejection', async () => {
    let rejectHandler;
    const handlerPromise = new Promise((_resolve, reject) => { rejectHandler = reject; });
    handlerPromise.catch(() => { /* absorbed */ });

    withWorkerDispatch(() => eventConsumer.trackDispatch(handlerPromise));

    expect(eventConsumer._test.getInFlightCount()).toBe(1);
    rejectHandler(new Error('handler failed'));
    await flushMicrotasks();
    expect(eventConsumer._test.getInFlightCount()).toBe(0);
  });

  test('trackDispatch with an already-settled promise still drains the counter', async () => {
    withWorkerDispatch(() => {
      eventConsumer.trackDispatch(Promise.resolve('already-done'));
    });

    expect(eventConsumer._test.getInFlightCount()).toBe(1);
    await flushMicrotasks();
    expect(eventConsumer._test.getInFlightCount()).toBe(0);
  });

  test('trackDispatch logs handler rejections (preserves error visibility)', async () => {
    const handlerPromise = Promise.reject(new Error('handler boom'));
    handlerPromise.catch(() => { /* absorbed */ });

    withWorkerDispatch(() => eventConsumer.trackDispatch(handlerPromise));

    await flushMicrotasks();

    expect(logger.error).toHaveBeenCalledWith(
      'Event consumer: dispatch handler rejected',
      expect.objectContaining({
        kind: 'unhandledRejection',
        error: 'handler boom',
      }),
    );
  });

  test('trackDispatch tolerates _resetStateForTest mid-flight (Set.delete is a no-op on non-member)', async () => {
    let resolveHandler;
    const handlerPromise = new Promise((r) => { resolveHandler = r; });
    withWorkerDispatch(() => eventConsumer.trackDispatch(handlerPromise));
    expect(eventConsumer._test.getInFlightCount()).toBe(1);

    eventConsumer._test._resetStateForTest();
    expect(eventConsumer._test.getInFlightCount()).toBe(0);
    resolveHandler('done');
    await flushMicrotasks();

    expect(logger.error).not.toHaveBeenCalledWith(
      expect.stringContaining('underflow'),
      expect.anything(),
    );
    expect(eventConsumer._test.getInFlightCount()).toBe(0);
  });

  test('pollOnce early-returns + backs off when in-flight at cap', async () => {
    const cap = eventConsumer._test.MAX_INFLIGHT_HANDLERS;

    const pendingHandlers = [];
    withWorkerDispatch(() => {
      for (let i = 0; i < cap; i += 1) {
        const p = new Promise(() => {}); // never resolves
        eventConsumer.trackDispatch(p);
        pendingHandlers.push(p);
      }
    });
    expect(eventConsumer._test.getInFlightCount()).toBe(cap);

    sqsMock.on(ReceiveMessageCommand).resolves({ Messages: [{ Body: '{}', ReceiptHandle: 'x' }] });
    const client = makeStubClient();

    await withMockedSqs(() => eventConsumer._test.pollOnce(client));

    expect(sqsMock.commandCalls(ReceiveMessageCommand)).toHaveLength(0);
    expect(logger.warn).toHaveBeenCalledWith(
      eventConsumer._test.AT_CAP_PAUSE_WARN_MSG,
      expect.objectContaining({ inFlight: cap, cap }),
    );
    expect(eventConsumer._test.isAtCapPauseLogged()).toBe(true);
  });

  async function pollOnceFast(client) {
    const p = withMockedSqs(() => eventConsumer._test.pollOnce(client));
    await jest.runOnlyPendingTimersAsync();
    return p;
  }

  test('pollOnce stays silent during a sustained at-cap streak (one warn at entry, no per-iteration noise)', async () => {
    jest.useFakeTimers({ doNotFake: ['nextTick', 'setImmediate', 'queueMicrotask'] });
    try {
      const cap = eventConsumer._test.MAX_INFLIGHT_HANDLERS;
      withWorkerDispatch(() => {
        for (let i = 0; i < cap; i += 1) {
          eventConsumer.trackDispatch(new Promise(() => {})); // never resolves
        }
      });

      sqsMock.on(ReceiveMessageCommand).resolves({ Messages: [] });
      const client = makeStubClient();
      const warnBaseline = logger.warn.mock.calls.length;
      const debugBaseline = logger.debug.mock.calls.length;

      await pollOnceFast(client);
      await pollOnceFast(client);
      await pollOnceFast(client);

      expect(logger.warn.mock.calls.length - warnBaseline).toBe(1);
      expect(logger.debug.mock.calls.length - debugBaseline).toBe(0);
    } finally {
      jest.useRealTimers();
    }
  });

  test('pollOnce: at-cap backoff doubles each iteration up to the ceiling', async () => {
    jest.useFakeTimers({ doNotFake: ['nextTick', 'setImmediate', 'queueMicrotask'] });
    try {
      const cap = eventConsumer._test.MAX_INFLIGHT_HANDLERS;
      withWorkerDispatch(() => {
        for (let i = 0; i < cap; i += 1) {
          eventConsumer.trackDispatch(new Promise(() => {}));
        }
      });

      sqsMock.on(ReceiveMessageCommand).resolves({ Messages: [] });
      const client = makeStubClient();

      expect(eventConsumer._test.getCurrentBackoffMs()).toBe(eventConsumer._test.INFLIGHT_BACKOFF_BASE_MS);

      await pollOnceFast(client);
      expect(eventConsumer._test.getCurrentBackoffMs()).toBe(200);

      await pollOnceFast(client);
      expect(eventConsumer._test.getCurrentBackoffMs()).toBe(400);

      await pollOnceFast(client);
      expect(eventConsumer._test.getCurrentBackoffMs()).toBe(800);

      await pollOnceFast(client);
      expect(eventConsumer._test.getCurrentBackoffMs()).toBe(eventConsumer._test.INFLIGHT_BACKOFF_MAX_MS);

      await pollOnceFast(client);
      expect(eventConsumer._test.getCurrentBackoffMs()).toBe(eventConsumer._test.INFLIGHT_BACKOFF_MAX_MS);
    } finally {
      jest.useRealTimers();
    }
  });

  test('_resetStateForTest restores currentBackoffMs to base', async () => {
    jest.useFakeTimers({ doNotFake: ['nextTick', 'setImmediate', 'queueMicrotask'] });
    try {
      const cap = eventConsumer._test.MAX_INFLIGHT_HANDLERS;
      withWorkerDispatch(() => {
        for (let i = 0; i < cap; i += 1) {
          eventConsumer.trackDispatch(new Promise(() => {}));
        }
      });
      sqsMock.on(ReceiveMessageCommand).resolves({ Messages: [] });
      const client = makeStubClient();

      for (let i = 0; i < 4; i += 1) {
        await pollOnceFast(client);
      }
      expect(eventConsumer._test.getCurrentBackoffMs()).toBe(eventConsumer._test.INFLIGHT_BACKOFF_MAX_MS);

      eventConsumer._test._resetStateForTest();
      expect(eventConsumer._test.getCurrentBackoffMs()).toBe(eventConsumer._test.INFLIGHT_BACKOFF_BASE_MS);
    } finally {
      jest.useRealTimers();
    }
  });

  test('pollOnce logs cap-released info on transition back to below-cap', async () => {
    const cap = eventConsumer._test.MAX_INFLIGHT_HANDLERS;
    const resolvers = [];
    withWorkerDispatch(() => {
      for (let i = 0; i < cap; i += 1) {
        let resolve;
        const p = new Promise((r) => { resolve = r; });
        resolvers.push(resolve);
        eventConsumer.trackDispatch(p);
      }
    });

    sqsMock.on(ReceiveMessageCommand).resolves({ Messages: [] });
    const client = makeStubClient();

    await withMockedSqs(() => eventConsumer._test.pollOnce(client));
    expect(eventConsumer._test.isAtCapPauseLogged()).toBe(true);
    expect(eventConsumer._test.getCurrentBackoffMs()).toBe(200);

    resolvers.forEach((r) => r('done'));
    await flushMicrotasks();
    expect(eventConsumer._test.getInFlightCount()).toBe(0);

    await withMockedSqs(() => eventConsumer._test.pollOnce(client));
    expect(logger.info).toHaveBeenCalledWith(
      eventConsumer._test.AT_CAP_RELEASED_INFO_MSG,
      expect.objectContaining({ cap }),
    );
    expect(eventConsumer._test.isAtCapPauseLogged()).toBe(false);
    expect(eventConsumer._test.getCurrentBackoffMs()).toBe(eventConsumer._test.INFLIGHT_BACKOFF_BASE_MS);
  });

  test('stop() finally resets currentBackoffMs to BASE (load-bearing assertion)', async () => {
    const originalDeadline = eventConsumer._test.getDrainDeadlineMs();
    eventConsumer._test._setDrainDeadlineForTest(50);
    try {
      const cap = eventConsumer._test.MAX_INFLIGHT_HANDLERS;
      withWorkerDispatch(() => {
        for (let i = 0; i < cap; i += 1) {
          eventConsumer.trackDispatch(new Promise(() => {}));
        }
      });
      sqsMock.on(ReceiveMessageCommand).resolves({ Messages: [] });
      const client = makeStubClient();

      jest.useFakeTimers({ doNotFake: ['nextTick', 'setImmediate', 'queueMicrotask'] });
      try {
        await pollOnceFast(client);
        expect(eventConsumer._test.getCurrentBackoffMs()).toBe(200);
      } finally {
        jest.useRealTimers();
      }

      eventConsumer.start(client);
      await eventConsumer.stop();
      expect(eventConsumer._test.getCurrentBackoffMs()).toBe(eventConsumer._test.INFLIGHT_BACKOFF_BASE_MS);
    } finally {
      eventConsumer._test._setDrainDeadlineForTest(originalDeadline);
    }
  });

  test('pollOnce: MAX → BASE reset via production below-cap path (not just one doubling)', async () => {
    jest.useFakeTimers({ doNotFake: ['nextTick', 'setImmediate', 'queueMicrotask'] });
    try {
      const cap = eventConsumer._test.MAX_INFLIGHT_HANDLERS;
      const resolvers = [];
      withWorkerDispatch(() => {
        for (let i = 0; i < cap; i += 1) {
          let resolve;
          const p = new Promise((r) => { resolve = r; });
          resolvers.push(resolve);
          eventConsumer.trackDispatch(p);
        }
      });
      sqsMock.on(ReceiveMessageCommand).resolves({ Messages: [] });
      const client = makeStubClient();

      for (let i = 0; i < 4; i += 1) {
        await pollOnceFast(client);
      }
      expect(eventConsumer._test.getCurrentBackoffMs()).toBe(eventConsumer._test.INFLIGHT_BACKOFF_MAX_MS);

      resolvers.forEach((r) => r('done'));
      await flushMicrotasks();
      expect(eventConsumer._test.getInFlightCount()).toBe(0);

      await pollOnceFast(client);
      expect(eventConsumer._test.getCurrentBackoffMs()).toBe(eventConsumer._test.INFLIGHT_BACKOFF_BASE_MS);
      expect(eventConsumer._test.isAtCapPauseLogged()).toBe(false);
    } finally {
      jest.useRealTimers();
    }
  });

  test('pollOnce proceeds with receive when below cap', async () => {
    expect(eventConsumer._test.getInFlightCount()).toBeLessThan(eventConsumer._test.MAX_INFLIGHT_HANDLERS);

    sqsMock.on(ReceiveMessageCommand).resolves({ Messages: [] });
    const client = makeStubClient();

    await withMockedSqs(() => eventConsumer._test.pollOnce(client));

    expect(sqsMock.commandCalls(ReceiveMessageCommand)).toHaveLength(1);
  });

  test('MAX_INFLIGHT_HANDLERS defaults to 100', () => {
    expect(eventConsumer._test.MAX_INFLIGHT_HANDLERS).toBe(100);
  });

  test('processMessage flag-wrap has no `await` between isWorkerDispatch toggles (static invariant)', () => {
    const src = require('fs').readFileSync(
      require('path').resolve(__dirname, '../src/event-consumer.js'),
      'utf8',
    );
    const startMarker = '// FLAG-WRAP-START';
    const endMarker = '// FLAG-WRAP-END';
    const startIdx = src.indexOf(startMarker);
    const endIdx = src.indexOf(endMarker, startIdx);
    expect(startIdx).toBeGreaterThan(0);
    expect(endIdx).toBeGreaterThan(startIdx);
    expect(src.indexOf(startMarker, startIdx + startMarker.length)).toBe(-1);
    expect(src.indexOf(endMarker, endIdx + endMarker.length)).toBe(-1);

    const block = src.slice(startIdx, endIdx);
    const stripped = block
      .replace(/\/\*[\s\S]*?\*\//g, '') // block comments first (multi-line)
      .replace(/\/\/[^\n]*/g, ''); // then line comments
    expect(stripped).not.toMatch(/\bawait\b/);
  });

  describe('validateInflightCap (soft-floor warning)', () => {
    function withCapturedWarns(run) {
      const origConsoleWarn = console.warn;
      const warns = [];
      console.warn = (...args) => warns.push(args.join(' '));
      try {
        run(warns);
      } finally {
        console.warn = origConsoleWarn;
      }
    }

    test.each([1, 5, 9])('cap=%i (below RECEIVE_MAX_MESSAGES=10) emits soft-floor warn', (cap) => {
      withCapturedWarns((warns) => {
        eventConsumer._test.validateInflightCap(cap);
        expect(warns.some((w) => w.includes('below') && w.includes('RECEIVE_MAX_MESSAGES'))).toBe(true);
      });
    });

    test.each([10, 50, 100, 200])('cap=%i (>= RECEIVE_MAX_MESSAGES) does NOT emit soft-floor warn', (cap) => {
      withCapturedWarns((warns) => {
        eventConsumer._test.validateInflightCap(cap);
        expect(warns.some((w) => w.includes('below'))).toBe(false);
      });
    });

    test('warn message names the env-var so an operator can correlate boot logs to SSM/task-def', () => {
      withCapturedWarns((warns) => {
        eventConsumer._test.validateInflightCap(5);
        expect(warns.some((w) => w.includes('QURL_BOT_MAX_INFLIGHT_HANDLERS'))).toBe(true);
      });
    });
  });
});

describe('event-consumer: discord.js@14.25.1 internal-API smoke', () => {
  test('package.json pins discord.js to a single exact version', () => {
    const pkg = require('../package.json');
    const decl = pkg.dependencies['discord.js'];
    expect(decl).toMatch(/^\d+\.\d+\.\d+$/);
  });

  test('client.actions.InteractionCreate.handle is a function', () => {
    const { Client, GatewayIntentBits } = require('discord.js');
    const client = new Client({ intents: [GatewayIntentBits.Guilds] });
    expect(client.actions).toBeDefined();
    expect(typeof client.actions.InteractionCreate.handle).toBe('function');
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
