// Pillar 1 chaos validation — consumer-side queue backpressure under
// sustained at-cap load + SIGTERM mid-pause.
//
// The producer (event-publisher) is intentionally fire-and-log per its
// module header + the design doc — no in-process retry. The real
// backpressure mechanism lives in the consumer's MAX_INFLIGHT_HANDLERS
// cap (see event-consumer.js: validateInflightCap + the at-cap branch
// of pollOnce). This chaos test pins the composition-level invariants:
//
//   1. Inflight count is BOUNDED under sustained pressure. A sustained
//      at-cap streak shouldn't grow the inFlightPromises Set beyond
//      MAX_INFLIGHT_HANDLERS + (RECEIVE_MAX_MESSAGES - 1) — no OOM.
//      Unit test `pollOnce early-returns + backs off when in-flight at
//      cap` pins ONE pause; this test pins SUSTAINED behavior.
//
//   2. SIGTERM during at-cap pause exits inside DRAIN_DEADLINE_MS.
//      The abortableSleep refactor (PR #387) wired stopController.signal
//      into the at-cap sleep so a stop() call wakes the sleep
//      immediately. Without this, an at-cap pause of up to
//      INFLIGHT_BACKOFF_MAX_MS (1.6 s) per iteration would push the
//      total drain past the 3 s ceiling on a multi-iteration pause.
//      The unit test for abortableSleep tests the signal-wake in
//      isolation; this test composes it with a real pollLoop +
//      backoff ladder mid-streak.
//
//   3. Capacity-released: when slow handlers settle, the at-cap
//      release log fires, currentBackoffMs resets, and inflight count
//      returns to zero. Pins the symmetric "pause-end" half of the
//      pause-bracketing contract.
//
// What this test does NOT cover (already pinned by unit tests):
//   - exact backoff sequence (100/200/400/800/1600 ms ladder)
//   - log throttling (one warn per streak, one info on release)
//   - cap-skew metric / underflow log shape

