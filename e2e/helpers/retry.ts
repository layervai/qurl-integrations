/**
 * Retry wrapper with exponential backoff.
 */

export interface RetryOptions {
  /** Maximum number of attempts (default: 3) */
  maxAttempts?: number;
  /** Initial delay in ms (default: 1000) */
  initialDelayMs?: number;
  /** Backoff multiplier (default: 2) */
  backoffMultiplier?: number;
  /** Maximum delay in ms (default: 30000) */
  maxDelayMs?: number;
  /** Optional predicate — only retry if this returns true for the error */
  retryIf?: (error: unknown) => boolean;
  /** Called before each retry with attempt number and error */
  onRetry?: (attempt: number, error: unknown) => void;
}

export async function retry<T>(
  fn: () => Promise<T>,
  options: RetryOptions = {},
): Promise<T> {
  const {
    maxAttempts = 3,
    initialDelayMs = 1_000,
    backoffMultiplier = 2,
    maxDelayMs = 30_000,
    retryIf,
    onRetry,
  } = options;

  let lastError: unknown;
  let delay = initialDelayMs;

  for (let attempt = 1; attempt <= maxAttempts; attempt++) {
    try {
      return await fn();
    } catch (error) {
      lastError = error;

      if (attempt === maxAttempts) break;
      if (retryIf && !retryIf(error)) break;

      onRetry?.(attempt, error);

      await sleep(delay);
      delay = Math.min(delay * backoffMultiplier, maxDelayMs);
    }
  }

  throw lastError;
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

/**
 * Retry with a simple constant delay (no backoff).
 */
export async function retryFixed<T>(
  fn: () => Promise<T>,
  attempts: number = 3,
  delayMs: number = 2_000,
): Promise<T> {
  return retry(fn, {
    maxAttempts: attempts,
    initialDelayMs: delayMs,
    backoffMultiplier: 1,
  });
}
