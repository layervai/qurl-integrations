// qURL webhook self-registration
//
// Pre-#469 the operator had to register the bot's webhook subscription
// with qurl-service via curl. That step gated the entire view-counter
// feature and silently failed-open: bot deploys with no subscription
// looked healthy but the counter never advanced. The sandbox rollout
// of #465 burned ~20 minutes on this exact gap.
//
// Multi-replica safety: an HTTP fleet of N replicas all booting
// together used to race on POST /v1/webhooks/{id}/secret —
// server-side last-write-wins meant (N-1) replicas held a stale
// secret in their in-memory config.QURL_WEBHOOK_SECRET, and the
// ALB-routed traffic across all of them 401'd on ~(N-1)/N of
// inbound webhooks until eventual restart. Fix in this rev: rotate
// the secret ONLY if (a) no subscription exists yet, or (b) the
// existing one matches our URL but the caller can't supply a
// known-good secret to verify against (initialSecret unset / set
// to the terraform-seeded PLACEHOLDER). Steady-state restarts find
// (existing sub + real SSM secret) and skip the rotate — every
// replica reads the same secret from SSM/env and stays in lock-step.
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

const QURL_ACCESSED = 'qurl.accessed';

// `PLACEHOLDER` is the terraform-seeded sentinel value for the
// QURL_WEBHOOK_SECRET SSM parameter — before any registrar run has
// landed a real secret, the parameter holds this literal. Treat it
// as "no real secret yet" so the bootstrap path can land a fresh
// rotation without permanently re-rotating every restart afterward.
const PLACEHOLDER_SECRET = 'PLACEHOLDER';

// Strip the `secret` field from a parsed body before stringifying.
// Error messages echo response bodies into CloudWatch; defense-in-
// depth scrubbing avoids the secret leaking via a logger error.
function redactSecret(body) {
  if (!body || typeof body !== 'object') return body;
  const clone = Array.isArray(body) ? [...body] : { ...body };
  if (clone.data && typeof clone.data === 'object' && 'secret' in clone.data) {
    clone.data = { ...clone.data, secret: '[REDACTED]' };
  }
  if ('secret' in clone) clone.secret = '[REDACTED]';
  return clone;
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
  const opts = {
    method,
    headers: { Authorization: `Bearer ${apiKey}` },
    signal: AbortSignal.timeout(timeoutMs),
  };
  if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  const resp = await fetch(`${apiEndpoint}${path}`, opts);
  const text = await resp.text();
  let parsed = text;
  try { parsed = text ? JSON.parse(text) : null; } catch { /* keep as text */ }
  if (!resp.ok) throw new QurlServiceError(resp.status, parsed, `${method} ${path}`);
  return parsed;
}

// Canonicalize URL for comparison — strict equality (`s.url === bridgeUrl`)
// misses trailing-slash / default-port / case-difference drift. Using
// URL().href round-trip handles case + default-port. We also strip
// any trailing slash from the pathname (URL().href preserves it),
// since `/webhooks/qurl` and `/webhooks/qurl/` are equivalent
// receivers for our routing. Fall back to the raw string on a parse
// error so a future malformed URL doesn't crash boot.
function canonicalUrl(u) {
  try {
    const url = new URL(u);
    if (url.pathname.length > 1 && url.pathname.endsWith('/')) {
      url.pathname = url.pathname.slice(0, -1);
    }
    return url.href;
  } catch { return u; }
}

// Returns the existing subscription for our URL, or null. Paginates so a
// caller with many subs doesn't lose the match on page 2. Page size of
// 100 is the qurl-service default cap; loop bounded at 50 iterations
// (5000 subs) so a misbehaving cursor can't spin forever.
async function findExistingSubscription({ apiEndpoint, apiKey, bridgeUrl }) {
  const target = canonicalUrl(bridgeUrl);
  let cursor = '';
  for (let i = 0; i < 50; i++) {
    const path = `/v1/webhooks${cursor ? `?cursor=${encodeURIComponent(cursor)}` : ''}`;
    const resp = await callQurlService({ method: 'GET', path, apiEndpoint, apiKey });
    const subs = (resp && resp.data) || [];
    const match = subs.find(s => canonicalUrl(s.url) === target);
    if (match) return match;
    const next = resp && resp.meta && resp.meta.next_cursor;
    if (!next) return null;
    cursor = next;
  }
  logger.warn('findExistingSubscription: pagination cap hit (50 pages); possible stuck cursor');
  return null;
}

