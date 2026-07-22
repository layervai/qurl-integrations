/**
 * File upload → revoke → status-404 E2E flow.
 *
 * Covers the revoke path for actual file resources (images/PDFs/etc.) — the
 * other revoke tests (smoke.test.ts, link-lifecycle.test.ts) only exercise
 * URL-based qURLs (`target_url=https://example.com/...`). This file fills
 * the gap by uploading a real file via the connector's `/upload` endpoint,
 * minting a per-recipient render-at-mint view (`POST /api/mint_link`) and
 * confirming it serves through the tunnel, then revoking by resource_id and
 * confirming the qURL resource is marked revoked via getResourceStatus.
 *
 * Revoke semantics: `DELETE /v1/resources/{id}` kills the qURL-layer token
 * chain (the qurl.link → NHP knock → tunnel view). #1111 removed the legacy
 * md5-addressed fileviewer URL, so under the render-at-mint tunnel the only
 * access is the minted qurl.link, which this revoke invalidates. The canonical
 * revoke assertion is the management API retaining the resource with
 * `status=revoked`, matching smoke.test.ts.
 *
 * Without this coverage, a regression in the revoke API for
 * connector-uploaded resources would ship silently (URL-mint revoke
 * already covered by smoke.test.ts).
 *
 * Also home to the knock-driven SINGLE-USE enforcement test ("a consumed
 * one-time link does not serve a second knock") — this file is the only
 * place a real consuming knock is driven (viewViaQurlLink); the URL-mint
 * suites can only exercise bare fetches, which don't run the SPA knock,
 * so their status checks pin counter coherence rather than enforcement
 * (URL-target knock coverage tracked in #951).
 *
 * Also pins two render-at-mint security guarantees (#1027, EPIC #1019), in the
 * "distinct-per-viewer watermark + `_` route-label SNI" test below:
 *
 *   - DISTINCT-PER-VIEWER WATERMARK: two qURLs minted for the SAME uploaded file
 *     resolve to two DIFFERENT `views/<mint-id>` objects — each a per-recipient
 *     baked copy. This is THE leak-traceability guarantee: a leaked image is
 *     attributable to the specific recipient whose `views/<mint-id>` it is.
 *     #1027 defines distinctness AS "different `views/` objects"; the mint-id is
 *     hex(16 random bytes), distinct per mint by construction. (The watermark
 *     itself is an INVISIBLE neural meta-seal mark — `watermark_metaseal_enabled`
 *     in sandbox — so distinctness lives in bits the harness can't see via bytes;
 *     the distinct `views/<mint-id>` object key is the strongest OBSERVABLE
 *     backstop. See the test for why a rendered-byte hash isn't asserted.)
 *   - SNI / DNS for the opaque `r_<route-label>` host: the public management
 *     `resource_id` is a key and must not be parsed as a hostname. A 200 through
 *     the actual tunnel URL proves DNS + wildcard-cert presentation for the
 *     `_`-bearing routing label end-to-end.
 */

import * as dotenv from 'dotenv';
import * as path from 'path';
dotenv.config({ path: path.resolve(__dirname, '..', '.env') });

import { trackedQurlResources } from '../helpers/cleanup';
import { loadEnv } from '../helpers/env';
import * as qurl from '../helpers/qurl-api';
import {
  TunnelViewTimeoutError,
  mintIdFromTunnelViewUrl,
  viewViaQurlLink,
  type ViewViaQurlLinkResult,
} from '../helpers/tunnelView';

const env = loadEnv();

// Track-before-validate, same as every other minting suite: an upload is
// recorded before any expect() can throw, so a failed knock/assertion
// can't strand the resource past cleanup (previously this file revoked
// only inline, leaking to TTL on a mid-test failure). In-test revokes go
// through tracked.revoke so the assertion stays while the ledger syncs.
const tracked = trackedQurlResources(env);

afterAll(() => tracked.revokeAll());

