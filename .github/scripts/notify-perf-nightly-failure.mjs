// notify-perf-nightly-failure.mjs — file (or refresh) a single tracking
// issue when the #2370 nightly `perf-nightly` schedule run does not reach a
// clean pass, and close it automatically on the next clean night.
//
// Why this exists
// ---------------
// perf-nightly.yml's `schedule` trigger has no PR, no reviewer, no default
// notification path — exactly the gap #1861 named for e2e.yml's own nightly
// run (see notify-nightly-failure.mjs's fuller "why"), and the same gap
// applies here: a regression in the #2370 statistical gate (a real
// production-cardinality query starting to OOM again, or a genuine memory
// or outcome-class regression the gate is designed to catch) reports red
// into a place nobody looks unless something files/refreshes a tracking
// issue for it. This reuses lib/nightly-health-notify.mjs's mechanism
// verbatim rather than re-deriving it — the SAME create/comment/close/noop
// lifecycle, a distinct stable title so the two lanes' incidents never
// collide.
//
// perf-nightly has one job (unlike e2e's seven), so this file's own
// "what's specific" surface is smaller: one RESULT_* env var.
//
// Env:
//   REPO             `owner/repo` (github.repository).
//   RUN_ID / RUN_URL identify the run in the issue body.
//   RESULT_PERF_NIGHTLY  `needs.perf-nightly.result`.
//
// Exit: 0 when the nightly reached a clean pass (whether or not a stale
// tracking issue needed closing); 1 when it did not, or when the gh calls
// themselves fail.
//
// node: builtins only (via lib/gh.mjs) + the `gh` CLI already authenticated
// by the workflow's GH_TOKEN.

import process from 'node:process';
import { runNotifyMain } from './lib/nightly-health-notify.mjs';

/** Mirrors notify-nightly-failure.mjs's NIGHTLY_TRACKING_LABELS — same two
 * pre-existing repo labels, no new label-provisioning step. */
export const PERF_NIGHTLY_TRACKING_LABELS = ['automated', 'area/ci'];

/** Exact, stable title — the dedup key, distinct from e2e's own so the two
 * lanes never share or clobber a tracking issue. */
export const PERF_NIGHTLY_TRACKING_TITLE = 'nightly perf-nightly run did not reach a clean pass';

function readJobResults(env) {
  return {
    'perf-nightly': env.RESULT_PERF_NIGHTLY ?? '',
  };
}

function main() {
  runNotifyMain({
    repo: process.env.REPO ?? '',
    runId: process.env.RUN_ID ?? '',
    runUrl: process.env.RUN_URL ?? '',
    jobResults: readJobResults(process.env),
    trackingLabels: PERF_NIGHTLY_TRACKING_LABELS,
    trackingTitle: PERF_NIGHTLY_TRACKING_TITLE,
    laneLabel: 'perf-nightly',
    issueRef: 'tsouza/cerberus#2370',
    contextTitle: 'perf-nightly-health-notify',
    failureNoticeTitle: 'nightly perf-nightly run failed',
  });
}

// Only dispatch when run as a script — importing for the unit test must not
// exit the test runner or shell out to `gh`.
const invokedDirectly = process.argv[1] && import.meta.url === new URL(`file://${process.argv[1]}`).href;
if (invokedDirectly) main();
