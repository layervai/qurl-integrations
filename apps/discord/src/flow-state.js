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
function assertExpiresAt(expiresAt) {
  if (typeof expiresAt !== 'number' || !Number.isFinite(expiresAt) || Math.floor(expiresAt) !== expiresAt) {
    throw new TypeError(`flow-state: expires_at must be a finite integer epoch-seconds Number (got ${typeof expiresAt}: ${JSON.stringify(expiresAt)}). DDB TTL only reaps Number-typed attributes; a string or float would silently orphan the row.`);
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
  if (payload == null) return null;
  return encryptStrict(JSON.stringify(payload));
}

function decryptPayload(ciphertext) {
  if (ciphertext == null) return null;
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
async function createFlow({ flow_id, stage, payload, expires_at }) {
  if (typeof flow_id !== 'string' || flow_id.length === 0) {
    throw new TypeError('flow-state.createFlow: flow_id must be a non-empty string');
  }
  if (typeof stage !== 'string' || stage.length === 0) {
    throw new TypeError('flow-state.createFlow: stage must be a non-empty string');
  }
  assertExpiresAt(expires_at);

  const item = {
    flow_id,
    stage,
    version: 1,
    payload: encryptPayload(payload),
    expires_at,
    created_at: nowEpochSeconds(),
    updated_at: nowEpochSeconds(),
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
async function loadFlow(flow_id, { grace_seconds = 0 } = {}) {
  if (typeof flow_id !== 'string' || flow_id.length === 0) {
    throw new TypeError('flow-state.loadFlow: flow_id must be a non-empty string');
  }
  const res = await ddb.send(new GetCommand({
    TableName: TABLE_NAME,
    Key: { flow_id },
    ConsistentRead: true,
  }));
  const item = res?.Item;
  if (!item) return null;
  const expires = item.expires_at;
  if (typeof expires === 'number' && Number.isFinite(expires)) {
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
// `terminal` is caller-supplied because flow-state cannot know
// which transitions end which flow types (the terminal set varies
// per flow — revoke has fewer stages than send). When `terminal`
// is true the next call should be deleteFlow.
async function transitionFlow(flow_id, expectedVersion, { stage_to, payload, terminal, extend_expires_at }) {
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
  if (extend_expires_at !== undefined) assertExpiresAt(extend_expires_at);

  // Read stage_from for the audit emission. The Update below is
  // the authoritative write — a concurrent transition between this
  // Get and the Update is detected by OCC and reported as conflict;
  // the stale stage_from in the audit emission is acceptable
  // (forensic field, not used for SLI math).
  let stage_from = null;
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
        terminal,
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
      terminal,
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
  if (extend_expires_at !== undefined) {
    updateExprParts.push('#e = :expires_at');
    exprNames['#e'] = 'expires_at';
    exprValues[':expires_at'] = extend_expires_at;
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
      } catch (_recheckErr) {
        // Recheck failure — stay conservative and report conflict
        // (the original Update failed due to a conditional check,
        // not an availability issue).
      }
      logger.audit(AUDIT_EVENTS.FLOW_TRANSITION, {
        flow_id,
        stage_from,
        stage_to,
        result,
        terminal,
      });
      return { result, version: null };
    }
    logger.error('flow-state.transitionFlow: update failed', { flow_id, reason: err && err.message });
    logger.audit(AUDIT_EVENTS.FLOW_TRANSITION, {
      flow_id,
      stage_from,
      stage_to,
      result: 'error',
      terminal,
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
