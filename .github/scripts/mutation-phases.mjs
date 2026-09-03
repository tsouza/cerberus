// mutation-phases.mjs — the single source of truth for the `mutation` lane's
// phase partition (mutation.yml's `mutate` strategy.matrix).
//
// Each entry is one gremlins run: a `scope` package (walked recursively), an
// optional scope-relative RE2 alternation carving that scope into sibling legs,
// a `workers` cap, and the `--threshold-efficacy` bar the leg must clear. The
// keys are the matrix keys mutation.yml interpolates as `matrix.scope` /
// `matrix.workers` / `matrix.exclude_files` / `matrix.efficacy` /
// `matrix.phase`, so this table renders straight into `strategy.matrix.include`.
//
// It lives in JS rather than inline YAML because mutation-matrix.mjs has to
// REASON about it — selecting the legs an ordinary PR's diff actually touches,
// and asserting the file regexes stay RE2-safe — and a workflow `include:`
// block is data the workflow cannot read back.
//
// include_files vs exclude_files (cerberus issue #2814)
// ----------------------------------------------------
// A leg names its file set one way or the other, never both, and which one it
// picks is decided by its ROLE in the scope, not by taste:
//
//   - A CURATED leg — one of several siblings sharing a scope, owning a small
//     hand-balanced set — uses `include_files`, an allowlist. Expressing the
//     same ownership as a denylist means naming every OTHER file in the
//     package, including files that do not exist yet, and that is precisely
//     what broke: a brand-new file matched none of its siblings' hand-written
//     denylists, so EVERY curated leg claimed it at once, a hard
//     `ownershipViolations` failure that surfaced only once CI ran and needed
//     N separate regexes hand-patched to clear. It happened twice in one
//     evening on two different new internal/chsql emitters. An allowlist
//     cannot make that mistake: a new file simply is not in it.
//
//   - The scope's CATCH-ALL leg — exactly one per scope — uses `exclude_files`,
//     naming its siblings' files and claiming everything else. That is what
//     makes a new file land somewhere by default instead of nowhere, and why
//     the catch-all is the one leg that must keep no positive file list to
//     fall out of sync.
//
// gremlins itself only understands `--exclude-files`, so resolvePhases
// (mutation-matrix.mjs) translates every allowlist into the equivalent denylist
// at emit/dump time by walking the scope's real files. That walk is where a new
// file gets classified for free: it matches no allowlist, lands in every
// curated leg's DERIVED denylist, and falls through to the catch-all. No hand
// edit anywhere.
//
// The cost of an allowlist is that it goes stale silently — a name left behind
// by a deleted or renamed file shrinks the leg without tripping the
// exactly-one-owner invariant. `tableViolations` therefore re-derives each
// allowlist from the tree and fails when the two disagree, so the staleness is
// a PR-time error rather than a drifting efficacy denominator.
//
// node: builtins only — no npm deps, no setup-node needed.

import { laneHarnessClosure } from './lib/lane-harness.mjs';

// Every leg clears the same bar, and this is the only place a bar is declared.
// `.gremlins.yaml` carries none: its former top-level `threshold-efficacy` was
// a key gremlins never read, so the whole-repo floor it advertised for `just
// mutate` never existed. The bar is per-leg so one weak package cannot hide
// behind a strong one in a repo-wide average.
const EFFICACY = 95;

// gremlins' default fan-out is runtime.NumCPU(). DEFAULT_WORKERS keeps it;
// SERIAL_WORKERS caps a leg at one concurrent `go test` cycle, which is what
// the heavy parser/lowering legs need to stay under the ubuntu-latest memory
// ceiling (see the phase4 rationale blocks below).
const DEFAULT_WORKERS = 0;
const SERIAL_WORKERS = 1;

// Canonical production ownership is deliberately independent of both the
// registry lane and the phase partition. A synchronized deletion from those
// two mutable declarations must still fail validation against this anchor.
export const MUTATION_PRODUCTION_GLOBS = Object.freeze([
  'internal/chplan/**',
  'internal/chsql/**',
  'internal/logql/**',
  'internal/optimizer/**',
  'internal/promql/**',
  'internal/qlcommon/**',
  'internal/spansscan/**',
  'internal/traceql/**',
]);

