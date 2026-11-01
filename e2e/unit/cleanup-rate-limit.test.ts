import { createServer } from 'node:http';
import { fetchWithCleanupRetry } from '../helpers/http';
import { trackedDiscordMessages, trackedQurlResources } from '../helpers/cleanup';

const originalFetch = global.fetch;
const fetchMock = jest.fn();
let warn: jest.SpyInstance;
const url = 'https://api.example.com/private-capability';
const rateLimited = (hint?: string) => new Response('rate limited', {
  status: 429, headers: hint === undefined ? {} : { 'Retry-After': hint },
});
beforeEach(() => {
  jest.useFakeTimers();
  fetchMock.mockReset();
  global.fetch = fetchMock as typeof fetch;
  warn = jest.spyOn(console, 'warn').mockImplementation(() => {});
});
afterEach(() => {
  global.fetch = originalFetch;
  jest.useRealTimers();
  warn.mockRestore();
});

test.each([
  ['2', 2000], ['0.5', 500], ['0', 250], ['.25', 250],
  [undefined, 1000], ['invalid', 1000], ['-1', 1000], ['12x', 1000], ['1e3', 1000],
])('honors Retry-After %s, releases the body, and reuses the exact request', async (hint, delay) => {
  const response = rateLimited(hint);
  const cancel = jest.spyOn(response.body!, 'cancel');
  fetchMock.mockResolvedValueOnce(response).mockResolvedValueOnce(new Response(null, { status: 204 }));
  const init = { method: 'DELETE', headers: { Authorization: 'Bearer test-key' } };
  const budget = { remainingMs: 10_000 };
  const pending = fetchWithCleanupRetry(url, init, budget);
  await jest.advanceTimersByTimeAsync(delay - 1);
  expect(fetchMock).toHaveBeenCalledTimes(1);
  await jest.advanceTimersByTimeAsync(1);
  expect((await pending).status).toBe(204);
  expect(fetchMock.mock.calls).toEqual([[url, init], [url, init]]);
  expect(cancel).toHaveBeenCalledTimes(1);
  expect(budget.remainingMs).toBe(10_000 - delay);
  expect(warn.mock.calls.flat().join(' ')).not.toMatch(/private-capability|test-key/);
});

test('honors an HTTP-date without retrying early', async () => {
  jest.setSystemTime(new Date('2026-10-30T12:00:00Z'));
  fetchMock.mockResolvedValueOnce(rateLimited('Fri, 30 Oct 2026 12:00:03 GMT'))
    .mockResolvedValueOnce(new Response(null, { status: 204 }));
  const pending = fetchWithCleanupRetry(url, { method: 'DELETE' }, { remainingMs: 3000 });
  await jest.advanceTimersByTimeAsync(2999);
  expect(fetchMock).toHaveBeenCalledTimes(1);
  await jest.advanceTimersByTimeAsync(1);
  expect((await pending).status).toBe(204);
});

test.each(['11', '99999999999999999999999999999999999999999999999999999'])('declines an over-budget hint %s without shortening it', async (hint) => {
  const response = rateLimited(hint);
  fetchMock.mockResolvedValue(response);
  const budget = { remainingMs: 10_000 };
  expect(await fetchWithCleanupRetry(url, { method: 'DELETE' }, budget)).toBe(response);
  expect(fetchMock).toHaveBeenCalledTimes(1);
  expect(budget.remainingMs).toBe(10_000);
  expect(warn).toHaveBeenCalledWith(expect.stringContaining('declined'));
});

test.each([403, 500, 502, 503, 504])('does not retry a cleanup HTTP %s', async (status) => {
  fetchMock.mockResolvedValue(new Response(null, { status }));
  expect((await fetchWithCleanupRetry(url, { method: 'DELETE' }, { remainingMs: 10_000 })).status).toBe(status);
  expect(fetchMock).toHaveBeenCalledTimes(1);
});

test('does not replay a failed network request', async () => {
  const error = new TypeError('network failure');
  fetchMock.mockRejectedValue(error);
  await expect(fetchWithCleanupRetry(url, { method: 'DELETE' }, { remainingMs: 10_000 })).rejects.toBe(error);
  expect(fetchMock).toHaveBeenCalledTimes(1);
});

test('a source sweep shares ten seconds across entries and retains failures', async () => {
  fetchMock.mockResolvedValueOnce(rateLimited('10'))
    .mockResolvedValueOnce(new Response(null, { status: 204 }))
    .mockResolvedValueOnce(rateLimited('1'))
    .mockResolvedValueOnce(new Response(null, { status: 204 }));
  const tracked = trackedQurlResources({ MINT_API_URL: url, QURL_API_KEY: 'test-key' });
  tracked.track('first'); tracked.track('second'); tracked.track('third');
  const result = tracked.revokeAll().catch((error: Error) => error);
  await jest.advanceTimersByTimeAsync(10_500);
  expect(await result).toEqual(new Error('Source cleanup failed for: second (not confirmed)'));
  expect(fetchMock).toHaveBeenCalledTimes(4);
  expect(fetchMock.mock.calls.map(([input]) => String(input))).toEqual([
    'https://api.example.com/v1/resources/first', 'https://api.example.com/v1/resources/first',
    'https://api.example.com/v1/resources/second', 'https://api.example.com/v1/resources/third',
  ]);
  fetchMock.mockReset().mockResolvedValue(new Response(null, { status: 204 }));
  await tracked.revokeAll();
  expect(fetchMock).toHaveBeenCalledTimes(1);
  expect(String(fetchMock.mock.calls[0][0])).toContain('/second');
});