async function createSubscription({ apiEndpoint, apiKey, bridgeUrl, description }) {
  const resp = await callQurlService({
    method: 'POST',
    path: '/v1/webhooks',
    apiEndpoint,
    apiKey,
    body: {
      url: bridgeUrl,
      events: [QURL_ACCESSED],
      description,
    },
  });
  return resp && resp.data;
}

async function rotateSecret({ apiEndpoint, apiKey, webhookId }) {
  const resp = await callQurlService({
    method: 'POST',
    path: `/v1/webhooks/${webhookId}/secret`,
    apiEndpoint,
    apiKey,
  });
  return resp && resp.data;
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
    logger[level]('Webhook secret persistence failed (auto-register continues with in-memory secret only)', {
      error: err.message,
      code: err.name,
    });
  }
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
 * @returns {Promise<{secret: string, webhookId: string, action: 'created' | 'rotated' | 'reused'}>}
 */
async function ensureWebhookSubscription(opts) {
  const { apiEndpoint, apiKey, bridgeUrl, description, initialSecret, persistSecret } = opts;
  if (!apiEndpoint || !apiKey || !bridgeUrl) {
    throw new Error('ensureWebhookSubscription: apiEndpoint, apiKey, bridgeUrl all required');
  }

  const existing = await findExistingSubscription({ apiEndpoint, apiKey, bridgeUrl });
  let webhookId;
  let secret;
  let action;

  // Skip-rotation guard: an existing subscription PLUS a known-good
  // initial secret means we're in steady-state — any other replica
  // already created the sub and put the secret in SSM/env, and we
  // can trust both. Rotating here would split-brain the fleet.
  const initialIsRealSecret = typeof initialSecret === 'string'
    && initialSecret.length > 0
    && initialSecret !== PLACEHOLDER_SECRET;

  if (existing && initialIsRealSecret) {
    webhookId = existing.webhook_id;
    secret = initialSecret;
    action = 'reused';
    // Reconcile events list if drifted — PATCH is idempotent so two
    // replicas racing here is harmless.
    const eventsMatch = Array.isArray(existing.events) && existing.events.includes(QURL_ACCESSED);
    if (!eventsMatch) {
      logger.info('Webhook subscription events drift — PATCHing to include qurl.accessed', { webhookId, current: existing.events });
      await patchEvents({ apiEndpoint, apiKey, webhookId, events: [QURL_ACCESSED] });
    }
    logger.info('Webhook subscription reused (existing found, SSM secret trusted, no rotate)', { webhookId, url: bridgeUrl });
    return { secret, webhookId, action };
  }

  if (existing) {
    // Bootstrap path: subscription exists but we don't have a usable
    // secret in-memory (placeholder or empty). Rotate to recover.
    // Multi-replica race here is bounded — qurl-service is last-
    // write-wins, and the steady-state guard above means this branch
    // only fires until SSM has a real secret. After the first
    // successful persist, subsequent restarts hit the reuse path.
    webhookId = existing.webhook_id;
    const eventsMatch = Array.isArray(existing.events) && existing.events.includes(QURL_ACCESSED);
    if (!eventsMatch) {
      logger.info('Webhook subscription events drift — PATCHing to include qurl.accessed', { webhookId, current: existing.events });
      await patchEvents({ apiEndpoint, apiKey, webhookId, events: [QURL_ACCESSED] });
    }
    const rotated = await rotateSecret({ apiEndpoint, apiKey, webhookId });
    secret = rotated.secret;
    action = 'rotated';
    logger.info('Webhook subscription reconciled (existing found, bootstrap rotate)', { webhookId, url: bridgeUrl });
  } else {
    const created = await createSubscription({ apiEndpoint, apiKey, bridgeUrl, description });
    webhookId = created.webhook_id;
    secret = created.secret;
    action = 'created';
    logger.info('Webhook subscription created', { webhookId, url: bridgeUrl });
  }

  if (!secret) {
    throw new Error(`Webhook subscription ${action} but no secret in response (qurl-service contract drift?)`);
  }

  await bestEffortPersist({ persistSecret, value: secret });

  return { secret, webhookId, action };
}

module.exports = {
  ensureWebhookSubscription,
  PLACEHOLDER_SECRET,
  // Exported for tests:
  _internals: {
    findExistingSubscription,
    createSubscription,
    rotateSecret,
    patchEvents,
    bestEffortPersist,
    canonicalUrl,
    redactSecret,
    QurlServiceError,
  },
};
