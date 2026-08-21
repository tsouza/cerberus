// notify-perf-nightly-selfcheck-failure.mjs — file (or refresh) a single
// tracking issue when #2437's periodic self-check finds the nightly perf
// gate no longer catches an injected regression, and close it automatically
// on the next clean self-check.
//
// Why this exists
// ---------------
// perf-nightly-selfcheck.yml's `schedule` trigger has no PR, no reviewer, no
// default notification path — the same gap #1861 named for e2e.yml's
// nightly run, and notify-perf-nightly-failure.mjs named for perf-nightly's
// own schedule. This is the THIRD consumer of lib/nightly-health-notify.mjs
// (#2370's PR B built the shared lib for exactly this reuse): the SAME
// create/comment/close/no-op lifecycle, a third distinct stable title so
// none of the three lanes' incidents collide.
//
// What makes this one different: a red run here does NOT mean the nightly
// perf gate rejected a real regression (that is perf-nightly's own
// tracking issue, #2370) — it means the SELF-CHECK found the gate stayed
// GREEN despite a deliberately injected one, which is a strictly worse
// signal (silent coverage loss) and gets its own tracking issue and its own
// referenced issue (#2437, not #2370) so the two failure classes are never
// confused in the same thread.
//
// Env:
//   REPO                  `owner/repo` (github.repository).
//   RUN_ID / RUN_URL       identify the run in the issue body.
//   RESULT_PERF_NIGHTLY_SELFCHECK  `needs.perf-nightly-selfcheck.result`.
//
// Exit: 0 when every mutation was caught (whether or not a stale tracking
// issue needed closing); 1 when at least one was not, or when the gh calls
// themselves fail.
//
// node: builtins only (via lib/gh.mjs) + the `gh` CLI already authenticated
// by the workflow's GH_TOKEN.

import process from 'node:process';
import { runNotifyMain } from './lib/nightly-health-notify.mjs';

/** Mirrors notify-perf-nightly-failure.mjs's own label set — same two
 * pre-existing repo labels, no new label-provisioning step. */
export const PERF_NIGHTLY_SELFCHECK_TRACKING_LABELS = ['automated', 'area/ci'];

/** Exact, stable title — the dedup key, distinct from both e2e's and
 * perf-nightly's own so all three lanes' incidents stay in separate
 * threads. */
export const PERF_NIGHTLY_SELFCHECK_TRACKING_TITLE =
  'perf-nightly self-check found an injected regression was not caught';

function readJobResults(env) {
  return {
    'perf-nightly-selfcheck': env.RESULT_PERF_NIGHTLY_SELFCHECK ?? '',
  };
}

function main() {
  runNotifyMain({
    repo: process.env.REPO ?? '',
    runId: process.env.RUN_ID ?? '',
    runUrl: process.env.RUN_URL ?? '',
    jobResults: readJobResults(process.env),
    trackingLabels: PERF_NIGHTLY_SELFCHECK_TRACKING_LABELS,
    trackingTitle: PERF_NIGHTLY_SELFCHECK_TRACKING_TITLE,
    laneLabel: 'perf-nightly-selfcheck',
    issueRef: 'tsouza/cerberus#2437',
    contextTitle: 'perf-nightly-selfcheck-health-notify',
    failureNoticeTitle: 'perf-nightly self-check did not catch an injected regression',
  });
}

// Only dispatch when run as a script — importing for the unit test must not
// exit the test runner or shell out to `gh`.
const invokedDirectly = process.argv[1] && import.meta.url === new URL(`file://${process.argv[1]}`).href;
if (invokedDirectly) main();
