// notify-nightly-failure.mjs — file (or refresh) a single tracking issue when
// the nightly `e2e` schedule run does not reach a clean pass, and close it
// automatically on the next clean night.
//
// Why this exists
// ---------------
// #1861 named two failures. The first — the nightly `shard-crawl` job being
// KILLED (`cancelled`) instead of reaching a real verdict — is fixed:
// crawl-terminal / dashboard-crawl-terminal (assert-crawl-terminal.mjs) now
// fail the run loudly on anything but `success`/`failure`, and real nightly
// runs since have reached `failure`, a genuine terminal result.
//
// That fix uncovered the second failure the issue's own acceptance text asked
// for and a first pass at this issue did not deliver: a `failure` that
// reports into a place nobody looks is exactly as silent as a `cancelled`
// that reports nowhere. The nightly `schedule` trigger has no PR, no
// reviewer, no default notification path — `e2e.yml`'s two exhaustive lanes
// are release gates, not PR gates (see the file header), so nothing reads a
// nightly run's result unless a human goes looking. The nightly ran
// `failure` for FOUR consecutive nights (2026-08-15 .. 2026-08-18) before
// anyone noticed, and it was noticed by accident — a release-staging PR
// happened to be the first `release/*` PR since the regression, and that
// lane's own required check is what surfaced it (see #1861's reopening
// comment).
//
// What this does
// ---------------
// Runs as the last job of a `schedule`-triggered e2e.yml run, downstream of
// every terminal aggregator/leaf job the lane has. The actual create/
// comment/close/noop lifecycle lives in lib/nightly-health-notify.mjs,
// shared with #2370's perf-nightly lane (notify-perf-nightly-failure.mjs) —
// this file supplies only what's specific to e2e: which jobs matter, the
// stable tracking title, and the labels.
//
// Uses the existing `automated` + `area/ci` labels (both already exist in the
// repo) rather than a new label, so there is no separate label-provisioning
// step that could itself go stale.
//
// Env:
//   REPO                        `owner/repo` (github.repository).
//   RUN_ID / RUN_URL            identify the run in the issue body.
//   RESULT_COMPOSE_SMOKE, RESULT_CRAWL_TERMINAL, RESULT_DASHBOARD,
//   RESULT_DASHBOARD_CRAWL_TERMINAL, RESULT_STARTUP_BENCH, RESULT_CHAOS,
//   RESULT_BWC_MINIO            `needs.<job>.result` for every terminal job
//                                the nightly run fans out to (e2e.yml's job
//                                graph — see the `nightly-health-notify` job).
//
// Exit: 0 when the nightly reached a clean pass (whether or not a stale
// tracking issue needed closing); 1 when it did not, or when the gh calls
// themselves fail.
//
// node: builtins only (via lib/gh.mjs) + the `gh` CLI already authenticated
// by the workflow's GH_TOKEN.

import process from 'node:process';
import {
  classifyNightlyHealth,
  findTrackingIssue,
  decideNotifyAction,
  buildFailureBody as libBuildFailureBody,
  buildRecoveryBody as libBuildRecoveryBody,
  runNotifyMain,
} from './lib/nightly-health-notify.mjs';

export { classifyNightlyHealth, findTrackingIssue, decideNotifyAction };

/** The one label pair used to find (and file) the tracking issue. Both
 * already exist as repo labels; `gh issue list --label` ANDs multiple
 * `--label` flags, so this narrows to issues carrying BOTH. */
export const NIGHTLY_TRACKING_LABELS = ['automated', 'area/ci'];

/** Exact, stable title — the dedup key. Never interpolate the run id or date
 * into it: doing so would make every bad night open a NEW issue instead of
 * refreshing the one that already tracks the incident. */
export const NIGHTLY_TRACKING_TITLE = 'nightly e2e run did not reach a clean pass';

export function buildFailureBody({ failed, runUrl, runId }) {
  return libBuildFailureBody({ laneLabel: 'e2e', failed, runUrl, runId, issueRef: 'tsouza/cerberus#1861' });
}

export function buildRecoveryBody({ runUrl, runId }) {
  return libBuildRecoveryBody({ laneLabel: 'e2e', runUrl, runId });
}

function readJobResults(env) {
  return {
    'compose-smoke': env.RESULT_COMPOSE_SMOKE ?? '',
    'crawl-terminal': env.RESULT_CRAWL_TERMINAL ?? '',
    dashboard: env.RESULT_DASHBOARD ?? '',
    'dashboard-crawl-terminal': env.RESULT_DASHBOARD_CRAWL_TERMINAL ?? '',
    'startup-bench': env.RESULT_STARTUP_BENCH ?? '',
    chaos: env.RESULT_CHAOS ?? '',
    'bwc-minio': env.RESULT_BWC_MINIO ?? '',
  };
}

function main() {
  runNotifyMain({
    repo: process.env.REPO ?? '',
    runId: process.env.RUN_ID ?? '',
    runUrl: process.env.RUN_URL ?? '',
    jobResults: readJobResults(process.env),
    trackingLabels: NIGHTLY_TRACKING_LABELS,
    trackingTitle: NIGHTLY_TRACKING_TITLE,
    laneLabel: 'e2e',
    issueRef: 'tsouza/cerberus#1861',
    contextTitle: 'nightly-health-notify',
    failureNoticeTitle: 'nightly e2e run failed',
  });
}

// Only dispatch when run as a script — importing for the unit test must not
// exit the test runner or shell out to `gh`.
const invokedDirectly = process.argv[1] && import.meta.url === new URL(`file://${process.argv[1]}`).href;
if (invokedDirectly) main();
