import { appendFileSync, statSync } from 'node:fs';
import { randomUUID } from 'node:crypto';
import { chromium } from 'playwright';
import { trackedQurlResources } from '../helpers/cleanup';
import { checkOwnershipConfig } from '../helpers/tunnelView';
import { getLinkStatus } from '../helpers/qurl-api';
import { resolveUrlTargetViaQurlLink } from '../helpers/urlTargetView';

const api = 'https://api.layerv.xyz/v1/qurls';
const suite = process.env.QURL_NATIVE_CLEANUP_ENABLED === '1' ? describe : describe.skip;
suite('URL-target one-use', () => {
  const tracked = trackedQurlResources({ MINT_API_URL: api, QURL_API_KEY: process.env.QURL_API_KEY ?? '' });
  let configured = false;
  let mintAttempted = false;
  let fixtureBound = false;
  function journal(record: Record<string, unknown>): void {
    appendFileSync(process.env.QURL_OWNERSHIP_RECEIPTS!, JSON.stringify({
      ...record, observed_at: new Date().toISOString(),
    }) + '\n', { mode: 0o600, flush: true });
  }

  beforeAll(async () => {
    if (process.env.MINT_API_URL !== api || process.env.QURL_NATIVE_CLEANUP_ENABLED !== '1') {
      throw new Error('URL-target one-use test requires the reviewed Sandbox ownership/cleanup configuration');
    }
    await checkOwnershipConfig();
    if (statSync(process.env.QURL_OWNERSHIP_RECEIPTS!).mode & 0o077) throw new Error('Ownership journal must be private');
    configured = true;
  });
  afterAll(async () => {
    if (!configured) return;
    let cleanupComplete = false;
    try {
      await tracked.revokeAll();
      cleanupComplete = true;
    } finally {
      journal({ event: !mintAttempted ? 'owned_url_target_no_fixture' :
        !fixtureBound || !cleanupComplete ? 'owned_url_target_recovery_required' : 'owned_url_target_api_cleanup_complete' });
    }
  });

  // Two native five-minute budgets plus navigation, API calls and runner margin.
  test('a consumed URL-target one-time link does not serve a second knock', async () => {
    // Launch before creating anything, so a missing browser cannot strand a fixture.
    const browser = await chromium.launch({ headless: true });
    try {
      const nonce = randomUUID();
      const target = `https://api.layerv.xyz/health?_e2e_nonce=${nonce}`;
      journal({ event: 'owned_url_target_mint_intent', nonce, target_url: target });
      mintAttempted = true;
      // Raw calls retain policy and target binding fields omitted by the shared helpers.
      // One attempt: an ambiguous POST remains in the fsynced journal for recovery.
      const created = await fetch(api, {
        method: 'POST', signal: AbortSignal.timeout(10_000), redirect: 'error',
        headers: { Authorization: `Bearer ${process.env.QURL_API_KEY}`, 'Content-Type': 'application/json' },
        body: JSON.stringify({ target_url: target, expires_in: '10m', one_time_use: true,
          max_sessions: 1, session_duration: '1m', label: `E2E URL one-use ${nonce}` }),
      });
      const body = await created.json().catch(() => null);
      const minted = body?.data ?? body;
      const resource_id = minted?.resource_id;
      const qurl_id = minted?.qurl_id ?? minted?.id;
      const expires_at = minted?.expires_at;
      journal({ event: 'owned_url_target_mint_response', nonce, http_status: created.status,
        resource_id, qurl_id, expires_at });
      if (!created.ok || typeof resource_id !== 'string' || typeof qurl_id !== 'string') {
        throw new Error('URL-target fixture identity was not confirmed; retain the mint intent');
      }
      const readback = await fetch(`${api}/${encodeURIComponent(qurl_id)}`, {
        headers: { Authorization: `Bearer ${process.env.QURL_API_KEY}` }, signal: AbortSignal.timeout(10_000), redirect: 'error',
      });
      const canonical = await readback.json().catch(() => null);
      const resource = canonical?.data ?? canonical;
      journal({ event: 'owned_url_target_binding_response', nonce, http_status: readback.status });
      if (!readback.ok || resource?.resource_id !== resource_id || resource?.target_url !== target) {
        throw new Error('URL-target fixture binding was not confirmed; retain known identities for recovery');
      }
      tracked.track(resource_id);
      tracked.trackChild(qurl_id);
      fixtureBound = true;
      const summary = Array.isArray(resource.qurls) ? resource.qurls.find((entry: unknown) =>
        entry !== null && typeof entry === 'object' && 'qurl_id' in entry && entry.qurl_id === qurl_id) : undefined;
      expect(summary).toMatchObject({ one_time_use: true, max_sessions: 1, use_count: 0, status: 'active' });
      const link = minted.qurl_link ?? minted.link ?? minted.url;
      let parsed: URL;
      try { parsed = new URL(link); }
      catch { throw new Error('URL-target fixture returned an invalid link'); }
      expect(parsed.origin).toBe('https://qurl.link.layerv.xyz');
      expect(parsed.username + parsed.password).toBe('');
      expect(Number.isFinite(Date.parse(expires_at))).toBe(true);
      const ownership = { resource_id, qurl_id, expires_at };
      for (const expectedNoAdmission of [false, true]) {
        const context = await browser.newContext();
        try {
          const served = await resolveUrlTargetViaQurlLink(await context.newPage(), link, ownership, expectedNoAdmission);
          expect(served).toBe(!expectedNoAdmission);
        } finally { await context.close(); }
      }
      try {
        const status = await getLinkStatus(api, process.env.QURL_API_KEY!, qurl_id, AbortSignal.timeout(10_000));
        expect(status.use_count).toBe(1);
        journal({ event: 'owned_url_target_one_use_status', resource_id, qurl_id, use_count: status.use_count });
      } catch (error) {
        if (!(error instanceof Error) || error.name !== 'StatusCheckError' ||
            !('status' in error) || error.status !== 404) throw error;
        journal({ event: 'owned_url_target_one_use_status', resource_id, qurl_id, http_status: 404 });
      }
    } finally { await browser.close(); }
  }, 13 * 60_000);
});
