
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

function withMockedSqs(fn) {
  const realClient = new SQSClient({ region: 'us-east-2' });
  eventPublisher._test._setSqsClientForTest(realClient);
  return fn();
}

function rawPacket({ op = 0, t = 'INTERACTION_CREATE', s = 1, d = {} } = {}) {
  return { op, t, s, d };
}

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
    expect(body.event_id).toBe('0:99');
  });

  test('raw d payload is passed through untouched (no normalization)', async () => {
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
    await flushMicro();
    expect(eventPublisher._test.getInFlightCount()).toBe(0);
  });

  test('sustained SendMessage failure → inFlightSends stays bounded, no retry buffer accumulates (fire-and-log invariant)', async () => {
    sqsMock.on(SendMessageCommand).rejects(new Error('persistent SQS outage'));
    eventPublisher.start();
    const N = 20;
    withMockedSqs(() => {
      for (let i = 0; i < N; i += 1) {
        eventPublisher.publish(rawPacket({ s: i + 1 }));
      }
    });
    await flushMicro();
    expect(eventPublisher._test.getInFlightCount()).toBe(0);
    const failureLogs = logger.error.mock.calls.filter(
      ([, ctx]) => ctx && ctx.kind === 'unhandledRejection',
    );
    expect(failureLogs.length).toBeGreaterThan(0);
  });
});

describe('event-publisher: drain on stop', () => {
  test('drain happy path: all sends settle within deadline → logs complete', async () => {
    sqsMock.on(SendMessageCommand).callsFake(() => new Promise((resolve) => {
      setTimeout(() => resolve({ MessageId: 'sqs-x' }), 5);
    }));
    eventPublisher.start();
    withMockedSqs(() => {
      eventPublisher.publish(rawPacket({ s: 1 }));
      eventPublisher.publish(rawPacket({ s: 2 }));
    });
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
    sqsMock.on(SendMessageCommand).callsFake(() => new Promise(() => {}));
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
    await eventPublisher.stop();
    expect(logger.info).toHaveBeenCalledWith(
      expect.stringContaining('no in-flight sends to drain'),
    );
    expect(logger.info).not.toHaveBeenCalledWith(
      expect.stringContaining('draining'),
      expect.anything(),
    );
  });

  test('getDrainDeadlineMs reflects mutations via _setDrainDeadlineForTest (not a stale snapshot)', () => {
    const before = eventPublisher._test.getDrainDeadlineMs();
    eventPublisher._test._setDrainDeadlineForTest(123);
    expect(eventPublisher._test.getDrainDeadlineMs()).toBe(123);
    expect(eventPublisher._test.getDrainDeadlineMs()).not.toBe(before);
  });
});
