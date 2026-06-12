package chplan

import "time"

// RangeBucketFanout is the single-pass, bounded sample-side fan-out plan
// node for the array-valued histogram-quantile / histogram-value-function
// range lowerings. It is the array-aggregate sibling of RangeLWR.
//
// Where RangeLWR collapses each (series, anchor) bucket to the hardwired
// scalar `argMax(Value, TimeUnix)`, RangeBucketFanout carries a
// configurable, variant-dependent set of aggregate functions (AggFuncs)
// and an explicit user group-key list (GroupBy) — the histogram range
// path needs `argMax(BucketCounts, TimeUnix)` + `argMax(ExplicitBounds,
// TimeUnix)` (classic bare LWR), `sumForEach(BucketCounts)` + `any(
// ExplicitBounds)` (classic rate/aggregated), or the exp-histogram merge
// reducers / groupArrays (native), and may group by a user `by/without`
// projection rather than just the full Attributes column.
//
// It supersedes the O(rows × N) shape the histogram range lowerings
// previously emitted:
//
//	Aggregate(GroupBy=[anchor_ts, <user-keys>], AggFuncs=<variant aggs>)
//	  Filter(TimeUnix <= anchor_ts AND TimeUnix > anchor_ts - <lookback>)
//	    CrossJoin(StepGrid(Start, End, Step), <Input>)
//
// with the bounded single-pass shape RangeLWR introduced (#804): each
// sample arrayJoins only over the ≤ Lookback/Step + 1 anchors whose
// half-open staleness window `(anchor - Offset - Lookback, anchor -
// Offset]` covers it, then a `GROUP BY (<user-keys>, anchor)` collapses
// each (series, anchor) bucket with the configured AggFuncs. The
// intermediate (sample, anchor) cardinality is `rows × (Lookback/Step)`,
// constant in the grid width N — the linear-in-N blowup is gone.
//
// Output schema (byte-identical to the Aggregate node it replaces):
//
//	(<AnchorAlias>, <GroupByAliases...>, <AggFuncs[i].Alias...>)
//
// — the anchor key first (re-aliased to AnchorAlias), then each user
// group key under its alias, then each aggregate under its alias. The
// wrapping reshape Project + HistogramQuantile{,Native} consume this
// exactly as they consumed the old Aggregate output.
//
// Input is the matchers-filtered scan (Scan, or Filter-over-Scan). It
// must expose the schema columns the AggFuncs read plus TimestampCol
// (the argMax tie-break / fan-out distance column) and the GroupBy
// expressions' source columns.
type RangeBucketFanout struct {
	Input Node

	// Start / End define the eval grid; Step is the grid spacing.
	// N = (End-Start)/Step + 1 anchors, end-inclusive.
	Start time.Time
	End   time.Time
	Step  time.Duration

	// Lookback is the staleness horizon for the per-anchor window
	// `(anchor - Offset - Lookback, anchor - Offset]` — instantLookback
	// (5m) for the bare/value-fn paths, the rate `[range]` for the
	// aggregated paths.
	Lookback time.Duration

	// Offset is the PromQL `offset` modifier folded onto the membership
	// window (shifts the window back by Offset; does NOT move the emitted
	// anchor timestamp). The histogram range lowerings fall back to
	// instant mode under modifiers, so this is zero in practice today; it
	// is carried for parity with RangeLWR.
	Offset time.Duration

	// GroupBy / GroupByAliases are the user group keys (the full
	// Attributes column for the bare paths, the `by/without` projection
	// for the aggregated paths). The anchor key is implicit — it is
	// always prepended under AnchorAlias and must NOT appear here.
	GroupBy        []Expr
	GroupByAliases []string

	// AggFuncs are the per-(series, anchor) collapse aggregates. Each
	// carries its own output Alias (BucketCounts / ExplicitBounds /
	// Scale / …) so downstream consumers read the columns by name.
	AggFuncs []AggFunc

	// AnchorAlias is the output column name for the grid anchor
	// (always "anchor_ts" today).
	AnchorAlias string

	// TimestampCol is the per-sample timestamp column on Input — the
	// argMax tie-break argument and the fan-out distance reference.
	TimestampCol string
}

func (*RangeBucketFanout) planNode() {}

func (r *RangeBucketFanout) Children() []Node { return []Node{r.Input} }

func (r *RangeBucketFanout) Equal(other Node) bool {
	o, ok := other.(*RangeBucketFanout)
	if !ok {
		return false
	}
	if !r.Start.Equal(o.Start) || !r.End.Equal(o.End) {
		return false
	}
	if r.Step != o.Step || r.Lookback != o.Lookback || r.Offset != o.Offset {
		return false
	}
	if r.AnchorAlias != o.AnchorAlias || r.TimestampCol != o.TimestampCol {
		return false
	}
	if len(r.GroupBy) != len(o.GroupBy) || len(r.AggFuncs) != len(o.AggFuncs) {
		return false
	}
	if len(r.GroupByAliases) != len(o.GroupByAliases) {
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
	for i := range r.AggFuncs {
		if !r.AggFuncs[i].Equal(o.AggFuncs[i]) {
			return false
		}
	}
	if r.Input == nil || o.Input == nil {
		return r.Input == nil && o.Input == nil
	}
	return r.Input.Equal(o.Input)
}