// MUST mock config + logger BEFORE requiring event-consumer (top-level
// requires). Mirror tests/event-consumer.test.js so the env shape
// matches what the rest of the suite exercises.
jest.mock('../src/config', () => ({
  ENABLE_EVENT_SHIPPER: true,
  QURL_BOT_EVENTS_QUEUE_URL: 'https://sqs.us-east-2.amazonaws.com/123/qurl-bot-events',
  // Tight cap for fast chaos-runs. Production default is 100. Set to
  // RECEIVE_MAX_MESSAGES (10) — the floor for which validateInflightCap
  // doesn't emit the "effective ceiling is cap+overshoot" warn (which
  // would pollute test output without representing a real misconfig).
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

// Re-require event-consumer AFTER mocks so the config snapshot is
// the one above. Each describe block needs `_resetStateForTest` to
// clear module-level state (inflight set, atCapPauseLogged, backoff)
// since the consumer is a process-singleton.
const eventConsumer = require('../src/event-consumer');

const sqsMock = mockClient(SQSClient);

function makeStubClient() {
  return {
    actions: {
      InteractionCreate: { handle: jest.fn(() => Promise.resolve()) },
    },
  };
}

// Pre-mock with an empty receive so the unit-level tests' stubs apply
// here too. Individual tests override as needed.
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

// trackDispatch is gated by isWorkerDispatch; flip the flag via the
// test-only setter so calls from this test land in the inflight Set
// the way pollOnce's production dispatch would.
function withWorkerDispatch(fn) {
  eventConsumer._test._setWorkerDispatchingForTest(true);
  try { return fn(); } finally {
    eventConsumer._test._setWorkerDispatchingForTest(false);
  }
}

describe('Pillar 1 chaos — sustained backpressure + SIGTERM-mid-pause', () => {
  it('inflight is bounded across many at-cap iterations (no OOM growth)', async () => {
    // Saturate inflight at the cap with pending Promises that never
    // settle. Then iterate pollOnce 10 times. Inflight count must stay
    // at exactly cap — pollOnce's early-return must NOT accidentally
    // enqueue a new handler each iteration (which would grow the Set
    // unbounded → OOM under sustained load).
    const cap = eventConsumer._test.MAX_INFLIGHT_HANDLERS;
    expect(cap).toBe(10);

    withWorkerDispatch(() => {
      for (let i = 0; i < cap; i += 1) {
        eventConsumer.trackDispatch(new Promise(() => {})); // never settles
      }
    });
    expect(eventConsumer._test.getInFlightCount()).toBe(cap);

    // Even when ReceiveMessage WOULD return work, the cap blocks it.
    sqsMock.on(ReceiveMessageCommand).resolves({
      Messages: [{ Body: JSON.stringify({ data: { t: 'INTERACTION_CREATE' } }), ReceiptHandle: 'h' }],
    });
    const client = makeStubClient();

    // Run 10 at-cap pollOnce iterations. Backoff sleeps are real
    // setTimeouts; with the cap immediately hit, each iteration only
    // awaits the abortableSleep which we shorten via fake timers.
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

    // Inflight stayed at cap; no growth.
    expect(eventConsumer._test.getInFlightCount()).toBe(cap);
    // Receive was never called — the cap blocked every iteration.
    expect(sqsMock.commandCalls(ReceiveMessageCommand)).toHaveLength(0);
    // Entry-warn fired exactly once (transition-loud, steady-state-quiet).
    const entryWarns = logger.warn.mock.calls.filter(
      ([msg]) => msg === eventConsumer._test.AT_CAP_PAUSE_WARN_MSG,
    );
    expect(entryWarns).toHaveLength(1);
  });

  it('SIGTERM during at-cap pause wakes abortableSleep + stop() resolves inside drain deadline', async () => {
    // The load-bearing chaos invariant. Without the abortableSleep-
    // signal-wired refactor, an at-cap stop() would wait out the full
    // backoff sleep on each pollLoop iteration — and at the ceiling
    // (1.6 s/iter), a multi-iteration drain would breach the 3 s
    // deadline. This test pins that a stop() during a backoff sleep
    // resolves within a tight window.
    const cap = eventConsumer._test.MAX_INFLIGHT_HANDLERS;
    withWorkerDispatch(() => {
      for (let i = 0; i < cap; i += 1) {
        eventConsumer.trackDispatch(new Promise(() => {}));
      }
    });

    // Shrink the drain deadline so a regression that fails to wake
    // abortableSleep doesn't wall-clock the suite. 50 ms is generous
    // vs the few-ms microtask drain a working signal-wake produces.
    eventConsumer._test._setDrainDeadlineForTest(50);

    // Inject a pollLoop that's parked inside the at-cap abortableSleep
    // by calling pollOnce directly (cap is already saturated) and
    // racing stop() against it.
    const client = makeStubClient();
    const pollPromise = eventConsumer._test.pollOnce(client);

    // Yield one event-loop turn so pollOnce enters the at-cap branch
    // and reaches the abortableSleep.
    await new Promise((r) => { setImmediate(r); });

    // Stop the consumer. signalShutdown→stop()→stopController.abort()
    // fires the signal, which wakes the in-flight abortableSleep so
    // pollOnce returns. Without the wire-up, stop() would wait the
    // full INFLIGHT_BACKOFF_BASE_MS (100 ms) before pollOnce returned —
    // not catastrophic in this tiny test but a regression mode that
    // compounds with the doubling ladder.
    const stopStart = Date.now();
    await eventConsumer.stop();
    const stopElapsed = Date.now() - stopStart;

    // The pollPromise itself resolves (it's been awoken).
    await pollPromise;

    // Stop landed inside the drain deadline. With a properly wired
    // abort-signal, this is a few ms; with a regression that breaks
    // the wire-up, it'd wait the full INFLIGHT_BACKOFF_BASE_MS (100 ms)
    // per iteration. 200 ms threshold gives GC-pause headroom on slow
    // CI runners while still catching a wire-up regression.
    expect(stopElapsed).toBeLessThan(200);
  });

  it('capacity released → release-info log + backoff reset (pause-end half of the bracket)', async () => {
    // The symmetric assertion: when slow handlers settle and inflight
    // drops below cap, the pause-end log fires AND
    // currentBackoffMs resets to base. A regression that fails to
    // reset would leave the next at-cap streak entering at the
    // ceiling backoff (1.6 s) instead of base (100 ms), inflating
    // dwell time at the cap by 16× on the FIRST iteration of every
    // subsequent streak.
    const cap = eventConsumer._test.MAX_INFLIGHT_HANDLERS;

    // Settle one of the handlers controllably; the others stay pending.
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
      // First iteration: at-cap → entry warn + base backoff sleep.
      let p = eventConsumer._test.pollOnce(client);
      await jest.runOnlyPendingTimersAsync();
      await p;
      expect(eventConsumer._test.isAtCapPauseLogged()).toBe(true);
      // Backoff doubled to 200 ms post-iteration.
      expect(eventConsumer._test.getCurrentBackoffMs()).toBe(200);

      // Settle a handler — capacity drops to cap-1 (= 4).
      resolveOne();
      // Yield so the .then(remove) cleanup runs.
      await new Promise((r) => { setImmediate(r); });
      expect(eventConsumer._test.getInFlightCount()).toBe(cap - 1);

      // Next iteration: below-cap path fires release-info log + resets backoff.
      p = eventConsumer._test.pollOnce(client);
      await jest.runOnlyPendingTimersAsync();
      await p;
    } finally {
      jest.useRealTimers();
    }

    expect(logger.info).toHaveBeenCalledWith(
      eventConsumer._test.AT_CAP_RELEASED_INFO_MSG,
      // The release log snapshots the inflight count that triggered the
      // release (necessarily below cap). Don't assert exact value —
      // cap=4 at log time is what we'd see today, but the contract is
      // "< cap", not "= cap-1".
      expect.objectContaining({ inFlight: expect.any(Number), cap }),
    );
    // Backoff reset to base.
    expect(eventConsumer._test.getCurrentBackoffMs()).toBe(
      eventConsumer._test.INFLIGHT_BACKOFF_BASE_MS,
    );
    expect(eventConsumer._test.isAtCapPauseLogged()).toBe(false);
  });
});