// Valid 1x1 transparent PNG (standard test fixture — widely used, CRC/zlib
// checks pass). Exercises the image-upload path including the fileviewer's
// watermark-stamping flow end-to-end, so a regression in image handling
// fails this test loudly.
//
// Source: the canonical "smallest valid PNG" (67 bytes) — base64 form is
// `iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII=`.
// Hex-literal spelling below keeps the test self-contained and easy to
// diff if bytes need swapping for a different fixture.
const ONE_PIXEL_PNG = Uint8Array.from(
  Buffer.from(
    'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII=',
    'base64',
  ),
);

async function mintAndViewConnectorResource(
  resourceId: string,
  expiresAt: string,
): Promise<{ qurlLink: string; view: ViewViaQurlLinkResult }> {
  const mintAndView = async () => {
    const minted = await qurl.mintConnectorView(env.UPLOAD_API_URL, resourceId, env.QURL_API_KEY, {
      expiresAt,
      oneTimeUse: true,
    });
    const view = await viewViaQurlLink(minted.qurl_link);
    return { qurlLink: minted.qurl_link, view };
  };

  try {
    return await mintAndView();
  } catch (err) {
    // A one-time qurl.link can be consumed by the browser/knock path even if
    // Playwright misses the tunnel view response during a transient tunnel delay.
    // Only that timeout gets a fresh mint; connector errors and non-200 views
    // still fail immediately so the smoke remains a real deploy gate.
    if (!(err instanceof TunnelViewTimeoutError)) {
      throw err;
    }
    // eslint-disable-next-line no-console
    console.warn('file-revoke: tunnel view timed out; reminting a fresh one-time view link');
    return await mintAndView();
  }
}

