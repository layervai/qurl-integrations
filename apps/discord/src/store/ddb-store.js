// store/ddb-store — DynamoDB backend
//
// Implements the Store contract (see `src/store/contract.js`) against
// the 11 DynamoDB tables provisioned by `modules/qurl-bot-ddb/`
// in the infra repo. Selected at boot when `STORE_TYPE=ddb`.
//
// Table naming: tables carry the env in their name
// (`qurl-bot-discord-<env>-<kebab-name>`). The env prefix is
// supplied via `DDB_TABLE_PREFIX` and is REQUIRED — there is no
// default. A previous version defaulted to `qurl-bot-discord-prod-`,
// which meant a developer running the bot locally with `STORE_TYPE=
// ddb` and any AWS creds in their shell would silently hit prod
// tables. Failing fast on unset prefix is a one-time onboarding
// inconvenience (set the env in the deployment template) traded for
// eliminating that footgun. The bot task role's IAM policy must
// allow data-plane verbs on the specific table ARNs + GSI ARNs —
// scoping happens infra-side, not here.
//
// App-layer encryption: sensitive fields
// (`guild_configs.qurl_api_key`, `qurl_send_configs.attachment_url`,
// `qurl_send_configs.interaction_token`) are envelope-encrypted via
// `utils/crypto.encrypt`. Ciphertext is stored as a regular `S` string.
// DDB server-side encryption (AWS-managed aws/dynamodb CMK,
// configured at the table level) is defense-in-
// depth — the primary encryption is the app-layer envelope.
//

const crypto = require('crypto');
const {
  DynamoDBClient,
} = require('@aws-sdk/client-dynamodb');
const {
  DynamoDBDocumentClient,
  GetCommand,
  PutCommand,
  DeleteCommand,
  UpdateCommand,
  QueryCommand,
  ScanCommand,
  BatchGetCommand,
  BatchWriteCommand,
  TransactWriteCommand,
} = require('@aws-sdk/lib-dynamodb');

const { encrypt, decrypt } = require('../utils/crypto');
const logger = require('../logger');
const {
  DM_STATUS,
  AUDIT_EVENTS,
  DDB_TRANSACTION_MAX_ACTIONS,
  ddbSendConfigGuardActionCount,
  ddbSendConfigGuardFitsTransaction,
} = require('../constants');

class SendConfigRevokedError extends Error {
  constructor(sendIds) {
    const ids = Array.isArray(sendIds) ? sendIds : [sendIds].filter(Boolean);
    const label = ids.length === 1 ? `send ${ids[0]}` : 'one or more sends';
    super(`recordQURLSendBatch: ${label} was revoked or no longer matched the sender before recipient rows were saved`);
    this.name = 'SendConfigRevokedError';
    this.code = 'SEND_CONFIG_REVOKED';
    this.sendId = ids.length === 1 ? ids[0] : undefined;
    this.sendIds = ids;
  }
}

function transactionGuardConditionFailed(err, guardCount) {
  // CancellationReasons are positional. recordQURLSendBatch builds guarded
  // transactions with all ConditionCheck items first, so only the first
  // guardCount reasons may represent revoked/deleted/sender-mismatch state.
  return err?.name === 'TransactionCanceledException'
    && Array.isArray(err.CancellationReasons)
    && err.CancellationReasons
      .slice(0, guardCount)
      .some(r => r?.Code === 'ConditionalCheckFailed');
}

function transactionConditionFailedSendIds(err, sendIds) {
  const failed = err.CancellationReasons
    .slice(0, sendIds.length)
    .flatMap((reason, index) => (reason?.Code === 'ConditionalCheckFailed' ? [sendIds[index]] : []));
  return failed;
}

function transactionWriteRetryable(err) {
  if (!err || typeof err !== 'object') return false;
  // The SDK retries top-level throttling, but transaction cancellations can
  // surface per-item throughput/conflict reasons as CancellationReasons. Keep
  // this narrow outer loop so those transaction-specific shapes get backoff
  // too, with the same ClientRequestToken reused for idempotency.
  const retryableNames = new Set([
    'ProvisionedThroughputExceededException',
    'RequestLimitExceeded',
    'ThrottlingException',
  ]);
  if (retryableNames.has(err.name)) return true;
  if (err.name !== 'TransactionCanceledException' || !Array.isArray(err.CancellationReasons)) {
    return false;
  }
  const retryableReasonCodes = new Set([
    'ProvisionedThroughputExceeded',
    'ThrottlingError',
    'TransactionConflict',
  ]);
  return err.CancellationReasons.some(r => retryableReasonCodes.has(r?.Code));
}

// Table name mapping. Map keys (SQL-era snake_case names) match the
// outputs.tf `table_names` shape in `modules/qurl-bot-ddb/` so
// consumers look up by the same semantic key.
//
// DDB_TABLE_PREFIX is required (no default). See header comment for
// the prod-footgun rationale. Whitespace-only values are treated as
// unset to defend against buggy container templating that emits
// `DDB_TABLE_PREFIX=` with no value — same shape as the STORE_TYPE
// normalization in `store/index.js`.
const TABLE_PREFIX = (process.env.DDB_TABLE_PREFIX ?? '').trim();
if (!TABLE_PREFIX) {
  throw new Error('DDB_TABLE_PREFIX is required when STORE_TYPE=ddb. Set it to the env-specific prefix (e.g. `qurl-bot-discord-sandbox-` for sandbox, `qurl-bot-discord-prod-` for prod) in the deployment template.');
}
// Trailing-dash check. The prefix is concatenated directly with each
// table's kebab-case suffix (`${TABLE_PREFIX}qurl-sends`), so a
// missing trailing dash silently produces malformed names like
// `qurl-bot-discord-prodqurl-sends` and the bot's first DDB call
// returns ResourceNotFoundException — clear at the first call but
// confusing in CloudWatch logs (looks like a permission or naming
// issue, not a config typo). Catch it at boot so the failure points
// directly at the env var.
if (!TABLE_PREFIX.endsWith('-')) {
  throw new Error(`DDB_TABLE_PREFIX must end with '-' (got '${TABLE_PREFIX}'). The prefix is concatenated with kebab-case table suffixes; a missing dash produces malformed names like '${TABLE_PREFIX}qurl-sends'. Add the trailing '-' in the deployment template.`);
}
const TABLES = Object.freeze({
  qurl_sends: `${TABLE_PREFIX}qurl-sends`,
  qurl_send_configs: `${TABLE_PREFIX}qurl-send-configs`,
  qurl_views: `${TABLE_PREFIX}qurl-views`,
  guild_configs: `${TABLE_PREFIX}guild-configs`,
});

// AWS_REGION is required (no fallback). Same shape of footgun the
// DDB_TABLE_PREFIX guard above closes: a developer with stray AWS
// creds + DDB_TABLE_PREFIX=qurl-bot-discord-prod- + unset
// AWS_REGION would silently land in whichever region the SDK
// defaults to (us-east-2 today via the prior `|| 'us-east-2'`
// fallback), which is presumably the prod region. Catch at boot.
// Whitespace-only treated as unset, mirroring the prefix guard.
const AWS_REGION = (process.env.AWS_REGION ?? '').trim();
if (!AWS_REGION) {
  throw new Error('AWS_REGION is required when STORE_TYPE=ddb. Set it to the env-specific region (e.g. `us-east-2` for sandbox, the matching prod region for prod) in the deployment template.');
}

// Allow tests + local-dev to inject a pre-configured endpoint via
// `process.env.DDB_TEST_ENDPOINT` (DDB-Local at http://localhost:8000,
// or aws-sdk-client-mock interception). Production path constructs
// the real client once at module load. The prod-only refusal lives
// in `config.js` (loaded first), so by the time this constructor
// runs in production, DDB_TEST_ENDPOINT is guaranteed unset.
const rawClient = new DynamoDBClient({
  region: AWS_REGION,
  ...(process.env.DDB_TEST_ENDPOINT ? { endpoint: process.env.DDB_TEST_ENDPOINT } : {}),
});
const ddb = DynamoDBDocumentClient.from(rawClient, {
  marshallOptions: {
    // Remove undefined values (default is to throw) so optional
    // fields the bot doesn't always populate flow through as
    // absent attributes rather than per-call errors.
    removeUndefinedValues: true,
    // Preserve `""` as a distinct value (default behavior is
    // fine — DDB v3 SDK handles this correctly). Noted for
    // clarity.
  },
});

// ── Helpers ──

const nowIso = () => new Date().toISOString();

// ── Pending OAuth state tokens ──

// Aggregate counters for the token-gated /metrics endpoint. Both are
// full-table Scans, which is why /health uses healthCheck() instead —
// see the note there. /metrics is rate-limited and bearer-gated, so the
// scan cost is bounded to deliberate operator scrapes.
async function getStats() {
  const [configuredGuilds, totalSends] = await Promise.all([
    countAll(TABLES.guild_configs),
    countAll(TABLES.qurl_sends),
  ]);
  return { configuredGuilds, totalSends };
}

// Paginated `Select: COUNT` scan. DDB Scan with Select=COUNT is
// STILL subject to the 1MB read cap — `res.Count` is the count for
// the current page only, with `LastEvaluatedKey` set when more
// pages exist. A naive single-call ScanCommand silently undercounts
// once the table size pushes past ~1MB worth of items, with no
// error signal. Accumulate Count across pages until LEK is empty.
async function countAll(TableName) {
  let total = 0;
  let ExclusiveStartKey;
  do {
    const res = await ddb.send(new ScanCommand({ TableName, Select: 'COUNT', ExclusiveStartKey }));
    total += res.Count || 0;
    ExclusiveStartKey = res.LastEvaluatedKey;
  } while (ExclusiveStartKey);
  return total;
}

// Paginated Query helper. Same shape rationale as scanAll/countAll
// — DDB Query response is capped at 1MB per page regardless of any
// requested Limit, with LastEvaluatedKey set when more pages exist.
// Callers iterating via `Items` directly silently truncate at the
// first page boundary. This helper accumulates all matching items
// across pages.
//
// Pass any QueryCommand input shape; ExclusiveStartKey is threaded
// for you. Don't hand it a Limit unless you also want the per-page
// cap (we use it on the GSI hot path in getRecentSends; the per-
// send recipient queries below intentionally don't set one).
async function queryAll(input) {
  const items = [];
  let ExclusiveStartKey;
  do {
    const res = await ddb.send(new QueryCommand({ ...input, ExclusiveStartKey }));
    items.push(...(res.Items || []));
    ExclusiveStartKey = res.LastEvaluatedKey;
  } while (ExclusiveStartKey);
  return items;
}

