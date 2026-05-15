// SQS-driven event publisher for the gateway tier (zero-downtime
// design, Pillar 1). Wired as a `client.on('raw', publish)` listener
// in src/index.js; every Discord dispatch lands here. The handler
// filters to `INTERACTION_CREATE` (the only event type the worker
// tier handles today — see event-consumer.js's `unhandled
// eventType` branch) and forwards the raw payload to SQS as the
// canonical envelope, then the worker tier polls the queue and
// dispatches via `client.actions.InteractionCreate.handle(data)` —
// same dispatcher path as a gateway-WS-driven interaction.
//
// Pairs with src/event-consumer.js. The envelope contract is owned
// by event-consumer's module header; this module's job is to
// MIRROR it. Any field added on the consumer side (validation,
// signing, new event types) must be added here in lockstep or the
// consumer's deletion-on-missing-field path will silently drain
// every message.
//
// Process-singleton: every piece of state — `running`, the
// in-flight Promise Set, the SQS client — lives in module-level
// scope. There is at most one publisher per process by design.
// A future caller that tries to `start()` twice in one process
// hits the second-start warn and the duplicate state takes no
// effect. If multi-publisher is ever needed (parallel shards in
// the same process), promote the state to instance/closure shape.
//
// Process-role gating: start() is called only when `isGateway` AND
// `config.ENABLE_EVENT_SHIPPER` are both true (see index.js).
// Combined mode + flag-on is rejected at boot
// (unsupportedRoleShipperCombo in boot-requirements.js) — see that
// helper's comment for the double-dispatch hazard. In the supported
// split shape, this module runs only in the gateway tier; the
// worker tier never starts a publisher.
//
// Hot-path discipline: the `raw` event fires on every gateway
// dispatch (HEARTBEAT_ACK, PRESENCE_UPDATE, READY, GUILD_CREATE,
// etc. — high frequency). We filter to `op === 0` (dispatch) +
// `t === 'INTERACTION_CREATE'` BEFORE any allocation or log so
// the non-interaction path costs a single comparison. The handler
// MUST NOT await — EventEmitter listeners are awaited only by
// `client.emit`'s synchronous-loop, and a long-running listener
// here would block the WebSocket-frame poll loop in discord.js.
// SendMessage runs detached: `sqsClient.send(...).catch(logErr)`
// + add the promise to `inFlightSends` for stop() drain.
//
// Send-failure semantics: SendMessage's promise is fire-and-log.
// A transient SQS error means the interaction is LOST from the
// worker's POV — Discord won't retry the gateway dispatch (it's
// a one-shot push, not poll-based), and we can't ACK the
// interaction because the worker tier is the only thing reading
// the queue. The 3-second interaction-token TTL is the bound:
// even if we queued the failed payload locally and retried, the
// user would see "interaction failed" before our retry succeeds.
// So we accept the loss, log loudly (`kind: 'unhandledRejection'`
// matches the tag used by the consumer + global handler in
// index.js for unified CloudWatch alarming), and let SQS's own
// availability SLO bound the loss rate. If sustained SendMessage
// failure becomes a real failure mode, the right answer is
// double-publish-to-DLQ + alarm — not in-process retry.
//
// Wall-clock `published_at_ms`: the consumer computes e2e latency
// as `Date.now() - published_at_ms`. Both sides use wall clock
// (not `hrtime.bigint()`) because hrtime is process-local and
// meaningless cross-process. NTP drift between gateway and worker
// hosts is the noise floor; bounded by the fleet's clock-skew SLO
// (~tens of ms in practice on ECS Fargate).
//
// Envelope size cap: the producer does NOT enforce a local size
// limit on `JSON.stringify(envelope)`. SQS Standard caps message
// bodies at 256 KB on the wire; an over-cap envelope triggers a
// SendMessage rejection that lands in the same `kind:
// unhandledRejection` log path as any other failure (the
// interaction is lost). The consumer-side `MAX_BODY_BYTES = 200 KB`
// cap (event-consumer.js) is the defense-in-depth on the receive
// path; the producer-side cap is implicit because the trust
// boundary (IAM-locked publisher set) keeps the realistic upper
// bound on `packet.d` well under 256 KB for INTERACTION_CREATE
// payloads (a /qurl invocation with full member/guild/data is
// ~5-10 KB). If the worker tier ever consumes a higher-volume
// event class (MESSAGE_CREATE with attachments, etc.), an explicit
// producer-side cap with a structured log on rejection becomes
// load-bearing.
//
// Drain on stop(): stop() awaits in-flight SendMessage promises
// up to DRAIN_DEADLINE_MS before returning. After that, any
// unsettled sends race the process exit. The pattern mirrors
// event-consumer.js's stop()/drain shape — different state, same
// contract.

