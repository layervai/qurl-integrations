// qURL webhook self-registration
//
// Pre-#469 the operator had to register the bot's webhook subscription
// with qurl-service via curl. That step gated the entire view-counter
// feature and silently failed-open: bot deploys with no subscription
// looked healthy but the counter never advanced. The sandbox rollout
// of #465 burned ~20 minutes on this exact gap.
//
// Multi-replica safety (steady-state): an HTTP fleet of N replicas
// all booting together used to race on POST /v1/webhooks/{id}/secret —
// server-side last-write-wins meant (N-1) replicas held a stale
// secret in their in-memory config.QURL_WEBHOOK_SECRET, and the
// ALB-routed traffic across all of them 401'd on ~(N-1)/N of
// inbound webhooks until eventual restart. Fix in this rev: rotate
// the secret ONLY if (a) no subscription exists yet, or (b) the
// existing one matches our URL but the caller can't supply a
// known-good secret to verify against (initialSecret unset/empty).
// Steady-state restarts find (existing sub + real SSM secret) and
// skip the rotate — every replica reads the same secret from SSM/env
// and stays in lock-step.
//
// Cold-bootstrap race (open): on the very first deploy to a fresh
// environment, no subscription exists yet AND no SSM secret exists
// yet, so all N replicas independently call POST /v1/webhooks →
// create N duplicate subscriptions, each with a distinct server-
// generated secret. SSM's PutParameter is last-write-wins so SSM
// converges to one replica's secret; the other N-1 replicas hold
// dead-secret subscriptions that 401 inbound deliveries forever.
// `dedupeSubscriptions` below is RECOVERY (next boot cleans up the
// duplicates) — it does NOT prevent the create-race on the first
// boot. The durable fix is upstream: qurl-service making POST
// /v1/webhooks idempotent on (owner_id, url). Until then, runbook
// pins the first deploy of a fresh environment to a single-replica
// rollout — see docs/qurl-webhook-rollout.md.
//
// Boot ordering note: the HTTP listener opens BEFORE the registrar
// resolves, so a webhook that arrives in the brief startup window
// hits the receiver before config.QURL_WEBHOOK_SECRET is updated.
// Result: 503 (if env didn't have a secret) or 401 (if env held a
// stale value). qurl-service retries both. The fire-and-forget call
// pattern in index.js owns this window; we don't mask it here.
//
// Owner-scope: subscriptions are tied to the owner_id of the API
// key used to create them. The bot uses QURL_API_KEY (same key it
// uses to mint qURLs), so the subscription receives events for the
// qURLs the bot creates — matching what /qurl send users care about.

const logger = require('./logger');
const { QURL_WEBHOOK_EVENTS } = require('./constants');

const QURL_ACCESSED = QURL_WEBHOOK_EVENTS.ACCESSED;
const QURL_EXPIRED = QURL_WEBHOOK_EVENTS.EXPIRED;

// Wire-protocol event set every subscription this bot owns MUST carry.
// Kept as the source of truth for create + reconcile so the two paths
// can never drift to different event lists. Order is irrelevant here;
// reconcile uses set comparison.
const TARGET_EVENTS = Object.freeze([QURL_ACCESSED, QURL_EXPIRED]);

// Strip secret-shaped fields anywhere in a parsed body before
// stringifying. Error messages echo response bodies into CloudWatch;
// defense-in-depth scrubbing avoids the secret leaking via a logger
// error. Walks objects + arrays recursively because qurl-service's
// error envelope shape isn't pinned — `data.webhook.secret`,
// `data[0].secret`, or `error.detail.secret` would all bypass a
// top-level-only redactor.
//
// Match policy: any key containing "secret" (case-insensitive), so
// `webhook_secret`, `signing_secret`, `secret_key`, `client_secret`
// all redact too. Receiver wouldn't accept those field names anyway —
// this is purely a log-leak guard.
const REDACT_MAX_DEPTH = 8;
const SECRET_KEY_RE = /secret/i;
function redactSecret(body, depth = 0) {
  // Fail-closed at the depth cap: return a marker instead of the raw
  // subtree, so a deeply-wrapped secret can't survive the truncation.
  // 8 levels is comfortably past any qurl-service error envelope shape
  // observed; deeper than that is anomalous and warrants visibility.
  if (depth > REDACT_MAX_DEPTH) return '[TRUNCATED]';
  if (!body || typeof body !== 'object') return body;
  if (Array.isArray(body)) return body.map(v => redactSecret(v, depth + 1));
  const out = {};
  for (const [k, v] of Object.entries(body)) {
    out[k] = SECRET_KEY_RE.test(k) ? '[REDACTED]' : redactSecret(v, depth + 1);
  }
  return out;
}

