import { trackedQurlResources } from '../helpers/cleanup';
import * as qurl from '../helpers/qurl-api';

jest.mock('../helpers/qurl-api');

const revokeLinkMock = qurl.revokeLink as jest.MockedFunction<typeof qurl.revokeLink>;
const env = { MINT_API_URL: 'https://api.example.com/v1/qurls', QURL_API_KEY: 'test-key' };

beforeEach(() => {
  revokeLinkMock.mockReset();
  revokeLinkMock.mockResolvedValue(true);
});
test('the tracker hides the opt-out from TypeScript callers', async () => {
  const tracked = trackedQurlResources(env);
  // @ts-expect-error cleanup alone owns the confirmation opt-out
  await tracked.revoke('res-1', { confirmPending: false });
  expect(revokeLinkMock).toHaveBeenCalledWith(
    env.MINT_API_URL, env.QURL_API_KEY, 'res-1', { confirmPending: false },
  );
});

test('a revoke under test confirms the protection update', async () => {
  const tracked = trackedQurlResources(env);
  tracked.track('res-1');

  await expect(tracked.revoke('res-1')).resolves.toBe(true);
  expect(revokeLinkMock).toHaveBeenCalledWith(
    env.MINT_API_URL, env.QURL_API_KEY, 'res-1', { confirmPending: true },
  );
});

test('the afterAll sweep does not confirm', async () => {
  const tracked = trackedQurlResources(env);
  tracked.track('res-1');
  tracked.track('res-2');

  await tracked.revokeAll();

  expect(revokeLinkMock).toHaveBeenCalledTimes(2);
  for (const call of revokeLinkMock.mock.calls) {
    expect(call[3]).toEqual({ confirmPending: false });
  }
});

test('a confirmed revoke drops the id so the sweep does not re-revoke it', async () => {
  const tracked = trackedQurlResources(env);
  tracked.track('res-1');

  await tracked.revoke('res-1');
  revokeLinkMock.mockClear();
  await tracked.revokeAll();

  expect(revokeLinkMock).not.toHaveBeenCalled();
});
test('the sweep warns and continues after an id returns not-ok', async () => {
  const warnSpy = jest.spyOn(console, 'warn').mockImplementation(() => {});
  try {
    revokeLinkMock.mockResolvedValueOnce(false).mockResolvedValue(true);
    const tracked = trackedQurlResources(env);
    tracked.track('res-1');
    tracked.track('res-2');

    await tracked.revokeAll();

    expect(revokeLinkMock).toHaveBeenCalledTimes(2); // did not stop at the failure
    expect(warnSpy).toHaveBeenCalledWith(expect.stringContaining('res-1'));
    expect(warnSpy).toHaveBeenCalledWith(expect.stringContaining('did not confirm revocation'));
  } finally {
    warnSpy.mockRestore();
  }
});

test('the sweep warns and continues after an id throws', async () => {
  const warnSpy = jest.spyOn(console, 'warn').mockImplementation(() => {});
  try {
    revokeLinkMock.mockRejectedValueOnce(new Error('boom')).mockResolvedValue(true);
    const tracked = trackedQurlResources(env);
    tracked.track('res-1');
    tracked.track('res-2');
    await expect(tracked.revokeAll()).resolves.toBeUndefined();

    expect(revokeLinkMock).toHaveBeenCalledTimes(2);
    expect(warnSpy).toHaveBeenCalledWith(expect.stringContaining('boom'));
  } finally {
    warnSpy.mockRestore();
  }
});
test('the sweep sends one request per id even under a sustained 503', async () => {
  const warnSpy = jest.spyOn(console, 'warn').mockImplementation(() => {});
  const realRevokeLink = jest.requireActual<typeof qurl>('../helpers/qurl-api').revokeLink;
  const originalFetch = global.fetch;
  const fetchMock = jest.fn(
    () => Promise.resolve(new Response(null, { status: 503, headers: { 'Retry-After': '30' } })),
  );
  global.fetch = fetchMock as typeof fetch;
  revokeLinkMock.mockImplementation(realRevokeLink);
  try {
    const tracked = trackedQurlResources(env);
    tracked.track('res-1');
    tracked.track('res-2');

    await tracked.revokeAll();
    expect(fetchMock).toHaveBeenCalledTimes(2);
  } finally {
    global.fetch = originalFetch;
    warnSpy.mockRestore();
  }
});
test('the sweep paces itself between ids', async () => {
  jest.useFakeTimers();
  try {
    const tracked = trackedQurlResources(env);
    tracked.track('res-1');
    tracked.track('res-2');

    const pending = tracked.revokeAll();
    await jest.advanceTimersByTimeAsync(0);
    expect(revokeLinkMock).toHaveBeenCalledTimes(1); // no pause before the first

    await jest.advanceTimersByTimeAsync(249);
    expect(revokeLinkMock).toHaveBeenCalledTimes(1); // still waiting
    await jest.advanceTimersByTimeAsync(1);
    expect(revokeLinkMock).toHaveBeenCalledTimes(2);

    await pending;
  } finally {
    jest.useRealTimers();
  }
});

test('a failed revoke stays tracked for the sweep to retry', async () => {
  const warnSpy = jest.spyOn(console, 'warn').mockImplementation(() => {});
  try {
    revokeLinkMock.mockResolvedValue(false);
    const tracked = trackedQurlResources(env);
    tracked.track('res-1');

    await expect(tracked.revoke('res-1')).resolves.toBe(false);
    revokeLinkMock.mockClear();
    await tracked.revokeAll();

    expect(revokeLinkMock).toHaveBeenCalledTimes(1);
  } finally {
    warnSpy.mockRestore();
  }
});
