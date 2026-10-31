/**
 * Headless-browser viewer for the fileviewer reverse-tunnel (render-at-mint).
 *
 * WHY a browser and not `fetch`/curl: infra repo PR #1111
 * decommissioned the legacy EC2 fileviewer host (fileviewer.layerv.xyz). Under
 * render-at-mint + the NHP tunnel a recipient NEVER hits a fetchable view URL
 * directly. The flow is entirely JS-driven:
 *
 *   1. The recipient opens the minted `qurl_link`. The capability lives in the
 *      signed URL FRAGMENT (`#qv2t1.…`), which the server never sees — only the SPA's
 *      JS reads it.
 *   2. The SPA POSTs the token to the resolve endpoint — the NHP "knock" — which
 *      `302`s the browser to the per-recipient tunnel view at
 *      `https://r_<id>.qurl.site…/views/<mint-id>`.
 *
 * `node fetch`/curl can't do step 1 (no JS to read the fragment), so they only
 * ever get the static SPA shell — never the tunnel view. A real browser can,
 * hence Playwright chromium, headless. The arrival of the `…/views/<id>` response
 * is the end-to-end signal: it proves the browser completed the knock AND the
 * tunnel served the view.
 *
 * Each `qurl_link` is ONE-TIME-USE (consumed on first view), so every view needs
 * its own fresh mint+link — we launch and close a fresh browser per call so no
 * cookie/storage state leaks between independent mints.
 *
 * Proven against sandbox (2026-06-15): `302 resolve.qurl.link…/plugins/qurl`
 * (the knock) → `200 r_<id>.qurl.site…/views/<mint-id>` (the tunnel view).
 */

import { chromium, type Browser, type APIRequestContext } from 'playwright';
import { spawnSync } from 'node:child_process';
import { appendFileSync, accessSync, constants } from 'node:fs';
import { createHash, randomUUID } from 'node:crypto';
import { fetchWithTransientRetry } from './http';

/** Matches the tunnel view URL on ANY environment:
 *   https://r_<id>.qurl.site<.layerv.xyz|.layerv.ai|…>/views/<mint-id>
 * Pinned to the `.qurl.site` host segment + the `/views/` path so it can't match
 * the `qurl.link` knock redirect or the top-frame SPA. Host-suffix-agnostic so
 * the same helper works against sandbox and prod. The FIRST response matching
 * this is the served view (asserted 200 below): the knock 302 lives on
 * `resolve.qurl.link`, not under `.qurl.site/views/`, so there's no intermediate
 * 3xx here to latch by mistake (verified against sandbox). If that redirect
 * topology ever changes, this would need to follow redirects to the terminal
 * response. */
const TUNNEL_VIEW_RE = /\.qurl\.site[^/]*\/views\//;

/** Pulls the `<mint-id>` (the per-recipient `views/<mint-id>` object key) out of a
 * resolved tunnel-view URL. The mint-id is hex(16 random bytes) = 32 lowercase hex
 * (connector `internal/handler/mintid.go`), DISTINCT per mint by construction — so
 * two views minted for the SAME upload resolve to DIFFERENT `views/<mint-id>`
 * objects, which IS the per-recipient-distinct-watermark guarantee (#1027: "distinct
 * watermarks (different `views/` objects)"). Returns undefined if the URL has no
 * `/views/<seg>` segment. Anchored on `/views/` and stops at the next `/` or `?` so
 * a trailing query/path can't bleed into the captured id. */
