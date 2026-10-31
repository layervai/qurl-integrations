import type { APIRequestContext } from 'playwright';
import { probeScopedSibling } from '../helpers/tunnelView';
const origin = 'https://q-' + 'a'.repeat(52) + '.qurl.site';
const own = origin + '/views/' + '1'.repeat(32);
const sibling = '/views/' + '2'.repeat(32);
function client(statuses: number[], body = 'Resource not found\n') {
  const dispose = jest.fn().mockResolvedValue(undefined);
  const get = jest.fn();
  for (const status of statuses)
    get.mockResolvedValueOnce({
      status: () => status,
      dispose,
      body: async () => Buffer.from(body),
      headers: () => ({ 'cache-control': 'no-store' }),
    });
  return { get, dispose, request: { get } as unknown as APIRequestContext };
}
test('records converged denial only as pending internal403 evidence', async () => {
  const c = client([200, 404, 200]);
  const result = await probeScopedSibling(c.request, own, sibling);
  expect(result).toMatchObject({
    ownBefore: 200,
    siblingWireStatus: 404,
    ownAfter: 200,
    siblingWireOutcome: 'CONVERGED_NOT_FOUND_PENDING_INTERNAL_403',
  });
  expect(c.get.mock.calls.map((x) => x[0])).toEqual([own, origin + sibling, own]);
  expect(c.get.mock.calls.every((x) => x[1].maxRedirects === 0 && x[1].maxRetries === 0)).toBe(
    true,
  );
  const ids = c.get.mock.calls.map((x) => x[1].headers['X-Request-ID']);
  expect(new Set(ids).size).toBe(3);
  expect(ids).toEqual([
    result.requestIDs.ownBefore,
    result.requestIDs.sibling,
    result.requestIDs.ownAfter,
  ]);
});
test.each([403, 503, 302])('HTTP%s is not the converged denial', async (status) => {
  await expect(
    probeScopedSibling(client([200, status, 200]).request, own, sibling),
  ).rejects.toThrow(/converged router denial|sibling request failed|object SERVED/);
});
test('arbitrary404 body is not the converged denial', async () => {
  await expect(
    probeScopedSibling(client([200, 404, 200], 'WAF denied').request, own, sibling),
  ).rejects.toThrow(/converged router denial|sibling request failed|object SERVED/);
});
test.each(['timeout', 'ENOTFOUND', 'secret'])(
  'unqualified transport error never passes',
  async (message) => {
    const c = client([200]);
    c.get.mockRejectedValueOnce(new Error(message));
    await expect(probeScopedSibling(c.request, own, sibling)).rejects.toThrow();
  },
);
test('exact connection-close class still records pending internal proof only', async () => {
  const c = client([]);
  c.get
    .mockResolvedValueOnce({ status: () => 200, dispose: async () => {} })
    .mockRejectedValueOnce(new Error('socket hang up'))
    .mockResolvedValueOnce({ status: () => 200, dispose: async () => {} });
  const result = await probeScopedSibling(c.request, own, sibling);
  expect(result.siblingWireStatus).toBeNull();
  expect(result.siblingWireOutcome).toBe('CONNECTION_CLOSED_PENDING_INTERNAL_403');
});
test('own control failure is not scope proof', async () => {
  await expect(probeScopedSibling(client([403]).request, own, sibling)).rejects.toThrow(
    /own control/,
  );
});
test.each([
  'https://elsewhere.example/',
  '/views/' + '1'.repeat(32),
  '/views/../sibling',
  '/views/' + '2'.repeat(32) + '?token=bad',
])('invalid target fails before request', async (path) => {
  const c = client([]);
  await expect(probeScopedSibling(c.request, own, path)).rejects.toThrow(/canonical/);
  expect(c.get).not.toHaveBeenCalled();
});
test('untrusted origin fails before request', async () => {
  const c = client([]);
  await expect(
    probeScopedSibling(c.request, own.replace('.site', '.site.evil.example'), sibling),
  ).rejects.toThrow(/canonical/);
  expect(c.get).not.toHaveBeenCalled();
});

// Receiving headers makes a later body reset a failed response, not a denial.
test.each([200, 403, 404, 503])('HTTP%s with a body reset cannot pass', async (status) => {
  const c = client([]);
  c.get.mockResolvedValueOnce({
    status: () => 200,
    dispose: async () => {},
  });
  c.get.mockResolvedValueOnce({
    status: () => status,
    body: async () => {
      throw new Error('socket hang up');
    },
    headers: () => ({ 'cache-control': 'no-store' }),
    dispose: async () => {},
  });
  await expect(probeScopedSibling(c.request, own, sibling)).rejects.toThrow(
    /converged router denial|sibling request failed|object SERVED/,
  );
  expect(c.get).toHaveBeenCalledTimes(2);
});

