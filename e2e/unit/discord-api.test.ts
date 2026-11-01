import { api, DiscordApiError } from '../helpers/discord-api';

const originalFetch = global.fetch;
const fetchMock = jest.fn();
beforeEach(() => {
  fetchMock.mockReset();
  global.fetch = fetchMock as typeof fetch;
});
afterAll(() => { global.fetch = originalFetch; });

test.each([403, 404, 500])('API errors expose the HTTP status %d', async status => {
  fetchMock.mockResolvedValueOnce(new Response('body includes : 404 and : 403 codes', { status }));
  const error = await api('test-token', 'GET', '/test').catch((caught: unknown) => caught);
  expect(error).toBeInstanceOf(DiscordApiError);
  expect(error).toMatchObject({ status });
});

test('returns the requested response shape and passes the request body', async () => {
  fetchMock.mockResolvedValueOnce(new Response(JSON.stringify({ id: 'message' }), { status: 200 }));
  const result = await api<{ id: string }>('test-token', 'POST', '/test', { content: 'test' });
  expect(result.id).toBe('message');
  expect(fetchMock).toHaveBeenCalledWith('https://discord.com/api/v10/test', expect.objectContaining({
    method: 'POST', body: JSON.stringify({ content: 'test' }),
    headers: { Authorization: 'Bot test-token', 'Content-Type': 'application/json' },
  }));
});

test('keeps the existing null result for no-content responses', async () => {
  fetchMock.mockResolvedValueOnce(new Response(null, { status: 204 }));
  await expect(api<null>('test-token', 'DELETE', '/test')).resolves.toBeNull();
});

test('preserves a network failure without a fabricated HTTP status', async () => {
  const failure = new TypeError('network unavailable');
  fetchMock.mockRejectedValueOnce(failure);
  await expect(api('test-token', 'GET', '/test')).rejects.toBe(failure);
});
