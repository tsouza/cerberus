// perf-guards-aggregate.mjs — roll the `perf-guards-shard` matrix up into the
// single `perf-guards` status check release.yml's RELEASE_REQUIRED_CHECKS
// resolves by name (#2230: `perf-guards` moved off branch protection's own
// required-contexts set onto the release gate only).
//
// Why this exists
// ---------------
// `perf-guards` used to be one job running `just perf-chdb` over the whole
// TXTAR corpus in one process. TestCardinalityRatchet's runtime is a straight
// line in corpus size, and the corpus is deliberately growing, so the lane
// walked from ~8 minutes to 867s against a 900s budget inside one day of
// parity-enrolment work (#2002). It cannot be parallelised inside the process —
// chdb-go's package-level NewSession caches ONE session per process regardless
// of DSN and is not safe for concurrent in-process use (#1987) — so the corpus
// is split across N separate runner processes instead.
//
// That split renames the thing GitHub reports: matrix children post as
// `perf-guards-shard (1)` … `perf-guards-shard (N)`, never `perf-guards`. Two
// systems resolve `perf-guards` by exact text — branch protection's
// required-contexts set, and release.yml's RELEASE_REQUIRED_CHECKS preflight —
// and a required context that is never posted sits "Expected" forever, blocking
// every merge. So this aggregator keeps the name, exactly as `compose-smoke`
// does for its own shard matrix in e2e.yml.
//
// Choosing an aggregator over N per-shard required contexts is deliberate. Per-
// shard contexts would have to be added to branch protection BEFORE they can
// ever report (a window where the gate is unenforced), removed from it before a
// shard count is ever reduced, and any stale `perf-guards (shard-9)` left in the
// set blocks every PR in the repo on a check that will never arrive. With the
// aggregator the shard count is an implementation detail of one workflow file.
//
// What it asserts
// ---------------
// A matrix exposes ONE rolled-up `.result` to its dependents: `success` only if
// EVERY leg succeeded, `failure` if any failed, `cancelled` if any was
// cancelled. The strict `!== 'success'` test below therefore catches failure AND
// cancelled AND skipped in one comparison; `contains(…, 'failure')` would let a
// cancelled matrix through green, which is the shape that silently de-gates a
// lane.
//
// The docs-only / non-heavy short-circuit is the other half.
// `perf-guards-shard` carries `if: needs.changes.outputs.docs_only != 'true'
// && needs.changes.outputs.run_heavy == 'true'`, so a docs-only PR OR an
// ordinary (non-release) PR — `perf-guards` is a release-gate lane, #2230 —
// skips the whole matrix, and this job must still report green — but ONLY on
// the right facts together: the `changes` job SUCCEEDED, and it is the one
// that said docs_only or said run_heavy=false. Reading the skip alone would be
// the hollow green this aggregate exists to prevent, because a crashed
// `changes` job also skips the matrix, and that is a lane that failed to
// decide rather than a lane with nothing to do.
//
// Env:
//   CHANGES_RESULT  `needs.changes.result` — did the path filter decide at all.
//   DOCS_ONLY       `needs.changes.outputs.docs_only` — its verdict.
//   RUN_HEAVY       `needs.changes.outputs.run_heavy` — its verdict; 'false'
//                   on an ordinary PR / merge-group entry legitimately skips
//                   the matrix (the release gate, not the merge gate, runs it).
//   SHARDS_RESULT   `needs.perf-guards-shard.result` — the rolled-up matrix.
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
export function classifyPerfGuards({ changesResult, docsOnly, shardsResult, shardCount, runHeavy }) {
  const legs = shardCount ? `${shardCount} shard(s)` : 'the shard matrix';
  // `runHeavy === 'false'` is a legitimate, deliberate reason to skip: #2230
  // moved `perf-guards` to the release gate, so an ordinary (non-release) PR
  // or a merge-group entry is SUPPOSED to short-circuit here. Absent/undefined
  // (older callers, and every existing test) behaves exactly as before this
  // was added — only an explicit 'false' counts as the non-heavy reason.
  const nonHeavySkip = runHeavy === 'false';

  if (shardsResult === 'skipped') {
    if (changesResult !== 'success') {
      return {
        ok: false,
        message:
          `perf-guards-shard skipped because the \`changes\` job did not succeed (${changesResult}) — ` +
          'the lane never decided whether it had work, so its silence is not evidence that the perf ' +
          'guards had nothing to check.',
      };
    }
    if (docsOnly !== 'true' && !nonHeavySkip) {
      return {
        ok: false,
        message:
          `perf-guards-shard skipped although \`changes\` reported docs_only='${docsOnly}' and ` +
          `run_heavy='${runHeavy}' — ${legs} should have run. A release-gate lane that skips on push / ` +
          'schedule / dispatch / a release/* PR is the gate being off.',
      };
    }
    if (docsOnly === 'true') {
      return {
        ok: true,
        message:
          'docs-only change — the chDB perf guards were not dispatched, and nothing claims they ran.',
      };
    }
    return {
      ok: true,
      message:
        'an ordinary (non-release) pull request or merge-group entry — perf-guards is a release-gate ' +
        'lane (#2230); the chDB perf guards were not dispatched here, and nothing claims they ran.',
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
        `one or more perf-guards shards did not succeed (rolled-up: ${shardsResult}). A matrix rolls up ` +
        'to `success` only when EVERY leg succeeded, so this covers a failed assertion, a leg killed by ' +
        'its own timeout (reported as `cancelled`), and a leg that never ran. Open the red ' +
        '`perf-guards-shard (n)` child check: it names the corpus slice, and its `-run` scope is the ' +
        'whole test/perf package, so the failure may be the cardinality ratchet or any other guard.',
    };
  }

  return {
    ok: true,
    message: `all ${legs} of the chDB perf guards passed; the corpus partition covers every fixture.`,
  };
}

function main() {
  const verdict = classifyPerfGuards({
    changesResult: process.env.CHANGES_RESULT ?? '',
    docsOnly: process.env.DOCS_ONLY ?? '',
    runHeavy: process.env.RUN_HEAVY ?? '',
    shardsResult: process.env.SHARDS_RESULT ?? '',
    shardCount: process.env.SHARD_COUNT ?? '',
  });
  if (verdict.ok) {
    notice(`perf-guards: ${verdict.message}`);
    process.exit(0);
  }
  error(`perf-guards: ${verdict.message}`, { title: 'perf-guards shard matrix did not pass' });
  process.exit(1);
}

// Only dispatch when run as a script — importing for the unit test must not
// exit the test runner.
const invokedDirectly = process.argv[1] && import.meta.url === new URL(`file://${process.argv[1]}`).href;
if (invokedDirectly) main();
