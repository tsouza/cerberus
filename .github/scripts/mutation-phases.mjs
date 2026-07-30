// mutation-phases.mjs — the single source of truth for the `mutation` lane's
// phase partition (mutation.yml's `mutate` strategy.matrix).
//
// Each entry is one gremlins run: a `scope` package (walked recursively), an
// optional scope-relative `exclude_files` RE2 alternation carving that scope
// into sibling legs, a `workers` cap, and the `--threshold-efficacy` bar the
// leg must clear. The keys are the matrix keys mutation.yml interpolates as
// `matrix.scope` / `matrix.workers` / `matrix.exclude_files` / `matrix.efficacy`
// / `matrix.phase`, so this table renders straight into `strategy.matrix.include`.
//
// It lives in JS rather than inline YAML because mutation-matrix.mjs has to
// REASON about it — selecting the legs an ordinary PR's diff actually touches,
// and asserting the exclude regexes stay RE2-safe — and a workflow `include:`
// block is data the workflow cannot read back.
//
// node: builtins only — no npm deps, no setup-node needed.

// Every leg clears the same bar. `.gremlins.yaml`'s threshold (currently 95) is
// the floor for the unscoped whole-repo `just mutate` invocation only; the
// per-phase bar is set here so one weak package cannot hide behind a strong one
// in a repo-wide average.
const EFFICACY = 95;

// gremlins' default fan-out is runtime.NumCPU(). DEFAULT_WORKERS keeps it;
// SERIAL_WORKERS caps a leg at one concurrent `go test` cycle, which is what
// the heavy parser/lowering legs need to stay under the ubuntu-latest memory
// ceiling (see the phase4 rationale blocks below).
const DEFAULT_WORKERS = 0;
const SERIAL_WORKERS = 1;

