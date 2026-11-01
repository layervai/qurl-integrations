import type { Page } from 'playwright';
import { appendFileSync } from 'node:fs';
import { recordAdmissionAttempt, type ViewViaQurlLinkOptions } from './tunnelView';
import { observeBrowserCleanup, nativeCleanupDisposition } from './nativeBrowserCleanup';

/** Observed Sandbox URL targets bootstrap a q- origin, then serve its root. */
export function isUrlTargetNavigation(value: string): boolean {
  try {
    const url = new URL(value);
    return url.protocol === 'https:' && !url.username && !url.password && !url.port &&
      /^q-[a-z2-7]{52}\.qurl\.site\.layerv\.xyz$/.test(url.hostname) &&
      url.pathname === '/' && !url.search && !url.hash;
  } catch { return false; }
}

/** This fixture targets Sandbox /health. Arrival alone never proves content. */
export async function resolveUrlTargetViaQurlLink(
  page: Page,
  link: string,
  ownership: ViewViaQurlLinkOptions['ownership'],
  expectedNoAdmission = false,
): Promise<boolean> {
  const timeoutMs = 30_000;
  await recordAdmissionAttempt(link, ownership, timeoutMs);
  const cleanup = await observeBrowserCleanup(page, link);
  if (!cleanup) throw new Error('URL-target test requires authenticated native cleanup');
  let served = false;
  try {
    // Register before navigation and settle rejection even if goto fails first.
    const response = page.waitForResponse((candidate) =>
      candidate.request().isNavigationRequest() && candidate.frame() === page.mainFrame() &&
      isUrlTargetNavigation(candidate.url()), { timeout: timeoutMs }).catch(() => undefined);
    try {
      await page.goto(link, { waitUntil: 'domcontentloaded', timeout: timeoutMs });
    } catch { throw new Error('URL-target navigation failed'); }
    const target = await response;
    if (target?.status() === 200 && target.headers()['content-type']?.includes('application/json')) {
      const body: unknown = await target.json().catch(() => null);
      served = body !== null && typeof body === 'object' && !Array.isArray(body) &&
        Object.keys(body).length === 1 && (body as { status?: unknown }).status === 'healthy';
    }
    return served;
  } finally {
    const result = await cleanup();
    const state = nativeCleanupDisposition(result, expectedNoAdmission, served);
    appendFileSync(process.env.QURL_OWNERSHIP_RECEIPTS!, JSON.stringify({
      event: 'owned_native_cleanup', ...ownership, observed_at: new Date().toISOString(),
      ...result, state,
    }) + '\n', { mode: 0o600, flush: true });
    if (state === 'unresolved') throw new Error('URL-target native cleanup or denial remains unresolved');
  }
}
