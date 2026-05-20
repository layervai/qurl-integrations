// SQS-driven view-update consumer (feat #60, sub-second view counter).
// Runs in the worker tier alongside the existing event-consumer. Long-
// polls the view-updates queue, parses each envelope, and dispatches
// into the process-local view-update-registry.
//
// Pairs with src/view-update-publisher.js. Envelope contract is owned
// by the publisher's module header; this module MIRRORS it. Any field
// added on the publisher side must be added here in lockstep or the
// dispatch-on-missing-field path will silently drop every message.
//
// Process-singleton: module-level state. At most one consumer per
// process. start() is no-op-with-warn on second call.
//
// At-least-once delivery semantics: SQS Standard guarantees at-least-
// once. The dispatch path is idempotent at the view-counter rendering
// layer — a duplicate dispatch for the same access_count is a no-op in
// monitorLinkStatus's render logic (it compares against the previously-
// rendered state). No dedup LRU here; the polling fallback in
// commands.js's monitorLinkStatus is the authoritative path, and SQS
// push is a latency optimization on top.
//
// DeleteMessage discipline: messages are deleted REGARDLESS of dispatch
// outcome (hit OR silent-drop-on-miss). Holding a message until the
// "right" replica picks it up would just re-deliver to the wrong
// replica on visibility-timeout expiry — the polling fallback covers
// the miss case correctly.

const { SQSClient, ReceiveMessageCommand, DeleteMessageCommand } = require('@aws-sdk/client-sqs');
const config = require('./config');
const logger = require('./logger');
const registry = require('./view-update-registry');

const RECEIVE_WAIT_SECONDS = 20;
const MAX_MESSAGES_PER_RECEIVE = 10;
const RECEIVE_VISIBILITY_SECONDS = 30;

let sqsClient = null;
let running = false;
let stopController = null;
let loopPromise = null;
let onFatalCb = null;

function _setSqsClientForTest(client) {
  sqsClient = client;
}

function createSqsClient() {
  return new SQSClient({ region: process.env.AWS_REGION || 'us-east-2' });
}

function isAbortError(err) {
  return err && (err.name === 'AbortError' || err.code === 'AbortError');
}

async function deleteMessage(receiptHandle) {
  try {
    await sqsClient.send(new DeleteMessageCommand({
      QueueUrl: config.QURL_BOT_VIEW_UPDATES_QUEUE_URL,
      ReceiptHandle: receiptHandle,
    }));
  } catch (err) {
    if (isAbortError(err)) return;
    logger.warn('view-update-consumer: DeleteMessage failed', {
      error: err.message,
    });
  }
}

// Parse-and-dispatch one SQS message. Always returns; never throws.
// Caller deletes the message after this returns.
function processMessage(message) {
  let envelope;
  try {
    envelope = JSON.parse(message.Body);
  } catch (err) {
    logger.warn('view-update-consumer: malformed JSON envelope, dropping', {
      error: err.message,
      bodyPrefix: typeof message.Body === 'string' ? message.Body.slice(0, 64) : null,
    });
    return;
  }
  const qurlId = envelope?.qurl_id;
  if (typeof qurlId !== 'string' || !qurlId) {
    logger.warn('view-update-consumer: envelope missing qurl_id, dropping');
    return;
  }
  const accessCount = envelope.access_count;
  if (!Number.isSafeInteger(accessCount) || accessCount < 0) {
    logger.warn('view-update-consumer: envelope access_count not a non-negative safe integer, dropping', {
      qurl_id: qurlId,
      access_count: accessCount,
    });
    return;
  }
  registry.dispatch(qurlId, {
    accessCount,
    consumed: envelope.consumed === true,
    eventId: typeof envelope.event_id === 'string' ? envelope.event_id : null,
    publishedAtMs: typeof envelope.published_at_ms === 'number' ? envelope.published_at_ms : null,
  });
  // Return whether dispatch hit is intentionally NOT logged here —
  // the (N-1)/N silent-drop rate would create unbounded log volume
  // with low signal. The registry exposes _sizeForTest() for tests
  // to observe hit/miss without log scraping.
}

