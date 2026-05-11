// flow-state — DDB-backed state-machine harness.
//
// Single-purpose: create / load / transition / delete a flow row in
// the `flow_state` DDB table provisioned by qurl-integrations-infra
// modules/qurl-bot-ddb (PR #504). Standalone module — NOT a Store
// contract method — because flow_state's lifecycle is tightly
// coupled to the SQS event-shipper architecture and shouldn't be
// reachable from code paths that legitimately depend on the Store
// abstraction (those run identically against SQLite for local dev;
// flow_state is DDB-only).
//
// Three correctness primitives:
//
//   1. OCC via `version` + `ConditionExpression: #v = :expected`.
//      A worker advancing a flow MUST pass the version it loaded;
//      a concurrent worker that already advanced the row will lose
//      the conditional check and the caller can decide to retry,
//      drop, or escalate. SQS at-least-once + the OCC gate gives
//      the system exactly-once-application semantics without a
//      FIFO queue.
//
//   2. TTL-driven cleanup. Every row has an `expires_at` Number
//      (epoch-seconds — DDB TTL is asynchronous and only reaps
//      Number-typed attributes; a string here would silently
//      orphan the row in the table forever). Asynchronous reap
//      delay can be up to ~48h, so the reader contract treats
//      `now > expires_at` as absent — see `loadFlow` below.
//
//   3. Mandatory app-layer payload encryption via `encryptStrict`.
//      The payload map transitively carries `attachment_url` and
//      recipient lists; both are unacceptable in plaintext-at-rest.
//      `encryptStrict` fails closed if `KEY_ENCRYPTION_KEY` is
//      unset, so a misconfigured deploy can't silently persist
//      plaintext.
//
// flow_id contract: must be a parseable shard-aware composite key
// as produced by `buildFlowId` in `src/flow-id.js`. `createFlow`
// validates structure at entry (parseFlowId returns non-null);
// transition/load/delete trust the caller — a malformed flow_id
// passed to those methods will simply hit not_found. Callers
// SHOULD route every flow_id through buildFlowId() rather than
// hand-rolling the join — drift on the separator convention
// between handler and worker is exactly the foot-gun flow-id.js
// exists to close.

const {
  DynamoDBClient,
} = require('@aws-sdk/client-dynamodb');
const {
  DynamoDBDocumentClient,
  GetCommand,
  PutCommand,
  DeleteCommand,
  UpdateCommand,
} = require('@aws-sdk/lib-dynamodb');

// `ConditionalCheckFailedException.name` is the stable identifier the
// AWS v3 SDK sets on the error — the exception class itself is in
// `@aws-sdk/client-dynamodb` and importing it here would couple this
// module to two SDK packages for one error-discrimination check.
// Matching on `.name` is what the rest of the codebase does
// (see `store/ddb-store.js`) and what the SDK's own docs recommend.
function isConditionalCheckFailed(err) {
  return err?.name === 'ConditionalCheckFailedException';
}

const { encryptStrict, decrypt } = require('./utils/crypto');
const logger = require('./logger');
const { AUDIT_EVENTS } = require('./constants');
const { parseFlowId } = require('./flow-id');

// Module-load validations. Same fail-fast pattern as ddb-store.js
// (DDB_TABLE_PREFIX + AWS_REGION). flow-state intentionally
// duplicates the DDB-client setup rather than importing it from
// ddb-store — flow-state is loaded on bot boot for ANY env that
// runs the event-shipper architecture (incl. envs that may not
// run STORE_TYPE=ddb), and tying it to ddb-store's module init
// would couple two independent code paths. A future PR can extract
// `src/store/ddb-client.js` once a third consumer appears.
// TODO(PR 12): consolidate DDB client init into a shared module
// when the gateway-tier RESUME table arrives — three consumers
// is the right break-even point for the extraction.
//
// Test authoring note: these throws fire at `require()` time, so a
// new test file importing flow-state MUST set DDB_TABLE_PREFIX and
// AWS_REGION on `process.env` BEFORE the require — see
// tests/flow-state.test.js for the canonical pattern. Forgetting
// gives a confusing module-load stack trace rather than a clean
// test-setup error.
const TABLE_PREFIX = (process.env.DDB_TABLE_PREFIX ?? '').trim();
if (!TABLE_PREFIX) {
  throw new Error('DDB_TABLE_PREFIX is required to use the flow-state harness. Set it in the deployment template (e.g. `qurl-bot-discord-sandbox-`).');
}
if (!TABLE_PREFIX.endsWith('-')) {
  throw new Error(`DDB_TABLE_PREFIX must end with '-' (got '${TABLE_PREFIX}'). flow-state concatenates it directly with 'flow-state'; a missing dash produces a malformed table name.`);
}
const AWS_REGION = (process.env.AWS_REGION ?? '').trim();
if (!AWS_REGION) {
  throw new Error('AWS_REGION is required to use the flow-state harness. Set it in the deployment template (e.g. `us-east-2`).');
}