class QurlServiceError extends Error {
  constructor(status, body, op) {
    const safe = redactSecret(body);
    super(`qurl-service ${op} returned ${status}: ${typeof safe === 'string' ? safe : JSON.stringify(safe).slice(0, 400)}`);
    this.status = status;
    this.body = safe;
    this.op = op;
  }
}

async function callQurlService({ method, path, apiEndpoint, apiKey, body, timeoutMs = 10_000 }) {
  const op = `${method} ${path}`;
  const opts = {
    method,
    headers: { Authorization: `Bearer ${apiKey}` },
    signal: AbortSignal.timeout(timeoutMs),
  };
  if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  let resp;
  try {
    resp = await fetch(`${apiEndpoint}${path}`, opts);
  } catch (err) {
    // fetch can reject before we have a response (AbortError on
    // timeout, DNS failure, TLS handshake error). Re-throw with `op`
    // attached so oncall greps for `op=GET /v1/webhooks` catch
    // network errors too, not just HTTP-status errors.
    err.op = op;
    throw err;
  }
  const text = await resp.text();
  let parsed = text;
  try { parsed = text ? JSON.parse(text) : null; } catch { /* keep as text */ }
  if (!resp.ok) throw new QurlServiceError(resp.status, parsed, op);
  return parsed;
}

// Canonicalize URL for comparison — strict equality (`s.url === bridgeUrl`)
// misses trailing-slash / default-port / host-case drift. URL().href
// round-trip handles HOST case (lowercased) + default ports (:80/:443
// elided). PATHNAME case is intentionally NOT folded — Express and
// qurl-service both route case-sensitively, so `/Webhooks/qurl` and
// `/webhooks/qurl` are genuinely different routes. QUERY STRINGS are
// preserved as-is (not normalized) — a sub registered with `?ver=1`
// is a different route than the unparametrized one. We do strip a
// trailing slash from the pathname (URL().href preserves it), since
// `/webhooks/qurl` and `/webhooks/qurl/` hit the same Express handler.
// Fall back to the raw string on a parse error so a future malformed
// URL doesn't crash boot.
function canonicalUrl(u) {
  try {
    const url = new URL(u);
    if (url.pathname.length > 1 && url.pathname.endsWith('/')) {
      url.pathname = url.pathname.slice(0, -1);
    }
    return url.href;
  } catch { return u; }
}

