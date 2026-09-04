'use strict';

const { spawnSync } = require('node:child_process');
const path = require('node:path');
const { setTimeout: wait } = require('node:timers/promises');

const APP_ROOT = path.resolve(__dirname, '..');
const AUDIT_ARGS = [
  'audit',
  // TODO(upstream-contract): npm 10.9.4 applies --omit=dev to the
  // --package-lock-only audit graph. Reverify whenever the pinned npm moves.
  '--package-lock-only',
  '--audit-level=high',
  '--omit=dev',
  '--json',
  // npm 10.9.4's inner registry retry can outlive this wrapper's per-attempt
  // timeout. Disable it so the structured, bounded outer retry owns timing.
  '--fetch-retries=0',
];
const ATTEMPT_TIMEOUT_MS = 45_000;
const RETRY_DELAYS_MS = Object.freeze([5_000, 10_000]);
const MAX_ATTEMPTS = RETRY_DELAYS_MS.length + 1;
// Literal so the Go workflow contract can compare it with timeout-minutes;
// Jest independently proves it still equals attempts * timeout + delays.
const TOTAL_RETRY_BUDGET_MS = 150_000;
const RETRYABLE_NETWORK_CODES = new Set([
  'EAI_AGAIN',
  'ECONNREFUSED',
  'ECONNRESET',
  'EHOSTUNREACH',
  'ENETUNREACH',
  'ENOTFOUND',
  'EPIPE',
  'ETIMEDOUT',
]);

// npm 10 exhausted its own fetch behavior and returned failures for the
// registry's audit POST in CI. This wrapper retries the complete command,
// while the structured predicate below keeps vulnerability reports and other
// npm failures immediate.
function parseJsonPayload(value) {
  if (typeof value !== 'string' || value.trim() === '') return null;
  const trimmed = value.trim();
  try {
    return JSON.parse(trimmed);
  } catch {
    // TODO(upstream-contract): npm can surround its --json envelope with
    // warning lines. Search only column-zero object boundaries so braces in a
    // warning cannot consume the actual envelope.
    const lines = trimmed.split(/\r?\n/);
    for (let start = 0; start < lines.length; start += 1) {
      if (!lines[start].startsWith('{')) continue;
      for (let end = lines.length - 1; end >= start; end -= 1) {
        if (!lines[end].trimEnd().endsWith('}')) continue;
        try {
          return JSON.parse(lines.slice(start, end + 1).join('\n'));
        } catch {
          // Try the next complete column-zero object boundary.
        }
      }
    }
    return null;
  }
}

function auditBody(result) {
  return parseJsonPayload(result?.stdout) ?? parseJsonPayload(result?.stderr);
}

function auditTransportCode(result, body = auditBody(result)) {
  // TODO(upstream-contract): npm 10.9.4 reports audit transport failures in
  // these structured fields. Unknown shapes fail closed without a retry.
  if (typeof result?.error?.code === 'string') return result.error.code.toUpperCase();

  const statusCode = Number(
    body?.error?.statusCode ?? body?.error?.status ?? body?.statusCode ?? body?.status,
  );
  if (statusCode === 408 || statusCode === 429 || (statusCode >= 500 && statusCode <= 599)) {
    return `E${statusCode}`;
  }

  const directCode = body?.error?.code ?? body?.code;
  if (typeof directCode === 'string' && isRetryableTransportCode(directCode.toUpperCase())) {
    return directCode.toUpperCase();
  }

  const message = [
    body?.message,
    typeof body?.error === 'string' ? body.error : null,
    body?.error?.message,
    body?.error?.summary,
    body?.error?.detail,
  ]
    .filter(value => typeof value === 'string')
    .join('\n');
  const networkCode = message.match(
    /\b(EAI_AGAIN|ECONNREFUSED|ECONNRESET|EHOSTUNREACH|ENETUNREACH|ENOTFOUND|EPIPE|ETIMEDOUT)\b/,
  );
  if (networkCode) return networkCode[1];
  if (/\bnetwork timeout\b/i.test(message)) return 'ETIMEDOUT';

  return null;
}

function isRetryableTransportCode(code) {
  return RETRYABLE_NETWORK_CODES.has(code) || /^E(?:408|429|5\d\d)$/.test(code || '');
}

function writeOutput(stream, value) {
  if (value) stream.write(value);
}

function oneLine(value, maxLength = 500) {
  if (typeof value !== 'string') return '';
  const compact = value.replace(/\s+/g, ' ').trim();
  return compact.length > maxLength ? `${compact.slice(0, maxLength)}…` : compact;
}

function writeSuccess(body, stdout) {
  const counts = body?.metadata?.vulnerabilities;
  if (!counts || typeof counts !== 'object') {
    stdout.write('npm audit passed.\n');
    return;
  }
  const count = severity => Number.isSafeInteger(counts[severity]) ? counts[severity] : 0;
  const lowerSeverity = count('info') + count('low') + count('moderate');
  stdout.write(
    `npm audit passed: ${count('high')} high, ${count('critical')} critical; `
    + `${lowerSeverity} lower-severity production vulnerabilities.\n`,
  );
}

