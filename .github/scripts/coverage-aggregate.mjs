// coverage-aggregate.mjs — decide whether coverage.yml's lane jobs
// (`coverage-default`, `coverage-chdb`, `coverage-chdb-ratchet`) rolled up
// cleanly enough for the `coverage` aggregator job to download and merge
// their profiles, tsouza/cerberus#2634 (and #2645 for the third lane).
//
// Why this exists
// ---------------
// `coverage` used to be one job running `just coverage` — the default-tag
// AND chdb-tagged `go test -coverpkg` sweeps, back to back — on a single
// runner. It repeatedly hit or came within minutes of its 60-minute timeout
// on real pushes to main as the package/fixture corpus grew, and the
// `-coverpkg` instrumentation means the job's cost scales with the WHOLE
// package graph rather than just the packages under active test, so the
// growth is structural, not a one-off spike.
//
// The fix mirrors `perf-guards-shard`/`perf-guards` in chdb.yml and
// `profile-shard`/`profile` in perf-profile.yml: split the two lanes
// `just coverage` used to run serially into two parallel jobs
// (`coverage-default`, `coverage-chdb`) and keep ONE aggregator job named
// exactly `coverage` — the name branch protection's required-contexts set
// and release.yml's RELEASE_REQUIRED_CHECKS both resolve by exact text — so
// neither ever sees a stale or missing context.
//
// #2645 added a THIRD lane, `coverage-chdb-ratchet` — a 2-leg matrix
// carrying TestCardinalityRatchet's shards that used to run serially inside
// `coverage-chdb` itself. `needs.coverage-chdb-ratchet.result`, read by a
// dependent job, is GitHub Actions' own aggregate over the whole matrix:
// 'success' only when every leg succeeded, 'failure' the moment any leg does
// (with `fail-fast: false`, a sibling leg still gets to finish and report
// its own result, but the job-level aggregate is still 'failure') — so
// treating it as one more fact alongside DEFAULT_RESULT/CHDB_RESULT already
// gives "a single failed leg fails the aggregator" for free, with no
// per-leg loop needed here.
//
// What it asserts
// ---------------
// Every lane job shares `coverage-plan`'s RUN_HEAVY job-level gate (an
// ordinary PR/merge-queue entry runs none of them; push/schedule/dispatch/a
// release/* PR runs all three). The aggregator has to read the three facts
// GitHub Actions exposes — `needs.coverage-default.result`,
// `needs.coverage-chdb.result`, `needs.coverage-chdb-ratchet.result` —
// together with whether this run was supposed to be heavy, and tell a
// legitimate skip (ordinary PR, nothing to merge) apart from an illegitimate
// one (heavy run whose lane never ran, or a lane that failed or was
// cancelled by its own timeout).
//
// `coverage-plan` also has to have actually succeeded: if it hard-fails (a
// genuine bug in coverage-run-heavy.mjs or its test, not the gracefully
// handled "source PR did not resolve" case that script itself fails safe
// on), its `run_heavy` output is unset, every lane's job-level `if:` reads
// it as the empty string and skips — which, without this check, would read
// identically to an ordinary PR's legitimate no-op and mask a real failure
// behind a green `coverage` check.
//
// Env:
//   PLAN_RESULT          `needs.coverage-plan.result`.
//   RUN_HEAVY             `needs.coverage-plan.outputs.run_heavy` ('true' | 'false').
//   DEFAULT_RESULT        `needs.coverage-default.result`.
//   CHDB_RESULT           `needs.coverage-chdb.result`.
//   CHDB_RATCHET_RESULT   `needs.coverage-chdb-ratchet.result`.
//
// Exit: 0 when it is safe to proceed (merge on a heavy run, or report the
// no-op on a non-heavy one); 1 otherwise.
//
// node: builtins only (via lib/gh.mjs).

import process from 'node:process';
import { error, notice } from './lib/gh.mjs';

/**
 * Pure decision over the five `needs`/env facts. Returns `{ ok, shouldMerge,
 * message }` — `shouldMerge` tells the caller whether to download and merge
 * the lane profiles; `ok`/`message` become the `::notice::`/`::error::` and
 * exit code. Kept pure so the test can drive every branch without a
 * workflow run.
 */
export function classifyCoverage({ planResult, runHeavy, defaultResult, chdbResult, chdbRatchetResult }) {
  if (planResult !== 'success') {
    return {
      ok: false,
      shouldMerge: false,
      message:
        `coverage-plan reported "${planResult}" instead of "success" — its RUN_HEAVY output cannot be ` +
        "trusted, so the lane jobs' skip (if any) cannot be told apart from a real failure. Open the red " +
        '`coverage-plan` check.',
    };
  }

  const heavy = runHeavy === 'true';
  const lanes = { 'coverage-default': defaultResult, 'coverage-chdb': chdbResult, 'coverage-chdb-ratchet': chdbRatchetResult };
  const laneSummary = Object.entries(lanes)
    .map(([name, result]) => `${name} (${result})`)
    .join(', ');

  if (!heavy) {
    if (Object.values(lanes).every((result) => result === 'skipped')) {
      return {
        ok: true,
        shouldMerge: false,
        message: 'ordinary PR — every coverage lane job was correctly skipped, nothing to merge.',
      };
    }
    return {
      ok: false,
      shouldMerge: false,
      message:
        `RUN_HEAVY was false but the lane jobs reported ${laneSummary} instead of all "skipped" — the ` +
        'job-level gate and this aggregator have drifted out of sync.',
    };
  }

  const neverRan = Object.entries(lanes).filter(([, result]) => result === 'skipped');
  if (neverRan.length > 0) {
    return {
      ok: false,
      shouldMerge: false,
      message:
        `this is a heavy run (push / schedule / dispatch / release PR) but ${laneSummary} — one or more lanes ` +
        "never ran; the job-level RUN_HEAVY gate on that lane has drifted from coverage-plan's own.",
    };
  }

  const notSucceeded = Object.entries(lanes).filter(([, result]) => result !== 'success');
  if (notSucceeded.length > 0) {
    return {
      ok: false,
      shouldMerge: false,
      message:
        `the lane jobs reported ${laneSummary} — every lane must succeed for a merged profile to mean ` +
        'anything. This covers a lane killed by its own timeout (reported as "cancelled") as well as an ' +
        'outright failure, and — for coverage-chdb-ratchet — a single failed matrix leg. Open the red lane job.',
    };
  }

  return {
    ok: true,
    shouldMerge: true,
    message: 'coverage-default, coverage-chdb and coverage-chdb-ratchet all passed — merging the profiles and running the floor gate.',
  };
}

function main() {
  const verdict = classifyCoverage({
    planResult: process.env.PLAN_RESULT ?? '',
    runHeavy: process.env.RUN_HEAVY ?? '',
    defaultResult: process.env.DEFAULT_RESULT ?? '',
    chdbResult: process.env.CHDB_RESULT ?? '',
    chdbRatchetResult: process.env.CHDB_RATCHET_RESULT ?? '',
  });
  if (verdict.ok) {
    notice(`coverage-aggregate: ${verdict.message}`);
    process.exit(0);
  }
  error(`coverage-aggregate: ${verdict.message}`, { title: 'coverage lane jobs did not roll up cleanly' });
  process.exit(1);
}

// Only dispatch when run as a script — importing for the unit test must not
// exit the test runner.
const invokedDirectly = process.argv[1] && import.meta.url === new URL(`file://${process.argv[1]}`).href;
if (invokedDirectly) main();