// Returns ALL subscriptions matching our URL (typically 0 or 1; >1
// only after a cold-bootstrap race created duplicates — see the
// dedupe path in ensureWebhookSubscription). Paginates so a caller
// with many subs doesn't lose the match on page 2. Page size 100
// requested explicitly; loop bounded at 50 iterations (5000 subs)
// so a misbehaving cursor can't spin forever.
//
// Also collects URL-migration orphan candidates in the same pass when
// `orphanFilter` is provided — a sub passes the filter iff it's a
// stale sibling of `target` at a different host (rename leftover).
// Doing both in one cursor walk avoids the second full-scan we'd
// otherwise pay on the create-fresh path. Callers that don't need the
// orphan list (per-guild bootstrap) pass no filter and only see matches.
//
// Two terminal states to distinguish:
//   - natural exhaustion (cursor walk ends, no more pages) → returns
//     {matches, orphans}, possibly empty. Caller takes create-fresh on
//     empty matches.
//   - 50-page cap hit (cursor never ends) → THROWS, refuses to fall
//     through to create-fresh which would silently compound
//     duplicates on every restart.
async function findExistingSubscriptions({ apiEndpoint, apiKey, bridgeUrl, orphanFilter }) {
  const target = canonicalUrl(bridgeUrl);
  const matches = [];
  const orphans = [];
  let cursor = '';
  for (let i = 0; i < 50; i++) {
    // Explicit limit=100 (vs relying on a server default that could
    // silently change). Combined with the 50-page cap, bounds the
    // total walk at 5000 subs.
    const qs = cursor ? `?cursor=${encodeURIComponent(cursor)}&limit=100` : '?limit=100';
    const path = `/v1/webhooks${qs}`;
    const resp = await callQurlService({ method: 'GET', path, apiEndpoint, apiKey });
    // Array.isArray guard — a future contract drift returning
    // `data: {...}` would otherwise iterate object property values in
    // a confusing way and silently treat them as subscription objects.
    const subs = Array.isArray(resp?.data) ? resp.data : [];
    for (const s of subs) {
      if (canonicalUrl(s.url) === target) matches.push(s);
      else if (orphanFilter && orphanFilter(s)) orphans.push(s);
    }
    const next = resp && resp.meta && resp.meta.next_cursor;
    if (!next) return { matches, orphans };
    cursor = next;
  }
  throw new Error('findExistingSubscriptions: pagination cap hit (50 pages, ~5000 subs); possible stuck cursor — refusing to fall through to create-fresh which would compound duplicates');
}

async function deleteSubscription({ apiEndpoint, apiKey, webhookId }) {
  try {
    await callQurlService({
      method: 'DELETE',
      path: `/v1/webhooks/${webhookId}`,
      apiEndpoint,
      apiKey,
    });
  } catch (err) {
    // 404 = another replica deleted it concurrently. Treat as
    // success — the goal state ("this duplicate is gone") is met.
    if (err.status !== 404) throw err;
  }
}

// Derive the stable description prefix from the bot's current description.
// `description` shape today: `Discord bot view counter (region=..., env=...)`
// (central registrar) or `Discord bot view counter (guild=..., ...)`
// (per-guild link). Everything in parens varies with env/region/guild;
// everything BEFORE the first ` (` is the stable identity of "subs this
// bot creates". Extract that prefix so the URL-migration orphan sweep
// only matches the bot's own historical subs, never a sibling service's.
//
// Falls back to the whole string if no ` (` present, so a caller passing
// a description without parens still gets a sensible match (string-exact).
function deriveDescriptionPrefix(description) {
  if (typeof description !== 'string' || description.length === 0) return '';
  const idx = description.indexOf(' (');
  return idx === -1 ? description : description.slice(0, idx);
}

// Pull the host from a URL string. Returns null on parse failure so the
// caller can skip the row defensively (same fallback shape as canonicalUrl).
function urlHost(u) {
  try { return new URL(u).host.toLowerCase(); } catch { return null; }
}

// Pull the pathname from a URL string with trailing-slash normalization
// (mirrors canonicalUrl semantics so `/webhooks/qurl` and `/webhooks/qurl/`
// compare equal). Returns null on parse failure.
function urlPathname(u) {
  try {
    const url = new URL(u);
    let p = url.pathname;
    if (p.length > 1 && p.endsWith('/')) p = p.slice(0, -1);
    return p;
  } catch { return null; }
}

