/**
 * Unit tests for src/view-update-handler.js — the render path
 * extracted from `monitorLinkStatus` (commands.js) for the view-
 * update push feature (feat #60). Covers the state matrix that's
 * load-bearing for sub-second view counter behavior:
 *
 *   - isStopped() early return
 *   - isViewCounterDegraded() early return
 *   - accessCount <= 0 early return
 *   - missing linkStatus entry early return
 *   - status === 'opened' idempotency guard (defeats double-increment
 *     when the polling tick races the push path)
 *   - happy path: linkStatus mutation + viewed bump + safeEdit call
 *   - hasInteraction() === false skips safeEdit (token expired)
 *   - onAllDone fires when pending hits zero
 *   - safeEdit rejection is caught + logged, NOT thrown
 *   - getButtonRow() called fresh on each invocation (catches stop()
 *     setting it to null without keeping a stale reference)
 *
 * cr round-3 #1 motivated the extraction — these tests close the
 * "what could regress later" surface that the in-place closure body
 * couldn't reach.
 */

const { createHandleViewUpdate } = require('../src/view-update-handler');

function buildDeps(overrides = {}) {
  const linkStatus = new Map();
  let viewed = 0;
  return {
    sendId: 'send-1',
    linkStatus,
    getButtonRow: () => ({ type: 1, components: [] }),
    isStopped: () => false,
    isViewCounterDegraded: () => false,
    hasInteraction: () => true,
    getViewed: () => viewed,
    setViewed: (n) => { viewed = n; },
    getExpectedCount: () => 2,
    buildStatusMsg: () => 'status msg',
    safeEdit: jest.fn(async () => undefined),
    onAllDone: jest.fn(),
    logger: { warn: jest.fn() },
    // Test introspection helpers — not part of the factory contract.
    _getViewed: () => viewed,
    _setViewed: (n) => { viewed = n; },
    ...overrides,
  };
}

