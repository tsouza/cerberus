// perf-nightly-selfcheck.mjs — the periodic mutation-style self-check that
// #2370's perf sentinel gates (test/perf/nightly/ and test/perf/smoke/) still
// catch a real regression, closing tsouza/cerberus#2437 (the one mitigation
// #2370's own "Bit-rot risk" paragraph named but did not build in PR A/B/C).
//
// Why this exists
// ----------------
// The nightly gate's two-pronged assertion (a committed per-sentinel memory
// ceiling, an absolute cap-relative ceiling, and an ExpectedStatus/
// ExpectedErrorSubstring check) can only ever be OBSERVED to work when a real
// regression happens to trip it. Nothing proves it stays load-bearing
// between real incidents: a change that silently widens a ceiling, breaks
// the status check, or otherwise defangs the ratchet would currently only be
// caught by a human reading a diff — exactly the failure mode #2364 itself
// was. This script proves the gates are still armed by deliberately breaking
// real memory-bounding mechanisms, one at a time, and asserting the gate that
// owns each ACTUALLY FAILS against the broken build. A gate that stays green
// through an injected regression is worse than no gate — it reads as coverage
// that is not there.
//
// Scope: BOTH #2370 sentinel corpora
// ----------------------------------
// The nightly corpus (test/perf/nightly/sentinels.go, `just
// perf-nightly-integration`) and the smoke one (test/perf/smoke/sentinels.go,
// `just perf-smoke-integration`) are two halves of ONE gate family —
// perf-sentinel-obligation.mjs names both as satisfying the same obligation —
// and they bit-rot the same way, so each mutation below names the `just`
// recipe whose gate must catch it. The script and its workflow keep the
// perf-nightly-selfcheck NAME (its tracking issue, #2437, is keyed to it)
// while covering the family.
//
// The mutations
// -------------
// Each targets a DIFFERENT memory-bounding mechanism, verified empirically
// against a real testcontainers ClickHouse before it was added here — see
// MUTATIONS below for the measured effect of each:
//
//   1. rate-window-fanout-bound-widened — internal/chsql/rate_window_fanout_
//      bound.go's maxRateWindowFanoutRows, 1000x. Defeats the #2429 resource
//      bound `request_rate_by_method` and `error_ratio_by_namespace` are
//      calibrated to be REJECTED by (ExpectedStatus 422); with the bound
//      defeated, ClickHouse instead runs the query for real and hits a raw
//      MEMORY_LIMIT_EXCEEDED (code 241) — a genuine OOM the gate's status
//      check still catches, just via a different failure mode than the one
//      it names.
//
//   2. spill-threshold-disabled — internal/engine/spill.go's spillThreshold,
//      inflated so the external-group-by/sort spill safety net never
//      engages. `pod_status_reason_gauge` (calibrated at ~77% of the memory
//      cap, #2435's own finding) OOMs for real once spilling stops covering
//      for it, flipping its expected 200 to a 422 the gate's status check
//      catches.
//
//   3. join-spill-stamp-removed — internal/engine/spill.go's
//      applyJoinSpillSettings, stripped of its
//      max_bytes_before_external_join stamp. The smoke corpus's
//      `join_spill_vector_join` sentinel (cerberus issue #2820) reads the
//      stamp back out of system.query_log's Settings map for every repeat,
//      so the gate goes red on the SETTINGS prong rather than on memory —
//      which is the whole point of that prong existing: the stamp is
//      result-equivalent and threshold-gated, so peak memory and HTTP status
//      are identical whether it fired or the mechanism was deleted outright.
//      This mutation is what proves the sentinel is not a hollow green.
//
// The three are ORTHOGONAL memory-bounding mechanisms (a resource-bound
// throwIf, a disk-spill fallback, a chopt-gated per-query stamp) exercising
// different sentinels in different corpora — proving one still works says
// nothing about the others. Per #2437's own scope, this is deliberately NOT
// exhaustive: one representative injected regression per mechanism is the
// goal, not covering every possible defeat of every sentinel.
//
// How it runs
// -----------
// For each mutation: assert the working tree is clean, apply the mutation
// (a plain, exact string replacement — if the target text has drifted since
// this script was written, it fails LOUDLY rather than silently mutating
// nothing), run the mutation's own `just` recipe, revert the file
// UNCONDITIONALLY (even on a script crash) via `git checkout --`, and record
// whether the gate failed as expected. This never commits or pushes
// anything — the mutation lives only in the ephemeral CI checkout for the
// span of one `just` invocation.
//
// Exit: 0 when every mutation was caught (the gate failed under each one);
// 1 when the working tree was not clean at the start, a mutation's target
// text was not found, or ANY mutation was NOT caught (the gate stayed green
// despite the injected regression — the actual bit-rot signal this script
// exists to surface).
//
// node: builtins only (via lib/gh.mjs's capture()/git()).

