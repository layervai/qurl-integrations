import * as qurl from '../helpers/qurl-api';

const mintUrl = 'https://api.example.com/v1/qurls';
const apiKey = 'test-key';
const qurlId = 'q_0123456789a';
const publicResourceId = 'MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEtest_public_resource_id';

const originalFetch = global.fetch;
const fetchMock = jest.fn();

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

beforeEach(() => {
  fetchMock.mockReset();
  global.fetch = fetchMock as typeof fetch;
});

afterAll(() => {
  global.fetch = originalFetch;
});

test.each([
  ['enveloped response', { data: { resource_id: publicResourceId, qurl_link: 'https://qurl.link/a', qurl_id: qurlId } }],
  ['bare response', { resource_id: publicResourceId, link: 'https://qurl.link/a', id: qurlId }],
  [
    'bare response with an unrelated data field',
    { resource_id: publicResourceId, link: 'https://qurl.link/a', id: qurlId, data: { unrelated: true } },
  ],
])('mintLink accepts a valid %s', async (_description, body) => {
  fetchMock.mockResolvedValueOnce(jsonResponse(body));

  await expect(
    qurl.mintLink(`${mintUrl}//`, apiKey, { target_url: 'https://example.com' }),
  ).resolves.toEqual({
    resource_id: publicResourceId,
    qurl_link: 'https://qurl.link/a',
    qurl_id: qurlId,
  });
  expect(fetchMock).toHaveBeenCalledWith(mintUrl, expect.objectContaining({ method: 'POST' }));
});

test.each([
  ['missing token fields', { data: { resource_id: publicResourceId } }],
  [
    'empty resource ID',
    { data: { resource_id: '', qurl_link: 'https://qurl.link/a', qurl_id: qurlId } },
  ],
])('mintLink rejects a response with %s', async (_description, body) => {
  fetchMock.mockResolvedValueOnce(jsonResponse(body));

  await expect(
    qurl.mintLink(mintUrl, apiKey, { target_url: 'https://example.com' }),
  ).rejects.toThrow(/invalid response shape/);
});

test('mintLink does not replay a rejected POST', async () => {
  fetchMock.mockRejectedValueOnce(new Error('simulated transport failure'));

  await expect(
    qurl.mintLink(mintUrl, apiKey, { target_url: 'https://example.com' }),
  ).rejects.toThrow(/simulated transport failure/);
  expect(fetchMock).toHaveBeenCalledTimes(1);
});

test('getLinkStatus strips trailing slashes and selects the token summary', async () => {
  fetchMock.mockResolvedValueOnce(jsonResponse({
    data: {
      resource_id: publicResourceId,
      status: 'active',
      qurls: [
        {
          qurl_id: qurlId,
          use_count: 1,
          status: 'consumed',
          expires_at: '2026-07-14T00:00:00Z',
        },
      ],
    },
  }));

  await expect(qurl.getLinkStatus(`${mintUrl}//`, apiKey, qurlId)).resolves.toMatchObject({
    qurl_id: qurlId,
    use_count: 1,
    status: 'consumed',
  });
  expect(fetchMock).toHaveBeenCalledWith(
    `${mintUrl}/${qurlId}`,
    { headers: { Authorization: `Bearer ${apiKey}` } },
  );
});

test('getResourceStatus keeps a soft-revoked resource visible', async () => {
  fetchMock.mockResolvedValueOnce(jsonResponse({
    data: { resource_id: publicResourceId, status: 'revoked' },
  }));

  await expect(
    qurl.getResourceStatus(mintUrl, apiKey, publicResourceId),
  ).resolves.toMatchObject({ resource_id: publicResourceId, status: 'revoked' });
});

test('getResourceStatus rejects a mismatched echoed resource ID', async () => {
  fetchMock.mockResolvedValueOnce(jsonResponse({
    data: { resource_id: 'different-public-resource-id', status: 'active' },
  }));

  await expect(
    qurl.getResourceStatus(mintUrl, apiKey, publicResourceId),
  ).rejects.toThrow(/returned mismatched resource/);
});

test('getResourceStatus accepts a bare response even when it has a data field', async () => {
  fetchMock.mockResolvedValueOnce(jsonResponse({
    resource_id: publicResourceId,
    status: 'active',
    data: { unrelated: true },
  }));

  await expect(
    qurl.getResourceStatus(mintUrl, apiKey, publicResourceId),
  ).resolves.toMatchObject({ resource_id: publicResourceId, status: 'active' });
});

test('getResourceStatus returns validated qURL summaries', async () => {
  fetchMock.mockResolvedValueOnce(jsonResponse({
    data: {
      resource_id: publicResourceId,
      status: 'active',
      qurls: [{ qurl_id: qurlId, use_count: 0, status: 'active' }],
    },
  }));

  await expect(
    qurl.getResourceStatus(mintUrl, apiKey, publicResourceId),
  ).resolves.toMatchObject({
    qurls: [{ qurl_id: qurlId, use_count: 0, status: 'active' }],
  });
});

test('getResourceStatus rejects an invalid resource shape', async () => {
  fetchMock.mockResolvedValueOnce(jsonResponse({ data: {} }));

  await expect(
    qurl.getResourceStatus(mintUrl, apiKey, publicResourceId),
  ).rejects.toThrow(/invalid resource shape/);
});

test('getResourceStatus rejects an invalid qURL summary shape', async () => {
  fetchMock.mockResolvedValueOnce(jsonResponse({
    data: {
      resource_id: publicResourceId,
      status: 'active',
      qurls: [{ qurl_id: qurlId, use_count: 'invalid', status: 'active' }],
    },
  }));

  await expect(
    qurl.getResourceStatus(mintUrl, apiKey, publicResourceId),
  ).rejects.toThrow(/invalid token status shape.*qurls\[0\]/);
});

