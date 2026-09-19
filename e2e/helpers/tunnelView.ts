/**
 * Headless-browser viewer for the fileviewer reverse-tunnel (render-at-mint).
 *
 * WHY a browser and not `fetch`/curl: PR qurl-integrations-infra#1111
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

import { chromium, type Browser } from 'playwright';
import { spawnSync } from 'node:child_process';
import { appendFileSync, accessSync, constants } from 'node:fs';
import { createHash } from 'node:crypto';
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
  ownership: { resource_id: string; qurl_id: string; expires_at: string };
  /** Wall-clock budget for navigation + knock + the tunnel-view response, in ms.
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
