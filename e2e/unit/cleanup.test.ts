/**
 * Unit coverage for the one thing in `trackedQurlResources` that no other test
 * can see: WHICH revoke budget each path asks for.
 *
 * `qurl-api.test.ts` pins what `revokeLink({ confirmPending: false })` costs.
 * What it cannot pin is that `revokeAll` actually passes it — and that wiring is
 * load-bearing: drop the argument and the default `true` takes over, which
 * turns concurrency.test.ts's ~60-resource sweep into a hook timeout that leaks
 * the resources the sweep exists to reclaim (cleanup.ts carries the
 * arithmetic). That failure gets a test rather than a comment.
 */

import { trackedQurlResources } from '../helpers/cleanup';
import * as qurl from '../helpers/qurl-api';

jest.mock('../helpers/qurl-api');

const revokeLinkMock = qurl.revokeLink as jest.MockedFunction<typeof qurl.revokeLink>;
// Same shape as qurl-api.test.ts's mintUrl and as env.MINT_API_URL in the live
// suites: the management COLLECTION url. revokeLink's fallback read appends
// `/{id}` to it, so the shape is part of its contract.
const env = { MINT_API_URL: 'https://api.example.com/v1/qurls', QURL_API_KEY: 'test-key' };

beforeEach(() => {
  revokeLinkMock.mockReset();
  revokeLinkMock.mockResolvedValue(true);
});

// The interface deliberately hides `confirmPending` so opting out stays
// cleanup's call. The real guard is the `@ts-expect-error` below: `tsc
// --noEmit` fails if the directive stops being needed, i.e. if the interface
// ever widens to accept it. Note the block is COMPILE-time only — the argument
// does reach the implementation at runtime, which is the other half of why the
// options-object shape matters (a stray value degrades to the defaults rather
// than silently disabling confirmation).
test('the tracker interface does not expose the opt-out', async () => {
  const tracked = trackedQurlResources(env);
  // @ts-expect-error revoke(resourceId) takes exactly one argument.
  await tracked.revoke('res-1', { confirmPending: false });
  expect(revokeLinkMock).toHaveBeenCalled();
});

test('a revoke under test confirms the protection update', async () => {
  const tracked = trackedQurlResources(env);
  tracked.track('res-1');

  await expect(tracked.revoke('res-1')).resolves.toBe(true);

  // The caller asserts on this boolean, so it must wait out the pending-503 to
  // answer truthfully.
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

// The warn-but-never-throw channel itself. The module header calls a
// systematically-failing cleanup the dangerous case, so the sweep must keep
// going after one id fails AND leave a line per failure — best-effort means
// "does not fail the run", not "does not tell you".
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
    // The legend, not an explanation: a reader must be able to tell a committed
    // 503 from the systematic-403 regression this channel exists to surface.
    expect(warnSpy).toHaveBeenCalledWith(expect.stringContaining('401/403'));
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

    // Never throws out of the hook — a cleanup failure must not mask the real
    // test failure that stranded the resource in the first place.
    await expect(tracked.revokeAll()).resolves.toBeUndefined();

    expect(revokeLinkMock).toHaveBeenCalledTimes(2);
    expect(warnSpy).toHaveBeenCalledWith(expect.stringContaining('boom'));
  } finally {
    warnSpy.mockRestore();
  }
});

// The arithmetic the opt-out is justified on, end to end: under a SUSTAINED
// 503 the sweep must still send one request per id. cleanup.test pins that
// revokeAll asks for confirmPending: false and qurl-api.test pins what that
// costs; this is the composition, which is where ~60 ids x 35s vs a 180s hook
// would actually bite.
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

    // Two ids, two requests — no confirm wait and no transient retry.
    expect(fetchMock).toHaveBeenCalledTimes(2);
  } finally {
    global.fetch = originalFetch;
    warnSpy.mockRestore();
  }
});

// The sweep's other load-bearing property, equally invisible from the live
// suites: it paces itself. A back-to-back burst of ~60 DELETEs after the
// concurrency stress test invites the 429s that would leave stragglers leaked
// until TTL, which is what the serial-plus-pause shape exists to avoid.
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
  // Suppressed like its siblings: revokeAll warns on the not-ok it will hit.
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
