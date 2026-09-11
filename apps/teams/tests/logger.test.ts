import { describe, expect, it } from 'vitest';
import type { LogContext, Logger } from '../src/interfaces.js';
import { jsonConsoleSink, RedactingLogger } from '../src/logger.js';

describe('RedactingLogger', () => {
  it('redacts secrets, OAuth values, nested errors, and opaque handles', () => {
    const entries: Array<{ message: string; context?: LogContext }> = [];
    const sink: Logger = {
      debug: (message, context) => entries.push({ message, ...(context === undefined ? {} : { context }) }),
      info: (message, context) => entries.push({ message, ...(context === undefined ? {} : { context }) }),
      warn: (message, context) => entries.push({ message, ...(context === undefined ? {} : { context }) }),
      error: (message, context) => entries.push({ message, ...(context === undefined ? {} : { context }) }),
    };
    const clientSecret = 'synthetic-client-secret-value';
    const opaqueState = Buffer.alloc(32, 8).toString('base64url');
    const edgeBoundedSecret = `-${'A'.repeat(41)}-`;
    const longOpaqueValue = 'B'.repeat(256);
    const logger = new RedactingLogger(sink, [clientSecret]);

    logger.error(`failed with ${clientSecret} and Bearer access-value`, {
      state: opaqueState,
      nested: { clientSecret, error: new Error(`echo ${clientSecret}`) },
      unlabelled: opaqueState,
      unlabelledEdge: edgeBoundedSecret,
      longOpaqueValue,
      status: 401,
    });

    const rendered = JSON.stringify(entries);
    expect(rendered).not.toContain(clientSecret);
    expect(rendered).not.toContain(opaqueState);
    expect(rendered).not.toContain(edgeBoundedSecret);
    expect(rendered).not.toContain(longOpaqueValue);
    expect(rendered).not.toContain('access-value');
    expect(rendered).toContain('[REDACTED]');
    expect(rendered).toContain('401');
  });

  it('preserves a recursively redacted error cause for diagnostics', () => {
    const entries: Array<{ context: LogContext | undefined }> = [];
    const sink: Logger = {
      debug: (_message, context) => entries.push({ context }),
      info: (_message, context) => entries.push({ context }),
      warn: (_message, context) => entries.push({ context }),
      error: (_message, context) => entries.push({ context }),
    };
    const logger = new RedactingLogger(sink, ['upstream-secret']);
    const cause = new Error('cause contains upstream-secret');
    logger.error('request failed', { error: new Error('outer failure', { cause }) });
    expect(entries[0]?.context).toMatchObject({ error: {
      name: 'Error',
      message: 'outer failure',
      cause: { name: 'Error', message: 'cause contains [REDACTED]' },
    } });
    const logged = entries[0]?.context?.error as { readonly stack?: string; readonly cause?: { readonly stack?: string } };
    // A stack is what makes the generic operator-facing failures triageable.
    expect(logged.stack).toContain('logger.test.ts');
    expect(logged.cause?.stack).toBeTypeOf('string');
    expect(JSON.stringify(entries)).not.toContain('upstream-secret');
  });

  it('renders repeated errors independently while retaining true cycle protection', () => {
    const entries: Array<{ context: LogContext | undefined }> = [];
    const sink: Logger = {
      debug: (_message, context) => entries.push({ context }),
      info: (_message, context) => entries.push({ context }),
      warn: (_message, context) => entries.push({ context }),
      error: (_message, context) => entries.push({ context }),
    };
    const logger = new RedactingLogger(sink);
    const repeated = new Error('same error');
    const circular: Record<string, unknown> = {};
    circular.self = circular;
    logger.error('request failed', { errors: [repeated, repeated], circular });
    expect(entries[0]?.context).toMatchObject({
      errors: [{ name: 'Error', message: 'same error' }, { name: 'Error', message: 'same error' }],
      circular: { self: '[Circular]' },
    });
  });
});

describe('json console sink', () => {
  it('renders real AWS and nested transport errors as redacted alarm-matchable strings', () => {
    const lines: string[] = [];
    const target = { debug: () => {}, info: () => {}, warn: () => {}, error: (line: string) => lines.push(line) };
    const logger = new RedactingLogger(jsonConsoleSink(target), ['upstream-secret']);
    const cause = Object.assign(new Error('connection failed upstream-secret'), { code: 'ECONNREFUSED' });
    const awsError = Object.assign(new Error('permission denied'), { name: 'AccessDeniedException', code: 'private-oauth-code' });
    logger.error('Teams command failed', { error: new TypeError('fetch failed', { cause }) });
    logger.error('Teams command failed', { error: awsError });
    const transport = JSON.parse(lines[0] ?? '{}');
    const dataPlane = JSON.parse(lines[1] ?? '{}');
    expect(transport.error).toBeTypeOf('string');
    expect(transport.error).toContain('ECONNREFUSED');
    expect(transport.error).toContain('fetch failed');
    expect(transport.error).toContain('[REDACTED]');
    expect(dataPlane.error).toBeTypeOf('string');
    expect(dataPlane.error).toContain('AccessDeniedException');
    expect(lines.join('\n')).not.toContain('upstream-secret');
    expect(lines.join('\n')).not.toContain('private-oauth-code');
  });

  it('emits one JSON line per call with the field names the alarms select on', () => {
    const lines: string[] = [];
    const target = {
      debug: (l: string) => lines.push(l),
      info: (l: string) => lines.push(l),
      warn: (l: string) => lines.push(l),
      error: (l: string) => lines.push(l),
    } as unknown as Console;
    const logger = new RedactingLogger(jsonConsoleSink(target));

    logger.error('qurl call failed', { error: 'http request: context deadline exceeded', tenantId: 't1' });
    logger.info('started', undefined);

    const failure = JSON.parse(lines[0] ?? '{}');
    // `level` upper-case and `error` are the keys app_alarms.tf filters on.
    expect(failure.level).toBe('ERROR');
    expect(failure.message).toBe('qurl call failed');
    expect(failure.error).toContain('context deadline exceeded');
    expect(failure.tenantId).toBe('t1');
    expect(JSON.parse(lines[1] ?? '{}').level).toBe('INFO');
  });

  it('still redacts before serializing', () => {
    const lines: string[] = [];
    const target = { debug: () => {}, info: () => {}, warn: () => {}, error: (l: string) => lines.push(l) } as unknown as Console;
    new RedactingLogger(jsonConsoleSink(target)).error('failed', { authorization: 'Bearer super-secret-value' });
    expect(lines[0]).not.toContain('super-secret-value');
  });
});
