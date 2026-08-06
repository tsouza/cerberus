// crawl-budget.mjs — single source of truth for the Grafana surface crawl's
// wall-clock budgets, shared by BOTH e2e shard planners
// (compose-smoke-matrix.mjs and dashboard-matrix.mjs).
//
// Two budgets govern one crawl, they live in two different files, and the
// ORDER between them is the invariant this module exists to hold:
//
//   - the SPEC budget — `testInfo.setTimeout(...)` at the top of
//     test/e2e/playwright/crawl/crawl.spec.ts — is how long the single
//     indivisible BFS `test()` may run. Blowing it is a Playwright TEST
//     FAILURE: a `failure` conclusion, a trace, a screenshot, and the surface
//     the sweep was parked on.
//   - the JOB cap — `timeout-minutes` on the shard job — is how long the
//     runner lets the WHOLE job live (stack boot, browser install, the
//     Playwright run, the evidence dump, teardown). Blowing it kills the job
//     mid-flight, and GitHub records a `timeout-minutes` kill with conclusion
//     `cancelled` — which is not a test result at all.
//
// So the job cap must stay strictly ABOVE the spec budget plus everything the
// job does around it. If it does not, the runner always wins the race and the
// lane can only ever report `cancelled`: no verdict, no evidence, and a
// surface pin that nothing enforces.
//
// That is exactly what #1861 was. A constant 30-min job cap was sized for the
// PR/push LEAN crawl (14-min spec budget) but applied unconditionally, while
// the nightly runs SWEEP_DEPTH=full with a 75-min spec budget. Every nightly
// full-depth crawl was therefore killed at 30 minutes, on both the compose and
// the k3d lane, and the ~237 full-depth rows of the surface inventory were
// enforced by nothing at all.
//
// The caps below are DERIVED from the spec budgets rather than typed in, so
// the ordering holds by construction. crawl-budget.test.mjs reads the real
// numbers back out of crawl.spec.ts, so the two files cannot drift apart in
// silence either.
//
// Env: none — this is a pure constants/derivation module.
//
// node: builtins only.

/** Spec budget for the PR/push lean sweep (crawl.spec.ts `testInfo.setTimeout`). */
export const CRAWL_SPEC_BUDGET_LEAN_MIN = 14;

/** Spec budget for the nightly full-depth sweep (crawl.spec.ts `testInfo.setTimeout`). */
export const CRAWL_SPEC_BUDGET_FULL_MIN = 75;

/**
 * Wall-clock the shard job spends OUTSIDE the Playwright run: disk reclaim,
 * image pull + stack boot (~4 min measured on the compose lane, ~5 min on
 * k3d), browser install, the failure-evidence dump, and teardown. Added to the
 * spec budget to get the job cap, so a crawl that overruns is cut by its OWN
 * timeout — a `failure` with evidence — before the runner can cut the job.
 */
export const CRAWL_JOB_OVERHEAD_HEADROOM_MIN = 15;

/** Job `timeout-minutes` for a lean (PR/push) crawl shard. */
export const CRAWL_SHARD_TIMEOUT_LEAN_MIN =
  CRAWL_SPEC_BUDGET_LEAN_MIN + CRAWL_JOB_OVERHEAD_HEADROOM_MIN;

/** Job `timeout-minutes` for a full-depth (nightly / dispatch / k3d) crawl shard. */
export const CRAWL_SHARD_TIMEOUT_FULL_MIN =
  CRAWL_SPEC_BUDGET_FULL_MIN + CRAWL_JOB_OVERHEAD_HEADROOM_MIN;

/** The spec's own budget at a depth, in minutes. */
export function crawlSpecBudgetMinutes(depth) {
  return depth === 'full' ? CRAWL_SPEC_BUDGET_FULL_MIN : CRAWL_SPEC_BUDGET_LEAN_MIN;
}

/**
 * The shard job's `timeout-minutes` at a depth. `depth` is 'full' on the
 * nightly schedule (and always on the k3d crawl shard, which only ever runs
 * SWEEP_DEPTH=full), 'lean' on PR/push.
 */
export function crawlShardTimeoutMinutes(depth) {
  return depth === 'full' ? CRAWL_SHARD_TIMEOUT_FULL_MIN : CRAWL_SHARD_TIMEOUT_LEAN_MIN;
}

/**
 * Pull the two spec budgets back out of crawl.spec.ts source text. The guard
 * test uses this to prove the constants above still describe the file they
 * claim to describe. Returns `null` when the pinned call shape is not found —
 * which the caller must treat as a failure, not as "no drift".
 */
export function parseSpecBudgetsFromSource(source) {
  const m =
    /testInfo\.setTimeout\(\s*depth === 'full'\s*\?\s*(\d+)\s*\*\s*60_000\s*:\s*(\d+)\s*\*\s*60_000\s*\)/.exec(
      source,
    );
  if (m === null) return null;
  return { fullMin: Number(m[1]), leanMin: Number(m[2]) };
}