const TABLE_NAME = `${TABLE_PREFIX}flow-state`;

const rawClient = new DynamoDBClient({
  region: AWS_REGION,
  ...(process.env.DDB_TEST_ENDPOINT ? { endpoint: process.env.DDB_TEST_ENDPOINT } : {}),
});
const ddb = DynamoDBDocumentClient.from(rawClient, {
  marshallOptions: { removeUndefinedValues: true },
});

// TTL writer contract enforcement. DDB silently keeps a row forever
// if `expires_at` is anything other than a Number — string-typed
// timestamps are the classic foot-gun (a `new Date().toISOString()`
// looks correct but is type "S" and never reaps). Hard-fail here
// rather than at GetItem-time when the orphan surfaces years later.
//
// We ALSO reject `expires_at <= now` — a forgotten `+ ttl_seconds`
// (a classic miscompute, e.g. `expiresAt = nowEpochSeconds()` with
// the addition forgotten) writes a row that's instantly expired:
//   1. passes type check, succeeds Put, fires FLOW_CREATED
//   2. every loadFlow returns null (now > expires_at)
//   3. caller can never advance to deleteFlow → SLI numerator drops
// ⇒ guaranteed contributor to `silently_dropped_flows`. Reject at
// write time instead so the bad call site is named in the throw.
function assertExpiresAt(expiresAt) {
  if (typeof expiresAt !== 'number' || !Number.isFinite(expiresAt) || Math.floor(expiresAt) !== expiresAt) {
    throw new TypeError(`flow-state: expires_at must be a finite integer epoch-seconds Number (got ${typeof expiresAt}: ${JSON.stringify(expiresAt)}). DDB TTL only reaps Number-typed attributes; a string or float would silently orphan the row.`);
  }
  if (expiresAt <= nowEpochSeconds()) {
    throw new RangeError(`flow-state: expires_at must be strictly in the future (got ${expiresAt}; now is ${nowEpochSeconds()}). A row whose TTL is in the past is born expired — it passes the Put, fires FLOW_CREATED, but loadFlow returns null forever after, silently inflating the SLI's silently_dropped numerator. Most often this is a forgotten '+ ttl_seconds' on the caller side.`);
  }
}

function nowEpochSeconds() {
  return Math.floor(Date.now() / 1000);
}

// Encrypt the payload object as a JSON string. Decrypt is the
// inverse. We encrypt the whole JSON blob rather than per-field
// because the payload shape is intentionally freeform — different
// flow types carry different keys, and a per-field allowlist would
// either churn with every flow added or be too permissive to be
// useful. Whole-blob encryption keeps the DDB attribute shape
// predictable (always a single `payload` string column) regardless
// of flow type.
function encryptPayload(payload) {
  // Load-bearing null guard: without it, payload=null would
  // encryptStrict(JSON.stringify(null)) = ciphertext-of-"null"
  // (the 4-byte string), storing a non-null DDB attribute that
  // decrypts to the string "null", which JSON.parse correctly
  // reverses but at the cost of a wasted encrypt call AND an
  // inconsistent DDB attribute shape (string sometimes, null
  // sometimes). Pass null through unchanged so the column is
  // either a real ciphertext or a DDB null — never both.
  if (payload == null) return null;
  return encryptStrict(JSON.stringify(payload));
}

function decryptPayload(ciphertext) {
  // No upfront null guard — `decrypt` passes null/undefined through
  // unchanged, and the `plaintext == null` check below catches it.
  // A single guard is clearer than two redundant ones.
  const plaintext = decrypt(ciphertext);
  if (plaintext == null) return null;
  try {
    return JSON.parse(plaintext);
  } catch (err) {
    // A row with a malformed JSON payload is unrecoverable for
    // anything except forensic inspection — emit and return null
    // rather than throwing so a worker handling the row can decide
    // to delete-and-move-on instead of crashing the consumer loop.
    logger.error('flow-state: payload JSON.parse failed; row likely corrupted', {
      reason: err && err.message,
    });
    return null;
  }
}

