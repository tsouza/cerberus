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
//   4. prewhere-split-forced — internal/chsql/emit_node.go's filtered-scan
//      PREWHERE gate, stripped of its SchemaStrategies.PrewhereByServer
//      test, so the emitter writes its own PREWHERE split on a server
//      where move_to_prewhere resolved in (cerberus issue #3955). The
//      nightly label_values_matcher_stock_key{,_renamed} sentinels read the
//      matcher's attribute maps for every row of the metric's granules
//      again and go red on their committed memory ceiling. Runs on 25.9,
//      the matrix's server inside move_to_prewhere's auto range — on the
//      others the gate's PrewhereByServer test is already false.
//
//   4b. prewhere-split-partition-bypassed — the same gate's partition
//      (partitionPrewhere) bypassed, so the emitter's explicit PREWHERE
//      holds every conjunct of the filtered scan, the label matcher on the
//      attribute maps included, instead of only the narrow ones. On the
//      servers where the emitter places PREWHERE (24.8 and 26.6 in the
//      matrix) the nightly rate_by_route_matcher_stock_key sentinel decodes
//      both maps for every row of the metric's granules ahead of any
//      filtering — measured 696 MB on 24.8 and 416 MB on 26.6 against
//      58 MB and 40 MB unmutated — and goes red on its committed ceiling.
//
//   4c. move-to-prewhere-auto-range-widened — internal/chopt/registry.go's
//      move_to_prewhere AutoRange, its upper end lifted past 26.6, so auto
//      leaves the split to the server on a line where the server orders
//      the matcher's map read first. On 26.6 rate_by_route_matcher_stock_key
//      measured 296 MB against 40 MB and goes red on its committed ceiling.
//
//   5. aggregation-in-order-stamp-lost — internal/engine/
//      query_settings_rules.go's optimize_aggregation_in_order stamp,
//      disabled. ClickHouse then hash-aggregates the instant selector the
//      nightly instant_selector_in_order sentinel issues; peak memory does
//      not separate the two plans beyond run-to-run noise, so that
//      sentinel goes red on its pipeline prong, which requires
//      AggregatingInOrderTransform in EXPLAIN PIPELINE of the dispatched
//      statement.
//
//   6. plural-native-quantile-analyzer-hazard-lost — internal/engine/
//      plan_shape.go's native-histogram analyzer hazard, blind to the plural
//      chplan.HistogramQuantilesNative node again. histogram_quantiles over
//      rate() of a native histogram then reaches ClickHouse under the analyzer
//      the native-histogram analyzer fix turns off, and its statement plans
//      for about 110 s against about 2.5 s for the single-phi quantile over the
//      same input. Peak memory does not rise, so the nightly
//      native_histogram_quantiles_rate sentinel goes red on its planning prong.
//
// Each mutation names the perf-nightly ClickHouse release lines whose
// self-check leg applies it (`clickhouse`): the self-check runs once per
// server of perf-nightly.yml's matrix, the leg's server named by
// PERF_NIGHTLY_CH_IMAGE, and a PREWHERE mutation is only a regression on the
// servers where the decision it breaks is in force. A mutation whose gate
// recipe takes no image (the smoke corpus boots its own server) is applied
// on one leg.
//
// The mechanisms are ORTHOGONAL (a resource-bound throwIf, a
// disk-spill fallback, a chopt-gated per-query stamp, the PREWHERE split, the
// in-order aggregation stamp, the native-histogram analyzer stamp) exercising
// different sentinels in different corpora — proving one still works says
// nothing about the others. Per #2437's own scope, this is deliberately NOT
// exhaustive: one representative injected regression per mechanism is the
// goal, not covering every possible defeat of every sentinel.
//
// How it runs
// -----------
// For each mutation of the leg: assert the working tree is clean, apply the mutation
// (a plain, exact string replacement — if the target text has drifted since
// this script was written, it fails LOUDLY rather than silently mutating
// nothing), run the mutation's own `just` recipe, revert the file
// UNCONDITIONALLY (even on a script crash) via `git checkout --`, and record
// whether the gate failed as expected. A mutation counts as caught only when
// every sentinel it names reports a failed subtest: a recipe that fails for
// another sentinel — one already near its ceiling, say — proves nothing about
// the mechanism the mutation broke. This never commits or pushes
// anything — the mutation lives only in the ephemeral CI checkout for the
// span of one `just` invocation.
//
// Env:
//   PERF_NIGHTLY_CH_IMAGE  the ClickHouse image of this leg; its tag's
//                          major.minor selects the leg's mutations and the
//                          perf-nightly-integration gate boots it.
//
// Exit: 0 when every mutation of the leg was caught (the gate failed under
// each one); 1 when the image is unset or no mutation applies to its line,
// the working tree was not clean at the start, a mutation's target text was
// not found, or ANY mutation was NOT caught (the gate stayed green despite
// the injected regression — the actual bit-rot signal this script exists to
// surface).
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
    clickhouse: ['25.9'],
    description:
      'Widen internal/chsql/rate_window_fanout_bound.go\'s maxRateWindowFanoutRows 1000x, defeating the ' +
      '#2429 resource bound request_rate_by_method/error_ratio_by_namespace rely on to be rejected (422).',
    recipe: 'perf-nightly-integration',
    file: 'internal/chsql/rate_window_fanout_bound.go',
    sentinels: ['request_rate_by_method', 'error_ratio_by_namespace'],
    find: 'maxRateWindowFanoutRows = 2_800_000\n',
    replace: 'maxRateWindowFanoutRows = 2_800_000_000\n',
  },
  {
    id: 'spill-threshold-disabled',
    clickhouse: ['25.9'],
    description:
      'Inflate internal/engine/spill.go\'s spillThreshold so the external-group-by/sort spill safety net ' +
      'never engages, letting pod_status_reason_gauge (~77% of the memory cap) OOM for real instead of spilling.',
    recipe: 'perf-nightly-integration',
    file: 'internal/engine/spill.go',
    sentinels: ['pod_status_reason_gauge'],
    find: 'return maxMemory / spillCapDenominator\n',
    replace: 'return maxMemory * 1000\n',
  },
  {
    id: 'join-spill-stamp-removed',
    clickhouse: ['25.9'],
    description:
      "Strip internal/engine/spill.go's applyJoinSpillSettings of its max_bytes_before_external_join stamp, " +
      'so a join-bearing plan dispatches with no join-spill bound at all. The smoke corpus\'s ' +
      'join_spill_vector_join sentinel reads that setting back out of system.query_log and must go red.',
    recipe: 'perf-smoke-integration',
    file: 'internal/engine/spill.go',
    sentinels: ['join_spill_vector_join'],
    find: '\treturn chclient.WithQuerySetting(ctx, settingMaxBytesBeforeExternalJoin, spillThreshold(maxMemory))\n',
    replace: '\treturn ctx\n',
  },
  {
    id: 'prewhere-split-forced',
    clickhouse: ['25.9'],
    description:
      "Strip internal/chsql/emit_node.go's filtered-scan PREWHERE gate of its PrewhereByServer test, so the " +
      'emitter writes its own PREWHERE split where move_to_prewhere resolved in (issue #3955). The nightly ' +
      'label_values_matcher_stock_key{,_renamed} sentinels must go red on their committed memory ceiling.',
    recipe: 'perf-nightly-integration',
    file: 'internal/chsql/emit_node.go',
    sentinels: ['label_values_matcher_stock_key', 'label_values_matcher_stock_key_renamed'],
    find: 'if !strategies.PrewhereByServer && !scanReadsSubquery(scan) && projectionTouchesWide(scan.Columns, shape) {\n',
    replace: 'if !scanReadsSubquery(scan) && projectionTouchesWide(scan.Columns, shape) {\n',
  },
  {
    id: 'prewhere-split-partition-bypassed',
    clickhouse: ['24.8', '26.6'],
    description:
      "Bypass internal/chsql/emit_node.go's PREWHERE partition, so the emitter's explicit PREWHERE holds every " +
      'conjunct of a filtered scan, the label matcher on the attribute maps included. The nightly ' +
      'rate_by_route_matcher_stock_key sentinel must go red on its committed memory ceiling.',
    recipe: 'perf-nightly-integration',
    file: 'internal/chsql/emit_node.go',
    sentinels: ['rate_by_route_matcher_stock_key'],
    find: '\t\tprewhereExprs, whereExprs = partitionPrewhere(conjuncts, shape)\n',
    replace: '\t\tprewhereExprs, whereExprs = conjuncts, nil\n',
  },
  {
    id: 'move-to-prewhere-auto-range-widened',
    clickhouse: ['26.6'],
    description:
      "Lift the upper end of internal/chopt/registry.go's move_to_prewhere AutoRange past 26.6, so auto leaves " +
      'the PREWHERE split to a server that reads the matcher\'s attribute maps first. The nightly ' +
      'rate_by_route_matcher_stock_key sentinel must go red on its committed memory ceiling.',
    recipe: 'perf-nightly-integration',
    file: 'internal/chopt/registry.go',
    sentinels: ['rate_by_route_matcher_stock_key'],
    find: 'AutoRange:  VersionRange{From: Version{Major: 24, Minor: 11}, Until: Version{Major: 26, Minor: 2}},\n',
    replace: 'AutoRange:  VersionRange{From: Version{Major: 24, Minor: 11}, Until: Version{Major: 99, Minor: 0}},\n',
  },
  {
    id: 'aggregation-in-order-stamp-lost',
    clickhouse: ['25.9'],
    description:
      "Disable internal/engine/query_settings_rules.go's optimize_aggregation_in_order stamp, so ClickHouse " +
      'hash-aggregates a GROUP BY over a sorting-key prefix. The nightly instant_selector_in_order sentinel ' +
      'must go red on its pipeline prong (AggregatingInOrderTransform in EXPLAIN PIPELINE).',
    recipe: 'perf-nightly-integration',
    file: 'internal/engine/query_settings_rules.go',
    sentinels: ['instant_selector_in_order'],
    find: 'if r.OptimizeAggregationInOrder && r.eligibleForAggregationInOrder(f, strategies) {\n',
    replace: 'if false && r.OptimizeAggregationInOrder && r.eligibleForAggregationInOrder(f, strategies) {\n',
  },
  {
    id: 'plural-native-quantile-analyzer-hazard-lost',
    clickhouse: ['25.9'],
    description:
      "Drop chplan.HistogramQuantilesNative from internal/engine/plan_shape.go's native-histogram analyzer " +
      'hazard, so histogram_quantiles over a native histogram plans under the analyzer the fix turns off. ' +
      'The nightly native_histogram_quantiles_rate sentinel must go red on its planning prong.',
    recipe: 'perf-nightly-integration',
    file: 'internal/engine/plan_shape.go',
    sentinels: ['native_histogram_quantiles_rate'],
    find: 'case *chplan.HistogramQuantileNative, *chplan.HistogramQuantilesNative, *chplan.HistogramProjection:\n',
    replace: 'case *chplan.HistogramQuantileNative, *chplan.HistogramProjection:\n',
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

// IMAGE_RECIPES are the gate recipes that boot the leg's ClickHouse image,
// passed as their argument; every other gate recipe boots its own server.
const IMAGE_RECIPES = new Set(['perf-nightly-integration']);

// gateArgs — the `just` arguments that run recipe on image.
export function gateArgs(recipe, image) {
  return IMAGE_RECIPES.has(recipe) ? [recipe, image] : [recipe];
}

// releaseLine — the "major.minor" of a ClickHouse image's tag
// ("clickhouse/clickhouse-server:26.6-alpine" -> "26.6"), or null when the
// tag carries none.
export function releaseLine(image) {
  const tag = String(image).split(':').pop();
  const m = /^(\d+)\.(\d+)(?:\D|$)/.exec(tag);
  return m ? `${Number(m[1])}.${Number(m[2])}` : null;
}

// mutationsFor — the mutations whose `clickhouse` lines include line.
export function mutationsFor(line) {
  return MUTATIONS.filter((m) => m.clickhouse.includes(line));
}

// runGate — `just <recipe> [image]`. Returns { caught, output } where
// `caught` is true iff the recipe exited non-zero (the gate failed, as it
// must under an injected regression).
function runGate(recipe, image) {
  const res = capture('just', gateArgs(recipe, image), { timeout: GATE_TIMEOUT_MS });
  return { caught: res.status !== 0, output: `${res.stdout}\n${res.stderr}` };
}

// failedSentinels — the subset of names whose subtest `go test` reported as
// failed in output (a `--- FAIL: <Test>/.../<name> (` line). A gate recipe
// that fails for some other sentinel, or for a reason unrelated to the
// mutation, does not count as catching it: only the sentinels a mutation
// names prove the mechanism it breaks is guarded.
export function failedSentinels(output, names) {
  return names.filter((name) => {
    const escaped = name.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
    return new RegExp(`--- FAIL: \\S*/${escaped} \\(`).test(output);
  });
}

// failureDetail — the output lines that report on any of names, so the log
// shows what each named sentinel measured when it failed.
export function failureDetail(output, names) {
  return output
    .split('\n')
    .filter((line) => /_test\.go:\d+:/.test(line) && names.some((name) => line.includes(`${name}:`) || line.includes(`${name} `)))
    .join('\n');
}

// runOneMutation — apply, run, revert (always), report. Never leaves the
// tree mutated even if `just` itself throws an unexpected error. The mutation
// is caught when the recipe failed AND every sentinel it names failed.
export function runOneMutation(mutation, image) {
  applyMutation(mutation);
  try {
    const { caught: recipeFailed, output } = runGate(mutation.recipe, image);
    const failed = failedSentinels(output, mutation.sentinels);
    const caught = recipeFailed && failed.length === mutation.sentinels.length;
    return {
      id: mutation.id,
      description: mutation.description,
      recipe: mutation.recipe,
      caught,
      failed,
      detail: failureDetail(output, mutation.sentinels),
      output,
    };
  } finally {
    revertMutation(mutation);
  }
}

async function main() {
  const image = process.env.PERF_NIGHTLY_CH_IMAGE || '';
  const line = releaseLine(image);
  if (!line) {
    throw new Error(`PERF_NIGHTLY_CH_IMAGE (${JSON.stringify(image)}) names no ClickHouse major.minor tag`);
  }
  const mutations = mutationsFor(line);
  if (mutations.length === 0) {
    throw new Error(`no mutation applies to ClickHouse ${line} — this leg would check nothing`);
  }
  assertCleanTree('before any mutation');

  const results = [];
  for (const mutation of mutations) {
    notice(
      `perf-nightly-selfcheck: applying mutation "${mutation.id}" on ClickHouse ${line} ` +
        `(gate: just ${gateArgs(mutation.recipe, image).join(' ')}) — ${mutation.description}`,
    );
    const result = runOneMutation(mutation, image);
    assertCleanTree(`after reverting mutation "${mutation.id}"`);
    results.push(result);
    if (result.caught) {
      log(
        `perf-nightly-selfcheck: "${mutation.id}" was caught — ${result.failed.join(', ')} failed as expected:\n` +
          result.detail,
      );
    } else {
      error(
        `perf-nightly-selfcheck: "${mutation.id}" was NOT caught — of the sentinels it names ` +
          `(${mutation.sentinels.join(', ')}), only [${result.failed.join(', ')}] failed under this injected ` +
          `regression (gate: just ${mutation.recipe}). ${mutation.description}`,
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

  notice(
    `perf-nightly-selfcheck: all ${results.length} mutation(s) for ClickHouse ${line} were caught — ` +
      'the perf sentinel gates are still armed.',
  );
}

const invokedDirectly = process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href;
if (invokedDirectly) {
  main().catch((e) => {
    error(`perf-nightly-selfcheck failed: ${e.message}`);
    process.exit(1);
  });
}
