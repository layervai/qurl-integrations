const logger = require('./logger');
const { emitMintFailureAudit } = require('./mint-failure-audit');

function startMintFailureCanary({ enabled, endpoint }) {
  if (!enabled) return null;
  let host;
  try { host = new URL(endpoint).hostname; } catch { return null; }
  if (host !== 'api.layerv.xyz') {
    logger.warn('Mint failure canary refused a non-sandbox API endpoint');
    return null;
  }
  const emit = () => {
    try {
      emitMintFailureAudit({ status: 503 }, {
        sendId: 'TEST-mint-failure-canary', kind: 'file', synthetic: true,
      });
    } catch {
      // A failed test sample must not stop the customer bot.
      try {
        logger.warn('TEST canary: mint-failure audit sample could not be emitted');
      } catch {
        // The missing-canary alarm covers a failed warning too.
      }
    }
  };
  emit();
  const timer = setInterval(emit, 60 * 60 * 1000);
  timer.unref();
  return timer;
}

module.exports = { startMintFailureCanary };
