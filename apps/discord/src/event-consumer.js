// SQS-driven event consumer for the worker tier (zero-downtime
// design, Pillar 1). The gateway tier publishes every Discord
// dispatch as an SQS message; this module long-polls the queue,
// reconstructs an Interaction object via discord.js's internal
// dispatch path, and emits `interactionCreate` on the local client.
// The existing `interactionCreate` listener (src/index.js, gated on
// `isGateway || isWorker`) routes the reconstructed interaction
// through handleCommand / handleFlowInteraction — same dispatcher
// the gateway role uses today.
//
// Process-singleton: every piece of state in this module — the poll
// loop's running/stopping flags, the AbortController, the SQS
// client, the dedup LRU — lives in module-level scope. There is at
// most one consumer per process by design.
// A future caller that tries to `start()` two consumers in one
// process will hit the second-start warn in `start()` and the
// duplicate poll won't take. If multi-consumer is ever needed,
// promote the state to an instance/closure shape.
//
// Reconstruction path: `client.actions.InteractionCreate.handle(data)`.
// This is discord.js@14.25.1 internal API (not exported by the
// package's public surface) — same function the library uses on a
// real INTERACTION_CREATE gateway dispatch. We pin discord.js to an
// exact version (no `~`) in package.json so a minor-version bump
// can't silently break the reconstruction shape, and
// `tests/event-consumer.test.js` smoke-tests the factory output
// against the methods our handlers depend on (deferReply, editReply,
// options.getString, customId, isChatInputCommand, etc.).
//
// Envelope contract — PR 10's producer must publish JSON in this
// shape (string body, single message attribute optional):
//
//   {
//     "eventType": "INTERACTION_CREATE",  // only type today
//     "shardId":   "0:1",                  // for future sharding LRU keys
//     "data":      { ...raw Discord INTERACTION_CREATE `d` payload... },
//     "event_id":  "0:1234567"             // <shardId>:<sequence>, dedup hint
//   }
//
// At-least-once delivery: SQS Standard guarantees at-least-once, so
// the consumer is idempotency-tolerant. For interactions specifically:
//   - Discord ACKs are naturally idempotent — a duplicate's
//     `/callback` returns "Unknown interaction" (token already
//     consumed) and the handler logs + exits.
//   - Flow-state transitions are guarded by DDB OCC
//     (`version = :expected`), so a duplicate that gets through to
//     `transitionFlow` lands a ConditionalCheckFailedException —
//     the handler treats this as "another worker advanced this
//     flow" and exits without acting.
// The event_id LRU below is a TELEMETRY signal (lets ops quantify
// the dup rate observed in prod); it is NOT the correctness primitive.
//
// DeleteMessage timing: we delete immediately after
// `client.actions.InteractionCreate.handle(data)` returns. discord.js's
// emit is synchronous but the registered listener (handleCommand /
// handleFlowInteraction) is async and runs detached on the event
// loop. Awaiting the listener's promise from the consumer would
// require either a side-channel WeakMap registry (handlers populate,
// consumer reads) or a from-scratch dispatcher that bypasses
// EventEmitter — both intrusive. Deleting eagerly is correct here
// because:
//   (a) Discord interaction tokens are valid for ~3s for the initial
//       ACK; if the handler crashes before ACK, an SQS retry many
//       seconds later (visibility timeout 60s) wouldn't be able to
//       respond either — the user already sees "interaction failed".
//   (b) Crashes that occur post-ACK (e.g., mid-DM-fan-out) are
//       persisted in flow-state DDB rows with their version, so the
//       /qurl send flow surfaces them on subsequent user action;
//       a redelivery of the original INTERACTION_CREATE wouldn't
//       help anyway.
// The visibility-timeout retry path remains the safety net for the
// narrow class of failures where the consumer process crashes between
// `handle()` and `DeleteMessage` — those are bounded by maxReceiveCount
// and end up in the DLQ for operator triage.

const { SQSClient, ReceiveMessageCommand, DeleteMessageCommand } = require('@aws-sdk/client-sqs');
const config = require('./config');
const logger = require('./logger');

