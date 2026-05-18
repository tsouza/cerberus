// Package promql is the from-scratch PromQL evaluator that serves as
// the independent specification for cerberus's property-testing
// framework. Unlike test/property/oracle/bridge.go — which delegates to
// Prometheus's own promql.Engine via internal/promshim/local — this
// package reimplements the subset of PromQL semantics the property
// test exercises, in cerberus's tree, from the [PromQL semantics
// documentation] and the canonical engine behavior. The whole point is
// to catch bugs cerberus shares with upstream Prometheus: when the
// bridge oracle agrees with cerberus on a buggy answer, that bug is
// invisible — but the from-scratch oracle, derived from the spec
// rather than the same code, will surface it.
//
// # MVP coverage (Phase 1 PR 2)
//
//   - Selectors: bare instant `m{...}`, range `m[5m]`, all four
//     matcher kinds (=, !=, =~, !~). The instant selector implements
//     Prom's LookbackDelta (default 5min) and the eval_ts boundary
//     (`Timestamp <= T && T - Timestamp < lookback`).
//   - Functions over range vectors: rate, increase, delta,
//     sum_over_time, avg_over_time, min_over_time, max_over_time,
//     count_over_time. rate/increase implement Prom's
//     counter-reset detection + window-edge extrapolation.
//   - Aggregations: sum, avg, min, max, count, topk, bottomk —
//     each with by()/without() grouping.
//   - Binary ops: arithmetic (+, -, *, /, %), comparison
//     (==, !=, <, >, <=, >=) with `bool` modifier, scalar-vector
//     and vector-vector matching with on/ignoring +
//     group_left/group_right (and label-include).
//   - Histograms: histogram_quantile(phi, sum by(le)(rate(<bucket>[
//     range]))) over classic-histogram (`_bucket`-suffixed) data.
//     Native histograms are not covered by the oracle.
//   - Modifiers: @<ts>, @start(), @end(), offset (positive, zero,
//     negative).
//
// # Critical semantic decisions
//
// These are the points where the oracle MUST differ from a naive
// implementation and follow PromQL semantics precisely:
//
//  1. Instant selector LWR: for `m{...}` at time T, return for each
//     matched series the latest sample with `Timestamp <= T` AND
//     `T - Timestamp < lookback` (default 5min).
//  2. Aggregation semantics: aggregate the LWR sample per series
//     first, THEN sum/avg/min/max/count. A naive implementation
//     that just sums every stored sample's value is wrong for
//     instant queries.
//  3. Series identity: keyed by labelset MINUS __name__ — Prom
//     convention. The oracle hashes the canonical sorted label
//     representation so two series with the same effective labels
//     collide deterministically.
//  4. Range vector window: `m[5m]` at T includes samples with
//     timestamps in (T-5m, T] — EXCLUSIVE on the left, INCLUSIVE
//     on the right.
//  5. rate/increase: per Prom's algorithm — counter-reset detection
//     plus extrapolation to window edges (the window's mathematical
//     [T-range, T] interval, NOT the first/last actual sample's
//     timestamps).
//  6. Float comparison: NaN != NaN per IEEE, but the oracle's diff
//     and aggregation paths treat NaN == NaN so a property-test
//     comparator doesn't flake on NaN noise.
//
// # Entry point
//
// Callers use [Evaluate], which is a pure function: it takes the
// dataset, a query (string form), and the eval timestamp; it parses
// the query via Prometheus's parser (so the AST shape matches what
// cerberus's pipeline sees), then walks the AST under in-tree
// evaluation rules. There is NO promql.Engine call — the engine and
// its many extension points are exactly what we're independent of.
//
// # File layout
//
//   - doc.go — this file.
//   - evaluator.go — top-level Evaluate; AST dispatch.
//   - data.go — the internal model: Series, Sample, hashing.
//   - selector.go — instant + range vector selector evaluation.
//   - matcher.go — label matchers against the in-memory model.
//   - functions.go — range-vector functions
//     (rate/increase/delta/*_over_time).
//   - aggregations.go — sum/avg/min/max/count/topk/bottomk with
//     by/without.
//   - binary.go — binary ops + vector matching.
//   - histogram.go — histogram_quantile over classic buckets.
//   - modifiers.go — @ts/@start()/@end() + offset application.
//
// [PromQL semantics documentation]: https://prometheus.io/docs/prometheus/latest/querying/basics/
package promql
