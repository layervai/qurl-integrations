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
  // _setStopControllerForTest is needed by tests #1 and #3 which
  // drive pollOnce directly and read stopController.signal without
  // going through start() (start() re-creates the controller, so
  // test #2 overwrites this seed harmlessly). Run unconditionally
  // here so all three tests get a fresh AbortController.
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
//
// Scoping: this helper wraps the cap-pre-fill ONLY. The flag flips
// back to false before any subsequent pollOnce runs, so anything
// pollOnce would try to track via processMessage's trackDispatch is
// a no-op (the gate skips). That's intentional — we want the cap
// saturated by the pre-fill and pollOnce's at-cap branch to fire;
// pollOnce shouldn't ADD to inflight during the test.
function withWorkerDispatch(fn) {
  eventConsumer._test._setWorkerDispatchingForTest(true);
  try { return fn(); } finally {
    eventConsumer._test._setWorkerDispatchingForTest(false);
  }
}

describe('Pillar 1 chaos — sustained backpressure + SIGTERM-mid-pause', () => {
  // Pin the floor cap once. Mocked config sets MAX_INFLIGHT_HANDLERS=10
  // (matches RECEIVE_MAX_MESSAGES so validateInflightCap stays quiet).
  // If the mocked value drifts, this surfaces it once at the describe
  // boundary instead of per-test.
  beforeAll(() => {
    expect(eventConsumer._test.MAX_INFLIGHT_HANDLERS).toBe(10);
  });

  it('inflight is bounded across many at-cap iterations (no OOM growth)', async () => {
    // Saturate inflight at the cap with pending Promises that never
    // settle. Then iterate pollOnce 10 times. Inflight count must stay
    // at exactly cap — pollOnce's early-return must NOT accidentally
    // enqueue a new handler each iteration (which would grow the Set
    // unbounded → OOM under sustained load).
    const cap = eventConsumer._test.MAX_INFLIGHT_HANDLERS;

    // `new Promise(() => {})` never settles; _resetStateForTest in
    // beforeEach .clear()s the inflight Set, releasing the references.
    // (See withWorkerDispatch's docstring for why the flag scoping is
    // pre-fill only.)
    withWorkerDispatch(() => {
      for (let i = 0; i < cap; i += 1) {
        eventConsumer.trackDispatch(new Promise(() => {}));
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

  it('SIGTERM during at-cap pause wakes abortableSleep without timer fire (signal-wake invariant)', async () => {
    // The load-bearing chaos invariant. Without the abortableSleep-
    // signal-wired refactor, an at-cap stop() would wait out the full
    // backoff sleep on each pollLoop iteration — and at the ceiling
    // (1.6 s/iter), a multi-iteration drain would breach the 3 s
    // deadline. Asserted deterministically here via fake timers: if
    // stop() wakes via signal, the loop terminates WITHOUT any
    // backoff setTimeout firing.
    //
    // Drives the production lifecycle (start() → pollLoop → pollOnce
    // parks → stop()). Using _test.pollOnce directly bypasses start()
    // which sets `running=true` — stop()'s first guard short-circuits
    // when !running, so a direct-pollOnce setup would silently no-op
    // stop() and pass for the wrong reason.
    const cap = eventConsumer._test.MAX_INFLIGHT_HANDLERS;
    // (See withWorkerDispatch's docstring for the flag scoping rationale.)
    withWorkerDispatch(() => {
      for (let i = 0; i < cap; i += 1) {
        eventConsumer.trackDispatch(new Promise(() => {}));
      }
    });

    // Shrink the drain deadline so stop()'s post-loop wait on the
    // never-settling inflight promises doesn't wall-clock the test.
    // The drain timer is real (not the abortable backoff sleep we're
    // testing), so even under fake-timers it'd otherwise consume
    // DRAIN_DEADLINE_MS of real time.
    eventConsumer._test._setDrainDeadlineForTest(50);

    jest.useFakeTimers({ doNotFake: ['nextTick', 'setImmediate', 'queueMicrotask'] });
    try {
      const client = makeStubClient();
      // start() is synchronous (kicks off pollLoop as a detached
      // promise). The `await` consumes one microtask tick, which is
      // enough for pollLoop to enter its first pollOnce → at-cap →
      // schedule a backoff setTimeout via abortableSleep. So by the
      // time start() resolves to the test, the loop is already
      // parked on exactly one (fake) backoff timer.
      await eventConsumer.start(client);

      // One more setImmediate yield to flush any straggling
      // microtasks pollLoop may have queued; not load-bearing —
      // the post-await timer count is already 1.
      await new Promise((r) => { setImmediate(r); });

      // Loop parked on exactly one (fake) backoff timer — the
      // structural shape. If this fails (count=2), the regression
      // likely lives in event-consumer.js's start(): it added a
      // setTimeout before its first internal await (a future metrics
      // tick, say), and the load-bearing `=== 0` post-stop assertion
      // below needs to be revisited.
      expect(jest.getTimerCount()).toBe(1);

      // Capture the signal NOW — stop() nulls stopController in its
      // finally block, so a post-stop read would see null.
      const { signal } = eventConsumer._test.getStopController();

      // Call stop. Its synchronous prefix (before the first internal
      // await) calls stopController.abort(), which fires the abort
      // event listeners — including abortableSleep's, which calls
      // clearTimeout(t) on the parked backoff timer.
      const stopPromise = eventConsumer.stop();

      // ── Load-bearing assertion ──
      // At this point, abort() has already run synchronously. If
      // abortableSleep is signal-wired (current contract), its
      // listener fired and cleared the backoff timer. stop()'s drain
      // timer hasn't been scheduled yet (it lives after `await
      // loopPromise` inside stop). Pending fake timer count must be 0.
      //
      // If a regression strips the addEventListener wiring from
      // abortableSleep, the backoff timer would still be parked here,
      // getTimerCount() would be 1, and this assertion fails — even
      // though runAllTimersAsync below would later fire the orphan
      // timer and let the loop unwind via the timeout branch (which
      // is exactly the regression mode this test exists to catch).
      expect(jest.getTimerCount()).toBe(0);
      expect(signal.aborted).toBe(true);

      // Let stop() finish its drain (which schedules + awaits a
      // real-but-faked setTimeout(DRAIN_DEADLINE_MS)) and unwind.
      await new Promise((r) => { setImmediate(r); });
      await jest.runAllTimersAsync();
      await stopPromise;
    } finally {
      jest.useRealTimers();
    }
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

      // Settle a handler — capacity drops to cap-1 (= 9).
      resolveOne();
      // Yield so the .then(remove) cleanup runs.
      await new Promise((r) => { setImmediate(r); });
      expect(eventConsumer._test.getInFlightCount()).toBe(cap - 1);

      // Next iteration: below-cap path fires release-info log + resets backoff.
      p = eventConsumer._test.pollOnce(client);
      await jest.runOnlyPendingTimersAsync();
      await p;

      // Release-side post-conditions land here, before the follow-on
      // streak rewrites them. atCapPauseLogged is now false, backoff
      // back to base, release-info logged.
      expect(logger.info).toHaveBeenCalledWith(
        eventConsumer._test.AT_CAP_RELEASED_INFO_MSG,
        // The release log snapshots the inflight count that triggered
        // the release (necessarily below cap). Don't assert exact value
        // — cap-1 at log time is what we'd see today, but the contract
        // is "< cap", not "= cap-1".
        expect.objectContaining({ inFlight: expect.any(Number), cap }),
      );
      expect(eventConsumer._test.getCurrentBackoffMs()).toBe(
        eventConsumer._test.INFLIGHT_BACKOFF_BASE_MS,
      );
      expect(eventConsumer._test.isAtCapPauseLogged()).toBe(false);

      // ── Follow-on at-cap streak: entry-warn must re-fire ──
      // Re-saturate to cap and run one more pollOnce. The entry warn
      // must fire a SECOND time — a regression that fails to clear
      // atCapPauseLogged on release would silently suppress every
      // subsequent at-cap warning, costing operators the signal.
      // isAtCapPauseLogged()=false above is the necessary condition;
      // this iteration is the sufficient one (closes the bracketing
      // contract on a real second entry, not just the flag flip).
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

    // Entry-warn fired on the first AND on the follow-on streak —
    // exactly two over the test.
    const entryWarns = logger.warn.mock.calls.filter(
      ([msg]) => msg === eventConsumer._test.AT_CAP_PAUSE_WARN_MSG,
    );
    expect(entryWarns).toHaveLength(2);
  });
});