// AWS_REGION is required (matches src/flow-state.js + src/store/ddb-store.js
// patterns — no `|| 'us-east-2'` fallback because a missing AWS_REGION
// would otherwise silently land in whichever region the SDK defaults to,
// which is exactly the misconfiguration we want to surface at boot).
//
// Read fresh inside createSqsClient() rather than captured at module
// load. Two reasons: (a) consistent with how the SDK clients are
// constructed elsewhere — flow-state.js captures at load but throws
// at module load too; we throw lazily, so we should read lazily; and
// (b) defensive against shim bootstrappers that set
// `process.env.AWS_REGION` after the bot's modules are required.
// In practice flow-state.js's universal-load-path throw fires first
// for any boot, so the lazy read here is defense-in-depth, but the
// asymmetry-with-where-we-read becomes load-bearing if that load-
// path assumption ever changes.

// SQS polling parameters. Hardcoded module constants — see the
// rationale below for each. If an operator needs to tune these in
// flight, add env overrides via the intEnv pattern in config.js
// (matches REFRESH_INTERVAL_MS in src/http-only-init.js).
//
// MaxNumberOfMessages (10) — SQS's API cap. We accept whatever the
// queue serves and process them in parallel.
//
// WaitTimeSeconds (20) — long-poll. The maximum value SQS allows;
// minimizes empty-receive cost when the queue is idle. Costs 0 if no
// message arrives in the 20s window.
//
// VisibilityTimeout (60) — how long after receive before the message
// is re-served to another consumer. Has to be > expected handler
// runtime; our slowest path (DM fan-out for `@everyone`) is bounded
// by the per-batch self-destruct creation, NOT the DM send loop
// (which runs detached after ACK), so 60s is plenty. Also has to be
// well under Discord's interaction-token TTL (15 min) so a retry
// could in principle still respond — defense in depth even though
// the "ACK before crash" class of failures isn't recoverable.
const RECEIVE_MAX_MESSAGES = 10;
const RECEIVE_WAIT_SECONDS = 20;
const RECEIVE_VISIBILITY_SECONDS = 60;

// Backoff between poll iterations when ReceiveMessage itself throws
// (transient AWS error, throttling, network blip). Short enough that
// recovery is fast; long enough that a sustained outage doesn't spin
// the event loop. NOT used between successful empty receives — those
// already absorb 20s via long-poll wait.
const POLL_ERROR_BACKOFF_MS = 1000;

// Event-id LRU cap. 100k entries chosen to cover the SQS-redrive
// window (>= 60s after receive) at sustained ~1k events/s on a busy
// gateway shard — i.e., the failure mode this telemetry exists to
// surface. With a 10k cap, redrives ≥10s out under sustained load
// would fall outside the window and the dup-rate gauge would lie
// during the exact incident it's meant to expose. On a quiet bot,
// the window stretches to hours; that's fine — the LRU is telemetry-
// only, OCC at the flow-state layer owns correctness (see module
// header). Memory cost: ~100k * (~16-char string + Set overhead) ≈
// a few MB worst case. Trimmed FIFO on insertion-order eviction —
// Set (like Map) preserves insertion order in JS, so the oldest
// entry is always the first key.
const SEEN_EVENT_ID_CAP = 100_000;

// LRU of recently-seen event_ids (envelope's `event_id` field,
// format `<shardId>:<sequence>`). `Set` rather than `Map<id, 1>`
// because we only care about membership — the value field of a Map
// would never be read. delete-then-add is the recency-refresh idiom.
const seenEventIds = new Set();

function recordSeen(eventId) {
  if (!eventId) return false;
  if (seenEventIds.has(eventId)) {
    // Refresh recency: move to the tail of insertion order so it
    // isn't first to evict if traffic spikes.
    seenEventIds.delete(eventId);
    seenEventIds.add(eventId);
    return true;
  }
  seenEventIds.add(eventId);
  if (seenEventIds.size > SEEN_EVENT_ID_CAP) {
    // Drop the oldest entry. Set.values() returns insertion order,
    // so .next().value is the oldest.
    const oldest = seenEventIds.values().next().value;
    seenEventIds.delete(oldest);
  }
  return false;
}

