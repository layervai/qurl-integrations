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

// qurl-service#1402 adds `additionalProperties: false` to the mint schemas, so
// a body key outside CreateQurlRequest is a 400 rather than the silent drop it
// used to be. This is the table guard: it reads the body mintLink actually put
// on the wire and fails on any key the schema does not declare. The allowed
// list is imported, not re-typed, so it cannot drift from the helper.
test.each([
  ['defaults only', {}],
  ['every supported option', { target_url: 'https://example.com', expires_in: '7d', label: 'a label' }],
  ['empty target_url (the negative-path shape)', { target_url: '' }],
])('mintLink sends no field outside CreateQurlRequest: %s', async (_description, opts) => {
  fetchMock.mockResolvedValueOnce(
    jsonResponse({ data: { resource_id: publicResourceId, qurl_link: 'https://qurl.link/a', qurl_id: qurlId } }),
  );

  await qurl.mintLink(mintUrl, apiKey, opts);

  const [, init] = fetchMock.mock.calls[0] as [string, RequestInit];
  const body = JSON.parse(String(init.body)) as Record<string, unknown>;
  const undeclared = Object.keys(body).filter(
    (k) => !(qurl.CREATE_QURL_ALLOWED_KEYS as readonly string[]).includes(k),
  );
  expect(undeclared).toEqual([]);
});