describe('createHandleViewUpdate', () => {
  describe('early returns (no mutation, no safeEdit)', () => {
    test('returns when isStopped() is true', () => {
      const deps = buildDeps({ isStopped: () => true });
      deps.linkStatus.set('qrl_a', { status: 'pending', username: 'u' });
      const handler = createHandleViewUpdate(deps);
      handler({ accessCount: 1 }, 'qrl_a');
      expect(deps.safeEdit).not.toHaveBeenCalled();
      expect(deps._getViewed()).toBe(0);
      expect(deps.linkStatus.get('qrl_a').status).toBe('pending');
    });

    test('returns when isViewCounterDegraded() is true', () => {
      const deps = buildDeps({ isViewCounterDegraded: () => true });
      deps.linkStatus.set('qrl_a', { status: 'pending', username: 'u' });
      const handler = createHandleViewUpdate(deps);
      handler({ accessCount: 1 }, 'qrl_a');
      expect(deps.safeEdit).not.toHaveBeenCalled();
      expect(deps._getViewed()).toBe(0);
    });

    test('returns when update.accessCount is 0', () => {
      const deps = buildDeps();
      deps.linkStatus.set('qrl_a', { status: 'pending', username: 'u' });
      const handler = createHandleViewUpdate(deps);
      handler({ accessCount: 0 }, 'qrl_a');
      expect(deps.safeEdit).not.toHaveBeenCalled();
      expect(deps._getViewed()).toBe(0);
    });

    test('returns when update is null', () => {
      const deps = buildDeps();
      deps.linkStatus.set('qrl_a', { status: 'pending', username: 'u' });
      const handler = createHandleViewUpdate(deps);
      handler(null, 'qrl_a');
      expect(deps.safeEdit).not.toHaveBeenCalled();
      expect(deps._getViewed()).toBe(0);
    });

    test('returns when qurl_id is not in linkStatus', () => {
      const deps = buildDeps();
      // linkStatus is empty
      const handler = createHandleViewUpdate(deps);
      handler({ accessCount: 1 }, 'qrl_unknown');
      expect(deps.safeEdit).not.toHaveBeenCalled();
      expect(deps._getViewed()).toBe(0);
    });

    test('idempotency: returns when status is already opened', () => {
      const deps = buildDeps();
      deps.linkStatus.set('qrl_a', { status: 'opened', username: 'u' });
      deps._setViewed(1);
      const handler = createHandleViewUpdate(deps);
      handler({ accessCount: 5 }, 'qrl_a');
      // Critical: viewed does NOT increment (defeats double-count
      // when polling tick races push path).
      expect(deps._getViewed()).toBe(1);
      expect(deps.safeEdit).not.toHaveBeenCalled();
      expect(deps.linkStatus.get('qrl_a').status).toBe('opened');
    });
  });

  describe('happy path (pending → opened transition)', () => {
    test('mutates linkStatus + bumps viewed + calls safeEdit with status msg', () => {
      const deps = buildDeps();
      deps.linkStatus.set('qrl_a', { status: 'pending', username: 'alice' });
      const handler = createHandleViewUpdate(deps);
      handler({ accessCount: 1 }, 'qrl_a');
      expect(deps.linkStatus.get('qrl_a')).toEqual({ status: 'opened', username: 'alice' });
      expect(deps._getViewed()).toBe(1);
      expect(deps.safeEdit).toHaveBeenCalledWith({
        content: 'status msg',
        components: expect.arrayContaining([expect.any(Object)]),
      });
    });

    test('preserves the existing username + spreads other fields', () => {
      const deps = buildDeps();
      deps.linkStatus.set('qrl_a', { status: 'pending', username: 'bob', custom: 'x' });
      const handler = createHandleViewUpdate(deps);
      handler({ accessCount: 7 }, 'qrl_a');
      expect(deps.linkStatus.get('qrl_a')).toEqual({ status: 'opened', username: 'bob', custom: 'x' });
    });

    test('safeEdit components are empty when all recipients viewed', () => {
      const deps = buildDeps({ getExpectedCount: () => 1 });
      deps.linkStatus.set('qrl_a', { status: 'pending', username: 'u' });
      const handler = createHandleViewUpdate(deps);
      handler({ accessCount: 1 }, 'qrl_a');
      expect(deps.safeEdit).toHaveBeenCalledWith({
        content: 'status msg',
        components: [],
      });
    });

    test('onAllDone fires on the transition that takes pending to 0', () => {
      const deps = buildDeps({ getExpectedCount: () => 2 });
      deps.linkStatus.set('qrl_a', { status: 'pending', username: 'a' });
      deps.linkStatus.set('qrl_b', { status: 'pending', username: 'b' });
      const handler = createHandleViewUpdate(deps);

      handler({ accessCount: 1 }, 'qrl_a');
      expect(deps.onAllDone).not.toHaveBeenCalled(); // 1 of 2

      handler({ accessCount: 1 }, 'qrl_b');
      expect(deps.onAllDone).toHaveBeenCalledTimes(1); // 2 of 2
    });

    test('onAllDone does NOT fire on transitions before pending hits 0', () => {
      const deps = buildDeps({ getExpectedCount: () => 5 });
      deps.linkStatus.set('qrl_a', { status: 'pending', username: 'a' });
      const handler = createHandleViewUpdate(deps);
      handler({ accessCount: 1 }, 'qrl_a');
      expect(deps.onAllDone).not.toHaveBeenCalled();
    });
  });

  describe('hasInteraction() = false (post-token-expiry)', () => {
    test('mutates linkStatus + viewed but skips safeEdit', () => {
      const deps = buildDeps({ hasInteraction: () => false });
      deps.linkStatus.set('qrl_a', { status: 'pending', username: 'u' });
      const handler = createHandleViewUpdate(deps);
      handler({ accessCount: 1 }, 'qrl_a');
      // State mutations happen so the in-memory monitor stays consistent
      // with reality (counter is just not re-rendered).
      expect(deps.linkStatus.get('qrl_a').status).toBe('opened');
      expect(deps._getViewed()).toBe(1);
      expect(deps.safeEdit).not.toHaveBeenCalled();
    });

    test('onAllDone DOES fire even when hasInteraction()=false (interval teardown is interaction-independent)', () => {
      const deps = buildDeps({ hasInteraction: () => false, getExpectedCount: () => 1 });
      deps.linkStatus.set('qrl_a', { status: 'pending', username: 'a' });
      const handler = createHandleViewUpdate(deps);
      handler({ accessCount: 1 }, 'qrl_a');
      // Contract per cr round-5 #3: onAllDone is hoisted ABOVE the
      // hasInteraction() gate so an interval teardown is reachable
      // even if interaction is nulled outside stop() (e.g., a future
      // token-expiry refactor). State mutates → pending hits 0 →
      // onAllDone fires; safeEdit is still gated on hasInteraction.
      expect(deps.onAllDone).toHaveBeenCalledTimes(1);
      expect(deps.safeEdit).not.toHaveBeenCalled();
    });
  });

  describe('render failure', () => {
    test('safeEdit rejection is caught + logged + NOT thrown', async () => {
      const safeEdit = jest.fn(async () => {
        throw new Error('Discord 401 (token expired mid-edit)');
      });
      const logger = { warn: jest.fn() };
      const deps = buildDeps({ safeEdit, logger });
      deps.linkStatus.set('qrl_a', { status: 'pending', username: 'u' });
      const handler = createHandleViewUpdate(deps);

      expect(() => handler({ accessCount: 1 }, 'qrl_a')).not.toThrow();

      // Wait a microtask for the promise rejection .catch to fire.
      await Promise.resolve();
      await Promise.resolve();

      expect(logger.warn).toHaveBeenCalledWith(
        'view-update render failed',
        expect.objectContaining({
          sendId: 'send-1',
          qurl_id: 'qrl_a',
          error: 'Discord 401 (token expired mid-edit)',
        }),
      );
    });
  });

  describe('getButtonRow() called fresh each invocation', () => {
    test('reflects post-construction reassignment without stale capture', () => {
      let buttonRow = { type: 1, components: [{ id: 'btn' }] };
      // expectedCount=3 so two flips leave pending=1 (buttonRow stays
      // in the components array). Default expectedCount=2 would render
      // components=[] on the second call (counter complete).
      const deps = buildDeps({ getButtonRow: () => buttonRow, getExpectedCount: () => 3 });
      deps.linkStatus.set('qrl_a', { status: 'pending', username: 'u' });
      const handler = createHandleViewUpdate(deps);

      handler({ accessCount: 1 }, 'qrl_a');
      const firstCallButtonRow = deps.safeEdit.mock.calls[0][0].components[0];
      expect(firstCallButtonRow).toEqual({ type: 1, components: [{ id: 'btn' }] });

      // Simulate post-stop reassignment to a different value. (Real
      // monitor sets buttonRow=null in stop(), but isStopped() would
      // also gate this; using a different shape here is enough to
      // confirm fresh lookup.)
      buttonRow = { type: 1, components: [{ id: 'replaced' }] };
      deps.linkStatus.set('qrl_b', { status: 'pending', username: 'u' });
      handler({ accessCount: 1 }, 'qrl_b');
      const secondCallButtonRow = deps.safeEdit.mock.calls[1][0].components[0];
      expect(secondCallButtonRow).toEqual({ type: 1, components: [{ id: 'replaced' }] });
    });
  });
});
