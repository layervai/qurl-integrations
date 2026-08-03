import type { LogContext, Logger } from './interfaces.js';

const REDACTED = '[REDACTED]';
const OPAQUE_VALUE = /(?<![A-Za-z0-9_-])[A-Za-z0-9_-]{43,128}(?![A-Za-z0-9_-])/g;
const BEARER_VALUE = /\bBearer\s+[^\s,;]+/gi;
const FORM_SECRET = /\b(client_secret|code|code_verifier)=([^&\s]+)/gi;

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
  if (value instanceof Error) {
    return { name: value.name, message: redactText(value.message, protectedValues) };
  }
  if (Array.isArray(value)) {
    return value.map((item) => redactValue(item, protectedValues, seen));
  }
  if (value !== null && typeof value === 'object') {
    if (seen.has(value)) {
      return '[Circular]';
    }
    seen.add(value);
    const output: Record<string, unknown> = {};
    for (const [key, nested] of Object.entries(value)) {
      output[key] = isSensitiveKey(key)
        ? REDACTED
        : redactValue(nested, protectedValues, seen);
    }
    return output;
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
