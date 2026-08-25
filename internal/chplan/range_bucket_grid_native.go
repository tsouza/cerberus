package chplan

import "time"

// RangeBucketGridNative is the ClickHouse-native sibling of
// [RangeBucketFanout] for the ONE range-mode shape whose fan-out cost is
// dominated by a lambda that captures the whole bucket matrix: the
// classic-histogram `rate` fold behind
// `histogram_quantile(phi, <agg> by(le) (rate(<bucket>[range])))`.
//
// Why the fan-out is expensive there, and why an aggregate is not. The
// fan-out shape reduces each (series, anchor) window to groupArrays of the
// window's ExplicitBounds / BucketCounts / TimeUnix, then folds ONE rung at
// a time with `arrayMap(u -> …, <union bounds>)` — a lambda whose body
// READS those groupArrays. ClickHouse materialises a lambda's captured
// columns once per outer-array element, so the fold builds |union bounds|
// copies of the entire per-series bucket matrix. The timeSeries*ToGrid
// family is a set of AGGREGATE functions: they consume rows through
// addBatch and never construct the captured-column replica, so the same
// arithmetic runs at a small constant working set.
//
// The node therefore expresses the fold as an aggregation instead of an
// array expression. It UNNESTS each stored histogram row into one row per
// `le` rung carrying that rung's CUMULATIVE observation count — which is
// exactly the scalar counter time series reference Prometheus models
// `<name>_bucket{le="X"}` as — runs the native per-grid-point rate
// aggregate over each rung independently, and regroups the per-rung rates
// back into one ladder per (series, anchor).
//
// Output schema, byte-identical to the reshape Project pair the fan-out
// shape wears (see internal/promql.classicBucketWindowReshape):
//
//	(<AnchorAlias>, <GroupByAliases...>, <BucketCountsCol>, <ExplicitBoundsCol>)
//
// so the across-series merge Aggregate above it is unaffected by the
// substitution.
//
// Input is the matchers-filtered scan (Scan, or Filter-over-Scan, possibly
// with the `le` restriction Project). It must expose TimestampCol,
// BucketCountsCol, ExplicitBoundsCol and the GroupBy expressions' source
// columns.
type RangeBucketGridNative struct {
	Input Node

	// Start / End define the eval grid; Step is the grid spacing. Both are
	// pinned (a materialised query_range grid) — this node is never built
	// for an instant evaluation.
	Start time.Time
	End   time.Time
	Step  time.Duration

	// Range is the rate window `[range]`: the per-anchor membership window
	// is `(anchor - Offset - Range, anchor - Offset]`, the same half-open
	// span [RangeBucketFanout.Lookback] describes.
	Range time.Duration

	// Offset is the PromQL `offset` modifier folded onto the membership
	// window only — it never moves the emitted anchor timestamp.
	Offset time.Duration

	// GroupBy / GroupByAliases are the SERIES identity keys. The anchor key
	// is implicit (always prepended under AnchorAlias) and must NOT appear
	// here. Keying on series identity rather than the user's `by/without`
	// labels is what puts the per-series two-sample floor on the axis
	// reference PromQL puts it on (#1629).
	GroupBy        []Expr
	GroupByAliases []string

	// AnchorAlias is the output column name for the grid anchor.
	AnchorAlias string

	// TimestampCol / BucketCountsCol / ExplicitBoundsCol name the classic
	// histogram row's columns on Input, and the latter two are also the
	// output aliases of the rebuilt ladder.
	TimestampCol      string
	BucketCountsCol   string
	ExplicitBoundsCol string
}

func (*RangeBucketGridNative) planNode() {}

func (r *RangeBucketGridNative) Children() []Node { return []Node{r.Input} }

// NumAnchors is the number of grid anchor points this native aggregate
// materialises: one row per Step across [Start, End] (end-inclusive), i.e.
// (End-Start)/Step + 1. Unlike [RangeBucketFanout] and [RangeLWR] this node
// is only ever built with a pinned, non-zero Start/End (range mode is a
// precondition — see emitRangeBucketGridNative's own validation), but the
// zero-grid guard is kept for the same defence-in-depth reason
// [RangeBucketFanout.NumAnchors] keeps it: a caller of this method should
// never have to also know this node's own construction invariants hold.
// Same rationale as [RangeBucketFanout.NumAnchors] for why this axis needs
// its own charge in [requireSubquerySampleBudget].
func (r *RangeBucketGridNative) NumAnchors() int64 {
	return numAnchorsFromGrid(r.Start, r.End, r.Step)
}

func (r *RangeBucketGridNative) Equal(other Node) bool {
	o, ok := other.(*RangeBucketGridNative)
	if !ok {
		return false
	}
	if !r.Start.Equal(o.Start) || !r.End.Equal(o.End) {
		return false
	}
	if r.Step != o.Step || r.Range != o.Range || r.Offset != o.Offset {
		return false
	}
	if r.AnchorAlias != o.AnchorAlias || r.TimestampCol != o.TimestampCol {
		return false
	}
	if r.BucketCountsCol != o.BucketCountsCol || r.ExplicitBoundsCol != o.ExplicitBoundsCol {
		return false
	}
	if len(r.GroupBy) != len(o.GroupBy) || len(r.GroupByAliases) != len(o.GroupByAliases) {
		return false
	}
	for i := range r.GroupByAliases {
		if r.GroupByAliases[i] != o.GroupByAliases[i] {
			return false
		}
	}
	for i := range r.GroupBy {
		if !r.GroupBy[i].Equal(o.GroupBy[i]) {
			return false
		}
	}
	if r.Input == nil || o.Input == nil {
		return r.Input == nil && o.Input == nil
	}
	return r.Input.Equal(o.Input)
}