async function scanAll(TableName, { consistentRead = false } = {}) {
  const items = [];
  let ExclusiveStartKey;
  do {
    const res = await ddb.send(new ScanCommand({
      TableName,
      ExclusiveStartKey,
      // ConsistentRead doubles RCU cost; the priming/refresh path for
      // the webhook subscription registry uses it because a recent
      // setGuildWebhookSubscription on a sibling replica MUST be
      // visible to this replica's next scan within the
      // SIBLING_LAG_GRACE_MS window. All other scanAll callers use
      // the default eventually-consistent path.
      ConsistentRead: consistentRead || undefined,
    }));
    items.push(...(res.Items || []));
    ExclusiveStartKey = res.LastEvaluatedKey;
  } while (ExclusiveStartKey);
  return items;
}

// ── Badges ──

async function recordQURLSend({
  sendId, senderDiscordId, recipientDiscordId, resourceId, resourceType,
  qurlLink, qurlId, expiresIn, channelId, targetType, guildId,
}) {
  // qurl_id is the GSI hash key on qurl_id-index, used by the
  // qurl.expired webhook handler to locate the recipient row for DM
  // editing. Sparse GSI: omit the attribute when empty so the row stays
  // out of the index (qurl-service returns "" for resources that don't
  // surface a qurl_id, and indexing those would clutter the GSI without
  // a lookup path that could use them).
  const Item = {
    send_id: sendId,
    recipient_discord_id: recipientDiscordId,
    sender_discord_id: senderDiscordId,
    resource_id: resourceId,
    resource_type: resourceType,
    qurl_link: qurlLink,
    expires_in: expiresIn,
    channel_id: channelId,
    target_type: targetType,
    dm_status: 'pending',
    created_at: nowIso(),
  };
  if (typeof qurlId === 'string' && qurlId.length > 0) {
    Item.qurl_id = qurlId;
  }
  // guild_id is a NON-KEY, non-indexed attribute — DynamoDB is schemaless
  // for non-key attrs, so the qurl-bot-ddb table needs no Terraform change
  // to carry it. It scopes watermark attribution to the minting guild
  // (#1101); the /qurl detect read filters sends on it (a write-time
  // invariant + a defense-in-depth filter at read time). Sparse like
  // qurl_id: a legacy/non-guild send omits it rather than writing an empty
  // string. It rides the `qurl_id-index` GSI for free (projection = ALL).
  if (typeof guildId === 'string' && guildId.length > 0) {
    Item.guild_id = guildId;
  }
  await ddb.send(new PutCommand({
    TableName: TABLES.qurl_sends,
    Item,
  }));
}

function qurlSendBatchItem(s, createdAt) {
  // qurl_id is sparsely written so an empty/missing value (e.g.
  // a future emit path that doesn't surface it) doesn't pollute
  // the qurl_id-index GSI. See recordQURLSend for the same gate.
  const Item = {
    send_id: s.sendId,
    recipient_discord_id: s.recipientDiscordId,
    sender_discord_id: s.senderDiscordId,
    resource_id: s.resourceId,
    resource_type: s.resourceType,
    qurl_link: s.qurlLink,
    expires_in: s.expiresIn,
    channel_id: s.channelId,
    target_type: s.targetType,
    dm_status: 'pending',
    created_at: createdAt,
  };
  if (typeof s.qurlId === 'string' && s.qurlId.length > 0) {
    Item.qurl_id = s.qurlId;
  }
  // guild_id: non-key attribute scoping watermark attribution to
  // the minting guild (#1101). Sparse like qurl_id — a legacy /
  // non-guild send omits it. No Terraform change needed (schemaless
  // non-key attr); rides the qurl_id-index GSI for the /qurl detect
  // read (projection = ALL). See recordQURLSend for the full why.
  if (typeof s.guildId === 'string' && s.guildId.length > 0) {
    Item.guild_id = s.guildId;
  }
  return Item;
}

async function recordQURLSendBatch(sends, options = {}) {
  if (!sends || sends.length === 0) return;
  const MAX_RETRIES = 5;
  // Single `now` shared across every batch chunk / guarded transaction of this
  // batch. INTENTIONAL parity with SQLite's transactional insert —
  // every recipient of one send shares one `created_at`, and
  // downstream queries (getRecentSends groups by send_id and uses
  // created_at as the GSI sort key) rely on that. Don't "fix" this
  // to nowIso()-per-chunk: a slow batch under throttle/retry would
  // produce a 100-recipient send with a 2s spread of created_at
  // values, fragmenting the GSI ordering and making a single send
  // look like several distinct events to consumers.
  const now = nowIso();
  const requireSendConfigUnrevoked = options.requireSendConfigUnrevoked === true;

  if (!requireSendConfigUnrevoked) {
    // Default path for initial sends: keep the cheaper BatchWriteItem flow.
    // Add Recipients is the only caller with a persisted config row that can
    // be revoked between read and write, so it opts into the guarded
    // transaction path below.
    const CHUNK = 25;
    for (let i = 0; i < sends.length; i += CHUNK) {
      const slice = sends.slice(i, i + CHUNK);
      let requestItems = {
        [TABLES.qurl_sends]: slice.map(s => ({ PutRequest: { Item: qurlSendBatchItem(s, now) } })),
      };
      for (let attempt = 0; attempt <= MAX_RETRIES; attempt++) {
        const res = await ddb.send(new BatchWriteCommand({ RequestItems: requestItems }));
        const unprocessed = res.UnprocessedItems || {};
        const remaining = unprocessed[TABLES.qurl_sends];
        if (!remaining || remaining.length === 0) break;
        if (attempt === MAX_RETRIES) {
          // All attempts exhausted — fail loud rather than silently
          // dropping recipients. Caller's retry policy (workflow-level)
          // decides whether to re-invoke the originating send command.
          throw new Error(`recordQURLSendBatch: ${remaining.length} unprocessed items after ${MAX_RETRIES + 1} attempts`);
        }
        // Exponential backoff: 50ms, 100ms, 200ms, 400ms, 800ms.
        // `.unref()`-equivalent for Promise sleep isn't needed; the
        // await keeps the event loop moving.
        await new Promise(resolve => setTimeout(resolve, 50 * Math.pow(2, attempt)));
        requestItems = unprocessed;
      }
    }
    return;
  }

  // Add Recipients uses one TransactWriteItems call so recipient-row Put(s)
  // are atomically guarded by the send config row's revoked_at state. This
  // keeps the extra transaction cost scoped to the path that has the post-read
  // revoke race: if a revoke wins and sets revoked_at before this transaction
  // commits, none of the newly minted recipient rows land. The Add Recipients
  // UI caps at Discord's 25-select limit today, so this should stay far below
  // the 100-action transaction cap. If a future caller exceeds the cap, fail
  // before any DDB write instead of partially committing earlier chunks.
  if (!ddbSendConfigGuardFitsTransaction(sends)) {
    throw new Error(`recordQURLSendBatch: guarded batch exceeds TransactWriteItems action limit (${ddbSendConfigGuardActionCount(sends)} > ${DDB_TRANSACTION_MAX_ACTIONS})`);
  }

  const senderMap = new Map();
  for (const send of sends) {
    const existingSender = senderMap.get(send.sendId);
    if (existingSender !== undefined && existingSender !== send.senderDiscordId) {
      throw new Error(`recordQURLSendBatch: conflicting sender_discord_id for send_id ${send.sendId}`);
    }
    senderMap.set(send.sendId, send.senderDiscordId);
  }

  const sendIds = [...senderMap.keys()];
  // Keep ConditionChecks before Puts. The cancellation-reason parser above
  // relies on this guard-first order to map ConditionalCheckFailed reasons
  // back to the send ids that lost the revoke/delete/sender-mismatch race.
  const TransactItems = [
    ...sendIds.map(sendId => ({
      ConditionCheck: {
        TableName: TABLES.qurl_send_configs,
        Key: { send_id: sendId },
        // Missing config rows, revoked rows, and sender mismatches all
        // fail closed here. Add Recipients already read the config row; if
        // it disappears before persistence, do not create recipient rows.
        ConditionExpression: 'sender_discord_id = :sender AND attribute_not_exists(revoked_at)',
        ExpressionAttributeValues: { ':sender': senderMap.get(sendId) },
      },
    })),
    ...sends.map(s => ({
      Put: {
        TableName: TABLES.qurl_sends,
        Item: qurlSendBatchItem(s, now),
      },
    })),
  ];
  const ClientRequestToken = crypto.randomUUID();
  for (let attempt = 0; attempt <= MAX_RETRIES; attempt++) {
    try {
      await ddb.send(new TransactWriteCommand({ TransactItems, ClientRequestToken }));
      break;
    } catch (err) {
      // Fail closed before considering retries: a ConditionCheck failure on
      // the config guard means revoked/deleted/sender-mismatch state won the
      // race, even if other transaction items also report transient reasons.
      if (transactionGuardConditionFailed(err, sendIds.length)) {
        throw new SendConfigRevokedError(transactionConditionFailedSendIds(err, sendIds));
      }
      if (attempt < MAX_RETRIES && transactionWriteRetryable(err)) {
        await new Promise(resolve => setTimeout(resolve, 50 * Math.pow(2, attempt)));
        continue;
      }
      throw err;
    }
  }
}

// ── QURL views (webhook-fed view counter for /qurl send + /qurl map) ──

// 30d TTL on `expires_at` (peer attribute name across every TTL table
// in qurl-bot-ddb), refreshed on every write. Safely past the longest
// monitor lifetime (1h cap) + longest link expiry (7d) + DDB's ~48h
// TTL precision — no monitor will ever read a row about to reap.
const QURL_VIEW_TTL_SECONDS = 30 * 24 * 60 * 60;

// `consumed` is persisted + returned by getQurlViews but not yet
// rendered by the monitor (runTick only checks accessCount > 0).
// Reserved for a future "🔥 N consumed" state on self-destruct sends —
// see commands.js buildStatusMsg for the render site.

