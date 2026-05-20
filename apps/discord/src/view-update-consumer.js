// SQS-driven view-update consumer (feat #60, sub-second view counter).
// Runs in the HTTP tier — same process that owns the webhook receiver
// (publisher) AND the live monitorLinkStatus instances (dispatch
// targets). Long-polls the view-updates queue, parses each envelope,
// and dispatches into the process-local view-update-registry.
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
//
// During shutdown: if stop() aborts after processMessage (synchronous,
// fast) but before deleteMessageBatch lands, the registry has already
// dispatched but the SQS message will redeliver after visibility
// timeout. Idempotent dispatch covers correctness (the
// `status === 'opened'` guard in the handler no-ops the redeliver);
// operators will see a duplicate access_count dispatch in metrics
// during the graceful-shutdown window. Acceptable cost — the
// alternative is awaiting the delete inside the abort signal, which
// extends shutdown latency past the 10s budget.

const { SQSClient, ReceiveMessageCommand, DeleteMessageBatchCommand } = require('@aws-sdk/client-sqs');
const config = require('./config');
const logger = require('./logger');
const registry = require('./view-update-registry');

const RECEIVE_WAIT_SECONDS = 20;
const MAX_MESSAGES_PER_RECEIVE = 10;
// 30s visibility timeout assumes synchronous processMessage (parse +
// registry.dispatch, no awaits). Worst-case batch of 10 messages at
// ~ms each is well under budget. If a future change makes
// processMessage async (e.g., an awaited dedup-LRU lookup against
// DDB), the 30s budget tightens — revisit this constant.
const RECEIVE_VISIBILITY_SECONDS = 30;
// Backoff after a ReceiveMessage error. Without this, a persistent
// fast-failure mode (IAM denied after credentials rotation, malformed
// QueueUrl, region drift) would spin the loop with zero delay between
// fails — burns CPU + log volume. 1s matches event-consumer.js's
// POLL_ERROR_BACKOFF_MS. AbortController short-circuits the sleep so
// gracefulShutdown still returns in tens of ms.
const POLL_ERROR_BACKOFF_MS = 1000;

let sqsClient = null;
let running = false;
let stopController = null;
let loopPromise = null;
let onFatalCb = null;

function _setSqsClientForTest(client) {
  sqsClient = client;
}

function createSqsClient() {
  // Same shape as event-publisher.js / event-consumer.js — reject +
  // throw on missing region. A wrong region surfaces as opaque
  // ReceiveMessage failures otherwise.
  const region = (process.env.AWS_REGION ?? '').trim();
  if (!region) {
    throw new Error('AWS_REGION is required to use the view-update consumer. Set it in the deployment template (e.g. `us-east-2`).');
  }
  return new SQSClient({ region });
}

// Local abortable sleep — resolves on timeout OR on stopController
// abort, whichever lands first. Used by pollLoop's error backoff so
// stop() returns in tens of ms instead of waiting for the backoff.
// Mirrors event-consumer.js's abortableSleep shape.
function abortableSleep(ms) {
  return new Promise((resolve) => {
    const ctrl = stopController;
    if (ctrl?.signal.aborted) {
      resolve();
      return;
    }
    let onAbort;
    const t = setTimeout(() => {
      if (ctrl && onAbort) ctrl.signal.removeEventListener('abort', onAbort);
      resolve();
    }, ms);
    t.unref?.();
    if (ctrl) {
      onAbort = () => {
        clearTimeout(t);
        resolve();
      };
      ctrl.signal.addEventListener('abort', onAbort, { once: true });
    }
  });
}

// Mirrors event-consumer.js's isAbortError: walks err.cause with a
// visited-set so cyclic / deeply-wrapped abort chains still match.
// Both shapes (name + code) cover Node + AWS SDK conventions.
function isAbortError(err) {
  const visited = new Set();
  let current = err;
  while (current && typeof current === 'object' && !visited.has(current)) {
    visited.add(current);
    if (current.name === 'AbortError'
      || current.name === 'CanceledError'
      || current.code === 'AbortError'
      || current.code === 'ABORT_ERR') return true;
    current = current.cause;
  }
  return false;
}

