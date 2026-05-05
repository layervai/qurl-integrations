// Auth0 id_token JWT verification with cached JWKS.
//
// Replaces the previous "decode the base64 payload, trust the TLS chain"
// pattern in routes/qurl-oauth.js. The success-page binding readout
// surfaces the email claim from the id_token as a load-bearing
// confused-deputy mitigation; making it cryptographically verified (vs
// "the response came from Auth0") removes the asterisk on that
// security claim — see PR #177 follow-up issue #178 / section B.
//
// Implementation: `jose` library, lazy-fetched JWKS cached in-process.
// The cache refreshes on a key-id miss so a quiet Auth0 key rotation
// doesn't leave us pinned on a retired key. Cache is per-Node-process;
// not shared across replicas — fine because each replica's cache
// converges independently in seconds.
const jose = require('jose');
const config = require('../config');
const logger = require('../logger');

let _jwksFn = null;

function getJwks() {
  if (_jwksFn) return _jwksFn;
  // verifyAuth0IdToken short-circuits on `!config.AUTH0_DOMAIN` BEFORE
  // calling getJwks(), so the unset case is unreachable here. No
  // defensive throw — dead code is worse than relying on the contract.
  // jose.createRemoteJWKSet returns a function that fetches + caches the
  // JWKS, with built-in key-id-miss refresh. Defaults: 5-min cache TTL,
  // 30-sec cooldown between refresh attempts on miss. Acceptable for our
  // workload (id_tokens are minted once per /qurl setup completion).
  _jwksFn = jose.createRemoteJWKSet(
    new URL(`https://${config.AUTH0_DOMAIN}/.well-known/jwks.json`),
    { cooldownDuration: 30000, cacheMaxAge: 5 * 60 * 1000 },
  );
  return _jwksFn;
}

/**
 * Verifies an Auth0 id_token's signature, issuer, audience, and expiry.
 *
 * Returns { ok: true, payload } on success, or { ok: false, reason }
 * on any verification failure. Reasons are coarse-grained on the wire
 * ("invalid"); caller logs the granular cause for triage.
 *
 * Issuer expected: `https://${AUTH0_DOMAIN}/` (Auth0's standard issuer
 * shape). Audience expected: AUTH0_CLIENT_ID (per OIDC spec, id_tokens
 * are audienced to the OAuth client, NOT to the API audience the
 * access_token uses).
 */
async function verifyAuth0IdToken(idToken) {
  if (typeof idToken !== 'string' || !idToken) {
    return { ok: false, reason: 'no_token' };
  }
  if (!config.AUTH0_DOMAIN || !config.AUTH0_CLIENT_ID) {
    return { ok: false, reason: 'auth0_not_configured' };
  }
  try {
    const { payload } = await jose.jwtVerify(idToken, getJwks(), {
      issuer: `https://${config.AUTH0_DOMAIN}/`,
      audience: config.AUTH0_CLIENT_ID,
      // Default `clockTolerance` is 0 — production Auth0 clock drift is
      // negligible. Explicit so a future "tolerate skew?" question has
      // a clear answer site.
      clockTolerance: 0,
    });
    return { ok: true, payload };
  } catch (err) {
    // Severity split: signature/claim/JWKS failures are the only signal
    // we have of a forged or wrong-tenant id_token — log warn so they
    // surface above the default prod log filter. Expiry is benign clock
    // skew and noisy under load — debug. jose error codes:
    // ERR_JWS_SIGNATURE_VERIFICATION_FAILED, ERR_JWT_EXPIRED,
    // ERR_JWT_CLAIM_VALIDATION_FAILED, ERR_JWKS_NO_MATCHING_KEY, etc.
    const benign = err?.code === 'ERR_JWT_EXPIRED';
    const log = benign ? logger.debug : logger.warn;
    log('Auth0 id_token verification failed', {
      code: err?.code, message: err?.message,
    });
    return { ok: false, reason: err?.code || 'verify_failed' };
  }
}

// Tests mock at the `jose` boundary (jest.mock('jose')) rather than
// reaching into this module's internal cache, so no test seam is
// exported. Earlier rounds had a `_setJwksFnForTesting` escape hatch
// that no test ever called — dropped per Justin's PR #177 round-9
// review item #10.
module.exports = { verifyAuth0IdToken };