// MAX-merge conditional update — first arrival ALWAYS wins; subsequent
// arrivals must be a distinct event AND either advance access_count OR
// flip consumed false → true at the same access_count. The trailing
// false→true consumed clause covers a qurl-service emission shape
// where a follow-on event records the burn without re-bumping the
// counter; without it we'd silently drop that signal.
//
// Asymmetry note: a consumed: true → false flip at the same
// access_count is silently dropped as "dedup" — intentional for
// self-destruct semantics ("once consumed, always consumed"). If
// qurl-service ever needs to un-consume a row, that's a separate
// API contract change.
//
// accessCount must be >=1. The webhook route rejects zero before calling
// this shared store method; keep the store gate too so a future direct
// caller cannot persist a row that the view counter would treat as unseen.
async function recordQurlView({
  qurlId, accessCount, consumed, eventId,
}) {
  if (!qurlId) throw new Error('recordQurlView: qurlId is required');
  if (typeof accessCount !== 'number' || accessCount <= 0) {
    throw new Error(`recordQurlView: accessCount must be a positive number (got ${accessCount})`);
  }
  if (!eventId) throw new Error('recordQurlView: eventId is required for replay protection');
  const nowMs = Date.now();
  const consumedBool = Boolean(consumed);
  try {
    const res = await ddb.send(new UpdateCommand({
      TableName: TABLES.qurl_views,
      Key: { qurl_id: qurlId },
      // `consumed` is a DDB reserved keyword — must be aliased via
      // ExpressionAttributeNames or DDB returns ValidationException.
      ConditionExpression:
        'attribute_not_exists(last_event_id) OR ('
        + 'last_event_id <> :eid AND ('
        + 'access_count < :n OR (access_count = :n AND #consumed = :false AND :c = :true)'
        + '))',
      UpdateExpression: 'SET access_count = :n, #consumed = :c, last_event_id = :eid, last_updated = :now, expires_at = :exp',
      ExpressionAttributeNames: { '#consumed': 'consumed' },
      ExpressionAttributeValues: {
        ':n': accessCount,
        ':c': consumedBool,
        ':eid': eventId,
        ':now': new Date(nowMs).toISOString(),
        // DDB silently refuses to expire rows whose TTL attribute isn't
        // a Number — keep this as epoch seconds (integer), not a string.
        ':exp': Math.floor(nowMs / 1000) + QURL_VIEW_TTL_SECONDS,
        ':false': false,
        ':true': true,
      },
      ReturnValues: 'ALL_OLD',
    }));
    const old = res?.Attributes;
    return {
      result: 'recorded',
      firstView: !old || old.access_count === 0,
    };
  } catch (err) {
    if (err.name === 'ConditionalCheckFailedException') {
      // Replay (same event_id) OR out-of-order (lower-or-equal count
      // with no consumed flip). Row already reflects a >= state.
      return { result: 'dedup', firstView: false };
    }
    throw err;
  }
}

// Returns a Map<qurl_id, {accessCount, consumed}>. BatchGet caps at 100
// keys per request, so this chunks at 100. That chunking is LOAD-BEARING,
// not defensive: max recipients is QURL_SEND_MAX_RECIPIENTS (default
// 20000), so a full send is up to ⌈N/100⌉ = 200 sequential BatchGets.
// The sender-counter fast-path now renders from fanout-scaled sharded
// counters in this same table, so this BatchGet-all path is not on the
// normal sub-second render path. It remains the monitor poll reader and a
// legacy fallback for live rows created before the aggregate existed.
async function getQurlViews(qurlIds) {
  if (!Array.isArray(qurlIds) || qurlIds.length === 0) return new Map();
  // Drop empties defensively — an empty qurl_id is a collision attractor.
  const keys = [...new Set(qurlIds.filter(Boolean))].map(id => ({ qurl_id: id }));
  if (keys.length === 0) return new Map();
  const CHUNK = 100;
  const out = new Map();
  const collect = (res) => {
    for (const item of (res.Responses && res.Responses[TABLES.qurl_views]) || []) {
      out.set(item.qurl_id, {
        accessCount: item.access_count || 0,
        consumed: Boolean(item.consumed),
      });
    }
  };
  for (let i = 0; i < keys.length; i += CHUNK) {
    const slice = keys.slice(i, i + CHUNK);
    const res = await ddb.send(new BatchGetCommand({
      RequestItems: { [TABLES.qurl_views]: { Keys: slice } },
    }));
    collect(res);
    // Single retry on UnprocessedKeys — persistent throttle is an oncall
    // signal, not a per-call retry loop.
    const unprocessed = res.UnprocessedKeys && res.UnprocessedKeys[TABLES.qurl_views];
    if (unprocessed && unprocessed.Keys && unprocessed.Keys.length > 0) {
      collect(await ddb.send(new BatchGetCommand({ RequestItems: { [TABLES.qurl_views]: unprocessed } })));
    }
  }
  return out;
}

async function updateSendDMStatus(sendId, recipientDiscordId, status) {
  await ddb.send(new UpdateCommand({
    TableName: TABLES.qurl_sends,
    Key: { send_id: sendId, recipient_discord_id: recipientDiscordId },
    UpdateExpression: 'SET dm_status = :s',
    ExpressionAttributeValues: { ':s': status },
  }));
}

// Coalesces dm_status + DM channel/message refs into one Update so the
// hot dispatch path stays at one DDB write per successful recipient.
async function markSendDMDelivered(sendId, recipientDiscordId, channelId, messageId) {
  await ddb.send(new UpdateCommand({
    TableName: TABLES.qurl_sends,
    Key: { send_id: sendId, recipient_discord_id: recipientDiscordId },
    UpdateExpression: 'SET dm_status = :s, dm_channel_id = :c, dm_message_id = :m',
    ExpressionAttributeValues: { ':s': DM_STATUS.SENT, ':c': channelId, ':m': messageId },
  }));
}

// qurl_id-index GSI lookup for the qurl.expired webhook handler.
//
// Uniqueness caveat: DDB does NOT enforce hash-key uniqueness on a
// GSI, so this query returns an array. The write-path invariant is "one qurl_id per
// recipient row" (each /qurl send mint is unique per recipient at
// mintLinksInBatches time), so a healthy table should always return
// length 0 or 1. The handler MUST handle length > 1 defensively
// (log + skip) rather than blind-indexing [0].
//
// `Limit: 2` is a defense-in-depth cap: the handler only needs to
// distinguish 0 / 1 / >1, so bounding the read at 2 prevents a
// pathological duplicate-key explosion (write-path regression
// landing N>>1 rows under one qurl_id) from blowing up RCU on the
// hot path. The `> 1` ambiguous-recipient skip in the handler
// already catches the duplicate case at length=2.
//
// #1101 detect note: /qurl detect applies a same-guild filter to these
// rows, and Limit:2 is safe for it ONLY because a qurl_id belongs to a
// single guild (one mint → one recipient → one guild), so every row a
// qurl_id can return is same-guild and the cap can't truncate away the
// caller's row. If that invariant were ever relaxed (a qurl_id legitimately
// spanning guilds with >2 total rows), Limit:2 could drop the caller's
// same-guild row and yield a false "no match" — revisit the cap then.
//
// Returns the rows (full attributes — GSI projection is ALL so
// dm_channel_id / dm_message_id / expired_edited_at are present).
async function findSendsByQurlId(qurlId) {
  if (typeof qurlId !== 'string' || qurlId.length === 0) return [];
  const res = await ddb.send(new QueryCommand({
    TableName: TABLES.qurl_sends,
    IndexName: 'qurl_id-index',
    KeyConditionExpression: 'qurl_id = :q',
    ExpressionAttributeValues: { ':q': qurlId },
    Limit: 2,
  }));
  return res.Items || [];
}

// Idempotency marker for the qurl.expired DM-edit path. Separate from
// dm_status, which the revoke path uses as a read precondition for
// "is there a DM to edit." Sole idempotency layer for this path (the
// view-counter route's eventId dedup is view-specific and doesn't
// reach the expired handler).
//
// Conditional UpdateItem on attribute_not_exists: first call wins,
// repeats throw ConditionalCheckFailedException which the caller
// reads as "already edited, skip DM PATCH". Returns true on first-
// edit, false if already-edited.
async function markExpiredDMEdited(sendId, recipientDiscordId) {
  try {
    await ddb.send(new UpdateCommand({
      TableName: TABLES.qurl_sends,
      Key: { send_id: sendId, recipient_discord_id: recipientDiscordId },
      UpdateExpression: 'SET expired_edited_at = :t',
      ConditionExpression: 'attribute_not_exists(expired_edited_at)',
      ExpressionAttributeValues: { ':t': nowIso() },
    }));
    return true;
  } catch (err) {
    if (err.name === 'ConditionalCheckFailedException') return false;
    throw err;
  }
}

// Rollback the idempotency marker — only called by the qurl.expired
// handler when editDM reported a TRANSIENT failure (Discord 5xx or
// network throw). Rolling back lets qurl-service's webhook retry
// re-enter the handler and re-attempt the edit cleanly. On a PERMANENT
// failure (`ok:false && expected:true` — recipient blocked the bot /
// deleted the DM) the marker MUST stay so the retry short-circuits.
//
// Best-effort: a throw from this rollback isn't a separate failure
// mode the caller can do anything about — the marker stays, the next
// retry hits `already-edited`, and the missed edit falls back to the
// 8-day S3 lifecycle. We surface the throw so the caller can log it,
// not for control flow.
async function clearExpiredDMEdited(sendId, recipientDiscordId) {
  await ddb.send(new UpdateCommand({
    TableName: TABLES.qurl_sends,
    Key: { send_id: sendId, recipient_discord_id: recipientDiscordId },
    UpdateExpression: 'REMOVE expired_edited_at',
  }));
}

// Idempotency marker for the qurl.accessed consumed-flip DM-edit path.
// Separate attribute from expired_edited_at by design: the two flips can
// both legitimately reach the same row (a one-time qURL is consumed,
// THEN its 30m TTL elapses and qurl-service still emits qurl.expired —
// see the EventQurlExpired contract, "fires regardless of prior state").
// Distinct markers let each path short-circuit its own redelivery
// without one suppressing the other, and let the expired handler detect
// "consumed already flipped the DM" via a cheap read of this attribute
// off the row it already fetched (findSendsByQurlId projects ALL).
//
// Same conditional-UpdateItem-on-attribute_not_exists shape as
// markExpiredDMEdited: first call wins, repeats throw
// ConditionalCheckFailedException which the caller reads as
// "already flipped, skip the DM PATCH". Returns true on first-flip,
// false if already-flipped.
async function markConsumedDMEdited(sendId, recipientDiscordId) {
  try {
    await ddb.send(new UpdateCommand({
      TableName: TABLES.qurl_sends,
      Key: { send_id: sendId, recipient_discord_id: recipientDiscordId },
      UpdateExpression: 'SET consumed_edited_at = :t',
      ConditionExpression: 'attribute_not_exists(consumed_edited_at)',
      ExpressionAttributeValues: { ':t': nowIso() },
    }));
    return true;
  } catch (err) {
    if (err.name === 'ConditionalCheckFailedException') return false;
    throw err;
  }
}