test.each([
  ['expires_at', { expires_at: 123 }, /invalid expires_at/],
  ['qURLs preview', { qurls: {} }, /invalid qURL preview/],
])('getResourceStatus identifies an invalid %s', async (_description, invalidField, message) => {
  fetchMock.mockResolvedValueOnce(jsonResponse({
    data: { resource_id: publicResourceId, status: 'active', ...invalidField },
  }));

  await expect(
    qurl.getResourceStatus(mintUrl, apiKey, publicResourceId),
  ).rejects.toThrow(message);
});

test('resource polling maps only an HTTP 404 to null', async () => {
  fetchMock.mockResolvedValueOnce(jsonResponse({ error: 'not found' }, 404));
  await expect(qurl.pollResourceStatus(
    mintUrl,
    apiKey,
    publicResourceId,
    (status) => status !== null,
    { timeoutMs: 0 },
  )).resolves.toBeNull();

  fetchMock.mockResolvedValueOnce(jsonResponse({ error: 'forbidden' }, 403));
  await expect(qurl.pollResourceStatus(
    mintUrl,
    apiKey,
    publicResourceId,
    (status) => status !== null,
    { timeoutMs: 0 },
  )).rejects.toThrow(/403/);
});

test('direct token lookup rejects a parent response without the requested token', async () => {
  fetchMock.mockResolvedValueOnce(jsonResponse({
    data: {
      resource_id: publicResourceId,
      status: 'active',
      qurls: [{ qurl_id: 'q_different', use_count: 0, status: 'active' }],
    },
  }));

  await expect(qurl.getLinkStatus(mintUrl, apiKey, qurlId)).rejects.toThrow(
    /without the requested token summary/,
  );
});

test('direct token lookup rejects a resource ID before making a request', async () => {
  await expect(
    qurl.getLinkStatus(mintUrl, apiKey, publicResourceId),
  ).rejects.toThrow(/requires a qurl_id.*getResourceStatus/);
  expect(fetchMock).not.toHaveBeenCalled();
});

test('token polling maps only an HTTP 404 to null', async () => {
  fetchMock.mockResolvedValueOnce(jsonResponse({ error: 'not found' }, 404));
  await expect(qurl.pollLinkStatus(
    mintUrl,
    apiKey,
    qurlId,
    (status) => status !== null,
    { timeoutMs: 0 },
  )).resolves.toBeNull();

  fetchMock.mockResolvedValueOnce(jsonResponse({ error: 'unauthorized' }, 401));
  await expect(qurl.pollLinkStatus(
    mintUrl,
    apiKey,
    qurlId,
    (status) => status !== null,
    { timeoutMs: 0 },
  )).rejects.toThrow(/401/);
});

test('direct status lookups surface non-retryable 5xx failures', async () => {
  fetchMock.mockResolvedValueOnce(jsonResponse({ error: 'resource failure' }, 500));
  await expect(
    qurl.getResourceStatus(mintUrl, apiKey, publicResourceId),
  ).rejects.toThrow(/qURL lookup failed: 500/);

  fetchMock.mockResolvedValueOnce(jsonResponse({ error: 'token failure' }, 500));
  await expect(
    qurl.getLinkStatus(mintUrl, apiKey, qurlId),
  ).rejects.toThrow(/qURL lookup failed: 500/);
});

test('status canary timeout does not misdiagnose the visibility cause', () => {
  expect(() => qurl.assertStatusVisible(null, 'test token')).toThrow(
    /did not become visible.*within the poll window/,
  );
});

test('polling retries a token that has not reached the resource preview yet', async () => {
  fetchMock.mockResolvedValueOnce(jsonResponse({
    data: { resource_id: publicResourceId, status: 'active', qurls: [] },
  }));
  fetchMock.mockResolvedValueOnce(jsonResponse({
    data: {
      resource_id: publicResourceId,
      status: 'active',
      qurls: [{ qurl_id: qurlId, use_count: 0, status: 'active' }],
    },
  }));

  await expect(qurl.pollLinkStatus(
    mintUrl,
    apiKey,
    qurlId,
    (status) => status !== null,
    { timeoutMs: 100, intervalMs: 0 },
  )).resolves.toMatchObject({ qurl_id: qurlId, status: 'active' });
  expect(fetchMock).toHaveBeenCalledTimes(2);
});

test('polling does not retry an invalid token status shape', async () => {
  fetchMock.mockResolvedValueOnce(jsonResponse({
    data: {
      resource_id: publicResourceId,
      status: 'active',
      qurls: [{ qurl_id: qurlId, use_count: 'invalid', status: 'active' }],
    },
  }));

  await expect(qurl.pollLinkStatus(
    mintUrl,
    apiKey,
    qurlId,
    (status) => status !== null,
    { timeoutMs: 100, intervalMs: 0 },
  )).rejects.toThrow(/invalid token status shape/);
  expect(fetchMock).toHaveBeenCalledTimes(1);
});

test('polling returns the last observation when its predicate never matches', async () => {
  fetchMock.mockResolvedValueOnce(jsonResponse({
    data: { resource_id: publicResourceId, status: 'active' },
  }));

  await expect(qurl.pollResourceStatus(
    mintUrl,
    apiKey,
    publicResourceId,
    (status) => status?.status === 'revoked',
    { timeoutMs: 0 },
  )).resolves.toMatchObject({ status: 'active' });
  expect(fetchMock).toHaveBeenCalledTimes(1);
});