test('mintLink omits target_url entirely when unset rather than sending undefined', () => {
  // JSON.stringify already drops an undefined value, so this is a guard on the
  // shape staying that way: `POST /v1/qurls` requires target_url, and the
  // negative-path suite asserts the 400 that a body without it produces.
  fetchMock.mockResolvedValueOnce(
    jsonResponse({ data: { resource_id: publicResourceId, qurl_link: 'https://qurl.link/a', qurl_id: qurlId } }),
  );

  return qurl.mintLink(mintUrl, apiKey, {}).then(() => {
    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(JSON.parse(String(init.body))).not.toHaveProperty('target_url');
  });
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

describe('revokeLink retry path', () => {
  let warnSpy: jest.SpyInstance;
  beforeEach(() => {
    warnSpy = jest.spyOn(console, 'warn').mockImplementation(() => {});
    jest.useFakeTimers();
  });
  afterEach(() => {
    jest.useRealTimers();
    warnSpy.mockRestore();
  });

  const pending503 = () => new Response('protection update pending', {
    status: 503, headers: { 'Retry-After': '30' },
  });

  test('confirms a pending revoke after the server delay', async () => {
    fetchMock.mockImplementationOnce(pending503)
      .mockImplementationOnce(() => new Response(null, { status: 204 }));
    const pending = qurl.revokeLink(mintUrl, apiKey, publicResourceId);
    await jest.advanceTimersByTimeAsync(31_999);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    await jest.advanceTimersByTimeAsync(1);
    await expect(pending).resolves.toBe(true);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(String(fetchMock.mock.calls[0][0])).toBe(
      `https://api.example.com/v1/resources/${encodeURIComponent(publicResourceId)}`,
    );
    expect(String(fetchMock.mock.calls[1][0])).toBe(String(fetchMock.mock.calls[0][0]));
    expect(fetchMock.mock.calls[1][1]).toMatchObject({
      method: 'DELETE', headers: { Authorization: `Bearer ${apiKey}` },
    });
  });

  test.each([400, 401, 403, 404, 409, 410, 429, 500, 502, 503, 504])(
    'does not accept a failed confirmation (%s) or substitute a management read', async (status) => {
      fetchMock.mockImplementationOnce(pending503)
        .mockImplementation(() => new Response(null, { status }));
      const pending = qurl.revokeLink(mintUrl, apiKey, publicResourceId);
      await jest.advanceTimersByTimeAsync(qurl.REVOKE_CONFIRM_WAIT_MS);
      await expect(pending).resolves.toBe(false);
      expect(fetchMock).toHaveBeenCalledTimes(2);
      expect(warnSpy).toHaveBeenCalledWith(
        expect.stringContaining(`DELETE returned ${status}; protection update not confirmed`),
      );
    },
  );

  test.each([true, false])('accepts an immediate 204 (confirmPending=%s)', async (confirmPending) => {
    fetchMock.mockImplementation(() => new Response(null, { status: 204 }));
    await expect(qurl.revokeLink(mintUrl, apiKey, publicResourceId, { confirmPending })).resolves.toBe(true);
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  test('cleanup sends only one DELETE during an outage', async () => {
    fetchMock.mockImplementation(pending503);
    await expect(qurl.revokeLink(mintUrl, apiKey, publicResourceId, {
      confirmPending: false,
    })).resolves.toBe(false);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(warnSpy).not.toHaveBeenCalled();
  });

  test('the confirmation delay stays within the exported budget', async () => {
    const startedAt = Date.now();
    const firedAt: number[] = [];
    fetchMock.mockImplementation(() => {
      firedAt.push(Date.now() - startedAt);
      return new Response(null, { status: 503, headers: { 'Retry-After': '35' } });
    });
    const pending = qurl.revokeLink(mintUrl, apiKey, publicResourceId);
    await jest.advanceTimersByTimeAsync(qurl.REVOKE_CONFIRM_WAIT_MS);
    await expect(pending).resolves.toBe(false);
    expect(firedAt).toEqual([0, qurl.REVOKE_CONFIRM_WAIT_MS]);
  });
});

// The dark-503 guard, from the other side: a caller that never asked for the
// wait keeps its 1s local backoff even when handed a directive. (Why the wait
// is opt-in at all is on http.ts's `maxRetryAfterMs`.)
test('getResourceStatus does not opt in, so it ignores Retry-After', async () => {
  jest.useFakeTimers();
  // Its own spy: this one lives outside the describe above but still drives a
  // retry, so it would otherwise print a [fetchWithTransientRetry] line.
  const warnSpy = jest.spyOn(console, 'warn').mockImplementation(() => {});
  try {
    fetchMock
      .mockResolvedValueOnce(
        new Response(null, { status: 503, headers: { 'Retry-After': '60' } }),
      )
      .mockResolvedValueOnce(jsonResponse({ data: { resource_id: publicResourceId, status: 'active' } }));

    const pending = qurl.getResourceStatus(mintUrl, apiKey, publicResourceId);
    await jest.advanceTimersByTimeAsync(1_000);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    await expect(pending).resolves.toMatchObject({ status: 'active' });
  } finally {
    jest.useRealTimers();
    warnSpy.mockRestore();
  }
});

 test('connector mint retains child cleanup identity and rejects incomplete receipts', async () => {
  const link = { qurl_link: 'https://qurl.link/#private', qurl_id: 'child', expires_at: '2026-09-20T00:00:00Z' };
  fetchMock.mockResolvedValueOnce(jsonResponse({ links: [link] }));
  await expect(qurl.mintConnectorView('https://upload.example.com', 'source', apiKey, { expiresAt: link.expires_at })).resolves.toEqual(link);
  fetchMock.mockResolvedValueOnce(jsonResponse({ links: [{ qurl_link: link.qurl_link }] }));
  await expect(qurl.mintConnectorView('https://upload.example.com', 'source', apiKey, { expiresAt: link.expires_at })).rejects.toThrow('incomplete child identity');
 });

test('child cleanup resolves its shared parent and deletes only the exact child', async () => {
  fetchMock.mockResolvedValueOnce(jsonResponse({ data: { resource_id: 'r_shared', status: 'active' } }))
    .mockResolvedValueOnce(new Response(null, { status: 204 }));
  await qurl.revokeChild(mintUrl, apiKey, qurlId);
  expect(fetchMock.mock.calls[0][0]).toBe(`${mintUrl}/${qurlId}`);
  expect(String(fetchMock.mock.calls[1][0])).toBe(`https://api.example.com/v1/resources/r_shared/qurls/${qurlId}`);
  expect(fetchMock.mock.calls[1][1].method).toBe('DELETE');
});

test.each([403, 404])('child cleanup fails closed on parent lookup %s without a delete', async (status) => {
  fetchMock.mockResolvedValueOnce(jsonResponse({}, status));
  await expect(qurl.revokeChild(mintUrl, apiKey, qurlId)).rejects.toThrow(`qURL lookup failed: ${status}`);
  expect(fetchMock).toHaveBeenCalledTimes(1);
});

test('child cleanup reports a refused delete', async () => {
  fetchMock.mockResolvedValueOnce(jsonResponse({ resource_id: 'r_shared', status: 'active' }))
    .mockResolvedValueOnce(new Response(null, { status: 403 }));
  await expect(qurl.revokeChild(mintUrl, apiKey, qurlId)).rejects.toThrow('revocation: 403');
});
