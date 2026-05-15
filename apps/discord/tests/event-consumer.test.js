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
  // Full reset of the consumer's module-level singleton state so
  // each test starts from a known shape. seenEventIds, sqsClient,
  // running/stopping/loopPromise, receiveAbortController — all
  // cleared. Without this, test ordering would matter (e.g., a
  // future test before the start/stop round-trip case would
  // observe whatever `running` was left at).
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
    // call we also fire-and-forget call stop() so stopping=true is
    // set synchronously (stop() flips it before any await). pollLoop's
    // next while-check exits cleanly. Total: 2 mock calls, 2 catches,
    // both go through the silent continue branch.
    const setIntervalSpy = jest.spyOn(global, 'setInterval');
    const baselineIntervals = setIntervalSpy.mock.calls.filter(([, ms]) => ms === 50).length;

    let receiveCount = 0;
    sqsMock.on(ReceiveMessageCommand).callsFake(() => {
      receiveCount += 1;
      if (receiveCount === 2) {
        // Fire-and-forget; stop() sets stopping=true synchronously
        // before any await, so by the time pollLoop's catch handles
        // this iteration's AbortError and re-checks while, the
        // loop exits.
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
    // No abortableSleep call — its 50 ms-tick setInterval count
    // unchanged from baseline.
    const finalIntervals = setIntervalSpy.mock.calls.filter(([, ms]) => ms === 50).length;
    expect(finalIntervals).toBe(baselineIntervals);

    setIntervalSpy.mockRestore();
  });
});

