// coverage-aggregate.mjs — decide whether coverage.yml's two lane jobs
// (`coverage-default`, `coverage-chdb`) rolled up cleanly enough for the
// `coverage` aggregator job to download and merge their profiles,
// tsouza/cerberus#2634.
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
// neither ever sees a stale or missing context. Unlike those two lanes this
// is not a matrix (there is no corpus to partition further yet — see the
// issue for the finer per-package-group follow-up), so there are exactly
// two named `needs` results to reconcile instead of one matrix roll-up.
//
// What it asserts
// ---------------
// Both lane jobs share `coverage-plan`'s RUN_HEAVY job-level gate (an
// ordinary PR/merge-queue entry runs neither lane; push/schedule/dispatch/a
// release/* PR runs both). The aggregator has to read the two facts GitHub
// Actions exposes about each lane — `needs.coverage-default.result` and
// `needs.coverage-chdb.result` — together with whether this run was
// supposed to be heavy, and tell a legitimate skip (ordinary PR, nothing to
// merge) apart from an illegitimate one (heavy run whose lane never ran, or
// a lane that failed or was cancelled by its own timeout).
//
// `coverage-plan` also has to have actually succeeded: if it hard-fails (a
// genuine bug in coverage-run-heavy.mjs or its test, not the gracefully
// handled "source PR did not resolve" case that script itself fails safe
// on), its `run_heavy` output is unset, both lanes' job-level `if:` reads it
// as the empty string and skips — which, without this check, would read
// identically to an ordinary PR's legitimate no-op and mask a real failure
// behind a green `coverage` check.
//
// Env:
//   PLAN_RESULT     `needs.coverage-plan.result`.
//   RUN_HEAVY       `needs.coverage-plan.outputs.run_heavy` ('true' | 'false').
//   DEFAULT_RESULT  `needs.coverage-default.result`.
//   CHDB_RESULT     `needs.coverage-chdb.result`.
//
// Exit: 0 when it is safe to proceed (merge on a heavy run, or report the
// no-op on a non-heavy one); 1 otherwise.
//
// node: builtins only (via lib/gh.mjs).

import process from 'node:process';
import { error, notice } from './lib/gh.mjs';

/**
 * Pure decision over the four `needs`/env facts. Returns `{ ok, shouldMerge,
 * message }` — `shouldMerge` tells the caller whether to download and merge
 * the two lane profiles; `ok`/`message` become the `::notice::`/`::error::`
 * and exit code. Kept pure so the test can drive every branch without a
 * workflow run.
 */
export function classifyCoverage({ planResult, runHeavy, defaultResult, chdbResult }) {
  if (planResult !== 'success') {
    return {
      ok: false,
      shouldMerge: false,
      message:
        `coverage-plan reported "${planResult}" instead of "success" — its RUN_HEAVY output cannot be ` +
        "trusted, so coverage-default/coverage-chdb's skip (if any) cannot be told apart from a real " +
        'failure. Open the red `coverage-plan` check.',
    };
  }

  const heavy = runHeavy === 'true';

  if (!heavy) {
    if (defaultResult === 'skipped' && chdbResult === 'skipped') {
      return {
        ok: true,
        shouldMerge: false,
        message: 'ordinary PR — coverage-default and coverage-chdb were correctly skipped, nothing to merge.',
      };
    }
    return {
      ok: false,
      shouldMerge: false,
      message:
        `RUN_HEAVY was false but coverage-default reported "${defaultResult}" and coverage-chdb reported ` +
        `"${chdbResult}" instead of both "skipped" — the job-level gate and this aggregator have drifted ` +
        'out of sync.',
    };
  }

  if (defaultResult === 'skipped' || chdbResult === 'skipped') {
    return {
      ok: false,
      shouldMerge: false,
      message:
        'this is a heavy run (push / schedule / dispatch / release PR) but coverage-default ' +
        `(${defaultResult}) and/or coverage-chdb (${chdbResult}) never ran — the job-level RUN_HEAVY gate ` +
        "on one of the lanes has drifted from coverage-plan's own.",
    };
  }

  if (defaultResult !== 'success' || chdbResult !== 'success') {
    return {
      ok: false,
      shouldMerge: false,
      message:
        `coverage-default reported "${defaultResult}" and coverage-chdb reported "${chdbResult}" — both ` +
        'must succeed for a merged profile to mean anything. This covers a lane killed by its own timeout ' +
        '(reported as "cancelled") as well as an outright failure. Open the red lane job.',
    };
  }

  return {
    ok: true,
    shouldMerge: true,
    message: 'both coverage-default and coverage-chdb passed — merging the two profiles and running the floor gate.',
  };
}

function main() {
  const verdict = classifyCoverage({
    planResult: process.env.PLAN_RESULT ?? '',
    runHeavy: process.env.RUN_HEAVY ?? '',
    defaultResult: process.env.DEFAULT_RESULT ?? '',
    chdbResult: process.env.CHDB_RESULT ?? '',
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