// Rollback of the consumed-flip marker — counterpart to
// clearExpiredDMEdited. Only called when editDM reported a TRANSIENT
// failure on the consumed-flip path: rolling the marker back lets a
// REDELIVERED qurl.accessed (or, failing that, the eventual
// qurl.expired backstop — which skips when consumed_edited_at is set)
// re-attempt the flip. On a PERMANENT failure (recipient blocked the
// bot / deleted the DM) the marker MUST stay so a redelivery
// short-circuits.
//
// Best-effort, same contract as clearExpiredDMEdited: a throw here isn't
// a control-flow signal — it means the marker stays, the redelivery (or
// expired backstop) sees it and skips, and the missed flip falls back to
// the qurl.expired edit only if THAT path's marker is also clear. We
// surface the throw so the caller can log it.
async function clearConsumedDMEdited(sendId, recipientDiscordId) {
  await ddb.send(new UpdateCommand({
    TableName: TABLES.qurl_sends,
    Key: { send_id: sendId, recipient_discord_id: recipientDiscordId },
    UpdateExpression: 'REMOVE consumed_edited_at',
  }));
}

// Sender-revoke check for the qurl.expired handler. A send whose
// sender ran /qurl revoke has its DM already edited to "Alice closed
// the door" — re-editing to "Closed N ago" would overwrite that copy
// with a less-specific message and obscure the revoke signal.
//
// Reads qurl_send_configs (the table markSendRevoked writes to);
// returns true iff revoked_at is set on the row. Falsy / missing row
// returns false so a config-less legacy send (none exist today, but
// the row insert in markSendRevoked is lazy) still gets the
// expiry-edit treatment.
async function isSendRevoked(sendId) {
  if (typeof sendId !== 'string' || sendId.length === 0) return false;
  const res = await ddb.send(new GetCommand({
    TableName: TABLES.qurl_send_configs,
    Key: { send_id: sendId },
  }));
  return Boolean(res.Item?.revoked_at);
}

async function getRecentSends(senderDiscordId, limit = 10) {
  // SQL did a LEFT JOIN on qurl_send_configs + GROUP BY send_id to
  // produce one row per send with per-send metadata. DDB: query the
  // sender's recent send_ids via the GSI to build the UNIQUE set of
  // recent sends (limit*3 overfetch for revoked-filter headroom),
  // then do a per-send Query to compute the accurate
  // recipient_count / delivered_count (the GSI result may truncate
  // if one send has many recipients). Then Promise.all the config
  // fetches. Bounded fanout at ~10 queries + 10 gets.
  // Paginate the GSI until we've collected `limit * 2` unique
  // send_ids (2x for revoked-filter headroom) OR the GSI is
  // exhausted. Fixed-Limit overfetch would starve older sends if a
  // single recent send has many recipients — their rows fill the
  // window and push older distinct sends past the limit. Paginating
  // until `limit * 2` unique send_ids collected avoids that
  // starvation. Capped at 10 pagination rounds to bound worst-case
  // cost when a sender has hundreds of recent sends.
  // GSI projection requirement: this Query reads `resource_type`,
  // `target_type`, `channel_id`, `expires_in`, `created_at` directly
  // off the returned rows (see metaBySendId usage below). That only
  // works because `sender_discord_id-created_at-index` is declared
  // with `projection_type = "ALL"` in the qurl-bot-ddb terraform
  // module (modules/qurl-bot-ddb/main.tf — search for the GSI block
  // on the `qurl_sends` table). If that ever narrows to KEYS_ONLY
  // or INCLUDE without these attrs, those fields silently become
  // `undefined` at runtime — unit tests won't catch it because the
  // mock returns full rows. Cross-repo invariant; flag both sides
  // if you change either.
  const metaBySendId = new Map();
  const sendIdOrder = [];
  let ExclusiveStartKey;
  const MAX_GSI_PAGES = 10;
  const TARGET_UNIQUE_SENDS = limit * 2;
  // Per-page GSI Limit: caps RCU on the hot path. Without it, a
  // sender whose latest send fanned out to 1000 recipients reads
  // ALL 1000 rows (1MB / 8KB-ish per row → up to ~125 RCU
  // amortized per page) every time the user runs /qurl history,
  // even though the function only needs `TARGET_UNIQUE_SENDS`
  // distinct send_ids. 100 is a comfortable upper bound on
  // recipients-per-typical-send while still letting up-to-100
  // distinct sends land in a single page on senders who use the
  // command for many small sends. Pagination already handles the
  // case where one page isn't enough.
  const GSI_PAGE_LIMIT = 100;
  for (let page = 0; page < MAX_GSI_PAGES && sendIdOrder.length < TARGET_UNIQUE_SENDS; page++) {
    const sendsRes = await ddb.send(new QueryCommand({
      TableName: TABLES.qurl_sends,
      IndexName: 'sender_discord_id-created_at-index',
      KeyConditionExpression: 'sender_discord_id = :s',
      ExpressionAttributeValues: { ':s': senderDiscordId },
      ScanIndexForward: false,
      Limit: GSI_PAGE_LIMIT,
      ExclusiveStartKey,
    }));
    // Why first-seen-wins (no overwrite branch): the GSI is queried
    // with ScanIndexForward: false, so DDB returns rows in
    // descending created_at order. The first row we see for a given
    // send_id IS the newest, and any later row in the same send is
    // strictly older. An overwrite-on-newer branch would be dead
    // code under that ordering. If a future change ever flips
    // ScanIndexForward to true, revisit this — first-seen would
    // become oldest-wins which isn't what we want.
    for (const row of sendsRes.Items || []) {
      if (!metaBySendId.has(row.send_id)) {
        sendIdOrder.push(row.send_id);
        metaBySendId.set(row.send_id, row);
      }
    }
    ExclusiveStartKey = sendsRes.LastEvaluatedKey;
    if (!ExclusiveStartKey) break;
  }

  // Parallel fetch: per-send recipient query + per-send config
  // getItem. Query on the BASE table (pk=send_id) returns every
  // recipient for that send, so counts are exact.
  //
  // Pagination matters here: a single send fanning out to thousands
  // of recipients (or shorter rows but >1MB total) would silently
  // truncate at one DDB page if we used a bare QueryCommand, and
  // recipient_count / delivered_count would under-report. queryAll
  // threads LastEvaluatedKey across pages so the count is exact
  // regardless of fan-out size.
  const [allRecipients, allConfigs] = await Promise.all([
    Promise.all(sendIdOrder.map(id => queryAll({
      TableName: TABLES.qurl_sends,
      KeyConditionExpression: 'send_id = :sid',
      ExpressionAttributeValues: { ':sid': id },
    }))),
    Promise.all(sendIdOrder.map(id => ddb.send(new GetCommand({
      TableName: TABLES.qurl_send_configs,
      Key: { send_id: id },
    })))),
  ]);

  const rows = [];
  for (let i = 0; i < sendIdOrder.length; i++) {
    const sendId = sendIdOrder[i];
    const meta = metaBySendId.get(sendId);
    const recipients = allRecipients[i] || [];
    const cfg = allConfigs[i].Item;
    if (cfg && cfg.revoked_at) continue;
    const recipientCount = recipients.length;
    const deliveredCount = recipients.filter(r => r.dm_status === 'sent').length;
    rows.push({
      send_id: sendId,
      resource_type: meta.resource_type,
      target_type: meta.target_type,
      channel_id: meta.channel_id,
      expires_in: meta.expires_in,
      created_at: meta.created_at,
      recipient_count: recipientCount,
      delivered_count: deliveredCount,
      attachment_name: cfg?.attachment_name,
      location_name: cfg?.location_name,
      personal_message: cfg?.personal_message,
    });
  }
  rows.sort((a, b) => (b.created_at || '').localeCompare(a.created_at || ''));
  return rows.slice(0, limit);
}

async function flipRevokedAt(sendId, senderDiscordId) {
  // Sets revoked_at on an existing config row, owner-scoped, with
  // idempotent CCFE swallow. Used by both the normal markSendRevoked
  // path AND the legacy-CCFE recovery path (when a non-revoke writer
  // raced us into inserting a config row mid-flight).
  try {
    await ddb.send(new UpdateCommand({
      TableName: TABLES.qurl_send_configs,
      Key: { send_id: sendId },
      UpdateExpression: 'SET revoked_at = :t',
      ExpressionAttributeValues: {
        ':t': nowIso(),
        ':s': senderDiscordId,
      },
      ConditionExpression: 'sender_discord_id = :s AND attribute_not_exists(revoked_at)',
    }));
  } catch (err) {
    // Swallow CCFE — either revoked_at is already set (idempotent
    // success) or sender_discord_id doesn't match (ownership reject,
    // matching the SQL `WHERE sender_discord_id = ?` filter). Any
    // other error propagates.
    if (err.name !== 'ConditionalCheckFailedException') throw err;
  }
}