// Worker state. start() flips `running` true and kicks off the poll
// loop; stop() flips `stopping` true and awaits loopPromise.
// pollOnce uses Promise.all over the per-message wrappers, which
// serializes "all messages in this batch were dispatched (emit +
// DeleteMessage attempted)" against loopPromise resolution.
//
// What stop() does NOT await: the async handlers registered on
// `interactionCreate` (handleCommand / handleFlowInteraction) run
// detached after the synchronous `client.actions.InteractionCreate.handle`
// emit. Their promises aren't captured anywhere we can await. A
// handler still mid-DDB-write when SIGTERM lands could race
// gracefulShutdown's eventual `db.close()` — same race the
// pre-existing in-process gateway path has, since the gateway also
// emits + runs detached. Not worse here than today.
//
// If a tighter "drain handlers before db close" guarantee is ever
// needed, options are: (a) capture handler promises via a WeakMap
// registry the listener populates and the consumer reads; (b) call
// the dispatcher functions directly instead of going through the
// emit path. Both are PR-10-or-later refactors.
//
// receiveAbortController is set per-poll-iteration and used by stop()
// to cancel an in-flight long-poll ReceiveMessage. Without this, a
// SIGTERM arriving while the consumer is parked in its (typical)
// 20-second long-poll wait would block stop() for the remainder of
// the wait — past the 10-second force-exit in gracefulShutdown
// (index.js), so process.exit(1) would fire before discordShutdown
// + db.close had a chance to run.
let running = false;
let stopping = false;
let loopPromise = null;
let receiveAbortController = null;
let sqsClient = null;

// Hook for unit tests: lets the test inject a mock SQSClient
// (aws-sdk-client-mock'd in tests/event-consumer.test.js) without
// constructing a real client at module load. Pure DI — production
// start() constructs the real client itself.
function _setSqsClientForTest(client) {
  sqsClient = client;
}

function createSqsClient() {
  const region = (process.env.AWS_REGION ?? '').trim();
  if (!region) {
    throw new Error('AWS_REGION is required to use the event consumer. Set it in the deployment template (e.g. `us-east-2`).');
  }
  return new SQSClient({ region });
}

// Every terminal path in processMessage deletes the message (success,
// malformed body, unknown eventType, reconstruction throw — see the
// module header's "DeleteMessage timing" rationale). Single helper so
// the three sites stay in lockstep and a future change touches one place.
async function deleteMessage(receiptHandle) {
  await sqsClient.send(new DeleteMessageCommand({
    QueueUrl: config.QURL_BOT_EVENTS_QUEUE_URL,
    ReceiptHandle: receiptHandle,
  }));
}

/**
 * Process one SQS message: parse the envelope, dispatch via
 * client.actions.InteractionCreate.handle, delete on success.
 *
 * Throws on parse failure or SQS API error so the caller's
 * try/catch can record the failure. Does NOT throw on dispatch
 * failures inside the handler (those bubble through the emit, but
 * EventEmitter's default behavior swallows async listener
 * rejections — the global `unhandledRejection` handler in index.js
 * catches them and logs). Per the module-header rationale, we
 * delete the message regardless of handler outcome.
 */
