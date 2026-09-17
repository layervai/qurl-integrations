
const VERDICT_LOG_MSG = 'qURL webhook qurl.accessed-consumed: flip verdict';

function flipVerdict(logger) {
  const call = logger.debug.mock.calls.find(([msg]) => msg === VERDICT_LOG_MSG);
  if (!call) return null;
  const { status, transient } = call[1];
  return { status, transient };
}

async function drainTicks(n = 8) {
  for (let i = 0; i < n; i += 1) {
    await new Promise((resolve) => setImmediate(resolve));
  }
}

async function flushFlip(logger) {
  for (let i = 0; i < 50; i += 1) {
    await new Promise((resolve) => setImmediate(resolve));
    if (flipVerdict(logger) !== null) return;
  }
}

module.exports = { flipVerdict, drainTicks, flushFlip, VERDICT_LOG_MSG };
