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
// loop's running flag, the lifetime AbortController, the SQS
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
// Envelope contract — src/event-publisher.js publishes JSON in this
// shape:
//
//   {
//     "eventType":        "INTERACTION_CREATE", // only type today
//     "shardId":          "0",                   // shard index; reserves k:n for sharding PR
//     "data":             { ...raw Discord INTERACTION_CREATE `d` payload... },
//     "event_id":         "0:1234567",           // <shardId>:<sequence>, dedup hint
//     "published_at_ms":  1739462812345          // Date.now() at envelope build
//   }
//
// `published_at_ms` is wall clock on the gateway host; e2e latency
// is `Date.now() - published_at_ms` on the worker host, so NTP
// drift between the two is the noise floor (bounded by the fleet
// clock-skew SLO — tens of ms in practice on ECS Fargate). hrtime
// would be process-local and meaningless cross-process.
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
//
// TODO(infra-publisher-set): the consumer trusts every field in the
// envelope (including data.user.id and data.member.permissions)
// because the SQS queue policy locks sqs:SendMessage to the gateway
// task role's principal. If the infra-side `qurl-bot-events` module
// ever grants publish rights to a sibling service, this trust shape
// MUST be replaced with envelope schema validation + signature
// verification before that publisher's first message lands. See
// docs/zero-downtime-design.md → "Trust boundary — SQS queue policy
// + envelope authenticity" for the full contract; `git grep
// TODO(infra-publisher-set)` from any infra-side change surfaces
// this requirement.
//
// Backpressure: the poll loop tracks in-flight detached handlers
// and pauses receives when `inFlightPromises.size >= MAX_INFLIGHT_HANDLERS`
// (env-overridable; see QURL_BOT_MAX_INFLIGHT_HANDLERS). Without
// this, under a bursty arrival pattern with slow downstream
// DDB/REST work, handler promises would accumulate without bound,
// risking memory pressure if the backlog grows faster than handlers
// drain. The cap is the safety net before the worker tier flag-flip
// in PR 10.
//
// Two purposes: (1) the cap pauses receives when capacity is
// exhausted; (2) the set of references lets stop() await a
// deadline-capped drain of in-flight handlers before
// gracefulShutdown proceeds to db.close(). See the stop() comment
// block below for the drain contract — a SIGTERM during a detached
// DDB write still races db.close() if the handler exceeds the
// DRAIN_DEADLINE_MS (3s) cap, but typical workloads settle well
// under that and the race window is meaningfully reduced.
//
// This is a soft cap, not a hard semaphore. pollOnce checks
// `inFlightPromises.size >= cap` BEFORE issuing a ReceiveMessage,
// but a receive that passes the check can return up to
// RECEIVE_MAX_MESSAGES (10) messages, and each subsequent
// processMessage adds via trackDispatch. So in a sustained burst,
// the set can transiently reach `cap + RECEIVE_MAX_MESSAGES - 1`
// (≈9% overshoot at the default cap of 100) before the next pollOnce
// gates. The cap is a backpressure signal — "stop pulling new work"
// — not a precise upper bound; size memory headroom assuming
// `cap + RECEIVE_MAX_MESSAGES` simultaneous handler promises.
// (Strict ceiling is cap + RECEIVE_MAX_MESSAGES - 1; the .env.example
// rounds up by 1 for headroom math.)
//
// Error classification: pollLoop's catch routes errors into three
// buckets — abort (silent continue, stop()-triggered), permanent
// (fatal log + process.exit(1) so ECS surfaces the misconfig in
// deploy-time monitoring), and transient (log + POLL_ERROR_BACKOFF_MS
// backoff). Permanent codes are typo'd queue URL, missing IAM perm,
// region drift, and bad creds — anything a retry won't fix. See
// PERMANENT_AWS_ERROR_CODES for the full set.
//
// How tracking works without coupling to handler internals: the
// consumer sets `isWorkerDispatch = true` synchronously around the
// `client.actions.InteractionCreate.handle(data)` call. The listener
// in src/index.js calls `eventConsumer.trackDispatch(result)` on
// every dispatch; trackDispatch checks the flag and only registers
// the promise's settle handler when the dispatch is consumer-driven
// (worker tier), NOT when it's gateway-WS-driven. The increment
// runs inside the synchronous emit window; the decrement fires
// whenever the dispatch promise settles, however much later.

const { SQSClient, ReceiveMessageCommand, DeleteMessageCommand } = require('@aws-sdk/client-sqs');
const config = require('./config');
const logger = require('./logger');
const { GATEWAY_DISPATCH_TYPES, LOG_KINDS } = require('./constants');

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

// Time-bounded warn dedup for the missing-event_id observation.
// First time we observe a non-string/whitespace event_id, we warn
// (visible at default log levels) and start a 1h cooldown — every
// subsequent missing event_id within that window logs at debug,
// then the next observation outside the window re-arms the warn.
// This catches the case where a producer regression is fixed and
// then resurfaces hours later (a pure one-shot flag would miss it);
// and keeps signal:noise sane (the alert isn't repeated on every
// message during a sustained regression). Reset by
// _resetStateForTest; the timestamp is wall-clock and cheap to read.
let lastMissingEventIdWarnMs = 0;
const MISSING_EVENT_ID_WARN_COOLDOWN_MS = 60 * 60 * 1000;

