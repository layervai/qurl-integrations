/**
 * Guards for the budget constants the live connector smoke is sized on.
 *
 * Two things live here that no per-test timeout can check: whether the
 * file-revoke ceilings still leave the CI job's reserve alone, and whether the
 * revoke ceiling still sits between the two `Retry-After` directives whose
 * separation is the only thing distinguishing a committed-but-pending 503 from
 * a deployment-state one.
 */

import {
  DARK_503_DIRECTIVE_MS,
  OBSERVED_PENDING_DIRECTIVE_MS,
  PENDING_REVOKE_CEILING_MS,
} from '../helpers/qurl-api';
import {
  FILE_REVOKE_TIMEOUTS_MS,
  SMOKE_JOB_BUDGET_MS,
  SMOKE_JOB_RESERVE_MS,
} from '../helpers/smoke-budgets';

// A DRIFT DETECTOR, not a proof the job fits: these are per-test CEILINGS
// reached only pathologically, the job timeout is wall clock, and four other
// live suites plus npm ci, SSM reads and a cold Playwright install come out of
// the same 10 minutes — a cold browser install alone can exceed the reserve.
// What it does is fail when a per-test ceiling is raised without being weighed
// against the job.
//
// It also makes the revoke docstring's "widening means raising timeout-minutes
// too, never one line" enforced instead of advisory: at PENDING_REVOKE_ATTEMPTS
// 3 the sum reaches 700s and this fails.
test('the file-revoke ceilings leave the job reserve intact', () => {
  const total = Object.values(FILE_REVOKE_TIMEOUTS_MS).reduce((a, b) => a + b, 0);

  // The reserve is a tripwire, not an allowance for the costs above, and this
  // sums FILE-REVOKE only — which is why the name says "reserve intact" rather
  // than "the job fits".
  //
  // Deliberately unguarded, listed so the next person raising the attempt count
  // knows which literals to check rather than which suites: every in-test
  // `tracked.revoke` now confirms, so concurrency.test.ts's 'revoke immediately
  // after mint' and 'parallel mint and revoke of different resources',
  // link-lifecycle.test.ts's three revoke cases, and smoke.test.ts's 'mint link
  // then revoke' all inherit jest.config.js's bare 120_000 default with no
  // REVOKE_CONFIRM_WAIT_MS term in it. They fit today (the parallel case runs
  // its five concurrently, so ~35s not ~175s); they are not derived, so nothing
  // fails if that stops being true.
  expect(total).toBeLessThanOrEqual(SMOKE_JOB_BUDGET_MS - SMOKE_JOB_RESERVE_MS);
});

// The inequality the dark-503 discrimination rests on, promoted out of prose.
// PENDING_REVOKE_CEILING_MS sitting strictly between the observed pending
// directive and the deployment-state one is the ONLY thing separating a
// committed-but-pending 503 from a dark 503 at revokeLink's call site — and an
// edit to the ceiling can cross either bound with nothing else failing.
test('the revoke ceiling separates the pending 503 from the dark 503', () => {
  expect(OBSERVED_PENDING_DIRECTIVE_MS).toBeLessThanOrEqual(PENDING_REVOKE_CEILING_MS);
  expect(PENDING_REVOKE_CEILING_MS).toBeLessThan(DARK_503_DIRECTIVE_MS);
});

// The sum alone would let two budgets be swapped for free — and swapping the
// watermark case (two cold chromium launches) with the double-revoke case
// (neither) would halve the margin on the file's highest-variance test while
// keeping the total identical. Pin the ordering the variance argument rests on.
test('the knock-bearing cases keep the roomier ceilings', () => {
  const t = FILE_REVOKE_TIMEOUTS_MS;

  // Every ceiling carries the same +REVOKE_CONFIRM_WAIT_MS, so this is really a
  // comparison of the bases — which is the point: it catches a swap that keeps
  // the sum intact. Strict, not `toBe(Math.max(...))`, since a tie would
  // satisfy that while flattening the ordering. Note it covers only the two
  // knock-bearing cases: doubleRevoke (no knock) outranks uploadViewRevoke
  // because its base came from jest's default, so there is no total order to
  // assert here.
  expect(t.distinctWatermark).toBeGreaterThan(t.singleUseKnock); // 2 cold knocks > 1
  expect(t.singleUseKnock).toBeGreaterThan(t.uploadViewRevoke); // + 20s negative arm
  // No third comparison: doubleRevoke (no knock) has a LARGER ceiling than
  // uploadViewRevoke, because its base was back-derived from jest's 120s
  // default rather than from its ~24s of work. The two above are the ordering
  // the variance argument actually rests on.

  // Known limit: these keys are free-form, so deleting a file-revoke case
  // leaves its budget in the sum above with nothing consuming it. Tying them to
  // the tests would mean importing the live suite, which loads env at module
  // scope — not worth it for a stale-by-inflation failure mode.
  expect(Object.keys(t)).toHaveLength(4);
});
