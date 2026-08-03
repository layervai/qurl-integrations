import { describe, expect, it } from 'vitest';
import type { LogContext, Logger } from '../src/interfaces.js';
import { RedactingLogger } from '../src/logger.js';

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
    const logger = new RedactingLogger(sink, [clientSecret]);

    logger.error(`failed with ${clientSecret} and Bearer access-value`, {
      state: opaqueState,
      nested: { clientSecret, error: new Error(`echo ${clientSecret}`) },
      unlabelled: opaqueState,
      unlabelledEdge: edgeBoundedSecret,
      status: 401,
    });

    const rendered = JSON.stringify(entries);
    expect(rendered).not.toContain(clientSecret);
    expect(rendered).not.toContain(opaqueState);
    expect(rendered).not.toContain(edgeBoundedSecret);
    expect(rendered).not.toContain('access-value');
    expect(rendered).toContain('[REDACTED]');
    expect(rendered).toContain('401');
  });
});
