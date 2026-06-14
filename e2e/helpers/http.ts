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
 *   - Retries only statuses where the request provably did NOT reach/complete at
 *     the app: 502/503 (ALB couldn't reach a healthy target — the drain-gap we
 *     target) plus 408/425/429 (rejected/timed-out before processing). This set
 *     is safe to retry even on the non-idempotent `/upload` POST: none of them can
 *     mean "the backend processed it but the response was lost," so a retry can't
 *     duplicate a resource. Deterministic 4xx (400/401/404/409) fail fast — real
 *     failures or test bugs.
 *   - Excludes 500 and 504 on purpose: 500 is an app-level error (a real failure,
 *     not a transient infra blip), and 504 (gateway timeout) can mean the backend
 *     DID process the request before the response was lost — retrying a POST would
 *     then duplicate the resource, and the orphan escapes the tests' afterAll
 *     cleanup (which only tracks the final success). The drain-gap is 502/503.
 *   - Excludes 403: on this path a 403 is a WAF-layer block (e.g. AWS managed
 *     IP-reputation flagging the CI runner's egress IP), which blocks the runner
 *     run-wide — an intra-run retry (same IP) can't recover it and would only
 *     delay the failure (tracked in qurl-integrations-infra#1091).
 *   - Does NOT catch fetch REJECTIONS (DNS / ECONNREFUSED / the sustained
 *     fileviewer.layerv.xyz:443 ConnectTimeout the ticket calls out). Those
 *     propagate immediately so a genuine outage fails fast.
 *   - Keeps the attempt budget bounded, so even a SUSTAINED 5xx eventually
 *     surfaces: the final Response is returned for the caller's own `!ok` throw.
 */

/**
 * Statuses worth a bounded retry on the connector/fileviewer serving path —
 * limited to ones where the request did NOT reach/complete at the app, so a retry
 * is safe even on a non-idempotent POST. See the module header for why 500/504,
 * 403, and the deterministic 4xx are absent. Module-private — the retry policy is
 * an implementation detail of the helper.
 */
const RETRYABLE_HTTP_STATUS: ReadonlySet<number> = new Set([
  408, // Request Timeout — request not fully received, so not processed
  425, // Too Early — TLS early-data replay guard; safe to replay
  429, // Too Many Requests — rejected before processing
  502, // Bad Gateway — ALB couldn't reach a healthy target (drain-gap)
  503, // Service Unavailable — ALB has no healthy target (the drain-gap)
]);

/**
 * `fetch()` with a bounded retry on transient HTTP statuses. Returns the final
 * `Response` (ok, non-retryable, or budget-exhausted) so the caller keeps its
 * own context-rich `!res.ok` error. Network rejections are NOT caught — they
 * propagate. The same `init` is reused across attempts, so any `body` must be
 * re-readable on resend (`FormData`/`Blob` are; a one-shot stream is not).
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
  let res = await fetch(input, init);
  for (
    let attempt = 1;
    attempt < maxAttempts && !res.ok && RETRYABLE_HTTP_STATUS.has(res.status);
    attempt++
  ) {
    // Release the discarded response's body so its socket returns to the pool
    // instead of lingering until GC (the 5xx body is never read).
    await res.body?.cancel().catch(() => {});
    // Drain-gaps / rolling deploys resolve in seconds, so a short linear backoff
    // is enough to clear the window without inflating a sustained-outage failure.
    await new Promise((r) => setTimeout(r, baseDelayMs * attempt));
    res = await fetch(input, init);
  }
  return res;
}