const { SQSClient, SendMessageCommand } = require('@aws-sdk/client-sqs');
const config = require('./config');
const logger = require('./logger');
const { GATEWAY_DISPATCH_TYPES, LOG_KINDS } = require('./constants');

// Shard index used in the envelope's `shardId` + `event_id` fields.
// Single-shard today, so a literal '0' is correct. PR 13 introduces
// `@discordjs/ws` (multi-shard capable) and will replace this with
// the per-shard value pulled from `WebSocketManager`'s dispatch
// callback. The string-typed shape lets the future `${k}:${n}`
// format land without changing the LRU's key contract on the
// consumer side.
//
// `event_id = ${SHARD_ID}:${packet.s}` mirrors the consumer's
// recordSeen() LRU key shape (see event-consumer.js module header).
// `packet.s` is Discord's per-dispatch sequence — monotonic within
// a shard, so for single-shard it's a globally-unique-per-session
// key. Re-IDENTIFY (post-reconnect) resets `s` to 1; the LRU
// tolerates the collision because (a) interactions from the prior
// session are already expired by Discord (3s TTL), and (b)
// flow-state OCC owns correctness.
//
// TODO(sharding-pr): replace the literal '0' with the per-shard
// value supplied by `@discordjs/ws`'s dispatch callback once PR 13
// migrates the gateway tier off discord.js. `git grep TODO(sharding-pr)`
// from that branch surfaces the call sites that need updating; the
// LRU key shape on the consumer side stays unchanged so this
// remains a producer-only change.
const SHARD_ID = '0';

// Hot-path filter constants. `op === 0` (GATEWAY_OP_DISPATCH) is a
// Discord-wire protocol literal — not part of constants.js because no
// other module checks gateway opcodes. The event-type literal is
// imported from constants.js so the publisher's filter and the
// consumer's eventType-validation always agree on the exact string.
const GATEWAY_OP_DISPATCH = 0;

// Module-level state. publish() reads `running` to skip the SQS
// path when start() hasn't been called (or stop() has fired);
// start()/stop() flip it. inFlightSends tracks unsettled
// SendMessage promises so stop() can drain.
let running = false;
let sqsClient = null;
const inFlightSends = new Set();

// Drain deadline mirrors event-consumer.js. Resolved via
// config.QURL_BOT_DRAIN_DEADLINE_MS — same env var, same range,
// same default — so the gateway and worker tiers share one knob
// per fleet. Splitting would let one side cannibalize the other's
// gracefulShutdown headroom.
//
// Mutable for tests; INITIAL captures the boot-time value so
// `_resetStateForTest` restores the config-resolved baseline rather
// than a hardcoded 3000. Production code never mutates this — only
// `_setDrainDeadlineForTest` does.
const INITIAL_DRAIN_DEADLINE_MS = config.QURL_BOT_DRAIN_DEADLINE_MS;
let DRAIN_DEADLINE_MS = INITIAL_DRAIN_DEADLINE_MS;

function _setSqsClientForTest(client) {
  sqsClient = client;
}

function createSqsClient() {
  // Lazy AWS_REGION read mirrors event-consumer.js — see that file's
  // createSqsClient comment for the rationale (defensive against
  // shim bootstrappers that set AWS_REGION after module load; also
  // makes a missing region land in the publish error path rather
  // than as a sync boot throw inside start()).
  const region = (process.env.AWS_REGION ?? '').trim();
  if (!region) {
    throw new Error('AWS_REGION is required to use the event publisher. Set it in the deployment template (e.g. `us-east-2`).');
  }
  return new SQSClient({ region });
}