// Create a new flow row. Idempotent on `flow_id` via
// `attribute_not_exists` — a caller that re-emits the same
// createFlow due to SQS at-least-once redelivery will get
// `{ created: false }` and can short-circuit. Successful create
// returns `{ created: true, version: 1 }`.
//
// Why caller-supplied `expires_at` rather than computing it here:
// different flow types have different lifecycle bounds (a
// `/qurl setup` modal has a 15-minute window; a `/qurl send`
// confirmation can sit for hours). The caller knows; this module
// just persists.
//
// Payload semantics: `null` and `undefined` both persist as a null
// DDB attribute. This is symmetric with createFlow but ASYMMETRIC
// with `transitionFlow` — there, `payload: undefined` means "leave
// existing payload untouched" (no `#p = :payload` in the Update),
// while `payload: null` means "clear the existing payload". On
// create the row has no existing payload so the distinction is
// moot, but callers porting code from create-then-transition
// should be aware that the same value carries different meaning
// across the two methods.
async function createFlow({ flow_id, stage, payload, expires_at }) {
  if (typeof flow_id !== 'string' || flow_id.length === 0) {
    throw new TypeError('flow-state.createFlow: flow_id must be a non-empty string');
  }
  // Entry-point structure validation. Once a malformed flow_id is in
  // the table its later operations would silently succeed (transitions
  // and deletes only look up by the literal key), and the audit
  // forensic path would fail at `parseFlowId` time with no signal —
  // the silent-drop foot-gun the reviewer flagged as "aspirational
  // coupling". Reject malformed keys at the only place a new row
  // can enter the table.
  if (parseFlowId(flow_id) === null) {
    throw new TypeError(`flow-state.createFlow: flow_id ${JSON.stringify(flow_id)} is not a parseable shard-aware composite key. Use buildFlowId({ shard_id, guild_id, channel_id, user_id }) from src/flow-id.js to construct it.`);
  }
  if (typeof stage !== 'string' || stage.length === 0) {
    throw new TypeError('flow-state.createFlow: stage must be a non-empty string');
  }
  assertExpiresAt(expires_at);

  const now = nowEpochSeconds();
  const item = {
    flow_id,
    stage,
    version: 1,
    payload: encryptPayload(payload),
    expires_at,
    created_at: now,
    updated_at: now,
  };

  try {
    await ddb.send(new PutCommand({
      TableName: TABLE_NAME,
      Item: item,
      ConditionExpression: 'attribute_not_exists(flow_id)',
    }));
  } catch (err) {
    if (isConditionalCheckFailed(err)) {
      // Existing row — idempotent no-op. Don't emit FLOW_CREATED;
      // the SLI denominator counts new flows, not redeliveries.
      //
      // Note: a payload mutation in the redelivered call is DISCARDED
      // silently. The contract is "create-once for this flow_id"; if
      // the caller's payload genuinely needs to evolve, the right
      // primitive is transitionFlow (which gives an OCC handle on
      // mutation) rather than re-trying createFlow with a different
      // payload. We do not warn-log on payload divergence today —
      // there's no cheap way to compare encrypted payloads without
      // decrypting both, and the legitimate case (SQS exact-duplicate)
      // would generate spurious warns.
      return { created: false };
    }
    throw err;
  }

  logger.audit(AUDIT_EVENTS.FLOW_CREATED, {
    flow_id,
    stage,
  });
  return { created: true, version: 1 };
}

