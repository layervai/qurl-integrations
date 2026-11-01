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
  // A qURL access link embeds its live bearer credential in the fragment.
  // Cover both normalized camelCase and raw SDK/storage snake_case fields,
  // including decorated names such as qurlLinkUrl / qurl_link_url.
  'qurllink', 'qurl_link',
];

// Exact-match keys (not substring) — content-hash names where substring
// would catch `commitHash` / `md5_prefix`, plus `private_key` which has
// no broad-safe substring rule. AUDIT_SECRET_KEYS extends this set.
// Go's shared observability test also parses this literal.
const REDACT_EXACT_KEYS = new Set([
  'hash',
  'md5', 'sha1', 'sha256', 'sha512',
  'digest', 'checksum',
  // body_hash covers both content-digest and HMAC interpretations;
  // either is sensitive-shaped enough to redact.
  'content_hash', 'body_hash',
  'private_key',
]);

// `String(key)` coerces — Object.entries() yields strings today, but a
// future caller (a different walker, a Map-like) might pass non-strings;
// the coercion keeps the predicate safe for free.
function shouldRedact(key) {
  const k = String(key).toLowerCase();
  if (REDACT_EXACT_KEYS.has(k)) return true;
  return REDACT_SUBSTRINGS.some(s => k.includes(s));
}

const QURL_LINK_SUBSTRINGS = REDACT_SUBSTRINGS.filter(s => s.startsWith('qurl'));

function isQurlAccessLinkKey(key) {
  const k = String(key).toLowerCase();
  return QURL_LINK_SUBSTRINGS.some(s => k.includes(s));
}

function isNonEmptyContainer(value) {
  if (Array.isArray(value)) return value.length > 0;
  return value != null && typeof value === 'object' && Object.keys(value).length > 0;
}

// Exact key names that audit() refuses to emit silently — these are the
// classic secret-bearers a caller is most likely to leak by accident.
// Exact-match for general secret names so legitimate audit dimensions like
// `tokens_minted` or `token_count` don't trigger false-positive warns. qURL
// access-link fields get a narrow substring rule in redactAuditSecrets so
// decorated link-bearing keys are also covered without blanking numeric
// dimensions such as qurl_link_count.
const AUDIT_SECRET_KEYS = new Set([
  ...REDACT_EXACT_KEYS,
  'token', 'secret', 'password', 'authorization', 'apikey', 'api_key',
  'auth_token', 'access_token', 'refresh_token', 'bearer_token',
  'session_token', 'client_secret', 'webhook_secret',
  // The Discord interaction-webhook token persisted on qurl_send_configs
  // (PR-B view counter) is a live ~15-min bearer cred. The audit path is
  // EXACT-match (not substring like REDACT_SUBSTRINGS), so the bare
  // 'token' entry above does NOT cover 'interaction_token' — name it so a
  // send-config row accidentally audit-shipped has it redacted. Defense-
  // in-depth: getSendConfig already strips it from its return, and the
  // info/warn/debug path's substring redactor catches it; this closes the
  // audit serializer too.
  'interaction_token',
  // guild_configs' encrypted API key attribute; setGuildApiKey's UPDATED_OLD
  // response carries it beside the admin-change audit fields. Audit path only:
  // REDACT_SUBSTRINGS' `api_key` already covers it for info/warn/debug.
  'qurl_api_key',
  // Keep canonical qURL link shapes explicit for documentation and so a
  // scalar under one of these names still emits the sensitive-key warning;
  // redactAuditSecrets additionally covers decorated string/container fields.
  'qurllink', 'qurl_link', 'qurllinkurl', 'qurl_link_url',
  'qurllinks', 'qurl_links',
]);

function isAuditSecretKey(key) {
  return AUDIT_SECRET_KEYS.has(String(key).toLowerCase());
}

// Keep matched-key descendants secret even when inner keys are generic (value,
// data) or absent (array elements). Audit key reporting keeps encounter order.
function redactAuditSecrets(value, depth = 0, secretKeys = new Set(), insideSecret = false, seen = new WeakSet()) {
  if (insideSecret && typeof value === 'string' && value.length > 0) {
    return { value: '[REDACTED]', secretKeys };
  }
  // Never invoke caller-provided toJSON hooks during serialization.
  if (typeof value === 'function') return { value: '[UNSUPPORTED]', secretKeys };
  if (value == null || typeof value !== 'object') return { value, secretKeys };
  if (ArrayBuffer.isView(value) || value instanceof ArrayBuffer) {
    return { value: insideSecret ? '[REDACTED]' : `[BINARY ${value.byteLength} bytes]`, secretKeys };
  }
  // Returning the original object at the depth limit could expose its secrets.
  if (depth > 5) return { value: '[TRUNCATED]', secretKeys };
  // Inspect each object once so cycles and shared graphs cannot expand the log.
  if (seen.has(value)) return { value: '[REPEATED]', secretKeys };
  seen.add(value);
  if (Array.isArray(value)) {
    return {
      value: value.map(v => redactAuditSecrets(v, depth + 1, secretKeys, insideSecret, seen).value),
      secretKeys,
    };
  }
  const out = {};
  for (const [k, v] of Object.entries(value)) {
    const secret = isAuditSecretKey(k) || (isQurlAccessLinkKey(k)
      && ((typeof v === 'string' && v.length > 0) || isNonEmptyContainer(v)));
    if (secret) secretKeys.add(k);
    if (secret && isQurlAccessLinkKey(k) && isNonEmptyContainer(v)) {
      out[k] = '[REDACTED]';
    } else {
      out[k] = redactAuditSecrets(v, depth + 1, secretKeys, insideSecret || secret, seen).value;
    }
  }
  return { value: out, secretKeys };
}