async function processMessage(client, message) {
  let parsed;
  try {
    parsed = JSON.parse(message.Body);
  } catch (err) {
    // Malformed envelope. Delete so we don't redrive a poison pill
    // through maxReceiveCount; surface loudly so a producer bug is
    // visible. The DLQ would also catch this after the redrive
    // policy fires, but eager-delete avoids the redelivery noise.
    logger.error('Event consumer: malformed message body, deleting', {
      messageId: message.MessageId,
      error: err.message,
    });
    await deleteMessage(message.ReceiptHandle);
    return;
  }

  // JSON.parse('null') succeeds with `parsed === null`, and
  // destructuring `null` throws TypeError — which would exit
  // processMessage BEFORE the DeleteMessage at the bottom, violating
  // the "delete on every terminal path" invariant (the message
  // would redrive until DLQ instead). Primitive/array bodies are
  // valid JSON too and would land in the unhandled-eventType branch
  // (already a DeleteMessage path), but rejecting them up-front is
  // simpler than relying on that branch and pins the contract that
  // the envelope is an object.
  if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) {
    logger.error('Event consumer: envelope is not a JSON object, deleting', {
      messageId: message.MessageId,
      bodyType: parsed === null ? 'null' : (Array.isArray(parsed) ? 'array' : typeof parsed),
    });
    await deleteMessage(message.ReceiptHandle);
    return;
  }

  const { eventType, data, event_id: eventId } = parsed;

  if (eventType !== 'INTERACTION_CREATE') {
    // Single-event-type today; PR 12+ may add MESSAGE_CREATE,
    // GUILD_CREATE, etc. An unknown type from a future producer
    // shouldn't block the queue — log + delete + move on.
    logger.warn('Event consumer: unhandled eventType, deleting', {
      eventType,
      messageId: message.MessageId,
    });
    await deleteMessage(message.ReceiptHandle);
    return;
  }

  const isDup = recordSeen(eventId);
  if (isDup) {
    // Debug log only — at-least-once delivery means a "dup" might
    // actually be the first real attempt that an earlier worker
    // received but didn't ACK, so we still process. Correctness is
    // owned by Discord's interaction-token uniqueness and OCC at
    // the flow-state layer. If ops wants an aggregatable dup-rate
    // metric, add a `qurl_bot_event_dup_total{shardId}` counter
    // alongside this log (reservation slot for the observability
    // Phase 1.0 follow-on PR).
    logger.debug('Event consumer: event_id seen recently', {
      eventId,
      shardId: parsed.shardId,
    });
  }

  // Reconstruct + emit via discord.js's internal dispatch path.
  // client.actions.InteractionCreate.handle is the same function the
  // WebSocketShard invokes on a real INTERACTION_CREATE gateway
  // dispatch (see node_modules/discord.js/src/client/actions/InteractionCreate.js).
  // It picks the right Interaction subclass (ChatInputCommand,
  // Button, ModalSubmit, etc.) based on data.type +
  // data.data.type|component_type, instantiates it with `new
  // InteractionClass(client, data)`, and emits 'interactionCreate'.
  // The shared listener registered in src/index.js (gated on
  // isGateway || isWorker) handles routing.
  try {
    client.actions.InteractionCreate.handle(data);
  } catch (err) {
    // Reconstruction failure (e.g., data shape we don't know how to
    // handle). Log but still delete — a poison message that throws
    // every time would otherwise loop until DLQ; logging surfaces
    // the producer bug.
    logger.error('Event consumer: dispatch reconstruction failed', {
      error: err.message,
      eventId,
      messageId: message.MessageId,
    });
  }

  await deleteMessage(message.ReceiptHandle);
}

async function pollOnce(client) {
  const queueUrl = config.QURL_BOT_EVENTS_QUEUE_URL;
  const cmd = new ReceiveMessageCommand({
    QueueUrl: queueUrl,
    MaxNumberOfMessages: RECEIVE_MAX_MESSAGES,
    WaitTimeSeconds: RECEIVE_WAIT_SECONDS,
    VisibilityTimeout: RECEIVE_VISIBILITY_SECONDS,
  });
  // Fresh abort controller per iteration. stop() aborts the active
  // one to cancel an in-flight long-poll. The AWS SDK rejects the
  // send() promise with an AbortError when this fires; pollLoop's
  // catch handles it (cleared as a non-fatal interruption) and the
  // `while (!stopping)` check exits the loop.
  receiveAbortController = new AbortController();
  const { Messages = [] } = await sqsClient.send(cmd, {
    abortSignal: receiveAbortController.signal,
  });
  if (Messages.length === 0) return;

  // Process messages in parallel. Each wrapper catches its own
  // failures so one rejection doesn't collapse the Promise.all and
  // strand siblings. The only escape from `processMessage` is a
  // DeleteMessage throw — every other error path is internally
  // caught + delete-eager. So this log is specifically the
  // SQS-delete-failed signal: the message will redrive after the
  // visibility timeout and re-dispatch the interaction. OCC at
  // flow-state + Discord token-uniqueness guard the post-handler
  // side effects; counts of this log line are the operational
  // signal for "how often do we redrive a successfully-dispatched
  // interaction." Worth alerting on if it climbs.
  await Promise.all(Messages.map(async (message) => {
    try {
      await processMessage(client, message);
    } catch (err) {
      logger.error('Event consumer: DeleteMessage failed; message will redeliver after visibility timeout', {
        error: err.message,
        messageId: message.MessageId,
      });
    }
  }));
}

