// Package optimizer rewrites a chplan tree to an equivalent, cheaper one by
// running registered rules grouped into Catalyst-style batches.
//
// A Driver is a sequence of Batches; each Batch is a sequence of Rules
// sharing a Strategy. Three strategies ship today:
//
//   - Once — run every rule in the batch exactly once, in declared
//     order. Suits genuinely-idempotent passes that are not part of
//     the language contract (e.g. ConstantFoldHeuristic).
//   - Analyzer — run every AnalyzerRule once, then verify idempotence
//     on a second pass. Panics on contract violation. Suits **semantic
//     / must-run** passes that produce a canonical form downstream
//     rules depend on (e.g. ConstantFoldSemantic).
//   - FixedPoint(n) — iterate the batch until no rule reports a change
//     (a fixpoint) or n iterations have elapsed. Suits **heuristic**
//     rules that unlock each other (filter fusion + filter–project /
//     filter–aggregate transpose).
//
// # AnalyzerRule vs OptimizerRule (DataFusion split)
//
// The optimizer borrows DataFusion's distinction between AnalyzerRule
// (semantic / must-run / idempotent) and OptimizerRule (heuristic /
// optional / may iterate) — see the DataFusion docs at
// https://docs.rs/datafusion-optimizer/latest/datafusion_optimizer/ for
// the upstream contract. Spark Catalyst and DuckDB make a similar cut;
// the Tsinghua "Selective Late Materialization" paper takes it as a
// given.
//
// Concretely: cerberus's original `ConstantFold` rule conflated two
// flavours of fold —
//
//   - pure-literal arithmetic / comparison (`1+2 → 3`, `1=0 → false`)
//     is a semantic invariant: downstream rules rely on pure-literal
//     subtrees having collapsed to a single Lit;
//   - boolean identity (`true AND X → X`, `false OR X → X`) is
//     ergonomic — it shrinks emitted SQL but the result is correct
//     either way.
//
// Today the conflated rule is split into ConstantFoldSemantic
// (AnalyzerRule, lives in the analyzer batch) and ConstantFoldHeuristic
// (OptimizerRule, lives in a Once batch right after the analyzer).
//
// # Default pipeline
//
// Default() returns the project's seed batch sequence:
//
//  1. analyzer.constant-fold-semantic (Analyzer)
//  2. optimizer.constant-fold-heuristic (Once)
//  3. optimizer.predicate-pushdown (FixedPoint) — FilterFusion +
//     FilterAggregateTranspose + FilterRangeWindowTranspose
//  4. optimizer.projection (FixedPoint) — ProjectionPushdown
//
// Each batch's name is prefixed `analyzer.` or `optimizer.` to make
// the contract obvious in trace logs.
//
// # Which rules fire on the current corpus
//
// Every technique has to earn its place. Measured against the full
// test/spec corpus (PromQL + LogQL + TraceQL), with the optimizer walk
// made total across all 26 chplan node types by #812, the active rules
// are:
//
//   - ConstantFoldSemantic (Analyzer) — canonical-form invariant;
//     fires whenever a lowering emits literal-only arithmetic.
//   - ConstantFoldHeuristic (Once) — fires (e.g. the TraceQL
//     `rate() by(kind)` drilldown whose predicate carries
//     `(... AND true) AND true` above a MetricsAggregate, reachable
//     only since #812's total walk).
//   - FilterFusion — fires on adjacent-filter shapes the lowerings
//     emit (histogram-bucket label filters, matrix-selector chains).
//   - FilterRangeWindowTranspose — fires (e.g. `topk(0, up)[5m:1m]`,
//     whose `topk(0,…)` lowers to a `Filter(false)` directly above a
//     RangeWindow, which the rule pushes under the window).
//   - ProjectionPushdown — fires broadly (the dominant rewrite, ~100
//     fixtures: every aggregate / binop / range lowering narrows its
//     scan column set through this pass).
//
// SPECULATIVE (0 fires on the current corpus — kept as cheap
// correctness insurance, not counted as an active optimization):
//
//   - FilterAggregateTranspose — would push a group-key label filter
//     stacked *above* an Aggregate down beneath it. No current lowering
//     emits `Filter(Aggregate(…))` with a bare group-key predicate —
//     PromQL `sum by (job) (m{job="x"})` lowers the `job="x"` matcher
//     into the scan PREWHERE, not above the aggregate. The rule is
//     retained so a future lowering that *does* surface a group-key
//     filter above an aggregate is handled correctly without a
//     re-derivation. Its unit + rule-interaction tests pin the
//     behaviour; the cost of keeping it is a no-op pattern probe per
//     fixpoint iteration.
//
// Retired (2026-06): FilterProjectTranspose (0 fires — no lowering
// emits `Filter(Project(…))`; its only durable contribution was the
// shared `onlyReferencesPassthrough` helper, hoisted to
// transpose_shared.go) and MVSubstitution (no rollup roadmap; the
// default schema shipped no live rollups, so the rule was a guaranteed
// no-op and is removed rather than carried as dead weight).
//
// # File layout
//
// Rules ship in their own files (filter_fusion.go, constant_fold.go,
// projection_pushdown.go, filter_aggregate_transpose.go,
// filter_range_window_transpose.go); the shared transpose helper lives
// in transpose_shared.go; the driver + batch + analyzer types live in
// rule.go, batch.go, and analyzer.go.
package optimizer
