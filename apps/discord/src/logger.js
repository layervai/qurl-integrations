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
const REDACT_SUBSTRINGS = [
  'token', 'secret', 'password', 'authorization', 'apikey', 'api_key',
];

function shouldRedact(key) {
  const k = String(key).toLowerCase();
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
]);

function isAuditSecretKey(key) {
  return AUDIT_SECRET_KEYS.has(String(key).toLowerCase());
}

function redact(value, depth = 0) {
  if (depth > 5 || value == null) return value;
  if (Array.isArray(value)) return value.map(v => redact(v, depth + 1));
  if (typeof value !== 'object') return value;
  const out = {};
  for (const [k, v] of Object.entries(value)) {
    if (shouldRedact(k)) {
      out[k] = typeof v === 'string' && v.length > 0 ? '[REDACTED]' : v;
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
  // debug noise. They also bypass the redact() pass on `meta`. redact()
  // matches on KEY name (not value), so future fields like `token_count`
  // or `tokens_minted` would be blanked otherwise — which would corrupt
  // a CloudWatch metric dimension. Callers MUST NOT pass secrets in meta:
  // because audit() does not redact, a key like `auth_token` or
  // `apiKey` lands in CloudWatch verbatim. The defense-in-depth warn
  // below catches this at runtime; the static contract is enforced
  // by the AUDIT_EVENTS allowlist in constants.js (callers always
  // pass meta from a small, pre-vetted set: send_id, kind, count,
  // expires_in, success, total).
  audit(event, meta = {}) {
    // Default param only fires for `undefined`. A caller passing `null`
    // (easy mistake from optional chaining: `someObj?.meta`) would
    // otherwise crash `Object.keys(null)` BEFORE the protected
    // try/catch around JSON.stringify, defeating the "audit never
    // breaks user flow" contract. Coerce to {} for any non-object.
    if (meta == null || typeof meta !== 'object') meta = {};
    // Defense-in-depth: warn if a meta key matches the exact-match
    // AUDIT_SECRET_KEYS set (auth_token, api_key, password, ...). We
    // don't redact (would corrupt dimensions) and we don't drop the
    // value (audit must always emit), but we surface the violation
    // as a CloudWatch-visible error log so a misbehaving caller is
    // catchable in dashboards rather than failing silently. Uses
    // exact-match instead of REDACT_SUBSTRINGS's includes() check
    // so legitimate dimensions like `tokens_minted` don't trigger.
    for (const key of Object.keys(meta)) {
      if (isAuditSecretKey(key)) {
        console.error(`[${formatTimestamp()}] ERROR: logger.audit received secret-shaped key "${sanitizeMessage(key)}" in event=${sanitizeMessage(event)}; emitting unredacted per audit contract — caller must remove from meta`);
        break;
      }
    }
    // Spread meta first, then pin event + agent last so a caller passing
    // `agent` or `event` in meta cannot overwrite the canonical value the
    // CloudWatch filters key off of.
    const auditPayload = { ...meta, event, agent: 'discord' };
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
      console.error(`[${formatTimestamp()}] ERROR: logger.audit serialization failed event=${sanitizeMessage(event)} reason=${sanitizeMessage(err && err.message)}`);
    }
  },
};

module.exports = logger;