async function markSendRevoked(sendId, senderDiscordId) {
  // Two-branch logic:
  // (a) Normal path: config row exists — flip revoked_at.
  // (b) Legacy path: config row doesn't exist — insert minimal row.
  // Scoped to senderDiscordId for defense-in-depth.
  const cfgRes = await ddb.send(new GetCommand({
    TableName: TABLES.qurl_send_configs,
    Key: { send_id: sendId },
  }));
  if (cfgRes.Item) {
    if (cfgRes.Item.sender_discord_id !== senderDiscordId) return; // ownership check
    if (cfgRes.Item.revoked_at) return; // idempotent
    await flipRevokedAt(sendId, senderDiscordId);
    return;
  }

  // Legacy — look up qurl_sends row, insert minimal config with revoked_at.
  // No `Limit: 1`: DDB applies Limit BEFORE FilterExpression, so the
  // first server-side row gets read and the filter on
  // sender_discord_id is then applied to that single row. Today every
  // recipient row of a given send_id shares one sender_discord_id (per
  // recordQURLSend / recordQURLSendBatch invariants), so a Limit:1
  // would be safe — but a future migration / manual repair / partial
  // write that violated the invariant would silently miss the
  // matching row and the revoke would no-op. Dropping the Limit makes
  // the lookup robust regardless of the partition layout: the
  // partition is bounded by recipient count anyway, and the match-
  // first-then-stop happens client-side via the .find() below.
  const legacyRes = await ddb.send(new QueryCommand({
    TableName: TABLES.qurl_sends,
    KeyConditionExpression: 'send_id = :sid',
    FilterExpression: 'sender_discord_id = :s',
    ExpressionAttributeValues: { ':sid': sendId, ':s': senderDiscordId },
  }));
  if (!legacyRes.Items || legacyRes.Items.length === 0) return;
  const legacy = legacyRes.Items[0];
  try {
    await ddb.send(new PutCommand({
      TableName: TABLES.qurl_send_configs,
      Item: {
        send_id: sendId,
        sender_discord_id: senderDiscordId,
        resource_type: legacy.resource_type,
        expires_in: legacy.expires_in || '24h',
        revoked_at: nowIso(),
        created_at: nowIso(),
      },
      ConditionExpression: 'attribute_not_exists(send_id)',
    }));
  } catch (err) {
    if (err.name !== 'ConditionalCheckFailedException') throw err;
    // Race: between the GetCommand at the top of this function
    // (which saw no config row) and this Put, SOMEONE inserted a
    // config row. Three possibilities:
    //   - Another markSendRevoked beat us — the row is revoked,
    //     no action needed.
    //   - saveSendConfig (or any non-revoke writer) inserted a
    //     config without revoked_at. The previous version
    //     swallowed the CCFE and returned, silently losing this
    //     revoke intent. Now we fall through to flipRevokedAt
    //     which Updates revoked_at on the existing row (with the
    //     same owner-scoped + attribute_not_exists guard the
    //     primary path uses).
    //   - A racing markSendRevoked AND a non-revoke writer
    //     interleave: flipRevokedAt's CCFE-swallow handles the
    //     "already revoked" case idempotently.
    await flipRevokedAt(sendId, senderDiscordId);
  }
}

async function saveSendConfig({
  sendId, senderDiscordId, resourceType, connectorResourceId, actualUrl,
  expiresIn, personalMessage, locationName, attachmentName,
  attachmentContentType, attachmentUrl, selfDestructSeconds,
}) {
  const encryptedUrl = attachmentUrl ? encrypt(attachmentUrl) : null;
  await ddb.send(new PutCommand({
    TableName: TABLES.qurl_send_configs,
    Item: {
      send_id: sendId,
      sender_discord_id: senderDiscordId,
      resource_type: resourceType,
      connector_resource_id: connectorResourceId,
      actual_url: actualUrl,
      expires_in: expiresIn,
      personal_message: personalMessage,
      location_name: locationName,
      attachment_name: attachmentName,
      attachment_content_type: attachmentContentType ?? null,
      attachment_url: encryptedUrl,
      self_destruct_seconds: selfDestructSeconds ?? null,
      // The view-counter render state (interaction_token, expected_count,
      // confirm_base_msg, confirm_expires_at, confirm_qurl_ids) is written
      // SEPARATELY by saveSendConfirmState AFTER the initial editReply —
      // saveSendConfig runs earlier (before the token / confirmMsg /
      // delivered exist), so it deliberately carries none of those fields.
      created_at: nowIso(),
    },
  }));
}

async function getSendConfig(sendId, senderDiscordId) {
  const res = await ddb.send(new GetCommand({
    TableName: TABLES.qurl_send_configs,
    Key: { send_id: sendId },
  }));
  const row = res.Item;
  if (!row) return row;
  if (row.sender_discord_id !== senderDiscordId) return undefined; // ownership check
  // SECURITY: never hand the SENSITIVE interaction_token back in the
  // full-row return. PR-B persists that live ~15-min bearer cred onto
  // THIS row, but getSendConfig's callers (handleAddRecipients, the
  // status card) only read scalar config fields — they never need the
  // token, and returning it risks a caller logging/audit-shipping/error-
  // dumping the whole config object and leaking the cred. The fast-path
  // reads the token via getSendRenderState (which is the ONLY return
  // shape that intentionally carries it, and its sole caller logs only
  // scalars). Strip it here so the full-row getter is token-free.
  // Same destructure-and-omit idiom as getGuildConfig's qurl_api_key strip
  // (the `_drop` throwaway matches that precedent + the `_`-prefix
  // no-unused-vars convention).
  const { interaction_token: _drop, ...safe } = row;
  return safe.attachment_url
    ? { ...safe, attachment_url: decrypt(safe.attachment_url) }
    : safe;
}

// View-counter render state for the cross-replica fast-path (PR-B).
// Returns ONLY the fields the off-monitor renderer needs to rebuild +
// edit the sender's confirmation — NOT the full row — so the SENSITIVE
// interaction_token never leaks into a logged/returned shape by accident
// (the token is read directly by PR-B's editReply call, not surfaced
// here). Keyed by send_id alone: unlike getSendConfig there is NO
// sender_discord_id ownership filter, because PR-B's webhook path has no
// Discord sender context — the send_id (an unguessable UUID minted
// server-side) is the only key it carries. Returns null when no row
// exists (legacy send predating render-state persistence, or a
// saveSendConfig that failed — both leave the fast-path inert and the
// in-memory monitor as the sole renderer).
//
// `viewedCount` is the legacy single-row aggregate retained only as a
// rollout floor for rows created before the sharded counter path below.
// The new path seeds it at send time but never increments it. New
// fast-path renders sum qurl_views counter shards instead of BatchGetting
// every qurl_id or hot-writing qurl_send_configs. The `lastRenderedCount`
// field remains the commit-after-edit floor: the sharded total can run
// ahead when a view records but the Discord edit is coalesced or fails.
//
// `terminal` is derived (not stored as one flag): the display is dead
// once the sender revoked (revoked_at) OR a window-close/expired path
// set confirm_terminal — either way a late fast-path edit must NOT
// resurrect a live-looking counter. `qurlIds` is an optional inline cache
// of minted qurl_ids for small sends; large sends deliberately store []
// so the row stays well below DDB's item limit and the rare fallback reads
// recipient rows via getSendItems instead.
//
// EXPIRY SELF-DEFENSE: the qurl_send_configs table has no DDB TTL on
// confirm_expires_at yet (that needs a qurl-bot-ddb terraform change —
// see saveSendConfirmState). So the bot enforces the token's lifetime
// itself: once `now > confirm_expires_at` we treat the render state as
// absent (return null), so the fast-path stops trusting an
// interaction_token past its ~15-min Discord TTL regardless of whether
// the row has been physically reaped. This decouples the feature from
// the cross-repo TTL change — at worst a dead token lingers at rest, but
// the bot never reads it.
async function getSendRenderState(sendId) {
  if (typeof sendId !== 'string' || sendId.length === 0) return null;
  const res = await ddb.send(new GetCommand({
    TableName: TABLES.qurl_send_configs,
    Key: { send_id: sendId },
    ConsistentRead: true,
  }));
  const row = res.Item;
  if (!row) return null;
  // Past the confirm window the interaction token is dead (Discord
  // rejects it); treat the whole render state as absent so the fast-path
  // skips and the poll backstop is the sole renderer.
  if (typeof row.confirm_expires_at === 'number' && Math.floor(Date.now() / 1000) > row.confirm_expires_at) {
    return null;
  }
  return {
    // SENSITIVE: a live Discord bearer cred (~15 min). NEVER log this
    // value — callers use it only as the editReply credential.
    interactionToken: row.interaction_token ? decrypt(row.interaction_token) : null,
    interactionAppId: row.interaction_app_id ?? null,
    expectedCount: row.expected_count ?? 0,
    viewedCount: typeof row.viewed_count === 'number' ? row.viewed_count : null,
    lastRenderedCount: row.last_rendered_count ?? 0,
    // Epoch MS of the last confirmed fast-path edit (0 if never). The
    // webhook fast-path leading-edge-debounces on this: skip the edit
    // when Date.now() - lastRenderedAt < QURL_VIEW_COUNTER_COALESCE_MS.
    // MS (not the seconds confirm_expires_at uses) because the cooldown
    // is sub-second — seconds granularity would be too coarse to bound a
    // sub-second burst. Written alongside last_rendered_count in the
    // SAME conditional UpdateItem (commit-after-edit), so it only
    // advances when an edit actually landed.
    lastRenderedAt: row.last_rendered_at ?? 0,
    baseMsg: row.confirm_base_msg ?? undefined,
    qurlIds: Array.isArray(row.confirm_qurl_ids) ? row.confirm_qurl_ids : [],
    terminal: Boolean(row.revoked_at) || row.confirm_terminal === true,
  };
}

const SEND_VIEW_COUNTER_MAX_SHARDS = 64;
// Roughly half DynamoDB's ~1k WCU/s single-partition ceiling, leaving
// burst headroom before the next power-of-two shard step kicks in.
const SEND_VIEW_COUNTER_TARGET_WRITES_PER_SHARD = 500;
const SEND_VIEW_COUNTER_PREFIX = '__send_view_count__';
// Keep the optional inline qurl_id cache comfortably below DynamoDB's
// 400KB item limit. Larger sends fall back to getSendItems when the
// sharded aggregate is unavailable; normal renders use the aggregate.
const SEND_CONFIRM_QURL_IDS_INLINE_LIMIT = 1000;

function normalizeConfirmQurlIdsForInlineCache(qurlIds) {
  if (qurlIds === undefined) return undefined;
  if (!Array.isArray(qurlIds)) return [];
  const ids = qurlIds.filter(id => typeof id === 'string' && id.length > 0);
  return ids.length <= SEND_CONFIRM_QURL_IDS_INLINE_LIMIT ? ids : [];
}

