// assert-crawl-terminal.mjs — assert the Grafana surface-crawl shard reached a
// TERMINAL test verdict, and fail loudly when it did not.
//
// Why this exists
// ---------------
// The crawl shard is deliberately de-gated from the required `compose-smoke`
// aggregate (see GATE_EXCLUDED_SHARDS in compose-smoke-matrix.mjs): it is slow
// BFS COVERAGE, not a correctness gate, and a coverage flake must not block
// every PR merge. That de-gate is right, but it left the lane with no reader at
// all — so when the shard stopped producing verdicts entirely, nothing said so.
//
// That is what #1861 was. The nightly `shard-crawl` job was killed by its own
// `timeout-minutes` on every run, three nights running. GitHub records a
// `timeout-minutes` kill as conclusion `cancelled`, which is NOT a test result:
// no assertions ran to completion, no inventory diff was computed, no verdict
// was produced. Meanwhile the required `compose-smoke` aggregate went green
// (correctly — it does not need the crawl matrix) while ~237 of the surface
// pin's rows, the ones only the full-depth nightly sweep can reach, were
// enforced by nothing. The pin's size implied coverage that was not happening.
//
// The contract this script enforces is TERMINALITY, not correctness: the crawl
// must end in `success` or `failure` — a real verdict, with a report and
// evidence behind it. `cancelled` (runner kill / infra) and `skipped` (the
// matrix never fanned out) are not verdicts, and this job goes red on them so
// the silence is impossible to repeat. A crawl that runs and legitimately FAILS
// is terminal: it is loud on its own child check, and re-gating it here would
// undo the de-gate on purpose.
//
// Env:
//   SETUP_RESULT       `needs.compose-smoke-setup.result` — the lane's fan-out
//                      decision. `skipped` means the change was out of scope
//                      for the compose stack and nothing should have run.
//   SCOPE_RESULT       `needs.compose-smoke-scope.result` — a skipped setup is
//                      only benign when the scope job itself SUCCEEDED.
//   HAS_INFORMATIONAL  `needs.compose-smoke-setup.outputs.has_informational` —
//                      "true" when the planner emitted a crawl matrix at all.
//                      A `false` here means the crawl lane silently stopped
//                      existing, which is the same hollow green one level up.
//   CRAWL_SHARD_RESULT `needs.compose-smoke-shard-info.result` — the rolled-up
//                      conclusion of the informational (crawl) matrix.
//   SWEEP_DEPTH        `lean` | `full` — the depth this event swept at, used to
//                      say how much of the pin the outcome covers.
//
// Exit: 0 when the crawl reached a terminal verdict (or the lane was correctly
// short-circuited); 1 otherwise.
//
// node: builtins only (via lib/gh.mjs).

import process from 'node:process';
import { error, notice } from './lib/gh.mjs';
import { crawlShardTimeoutMinutes, crawlSpecBudgetMinutes } from './lib/crawl-budget.mjs';

/** Conclusions that mean the crawl actually ran to a verdict. */
const TERMINAL_RESULTS = new Set(['success', 'failure']);

/**
 * Pure decision over the four `needs` facts. Returns `{ ok, message }`; the
 * caller turns that into a `::notice::`/`::error::` and an exit code. Kept pure
 * so the guard test can drive every branch without a workflow run.
 */
export function classifyCrawlOutcome({
  scopeResult,
  setupResult,
  hasInformational,
  crawlResult,
  depth,
}) {
  // The lane short-circuits on a change the compose stack cannot see. That is
  // benign ONLY on the same two facts the required aggregate checks: the scope
  // job succeeded, and it is the reason setup skipped. A crashed scope job also
  // skips setup, and that is a lane that failed to decide.
  if (setupResult === 'skipped') {
    if (scopeResult !== 'success') {
      return {
        ok: false,
        message:
          `compose-smoke-setup skipped because compose-smoke-scope did not succeed (${scopeResult}) — ` +
          'the lane never decided whether the crawl had work, so its silence proves nothing.',
      };
    }
    return {
      ok: true,
      message:
        'compose lane is out of scope for this change — the crawl shard was not dispatched, and no surface pin was claimed to be enforced.',
    };
  }

  if (setupResult !== 'success') {
    return {
      ok: false,
      message: `compose-smoke-setup did not succeed (${setupResult}) — the crawl matrix was never planned.`,
    };
  }

  if (hasInformational !== 'true') {
    return {
      ok: false,
      message:
        'compose-smoke-setup emitted NO informational (crawl) matrix — the surface-crawl coverage lane has silently stopped existing. ' +
        'GATE_EXCLUDED_SHARDS in .github/scripts/compose-smoke-matrix.mjs must name a shard that is in SHARDS.',
    };
  }

  if (TERMINAL_RESULTS.has(crawlResult)) {
    return {
      ok: true,
      message:
        `surface crawl reached a terminal verdict at depth=${depth}: ${crawlResult}. ` +
        'Its own `compose-smoke-shard-info (shard-crawl)` check carries that verdict; this gate only asserts the crawl was allowed to produce one.',
    };
  }

  if (crawlResult === 'cancelled') {
    const scope =
      depth === 'full'
        ? 'the FULL-depth surface pin — the rows no other lane visits — was enforced by nothing on this run'
        : 'the lean surface pin was enforced by nothing on this run';
    return {
      ok: false,
      message:
        `surface crawl shard was CANCELLED at depth=${depth}, which is not a test result: the job was killed mid-flight, ` +
        `so no inventory diff was computed and ${scope}. ` +
        `A \`timeout-minutes\` kill reports as \`cancelled\`; at this depth the job cap is ${crawlShardTimeoutMinutes(depth)} min ` +
        `against a ${crawlSpecBudgetMinutes(depth)} min spec budget (.github/scripts/lib/crawl-budget.mjs), so a cap that no longer ` +
        'clears the budget is the first thing to check. Otherwise read the shard log for the runner-side kill (disk, OOM, infra).',
    };
  }

  if (crawlResult === 'skipped') {
    return {
      ok: false,
      message:
        'surface crawl shard was SKIPPED although the compose lane fanned out — the coverage lane reported nothing while the pin implies it did.',
    };
  }

  return {
    ok: false,
    message: `surface crawl shard reported an unrecognised conclusion "${crawlResult}" — treat it as no verdict.`,
  };
}

function main() {
  const verdict = classifyCrawlOutcome({
    scopeResult: process.env.SCOPE_RESULT ?? '',
    setupResult: process.env.SETUP_RESULT ?? '',
    hasInformational: process.env.HAS_INFORMATIONAL ?? '',
    crawlResult: process.env.CRAWL_SHARD_RESULT ?? '',
    depth: process.env.SWEEP_DEPTH ?? 'lean',
  });
  if (verdict.ok) {
    notice(`crawl-terminal: ${verdict.message}`);
    process.exit(0);
  }
  error(`crawl-terminal: ${verdict.message}`, { title: 'surface crawl produced no verdict' });
  process.exit(1);
}

// Only dispatch when run as a script — importing for the unit test must not
// exit the test runner.
const invokedDirectly = process.argv[1] && import.meta.url === new URL(`file://${process.argv[1]}`).href;
if (invokedDirectly) main();
