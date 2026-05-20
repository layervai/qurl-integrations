/**
 * Unit tests for src/view-update-consumer.js — the SQS consumer for
 * view-update push (feat #60).
 *
 * Covers:
 *   - start/stop lifecycle: idempotency, flag/queue-url guards
 *   - processMessage: valid envelope dispatches to registry
 *   - processMessage: malformed JSON dropped (no throw)
 *   - processMessage: envelope missing qurl_id dropped
 *   - processMessage: invalid access_count dropped
 *   - pollOnce: receives + deletes each message
 *   - pollOnce: silent-drop-on-miss still deletes the message
 *     (LOAD-BEARING: holding the message would just re-deliver to
 *     the same replica on visibility-timeout expiry)
 *   - AbortError on ReceiveMessage handled cleanly
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
const { SQSClient, ReceiveMessageCommand, DeleteMessageBatchCommand } = require('@aws-sdk/client-sqs');

const sqsMock = mockClient(SQSClient);

const consumer = require('../src/view-update-consumer');
const registry = require('../src/view-update-registry');
const logger = require('../src/logger');

beforeAll(() => {
  if (!process.env.AWS_REGION) process.env.AWS_REGION = 'us-east-2';
});

beforeEach(async () => {
  if (consumer._test.isRunning()) await consumer.stop();
  consumer._test._resetStateForTest();
  registry._test._resetForTest();
  sqsMock.reset();
  jest.clearAllMocks();
  const realClient = new SQSClient({ region: 'us-east-2' });
  consumer._test._setSqsClientForTest(realClient);
});

describe('view-update-consumer', () => {
  describe('start / stop lifecycle', () => {
    test('start() requires the flag', () => {
      const config = require('../src/config');
      const original = config.ENABLE_VIEW_UPDATE_PUSH;
      config.ENABLE_VIEW_UPDATE_PUSH = false;
      try {
        expect(() => consumer.start()).toThrow(/ENABLE_VIEW_UPDATE_PUSH=false/);
      } finally {
        config.ENABLE_VIEW_UPDATE_PUSH = original;
      }
    });

    test('start() requires queue URL', () => {
      const config = require('../src/config');
      const original = config.QURL_BOT_VIEW_UPDATES_QUEUE_URL;
      config.QURL_BOT_VIEW_UPDATES_QUEUE_URL = undefined;
      try {
        expect(() => consumer.start()).toThrow(/QURL_BOT_VIEW_UPDATES_QUEUE_URL is not set/);
      } finally {
        config.QURL_BOT_VIEW_UPDATES_QUEUE_URL = original;
      }
    });

    test('start() is idempotent on second call', async () => {
      sqsMock.on(ReceiveMessageCommand).resolves({ Messages: [] });
      consumer.start();
      expect(consumer._test.isRunning()).toBe(true);
      consumer.start();
      expect(logger.warn).toHaveBeenCalledWith(
        expect.stringMatching(/start.*already running/),
      );
      await consumer.stop();
    });

    test('stop() is a no-op when not running', async () => {
      await expect(consumer.stop()).resolves.toBeUndefined();
    });
  });

  describe('processMessage', () => {
    test('valid envelope dispatches to registry', () => {
      const cb = jest.fn();
      registry.register('qrl_x', cb);
      consumer._test.processMessage({
        Body: JSON.stringify({
          qurl_id: 'qrl_x',
          access_count: 7,
          consumed: false,
          event_id: 'evt_x',
          published_at_ms: 1739462812345,
        }),
        ReceiptHandle: 'rh-1',
      });
      expect(cb).toHaveBeenCalledWith(
        expect.objectContaining({
          accessCount: 7,
          consumed: false,
          eventId: 'evt_x',
          publishedAtMs: 1739462812345,
        }),
        'qrl_x',
      );
    });

    test('non-object envelope (e.g. JSON number) is dropped', () => {
      consumer._test.processMessage({
        Body: JSON.stringify(42),
        ReceiptHandle: 'rh-1',
      });
      expect(logger.warn).toHaveBeenCalledWith(
        expect.stringContaining('not an object'),
      );
    });

    test('malformed JSON is dropped without throwing', () => {
      expect(() => consumer._test.processMessage({
        Body: '{not valid json',
        ReceiptHandle: 'rh-1',
      })).not.toThrow();
      expect(logger.warn).toHaveBeenCalledWith(
        expect.stringContaining('malformed JSON envelope'),
        expect.any(Object),
      );
    });

    test('envelope missing qurl_id is dropped', () => {
      consumer._test.processMessage({
        Body: JSON.stringify({ access_count: 1 }),
        ReceiptHandle: 'rh-1',
      });
      expect(logger.warn).toHaveBeenCalledWith(
        expect.stringContaining('missing qurl_id'),
      );
    });

    test('envelope with invalid access_count is dropped', () => {
      const cb = jest.fn();
      registry.register('qrl_x', cb);

      consumer._test.processMessage({
        Body: JSON.stringify({ qurl_id: 'qrl_x', access_count: -1 }),
        ReceiptHandle: 'rh-1',
      });
      consumer._test.processMessage({
        Body: JSON.stringify({ qurl_id: 'qrl_x', access_count: 1.5 }),
        ReceiptHandle: 'rh-2',
      });
      consumer._test.processMessage({
        Body: JSON.stringify({ qurl_id: 'qrl_x', access_count: 'seven' }),
        ReceiptHandle: 'rh-3',
      });

      expect(cb).not.toHaveBeenCalled();
      expect(logger.warn).toHaveBeenCalledWith(
        expect.stringContaining('access_count not a non-negative safe integer'),
        expect.any(Object),
      );
    });

    test('silent-drop on registry miss is not logged', () => {
      // No callback registered for qrl_nope. Logger should not be
      // called — the (N-1)/N miss rate would otherwise create
      // unbounded log volume with low signal.
      consumer._test.processMessage({
        Body: JSON.stringify({ qurl_id: 'qrl_nope', access_count: 1, consumed: false }),
        ReceiptHandle: 'rh-1',
      });
      expect(logger.warn).not.toHaveBeenCalled();
      expect(logger.error).not.toHaveBeenCalled();
    });
  });

  describe('pollOnce', () => {
    test('receives + deletes each message', async () => {
      const cb = jest.fn();
      registry.register('qrl_x', cb);

      // resolvesOnce + resolves: only the FIRST ReceiveMessage call
      // gets the message; subsequent calls (whether from the background
      // pollLoop spawned by start() or the manual pollOnce() below)
      // see an empty queue. Net effect: exactly one delete, regardless
      // of which loop's pollOnce processed the message — synchronous
      // pollLoop scheduling means the background loop usually wins the
      // race, but assertions don't depend on the winner.
      sqsMock.on(ReceiveMessageCommand)
        .resolvesOnce({
          Messages: [
            {
              MessageId: 'm1',
              ReceiptHandle: 'rh-1',
              Body: JSON.stringify({ qurl_id: 'qrl_x', access_count: 3, consumed: false }),
            },
          ],
        })
        .resolves({ Messages: [] });
      sqsMock.on(DeleteMessageBatchCommand).resolves({});

      consumer.start();
      await consumer._test.pollOnce();

      expect(cb).toHaveBeenCalled();
      const delCalls = sqsMock.commandCalls(DeleteMessageBatchCommand);
      expect(delCalls).toHaveLength(1);
      const entries = delCalls[0].args[0].input.Entries;
      expect(entries).toHaveLength(1);
      expect(entries[0].ReceiptHandle).toBe('rh-1');

      await consumer.stop();
    });

    test('silent-drop-on-miss still deletes the message', async () => {
      // No callback registered. Consumer should still DELETE so it
      // doesn't re-deliver to the same replica on visibility expiry.
      sqsMock.on(ReceiveMessageCommand)
        .resolvesOnce({
          Messages: [
            {
              MessageId: 'm1',
              ReceiptHandle: 'rh-1',
              Body: JSON.stringify({ qurl_id: 'qrl_nobody_home', access_count: 1, consumed: false }),
            },
          ],
        })
        .resolves({ Messages: [] });
      sqsMock.on(DeleteMessageBatchCommand).resolves({});

      consumer.start();
      await consumer._test.pollOnce();

      const delCalls = sqsMock.commandCalls(DeleteMessageBatchCommand);
      expect(delCalls).toHaveLength(1);
      expect(delCalls[0].args[0].input.Entries[0].ReceiptHandle).toBe('rh-1');

      await consumer.stop();
    });

    test('AbortError on ReceiveMessage is handled cleanly (no log)', async () => {
      const abortErr = new Error('aborted');
      abortErr.name = 'AbortError';
      sqsMock.on(ReceiveMessageCommand).rejects(abortErr);

      consumer.start();
      await consumer._test.pollOnce();

      // AbortError is the expected shape during stop() — must not
      // be logged as a real failure.
      const errLogs = logger.warn.mock.calls.filter(
        ([msg]) => typeof msg === 'string' && msg.includes('ReceiveMessage failed'),
      );
      expect(errLogs).toHaveLength(0);

      await consumer.stop();
    });

    test('AbortError via err.code (older SDK shape) is also handled cleanly', async () => {
      const abortErr = new Error('aborted');
      abortErr.code = 'AbortError';
      sqsMock.on(ReceiveMessageCommand).rejects(abortErr);

      consumer.start();
      await consumer._test.pollOnce();

      const errLogs = logger.warn.mock.calls.filter(
        ([msg]) => typeof msg === 'string' && msg.includes('ReceiveMessage failed'),
      );
      expect(errLogs).toHaveLength(0);

      await consumer.stop();
    });

    test('non-AbortError on ReceiveMessage is logged + returns cleanly', async () => {
      sqsMock.on(ReceiveMessageCommand).rejects(new Error('SQS throttled'));

      consumer.start();
      await consumer._test.pollOnce();

      expect(logger.warn).toHaveBeenCalledWith(
        expect.stringContaining('ReceiveMessage failed'),
        expect.objectContaining({ error: 'SQS throttled' }),
      );

      await consumer.stop();
    });
  });
});
