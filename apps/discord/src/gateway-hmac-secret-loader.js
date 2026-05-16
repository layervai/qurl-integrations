// Parses + validates the Pillar 3 control-channel HMAC secret.
//
// Wire shape: the SSM SecureString
// `/${project}/GATEWAY_HANDOFF_HMAC` is injected by the ECS task-def
// `secrets =` block as the env var `GATEWAY_HANDOFF_HMAC`. The decoded
// value is the JSON string `{"current": "<hex>", "previous": "<hex>"}`
// where each hex value is a 32-byte (64-char) HMAC-SHA256 key.
// `previous` is optional — null/undefined/missing all mean "no
// rotation window active" (single-key mode).
//
// Why hex-64 specifically:
//   - `crypto.createHmac('sha256', secret)` accepts any string and
//     keys on its UTF-8 bytes, so the gateway-hmac module itself
//     does not enforce hex.
//   - Operator tooling (rotation scripts, key generation,
//     break-glass procedures) standardize on 64-char hex (i.e. 32
//     raw bytes). Enforcing the shape at the loader is the
//     single chokepoint where a malformed/short secret gets caught
//     at boot rather than silently weakening the HMAC posture or
//     surfacing as a baffling verify-mismatch in prod.
//   - 32 bytes matches the SHA-256 block size; shorter keys
//     would still HMAC but reduce the brute-force space.
//
// Failure mode: throw at boot. The caller (boot in index.js) wraps
// this in the existing gracefulShutdown(1) path so the ECS task
// exits with status 1 and the task definition's restart policy
// surfaces it as a CrashLoopBackOff — the same posture every other
// secret-misconfig in this app already has.

const HEX64 = /^[0-9a-f]{64}$/i;

function fail(reason) {
  // Single error class so callers can pattern-match on .name if
  // they need to distinguish boot-misconfig from runtime errors
  // — production currently does not, but keeping the shape stable
  // future-proofs the boot-fail observability work.
  const err = new Error(`gateway-hmac-secret-loader: ${reason}`);
  err.code = 'GATEWAY_HMAC_SECRET_MALFORMED';
  throw err;
}

// `raw` is the env-var string — typically `process.env.GATEWAY_HANDOFF_HMAC`,
// injected at task launch from SSM. Tests pass it directly to
// avoid global env mutation. Returns `{current, previous}` ready
// to forward to createGatewayHmac.
function loadGatewayHmacSecret(raw) {
  if (typeof raw !== 'string' || raw.length === 0) {
    fail('GATEWAY_HANDOFF_HMAC env var is empty or missing');
  }

  let parsed;
  try {
    parsed = JSON.parse(raw);
  } catch (err) {
    // The raw value is a secret — never echo it into the error
    // string. We surface ONLY the JSON.parse error message, which
    // discloses the parse-failure position but not the bytes.
    fail(`GATEWAY_HANDOFF_HMAC is not valid JSON: ${err.message}`);
  }

  if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
    fail('GATEWAY_HANDOFF_HMAC must decode to a JSON object');
  }

  const { current, previous } = parsed;

  if (typeof current !== 'string' || !HEX64.test(current)) {
    fail('GATEWAY_HANDOFF_HMAC.current must be a 64-char hex string (32-byte HMAC key)');
  }

  // previous is optional. Accept null, undefined, or missing — all
  // mean "single-key mode" (no active rotation window). Any other
  // type is a config mistake worth failing on.
  let normalizedPrevious = null;
  if (previous != null) {
    if (typeof previous !== 'string' || !HEX64.test(previous)) {
      fail('GATEWAY_HANDOFF_HMAC.previous, if present, must be a 64-char hex string');
    }
    normalizedPrevious = previous;
  }

  return { current, previous: normalizedPrevious };
}

module.exports = {
  loadGatewayHmacSecret,
};
