/**
 * Per-test time budgets for the connector-stack smoke, kept here rather than as
 * literals in the test file so their SUM can be checked by a unit test instead
 * of asserted in a comment: a hand-written total drifts from the literals it
 * describes, and nothing fails when it does.
 *
 * The check is a drift detector, not a proof the job fits — these are ceilings
 * reached only pathologically, and four other live suites share the same
 * wall clock. It fails when a per-test ceiling is raised without weighing it
 * against the job, which is the mistake that actually happened.
 *
 * Each value below is a case's budget MINUS the revoke confirm wait.
 * `file-revoke.test.ts` adds `REVOKE_CONFIRM_WAIT_MS` back, so a change to the
 * revoke budget moves every ceiling without touching this file.
 */

import { REVOKE_CONFIRM_WAIT_MS } from './qurl-api';

/**
 * TODO(upstream-contract): the E2E Smoke job in qurl-integrations-infra
 * (.github/workflows/e2e-smoke.yml) is `timeout-minutes: 10`, and it pays
 * `npm ci`, SSM reads and a cold Playwright install out of the same budget —
 * so the ceilings below must leave real room, not merely fit. Nothing in this
 * repo fails loudly if infra moves that number; this is the lockstep site.
 */
export const SMOKE_JOB_BUDGET_MS = 10 * 60_000;

/** How much of the job budget the file-revoke ceilings must leave alone. It is
 * a TRIPWIRE, not an allowance: the real fixed overhead (npm ci, SSM reads, a
 * cold Playwright install) plus four other live suites exceeds it. Named here
 * rather than inline in the test so that raising it — the path of least
 * resistance when the tripwire fires — happens in the file that explains why it
 * matters, and shows up as a deliberate edit rather than a test tweak. */
export const SMOKE_JOB_RESERVE_MS = 30_000;

/** Each `file-revoke.test.ts` case's budget EXCLUDING the revoke confirm wait —
 * not a worst-case estimate. Only `uploadViewRevoke` is close to its stated
 * ~64s of work; the other three are the pre-PR ceilings minus one confirm wait,
 * deliberately keeping the variance margin they already had. A cold chromium
 * launch (two of them for `distinctWatermark`) and the 20s negative-knock arm
 * are the variance in question, and a ceiling trimmed to the estimate turns a
 * slow runner into a jest timeout with no assertion and no cause.
 *
 * The margin being folded in rather than separate is worth knowing if
 * PENDING_REVOKE_ATTEMPTS ever rises: each of those three would then grow by
 * another wait on top of padding that already absorbs one, which is part of why
 * the 3-attempt sum reaches 700s. */
export const FILE_REVOKE_BASE_MS = {
  uploadViewRevoke: 75_000,
  distinctWatermark: 145_000,
  singleUseKnock: 115_000,
  doubleRevoke: 85_000,
} as const;

/** What each case is actually given, and what the unit test sums. Written out
 * rather than mapped: `Object.entries` widens the key type, so the mapped form
 * needed a cast that would have hidden a typo'd or dropped key. */
export const FILE_REVOKE_TIMEOUTS_MS = {
  uploadViewRevoke: FILE_REVOKE_BASE_MS.uploadViewRevoke + REVOKE_CONFIRM_WAIT_MS,
  distinctWatermark: FILE_REVOKE_BASE_MS.distinctWatermark + REVOKE_CONFIRM_WAIT_MS,
  singleUseKnock: FILE_REVOKE_BASE_MS.singleUseKnock + REVOKE_CONFIRM_WAIT_MS,
  doubleRevoke: FILE_REVOKE_BASE_MS.doubleRevoke + REVOKE_CONFIRM_WAIT_MS,
};
