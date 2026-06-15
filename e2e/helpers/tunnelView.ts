/**
 * Headless-browser viewer for the fileviewer reverse-tunnel (render-at-mint).
 *
 * WHY a browser and not `fetch`/curl: PR qurl-integrations-infra#1111
 * decommissioned the legacy EC2 fileviewer host (fileviewer.layerv.xyz). Under
 * render-at-mint + the NHP tunnel a recipient NEVER hits a fetchable view URL
 * directly. The flow is entirely JS-driven:
 *
 *   1. The recipient opens the minted `qurl_link`. The capability lives in the
 *      URL FRAGMENT (`#at_<token>`), which the server never sees — only the SPA's
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

/** Matches the tunnel view URL on ANY environment:
 *   https://r_<id>.qurl.site<.layerv.xyz|.layerv.ai|…>/views/<mint-id>
 * Pinned to the `.qurl.site` host segment + the `/views/` path so it can't match
 * the `qurl.link` knock redirect or the top-frame SPA. Host-suffix-agnostic so
 * the same helper works against sandbox and prod. */
const TUNNEL_VIEW_RE = /\.qurl\.site[^/]*\/views\//;

export interface ViewViaQurlLinkOptions {
  /** Browser launch headless? Defaults to true; set `PLAYWRIGHT_HEADED=1`
   *  (or `HEADLESS=0`) in the env to watch it run locally for debugging. */
  headless?: boolean;
  /** Wall-clock budget for navigation + knock + the tunnel-view response, in ms.
   *  Default 30_000. */
  timeoutMs?: number;
}

export interface ViewViaQurlLinkResult {
  /** HTTP status of the tunnel-view response (200 means the view served). */
  status: number;
}

function resolveHeadless(opt?: boolean): boolean {
  if (opt !== undefined) return opt;
  // Local-debug overrides; default headless for CI.
  return !(process.env.PLAYWRIGHT_HEADED === '1' || process.env.HEADLESS === '0');
}

/**
 * Open a minted `qurl_link` in a headless browser, let the SPA drive the NHP
 * knock, and resolve when the reverse-tunnel view response (`…/views/<id>`)
 * arrives — returning its HTTP status (200 = the view served).
 *
 * THROWS if no `…/views/<id>` response arrives within `timeoutMs` — the negative
 * signal a caller relies on (a revoked/consumed/expired link, or a tunnel that's
 * down, never serves the view).
 */
export async function viewViaQurlLink(
  qurlLink: string,
  opts: ViewViaQurlLinkOptions = {},
): Promise<ViewViaQurlLinkResult> {
  const timeoutMs = opts.timeoutMs ?? 30_000;
  let browser: Browser | undefined;
  try {
    browser = await chromium.launch({ headless: resolveHeadless(opts.headless) });
    const page = await (await browser.newContext()).newPage();

    // Wait on the tunnel-view RESPONSE, registered BEFORE goto so one that lands
    // mid-navigation isn't missed. domcontentloaded, NOT networkidle: the tunnel
    // keepalive means networkidle never fires. The predicate is URL-only — status
    // is checked after, so a non-200 view throws "returned N", not "no response".
    const tunnelResponse = page.waitForResponse((r) => TUNNEL_VIEW_RE.test(r.url()), {
      timeout: timeoutMs,
    });
    await page.goto(qurlLink, { waitUntil: 'domcontentloaded', timeout: timeoutMs });

    const resp = await tunnelResponse.catch(() => {
      throw new Error(
        `viewViaQurlLink: no tunnel-view response (…/views/<id>) within ${timeoutMs}ms — ` +
          `the knock did not resolve to a served view (link revoked/consumed/expired, or tunnel down).`,
      );
    });
    if (resp.status() !== 200) {
      throw new Error(`viewViaQurlLink: tunnel-view returned ${resp.status()} (expected 200).`);
    }
    return { status: resp.status() };
  } finally {
    // Always tear the browser down — a leaked chromium would hang jest's worker exit.
    await browser?.close();
  }
}
