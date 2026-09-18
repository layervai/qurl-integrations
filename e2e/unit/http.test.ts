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
 * to every attempt. These fixtures are null-bodied so nothing is actually
 * cancelled today, but the helper does cancel each discarded body — so a shared
 * instance would stop being honest the moment anyone gives one a body. */
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
  // However the budget ends, the caller must get a real Response back for its
  // own `!res.ok` error — never a thrown or swallowed one.
  expect((await pending).status).toBe(503);
  return waits;
}

test('declines a directive above the ceiling but keeps the local retry', async () => {
  // `Retry-After: 600` against a 35s ceiling. Waiting it out is a 10min stall,
  // past every jest timeout here; clamping DOWN to 35s would re-ask inside the
  // window the server just told us to skip and fail 35s later for nothing. So
  // the directive is declined — but the ordinary 1s backoff retry still runs,
  // because opting in must never cost a caller the drain-gap retry it would
  // have had by default.
  expect(await waitsBefore({ maxAttempts: 2, maxRetryAfterMs: 35_000 })).toEqual([1_000]);
  expect(fetchMock).toHaveBeenCalledTimes(2);
  // ...and says so, rather than leaving a red with no cause in the logs.
  // Said on the SAME line as the retry it explains — one grep-able line per
  // retry decision, not two. Asserted by its parts rather than verbatim, so
  // rewording the prose doesn't fail the suite but dropping either half does.
  expect(warnSpy).toHaveBeenCalledTimes(1);
  const [line] = warnSpy.mock.calls[0] as [string];
  expect(line).toContain('retry 1/1 in 1000ms');
  expect(line).toContain('declined');
  // `toContain('600')` would be vacuous next to '600000'; pin the raw echo in
  // a form only the raw echo satisfies.
  expect(line).toContain('Retry-After: 600 =');
  expect(line).toContain('600000ms');
});

// The module treats "log the ORIGIN only, never the full URL" as a security
// invariant: the fileviewer `/view/<mint-id>` path carries a capability. There
// are now TWO warn sites, so pin the redaction on both — swapping `origin` for
// `input` in either would otherwise leave this whole suite green.
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

test('honors a directive exactly AT the ceiling', async () => {
  // The inequality is `>`, and the dark-503 discrimination is documented as
  // inclusive (30 <= 35 < 60). Without this the suite only brackets the
  // threshold into (30, 600], so flipping `>` to `>=` — the likely result of a
  // well-meaning "clamp" refactor — would survive every other test here.
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
  // The missing-header branch, distinct from the malformed-value one below:
  // opting in must not make a directive-less 503 behave differently.
  fetchMock.mockImplementation(respond(503));
  const pending = fetchWithTransientRetry(
    url, { method: 'DELETE' }, { maxAttempts: 2, maxRetryAfterMs: 35_000 },
  );
  await jest.advanceTimersByTimeAsync(1_000);
  expect(fetchMock).toHaveBeenCalledTimes(2);
  await pending;
  // ...and SAYS so, with a token CI can grep. Without it the line is
  // indistinguishable from an ordinary drain-gap retry, so a red gives no hint
  // the confirm mechanism was bypassed — the hazard revokeLink's
  // TODO(upstream-contract) names.
  expect(warnSpy).toHaveBeenCalledWith(expect.stringContaining('no usable Retry-After'));
});

test('a non-opted-in 503 does not claim a degraded directive', async () => {
  // The same shape without the opt-in is just a normal retry, so the note must
  // not fire — it would be noise on every drain-gap retry in the suite.
  fetchMock.mockImplementation(respond(503));
  const pending = fetchWithTransientRetry(url, { method: 'DELETE' }, { maxAttempts: 2 });
  await jest.advanceTimersByTimeAsync(1_000);
  await pending;
  expect(warnSpy).toHaveBeenCalledTimes(1); // an ordinary retry notice
  expect(warnSpy).not.toHaveBeenCalledWith(expect.stringContaining('no usable Retry-After'));
});

test('a padded Retry-After is honored', async () => {
  // This is why the parse needs no `.trim()`: the Headers API normalizes
  // leading/trailing whitespace when the value is set, so a padded directive
  // reaches the regex already clean. Verified rather than assumed — removing
  // the trim was only safe because of it.
  fetchMock.mockImplementation(respond(503, { 'Retry-After': '  30  ' }));
  const pending = fetchWithTransientRetry(
    url, { method: 'DELETE' }, { maxAttempts: 2, maxRetryAfterMs: 35_000 },
  );
  await jest.advanceTimersByTimeAsync(29_999);
  expect(fetchMock).toHaveBeenCalledTimes(1); // honored, not treated as garbage
  await jest.advanceTimersByTimeAsync(1);
  await pending;
  expect(fetchMock).toHaveBeenCalledTimes(2);
});