export function mintIdFromTunnelViewUrl(url: string): string | undefined {
  return /\/views\/([^/?#]+)/.exec(url)?.[1];
}

export interface ViewViaQurlLinkOptions {
  /** Browser launch headless? Defaults to true; set `PLAYWRIGHT_HEADED=1`
   *  (or `HEADLESS=0`) in the env to watch it run locally for debugging. */
  headless?: boolean;
  /** Require the sole iframe in the main document to be visible with this exact target in markup. */
  expectedIframeSrc?: string;
  /** Probe a known existing sibling through THIS session, without another knock. */
  siblingViewPath?: string;
  ownership: { resource_id: string; qurl_id: string; expires_at: string };
  /** Wall-clock budget for navigation + knock + response + optional iframe and scope checks, in ms.
   *  Default 30_000. */
  timeoutMs?: number;
}

export interface ViewViaQurlLinkResult {
  /** HTTP status of the tunnel-view response (200 means the view served). */
  status: number;
  /** The resolved tunnel-view URL the knock landed on
   *  (`https://r_<id>.qurl.site…/views/<mint-id>`). Lets a caller assert on the
   *  per-recipient `views/<mint-id>` object key (distinct-per-viewer, #1027) and on
   *  the `r_<id>` host label (DNS + wildcard-cert-via-SNI resolving an `_`-bearing
   *  label, end-to-end). Use `mintIdFromTunnelViewUrl` to extract the mint-id. */
  url: string;
}

function resolveHeadless(opt?: boolean): boolean {
  if (opt !== undefined) return opt;
  // Local-debug overrides; default headless for CI.
  return !(process.env.PLAYWRIGHT_HEADED === '1' || process.env.HEADLESS === '0');
}

/**
 * Open a minted `qurl_link` in a headless browser, let the SPA drive the NHP
 * knock, and resolve when the reverse-tunnel view response (`…/views/<id>`)
 * arrives — returning its HTTP status (200 = the view served) and the resolved
 * tunnel-view URL (`r_<id>.qurl.site…/views/<mint-id>`) for per-recipient/SNI
 * assertions.
 *
 * THROWS if no `…/views/<id>` response arrives within `timeoutMs` — the negative
 * signal a caller relies on (a revoked/consumed/expired link, or a tunnel that's
 * down, never serves the view).
 */
export async function viewViaQurlLink(
  qurlLink: string,
  opts: ViewViaQurlLinkOptions,
): Promise<ViewViaQurlLinkResult> {
  const timeoutMs = opts.timeoutMs ?? 30_000;
  let browser: Browser | undefined;
  try {
    browser = await chromium.launch({ headless: resolveHeadless(opts.headless) });
    const page = await (await browser.newContext()).newPage();

    // Wait on the tunnel-view RESPONSE, registered BEFORE goto so one that lands
    // mid-navigation isn't missed. Settle it into an always-resolving promise
    // (Response | undefined): goto and the waiter share timeoutMs, so if goto
    // rejects first the bare waiter would reject later with nothing awaiting it —
    // an unhandled rejection jest can misattribute to a later test. domcontentloaded,
    // NOT networkidle: the tunnel keepalive means networkidle never fires. The
    // predicate is URL-only — status is checked after, so a non-200 view throws
    // "returned N", not "no response".
    await recordAdmissionAttempt(qurlLink, opts.ownership, timeoutMs);
    const deadline = Date.now() + timeoutMs;
    const tunnelResponse = page
      .waitForResponse((r) => TUNNEL_VIEW_RE.test(r.url()), { timeout: timeoutMs })
      .catch(() => undefined);
    try {
      await page.goto(qurlLink, { waitUntil: 'domcontentloaded', timeout: timeoutMs });
    } catch {
      // Playwright errors can include the capability fragment in the URL.
      throw new Error('viewViaQurlLink: navigation failed');
    }

    const resp = await tunnelResponse;
    if (!resp) {
      throw new Error(
        `viewViaQurlLink: no tunnel-view response (…/views/<id>) within ${timeoutMs}ms — ` +
          `the knock did not resolve to a served view (link revoked/consumed/expired, or tunnel down).`,
      );
    }
    if (resp.status() !== 200) {
      throw new Error(`viewViaQurlLink: tunnel-view returned ${resp.status()} (expected 200).`);
    }
    if (opts.expectedIframeSrc !== undefined) {
      // TODO(upstream-contract): NHP portal bootstrap replaces the SPA with the native viewer.
      const iframe = page.locator('iframe');
      let actualSrc: string | null;
      try {
        if (resp.frame() !== page.mainFrame() || !resp.request().isNavigationRequest()) {
          throw new Error('recipient view is not a main-document navigation');
        }
        await page.waitForURL(TUNNEL_VIEW_RE, { waitUntil: 'domcontentloaded', timeout: Math.max(1, deadline - Date.now()) });
        await iframe.waitFor({ state: 'visible', timeout: Math.max(1, deadline - Date.now()) });
        actualSrc = await iframe.getAttribute('src', { timeout: Math.max(1, deadline - Date.now()) });
      } catch {
        // Locator errors can include an untrusted src with a capability token.
        throw new Error('viewViaQurlLink: no single visible iframe before the deadline');
      }
      if (actualSrc !== opts.expectedIframeSrc) {
        throw new Error('viewViaQurlLink: iframe target differs from the uploaded URL');
      }
    }
    if (opts.siblingViewPath !== undefined) {
      await probeScopedSibling(page.context().request, resp.url(), opts.siblingViewPath, deadline, (probe) => {
        appendFileSync(process.env.QURL_OWNERSHIP_RECEIPTS!, JSON.stringify({
          event: 'owned_scope_boundary_probe', resource_id: opts.ownership.resource_id,
          qurl_id: opts.ownership.qurl_id, observed_at: new Date().toISOString(),
          expected_target_path: new URL(resp.url()).pathname, sibling_target_path: opts.siblingViewPath,
          probe_status: probe.probeStatus, failure_code: probe.failureCode,
          own_before_status: probe.ownBefore, sibling_wire_status: probe.siblingWireStatus,
          sibling_wire_outcome: probe.siblingWireOutcome, own_after_status: probe.ownAfter,
          request_ids: probe.requestIDs,
          internal_authorize_403: 'pending_exact_request_id_readback_not_proved_by_wire_denial',
          persisted_target_path: 'pending_independent_native_state_readback',
        }) + '\n', { mode: 0o600 });
      });
    }
    return { status: resp.status(), url: resp.url() };
  } finally {
    // Always tear the browser down — a leaked chromium would hang jest's worker exit.
    await browser?.close();
  }
}

/** Persist before navigation, including attempts with no reply. This is ownership
 * evidence for the existing bounded collector, not proof of admission or CLOSED. */
export async function recordAdmissionAttempt(link: string, child: ViewViaQurlLinkOptions['ownership'], timeoutMs: number): Promise<void> {
  const verifier = process.env.QURL_OWNERSHIP_VERIFIER;
  const path = process.env.QURL_OWNERSHIP_RECEIPTS;
  const api = process.env.MINT_API_URL;
  const key = process.env.QURL_API_KEY;
  if (!verifier || !path || !api || !key || !child.resource_id || !child.qurl_id || !Number.isFinite(Date.parse(child.expires_at))) {
    throw new Error('owned admission receipt configuration is incomplete');
  }
  const result = spawnSync(verifier, [], { input: link, encoding: 'utf8', timeout: 10_000, maxBuffer: 16_384 });
  if (result.error || result.status !== 0) throw new Error('signed ownership verification failed');
  const identity = JSON.parse(result.stdout) as Record<string, string>;
  if (!identity.agent_public_key || !identity.resource_public_key_b64 || !identity.cell_public_key_b64) {
    throw new Error('verified public identity is incomplete');
  }
  const owner = await readOwner();
  const now = new Date();
  appendFileSync(path, JSON.stringify({
    event: 'owned_admission_attempt', owner_id: owner, resource_id: child.resource_id,
    qurl_id: child.qurl_id, expires_at: child.expires_at, observed_at: now.toISOString(),
    attempt_deadline: new Date(now.getTime() + timeoutMs).toISOString(),
    public_identity: identity, catalog_binding: 'pending_independent_readback',
    child_binding: 'pending_independent_readback',
    // TODO(upstream-contract): NHP sessionControlAgentSessionPK hashes the standard-base64 STRING.
    agent_membership_pk: 'AGENT#' + createHash('sha256').update(identity.agent_public_key).digest('hex'),
  }) + '\n', { mode: 0o600 });
}

// TODO(upstream-contract): qurl-service GET /v1/me returns data.owner_id.
async function readOwner(): Promise<string> {
  const meURL = new URL('/v1/me', process.env.MINT_API_URL!);
  const response = await fetchWithTransientRetry(meURL.toString(), { headers: { Authorization: `Bearer ${process.env.QURL_API_KEY}` } });
  if (!response.ok) throw new Error(`ownership identity read failed: ${response.status}`);
  const me = await response.json() as { data?: { owner_id?: string } };
  const owner = me.data?.owner_id;
  if (!owner?.trim()) throw new Error('ownership identity missing owner_id');
  return owner;
}

/** Fail before fixture creation if runner wiring, public trust or identity is unavailable. */
export async function checkOwnershipConfig(): Promise<void> {
  for (const name of ['QURL_OWNERSHIP_VERIFIER', 'QURL_OWNERSHIP_RECEIPTS', 'QURL_PUBLIC_CONFIG_URL', 'MINT_API_URL', 'QURL_API_KEY']) {
    if (!process.env[name]) throw new Error(`required ownership receipt configuration missing: ${name}`);
  }
  accessSync(process.env.QURL_OWNERSHIP_VERIFIER!, constants.X_OK);
  appendFileSync(process.env.QURL_OWNERSHIP_RECEIPTS!, '', { mode: 0o600 });
  const result = spawnSync(process.env.QURL_OWNERSHIP_VERIFIER!, ['--check-config'], { encoding: 'utf8', timeout: 10_000, maxBuffer: 16_384 });
  if (result.error || result.status !== 0) throw new Error('public ownership configuration failed');
  await readOwner();
}

export interface ScopedSiblingProbeReceipt {
  probeStatus: 'PASS' | 'FAILED';
  failureCode: string | null;
  ownBefore: number | null;
  siblingWireStatus: number | null;
  siblingWireOutcome: string | null;
  ownAfter: number | null;
  requestIDs: { ownBefore: string; sibling: string; ownAfter: string };
}

/** Reuse the browser context's existing cookie on one admitted origin.
 * The final callback runs once on success or failure, before the error escapes. */
export async function probeScopedSibling(
  request: APIRequestContext,
  ownViewURL: string,
  siblingPath: string,
  deadline = Date.now() + 30_000,
  onFinal?: (receipt: ScopedSiblingProbeReceipt) => void,
): Promise<ScopedSiblingProbeReceipt> {
  const prefix = 'scope-' + randomUUID();
  const receipt: ScopedSiblingProbeReceipt = {
    probeStatus: 'FAILED',
    failureCode: null,
    ownBefore: null,
    siblingWireStatus: null,
    siblingWireOutcome: null,
    ownAfter: null,
    requestIDs: {
      ownBefore: prefix + '-pre',
      sibling: prefix + '-sib',
      ownAfter: prefix + '-post',
    },
  };
  let failureMessage: string | undefined;
  function fail(code: string, message: string): never {
    receipt.failureCode = code;
    failureMessage = message;
    throw new Error(message);
  }
  function remaining() {
    const left = Math.floor(deadline - Date.now());
    if (!Number.isFinite(left) || left <= 0)
      fail('DEADLINE_EXHAUSTED', 'scope probe deadline exhausted');
    return left;
  }
  try {
    const own = new URL(ownViewURL);
    // Reviewed domains only; adding a suffix requires source review.
    if (
      own.protocol !== 'https:' ||
      !/^q-[a-z2-7]{52}\.qurl\.site(?:\.layerv\.(?:xyz|ai))?$/.test(own.hostname) ||
      !/^\/views\/[0-9a-f]{32}$/.test(own.pathname) ||
      own.search ||
      own.hash ||
      own.username ||
      own.password ||
      !/^\/views\/[0-9a-f]{32}$/.test(siblingPath) ||
      siblingPath === own.pathname
    )
      fail('INVALID_TARGET', 'scope probe requires two distinct canonical recipient view paths');
    const sibling = new URL(siblingPath, own.origin);
    async function get(target: URL, requestID: string) {
      return request.get(target.href, {
        maxRedirects: 0,
        maxRetries: 0,
        timeout: Math.min(10_000, remaining()),
        headers: { 'X-Request-ID': requestID },
      });
    }
    async function ownControl(which: 'ownBefore' | 'ownAfter') {
      let response;
      try {
        response = await get(own, receipt.requestIDs[which]);
      } catch {
        if (receipt.failureCode) throw new Error(failureMessage);
        fail('OWN_REQUEST_FAILED', 'scope probe own control request failed');
      }
      try {
        receipt[which] = response.status();
        remaining();
        if (receipt[which] !== 200)
          fail(
            'OWN_STATUS',
            `scope probe own control requires HTTP 200 (received ${receipt[which]})`,
          );
      } finally {
        await response.dispose();
      }
    }
    await ownControl('ownBefore');
    try {
      const response = await get(sibling, receipt.requestIDs.sibling);
      try {
        receipt.siblingWireStatus = response.status();
        if (receipt.siblingWireStatus >= 200 && receipt.siblingWireStatus < 300)
          fail(
            'CROSS_RECIPIENT_2XX',
            `scope probe: sibling object SERVED to another recipient's session (HTTP ${receipt.siblingWireStatus})`,
          );
        remaining();
        const body = await response.body();
        remaining();
        // TODO(upstream-contract): router silentDrop owns this canonical 404 body/cache shape.
        if (
          receipt.siblingWireStatus !== 404 ||
          body.length > 1024 ||
          body.toString('utf8') !== 'Resource not found\n' ||
          response.headers()['cache-control'] !== 'no-store'
        )
          fail(
            'UNEXPECTED_SIBLING_RESPONSE',
            `scope probe sibling lacks the converged router denial shape (HTTP ${receipt.siblingWireStatus})`,
          );
        receipt.siblingWireOutcome = 'CONVERGED_NOT_FOUND_PENDING_INTERNAL_403';
      } finally {
        await response.dispose();
      }
    } catch (err) {
      if (receipt.failureCode) throw new Error(failureMessage);
      // APIRequestContext uses Node errors. A reset can come from an edge as
      // well as the router, so this remains pending exact internal-403 proof.
      if (
        receipt.siblingWireStatus !== null ||
        !(err instanceof Error) ||
        !/ECONNRESET|socket hang up/.test(err.message)
      )
        fail(
          'SIBLING_REQUEST_FAILED',
          `scope probe sibling request failed (HTTP ${receipt.siblingWireStatus ?? 'unavailable'})`,
        );
      remaining();
      receipt.siblingWireOutcome = 'CONNECTION_CLOSED_PENDING_INTERNAL_403';
    }
    await ownControl('ownAfter');
    receipt.probeStatus = 'PASS';
    return receipt;
  } catch (err) {
    if (receipt.failureCode) throw new Error(failureMessage);
    return fail('UNEXPECTED_FAILURE', 'scope probe failed; see its status-only receipt');
  } finally {
    onFinal?.(receipt);
  }
}
