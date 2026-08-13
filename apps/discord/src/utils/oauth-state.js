// Shared OAuth state-signing machinery for HMAC-signed state flows.
// Today the qURL OAuth setup flow (utils/qurl-oauth-state.js, state =
// `b64url(JSON).sig`) is the only consumer; the factory shape is kept
// because the SECRET-RESOLUTION machinery (env precedence, test-harness
// fallback, warn-once) is the part worth centralizing, and a second
// flow should inherit it rather than hand-roll a copy. A future flow's
// payload must stay distinct from the qURL side's — its HMAC-covered
// `kind` field is what makes cross-purpose forgery impossible.
//
// Each flow constructs its own signer via createStateSigner(), passing:
//   flowLabel        — human prose for error/warn messages, e.g.
//                      'qURL OAuth state' (brand spelling).
//   secretConfigKeys — ordered config-key precedence for the flow's
//                      secret(s), highest first. The first truthy
//                      value wins.
//
// Secrets are read through config (src/config.js snapshots process.env
// at load) rather than raw process.env — config is the validating
// config module, and per-flow signing happens long after boot so there
// is no load-order constraint. The test-harness detection below is the
// documented exception: NODE_ENV / JEST_WORKER_ID / CI are runtime-
// environment probes owned by Jest/CI, not app config, and config.js
// itself reads NODE_ENV directly for the same reason.
const crypto = require('crypto');
const config = require('../config');
const logger = require('../logger');
const { verifyHmacSha256 } = require('./webhook-hardening');

// Minimum acceptable secret length. 32 chars is the
// floor for an HMAC-SHA256 secret with adequate entropy — half the
// 64-char values the documented generator (`openssl rand -hex 32`)
// and the `0`.repeat(64) test fixture produce. A 4-char accidental
// value would HMAC just fine with no security; reject upfront. Applies
// to whichever key in the resolution order wins (a manual/placeholder
// env on a misconfigured dev box would otherwise slip past).
const MIN_STATE_SECRET_LENGTH = 32;

// Build a signer for one OAuth state flow. Returns { sign, verify }
// closures over the flow's secret resolution:
//
//   sign(data)           → hex HMAC-SHA256 signature over `data`.
//   verify(data, sigHex) → timing-safe boolean; false (never throws)
//                          when sigHex is malformed hex or wrong length
//                          — but only once the secret resolves. NOT
//                          total over misconfiguration: a missing or
//                          sub-floor secret throws out of verify exactly
//                          as it does out of sign (secret resolution
//                          runs first, before sigHex is inspected) — a
//                          fail-loud 500 on the callback beats silently
//                          rejecting every legitimate state until
//                          someone notices.
//
// Secret precedence (highest first):
//   1. secretConfigKeys, in order — the flow-dedicated secret first,
//      then any legacy shared secret it supersedes (see #184 for the
//      qURL chain). Rotation playbook: provision the new var in SSM,
//      deploy (the dual-read happens automatically); once every replica
//      has the new var, drop the old one. The state TTL bounds the "old
//      links don't validate against the new key" window — no separate
//      dual-key reader needed.
//   2. Test fallback — per-SIGNER random secret for jest only. Random
//      (not static) so even inside the harness there's no key that, if
//      accidentally shipped, would be forgeable; per-signer so the two
//      flows can't accidentally verify each other's fallback-signed
//      states. Tests that need a stable secret set the env var
//      explicitly. Gated on NODE_ENV=test AND (JEST_WORKER_ID or
//      CI=true): merely setting NODE_ENV=test by accident in a deployed
//      env doesn't enable the forgeable key — everywhere else throws
//      hard so a misconfig is loud.
function createStateSigner({ flowLabel, secretConfigKeys }) {
  // A missing/empty key list would leave nothing to resolve, so every
  // sign/verify would fall through to the throw (or, inside jest, to
  // the random test fallback) — fail loudly at construction instead.
  // flowLabel only feeds message prose; a bad value surfaces legibly in
  // the first error string.
  if (!Array.isArray(secretConfigKeys) || secretConfigKeys.length === 0) {
    throw new TypeError('createStateSigner: secretConfigKeys must be a non-empty array');
  }
  const resolutionOrder = [...secretConfigKeys];
  let warnedFallback = false;
  // Computed eagerly at construction even where it's never read
  // (production, where a real secret always resolves) — two signers
  // per process at single-digit microseconds each. Deliberate: an
  // unconditional value beats a lazy-init branch inside stateSecret().
  const testFallbackSecret = crypto.randomBytes(32).toString('hex');

  // Resolved lazily on every sign/verify (not captured at construction)
  // so jest suites that mutate their mocked config between tests are
  // observed — and so the "refuse to mint" throws surface at the OAuth
  // interaction that needs the secret, not at require time of a module
  // whose flow may be dormant in this deploy mode.
  function stateSecret() {
    const key = resolutionOrder.find((k) => config[k]);
    if (!key) {
      const inTestHarness = process.env.NODE_ENV === 'test'
        && (process.env.JEST_WORKER_ID || process.env.CI === 'true');
      if (!inTestHarness) {
        throw new Error(`Refusing to mint ${flowLabel}: ${resolutionOrder.join(' or ')} must be set.`);
      }
      if (!warnedFallback) {
        logger.warn(`${flowLabel} HMAC using per-process random test fallback — set ${secretConfigKeys[0]}`);
        warnedFallback = true;
      }
      return testFallbackSecret;
    }
    const secret = config[key];
    if (secret.length < MIN_STATE_SECRET_LENGTH) {
      throw new Error(`Refusing to mint ${flowLabel}: ${key} is shorter than ${MIN_STATE_SECRET_LENGTH} chars (got ${secret.length}). Provision a value from \`openssl rand -hex 32\` in SSM.`);
    }
    return secret;
  }

  function sign(data) {
    return crypto.createHmac('sha256', stateSecret())
      .update(data)
      .digest('hex');
  }

  // Delegates the constant-time compare to the shared webhook helper
  // (third hand-rolled copy avoided; see webhook-hardening.js's header
  // on why these drift). Two semantics inherited from it, both
  // unobservable behind the callers' /^[0-9a-f]{64}$/ gates: the
  // compare runs over the ASCII-hex rendering (case-sensitive — an
  // uppercase rendering of a valid sig would be rejected, where the
  // old decoded-bytes compare accepted it), and its falsy-input guard
  // means empty `data` never verifies (both callers regex-validate
  // their inputs as non-empty before signing).
  function verify(data, sigHex) {
    return verifyHmacSha256(data, stateSecret(), sigHex);
  }

  return { sign, verify };
}

module.exports = {
  createStateSigner,
  MIN_STATE_SECRET_LENGTH,
};
