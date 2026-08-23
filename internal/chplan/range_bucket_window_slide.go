package chplan

import "time"

// RangeBucketWindowSlide is the anchor-injection sibling of
// [RangeBucketFanout] (and the second ClickHouse-native sibling alongside
// [RangeBucketGridNative]) for the SUM-fold classic-histogram idiom:
// `histogram_quantile(phi, sum by(le, ...)(sum_over_time(<bucket>[range])))`
// in range mode, CUMULATIVE temporality.
//
// Why this exists next to [RangeBucketGridNative] rather than replacing it.
// RangeBucketGridNative handles `rate` by unnesting every stored row into
// one row per `le` rung and running the native counter-aware
// timeSeriesRateToGrid aggregate over each rung's own cumulative-over-time
// series — the reset-aware extrapolation `rate` needs. `sum_over_time` needs
// none of that: reference PromQL's sum_over_time treats even a counter
// series as a plain gauge and sums the raw sample values inside the window,
// with no reset repair and no extrapolation
// (internal/promql.histogramWindowFold's default branch is a bare
// `arraySum(values)` per rung). That is exactly what ClickHouse's
// `sumForEach` computes over an Array(Float64) column: an elementwise sum
// across rows, padding shorter arrays with zero. So this node's per-series,
// per-anchor ladder never needs the per-rung unnest RangeBucketGridNative
// pays for — one row per raw sample, not one row per (sample, rung), is
// enough.
//
// The mechanism, confirmed by issue #2408's Task-1 boundary spike. A window
// function whose frame is keyed on a millisecond-encoded timestamp column
// via `RANGE BETWEEN <lookbackMs-1> PRECEDING AND CURRENT ROW`
// ([chsql.RangeBetweenPrecedingAndCurrentRow]) evaluates its aggregate over
// the physical rows preceding and including the CURRENT row's own encoded
// position. An ASOF JOIN of the anchor grid against the nearest preceding
// sample was tried FIRST and proven structurally wrong: the frame then
// anchors at the MATCHED SAMPLE's timestamp, not the query anchor's — wrong
// whenever the two differ (measured non-zero mismatches at every tested
// grid). The corrected, proven-exact mechanism this node's emitter
// implements is anchor injection: UNION ALL one synthetic sentinel row per
// requested anchor into the real row stream (encoded at the anchor's own
// millisecond position, an `is_anchor` marker column distinguishing it),
// run the window aggregate over the merged stream, and keep only the
// sentinel rows — so the frame's aggregate is evaluated AT the anchor
// itself, never at whichever sample happened to precede it. 0/883,385
// mismatches plus 13 hand-placed nanosecond-precision edge cases in the
// spike (issue #2408, comment
// https://github.com/tsouza/cerberus/issues/2408#issuecomment-5383225913).
//
// Output schema, byte-identical to [RangeBucketGridNative]'s and to the
// reshape Project pair the fan-out shape wears (see
// internal/promql.classicBucketWindowReshape):
//
//	(<AnchorAlias>, <GroupByAliases...>, <BucketCountsCol>, <ExplicitBoundsCol>)
//
// Input is the matchers-filtered scan (Scan, or Filter-over-Scan, possibly
// with the `le` restriction Project). It must expose TimestampCol,
// BucketCountsCol, ExplicitBoundsCol and the GroupBy expressions' source
// columns — the identical contract [RangeBucketGridNative] documents.
type RangeBucketWindowSlide struct {
	Input Node

	// Start / End define the eval grid; Step is the grid spacing. Both are
	// pinned (a materialised query_range grid) — this node is never built
	// for an instant evaluation.
	Start time.Time
	End   time.Time
	Step  time.Duration

	// Range is the rate window `[range]`: the per-anchor membership window
	// is `(anchor - Offset - Range, anchor - Offset]`, the same half-open
	// span [RangeBucketFanout.Lookback] / [RangeBucketGridNative.Range]
	// describe. It doubles as the window frame's own width once encoded to
	// milliseconds — see the emitter.
	Range time.Duration

	// Offset is the PromQL `offset` modifier folded onto the membership
	// window only — it never moves the emitted anchor timestamp.
	Offset time.Duration

	// GroupBy / GroupByAliases are the SERIES identity keys. The anchor key
	// is implicit (always prepended under AnchorAlias) and must NOT appear
	// here. Keying on series identity rather than the user's `by/without`
	// labels is what puts the per-series two-sample floor on the axis
	// reference PromQL puts it on (#1629) — mirrors
	// [RangeBucketGridNative.GroupBy] exactly.
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

func (*RangeBucketWindowSlide) planNode() {}

func (r *RangeBucketWindowSlide) Children() []Node { return []Node{r.Input} }

// NumAnchors is the number of grid anchor points this node materialises as
// sentinel rows: one per Step across [Start, End] (end-inclusive), i.e.
// (End-Start)/Step + 1. Mirrors [RangeBucketGridNative.NumAnchors] exactly,
// including the zero-grid defence-in-depth guard — see that method's doc
// for why a caller should never have to also know this node's own
// construction invariants hold.
func (r *RangeBucketWindowSlide) NumAnchors() int64 {
	if r.Start.IsZero() || r.End.IsZero() || r.Step <= 0 {
		return 0
	}
	return r.End.Sub(r.Start).Nanoseconds()/r.Step.Nanoseconds() + 1
}

func (r *RangeBucketWindowSlide) Equal(other Node) bool {
	o, ok := other.(*RangeBucketWindowSlide)
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