function writeNonRetryableFailure(result, body, stdout, stderr) {
  const vulnerabilities = body?.vulnerabilities;
  if (vulnerabilities && typeof vulnerabilities === 'object') {
    const gated = Object.entries(vulnerabilities)
      .filter(([, vulnerability]) => ['high', 'critical'].includes(vulnerability?.severity));
    if (gated.length > 0) {
      stderr.write('npm audit found high/critical production vulnerabilities:\n');
      for (const [packageName, vulnerability] of gated) {
        const advisory = Array.isArray(vulnerability.via)
          ? vulnerability.via.find(item => item && typeof item === 'object')
          : null;
        const chain = Array.isArray(vulnerability.via)
          ? vulnerability.via.filter(item => typeof item === 'string').join(' → ')
          : '';
        const title = oneLine(advisory?.title);
        const url = oneLine(advisory?.url);
        const details = [];
        if (title) details.push(title);
        else if (chain) details.push(`via ${chain}`);
        if (url) details.push(url);
        if (vulnerability.fixAvailable === true) details.push('fix available');
        else if (vulnerability.fixAvailable && typeof vulnerability.fixAvailable === 'object') {
          const fixName = oneLine(vulnerability.fixAvailable.name);
          const fixVersion = oneLine(vulnerability.fixAvailable.version);
          details.push(`fix available${fixName ? ` via ${fixName}${fixVersion ? `@${fixVersion}` : ''}` : ''}`);
        }
        stderr.write(
          `- ${packageName} (${vulnerability.severity})`
          + `${details.length > 0 ? `: ${details.join(' — ')}` : ''}\n`,
        );
      }
      return;
    }
  }

  const structuredError = body?.error;
  if (structuredError && typeof structuredError === 'object') {
    const code = oneLine(structuredError.code);
    const detail = oneLine(
      structuredError.summary ?? structuredError.message ?? structuredError.detail,
    );
    stderr.write(`npm audit failed${code ? ` (${code})` : ''}${detail ? `: ${detail}` : ''}.\n`);
    return;
  }

  if (result?.error) {
    const code = oneLine(result.error.code);
    const detail = oneLine(result.error.message);
    stderr.write(`npm audit could not run${code ? ` (${code})` : ''}${detail ? `: ${detail}` : ''}.\n`);
    return;
  }

  // Unknown/non-JSON output cannot be classified safely. Preserve it for
  // diagnosis, but never retry it.
  writeOutput(stdout, result?.stdout);
  writeOutput(stderr, result?.stderr);
}

async function runAudit({
  spawn = spawnSync,
  sleep = wait,
  stdout = process.stdout,
  stderr = process.stderr,
} = {}) {
  for (let attempt = 1; attempt <= MAX_ATTEMPTS; attempt += 1) {
    const result = spawn('npm', AUDIT_ARGS, {
      cwd: APP_ROOT,
      encoding: 'utf8',
      killSignal: 'SIGKILL',
      maxBuffer: 8 * 1024 * 1024,
      timeout: ATTEMPT_TIMEOUT_MS,
    });

    if (result.status === 0) {
      if (attempt > 1) stderr.write(`npm audit passed on attempt ${attempt}/${MAX_ATTEMPTS}.\n`);
      writeSuccess(auditBody(result), stdout);
      return 0;
    }

    const exitCode = Number.isInteger(result.status) && result.status > 0 ? result.status : 1;
    const body = auditBody(result);
    const errorCode = auditTransportCode(result, body);
    if (!isRetryableTransportCode(errorCode)) {
      writeNonRetryableFailure(result, body, stdout, stderr);
      return exitCode;
    }
    if (attempt === MAX_ATTEMPTS) {
      const detail = oneLine(body?.error?.summary ?? body?.error?.detail ?? body?.error?.message);
      stderr.write(
        `npm audit registry transport failed after ${MAX_ATTEMPTS} attempts (${errorCode})`
        + `${detail ? `: ${detail}` : ''}.\n`,
      );
      return exitCode;
    }

    const delay = RETRY_DELAYS_MS[attempt - 1];
    stderr.write(
      `npm audit transport failure ${errorCode}; retrying in ${delay / 1000}s `
      + `(${attempt}/${MAX_ATTEMPTS})\n`,
    );
    await sleep(delay);
  }

}

if (require.main === module) {
  runAudit().then(exitCode => {
    process.exitCode = exitCode;
  }).catch(error => {
    console.error(error);
    process.exitCode = 1;
  });
}

module.exports = {
  ATTEMPT_TIMEOUT_MS,
  AUDIT_ARGS,
  MAX_ATTEMPTS,
  RETRY_DELAYS_MS,
  TOTAL_RETRY_BUDGET_MS,
  runAudit,
};