function sendViewedCountShardCount(expectedCount) {
  // Invalid/legacy expected_count falls back to the one-shard floor. The
  // persisted value should be positive and only grow; choosing the floor keeps
  // later repaired reads a superset of earlier writes.
  // A view racing a /qurl add re-persist can briefly read the old shard count;
  // that can undercount newer wider-shard writes until the next strong render
  // state read or poll backstop, but the monotonic floor prevents regression.
  const count = Number.isSafeInteger(expectedCount) && expectedCount > 0
    ? expectedCount
    : 1;
  const needed = Math.ceil(count / SEND_VIEW_COUNTER_TARGET_WRITES_PER_SHARD);
  let shards = 1;
  while (shards < needed && shards < SEND_VIEW_COUNTER_MAX_SHARDS) shards *= 2;
  return shards;
}

function sendViewedCountShard(qurlId, shardCount) {
  return crypto.createHash('sha256').update(String(qurlId)).digest()[0] % shardCount;
}

function sendViewedCountKey(sendId, shard) {
  return `${SEND_VIEW_COUNTER_PREFIX}${sendId}#${String(shard).padStart(2, '0')}`;
}

function sendViewedCountKeys(sendId, shardCount) {
  return Array.from({ length: shardCount }, (_, shard) => ({
    qurl_id: sendViewedCountKey(sendId, shard),
  }));
}

// First-view aggregate for the sender counter. This deliberately lives in
// qurl_views as synthetic counter rows per send, not on the single
// qurl_send_configs row. Small sends use one shard; high-fanout sends
// scale up to 64 shards so a 20k-recipient burst spreads writes instead
// of funnelling every first view through one hot item. /qurl add only
// grows expected_count, so later renders that read more shards still
// include earlier counts written to the lower shard range.
async function incrementSendViewedCount(sendId, qurlId, expectedCount) {
  if (!sendId) throw new Error('incrementSendViewedCount: sendId is required');
  if (!qurlId) throw new Error('incrementSendViewedCount: qurlId is required');
  const shardCount = sendViewedCountShardCount(expectedCount);
  const shard = sendViewedCountShard(qurlId, shardCount);
  await ddb.send(new UpdateCommand({
    TableName: TABLES.qurl_views,
    Key: { qurl_id: sendViewedCountKey(sendId, shard) },
    UpdateExpression: 'SET send_id = :sid, counter_shard = :shard, expires_at = :exp ADD viewed_count :one',
    ExpressionAttributeValues: {
      ':sid': sendId,
      ':shard': shard,
      ':exp': Math.floor(Date.now() / 1000) + QURL_VIEW_TTL_SECONDS,
      ':one': 1,
    },
  }));
}

async function getSendViewedCount(sendId, expectedCount) {
  if (!sendId) return 0;
  const keys = sendViewedCountKeys(sendId, sendViewedCountShardCount(expectedCount));
  const res = await ddb.send(new BatchGetCommand({
    RequestItems: { [TABLES.qurl_views]: { Keys: keys, ConsistentRead: true } },
  }));
  let total = 0;
  const collect = (batch) => {
    for (const item of (batch.Responses && batch.Responses[TABLES.qurl_views]) || []) {
      if (typeof item.viewed_count === 'number' && item.viewed_count > 0) {
        total += item.viewed_count;
      }
    }
  };
  collect(res);
  const unprocessed = res.UnprocessedKeys && res.UnprocessedKeys[TABLES.qurl_views];
  if (unprocessed && unprocessed.Keys && unprocessed.Keys.length > 0) {
    // Single retry by design: if sustained DDB throttling still leaves a
    // partial sum, the next non-coalesced render and the poll backstop
    // re-read from source-of-truth qurl_views rows and correct the count.
    collect(await ddb.send(new BatchGetCommand({
      RequestItems: { [TABLES.qurl_views]: { ...unprocessed, ConsistentRead: true } },
    })));
  }
  return total;
}

// Strong, token-free read of the displayed counter floor. The poll renderer
// uses this to avoid rendering below a fast-path edit that already advanced
// last_rendered_count on another replica.
async function getSendRenderedCount(sendId) {
  if (!sendId) return 0;
  const res = await ddb.send(new GetCommand({
    TableName: TABLES.qurl_send_configs,
    Key: { send_id: sendId },
    ProjectionExpression: 'last_rendered_count',
    ConsistentRead: true,
  }));
  const n = res.Item?.last_rendered_count;
  return typeof n === 'number' && n > 0 ? n : 0;
}

// Monotonic AFTER-edit commit for the view counter (PR-B calls this
// ONLY after a Discord edit confirmed the new count is displayed).
// Conditional UpdateItem: SET last_rendered_count = :n guarded by
// "never set yet OR strictly less than :n", so two replicas racing to
// advance the same send can't move the displayed count backwards —
// whichever confirms the higher count wins, the loser CCFEs. Returns
// true when this call advanced the count, false on
// ConditionalCheckFailedException (a concurrent/stale advance — the
// caller treats it as "someone already showed an equal-or-higher count,
// nothing to do"). Same CCFE-as-control-flow shape as
// markExpiredDMEdited / flipRevokedAt.
//
// COALESCING: last_rendered_at (epoch MS) is stamped in the SAME
// successful conditional write as last_rendered_count. If another replica
// already committed an equal-or-higher count, the whole write CCFEs and
// that winning replica's timestamp is the debounce clock. The FAILURE path
// stamps the clock alone via touchRenderedAt (below), so retry storms still
// coalesce without claiming a count was displayed.
async function tryAdvanceRenderedCount(sendId, n) {
  try {
    await ddb.send(new UpdateCommand({
      TableName: TABLES.qurl_send_configs,
      Key: { send_id: sendId },
      UpdateExpression: 'SET last_rendered_count = :n, last_rendered_at = :now',
      ConditionExpression: 'attribute_not_exists(last_rendered_count) OR last_rendered_count < :n',
      ExpressionAttributeValues: { ':n': n, ':now': Date.now() },
    }));
    return true;
  } catch (err) {
    if (err.name === 'ConditionalCheckFailedException') return false;
    throw err;
  }
}

// FAILURE-PATH debounce stamp: refresh the coalesce clock WITHOUT touching
// the count, called when an edit ATTEMPT completed but did NOT confirm
// (fast-path r.ok === false). Unconditional SET — no count guard, because
// the whole point is to arm the cooldown even though nothing was
// displayed. Keeps a high-fan-out burst from re-attempting one PATCH per
// view during a transient Discord edit outage; the count floor is left
// untouched so the poll backstop still self-heals the display. Best-
// effort: the fast-path swallows a throw (the poll covers the miss).
async function touchRenderedAt(sendId) {
  await ddb.send(new UpdateCommand({
    TableName: TABLES.qurl_send_configs,
    Key: { send_id: sendId },
    UpdateExpression: 'SET last_rendered_at = :now',
    ExpressionAttributeValues: { ':now': Date.now() },
  }));
}

// Distributed leading-edge debounce claim. Unlike tryAdvanceRenderedCount,
// this does NOT claim the count moved; it only stamps the shared edit-attempt
// clock when the previous attempt is outside the coalesce window. That turns
// a cross-replica burst into one Discord PATCH per send/window instead of one
// per replica/window, while preserving the commit-after-edit count invariant.
async function tryClaimRenderAttempt(sendId, staleBeforeMs) {
  if (!sendId) return false;
  try {
    await ddb.send(new UpdateCommand({
      TableName: TABLES.qurl_send_configs,
      Key: { send_id: sendId },
      UpdateExpression: 'SET last_rendered_at = :now',
      ConditionExpression: 'attribute_not_exists(last_rendered_at) OR last_rendered_at <= :cutoff',
      ExpressionAttributeValues: { ':now': Date.now(), ':cutoff': staleBeforeMs },
    }));
    return true;
  } catch (err) {
    if (err.name === 'ConditionalCheckFailedException') return false;
    throw err;
  }
}

// Sticky fast-path kill-switch. Revoke/window-close use it for frozen
// displays; mid-life /qurl add degrade uses it for an alive-but-bare poll
// display. New code that needs display liveness must add its own signal,
// not infer it from confirm_terminal. Follow-up #875 renames this to the
// mechanism it actually gates: confirm_fast_path_off.
async function markConfirmTerminal(sendId) {
  await ddb.send(new UpdateCommand({
    TableName: TABLES.qurl_send_configs,
    Key: { send_id: sendId },
    UpdateExpression: 'SET confirm_terminal = :t',
    ExpressionAttributeValues: { ':t': true },
  }));
}

// Persists the cross-replica fast-path's render state onto the
// qurl_send_configs row AFTER the initial confirmation editReply has
// landed (the send-pipeline calls saveSendConfig EARLIER, before
// confirmMsg / delivered / the interaction token exist, so this is a
// SEPARATE write rather than more optional args on saveSendConfig).
//
// PARTIAL UPDATE BY DESIGN — the SET clause is built from ONLY the keys
// the caller actually passed (anything `=== undefined` is skipped). Two
// callers with different field sets share this fn safely:
//   - send-time wires the full set (interactionToken, interactionAppId,
//     expectedCount, confirmBaseMsg, confirmExpiresAt, confirmQurlIds) to
//     arm the fast-path; and
//   - /qurl add re-persists ONLY expectedCount + confirmBaseMsg +
//     confirmQurlIds to track the new totals + newly-minted links.
// A fixed five-attribute SET would let the add caller's omitted
// interactionToken land as null and PERMANENTLY disarm the fast-path
// (its absent-guard skips on a null token). Building the clause from the
// present keys is what keeps the add re-persist from clobbering the live
// token. No-op (no write) when no recognized field is present.
// confirmQurlIds is only an inline fallback cache; cap it before write so
// max-fanout sends cannot push qurl_send_configs near DDB's item limit.
//
// SECURITY: interactionToken is a live Discord interaction-webhook bearer
// cred (~15 min, TTL'd via confirm_expires_at). NEVER log it — this fn
// writes it but never logs, and the field name is the only thing that
// ever appears in a log.
async function saveSendConfirmState(sendId, {
  interactionToken, interactionAppId, expectedCount, confirmBaseMsg, confirmExpiresAt, confirmQurlIds, viewedCount,
} = {}) {
  // Map of attribute → provided value, keeping only the keys the caller
  // actually passed so an omitted field is left untouched (never nulled).
  const fields = {
    interaction_token: interactionToken === undefined ? undefined : encrypt(interactionToken),
    interaction_app_id: interactionAppId,
    expected_count: expectedCount,
    confirm_base_msg: confirmBaseMsg,
    confirm_expires_at: confirmExpiresAt,
    confirm_qurl_ids: normalizeConfirmQurlIdsForInlineCache(confirmQurlIds),
    viewed_count: viewedCount,
  };
  const sets = [];
  const values = {};
  let expectedCountPlaceholder = null;
  let i = 0;
  for (const [attr, val] of Object.entries(fields)) {
    if (val === undefined) continue;
    const placeholder = `:v${i++}`;
    sets.push(`${attr} = ${placeholder}`);
    values[placeholder] = val;
    if (attr === 'expected_count') expectedCountPlaceholder = placeholder;
  }
  if (sets.length === 0) return;
  const update = {
    TableName: TABLES.qurl_send_configs,
    Key: { send_id: sendId },
    UpdateExpression: `SET ${sets.join(', ')}`,
    ExpressionAttributeValues: values,
  };
  if (expectedCountPlaceholder) {
    // Fail closed on stale lower totals. The only production add path
    // serializes growth; if a future/stale caller tries to lower the
    // fanout, keep the matching baseMsg/qurlIds from landing too so the
    // render state stays internally consistent.
    update.ConditionExpression = `attribute_not_exists(expected_count) OR expected_count <= ${expectedCountPlaceholder}`;
  }
  try {
    await ddb.send(new UpdateCommand(update));
    return true;
  } catch (err) {
    if (err.name === 'ConditionalCheckFailedException' && expectedCountPlaceholder) {
      return false;
    }
    throw err;
  }
}

