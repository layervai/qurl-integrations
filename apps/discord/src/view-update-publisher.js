// SQS-driven view-update publisher (feat #60, sub-second view counter).
// Wired into qurl-webhook.js after a successful recordQurlView returns
// `result === 'recorded'` — i.e., a real new view event (not a
// per-event dedup hit). The consumer (view-update-consumer.js) drains
// the queue on worker replicas and dispatches into the process-local
// view-update-registry.
//
// Pairs with src/view-update-consumer.js. Envelope contract:
//
//   {
//     "qurl_id":         "qrl_xxxx",
//     "access_count":    42,           // int from qurl-service WebhookEvent.Data
//     "consumed":        false,        // bool
//     "event_id":        "evt_xxxx",   // qurl-service-emitted dedup key
//     "published_at_ms": 1739462812345 // Date.now() at envelope build
//   }
//
// Latency optimization, NOT correctness primitive. The
// monitorLinkStatus polling path in commands.js (unchanged by this PR)
// is the authoritative view-counter mechanism. SQS push only shaves
// latency on the half-of-events that land on the replica running the
// monitor (see view-update-registry.js's SILENT DROP doc).
//
// Send-failure semantics: fire-and-log. A transient SQS error means
// the view event is LOST from the SQS-push perspective; the polling
// path catches it on the next interval. Mirrors event-publisher.js's
// posture — the in-process fallback (here: polling) makes a queue
// outage observable without action.
//
// Process-singleton: module-level state. At most one publisher per
// process. start() is no-op-with-warn on second call.

const { SQSClient, SendMessageCommand } = require('@aws-sdk/client-sqs');
const config = require('./config');
const logger = require('./logger');

const INITIAL_DRAIN_DEADLINE_MS = 5_000;
let DRAIN_DEADLINE_MS = INITIAL_DRAIN_DEADLINE_MS;

let sqsClient = null;
let running = false;
const inFlightSends = new Set();

function _setSqsClientForTest(client) {
  sqsClient = client;
}

function createSqsClient() {
  return new SQSClient({ region: process.env.AWS_REGION || 'us-east-2' });
}

// Fire-and-log. No await. Safe to call from the hot path
// (qurl-webhook handler).
function publish({ qurlId, accessCount, consumed, eventId }) {
  if (!running) return; // safe no-op when disabled
  if (typeof qurlId !== 'string' || !qurlId) {
    // Defensive — the caller already validates the shape. A regression
    // that called publish() with junk would otherwise build a bad
    // envelope and burn a SendMessage on it.
    logger.warn('view-update-publisher: publish() called with invalid qurlId', { qurlId });
    return;
  }
  const envelope = {
    qurl_id: qurlId,
    access_count: accessCount,
    consumed: consumed === true,
    event_id: eventId,
    published_at_ms: Date.now(),
  };
  let sendPromise;
  sendPromise = sqsClient.send(new SendMessageCommand({
    QueueUrl: config.QURL_BOT_VIEW_UPDATES_QUEUE_URL,
    MessageBody: JSON.stringify(envelope),
  }))
    .catch((err) => {
      logger.warn('view-update-publisher: SendMessage failed (fire-and-log)', {
        qurl_id: qurlId,
        error: err.message,
        kind: 'unhandledRejection',
      });
    })
    .finally(() => {
      inFlightSends.delete(sendPromise);
    });
  inFlightSends.add(sendPromise);
}

function start() {
  if (running) {
    logger.warn('view-update-publisher: start() called while already running — no-op');
    return;
  }
  if (!config.ENABLE_VIEW_UPDATE_PUSH) {
    throw new Error('view-update-publisher: start() called with ENABLE_VIEW_UPDATE_PUSH=false');
  }
  if (!config.QURL_BOT_VIEW_UPDATES_QUEUE_URL) {
    throw new Error('view-update-publisher: QURL_BOT_VIEW_UPDATES_QUEUE_URL is not set');
  }
  if (!sqsClient) {
    sqsClient = createSqsClient();
  }
  running = true;
  logger.info('view-update-publisher: starting', {
    queueUrl: config.QURL_BOT_VIEW_UPDATES_QUEUE_URL,
  });
}

// Drain in-flight SendMessage promises up to DRAIN_DEADLINE_MS.
// Mirrors event-publisher.stop()'s discipline.
async function stop() {
  if (!running) return;
  running = false;
  if (inFlightSends.size === 0) {
    logger.info('view-update-publisher: stop complete (no in-flight sends to drain)');
    return;
  }
  const drainCount = inFlightSends.size;
  const drainStartNs = process.hrtime.bigint();
  let drainTimer;
  try {
    await Promise.race([
      Promise.allSettled([...inFlightSends]),
      new Promise((resolve) => {
        drainTimer = setTimeout(resolve, DRAIN_DEADLINE_MS);
      }),
    ]);
  } finally {
    clearTimeout(drainTimer);
  }
  const elapsedMs = Number((process.hrtime.bigint() - drainStartNs) / 1_000_000n);
  if (inFlightSends.size > 0) {
    logger.warn('view-update-publisher: drain deadline elapsed, proceeding with sends still in-flight', {
      unsettled: inFlightSends.size,
      settled: drainCount - inFlightSends.size,
      elapsedMs,
    });
  } else {
    logger.info('view-update-publisher: drain complete', {
      count: drainCount,
      elapsedMs,
    });
  }
}

function _resetStateForTest() {
  running = false;
  sqsClient = null;
  inFlightSends.clear();
  DRAIN_DEADLINE_MS = INITIAL_DRAIN_DEADLINE_MS;
}

module.exports = {
  start,
  stop,
  publish,
  _test: {
    _setSqsClientForTest,
    _resetStateForTest,
    getInFlightCount: () => inFlightSends.size,
    isRunning: () => running,
    getDrainDeadlineMs: () => DRAIN_DEADLINE_MS,
    _setDrainDeadlineForTest: (ms) => { DRAIN_DEADLINE_MS = ms; },
  },
};
