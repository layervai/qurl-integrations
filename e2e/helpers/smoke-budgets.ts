/**
 * Per-test time budgets for the connector-stack smoke, kept here rather than as
 * literals in the test file so the one claim nothing else can check — that they
 * FIT the CI job — is enforced by a unit test instead of asserted in a comment.
 *
 * Twice during qurl-integrations#1502 a hand-written job-total drifted from the
 * literals it described, once justifying a ceiling that was too tight. The
 * per-test timeouts were already derived (`<non-revoke worst case> +
 * REVOKE_CONFIRM_WAIT_MS`); this closes the same gap for their sum.
 *
 * Each value below is the NON-REVOKE worst case — upload, mints, knocks, status
 * reads. `file-revoke.test.ts` adds `REVOKE_CONFIRM_WAIT_MS` to each, so a
 * change to the revoke budget moves every ceiling without touching this file.
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

/** Non-revoke worst case per `file-revoke.test.ts` case, in declaration order.
 * The two highest-variance cases (a cold chromium launch each, and a full 20s
 * negative-knock arm for the third) deliberately keep the roomier budgets they
 * had before the confirm wait existed — a tight ceiling there turns a slow
 * runner into a jest timeout with no assertion and no cause. */
export const FILE_REVOKE_NON_REVOKE_MS = {
  uploadViewRevoke: 75_000,
  distinctWatermark: 145_000,
  singleUseKnock: 115_000,
  doubleRevoke: 85_000,
} as const;

/** What each case is actually given, and what the unit test sums. */
export const FILE_REVOKE_TIMEOUTS_MS = Object.fromEntries(
  Object.entries(FILE_REVOKE_NON_REVOKE_MS).map(([k, v]) => [k, v + REVOKE_CONFIRM_WAIT_MS]),
) as Record<keyof typeof FILE_REVOKE_NON_REVOKE_MS, number>;
