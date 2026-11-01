import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import type { Page } from 'playwright';
import { isUrlTargetNavigation, resolveUrlTargetViaQurlLink } from '../helpers/urlTargetView';
import { observeBrowserCleanup } from '../helpers/nativeBrowserCleanup';

jest.mock('../helpers/tunnelView', () => ({ recordAdmissionAttempt: jest.fn() }));
jest.mock('../helpers/nativeBrowserCleanup', () => ({
  ...jest.requireActual('../helpers/nativeBrowserCleanup'), observeBrowserCleanup: jest.fn(),
}));

test('URL-target navigation requires the observed exact Sandbox origin and root', () => {
  const host = `q-${'a'.repeat(52)}.qurl.site.layerv.xyz`;
  expect(isUrlTargetNavigation(`https://${host}/`)).toBe(true);
  for (const value of [
    `http://${host}/`, `https://${host}.evil.test/`, `https://user@${host}/`,
    `https://${host}:444/`, `https://${host}/views/foo`, `https://${host}/?token=x`,
    `https://${host}/#secret`, 'https://qurl.link.layerv.xyz/',
    'https://example.com/', 'invalid',
  ]) expect(isUrlTargetNavigation(value)).toBe(false);
});

test('navigation cannot pass on an error body or substitute for authenticated denial', async () => {
  const directory = mkdtempSync(join(tmpdir(), 'url-target-unit-'));
  const previous = process.env.QURL_OWNERSHIP_RECEIPTS;
  process.env.QURL_OWNERSHIP_RECEIPTS = join(directory, 'receipts.jsonl');
  const ownership = { resource_id: 'owned', qurl_id: 'q_owned', expires_at: '2030-01-01T00:00:00Z' };
  const frame = {};
  const page = (body: unknown, status = 200, contentType = 'application/json') => ({
    mainFrame: () => frame,
    goto: jest.fn().mockResolvedValue(undefined),
    waitForResponse: jest.fn().mockResolvedValue({
      status: () => status, headers: () => ({ 'content-type': contentType }), json: async () => body,
    }),
  }) as unknown as Page;
  try {
    jest.mocked(observeBrowserCleanup).mockImplementation(async () => async () =>
      ({ exchanges: 1, closed: 1, denied: 0, unresolved: 0 }));
    for (const body of [null, [], { status: 'error' }, { status: 'healthy', error: 'bad' }]) {
      expect(await resolveUrlTargetViaQurlLink(page(body), 'test-link', ownership)).toBe(false);
    }
    expect(await resolveUrlTargetViaQurlLink(page({ status: 'healthy' }, 503), 'test-link', ownership)).toBe(false);
    expect(await resolveUrlTargetViaQurlLink(page({ status: 'healthy' }, 200, 'text/html'), 'test-link', ownership)).toBe(false);
    expect(await resolveUrlTargetViaQurlLink(page({ status: 'healthy' }), 'test-link', ownership)).toBe(true);
    await expect(resolveUrlTargetViaQurlLink(page(null), 'test-link', ownership, true)).rejects.toThrow('unresolved');
    jest.mocked(observeBrowserCleanup).mockImplementation(async () => async () =>
      ({ exchanges: 1, closed: 0, denied: 1, unresolved: 0 }));
    expect(await resolveUrlTargetViaQurlLink(page(null), 'test-link', ownership, true)).toBe(false);
    await expect(resolveUrlTargetViaQurlLink(page({ status: 'healthy' }), 'test-link', ownership, true)).rejects.toThrow('unresolved');
  } finally {
    if (previous === undefined) delete process.env.QURL_OWNERSHIP_RECEIPTS;
    else process.env.QURL_OWNERSHIP_RECEIPTS = previous;
    rmSync(directory, { recursive: true, force: true });
  }
});
