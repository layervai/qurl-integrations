/**
 * File upload → revoke → status-404 E2E flow.
 *
 * Covers the revoke path for actual file resources (images/PDFs/etc.) — the
 * other revoke tests (smoke.test.ts, link-lifecycle.test.ts) only exercise
 * URL-based qURLs (`target_url=https://example.com/...`). This file fills
 * the gap by uploading a real file via the connector's `/upload` endpoint,
 * minting a per-recipient render-at-mint view (`POST /api/mint_link`) and
 * confirming it serves through the tunnel, then revoking by resource_id and
 * confirming the qURL resource is marked revoked via getLinkStatus.
 *
 * Revoke semantics: `DELETE /v1/resources/{id}` kills the qURL-layer token
 * chain (the qurl.link → NHP knock → tunnel view). #1111 removed the legacy
 * md5-addressed fileviewer URL, so under the render-at-mint tunnel the only
 * access is the minted qurl.link, which this revoke invalidates. The canonical
 * revoke assertion is `getLinkStatus` returning 404, matching smoke.test.ts.
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
 * "distinct-per-viewer watermark + `_`/`-` resource-id SNI" test below:
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
 *   - SNI / DNS for the `r_<id>` host: the per-recipient view is served from a
 *     single wildcard `r_<id>.qurl.site` label whose id carries `_` (the `r_`
 *     prefix). A 200 through it proves DNS + the wildcard cert presenting via
 *     TLS-SNI for an `_`-bearing label end-to-end — the real backstop for the
 *     relaxed `view_domain` charset.
 */

import * as dotenv from 'dotenv';
import * as path from 'path';
dotenv.config({ path: path.resolve(__dirname, '..', '.env') });

import { loadEnv } from '../helpers/env';
import * as qurl from '../helpers/qurl-api';
import { mintIdFromTunnelViewUrl, viewViaQurlLink } from '../helpers/tunnelView';

const env = loadEnv();

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

