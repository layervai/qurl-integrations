/**
 * qURL OAuth setup — bot HTTP-server smoke test.
 *
 * Pings the deployed bot's `/oauth/qurl/start` route and verifies the CSRF
 * gate + not-configured gate behave as documented. Skips if `BOT_HTTP_URL`
 * is unset (most environments don't configure the bot's public HTTP host
 * for E2E because the URL changes per environment).
 *
 * What this test covers:
 *   - The route is mounted (no 404 — proves server.js wiring).
 *   - Garbage-state inputs are rejected before any redirect to Auth0
 *     (CSRF guard works in the deployed env, not just locally).
 *   - When AUTH0_* secrets aren't configured, the route returns 503 with
 *     the documented "not configured" page (Stage-1 fallback path).
 *
 * What this test does NOT cover (manual runbook required — see comment):
 *   - End-to-end Auth0 login + consent flow (requires Auth0 test tenant
 *     credentials + a real browser; no Playwright in this E2E suite yet).
 *   - The actual API-key mint via POST /v1/api-keys (the mint is reached
 *     only after Auth0 returns a valid `code`, which we can't fake here).
 *   - Persisting the minted key to DDB `guild_configs` (covered by unit
 *     tests; verifying in the deployed env requires browser-driven OAuth).
 *
 * Manual smoke runbook (run after Justin registers the Auth0 application
 * and sets prod SSM secrets):
 *
 *   1. In Discord, invite the bot to a sandbox test guild.
 *   2. Run `/qurl setup` as the guild admin.
 *   3. Click the ephemeral "Authorize qURL" link.
 *   4. Sign in to layerv.ai (Auth0); consent to the requested scopes.
 *   5. Verify the success page renders ("✅ qURL is connected to your
 *      Discord server").
 *   6. Verify a DM lands from the bot saying "qURL is connected to your
 *      Discord server."
 *   7. Verify DDB row exists: `aws dynamodb get-item --table-name
 *      <table_prefix>guild-configs --key '{"guild_id":{"S":"<id>"}}'`.
 *   8. Run `/qurl file` (or `/qurl map`) with a test recipient — confirm
 *      it succeeds (proves the persisted key works through the connector
 *      + mint).
 *   9. Run `/qurl status` — confirm it shows the key prefix.
 */

import * as dotenv from 'dotenv';
import * as path from 'path';
dotenv.config({ path: path.resolve(__dirname, '..', '.env') });

import { loadOptionalEnv } from '../helpers/env';

const optEnv = loadOptionalEnv();
const SUITE_ENABLED = Boolean(optEnv.BOT_HTTP_URL);

(SUITE_ENABLED ? describe : describe.skip)('qURL OAuth setup — bot HTTP smoke', () => {
  const botUrl = optEnv.BOT_HTTP_URL!;

  test('GET /oauth/qurl/start without state returns 400 (CSRF guard fires)', async () => {
    const res = await fetch(`${botUrl}/oauth/qurl/start`);
    // Two valid responses depending on whether AUTH0_* is configured:
    //   - 503 if Auth0 not configured (route checks config first, before state)
    //   - 400 if Auth0 configured but state is missing/invalid
    expect([400, 503]).toContain(res.status);
    // The bot serves an HTML page on every error path through this
    // route (via renderPage) so a future change that accidentally
    // returns JSON or a plain string surfaces here.
    expect(res.headers.get('content-type') || '').toMatch(/text\/html/i);
    const body = await res.text();
    if (res.status === 400) {
      expect(body).toMatch(/Invalid setup link|setup link is invalid|expired/i);
    } else {
      expect(body).toMatch(/not configured/i);
    }
  });

  test('GET /oauth/qurl/start with garbage state returns 400 or 503', async () => {
    const res = await fetch(`${botUrl}/oauth/qurl/start?state=not-a-valid-signed-state`);
    expect([400, 503]).toContain(res.status);
  });

  test('GET /oauth/qurl/callback without code returns 400 or 503', async () => {
    const res = await fetch(`${botUrl}/oauth/qurl/callback`);
    expect([400, 503]).toContain(res.status);
  });

  test('GET /oauth/qurl/callback with Auth0 error param returns 400 or 503', async () => {
    const res = await fetch(`${botUrl}/oauth/qurl/callback?error=access_denied&error_description=user+declined`);
    expect([400, 503]).toContain(res.status);
    const body = await res.text();
    if (res.status === 400) {
      expect(body).toMatch(/declined|invalid|expired/i);
    }
  });

  test('GET /oauth/qurl/start route is mounted (not 404)', async () => {
    const res = await fetch(`${botUrl}/oauth/qurl/start`);
    expect(res.status).not.toBe(404);
  });
});
