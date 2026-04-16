/**
 * Open a QURL link in a fresh browser context and return the result.
 */

import { Browser, BrowserContext, Page } from '@playwright/test';

export interface QurlLinkResult {
  /** Final HTTP status code */
  status: number;
  /** Final URL after redirects */
  finalUrl: string;
  /** Page title */
  title: string;
  /** Page body text */
  bodyText: string;
  /** Whether the link resolved successfully (2xx status) */
  ok: boolean;
  /** Whether the page showed an expiry / revoked message */
  isExpired: boolean;
}

const QURL_LINK_PATTERN = /https?:\/\/(?:qurl\.io|qurl\.dev|localhost:\d+)\/[a-zA-Z0-9_-]+/g;

/**
 * Extract QURL links from a text string.
 */
export function extractQurlLink(text: string): string[] {
  return text.match(QURL_LINK_PATTERN) ?? [];
}

/**
 * Extract the first QURL link from text, or throw.
 */
export function extractFirstQurlLink(text: string): string {
  const links = extractQurlLink(text);
  if (links.length === 0) {
    throw new Error(`No QURL link found in text: "${text.slice(0, 200)}"`);
  }
  return links[0];
}

/**
 * Open a QURL link in a fresh incognito context and return status info.
 */
export async function openQurlLink(
  browser: Browser,
  url: string,
): Promise<QurlLinkResult> {
  let context: BrowserContext | null = null;

  try {
    context = await browser.newContext({
      // Fresh context — no cookies/auth, simulates anonymous user
      ignoreHTTPSErrors: true,
    });

    const page: Page = await context.newPage();

    const response = await page.goto(url, {
      waitUntil: 'networkidle',
      timeout: 30_000,
    });

    const status = response?.status() ?? 0;
    const finalUrl = page.url();
    const title = await page.title();
    const bodyText = await page.locator('body').textContent() ?? '';

    const isExpired =
      bodyText.toLowerCase().includes('expired') ||
      bodyText.toLowerCase().includes('revoked') ||
      bodyText.toLowerCase().includes('no longer available') ||
      status === 410;

    return {
      status,
      finalUrl,
      title,
      bodyText,
      ok: status >= 200 && status < 300,
      isExpired,
    };
  } finally {
    if (context) {
      await context.close();
    }
  }
}
