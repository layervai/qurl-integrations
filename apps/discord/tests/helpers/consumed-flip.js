// Shared test helpers for the qurl.accessed consumed-flip path. The
// flip runs FIRE-AND-FORGET (Promise.resolve().then(...)) off the
// already-returned 200 and terminates by logging a single 'flip verdict'
// debug line, so test files need a uniform way to (a) drain the deferred
// chain and (b) read the terminal verdict. Both qurl-webhook-consumed*
// suites use these; kept here (per tests/helpers/ convention) so the two
// files don't drift on the queue-flush / verdict-shape details.
//
// `logger` is the mocked '../src/logger' module each caller installs via
// jest.mock — passed in rather than required here so the helper stays
// agnostic to the caller's mock wiring.

const VERDICT_LOG_MSG = 'qURL webhook qurl.accessed-consumed: flip verdict';

// The {status, transient} the background flip logged as its terminal
// verdict, or null if it hasn't logged one (e.g. the flip was never
// scheduled because consumed !== true). Projects just the outcome
// fields — the log line also carries qurl_id/event_id, which aren't
// what verdict assertions are fencing.
function flipVerdict(logger) {
  const call = logger.debug.mock.calls.find(([msg]) => msg === VERDICT_LOG_MSG);
  if (!call) return null;
  const { status, transient } = call[1];
  return { status, transient };
}

// Drain `n` macrotask ticks. Use for the never-scheduled cases
// (consumed:false / stringified "true") where no verdict will ever land,
// so polling on it would just spin the full budget.
async function drainTicks(n = 8) {
  for (let i = 0; i < n; i += 1) {
    await new Promise((resolve) => setImmediate(resolve));
  }
}

// Drain until the flip's terminal verdict log lands (the uniform end
// signal on every branch — happy, skip, OR transient), bounded so a path
// that never schedules the flip returns instead of hanging.
async function flushFlip(logger) {
  for (let i = 0; i < 50; i += 1) {
    await new Promise((resolve) => setImmediate(resolve));
    if (flipVerdict(logger) !== null) return;
  }
}

module.exports = { flipVerdict, drainTicks, flushFlip, VERDICT_LOG_MSG };