// Deduped resource_id list. Currently only used by tests; src/ uses
// `getSendItems` for the per-recipient mapping. Delegates to
// `getSendItems` (single source of truth for pagination).
async function getSendResourceIds(sendId, senderDiscordId) {
  const items = await getSendItems(sendId, senderDiscordId);
  return [...new Set(items.map(i => i.resource_id))];
}

// Returns the full per-recipient items so the revoke path can map
// per-link success/failure back to a Discord user id for display.
// Pagination required: a single send fanning out to thousands of
// recipients can exceed the 1MB Query page cap. Without queryAll,
// resource_ids on later pages would silently drop and the revoke
// path would skip them.
//
// RETURN SHAPE: { resource_id, recipient_discord_id, dm_channel_id?,
// dm_message_id?, dm_status? }. The dm_* fields are written by
// markSendDMDelivered after a successful sendDM and consumed by the
// revoke loop's editTargets builder (commands.js) to PATCH the
// recipient's DM to "closed the door". Legacy rows predating the
// ref-capture wire-up have those fields unset — the revoke loop's
// missing-refs guard skips the edit naturally.
async function getSendItems(sendId, senderDiscordId) {
  const items = await queryAll({
    TableName: TABLES.qurl_sends,
    KeyConditionExpression: 'send_id = :sid',
    FilterExpression: 'sender_discord_id = :s',
    ExpressionAttributeValues: { ':sid': sendId, ':s': senderDiscordId },
  });
  return items.map(item => ({
    resource_id: item.resource_id,
    recipient_discord_id: item.recipient_discord_id,
    // qurl_id is the sparse GSI hash key (recordQURLSendBatch writes it
    // only when the mint surfaced one). Projected here so the webhook
    // fast-path can map a send's recipient rows → its tracked qurl_ids
    // and count DISTINCT viewed links. Legacy / non-guild sends omit it
    // (undefined); the fast-path filters falsy before the views BatchGet.
    qurl_id: item.qurl_id,
    // dm_channel_id / dm_message_id are written by markSendDMDelivered
    // after a successful sendDM; legacy rows predating that wire-up
    // have them unset, in which case the revoke path skips the DM
    // edit. dm_status gates the same skip — failed deliveries have
    // nothing to edit.
    dm_channel_id: item.dm_channel_id,
    dm_message_id: item.dm_message_id,
    dm_status: item.dm_status,
  }));
}

// ── Guild (BYOK) API keys ──

async function getGuildApiKey(guildId) {
  const res = await ddb.send(new GetCommand({
    TableName: TABLES.guild_configs,
    Key: { guild_id: guildId },
  }));
  return res.Item ? decrypt(res.Item.qurl_api_key) : null;
}

async function setGuildApiKey(guildId, apiKey, configuredBy) {
  const now = nowIso();
  // SQLite's `ON CONFLICT(guild_id) DO UPDATE SET qurl_api_key=…,
  // configured_by=…, updated_at=…` deliberately preserved
  // `configured_at` across re-keys — the field tracks "when was
  // this guild FIRST configured" and downstream cohort analytics
  // depend on that semantic. A naive PutItem rewrites the whole
  // row, including resetting configured_at. Mirror createLink's
  // shape: UpdateCommand with `if_not_exists(configured_at, :u)`
  // so first-write sets it and re-keys leave it alone.
  await ddb.send(new UpdateCommand({
    TableName: TABLES.guild_configs,
    Key: { guild_id: guildId },
    UpdateExpression: 'SET qurl_api_key = :k, configured_by = :b, updated_at = :u, configured_at = if_not_exists(configured_at, :u)',
    ExpressionAttributeValues: {
      ':k': encrypt(apiKey),
      ':b': configuredBy,
      ':u': now,
    },
  }));
}

// Raw delete. No qurl-service subscription teardown. Today there is
// no production caller; a future /qurl unlink admin command MUST
// add an orchestrator that issues DELETE /v1/webhooks/{id} BEFORE
// calling this, or it'll orphan the subscription on qurl-service.
async function _removeGuildApiKeyRaw(guildId) {
  const res = await ddb.send(new DeleteCommand({
    TableName: TABLES.guild_configs,
    Key: { guild_id: guildId },
    ReturnValues: 'ALL_OLD',
  }));
  return !!res.Attributes;
}

async function getGuildConfig(guildId) {
  const res = await ddb.send(new GetCommand({
    TableName: TABLES.guild_configs,
    Key: { guild_id: guildId },
  }));
  const row = res.Item;
  if (!row) return row;
  // Strip the api key before returning — callers that need it use
  // getGuildConfigWithApiKey explicitly. Underscore-prefix on the
  // dropped field is the idiomatic "intentionally unused" marker
  // for eslint's no-unused-vars rule (configured to allow leading
  // `_`); avoids the `void` workaround.
  const { qurl_api_key: _drop, ...safe } = row;
  return safe;
}

async function getGuildConfigWithApiKey(guildId) {
  const res = await ddb.send(new GetCommand({
    TableName: TABLES.guild_configs,
    Key: { guild_id: guildId },
  }));
  const row = res.Item;
  if (!row) return row;
  return { ...row, qurl_api_key: row.qurl_api_key ? decrypt(row.qurl_api_key) : row.qurl_api_key };
}

// ── Per-guild qurl-service webhook subscriptions (BYOK view counter) ──

// webhook_secret stored encrypted (same envelope as qurl_api_key).
// webhook_id + webhook_owner_id stored plain — neither is a credential
// (webhook_id is a public-ish opaque identifier returned to API callers;
// webhook_owner_id is an auth0 sub already echoed in inbound webhook
// payloads). Keeping them plain avoids a decrypt() on every cache prime
// and lets ad-hoc DDB queries by owner work without round-tripping
// through the crypto module.
async function setGuildWebhookSubscription(guildId, { webhookId, webhookSecret, webhookOwnerId }) {
  if (!guildId || !webhookId || !webhookSecret || !webhookOwnerId) {
    throw new Error('setGuildWebhookSubscription: guildId, webhookId, webhookSecret, webhookOwnerId all required');
  }
  // ConditionExpression: only write webhook_* attributes onto a row
  // that ALREADY has a qurl_api_key. Without this guard, a future
  // caller-bug or race that ran setGuildWebhookSubscription before
  // setGuildApiKey would create an orphan guild_configs row with
  // webhook_* attrs but no api key — receiver would have a secret
  // but no link to the linking admin's identity. Mirrors the
  // attribute_exists pattern in propagateGuildWebhookSubscription.
  await ddb.send(new UpdateCommand({
    TableName: TABLES.guild_configs,
    Key: { guild_id: guildId },
    ConditionExpression: 'attribute_exists(qurl_api_key)',
    UpdateExpression: 'SET webhook_id = :wid, webhook_secret = :wsec, webhook_owner_id = :woid, updated_at = :u',
    ExpressionAttributeValues: {
      ':wid': webhookId,
      ':wsec': encrypt(webhookSecret),
      ':woid': webhookOwnerId,
      ':u': nowIso(),
    },
  }));
}

// REMOVE the three webhook_* attributes; leaves the API key row intact.
// Used when a guild rotates to a key whose owner_id differs from the
// previous one and the caller has already DELETE'd the old subscription
// (or accepted the orphan).
async function clearGuildWebhookSubscription(guildId) {
  await ddb.send(new UpdateCommand({
    TableName: TABLES.guild_configs,
    Key: { guild_id: guildId },
    UpdateExpression: 'REMOVE webhook_id, webhook_secret, webhook_owner_id SET updated_at = :u',
    ExpressionAttributeValues: { ':u': nowIso() },
  }));
}

// Reference-counting helper for the unlink path. Returns all guild_ids
// (including the caller's guild_id, if any) currently associated with
// the given webhook_owner_id. The caller decides whether to issue
// DELETE on qurl-service based on the count (don't kill sibling guilds
// that share the same auth0 admin's API key).
//
// ConsistentRead because the link-time caller (propagateGuildWebhookSubscription)
// runs IMMEDIATELY after a setGuildWebhookSubscription write — an
// eventually-consistent scan could miss the just-written primary row
// (or sibling rows from a concurrent link) and silently skip the
// propagation that fixes rotate-drift. RCU cost is acceptable on a
// low-cardinality table.
// TODO(#486): replace scanAll with a Query on the webhook_owner_id
// GSI when guild_configs > ~10k rows. Every `/qurl setup` and OAuth
// callback hits this via propagateGuildWebhookSubscription, so the
// link-path cost is O(table_size) per call — same fix as the
// 30s priming scan, single migration covers both.
async function listGuildSubscriptionsByOwner(webhookOwnerId) {
  const rows = await scanAll(TABLES.guild_configs, { consistentRead: true });
  return rows
    .filter(r => r.webhook_owner_id === webhookOwnerId && r.webhook_id)
    .map(r => ({ guildId: r.guild_id, webhookId: r.webhook_id }));
}