/**
 * Handle a raw gateway dispatch. Wired as the `raw` listener on the
 * Discord client in src/index.js. Filters to INTERACTION_CREATE and
 * forwards as an SQS envelope; everything else is a no-op (single
 * `op` / `t` comparison, no allocation).
 *
 * Synchronous return: the SendMessage promise runs detached. The
 * EventEmitter does not await listeners; this matches that contract
 * explicitly so a future reader doesn't add an `await` that would
 * stall the WebSocket frame poll.
 *
 * @param {object} packet - The raw gateway packet (discord.js
 *   `raw` event payload). Shape: `{ op, t, s, d }`.
 */
function publish(packet) {
  // Defense against a falsy packet (a future discord.js refactor or
  // test stub passing nothing) — gated once before the filter so
  // every subsequent access can use plain dot-notation. Cheaper
  // than `packet?.` on each access for the per-frame hot path.
  if (!packet) return;
  // Filter-first: the raw event fires on every dispatch (heartbeat
  // acks, presence updates, etc.). Bail before any work on the
  // non-interaction path. Order: `op` first because the comparison
  // is an integer; `t` second because string-eq is slower.
  if (packet.op !== GATEWAY_OP_DISPATCH || packet.t !== GATEWAY_DISPATCH_TYPES.INTERACTION_CREATE) {
    return;
  }
  // Drop dispatches that arrive before start() (race window during
  // boot: the `raw` listener is registered at module-top, but
  // start() runs inside index.js's start(); a dispatch that
  // sneaked in before start() would have no sqsClient to call).
  // Logged at debug so a real boot bug surfaces but a one-shot
  // race doesn't flood. `running` is also flipped false by stop()
  // so dispatches that arrive during graceful shutdown drop here.
  if (!running) {
    logger.debug('Event publisher: dispatch received before start() / after stop(), dropping', {
      eventType: packet.t,
    });
    return;
  }
  // Envelope shape mirrors the consumer's contract (see
  // event-consumer.js module header). `data` is the raw `d` field
  // from the gateway frame, untouched — discord.js's
  // `client.actions.InteractionCreate.handle(data)` on the worker
  // side instantiates the right Interaction subclass from it.
  //
  // `event_id` = `${SHARD_ID}:${packet.s}` matches recordSeen()'s
  // LRU key shape. `published_at_ms` is read once at envelope-build
  // time (NOT at SendMessage-submit time) so retry latency or
  // SQS-side queuing doesn't inflate the e2e number — the metric
  // measures "time from gateway-frame-arrival to worker-dispatch".
  const envelope = {
    eventType: packet.t,
    shardId: SHARD_ID,
    data: packet.d,
    event_id: `${SHARD_ID}:${packet.s}`,
    published_at_ms: Date.now(),
  };
  // Wrap envelope-building + send-dispatch in try/catch so a
  // synchronous throw (rare with @aws-sdk/client-sqs v3 but possible
  // on malformed input, an invalid QueueUrl, or a future SDK
  // tightening) routes through the SAME `kind: 'unhandledRejection'`
  // log tag the async-rejection path uses. Without this wrap, a sync
  // throw would propagate out of the EventEmitter listener and
  // surface via discord.js's `client.on('error')` channel under a
  // different shape — breaking the unified CloudWatch query that
  // pivots on the structured `kind` field.
  let sendPromise;
  try {
    const cmd = new SendMessageCommand({
      QueueUrl: config.QURL_BOT_EVENTS_QUEUE_URL,
      MessageBody: JSON.stringify(envelope),
    });
    sendPromise = sqsClient.send(cmd);
  } catch (err) {
    logger.error('Event publisher: SendMessage threw synchronously (interaction lost)', {
      kind: LOG_KINDS.UNHANDLED_REJECTION,
      error: err?.message || String(err),
      stack: err?.stack,
      eventId: envelope.event_id,
    });
    return;
  }
  // Detached send. Adding `.finally` to the original send promise
  // counts as a handler for Node's unhandled-rejection bookkeeping,
  // so the `.catch` below is required to actually log the error
  // (otherwise rejection would be silently absorbed by .finally).
  // `kind: 'unhandledRejection'` matches the consumer's
  // trackDispatch tag + index.js's global handler tag — one
  // CloudWatch query covers all three sites.
  inFlightSends.add(sendPromise);
  sendPromise.finally(() => {
    inFlightSends.delete(sendPromise);
  }).catch((err) => {
    logger.error('Event publisher: SendMessage failed (interaction lost)', {
      kind: LOG_KINDS.UNHANDLED_REJECTION,
      error: err?.message || String(err),
      stack: err?.stack,
      eventId: envelope.event_id,
    });
  });
}