// URL-migration orphan predicate.
//
// Symptom (sandbox, 2026-06): the bot's `base_url` was renamed from
// `discord.layerv.xyz` → `discord.connector.layerv.xyz`. The registrar's
// canonical-URL matcher correctly found ZERO subs at the new URL, so it
// CREATED a fresh subscription. The OLD subscription stayed alive in
// qurl-service — its secret unchanged, qurl-service kept trying to
// deliver to the old host (which still resolved to the same ALB), every
// delivery failed sig-verification at the bot (the bot now reads the
// NEW sub's secret from SSM). Net: 1475 failed deliveries to a permanent
// orphan + cluttered subscription list.
//
// Cleanup criteria (conservative — false-positive deletion of someone
// else's sub is far worse than a missed orphan):
//   1. URL pathname matches the bot's bridge URL pathname (same route)
//   2. URL host DIFFERS from the bot's current bridge URL host
//      (cross-host = candidate for rename)
//   3. Description STARTS WITH the bot's stable description prefix
//      (e.g. `Discord bot view counter`) — this is the safety net that
//      keeps connector subs (description: `qurl-s3-connector ...`) out
//      of the deletion set even though they share owner_id with the bot
//      under today's bot-API-key-shared model.
//
// Returned as a closure so it composes into `findExistingSubscriptions`
// as the `orphanFilter` arg: one cursor walk produces both the matches
// AND the orphan candidates, avoiding a second full-scan on every
// create-fresh boot. Returns null when no safe prefix can be derived —
// callers treat that as "skip the sweep".
function buildUrlMigrationOrphanFilter({ bridgeUrl, descriptionPrefix }) {
  if (!descriptionPrefix) return null;
  const targetHost = urlHost(bridgeUrl);
  const targetPath = urlPathname(bridgeUrl);
  if (!targetHost || !targetPath) return null;
  return function isUrlMigrationOrphan(s) {
    const subHost = urlHost(s.url);
    const subPath = urlPathname(s.url);
    if (subHost === null || subPath === null) return false;
    if (subPath !== targetPath) return false;
    if (subHost === targetHost) return false;
    const subDesc = typeof s.description === 'string' ? s.description : '';
    return subDesc.startsWith(descriptionPrefix);
  };
}

// Best-effort DELETE for each URL-migration orphan. A 5xx on one orphan
// logs an error and continues to the next + to the normal create path.
// Next invocation retries the orphan delete idempotently. We deliberately
// do NOT propagate the failure: blocking the create on a stale-orphan
// delete failure would leave the bot UN-registered (no new sub) AND
// still-orphaned (old sub undeleted) — strictly worse than the orphan-
// only state we started in.
//
// Sequential DELETEs (vs parallel like the dedupe path) — orphan counts
// are typically 0-1 per migration, and serial gives the cleanest
// per-orphan failure isolation in the logs.
async function cleanupUrlMigrationOrphans({ apiEndpoint, apiKey, orphans }) {
  for (const orphan of orphans) {
    try {
      await deleteSubscription({ apiEndpoint, apiKey, webhookId: orphan.webhook_id });
      logger.info('URL-migration orphan deleted', {
        old_url: orphan.url,
        webhook_id: orphan.webhook_id,
        description: orphan.description,
        failure_count: orphan.failure_count,
        last_delivery_success: orphan.last_delivery_success,
      });
    } catch (err) {
      logger.error('URL-migration orphan delete failed (continuing — next invocation retries)', {
        old_url: orphan.url,
        webhook_id: orphan.webhook_id,
        error: err.message,
        status: err.status,
        op: err.op,
      });
    }
  }
}

// Pick a deterministic survivor across replicas so two replicas racing
// on dedupe converge on the same one without coordination. Oldest-by-
// created_at is the natural choice (first-to-exist wins); fall back to
// lexicographic webhook_id when qurl-service omits the timestamp. Both
// keys are independent of replica identity, so both replicas pick the
// same survivor and both call DELETE on the same set of duplicates
// (404-on-second is handled in `deleteSubscription`).
function pickSurvivor(matches) {
  if (matches.length === 0) return null;
  if (matches.length === 1) return matches[0];
  const sorted = [...matches].sort((a, b) => {
    // Asymmetric timestamps: the row with a timestamp wins (it carries
    // more information than the row without). Only fall through to lex
    // when both lack or both share the same timestamp.
    if (a.created_at && !b.created_at) return -1;
    if (!a.created_at && b.created_at) return 1;
    if (a.created_at && b.created_at && a.created_at !== b.created_at) {
      return a.created_at < b.created_at ? -1 : 1;
    }
    return (a.webhook_id || '') < (b.webhook_id || '') ? -1 : 1;
  });
  return sorted[0];
}