test('a non-absolute input logs the placeholder, never the path', async () => {
  // The `catch { origin = '<url>' }` arm. Redaction is a security invariant
  // here, so the fallback gets the same assertion as the happy path.
  fetchMock.mockImplementation(respond(503, { 'Retry-After': '30' }));
  const pending = fetchWithTransientRetry(
    '/v1/resources/abc', { method: 'DELETE' }, { maxAttempts: 2, maxRetryAfterMs: 35_000 },
  );
  await jest.advanceTimersByTimeAsync(30_000);
  await pending;

  const [line] = warnSpy.mock.calls[0] as [string];
  expect(line).toContain('<url>');
  expect(line).not.toContain('/v1/resources/abc');
});

test('the ceiling is per attempt, not a total budget', async () => {
  // What file-revoke.test.ts's timeouts are sized on: at maxAttempts 3 an
  // opted-in caller waits the directive TWICE, which is why revokeLink exports
  // the product rather than leaving each budget to restate it.
  fetchMock.mockImplementation(respond(503, { 'Retry-After': '30' }));
  const pending = fetchWithTransientRetry(
    url, { method: 'DELETE' }, { maxAttempts: 3, maxRetryAfterMs: 35_000 },
  );
  await jest.advanceTimersByTimeAsync(30_000);
  expect(fetchMock).toHaveBeenCalledTimes(2);
  await jest.advanceTimersByTimeAsync(30_000);
  expect(fetchMock).toHaveBeenCalledTimes(3); // 60s total, not 35s
  await pending;
});

test('honors a directive that fits under the ceiling as-is', async () => {
  // The production case: qurl-service's 30s against revokeLink's 35s ceiling.
  // The clamp must not round it down and the backoff must not shorten it.
  fetchMock.mockImplementation(respond(503, { 'Retry-After': '30' }));
  const pending = fetchWithTransientRetry(
    url, { method: 'DELETE' }, { maxAttempts: 2, maxRetryAfterMs: 35_000 },
  );
  await jest.advanceTimersByTimeAsync(29_999);
  expect(fetchMock).toHaveBeenCalledTimes(1);
  await jest.advanceTimersByTimeAsync(1);
  await pending;
  expect(fetchMock).toHaveBeenCalledTimes(2);
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
  // Scoped to 503 on purpose — see http.ts's `maxRetryAfterMs` for why.
  fetchMock.mockImplementation(respond(429, { 'Retry-After': '30' }));
  const pending = fetchWithTransientRetry(
    url, { method: 'DELETE' }, { maxAttempts: 2, maxRetryAfterMs: 35_000 },
  );
  await jest.advanceTimersByTimeAsync(1_000);
  expect(fetchMock).toHaveBeenCalledTimes(2);
  await pending;
});

test.each([
  ['502', 502],
  ['504', 504],
])('does not honor Retry-After on a %s either', async (_description, status) => {
  // Retryable for an idempotent method, but not a status this stack attaches a
  // meaningful directive to — only 503 is. Rounds out the scoping matrix.
  fetchMock.mockImplementation(respond(status, { 'Retry-After': '30' }));
  const pending = fetchWithTransientRetry(
    url, { method: 'DELETE' }, { maxAttempts: 2, maxRetryAfterMs: 35_000 },
  );
  await jest.advanceTimersByTimeAsync(1_000);
  expect(fetchMock).toHaveBeenCalledTimes(2);
  await pending;
});

test('treats Retry-After: 0 as an honored directive of zero', async () => {
  // Passes /^\d+$/ and yields 0, so the local backoff must still apply rather
  // than the helper firing an immediate retry — and it must NOT be reported as
  // a bypassed confirm mechanism: a parsed 0 means "retry now", which is the
  // mechanism working, not degrading.
  fetchMock.mockImplementation(respond(503, { 'Retry-After': '0' }));
  const pending = fetchWithTransientRetry(
    url, { method: 'DELETE' }, { maxAttempts: 2, maxRetryAfterMs: 35_000 },
  );
  await jest.advanceTimersByTimeAsync(999);
  expect(fetchMock).toHaveBeenCalledTimes(1);
  await jest.advanceTimersByTimeAsync(1);
  await pending;
  expect(fetchMock).toHaveBeenCalledTimes(2);
  // The property that actually lives in `degraded`: a parsed 0 is the mechanism
  // working, so it must not be reported as a bypass. Asserted on the warn line
  // it would appear in — nothing calls console.error, so an errorSpy assertion
  // here would pass no matter which branch `'0'` fell into.
  expect(warnSpy).not.toHaveBeenCalledWith(expect.stringContaining('no usable Retry-After'));
});