test('any sibling 2xx reports a breach with status only and preserves one failed receipt', async () => {
  const final = jest.fn();
  await expect(
    probeScopedSibling(client([200, 204]).request, own, sibling, Date.now() + 1000, final),
  ).rejects.toThrow("sibling object SERVED to another recipient's session (HTTP 204)");
  expect(final).toHaveBeenCalledTimes(1);
  expect(final.mock.calls[0][0]).toMatchObject({
    probeStatus: 'FAILED',
    failureCode: 'CROSS_RECIPIENT_2XX',
    ownBefore: 200,
    siblingWireStatus: 204,
    ownAfter: null,
  });
  expect(final.mock.calls[0][0].requestIDs.sibling).toMatch(/^scope-.*-sib$/);
});

test.each([undefined, 'private', 'no-cache'])(
  'missing or different cache-control fails with a final receipt',
  async (cache) => {
    const c = client([]);
    c.get.mockResolvedValueOnce({
      status: () => 200,
      dispose: async () => {},
    });
    c.get.mockResolvedValueOnce({
      status: () => 404,
      body: async () => Buffer.from('Resource not found\n'),
      headers: () => ({ 'cache-control': cache }),
      dispose: async () => {},
    });
    const final = jest.fn();
    await expect(
      probeScopedSibling(c.request, own, sibling, Date.now() + 1000, final),
    ).rejects.toThrow(/converged router denial/);
    expect(final).toHaveBeenCalledTimes(1);
    expect(final.mock.calls[0][0].failureCode).toBe('UNEXPECTED_SIBLING_RESPONSE');
  },
);

test('oversize body fails without leaking its content', async () => {
  const final = jest.fn();
  await expect(
    probeScopedSibling(
      client([200, 404], 'private-secret'.repeat(100)).request,
      own,
      sibling,
      Date.now() + 1000,
      final,
    ),
  ).rejects.toThrow(/converged router denial/);
  expect(JSON.stringify(final.mock.calls)).not.toContain('private-secret');
});

test('transport failure preserves IDs but never its raw text', async () => {
  const c = client([200]);
  c.get.mockRejectedValueOnce(new Error('private-secret'));
  const final = jest.fn();
  try {
    await probeScopedSibling(c.request, own, sibling, Date.now() + 1000, final);
    throw new Error('unexpected pass');
  } catch (err) {
    expect(String(err)).not.toContain('private-secret');
  }
  expect(final).toHaveBeenCalledTimes(1);
  expect(final.mock.calls[0][0]).toMatchObject({
    probeStatus: 'FAILED',
    failureCode: 'SIBLING_REQUEST_FAILED',
  });
  expect(final.mock.calls[0][0].requestIDs.sibling).toMatch(/^scope-.*-sib$/);
});

test('remaining deadline clamps every request and refuses a spent budget', async () => {
  const now = jest.spyOn(Date, 'now').mockReturnValue(1000);
  try {
    const c = client([200, 404, 200]);
    const final = jest.fn();
    const result = await probeScopedSibling(c.request, own, sibling, 1500, final);
    expect(c.get.mock.calls.every((x) => x[1].timeout === 500)).toBe(true);
    expect(final).toHaveBeenCalledTimes(1);
    expect(result.probeStatus).toBe('PASS');
    const expired = client([]);
    const failed = jest.fn();
    await expect(probeScopedSibling(expired.request, own, sibling, 1000, failed)).rejects.toThrow(
      /deadline exhausted/,
    );
    expect(expired.get).not.toHaveBeenCalled();
    expect(failed).toHaveBeenCalledTimes(1);
    expect(failed.mock.calls[0][0].failureCode).toBe('DEADLINE_EXHAUSTED');
  } finally {
    now.mockRestore();
  }
});

test('a spent deadline after the own request preserves failure IDs without a sibling request', async () => {
  let current = 1000;
  const now = jest.spyOn(Date, 'now').mockImplementation(() => current);
  try {
    const c = client([]);
    c.get.mockImplementationOnce(async () => {
      current = 1600;
      return { status: () => 200, dispose: async () => {} };
    });
    const final = jest.fn();
    await expect(probeScopedSibling(c.request, own, sibling, 1500, final)).rejects.toThrow(
      /deadline exhausted/,
    );
    expect(c.get).toHaveBeenCalledTimes(1);
    expect(final).toHaveBeenCalledTimes(1);
    expect(final.mock.calls[0][0]).toMatchObject({
      probeStatus: 'FAILED',
      failureCode: 'DEADLINE_EXHAUSTED',
    });
    expect(final.mock.calls[0][0].requestIDs.sibling).toMatch(/^scope-.*-sib$/);
  } finally {
    now.mockRestore();
  }
});
