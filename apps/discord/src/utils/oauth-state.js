// Shared OAuth state-signing machinery for the two HMAC-signed state
// flows: the GitHub OAuth binding in commands.js (state = `nonce.sig`
// over `${discordId}:${nonce}`) and the qURL OAuth setup flow in
// utils/qurl-oauth-state.js (state = `b64url(JSON).sig`). The payload
// shapes stay distinct at each call site by design — the differing
// payloads plus the qURL side's HMAC-covered `kind` field make
// cross-purpose forgery impossible. What was drifting between them was
// the SECRET-RESOLUTION machinery (env precedence, test-harness
// fallback, warn-once), so that lives here exactly once.
//
// Each flow constructs its own signer via createStateSigner(), passing:
//   flowLabel        — human prose for error/warn messages, e.g.
//                      'qURL OAuth state' (brand spelling) or 'OAuth state'.
//   secretConfigKeys — ordered config-key precedence for the flow's
//                      dedicated secret(s), highest first. The first
//                      truthy value wins. GITHUB_CLIENT_SECRET is
//                      appended internally as the always-last fallback
//                      (env-parity with the original GitHub OAuth
//                      signer) — callers cannot omit or reorder it.
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

// Minimum acceptable secret length — per round-9 #4. 32 chars is the
// floor for an HMAC-SHA256 secret with adequate entropy (matches the
// `0`.repeat(64) test fixture's order of magnitude, well below the
// 128-char hex secrets ops actually provisions). A 4-char accidental
// value would HMAC just fine with no security; reject upfront. Applies
// to whichever key in the resolution order wins — dedicated secrets
// and the GITHUB_CLIENT_SECRET fallback alike (Auth0 / GitHub
// provision 32+ char client secrets by default, but a manual
// /placeholder env on a misconfigured dev box would slip past
// otherwise). Historically only the qURL flow enforced this; the
// GitHub flow silently accepted any length — extracting the shared
// resolver closed that drift.
const MIN_STATE_SECRET_LENGTH = 32;

// Build a signer for one OAuth state flow. Returns { sign, verify }
// closures over the flow's secret resolution:
//
//   sign(data)           → hex HMAC-SHA256 signature over `data`.
//   verify(data, sigHex) → timing-safe boolean; false (never throws)
//                          when sigHex is malformed hex or wrong length.
//
// Secret precedence (highest first):
//   1. secretConfigKeys, in order — flow-dedicated secrets, so ops can
//      rotate one flow's signer without invalidating the other's
//      in-flight states (blast-radius isolation; see #184 for the qURL
//      chain). Rotation playbook: provision the new var in SSM, deploy
//      (the dual-read happens automatically); once every replica has
//      the new var, drop the old one. The state TTL bounds the "old
//      links don't validate against the new key" window — no separate
//      dual-key reader needed.
//   2. GITHUB_CLIENT_SECRET — last-ditch fallback for backward-compat
//      with deployments that predate the dedicated secrets.
//   3. Test fallback — per-SIGNER random secret for jest only. Random
//      (not static) so even inside the harness there's no key that, if
//      accidentally shipped, would be forgeable; per-signer so the two
//      flows can't accidentally verify each other's fallback-signed
//      states. Tests that need a stable secret set the env var
//      explicitly. Gated on NODE_ENV=test AND (JEST_WORKER_ID or
//      CI=true): merely setting NODE_ENV=test by accident in a deployed
//      env doesn't enable the forgeable key — everywhere else throws
//      hard so a misconfig is loud.
function createStateSigner({ flowLabel, secretConfigKeys }) {
  // A missing/empty key list would silently resolve straight to
  // GITHUB_CLIENT_SECRET — a precedence change, not a cosmetic bug —
  // so it fails loudly at construction. flowLabel only feeds message
  // prose; a bad value surfaces legibly in the first error string.
  if (!Array.isArray(secretConfigKeys) || secretConfigKeys.length === 0) {
    throw new TypeError('createStateSigner: secretConfigKeys must be a non-empty array');
  }
  const resolutionOrder = [...secretConfigKeys, 'GITHUB_CLIENT_SECRET'];
  let warnedFallback = false;
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
      throw new Error(`Refusing to mint ${flowLabel}: ${key} is shorter than ${MIN_STATE_SECRET_LENGTH} chars (got ${secret.length}). Provision a 64+ char value in SSM.`);
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
  // on why these drift). Its falsy-input guard means empty `data` never
  // verifies — both callers regex-validate their inputs as non-empty
  // before signing, so the guard is unreachable defense-in-depth here.
  function verify(data, sigHex) {
    return verifyHmacSha256(data, stateSecret(), sigHex);
  }

  return { sign, verify };
}

module.exports = {
  createStateSigner,
  MIN_STATE_SECRET_LENGTH,
};