// Max chars for the human-readable description in qurl-service. Sliced
// at the wire boundary (here) rather than at the caller so future
// callers don't have to remember the cap. The 200 is defense against
// a hypothetical future server-side cap that would otherwise 4xx the
// create into an infinite retry-create loop — qurl-service doesn't
// document one today.
const DESCRIPTION_MAX_LEN = 200;

// Note: `description` is set only at create time and is not reconciled
// on subsequent boots. Region / NODE_ENV captured in the description
// string can go stale after env rename / region migration; that's
// observability-only (qurl-service UI label) and not worth a PATCH
// on every boot.
async function createSubscription({ apiEndpoint, apiKey, bridgeUrl, description }) {
  const safeDescription = typeof description === 'string' ? description.slice(0, DESCRIPTION_MAX_LEN) : '';
  const resp = await callQurlService({
    method: 'POST',
    path: '/v1/webhooks',
    apiEndpoint,
    apiKey,
    body: {
      url: bridgeUrl,
      events: [...TARGET_EVENTS],
      description: safeDescription,
    },
  });
  // Defensive: if a future contract drift returns {data: null} or
  // omits the fields, accessing .webhook_id/.secret would throw a
  // raw TypeError with no context. Surface the contract violation
  // explicitly so the boot-log error is greppable.
  const data = resp?.data;
  // length > 0 too — an empty-string secret would let the receiver
  // verify against a zero-length HMAC key (any signature against ''
  // produces the same digest), trivially bypassable.
  if (!data
      || typeof data.webhook_id !== 'string' || data.webhook_id.length === 0
      || typeof data.secret !== 'string' || data.secret.length === 0) {
    throw new Error('createSubscription: contract drift (response missing or empty webhook_id/secret)');
  }
  return data;
}

async function rotateSecret({ apiEndpoint, apiKey, webhookId }) {
  const resp = await callQurlService({
    method: 'POST',
    path: `/v1/webhooks/${webhookId}/secret`,
    apiEndpoint,
    apiKey,
  });
  const data = resp?.data;
  if (!data || typeof data.secret !== 'string' || data.secret.length === 0) {
    throw new Error('rotateSecret: contract drift (response missing or empty secret)');
  }
  return data;
}

async function patchEvents({ apiEndpoint, apiKey, webhookId, events }) {
  await callQurlService({
    method: 'PATCH',
    path: `/v1/webhooks/${webhookId}`,
    apiEndpoint,
    apiKey,
    body: { events },
  });
}

// Best-effort: don't throw on failure. Secret persistence is
// observability, not load-bearing. The caller decides the
// persistence backend (SSM today, could be DDB / Secrets Manager
// later) via the persistSecret callback.
async function bestEffortPersist({ persistSecret, value }) {
  if (typeof persistSecret !== 'function') return;
  try {
    await persistSecret(value);
    logger.info('qURL webhook secret persisted');
  } catch (err) {
    const level = err.name === 'AccessDeniedException' ? 'warn' : 'error';
    // Cap err.message length — if a future SDK ever echoes the
    // attempted-write value into a validation error, an unbounded log
    // line would leak the secret to CloudWatch. 200 chars is enough
    // for typical SDK errors ("User is not authorized to perform: ...")
    // and short enough that a multi-KB leak would be visibly truncated.
    const safeMsg = typeof err.message === 'string' ? err.message.slice(0, 200) : '';
    logger[level]('qURL webhook secret persistence failed (auto-register continues with in-memory secret only)', {
      error: safeMsg,
      code: err.name,
    });
  }
}

