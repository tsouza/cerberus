// perf-profile-aggregate.mjs — decide whether the `profile-shard` matrix
// rolled up cleanly enough for the `profile` aggregator job to merge its
// per-leg JSON outputs (perf-profile.yml).
//
// Why this exists
// ---------------
// `profile` used to be one job walking the WHOLE test/spec/** corpus serially
// through cmd/perf-profile. Of the 11 most recent non-PR runs before a
// stopgap timeout doubling, 9 were killed by the job's own timeout with no
// error of its own — the walk simply outgrew its budget as the corpus kept
// growing (tsouza/cerberus#2375). The durable fix is the same one
// `perf-guards-shard` / `perf-guards` in chdb.yml already applies to this
// exact corpus (test/perf/profile.ProfileCorpusShard, shared by both lanes):
// split the walk across a matrix of `profile-shard` legs, each profiling one
// disjoint PERF_SHARD_INDEX/PERF_SHARD_COUNT slice, and keep ONE aggregator
// job named `profile` so branch protection's required-contexts set (well,
// `profile` is not itself in that set — see below) and, more importantly,
// release.yml's RELEASE_REQUIRED_CHECKS (which DOES resolve `profile` by
// exact text) never see a stale or missing context.
//
// Choosing an aggregator over N per-shard required contexts is deliberate,
// mirroring perf-guards-aggregate.mjs's reasoning: per-shard contexts would
// have to be added to release.yml's pin BEFORE they can ever report, removed
// before a shard count is ever reduced, and any stale
// `profile-shard (n)` left behind blocks a release forever on a check that
// will never arrive. With the aggregator the shard count is an
// implementation detail of one workflow file.
//
// What it asserts
// ---------------
// `profile-shard` is job-level gated on the SAME RUN_HEAVY condition
// perf-profile.yml has always used (push/schedule/dispatch, or a `release/*`
// PR): on an ordinary PR the whole matrix is skipped rather than spun up as
// N no-op legs. The aggregator has to read the ONE fact GitHub Actions
// exposes about a skipped/rolled-up matrix — `needs.profile-shard.result` —
// together with whether this run was supposed to be heavy, and tell the two
// legitimate skips (ordinary PR, nothing to do) apart from the two illegit-
// imate ones (heavy run whose matrix never ran; heavy run where a leg failed
// or was cancelled by ITS OWN timeout).
//
// Env:
//   RUN_HEAVY      the job's own RUN_HEAVY env value ('true' | 'false').
//   SHARDS_RESULT  `needs.profile-shard.result`.
//
// Exit: 0 when it is safe to proceed (merge on a heavy run, or report the
// no-op on a non-heavy one); 1 otherwise.
//
// node: builtins only (via lib/gh.mjs).

import process from 'node:process';
import { error, notice } from './lib/gh.mjs';

/**
 * Pure decision over the two `needs`/env facts. Returns `{ ok, shouldMerge,
 * message }` — `shouldMerge` tells the caller whether to run the merge step;
 * `ok`/`message` become the `::notice::`/`::error::` and exit code. Kept pure
 * so the test can drive every branch without a workflow run.
 */
export function classifyPerfProfile({ runHeavy, shardsResult }) {
  const heavy = runHeavy === 'true';

  if (!heavy) {
    if (shardsResult === 'skipped') {
      return {
        ok: true,
        shouldMerge: false,
        message: 'ordinary PR — profile-shard was correctly skipped, nothing to merge.',
      };
    }
    return {
      ok: false,
      shouldMerge: false,
      message:
        `RUN_HEAVY was false but profile-shard reported "${shardsResult}" instead of "skipped" — ` +
        'the job-level gate and this aggregator have drifted out of sync.',
    };
  }

  if (shardsResult === 'skipped') {
    return {
      ok: false,
      shouldMerge: false,
      message:
        'this is a heavy run (push / schedule / dispatch / release PR) but profile-shard never ran — ' +
        'the job-level RUN_HEAVY gate on profile-shard has drifted from this job\'s own.',
    };
  }

  if (shardsResult !== 'success') {
    return {
      ok: false,
      shouldMerge: false,
      message:
        `one or more profile-shard legs did not succeed (rolled-up: ${shardsResult}). A matrix rolls up to ` +
        '"success" only when EVERY leg succeeded, so this covers a leg killed by its own timeout (reported ' +
        'as "cancelled") and a leg that never ran. Open the red `profile-shard (n)` child check.',
    };
  }

  return {
    ok: true,
    shouldMerge: true,
    message: 'every profile-shard leg passed — merging the per-shard profiles into one report.',
  };
}

function main() {
  const verdict = classifyPerfProfile({
    runHeavy: process.env.RUN_HEAVY ?? '',
    shardsResult: process.env.SHARDS_RESULT ?? '',
  });
  if (verdict.ok) {
    notice(`perf-profile-aggregate: ${verdict.message}`);
    process.exit(0);
  }
  error(`perf-profile-aggregate: ${verdict.message}`, { title: 'profile-shard matrix did not roll up cleanly' });
  process.exit(1);
}

// Only dispatch when run as a script — importing for the unit test must not
// exit the test runner.
const invokedDirectly = process.argv[1] && import.meta.url === new URL(`file://${process.argv[1]}`).href;
if (invokedDirectly) main();
