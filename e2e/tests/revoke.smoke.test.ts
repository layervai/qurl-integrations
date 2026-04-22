/**
 * QURL revoke end-to-end smoke test.
 *
 * Validates the user-visible invariant that the bot's /qurl revoke path
 * depends on: calling `DELETE /v1/resources/{id}` against the deployed
 * QURL API actually revokes the qurl — follow-up accesses of the qurl
 * link fail where they previously succeeded.
 *
 * Why `accessLink` and not `getLinkStatus`: the `/v1/qurls/{id}/status`
 * endpoint is unreliable for URL-based qurls — it returns 404 for
 * freshly-minted LIVE resources as well as revoked ones (see the
 * graceful try/catch in `smoke.test.ts:78-81` acknowledging the same
 * "may not be tracked server-side for SPA links" behavior). An earlier
 * version of this test used getLinkStatus as both pre- and post-revoke
 * probe; the pre-check failed in prod (resource returning 404 when
 * still live) AND the post-check was passing tautologically (same 404
 * regardless of revocation state). accessLink hits the actual user
 * path (follow the qurl link, check HTTP status) and distinguishes
 * live from revoked reliably.
 *
 * Why this test isn't redundant with `file-revoke.test.ts`: that file
 * sits outside the current smoke filter (`smoke|google-maps|discord-channels`)
 * and uses file-based qurls via the connector. This covers URL-based
 * qurls and runs on every deploy.
 */

import { loadEnv } from '../helpers/env';
import { mintLink, accessLink, revokeLink } from '../helpers/qurl-api';

describe('QURL revoke (smoke)', () => {
  const env = loadEnv();

  it('DELETE /v1/resources/{id} makes subsequent qurl-link access fail', async () => {
    // Mint a URL-based qurl. max_uses=10 so the pre-revoke access
    // doesn't self-consume the link before we can compare it against
    // the post-revoke state.
    const minted = await mintLink(env.MINT_API_URL, env.QURL_API_KEY, {
      target_url: 'https://example.com/e2e-revoke-smoke',
      expires_in: '1h',
      description: 'e2e revoke-smoke test',
      max_uses: 10,
    });
    expect(minted.qurl_link).toBeTruthy();
    expect(minted.resource_id).toBeTruthy();

    // Pre-revoke: link is live and reachable. If this fails the post-
    // revoke assertion below is meaningless — fail loud with a status
    // snapshot rather than letting the later expect pass for the wrong
    // reason.
    const preAccess = await accessLink(minted.qurl_link);
    expect(preAccess.ok).toBe(true);

    const revoked = await revokeLink(env.MINT_API_URL, env.QURL_API_KEY, minted.resource_id);
    expect(revoked).toBe(true);

    // Canonical revoke assertion: the qurl link is no longer reachable.
    // The deployed QURL API returns 404 for revoked resources; any
    // non-2xx response is accepted here because the specific failure
    // code is a QURL-API implementation detail and the user-visible
    // invariant is just "the link stops working."
    const postAccess = await accessLink(minted.qurl_link);
    expect(postAccess.ok).toBe(false);
  });
});
