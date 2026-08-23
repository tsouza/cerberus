package chplan //nolint:dupl // shared method bodies already factored into bucket_grid_carrier_fields.go; the residual is the two distinct chplan.Node struct declarations

import "time"

// This file's struct declaration + NumAnchors/Equal still dupl-match
// range_bucket_window_slide.go even after bucket_grid_carrier_fields.go
// extracted their shared method BODIES: the two node types' FIELD LIST is
// still identical, and it must stay that way as two separate chplan.Node
// kinds — see bucket_grid_carrier_fields.go's own doc for why embedding a
// shared struct (which would resolve this at the type level too) is not
// worth breaking every existing keyed struct literal across the tree that
// constructs one of these nodes directly.

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
	return bucketGridCarrierNumAnchors(r.Start, r.End, r.Step)
}

func (r *RangeBucketGridNative) Equal(other Node) bool {
	o, ok := other.(*RangeBucketGridNative)
	if !ok {
		return false
	}
	return bucketGridCarrierFields{
		input: r.Input, start: r.Start, end: r.End, step: r.Step, rng: r.Range, offset: r.Offset,
		groupBy: r.GroupBy, groupByAliases: r.GroupByAliases, anchorAlias: r.AnchorAlias,
		timestampCol: r.TimestampCol, bucketCountsCol: r.BucketCountsCol, explicitBoundsCol: r.ExplicitBoundsCol,
	}.equal(bucketGridCarrierFields{
		input: o.Input, start: o.Start, end: o.End, step: o.Step, rng: o.Range, offset: o.Offset,
		groupBy: o.GroupBy, groupByAliases: o.GroupByAliases, anchorAlias: o.AnchorAlias,
		timestampCol: o.TimestampCol, bucketCountsCol: o.BucketCountsCol, explicitBoundsCol: o.ExplicitBoundsCol,
	})
}