import { readFileSync, writeFileSync } from 'node:fs';
import process from 'node:process';
import { pathToFileURL } from 'node:url';

import { capture, error, git, log, notice } from './lib/gh.mjs';

// GATE_TIMEOUT_MS bounds one gate recipe run. Each real run measured under
// two minutes locally (see the module header); this leaves wide headroom for
// CI-runner variance across the sequential invocations without risking the
// workflow's own job timeout swallowing a genuine hang as a silent non-catch.
const GATE_TIMEOUT_MS = 15 * 60 * 1000;

export const MUTATIONS = [
  {
    id: 'rate-window-fanout-bound-widened',
    description:
      'Widen internal/chsql/rate_window_fanout_bound.go\'s maxRateWindowFanoutRows 1000x, defeating the ' +
      '#2429 resource bound request_rate_by_method/error_ratio_by_namespace rely on to be rejected (422).',
    recipe: 'perf-nightly-integration',
    file: 'internal/chsql/rate_window_fanout_bound.go',
    find: 'maxRateWindowFanoutRows = 2_800_000\n',
    replace: 'maxRateWindowFanoutRows = 2_800_000_000\n',
  },
  {
    id: 'spill-threshold-disabled',
    description:
      'Inflate internal/engine/spill.go\'s spillThreshold so the external-group-by/sort spill safety net ' +
      'never engages, letting pod_status_reason_gauge (~77% of the memory cap) OOM for real instead of spilling.',
    recipe: 'perf-nightly-integration',
    file: 'internal/engine/spill.go',
    find: 'return maxMemory / spillCapDenominator\n',
    replace: 'return maxMemory * 1000\n',
  },
  {
    id: 'join-spill-stamp-removed',
    description:
      "Strip internal/engine/spill.go's applyJoinSpillSettings of its max_bytes_before_external_join stamp, " +
      'so a join-bearing plan dispatches with no join-spill bound at all. The smoke corpus\'s ' +
      'join_spill_vector_join sentinel reads that setting back out of system.query_log and must go red.',
    recipe: 'perf-smoke-integration',
    file: 'internal/engine/spill.go',
    find: '\treturn chclient.WithQuerySetting(ctx, settingMaxBytesBeforeExternalJoin, spillThreshold(maxMemory))\n',
    replace: '\treturn ctx\n',
  },
];

// assertCleanTree — refuses to mutate on top of unknown working-tree state.
function assertCleanTree(label) {
  const status = git(['status', '--porcelain']);
  if (status.status !== 0) {
    throw new Error(`perf-nightly-selfcheck: \`git status\` failed (${label}): ${status.stderr.trim()}`);
  }
  if (status.stdout.trim() !== '') {
    throw new Error(
      `perf-nightly-selfcheck: working tree is not clean (${label}) — refusing to mutate on top of it:\n` +
        status.stdout,
    );
  }
}

