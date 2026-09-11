import type { LogContext, Logger } from './interfaces.js';

const REDACTED = '[REDACTED]';
const OPAQUE_VALUE = /(?<![A-Za-z0-9_-])[A-Za-z0-9_-]{43,}(?![A-Za-z0-9_-])/g;
const BEARER_VALUE = /\bBearer\s+[^\s,;]+/gi;
const FORM_SECRET = /\b(client_secret|code|code_verifier)=([^&\s]+)/gi;
// Only known transport codes may escape the general OAuth `code` redaction.
const TRANSPORT_ERROR_CODES = new Set(['ECONNREFUSED', 'ECONNRESET', 'ETIMEDOUT', 'ENOTFOUND', 'EAI_AGAIN', 'ENETUNREACH', 'CERT_HAS_EXPIRED', 'DEPTH_ZERO_SELF_SIGNED_CERT', 'UNABLE_TO_VERIFY_LEAF_SIGNATURE']);

export type LoggerSink = Logger;

function redactText(value: string, protectedValues: readonly string[]): string {
  let redacted = value.replace(BEARER_VALUE, `Bearer ${REDACTED}`)
    .replace(FORM_SECRET, `$1=${REDACTED}`)
    .replace(OPAQUE_VALUE, REDACTED);
  for (const secret of protectedValues) {
    if (secret.length > 0) {
      redacted = redacted.split(secret).join(REDACTED);
    }
  }
  return redacted;
}

function isSensitiveKey(key: string): boolean {
  const normalized = key.replace(/[-_]/g, '').toLowerCase();
  return normalized === 'code'
    || normalized === 'authorizationcode'
    || normalized.includes('authorization')
    || normalized.includes('cookie')
    || normalized.includes('email')
    || normalized.includes('nonce')
    || normalized.includes('password')
    || normalized.includes('pkce')
    || normalized.includes('secret')
    || normalized.includes('state')
    || normalized.includes('token')
    || normalized.includes('verifier');
}

function redactValue(value: unknown, protectedValues: readonly string[], seen: WeakSet<object>): unknown {
  if (typeof value === 'string') {
    return redactText(value, protectedValues);
  }
  if (value !== null && typeof value === 'object') {
    if (seen.has(value)) {
      return '[Circular]';
    }
    seen.add(value);
    try {
      if (value instanceof Error) {
        const code = 'code' in value && typeof value.code === 'string' ? value.code : '';
        return {
          name: value.name,
          message: redactText(value.message, protectedValues),
          ...(TRANSPORT_ERROR_CODES.has(code) ? { code } : {}),
          // A stack carries frames and paths, not values, and is still passed
          // through the same redaction as the message. Dropping it leaves the
          // generic operator-facing failures with nothing to triage from.
          ...(typeof value.stack === 'string' ? { stack: redactText(value.stack, protectedValues) } : {}),
          ...(value.cause === undefined ? {} : { cause: redactValue(value.cause, protectedValues, seen) }),
        };
      }
      if (Array.isArray(value)) {
        return value.map((item) => redactValue(item, protectedValues, seen));
      }
      const output: Record<string, unknown> = {};
      for (const [key, nested] of Object.entries(value)) {
        output[key] = isSensitiveKey(key)
          ? REDACTED
          : redactValue(nested, protectedValues, seen);
      }
      return output;
    } finally {
      // Track only the active traversal branch. A repeated Error or object is
      // useful diagnostic context; only a true recursive reference is cyclic.
      seen.delete(value);
    }
  }
  return value;
}

function redactContext(context: LogContext | undefined, protectedValues: readonly string[]): LogContext | undefined {
  if (context === undefined) {
    return undefined;
  }
  return redactValue(context, protectedValues, new WeakSet()) as LogContext;
}

/** A logging boundary that redacts credential-shaped values recursively. */
export class RedactingLogger implements Logger {
  readonly #sink: LoggerSink;
  readonly #protectedValues: readonly string[];

  constructor(sink: LoggerSink, protectedValues: readonly string[] = []) {
    this.#sink = sink;
    this.#protectedValues = [...protectedValues];
  }

  debug(message: string, context?: LogContext): void {
    this.#write('debug', message, context);
  }

  info(message: string, context?: LogContext): void {
    this.#write('info', message, context);
  }

  warn(message: string, context?: LogContext): void {
    this.#write('warn', message, context);
  }

  error(message: string, context?: LogContext): void {
    this.#write('error', message, context);
  }

  #write(level: keyof Logger, message: string, context?: LogContext): void {
    const safeMessage = redactText(message, this.#protectedValues);
    const safeContext = redactContext(context, this.#protectedValues);
    if (safeContext === undefined) {
      this.#sink[level](safeMessage);
      return;
    }
    this.#sink[level](safeMessage, safeContext);
  }
}

/**
 * A sink that emits one JSON object per line, matching the field contract the
 * fleet's CloudWatch metric filters select on.
 *
 * TODO(upstream-contract): `level` (upper-case) and `error` are the JSON keys
 * `qurl-bot-teams/terraform/app_alarms.tf` filters on, mirroring apps/slack's
 * `slog.JSONHandler` output and `qurl-bot-slack/terraform/app_alarms.tf`.
 * `error` must be a string: those filters perform wildcard string matches,
 * including against nested transport causes and AWS exception names.
 * Changing either key silently blinds those alarms -- the filters keep
 * matching nothing and the alarm sits in OK forever. Change both together.
 *
 * Plain `console.*` output is not selectable by a JSON metric filter at all,
 * which is why this exists rather than writing text.
 */
export function jsonConsoleSink(target: Pick<Console, 'debug' | 'info' | 'warn' | 'error'>): LoggerSink {
  const emit = (level: string, method: 'debug' | 'info' | 'warn' | 'error') =>
    (message: string, context?: LogContext): void => {
      // The context is already redacted by RedactingLogger before it reaches a
      // sink; this only serializes.
      const line: Record<string, unknown> = { level, message, ...(context ?? {}) };
      if (line.error !== null && typeof line.error === 'object') line.error = JSON.stringify(line.error);
      target[method](JSON.stringify(line));
    };
  return {
    debug: emit('DEBUG', 'debug'),
    info: emit('INFO', 'info'),
    warn: emit('WARN', 'warn'),
    error: emit('ERROR', 'error'),
  };
}