function redact(value, depth = 0, insideSecret = false, seen = new WeakSet()) {
  if (insideSecret && typeof value === 'string' && value.length > 0) return '[REDACTED]';
  if (typeof value === 'function') return '[UNSUPPORTED]';
  if (value == null || typeof value !== 'object') return value;
  if (ArrayBuffer.isView(value) || value instanceof ArrayBuffer) {
    return insideSecret ? '[REDACTED]' : `[BINARY ${value.byteLength} bytes]`;
  }
  if (depth > 5) return '[TRUNCATED]';
  if (seen.has(value)) return '[REPEATED]';
  seen.add(value);
  if (Array.isArray(value)) return value.map(v => redact(v, depth + 1, insideSecret, seen));
  const out = {};
  for (const [k, v] of Object.entries(value)) {
    const secret = shouldRedact(k);
    if (secret && isQurlAccessLinkKey(k) && isNonEmptyContainer(v)) {
      out[k] = '[REDACTED]';
    } else {
      out[k] = redact(v, depth + 1, insideSecret || secret, seen);
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
  let metaStr = '';
  try {
    const safe = redact(meta);
    if (safe != null && Object.keys(safe).length > 0) metaStr = ` ${JSON.stringify(safe)}`;
  } catch {
    // The metadata and exception message can both contain secrets.
    metaStr = ' [meta_serialization_failed]';
  }
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
  // the infra repo's qurl-bot-discord/terraform/main.tf
  // pick these up.
  //
  // `agent` is hard-coded to "discord" for this codebase. Future
  // integrations (Slack, Teams, CLI, web/portal) emit their own
  // constant value so a single CloudWatch metric Minted{Agent} can
  // attribute mints across the whole product. The string set is
  // canonical: "discord" | "slack" | "teams" | "cli" | "web" | "api".
  //
  // Audit lines bypass currentLevel — they're observability, not
  // debug noise. They also bypass redact()'s broad substring policy in
  // favor of redactAuditSecrets()'s exact general-secret keys plus narrow
  // qURL-link matching, so legitimate dimensions like `tokens_minted` survive.
  // The AUDIT_EVENTS allowlist in
  // constants.js documents the canonical vocabulary.
  audit(event, meta = {}) {
    // Event names are strings; never invoke caller serialization hooks.
    if (typeof event !== 'string') event = 'invalid_audit_event';
    // Audit dimensions must be an object, not a scalar or array.
    if (meta == null || typeof meta !== 'object' || Array.isArray(meta)) meta = {};
    // Exact audit-key matching preserves dimensions such as tokens_minted.
    // Normal logs use the broader substring policy instead.
    try {
      const { value: cleanedMeta, secretKeys } = redactAuditSecrets(meta);
      if (secretKeys.size > 0) {
        // Logged via console.error so it surfaces at error level in
        // CloudWatch (the logger has no separate warn channel that
        // emits at error severity). Defense-in-depth alongside the
        // value redaction above. Report all matched keys once so callers can
        // fix them in one pass. Event metadata stays in the structured audit
        // record; stderr and stdout records need not arrive in order.
        const namedKeys = [...secretKeys].map(k => `"${sanitizeMessage(k)}"`).join(', ');
        console.error(`[${formatTimestamp()}] ERROR: logger.audit received sensitive key(s) [${namedKeys}]; value(s) redacted in emitted payload — caller must remove from meta`);
      }
      // Spread cleaned meta first, then pin event + agent last so a
      // caller passing `agent` or `event` in meta cannot overwrite the
      // canonical value the CloudWatch filters key off of.
      const auditPayload = { ...cleanedMeta, event, agent: 'discord' };
      // Single-line JSON, parseable by `{ $.audit.event = "..." }`
      // CloudWatch filter syntax. No timestamp prefix — the JSON has
      // its own ts field. console.log adds a trailing newline.
      //
      // Redaction and serialization must not break the caller's user flow.
      console.log(JSON.stringify({
        audit: auditPayload,
        ts: formatTimestamp(),
      }));
    } catch {
      // Two-tier degradation: emit a minimal audit line with a fixed
      // synthetic event so CloudWatch metric filters can pattern-match
      // `{ $.audit.event = "audit_serialization_failed" }` and surface
      // the gap. Omit all caller data, including the event: coercion hooks
      // can throw and event values are not safe failure diagnostics.
      // If even this throws (effectively impossible), fall through to
      // a plain error log so an operator still sees something.
      try {
        console.log(JSON.stringify({
          audit: {
            event: 'audit_serialization_failed',
            agent: 'discord',
            reason: 'metadata_serialization_failed',
          },
          ts: formatTimestamp(),
        }));
      } catch {
        console.error(`[${formatTimestamp()}] ERROR: logger.audit serialization failed reason=metadata_serialization_failed fallback_reason=serialization_failed`);
      }
    }
  },
};

module.exports = logger;
// Test-only: exposes the redact constants so a drift-guard test can iterate
// the live set rather than duplicate it. Gated on NODE_ENV='test' so it's
// absent from prod bundles entirely (Jest sets NODE_ENV=test by default).
// Defensive copies so a buggy test can't mutate the live Sets and corrupt
// subsequent log calls.
if (process.env.NODE_ENV === 'test') {
  module.exports.__testExports = {
    REDACT_EXACT_KEYS: new Set(REDACT_EXACT_KEYS),
    AUDIT_SECRET_KEYS: new Set(AUDIT_SECRET_KEYS),
  };
}