test('child lookup and exact-child DELETE share the source sweep budget', async () => {
  fetchMock.mockResolvedValueOnce(rateLimited('1'))
    .mockResolvedValueOnce(new Response(JSON.stringify({ resource_id: 'parent', status: 'active' })))
    .mockResolvedValueOnce(rateLimited('1'))
    .mockResolvedValueOnce(new Response(null, { status: 204 }));
  const tracked = trackedQurlResources({ MINT_API_URL: 'https://api.example.com/v1/qurls', QURL_API_KEY: 'test-key' });
  tracked.trackChild('q_owned');
  const pending = tracked.revokeAll();
  await jest.advanceTimersByTimeAsync(2000);
  await pending;
  expect(fetchMock).toHaveBeenCalledTimes(4);
  expect(fetchMock.mock.calls.slice(2).map(([input]) => String(input))).toEqual([
    'https://api.example.com/v1/resources/parent/qurls/q_owned',
    'https://api.example.com/v1/resources/parent/qurls/q_owned',
  ]);
});

test('message cleanup retries once, warns on persistent limits, and continues without throwing', async () => {
  fetchMock.mockResolvedValueOnce(rateLimited('0.5')).mockResolvedValueOnce(rateLimited('0.5'))
    .mockResolvedValueOnce(new Response(null, { status: 204 }));
  const tracked = trackedDiscordMessages({ BOT_TOKEN: 'test-token' });
  tracked.track({ id: 'first', channel_id: 'channel' });
  tracked.track({ id: 'second', channel_id: 'channel' });
  const pending = tracked.deleteAll();
  await jest.advanceTimersByTimeAsync(750);
  await expect(pending).resolves.toBeUndefined();
  expect(fetchMock).toHaveBeenCalledTimes(3);
  expect(warn).toHaveBeenCalledWith(expect.stringContaining('best-effort delete of message first'));
});

test('the cleanup transport retries against a real local HTTP server', async () => {
  jest.useRealTimers();
  global.fetch = originalFetch;
  const received: Array<{method?: string; url?: string; authorization?: string}> = [];
  const server = createServer((req, res) => {
    received.push({method:req.method, url:req.url, authorization:req.headers.authorization});
    if (received.length === 1) {
      res.writeHead(429, {'Retry-After':'0'});
      res.end('rate limited');
    } else { res.writeHead(204); res.end(); }
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  try {
    const address = server.address();
    if (!address || typeof address === 'string') throw new Error('missing local port');
    const response = await fetchWithCleanupRetry(`http://127.0.0.1:${address.port}/owned`, {
      method:'DELETE', headers:{Authorization:'Bearer local-fixture'},
    }, {remainingMs:1000});
    expect(response.status).toBe(204);
    expect(received).toEqual([
      {method:'DELETE', url:'/owned', authorization:'Bearer local-fixture'},
      {method:'DELETE', url:'/owned', authorization:'Bearer local-fixture'},
    ]);
  } finally {
    server.closeAllConnections();
    await new Promise<void>((resolve, reject) => server.close((error) => error ? reject(error) : resolve()));
  }
});


test.each([502, 503, 504])('child cleanup keeps its transient %s retries', async (status) => {
  fetchMock.mockResolvedValueOnce(new Response(null, { status }))
    .mockResolvedValueOnce(new Response(JSON.stringify({ resource_id: 'parent', status: 'active' })))
    .mockResolvedValueOnce(new Response(null, { status }))
    .mockResolvedValueOnce(new Response(null, { status: 204 }));
  const tracked = trackedQurlResources({ MINT_API_URL: 'https://api.example.com/v1/qurls', QURL_API_KEY: 'test-key' });
  tracked.trackChild('q_owned');
  const pending = tracked.revokeAll();
  await jest.advanceTimersByTimeAsync(2000);
  await expect(pending).resolves.toBeUndefined();
  expect(fetchMock).toHaveBeenCalledTimes(4);
});


test('child cleanup retries a persistent 429 only once', async () => {
  fetchMock.mockImplementation(() => Promise.resolve(rateLimited('1')));
  const tracked = trackedQurlResources({ MINT_API_URL: 'https://api.example.com/v1/qurls', QURL_API_KEY: 'test-key' });
  tracked.trackChild('q_owned');
  const result = tracked.revokeAll().catch((error: Error) => error);
  await jest.advanceTimersByTimeAsync(1000);
  expect(await result).toEqual(new Error('Child cleanup failed for: q_owned (HTTP 429)'));
  expect(fetchMock).toHaveBeenCalledTimes(2);
});

test('an abort stops the retry wait and prevents a second request', async () => {
  fetchMock.mockResolvedValue(rateLimited('5'));
  const controller = new AbortController();
  const result = fetchWithCleanupRetry(url, { method: 'DELETE', signal: controller.signal }, {
    remainingMs: 10_000,
  }).catch((error: Error) => error);
  await jest.advanceTimersByTimeAsync(0);
  controller.abort();
  expect(await result).toMatchObject({ name: 'AbortError' });
  expect(fetchMock).toHaveBeenCalledTimes(1);
});