describe('event-consumer: abortableSleep', () => {
  test('timeout-wins-race path clears the polling interval (no leak)', async () => {
    // Pins the fix for the setInterval leak: when setTimeout fires
    // first (the common case — backoff completes without a stop()),
    // the polling setInterval MUST be cleared inside the resolve
    // handler. Without the fix, every error-backoff iteration would
    // accumulate one orphan interval ticking every 50 ms forever.
    //
    // Spy on clearInterval and assert it was called for the handle
    // that setInterval returned. Real timers (not jest fake) so the
    // 30 ms sleep completes on its own; smaller than POLL_ERROR_BACKOFF_MS
    // (1000) to keep the test fast.
    const setIntervalSpy = jest.spyOn(global, 'setInterval');
    const clearIntervalSpy = jest.spyOn(global, 'clearInterval');
    try {
      await eventConsumer._test.abortableSleep(30);
      // Count the abortableSleep-created intervals (50ms polling
      // tick) and assert each was cleared. Other intervals in the
      // test runtime may also be present — filter by the 50ms tick.
      const created = setIntervalSpy.mock.calls.filter(([, ms]) => ms === 50);
      expect(created.length).toBeGreaterThanOrEqual(1);
      const intervalHandles = setIntervalSpy.mock.results
        .filter((_r, i) => setIntervalSpy.mock.calls[i][1] === 50)
        .map((r) => r.value);
      for (const handle of intervalHandles) {
        expect(clearIntervalSpy).toHaveBeenCalledWith(handle);
      }
    } finally {
      setIntervalSpy.mockRestore();
      clearIntervalSpy.mockRestore();
    }
  });

  // The stopping-wins-race path is exercised end-to-end by the
  // pollLoop error-backoff test below (which asserts stop() returns
  // in < 500 ms while a 1 s abortableSleep is in flight). No
  // separate unit test needed here.
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

    // stop() should pre-empt the abortableSleep via the `stopping`
    // flag check and return promptly.
    const startTime = Date.now();
    await eventConsumer.stop();
    const elapsedMs = Date.now() - startTime;

    expect(logger.error).toHaveBeenCalledWith(
      expect.stringContaining('poll iteration failed'),
      expect.objectContaining({ error: 'AWS throttling' }),
    );
    // Well under the 1s POLL_ERROR_BACKOFF_MS — confirms
    // abortableSleep honored the stopping flag.
    expect(elapsedMs).toBeLessThan(500);
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

  test('pollOnce passes an abortSignal in the SDK send options', async () => {
    // Pins the contract that the receive call carries an abort signal.
    // aws-sdk-client-mock doesn't propagate HttpHandlerOptions to the
    // mock handler, so we spy on the underlying client.send directly
    // to inspect the second arg. Without this assertion, a refactor
    // that drops `{ abortSignal: ... }` from `sqsClient.send(cmd, options)`
    // wouldn't fail any test — and the graceful-shutdown latency
    // guarantee would silently regress.
    sqsMock.on(ReceiveMessageCommand).resolves({ Messages: [] });
    const realClient = new SQSClient({ region: 'us-east-2' });
    const sendSpy = jest.spyOn(realClient, 'send');
    eventConsumer._test._setSqsClientForTest(realClient);

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

    sendSpy.mockRestore();
  });

  test('pollOnce installs a fresh receiveAbortController that can be aborted', async () => {
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

    // No event-loop yield between start() and stop() here on purpose.
    // start() returns the pollLoop's promise synchronously but pollLoop's
    // body doesn't begin executing until the microtask queue runs.
    // stop() flips stopping=true synchronously, THEN awaits loopPromise.
    // By the time pollLoop's body finally runs in the microtask queue
    // after stop()'s await, stopping is already true and the
    // while-check exits immediately — no pollOnce iteration, no
    // accumulating commandCalls in the SDK mock. The behavior under
    // test is "start + double-start warn + stop idempotent", not the
    // full poll cycle (that's covered by other tests in this file).
    await eventConsumer.stop();
    // Stop is idempotent.
    await eventConsumer.stop();
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
});

describe('event-consumer: backpressure (in-flight handler cap)', () => {
  // Test helpers. Pulled up here so the tests below stay focused on
  // intent rather than re-stating boilerplate.

  // Flush queued microtasks. `.finally → .catch` in trackDispatch
  // is two microtask hops; default n=3 leaves a one-tick margin so a
  // future chain extension doesn't require revisiting every test.
  async function flushMicrotasks(n = 3) {
    for (let i = 0; i < n; i += 1) {
      // eslint-disable-next-line no-await-in-loop -- sequential by design
      await new Promise((r) => setImmediate(r));
    }
  }

  // Run `fn` with isWorkerDispatch flipped true (matches the
  // synchronous flag-wrap processMessage performs around handle()),
  // then restore. Six tests repeat this pattern; helper keeps the
  // setup intent visible and the flag flip impossible to miss.
  function withWorkerDispatch(fn) {
    eventConsumer._test._setWorkerDispatchingForTest(true);
    try {
      return fn();
    } finally {
      eventConsumer._test._setWorkerDispatchingForTest(false);
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
      expect.objectContaining({ error: 'handler boom' }),
    );
  });

  test('pollOnce early-returns + backs off when in-flight at cap', async () => {
    // When inFlightCount >= MAX_INFLIGHT_HANDLERS, pollOnce must
    // skip the receive call and sleep INFLIGHT_BACKOFF_MS to let
    // handlers drain. Pins that the cap is enforced + the receive
    // path is bypassed.
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

  test('pollOnce only warns once per at-cap streak (debug for subsequent polls)', async () => {
    // Pins the throttle: a sustained at-cap condition produces one
    // warn at the entry transition and debug lines for the rest of
    // the streak. Without this, a slow downstream wedge would spam
    // logger.warn ~10x/s per worker.
    const cap = eventConsumer._test.MAX_INFLIGHT_HANDLERS;
    withWorkerDispatch(() => {
      for (let i = 0; i < cap; i += 1) {
        eventConsumer.trackDispatch(new Promise(() => {})); // never resolves
      }
    });

    sqsMock.on(ReceiveMessageCommand).resolves({ Messages: [] });
    const client = makeStubClient();

    // First poll: warn fires.
    await withMockedSqs(() => eventConsumer._test.pollOnce(client));
    expect(logger.warn).toHaveBeenCalledTimes(1);
    expect(logger.debug).not.toHaveBeenCalledWith(
      eventConsumer._test.AT_CAP_PAUSE_DEBUG_MSG,
      expect.anything(),
    );

    // Subsequent polls in the same streak: debug only, no extra warn.
    await withMockedSqs(() => eventConsumer._test.pollOnce(client));
    await withMockedSqs(() => eventConsumer._test.pollOnce(client));
    expect(logger.warn).toHaveBeenCalledTimes(1); // still 1
    expect(logger.debug).toHaveBeenCalledWith(
      eventConsumer._test.AT_CAP_PAUSE_DEBUG_MSG,
      expect.objectContaining({ inFlight: cap, cap }),
    );
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

    // Enter at-cap state (warn fires, gate is set).
    await withMockedSqs(() => eventConsumer._test.pollOnce(client));
    expect(eventConsumer._test.isAtCapPauseLogged()).toBe(true);

    // Drain all handlers: resolve every tracked promise, then let
    // the .finally microtask chain flush so inFlightCount drops to 0.
    resolvers.forEach((r) => r('done'));
    await flushMicrotasks();
    expect(eventConsumer._test.getInFlightCount()).toBe(0);

    // Below-cap poll: release-info fires, gate clears.
    await withMockedSqs(() => eventConsumer._test.pollOnce(client));
    expect(logger.info).toHaveBeenCalledWith(
      eventConsumer._test.AT_CAP_RELEASED_INFO_MSG,
      expect.objectContaining({ cap }),
    );
    expect(eventConsumer._test.isAtCapPauseLogged()).toBe(false);
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
    // between the `isWorkerDispatch = true` assignment and the
    // `isWorkerDispatch = false` in the matching finally and assert
    // no `await` token appears. Approximate (no JS parse), but pins
    // the constraint better than a comment alone — a future editor
    // who adds `await logger.x()` inside the block trips this test.
    const src = require('fs').readFileSync(
      require('path').resolve(__dirname, '../src/event-consumer.js'),
      'utf8',
    );
    const startIdx = src.indexOf('isWorkerDispatch = true');
    const endIdx = src.indexOf('isWorkerDispatch = false', startIdx);
    expect(startIdx).toBeGreaterThan(0);
    expect(endIdx).toBeGreaterThan(startIdx);
    const block = src.slice(startIdx, endIdx);
    // Strip line comments so a documenting reference to `await` in a
    // comment doesn't false-positive. Block-comments inside the
    // wrap aren't expected; we keep the regex simple.
    const stripped = block.replace(/\/\/[^\n]*/g, '');
    expect(stripped).not.toMatch(/\bawait\b/);
  });

  describe('MAX_INFLIGHT_HANDLERS env validation (module-load IIFE)', () => {
    // The IIFE that parses QURL_BOT_MAX_INFLIGHT_HANDLERS runs at
    // module-load. To exercise it under varying env values we
    // re-require the module inside jest.isolateModules with the env
    // var stubbed, capture console.warn output, and assert the
    // resolved value + warning content. Pins the validation branch
    // that production depends on for fail-loud-on-typo behavior.
    function withIsolatedEnv(envValue, run) {
      jest.isolateModules(() => {
        const prev = process.env.QURL_BOT_MAX_INFLIGHT_HANDLERS;
        const origConsoleWarn = console.warn;
        const warns = [];
        console.warn = (...args) => warns.push(args.join(' '));
        try {
          if (envValue === undefined) {
            delete process.env.QURL_BOT_MAX_INFLIGHT_HANDLERS;
          } else {
            process.env.QURL_BOT_MAX_INFLIGHT_HANDLERS = envValue;
          }
          const fresh = require('../src/event-consumer');
          run(fresh, warns);
        } finally {
          console.warn = origConsoleWarn;
          if (prev === undefined) {
            delete process.env.QURL_BOT_MAX_INFLIGHT_HANDLERS;
          } else {
            process.env.QURL_BOT_MAX_INFLIGHT_HANDLERS = prev;
          }
        }
      });
    }

    test.each([
      ['100abc', 'trailing garbage'],
      ['-5', 'negative integer'],
      ['0', 'zero'],
      ['1.5', 'non-integer'],
      ['Infinity', 'infinity literal'],
      ['NaN', 'NaN literal'],
      ['abc', 'non-numeric'],
      [' ', 'whitespace'],
    ])('rejects %p (%s) and falls back to default', (envValue) => {
      withIsolatedEnv(envValue, (fresh, warns) => {
        expect(fresh._test.MAX_INFLIGHT_HANDLERS).toBe(100);
        expect(warns.some((w) => w.includes('QURL_BOT_MAX_INFLIGHT_HANDLERS') && w.includes('rejected'))).toBe(true);
      });
    });

    test.each([
      ['50', 50],
      ['200', 200],
      ['1', 1],
    ])('accepts %p as %i', (envValue, expected) => {
      withIsolatedEnv(envValue, (fresh, warns) => {
        expect(fresh._test.MAX_INFLIGHT_HANDLERS).toBe(expected);
        // No warning for valid values.
        expect(warns.some((w) => w.includes('rejected'))).toBe(false);
      });
    });

    test('unset env var resolves to default without warning', () => {
      withIsolatedEnv(undefined, (fresh, warns) => {
        expect(fresh._test.MAX_INFLIGHT_HANDLERS).toBe(100);
        expect(warns).toHaveLength(0);
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
