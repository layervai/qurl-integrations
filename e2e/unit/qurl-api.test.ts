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
  // Only this group drives a retry, so the console.warn suppression is scoped
  // here rather than file-wide: fetchWithTransientRetry warns on every retry by
  // design, and the other ~30 tests in this file should keep their warnings.
  let warnSpy: jest.SpyInstance;
  beforeEach(() => { warnSpy = jest.spyOn(console, 'warn').mockImplementation(() => {}); });
  afterEach(() => { warnSpy.mockRestore(); });

  /** A management read answering with the given resource status. */
  const statusRead = (status: string) => () =>
    new Response(JSON.stringify({ data: { resource_id: publicResourceId, status } }), {
      status: 200,
      headers: { 'Content-Type': 'application/json' },
    });
  const pending503 = () =>
    new Response(null, { status: 503, headers: { 'Retry-After': '30' } });

  // The ordinary case, and the one every URL mint in smoke/link-lifecycle/
  // concurrency takes: unprotected resource, first DELETE answers 204. One
  // request, no wait, NO fallback read — a regression making the fallback
  // unconditional would otherwise slip through, since every other test here
  // starts from a non-2xx.
  test.each([
    ['the confirming default', undefined],
    ['the non-confirming sweep budget', { confirmPending: false }],
  ])('a first-attempt 204 returns true in one request under %s', async (_d, opts) => {
    fetchMock.mockImplementationOnce(() => new Response(null, { status: 204 }));

    await expect(
      qurl.revokeLink(mintUrl, apiKey, publicResourceId, opts),
    ).resolves.toBe(true);
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  // qurl-service commits the revocation, then answers 503 + `Retry-After: 30`
  // until the NHP protection update lands. Every connector upload is protected,
  // so a single-shot DELETE reported a false failure and red-flagged four
  // file-revoke smoke tests on a revocation that had already happened.
  test('retries the revocation-pending 503 and honors Retry-After', async () => {
    jest.useFakeTimers();
    try {
      fetchMock
        .mockImplementationOnce(pending503)
        .mockImplementationOnce(() => new Response(null, { status: 204 }));

      const promise = qurl.revokeLink(mintUrl, apiKey, publicResourceId);
      // The retry must wait out the server's directive, not the 1s local
      // backoff. Advance a full 30s after that check rather than the exact
      // remaining 29s: this is the only 503 fixture here with a real body, so
      // exact arithmetic would depend on the body cancel settling in-tick.
      await jest.advanceTimersByTimeAsync(1_000);
      expect(fetchMock).toHaveBeenCalledTimes(1);
      await jest.advanceTimersByTimeAsync(30_000);

      await expect(promise).resolves.toBe(true);
      expect(fetchMock).toHaveBeenCalledTimes(2); // no management read needed
      // The retry reuses `init`, so it carries the same method AND credential.
      expect(fetchMock.mock.calls[1][1]).toMatchObject({
        method: 'DELETE',
        headers: { Authorization: `Bearer ${apiKey}` },
      });
    } finally {
      jest.useRealTimers();
    }
  });

  // 2xx on the confirm retry is the assumption the fix would otherwise rest on
  // (the repro never observed it). Pin both shapes the service could send.
  test.each([
    ['204 No Content', 204],
    ['200 OK', 200],
  ])('accepts %s on the confirm retry without a management read', async (_d, status) => {
    jest.useFakeTimers();
    try {
      fetchMock
        .mockImplementationOnce(pending503)
        .mockImplementationOnce(() => new Response(null, { status }));

      const promise = qurl.revokeLink(mintUrl, apiKey, publicResourceId);
      await jest.advanceTimersByTimeAsync(30_000);
      await expect(promise).resolves.toBe(true);
      expect(fetchMock).toHaveBeenCalledTimes(2);
    } finally {
      jest.useRealTimers();
    }
  });

  // THE reason the fix no longer depends on an unobserved status code. If the
  // service answers 404 once the update lands — which link-lifecycle pins as a
  // legitimate second-revoke answer — the management read still says `revoked`,
  // so a committed revocation is not reported as a failure. The confirm DELETE
  // is the strong (convergence) signal; this is the correct-but-weaker one.
  test.each([
    ['404 Not Found', 404],
    ['409 Conflict', 409],
    ['410 Gone', 410],
  ])('falls back to the management read when the confirm answers %s', async (_d, status) => {
    jest.useFakeTimers();
    try {
      fetchMock
        .mockImplementationOnce(pending503)
        .mockImplementationOnce(() => new Response(null, { status }))
        .mockImplementationOnce(statusRead('revoked'));

      const promise = qurl.revokeLink(mintUrl, apiKey, publicResourceId);
      await jest.advanceTimersByTimeAsync(30_000);

      await expect(promise).resolves.toBe(true);
      expect(fetchMock).toHaveBeenCalledTimes(3);
      // Convergence is unconfirmed, so it says so rather than passing silently.
      expect(warnSpy).toHaveBeenCalledWith(
        expect.stringContaining('convergence unconfirmed'),
      );
    } finally {
      jest.useRealTimers();
    }
  });

  // ...and the fallback cannot manufacture the false positive that ruled out
  // trusting a bare 404: a resource that never existed does not read `revoked`.
  test('does not fall back into success when the resource is not revoked', async () => {
    jest.useFakeTimers();
    try {
      fetchMock
        .mockImplementationOnce(pending503)
        .mockImplementationOnce(() => new Response(null, { status: 404 }))
        .mockImplementationOnce(statusRead('active'));

      const promise = qurl.revokeLink(mintUrl, apiKey, publicResourceId);
      await jest.advanceTimersByTimeAsync(30_000);
      await expect(promise).resolves.toBe(false);
    } finally {
      jest.useRealTimers();
    }
  });

  // A dark 503 fails the management read too, so the fallback stays closed.
  test('stays false when the management read also fails', async () => {
    jest.useFakeTimers();
    try {
      fetchMock.mockImplementation(() => new Response(null, { status: 503 }));

      const promise = qurl.revokeLink(mintUrl, apiKey, publicResourceId);
      await jest.advanceTimersByTimeAsync(120_000);
      await expect(promise).resolves.toBe(false);
    } finally {
      jest.useRealTimers();
    }
  });

  // A first-attempt 404 is not retryable, so the DELETE is single-shot — but
  // the fallback still runs, and negative-paths' nonexistent resource reads
  // nothing, so it stays false.
  test('reports a 404 on a resource that never existed as failure', async () => {
    fetchMock
      .mockImplementationOnce(() => new Response(null, { status: 404 }))
      .mockImplementationOnce(() => new Response(null, { status: 404 }));

    await expect(qurl.revokeLink(mintUrl, apiKey, publicResourceId)).resolves.toBe(false);
    expect(fetchMock).toHaveBeenCalledTimes(2); // one DELETE, one read
  });

  // The deliberate widening: because DELETE is idempotent, a confirming revoke
  // also retries the non-503 transient statuses — once, on the local backoff.
  test('retries a 502 exactly once', async () => {
    jest.useFakeTimers();
    try {
      fetchMock
        .mockImplementationOnce(() => new Response(null, { status: 502, headers: { 'Retry-After': '30' } }))
        .mockImplementationOnce(() => new Response(null, { status: 502 }))
        .mockImplementationOnce(statusRead('active'));

      const promise = qurl.revokeLink(mintUrl, apiKey, publicResourceId);
      await jest.advanceTimersByTimeAsync(1_000); // local backoff, not the 30s
      await expect(promise).resolves.toBe(false);
      expect(fetchMock).toHaveBeenCalledTimes(3); // 2 DELETEs + the read
    } finally {
      jest.useRealTimers();
    }
  });

  // The fallback is skipped where the read would use the same credential and
  // fail the same way — so a systematic auth regression costs one request per
  // call site, not two.
  test.each([
    ['401 Unauthorized', 401],
    ['403 Forbidden', 403],
  ])('skips the management read on %s', async (_d, status) => {
    fetchMock.mockImplementationOnce(() => new Response(null, { status }));

    await expect(qurl.revokeLink(mintUrl, apiKey, publicResourceId)).resolves.toBe(false);
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  // The dark-503 guard at the call site the PR leans on hardest: revokeLink's
  // ceiling is BELOW the deployment-state 503's 60s directive, so that one is
  // declined rather than waited out — a deploy window costs ~1s, not ~35s.
  test('declines a directive above its ceiling', async () => {
    jest.useFakeTimers();
    try {
      fetchMock.mockImplementation(
        () => new Response(null, { status: 503, headers: { 'Retry-After': '60' } }),
      );

      const promise = qurl.revokeLink(mintUrl, apiKey, publicResourceId);
      await jest.advanceTimersByTimeAsync(10_000); // 1s backoff + the read's own
      await expect(promise).resolves.toBe(false);
      // Two DELETEs a second apart — retried, not abandoned — plus the fallback
      // read's own bounded attempts. The cost the docstring weighs against
      // fail-fast, pinned as a number rather than prose.
      expect(fetchMock.mock.calls.length).toBeGreaterThanOrEqual(3);
    } finally {
      jest.useRealTimers();
    }
  });

  // The way this fix could silently revert to the original red, end to end —
  // the hazard revokeLink's TODO(upstream-contract) names.
  test('a pending 503 without a directive reverts to the local backoff', async () => {
    jest.useFakeTimers();
    try {
      fetchMock.mockImplementation(() => new Response(null, { status: 503 }));

      const promise = qurl.revokeLink(mintUrl, apiKey, publicResourceId);
      // 999ms: the DELETE retry has NOT fired yet, so this pins the 1s local
      // backoff rather than the 30s window it should have waited.
      await jest.advanceTimersByTimeAsync(999);
      expect(fetchMock).toHaveBeenCalledTimes(1);
      await jest.advanceTimersByTimeAsync(10_000); // retry + the fallback read
      await expect(promise).resolves.toBe(false);
      expect(warnSpy).toHaveBeenCalledWith(expect.stringContaining('no usable Retry-After'));
    } finally {
      jest.useRealTimers();
    }
  });

  // Bulk cleanup opts out entirely: ONE request, no confirm wait, no fallback
  // read — cleanup.ts's serial sweep budgets ~2s per id.
  test('confirmPending: false makes it a single request', async () => {
    jest.useFakeTimers();
    try {
      fetchMock.mockImplementation(pending503);

      const promise = qurl.revokeLink(mintUrl, apiKey, publicResourceId, {
        confirmPending: false,
      });
      await jest.advanceTimersByTimeAsync(60_000);
      await expect(promise).resolves.toBe(false);
      expect(fetchMock).toHaveBeenCalledTimes(1);
    } finally {
      jest.useRealTimers();
    }
  });

  // The exported budget must match what a confirming revoke can actually spend.
  test('REVOKE_CONFIRM_WAIT_MS bounds a real confirming revoke', async () => {
    jest.useFakeTimers();
    try {
      // The ceiling case, not the 30s usually observed. Elapsed times are
      // recorded INSIDE the mock: `Date.now()` measured around the await would
      // return however far the test advanced the clock, and would pass at any
      // budget at all.
      const startedAt = Date.now();
      const firedAt: number[] = [];
      fetchMock.mockImplementation(() => {
        firedAt.push(Date.now() - startedAt);
        return Promise.resolve(
          new Response(null, { status: 503, headers: { 'Retry-After': '35' } }),
        );
      });
      const promise = qurl.revokeLink(mintUrl, apiKey, publicResourceId);
      await jest.advanceTimersByTimeAsync(qurl.REVOKE_CONFIRM_WAIT_MS);
      await jest.advanceTimersByTimeAsync(10_000); // the fallback read's own budget

      await expect(promise).resolves.toBe(false);
      // The last DELETE starts exactly at the exported budget — so the export
      // IS the worst case, not a number that happens to sit near it. Exact on
      // purpose: raising PENDING_REVOKE_ATTEMPTS doubles the EXPECTED side to
      // [0, 70_000] while the observed stays [0, 35_000], so this fails — the
      // forcing function the revoke docstring relies on. Do not loosen it.
      expect(firedAt.slice(0, 2)).toEqual([0, qurl.REVOKE_CONFIRM_WAIT_MS]);
    } finally {
      jest.useRealTimers();
    }
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
