import { OAuthCoreError } from './errors.js';

/** Read a response stream to a fixed byte cap without assuming a JSON body. */
export async function readBoundedBytes(
  response: Response,
  limitBytes: number,
  errors: { readonly invalidLimit: () => Error; readonly tooLarge: () => Error },
): Promise<Uint8Array> {
  if (!Number.isSafeInteger(limitBytes) || limitBytes < 1) {
    throw errors.invalidLimit();
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
      if (read.done) break;
      total += read.value.byteLength;
      if (total > limitBytes) {
        await reader.cancel().catch(() => undefined);
        throw errors.tooLarge();
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

export async function readBoundedBody(
  response: Response,
  limitBytes: number,
  tooLargeCode: 'TOKEN_RESPONSE_TOO_LARGE' | 'JWKS_RESPONSE_TOO_LARGE',
): Promise<Uint8Array> {
  return readBoundedBytes(response, limitBytes, {
    invalidLimit: () => new OAuthCoreError('INVALID_INPUT', 'Response body limit is invalid.'),
    tooLarge: () => new OAuthCoreError(tooLargeCode, 'OAuth provider response exceeded the configured size limit.'),
  });
}

export function decodeUtf8WithError(body: Uint8Array, invalidUtf8: () => Error): string {
  try {
    return new TextDecoder('utf-8', { fatal: true }).decode(body);
  } catch {
    throw invalidUtf8();
  }
}

export function decodeUtf8(body: Uint8Array): string {
  return decodeUtf8WithError(body, () => new OAuthCoreError('TOKEN_INVALID_RESPONSE', 'OAuth provider returned invalid UTF-8.'));
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