// Reconcile the events list on an existing subscription so it exactly
// covers TARGET_EVENTS. PATCH is idempotent so two replicas racing here
// is harmless. Factored out because the reuse path and the rotate path
// both need it.
//
// Set-based comparison (not `.includes(ACCESSED)`): the pre-EXPIRED
// version of this function would short-circuit as soon as `accessed`
// was present, silently leaving an `accessed`-only subscription in
// place even when the target set had grown to include `expired`. Any
// subsequent additions to TARGET_EVENTS would suffer the same drift.
// Symmetric-difference logic ensures the subscription's event list
// matches TARGET_EVENTS exactly, so dropping the inclusion-check
// short-circuit is intentional.
async function reconcileEvents({ apiEndpoint, apiKey, existing }) {
  // Array.isArray guard — a future contract drift returning
  // `events: "qurl.accessed,qurl.expired"` (string) would otherwise
  // pass a `.includes(...)` style check via string-contains and skip
  // the PATCH despite drift. Treat any non-array as missing.
  const current = Array.isArray(existing?.events) ? existing.events : [];
  const currentSet = new Set(current);
  const targetSet = new Set(TARGET_EVENTS);
  const sameSize = currentSet.size === targetSet.size;
  const sameMembers = sameSize && [...targetSet].every(e => currentSet.has(e));
  if (sameMembers) return;
  logger.info('qURL webhook subscription events drift — PATCHing to TARGET_EVENTS', {
    webhookId: existing.webhook_id,
    current,
    target: [...TARGET_EVENTS],
  });
  await patchEvents({ apiEndpoint, apiKey, webhookId: existing.webhook_id, events: [...TARGET_EVENTS] });
}

/**
 * Ensure a qurl.accessed subscription exists for the bot's bridge URL.
 * Returns the secret to use for HMAC verification.
 *
 * @param {object} opts
 * @param {string} opts.apiEndpoint    - qurl-service base URL
 * @param {string} opts.apiKey         - bot's QURL_API_KEY
 * @param {string} opts.bridgeUrl      - bot's own /webhooks/qurl URL
 * @param {string} opts.description    - human-readable; surfaces in qurl-service UI
 * @param {string} [opts.initialSecret] - the secret the caller already has in-memory (e.g. from SSM/env). When set to a non-placeholder value AND an existing subscription is found, the registrar SKIPS rotation — every replica reuses the same secret instead of rotating each other into uselessness.
 * @param {Function} [opts.persistSecret] - optional async(secret) → void callback for best-effort persistence
 * @returns {Promise<{secret: string, webhookId: string, action: 'created' | 'rotated' | 'reused', ownerId: string}>}
 *   `ownerId` is the qurl-service auth0 owner the API key resolves to. Required
 *   by per-guild callers for receiver routing.
 */