// Batch-delete up to 10 messages in one SDK round-trip. SQS's
// DeleteMessageBatch accepts 1–10 entries with caller-supplied Ids
// (we use the array index as Id — only needs uniqueness within the
// batch). Failures are reported in `Failed[]` on the response; we
// log + continue rather than re-delete (the message will redeliver
// after visibility timeout — and the dispatch path is idempotent,
// so a redeliver of an already-dispatched event renders the same
// linkStatus state via the polling fallback at the worst).
async function deleteMessageBatch(messages) {
  if (messages.length === 0) return;
  try {
    // Pass abortSignal so a graceful-shutdown that lands mid-delete
    // returns within tens of ms (matches the ReceiveMessage abort
    // posture below). Consistency point with the receive path.
    const resp = await sqsClient.send(new DeleteMessageBatchCommand({
      QueueUrl: config.QURL_BOT_VIEW_UPDATES_QUEUE_URL,
      Entries: messages.map((m, i) => ({
        Id: String(i),
        ReceiptHandle: m.ReceiptHandle,
      })),
    }), { abortSignal: stopController?.signal });
    if (resp.Failed && resp.Failed.length > 0) {
      logger.warn('view-update-consumer: DeleteMessageBatch had partial failures', {
        failed_count: resp.Failed.length,
        errors: resp.Failed.map((f) => ({ id: f.Id, code: f.Code, message: f.Message })),
      });
    }
  } catch (err) {
    if (isAbortError(err)) return;
    logger.warn('view-update-consumer: DeleteMessageBatch failed', {
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
  if (envelope === null || typeof envelope !== 'object') {
    logger.warn('view-update-consumer: envelope is not an object, dropping');
    return;
  }
  const qurlId = envelope.qurl_id;
  if (typeof qurlId !== 'string' || !qurlId) {
    logger.warn('view-update-consumer: envelope missing qurl_id, dropping');
    return;
  }
  const accessCount = envelope.access_count;
  if (!Number.isSafeInteger(accessCount) || accessCount <= 0) {
    // Tightened from `< 0` to `<= 0` so 0-count envelopes drop at
    // the parse boundary (matches the handler's `accessCount > 0`
    // gate). qurl.accessed events always carry `access_count >= 1`,
    // so a 0 would be a wire-shape regression worth surfacing.
    logger.warn('view-update-consumer: envelope access_count not a positive safe integer, dropping', {
      qurl_id: qurlId,
      access_count: accessCount,
    });
    return;
  }
  registry.dispatch(qurlId, {
    accessCount,
    consumed: envelope.consumed === true,
    // TODO(view-update-dedup, #476): eventId is plumbed end-to-end so a
    // future dedup layer (in the registry or the monitor's
    // handleViewUpdate) can drop SQS at-least-once duplicates without
    // a wire-shape change. No consumer reads it today — the polling
    // path's status === 'opened' guard makes per-event dedup
    // unnecessary for the current render contract.
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
    }), { abortSignal: stopController?.signal });
  } catch (err) {
    if (isAbortError(err)) return;
    logger.warn('view-update-consumer: ReceiveMessage failed', {
      error: err.message,
    });
    // Back off so a persistent failure (IAM denial, malformed
    // QueueUrl, region drift) doesn't tight-spin the poll loop.
    await abortableSleep(POLL_ERROR_BACKOFF_MS);
    return;
  }
  const messages = resp.Messages || [];
  for (const message of messages) {
    processMessage(message);
  }
  // Single batch round-trip for up to 10 deletes vs. sequential
  // per-message DeleteMessage calls (~10ms each = ~100ms wall-time
  // for a full batch). Failures inside the batch are logged + ignored;
  // visibility timeout + idempotent dispatch handle the redelivery.
  await deleteMessageBatch(messages);
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
      // Flip running=false BEFORE invoking onFatalCb so module state
      // matches reality. Production wiring always
      // passes onFatal=gracefulShutdown which would call stop() and
      // flip this anyway, but the onFatal contract is documented
      // optional — without this flip, a no-onFatal caller would be
      // wedged with isRunning()=true but no actual polling. A
      // subsequent start() would warn-and-no-op, masking the
      // failure.
      running = false;
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
  // Safe-nav on stopController in case start() partially-initialized
  // (set running=true then threw before stopController = new ...
  // landed). Practically impossible since `new AbortController()`
  // shouldn't throw, but free defense vs. a future refactor.
  if (!running || stopController?.signal.aborted) return;
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
    // Lets tests drive pollOnce directly without spawning the
    // background pollLoop via start(). Required because pollOnce
    // needs stopController.signal to exist (the abort plumbing for
    // the long-poll receive).
    _setStopControllerForTest: (controller) => { stopController = controller; },
    _setRunningForTest: (v) => { running = v; },
    pollOnce,
    processMessage,
    isRunning: () => running,
  },
};