describe('File Revoke', () => {
  test('upload file → view 200 → revoke → getLinkStatus 404', async () => {
    const upload = await qurl.uploadFile(
      env.UPLOAD_API_URL,
      { bytes: ONE_PIXEL_PNG, filename: 'revoke-test.png', mime: 'image/png' },
      env.QURL_API_KEY,
    );
    expect(upload.resource_id).toMatch(/^r_/);

    // Mint the RECIPIENT view, then drive the real knock against THAT link.
    // Why not reuse upload.qurl_link: in render-at-mint mode the /upload qurl_link
    // is the connector's "never-shared per-upload qURL" (its knock 404s).
    // Recipients only ever reach content through a per-recipient view minted via
    // POST /api/mint_link, whose knock resolves to
    // r_<tunnel>.qurl.site/views/<mint-id>. expires_at is required by
    // render-at-mint (1h here; the connector clamps to its own cap).
    const expiresAt = new Date(Date.now() + 60 * 60 * 1000).toISOString();
    const minted = await qurl.mintConnectorView(env.UPLOAD_API_URL, upload.resource_id, env.QURL_API_KEY, {
      expiresAt,
      oneTimeUse: true,
    });

    // View through the REAL recipient path: qurl.link → NHP knock → tunnel view.
    // #1111 decommissioned the legacy fileviewer host, so only a real browser
    // completes the SPA-driven knock (the SPA reads the #at_ fragment in JS). A
    // 200 means the baked image served end-to-end through the tunnel.
    const view = await viewViaQurlLink(minted.qurl_link);
    expect(view.status).toBe(200);

    // #950 canary, resource_id side, guarding this file's post-revoke
    // 404 assertions (rationale in qurl-api.ts's getLinkStatus doc).
    const pre = await qurl.pollLinkStatus(
      env.MINT_API_URL, env.QURL_API_KEY, upload.resource_id, (s) => s !== null,
    );
    qurl.assertStatusVisible(pre, `pre-revoke uploaded resource_id ${upload.resource_id}`);

    const revoked = await qurl.revokeLink(env.MINT_API_URL, env.QURL_API_KEY, upload.resource_id);
    expect(revoked).toBe(true);

    // Canonical post-revoke assertion: the resource status API reports 404 once
    // revoked (matches smoke.test.ts). (Synchronous delete-on-revoke of the
    // baked object is tracked separately under the render-at-mint cleanup path.)
    await expect(
      qurl.getLinkStatus(env.MINT_API_URL, env.QURL_API_KEY, upload.resource_id),
    ).rejects.toThrow(/404/);
    // Generous timeout: connector mint + headless-browser knock (cold chromium
    // launch + navigation + the helper's own 30s tunnel-view budget) on CI.
  }, 90_000);

  test('distinct-per-viewer watermark + `_`/`-` resource-id SNI on the tunnel', async () => {
    // ONE upload → TWO minted recipient views. The whole point of render-at-mint:
    // each recipient gets their OWN baked `views/<mint-id>` object, so a leak is
    // traceable to the specific recipient.
    const upload = await qurl.uploadFile(
      env.UPLOAD_API_URL,
      { bytes: ONE_PIXEL_PNG, filename: 'distinct-viewer.png', mime: 'image/png' },
      env.QURL_API_KEY,
    );
    expect(upload.resource_id).toMatch(/^r_/);

    // Mint two independent views for the SAME resource. expires_at is required by
    // render-at-mint (1h; the connector clamps to its own cap). One-time-use, so
    // each link is consumed by exactly one knock — matching one viewViaQurlLink call.
    // Sequential, NOT Promise.all: this mirrors the proven single-mint path exactly
    // (the existing test above, green in the sandbox gate) rather than introducing a
    // concurrent per-resource double-bake the gate has never exercised — robustness
    // for the first post-merge run, at the cost of one extra mint round-trip.
    const expiresAt = new Date(Date.now() + 60 * 60 * 1000).toISOString();
    const mintedA = await qurl.mintConnectorView(env.UPLOAD_API_URL, upload.resource_id, env.QURL_API_KEY, {
      expiresAt,
      oneTimeUse: true,
    });
    const mintedB = await qurl.mintConnectorView(env.UPLOAD_API_URL, upload.resource_id, env.QURL_API_KEY, {
      expiresAt,
      oneTimeUse: true,
    });
    // Each mint MUST yield its own distinct qurl.link, or the two viewers below
    // would just be re-driving the same one-time link (and the second would 404).
    expect(mintedA.qurl_link).not.toBe(mintedB.qurl_link);

    // Drive BOTH minted links through the real recipient path (qurl.link → NHP
    // knock → tunnel view). Sequential, NOT Promise.all: each call launches its own
    // cold chromium and these run on a single jest worker (maxWorkers:1) — serial
    // keeps peak memory to one browser and avoids the two knocks racing for the
    // shared CI egress IP's WAF budget.
    const viewA = await viewViaQurlLink(mintedA.qurl_link);
    const viewB = await viewViaQurlLink(mintedB.qurl_link);

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
    // Both views are served from the single wildcard `r_<id>.qurl.site` label. The
    // id carries `_` (the `r_` prefix); a 200 through that host proves DNS + the
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
    const revoked = await qurl.revokeLink(env.MINT_API_URL, env.QURL_API_KEY, upload.resource_id);
    expect(revoked).toBe(true);
    await expect(
      qurl.getLinkStatus(env.MINT_API_URL, env.QURL_API_KEY, upload.resource_id),
    ).rejects.toThrow(/404/);
    // Generous timeout: two connector mints + TWO sequential cold-chromium knocks
    // (each with the helper's own 30s tunnel-view budget) + revoke, on CI.
  }, 180_000);

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
    expect(upload.resource_id).toMatch(/^r_/);

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

    // Cleanup (inline, matching this file's revoke-as-assertion style).
    const revoked = await qurl.revokeLink(env.MINT_API_URL, env.QURL_API_KEY, upload.resource_id);
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

    const first = await qurl.revokeLink(env.MINT_API_URL, env.QURL_API_KEY, upload.resource_id);
    expect(first).toBe(true);

    // Second revoke should not throw. `revokeLink` is typed Promise<boolean>
    // and the smoke/link-lifecycle sibling test uses the same "does not
    // throw" contract. Covered by `resolves.not.toThrow()` for the
    // explicit contract expression.
    await expect(
      qurl.revokeLink(env.MINT_API_URL, env.QURL_API_KEY, upload.resource_id),
    ).resolves.not.toThrow();

    // Resource is still revoked after the redundant call.
    await expect(
      qurl.getLinkStatus(env.MINT_API_URL, env.QURL_API_KEY, upload.resource_id),
    ).rejects.toThrow(/404/);
  });
});
