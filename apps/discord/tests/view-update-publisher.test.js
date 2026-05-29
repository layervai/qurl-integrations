/**
 * Unit tests for src/view-update-publisher.js — the SQS producer for
 * view-update push (feat #60, sub-second view counter).
 *
 * Covers:
 *   - publish(): envelope shape, SendMessage call
 *   - start/stop lifecycle: idempotency, flag/queue-url guards
 *   - pre-start drop (publish() called before start() is a safe no-op)
 *   - SendMessage failure logging (kind: LOG_KINDS.VIEW_UPDATE_PUBLISH_FAIL
 *     — decoupled from event-publisher.js's UNHANDLED_REJECTION so
 *     paging pivots stay separate)
 *   - drain on stop()
 *   - input validation (publish() with invalid qurlId)
 */

jest.mock('../src/config', () => ({
  ENABLE_VIEW_UPDATE_PUSH: true,
  QURL_BOT_VIEW_UPDATES_QUEUE_URL: 'https://sqs.us-east-2.amazonaws.com/123/qurl-bot-view-updates',
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

const publisher = require('../src/view-update-publisher');
const logger = require('../src/logger');

beforeAll(() => {
  if (!process.env.AWS_REGION) process.env.AWS_REGION = 'us-east-2';
});

beforeEach(() => {
  publisher._test._resetStateForTest();
  publisher._test._setDrainDeadlineForTest(3000);
  sqsMock.reset();
  jest.clearAllMocks();
  const realClient = new SQSClient({ region: 'us-east-2' });
  publisher._test._setSqsClientForTest(realClient);
});

describe('view-update-publisher', () => {
  describe('start / stop lifecycle', () => {
    test('start() requires the flag', async () => {
      publisher._test._resetStateForTest();
      // Override the mocked config so the flag appears off.
      const config = require('../src/config');
      const originalFlag = config.ENABLE_VIEW_UPDATE_PUSH;
      config.ENABLE_VIEW_UPDATE_PUSH = false;
      try {
        expect(() => publisher.start()).toThrow(/ENABLE_VIEW_UPDATE_PUSH=false/);
      } finally {
        config.ENABLE_VIEW_UPDATE_PUSH = originalFlag;
      }
    });

    test('start() requires queue URL', () => {
      const config = require('../src/config');
      const originalUrl = config.QURL_BOT_VIEW_UPDATES_QUEUE_URL;
      config.QURL_BOT_VIEW_UPDATES_QUEUE_URL = undefined;
      try {
        expect(() => publisher.start()).toThrow(/QURL_BOT_VIEW_UPDATES_QUEUE_URL is not set/);
      } finally {
        config.QURL_BOT_VIEW_UPDATES_QUEUE_URL = originalUrl;
      }
    });

    test('start() is idempotent on second call', () => {
      publisher.start();
      expect(publisher._test.isRunning()).toBe(true);
      publisher.start();
      expect(logger.warn).toHaveBeenCalledWith(
        expect.stringMatching(/start.*already running/),
      );
    });

    test('stop() is a no-op when not running', async () => {
      await expect(publisher.stop()).resolves.toBeUndefined();
    });
  });

  describe('publish()', () => {
    test('safe no-op when not running (pre-start drop)', () => {
      publisher.publish({ qurlId: 'qrl_a', accessCount: 1, eventId: 'evt_1' });
      expect(sqsMock.commandCalls(SendMessageCommand)).toHaveLength(0);
    });

    test('publishes envelope shape after start()', async () => {
      sqsMock.on(SendMessageCommand).resolves({ MessageId: 'msg-1' });
      publisher.start();

      publisher.publish({
        qurlId: 'qrl_xyz',
        accessCount: 5,
        eventId: 'evt_xyz',
      });

      // publish() is fire-and-log; the underlying send resolves
      // asynchronously. Drain via stop() to await the in-flight
      // promise (mirrors event-publisher's test pattern).
      await publisher.stop();

      const calls = sqsMock.commandCalls(SendMessageCommand);
      expect(calls).toHaveLength(1);
      const input = calls[0].args[0].input;
      expect(input.QueueUrl).toBe('https://sqs.us-east-2.amazonaws.com/123/qurl-bot-view-updates');
      const body = JSON.parse(input.MessageBody);
      expect(body.qurl_id).toBe('qrl_xyz');
      expect(body.access_count).toBe(5);
      expect(body.event_id).toBe('evt_xyz');
      expect(typeof body.published_at_ms).toBe('number');
      // `consumed` deliberately omitted from envelope (cr round-14 #3)
      // — handler doesn't read it. Pin the absence so a re-add lands
      // alongside the envelope-contract module follow-up (#476).
      expect(body).not.toHaveProperty('consumed');
    });

    test('rejects invalid qurlId without sending', () => {
      publisher.start();
      publisher.publish({ qurlId: '', accessCount: 1, eventId: 'evt' });
      publisher.publish({ qurlId: null, accessCount: 1, eventId: 'evt' });
      publisher.publish({ qurlId: 123, accessCount: 1, eventId: 'evt' });
      expect(sqsMock.commandCalls(SendMessageCommand)).toHaveLength(0);
      expect(logger.warn).toHaveBeenCalledWith(
        expect.stringContaining('invalid qurlId'),
        expect.any(Object),
      );
    });

    test('drops with log when inFlightSends cap reached (backpressure)', async () => {
      // Stub send to return a never-resolving promise so each publish
      // accumulates in inFlightSends without ever cleaning up. Then
      // observe that the (MAX+1)th publish is dropped.
      const neverResolves = new Promise(() => {});
      sqsMock.on(SendMessageCommand).callsFake(() => neverResolves);
      publisher.start();
      // Cap is 1000 (MAX_INFLIGHT_SENDS); fire 1000 to fill it, then
      // assert the 1001st drops. Each call queues a microtask via the
      // .catch().finally() chain, so we don't need awaits between.
      for (let i = 0; i < 1000; i++) {
        publisher.publish({ qurlId: `qrl_${i}`, accessCount: 1, eventId: `evt_${i}` });
      }
      expect(publisher._test.getInFlightCount()).toBe(1000);
      const sendCallsAtCap = sqsMock.commandCalls(SendMessageCommand).length;
      // The next publish should drop without firing a send.
      publisher.publish({ qurlId: 'qrl_overflow', accessCount: 1, eventId: 'evt_overflow' });
      expect(sqsMock.commandCalls(SendMessageCommand)).toHaveLength(sendCallsAtCap);
      expect(logger.warn).toHaveBeenCalledWith(
        expect.stringContaining('in-flight cap reached'),
        expect.objectContaining({
          qurl_id: 'qrl_overflow',
          in_flight: 1000,
          cap: 1000,
          kind: 'viewUpdatePublishFail',
        }),
      );
      // Cleanup: skip stop()'s drain (would hang on the never-
      // resolving promises) by hard-resetting module state.
      publisher._test._resetStateForTest();
    });

    test('rejects invalid accessCount without sending (parity with consumer gate)', () => {
      publisher.start();
      for (const ac of [0, -1, 1.5, NaN, 'one', null, undefined]) {
        publisher.publish({ qurlId: 'qrl_a', accessCount: ac, eventId: 'evt' });
      }
      expect(sqsMock.commandCalls(SendMessageCommand)).toHaveLength(0);
      expect(logger.warn).toHaveBeenCalledWith(
        expect.stringContaining('invalid accessCount'),
        expect.any(Object),
      );
    });

    test('SendMessage failure is fire-and-log (no unhandled rejection)', async () => {
      sqsMock.on(SendMessageCommand).rejects(new Error('SQS throttled'));
      publisher.start();
      publisher.publish({ qurlId: 'qrl_a', accessCount: 1, eventId: 'evt_a' });
      await publisher.stop(); // drain
      expect(logger.warn).toHaveBeenCalledWith(
        expect.stringContaining('SendMessage failed'),
        expect.objectContaining({
          qurl_id: 'qrl_a',
          error: 'SQS throttled',
          kind: 'viewUpdatePublishFail',
        }),
      );
    });

    test('sync throw from SQS client is caught (does NOT propagate to caller)', () => {
      // Force a sync throw by stubbing the SQS client's send to throw
      // immediately (vs. returning a rejecting promise). The webhook
      // route's catch block would otherwise flip a 200 to a 500.
      const throwingClient = {
        send: () => { throw new Error('synchronous SDK validation error'); },
      };
      publisher._test._setSqsClientForTest(throwingClient);
      publisher.start();
      expect(() => publisher.publish({
        qurlId: 'qrl_a',
        accessCount: 1,
        eventId: 'evt_a',
      })).not.toThrow();
      expect(logger.warn).toHaveBeenCalledWith(
        expect.stringContaining('publish() sync threw'),
        expect.objectContaining({
          qurl_id: 'qrl_a',
          kind: 'viewUpdatePublishFail',
        }),
      );
    });
  });
});
