// Simple structured logger with timestamps

const levels = {
  error: 0,
  warn: 1,
  info: 2,
  debug: 3,
};

const currentLevel = levels[process.env.LOG_LEVEL] ?? levels.info;

function formatTimestamp() {
  return new Date().toISOString();
}

// Redact common secret-ish field names anywhere in a meta object before
// stringifying. Defense-in-depth against a caller accidentally logging
// `{ apiKey, token, password, ... }`.
// Substrings that should never appear unredacted in logs. Matched case-
// insensitively against the key name via includes(), so a future field
// named refreshToken / bearerToken / apiSecret / myPassword is auto-caught.
// See REDACT_EXACT_KEYS below for the exact-match alternative.
const REDACT_SUBSTRINGS = [
  'token', 'secret', 'password', 'authorization', 'apikey', 'api_key',
];

// Exact-match keys (not substring): names that need explicit coverage
// because substring matching would either over-catch or under-catch them.
// Two categories live here today:
//   1. Content-derived hash names (`hash`, `md5`, `sha*`, `digest`,
//      `checksum`, `content_hash`, `body_hash`). The connector's md5 of
//      uploaded content is the original motivator (per-call-site
//      truncation via md5Prefix() in connector.js); the rest are
//      forward-looking guards. Substring on `hash` would blank
//      `commitHash` / `webhookHash` / `md5_prefix`.
//   2. Substring-uncovered audit-secret-key entries. `private_key` is
//      in AUDIT_SECRET_KEYS but doesn't match any REDACT_SUBSTRINGS rule
//      (no `private_` / `_key` substring is broad-safe). Pinned here so
//      both pathways treat it identically.
// Mirrored in AUDIT_SECRET_KEYS below — sets kept in sync by hand (#221).
const REDACT_EXACT_KEYS = new Set([
  'hash',
  'md5', 'sha1', 'sha256', 'sha512',
  'digest', 'checksum',
  'content_hash', 'body_hash',
  'private_key',
]);

// Internal: callers must pass strings. Only invoked from Object.entries()
// loops, which yield string keys. If this becomes externally-used, restore
// a String() coercion or guard non-string inputs.
function shouldRedact(key) {
  const k = key.toLowerCase();
  if (REDACT_EXACT_KEYS.has(k)) return true;
  return REDACT_SUBSTRINGS.some(s => k.includes(s));
}

// Exact key names that audit() refuses to emit silently — these are the
// classic secret-bearers a caller is most likely to leak by accident.
// Exact-match (not substring) so legitimate audit dimensions like
// `tokens_minted` or `token_count` don't trigger false-positive warns.
// REDACT_SUBSTRINGS is too aggressive for audit metadata: the bypass
// exists precisely because legitimate dimensions can contain those
// substrings.
const AUDIT_SECRET_KEYS = new Set([
  'token', 'secret', 'password', 'authorization', 'apikey', 'api_key',
  'auth_token', 'access_token', 'refresh_token', 'bearer_token',
  'session_token', 'private_key', 'client_secret', 'webhook_secret',
  // Content-derived hash names — see REDACT_EXACT_KEYS above for rationale.
  // Kept in sync by hand today; consolidation tracked in #221.
  'hash', 'md5', 'sha1', 'sha256', 'sha512',
  'digest', 'checksum',
  'content_hash', 'body_hash',
]);

// Internal: callers must pass strings (same contract as shouldRedact).
function isAuditSecretKey(key) {
  return AUDIT_SECRET_KEYS.has(key.toLowerCase());
}