// Load a flow row by flow_id. Returns the row with the payload
// decrypted, or `null` if the row is absent OR has logically
// expired (now > expires_at). The DDB TTL reap is asynchronous —
// up to ~48h delay between `now > expires_at` and the physical
// row deletion — so a reader that returns the still-present row
// would silently violate the "flow timed out" contract. Filter
// at the reader.
//
// grace_seconds (default 0) lets callers tolerate a small clock-
// skew window if they're reading on the back of a write they
// just performed. Default 0 because the canonical use case is
// "is this flow still alive?" and the answer should be honest.
// Negative grace values are accepted (skews "expired" earlier);
// no use case yet but cheaper to leave permissive than to gate
// on a corner case a future caller might legitimately want.
//
// ConsistentRead: the read is strongly consistent. Canonical use
// case is "I just transitioned this flow and want to re-verify"
// or "I'm about to transition and want the current version" —
// both need strong consistency. Eventually-consistent reads
// would also double RCU efficiency, but the harness today has
// no orthogonal-read callers (admin tooling, audit forensics)
// to justify a parameter. Add `consistent_read: false` here if
// such a caller arrives.
async function loadFlow(flow_id, { grace_seconds = 0 } = {}) {
  if (typeof flow_id !== 'string' || flow_id.length === 0) {
    throw new TypeError('flow-state.loadFlow: flow_id must be a non-empty string');
  }
  // Type-check grace_seconds. Without this, `grace_seconds:
  // 'forever'` would evaluate `expires + 'forever'` → string
  // concat → NaN → `now > NaN` is false → expired-as-alive. A
  // caller passing untrusted config could surface stale flows.
  if (typeof grace_seconds !== 'number' || !Number.isFinite(grace_seconds)) {
    throw new TypeError(`flow-state.loadFlow: grace_seconds must be a finite number (got ${typeof grace_seconds}: ${JSON.stringify(grace_seconds)})`);
  }
  const res = await ddb.send(new GetCommand({
    TableName: TABLE_NAME,
    Key: { flow_id },
    ConsistentRead: true,
  }));
  const item = res?.Item;
  if (!item) return null;
  const expires = item.expires_at;
  // Reader strictness must MATCH the writer's assertExpiresAt() —
  // a row whose expires_at is a non-integer float would otherwise
  // round-trip undetected (writer rejects, but a regression or
  // manual console put could still produce one). Same set: finite
  // integer Number. Anything else falls through to the warn+null
  // branch below.
  if (typeof expires === 'number' && Number.isFinite(expires) && Math.floor(expires) === expires) {
    if (nowEpochSeconds() > expires + grace_seconds) {
      // Logically expired but not yet reaped. Treat as absent.
      return null;
    }
  } else {
    // expires_at is present but not a finite Number — could be a
    // corrupted row (manual console put, legacy writer regression,
    // future schema mismatch). Fail-safe: treat as expired rather
    // than returning a row that DDB TTL will never reap. Symmetric
    // to the assertExpiresAt() guard on the writer side.
    logger.warn('flow-state.loadFlow: row has missing or non-numeric expires_at; treating as expired', {
      flow_id,
      expires_at_type: typeof expires,
    });
    return null;
  }
  return {
    flow_id: item.flow_id,
    stage: item.stage,
    version: item.version,
    payload: decryptPayload(item.payload),
    expires_at: item.expires_at,
    created_at: item.created_at,
    updated_at: item.updated_at,
  };
}

