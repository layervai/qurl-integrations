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
  // event-consumer reads these at module-top; intEnv validation
  // itself is tested separately in tests/config-int-env.test.js.
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
  // Full reset of the consumer's module-level singleton state so
  // each test starts from a known shape. seenEventIds, sqsClient,
  // running, loopPromise, stopController — all cleared. Without
  // this, test ordering would matter (e.g., a future test before
  // the start/stop round-trip case would observe whatever `running`
  // was left at).
  eventConsumer._test._resetStateForTest();
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
// mock client via the _setSqsClientForTest DI hook. We also seed a
// stopController (the AbortSignal refactor moved cancellation from a
// per-iteration controller to a single lifetime one — pollOnce reads
// `stopController.signal` for the SDK's abortSignal, so tests
// bypassing start() need an explicit controller).
function withMockedSqs(fn) {
  // The aws-sdk-client-mock library intercepts at the SDK-command
  // level, so any SQSClient instance routes through the mock. But
  // since the consumer's sqsClient may be null until start() runs,
  // we construct one explicitly and inject it.
  const realClient = new SQSClient({ region: 'us-east-2' });
  eventConsumer._test._setSqsClientForTest(realClient);
  // Idempotent: only init if there isn't one already. Tests that
  // depend on controller IDENTITY across multiple pollOnce calls
  // (e.g., the "single lifetime controller" assertion) would
  // otherwise see a fresh controller on every withMockedSqs call.
  if (!eventConsumer._test.getStopController()) {
    eventConsumer._test._setStopControllerForTest();
  }
  return fn();
}

// Run `fn` with isWorkerDispatch flipped true (matches the
// synchronous flag-wrap processMessage performs around handle()),
// then restore. Used by tests that exercise trackDispatch directly
// rather than going through processMessage. Lives at module scope
// so both the start/stop lifecycle and backpressure describe blocks
// share one definition.
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

  test('whitespace-only event_id is rejected (does NOT pollute LRU)', () => {
    // Pins the subtle case where a producer regression that emits
    // `event_id: ' '` would otherwise key every message under the
    // same id and falsely report 100% dup rate. Strict trim-check
    // rejects empty/whitespace strings before they enter the LRU.
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

  // Each entry exercises a parse-succeeds-but-shape-is-wrong path.
  // Without the non-object guard, `null` would TypeError out of
  // destructuring and skip DeleteMessage — message would redrive
  // until DLQ, violating the "delete on every terminal path"
  // invariant. Pinning every variant here so the guard can't
  // silently regress.
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
    // recordSeen runs BEFORE the dispatch try/catch — by design,
    // because the event has reached the worker (was "attempted-to-
    // dispatch"). A poison message that throws on reconstruction
    // still populates the LRU; the dup-debug log carries
    // `dispatchOk: false` so ops can disambiguate real dups from
    // poison-pill retries. Pin this contract so a refactor that
    // hoists recordSeen past the try/catch can't quietly invert it.
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

    // Re-deliver the same event_id; the dup-debug log should fire
    // with dispatchOk: false (because reconstruction still throws).
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
    // The warn is rate-limited to once per 1h cooldown so a
    // sustained producer regression doesn't flood logs. The
    // re-arm catches the case where a regression is fixed and
    // resurfaces hours later. Subsequent observations within the
    // cooldown window are debug-only.
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

    // First two close-together: first warns, second is debug-only.
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
    // Warn fires exactly once within the cooldown window.
    const warnCalls = logger.warn.mock.calls.filter(
      ([msg]) => typeof msg === 'string' && msg.includes('envelope missing valid event_id'),
    );
    expect(warnCalls).toHaveLength(1);

    // Simulate ≥1h passing by spying Date.now to return a value
    // past the cooldown. The next observation should re-arm the warn.
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
    // The e2e latency log is the publish→dispatch metric: wall-clock
    // delta from gateway-host envelope-build to worker-host receive.
    // Pin the structured field name (qurl_bot_event_e2e_ms) since
    // CloudWatch log-based-metrics + alarms pivot on it.
    sqsMock.on(DeleteMessageCommand).resolves({});
    const client = makeStubClient();
    // Pin "now" so the delta is deterministic. Date.now() inside
    // the consumer reads this mocked value.
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
    // Forward-compat with envelopes that predate PR 10 (or a future
    // producer regression that drops the field). Missing the field
    // is observability-only, not correctness — so debug, not warn.
    sqsMock.on(DeleteMessageCommand).resolves({});
    const client = makeStubClient();
    const message = makeMessage({
      eventType: 'INTERACTION_CREATE',
      shardId: '0',
      data: { type: 2, data: { name: 'qurl' }, id: 'i-no-ts' },
      event_id: '0:43',
      // no published_at_ms
    });
    await withMockedSqs(() => eventConsumer._test.processMessage(client, message));
    expect(logger.debug).toHaveBeenCalledWith(
      expect.stringContaining('missing published_at_ms'),
      expect.objectContaining({ publishedAtMsType: 'undefined' }),
    );
    // The info "dispatched" path must NOT fire when the field is absent.
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
    // Pins the cheap defense against an oversized body — today the
    // trust boundary (IAM-locked publisher) prevents abuse, but if
    // the publisher set ever loosens, a 1 MB payload would
    // otherwise drive JSON.parse cost (and any downstream
    // serialization in handlers) into the consumer process.
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
    // Pins the Buffer.byteLength fix vs the previous String.length
    // measurement. A 110k-char string of 3-byte UTF-8 chars (e.g.
    // CJK ideographs) is 330k bytes — over the 200 KB cap — but
    // its String.length is only 110k, well under the previous
    // char-based cap. The pre-fix code would have let this through
    // to JSON.parse despite being well over SQS's 256 KB wire cap.
    sqsMock.on(DeleteMessageCommand).resolves({});
    const client = makeStubClient();
    // '日' is 3 bytes in UTF-8; 110,000 chars = 330,000 bytes.
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
        // bodyBytes carries the byte-length, NOT the char-length.
        bodyBytes: 330_000,
      }),
    );
  });

  test('seenEventIds not populated on malformed body or unknown eventType', async () => {
    // recordSeen sits AFTER the eventType gate, so envelope-shape
    // failures (malformed JSON, non-object, unknown eventType) must
    // not pollute the dedup LRU. Pins the ordering — a refactor that
    // hoists recordSeen above the gate would inflate the LRU with
    // event_ids that never dispatched.
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
      expect.stringContaining('DeleteMessage failed'),
      expect.objectContaining({ messageId: 'm-2' }),
    );
  });
});