async function ensureWebhookSubscription(opts) {
  const { apiEndpoint, apiKey, bridgeUrl, description, initialSecret, persistSecret } = opts;
  if (!apiEndpoint || !apiKey || !bridgeUrl) {
    throw new Error('ensureWebhookSubscription: apiEndpoint, apiKey, bridgeUrl all required');
  }

  // Find all subs matching our URL. Normally 0 or 1. >1 means a prior
  // cold-bootstrap created duplicates — RECOVER by keeping the
  // deterministic survivor + deleting the rest. Both replicas independently
  // pick the same survivor (oldest by created_at) so concurrent dedupe
  // converges without coordination.
  //
  // Co-collect URL-migration orphan candidates in the same cursor walk —
  // subs at the same `/webhooks/qurl` path but a DIFFERENT host whose
  // description starts with this bot's stable prefix. Picked up here so
  // the create-fresh branch below doesn't pay a second full scan. See
  // buildUrlMigrationOrphanFilter for the safety criteria.
  const orphanFilter = buildUrlMigrationOrphanFilter({
    bridgeUrl,
    descriptionPrefix: deriveDescriptionPrefix(description),
  });
  const { matches, orphans } = await findExistingSubscriptions({
    apiEndpoint, apiKey, bridgeUrl, orphanFilter,
  });
  const existing = pickSurvivor(matches);
  const wasDedupe = matches.length > 1 && existing != null;
  if (wasDedupe) {
    const losers = matches.filter(s => s.webhook_id !== existing.webhook_id);
    logger.warn('qURL webhook subscription duplicates detected — deleting non-survivors', {
      total: matches.length,
      survivor: existing.webhook_id,
      losers: losers.map(s => s.webhook_id),
      url: bridgeUrl,
    });
    // Parallel — each DELETE is independent + the 404-on-concurrent-
    // delete path is already swallowed per-loser, so concurrency is
    // safe. N=2-5 in practice (replica count); sequential would add
    // ~100-200ms per loser on the recovery boot. allSettled (vs all)
    // so a non-404 rejection on one sibling doesn't leave siblings'
    // promises unhandled — we collect every rejection, then throw the
    // first one explicitly so the call site sees a single greppable
    // failure with clean stack trace.
    const results = await Promise.allSettled(losers.map(loser =>
      deleteSubscription({ apiEndpoint, apiKey, webhookId: loser.webhook_id }),
    ));
    const firstReject = results.find(r => r.status === 'rejected');
    if (firstReject) throw firstReject.reason;
  }

  let webhookId;
  let secret;
  let action;

  // Skip-rotation guard: an existing subscription PLUS a known-good
  // initial secret means we're in steady-state — any other replica
  // already created the sub and put the secret in SSM/env, and we
  // can trust both. Rotating here would split-brain the fleet.
  //
  // EXCEPT after dedupe: the SSM secret may belong to a replica's POST
  // that we just DELETEd (cold-bootstrap created N subs each with a
  // distinct server-generated secret; SSM is last-write-wins, so the
  // surviving sub's secret is almost certainly NOT the one in SSM).
  // Force a rotate so SSM gets a known-good value tied to the
  // survivor. One-time cost on dedupe; subsequent restarts reuse.
  const initialIsRealSecret = typeof initialSecret === 'string'
    && initialSecret.length > 0;

  // Forwarded so per-guild callers can route inbound webhooks
  // without a second GET /v1/webhooks.
  let ownerId;

  if (existing && initialIsRealSecret && !wasDedupe) {
    // Trust assumption: the SSM-loaded `initialSecret` matches the
    // existing sub's server-side secret. No challenge/verify here.
    // If SSM ever desyncs from qurl-service (partial backup restore,
    // manual SSM edit, missed rotation step), the receiver 401s
    // every webhook. Operator recovery: clear SSM, re-invoke Lambda
    // → bootstrap-rotate path lands a known-good shared secret.
    webhookId = existing.webhook_id;
    secret = initialSecret;
    action = WEBHOOK_ACTIONS.REUSED;
    ownerId = existing.owner_id;
    // Wrap the PATCH in try/catch matching the rotate branch — a
    // transient 5xx here shouldn't flip the boot log to "self-
    // registration failed" when the in-memory secret is already
    // correct (initialSecret). Events drift is recoverable on the
    // next boot via the same code path.
    try {
      await reconcileEvents({ apiEndpoint, apiKey, existing });
    } catch (err) {
      logger.error('qURL webhook subscription events PATCH failed in reuse path (continuing — receiver still works with initialSecret)', {
        webhookId, op: 'reconcileEvents', error: err.message, status: err.status,
      });
    }
    logger.info('qURL webhook subscription reused (existing found, SSM secret trusted, no rotate)', { webhookId, url: bridgeUrl });
    return { secret, webhookId, action, ownerId };
  }

  if (existing) {
    // Bootstrap path: subscription exists but we don't have a usable
    // secret in-memory (initialSecret empty or unset). Rotate FIRST
    // so the boot can succeed even if the events PATCH fails — a
    // stuck PATCH shouldn't block secret recovery.
    webhookId = existing.webhook_id;
    ownerId = existing.owner_id;
    const rotated = await rotateSecret({ apiEndpoint, apiKey, webhookId });
    secret = rotated.secret;
    action = WEBHOOK_ACTIONS.ROTATED;
    try {
      await reconcileEvents({ apiEndpoint, apiKey, existing });
    } catch (err) {
      // Log + continue: rotation already landed, so the bot is
      // functionally healthy. Events drift can be reconciled on the
      // next boot or via a manual PATCH.
      logger.error('qURL webhook subscription events PATCH failed after rotate (continuing — rotation succeeded)', {
        webhookId, op: 'reconcileEvents', error: err.message, status: err.status,
      });
    }
    logger.info('qURL webhook subscription reconciled (existing found, bootstrap rotate)', { webhookId, url: bridgeUrl });
  } else {
    // No existing sub at the requested URL. Before creating a fresh one,
    // best-effort DELETE the URL-migration orphans we picked up during
    // the cursor walk above — prior subs this bot registered at a
    // DIFFERENT host (e.g. `base_url` rename) on the same `/webhooks/qurl`
    // path. Without this, a rename leaves the old sub alive in
    // qurl-service forever, retrying deliveries that the bot can no
    // longer verify. See buildUrlMigrationOrphanFilter for the safety
    // criteria (description-prefix match keeps sibling-service subs out
    // of scope).
    if (orphans.length > 0) {
      await cleanupUrlMigrationOrphans({ apiEndpoint, apiKey, orphans });
    }
    const created = await createSubscription({ apiEndpoint, apiKey, bridgeUrl, description });
    webhookId = created.webhook_id;
    secret = created.secret;
    action = WEBHOOK_ACTIONS.CREATED;
    ownerId = created.owner_id;
    logger.info('qURL webhook subscription created', { webhookId, url: bridgeUrl });
  }

  // Note: no `if (!secret)` guard here — `createSubscription` and
  // `rotateSecret` both validate `typeof data.secret === 'string'`
  // and throw their own contract-drift error before returning. A
  // second check at this point would be unreachable.

  await bestEffortPersist({ persistSecret, value: secret });

  return { secret, webhookId, action, ownerId };
}