// applyMutation — exact-string-replace `mutation.file`. Throws if the target
// text is not found EXACTLY once, so a source drift fails loudly instead of
// silently mutating nothing (a no-op mutation would make every subsequent
// "caught" verdict meaningless). Exported for the drift-detection unit
// tests, which drive it against a temp file rather than the real source
// tree.
export function applyMutation(mutation) {
  const original = readFileSync(mutation.file, 'utf8');
  const occurrences = original.split(mutation.find).length - 1;
  if (occurrences !== 1) {
    throw new Error(
      `perf-nightly-selfcheck: mutation "${mutation.id}"'s target text was found ${occurrences} time(s) in ` +
        `${mutation.file} (expected exactly 1) — the source has drifted since this mutation was written; ` +
        'update MUTATIONS in perf-nightly-selfcheck.mjs.',
    );
  }
  writeFileSync(mutation.file, original.replace(mutation.find, mutation.replace), 'utf8');
}

// revertMutation — unconditional `git checkout -- <file>`, called from a
// finally block so a crash mid-mutation never leaves the tree dirty.
function revertMutation(mutation) {
  const res = git(['checkout', '--', mutation.file]);
  if (res.status !== 0) {
    throw new Error(
      `perf-nightly-selfcheck: failed to revert ${mutation.file} after mutation "${mutation.id}": ` +
        res.stderr.trim(),
    );
  }
}

// runGate — `just <recipe>`. Returns { caught, output } where `caught` is
// true iff the recipe exited non-zero (the gate failed, as it must under an
// injected regression).
function runGate(recipe) {
  const res = capture('just', [recipe], { timeout: GATE_TIMEOUT_MS });
  return { caught: res.status !== 0, output: `${res.stdout}\n${res.stderr}` };
}

// runOneMutation — apply, run, revert (always), report. Never leaves the
// tree mutated even if `just` itself throws an unexpected error.
export function runOneMutation(mutation) {
  applyMutation(mutation);
  try {
    const { caught, output } = runGate(mutation.recipe);
    return { id: mutation.id, description: mutation.description, recipe: mutation.recipe, caught, output };
  } finally {
    revertMutation(mutation);
  }
}

async function main() {
  assertCleanTree('before any mutation');

  const results = [];
  for (const mutation of MUTATIONS) {
    notice(
      `perf-nightly-selfcheck: applying mutation "${mutation.id}" (gate: just ${mutation.recipe}) — ` +
        mutation.description,
    );
    const result = runOneMutation(mutation);
    assertCleanTree(`after reverting mutation "${mutation.id}"`);
    results.push(result);
    if (result.caught) {
      log(`perf-nightly-selfcheck: "${mutation.id}" was caught — the gate failed as expected.`);
    } else {
      error(
        `perf-nightly-selfcheck: "${mutation.id}" was NOT caught — the nightly gate stayed green despite ` +
          `this injected regression (gate: just ${mutation.recipe}). ${mutation.description}`,
        { title: 'perf-nightly self-check: bit-rot detected' },
      );
      log(`--- last just ${mutation.recipe} output for "${mutation.id}" ---\n${result.output}`);
    }
  }

  const uncaught = results.filter((r) => !r.caught);
  if (uncaught.length > 0) {
    error(
      `perf-nightly-selfcheck: ${uncaught.length}/${results.length} mutation(s) were not caught: ` +
        `${uncaught.map((r) => `${r.id} (just ${r.recipe})`).join(', ')}. A #2370 perf sentinel gate has ` +
        'bit-rotted for at least one memory-bounding mechanism class — see tsouza/cerberus#2437.',
    );
    process.exit(1);
  }

  notice(`perf-nightly-selfcheck: all ${results.length} mutation(s) were caught — the perf sentinel gates are still armed.`);
}

const invokedDirectly = process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href;
if (invokedDirectly) {
  main().catch((e) => {
    error(`perf-nightly-selfcheck failed: ${e.message}`);
    process.exit(1);
  });
}
