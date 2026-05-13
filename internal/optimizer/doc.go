// Package optimizer rewrites a chplan tree to an equivalent, cheaper one by
// running registered rules grouped into Catalyst-style batches.
//
// A Driver is a sequence of Batches; each Batch is a sequence of Rules
// sharing a Strategy. Two strategies ship today:
//
//   - Once — run every rule in the batch exactly once, in declared
//     order. Suits idempotent / analyzer-shaped passes (constant
//     folding).
//   - FixedPoint(n) — iterate the batch until no rule reports a change
//     (a fixpoint) or n iterations have elapsed. Suits rules that
//     unlock each other (filter fusion + filter–project / filter–
//     aggregate transpose).
//
// Default() returns the project's seed batch sequence: constant folding
// (Once), then predicate pushdown (FixedPoint), then projection pushdown
// (FixedPoint). Later RC3 rules (PREWHERE promotion at R3.4, the
// analyzer / optimizer split at R3.5) plug into this structure without
// changing the driver wiring.
//
// Rules ship in their own files (filter_fusion.go, constant_fold.go,
// projection_pushdown.go, filter_project_transpose.go,
// filter_aggregate_transpose.go); the driver + batch types live in
// rule.go and batch.go.
package optimizer
