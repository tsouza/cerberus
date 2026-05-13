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
// optional / may iterate) — see docs/optimizer-research.md § 4 for the
// rationale and the DataFusion docs at
// https://docs.rs/datafusion-optimizer/latest/datafusion_optimizer/ for
// the upstream contract. Spark Catalyst and DuckDB make a similar cut;
// the Tsinghua "Selective Late Materialization" paper (RC3 R3.7) takes
// it as a given.
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
// R3.5 splits the conflated rule into ConstantFoldSemantic
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
//     FilterProjectTranspose + FilterAggregateTranspose
//  4. optimizer.projection (FixedPoint) — ProjectionPushdown
//
// Later RC3 rules (PREWHERE promotion at R3.4, Filter–RangeWindow
// transpose at R3.8) plug into this structure without changing the
// driver wiring. Each batch's name is prefixed `analyzer.` or
// `optimizer.` to make the contract obvious in trace logs.
//
// # File layout
//
// Rules ship in their own files (filter_fusion.go, constant_fold.go,
// projection_pushdown.go, filter_project_transpose.go,
// filter_aggregate_transpose.go); the driver + batch + analyzer types
// live in rule.go, batch.go, and analyzer.go.
package optimizer