export const PHASES = [
  { phase: 'phase1', scope: './internal/chplan', efficacy: EFFICACY, workers: DEFAULT_WORKERS },

  // ---------------------------------------------------------------------------
  // phase2 — three sibling legs over ./internal/chsql.
  //
  // Unsharded, phase2 was the mutation lane's sole critical path: 936 executed
  // mutants at ~2.4s each put it at 38 minutes while the next-slowest leg
  // (phase4-promql) finished in 13. Every other leg idled for 25 minutes waiting
  // on it, so the lane's wall time was phase2's wall time and nothing else.
  //
  // Unlike the phase4-logql split, this one is NOT a memory-ceiling workaround —
  // chsql's test binary links no heavy parser graph and the leg never OOM'd. It
  // is a pure wall-clock split, so the legs keep gremlins' default fan-out.
  //
  // The partition is by MEASURED executed-mutant count (KILLED + LIVED per file,
  // read off a full phase2 report), greedily balanced to ~313/310/313 — roughly
  // 12.5 minutes each. It is deliberately not thematic: range_window.go alone
  // carries 186 mutants, so any split that kept "the range-window emitters"
  // together would rebuild the critical path it exists to remove.
  //
  // Rebalance by re-measuring, never by moving a file to keep a shard above the
  // bar. Shard boundaries chosen to hide a weak file are the same evasion as
  // relaxing `efficacy` — both leave the tests that failed to kill a mutant
  // exactly as weak, and only change which number reports it.
  {
    phase: 'phase2-compare',
    scope: './internal/chsql',
    efficacy: EFFICACY,
    workers: DEFAULT_WORKERS,
    // metrics_compare + prewhere + exemplars + structural_join + late_mat +
    // range_window_native + range_bucket_fanout + emit + vector_set_op + set_op.
    exclude_files:
      '^(absent_over_time|builder|chaos_sleep|chaos_sleep_stub|ddl|doc|emit_node|histogram_over_time|histogram_quantile|histogram_quantile_native|info_join|metrics_second_stage|nary_vector_set_op|nested_set_annotate|query_exemplars|range_lwr|range_window|range_window_fused|range_window_resample|scan_resource_bound|search_trace_limit|tableshape|vector_join)\\.go$',
  },
  {
    phase: 'phase2-builder',
    scope: './internal/chsql',
    efficacy: EFFICACY,
    workers: DEFAULT_WORKERS,
    // builder + emit_node + histogram_over_time + vector_join + range_lwr +
    // histogram_quantile_native + range_window_resample + query_exemplars.
    exclude_files:
      '^(absent_over_time|chaos_sleep|chaos_sleep_stub|ddl|doc|emit|exemplars|histogram_quantile|info_join|late_mat|metrics_compare|metrics_second_stage|nary_vector_set_op|nested_set_annotate|prewhere|range_bucket_fanout|range_window|range_window_fused|range_window_native|scan_resource_bound|search_trace_limit|set_op|structural_join|tableshape|vector_set_op)\\.go$',
  },
  {
    phase: 'phase2-other',
    scope: './internal/chsql',
    efficacy: EFFICACY,
    workers: DEFAULT_WORKERS,
    // catch-all leg: range_window + nested_set_annotate + scan_resource_bound +
    // ddl + range_window_fused + metrics_second_stage + nary_vector_set_op +
    // tableshape, plus every file no other leg claims. A file newly added to
    // internal/chsql needs no edit here — it is picked up automatically, which
    // is why this leg keeps no positive file list to fall out of sync. The two
    // legs above DO enumerate it, so a new file is briefly mutated three times
    // over; TestMutationLegsPartitionEveryScopedFile fails on exactly that.
    exclude_files:
      '^(builder|emit|emit_node|exemplars|histogram_over_time|histogram_quantile_native|late_mat|metrics_compare|prewhere|query_exemplars|range_bucket_fanout|range_lwr|range_window_native|range_window_resample|set_op|structural_join|vector_join|vector_set_op)\\.go$',
  },
  {
    phase: 'phase3-optimizer',
    scope: './internal/optimizer',
    efficacy: EFFICACY,
    workers: DEFAULT_WORKERS,
  },
  {
    phase: 'phase4-promql',
    scope: './internal/promql',
    efficacy: EFFICACY,
    workers: DEFAULT_WORKERS,
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
  // other), each scoped to ./internal/logql but with --exclude-files regexes
  // carving the source set into roughly equal slices. Round 8 split `other` into
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
  // mutation). The remaining survivors are GENUINELY EQUIVALENT and can't be killed:
  //   - `make([]Expr, 0, len(Groups)*2 (+1))` slice-CAPACITY hints
  //     (range_aggregation.go, vector_aggregation.go, duration.go) — a cap literal
  //     changes only allocator behaviour, not output (CLAUDE.md: capacity hints
  //     out of scope).
  //   - empty-group CONDITIONALS_BOUNDARY guards (`len(...) > 0` vs `>= 0`) whose
  //     only distinguishing input (`by ()`) threads a len-0 label slice the
  //     lowering collapses to nil — a byte-identical no-op.
  // With the killable set covered the bar is back at 95; the equivalents sit
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
  // binary, on every one of the four legs. Fix: exclude the whole lsyntax
  // subtree (scope-relative '^lsyntax/', mirroring the phase4-traceql-lower
  // '^ast/' precedent) from all four legs, and give lsyntax its own dedicated
  // legs so the package keeps its mutation coverage instead of losing it.
  // ---------------------------------------------------------------------------
  {
    phase: 'phase4-logql-lower',
    scope: './internal/logql',
    efficacy: EFFICACY,
    workers: SERIAL_WORKERS,
    // Mutate only lower.go (~50 KB, the package's largest source file).
    // Excludes every other logql source plus the lsyntax subtree.
    exclude_files:
      '^lsyntax/|(binary|detected_level|dotted_labels|literal|label_fns|top_level_columns|range_aggregation|vector_aggregation|lang)\\.go$|^logpattern/|^(doc|duration|ip|jsonpath|numbytes|pattern_filter|variants)\\.go$',
  },
  {
    phase: 'phase4-logql-aggregation',
    scope: './internal/logql',
    efficacy: EFFICACY,
    workers: SERIAL_WORKERS,
    // Mutate range_aggregation.go + vector_aggregation.go + lang.go (~54 KB).
    exclude_files:
      '^lsyntax/|(binary|detected_level|dotted_labels|literal|label_fns|top_level_columns|lower)\\.go$|^logpattern/|^(doc|duration|ip|jsonpath|numbytes|pattern_filter|variants)\\.go$',
  },
  {
    phase: 'phase4-logql-other-a',
    scope: './internal/logql',
    efficacy: EFFICACY,
    workers: SERIAL_WORKERS,
    // Mutate binary.go + detected_level.go + dotted_labels.go (~21 KB).
    exclude_files:
      '^lsyntax/|(lower|range_aggregation|vector_aggregation|lang|literal|label_fns|top_level_columns)\\.go$|^logpattern/|^(doc|duration|ip|jsonpath|numbytes|pattern_filter|variants)\\.go$',
  },
  {
    phase: 'phase4-logql-other-b',
    scope: './internal/logql',
    efficacy: EFFICACY,
    workers: SERIAL_WORKERS,
    // The catch-all leg: literal.go + label_fns.go + top_level_columns.go, plus
    // every file no other leg claims (doc.go, duration.go, ip.go, jsonpath.go,
    // numbytes.go, pattern_filter.go, variants.go, logpattern/). A file newly
    // added to internal/logql needs no edit here — it is picked up
    // automatically, which is why this leg keeps no positive file list to fall
    // out of sync.
    exclude_files:
      '^lsyntax/|(lower|range_aggregation|vector_aggregation|lang|binary|detected_level|dotted_labels)\\.go$',
  },
  {
    phase: 'phase4-logql-parser',
    scope: './internal/logql/lsyntax',
    efficacy: EFFICACY,
    workers: SERIAL_WORKERS,
    // Mutate parser.go only (~27 KB, the recursive-descent LogQL parser — the
    // densest control-flow surface in the package, same shape as
    // phase4-traceql-parser). dotted_labels.go belongs to the lsyntax leg.
    exclude_files: '^(ast|binop|dotted_labels|errors|labelfilter|lexer|ops|string)\\.go$',
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
  // Round 12 splits into four legs, each an RE2-safe alternation of the files to
  // exclude.
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
    // live. exclude-files matches SCOPE-RELATIVE paths (here, relative to
    // ./internal/traceql/ast), so entries are bare filenames.
    exclude_files: '^(assert|attribute|doc|enum|expr|lexer|metrics|parse|pipeline|rewrite|static)\\.go$',
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
    // lower.go only (~84 KB). scope ./internal/traceql recurses into ast/, so
    // exclude the whole ast subtree plus the other top-level files.
    exclude_files: '^ast/|^(aggregate|doc|group_coalesce|metrics_compare|metrics_pipeline|search_limit|select)\\.go$',
  },
  {
    phase: 'phase4-traceql-other',
    scope: './internal/traceql',
    efficacy: EFFICACY,
    workers: SERIAL_WORKERS,
    // The remaining top-level files (aggregate, metrics_compare,
    // metrics_pipeline, group_coalesce, search_limit, select).
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

// Paths that change the LANE ITSELF rather than a single scope: the phase table,
// the selector, the threshold gate, the workflow, gremlins' own config, and the
// module graph every leg's `go test` links. A PR touching any of them gets the
// FULL matrix, because a scoped subset would be testing the new harness against
// an arbitrary slice of the tree and proving nothing about the rest.
export const HARNESS_PATHS = [
  '.github/workflows/mutation.yml',
  '.github/scripts/mutation-phases.mjs',
  '.github/scripts/mutation-matrix.mjs',
  '.github/scripts/gremlins-threshold.mjs',
  '.github/scripts/lib/scope-gate.mjs',
  '.gremlins.yaml',
  'go.mod',
  'go.sum',
];
