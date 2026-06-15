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
 *   2. The SPA performs the NHP "knock" (a `302` through
 *      `resolve.qurl.link…/plugins/qurl`).
 *   3. The actual view renders in a CHILD FRAME at
 *      `https://r_<id>.qurl.site…/views/<mint-id>` — NOT the top frame.
 *
 * `node fetch`/curl cannot do step 1 (no JS to read the fragment) or step 3 (no
 * frame). A real browser can — hence Playwright chromium, headless.
 *
 * Each `qurl_link` is ONE-TIME-USE (consumed on first view), so every view needs
 * its own fresh mint+link. We therefore launch and close a fresh browser per
 * call rather than sharing one — the call is the unit of work, and a per-call
 * browser keeps state (cookies/storage) from leaking between independent mints.
 *
 * Proven against sandbox (2026-06-15): a real mint produced
 *   302 resolve.qurl.link.layerv.xyz/plugins/qurl   (the knock)
 *   200 r_<id>.qurl.site.layerv.xyz/views/<mint-id>  (the tunnel view)
 * and the child frame rendered an `<img>`.
 */

import { chromium, type Browser, type Frame, type Response } from 'playwright';

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
  /** Total wall-clock budget for the navigation + knock + tunnel-frame render,
   *  in ms. Default 30_000. The page.goto itself is capped at this value too. */
  timeoutMs?: number;
}

export interface ViewViaQurlLinkResult {
  /** Rendered HTML of the TUNNEL CHILD FRAME (not the top SPA frame). */
  html: string;
  /** HTTP status of the tunnel-view response (the `…/views/<id>` 200). */
  status: number;
  /** The fully-resolved tunnel-frame URL (carries the capability mint-id — do
   *  NOT log it; the caller asserts on `html`/`status`, not this). */
  frameUrl: string;
}

function resolveHeadless(opt?: boolean): boolean {
  if (opt !== undefined) return opt;
  // Local-debug overrides; default headless for CI.
  if (process.env.PLAYWRIGHT_HEADED === '1' || process.env.HEADLESS === '0') {
    return false;
  }
  return true;
}

/**
 * Open a minted `qurl_link` in a headless browser, drive the NHP knock, wait for
 * the fileviewer reverse-tunnel CHILD FRAME to render, and return its HTML +
 * status + URL.
 *
 * Throws a clear error if no tunnel-frame `200` (a `…/views/<id>` response)
 * appears within `timeoutMs` — that is the signal a caller asserts on for the
 * negative case (e.g. a revoked link must NOT render a tunnel view).
 *
 * The browser is launched and closed per call (the `qurl_link` is one-time-use,
 * so each view is a distinct mint anyway).
 */
export async function viewViaQurlLink(
  qurlLink: string,
  opts: ViewViaQurlLinkOptions = {},
): Promise<ViewViaQurlLinkResult> {
  const headless = resolveHeadless(opts.headless);
  const timeoutMs = opts.timeoutMs ?? 30_000;

  let browser: Browser | undefined;
  try {
    browser = await chromium.launch({ headless });
    const context = await browser.newContext();
    const page = await context.newPage();

    // Capture the tunnel-view response status as soon as it lands. The view
    // renders in a child frame, so we watch ALL responses (not just the top
    // document) and latch the first one whose URL matches the tunnel-view shape.
    let tunnelStatus: number | undefined;
    let tunnelUrl: string | undefined;
    page.on('response', (resp: Response) => {
      const url = resp.url();
      if (TUNNEL_VIEW_RE.test(url) && tunnelStatus === undefined) {
        tunnelStatus = resp.status();
        tunnelUrl = url;
      }
    });

    // domcontentloaded, NOT networkidle: the tunnel transport holds a keepalive
    // connection open, so 'networkidle' never fires and would hang to timeout.
    await page.goto(qurlLink, { waitUntil: 'domcontentloaded', timeout: timeoutMs });

    // Wait for the tunnel view to be SERVED — a 200 on `…/views/<id>`. We key on
    // the RESPONSE, not a specific frame: the view renders in a CHILD FRAME for
    // baked types (image/pdf — an <img> iframe) but in the TOP FRAME for url-type
    // (the SPA fetches the 200 and renders the "Open in Google Maps" card client-
    // side inline). Requiring a child frame would falsely fail every url-type view.
    const deadline = Date.now() + timeoutMs;
    while (Date.now() < deadline && tunnelStatus === undefined) {
      await page.waitForTimeout(250);
    }

    if (tunnelStatus === undefined) {
      throw new Error(
        `viewViaQurlLink: no tunnel-view response (…/views/<id>) within ${timeoutMs}ms — ` +
          `the knock did not resolve to a rendered view (link revoked/consumed/expired, ` +
          `or the tunnel is down).`,
      );
    }
    if (tunnelStatus !== 200) {
      throw new Error(
        `viewViaQurlLink: tunnel-view returned ${tunnelStatus} (expected 200) — ` +
          `the knock resolved but the view did not render.`,
      );
    }

    // The 200 landed, but the SPA may still be mid-render: during the knock +
    // client-side render the top document carries class="verifying", which it
    // DROPS once the view is ready (the url-type "Open in Google Maps" card
    // renders inline only after this; collecting too early yields the verifying
    // shell). Wait for the class to clear. For baked image/pdf the view is in a
    // child frame and the top may stay "verifying" — the `.catch` lets those
    // fall through (the child-frame HTML is already present), then a short settle.
    await page
      .waitForFunction(() => !document.documentElement.classList.contains('verifying'), {
        timeout: Math.max(8_000, deadline - Date.now()),
      })
      .catch(() => {});
    await page.waitForTimeout(1_000);
    const frameHtmls = await Promise.all(
      page.frames().map((f: Frame) => f.content().catch(() => '')),
    );
    const tunnelFrame = page.frames().find((f) => TUNNEL_VIEW_RE.test(f.url()));
    return {
      html: frameHtmls.join('\n'),
      status: tunnelStatus,
      frameUrl: tunnelUrl ?? tunnelFrame?.url() ?? page.url(),
    };
  } finally {
    // Always tear the browser down — a leaked chromium would hang jest's worker
    // exit. `?.close()` is null-safe if launch threw.
    await browser?.close();
  }
}