/**
 * Initialize the publisher. Idempotent: a second call while already
 * running is a no-op (warn-logged so an accidental double-start is
 * visible). Must be called BEFORE `client.login()` so the `raw`
 * listener registered at module-top in index.js has a live sqsClient
 * by the time the first frame arrives.
 */
function start() {
  if (running) {
    logger.warn('Event publisher: start() called while already running — no-op');
    return;
  }
  if (!config.ENABLE_EVENT_SHIPPER) {
    // Defense in depth — caller in index.js already gates on
    // (isGateway && config.ENABLE_EVENT_SHIPPER). Explicit here so a
    // future caller without the gate doesn't accidentally bring up
    // a publisher pointing at an unset queue URL.
    throw new Error('Event publisher: start() called with ENABLE_EVENT_SHIPPER=false');
  }
  if (!config.QURL_BOT_EVENTS_QUEUE_URL) {
    throw new Error('Event publisher: QURL_BOT_EVENTS_QUEUE_URL is not set');
  }
  if (!sqsClient) {
    sqsClient = createSqsClient();
  }
  running = true;
  logger.info('Event publisher: starting', {
    queueUrl: config.QURL_BOT_EVENTS_QUEUE_URL,
    shardId: SHARD_ID,
  });
}

/**
 * Stop accepting new publishes and drain in-flight SendMessage
 * promises up to DRAIN_DEADLINE_MS. Idempotent on not-running.
 * Awaits the drain; safe to call from gracefulShutdown.
 *
 * Behavior matches event-consumer.stop()'s drain contract:
 * Promise.allSettled racing a deadline timer, structured logging
 * on each terminal outcome (complete / deadline-elapsed / no-op-idle).
 */
async function stop() {
  if (!running) return;
  running = false;
  logger.info('Event publisher: stopping');
  if (inFlightSends.size === 0) {
    logger.info('Event publisher: stop complete (no in-flight sends to drain)');
    return;
  }
  const drainCount = inFlightSends.size;
  // hrtime.bigint() is monotonic — Date.now() is wall-clock and
  // can jump under NTP adjustments mid-drain. The published_at_ms
  // field elsewhere in this module uses wall clock because it
  // needs cross-process comparability, but elapsed-on-this-host is
  // purely local and should be monotonic.
  const drainStartNs = process.hrtime.bigint();
  logger.info('Event publisher: draining in-flight SendMessage promises', {
    count: drainCount,
    deadlineMs: DRAIN_DEADLINE_MS,
  });
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
  // Post-race size check is the discriminator (not the race winner):
  // each promise's `.finally` runs before allSettled's continuation,
  // so on the happy path the set is empty by the time we read it.
  // On the deadline path the set still holds whatever didn't settle.
  if (inFlightSends.size > 0) {
    logger.warn('Event publisher: drain deadline elapsed, proceeding with sends still in-flight (interactions may be lost)', {
      unsettled: inFlightSends.size,
      settled: drainCount - inFlightSends.size,
      elapsedMs,
    });
  } else {
    logger.info('Event publisher: drain complete', {
      count: drainCount,
      elapsedMs,
    });
  }
}

// Reset every piece of module-level state to its post-construction
// shape. Tests call this in beforeEach to avoid cross-contamination
// (the publisher is a process-singleton — see module header).
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
    SHARD_ID,
    getInFlightCount: () => inFlightSends.size,
    isRunning: () => running,
    // Mutable (so tests can shrink the deadline) — getter-export to
    // avoid the snapshot-at-module-load footgun the consumer ran
    // into in PR #395.
    getDrainDeadlineMs: () => DRAIN_DEADLINE_MS,
    _setDrainDeadlineForTest: (ms) => { DRAIN_DEADLINE_MS = ms; },
  },
};
