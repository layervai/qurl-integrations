'use strict';

const {
  ATTEMPT_TIMEOUT_MS,
  AUDIT_ARGS,
  MAX_ATTEMPTS,
  RETRY_DELAYS_MS,
  TOTAL_RETRY_BUDGET_MS,
  runAudit,
} = require('../scripts/audit-production-dependencies');

function captureStream() {
  let value = '';
  return {
    stream: { write: chunk => { value += chunk; } },
    value: () => value,
  };
}

function auditResult(status, body = {}, stderr = '') {
  return { status, stdout: `${JSON.stringify(body)}\n`, stderr };
}

describe('production dependency audit', () => {
  test('pins the production lockfile audit flags literally', () => {
    expect(AUDIT_ARGS).toEqual([
      'audit',
      '--package-lock-only',
      '--audit-level=high',
      '--omit=dev',
      '--json',
      '--fetch-retries=0',
    ]);
  });

  test('retries npm\'s nested E503 envelope and preserves the audit command', async () => {
    const spawn = jest.fn()
      .mockReturnValueOnce(auditResult(1, {
        error: {
          code: 'E503',
          summary: '503 Service Unavailable - POST https://registry.npmjs.org/-/npm/v1/security/audits/quick',
          detail: 'Service Unavailable',
        },
      }))
      .mockReturnValueOnce(auditResult(0, { metadata: { vulnerabilities: { high: 0 } } }));
    const sleep = jest.fn().mockResolvedValue(undefined);
    const stdout = captureStream();
    const stderr = captureStream();

    await expect(runAudit({ spawn, sleep, stdout: stdout.stream, stderr: stderr.stream }))
      .resolves.toBe(0);

    expect(spawn).toHaveBeenCalledTimes(2);
    for (const [command, args, options] of spawn.mock.calls) {
      expect(command).toBe('npm');
      expect(args).toEqual(AUDIT_ARGS);
      expect(options).toEqual(expect.objectContaining({
        encoding: 'utf8',
        timeout: ATTEMPT_TIMEOUT_MS,
        killSignal: 'SIGKILL',
      }));
    }
    expect(sleep).toHaveBeenCalledTimes(1);
    expect(stderr.value()).toContain('E503');
    expect(stdout.value()).toContain('npm audit passed: 0 high, 0 critical');
  });

  test('retries a structured transport envelope emitted on stderr', async () => {
    const spawn = jest.fn()
      .mockReturnValueOnce({
        status: 1,
        stdout: '',
        stderr: `${JSON.stringify({
          error: {
            code: 'ECONNREFUSED',
            summary: 'request to registry failed',
          },
        })}\n`,
      })
      .mockReturnValueOnce(auditResult(0, { metadata: { vulnerabilities: { high: 0 } } }));
    const sleep = jest.fn().mockResolvedValue(undefined);

    await expect(runAudit({
      spawn,
      sleep,
      stdout: captureStream().stream,
      stderr: captureStream().stream,
    })).resolves.toBe(0);

    expect(spawn).toHaveBeenCalledTimes(2);
  });

  test.each([
    ['nested status', { error: { statusCode: 503, summary: 'Service unavailable' } }],
    ['DNS code', { error: { code: 'ENOTFOUND', summary: 'registry DNS lookup failed' } }],
    ['HTTP timeout', { statusCode: 408, message: 'registry timeout' }],
  ])('retries %s only when the structured envelope identifies transport failure', async (_label, body) => {
    const spawn = jest.fn()
      .mockReturnValueOnce(auditResult(1, body))
      .mockReturnValueOnce(auditResult(0, { metadata: { vulnerabilities: { high: 0 } } }));

    await expect(runAudit({
      spawn,
      sleep: jest.fn().mockResolvedValue(undefined),
      stdout: captureStream().stream,
      stderr: captureStream().stream,
    })).resolves.toBe(0);

    expect(spawn).toHaveBeenCalledTimes(2);
  });

  test('prints a concise human-readable vulnerability report without raw JSON', async () => {
    const result = auditResult(1, {
      auditReportVersion: 2,
      vulnerabilities: {
        undici: {
          name: 'undici',
          severity: 'high',
          via: [{ title: 'request smuggling', url: 'https://example.test/advisory', severity: 'high' }],
          nodes: ['node_modules/undici'],
        },
        jest: { name: 'jest', severity: 'moderate', via: [], nodes: ['node_modules/jest'] },
      },
    });
    const stdout = captureStream();
    const stderr = captureStream();

    await expect(runAudit({
      spawn: jest.fn().mockReturnValue(result),
      sleep: jest.fn(),
      stdout: stdout.stream,
      stderr: stderr.stream,
    })).resolves.toBe(1);

    expect(stderr.value()).toContain('npm audit found high/critical production vulnerabilities:');
    expect(stderr.value()).toContain('- undici (high): request smuggling');
    expect(stderr.value()).toContain('https://example.test/advisory');
    expect(stderr.value()).not.toContain('"auditReportVersion"');
    expect(stdout.value()).toBe('');
  });

  test('reports a transitive vulnerability chain and fix availability', async () => {
    const result = auditResult(1, {
      vulnerabilities: {
        '@aws-sdk/client-s3': {
          severity: 'high',
          via: ['undici'],
          fixAvailable: true,
        },
      },
    });
    const stderr = captureStream();

    await expect(runAudit({
      spawn: jest.fn().mockReturnValue(result),
      sleep: jest.fn(),
      stdout: captureStream().stream,
      stderr: stderr.stream,
    })).resolves.toBe(1);

    expect(stderr.value()).toContain('- @aws-sdk/client-s3 (high): via undici — fix available');
  });

  test('prints lower-severity production counts for a passing audit', async () => {
    const stdout = captureStream();

    await expect(runAudit({
      spawn: jest.fn().mockReturnValue(auditResult(0, {
        metadata: { vulnerabilities: { low: 2, moderate: 3, high: 0, critical: 0 } },
      })),
      sleep: jest.fn(),
      stdout: stdout.stream,
      stderr: captureStream().stream,
    })).resolves.toBe(0);

    expect(stdout.value()).toBe(
      'npm audit passed: 0 high, 0 critical; 5 lower-severity production vulnerabilities.\n',
    );
  });

  test.each([
    ['a non-retryable registry response', auditResult(1, { error: { code: 'E400' } })],
    ['malformed output', { status: 1, stdout: 'not json', stderr: 'npm failed' }],
  ])('does not retry %s', async (_label, result) => {
    const spawn = jest.fn().mockReturnValue(result);
    const sleep = jest.fn();

    await expect(runAudit({
      spawn,
      sleep,
      stdout: captureStream().stream,
      stderr: captureStream().stream,
    })).resolves.toBe(1);

    expect(spawn).toHaveBeenCalledTimes(1);
    expect(sleep).not.toHaveBeenCalled();
  });

  test('retries a locally enforced audit timeout and stops at the attempt cap', async () => {
    const spawn = jest.fn().mockReturnValue({
      status: null,
      stdout: '',
      stderr: '',
      error: { code: 'ETIMEDOUT' },
    });
    const sleep = jest.fn().mockResolvedValue(undefined);

    await expect(runAudit({
      spawn,
      sleep,
      stdout: captureStream().stream,
      stderr: captureStream().stream,
    })).resolves.toBe(1);

    expect(spawn).toHaveBeenCalledTimes(MAX_ATTEMPTS);
    expect(sleep).toHaveBeenCalledTimes(MAX_ATTEMPTS - 1);
    expect(sleep.mock.calls).toEqual(RETRY_DELAYS_MS.map(delay => [delay]));

    const retryDelayMs = sleep.mock.calls.reduce((total, [delay]) => total + delay, 0);
    expect((MAX_ATTEMPTS * ATTEMPT_TIMEOUT_MS) + retryDelayMs).toBe(TOTAL_RETRY_BUDGET_MS);
    expect(TOTAL_RETRY_BUDGET_MS).toBe(150_000);
  });

  test('reports the final transport failure concisely without dumping the JSON envelope', async () => {
    const spawn = jest.fn().mockReturnValue(auditResult(1, {
      error: { code: 'E503', summary: 'huge raw npm body' },
    }));
    const stdout = captureStream();
    const stderr = captureStream();

    await expect(runAudit({
      spawn,
      sleep: jest.fn().mockResolvedValue(undefined),
      stdout: stdout.stream,
      stderr: stderr.stream,
    })).resolves.toBe(1);

    expect(stderr.value()).toContain(
      `npm audit registry transport failed after ${MAX_ATTEMPTS} attempts (E503): huge raw npm body.`,
    );
    expect(stdout.value()).toBe('');
  });

  test('preserves a non-retryable npm exit code', async () => {
    await expect(runAudit({
      spawn: jest.fn().mockReturnValue(auditResult(7, { error: { code: 'E400' } })),
      sleep: jest.fn(),
      stdout: captureStream().stream,
      stderr: captureStream().stream,
    })).resolves.toBe(7);
  });
});