export const PHASES = [
  { phase: 'phase1', scope: './internal/chplan', efficacy: EFFICACY, workers: DEFAULT_WORKERS },

  // ---------------------------------------------------------------------------
  // phase2 — four sibling legs over ./internal/chsql (rebalanced from three by
  // cerberus issue #2636).
  //
  // Unsharded, phase2 was originally the mutation lane's sole critical path:
  // 936 executed mutants at ~2.4s each put it at 38 minutes while the
  // next-slowest leg (phase4-promql) finished in 13. The three-way split below
  // this comment used to balance to ~313/310/313 — roughly 12.5 minutes each.
  //
  // internal/chsql grew past that rebalance point: a real push-to-main (run
  // 32900297867, cited in issue #2636) measured all three legs individually at
  // 20-29 minutes (phase2-builder 20m28s, phase2-compare 22m11s, phase2-other
  // 29m09s) — each back near or past the original 38-minute unsplit figure,
  // just spread across more legs instead of one.
  //
  // Re-measured via `gremlins unleash --dry-run ./internal/chsql`: dry-run
  // finds every mutant and its coverage status without running a single test,
  // so its RUNNABLE (covered) count per file is the exact set a real run would
  // execute — cross-validated against internal/promql below, where the
  // dry-run total matched a real CI failure's executed-mutant count (backed
  // out of its reported efficacy) to within two mutants. internal/chsql now
  // carries 1266 executed mutants (was 936); applying the OLD three-way
  // split's file assignment to today's counts reproduces the CI-measured
  // imbalance almost exactly (381 / 404 / 481), confirming growth landed
  // unevenly across files rather than uniformly.
  //
  // Unlike the phase4-logql split, this one is NOT a memory-ceiling workaround
  // — chsql's test binary links no heavy parser graph and no leg has ever
  // OOM'd. It is a pure wall-clock split, so the legs keep gremlins' default
  // fan-out.
  //
  // Four legs, greedily balanced the same way as the original three (largest
  // file first, assigned to the smallest running total), land at
  // 316/317/318/315 — almost exactly the original ~315-mutant band, because
  // 1266 / 4 lands where 936 / 3 did. Deliberately not thematic: range_window.go
  // alone now carries 228 mutants (was 186), so any split that kept "the
  // range-window emitters" together would rebuild the critical path it exists
  // to remove.
  //
  // Rebalance by re-measuring, never by moving a file to keep a shard above the
  // bar. Shard boundaries chosen to hide a weak file are the same evasion as
  // relaxing `efficacy` — both leave the tests that failed to kill a mutant
  // exactly as weak, and only change which number reports it.
  {
    phase: 'phase2-range',
    scope: './internal/chsql',
    efficacy: EFFICACY,
    workers: DEFAULT_WORKERS,
    // range_window(228) + aggregate_range_lwr_fusion(29) +
    // range_bucket_fanout(17) + range_window_stale_resample(11) +
    // fnresolution(4) + lwr_fanout_bound(2). range_window.go alone is the
    // package's single largest file; the rest of this leg is greedy-balance
    // filler, not theme. late_mat(25) was part of this balance until cerberus
    // #2830 deleted late_mat.go outright; the allowlist kept naming it until
    // the include-tightness check in mutation-matrix.mjs caught the dead name.
    include_files: '^(aggregate_range_lwr_fusion|fnresolution|lwr_fanout_bound|range_bucket_fanout|range_window|range_window_stale_resample)\\.go$',
  },
  {
    phase: 'phase2-builder',
    scope: './internal/chsql',
    efficacy: EFFICACY,
    workers: DEFAULT_WORKERS,
    // builder(184) + nested_set_annotate(33) +
    // range_window_variants(31) + vector_join(27) +
    // range_lwr(24) + vector_set_op(10) +
    // nary_vector_set_op(6) + rate_window_fanout_bound(2).
    include_files: '^(builder|nary_vector_set_op|nested_set_annotate|range_lwr|range_window_variants|rate_window_fanout_bound|vector_join|vector_set_op)\\.go$',
  },
  {
    phase: 'phase2-compare',
    scope: './internal/chsql',
    efficacy: EFFICACY,
    workers: DEFAULT_WORKERS,
    // metrics_compare(84) + emit_node(54) +
    // exemplars(42) + histogram_quantile_native(41) +
    // range_window_fused(29) + histogram_over_time(25) +
    // histogram_projection(21) + emit(12) +
    // metrics_second_stage(10).
    include_files: '^(emit|emit_node|exemplars|histogram_over_time|histogram_projection|histogram_quantile_native|metrics_compare|metrics_second_stage|range_window_fused)\\.go$',
  },
  {
    phase: 'phase2-other',
    scope: './internal/chsql',
    efficacy: EFFICACY,
    workers: DEFAULT_WORKERS,
    // catch-all leg: prewhere + ddl + structural_join + set_op +
    // range_window_grid_native + scan_resource_bound + emit_size_bound +
    // query_exemplars + histogram_quantile + tableshape +
    // range_bucket_grid_native + range_bucket_grid_native_bound, plus every
    // file no other leg claims (the true zero-mutant/uncovered files:
    // absent_over_time, chaos_sleep, chaos_sleep_stub, doc,
    // histogram_float_vector_join, histogram_vector_join, info_join,
    // mixed_vector_join, search_trace_limit — range_bucket_grid_native and
    // range_bucket_grid_native_bound were wrongly listed here before cerberus
    // issue #2741: both carry real, covered mutants, they just had no
    // dedicated `_mutation_test.go` file defending their input-validation
    // guards until #2741 added one). A file newly added to internal/chsql
    // needs no edit here — it is picked up automatically, which is why this
    // leg keeps no positive file list to fall out of sync.
    //
    // Documented-equivalent tally (docs/test-strategy.md's "Surviving-mutant
    // policy" #1 — proven, permanent, and NOT absorbed by lowering `efficacy`
    // below `MUTATION_MIN_EFFICACY`; see that section for why). Re-measured
    // via `gremlins unleash --dry-run` scoped to this leg's own
    // `exclude_files`: 356 executed mutants today. 10 are proven equivalent
    // (≈2.8%, comfortably under the ~5-point margin `efficacy` below leaves):
    // prewhere.go:131,:148,:187,:207 (INVERT_LOOPCTRL — a "found it, stop"
    // boolean latch or a sorted-subarray early exit; scanning further can
    // never change the result) and :283 (INVERT_LOGICAL — a swap guard whose
    // two orientations always resolve the same (columnOK, literalOK) pair —
    // see prewhere_mutation_test.go's own footer), set_op.go:327,:490 and
    // structural_join.go:525,:738 (set_op_mutation_test.go /
    // structural_join_anchor_mutation_test.go's own footers), and
    // emit_size_bound.go:251 (CONDITIONALS_BOUNDARY on a running-max update
    // that reassigns the SAME value at the boundary — see
    // emit_size_bound_mutation_test.go's own footer). cerberus issue #2741's
    // own 13-survivor CI failure was NOT caused by this list growing past
    // the margin — six of the thirteen were real, previously-undefended
    // gaps in range_bucket_grid_native.go / range_bucket_grid_native_bound.go
    // (now fixed) and one more was a #2730-class CI-timing flake on an
    // existing, correct test (prewhere.go:287, reproduced and confirmed by
    // manual mutation-and-revert — see prewhere_mutation_test.go's own
    // TestIsNarrowIntegerDiscriminatorFinalReturnLogical). Re-count this
    // tally the next time a mutant here gets a new "NOT KILLABLE" note, and
    // re-partition the leg wider if it ever approaches the margin.
    exclude_files:
      '^(aggregate_range_lwr_fusion|builder|emit|emit_node|exemplars|fnresolution|histogram_over_time|histogram_projection|histogram_quantile_native|lwr_fanout_bound|metrics_compare|metrics_second_stage|nary_vector_set_op|nested_set_annotate|range_bucket_fanout|range_lwr|range_window|range_window_fused|range_window_stale_resample|range_window_variants|rate_window_fanout_bound|vector_join|vector_set_op)\\.go$',
  },
  {
    phase: 'phase3-optimizer',
    scope: './internal/optimizer',
    efficacy: EFFICACY,
    workers: DEFAULT_WORKERS,
  },

  // ---------------------------------------------------------------------------
  // phase4-promql — twelve sibling legs over ./internal/promql (split from one
  // by cerberus issue #2636).
  //
  // Unsplit, phase4-promql was the FAST leg when phase2 was first split ("the
  // next-slowest leg... finished in 13" minutes — see phase2's own history
  // above). internal/promql has since grown far past that: the same real
  // push-to-main (run 32900297867) showed it still `in_progress` past 40
  // minutes while every phase4-logql-* and phase4-traceql-* leg (already split
  // multiple ways each) finished under 15. A later push failed the phase
  // outright at 94.26% efficacy (216 lived mutants) — a real test-quality gap
  // tracked separately from this split; splitting only stops phase4-promql
  // from being the lane's sole critical path and lets the 216 lived mutants be
  // worked leg by leg instead of as one 40+-minute monolith.
  //
  // Re-measured via `gremlins unleash --dry-run ./internal/promql`: 3765
  // executed (RUNNABLE) mutants, roughly 3x internal/chsql's 1266. This total
  // is independently confirmed by the real CI failure above: 216 lived at
  // 94.26% efficacy backs out to 216 / (1 - 0.9426) ≈ 3763 total executed
  // mutants — a two-mutant difference from the dry-run count, well inside
  // normal noise between two separate `go test` runs.
  //
  // Two files are individually larger than the ~315-mutant per-leg band phase2
  // above rebalanced to, and neither can be subdivided further (gremlins
  // partitions by whole file, not by line): lower.go (485 mutants) and
  // histogram_quantile.go (359). Each gets its own dedicated leg, the same way
  // phase4-traceql-lower and phase4-logql-lower each get a dedicated leg for
  // their own oversized file. The remaining 3765 - 485 - 359 = 2921 mutants
  // across the other 84 files greedily balance across ten legs at 291-293
  // mutants each — the same ~315-mutant target band phase2 above rebalanced
  // to, kept consistent across this whole file rather than re-derived per
  // package.
  //
  // Deliberately not thematic, same as phase2: the histogram_native_mixed_or_*
  // family (added in the #2624 audit-fix batch) is scattered across almost
  // every leg below rather than kept together, because keeping ~30
  // similarly-named files in one leg would just rebuild a new single dominant
  // leg out of a family instead of out of a file.
  {
    phase: 'phase4-promql-lower',
    scope: './internal/promql',
    efficacy: EFFICACY,
    workers: DEFAULT_WORKERS,
    // lower.go only (485 mutants, the package's single largest file,
    // same reason phase4-traceql-lower and phase4-logql-lower each get a
    // dedicated leg for their own oversized file).
    include_files:
      '^(lower)\\.go$',
  },
  {
    phase: 'phase4-promql-quantile',
    scope: './internal/promql',
    efficacy: EFFICACY,
    workers: DEFAULT_WORKERS,
    // histogram_quantile.go only (359 mutants, the second largest — also
    // too big to fold into a balanced filler leg without recreating the
    // imbalance this split exists to fix).
    include_files:
      '^(histogram_quantile)\\.go$',
  },
  {
    phase: 'phase4-promql-a',
    scope: './internal/promql',
    efficacy: EFFICACY,
    workers: DEFAULT_WORKERS,
    // subquery(268) + histogram_native_mixed_or_arithmetic(12) +
    // histogram_native_drop_aggregation(9) + histogram_native_mixed_or_datefn(3).
    include_files:
      '^(histogram_native_drop_aggregation|histogram_native_mixed_or_arithmetic|histogram_native_mixed_or_datefn|subquery)\\.go$',
  },
  {
    phase: 'phase4-promql-b',
    scope: './internal/promql',
    efficacy: EFFICACY,
    workers: DEFAULT_WORKERS,
    // histogram_quantile_window(181) + metadata_catalog(35) +
    // histogram_native_last_first_over_time(29) + schema_lookup(19) +
    // histogram_native_float_vector_scaling_binop(16) + histogram_synthetic_names(8) +
    // histogram_native_mixed_or_aggregate_float_only(4).
    include_files:
      '^(histogram_native_float_vector_scaling_binop|histogram_native_last_first_over_time|histogram_native_mixed_or_aggregate_float_only|histogram_quantile_window|histogram_synthetic_names|metadata_catalog|schema_lookup)\\.go$',
  },
  {
    phase: 'phase4-promql-c',
    scope: './internal/promql',
    efficacy: EFFICACY,
    workers: DEFAULT_WORKERS,
    // histogram_native_range_fn(96) + histogram_value_fns(52) +
    // histogram_native_binop_card(39) + histogram_native_reset(33) + scalar(27) +
    // histogram_native_dropping_shape(19) + histogram_native_mixed_or_subquery_range_fn(13) +
    // histogram_native_mixed_or_vector_plain_comparison(11) + scalar_guard(3).
    include_files:
      '^(histogram_native_binop_card|histogram_native_dropping_shape|histogram_native_mixed_or_subquery_range_fn|histogram_native_mixed_or_vector_plain_comparison|histogram_native_range_fn|histogram_native_reset|histogram_value_fns|scalar|scalar_guard)\\.go$',
  },
  {
    phase: 'phase4-promql-d',
    scope: './internal/promql',
    efficacy: EFFICACY,
    workers: DEFAULT_WORKERS,
    // scalar_args(77) + resource_attributes(55) + date_fns(40) + regex_histogram_lower(37) +
    // histogram_native_over_time(31) + histogram_native_timestamp(23) +
    // histogram_native_mixed_or_comparison(18) + histogram_native_mixed_or_info(7) + unary(4).
    include_files:
      '^(date_fns|histogram_native_mixed_or_comparison|histogram_native_mixed_or_info|histogram_native_over_time|histogram_native_timestamp|regex_histogram_lower|resource_attributes|scalar_args|unary)\\.go$',
  },
  {
    phase: 'phase4-promql-e',
    scope: './internal/promql',
    efficacy: EFFICACY,
    workers: DEFAULT_WORKERS,
    // histogram_native_mixed_or_vector_arithmetic(71) + instant_fns(56) + absent(49) +
    // duplicate_labelset_guard(36) + histogram_bucket(29) +
    // histogram_native_mixed_or_vector_plain_arithmetic(22) + histogram_merge_bound(18) +
    // histogram_native_mixed_or_aggregate_presence(6) + histogram_native_unary(6).
    include_files:
      '^(absent|duplicate_labelset_guard|histogram_bucket|histogram_merge_bound|histogram_native_mixed_or_aggregate_presence|histogram_native_mixed_or_vector_arithmetic|histogram_native_mixed_or_vector_plain_arithmetic|histogram_native_unary|instant_fns)\\.go$',
  },
  {
    phase: 'phase4-promql-f',
    scope: './internal/promql',
    efficacy: EFFICACY,
    workers: DEFAULT_WORKERS,
    // histogram_quantile_native_window(70) + histogram_native_resets(59) + label_fns(43) +
    // synthetic(37) + modifiers(31) + histogram_native_binop_match(23) +
    // histogram_native_mixed_or_scale(17) + scalar_domain(8) + histogram_native_avg(3) +
    // histogram_native_mixed_or_sort(1).
    include_files:
      '^(histogram_native_avg|histogram_native_binop_match|histogram_native_mixed_or_scale|histogram_native_mixed_or_sort|histogram_native_resets|histogram_quantile_native_window|label_fns|modifiers|scalar_domain|synthetic)\\.go$',
  },
  {
    phase: 'phase4-promql-g',
    scope: './internal/promql',
    efficacy: EFFICACY,
    workers: DEFAULT_WORKERS,
    // binary(70) + info_fn(59) + lower_strategy(42) + histogram_native_mixed_or_math_fn(38) +
    // histogram_native_count_present_over_time(31) + histogram_quantile_classic_native(23) +
    // classic_bucket_merge_bound(16) + histogram_native_float_fn(10) + histogram_wire_arms(3).
    include_files:
      '^(binary|classic_bucket_merge_bound|classic_bucket_merge_summap|histogram_native_count_present_over_time|histogram_native_float_fn|histogram_native_mixed_or_math_fn|histogram_quantile_classic_native|histogram_wire_arms|info_fn|lower_strategy)\\.go$',
  },
  {
    phase: 'phase4-promql-h',
    scope: './internal/promql',
    efficacy: EFFICACY,
    workers: DEFAULT_WORKERS,
    // histogram_quantile_float(68) + histogram_native_binop(60) + range_fns(45) + sort(36) +
    // histogram_native_mixed_or_subquery_aggregate_range_fn(30) +
    // histogram_native_mixed_or_subquery_resets_changes(26, cerberus issue #2615) +
    // histogram_native_mixed_or_subquery_last_first (cerberus issue #2714 — joins
    // its resets_changes sibling here, same shape and scale) +
    // histogram_native_mixed_or_subquery_further_setop_range_fn (cerberus issue
    // #2724 — same family, joins this leg too) +
    // histogram_native_mixed_or_aggregate(27) + histogram_native_mixed_or(12) +
    // histogram_shape_guard(12) + histogram_native_mixed_or_aggregate_count_values(2).
    include_files:
      '^(histogram_native_binop|histogram_native_mixed_or|histogram_native_mixed_or_aggregate|histogram_native_mixed_or_aggregate_count_values|histogram_native_mixed_or_subquery_aggregate_range_fn|histogram_native_mixed_or_subquery_further_setop_range_fn|histogram_native_mixed_or_subquery_last_first|histogram_native_mixed_or_subquery_resets_changes|histogram_quantile_float|histogram_shape_guard|range_fns|sort)\\.go$',
  },
  {
    phase: 'phase4-promql-i',
    scope: './internal/promql',
    efficacy: EFFICACY,
    workers: DEFAULT_WORKERS,
    // histogram_native_count_values(67) + histogram_native_binop_eq(61) +
    // histogram_native_count(43) + histogram_native_mixed_or_vector_comparison(38) +
    // histogram_native_subquery_select(29) + histogram_native_bare(27) +
    // histogram_native_mixed_or_value_fn(15) + histogram_native_value_producing_call(7) +
    // histogram_native_mixed_or_label(5) + histogram_native_subquery_call_subquery(18,
    // cerberus issue #2726 — folded in here rather than kept in its own
    // dedicated leg: a lone-mutant CI-timing flip in an 18-mutant leg
    // tanks its efficacy outright (8 killed / 1 lived / 1 not-covered / 8
    // timed out = 88.9%), while the identical flip is statistical noise
    // once diluted into this leg's own ~292 mutants) +
    // histogram_native_subquery_call_subquery_outer_fn (cerberus issue
    // #2728 — the SAME composition one nesting level out, joining its own
    // sibling here for the same dilution reason. Its name gate and its
    // three-branch grid arithmetic each get a DEDICATED default-lane test
    // rather than incidental coverage from a sweep, which is what cerberus
    // issue #2730 showed a shared leg needs to stay honest on a
    // CPU-constrained runner).
    include_files:
      '^(histogram_native_bare|histogram_native_binop_eq|histogram_native_count|histogram_native_count_values|histogram_native_mixed_or_label|histogram_native_mixed_or_value_fn|histogram_native_mixed_or_vector_comparison|histogram_native_subquery_call_subquery|histogram_native_subquery_call_subquery_outer_fn|histogram_native_subquery_select|histogram_native_value_producing_call)\\.go$',
  },
  {
    phase: 'phase4-promql-other',
    scope: './internal/promql',
    efficacy: EFFICACY,
    workers: DEFAULT_WORKERS,
    // catch-all leg: histogram_quantile_range(65) + histogram_quantile_le(64) +
    // histogram_native_scalar_binop(41) + histogram_native_sum(38) +
    // histogram_native_ts_of_first_last_over_time(32) + histogram_native_label_replace(22) +
    // histogram_native_set_op(19) + histogram_native_float_vector_binop(6) +
    // histogram_native_mixed_or_aggregate_topk(4),
    // plus every file no other leg claims (the zero-mutant/uncovered files: doc,
    // histogram_native_mixed_or_scalar, histogram_native_mixed_or_sort_by_label,
    // histogram_native_mixed_or_timestamp, parser_shape). A file newly added to
    // internal/promql needs no edit here — it is picked up automatically, which is
    // why this leg keeps no positive file list to fall out of sync.
    exclude_files:
      '^(absent|binary|classic_bucket_merge_bound|classic_bucket_merge_summap|date_fns|duplicate_labelset_guard|histogram_bucket|histogram_merge_bound|histogram_native_avg|histogram_native_bare|histogram_native_binop|histogram_native_binop_card|histogram_native_binop_eq|histogram_native_binop_match|histogram_native_count|histogram_native_count_present_over_time|histogram_native_count_values|histogram_native_drop_aggregation|histogram_native_dropping_shape|histogram_native_float_fn|histogram_native_float_vector_scaling_binop|histogram_native_last_first_over_time|histogram_native_mixed_or|histogram_native_mixed_or_aggregate|histogram_native_mixed_or_aggregate_count_values|histogram_native_mixed_or_aggregate_float_only|histogram_native_mixed_or_aggregate_presence|histogram_native_mixed_or_arithmetic|histogram_native_mixed_or_comparison|histogram_native_mixed_or_datefn|histogram_native_mixed_or_info|histogram_native_mixed_or_label|histogram_native_mixed_or_math_fn|histogram_native_mixed_or_scale|histogram_native_mixed_or_sort|histogram_native_mixed_or_subquery_aggregate_range_fn|histogram_native_mixed_or_subquery_range_fn|histogram_native_mixed_or_subquery_further_setop_range_fn|histogram_native_mixed_or_subquery_last_first|histogram_native_mixed_or_subquery_resets_changes|histogram_native_mixed_or_value_fn|histogram_native_mixed_or_vector_arithmetic|histogram_native_mixed_or_vector_comparison|histogram_native_mixed_or_vector_plain_arithmetic|histogram_native_mixed_or_vector_plain_comparison|histogram_native_over_time|histogram_native_range_fn|histogram_native_reset|histogram_native_resets|histogram_native_subquery_call_subquery|histogram_native_subquery_call_subquery_outer_fn|histogram_native_subquery_select|histogram_native_timestamp|histogram_native_unary|histogram_native_value_producing_call|histogram_quantile|histogram_quantile_classic_native|histogram_quantile_float|histogram_quantile_native_window|histogram_quantile_window|histogram_shape_guard|histogram_synthetic_names|histogram_value_fns|histogram_wire_arms|info_fn|instant_fns|label_fns|lower|lower_strategy|metadata_catalog|modifiers|range_fns|regex_histogram_lower|resource_attributes|scalar|scalar_args|scalar_domain|scalar_guard|schema_lookup|sort|subquery|synthetic|unary)\\.go$',
  },

  // ---------------------------------------------------------------------------
  // phase4-logql — four sibling legs over ./internal/logql, plus two over the
  // lsyntax subpackage.
  //
  // phase4-logql historically OOM-killed the ubuntu-latest runner (~7 GB RAM) at
  // the 3-minute mark. Rounds 1-3 tried gremlins' default runtime.NumCPU()
  // workers and the runner died ~3 min in. Round 4 capped workers at 2, round 5
  // dropped to workers=1, and round 6's job log (77180086677,
  // 2026-05-21T13:13:26Z) confirmed `--workers "1"` engaged, every emitted
  // mutant was KILLED (no LIVED), gremlins.json never written, and the runner
  // received a shutdown signal at the same ~3m37s mark. The bottleneck is
  // therefore NOT parallel `go test` fan-out but cumulative runner RSS growth
  // from gremlins' long-lived process state + each mutant's full
  // `go test ./internal/logql` cycle dragging the heavy LogQL parser dep graph
  // (loki + prometheus + dskit + memberlist) through the linker. Workers=1 alone
  // cannot land below the ceiling because the wall-clock budget before OOM
  // admits ~80 of ~300+ mutants in the package.
  //
  // Round 7 split phase4-logql into three sibling entries (lower / aggregation /
  // other), each scoped to ./internal/logql but with file regexes carving the
  // source set into roughly equal slices. Round 8 split `other` into
  // `other-a` + `other-b`. Round 9 peeled dotted_labels.go into its own slice,
  // and round 10 (job 77211633492) proved even a single-file slice of that
  // surface SIGTERM'd at ~3 min — the runner-budget ceiling sat below it. That
  // was the runaway-mutant class, now bounded absolutely by --timeout-max, so
  // dotted_labels.go is mutated again in other-a rather than left uncovered.
  //
  // phase4-logql-aggregation was historically relaxed to 93 (94.83%, 18 LIVED)
  // citing equivalent mutants. The KILLABLE survivors are now pinned by
  // range_aggregation_mutation_test.go + vector_aggregation_mutation_test.go
  // (10 killed: absent-window Interval+Offset arithmetic, the absent synth-label
  // conjunction, the post-filter error-mark return gate, the topk/quantile outer-by
  // threading + ungrouped-partition guards — each verified by hand-applying the
  // mutation). Of the remaining survivors:
  //   - `make([]Expr, 0, len(Groups)*2 (+1))` slice-CAPACITY hints
  //     (range_aggregation.go, vector_aggregation.go, duration.go) are NOT
  //     equivalent, though this block long claimed they were. A cap literal does
  //     change only allocator behaviour and not output, but observability is not
  //     confined to output: all three build a slice that becomes a returned
  //     `chplan.FuncCall`'s exported `Args` and fill the hint exactly, so `cap`
  //     reads the arithmetic back. detected_level.go's two hints are killed that
  //     way; cerberus issue #2984 tracks applying the same check here and across
  //     the rest of the matrix's capacity survivors.
  //   - empty-group CONDITIONALS_BOUNDARY guards (`len(...) > 0` vs `>= 0`) whose
  //     only distinguishing input (`by ()`) threads a len-0 label slice the
  //     lowering collapses to nil — a byte-identical no-op.
  // With the killable set covered the bar is back at 95; the survivors sit
  // comfortably under it (~97.7%).
  //
  // Round 13 (2026-07-25): all four ./internal/logql legs started dying with the
  // same OOM signature ~20-25 min into every push-to-main run, 100% reproducible,
  // starting exactly at the commit that moved internal/logql/dotted_labels.go's
  // implementation AND its large table-driven test suite into
  // internal/logql/lsyntax. `scope: ./internal/logql` recurses into every
  // subdirectory, so lsyntax/*.go was ALREADY being swept into these four legs —
  // none of the exclude regexes covered lsyntax's own files (ast.go, lexer.go,
  // parser.go, …), only dotted_labels.go itself (matched by filename regardless
  // of directory). Once dotted_labels_test.go relocated into package lsyntax,
  // every covered mutant anywhere in lsyntax forced
  // `go test .../internal/logql/lsyntax` to link and run that heavier test
  // binary, on every one of the four legs. Fix: keep the whole lsyntax subtree
  // out of all four legs, and give lsyntax its own dedicated legs so the
  // package keeps its mutation coverage instead of losing it. The three curated
  // legs do that by construction now — an `include_files` allowlist of bare
  // filenames can never match a `lsyntax/…` scope-relative path — and the
  // catch-all still names the subtree explicitly ('^lsyntax/', mirroring
  // phase4-traceql-other's '^ast/' precedent), which is what an
  // everything-else leg has to do to keep the subtree out.
  // ---------------------------------------------------------------------------
  {
    phase: 'phase4-logql-lower',
    scope: './internal/logql',
    efficacy: EFFICACY,
    workers: SERIAL_WORKERS,
    // Mutate only lower.go (~50 KB, the package's largest source file).
    include_files:
      '^(lower)\\.go$',
  },
  {
    phase: 'phase4-logql-aggregation',
    scope: './internal/logql',
    efficacy: EFFICACY,
    workers: SERIAL_WORKERS,
    // Mutate range_aggregation.go + vector_aggregation.go + lang.go (~54 KB).
    include_files:
      '^(lang|range_aggregation|vector_aggregation)\\.go$',
  },
  {
    phase: 'phase4-logql-other-a',
    scope: './internal/logql',
    efficacy: EFFICACY,
    workers: SERIAL_WORKERS,
    // Mutate binary.go + detected_level.go + dotted_labels.go (~21 KB).
    include_files:
      '^(binary|detected_level|dotted_labels)\\.go$',
  },
  {
    phase: 'phase4-logql-other-b',
    scope: './internal/logql',
    efficacy: EFFICACY,
    workers: SERIAL_WORKERS,
    // The catch-all leg: drop_keep.go + literal.go + label_fns.go +
    // top_level_columns.go, plus every file no other leg claims (doc.go,
    // duration.go, ip.go, jsonpath.go, numbytes.go, pattern_filter.go,
    // variants.go, logpattern/). A file newly added to internal/logql needs no
    // edit here — it is picked up automatically, which is why this leg keeps no
    // positive file list to fall out of sync.
    exclude_files:
      '^lsyntax/|^(lower|range_aggregation|vector_aggregation|lang|binary|detected_level|dotted_labels)\\.go$',
  },
  {
    phase: 'phase4-logql-parser',
    scope: './internal/logql/lsyntax',
    efficacy: EFFICACY,
    workers: SERIAL_WORKERS,
    // Mutate parser.go only (~27 KB, the recursive-descent LogQL parser — the
    // densest control-flow surface in the package, same shape as
    // phase4-traceql-parser). dotted_labels.go belongs to the lsyntax leg.
    include_files:
      '^(parser)\\.go$',
  },
  {
    phase: 'phase4-logql-lsyntax',
    scope: './internal/logql/lsyntax',
    efficacy: EFFICACY,
    workers: SERIAL_WORKERS,
    // The rest of the lsyntax package (AST node defs, lexer, label-filter
    // matching, operators, dotted-label normalisation); parser.go runs above.
    exclude_files: '^parser\\.go$',
  },

  // ---------------------------------------------------------------------------
  // phase4-traceql — four legs, split by PACKAGE.
  //
  // The original monolithic phase OOM-killed the runner after ~3 minutes: it
  // contained ~109 timeout-inducing mutants in the recursive-descent parser plus
  // tens of thousands more in lower.go. Round 11's first split OOM-killed
  // `-other` (it kept most of the ast package) and the `-parser`/`-lower` legs
  // errored before running: their exclude regexes used negative lookahead
  // `(?!…)`, which Go's RE2 engine rejects — pinned now by mutation-matrix.mjs's
  // RE2-safety assertion, so that mistake fails at PR time instead of mid-run.
  // Round 12 splits into four legs, each an RE2-safe alternation naming the
  // files it owns (the two curated legs) or the files its siblings own (the two
  // catch-alls).
  //
  // Efficacy is preserved by scoping rather than lost: gremlins runs the mutated
  // file's own package tests per mutant, so scoping the ast legs to
  // ./internal/traceql/ast still runs parser_test.go / *_mutation_test.go.
  // ---------------------------------------------------------------------------
  {
    phase: 'phase4-traceql-parser',
    scope: './internal/traceql/ast',
    efficacy: EFFICACY,
    workers: SERIAL_WORKERS,
    // ast/parser.go only — where the ~109 timeout-inducing loop-control mutants
    // live. Both file patterns match SCOPE-RELATIVE paths (here, relative to
    // ./internal/traceql/ast), so entries are bare filenames.
    include_files:
      '^(parser)\\.go$',
  },
  {
    phase: 'phase4-traceql-ast',
    scope: './internal/traceql/ast',
    efficacy: EFFICACY,
    workers: SERIAL_WORKERS,
    // The rest of the ast package (lexer + node defs); parser.go runs above.
    exclude_files: '^parser\\.go$',
  },
  {
    phase: 'phase4-traceql-lower',
    scope: './internal/traceql',
    efficacy: EFFICACY,
    workers: SERIAL_WORKERS,
    // lower.go only (~84 KB). scope ./internal/traceql recurses into ast/; the
    // allowlist keeps that subtree out by construction, since a bare-filename
    // pattern can never match an `ast/…` scope-relative path.
    include_files:
      '^(lower)\\.go$',
  },
  {
    phase: 'phase4-traceql-other',
    scope: './internal/traceql',
    efficacy: EFFICACY,
    workers: SERIAL_WORKERS,
    // The remaining top-level files (aggregate, metrics_compare,
    // metrics_pipeline, group_coalesce, search_limit, select,
    // spanset_operand).
    exclude_files: '^ast/|^lower\\.go$',
  },

  {
    phase: 'phase5-qlcommon',
    scope: './internal/qlcommon',
    efficacy: EFFICACY,
    workers: DEFAULT_WORKERS,
  },
  // phase6-spansscan covers the shared spans-scan partition-pruning matcher —
  // the predicate functions behind the universal emit-chokepoint guard and the
  // perf/fanout corpus lint. A tiny stdlib-only leaf package, so the run is fast.
  {
    phase: 'phase6-spansscan',
    scope: './internal/spansscan',
    efficacy: EFFICACY,
    workers: DEFAULT_WORKERS,
  },
];

