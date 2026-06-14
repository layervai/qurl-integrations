/**
 * Shared HTTP helper for the E2E smoke: a BOUNDED retry on transient HTTP
 * responses from the connector-stack serving path (connector `/upload` +
 * fileviewer `/view`).
 *
 * Why this exists: the connector post-deploy smoke runs WHILE the connector-v2
 * ECS rollout is still in flight — the infra `terraform` apply has no
 * wait_for_steady_state, so the smoke races the rollout
 * (qurl-integrations-infra#1085). A brief rolling/drain window can serve a 5xx
 * from the ALB before the replacement task is healthy, which previously
 * hard-failed the smoke on the very first response (a false red).
 *
 * What it deliberately does NOT do, so it can never MASK a real outage:
 *   - Retries are METHOD-AWARE so a non-idempotent POST (`/upload`) can never be
 *     retried into a duplicate resource:
 *       · ANY method retries {408, 425, 429, 503} — statuses where the request
 *         provably did NOT reach/complete at the app (503 = ALB has no healthy
 *         target, the drain-gap; 408/425/429 = rejected/timed-out before
 *         processing), so a retry can't duplicate work even on a POST. NOTE the
 *         503-on-POST guarantee is contingent on the connector signaling
 *         rate-limits via HTTP 200 + `error` (not an app-level 503), so a 503
 *         here is always the ALB no-healthy-target case, never a post-accept app
 *         503. If the connector ever returns a real app-level 503 after partially
 *         processing, move 503 to the idempotent-only set below.
 *       · IDEMPOTENT methods (GET/HEAD/…, e.g. the `/view` read) ALSO retry
 *         {502, 504} — transient gateway failures where the backend MAY have
 *         already processed the request before the response was lost. Retrying
 *         those is safe on a GET but would risk a duplicate on a POST, so they
 *         are excluded for non-idempotent methods.
 *   - Excludes 500 everywhere: an app-level error is a real failure, not a
 *     transient infra blip — it should fail fast.
 *   - Excludes 403: on this path a 403 is a WAF-layer block (e.g. AWS managed
 *     IP-reputation flagging the CI runner's egress IP), which blocks the runner
 *     run-wide — an intra-run retry (same IP) can't recover it and would only
 *     delay the failure (tracked in qurl-integrations-infra#1091).
 *   - Deterministic 4xx (400/401/404/409) fail fast — real failures or test bugs.
 *   - Does NOT catch fetch REJECTIONS (DNS / ECONNREFUSED / the sustained
 *     fileviewer.layerv.xyz:443 ConnectTimeout the ticket calls out). Those
 *     propagate immediately so a genuine outage fails fast.
 *   - Keeps the attempt budget bounded, so even a SUSTAINED 5xx eventually
 *     surfaces: the final Response is returned for the caller's own `!ok` throw.
 */

// Retryable on ANY method — the request provably did not reach/complete at the
// app, so a retry is safe even on the non-idempotent `/upload` POST.
const RETRYABLE_ANY_METHOD: ReadonlySet<number> = new Set([
  408, // Request Timeout — request not fully received, so not processed
  425, // Too Early — TLS early-data replay guard; safe to replay
  429, // Too Many Requests — rejected before processing
  503, // Service Unavailable — ALB has no healthy target (the drain-gap)
]);

// Retryable ONLY for idempotent methods: transient gateway failures where the
// backend MAY have processed the request before the response was lost — safe to
// replay on a GET, but would risk a duplicate resource on a POST.
const RETRYABLE_IDEMPOTENT_ONLY: ReadonlySet<number> = new Set([
  502, // Bad Gateway — target accepted then closed / returned malformed
  504, // Gateway Timeout — target may have processed before the timeout
]);

// Per RFC 9110 §9.2.2: these methods are idempotent (safe to replay); POST and
// PATCH are not. `fetch()` defaults to GET when no method is given.
const IDEMPOTENT_METHODS: ReadonlySet<string> = new Set([
  'GET',
  'HEAD',
  'OPTIONS',
  'PUT',
  'DELETE',
  'TRACE',
]);

function isRetryableStatus(status: number, method: string): boolean {
  if (RETRYABLE_ANY_METHOD.has(status)) return true;
  return IDEMPOTENT_METHODS.has(method) && RETRYABLE_IDEMPOTENT_ONLY.has(status);
}

/**
 * `fetch()` with a bounded retry on transient HTTP statuses (method-aware — see
 * the module header). Returns the final `Response` (ok, non-retryable, or
 * budget-exhausted) so the caller keeps its own context-rich `!res.ok` error.
 * Network rejections are NOT caught — they propagate. The same `init` is reused
 * across attempts, so any `body` must be re-readable on resend (`FormData`/`Blob`
 * are; a one-shot stream is not).
 *
 * @param maxAttempts total attempts including the first (default 3)
 * @param baseDelayMs linear backoff base — waits `baseDelayMs * attempt` between
 *   tries, i.e. 1s then 2s at the default (well under jest's 120s timeout)
 */
export async function fetchWithTransientRetry(
  input: string | URL,
  init?: RequestInit,
  { maxAttempts = 3, baseDelayMs = 1000 }: { maxAttempts?: number; baseDelayMs?: number } = {},
): Promise<Response> {
  const method = (init?.method ?? 'GET').toUpperCase();
  let res = await fetch(input, init);
  for (
    let attempt = 1;
    attempt < maxAttempts && !res.ok && isRetryableStatus(res.status, method);
    attempt++
  ) {
    const delayMs = baseDelayMs * attempt;
    // Surface the retry in CI logs so a run that RECOVERED after a blip doesn't
    // look identical to one that never blipped — the drain-gap signal #1085 wants.
    // Log the ORIGIN only, not the full URL: the fileviewer `/view/<mint-id>` path
    // carries the capability mint-id, which must not land in CI logs.
    let origin: string;
    try {
      origin = new URL(input).origin;
    } catch {
      origin = '<url>'; // non-absolute input: don't throw, don't leak
    }
    console.warn(
      `[fetchWithTransientRetry] ${method} ${origin} -> ${res.status}; ` +
        `retry ${attempt}/${maxAttempts - 1} in ${delayMs}ms`,
    );
    // Release the discarded response's body so its socket returns to the pool
    // instead of lingering until GC (the 5xx body is never read).
    await res.body?.cancel().catch(() => {});
    // Drain-gaps / rolling deploys resolve in seconds, so a short linear backoff
    // is enough to clear the window without inflating a sustained-outage failure.
    await new Promise((r) => setTimeout(r, delayMs));
    res = await fetch(input, init);
  }
  return res;
}
