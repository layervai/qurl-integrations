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
 *         503-on-POST guarantee is contingent on the connector never emitting an
 *         app-level 503 *after* partially processing. Both POST endpoints honor
 *         that: `/upload` signals rate-limits via HTTP 200 + `error` (never a
 *         503), and `/api/mint_link` DOES emit one app-level 503 — its pre-bake
 *         "render-at-mint not ready" guard — but that fires BEFORE any bake, so a
 *         retry still can't duplicate a `views/<mint-id>` object (its post-bake
 *         failures surface as 502, which POSTs don't retry). TODO(upstream-contract):
 *         this mirrors the qurl-s3-connector mint/upload contract (handler.go —
 *         pre-bake 503 guard + 502 default) — if either endpoint ever returns a
 *         503 AFTER partially processing, move 503 to the idempotent-only set below.
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
 *   - Does NOT honor `Retry-After` by default: a caller opts in with its own
 *     `maxRetryAfterMs` budget. The reasoning for that — which 503s are worth
 *     waiting out and why only a call site can tell — is stated once on that
 *     param below, and is the canonical copy.
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
 * @param maxRetryAfterMs OPT-IN ceiling for honoring a 503's `Retry-After`; 0
 *   (default) ignores the header entirely. It is both the enable flag and the
 *   cap, so a small value means "cap there", not "off" — pass 0 to disable.
 *   Opt-in rather than always-on because a `Retry-After` is only worth waiting
 *   out when the condition is CONTINGENT, and this stack emits both kinds under
 *   the SAME `service_unavailable` code: qurl-service's revocation-pending 30
 *   (transient) and its deployment-state "dark 503" 60 (standing — waiting only
 *   makes a permanent failure slower to report). Nothing THIS HELPER INSPECTS
 *   separates them — their bodies do differ, but parsing one is #1505's job —
 *   so only the call site can. Everyone else keeps the 1s/2s backoff.
 *
 *   The ceiling is the longest directive the caller will honor, PER ATTEMPT, so
 *   the opted-in worst case is `(maxAttempts - 1) x ceiling` — assuming every
 *   retried response carries a directive AT the ceiling; a shorter one, or a
 *   declined one costs less. The caller owns both numbers together;
 *   `revokeLink` sets both and exports their product so the live budgets derive
 *   from it rather than restating it.
 *
 *   A LONGER directive is DECLINED rather than clamped to the ceiling —
 *   re-asking early would
 *   draw the same response, just later — and that attempt falls back to the
 *   ordinary local backoff. So opting in never costs a caller a retry it would
 *   have had at the default; too small a ceiling only stops directives being
 *   honored, it does not reduce resilience below the default.
 *
 *   Scoped to 503 even when opted in: a 429 directive on this stack means "you
 *   burst", and honoring it would let one shed DELETE cost 35s inside the
 *   serial cleanup sweeps that budget ~2s each (concurrency.test.ts's 180s
 *   afterAll over ~60 resources). The local backoff plus each sweep's own
 *   pacing already covers those.
 */
export async function fetchWithTransientRetry(
  input: string | URL,
  init?: RequestInit,
  { maxAttempts = 3, baseDelayMs = 1000, maxRetryAfterMs = 0 }:
    { maxAttempts?: number; baseDelayMs?: number; maxRetryAfterMs?: number } = {},
): Promise<Response> {
  // Clamp once here so the loop never has to defend against a negative ceiling.
  const retryAfterCeilingMs = Math.max(0, maxRetryAfterMs);
  const method = (init?.method ?? 'GET').toUpperCase();
  let res = await fetch(input, init);
  for (
    let attempt = 1;
    attempt < maxAttempts && !res.ok && isRetryableStatus(res.status, method);
    attempt++
  ) {
    // `Retry-After` wins only when it asks for LONGER than the local backoff —
    // a server asking us to slow down is authoritative, one asking us to hurry
    // is not. 503-only and opt-in: see `maxRetryAfterMs` above.
    // TODO(upstream-contract): qurl-service emits the delta-seconds form only.
    // Anything non-numeric (including the HTTP-date form RFC 9110 also allows)
    // falls through to the linear backoff rather than producing a NaN delay.
    // No `.trim()`: the Headers API normalizes leading/trailing whitespace, so
    // `get()` never returns a padded value. The padded-directive test in
    // unit/http.test.ts exercises the Headers CONSTRUCTOR; on the wire llhttp
    // strips OWS before it gets here, so both paths are covered but only the
    // former is pinned.
    const retryAfterRaw = res.status === 503 ? res.headers.get('retry-after') ?? '' : '';
    const honorsDirective = retryAfterCeilingMs > 0 && /^\d+$/.test(retryAfterRaw);
    const directiveMs = honorsDirective ? Number(retryAfterRaw) * 1000 : 0;
    // Surface every retry decision in CI logs so a run that RECOVERED after a
    // blip doesn't look identical to one that never blipped — the drain-gap
    // signal #1085 wants — and so a run that STOPPED says why. Log the ORIGIN
    // only, not the full URL: the fileviewer `/view/<mint-id>` path carries the
    // capability mint-id, which must not land in CI logs.
    let origin: string;
    try {
      origin = new URL(input).origin;
    } catch {
      origin = '<url>'; // non-absolute input: don't throw, don't leak
    }
    // Over the ceiling: decline the directive and fall back to the local
    // backoff — never drop the retry. See `maxRetryAfterMs` above for why.
    const overCeiling = directiveMs > retryAfterCeilingMs;
    // Opted in, got a 503, and no directive to honor — so this attempt falls
    // back to the local backoff. Note this is BROADER than "the confirm window
    // was skipped": a gateway 503 in front of the service carries no
    // `Retry-After` either, and has nothing to do with a protection update. The
    // logged text says only what is true (no usable directive, local backoff
    // only) and leaves the cause to the reader.
    const degraded = retryAfterCeilingMs > 0 && res.status === 503 && !honorsDirective;
    const delayMs = overCeiling
      ? baseDelayMs * attempt
      : Math.max(baseDelayMs * attempt, directiveMs);
    // One level for every retry decision, with a grep-able token instead of an
    // escalation: the degraded predicate also matches the ALB drain-gap 503 —
    // no `Retry-After`, and this module's founding scenario — so raising its
    // level would fire on the benign case and erode the signal.
    console.warn(
      `[fetchWithTransientRetry] ${method} ${origin} -> ${res.status}; ` +
        `retry ${attempt}/${maxAttempts - 1} in ${delayMs}ms` +
        // Folded into the same line rather than emitted as a second warn: the
        // module's logging contract is one grep-able line per retry decision.
        // The no-directive note matters as much as the declined one: a caller
        // that opted in and got a 503 WITHOUT a usable `Retry-After` silently
        // falls back to the 1s backoff, retries well inside the window it meant
        // to wait out, and reds with a line identical to an ordinary drain-gap
        // retry. Saying so is what makes that degradation visible in CI.
        (overCeiling
          // Echo the raw header value too, so the CI line matches the wire.
          ? ` (declined Retry-After: ${retryAfterRaw.slice(0, 64)} = ${directiveMs}ms, ` +
            `over the ${retryAfterCeilingMs}ms ceiling)`
          : degraded
            // Bounded + quoted: unlike the declined branch this value never
            // passed /^\d+$/, so it is arbitrary header bytes landing in
            // retained CI logs. Headers can't carry CR/LF, but they can carry
            // kilobytes of control characters.
            ? ` (no usable Retry-After${retryAfterRaw ? `: ${JSON.stringify(retryAfterRaw.slice(0, 64))}` : ''}` +
              '; local backoff only)'
            : ''),
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
