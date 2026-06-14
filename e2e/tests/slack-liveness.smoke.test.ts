/**
 * Slack bot — HTTP liveness/signature/dispatch smoke test.
 *
 * Drives the *live* deployed Slack bot over HTTP with synthetic, signed
 * requests and asserts the response. This is the prod-gate analogue of the
 * Discord smoke's bot-liveness checks (identify/send/read) — it proves the
 * service is up and reachable, signature enforcement works, the deployed
 * SLACK_SIGNING_SECRET matches what we signed with, and the slash-command
 * surface dispatches.
 *
 * Scope is deliberately liveness-grade — read-only and side-effect-free:
 *   - GET /health            → 200 {"status":"ok"}
 *   - signed url_verification → 200, echoes the challenge (signature OK)
 *   - bad signature          → 401 (proves signature enforcement isn't bypassed)
 *   - signed `/qurl help`    → 200 help text (synchronous dispatch, no mint)
 *
 * Because this runs as a deploy gate that can race a rolling deploy, the probes
 * go through `fetchWithTransientRetry` (like the connector smokes) so a brief
 * drain-gap 503 doesn't false-red the gate — safe here because every probe is
 * side-effect-free and a 401 is non-retryable, so enforcement still fails fast.
 * (POST probes only retry the drain-gap 503, not 502/504; see http.ts for the
 * method-aware retry set.)
 *
 * What it does NOT do (by design): it does NOT mint a qURL through Slack.
 * `response_url` is hard-pinned to hooks.slack.com (apps/slack/internal/
 * process.go), so a synthetic command's async reply is discarded and can't be
 * observed; and qURL functional coverage already exists globally via the shared
 * suite against the same qurl-service. Verifying a real user-visible link through
 * Slack needs a test workspace + agent/browser harness (deferred).
 *
 * Skips when SLACK_BOT_BASE_URL is unset. The signed cases additionally need
 * SLACK_SIGNING_SECRET; they skip (not fail) when it's absent, so the suite is
 * safe to run unconfigured wherever the e2e suite runs.
 *
 * Failure-mode note: the signer uses the runner's wall clock, and the app rejects
 * timestamps outside a 5-minute skew (signature.go slackTimestampSkew). If a
 * runner's clock drifts >5 min from the deployed bot, the *positive* signed cases
 * (url_verification, /qurl help) will 401-as-stale while the bad-signature case
 * still passes — a confusing red that's clock drift, not a signing regression.
 */

import * as dotenv from 'dotenv';
import * as path from 'path';
// Explicit load matches every sibling e2e test. It overlaps jest.config's
// `setupFiles: ['dotenv/config']`, but the absolute path is intentional — it's
// robust to the run cwd, unlike the cwd-relative setupFiles load.
dotenv.config({ path: path.resolve(__dirname, '..', '.env') });

import { randomUUID } from 'crypto';
import { loadOptionalEnv } from '../helpers/env';
import { postSigned } from '../helpers/slack-sign';
import { fetchWithTransientRetry } from '../helpers/http';

const optEnv = loadOptionalEnv();
// Trim a trailing slash at read time so a `https://host/` config typo doesn't
// become `host//health` (which the exact-path router would 404 with a baffling,
// non-retryable failure). Optional-chained so `undefined` stays undefined — the
// skip gate and the `describe.skip` body (which Jest still executes) must never
// dereference it when the suite is unconfigured.
const baseUrl = optEnv.SLACK_BOT_BASE_URL?.replace(/\/+$/, '');
const signingSecret = optEnv.SLACK_SIGNING_SECRET;

const SUITE_ENABLED = Boolean(baseUrl);
const SIGNED_ENABLED = SUITE_ENABLED && Boolean(signingSecret);

const FORM = 'application/x-www-form-urlencoded';

