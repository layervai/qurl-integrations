/**
 * Unit coverage for the one thing in `trackedQurlResources` that no other test
 * can see: WHICH revoke budget each path asks for.
 *
 * `qurl-api.test.ts` pins what `revokeLink({ confirmPending: false })` costs.
 * What it cannot pin is that `revokeAll` actually passes it — and that wiring is
 * load-bearing. Drop the argument and the default `true` takes over, so
 * concurrency.test.ts's ~60-resource sweep goes from ~60 requests to ~60 waits
 * of up to 35s each: the 180s hook times out mid-sweep and the resources the
 * sweep exists to reclaim leak to their TTL. That is the exact failure this
 * module was written to prevent, so it gets a test rather than a comment.
 */

import { trackedQurlResources } from '../helpers/cleanup';
import * as qurl from '../helpers/qurl-api';

jest.mock('../helpers/qurl-api');

const revokeLinkMock = qurl.revokeLink as jest.MockedFunction<typeof qurl.revokeLink>;
const env = { MINT_API_URL: 'https://api.example.com/v1/resources', QURL_API_KEY: 'test-key' };

beforeEach(() => {
  revokeLinkMock.mockReset();
  revokeLinkMock.mockResolvedValue(true);
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

test('a failed revoke stays tracked for the sweep to retry', async () => {
  revokeLinkMock.mockResolvedValue(false);
  const tracked = trackedQurlResources(env);
  tracked.track('res-1');

  await expect(tracked.revoke('res-1')).resolves.toBe(false);
  revokeLinkMock.mockClear();
  await tracked.revokeAll();

  expect(revokeLinkMock).toHaveBeenCalledTimes(1);
});