describe('File Revoke', () => {
  test('upload file → view 200 → revoke → resource status revoked', async () => {
    const upload = await qurl.uploadFile(
      env.UPLOAD_API_URL,
      { bytes: ONE_PIXEL_PNG, filename: 'revoke-test.png', mime: 'image/png' },
      env.QURL_API_KEY,
    );
    tracked.track(upload.resource_id);

    // Mint the RECIPIENT view, then drive the real knock against THAT link.
    // Why not reuse upload.qurl_link: in render-at-mint mode the /upload qurl_link
    // is the connector's "never-shared per-upload qURL" (its knock 404s).
    // Recipients only ever reach content through a per-recipient view minted via
    // POST /api/mint_link, whose knock resolves to
    // r_<tunnel>.qurl.site/views/<mint-id>. expires_at is required by
    // render-at-mint (1h here; the connector clamps to its own cap).
    const expiresAt = new Date(Date.now() + 60 * 60 * 1000).toISOString();

    // View through the REAL recipient path: qurl.link → NHP knock → tunnel view.
    // #1111 decommissioned the legacy fileviewer host, so only a real browser
    // completes the SPA-driven knock (the SPA reads the #at_ fragment in JS). A
    // 200 means the baked image served end-to-end through the tunnel.
    const { view } = await mintAndViewConnectorResource(upload.resource_id, expiresAt);
    expect(view.status).toBe(200);

    // Resource-level canary guarding the post-revoke lifecycle assertion.
    const pre = await qurl.pollResourceStatus(
      env.MINT_API_URL, env.QURL_API_KEY, upload.resource_id, (s) => s !== null,
    );
    qurl.assertStatusVisible(pre, `pre-revoke uploaded resource_id ${upload.resource_id}`);

    const revoked = await tracked.revoke(upload.resource_id);
    expect(revoked).toBe(true);

    // Canonical post-revoke assertion: management reads retain the resource and
    // report `revoked` (matches smoke.test.ts). (Synchronous delete-on-revoke of the
    // baked object is tracked separately under the render-at-mint cleanup path.)
    const status = await qurl.getResourceStatus(
      env.MINT_API_URL, env.QURL_API_KEY, upload.resource_id,
    );
    expect(status.status).toBe('revoked');
    // Generous timeout: connector mint + headless-browser knock (cold chromium
    // launch + navigation + the helper's own 30s tunnel-view budget) on CI.
  }, 120_000);

  test('distinct-per-viewer watermark + `_` route-label SNI on the tunnel', async () => {
    // ONE upload → TWO minted recipient views. The whole point of render-at-mint:
    // each recipient gets their OWN baked `views/<mint-id>` object, so a leak is
    // traceable to the specific recipient.
    const upload = await qurl.uploadFile(
      env.UPLOAD_API_URL,
      { bytes: ONE_PIXEL_PNG, filename: 'distinct-viewer.png', mime: 'image/png' },
      env.QURL_API_KEY,
    );
    tracked.track(upload.resource_id);

    // Mint and view two independent one-time views for the SAME resource.
    // expires_at is required by render-at-mint (1h; the connector clamps to its
    // own cap). Sequential, NOT Promise.all: this mirrors the proven single-mint
    // path exactly rather than introducing a concurrent per-resource double-bake
    // the gate has never exercised.
    const expiresAt = new Date(Date.now() + 60 * 60 * 1000).toISOString();
    const viewedA = await mintAndViewConnectorResource(upload.resource_id, expiresAt);
    const viewedB = await mintAndViewConnectorResource(upload.resource_id, expiresAt);
    // Each mint MUST yield its own distinct qurl.link, or the two viewers below
    // would just be re-driving the same one-time link (and the second would 404).
    expect(viewedA.qurlLink).not.toBe(viewedB.qurlLink);

    // Both helper calls drove the real recipient path (qurl.link → NHP knock →
    // tunnel view). Serial execution keeps peak memory to one browser and avoids
    // the two knocks racing for the shared CI egress IP's WAF budget.
    const { view: viewA } = viewedA;
    const { view: viewB } = viewedB;

    // Both recipients get a served (200) per-recipient object end-to-end.
    expect(viewA.status).toBe(200);
    expect(viewB.status).toBe(200);

    // ── Distinct-per-viewer watermark (THE leak-traceability guarantee) ──
    // Each view resolves to its OWN `views/<mint-id>` object. The mint-id is a
    // 128-bit crypto-random hex token, distinct per mint by construction, so two
    // mints for the same upload land on two DIFFERENT baked objects — which IS how
    // #1027 defines "distinct watermarks: different `views/` objects". This is the
    // strongest OBSERVABLE assertion: the watermark is an invisible neural
    // meta-seal mark (`watermark_metaseal_enabled` in sandbox), so a rendered-byte
    // hash can't witness distinctness — on a 1×1 transparent fixture the mark has
    // no spatial capacity and the re-encode could be byte-identical OR differ only
    // by encoder nondeterminism, so a byte comparison would be meaningless or flaky
    // either way. Forensic bit-level distinctness is the watermark service's
    // /detect contract (covered by the connector's verify-watermark tooling), not
    // something this browser harness can see. The distinct `views/<mint-id>` object
    // key is the pinned guarantee here.
    const mintIdA = mintIdFromTunnelViewUrl(viewA.url);
    const mintIdB = mintIdFromTunnelViewUrl(viewB.url);
    expect(mintIdA).toMatch(/^[0-9a-f]+$/); // hex mint-id (mintid.go: isHexMintID)
    expect(mintIdB).toMatch(/^[0-9a-f]+$/);
    expect(mintIdA).not.toBe(mintIdB); // ← distinct per-recipient `views/` objects

    // ── `_`-bearing resource-id host resolves over DNS + TLS-SNI ──
    // Both views are served from the single wildcard `r_<route-label>.qurl.site`
    // host. The routing label carries `_` (the `r_` prefix); a 200 proves DNS + the
    // wildcard cert presenting via SNI for an `_`-bearing label end-to-end — the
    // backstop for the relaxed `view_domain` charset. (Asserted on both resolved
    // URLs since they share the tunnel host.) NOTE: the live sandbox tunnel host id
    // contains `_` but no `-`; the `-` half of the charset is not exercised by the
    // live host — see the PR body.
    for (const url of [viewA.url, viewB.url]) {
      const host = new URL(url).hostname;
      expect(host).toMatch(/^r_[a-z0-9_-]+\.qurl\.site/);
      expect(host).toContain('_'); // the relaxed-charset character, resolving + TLS-valid
    }

    // Cleanup: revoke the shared resource (kills both views' token chains).
    const revoked = await tracked.revoke(upload.resource_id);
    expect(revoked).toBe(true);
    const status = await qurl.getResourceStatus(
      env.MINT_API_URL, env.QURL_API_KEY, upload.resource_id,
    );
    expect(status.status).toBe('revoked');
    // Generous timeout: two connector mints + TWO sequential cold-chromium knocks
    // with one possible remint/retry each (4 total 30s tunnel-view budgets) +
    // revoke, on CI.
  }, 240_000);

  test('a consumed one-time link does not serve a second knock (single-use enforced)', async () => {
    // THE knock-driven enforcement guard for one-time links. The URL-mint
    // suites (smoke/concurrency) can only exercise the bare-fetch path —
    // the SPA knock that CONSUMES a use is client-side JS, so their
    // status checks pin counter coherence, not enforcement (a bare GET
    // may never advance use_count; URL-target knock coverage is #951).
    // This test drives the real recipient flow twice: the first knock
    // serves the view (consuming the link); a second knock on the SAME
    // link must not serve.
    const upload = await qurl.uploadFile(
      env.UPLOAD_API_URL,
      { bytes: ONE_PIXEL_PNG, filename: 'single-use.png', mime: 'image/png' },
      env.QURL_API_KEY,
    );
    tracked.track(upload.resource_id);

    const expiresAt = new Date(Date.now() + 60 * 60 * 1000).toISOString();
    const minted = await qurl.mintConnectorView(env.UPLOAD_API_URL, upload.resource_id, env.QURL_API_KEY, {
      expiresAt,
      oneTimeUse: true,
    });

    const first = await viewViaQurlLink(minted.qurl_link);
    expect(first.status).toBe(200);

    // Both rejection messages are valid "did not serve" shapes (knock
    // rejected → no …/views/ response at all, or a non-200 view). A
    // RESOLVING second view fails this assertion — the exact
    // reusable-links bug. Message-narrowed so an unrelated goto/network
    // failure still reds instead of false-passing as "not served".
    // Reduced budget for the negative arm: a real view lands in seconds
    // (tunnelView.ts's sandbox-proven note), so 20s amply proves "did
    // not serve" without spending the default 30s on a pass-by-timeout.
    await expect(
      viewViaQurlLink(minted.qurl_link, { timeoutMs: 20_000 }),
    ).rejects.toThrow(/no tunnel-view response|tunnel-view returned/);

    // Cleanup as assertion (tracked.revoke also syncs the afterAll ledger).
    const revoked = await tracked.revoke(upload.resource_id);
    expect(revoked).toBe(true);
    // Generous timeout: upload + connector mint + one served cold-chromium
    // knock + one negative knock that waits out its full 20s budget + revoke.
  }, 150_000);

  test('double revoke on file is idempotent', async () => {
    const upload = await qurl.uploadFile(
      env.UPLOAD_API_URL,
      { bytes: ONE_PIXEL_PNG, filename: 'double-revoke.png', mime: 'image/png' },
      env.QURL_API_KEY,
    );
    tracked.track(upload.resource_id);

    const first = await tracked.revoke(upload.resource_id);
    expect(first).toBe(true);

    // Second revoke should not throw. `revokeLink` is typed Promise<boolean>
    // and the smoke/link-lifecycle sibling test uses the same "does not
    // throw" contract. Covered by `resolves.not.toThrow()` for the
    // explicit contract expression.
    await expect(
      qurl.revokeLink(env.MINT_API_URL, env.QURL_API_KEY, upload.resource_id),
    ).resolves.not.toThrow();

    // Resource is still revoked after the redundant call.
    const status = await qurl.getResourceStatus(
      env.MINT_API_URL, env.QURL_API_KEY, upload.resource_id,
    );
    expect(status.status).toBe('revoked');
  });
});