// Advance a flow's stage under OCC. The caller passes the version
// it loaded; this method only succeeds if the row's current version
// still matches. Returns one of:
//   { result: 'success',    version: <new_version> }
//   { result: 'conflict',   version: null }   — OCC lost (concurrent advance)
//   { result: 'not_found',  version: null }   — row absent
//   { result: 'error',      version: null }   — unexpected DDB failure (rethrown after emit)
//
// **OCC gates on `version` only, not `stage`.** A caller passing
// `expectedVersion: 5, stage_to: 'B'` succeeds if the row's current
// version is 5, even if its current stage isn't what the caller
// expected. This module is a generic harness — state-machine
// semantics (which `stage_from` is a legal precursor to `stage_to`)
// are the caller's responsibility. Callers wanting strict stage
// transitions should assert `stage_from` against expectations
// after a success result, or pre-check before calling.
//
// `terminal` is caller-supplied because flow-state cannot know
// which transitions end which flow types (the terminal set varies
// per flow — revoke has fewer stages than send). When `terminal`
// is true the next call should be deleteFlow.
//
// **`terminal` is forced to `false` on non-success results.** If
// the transition didn't actually advance the row (not_found,
// conflict, error), then nothing terminal happened — emitting
// the caller's `terminal: true` would let a forensic query like
// `count_by(terminal=true)` over-count by including failed
// transitions. The SLI math is unaffected (it splits on `result`
// + `terminal`, never `terminal` alone), but the audit shape
// should be honest.
//
// `set_expires_at` (optional): replaces the row's expires_at to
// the given value. **Named "set", not "extend"** because the
// harness does NOT enforce monotonicity — a caller passing a
// value earlier than the row's current expires_at will shorten
// the lifetime, not extend it. Callers wanting monotonic-only
// extension should compare against `loadFlow().expires_at`
// first; the harness can't enforce it without an extra Read or
// a stricter ConditionExpression (which would complicate the
// not_found/conflict discrimination).
//
// **Audit-emit-on-rethrow contract.** On `result: 'error'` paths
// the FLOW_TRANSITION audit fires BEFORE the rethrow. A caller
// that retries a transient `NetworkingError` will produce N
// `result: 'error'` events for one logical attempt — this is
// intentional. The error count is interesting per-attempt (a
// retry storm should be visible in the metric), and the SLI math
// `silently_dropped = count(FLOW_CREATED) - count(FLOW_DELETED)`
// is unaffected because it doesn't read the `error` bucket.
async function transitionFlow(flow_id, expectedVersion, { stage_to, payload, terminal, set_expires_at }) {
  if (typeof flow_id !== 'string' || flow_id.length === 0) {
    throw new TypeError('flow-state.transitionFlow: flow_id must be a non-empty string');
  }
  if (typeof expectedVersion !== 'number' || !Number.isInteger(expectedVersion) || expectedVersion < 1) {
    throw new TypeError(`flow-state.transitionFlow: expectedVersion must be a positive integer (got ${expectedVersion})`);
  }
  if (typeof stage_to !== 'string' || stage_to.length === 0) {
    throw new TypeError('flow-state.transitionFlow: stage_to must be a non-empty string');
  }
  if (typeof terminal !== 'boolean') {
    throw new TypeError(`flow-state.transitionFlow: terminal must be a boolean (got ${typeof terminal})`);
  }
  if (set_expires_at !== undefined) assertExpiresAt(set_expires_at);

  // Read stage_from for the audit emission. The Update below is
  // the authoritative write — a concurrent transition between this
  // Get and the Update is detected by OCC and reported as conflict;
  // the stale stage_from in the audit emission is acceptable
  // (forensic field, not used for SLI math).
  let stage_from = null;
  const extended = set_expires_at !== undefined;
  try {
    const got = await ddb.send(new GetCommand({
      TableName: TABLE_NAME,
      Key: { flow_id },
      ProjectionExpression: 'stage',
      ConsistentRead: true,
    }));
    if (!got?.Item) {
      logger.audit(AUDIT_EVENTS.FLOW_TRANSITION, {
        flow_id,
        stage_from: null,
        stage_to,
        result: 'not_found',
        terminal: false,
        extended,
      });
      return { result: 'not_found', version: null };
    }
    stage_from = got.Item.stage ?? null;
  } catch (err) {
    logger.error('flow-state.transitionFlow: pre-read failed', { flow_id, reason: err && err.message });
    logger.audit(AUDIT_EVENTS.FLOW_TRANSITION, {
      flow_id,
      stage_from: null,
      stage_to,
      result: 'error',
      terminal: false,
      extended,
    });
    throw err;
  }

  const updateExprParts = ['#s = :stage_to', '#v = #v + :one', '#u = :updated_at'];
  const exprNames = {
    '#s': 'stage',
    '#v': 'version',
    '#u': 'updated_at',
  };
  const exprValues = {
    ':stage_to': stage_to,
    ':one': 1,
    ':updated_at': nowEpochSeconds(),
    ':expected': expectedVersion,
  };
  if (payload !== undefined) {
    updateExprParts.push('#p = :payload');
    exprNames['#p'] = 'payload';
    exprValues[':payload'] = encryptPayload(payload);
  }
  if (set_expires_at !== undefined) {
    updateExprParts.push('#e = :expires_at');
    exprNames['#e'] = 'expires_at';
    exprValues[':expires_at'] = set_expires_at;
  }

  try {
    const res = await ddb.send(new UpdateCommand({
      TableName: TABLE_NAME,
      Key: { flow_id },
      UpdateExpression: `SET ${updateExprParts.join(', ')}`,
      ConditionExpression: 'attribute_exists(flow_id) AND #v = :expected',
      ExpressionAttributeNames: exprNames,
      ExpressionAttributeValues: exprValues,
      ReturnValues: 'UPDATED_NEW',
    }));
    const newVersion = res?.Attributes?.version ?? expectedVersion + 1;
    logger.audit(AUDIT_EVENTS.FLOW_TRANSITION, {
      flow_id,
      stage_from,
      stage_to,
      result: 'success',
      terminal,
      extended,
    });
    return { result: 'success', version: newVersion };
  } catch (err) {
    if (isConditionalCheckFailed(err)) {
      // The OCC gate. Could be (a) row disappeared between pre-read
      // and Update (TTL reap or concurrent delete) → not_found, or
      // (b) version mismatch (concurrent transition) → conflict.
      // Distinguish with a second Get so the caller can pick the
      // right retry strategy.
      let result = 'conflict';
      try {
        const recheck = await ddb.send(new GetCommand({
          TableName: TABLE_NAME,
          Key: { flow_id },
          ProjectionExpression: 'flow_id',
          ConsistentRead: true,
        }));
        if (!recheck?.Item) result = 'not_found';
      } catch (recheckErr) {
        // Recheck failure — stay conservative and report conflict
        // (the original Update failed due to a conditional check,
        // not an availability issue). Warn so a DDB availability
        // blip masked by the conservative fallback still surfaces
        // in CloudWatch — silent fallback would hide a real signal.
        logger.warn('flow-state.transitionFlow: post-CCFE recheck failed; defaulting to result=conflict', {
          flow_id,
          reason: recheckErr && recheckErr.message,
        });
      }
      logger.audit(AUDIT_EVENTS.FLOW_TRANSITION, {
        flow_id,
        stage_from,
        stage_to,
        result,
        terminal: false,
        extended,
      });
      return { result, version: null };
    }
    logger.error('flow-state.transitionFlow: update failed', { flow_id, reason: err && err.message });
    logger.audit(AUDIT_EVENTS.FLOW_TRANSITION, {
      flow_id,
      stage_from,
      stage_to,
      result: 'error',
      terminal: false,
      extended,
    });
    throw err;
  }
}

