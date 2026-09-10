import { OAuthCoreError } from './errors.js';

export async function readBoundedBody(
  response: Response,
  limitBytes: number,
  tooLargeCode: 'TOKEN_RESPONSE_TOO_LARGE' | 'JWKS_RESPONSE_TOO_LARGE',
): Promise<Uint8Array> {
  if (!Number.isSafeInteger(limitBytes) || limitBytes < 1) {
    throw new OAuthCoreError('INVALID_INPUT', 'Response body limit is invalid.');
  }
  if (response.body === null) {
    return new Uint8Array();
  }

  const reader = response.body.getReader();
  const chunks: Uint8Array[] = [];
  let total = 0;
  try {
    while (true) {
      const read = await reader.read();
      if (read.done) {
        break;
      }
      total += read.value.byteLength;
      if (total > limitBytes) {
        await reader.cancel().catch(() => undefined);
        throw new OAuthCoreError(
          tooLargeCode,
          'OAuth provider response exceeded the configured size limit.',
        );
      }
      chunks.push(read.value);
    }
  } finally {
    reader.releaseLock();
  }

  const body = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    body.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return body;
}

export function decodeUtf8(body: Uint8Array): string {
  try {
    return new TextDecoder('utf-8', { fatal: true }).decode(body);
  } catch {
    throw new OAuthCoreError('TOKEN_INVALID_RESPONSE', 'OAuth provider returned invalid UTF-8.');
  }
}

export async function withStrictTimeout<T>(
  timeoutMs: number,
  timeoutCode: 'TOKEN_TIMEOUT' | 'JWKS_TIMEOUT',
  operation: (signal: AbortSignal) => Promise<T>,
): Promise<T> {
  if (!Number.isSafeInteger(timeoutMs) || timeoutMs < 1) {
    throw new OAuthCoreError('INVALID_INPUT', 'Network timeout is invalid.');
  }

  const controller = new AbortController();
  let timeoutHandle: ReturnType<typeof setTimeout> | undefined;
  const timeout = new Promise<never>((_resolve, reject) => {
    timeoutHandle = setTimeout(() => {
      controller.abort();
      reject(new OAuthCoreError(
        timeoutCode,
        'OAuth provider request timed out.',
        { retryable: true },
      ));
    }, timeoutMs);
  });

  try {
    return await Promise.race([operation(controller.signal), timeout]);
  } finally {
    if (timeoutHandle !== undefined) {
      clearTimeout(timeoutHandle);
    }
  }
}