// Build a persistSecret callback that writes the rotated secret to an
// SSM SecureString parameter with a 5s timeout. Extracted from
// index.js so the timeout-placement (abortSignal on send's second arg,
// NOT on the Command constructor — the constructor silently drops
// extra args and defeats the timeout) is greppable and pinnable in
// tests. Callers inject the SDK module + client to keep this file
// SDK-import-free (so unit tests don't have to mock SSM SDK eagerly).
function buildSsmPersistSecret({ ssmClient, paramName, PutParameterCommand, timeoutMs = 5_000 }) {
  return async (secret) => {
    await ssmClient.send(
      new PutParameterCommand({
        Name: paramName,
        Type: 'SecureString',
        Value: secret,
        Overwrite: true,
      }),
      { abortSignal: AbortSignal.timeout(timeoutMs) },
    );
  };
}

// Frozen enum mirrors the LINK_RESULTS / VERIFY_RESULTS pattern in
// sibling modules — a typo in any assignment site fails as
// `WEBHOOK_ACTIONS.UNDEFINED_THING` at require time, not as a
// silent string-comparison miss in a future caller.
const WEBHOOK_ACTIONS = Object.freeze({
  CREATED: 'created',
  ROTATED: 'rotated',
  REUSED: 'reused',
});

module.exports = {
  ensureWebhookSubscription,
  deleteSubscription,
  buildSsmPersistSecret,
  WEBHOOK_ACTIONS,
  // Exposed for webhook-subscriptions.js so the registry's
  // discoverDefaultOwnerId tick goes through the same QurlServiceError /
  // op-tagged transport as the rest of the registrar surface — kept off
  // _internals because it has a stable contract and an external caller.
  callQurlService,
  _internals: {
    canonicalUrl,
    pickSurvivor,
    redactSecret,
    QurlServiceError,
    deriveDescriptionPrefix,
    buildUrlMigrationOrphanFilter,
    cleanupUrlMigrationOrphans,
  },
};