// AbortController surfaces aborted requests with err.name === 'AbortError'
// at the AWS SDK layer; some runtimes also expose the same condition
// via err.code. Different @aws-sdk minor versions have also been
// observed to wrap aborts as `CanceledError` (smithy client). Match
// both shapes so a stop()-triggered abort doesn't get logged as a
// real failure or trigger the error backoff.
//
// Deliberately does NOT match err.name === 'TimeoutError': that's
// the SDK's own request-timeout, NOT our abort. A sustained AWS-side
// flaky endpoint that keeps tripping client-side timeouts MUST land
// in the error-backoff path so an operator sees the log line and the
// loop doesn't spin without a backoff.
function isAbortError(err) {
  if (!err) return false;
  return err.name === 'AbortError'
    || err.name === 'CanceledError'
    || err.code === 'AbortError';
}

// Sleep that resolves either when the timeout fires OR when stop()
// flips `stopping` true. Without the early-out, a stop() call during
// the error-backoff would block for the full POLL_ERROR_BACKOFF_MS
// before the while-check exits — on top of the 10s graceful-shutdown
// budget, the abort-the-receive savings would be partly undone.
//
// Both timers MUST clear each other on resolve. The setInterval
// would otherwise keep firing every 50 ms forever after the
// setTimeout wins the race (the common case: a single transient
// AWS error → backoff completes → loop continues). Under sustained
// failure, every iteration adds an orphan interval. .unref() lets
// the process exit cleanly but doesn't stop the ticks; clearing
// inside the resolve handler does.
function abortableSleep(ms) {
  return new Promise((resolve) => {
    let check;
    const t = setTimeout(() => {
      if (check) clearInterval(check);
      resolve();
    }, ms);
    // stop() doesn't expose a Promise we can await; poll the
    // `stopping` flag at a coarse interval so the sleep returns
    // promptly when shutdown lands. 50 ms is fine — well under the
    // 1 s POLL_ERROR_BACKOFF_MS and well above the cost of a
    // setInterval tick.
    check = setInterval(() => {
      if (stopping) {
        clearTimeout(t);
        clearInterval(check);
        resolve();
      }
    }, 50);
    // .unref()ed so a stray timer doesn't pin the event loop if
    // the loop exits via a different path mid-sleep.
    t.unref?.();
    check.unref?.();
  });
}

async function pollLoop(client) {
  while (!stopping) {
    try {
      await pollOnce(client);
    } catch (err) {
      if (isAbortError(err)) {
        // stop() aborted the in-flight receive. The while-loop's
        // !stopping check exits next iteration; no logging or
        // backoff (this is a clean interruption, not a failure).
        continue;
      }
      // ReceiveMessage failure (transient AWS, throttling, etc.) —
      // brief backoff so we don't burn CPU on a sustained outage.
      // pollOnce's per-message errors are caught inside the
      // Promise.all wrapper, so reaching here means the *receive*
      // itself failed.
      logger.error('Event consumer: poll iteration failed', { error: err.message });
      await abortableSleep(POLL_ERROR_BACKOFF_MS);
    }
  }
}

/**
 * Start polling the SQS queue. Returns immediately; the poll loop
 * runs detached. Idempotent — a second call while already running
 * is a no-op (warn-logged so an accidental double-start is visible).
 *
 * @param {import('discord.js').Client} client - Discord client with
 *   .actions.InteractionCreate.handle available (i.e., a real
 *   discord.js Client, not a stub).
 */