// Returns a cloned meta value with any object key in AUDIT_SECRET_KEYS
// replaced by '[REDACTED]'. Recurses to depth 5 with redact()'s array
// handling so a buried `{ context: { auth_token } }` is also covered.
// Also returns `secretKeys`, an array of every offending key observed
// (deduped, in encounter order) so audit() can name all of them in
// the warn line — partial reporting would let a caller fix one key
// and re-run only to discover another the next time.
//
// Note on the contract shift: the original audit() bypassed redact()
// entirely because the legacy substring-match REDACT_SUBSTRINGS would
// blank legitimate dimensions like `tokens_minted`. AUDIT_SECRET_KEYS
// uses exact-match against a closed set of canonical secret names
// (auth_token, api_key, password, ...), none of which collide with any
// known legitimate audit dimension. So we can redact the value of an
// offending key without risk of corrupting a metric — and that's
// strictly safer than emitting the secret verbatim and relying on a
// dashboard sweep to catch it.
function redactAuditSecrets(value, depth = 0, secretKeys = []) {
  if (depth > 5 || value == null || typeof value !== 'object') {
    return { value, secretKeys };
  }
  if (Array.isArray(value)) {
    const arr = value.map((v) => redactAuditSecrets(v, depth + 1, secretKeys).value);
    return { value: arr, secretKeys };
  }
  const out = {};
  for (const [k, v] of Object.entries(value)) {
    if (isAuditSecretKey(k)) {
      // Blank non-empty strings; recurse into non-null objects/arrays so a
      // `{ auth_token: { ... nested auth_token ... } }` accident still has
      // its inner sensitive keys examined. Other primitives (numbers, null,
      // undefined) pass through — they can't carry a usable secret and
      // zero-string-replace would lose type info.
      //
      // Note: the recursion uses isAuditSecretKey (exact-match), NOT the
      // wider shouldRedact (substring) rule. An inner `myToken` would
      // therefore SURVIVE here even though it'd be blanked by redact().
      // Intentional — audit dimensions like `tokens_minted` must not be
      // redacted via substring.
      if (typeof v === 'string' && v.length > 0) out[k] = '[REDACTED]';
      else if (v != null && typeof v === 'object') out[k] = redactAuditSecrets(v, depth + 1, secretKeys).value;
      else out[k] = v;
      if (!secretKeys.includes(k)) secretKeys.push(k);
    } else {
      out[k] = redactAuditSecrets(v, depth + 1, secretKeys).value;
    }
  }
  return { value: out, secretKeys };
}

function redact(value, depth = 0) {
  if (depth > 5 || value == null) return value;
  if (Array.isArray(value)) return value.map(v => redact(v, depth + 1));
  if (typeof value !== 'object') return value;
  const out = {};
  for (const [k, v] of Object.entries(value)) {
    if (shouldRedact(k)) {
      // Blank non-empty strings; recurse into non-null objects/arrays so
      // a `{ hash: { token: 'real' } }` accident still has its inner keys
      // examined. Other primitives (numbers, null, etc.) pass through.
      if (typeof v === 'string' && v.length > 0) out[k] = '[REDACTED]';
      else if (v != null && typeof v === 'object') out[k] = redact(v, depth + 1);
      else out[k] = v;
    } else {
      out[k] = redact(v, depth + 1);
    }
  }
  return out;
}

// Strip ASCII control chars (incl. \r\n) from the message so a caller that
// interpolates attacker-controlled data (e.g. an x-github-event header,
// a webhook payload field) cannot inject fake log lines. Meta is already
// JSON-encoded so its newlines are escaped; message is raw.
// eslint-disable-next-line no-control-regex
const CONTROL_CHARS_RE = /[\x00-\x1f\x7f]/g;
function sanitizeMessage(message) {
  if (typeof message !== 'string') message = String(message);
  return message.replace(CONTROL_CHARS_RE, ' ');
}
function formatMessage(level, message, meta = {}) {
  const safe = redact(meta);
  const metaStr = Object.keys(safe).length > 0 ? ` ${JSON.stringify(safe)}` : '';
  return `[${formatTimestamp()}] ${level.toUpperCase()}: ${sanitizeMessage(message)}${metaStr}`;
}