// Explicit delete. Emits FLOW_DELETED (the SLI numerator) — TTL
// reap deliberately does NOT emit this event so the
// silently-dropped count surfaces in the difference. If a future
// PR adds a TTL-driven sweeper it MUST emit a distinct event
// (e.g. FLOW_REAPED) so the SLI math stays intact.
//
// `reason` is one of 'terminal' | 'abort' | 'admin_cleanup' per
// the audit event's payload contract. Caller-supplied because
// flow-state can't know whether a given delete is a clean
// completion vs. a user abort.
async function deleteFlow(flow_id, { stage, reason }) {
  if (typeof flow_id !== 'string' || flow_id.length === 0) {
    throw new TypeError('flow-state.deleteFlow: flow_id must be a non-empty string');
  }
  if (typeof stage !== 'string' || stage.length === 0) {
    throw new TypeError('flow-state.deleteFlow: stage must be a non-empty string');
  }
  if (reason !== 'terminal' && reason !== 'abort' && reason !== 'admin_cleanup') {
    throw new TypeError(`flow-state.deleteFlow: reason must be one of 'terminal'|'abort'|'admin_cleanup' (got ${JSON.stringify(reason)})`);
  }

  // Conditional Delete + emit-on-success. The SLI math
  //   silently_dropped = count(FLOW_CREATED) - count(FLOW_DELETED)
  // requires at-most-once emission per logical flow on BOTH sides.
  // CREATED is gated by `attribute_not_exists`; DELETED needs the
  // mirror — `attribute_exists` — otherwise an SQS redelivery of an
  // abort/admin_cleanup path (which have no version-checked
  // predecessor to gate on) would Delete-success-idempotently a
  // second time and double-emit FLOW_DELETED. Numerator inflates,
  // signal lost.
  //
  // Returns `{ deleted: bool }` so callers can gate user-visible
  // "flow completed" side effects on actual deletion (a false
  // return means someone else already deleted, or the TTL reaped
  // — either way, don't re-DM the user).
  try {
    await ddb.send(new DeleteCommand({
      TableName: TABLE_NAME,
      Key: { flow_id },
      ConditionExpression: 'attribute_exists(flow_id)',
    }));
  } catch (err) {
    if (isConditionalCheckFailed(err)) {
      // Row was already gone (redelivery, TTL reap, or concurrent
      // delete). No-op. Do NOT emit FLOW_DELETED — the SLI's
      // count(FLOW_DELETED) must stay at-most-once per logical flow.
      return { deleted: false };
    }
    throw err;
  }

  logger.audit(AUDIT_EVENTS.FLOW_DELETED, {
    flow_id,
    stage,
    reason,
  });
  return { deleted: true };
}

module.exports = {
  createFlow,
  loadFlow,
  transitionFlow,
  deleteFlow,
  // Exposed for tests + future modules that need the canonical
  // table name (e.g. an admin CLI). Not for production code paths
  // that should go through the four functions above.
  __TABLE_NAME: TABLE_NAME,
};