(SUITE_ENABLED ? describe : describe.skip)('Slack bot — HTTP liveness smoke', () => {
  // Hoisted once after the gate (sibling convention) so call sites don't repeat
  // `!`. These are only dereferenced inside test bodies, which run only when the
  // suite is enabled (SUITE_ENABLED / SIGNED_ENABLED) — i.e. only when present.
  const base = baseUrl!;
  const secret = signingSecret!;

  test('GET /health returns 200 {"status":"ok"}', async () => {
    const res = await fetchWithTransientRetry(`${base}/health`);
    expect(res.status).toBe(200);
    // Guard content-type so a 200 non-JSON page (e.g. an infra error page) fails
    // with a clear assertion rather than an opaque res.json() parse error.
    expect(res.headers.get('content-type') ?? '').toMatch(/json/i);
    const body = (await res.json()) as { status?: string };
    expect(body.status).toBe('ok');
  });

  // An unsigned POST hits the missing-header branch (errSlackSignatureMissing),
  // distinct from the wrong-secret mismatch branch below. Needs no secret, so it
  // runs whenever the suite is enabled. Scope note: this proves "unsigned is
  // rejected", NOT that the deployed secret is configured — an empty signing
  // secret also 401s here (errSlackSigningSecretEmpty short-circuits first). The
  // positive signed `url_verification` case below is what proves the secret is set.
  test('an unsigned request is rejected with 401', async () => {
    const res = await fetchWithTransientRetry(`${base}/slack/events`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ type: 'url_verification', challenge: 'should-not-echo' }),
    });
    expect(res.status).toBe(401);
    // Defense-in-depth: a rejected request must never have reached the
    // url_verification handler, so the challenge must not be echoed back.
    expect(await res.text()).not.toContain('should-not-echo');
  });

  // Signed cases need the deployed signing secret to forge a valid signature.
  const itSigned = SIGNED_ENABLED ? test : test.skip;

  itSigned('signed url_verification challenge returns 200 and echoes it', async () => {
    // Unique per run so a cached/echoed-elsewhere value can't false-pass.
    const challenge = `e2e-smoke-${randomUUID()}`;
    const body = JSON.stringify({ type: 'url_verification', challenge });
    const res = await postSigned(base, '/slack/events', body, secret);
    expect(res.status).toBe(200);
    const json = (await res.json()) as { challenge?: string };
    // Exact echo proves: signature verified (deployed secret matches) AND the
    // events route is reachable end-to-end on the deployed service.
    expect(json.challenge).toBe(challenge);
  });

  itSigned('a request with a bad signature is rejected with 401', async () => {
    const body = JSON.stringify({ type: 'url_verification', challenge: 'should-not-echo' });
    // Valid timestamp + correctly-shaped header, but signed with the wrong
    // secret → HMAC mismatch. Proves signature enforcement is actually on (i.e.
    // badly-signed traffic can't slip through to the handler). 401 is
    // non-retryable, so this fails fast rather than burning the retry budget.
    const res = await postSigned(base, '/slack/events', body, `${secret}-tampered`);
    expect(res.status).toBe(401);
    // Same defense-in-depth: the mismatch must reject before the handler runs.
    expect(await res.text()).not.toContain('should-not-echo');
  });

  itSigned('signed `/qurl help` slash command returns 200 help text', async () => {
    // `text=help` returns synchronously via respondSlack (no qurl-service call,
    // no response_url, no side effects — handler.go dispatchUserCommand). The
    // command literal need not match the env's registered name; routing keys
    // on the `-admin` suffix, and help renders for any non-admin command. The
    // team/channel/user fields are synthetic — help doesn't depend on a real install.
    const form = new URLSearchParams({
      command: '/qurl',
      text: 'help',
      team_id: 'T_E2E_SMOKE',
      channel_id: 'C_E2E_SMOKE',
      user_id: 'U_E2E_SMOKE',
    }).toString();
    const res = await postSigned(base, '/slack/commands', form, secret, FORM);
    expect(res.status).toBe(200);
    const json = (await res.json()) as { response_type?: string; text?: string };
    expect(json.response_type).toBe('ephemeral');
    // Loose assertion on the help copy so wording tweaks don't break the smoke,
    // while still proving the dispatcher rendered the user help surface.
    expect(json.text ?? '').toMatch(/qurl/i);
  });
});