const logger = {
  error(message, meta = {}) {
    if (currentLevel >= levels.error) {
      console.error(formatMessage('error', message, meta));
    }
  },

  warn(message, meta = {}) {
    if (currentLevel >= levels.warn) {
      console.warn(formatMessage('warn', message, meta));
    }
  },

  info(message, meta = {}) {
    if (currentLevel >= levels.info) {
      console.log(formatMessage('info', message, meta));
    }
  },

  debug(message, meta = {}) {
    if (currentLevel >= levels.debug) {
      console.log(formatMessage('debug', message, meta));
    }
  },

  // Structured audit event. Emitted as a JSON-only log line (no
  // human-readable preamble) so CloudWatch Logs metric filters can
  // pattern-match `{ $.audit.event = "<name>" }` and dimension by
  // `$.audit.agent`. The terraform filters at
  // qurl-integrations-infra/qurl-bot-discord/terraform/main.tf
  // pick these up.
  //
  // `agent` is hard-coded to "discord" for this codebase. Future
  // integrations (Slack, Teams, CLI, web/portal) emit their own
  // constant value so a single CloudWatch metric Minted{Agent} can
  // attribute mints across the whole product. The string set is
  // canonical: "discord" | "slack" | "teams" | "cli" | "web" | "api".
  //
  // Audit lines bypass currentLevel — they're observability, not
  // debug noise. They also bypass the top-level redact() pass on
  // `meta` because that pass matches on KEY-name SUBSTRING and would
  // blank legitimate dimensions like `tokens_minted` or `token_count`.
  // Instead, audit() runs redactAuditSecrets() — same shape but uses
  // exact-match against a closed AUDIT_SECRET_KEYS set so only canonical
  // secret-bearer names (auth_token, api_key, password, ...) are
  // blanked. Callers SHOULD still avoid passing secrets in meta —
  // the AUDIT_EVENTS allowlist in constants.js documents the small,
  // pre-vetted vocabulary (send_id, kind, count, expires_in, success,
  // total). The redaction here is belt-and-suspenders: an error log
  // fires too so a misbehaving caller is catchable in dashboards.
  audit(event, meta = {}) {
    // Default param only fires for `undefined`. A caller passing `null`
    // (easy mistake from optional chaining: `someObj?.meta`) would
    // otherwise crash `Object.entries(null)` BEFORE the protected
    // try/catch around JSON.stringify, defeating the "audit never
    // breaks user flow" contract. Also coerces arrays — typeof [] is
    // 'object' so the typeof check alone would let `audit('x', [1,2])`
    // through and produce a confusing `{0:1,1:2,event,agent}` payload.
    if (meta == null || typeof meta !== 'object' || Array.isArray(meta)) meta = {};
    // Targeted secret redaction: walk meta, replace the value of any
    // key in AUDIT_SECRET_KEYS with '[REDACTED]', return the first
    // offending key name so the operator log below can name it. Safe
    // to redact because exact-match doesn't false-positive on
    // legitimate dimensions (proven by the tokens_minted test).
    const { value: cleanedMeta, secretKeys } = redactAuditSecrets(meta);
    if (secretKeys.length > 0) {
      // Logged via console.error so it surfaces at error level in
      // CloudWatch (the logger has no separate warn channel that
      // emits at error severity). Defense-in-depth alongside the
      // value redaction above — the redacted payload still emits,
      // but the operator can grep the error log to find every
      // misbehaving call site and remove the key from meta. ALL
      // offending keys are listed so a caller fixing the first
      // doesn't have to re-run to discover the second.
      const namedKeys = secretKeys.map(k => `"${sanitizeMessage(k)}"`).join(', ');
      console.error(`[${formatTimestamp()}] ERROR: logger.audit received secret-shaped key(s) [${namedKeys}] in event=${sanitizeMessage(event)}; value(s) redacted in emitted payload — caller must remove from meta`);
    }
    // Spread cleaned meta first, then pin event + agent last so a
    // caller passing `agent` or `event` in meta cannot overwrite the
    // canonical value the CloudWatch filters key off of.
    const auditPayload = { ...cleanedMeta, event, agent: 'discord' };
    // Single-line JSON, parseable by `{ $.audit.event = "..." }`
    // CloudWatch filter syntax. No timestamp prefix — the JSON has
    // its own ts field. console.log adds a trailing newline.
    //
    // Wrap JSON.stringify in try/catch — a circular reference, BigInt,
    // or other non-serializable value in meta would otherwise throw
    // out of audit() and into the caller, which on the per-recipient
    // batchSettled callback would fail an entire DM. Audit must never
    // break the user-visible flow; degrade to an error log instead.
    try {
      console.log(JSON.stringify({
        audit: auditPayload,
        ts: formatTimestamp(),
      }));
    } catch (err) {
      // Two-tier degradation: emit a minimal audit line with a fixed
      // synthetic event so CloudWatch metric filters can pattern-match
      // `{ $.audit.event = "audit_serialization_failed" }` and surface
      // the gap. The fallback payload only contains primitive strings
      // — no caller meta — so it cannot itself trip JSON.stringify.
      // If even this throws (effectively impossible), fall through to
      // a plain error log so an operator still sees something.
      try {
        console.log(JSON.stringify({
          audit: {
            event: 'audit_serialization_failed',
            agent: 'discord',
            original_event: String(event),
            reason: sanitizeMessage(err && err.message),
          },
          ts: formatTimestamp(),
        }));
      } catch (fallbackErr) {
        console.error(`[${formatTimestamp()}] ERROR: logger.audit serialization failed event=${sanitizeMessage(event)} reason=${sanitizeMessage(err && err.message)} fallback_reason=${sanitizeMessage(fallbackErr && fallbackErr.message)}`);
      }
    }
  },
};

module.exports = logger;
// Test-only: exposes the redact constants so a drift-guard test can iterate
// the live set rather than duplicate it. Not part of the public API; do
// not consume from production code. Defensive copies — a buggy test must
// not be able to mutate the live Sets and corrupt subsequent log calls.
module.exports.__testExports = {
  REDACT_EXACT_KEYS: new Set(REDACT_EXACT_KEYS),
  AUDIT_SECRET_KEYS: new Set(AUDIT_SECRET_KEYS),
};
