// roundtrip-promql-aggregate.mjs — roll the `roundtrip-promql-shard` matrix up
// into the single `roundtrip (promql)` status check release.yml's
// RELEASE_REQUIRED_CHECKS resolves by name (tsouza/cerberus#2629).
//
// Why this exists
// ---------------
// The promql leg of `roundtrip (<ql>)` used to be one matrix entry sharing a
// single 4-core `ubuntu-latest` runner across 3 processes (FANOUT.promql, in
// chdb-roundtrip.mjs). The corpus grew past 700 TXTAR fixtures and that
// in-runner fan-out has a hard ceiling — chDB threads within a single query,
// so oversubscribing past ~3-4 processes on one runner hurts rather than
// helps — so promql is now split across N separate RUNNERS instead (the
// `roundtrip-promql-shard` matrix), each still fanning out ~3 processes
// in-runner. Total parallelism scales with shard count × in-runner fan-out
// instead of being capped at ~3-4.
//
// That split renames the thing GitHub reports: matrix children post as
// `roundtrip-promql-shard (1)` … `roundtrip-promql-shard (N)`, never
// `roundtrip (promql)`. Two systems resolve `roundtrip (promql)` by exact
// text — release.yml's RELEASE_REQUIRED_CHECKS preflight and
// `.github/ci-lanes.json`'s release-required registry entry — and a required
// context that is never posted sits "Expected" forever, blocking every
// release. So this aggregator keeps the name, exactly as `perf-guards` does
// for its own shard matrix in this same workflow.
//
// What it asserts
// ---------------
// A matrix exposes ONE rolled-up `.result` to its dependents: `success` only
// if EVERY leg succeeded, `failure` if any failed, `cancelled` if any was
// cancelled. The strict `!== 'success'` test below therefore catches failure
// AND cancelled AND skipped in one comparison; `contains(…, 'failure')` would
// let a cancelled matrix through green, which is the shape that silently
// de-gates a lane.
//
// The docs-only / non-heavy short-circuit is the other half.
// `roundtrip-promql-shard` carries `if: needs.changes.outputs.docs_only !=
// 'true' && needs.changes.outputs.run_heavy == 'true'`, so a docs-only PR OR
// an ordinary (non-release) PR skips the whole matrix, and this job must
// still report green — but ONLY on the right facts together: the `changes`
// job SUCCEEDED, and it is the one that said docs_only or said
// run_heavy=false. Reading the skip alone would be the hollow green this
// aggregate exists to prevent, because a crashed `changes` job also skips the
// matrix, and that is a lane that failed to decide rather than a lane with
// nothing to do.
//
// Env:
//   CHANGES_RESULT  `needs.changes.result` — did the path filter decide at all.
//   DOCS_ONLY       `needs.changes.outputs.docs_only` — its verdict.
//   RUN_HEAVY       `needs.changes.outputs.run_heavy` — its verdict; 'false'
//                   on an ordinary PR / merge-group entry legitimately skips
//                   the matrix (the release gate, not the merge gate, runs it).
//   SHARDS_RESULT   `needs.roundtrip-promql-shard.result` — the rolled-up matrix.
//   SHARD_COUNT     the number of legs the matrix declares, for the log line.
//
// Exit: 0 when every required shard passed (or the lane was correctly
// short-circuited); 1 otherwise.
//
// node: builtins only (via lib/gh.mjs).

import process from 'node:process';
import { error, notice } from './lib/gh.mjs';

/**
 * Pure decision over the three `needs` facts. Returns `{ ok, message }`; the
 * caller turns that into a `::notice::`/`::error::` and an exit code. Kept pure
 * so the guard test can drive every branch without a workflow run.
 */
export function classifyRoundtripPromql({ changesResult, docsOnly, shardsResult, shardCount, runHeavy }) {
  const legs = shardCount ? `${shardCount} shard(s)` : 'the shard matrix';
  // `runHeavy === 'false'` is a legitimate, deliberate reason to skip: an
  // ordinary (non-release) PR or a merge-group entry short-circuits the same
  // way every other release-gate lane in this workflow does. Absent/undefined
  // behaves exactly as before this was added — only an explicit 'false'
  // counts as the non-heavy reason.
  const nonHeavySkip = runHeavy === 'false';

  if (shardsResult === 'skipped') {
    if (changesResult !== 'success') {
      return {
        ok: false,
        message:
          `roundtrip-promql-shard skipped because the \`changes\` job did not succeed (${changesResult}) — ` +
          'the lane never decided whether it had work, so its silence is not evidence that the promql ' +
          'round-trip had nothing to check.',
      };
    }
    if (docsOnly !== 'true' && !nonHeavySkip) {
      return {
        ok: false,
        message:
          `roundtrip-promql-shard skipped although \`changes\` reported docs_only='${docsOnly}' and ` +
          `run_heavy='${runHeavy}' — ${legs} should have run. A release-gate lane that skips on push / ` +
          'schedule / dispatch / a release/* PR is the gate being off.',
      };
    }
    if (docsOnly === 'true') {
      return {
        ok: true,
        message:
          'docs-only change — the promql chDB round-trip was not dispatched, and nothing claims it ran.',
      };
    }
    return {
      ok: true,
      message:
        'an ordinary (non-release) pull request or merge-group entry — roundtrip (promql) is a ' +
        'release-gate lane; the promql chDB round-trip was not dispatched here, and nothing claims it ran.',
    };
  }

  if (changesResult !== 'success') {
    return {
      ok: false,
      message: `the \`changes\` job did not succeed (${changesResult}).`,
    };
  }

  if (shardsResult !== 'success') {
    return {
      ok: false,
      message:
        `one or more roundtrip-promql shards did not succeed (rolled-up: ${shardsResult}). A matrix rolls up ` +
        'to `success` only when EVERY leg succeeded, so this covers a failed fixture, a leg killed by its ' +
        'own timeout (reported as `cancelled`), and a leg that never ran. Open the red ' +
        '`roundtrip-promql-shard (n)` child check: it names the corpus slice.',
    };
  }

  return {
    ok: true,
    message: `all ${legs} of the promql chDB round-trip passed; the corpus partition covers every fixture.`,
  };
}

function main() {
  const verdict = classifyRoundtripPromql({
    changesResult: process.env.CHANGES_RESULT ?? '',
    docsOnly: process.env.DOCS_ONLY ?? '',
    runHeavy: process.env.RUN_HEAVY ?? '',
    shardsResult: process.env.SHARDS_RESULT ?? '',
    shardCount: process.env.SHARD_COUNT ?? '',
  });
  if (verdict.ok) {
    notice(`roundtrip (promql): ${verdict.message}`);
    process.exit(0);
  }
  error(`roundtrip (promql): ${verdict.message}`, { title: 'roundtrip-promql shard matrix did not pass' });
  process.exit(1);
}

// Only dispatch when run as a script — importing for the unit test must not
// exit the test runner.
const invokedDirectly = process.argv[1] && import.meta.url === new URL(`file://${process.argv[1]}`).href;
if (invokedDirectly) main();