async function pollOnce() {
  let resp;
  try {
    resp = await sqsClient.send(new ReceiveMessageCommand({
      QueueUrl: config.QURL_BOT_VIEW_UPDATES_QUEUE_URL,
      MaxNumberOfMessages: MAX_MESSAGES_PER_RECEIVE,
      WaitTimeSeconds: RECEIVE_WAIT_SECONDS,
      VisibilityTimeout: RECEIVE_VISIBILITY_SECONDS,
    }), { abortSignal: stopController.signal });
  } catch (err) {
    if (isAbortError(err)) return;
    logger.warn('view-update-consumer: ReceiveMessage failed', {
      error: err.message,
    });
    return;
  }
  const messages = resp.Messages || [];
  for (const message of messages) {
    processMessage(message);
    // Delete after processing; sequential awaits keep the in-flight
    // SDK call count bounded by 1 — at 10 messages × ~10ms each
    // that's ~100ms total, well inside RECEIVE_VISIBILITY_SECONDS.
    await deleteMessage(message.ReceiptHandle);
  }
}

async function pollLoop() {
  while (running && !stopController.signal.aborted) {
    try {
      await pollOnce();
    } catch (err) {
      // pollOnce catches its own errors; this is defense-in-depth
      // against the truly unexpected (e.g., logger.warn itself
      // throwing). On a sustained unexpected failure, the loop would
      // tight-spin — break out to the onFatal handler.
      logger.error('view-update-consumer: unexpected error in pollOnce', {
        error: err?.message,
      });
      if (onFatalCb) {
        try {
          onFatalCb(err);
        } catch (cbErr) {
          logger.error('view-update-consumer: onFatal callback threw', {
            error: cbErr?.message,
          });
        }
      }
      return;
    }
  }
}

function start({ onFatal } = {}) {
  if (running) {
    logger.warn('view-update-consumer: start() called while already running — no-op');
    return;
  }
  if (!config.ENABLE_VIEW_UPDATE_PUSH) {
    throw new Error('view-update-consumer: start() called with ENABLE_VIEW_UPDATE_PUSH=false');
  }
  if (!config.QURL_BOT_VIEW_UPDATES_QUEUE_URL) {
    throw new Error('view-update-consumer: QURL_BOT_VIEW_UPDATES_QUEUE_URL is not set');
  }
  if (!sqsClient) {
    sqsClient = createSqsClient();
  }
  running = true;
  stopController = new AbortController();
  onFatalCb = typeof onFatal === 'function' ? onFatal : null;
  logger.info('view-update-consumer: starting', {
    queueUrl: config.QURL_BOT_VIEW_UPDATES_QUEUE_URL,
    waitSeconds: RECEIVE_WAIT_SECONDS,
    visibilitySeconds: RECEIVE_VISIBILITY_SECONDS,
  });
  loopPromise = pollLoop().catch((err) => {
    logger.error('view-update-consumer: poll loop crashed', { error: err?.message });
  });
}

async function stop() {
  if (!running || stopController.signal.aborted) return;
  stopController.abort();
  logger.info('view-update-consumer: stopping');
  running = false;
  // Await loopPromise so the caller's await-resolution coincides with
  // the loop having unwound — important for gracefulShutdown
  // ordering vs. process exit.
  if (loopPromise) {
    try {
      await loopPromise;
    } catch {
      // pollLoop's outer .catch already logged.
    }
  }
  logger.info('view-update-consumer: stopped');
}

function _resetStateForTest() {
  running = false;
  sqsClient = null;
  stopController = null;
  loopPromise = null;
  onFatalCb = null;
}

module.exports = {
  start,
  stop,
  _test: {
    _setSqsClientForTest,
    _resetStateForTest,
    pollOnce,
    processMessage,
    isRunning: () => running,
  },
};
