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
test('source cleanup attempts all entries, rejects and retains only failures', async () => {
  revokeLinkMock.mockResolvedValueOnce(false).mockResolvedValue(true);
  const tracked = trackedQurlResources(env);
  tracked.track('res-1');
  tracked.track('res-1');
  tracked.track('res-2');

  await expect(tracked.revokeAll()).rejects.toThrow(/^Source cleanup failed for: res-1 \(not confirmed\)$/);
  expect(revokeLinkMock.mock.calls.map((call) => call[2])).toEqual(['res-1', 'res-2']);

  revokeLinkMock.mockClear();
  await expect(tracked.revokeAll()).resolves.toBeUndefined();
  expect(revokeLinkMock.mock.calls.map((call) => call[2])).toEqual(['res-1']);
});

test('source exceptions are sanitized and do not prevent later cleanup', async () => {
  revokeLinkMock.mockRejectedValueOnce(new Error('private response must not leak')).mockResolvedValue(true);
  const tracked = trackedQurlResources(env);
  tracked.track('res-1');
  tracked.track('res-2');

  await expect(tracked.revokeAll()).rejects.toThrow(/^Source cleanup failed for: res-1 \(Error\)$/);
  expect(revokeLinkMock).toHaveBeenCalledTimes(2);
});
test('the sweep sends one request per id even under a sustained 503', async () => {
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

    await expect(tracked.revokeAll()).rejects.toThrow('Source cleanup failed for:');
    expect(fetchMock).toHaveBeenCalledTimes(2);
  } finally {
    global.fetch = originalFetch;
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
  revokeLinkMock.mockResolvedValue(false);
  const tracked = trackedQurlResources(env);
  tracked.track('res-1');

  await expect(tracked.revoke('res-1')).resolves.toBe(false);
  revokeLinkMock.mockClear();
  await expect(tracked.revokeAll()).rejects.toThrow('Source cleanup failed for:');

  expect(revokeLinkMock).toHaveBeenCalledTimes(1);
});

test('child cleanup deduplicates, continues on failure, fails the hook and retains only failures', async () => {
  const childMock = qurl.revokeChild as jest.MockedFunction<typeof qurl.revokeChild>;
  childMock.mockReset();
  childMock.mockRejectedValueOnce(Object.assign(new Error('private response must not leak'), { status: 403 })).mockResolvedValue(undefined);
  const tracked = trackedQurlResources(env);
  tracked.trackChild('q_first');
  tracked.trackChild('q_first');
  tracked.trackChild('q_second');
  tracked.track('source');
  await expect(tracked.revokeAll()).rejects.toThrow(/^Child cleanup failed for: q_first \(HTTP 403\)$/);
  expect(childMock.mock.calls).toEqual([
    [env.MINT_API_URL, env.QURL_API_KEY, 'q_first'],
    [env.MINT_API_URL, env.QURL_API_KEY, 'q_second'],
  ]);
  expect(revokeLinkMock).toHaveBeenCalledWith(env.MINT_API_URL, env.QURL_API_KEY, 'source', { confirmPending: false });
  childMock.mockClear();
  await expect(tracked.revokeAll()).resolves.toBeUndefined();
  expect(childMock.mock.calls).toEqual([[env.MINT_API_URL, env.QURL_API_KEY, 'q_first']]);
});

test('mixed child and source failures are reported after every owned entry is attempted', async () => {
  const childMock = qurl.revokeChild as jest.MockedFunction<typeof qurl.revokeChild>;
  childMock.mockReset();
  childMock.mockRejectedValueOnce(Object.assign(new Error('private child body'), { status: 403 })).mockResolvedValue(undefined);
  revokeLinkMock.mockRejectedValueOnce(Object.assign(new TypeError('private source body'), { status: 401 })).mockResolvedValue(true);
  const tracked = trackedQurlResources(env);
  tracked.trackChild('q_first');
  tracked.trackChild('q_second');
  tracked.track('res-1');
  tracked.track('res-2');

  await expect(tracked.revokeAll()).rejects.toThrow(
    /^Child cleanup failed for: q_first \(HTTP 403\); Source cleanup failed for: res-1 \(HTTP 401\)$/,
  );
  expect(childMock).toHaveBeenCalledTimes(2);
  expect(revokeLinkMock).toHaveBeenCalledTimes(2);
});

// Load the actual suite hooks without registering or running their live tests.
function captureCleanupHook(suite: string, resources: object, messages: object = {}) {
  jest.resetModules();
  let hook!: () => Promise<void>;
  const bodies: Array<() => Promise<void>> = [];
  const afterSpy = jest.spyOn(global, 'afterAll').mockImplementation((fn) => {
    hook = fn as () => Promise<void>;
  });
  const describeSpy = jest.spyOn(global, 'describe').mockImplementation(
    ((_name: string, body: () => void) => body()) as typeof describe,
  );
  const testSpy = jest.spyOn(global, 'test').mockImplementation(
    ((_name: string, body: () => Promise<void>) => { bodies.push(body); }) as typeof test,
  );
  try {
    jest.doMock('../helpers/env', () => ({ loadEnv: () => env }));
    jest.doMock('../helpers/cleanup', () => ({
      trackedQurlResources: () => resources,
      trackedDiscordMessages: () => messages,
    }));
    jest.isolateModules(() => { require(suite); });
    return { hook, bodies };
  } finally {
    afterSpy.mockRestore();
    describeSpy.mockRestore();
    testSpy.mockRestore();
    jest.dontMock('../helpers/env');
    jest.dontMock('../helpers/cleanup');
  }
}

test('the smoke hook waits for message cleanup before reporting a resource failure', async () => {
  const failure = new Error('resource cleanup failed');
  let finishMessages!: () => void;
  const deleteAll = jest.fn(() => new Promise<void>((resolve) => { finishMessages = resolve; }));
  const { hook } = captureCleanupHook('../tests/smoke.test', {
    revokeAll: jest.fn().mockRejectedValue(failure),
  }, { deleteAll });
  let settled = false;
  const result = hook().then(() => undefined, (error) => { settled = true; return error; });
  await new Promise<void>((resolve) => setImmediate(resolve));
  expect(deleteAll).toHaveBeenCalledTimes(1);
  expect(settled).toBe(false);
  finishMessages();
  expect(await result).toBe(failure);
});

test('the viewer TTL hook removes its local fixture even if resource cleanup fails', async () => {
  const fs = jest.requireActual<typeof import('fs')>('fs');
  const failure = new Error('resource cleanup failed');
  const upload = jest.fn().mockResolvedValue({ resource_id: 'owned-source' });
  jest.doMock('../helpers/qurl-api', () => ({ uploadFile: upload }));
  let fixture: string | undefined;
  try {
    const { hook, bodies } = captureCleanupHook('../tests/qurl-send-viewer-ttl.smoke.test', {
      track: jest.fn(), revokeAll: jest.fn().mockRejectedValue(failure),
    });
    await bodies[0]();
    fixture = upload.mock.calls[0][1] as string;
    expect(fs.existsSync(fixture)).toBe(true);
    await expect(hook()).rejects.toBe(failure);
    expect(fs.existsSync(fixture)).toBe(false);
  } finally {
    if (fixture && fs.existsSync(fixture)) fs.unlinkSync(fixture);
    jest.dontMock('../helpers/qurl-api');
  }
});
