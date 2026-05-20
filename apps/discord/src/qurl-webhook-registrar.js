// qURL webhook self-registration
//
// Pre-#469 the operator had to register the bot's webhook subscription
// with qurl-service via curl. That step gated the entire view-counter
// feature and silently failed-open: bot deploys with no subscription
// looked healthy but the counter never advanced. The sandbox rollout
// of #465 burned ~20 minutes on this exact gap.
//
// Now: bot self-registers (or reconciles + rotates) on every boot.
// The secret returned by qurl-service POST/regenerate lives in
// process memory; we attempt a best-effort SSM put so an oncall
// running `get-parameter` can see the current secret, but the SSM
// write is NOT load-bearing for correctness — if PutParameter fails
// (e.g. missing IAM grant), the in-memory secret is what the
// receiver verifies against and the SSM value is stale until the
// next boot succeeds.
//
// Subscription lifecycle invariant: at any moment there is at most
// one subscription per (owner_id, bot URL) pair. On boot the bot
// finds the existing one (if any) and rotates its secret; otherwise
// creates fresh. Duplicates from concurrent boots across an HTTP
// fleet are possible but bounded — the cleanup is a manual
// `gh issue` for now (rare; not load-bearing).
//
// Owner-scope: subscriptions are tied to the owner_id of the API
// key used to create them. The bot uses QURL_API_KEY (same key it
// uses to mint qURLs), so the subscription receives events for the
// qURLs the bot creates — matching what /qurl send users care about.

const logger = require('./logger');

const QURL_ACCESSED = 'qurl.accessed';

class QurlServiceError extends Error {
  constructor(status, body, op) {
    super(`qurl-service ${op} returned ${status}: ${typeof body === 'string' ? body : JSON.stringify(body).slice(0, 400)}`);
    this.status = status;
    this.body = body;
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

// Returns the existing subscription for our URL, or null.
async function findExistingSubscription({ apiEndpoint, apiKey, bridgeUrl }) {
  const resp = await callQurlService({
    method: 'GET',
    path: '/v1/webhooks',
    apiEndpoint,
    apiKey,
  });
  const subs = (resp && resp.data) || [];
  return subs.find(s => s.url === bridgeUrl) || null;
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
// later) via the persistSecret callback. If it throws, we just
// log + continue — the in-memory secret returned to the caller
// is what the receiver verifies against.
async function bestEffortPersist({ persistSecret, value }) {
  if (typeof persistSecret !== 'function') return;
  try {
    await persistSecret(value);
    logger.info('qURL webhook secret persisted');
  } catch (err) {
    // AccessDenied is the expected case before the IAM grant lands —
    // log at warn rather than error so it's visible but not alarming.
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
 * @param {string} opts.apiEndpoint    - qurl-service base URL (config.QURL_ENDPOINT)
 * @param {string} opts.apiKey         - bot's QURL_API_KEY
 * @param {string} opts.bridgeUrl      - bot's own /webhooks/qurl URL (BASE_URL + '/webhooks/qurl')
 * @param {string} opts.description    - human-readable; surfaces in qurl-service UI
 * @param {Function} [opts.persistSecret] - optional async(secret) → void callback for best-effort persistence (e.g. SSM PutParameter). Failures are caught and logged but do NOT propagate — the in-memory secret remains usable.
 * @returns {Promise<{secret: string, webhookId: string, action: 'created' | 'rotated'}>}
 */
async function ensureWebhookSubscription(opts) {
  const { apiEndpoint, apiKey, bridgeUrl, description, persistSecret } = opts;
  if (!apiEndpoint || !apiKey || !bridgeUrl) {
    throw new Error('ensureWebhookSubscription: apiEndpoint, apiKey, bridgeUrl all required');
  }

  const existing = await findExistingSubscription({ apiEndpoint, apiKey, bridgeUrl });
  let webhookId;
  let secret;
  let action;

  if (existing) {
    webhookId = existing.webhook_id;
    // Reconcile events list if drifted.
    const eventsMatch = Array.isArray(existing.events) && existing.events.includes(QURL_ACCESSED);
    if (!eventsMatch) {
      logger.info('Webhook subscription events drift — PATCHing to include qurl.accessed', { webhookId, current: existing.events });
      await patchEvents({ apiEndpoint, apiKey, webhookId, events: [QURL_ACCESSED] });
    }
    // Rotate the secret on every boot. We can't recover the existing
    // secret via GET (qurl-service returns it only on create/rotate),
    // so a fresh boot has to rotate to get into a known state. The
    // small in-flight window where qurl-service has the new secret
    // but a delivery in flight signed with the old will fail
    // signature check — qurl-service retries those, so the worst
    // case is a single late retry per boot. Acceptable.
    const rotated = await rotateSecret({ apiEndpoint, apiKey, webhookId });
    secret = rotated.secret;
    action = 'rotated';
    logger.info('Webhook subscription reconciled (existing found, secret rotated)', { webhookId, url: bridgeUrl });
  } else {
    const created = await createSubscription({ apiEndpoint, apiKey, bridgeUrl, description });
    webhookId = created.webhook_id;
    secret = created.secret;
    action = 'created';
    logger.info('Webhook subscription created', { webhookId, url: bridgeUrl });
  }

  if (!secret) {
    // The server's response shape requires `secret` on create/rotate.
    // If it's missing the contract has drifted — fail loudly rather
    // than continue with an empty secret that would 401 every event.
    throw new Error(`Webhook subscription ${action} but no secret in response (qurl-service contract drift?)`);
  }

  await bestEffortPersist({ persistSecret, value: secret });

  return { secret, webhookId, action };
}

module.exports = {
  ensureWebhookSubscription,
  // Exported for tests:
  _internals: {
    findExistingSubscription,
    createSubscription,
    rotateSecret,
    patchEvents,
    bestEffortPersist,
    QurlServiceError,
  },
};
