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
//     FilterProjectTranspose + FilterAggregateTranspose +
//     FilterRangeWindowTranspose
//  4. optimizer.projection (FixedPoint) — ProjectionPushdown
//  5. optimizer.mv-substitution (FixedPoint) — MVSubstitution.
//     Runs last so predicate pushdown has already
//     surfaced the `RangeWindow(Scan(base))` patterns the rule
//     matches against. The rule needs a Metrics schema to read its
//     rollup registry from; Default() binds the default OTel schema,
//     DefaultWithSchema lets handler wiring override.
//
// Each batch's name is prefixed `analyzer.` or `optimizer.` to make
// the contract obvious in trace logs.
//
// # Cost model (mv-substitution)
//
// The MV-substitution batch is the first place cerberus picks among
// equivalent plans (a rollup-scanned query and a base-scanned query
// produce the same answer when the substitution is safe). The current
// cost model is deliberately simple — `firstApplicable`, which
// picks the first registry-listed rollup that passes the safety
// conditions — and an unexported `costModel` interface stub so v2
// can swap in a real estimator (per Jindal VLDB 2018 §4–§6) without
// touching the rule. See mv_substitution.go for the safety-condition
// breakdown and docs/optimizer-research.md § 6 for the design
// rationale.
//
// # File layout
//
// Rules ship in their own files (filter_fusion.go, constant_fold.go,
// projection_pushdown.go, filter_project_transpose.go,
// filter_aggregate_transpose.go, filter_range_window_transpose.go,
// mv_substitution.go); the driver + batch + analyzer types live in
// rule.go, batch.go, and analyzer.go.
package optimizer