function recordSeen(eventId) {
  // Be precise about the LRU's contract — the producer publishes
  // string event_ids (format `<shardId>:<sequence>`). A non-string
  // (number, object, missing) OR a whitespace-only string is
  // treated as "no id" so the LRU doesn't accidentally key on `0`,
  // `''`, `{}`, or `' '`. The whitespace-only case is the subtle
  // one: a producer regression that emits `event_id: ' '` would
  // make every message look like a dup under the same key and the
  // dup-rate gauge would falsely report 100%. The earlier
  // missing-event_id branch in processMessage logs separately.
  if (typeof eventId !== 'string' || eventId.trim() === '') return false;
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

// Worker state. start() constructs a fresh `stopController` and
// flips `running` true; stop() calls `stopController.abort()` and
// awaits loopPromise. pollOnce uses Promise.all over the per-message
// wrappers, which serializes "all messages in this batch were
// dispatched (emit + DeleteMessage attempted)" against loopPromise
// resolution.
//
// stopController drives three shutdown paths from a single signal:
//   1. SDK long-poll cancellation — the receive's abortSignal option
//      reads stopController.signal directly, so a SIGTERM landing
//      mid-receive rejects with AbortError within tens of ms instead
//      of waiting the full RECEIVE_WAIT_SECONDS (20s). Critical for
//      fitting inside gracefulShutdown's 10s force-exit budget.
//   2. abortableSleep wake-up — backpressure pauses and error
//      backoffs register an 'abort' listener on stopController.signal
//      and resolve on a microtask when stop() fires.
//   3. pollLoop terminator — the while-loop reads signal.aborted.
// Multiple listeners on one AbortSignal is a standard pattern.
//
// What stop() awaits: the poll loop (loopPromise) AND a
// deadline-capped drain of detached handlers tracked by
// trackDispatch. After loopPromise resolves, every processMessage's
// emit window has completed and trackDispatch holds references to
// every handler promise the worker dispatched. stop() then awaits
// `Promise.allSettled([...inFlightPromises])` racing a
// DRAIN_DEADLINE_MS (3s) timer — handlers that don't settle by
// then race db.close() the same way they did pre-#391, but typical
// workloads (DDB writes, REST round-trips) settle well under 3s,
// so the drain reduces the race window meaningfully. The
// gracefulShutdown 10s budget in index.js still bounds the total;
// 3s for drain leaves room for db.close() and Discord teardown.
//
// What stop() still does NOT guarantee: handlers exceeding the 3s
// drain deadline. A slow DM-fan-out batch that runs past the
// deadline races db.close() the same way the pre-PR gateway path
// always has (since the gateway also emits + runs detached). The
// drain is a *probability improvement*, not a correctness change.
let running = false;
let loopPromise = null;
let stopController = null;
let sqsClient = null;
// onFatal callback supplied at start() — invoked when pollLoop hits a
// permanent AWS error. Default (null) crashes via process.exit(1);
// index.js wires `() => gracefulShutdown(1)` so cleanup runs first.
let onFatalCb = null;

// Backpressure cap (see module header). pollOnce early-returns
// when inFlightCount >= MAX_INFLIGHT_HANDLERS and waits
// INFLIGHT_BACKOFF_MS for the count to drain before re-polling.
// Default of 100 sized for the realistic invite-only-beta worst
// case (10-message batches × ~5-10 batches before slowest
// handler completes). Operator can override via env when prod
// volume + slowness data is in hand.
//
// Captured at module load via config.QURL_BOT_MAX_INFLIGHT_HANDLERS
// (which routes through config.intEnv for shared rejection +
// fallback semantics — strict integer, must be positive). Per-poll
// re-read would let a typo in the env-update path silently change
// behavior without an audit trail in the deploy log.
const MAX_INFLIGHT_HANDLERS = config.QURL_BOT_MAX_INFLIGHT_HANDLERS;

// Defense-in-depth: values below RECEIVE_MAX_MESSAGES produce
// degenerate overshoot ratios (cap=1 can transiently reach 10
// in-flight, 10x the configured value). Accept the value — an
// operator who wants cap=1 to rate-limit hard may have a reason —
// but warn so the booted ceiling matches their mental model.
//
// Extracted as a function so the warning emission is unit-testable
// without re-requiring the whole module under a doMock'd config.
// Called at module-load below with the boot-time cap; tests can
// call it directly with any cap value via `_test.validateInflightCap`.
//
// Intentional console.warn (not logger.warn) for boot-time
// visibility before any logger transports attach to a structured
// sink. A misconfigured env-var should be obvious on container
// stdout regardless of LOG_LEVEL or transport state.
function validateInflightCap(cap) {
  if (cap < RECEIVE_MAX_MESSAGES) {
    console.warn(
      `[event-consumer] QURL_BOT_MAX_INFLIGHT_HANDLERS=${cap} is below ` +
      `RECEIVE_MAX_MESSAGES (${RECEIVE_MAX_MESSAGES}); effective in-flight ceiling ` +
      `is ${cap + RECEIVE_MAX_MESSAGES - 1} due to batch overshoot. ` +
      `Pick ${RECEIVE_MAX_MESSAGES}+ for the overshoot ratio to match the cap value.`,
    );
  }
}
validateInflightCap(MAX_INFLIGHT_HANDLERS);

// At-cap pause backoff. Exponential backoff (doubling) from BASE to
// MAX while the cap stays full; resets to BASE on the first below-cap
// poll. The wake *rate* decays — the sleep duration grows.
// A short cap streak (handlers draining within a second) keeps the
// 100ms responsiveness; a sustained streak (slow downstream + bursty
// arrivals) drops the wake rate to ~0.6/s instead of 10/s, cutting
// CPU + log noise without significantly delaying recovery.
// abortableSleep honors the abort signal so a SIGTERM during a
// backpressure pause still exits inside the graceful-shutdown budget.
const INFLIGHT_BACKOFF_BASE_MS = 100;
const INFLIGHT_BACKOFF_MAX_MS = 1600;
let currentBackoffMs = INFLIGHT_BACKOFF_BASE_MS;

// Maximum time stop() waits for in-flight handler promises to
// settle before proceeding past `await loopPromise`. Stays well
// under gracefulShutdown's 10s budget so db.close() has room to run
// even when the deadline fires. Handlers that haven't settled by
// then race db.close() the same way they did pre-#391, but the
// drain reduces the race window meaningfully for typical workloads.
//
// Resolved via config.QURL_BOT_DRAIN_DEADLINE_MS (which routes
// through config.intEnv's strict-integer + range checks; see the
// config-side comment for the [100, 8000] envelope).
//
// Mutable (not const) so tests can shrink the deadline to keep the
// suite fast. INITIAL captures the boot-time value so
// `_resetStateForTest` restores the config-resolved baseline rather
// than a hardcoded 3000 — important when CI runs with an env-override.
// Production code never mutates DRAIN_DEADLINE_MS — only
// `_setDrainDeadlineForTest` does.
const INITIAL_DRAIN_DEADLINE_MS = config.QURL_BOT_DRAIN_DEADLINE_MS;
let DRAIN_DEADLINE_MS = INITIAL_DRAIN_DEADLINE_MS;

// In-flight handler promises (added in trackDispatch when a
// consumer-driven dispatch is registered; deleted when that
// promise settles). isWorkerDispatch is the per-emit gate that
// distinguishes consumer-driven dispatches (which we register)
// from gateway-WS-driven dispatches in combined mode (which we
// don't — the gateway tier doesn't backpressure against itself).
// The flag is set true synchronously around
// `client.actions.InteractionCreate.handle(data)` in processMessage
// and cleared in the finally; the listener's emit fires inside that
// window because EventEmitter.emit is synchronous.
//
// Set rather than a bare counter: `.size` gives O(1) cap checks AND
// the live set of references lets stop() await drain via
// `Promise.allSettled([...inFlightPromises])`. Set.delete on a
// non-member is a no-op, so stale .finally callbacks from
// `_resetStateForTest` or hot-reload can't drift the size — the
// underflow guard the previous counter shape needed is gone.
const inFlightPromises = new Set();
let isWorkerDispatch = false;

// State-machine rate-limit on the at-cap log. The *transition* into
// at-cap is the loud signal (warn-on-entry), the transition back to
// below-cap is the info-on-release. Steady-state at-cap is silent —
// pairing entry+exit gives the duration of the pause without
// per-iteration noise. atCapPauseLogged is the "we've logged the
// warn for this streak" gate; cleared when pollOnce next observes a
// below-cap state. Log messages live in constants so monitoring
// alerts that grep for them stay in sync with the call site if the
// wording drifts.
let atCapPauseLogged = false;
const AT_CAP_PAUSE_WARN_MSG = 'Event consumer: in-flight cap reached, pausing receive until handlers drain';
const AT_CAP_RELEASED_INFO_MSG = 'Event consumer: in-flight cap released, resuming receive';

// Reset the paired at-cap state — backoff value + warn gate. The two
// always move together: a non-base `currentBackoffMs` implies an
// in-progress streak (the at-cap branch is the only writer that grows
// it), which implies `atCapPauseLogged === true`. Three call sites
// reset both (pollOnce below-cap transition, stop()'s finally,
// _resetStateForTest); the helper keeps them in lockstep so a future
// fourth site can't silently break the invariant by forgetting one.
function clearAtCapState() {
  atCapPauseLogged = false;
  currentBackoffMs = INFLIGHT_BACKOFF_BASE_MS;
}

// Hook for unit tests: lets the test inject a mock SQSClient
// (aws-sdk-client-mock'd in tests/event-consumer.test.js) without
// constructing a real client at module load. Pure DI — production
// start() constructs the real client itself.
function _setSqsClientForTest(client) {
  sqsClient = client;
}

function createSqsClient() {
  // Note: AWS credential resolution (IAM task role, env vars,
  // credential-chain) happens lazily on first `.send()`, NOT at
  // SQSClient construction. A missing/invalid task role will
  // surface as a ReceiveMessageCommand failure inside pollLoop —
  // landing in the error-backoff path rather than as a boot
  // failure. Treated as acceptable because (a) the bot's task
  // execution role is required for the rest of the AWS surface
  // (DDB flow-state, etc.) and would fail those too, and (b) ops
  // monitoring on the pollLoop error log catches the case. If we
  // ever need explicit boot-time credential resolution, the
  // pattern is `await sqsClient.config.credentials()` inside
  // start() and propagate any throw.
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
// SQS Standard caps message bodies at 256 KB on the wire (bytes,
// not chars). We cap ours conservatively at ~200 KB to leave SQS
// attribute-header headroom. Anything over this is rejected before
// JSON.parse — cheap defense if the publisher set ever loosens
// (today the trust boundary is IAM-locked, see the
// TODO(infra-publisher-set) marker in the module header). Also the
// natural place a future envelope-signature check would land.
//
// Measured via Buffer.byteLength(_, 'utf8') rather than
// `String.length` because String.length is a UTF-16 code-unit
// count — a multi-byte payload would silently slip under a
// char-based cap. SQS's wire-format cap is in bytes; ours must be
// too.
const MAX_BODY_BYTES = 200 * 1024;

async function processMessage(client, message) {
  if (message.Body) {
    const bodyBytes = Buffer.byteLength(message.Body, 'utf8');
    if (bodyBytes > MAX_BODY_BYTES) {
      logger.error('Event consumer: message body exceeds size cap, deleting', {
        messageId: message.MessageId,
        bodyBytes,
        cap: MAX_BODY_BYTES,
      });
      await deleteMessage(message.ReceiptHandle);
      return;
    }
  }
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

  const { eventType, data, event_id: eventId, published_at_ms: publishedAtMs } = parsed;

  if (eventType !== GATEWAY_DISPATCH_TYPES.INTERACTION_CREATE) {
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

  if (typeof eventId !== 'string' || eventId.trim() === '') {
    // Telemetry blind spot: recordSeen short-circuits on a
    // non-string or whitespace-only event_id, so dup-detection
    // won't see this message. WARN-rate-limited to once per hour
    // so a sustained producer regression doesn't flood logs while
    // a transient-and-resurfaced regression still triggers a fresh
    // alert. Subsequent observations within the cooldown window
    // are debug-only.
    const now = Date.now();
    if (now - lastMissingEventIdWarnMs >= MISSING_EVENT_ID_WARN_COOLDOWN_MS) {
      lastMissingEventIdWarnMs = now;
      logger.warn('Event consumer: envelope missing valid event_id; LRU dedup will not track (warn re-armed every 1h; subsequent observations log at debug)', {
        messageId: message.MessageId,
        eventType,
        eventIdType: typeof eventId,
      });
    } else {
      logger.debug('Event consumer: envelope missing valid event_id; LRU dedup will not track', {
        messageId: message.MessageId,
        eventType,
        eventIdType: typeof eventId,
      });
    }
  }
  const isDup = recordSeen(eventId);

  // End-to-end latency: gateway-host wall clock at envelope build
  // → worker-host wall clock at receive. Cross-host comparison, so
  // NTP drift between gateway and worker is the noise floor (bounded
  // by the fleet's ECS Fargate clock-skew SLO, ~tens of ms). The
  // structured `qurl_bot_event_e2e_ms` field is the metric handle —
  // log-based metrics / CloudWatch alarms pivot on it without
  // grepping the message text. Skip when the producer didn't supply
  // `published_at_ms` (older envelopes, malformed publisher) — a
  // missing field is debug, not warn, because the e2e signal is
  // observability, not correctness.
  if (typeof publishedAtMs === 'number' && Number.isFinite(publishedAtMs)) {
    const e2eMs = Date.now() - publishedAtMs;
    // Log fires AT RECEIVE TIME, not after dispatch — see the
    // `client.actions.InteractionCreate.handle(data)` call further
    // down (around the FLAG-WRAP-START sentinel). The metric is
    // gateway-host wall-clock-publish → worker-host wall-clock-receive,
    // i.e., a receive-latency SLI. Don't rename to 'dispatched' /
    // 'handled' — those framings imply the handler ran, which we
    // can't yet attest from this code path.
    logger.info('Event consumer: received', {
      qurl_bot_event_e2e_ms: e2eMs,
      eventId,
      shardId: parsed.shardId,
    });
  } else {
    logger.debug('Event consumer: envelope missing published_at_ms; e2e latency not measured', {
      eventId,
      publishedAtMsType: typeof publishedAtMs,
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
  let dispatchOk = true;
  // Flag for trackDispatch (called by the listener in src/index.js).
  // EventEmitter.emit is synchronous, so the listener fires while
  // this flag is still true; trackDispatch reads the flag and only
  // registers consumer-driven dispatches in the backpressure cap.
  // Cleared in the finally so a gateway-WS-driven dispatch landing
  // after this returns is correctly untracked.
  //
  // CRITICAL INVARIANT — DO NOT add `await` inside this try/finally.
  // The flag-wrap is correct only because there is no yield between
  // `isWorkerDispatch = true` and `isWorkerDispatch = false`. A
  // pre-emit `await logger.x()` or an `await` on handle() would let
  // a concurrent processMessage (under Promise.allSettled in
  // pollOnce) observe a leaked true flag, silently miscounting
  // gateway-WS-driven dispatches against the worker cap in combined
  // mode. If you need async work, do it BEFORE this block or AFTER
  // the finally clears the flag.
  //
  // The FLAG-WRAP-START / FLAG-WRAP-END sentinels below bracket the
  // block; the static invariant test in tests/event-consumer.test.js
  // slices between them and asserts no `await` appears. Do NOT rename
  // or relocate the sentinels without updating the test.
  // FLAG-WRAP-START
  isWorkerDispatch = true;
  try {
    client.actions.InteractionCreate.handle(data);
  } catch (err) {
    dispatchOk = false;
    // Reconstruction failure (e.g., data shape we don't know how to
    // handle). Log but still delete — a poison message that throws
    // every time would otherwise loop until DLQ; logging surfaces
    // the producer bug.
    logger.error('Event consumer: dispatch reconstruction failed', {
      error: err.message,
      eventId,
      messageId: message.MessageId,
    });
  } finally {
    isWorkerDispatch = false;
  }
  // FLAG-WRAP-END

  if (isDup) {
    // Debug log only — at-least-once delivery means a "dup" might
    // actually be the first real attempt that an earlier worker
    // received but didn't ACK, so we still process. Correctness is
    // owned by Discord's interaction-token uniqueness and OCC at
    // the flow-state layer. `dispatchOk` lets ops disambiguate
    // "this was actually delivered twice" from "this is a poison-
    // pill we tried to dispatch on a prior receive and threw" —
    // both show up as dups in the LRU because recordSeen runs
    // before the dispatch try/catch. If ops wants an aggregatable
    // dup-rate metric, add a `qurl_bot_event_dup_total{shardId,
    // dispatchOk}` counter alongside this log (reservation slot
    // for the observability Phase 1.0 follow-on PR).
    logger.debug('Event consumer: event_id seen recently', {
      eventId,
      shardId: parsed.shardId,
      dispatchOk,
    });
  }

  await deleteMessage(message.ReceiptHandle);
}

// Called by the `interactionCreate` listener in src/index.js for
// every dispatch. Reads the isWorkerDispatch flag set synchronously
// around the consumer's `handle()` call to distinguish consumer-
// driven dispatches (which count against the backpressure cap)
// from gateway-WS-driven dispatches (which don't — gateway tier
// doesn't backpressure against itself). Increment runs inside the
// synchronous emit window; decrement registers on the dispatch
// promise's `.finally()` so both success and rejection bring the
// counter back down. A non-promise return (older handlers, or
// nothing returned for unrouted interaction types) is a no-op.
function trackDispatch(maybePromise) {
  if (!isWorkerDispatch) return;
  if (!maybePromise || typeof maybePromise.then !== 'function') return;
  inFlightPromises.add(maybePromise);
  // `.finally` runs before `.catch` on rejection, so the delete
  // always lands first — the .catch below sees the rejection AFTER
  // the promise has been removed from the set.
  //
  // Set.delete on a non-member is a no-op, so stale .finally
  // callbacks (after _resetStateForTest clears the set, or a
  // hot-reload swaps modules) silently no-op rather than drifting
  // the size negative. No underflow guard needed.
  maybePromise.finally(() => {
    inFlightPromises.delete(maybePromise);
  }).catch((err) => {
    // Attaching `.finally()` above counts as a handler for Node's
    // unhandled-rejection bookkeeping, so a rejection that pre-PR
    // would have surfaced at `process.on('unhandledRejection', ...)`
    // in src/index.js is now absorbed here. Log it explicitly so a
    // failing handler still produces an error signal — otherwise the
    // backpressure tracker would silently mask handler bugs.
    //
    // `kind: 'unhandledRejection'` tags the log so a single CloudWatch
    // query (filtering on the structured field) can find both this
    // consumer-driven path AND the gateway-WS-driven path through
    // `process.on('unhandledRejection', ...)` in src/index.js, which
    // already emits the same field. The two call sites agree on the
    // tag today; an alarm rule pivoting on `kind=unhandledRejection`
    // covers both tiers without per-tier rules. The
    // `err?.message || String(err)` fallback mirrors the gateway
    // handler's shape for non-Error rejections.
    logger.error('Event consumer: dispatch handler rejected', {
      kind: LOG_KINDS.UNHANDLED_REJECTION,
      error: err?.message || String(err),
      stack: err?.stack,
    });
  });
}

async function pollOnce(client) {
  // Backpressure check: if too many handlers are in-flight, pause
  // receives until they drain. Returns from this iteration without
  // pulling messages; pollLoop will call us again, and the
  // `while (!stopController.signal.aborted)` check naturally exits
  // if stop() lands during the pause. abortableSleep also wakes on
  // signal abort so the pause doesn't blow the graceful-shutdown
  // budget.
  //
  // Log throttling: only the transition into at-cap is loud (warn);
  // steady-state at-cap is silent. The transition back to below-cap
  // (handled after this branch) logs at info. This matches the
  // `lastMissingEventIdWarnMs` pattern already in this file —
  // transition-loud, steady-state-quiet.
  // Snapshot capPayload at pollOnce entry. Shared across the at-cap
  // entry-warn AND the release-info branch — so the release log's
  // `inFlight` field is the value that *triggered* the release
  // (necessarily below cap), not "drained to N" at the moment of
  // logging. That's the right framing for pairing pause-start
  // (entry-snapshot at cap) with pause-end (entry-snapshot below
  // cap) in CloudWatch.
  const capPayload = { inFlight: inFlightPromises.size, cap: MAX_INFLIGHT_HANDLERS };
  // `>=` (not `>`): the cap is the first paused value, so cap=100
  // means we pause AT 100 in-flight, not at 101. Steady-state ceiling
  // is therefore (cap - 1) + RECEIVE_MAX_MESSAGES overshoot. Off-by-
  // one is intentional and matches the .env.example math.
  if (inFlightPromises.size >= MAX_INFLIGHT_HANDLERS) {
    if (!atCapPauseLogged) {
      logger.warn(AT_CAP_PAUSE_WARN_MSG, capPayload);
      atCapPauseLogged = true;
    }
    // Steady-state at-cap is intentionally silent — the entry warn
    // + the exit info already bookend the pause for operators.
    await abortableSleep(currentBackoffMs);
    // Exponential backoff: each consecutive at-cap iteration doubles
    // the wait up to MAX, so a sustained streak drops the wake rate
    // from 10/s to ~0.6/s at the ceiling without significantly
    // delaying recovery. (The first 5 iterations average ~1.6/s
    // before settling at the 1.6s ceiling — see the cadence table
    // in PR #407 for the per-iteration breakdown.)
    // Trade-off: a release that lands mid-sleep delays the next
    // below-cap observation (and the paired pause-end log) by up to
    // MAX (1.6s) worst-case, ~MAX/2 (~800ms) average since releases
    // land uniformly within the sleep window. Operators pairing
    // pause-start/pause-end durations in CloudWatch see this as
    // inflated dwell time at the ceiling.
    //
    // No jitter: the downstream wedge isn't sensitive to wake-time
    // alignment, so even if multiple worker processes hit the cap
    // at the same wall-clock moment (shared-downstream wedge) and
    // their doubling ladders align, plain doubling is still the
    // right shape — there's no thundering-herd surface to spread.
    currentBackoffMs = Math.min(currentBackoffMs * 2, INFLIGHT_BACKOFF_MAX_MS);
    return;
  }
  // Below-cap path: if we just transitioned out of at-cap, log the
  // release so operators can match "pause started" with "pause ended"
  // pairs in CloudWatch. Reset both the warn gate and the backoff so
  // the next at-cap entry re-fires the warn AND starts at the base
  // wait (each streak gets exactly one warn and starts responsive).
  if (atCapPauseLogged) {
    logger.info(AT_CAP_RELEASED_INFO_MSG, capPayload);
    clearAtCapState();
  }
  const queueUrl = config.QURL_BOT_EVENTS_QUEUE_URL;
  const cmd = new ReceiveMessageCommand({
    QueueUrl: queueUrl,
    MaxNumberOfMessages: RECEIVE_MAX_MESSAGES,
    WaitTimeSeconds: RECEIVE_WAIT_SECONDS,
    VisibilityTimeout: RECEIVE_VISIBILITY_SECONDS,
  });
  // Share stopController's signal directly with the AWS SDK. stop()
  // calls abort() once on the lifetime controller, which both
  // cancels the in-flight long-poll AND wakes any in-flight
  // abortableSleep — no per-iteration controller, no separate flag
  // polling. The AWS SDK rejects send() with AbortError when the
  // signal fires; pollLoop's catch handles it (cleared as a
  // non-fatal interruption) and the `signal.aborted` check exits
  // the loop.
  const { Messages = [] } = await sqsClient.send(cmd, {
    abortSignal: stopController.signal,
  });
  if (Messages.length === 0) return;

  // Process messages in parallel. Each wrapper catches its own
  // failures so one rejection doesn't collapse the batch and strand
  // siblings. The only escape from `processMessage` is a
  // DeleteMessage throw — every other error path is internally
  // caught + delete-eager. So this log is specifically the
  // SQS-delete-failed signal: the message will redrive after the
  // visibility timeout and re-dispatch.
  //
  // Operational interpretation depends on the upstream code path:
  // - INTERACTION_CREATE that dispatched successfully → OCC at
  //   flow-state + Discord token-uniqueness guard the post-handler
  //   side effects on redelivery. Counts here = "how often do we
  //   redrive a successfully-dispatched interaction." Worth
  //   alerting on if it climbs.
  // - Poison-pill paths (malformed body, unknown eventType,
  //   non-object envelope) → no post-handler side effects to
  //   guard; the message just keeps redelivering until
  //   maxReceiveCount fires and SQS moves it to the DLQ. Normal
  //   redrive policy is the bound.
  //
  // `Promise.allSettled` (rather than `Promise.all`) makes the
  // per-message isolation a hard contract: even if a future change
  // adds work outside the inner try/catch and lets a rejection
  // escape, allSettled still awaits the remaining promises before
  // returning. With Promise.all, an uncaught rejection would
  // collapse the batch and orphan in-flight receipts.
  await Promise.allSettled(Messages.map(async (message) => {
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
  // Walk err.cause until we hit an abort shape, a non-object, or a
  // node we've already visited. Visited-set (vs a depth counter)
  // catches both cyclic chains AND non-cyclic chains deeper than
  // an arbitrary cap — future @aws-sdk releases that wrap aborts
  // in more than a few cause layers would still match. Bounded
  // memory because the chain length is bounded by distinct error
  // instances, which is small in practice.
  const visited = new Set();
  let current = err;
  while (current && typeof current === 'object' && !visited.has(current)) {
    visited.add(current);
    if (current.name === 'AbortError'
      || current.name === 'CanceledError'
      || current.code === 'AbortError'
      || current.code === 'ABORT_ERR') return true; // Node's standard AbortError shape (DOMException-style code).
    current = current.cause;
  }
  return false;
}

// AWS error codes that indicate permanent misconfiguration rather than
// transient flakiness. Retrying these forever just spams logs without
// resolving the underlying problem — better to crash so ECS surfaces
// the misconfig as a high task-restart rate in deploy-time monitoring.
// The boot-time `missingEventShipperKeys` check catches the *missing*
// queue URL case; this catches the *misconfigured-but-set* URL,
// missing IAM perms, region mismatch, and bad-credential cases.
//
// Names include both AWS-API-layer codes (v2-style, still present in
// error.Code on some SDK paths) and SDK-v3 error class names
// (error.name). Walking err.cause matches the isAbortError pattern
// for parity with how @aws-sdk wraps errors across versions.
//
// Intentionally NOT classified as permanent (kept in the transient
// path):
//   - ExpiredToken / ExpiredTokenException — IRSA/ECS task-role STS
//     creds rotate every ~6h; a brief gap during refresh self-heals.
//   - SignatureDoesNotMatch — usually clock skew, which also self-heals
//     once chrony catches up.
// Re-classify if prod data shows these recur past a fail-fast threshold;
// today they belong with throttling. AccessDenied is in the permanent
// set on the bet that misconfig (typo'd queue ARN, missing IAM grant)
// dominates over deploy-time IAM eventual-consistency gaps. If prod
// telemetry ever shows AccessDenied spikes correlated with deploys
// rather than config changes, demote it back to transient.
const PERMANENT_AWS_ERROR_CODES = new Set([
  // Queue doesn't exist (typo'd URL or wrong region in queue URL).
  'QueueDoesNotExist',
  'AWS.SimpleQueueService.NonExistentQueue',
  // IAM lacks sqs:ReceiveMessage on the queue.
  'AccessDenied',
  'AccessDeniedException',
  // Malformed queue URL.
  'InvalidAddress',
  'InvalidQueueUrl',
  // Bad endpoint (typically AWS_REGION drift between deploy + queue).
  'UnknownEndpoint',
  // Bad/expired credentials.
  'UnrecognizedClientException',
  'InvalidClientTokenId',
]);

// Returns the matched permanent-error code from anywhere in err's
// cause chain, or null if none. Any matching key at any layer wins —
// if an error has both .name = 'TimeoutError' (transient) and
// .code = 'QueueDoesNotExist' (permanent), this returns
// 'QueueDoesNotExist'. The conservative cut is "if any layer says
// permanent, treat as permanent" — under SDK wrapping a real
// misconfig can surface anywhere in the chain.
function permanentAwsErrorCode(err) {
  const visited = new Set();
  let current = err;
  while (current && typeof current === 'object' && !visited.has(current)) {
    visited.add(current);
    if (PERMANENT_AWS_ERROR_CODES.has(current.name)) return current.name;
    if (PERMANENT_AWS_ERROR_CODES.has(current.code)) return current.code;
    if (PERMANENT_AWS_ERROR_CODES.has(current.Code)) return current.Code;
    current = current.cause;
  }
  return null;
}

// Sleep that resolves either when the timeout fires OR when
// stopController.signal fires. Without the early-out, a stop() call
// during the error-backoff would block for the full
// POLL_ERROR_BACKOFF_MS before the while-check exits — on top of the
// 10s graceful-shutdown budget, the abort-the-receive savings would
// be partly undone.
//
// Event-driven (no polling tick): stopController.signal's 'abort'
// event fires synchronously on stop()'s abort() call, so an
// in-flight shutdown wakes the sleep within a microtask rather than
// the 50ms polling interval the previous version used. The setTimeout is
// cleared on the abort path so it doesn't fire spuriously after
// resolution.
//
// Defensive: if start() hasn't run (stopController is null) the
// sleep degrades to a pure setTimeout — abortableSleep is also
// reachable from the at-cap backpressure pause, and we don't want
// a missing controller to throw and crash the loop.
function abortableSleep(ms) {
  return new Promise((resolve) => {
    // Capture once so addEventListener and removeEventListener target
    // the same controller by identity, even if module-level
    // stopController is swapped during the sleep.
    const ctrl = stopController;
    // Already-aborted fast path: a stop() that landed before this
    // sleep started returns immediately.
    if (ctrl?.signal.aborted) {
      resolve();
      return;
    }
    let onAbort;
    const t = setTimeout(() => {
      if (ctrl && onAbort) {
        ctrl.signal.removeEventListener('abort', onAbort);
      }
      resolve();
    }, ms);
    // .unref()ed so a stray timer can't pin the event loop if the
    // loop exits via a different path mid-sleep.
    t.unref?.();
    if (ctrl) {
      onAbort = () => {
        clearTimeout(t);
        resolve();
      };
      // Timeout-wins branch (above) removes the listener explicitly;
      // { once: true } here is defensive — AbortSignal fires 'abort'
      // at most once per spec. If timeout and abort fire near-
      // simultaneously, both resolve() calls may run; the second is
      // a Promise no-op (resolve is idempotent).
      ctrl.signal.addEventListener('abort', onAbort, { once: true });
    }
  });
}

// Dispatch the fatal-shutdown route. If onFatalCb is wired, call it
// (sync throws AND async rejections both fall back to process.exit(1)).
// If not, exit directly. Does NOT await — the caller's defensive
// return must stay synchronous so pollLoop exits before the next
// iteration (under test mocks an await would let it iterate).
function invokeOnFatal() {
  if (!onFatalCb) {
    process.exit(1);
    return;
  }
  try {
    Promise.resolve(onFatalCb()).catch((cbErr) => {
      logger.error('Event consumer: onFatal rejected, falling back to process.exit', {
        error: cbErr?.message,
      });
      process.exit(1);
    });
  } catch (cbErr) {
    logger.error('Event consumer: onFatal threw, falling back to process.exit', {
      error: cbErr?.message,
    });
    process.exit(1);
  }
}

async function pollLoop(client) {
  // Null-guard symmetric with abortableSleep + stop(). Paranoia, not
  // a real path: start() always assigns stopController before invoking
  // pollLoop, and stop() nulls it only after `await loopPromise`
  // resolves (by which point pollLoop has already exited).
  while (stopController && !stopController.signal.aborted) {
    try {
      await pollOnce(client);
    } catch (err) {
      if (isAbortError(err)) {
        // stop() aborted the in-flight receive. The while-loop's
        // signal.aborted check exits next iteration; no logging or
        // backoff (this is a clean interruption, not a failure).
        continue;
      }
      const permanentCode = permanentAwsErrorCode(err);
      if (permanentCode) {
        // Permanent AWS misconfig (typo'd queue URL, missing IAM
        // perm, region drift, bad creds). Surface to the caller so
        // ECS sees a high task-restart rate in deploy-time monitoring
        // — retrying would spam logs and bleed the misconfig into
        // runtime noise. The boot check catches the missing-URL
        // case; this catches the misconfigured-but-set ones.
        //
        // Routing: if start() received an onFatal callback (index.js
        // wires `() => gracefulShutdown(1)`), call it so in-flight
        // handlers, the WAL checkpoint, and the WebSocket session
        // get torn down before exit. Otherwise fall back to direct
        // process.exit(1) — keeps the standalone-test contract and
        // is the safer default if a future caller forgets to wire
        // onFatal.
        logger.error('Event consumer: permanent AWS failure, exiting', {
          error: err.message, code: permanentCode,
        });
        invokeOnFatal();
        // Defensive return: stops the loop from iterating again into
        // the same failure under test mocks AND during the async
        // gap before gracefulShutdown's drain awaits this loop.
        return;
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
 * @param {object} [opts]
 * @param {() => void | Promise<void>} [opts.onFatal] - Invoked when
 *   pollLoop hits a permanent AWS error. index.js wires
 *   `() => gracefulShutdown(1)` so in-flight handlers, WAL checkpoints,
 *   and WebSocket sessions get torn down before exit. May return a
 *   Promise — both sync throws and async rejections fall back to
 *   `process.exit(1)`. Default (no callback) crashes directly via
 *   `process.exit(1)`, preserving the standalone-test contract.
 */
function start(client, { onFatal } = {}) {
  if (running) {
    // Double-start no-op preserves the existing onFatalCb (the
    // assignment below is unreachable on this path), so the callback
    // wired by the first start() stays in effect. Intentional — a
    // racing caller shouldn't be able to silently swap the fatal
    // route by calling start() again.
    logger.warn('Event consumer: start() called while already running — no-op');
    return;
  }
  // Pre-flight the internal-API surface we depend on. If a future
  // discord.js update relocates `client.actions.InteractionCreate.handle`
  // (or a future caller passes a stub without it), every dispatch
  // would silently throw inside the try/catch in processMessage,
  // drain the queue, and do nothing — a class of failure that's
  // hard to catch in monitoring because everything "works" except
  // the actual side effects. Turn this into a loud boot failure.
  if (typeof client?.actions?.InteractionCreate?.handle !== 'function') {
    throw new Error('Event consumer: client.actions.InteractionCreate.handle is not a function — discord.js internal-API drift?');
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
  stopController = new AbortController();
  onFatalCb = typeof onFatal === 'function' ? onFatal : null;
  logger.info('Event consumer: starting', {
    queueUrl: config.QURL_BOT_EVENTS_QUEUE_URL,
    waitSeconds: RECEIVE_WAIT_SECONDS,
    visibilitySeconds: RECEIVE_VISIBILITY_SECONDS,
  });
  // Localized catch on the loop promise so a crash inside pollLoop
  // (e.g., logger.error itself throwing, an unforeseen sync throw
  // outside the try/catch) doesn't propagate as an unhandled
  // rejection. pollLoop's own try/catch should catch everything
  // routine; this is defense-in-depth against the truly unexpected.
  // The error is logged here so the failure mode is local to the
  // consumer's module rather than relying on the global
  // unhandledRejection handler in index.js.
  loopPromise = pollLoop(client).catch((err) => {
    logger.error('Event consumer: poll loop crashed', { error: err?.message });
  });
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
  // Idempotent guards. The first caller through passes both checks;
  // the second caller (e.g., SIGTERM racing an uncaughtException, both
  // routing through gracefulShutdown) hits either !running (if it
  // arrives after the first cleared running below) or signal.aborted
  // (if it arrives after abort() fired) and short-circuits. The OR is
  // belt-and-suspenders against ordering across the await below.
  if (!running || stopController.signal.aborted) return;
  // Single abort() call wakes:
  //   1. The in-flight ReceiveMessage long-poll (SDK consumes the
  //      same signal and rejects send() with AbortError) — stop()
  //      returns within tens of ms instead of waiting up to
  //      RECEIVE_WAIT_SECONDS (20s).
  //   2. Any in-flight abortableSleep — backpressure pauses + error
  //      backoffs return on a microtask rather than burning the
  //      remainder of their interval.
  //   3. The pollLoop's `while (!signal.aborted)` terminator —
  //      after the current iteration unwinds, the next check exits.
  // Critical for graceful-shutdown: index.js's 10s force-exit fires
  // before a synchronously-waiting receive could complete on its own.
  stopController.abort();
  logger.info('Event consumer: stopping');
  try {
    // Isolate the loopPromise wait from the drain: if pollLoop
    // rejects (rare — its own catch covers routine errors, so this
    // is an unforeseen sync throw or similar), we still want to
    // drain. trackDispatch's set is maintained synchronously around
    // emits, so its state after a loop crash still accurately
    // reflects in-flight handler promises — draining is meaningful
    // even if the loop itself died.
    try {
      await loopPromise;
    } catch (err) {
      logger.error('Event consumer: error during stop', { error: err.message });
    }

    // After loopPromise settles (success or failure), every
    // processMessage's synchronous-emit window has completed, so
    // trackDispatch has captured all in-flight handler promises
    // the worker dispatched. Wait up to DRAIN_DEADLINE_MS for those
    // detached handlers (DDB writes, REST calls, DM fan-out) to
    // settle so the subsequent db.close() in gracefulShutdown
    // doesn't race them. The deadline is non-negotiable — index.js's
    // 10s force-exit fires regardless, so we cap our wait at 3s to
    // leave room for db.close() and any other shutdown work.
    //
    // Spread is cosmetic — allSettled also accepts a Set directly.
    if (inFlightPromises.size > 0) {
      const drainCount = inFlightPromises.size;
      // hrtime.bigint() is monotonic — Date.now() is wall-clock and
      // can jump under NTP adjustments mid-drain. Cost is the same;
      // monotonic is the correct shape for elapsed measurement.
      const drainStartNs = process.hrtime.bigint();
      logger.info('Event consumer: draining in-flight handlers', {
        count: drainCount,
        deadlineMs: DRAIN_DEADLINE_MS,
      });
      let drainTimer;
      try {
        await Promise.race([
          Promise.allSettled([...inFlightPromises]),
          new Promise((resolve) => {
            drainTimer = setTimeout(resolve, DRAIN_DEADLINE_MS);
          }),
        ]);
      } finally {
        clearTimeout(drainTimer);
      }
      const elapsedMs = Number((process.hrtime.bigint() - drainStartNs) / 1_000_000n);
      // The post-race size check is the discriminator (not the race
      // winner): when allSettled wins, each promise's trackDispatch-
      // registered `.finally` runs before allSettled's continuation,
      // so by the time we read `.size` here the set is already empty
      // on the happy path. On the deadline path the set still holds
      // whatever didn't settle.
      if (inFlightPromises.size > 0) {
        // Deadline elapsed before all handlers settled. Log at warn —
        // the same race the pre-#391 path always had (handlers racing
        // db.close), now bounded. Operators paging on a flood of these
        // would tune DRAIN_DEADLINE_MS or chase the slow downstream.
        logger.warn('Event consumer: drain deadline elapsed, proceeding with handlers still in-flight', {
          unsettled: inFlightPromises.size,
          settled: drainCount - inFlightPromises.size,
          elapsedMs,
        });
      } else {
        logger.info('Event consumer: drain complete', {
          count: drainCount,
          elapsedMs,
        });
      }
    }
  } finally {
    running = false;
    loopPromise = null;
    // Null the controller so a caller introspecting state between
    // stop+start (or never restarting) sees the pre-start() shape.
    // start() constructs a fresh controller, so the happy path is
    // unaffected; clearing here also ensures _resetStateForTest's
    // null contract holds without a second branch.
    stopController = null;
    onFatalCb = null;
    // Reset the at-cap pair (gate + backoff) so a
    // `start → at-cap → stop → start` round-trip doesn't inherit a
    // stale `true`/ceiling and miss the next streak's transition warn
    // or start the next streak at MAX. Production never restarts
    // after stop, but integration tests that exercise the lifecycle
    // would otherwise see a phantom suppressed warn.
    clearAtCapState();
    // Intentionally do NOT null `sqsClient`. Production never calls
    // start() after stop() — gracefulShutdown runs once at SIGTERM,
    // then process.exit. Reusing the client across a hypothetical
    // future stop+start pair is cheaper than re-resolving region +
    // re-constructing the credential chain. If a future caller
    // mutates AWS_REGION between stop+start and expects the new
    // value to take effect, that caller MUST also call
    // _resetStateForTest (or null sqsClient explicitly) — this is
    // the asymmetry with the test helper, which DOES clear it.
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
  loopPromise = null;
  stopController = null;
  sqsClient = null;
  onFatalCb = null;
  seenEventIds.clear();
  lastMissingEventIdWarnMs = 0;
  inFlightPromises.clear();
  isWorkerDispatch = false;
  clearAtCapState();
  DRAIN_DEADLINE_MS = INITIAL_DRAIN_DEADLINE_MS;
}

module.exports = {
  start,
  stop,
  trackDispatch,
  _test: {
    _setSqsClientForTest,
    _resetStateForTest,
    recordSeen,
    // Live reference to the module-level Set — tests inspect .size,
    // .has(), .keys() directly. _resetStateForTest calls .clear()
    // on this same instance rather than reassigning, so the
    // captured reference stays valid across resets. If a future
    // cleanup ever swaps `.clear()` for `seenEventIds = new Set()`,
    // this export will need to become a getter to avoid the test
    // indirection silently breaking.
    seenEventIds,
    processMessage,
    pollOnce,
    isAbortError,
    permanentAwsErrorCode,
    abortableSleep,
    SEEN_EVENT_ID_CAP,
    MAX_INFLIGHT_HANDLERS,
    validateInflightCap,
    INFLIGHT_BACKOFF_BASE_MS,
    INFLIGHT_BACKOFF_MAX_MS,
    getCurrentBackoffMs: () => currentBackoffMs,
    // DRAIN_DEADLINE_MS is mutable (so tests can shrink it), so
    // expose as a getter — a snapshot-at-module-load export would
    // silently read 3000 even after _setDrainDeadlineForTest(50)
    // mutated the live value.
    getDrainDeadlineMs: () => DRAIN_DEADLINE_MS,
    AT_CAP_PAUSE_WARN_MSG,
    AT_CAP_RELEASED_INFO_MSG,
    // Live getter for the lifetime abort controller. Was previously
    // `getReceiveAbortController` (per-iteration); the AbortSignal
    // refactor consolidated to a single lifetime controller, so the
    // name now matches its actual scope.
    getStopController: () => stopController,
    // Test-only initializer for the stop controller. Tests that
    // exercise pollOnce directly (bypassing start()) need a
    // controller to exist so the SDK send() call can read its
    // signal. Always constructs fresh — to clear, use
    // _resetStateForTest. Production code only sets this via start().
    _setStopControllerForTest: () => { stopController = new AbortController(); },
    // Returns the in-flight count by reading `.size` on the live
    // Set. Kept under the historical name for test-site stability;
    // semantically still "how many handlers are tracked right now."
    getInFlightCount: () => inFlightPromises.size,
    isRunning: () => running,
    isWorkerDispatching: () => isWorkerDispatch,
    isAtCapPauseLogged: () => atCapPauseLogged,
    // Test-only setter. trackDispatch reads the module-level
    // `isWorkerDispatch` directly (not via `isWorkerDispatching()`),
    // so a jest.spyOn on the introspection helper can't influence it.
    // Tests that want to simulate the synchronous flag wrap done by
    // processMessage call this helper instead.
    _setWorkerDispatchingForTest: (v) => { isWorkerDispatch = !!v; },
    // Test-only setter for the drain deadline. Default 3000ms is
    // too slow for the suite to wait through on every deadline-elapsed
    // test; tests shrink it to ~50ms so the unhappy-path assertions
    // run quickly. `_resetStateForTest` restores the default between
    // tests so changes don't leak.
    _setDrainDeadlineForTest: (ms) => { DRAIN_DEADLINE_MS = ms; },
    // Test-only setter for `loopPromise`. Lets tests inject a
    // rejected promise to simulate a pollLoop crash without
    // actually crashing the loop. CAVEAT: if the test already
    // called start(), the REAL pollLoop is still running in the
    // background — overwriting the promise reference here doesn't
    // halt it. Tests relying on this helper should either call
    // stop() (which aborts the signal and retires the real loop)
    // within their assertions, or accept that `_resetStateForTest`
    // in beforeEach is the cleanup point. `_resetStateForTest` itself
    // sets loopPromise back to null.
    _setLoopPromiseForTest: (p) => { loopPromise = p; },
  },
};
