/**
 * Unit coverage for `fetchWithTransientRetry`'s wait arithmetic.
 *
 * The retryable-status matrix is exercised through its callers in
 * qurl-api.test.ts; what lives here is the part with no caller-visible shape —
 * how long the helper waits, and which responses are allowed to influence that.
 * These are the promises the module header makes ("clamped", "opt-in",
 * "delta-seconds only", "503 only"), so they get direct assertions rather than
 * being inferred from a revoke's boolean.
 */

import { fetchWithTransientRetry } from '../helpers/http';

const url = 'https://api.example.com/v1/resources/abc';
const originalFetch = global.fetch;
const fetchMock = jest.fn();
let warnSpy: jest.SpyInstance;

/** A fresh Response per call: `mockResolvedValue` would hand the same instance
 * to every attempt, and the helper cancels the body of each one it discards. */
function respond(status: number, headers: Record<string, string> = {}) {
  return () => new Response(null, { status, headers });
}

beforeEach(() => {
  fetchMock.mockReset();
  global.fetch = fetchMock as typeof fetch;
  // The helper warns on every retry by design; keep the suite output readable.
  warnSpy = jest.spyOn(console, 'warn').mockImplementation(() => {});
  jest.useFakeTimers();
});

afterEach(() => {
  jest.useRealTimers();
  warnSpy.mockRestore();
});

afterAll(() => {
  global.fetch = originalFetch;
});

/** Drive the helper to completion under fake timers, reporting the virtual ms
 * elapsed before each retry fired — the value under test. */
async function waitsBefore(
  options: Parameters<typeof fetchWithTransientRetry>[2],
): Promise<number[]> {
  const startedAt = Date.now();
  const waits: number[] = [];
  fetchMock.mockImplementation(() => {
    if (fetchMock.mock.calls.length > 1) waits.push(Date.now() - startedAt);
    return Promise.resolve(new Response(null, { status: 503, headers: { 'Retry-After': '600' } }));
  });
  const pending = fetchWithTransientRetry(url, { method: 'DELETE' }, options);
  await jest.advanceTimersByTimeAsync(10 * 60_000);
  await pending;
  return waits;
}

test('clamps an absurd Retry-After to the caller ceiling', async () => {
  // `Retry-After: 600` against a 35s ceiling — the "hostile or absurd
  // directive" the option promises to cap. Without the clamp this is a 10min
  // stall, well past every jest timeout in the suite.
  expect(await waitsBefore({ maxAttempts: 2, maxRetryAfterMs: 35_000 })).toEqual([35_000]);
});

test('ignores Retry-After entirely when the caller does not opt in', async () => {
  // The dark-503 guard: same status, same header, no opt-in -> 1s local backoff.
  expect(await waitsBefore({ maxAttempts: 2 })).toEqual([1_000]);
});

test('keeps the local backoff when it already exceeds the directive', async () => {
  fetchMock.mockImplementation(respond(503, { 'Retry-After': '1' }));
  const pending = fetchWithTransientRetry(
    url, { method: 'DELETE' }, { maxAttempts: 2, baseDelayMs: 5_000, maxRetryAfterMs: 35_000 },
  );
  await jest.advanceTimersByTimeAsync(4_999);
  expect(fetchMock).toHaveBeenCalledTimes(1); // a 1s directive must not shorten it
  await jest.advanceTimersByTimeAsync(1);
  await pending;
  expect(fetchMock).toHaveBeenCalledTimes(2);
});

test.each([
  ['the HTTP-date form', 'Wed, 21 Oct 2026 07:28:00 GMT'],
  ['a negative value', '-30'],
  ['a garbage value', 'soon'],
])('falls back to the local backoff for %s', async (_description, value) => {
  fetchMock.mockImplementation(respond(503, { 'Retry-After': value }));
  const pending = fetchWithTransientRetry(
    url, { method: 'DELETE' }, { maxAttempts: 2, maxRetryAfterMs: 35_000 },
  );
  await jest.advanceTimersByTimeAsync(1_000);
  expect(fetchMock).toHaveBeenCalledTimes(2); // not NaN, not 35s
  await pending;
});

test('does not honor Retry-After on a 429 even when opted in', async () => {
  // Scoped to 503 on purpose: a 429 directive means "you burst", and letting
  // one shed DELETE cost 35s would blow the serial cleanup sweeps that budget
  // ~2s each (concurrency.test.ts's 180s afterAll over ~60 resources).
  fetchMock.mockImplementation(respond(429, { 'Retry-After': '30' }));
  const pending = fetchWithTransientRetry(
    url, { method: 'DELETE' }, { maxAttempts: 2, maxRetryAfterMs: 35_000 },
  );
  await jest.advanceTimersByTimeAsync(1_000);
  expect(fetchMock).toHaveBeenCalledTimes(2);
  await pending;
});

test('treats Retry-After: 0 as no directive', async () => {
  // Passes /^\d+$/ and yields 0, so the local backoff must still apply rather
  // than the helper firing an immediate retry.
  fetchMock.mockImplementation(respond(503, { 'Retry-After': '0' }));
  const pending = fetchWithTransientRetry(
    url, { method: 'DELETE' }, { maxAttempts: 2, maxRetryAfterMs: 35_000 },
  );
  await jest.advanceTimersByTimeAsync(999);
  expect(fetchMock).toHaveBeenCalledTimes(1);
  await jest.advanceTimersByTimeAsync(1);
  await pending;
  expect(fetchMock).toHaveBeenCalledTimes(2);
});
