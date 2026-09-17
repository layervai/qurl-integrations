
jest.mock('../src/config', () => ({
  ENABLE_EVENT_SHIPPER: true,
  QURL_BOT_EVENTS_QUEUE_URL: 'https://sqs.us-east-2.amazonaws.com/123/qurl-bot-events',
  QURL_BOT_MAX_INFLIGHT_HANDLERS: 10,
  QURL_BOT_DRAIN_DEADLINE_MS: 3000,
}));

jest.mock('../src/logger', () => ({
  info: jest.fn(), warn: jest.fn(), error: jest.fn(),
  debug: jest.fn(), audit: jest.fn(),
}));

const { mockClient } = require('aws-sdk-client-mock');
const {
  SQSClient, ReceiveMessageCommand, DeleteMessageCommand,
} = require('@aws-sdk/client-sqs');

const logger = require('../src/logger');

const eventConsumer = require('../src/event-consumer');

const sqsMock = mockClient(SQSClient);

function makeStubClient() {
  return {
    actions: {
      InteractionCreate: { handle: jest.fn(() => Promise.resolve()) },
    },
  };
}

beforeEach(() => {
  sqsMock.reset();
  sqsMock.on(ReceiveMessageCommand).resolves({ Messages: [] });
  sqsMock.on(DeleteMessageCommand).resolves({});
  eventConsumer._test._resetStateForTest();
  eventConsumer._test._setSqsClientForTest(new SQSClient({}));
  eventConsumer._test._setStopControllerForTest();
  logger.info.mockClear();
  logger.warn.mockClear();
  logger.error.mockClear();
  logger.debug.mockClear();
});

afterAll(() => sqsMock.restore());

function withWorkerDispatch(fn) {
  eventConsumer._test._setWorkerDispatchingForTest(true);
  try { return fn(); } finally {
    eventConsumer._test._setWorkerDispatchingForTest(false);
  }
}

describe('Pillar 1 chaos — sustained backpressure + SIGTERM-mid-pause', () => {
  beforeAll(() => {
    expect(eventConsumer._test.MAX_INFLIGHT_HANDLERS).toBe(10);
  });

  it('inflight is bounded across many at-cap iterations (no OOM growth)', async () => {
    const cap = eventConsumer._test.MAX_INFLIGHT_HANDLERS;

    withWorkerDispatch(() => {
      for (let i = 0; i < cap; i += 1) {
        eventConsumer.trackDispatch(new Promise(() => {}));
      }
    });
    expect(eventConsumer._test.getInFlightCount()).toBe(cap);

    sqsMock.on(ReceiveMessageCommand).resolves({
      Messages: [{ Body: JSON.stringify({ data: { t: 'INTERACTION_CREATE' } }), ReceiptHandle: 'h' }],
    });
    const client = makeStubClient();

    jest.useFakeTimers({ doNotFake: ['nextTick', 'setImmediate', 'queueMicrotask'] });
    try {
      for (let i = 0; i < 10; i += 1) {
        const p = eventConsumer._test.pollOnce(client);
        // Drain the timer + microtasks each iteration.
        // eslint-disable-next-line no-await-in-loop
        await jest.runOnlyPendingTimersAsync();
        // eslint-disable-next-line no-await-in-loop
        await p;
      }
    } finally {
      jest.useRealTimers();
    }

    expect(eventConsumer._test.getInFlightCount()).toBe(cap);
    expect(sqsMock.commandCalls(ReceiveMessageCommand)).toHaveLength(0);
    const entryWarns = logger.warn.mock.calls.filter(
      ([msg]) => msg === eventConsumer._test.AT_CAP_PAUSE_WARN_MSG,
    );
    expect(entryWarns).toHaveLength(1);
  });

  it('SIGTERM during at-cap pause wakes abortableSleep without timer fire (signal-wake invariant)', async () => {
    const cap = eventConsumer._test.MAX_INFLIGHT_HANDLERS;
    withWorkerDispatch(() => {
      for (let i = 0; i < cap; i += 1) {
        eventConsumer.trackDispatch(new Promise(() => {}));
      }
    });

    eventConsumer._test._setDrainDeadlineForTest(50);

    jest.useFakeTimers({ doNotFake: ['nextTick', 'setImmediate', 'queueMicrotask'] });
    try {
      const client = makeStubClient();
      await eventConsumer.start(client);

      await new Promise((r) => { setImmediate(r); });

      expect(jest.getTimerCount()).toBe(1);

      const { signal } = eventConsumer._test.getStopController();

      const stopPromise = eventConsumer.stop();

      expect(jest.getTimerCount()).toBe(0);
      expect(signal.aborted).toBe(true);

      await new Promise((r) => { setImmediate(r); });
      await jest.runAllTimersAsync();
      await stopPromise;
    } finally {
      jest.useRealTimers();
    }
  });

  it('capacity released → release-info log + backoff reset (pause-end half of the bracket)', async () => {
    const cap = eventConsumer._test.MAX_INFLIGHT_HANDLERS;

    let resolveOne;
    const settleable = new Promise((r) => { resolveOne = r; });
    withWorkerDispatch(() => {
      eventConsumer.trackDispatch(settleable);
      for (let i = 0; i < cap - 1; i += 1) {
        eventConsumer.trackDispatch(new Promise(() => {}));
      }
    });
    expect(eventConsumer._test.getInFlightCount()).toBe(cap);

    const client = makeStubClient();

    jest.useFakeTimers({ doNotFake: ['nextTick', 'setImmediate', 'queueMicrotask'] });
    try {
      let p = eventConsumer._test.pollOnce(client);
      await jest.runOnlyPendingTimersAsync();
      await p;
      expect(eventConsumer._test.isAtCapPauseLogged()).toBe(true);
      expect(eventConsumer._test.getCurrentBackoffMs()).toBe(200);

      resolveOne();
      await new Promise((r) => { setImmediate(r); });
      expect(eventConsumer._test.getInFlightCount()).toBe(cap - 1);

      p = eventConsumer._test.pollOnce(client);
      await jest.runOnlyPendingTimersAsync();
      await p;

      expect(logger.info).toHaveBeenCalledWith(
        eventConsumer._test.AT_CAP_RELEASED_INFO_MSG,
        expect.objectContaining({ inFlight: expect.any(Number), cap }),
      );
      expect(eventConsumer._test.getCurrentBackoffMs()).toBe(
        eventConsumer._test.INFLIGHT_BACKOFF_BASE_MS,
      );
      expect(eventConsumer._test.isAtCapPauseLogged()).toBe(false);

      withWorkerDispatch(() => {
        eventConsumer.trackDispatch(new Promise(() => {}));
      });
      expect(eventConsumer._test.getInFlightCount()).toBe(cap);

      p = eventConsumer._test.pollOnce(client);
      await jest.runOnlyPendingTimersAsync();
      await p;
    } finally {
      jest.useRealTimers();
    }

    const entryWarns = logger.warn.mock.calls.filter(
      ([msg]) => msg === eventConsumer._test.AT_CAP_PAUSE_WARN_MSG,
    );
    expect(entryWarns).toHaveLength(2);
  });
});
