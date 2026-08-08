package chplan

import "time"

// RangeLWRSampleTimestampColumn is the name of the OPTIONAL fifth column
// [RangeLWR] publishes when [RangeLWR.SampleTimestamp] is set: the own
// timestamp of the sample the LWR selected for each (series, anchor)
// bucket, as distinct from the anchor the row is indexed by.
//
// The two are genuinely different values. `TimestampCol` on a RangeLWR row
// is the STEP ANCHOR — a range query's rows are indexed by step, and two
// adjacent anchors under one lookback window carry the SAME sample. This
// column is that sample's own time, which is what PromQL's
// `timestamp(<selector>)` reports as its VALUE (reference Prometheus's
// rangeEvalTimestampFunctionOverVectorSelector emits `float64(s.T)/1000`,
// the selected sample's time, while `rangeEval` stamps the output point
// with the step). Nothing else needs it, so it is published on request
// rather than always — see [RangeLWR.SampleTimestamp].
const RangeLWRSampleTimestampColumn = "lwr_sample_ts"

// RangeLWR is the single-pass, bounded last-with-respect-to (LWR) plan
// node for a BARE instant-vector selector evaluated over a PromQL
// `query_range` window. It replaces the O(rows × anchors) StepGrid
// CROSS JOIN + per-anchor argMax shape the lowering previously emitted
// (see internal/promql/lower.go:wrapRangeLatestPerSeries) with an
// O(rows × lookback/step) sample-side fan-out that does NOT scale with
// the anchor count N.
//
// Semantics (identical to the StepGrid path it supersedes):
//
//   - The eval grid is `[Start, End]` spaced by Step, end-inclusive
//     (N = (End-Start)/Step + 1 anchors).
//   - At each anchor `t` the value is the most-recent sample in the
//     half-open staleness window `(t - Offset - Lookback, t - Offset]`
//     (default Lookback = 5m). When no sample falls in that window the
//     anchor produces NO row — the PromQL staleness gap.
//   - Output schema is the canonical 4-column Sample contract:
//     `(MetricNameCol, AttributesCol, TimeUnix = anchor_ts, ValueCol)`
//     — one row per (series, anchor) that had a sample, with TimeUnix
//     set to the UNSHIFTED grid anchor (the Offset shifts only the
//     membership window, not the emitted timestamp).
//
// The emitter (internal/chsql.emitRangeLWR) renders this as a single
// pass over Input: each sample row fans out (via arrayJoin over a
// bounded `range(lo, hi)` index set) to ONLY the ≤ Lookback/Step + 1
// anchors whose staleness window contains it, then a
// `GROUP BY (series, anchor_ts)` with `argMax(Value, TimeUnix)` collapses
// each (series, anchor) bucket to its newest in-window sample. Because
// each sample touches a bounded, N-independent number of anchors the
// intermediate (sample, anchor) cardinality is `rows × (Lookback/Step)`,
// constant in the grid width — the linear-in-N blowup is gone.
//
// Input is the matchers-filtered scan (Scan, or Filter-over-Scan, or the
// gauge+sum merge() Scan / companion UnionAll). It must expose the
// MetricNameCol / AttributesCol / TimestampCol / ValueCol columns under
// those names; the dual-table gauge+sum merge for unsuffixed names is
// preserved transparently because it lives inside Input's Scan
// (UnionTables → CH `merge(...)`).
type RangeLWR struct {
	Input Node

	// Start / End define the eval grid; Step is the grid spacing.
	Start time.Time
	End   time.Time
	Step  time.Duration

	// Lookback is the staleness horizon (instantLookback, default 5m).
	// The per-anchor window is `(anchor - Offset - Lookback, anchor - Offset]`.
	Lookback time.Duration

	// Offset is the PromQL `offset` modifier folded onto the selector.
	// It shifts the membership window back by Offset (a negative Offset
	// shifts it forward) WITHOUT moving the emitted anchor timestamp.
	Offset time.Duration

	// StepAlign marks a Start/End grid that was epoch-aligned (phase 0)
	// AT LOWERING TIME — the subquery inner-sample-grid shape
	// internal/promql.subqueryGridCtx builds for a bare-selector or
	// binary-expression subquery inner (mirrors RangeWindow.StepAlign's
	// semantics, PromQL's `interval * ((endTs - offset) / interval)`
	// evaluation grid). Unlike RangeWindow, whose chsql emitter re-floors
	// the anchor grid dynamically at SQL-render time regardless of the
	// Go-level Start/End it carries, RangeLWR's emitter (chsql.emitRangeLWR)
	// computes its anchor count from the concrete Start/End it is given —
	// there is no render-time floor to fall back on. So StepAlign's sole
	// consumer is chplan.ReanchorRange: when true, re-anchoring a shard
	// re-derives a fresh epoch-floored [Start, End] from the shard's own
	// predicted bounds instead of assigning them verbatim, keeping every
	// shard's anchor grid at the SAME absolute phase the unsliced query
	// would have used. False (the default) is every other RangeLWR shape —
	// the top-level query_range eval grid and any offset-based leaf — whose
	// phase is anchored to the request's own start/end, not to epoch 0.
	StepAlign bool

	// SampleTimestamp asks the emitter to publish ONE EXTRA output column,
	// [RangeLWRSampleTimestampColumn], carrying `max(TimestampCol)` over each
	// (series, anchor) bucket — the own timestamp of the sample the
	// `argMax(ValueCol, TimestampCol)` collapse picked. The collapse keeps the
	// VALUE and discards the timestamp that selected it, so without this the
	// sample's time is unrecoverable above the node and the only timestamp in
	// scope is the anchor.
	//
	// The four canonical columns are unchanged when it is set: TimestampCol
	// still carries the STEP ANCHOR, because a range query's rows are indexed
	// by step. Only a consumer that asked for the fifth column sees it, and
	// the projection that reads it drops it again, so the canonical Sample
	// contract holds either side of the request.
	//
	// Set by the `timestamp(<vector-selector>)` lowering in range mode
	// (internal/promql.timestampResultExpr), the one PromQL shape whose result
	// is the selected sample's own time rather than the evaluation step. False
	// everywhere else, which keeps every other range query's SQL unchanged.
	SampleTimestamp bool

	// Column names on Input (canonical OTel-CH: MetricName / Attributes /
	// TimeUnix / Value).
	MetricNameCol string
	AttributesCol string
	TimestampCol  string
	ValueCol      string
}

func (*RangeLWR) planNode() {}

func (r *RangeLWR) Children() []Node { return []Node{r.Input} }

func (r *RangeLWR) Equal(other Node) bool {
	o, ok := other.(*RangeLWR)
	if !ok {
		return false
	}
	if !r.Start.Equal(o.Start) || !r.End.Equal(o.End) {
		return false
	}
	if r.Step != o.Step || r.Lookback != o.Lookback || r.Offset != o.Offset {
		return false
	}
	if r.StepAlign != o.StepAlign || r.SampleTimestamp != o.SampleTimestamp {
		return false
	}
	if r.MetricNameCol != o.MetricNameCol || r.AttributesCol != o.AttributesCol {
		return false
	}
	if r.TimestampCol != o.TimestampCol || r.ValueCol != o.ValueCol {
		return false
	}
	if r.Input == nil || o.Input == nil {
		return r.Input == nil && o.Input == nil
	}
	return r.Input.Equal(o.Input)
}
