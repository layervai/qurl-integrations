'use strict';

const path = require('node:path');

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

function passingAuditBody(vulnerabilities = { high: 0, critical: 0 }, prod = 1) {
  return { metadata: { vulnerabilities, dependencies: { prod } } };
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
      '--fetch-timeout=40000',
    ]);
    expect(Object.isFrozen(AUDIT_ARGS)).toBe(true);
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
      .mockReturnValueOnce(auditResult(0, passingAuditBody()));
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
        cwd: path.resolve(__dirname, '..'),
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
      .mockReturnValueOnce(auditResult(0, passingAuditBody()));
    const sleep = jest.fn().mockResolvedValue(undefined);

    await expect(runAudit({
      spawn,
      sleep,
      stdout: captureStream().stream,
      stderr: captureStream().stream,
    })).resolves.toBe(0);

    expect(spawn).toHaveBeenCalledTimes(2);
  });

  test('retries a structured envelope surrounded by warning text containing braces', async () => {
    const spawn = jest.fn()
      .mockReturnValueOnce({
        status: 1,
        stdout: '',
        stderr: 'npm warn ignoring config {legacy}\n{\n  "error": {\n    "code": "E503",\n    "summary": "Service unavailable"\n  }\n}\nnpm warn done\n',
      })
      .mockReturnValueOnce(auditResult(0, passingAuditBody()));

    await expect(runAudit({
      spawn,
      sleep: jest.fn().mockResolvedValue(undefined),
      stdout: captureStream().stream,
      stderr: captureStream().stream,
    })).resolves.toBe(0);

    expect(spawn).toHaveBeenCalledTimes(2);
  });

  test('retries a compact transport envelope surrounded by warning text', async () => {
    const spawn = jest.fn()
      .mockReturnValueOnce({
        status: 1,
        stdout: '',
        stderr: 'npm warn ignoring config\n{"error":{"code":"E503","summary":"Service unavailable"}}\nnpm warn done\n',
      })
      .mockReturnValueOnce(auditResult(0, passingAuditBody()));

    await expect(runAudit({
      spawn,
      sleep: jest.fn().mockResolvedValue(undefined),
      stdout: captureStream().stream,
      stderr: captureStream().stream,
    })).resolves.toBe(0);

    expect(spawn).toHaveBeenCalledTimes(2);
  });

  test('retries npm audit endpoint timeout text when its JSON error fields are blank', async () => {
    const spawn = jest.fn()
      .mockReturnValueOnce({
        status: 1,
        stdout: '{"error":{"summary":"","detail":""}}\n',
        stderr: 'npm warn audit network timeout at: https://registry.npmjs.org/-/npm/v1/security/audits/quick\n',
      })
      .mockReturnValueOnce(auditResult(0, passingAuditBody()));

    await expect(runAudit({
      spawn,
      sleep: jest.fn().mockResolvedValue(undefined),
      stdout: captureStream().stream,
      stderr: captureStream().stream,
    })).resolves.toBe(0);

    expect(spawn).toHaveBeenCalledTimes(2);
  });

  test('retries npm audit endpoint timeout text with trailing whitespace', async () => {
    const spawn = jest.fn()
      .mockReturnValueOnce({
        status: 1,
        stdout: '{"error":{"summary":"","detail":""}}\n',
        stderr: 'npm warn audit network timeout at: https://registry.npmjs.org/-/npm/v1/security/audits/quick  \n',
      })
      .mockReturnValueOnce(auditResult(0, passingAuditBody()));

    await expect(runAudit({
      spawn,
      sleep: jest.fn().mockResolvedValue(undefined),
      stdout: captureStream().stream,
      stderr: captureStream().stream,
    })).resolves.toBe(0);

    expect(spawn).toHaveBeenCalledTimes(2);
  });

  test.each([
    ['nested status', { error: { statusCode: 503, summary: 'Service unavailable' } }],
    ['DNS code', { error: { code: 'ENOTFOUND', summary: 'registry DNS lookup failed' } }],
    ['top-level DNS code', { code: 'ENOTFOUND' }],
    ['DNS code in error message', { error: { message: 'getaddrinfo ENOTFOUND registry.npmjs.org' } }],
    ['network-timeout message', { message: 'network timeout contacting registry' }],
    ['HTTP timeout', { statusCode: 408, message: 'registry timeout' }],
  ])('retries %s only when the structured envelope identifies transport failure', async (_label, body) => {
    const spawn = jest.fn()
      .mockReturnValueOnce(auditResult(1, body))
      .mockReturnValueOnce(auditResult(0, passingAuditBody()));

    await expect(runAudit({
      spawn,
      sleep: jest.fn().mockResolvedValue(undefined),
      stdout: captureStream().stream,
      stderr: captureStream().stream,
    })).resolves.toBe(0);

    expect(spawn).toHaveBeenCalledTimes(2);
  });

  test('retries npm 10.9.4 when its retired quick fallback rejects a timed-out bulk audit', async () => {
    const retiredQuickFallback = {
      message: '400 Bad Request - POST https://registry.npmjs.org/-/npm/v1/security/audits/quick - Bad Request',
      method: 'POST',
      uri: 'https://registry.npmjs.org/-/npm/v1/security/audits/quick',
      headers: {
        'npm-notice': [
          'This endpoint is being retired. Use the bulk advisory endpoint instead. See the following docs for more info: https://api-docs.npmjs.com/#tag/Audit',
        ],
      },
      statusCode: 400,
      body: {
        statusCode: 400,
        error: 'Bad Request',
        message: 'Invalid package tree, run npm install to rebuild your package-lock.json',
      },
      error: { summary: '', detail: '' },
    };
    const spawn = jest.fn()
      .mockReturnValueOnce(auditResult(1, retiredQuickFallback))
      .mockReturnValueOnce(auditResult(0, passingAuditBody()));
    const sleep = jest.fn().mockResolvedValue(undefined);
    const stderr = captureStream();

    await expect(runAudit({
      spawn,
      sleep,
      stdout: captureStream().stream,
      stderr: stderr.stream,
    })).resolves.toBe(0);

    expect(spawn).toHaveBeenCalledTimes(2);
    expect(sleep).toHaveBeenCalledTimes(1);
    expect(stderr.value()).toContain('EAUDITQUICKRETIRED');
  });

  test('does not retry a vulnerability report decorated like the retired quick fallback', async () => {
    const spawn = jest.fn().mockReturnValue(auditResult(1, {
      method: 'POST',
      uri: 'https://registry.npmjs.org/-/npm/v1/security/audits/quick',
      statusCode: 400,
      body: {
        statusCode: 400,
        message: 'Invalid package tree, run npm install to rebuild your package-lock.json',
      },
      headers: {
        'npm-notice': ['This endpoint is being retired. Use the bulk advisory endpoint instead.'],
      },
      vulnerabilities: {
        undici: { severity: 'high', via: [], nodes: ['node_modules/undici'] },
      },
    }));
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

  test('does not retry when an advisory title contains a transport error code', async () => {
    const spawn = jest.fn().mockReturnValue(auditResult(1, {
      auditReportVersion: 2,
      vulnerabilities: {
        example: {
          severity: 'high',
          via: [{ title: 'ETIMEDOUT handling bypass', severity: 'high' }],
          nodes: ['node_modules/example'],
        },
      },
    }));
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
      spawn: jest.fn().mockReturnValue(auditResult(0, passingAuditBody({
        low: 2, moderate: 3, high: 0, critical: 0,
      }))),
      sleep: jest.fn(),
      stdout: stdout.stream,
      stderr: captureStream().stream,
    })).resolves.toBe(0);

    expect(stdout.value()).toBe(
      'npm audit passed: 0 high, 0 critical; 5 lower-severity vulnerabilities across '
      + '1 production dependencies.\n',
    );
  });

  test('fails closed if npm exits zero while reporting a high vulnerability', async () => {
    const stderr = captureStream();

    await expect(runAudit({
      spawn: jest.fn().mockReturnValue(auditResult(0, {
        metadata: { vulnerabilities: { high: 1, critical: 0 } },
      })),
      sleep: jest.fn(),
      stdout: captureStream().stream,
      stderr: stderr.stream,
    })).resolves.toBe(1);

    expect(stderr.value()).toContain('reported 1 high and 0 critical vulnerabilities');
  });

  test.each([
    ['missing production dependency metadata', undefined],
    ['zero audited production dependencies', 0],
    ['invalid production dependency count', '1'],
  ])('fails closed on a zero-vulnerability result with %s', async (_label, prod) => {
    const body = passingAuditBody();
    if (prod === undefined) delete body.metadata.dependencies;
    else body.metadata.dependencies.prod = prod;
    const stderr = captureStream();

    await expect(runAudit({
      spawn: jest.fn().mockReturnValue(auditResult(0, body)),
      sleep: jest.fn(),
      stdout: captureStream().stream,
      stderr: stderr.stream,
    })).resolves.toBe(1);

    expect(stderr.value()).toContain('invalid audited production dependency count');
  });

  test.each([
    ['missing critical count', { high: 0 }],
    ['string high count', { high: '1', critical: 0 }],
    ['negative critical count', { high: 0, critical: -1 }],
  ])('fails closed when npm exits zero with %s', async (_label, vulnerabilities) => {
    const stderr = captureStream();

    await expect(runAudit({
      spawn: jest.fn().mockReturnValue(auditResult(0, {
        metadata: { vulnerabilities },
      })),
      sleep: jest.fn(),
      stdout: captureStream().stream,
      stderr: stderr.stream,
    })).resolves.toBe(1);

    expect(stderr.value()).toContain('invalid metadata.vulnerabilities counts');
  });

  test('fails closed when npm exits zero without the audit metadata envelope', async () => {
    const stdout = captureStream();
    const stderr = captureStream();

    await expect(runAudit({
      spawn: jest.fn().mockReturnValue({ status: 0, stdout: 'not an audit report\n', stderr: '' }),
      sleep: jest.fn(),
      stdout: stdout.stream,
      stderr: stderr.stream,
    })).resolves.toBe(1);

    expect(stdout.value()).toBe('');
    expect(stderr.value()).toContain('missing metadata.vulnerabilities');
    expect(stderr.value()).toContain('not an audit report');
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

  test('preserves a bounded diagnostic for an empty structured error', async () => {
    const stderr = captureStream();
    const raw = `${JSON.stringify({ error: { summary: '', detail: '' } })}\n`;

    await expect(runAudit({
      spawn: jest.fn().mockReturnValue({ status: 1, stdout: raw, stderr: 'npm error audit endpoint returned an error\n' }),
      sleep: jest.fn(),
      stdout: captureStream().stream,
      stderr: stderr.stream,
    })).resolves.toBe(1);

    expect(stderr.value()).toContain('npm audit failed with unclassified output:');
    expect(stderr.value()).toContain('audit endpoint returned an error');
    expect(stderr.value().length).toBeLessThanOrEqual(2200);
  });

  test('does not retry a missing npm binary and prints the spawn diagnostic', async () => {
    const spawn = jest.fn().mockReturnValue({
      status: null,
      stdout: '',
      stderr: '',
      error: { code: 'ENOENT', message: 'spawnSync npm ENOENT' },
    });
    const sleep = jest.fn();
    const stderr = captureStream();

    await expect(runAudit({
      spawn,
      sleep,
      stdout: captureStream().stream,
      stderr: stderr.stream,
    })).resolves.toBe(1);

    expect(spawn).toHaveBeenCalledTimes(1);
    expect(sleep).not.toHaveBeenCalled();
    expect(stderr.value()).toContain('npm audit could not run (ENOENT): spawnSync npm ENOENT.');
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
