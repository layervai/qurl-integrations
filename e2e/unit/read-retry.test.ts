import { api } from '../helpers/discord-api';
import { accessLink, accessLinkNoRedirect } from '../helpers/qurl-api';

const url = 'https://qurl.link/#secret-test-capability';
const readers = [
  ['SPA access', () => accessLink(url)],
  ['SPA access without redirects', () => accessLinkNoRedirect(url)],
  ['Discord GET', () => api('secret-test-token', 'GET', '/users/@me')],
] as const;
const originalFetch = global.fetch;
const fetchMock = jest.fn();
let warn: jest.SpyInstance;
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

test.each(readers)('%s respects a fractional Retry-After before retrying', async (_name, read) => {
  fetchMock.mockResolvedValueOnce(new Response(null, { status: 429, headers: { 'Retry-After': '0.4' } }))
    .mockResolvedValueOnce(new Response('{}'));
  const pending = read();
  await jest.advanceTimersByTimeAsync(399);
  expect(fetchMock).toHaveBeenCalledTimes(1);
  await jest.advanceTimersByTimeAsync(1);
  await pending;
  expect(fetchMock).toHaveBeenCalledTimes(2);
  expect(JSON.stringify(warn.mock.calls)).not.toMatch(/secret-test/);
});

test.each(readers)('%s bounds gateway retries to three attempts', async (_name, read) => {
  fetchMock.mockImplementation(async () => new Response(null, { status: 503 }));
  const pending = read().catch((error: unknown) => error);
  await jest.advanceTimersByTimeAsync(3000);
  await pending;
  expect(fetchMock).toHaveBeenCalledTimes(3);
});

test.each(readers)('%s declines a long rate limit instead of retrying early', async (_name, read) => {
  fetchMock.mockImplementation(async () => new Response(null, { status: 429, headers: { 'Retry-After': '11' } }));
  await read().catch((error: unknown) => error);
  expect(fetchMock).toHaveBeenCalledTimes(1);
  expect(jest.getTimerCount()).toBe(0);
});

test.each(readers)('%s preserves network errors without replay', async (_name, read) => {
  const failure = new TypeError('connection reset');
  fetchMock.mockRejectedValue(failure);
  await expect(read()).rejects.toBe(failure);
  expect(fetchMock).toHaveBeenCalledTimes(1);
});

test.each(['POST', 'PATCH'])('Discord %s stays non-retrying on an ambiguous 503', async (method) => {
  fetchMock.mockResolvedValue(new Response(null, { status: 503 }));
  await expect(api('test-token', method, '/test', { content: 'test' })).rejects.toMatchObject({ status: 503 });
  expect(fetchMock).toHaveBeenCalledTimes(1);
});

test('parallel reads each get a separate rate-limit wait budget', async () => {
  const seen = new Set<string>();
  fetchMock.mockImplementation(async (input: string) => {
    if (!seen.has(input)) {
      seen.add(input);
      return new Response(null, { status: 429, headers: { 'Retry-After': '10' } });
    }
    return new Response('{}');
  });
  const pending = Promise.all([accessLink(`${url}-1`), accessLink(`${url}-2`)]);
  await jest.advanceTimersByTimeAsync(9999);
  expect(fetchMock).toHaveBeenCalledTimes(2);
  await jest.advanceTimersByTimeAsync(1);
  expect((await pending).every(result => result.ok)).toBe(true);
  expect(fetchMock).toHaveBeenCalledTimes(4);
});
