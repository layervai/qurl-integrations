/**
 * QURL revoke end-to-end smoke test.
 *
 * Validates the one invariant the bot's /qurl revoke path depends on:
 * calling `DELETE /v1/resources/{id}` against the deployed QURL API
 * actually revokes the qurl — subsequent status checks return 404.
 *
 * This is the failure class covered here, not tested elsewhere in the
 * post-deploy smoke filter (`smoke|google-maps|discord-channels`):
 * - A QURL API regression that silently accepts the DELETE but keeps
 *   the resource live would leave the /qurl revoke dropdown filter
 *   (issue: already-revoked sends stay visible) *technically* correct
 *   at the DB layer while being meaningless in practice.
 * - Discord-dropdown rendering cannot be validated from smoke (no
 *   slash-command invocation without a user token; see PR #101).
 * - Scope-wise this is the narrowest test that proves the user-visible
 *   revoke guarantee end-to-end.
 *
 * URL-based qurls (not file-based) are used here to keep the test fast
 * and decoupled from the connector. File-based revoke is covered by
 * file-revoke.test.ts, which currently sits outside the smoke filter.
 */

import { loadEnv } from '../helpers/env';
import { mintLink, getLinkStatus, revokeLink } from '../helpers/qurl-api';

describe('QURL revoke (smoke)', () => {
  const env = loadEnv();

  it('DELETE /v1/resources/{id} marks the resource revoked (subsequent status check fails)', async () => {
    // Mint a URL-based qurl that points at an arbitrary public URL.
    // max_uses=10 so the test doesn't accidentally self-consume the
    // link by a successful access *before* the revoke fires.
    const minted = await mintLink(env.MINT_API_URL, env.QURL_API_KEY, {
      target_url: 'https://example.com/e2e-revoke-smoke',
      expires_in: '1h',
      description: 'e2e revoke-smoke test',
      max_uses: 10,
    });
    expect(minted.resource_id).toBeTruthy();

    // Sanity-check the resource exists before we revoke it. If this
    // fails the revoke assertion below is meaningless — fail loud with
    // a clear message rather than letting the later expect pass for
    // the wrong reason.
    const preStatus = await getLinkStatus(env.MINT_API_URL, env.QURL_API_KEY, minted.resource_id);
    expect(preStatus).toBeTruthy();

    const revoked = await revokeLink(env.MINT_API_URL, env.QURL_API_KEY, minted.resource_id);
    expect(revoked).toBe(true);

    // Canonical revoke assertion: getLinkStatus throws on non-2xx, and
    // the deployed QURL API returns 404 for revoked resources. Catching
    // the error lets us inspect the status in the message so a
    // different failure mode (500, 403) produces a diagnosable report
    // rather than just "threw".
    try {
      await getLinkStatus(env.MINT_API_URL, env.QURL_API_KEY, minted.resource_id);
      throw new Error(
        `getLinkStatus unexpectedly succeeded on revoked resource ${minted.resource_id} — ` +
        `the QURL API is accepting DELETE but keeping the resource queryable.`,
      );
    } catch (err) {
      const msg = String(err instanceof Error ? err.message : err);
      expect(msg).toMatch(/404/);
    }
  });
});