function start(client) {
  if (running) {
    logger.warn('Event consumer: start() called while already running — no-op');
    return;
  }
  if (!config.ENABLE_EVENT_SHIPPER) {
    // Defense in depth — caller in index.js already gates on
    // isWorker (which includes the flag), but explicit here so a
    // future caller without the gate doesn't accidentally bring up
    // a consumer against an unset queue URL.
    throw new Error('Event consumer: start() called with ENABLE_EVENT_SHIPPER=false');
  }
  if (!config.QURL_BOT_EVENTS_QUEUE_URL) {
    throw new Error('Event consumer: QURL_BOT_EVENTS_QUEUE_URL is not set');
  }
  if (!sqsClient) {
    sqsClient = createSqsClient();
  }
  running = true;
  stopping = false;
  logger.info('Event consumer: starting', {
    queueUrl: config.QURL_BOT_EVENTS_QUEUE_URL,
    waitSeconds: RECEIVE_WAIT_SECONDS,
    visibilitySeconds: RECEIVE_VISIBILITY_SECONDS,
  });
  loopPromise = pollLoop(client);
}

/**
 * Signal the poll loop to stop, abort any in-flight long-poll, and
 * await `loopPromise`. Awaits the poll loop's *dispatch + delete*
 * cycle — NOT the async handlers fired via emit, which run detached
 * (see "What stop() does NOT await" in the module header for the
 * full contract).
 *
 * Idempotent on both not-running and already-stopping.
 *
 * The mid-flight ReceiveMessage is aborted via the AbortController
 * (rather than allowed to time out at WaitTimeSeconds) so stop()
 * returns within tens of ms — critical for fitting inside the 10 s
 * graceful-shutdown budget in `gracefulShutdown` (index.js).
 */
async function stop() {
  // Idempotent on both not-running AND already-stopping: two
  // concurrent stop() calls (e.g., SIGTERM racing an uncaughtException
  // both routing through gracefulShutdown) would otherwise both
  // pass !running, both abort, both await loopPromise. Harmless but
  // racy on the running=false reset; the stopping check makes the
  // single-stopper invariant explicit.
  if (!running || stopping) return;
  stopping = true;
  // Cancel the in-flight long-poll so stop() returns within tens of
  // ms instead of waiting up to RECEIVE_WAIT_SECONDS (20s) for the
  // current receive to time out. The pollLoop's catch recognizes
  // the abort and exits cleanly without backoff or error logging.
  // Critical for graceful-shutdown: index.js's 10s force-exit fires
  // before a synchronously-waiting receive could complete on its own.
  //
  // Narrow race: if stop() lands BETWEEN iterations (after pollOnce
  // returned, before the next iteration assigned a fresh controller)
  // we abort the already-completed previous controller, which is a
  // no-op. That's benign — the next `while (!stopping)` check exits
  // the loop without starting another receive. The abort fires
  // every other time and is the load-bearing path.
  if (receiveAbortController) {
    receiveAbortController.abort();
  }
  logger.info('Event consumer: stopping');
  try {
    await loopPromise;
  } catch (err) {
    logger.error('Event consumer: error during stop', { error: err.message });
  } finally {
    running = false;
    // Reset stopping alongside running so a caller introspecting
    // state between stop+start (or never restarting) sees a clean
    // post-condition that matches the pre-start() shape. start()
    // also resets stopping=false, so the happy path is unaffected.
    stopping = false;
    loopPromise = null;
  }
}

// Reset every piece of module-level state to its post-construction
// shape. Tests call this in beforeEach to avoid cross-contamination
// (the consumer is a process-singleton — see module header — so
// state would otherwise leak between describe blocks). NOT exposed
// in production: there's no legitimate runtime caller, only test
// harnesses that need a clean slate per test.
function _resetStateForTest() {
  running = false;
  stopping = false;
  loopPromise = null;
  receiveAbortController = null;
  sqsClient = null;
  seenEventIds.clear();
}

module.exports = {
  start,
  stop,
  _test: {
    _setSqsClientForTest,
    _resetStateForTest,
    recordSeen,
    seenEventIds,
    processMessage,
    pollOnce,
    isAbortError,
    abortableSleep,
    SEEN_EVENT_ID_CAP,
    getReceiveAbortController: () => receiveAbortController,
  },
};
