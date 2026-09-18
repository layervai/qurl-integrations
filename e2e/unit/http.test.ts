import { fetchWithTransientRetry } from '../helpers/http';

const url = 'https://api.example.com/v1/resources/abc';
const originalFetch = global.fetch;
const fetchMock = jest.fn();
let warnSpy: jest.SpyInstance;

function respond(status: number, headers: Record<string, string> = {}) {
  return () => new Response(null, { status, headers });
}

beforeEach(() => {
  fetchMock.mockReset();
  global.fetch = fetchMock as typeof fetch;
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
  expect((await pending).status).toBe(503);
  return waits;
}

test('declines a directive above the ceiling but keeps the local retry', async () => {
  expect(await waitsBefore({ maxAttempts: 2, maxRetryAfterMs: 35_000 })).toEqual([1_000]);
  expect(fetchMock).toHaveBeenCalledTimes(2);
  expect(warnSpy).toHaveBeenCalledTimes(1);
  const [line] = warnSpy.mock.calls[0] as [string];
  expect(line).toContain('retry 1/1 in 1000ms');
  expect(line).toContain('declined');
  expect(line).toContain('Retry-After: 600 =');
  expect(line).toContain('600000ms');
});
test.each([
  ['the retrying path', '30'],
  ['the declined-directive path', '600'],
])('logs the origin only, never the path (%s)', async (_description, retryAfter) => {
  fetchMock.mockImplementation(respond(503, { 'Retry-After': retryAfter }));
  const pending = fetchWithTransientRetry(
    url, { method: 'DELETE' }, { maxAttempts: 2, maxRetryAfterMs: 35_000 },
  );
  await jest.advanceTimersByTimeAsync(35_000);
  await pending;

  expect(warnSpy).toHaveBeenCalled();
  for (const [line] of warnSpy.mock.calls) {
    expect(line).toContain('https://api.example.com');
    expect(line).not.toContain('/v1/resources/abc');
  }
});

test('honors a directive exactly AT the ceiling, with the pad capped', async () => {
  fetchMock.mockImplementation(respond(503, { 'Retry-After': '35' }));
  const pending = fetchWithTransientRetry(
    url, { method: 'DELETE' }, { maxAttempts: 2, maxRetryAfterMs: 35_000 },
  );
  await jest.advanceTimersByTimeAsync(34_999);
  expect(fetchMock).toHaveBeenCalledTimes(1); // honored, not declined at 1s
  await jest.advanceTimersByTimeAsync(1);
  await pending;
  expect(fetchMock).toHaveBeenCalledTimes(2);
  expect(warnSpy).not.toHaveBeenCalledWith(expect.stringContaining('declined'));
});

test('a negative ceiling cannot stop a loop the caller never opted into', async () => {
  fetchMock.mockImplementation(respond(503, { 'Retry-After': '30' }));
  const pending = fetchWithTransientRetry(
    url, { method: 'DELETE' }, { maxAttempts: 2, maxRetryAfterMs: -1 },
  );
  await jest.advanceTimersByTimeAsync(1_000);
  expect(fetchMock).toHaveBeenCalledTimes(2); // ordinary local backoff
  await pending;
});

test('a 503 with no Retry-After at all still uses the local backoff', async () => {
  fetchMock.mockImplementation(respond(503));
  const pending = fetchWithTransientRetry(
    url, { method: 'DELETE' }, { maxAttempts: 2, maxRetryAfterMs: 35_000 },
  );
  await jest.advanceTimersByTimeAsync(1_000);
  expect(fetchMock).toHaveBeenCalledTimes(2);
  await pending;
  expect(warnSpy).toHaveBeenCalledWith(expect.stringContaining('no usable Retry-After'));
});

test('a non-opted-in 503 does not claim a degraded directive', async () => {
  fetchMock.mockImplementation(respond(503));
  const pending = fetchWithTransientRetry(url, { method: 'DELETE' }, { maxAttempts: 2 });
  await jest.advanceTimersByTimeAsync(1_000);
  await pending;
  expect(warnSpy).toHaveBeenCalledTimes(1); // an ordinary retry notice
  expect(warnSpy).not.toHaveBeenCalledWith(expect.stringContaining('no usable Retry-After'));
});

test('a padded Retry-After is honored', async () => {
  fetchMock.mockImplementation(respond(503, { 'Retry-After': '  30  ' }));
  const pending = fetchWithTransientRetry(
    url, { method: 'DELETE' }, { maxAttempts: 2, maxRetryAfterMs: 35_000 },
  );
  await jest.advanceTimersByTimeAsync(31_999);
  expect(fetchMock).toHaveBeenCalledTimes(1); // honored, not treated as garbage
  await jest.advanceTimersByTimeAsync(1);
  await pending;
  expect(fetchMock).toHaveBeenCalledTimes(2);
});

test('a non-absolute input logs the placeholder, never the path', async () => {
  fetchMock.mockImplementation(respond(503, { 'Retry-After': '30' }));
  const pending = fetchWithTransientRetry(
    '/v1/resources/abc', { method: 'DELETE' }, { maxAttempts: 2, maxRetryAfterMs: 35_000 },
  );
  await jest.advanceTimersByTimeAsync(32_000);
  await pending;

  const [line] = warnSpy.mock.calls[0] as [string];
  expect(line).toContain('<url>');
  expect(line).not.toContain('/v1/resources/abc');
});

test('the ceiling is per attempt, not a total budget', async () => {
  fetchMock.mockImplementation(respond(503, { 'Retry-After': '30' }));
  const pending = fetchWithTransientRetry(
    url, { method: 'DELETE' }, { maxAttempts: 3, maxRetryAfterMs: 35_000 },
  );
  await jest.advanceTimersByTimeAsync(32_000);
  expect(fetchMock).toHaveBeenCalledTimes(2);
  await jest.advanceTimersByTimeAsync(32_000);
  expect(fetchMock).toHaveBeenCalledTimes(3); // two waits, not one
  await pending;
});

test('honors a directive under the ceiling, plus the pad', async () => {
  fetchMock.mockImplementation(respond(503, { 'Retry-After': '30' }));
  const pending = fetchWithTransientRetry(
    url, { method: 'DELETE' }, { maxAttempts: 2, maxRetryAfterMs: 35_000 },
  );
  await jest.advanceTimersByTimeAsync(31_999);
  expect(fetchMock).toHaveBeenCalledTimes(1);
  await jest.advanceTimersByTimeAsync(1);
  await pending;
  expect(fetchMock).toHaveBeenCalledTimes(2);
});

test('ignores Retry-After entirely when the caller does not opt in', async () => {
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

test.each([
  ['408 Request Timeout', 408],
  ['425 Too Early', 425],
  ['429 Too Many Requests', 429],
  ['502 Bad Gateway', 502],
  ['504 Gateway Timeout', 504],
])('does not honor Retry-After on a %s even when opted in', async (_d, status) => {
  fetchMock.mockImplementation(respond(status, { 'Retry-After': '30' }));
  const pending = fetchWithTransientRetry(
    url, { method: 'DELETE' }, { maxAttempts: 2, maxRetryAfterMs: 35_000 },
  );
  await jest.advanceTimersByTimeAsync(1_000);
  expect(fetchMock).toHaveBeenCalledTimes(2);
  await pending;
});

test('bounds and quotes an abusive Retry-After before it reaches the log', async () => {
  const abusive = `x\u0007${'A'.repeat(5_000)}`;
  fetchMock.mockImplementation(respond(503, { 'Retry-After': abusive }));
  const pending = fetchWithTransientRetry(
    url, { method: 'DELETE' }, { maxAttempts: 2, maxRetryAfterMs: 35_000 },
  );
  await jest.advanceTimersByTimeAsync(1_000);
  await pending;

  const [line] = warnSpy.mock.calls[0] as [string];
  expect(line).toContain('no usable Retry-After');
  expect(line.length).toBeLessThan(400); // bounded, not the 5KB header
  expect(line).not.toContain('\u0007'); // JSON.stringify escaped the control char
});

test('treats Retry-After: 0 as an honored directive of zero', async () => {
  fetchMock.mockImplementation(respond(503, { 'Retry-After': '0' }));
  const pending = fetchWithTransientRetry(
    url, { method: 'DELETE' }, { maxAttempts: 2, maxRetryAfterMs: 35_000 },
  );
  await jest.advanceTimersByTimeAsync(999);
  expect(fetchMock).toHaveBeenCalledTimes(1);
  await jest.advanceTimersByTimeAsync(1);
  await pending;
  expect(fetchMock).toHaveBeenCalledTimes(2);
  expect(warnSpy).not.toHaveBeenCalledWith(expect.stringContaining('no usable Retry-After'));
});