describe('event-consumer: data: undefined envelope shape', () => {
  test('INTERACTION_CREATE with missing data → reconstruction throws → still deletes', async () => {
    // Pins the contract that an envelope with eventType=INTERACTION_CREATE
    // but `data` missing falls through to client.actions.InteractionCreate.handle,
    // which throws inside discord.js when destructuring an undefined
    // `data`. The try/catch in processMessage catches the throw, logs
    // 'dispatch reconstruction failed', and DeleteMessage still fires —
    // preserving the every-terminal-path-deletes invariant for the
    // poison-pill containment shape.
    sqsMock.on(DeleteMessageCommand).resolves({});
    const client = makeStubClient();
    client.actions.InteractionCreate.handle.mockImplementation((data) => {
      // Mirror discord.js's behavior: destructuring undefined.data
      // throws TypeError inside InteractionCreateAction.handle's
      // switch on `data.type`.
      if (!data) throw new TypeError("Cannot read properties of undefined (reading 'type')");
    });
    const message = makeMessage({
      eventType: 'INTERACTION_CREATE',
      // data: undefined (missing field)
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
    // Pins the contract that a stop()-triggered abort is silent in
    // operator logs (no "poll iteration failed" line) and bypasses
    // abortableSleep (no 1s backoff). This is a quietly load-bearing
    // UX detail on every routine deploy — log noise during graceful
    // shutdown would mask real failures.
    //
    // Strategy: mock throws AbortError every call; on the SECOND
    // call we also fire-and-forget call stop() so the abort signal
    // fires synchronously (stop() calls abort() before any await).
    // pollLoop's next while-check sees signal.aborted and exits
    // cleanly. Total: 2 mock calls, 2 catches, both go through the
    // silent continue branch.
    const setIntervalSpy = jest.spyOn(global, 'setInterval');
    const baselineIntervals = setIntervalSpy.mock.calls.filter(([, ms]) => ms === 50).length;

    let receiveCount = 0;
    sqsMock.on(ReceiveMessageCommand).callsFake(() => {
      receiveCount += 1;
      if (receiveCount === 2) {
        // Fire-and-forget; stop() calls abort() synchronously before
        // any await, so by the time pollLoop's catch handles this
        // iteration's AbortError and re-checks while, signal.aborted
        // is true and the loop exits.
        eventConsumer.stop();
      }
      const err = new Error('Request aborted');
      err.name = 'AbortError';
      throw err;
    });
    eventConsumer._test._setSqsClientForTest(new SQSClient({ region: 'us-east-2' }));

    eventConsumer.start(makeStubClient());
    // Give the loop + stop time to settle.
    await new Promise((r) => setTimeout(r, 50));

    expect(receiveCount).toBe(2);
    // Silent: no "poll iteration failed" log.
    expect(logger.error).not.toHaveBeenCalledWith(
      expect.stringContaining('poll iteration failed'),
      expect.anything(),
    );
    // No abortableSleep call — and even if there were, abortableSleep
    // would not register a 50ms setInterval (it uses setTimeout +
    // addEventListener). Filter both sides on ms===50 so Jest internals
    // or other helpers that may register non-50ms setIntervals don't
    // contaminate the assertion.
    const finalIntervals = setIntervalSpy.mock.calls.filter(([, ms]) => ms === 50).length;
    expect(finalIntervals).toBe(baselineIntervals);

    setIntervalSpy.mockRestore();
  });
});

describe('event-consumer: abortableSleep (AbortSignal-driven)', () => {
  // Tests pin abortableSleep's contract: timeout-wins path returns
  // on its own; abort-wins path returns on a microtask after abort()
  // fires; controller listener cleaned up on either path.

  test('timeout-wins-race path resolves without any setInterval polling tick', async () => {
    // Pin the absence of 50ms-tick setInterval polling. A regression
    // re-introducing it would coarsen shutdown latency (50ms granular
    // wakes) and add per-sleep timer pressure.
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
    // The whole point of the refactor: a stop() call during sleep
    // returns within a microtask, not on the next 50ms polling tick.
    // Set a long timeout and abort almost-immediately to prove the
    // sleep didn't wait for the timeout.
    eventConsumer._test._setStopControllerForTest();
    const start = Date.now();
    const sleepPromise = eventConsumer._test.abortableSleep(10_000);
    // Harmless yield: the Promise executor runs synchronously, so the
    // addEventListener call is wired by the time abortableSleep
    // returns. The already-aborted fast path in abortableSleep covers
    // the abort-before-listener case separately.
    await Promise.resolve();
    eventConsumer._test.getStopController().abort();
    await sleepPromise;
    const elapsed = Date.now() - start;
    // Tight bound: the abort path resolves on a microtask, so this
    // should be near-zero. 100ms is 100× smaller than the 10_000ms
    // timeout, so still proves the abort path while leaving slack for
    // CI scheduler jitter under contention.
    expect(elapsed).toBeLessThan(100);
  });

  test('already-aborted signal: abortableSleep resolves immediately (fast path)', async () => {
    // Defense in depth — if stop() landed before abortableSleep
    // started (e.g., between iterations of pollLoop), the new sleep
    // must return immediately rather than parking on a timeout that
    // outlives the dying loop.
    eventConsumer._test._setStopControllerForTest();
    eventConsumer._test.getStopController().abort();
    const start = Date.now();
    await eventConsumer._test.abortableSleep(5_000);
    const elapsed = Date.now() - start;
    // Should be near-zero. A failure here would mean the fast-path
    // guard at the top of abortableSleep regressed and the sleep
    // parked for the full 5 seconds.
    expect(elapsed).toBeLessThan(100);
  });

  test('null stopController: abortableSleep degrades to a pure setTimeout (defensive)', async () => {
    // abortableSleep is also reachable from the at-cap backpressure
    // pause; if a future caller invokes it before start() ran, we
    // don't want the missing controller to throw and crash the
    // loop. Pin the degradation: pure setTimeout, no listener wire.
    eventConsumer._test._resetStateForTest();
    expect(eventConsumer._test.getStopController()).toBeNull();
    const start = Date.now();
    await eventConsumer._test.abortableSleep(25);
    const elapsed = Date.now() - start;
    // Roughly the timeout — proves the sleep didn't throw or
    // short-circuit. Generous bound.
    expect(elapsed).toBeGreaterThanOrEqual(20);
    expect(elapsed).toBeLessThan(500);
  });
});

describe('event-consumer: pollLoop error backoff', () => {
  test('pollLoop catches ReceiveMessage errors, logs, sleeps, then continues', async () => {
    // Make the first ReceiveMessage call throw; the second + onward
    // resolve empty. pollLoop's catch should log + backoff (with
    // abortableSleep) + continue. The test asserts the error log
    // fires and pollLoop doesn't propagate (i.e., loopPromise
    // resolves cleanly after stop()).
    let receiveCount = 0;
    sqsMock.on(ReceiveMessageCommand).callsFake(() => {
      receiveCount += 1;
      if (receiveCount === 1) throw new Error('AWS throttling');
      return { Messages: [] };
    });
    eventConsumer._test._setSqsClientForTest(new SQSClient({ region: 'us-east-2' }));
    const client = makeStubClient();

    eventConsumer.start(client);
    // Yield long enough for at least one iteration past the throw.
    // The throw is sync (from callsFake), so the catch fires
    // immediately, then abortableSleep(1000) parks the loop.
    // setImmediate is enough to land in the sleep.
    await new Promise((r) => setImmediate(r));

    // stop() should pre-empt the abortableSleep via the
    // stopController.signal abort event and return promptly.
    const startTime = Date.now();
    await eventConsumer.stop();
    const elapsedMs = Date.now() - startTime;

    expect(logger.error).toHaveBeenCalledWith(
      expect.stringContaining('poll iteration failed'),
      expect.objectContaining({ error: 'AWS throttling' }),
    );
    // Tight bound: abort-wakes-sleep is microtask-level, so a stop()
    // during the 1s POLL_ERROR_BACKOFF_MS should resolve well under
    // 100ms. A regression to ANY synchronous polling would push this
    // toward the 1s budget and surface here.
    expect(elapsedMs).toBeLessThan(100);
  });

  test('pollLoop exits on permanent AWS error (QueueDoesNotExist) — fatal log + process.exit(1)', async () => {
    // Misconfigured-but-set queue URL (typo, wrong region, etc.) was
    // the gap the boot check couldn't catch. Retrying forever would
    // spam logs without resolving anything; fail-fast surfaces the
    // misconfig as ECS task-restart noise in deploy-time monitoring.
    // Test mocks process.exit so the test runner survives; pollLoop's
    // defensive `return` after exit(1) prevents the loop from
    // iterating into the same failure under the mock.
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
      // Yield so pollOnce throws + catch routes through exit().
      await new Promise((r) => setImmediate(r));

      // toHaveBeenCalledTimes(1) — if the defensive return after exit
      // ever gets dropped, the test mock would let the loop iterate
      // into the same failure and exit would be called repeatedly.
      expect(exitSpy).toHaveBeenCalledWith(1);
      expect(exitSpy).toHaveBeenCalledTimes(1);
      expect(logger.error).toHaveBeenCalledWith(
        expect.stringContaining('permanent AWS failure'),
        expect.objectContaining({ code: 'QueueDoesNotExist' }),
      );
      // Transient error log MUST NOT fire on this path — we want one
      // fatal line, not the same misconfig spammed.
      expect(logger.error).not.toHaveBeenCalledWith(
        expect.stringContaining('poll iteration failed'),
        expect.anything(),
      );
      // running stays true after pollLoop's defensive return — pollLoop
      // doesn't manipulate the flag itself; stop() owns that. Explicit
      // assertion locks the contract so a future refactor that flips
      // running inside the error branch (and breaks stop()'s
      // already-stopped guard) fails loudly here.
      expect(eventConsumer._test.isRunning()).toBe(true);
      // stop() returns cleanly even though pollLoop already returned
      // on its own (running is still true; stop() flips it + aborts).
      await eventConsumer.stop();
      expect(eventConsumer._test.isRunning()).toBe(false);
    } finally {
      exitSpy.mockRestore();
    }
  });

  test('pollLoop exits on err.cause-wrapped permanent error (SDK wrapping)', async () => {
    // The unit test for permanentAwsErrorCode pins the cause-walk
    // shape; this pins that the integration path actually invokes
    // the walk, not just the top-level name check. A future SDK
    // refresh that wraps errors in an extra layer must still trip
    // fail-fast — without this, the regression would only surface
    // in prod when the misconfig fires.
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
    // index.js wires `onFatal: () => gracefulShutdown(1)` so the
    // permanent-error path runs the same teardown SIGTERM does
    // (in-flight handler drain, db.close() WAL checkpoint, WebSocket
    // close). Without this, a direct process.exit truncates all of
    // it. Pin that an onFatal callback intercepts; process.exit is
    // NOT called.
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
    // Defense in depth: if gracefulShutdown itself throws (rare —
    // its own try/catch covers routine errors), we still want the
    // task to terminate so ECS surfaces the misconfig. Pin the
    // fallback so a future onFatal regression doesn't silently
    // leave the process running after the fatal log.
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
    // Sister to the sync-throw test. The wired callback in production
    // is `() => gracefulShutdown(1)` which returns a Promise — a
    // sync try/catch alone would miss an async rejection from inside
    // gracefulShutdown. The .catch() on Promise.resolve(onFatalCb())
    // covers that leg.
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
      // Two setImmediate yields: first for pollOnce to throw + catch
      // to call onFatal, second for the rejection's microtask to land
      // on the .catch() and trigger the fallback exit.
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
    // Pins the pre-flight check that turns a class of silent runtime
    // failures (discord.js internal-API drift, stub client passed
    // in) into a loud boot error. Without this, every dispatch
    // would throw inside the try/catch in processMessage, drain the
    // queue, and do nothing — invisible in standard monitoring.
    expect(() => eventConsumer.start({})).toThrow('discord.js internal-API drift');
    expect(() => eventConsumer.start({ actions: {} })).toThrow('discord.js internal-API drift');
    expect(() => eventConsumer.start({ actions: { InteractionCreate: {} } })).toThrow('discord.js internal-API drift');
    expect(() => eventConsumer.start(null)).toThrow('discord.js internal-API drift');
  });

  // The top-level jest.mock('../src/config', ...) returns a literal
  // object that's hoisted into the module cache. Mutating its fields
  // is the simplest way to test config-gated branches without
  // wrestling with jest.isolateModules + jest.doMock interactions
  // (which require the original mock to be a factory, not a literal,
  // to take effect within the isolated scope).
  // try/finally save+restore pattern for mutating the mocked config.
  // jest.replaceProperty would be terser, but only auto-restores when
  // `restoreMocks: true` is set globally — and this repo's
  // jest.config.js intentionally leaves restoreMocks off so several
  // specs can rely on jest.spyOn results persisting across tests
  // (see the comment in jest.config.js). The explicit save/restore
  // here is fully scoped to this test and survives an assertion
  // throw because finally always runs.
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
    // Node's standard AbortError uses code = 'ABORT_ERR' (DOMException-
    // style). The @aws-sdk versions we use have been observed
    // emitting this shape; matching it keeps stop()-triggered aborts
    // silent across SDK refreshes.
    const e3 = new Error('aborted'); e3.code = 'ABORT_ERR';
    expect(isAbortError(e3)).toBe(true);
    // @smithy CanceledError shape.
    const e4 = new Error('canceled'); e4.name = 'CanceledError';
    expect(isAbortError(e4)).toBe(true);
    // Future @aws-sdk wrapper that nests the abort under err.cause.
    const e5 = new Error('Request failed'); e5.cause = { name: 'AbortError' };
    expect(isAbortError(e5)).toBe(true);
    const e6 = new Error('Request failed'); e6.cause = { name: 'CanceledError' };
    expect(isAbortError(e6)).toBe(true);
    // Doubly-nested cause chain — should still match via the
    // recursive walk capped at 5 hops.
    const e7 = new Error('outer');
    e7.cause = { name: 'Wrapper', cause: { name: 'AbortError' } };
    expect(isAbortError(e7)).toBe(true);
    // TimeoutError is the SDK's own request-timeout, NOT our abort.
    // Must land in the error-backoff path so flaky AWS endpoints
    // surface in logs + backoff instead of spinning silently.
    const e8 = new Error('timeout'); e8.name = 'TimeoutError';
    expect(isAbortError(e8)).toBe(false);
    // cause without an abort-shape name doesn't match either.
    const e9 = new Error('Request failed'); e9.cause = { name: 'TimeoutError' };
    expect(isAbortError(e9)).toBe(false);
    // Cyclic cause chain — recursion is depth-capped, doesn't hang.
    const e10 = new Error('cyclic');
    e10.cause = e10;
    expect(isAbortError(e10)).toBe(false);
  });

  test('permanentAwsErrorCode recognizes misconfig shapes, returns null for transient', () => {
    const { permanentAwsErrorCode } = eventConsumer._test;
    // Non-error inputs degrade cleanly.
    expect(permanentAwsErrorCode(null)).toBeNull();
    expect(permanentAwsErrorCode(undefined)).toBeNull();
    expect(permanentAwsErrorCode(new Error('boom'))).toBeNull();

    // SDK-v3 error-class names (error.name).
    const queueGone = Object.assign(new Error('q gone'), { name: 'QueueDoesNotExist' });
    expect(permanentAwsErrorCode(queueGone)).toBe('QueueDoesNotExist');
    const accessDenied = Object.assign(new Error('AD'), { name: 'AccessDeniedException' });
    expect(permanentAwsErrorCode(accessDenied)).toBe('AccessDeniedException');

    // AWS-API-layer codes (error.Code, v2-style, still present on some
    // v3 paths).
    const v2NonExistent = Object.assign(new Error('q gone'), {
      Code: 'AWS.SimpleQueueService.NonExistentQueue',
    });
    expect(permanentAwsErrorCode(v2NonExistent)).toBe('AWS.SimpleQueueService.NonExistentQueue');
    const invalidUrl = Object.assign(new Error('bad url'), { code: 'InvalidQueueUrl' });
    expect(permanentAwsErrorCode(invalidUrl)).toBe('InvalidQueueUrl');

    // Region mismatch surfaces as UnknownEndpoint.
    const badRegion = Object.assign(new Error('endpoint'), { name: 'UnknownEndpoint' });
    expect(permanentAwsErrorCode(badRegion)).toBe('UnknownEndpoint');

    // Bad creds.
    const badCreds = Object.assign(new Error('creds'), { name: 'UnrecognizedClientException' });
    expect(permanentAwsErrorCode(badCreds)).toBe('UnrecognizedClientException');

    // Wrapped error: SDK-style err.cause chain still matches via the walk.
    const wrapped = Object.assign(new Error('wrap'), {
      cause: { name: 'QueueDoesNotExist' },
    });
    expect(permanentAwsErrorCode(wrapped)).toBe('QueueDoesNotExist');

    // Transient errors (throttling, network, timeout) must NOT match —
    // the existing error-backoff handles those.
    const throttle = Object.assign(new Error('rate'), { name: 'ThrottlingException' });
    expect(permanentAwsErrorCode(throttle)).toBeNull();
    const networkErr = Object.assign(new Error('econnreset'), { code: 'ECONNRESET' });
    expect(permanentAwsErrorCode(networkErr)).toBeNull();
    const timeoutErr = Object.assign(new Error('t/o'), { name: 'TimeoutError' });
    expect(permanentAwsErrorCode(timeoutErr)).toBeNull();

    // Cyclic cause chain — bounded by visited-set, doesn't hang.
    const cyclic = new Error('cyclic');
    cyclic.cause = cyclic;
    expect(permanentAwsErrorCode(cyclic)).toBeNull();
  });

  test('pollOnce passes the stopController.signal in the SDK send options', async () => {
    // Pins the contract that the receive call carries an abort signal.
    // aws-sdk-client-mock doesn't propagate HttpHandlerOptions to the
    // mock handler, so we spy on the underlying client.send directly
    // to inspect the second arg. Without this assertion, a refactor
    // that drops `{ abortSignal: ... }` from `sqsClient.send(cmd, options)`
    // wouldn't fail any test — and the graceful-shutdown latency
    // guarantee would silently regress. The signal passed must be
    // the lifetime stopController's signal, not a fresh per-iteration
    // controller.
    sqsMock.on(ReceiveMessageCommand).resolves({ Messages: [] });
    const realClient = new SQSClient({ region: 'us-east-2' });
    const sendSpy = jest.spyOn(realClient, 'send');
    eventConsumer._test._setSqsClientForTest(realClient);
    eventConsumer._test._setStopControllerForTest();

    await eventConsumer._test.pollOnce(makeStubClient());

    expect(sendSpy).toHaveBeenCalled();
    // First call is the ReceiveMessageCommand. Find it explicitly so
    // we don't false-positive on a future change that adds prior
    // commands.
    const receiveCall = sendSpy.mock.calls.find(
      ([cmd]) => cmd instanceof ReceiveMessageCommand,
    );
    expect(receiveCall).toBeDefined();
    const [, options] = receiveCall;
    expect(options).toBeDefined();
    expect(options.abortSignal).toBeDefined();
    // The signal must be live (not pre-aborted) at the time of send.
    expect(options.abortSignal.aborted).toBe(false);
    // The signal MUST be the lifetime stopController's signal — a
    // future regression that constructs a fresh per-iteration
    // controller would re-introduce the indirection the refactor
    // collapsed.
    expect(options.abortSignal).toBe(eventConsumer._test.getStopController().signal);

    sendSpy.mockRestore();
  });

  test('stopController persists across pollOnce iterations (single lifetime controller, not per-iteration)', async () => {
    // The pre-refactor pattern constructed a fresh controller every
    // pollOnce; the AbortSignal refactor consolidated to one lifetime
    // controller shared across iterations. Pin that contract — a
    // regression to per-iteration would lose the abortableSleep wake
    // semantics the refactor was about.
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

    // Aborting the lifetime controller wakes both the in-flight
    // receive AND any in-flight abortableSleep (the contract the
    // refactor pinned).
    ctrlAfterFirst.abort();
    expect(ctrlAfterFirst.signal.aborted).toBe(true);
  });

  test('start() + stop() round-trip; second start logs warn (idempotent)', async () => {
    // ReceiveMessage returns empty immediately so pollLoop iterates
    // quickly between start() and stop().
    sqsMock.on(ReceiveMessageCommand).resolves({ Messages: [] });
    eventConsumer._test._setSqsClientForTest(new SQSClient({ region: 'us-east-2' }));
    const client = makeStubClient();

    eventConsumer.start(client);
    // Second start while running: no-op + warn.
    eventConsumer.start(client);
    expect(logger.warn).toHaveBeenCalledWith(
      expect.stringContaining('start() called while already running'),
    );

    // No event-loop yield between start() and stop() here on purpose.
    // start() returns the pollLoop's promise synchronously but pollLoop's
    // body doesn't begin executing until the microtask queue runs.
    // stop() calls stopController.abort() synchronously, THEN awaits
    // loopPromise. By the time pollLoop's body finally runs in the
    // microtask queue after stop()'s await, signal.aborted is already
    // true and the while-check exits immediately — no pollOnce
    // iteration, no accumulating commandCalls in the SDK mock. The behavior under
    // test is "start + double-start warn + stop idempotent", not the
    // full poll cycle (that's covered by other tests in this file).
    await eventConsumer.stop();
    // Stop is idempotent.
    await eventConsumer.stop();

    // stop()'s finally restores currentBackoffMs to BASE alongside
    // running/loopPromise/stopController. Pins the contract called
    // out in the at-cap backoff describe block (#390).
    expect(eventConsumer._test.getCurrentBackoffMs()).toBe(eventConsumer._test.INFLIGHT_BACKOFF_BASE_MS);
  });

  // A "start() actually registers pollOnce" companion test was
  // considered but skipped: the SDK mock pattern that lets the
  // abort-silent test bound its iterations (throw AbortError so
  // pollLoop catches and the next while-check exits) doesn't
  // translate to a Messages-resolved path — the test either races
  // microtask ordering or accumulates unbounded mock.commandCalls.
  // The "abort silently exits pollLoop" test above already pins
  // that pollLoop's loop body actually runs (asserts receiveCount
  // ≥ 1), which covers the underlying concern.

  test('stop() drains in-flight handlers before resolving (settled within deadline)', async () => {
    // Pins the drain contract: stop() should await the in-flight
    // handler promises captured by trackDispatch before returning,
    // so gracefulShutdown's subsequent db.close() doesn't race a
    // handler's mid-DDB-write. Resolution is deferred to a setTimeout
    // so the .finally microtasks haven't fired by the time stop()
    // reaches its drain branch — the drain enters with promises
    // still pending, awaits them via allSettled, logs "drain
    // complete" before stop() resolves.
    sqsMock.on(ReceiveMessageCommand).resolves({ Messages: [] });
    eventConsumer._test._setSqsClientForTest(new SQSClient({ region: 'us-east-2' }));
    // Tight deadline so the test stays fast even if a CI host is
    // slow — the resolution timer below fires well inside this
    // budget, so the deadline-elapsed branch shouldn't trigger.
    eventConsumer._test._setDrainDeadlineForTest(500);
    const client = makeStubClient();
    eventConsumer.start(client);

    // Register two resolvable promises via trackDispatch.
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

    // Schedule resolution to fire DURING the drain wait. setTimeout
    // (not a synchronous resolve) so the .finally microtasks haven't
    // already run by the time stop()'s drain branch executes.
    setTimeout(() => resolvers.forEach((r) => r('done')), 20);
    await eventConsumer.stop();

    expect(logger.info).toHaveBeenCalledWith(
      'Event consumer: drain complete',
      expect.objectContaining({ count: 2 }),
    );
    expect(eventConsumer._test.getInFlightCount()).toBe(0);
  });

  test('stop() returns within deadline when handlers do not settle', async () => {
    // Pins the deadline contract: stop() must NOT block indefinitely
    // on a never-resolving handler. Shrink the deadline so the test
    // doesn't wait the full 3s default; assert the warn log fires
    // and stop() resolves within a margin of the deadline.
    sqsMock.on(ReceiveMessageCommand).resolves({ Messages: [] });
    eventConsumer._test._setSqsClientForTest(new SQSClient({ region: 'us-east-2' }));
    eventConsumer._test._setDrainDeadlineForTest(50);

    const client = makeStubClient();
    eventConsumer.start(client);

    // Register a never-resolving promise so the drain hits its deadline.
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
    // Stop() returned even though the handler never settled. Allow
    // a generous upper bound for CI flakiness; the assertion is
    // "stop didn't hang past the deadline by an order of magnitude."
    expect(elapsed).toBeLessThan(50 * 20);
  });

  test('getDrainDeadlineMs reflects live value after _setDrainDeadlineForTest', () => {
    // The deadline is mutable so tests can shrink it without
    // polluting prod with a config knob. The `_test` export is a
    // getter (not a value-snapshot) so introspection reflects the
    // live variable — pin that contract so a future refactor back
    // to a value-export trips this test.
    expect(eventConsumer._test.getDrainDeadlineMs()).toBe(3000);
    eventConsumer._test._setDrainDeadlineForTest(50);
    expect(eventConsumer._test.getDrainDeadlineMs()).toBe(50);
  });

  test('stop() drains even when loopPromise rejects (loop crash does not skip drain)', async () => {
    // Pin the round-3 cr fix: a pollLoop crash (rare — its own catch
    // covers routine errors) used to skip the drain because the
    // drain block was inside the same try as `await loopPromise`.
    // Restructured so the loop-crash log and the drain are
    // independent: the trackDispatch set's state is coherent after
    // a loop crash (additions are synchronous around emits), so
    // draining is meaningful regardless of loop outcome.
    sqsMock.on(ReceiveMessageCommand).resolves({ Messages: [] });
    eventConsumer._test._setSqsClientForTest(new SQSClient({ region: 'us-east-2' }));
    eventConsumer._test._setDrainDeadlineForTest(500);
    const client = makeStubClient();
    eventConsumer.start(client);

    // Replace the loopPromise with one that rejects so the inner
    // try/catch fires (simulating a pollLoop crash). The drain
    // should still run because it's outside that inner try.
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
    // Pins the no-op path: an idle worker stop()s without firing the
    // drain logs (drainCount > 0 gate). Common case in production —
    // most stops will land between bursts when no handlers are pending.
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
  // Test helpers. Pulled up here so the tests below stay focused on
  // intent rather than re-stating boilerplate.

  // Flush queued microtasks. The trackDispatch chain is exactly
  // two hops today: `.finally` (decrement) then `.catch` (error log
  // or pass-through). Default n=3 = 2 hops + 1 margin, so a future
  // chain extension (e.g., another `.then` between finally and catch)
  // doesn't require revisiting every test. The +1 margin is the
  // load-bearing slack — drop it only if you re-derive the chain
  // depth and confirm no implicit hop sneaks in.
  async function flushMicrotasks(n = 3) {
    for (let i = 0; i < n; i += 1) {
      // eslint-disable-next-line no-await-in-loop -- sequential by design
      await new Promise((r) => setImmediate(r));
    }
  }

  test('trackDispatch is a no-op when isWorkerDispatch is false (gateway mode)', () => {
    // Pins the contract that the gateway-WS-driven path doesn't
    // count against the worker's cap. The flag stays false unless
    // the consumer's processMessage explicitly sets it; a stray
    // call from the listener in gateway-only mode increments
    // nothing.
    expect(eventConsumer._test.isWorkerDispatching()).toBe(false);
    expect(eventConsumer._test.getInFlightCount()).toBe(0);
    eventConsumer.trackDispatch(Promise.resolve('ignored'));
    expect(eventConsumer._test.getInFlightCount()).toBe(0);
  });

  test('trackDispatch handles non-promise inputs without throwing', () => {
    // The listener returns undefined for unrouted interaction types.
    // trackDispatch must accept that without throwing or counting.
    eventConsumer.trackDispatch(undefined);
    eventConsumer.trackDispatch(null);
    eventConsumer.trackDispatch('not a promise');
    eventConsumer.trackDispatch({ then: 'not-a-fn' });
    expect(eventConsumer._test.getInFlightCount()).toBe(0);
  });

  test('processMessage sets isWorkerDispatch true around handle() and clears in finally', async () => {
    // The flag must be true exactly during the synchronous
    // client.actions.InteractionCreate.handle(data) call so the
    // listener's emit fires inside the window. Verified by spying
    // on handle and asserting the flag's value inside the spy.
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
    // After processMessage returns, the flag is back to false.
    expect(eventConsumer._test.isWorkerDispatching()).toBe(false);
  });

  test('processMessage clears isWorkerDispatch even when handle() throws', async () => {
    // Pin the finally — without it, a reconstruction throw would
    // leave the flag stuck at true, and a subsequent gateway-WS
    // dispatch in combined mode would be incorrectly counted
    // against the worker cap.
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
    // Counter should not have been incremented either — handle()
    // threw before the listener could fire its return value into
    // trackDispatch. Pinning this ensures a future refactor that
    // moves the increment outside the listener can't leak a slot.
    expect(eventConsumer._test.getInFlightCount()).toBe(0);
  });

  test('trackDispatch increments then decrements on promise resolve', async () => {
    // Simulate a consumer-driven dispatch by manually flipping the
    // flag (mirrors what processMessage does synchronously around
    // handle()). Register a pending promise; assert the counter
    // ticks up. Resolve it; assert the counter ticks back down.
    let resolveHandler;
    const handlerPromise = new Promise((r) => { resolveHandler = r; });

    // Mirror processMessage's flag wrap.
    withWorkerDispatch(() => eventConsumer.trackDispatch(handlerPromise));

    expect(eventConsumer._test.getInFlightCount()).toBe(1);
    resolveHandler('done');
    // Wait for the finally callback to run (microtask boundary).
    await handlerPromise;
    await flushMicrotasks();
    expect(eventConsumer._test.getInFlightCount()).toBe(0);
  });

  test('trackDispatch decrements on promise rejection', async () => {
    // Same as above but reject the promise; the finally should
    // still decrement the counter so a failing handler doesn't
    // leak a slot in the cap.
    let rejectHandler;
    const handlerPromise = new Promise((_resolve, reject) => { rejectHandler = reject; });
    // Pre-attach a catch so the rejection doesn't surface as an
    // unhandledRejection in the test runtime.
    handlerPromise.catch(() => { /* absorbed */ });

    withWorkerDispatch(() => eventConsumer.trackDispatch(handlerPromise));

    expect(eventConsumer._test.getInFlightCount()).toBe(1);
    rejectHandler(new Error('handler failed'));
    await flushMicrotasks();
    expect(eventConsumer._test.getInFlightCount()).toBe(0);
  });

  test('trackDispatch with an already-settled promise still drains the counter', async () => {
    // A handler that completes synchronously (or returns a resolved
    // promise from an early short-circuit) lands here. The `.finally`
    // callback still runs on the microtask boundary, so the
    // increment-then-decrement holds — but it's worth pinning the
    // contract so a future refactor that tries to skip registration
    // for already-settled promises (as a "perf optimization") is
    // caught.
    withWorkerDispatch(() => {
      eventConsumer.trackDispatch(Promise.resolve('already-done'));
    });

    // Increment was synchronous (inside withWorkerDispatch).
    expect(eventConsumer._test.getInFlightCount()).toBe(1);
    // Decrement is deferred to the .finally microtask.
    await flushMicrotasks();
    expect(eventConsumer._test.getInFlightCount()).toBe(0);
  });

  test('trackDispatch logs handler rejections (preserves error visibility)', async () => {
    // Regression for cr feedback on PR #389: attaching `.finally()`
    // in trackDispatch counts as a handler for Node's unhandled-
    // rejection bookkeeping, so a rejection that pre-PR would have
    // surfaced at `process.on('unhandledRejection', ...)` in
    // src/index.js gets absorbed in the trailing `.catch`. Pin the
    // contract that the catch logs at error so handler bugs still
    // produce a CloudWatch signal.
    const handlerPromise = Promise.reject(new Error('handler boom'));
    // Pre-attach to absorb the rejection in the test runtime as a
    // separate chain; trackDispatch's logging path is independent.
    handlerPromise.catch(() => { /* absorbed */ });

    withWorkerDispatch(() => eventConsumer.trackDispatch(handlerPromise));

    // Allow the .finally + .catch microtasks to flush.
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
    // PR #391 replaced the counter+underflow-guard pattern with a
    // Set<Promise>. Set.delete on a non-member is a no-op, so a
    // stale .finally callback firing AFTER _resetStateForTest cleared
    // the set silently does nothing — no error log, no negative size.
    // This pins the "set semantics absorb test pollution" property
    // that justified removing the fail-loud underflow guard.
    let resolveHandler;
    const handlerPromise = new Promise((r) => { resolveHandler = r; });
    withWorkerDispatch(() => eventConsumer.trackDispatch(handlerPromise));
    expect(eventConsumer._test.getInFlightCount()).toBe(1);

    eventConsumer._test._resetStateForTest();
    expect(eventConsumer._test.getInFlightCount()).toBe(0);
    resolveHandler('done');
    await flushMicrotasks();

    // No "underflow" log fired — the new path can't reach that branch.
    expect(logger.error).not.toHaveBeenCalledWith(
      expect.stringContaining('underflow'),
      expect.anything(),
    );
    expect(eventConsumer._test.getInFlightCount()).toBe(0);
  });

  test('pollOnce early-returns + backs off when in-flight at cap', async () => {
    // When inFlightCount >= MAX_INFLIGHT_HANDLERS, pollOnce must
    // skip the receive call and sleep at least the base backoff to
    // let handlers drain. Pins that the cap is enforced + the
    // receive path is bypassed.
    const cap = eventConsumer._test.MAX_INFLIGHT_HANDLERS;

    // Manually crank inFlightCount up to cap via the tracker. Use
    // pending promises so the count stays elevated for the duration
    // of the test.
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

    // The receive was NOT called (cap blocked it).
    expect(sqsMock.commandCalls(ReceiveMessageCommand)).toHaveLength(0);
    // First at-cap poll in the streak warns (transition signal),
    // not debug — see the state-machine throttle in pollOnce.
    // Assert against the module-level constant so a wording drift
    // in event-consumer.js fails the test instead of silently
    // breaking CloudWatch alarms that grep the literal string.
    expect(logger.warn).toHaveBeenCalledWith(
      eventConsumer._test.AT_CAP_PAUSE_WARN_MSG,
      expect.objectContaining({ inFlight: cap, cap }),
    );
    expect(eventConsumer._test.isAtCapPauseLogged()).toBe(true);
  });

  // Helper: kick off pollOnce and immediately advance fake timers so
  // the at-cap abortableSleep resolves in test time, not wall time.
  // Without this, sustained-at-cap tests pay 100+200+400+800+1600 =
  // 3.1s of real-time sleep per iteration sequence. setImmediate +
  // queueMicrotask are excluded from fake-timer wrapping so the
  // existing flushMicrotasks() + Promise scheduling stay unaffected.
  async function pollOnceFast(client) {
    const p = withMockedSqs(() => eventConsumer._test.pollOnce(client));
    await jest.runOnlyPendingTimersAsync();
    return p;
  }

  test('pollOnce stays silent during a sustained at-cap streak (one warn at entry, no per-iteration noise)', async () => {
    // The entry warn + the eventual exit info already bookend the
    // pause for operators — steady-state at-cap is intentionally
    // silent. Pre-#390, this test asserted a per-iteration debug
    // log; that pattern is gone now (it spammed ~10×/s per worker
    // with no operational value beyond what entry+exit convey).
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

      // Exactly one warn fired (the entry), no debugs across the streak.
      expect(logger.warn.mock.calls.length - warnBaseline).toBe(1);
      expect(logger.debug.mock.calls.length - debugBaseline).toBe(0);
    } finally {
      jest.useRealTimers();
    }
  });

  test('pollOnce: at-cap backoff doubles each iteration up to the ceiling', async () => {
    // Exponential backoff: 100ms → 200 → 400 → 800 → 1600 (max).
    // Cuts wake rate from 10/s to ~0.6/s under sustained at-cap
    // without significantly delaying recovery (a release in the
    // middle of a backoff still wakes via the next below-cap check).
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

      // One more iteration past the ceiling: still pinned at MAX.
      await pollOnceFast(client);
      expect(eventConsumer._test.getCurrentBackoffMs()).toBe(eventConsumer._test.INFLIGHT_BACKOFF_MAX_MS);
    } finally {
      jest.useRealTimers();
    }
  });

  test('pollOnce: post-streak resets via _resetStateForTest restores base', async () => {
    // _resetStateForTest is the beforeEach harness path. stop()'s
    // finally also resets currentBackoffMs — that branch is asserted
    // in the start()+stop() round-trip test in the start/stop
    // lifecycle describe block.
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

      // Drive to MAX (4 doublings: 100 → 200 → 400 → 800 → 1600).
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
    // Pins the recovery path: after an at-cap streak, dropping
    // below cap fires the release-info log so operators can pair
    // pause-start with pause-end events in CloudWatch. Uses
    // resolvable promises so the drain runs through trackDispatch's
    // real .finally decrement path — no test-only setter shortcut.
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

    // Enter at-cap state (warn fires, gate set, backoff doubled).
    await withMockedSqs(() => eventConsumer._test.pollOnce(client));
    expect(eventConsumer._test.isAtCapPauseLogged()).toBe(true);
    expect(eventConsumer._test.getCurrentBackoffMs()).toBe(200);

    // Drain all handlers: resolve every tracked promise, then let
    // the .finally microtask chain flush so inFlightCount drops to 0.
    resolvers.forEach((r) => r('done'));
    await flushMicrotasks();
    expect(eventConsumer._test.getInFlightCount()).toBe(0);

    // Below-cap poll: release-info fires, gate clears, backoff
    // resets to base so the next at-cap entry starts responsive.
    await withMockedSqs(() => eventConsumer._test.pollOnce(client));
    expect(logger.info).toHaveBeenCalledWith(
      eventConsumer._test.AT_CAP_RELEASED_INFO_MSG,
      expect.objectContaining({ cap }),
    );
    expect(eventConsumer._test.isAtCapPauseLogged()).toBe(false);
    expect(eventConsumer._test.getCurrentBackoffMs()).toBe(eventConsumer._test.INFLIGHT_BACKOFF_BASE_MS);
  });

  test('pollOnce proceeds with receive when below cap', async () => {
    // Sanity-check the other side of the gate — when inFlightCount
    // is below the cap, pollOnce calls ReceiveMessage normally.
    expect(eventConsumer._test.getInFlightCount()).toBeLessThan(eventConsumer._test.MAX_INFLIGHT_HANDLERS);

    sqsMock.on(ReceiveMessageCommand).resolves({ Messages: [] });
    const client = makeStubClient();

    await withMockedSqs(() => eventConsumer._test.pollOnce(client));

    expect(sqsMock.commandCalls(ReceiveMessageCommand)).toHaveLength(1);
  });

  test('MAX_INFLIGHT_HANDLERS defaults to 100', () => {
    // Pins the default — operators reading the .env.example see
    // this number and may rely on it for their soak headroom math.
    expect(eventConsumer._test.MAX_INFLIGHT_HANDLERS).toBe(100);
  });

  test('processMessage flag-wrap has no `await` between isWorkerDispatch toggles (static invariant)', () => {
    // Load-bearing invariant: the synchronous flag-wrap around
    // `client.actions.InteractionCreate.handle(data)` in
    // processMessage is correct ONLY because there's no `await`
    // between `isWorkerDispatch = true` and the matching
    // `isWorkerDispatch = false` in the finally. An `await` inside
    // the try/finally would let a concurrent processMessage (running
    // under Promise.allSettled in pollOnce) observe a leaked-true
    // flag and silently miscount a gateway-WS-driven dispatch as
    // worker-driven in combined mode.
    //
    // Test by reading the source: scan the slice of event-consumer.js
    // between `// FLAG-WRAP-START` and `// FLAG-WRAP-END` sentinels
    // bracketing the wrap, and assert no `await` token appears.
    // Approximate (no JS parse), but pins the constraint better than
    // a comment alone — a future editor who adds `await logger.x()`
    // inside the block trips this test. Sentinel-based bracketing
    // survives file reordering or future additional flag assignments
    // elsewhere in the module.
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
    // Also pin uniqueness — if the sentinels appear more than once,
    // a future refactor may have copied the block and the slice no
    // longer represents a single contiguous wrap.
    expect(src.indexOf(startMarker, startIdx + startMarker.length)).toBe(-1);
    expect(src.indexOf(endMarker, endIdx + endMarker.length)).toBe(-1);

    const block = src.slice(startIdx, endIdx);
    // Strip BOTH line comments and block comments so a documenting
    // reference to `await` in either form doesn't false-positive.
    // Template literals containing the word `await` aren't expected
    // in the wrap (we'd notice during code review of a tight 25-line
    // block), so we don't try to strip those — defense-in-depth has
    // diminishing returns past comments.
    const stripped = block
      .replace(/\/\*[\s\S]*?\*\//g, '') // block comments first (multi-line)
      .replace(/\/\/[^\n]*/g, ''); // then line comments
    expect(stripped).not.toMatch(/\bawait\b/);
  });

  describe('validateInflightCap (soft-floor warning)', () => {
    // The env-int-parsing IIFEs that used to live here moved to
    // config.intEnv (validated in tests/config-int-env.test.js).
    // What remains in event-consumer.js is the consumer-specific
    // soft-floor warning that fires when the cap is below
    // RECEIVE_MAX_MESSAGES — the cap is accepted (operators may
    // want it for hard-rate-limit testing) but the warn helps the
    // booted ceiling match operator intent.
    //
    // Extracted as a function so we can call it directly with any
    // value rather than fighting the module-load mock plumbing.
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
    // Single triple-numeric version only — rejects ranges (`14.25.1
    // - 14.25.9`), `||` lists, hyphen ranges, x-spec (`14.x`), and
    // prefix specifiers (`~14.25.1`, `^14.25.1`). The consumer
    // depends on `client.actions.InteractionCreate.handle` internal
    // API; any matcher loose enough to admit a minor bump silently
    // is the regression we're guarding against.
    expect(decl).toMatch(/^\d+\.\d+\.\d+$/);
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
