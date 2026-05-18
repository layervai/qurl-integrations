/**
 * Unit tests for src/event-publisher.js — the SQS producer for the
 * gateway tier (zero-downtime design, Pillar 1). Pairs with
 * tests/event-consumer.test.js (worker side).
 *
 * Covers:
 *   - publish(): INTERACTION_CREATE filter (op/t both load-bearing)
 *   - envelope shape (eventType, shardId, data, event_id,
 *     published_at_ms) matches event-consumer.js's contract
 *   - SendMessage call: QueueUrl + JSON body
 *   - start/stop lifecycle: idempotency, flag/queue-url guards
 *   - send-failure logging (kind: 'unhandledRejection' tag parity
 *     with consumer + global handler)
 *   - drain on stop(): allSettled-vs-deadline race outcomes
 *   - pre-start drop (race during boot before login resolves)
 *
 * Does NOT cover:
 *   - Real SQS behavior (throttling, retry, message-attribute
 *     handling) — integration territory.
 *   - Cross-process e2e latency — that requires both tiers running
 *     against a real queue; smoke-suite territory post-PR-10.
 */

jest.mock('../src/config', () => ({
  ENABLE_EVENT_SHIPPER: true,
  QURL_BOT_EVENTS_QUEUE_URL: 'https://sqs.us-east-2.amazonaws.com/123/qurl-bot-events',
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
const { SQSClient, SendMessageCommand } = require('@aws-sdk/client-sqs');

const sqsMock = mockClient(SQSClient);

const eventPublisher = require('../src/event-publisher');
const logger = require('../src/logger');

// AWS_REGION setup: event-publisher's createSqsClient() throws if
// AWS_REGION isn't set, and `start()` calls it lazily. Most tests
// inject a mock client via `_setSqsClientForTest` BEFORE start(),
// but the start/stop lifecycle tests call start() directly. Seed a
// fake region so the suite passes locally without depending on CI
// env. Restore the original value in afterAll so cross-suite state
// stays clean (jest --runInBand shares the process env).
let originalAwsRegion;
beforeAll(() => {
  originalAwsRegion = process.env.AWS_REGION;
  if (!process.env.AWS_REGION) {
    process.env.AWS_REGION = 'us-east-2';
  }
});
afterAll(() => {
  if (originalAwsRegion === undefined) {
    delete process.env.AWS_REGION;
  } else {
    process.env.AWS_REGION = originalAwsRegion;
  }
});

beforeEach(() => {
  sqsMock.reset();
  jest.clearAllMocks();
  eventPublisher._test._resetStateForTest();
});

// Inject the mocked SQSClient (matches the consumer test's pattern).
// aws-sdk-client-mock intercepts at the SDK-command level, so any
// SQSClient instance routes through the mock; we construct one
// explicitly and inject it via the DI hook because start() lazy-
// constructs and most tests call publish() directly.
function withMockedSqs(fn) {
  const realClient = new SQSClient({ region: 'us-east-2' });
  eventPublisher._test._setSqsClientForTest(realClient);
  return fn();
}

// Helper: build a raw discord.js gateway packet shape.
// op=0 is GATEWAY_DISPATCH; t is the event name; s is the per-shard
// sequence; d is the payload.
function rawPacket({ op = 0, t = 'INTERACTION_CREATE', s = 1, d = {} } = {}) {
  return { op, t, s, d };
}

// publish() awaits no promise but submits a detached SendMessage.
// Tests that assert against SendMessage must yield the event loop
// once for the .send() promise to settle. One macrotask flush
// (setImmediate) is enough for the current 1-deep promise chain:
// the .send() resolve, the .finally() that removes from
// inFlightSends, and the .catch() that would fire on rejection.
// If a future change adds another `.then(...).then(...)` layer
// inside publish() (e.g., post-send metric emission), tests
// asserting against the deepest layer's effect will need a second
// flush — or convert to a `flushPromises` helper that drains in
// a loop. Today's depth-1 assumption is pinned by the assertion
// shapes below.
function flushMicro() {
  return new Promise((resolve) => setImmediate(resolve));
}

describe('event-publisher: publish filter', () => {
  test('INTERACTION_CREATE dispatch → SendMessage called once', async () => {
    sqsMock.on(SendMessageCommand).resolves({ MessageId: 'sqs-1' });
    eventPublisher.start();
    withMockedSqs(() => {
      eventPublisher.publish(rawPacket({
        s: 42,
        d: { type: 2, data: { name: 'qurl' }, id: 'i-1' },
      }));
    });

    await flushMicro();

    const sends = sqsMock.commandCalls(SendMessageCommand);
    expect(sends).toHaveLength(1);
    expect(sends[0].args[0].input).toMatchObject({
      QueueUrl: 'https://sqs.us-east-2.amazonaws.com/123/qurl-bot-events',
    });
  });

  test('non-dispatch opcodes (HEARTBEAT_ACK / HELLO etc.) → no SendMessage', () => {
    eventPublisher.start();
    withMockedSqs(() => {
      eventPublisher.publish(rawPacket({ op: 11, t: null })); // heartbeat ack
      eventPublisher.publish(rawPacket({ op: 10, t: null })); // hello
      eventPublisher.publish(rawPacket({ op: 7, t: null })); // reconnect
    });
    expect(sqsMock.commandCalls(SendMessageCommand)).toHaveLength(0);
  });

  test('dispatch but t !== INTERACTION_CREATE → no SendMessage', () => {
    eventPublisher.start();
    withMockedSqs(() => {
      eventPublisher.publish(rawPacket({ t: 'GUILD_CREATE' }));
      eventPublisher.publish(rawPacket({ t: 'MESSAGE_CREATE' }));
      eventPublisher.publish(rawPacket({ t: 'PRESENCE_UPDATE' }));
      eventPublisher.publish(rawPacket({ t: 'READY' }));
    });
    expect(sqsMock.commandCalls(SendMessageCommand)).toHaveLength(0);
  });

  test('falsy / malformed packet does not throw (defense-in-depth)', () => {
    eventPublisher.start();
    withMockedSqs(() => {
      // None of these should crash the gateway WS loop.
      expect(() => eventPublisher.publish(undefined)).not.toThrow();
      expect(() => eventPublisher.publish(null)).not.toThrow();
      expect(() => eventPublisher.publish({})).not.toThrow();
      expect(() => eventPublisher.publish({ op: 0 })).not.toThrow();
    });
    expect(sqsMock.commandCalls(SendMessageCommand)).toHaveLength(0);
  });
});

describe('event-publisher: envelope shape', () => {
  test('matches consumer contract: eventType + shardId + data + event_id + published_at_ms', async () => {
    sqsMock.on(SendMessageCommand).resolves({ MessageId: 'sqs-2' });
    eventPublisher.start();
    const before = Date.now();
    withMockedSqs(() => {
      eventPublisher.publish(rawPacket({
        s: 1234567,
        d: { type: 3, data: { custom_id: 'flow:foo:bar' }, id: 'i-2' },
      }));
    });
    const after = Date.now();

    await flushMicro();

    const sends = sqsMock.commandCalls(SendMessageCommand);
    expect(sends).toHaveLength(1);
    const body = JSON.parse(sends[0].args[0].input.MessageBody);
    expect(body.eventType).toBe('INTERACTION_CREATE');
    expect(body.shardId).toBe('0');
    expect(body.event_id).toBe('0:1234567');
    expect(body.data).toEqual({ type: 3, data: { custom_id: 'flow:foo:bar' }, id: 'i-2' });
    expect(typeof body.published_at_ms).toBe('number');
    // published_at_ms is set in publish() between `before` and `after`.
    // Pin the bounds so a refactor that captures the timestamp at
    // SendMessage-submit time (vs envelope build) is caught.
    expect(body.published_at_ms).toBeGreaterThanOrEqual(before);
    expect(body.published_at_ms).toBeLessThanOrEqual(after);
  });

  test('event_id format is `${shardId}:${packet.s}` — matches consumer LRU shape', async () => {
    sqsMock.on(SendMessageCommand).resolves({ MessageId: 'sqs-3' });
    eventPublisher.start();
    withMockedSqs(() => {
      eventPublisher.publish(rawPacket({ s: 99 }));
    });
    await flushMicro();
    const body = JSON.parse(sqsMock.commandCalls(SendMessageCommand)[0].args[0].input.MessageBody);
    // Pinning the literal shape — a future PR introducing sharding will
    // change SHARD_ID and this test will fail loudly, prompting the
    // contract-update conversation rather than silently shipping.
    expect(body.event_id).toBe('0:99');
  });

  test('raw d payload is passed through untouched (no normalization)', async () => {
    // The worker side calls client.actions.InteractionCreate.handle(data)
    // — that internal API expects the raw Discord wire shape. Any
    // producer-side normalization would silently break a future
    // discord.js minor version.
    sqsMock.on(SendMessageCommand).resolves({ MessageId: 'sqs-4' });
    eventPublisher.start();
    const exoticPayload = {
      type: 2,
      data: { type: 1, options: [{ name: 'recipients', value: '@everyone' }] },
      guild_id: '123',
      channel_id: '456',
      member: { permissions: '8' },
      token: 'opaque',
      version: 1,
    };
    withMockedSqs(() => {
      eventPublisher.publish(rawPacket({ d: exoticPayload }));
    });
    await flushMicro();
    const body = JSON.parse(sqsMock.commandCalls(SendMessageCommand)[0].args[0].input.MessageBody);
    expect(body.data).toEqual(exoticPayload);
  });
});

describe('event-publisher: start/stop lifecycle', () => {
  test('start() throws when ENABLE_EVENT_SHIPPER=false', () => {
    const config = require('../src/config');
    const originalFlag = config.ENABLE_EVENT_SHIPPER;
    config.ENABLE_EVENT_SHIPPER = false;
    try {
      expect(() => eventPublisher.start()).toThrow(/ENABLE_EVENT_SHIPPER=false/);
    } finally {
      config.ENABLE_EVENT_SHIPPER = originalFlag;
    }
  });

  test('start() throws when queue URL is missing', () => {
    const config = require('../src/config');
    const original = config.QURL_BOT_EVENTS_QUEUE_URL;
    config.QURL_BOT_EVENTS_QUEUE_URL = '';
    try {
      expect(() => eventPublisher.start()).toThrow(/QURL_BOT_EVENTS_QUEUE_URL/);
    } finally {
      config.QURL_BOT_EVENTS_QUEUE_URL = original;
    }
  });

  test('start() twice logs warn on second call (idempotent)', () => {
    eventPublisher.start();
    eventPublisher.start();
    expect(logger.warn).toHaveBeenCalledWith(
      expect.stringContaining('start() called while already running'),
    );
    expect(eventPublisher._test.isRunning()).toBe(true);
  });

  test('stop() before start() is a no-op (idempotent)', async () => {
    await expect(eventPublisher.stop()).resolves.toBeUndefined();
    expect(logger.info).not.toHaveBeenCalled();
  });

  test('publish() before start() drops at debug + does NOT call SendMessage', () => {
    // No start() — `running` is false. Race window during boot:
    // discord.js's raw event could in principle fire before
    // index.js's start() reaches eventPublisher.start(). The
    // debug-drop guard means we never crash, just lose the event
    // (which would have lost anyway since the worker isn't reading
    // until its own start). Asserts the guard's existence.
    withMockedSqs(() => {
      eventPublisher.publish(rawPacket());
    });
    expect(sqsMock.commandCalls(SendMessageCommand)).toHaveLength(0);
    expect(logger.debug).toHaveBeenCalledWith(
      expect.stringContaining('before start()'),
      expect.objectContaining({ eventType: 'INTERACTION_CREATE' }),
    );
  });

  test('publish() after stop() drops at debug + does NOT call SendMessage', async () => {
    eventPublisher.start();
    await eventPublisher.stop();
    withMockedSqs(() => {
      eventPublisher.publish(rawPacket());
    });
    expect(sqsMock.commandCalls(SendMessageCommand)).toHaveLength(0);
    expect(logger.debug).toHaveBeenCalledWith(
      expect.stringContaining('after stop()'),
      expect.objectContaining({ eventType: 'INTERACTION_CREATE' }),
    );
  });
});

describe('event-publisher: send-failure logging', () => {
  test('SendMessage rejection → logs error with kind=unhandledRejection tag', async () => {
    // The kind tag is load-bearing for the unified CloudWatch query
    // that finds rejections across all three sites (publisher,
    // consumer trackDispatch, index.js global). A wording drift
    // here without the tag would create a blind spot.
    sqsMock.on(SendMessageCommand).rejects(new Error('throttled by SQS'));
    eventPublisher.start();
    withMockedSqs(() => {
      eventPublisher.publish(rawPacket({ s: 7 }));
    });
    await flushMicro();
    expect(logger.error).toHaveBeenCalledWith(
      expect.stringContaining('SendMessage failed'),
      expect.objectContaining({
        kind: 'unhandledRejection',
        error: 'throttled by SQS',
        eventId: '0:7',
      }),
    );
  });

  test('synchronous throw from sqsClient.send routes through the same kind tag (no error-emitter divergence)', async () => {
    // Pins the closed-blind-spot: a sync throw from the AWS SDK
    // (rare with v3 but possible on malformed input) must NOT
    // propagate out of the EventEmitter listener as a different
    // error shape — the unified CloudWatch query that filters on
    // `kind: 'unhandledRejection'` would lose this site otherwise.
    // Simulate with a mock client whose .send throws synchronously.
    const throwingClient = {
      send: () => { throw new Error('sync-throw: malformed input'); },
    };
    eventPublisher._test._setSqsClientForTest(throwingClient);
    eventPublisher.start();
    expect(() => eventPublisher.publish(rawPacket({ s: 11 }))).not.toThrow();
    expect(logger.error).toHaveBeenCalledWith(
      expect.stringContaining('threw synchronously'),
      expect.objectContaining({
        kind: 'unhandledRejection',
        error: 'sync-throw: malformed input',
        eventId: '0:11',
      }),
    );
    // No promise enters inFlightSends on the sync-throw path —
    // otherwise stop() would await a promise that doesn't exist.
    expect(eventPublisher._test.getInFlightCount()).toBe(0);
  });

  test('failed send still removes promise from inFlightSends (no leak)', async () => {
    sqsMock.on(SendMessageCommand).rejects(new Error('AWS down'));
    eventPublisher.start();
    withMockedSqs(() => {
      eventPublisher.publish(rawPacket({ s: 1 }));
      eventPublisher.publish(rawPacket({ s: 2 }));
      eventPublisher.publish(rawPacket({ s: 3 }));
    });
    // After flush, all three should have settled (rejected) and
    // been removed via .finally → inFlightSends.delete.
    await flushMicro();
    expect(eventPublisher._test.getInFlightCount()).toBe(0);
  });

  test('sustained SendMessage failure → inFlightSends stays bounded, no retry buffer accumulates (fire-and-log invariant)', async () => {
    // Pins the documented fire-and-log design (this file's module
    // header explicitly rejects in-process retry / accumulation). A
    // future "let's add a retry buffer" refactor would break the
    // invariant — inFlightSends would grow unboundedly with drained-
    // but-pending sends. Publishing N packets against a throwing SQS
    // and asserting post-settle count is zero catches both the
    // .finally→delete-bypassed and .catch-bypassed regressions in
    // one shot. N=20 is enough — every iteration takes the same
    // code path; 200 added no extra coverage and made the test
    // visibly slower.
    sqsMock.on(SendMessageCommand).rejects(new Error('persistent SQS outage'));
    eventPublisher.start();
    const N = 20;
    withMockedSqs(() => {
      for (let i = 0; i < N; i += 1) {
        eventPublisher.publish(rawPacket({ s: i + 1 }));
      }
    });
    await flushMicro();
    // The load-bearing invariant: no in-memory accumulation. A future
    // batch/coalesce optimization could legitimately reduce the
    // per-publish log ratio, so the count assertion is `> 0` (must
    // log SOMETHING per failure) — the count-to-zero is what matters.
    expect(eventPublisher._test.getInFlightCount()).toBe(0);
    const failureLogs = logger.error.mock.calls.filter(
      ([, ctx]) => ctx && ctx.kind === 'unhandledRejection',
    );
    expect(failureLogs.length).toBeGreaterThan(0);
  });
});

describe('event-publisher: drain on stop', () => {
  test('drain happy path: all sends settle within deadline → logs complete', async () => {
    // Resolve each SendMessage after a small delay so they're
    // actually in-flight when stop() awaits — otherwise the set
    // would be empty by the time stop() reads it and the
    // "no in-flight" branch would log instead.
    sqsMock.on(SendMessageCommand).callsFake(() => new Promise((resolve) => {
      setTimeout(() => resolve({ MessageId: 'sqs-x' }), 5);
    }));
    eventPublisher.start();
    withMockedSqs(() => {
      eventPublisher.publish(rawPacket({ s: 1 }));
      eventPublisher.publish(rawPacket({ s: 2 }));
    });
    // Brief yield so the synchronous publish() finishes building
    // both envelopes + adding both promises to inFlightSends BEFORE
    // stop() reads the set's size. Without this, fast pathing
    // through publish() could miss the second add.
    await Promise.resolve();
    expect(eventPublisher._test.getInFlightCount()).toBe(2);
    await eventPublisher.stop();
    expect(logger.info).toHaveBeenCalledWith(
      expect.stringContaining('drain complete'),
      expect.objectContaining({ count: 2 }),
    );
    expect(eventPublisher._test.getInFlightCount()).toBe(0);
  });

  test('drain deadline elapses: never-settling send → logs deadline-elapsed warn', async () => {
    // Never resolve — simulates SQS hung mid-send during shutdown.
    sqsMock.on(SendMessageCommand).callsFake(() => new Promise(() => {}));
    // Shrink deadline to keep the suite fast.
    eventPublisher._test._setDrainDeadlineForTest(50);
    eventPublisher.start();
    withMockedSqs(() => {
      eventPublisher.publish(rawPacket({ s: 1 }));
    });
    await Promise.resolve();
    await eventPublisher.stop();
    expect(logger.warn).toHaveBeenCalledWith(
      expect.stringContaining('drain deadline elapsed'),
      expect.objectContaining({ unsettled: 1 }),
    );
  });

  test('no-op-idle drain: nothing in flight → logs "stop complete (no in-flight sends)"', async () => {
    eventPublisher.start();
    // No publish() — inFlightSends is empty when stop() runs.
    await eventPublisher.stop();
    expect(logger.info).toHaveBeenCalledWith(
      expect.stringContaining('no in-flight sends to drain'),
    );
    // Did NOT log a draining message.
    expect(logger.info).not.toHaveBeenCalledWith(
      expect.stringContaining('draining'),
      expect.anything(),
    );
  });

  test('getDrainDeadlineMs reflects mutations via _setDrainDeadlineForTest (not a stale snapshot)', () => {
    // Pin the live-getter contract — a regression that exported the
    // mutable value snapshot-at-module-load would silently break the
    // deadline shrinker the drain tests rely on. Mirrors the same
    // contract enforced in event-consumer's _test.getDrainDeadlineMs.
    const before = eventPublisher._test.getDrainDeadlineMs();
    eventPublisher._test._setDrainDeadlineForTest(123);
    expect(eventPublisher._test.getDrainDeadlineMs()).toBe(123);
    expect(eventPublisher._test.getDrainDeadlineMs()).not.toBe(before);
  });
});