// The workflow this lane IS. Everything the lane executes is reachable from it,
// so it is the one entry point the derivation below needs.
export const MUTATION_LANE_WORKFLOW = '.github/workflows/mutation.yml';

// Inputs the lane READS rather than executes, and which therefore appear in no
// source graph: gremlins' own configuration, and the module graph every leg's
// `go test` links and mutates. Declared, because nothing can derive them — and
// short enough to review, which is the property the old hand-written harness
// list lost the moment it grew past what a reader could hold.
//
// `.github/ci-lanes.json` is deliberately NOT here: registry changes go through
// the semantic projection in mutation-matrix.mjs, so unrelated metadata does not
// spend a full matrix while mutation-relevant edits still do. Just recipes are
// local entry points and do not select CI mutation work.
export const MUTATION_DATA_PATHS = ['.gremlins.yaml', 'go.mod', 'go.sum'];

// Paths that change the LANE ITSELF rather than a single scope, and therefore
// force the FULL matrix.
//
// DERIVED, not listed. The executed half comes from lane-harness.mjs walking
// mutation.yml's own `node` steps and composite actions into the module graph
// reachable from them; only the read-only data inputs above are declared. A
// hand-written array is the shape that rots: `mutant-memory-guard.mjs` sat in
// front of every leg's test binary, deciding each mutant's fate, and was absent
// from the array for its whole life (cerberus #2948) — as were `lib/gh.mjs`,
// `go-module-fetch.mjs`, `lib/registry.mjs` and the `setup-go` action. Deriving
// makes "the runner gained a file" and "the harness set gained a path" the same
// event instead of two, the second of which nobody performs.
export const HARNESS_PATHS = [
  ...laneHarnessClosure({ workflow: MUTATION_LANE_WORKFLOW }),
  ...MUTATION_DATA_PATHS,
];