// Propagate (webhookId, webhookSecret) to every sibling guild row
// owned by the same webhookOwnerId. Called after
// ensureWebhookSubscription rotates the shared secret so sibling
// rows don't keep stale ciphertext that the cache tick could
// deterministically pick on Scan-order tiebreak.
//
// `excludeGuildId` (optional): skip this guild — the caller has
// already persisted it. Returns counts of rows updated/failed (the
// excluded guild is not counted).
async function propagateGuildWebhookSubscription(
  webhookOwnerId,
  { webhookId, webhookSecret, excludeGuildId },
) {
  if (!webhookOwnerId || !webhookId || !webhookSecret) {
    throw new Error('propagateGuildWebhookSubscription: webhookOwnerId, webhookId, webhookSecret all required');
  }
  const allMatches = await listGuildSubscriptionsByOwner(webhookOwnerId);
  // Common case for a first-time admin: only the just-written primary
  // row matches the owner. Short-circuit before the scan-filter pass.
  if (excludeGuildId && allMatches.length === 1 && allMatches[0].guildId === excludeGuildId) {
    return { updated: 0, failed: 0 };
  }
  const siblings = excludeGuildId
    ? allMatches.filter(s => s.guildId !== excludeGuildId)
    : allMatches;
  if (siblings.length === 0) return { updated: 0, failed: 0 };

  const updatedAt = nowIso();
  const encryptedSecret = encrypt(webhookSecret);
  const results = await Promise.allSettled(siblings.map((s) => ddb.send(new UpdateCommand({
    TableName: TABLES.guild_configs,
    Key: { guild_id: s.guildId },
    UpdateExpression: 'SET webhook_id = :wid, webhook_secret = :wsec, updated_at = :u',
    // Defense against a race where the row was cleared between
    // listGuildSubscriptionsByOwner and this write — never mint
    // subscription state on a row that opted out.
    ConditionExpression: 'attribute_exists(webhook_owner_id)',
    ExpressionAttributeValues: {
      ':wid': webhookId,
      ':wsec': encryptedSecret,
      ':u': updatedAt,
    },
  }))));

  let updated = 0;
  let failed = 0;
  for (const r of results) {
    if (r.status === 'fulfilled') { updated += 1; continue; }
    // ConditionalCheckFailedException = sibling cleared between
    // list and write; benign, not a failure.
    if (r.reason?.name === 'ConditionalCheckFailedException') continue;
    failed += 1;
  }
  return { updated, failed };
}

// Returns every guild_configs row with a provisioned webhook
// subscription, secret decrypted. Forwards `updatedAt` so the
// in-process cache can tiebreak sibling rows during rotation drift.
//
// Eventually-consistent on purpose: the receiver's
// `SIBLING_LAG_GRACE_MS` window upgrades any OWNER_UNKNOWN within
// REFRESH_INTERVAL_MS + grace to 503-retriable, so a sibling replica
// that hasn't seen DDB's replicated write yet does NOT 401 — the
// next 30s tick catches it. Strong consistency would double the
// per-tick RCU cost for a property the receiver already provides.
// `propagateGuildWebhookSubscription` is where strong consistency
// actually pays for itself (write-then-read same flow).
// Per-row decrypt-fail alarm-once tracker. A permanently-corrupt row
// would otherwise emit 2880 audits/day (30s tick × 2 replicas).
// Cleared per guildId on its next successful decrypt — so a row that
// recovers (operator re-encrypts via /qurl setup or backfill) resets
// and re-alarms if it breaks again.
const _decryptAlarmedGuilds = new Set();

async function scanGuildSubscriptions() {
  const rows = await scanAll(TABLES.guild_configs);
  const out = [];
  let provisionedCount = 0;
  let decryptFailCount = 0;
  for (const r of rows) {
    if (!r.webhook_id || !r.webhook_secret || !r.webhook_owner_id) continue;
    provisionedCount += 1;
    let webhookSecret;
    try {
      webhookSecret = decrypt(r.webhook_secret);
      // Successful decrypt — if this row was alarmed, clear so a
      // future re-break re-fires (we want "the row went bad again"
      // to page, not just "the row was ever bad").
      if (_decryptAlarmedGuilds.has(r.guild_id)) _decryptAlarmedGuilds.delete(r.guild_id);
    } catch (err) {
      // One corrupt row (key rotation gap, manual DDB tamper, partial
      // migration) must NOT abort the entire scan — the cache would
      // stay unprimed and every inbound webhook would 401. Log + emit
      // an audit so a CloudWatch metric-filter alarm can fire on
      // sustained decrypt failures (KMS key drift); backfill or
      // /qurl setup re-encrypts the row.
      //
      // Alarm-once per guild_id per "outage": the audit fires on the
      // FIRST decrypt failure for this row and stays silent until a
      // successful decrypt resets the tracker (see try-branch above).
      // The warn-log still fires every tick so per-row breakage stays
      // visible at log-stream granularity.
      logger.warn('scanGuildSubscriptions: decrypt failed for row, skipping', {
        guildId: r.guild_id, error: err.message,
      });
      if (!_decryptAlarmedGuilds.has(r.guild_id)) {
        logger.audit(AUDIT_EVENTS.QURL_WEBHOOK_CACHE_ROW_DECRYPT_FAIL, {
          guild_id: r.guild_id, error_type: err.name || 'unknown',
        });
        _decryptAlarmedGuilds.add(r.guild_id);
      }
      decryptFailCount += 1;
      continue;
    }
    out.push({
      guildId: r.guild_id,
      webhookId: r.webhook_id,
      webhookSecret,
      webhookOwnerId: r.webhook_owner_id,
      // ISO-8601 string OR undefined for rows pre-dating the
      // updated_at write. The cache treats `undefined` as "older
      // than any timestamped row" so a stale legacy row never beats
      // a freshly-written one in the tiebreak.
      updatedAt: r.updated_at,
    });
  }
  // Escalate when more than half of provisioned rows failed decrypt AND
  // we saw at least 3 (avoids alarm spam on a 1-row sandbox table that
  // tripped a single transient decrypt error). The per-row audit fires
  // identically whether 1 row or all rows fail; this distinct event
  // gives ops a metric filter that means "KMS-wide outage" vs "one bad
  // row." Skip when 0 rows are provisioned — division-by-zero guard
  // and a no-op the alarm doesn't need to see.
  // TODO: re-tune the 3-row floor at ≥20 BYOK guilds — at higher cardinality,
  //   ">50% of ≥3" is too sensitive (it would fire on a 5-of-9 partial
  //   tenant-key tier outage that's a real-but-not-mass event); switch to a
  //   percentile band + absolute floor (e.g. >50% AND ≥5) or a separate
  //   "tier-scoped" decrypt audit.
  if (provisionedCount >= 3 && decryptFailCount * 2 > provisionedCount) {
    logger.audit(AUDIT_EVENTS.QURL_WEBHOOK_CACHE_MASS_DECRYPT_FAIL, {
      failed: decryptFailCount, provisioned: provisionedCount,
    });
  }
  return out;
}

// ── Orphaned OAuth tokens ──

// Cheap "is the data layer functional" probe for /health. Single
// GetItem against a sentinel key on a low-cardinality table verifies
// SDK init, network path, IAM, and that the table exists, in one
// ~1-RCU round-trip. Throws if any of those are broken so the
// orchestrator replaces the container.
//
// Why not Scan/getStats(): /health is hit at LB cadence (10–30s).
// `getStats()` against DDB is a paginated full-table Scan. Using
// it at health-check cadence would
// scale RCU cost with table size and amplify cost-per-instance in
// any fleet. /metrics keeps the full aggregation — that's the right
// home for it.
async function healthCheck() {
  // Probe `guild_configs` rather than a higher-traffic table. It has
  // the lowest write rate (configured per guild, not per send), the
  // simplest key schema (just `guild_id` HASH), and exists in every
  // environment. GetItem on a missing key returns an empty result
  // (no exception),
  // so a sentinel `__healthcheck__` guild_id is safe and never collides
  // with a real Discord guild snowflake (numeric only).
  await ddb.send(new GetCommand({
    TableName: TABLES.guild_configs,
    Key: { guild_id: '__healthcheck__' },
  }));
  return { ok: true };
}

async function close() {
  // DDB client has no persistent connection to close; AWS SDK v3
  // manages sockets internally via keep-alive. The method exists
  // for Store contract parity (other backends may need real cleanup).
  logger.info('DDB store closed (no-op)');
}

module.exports = {
  // Stats
  getStats,
  // QURL sends
  recordQURLSend, recordQURLSendBatch, updateSendDMStatus, markSendDMDelivered,
  getRecentSends, markSendRevoked, isSendRevoked,
  saveSendConfig, getSendConfig, getSendResourceIds, getSendItems,
  findSendsByQurlId, markExpiredDMEdited, clearExpiredDMEdited,
  markConsumedDMEdited, clearConsumedDMEdited,
  // View-counter render state (cross-replica fast-path, PR-B)
  saveSendConfirmState,
  getSendRenderState, incrementSendViewedCount, getSendViewedCount,
  getSendRenderedCount, tryAdvanceRenderedCount, touchRenderedAt, tryClaimRenderAttempt, markConfirmTerminal,
  // QURL views (webhook-fed)
  recordQurlView, getQurlViews,
  // Guild configs
  getGuildApiKey, setGuildApiKey, _removeGuildApiKeyRaw, getGuildConfig, getGuildConfigWithApiKey,
  // Per-guild webhook subscriptions (BYOK view counter)
  setGuildWebhookSubscription, clearGuildWebhookSubscription,
  listGuildSubscriptionsByOwner, scanGuildSubscriptions, propagateGuildWebhookSubscription,
  // Lifecycle
  close, healthCheck,
  // Test-only: surface the prefixed table-name map so
  // `tests/provisioner-schema-parity.test.js` can pin parity with
  // `scripts/provision-ddb-local.js`. NOT part of the Store contract
  // — production callers must NOT reach into this; the leading
  // underscore signals intent. If you find yourself depending on
  // this outside tests, add a real public API instead.
  _TABLES_FOR_TESTING: TABLES,
  // Test-only: clear the per-row decrypt-alarm tracker so a suite
  // that asserts the alarm-fires-on-first-failure semantic starts
  // each test from a clean slate. Production callers MUST NOT use
  // this — production semantics depend on the tracker persisting
  // for the process lifetime.
  _resetDecryptAlarmedForTesting: () => _decryptAlarmedGuilds.clear(),
};
