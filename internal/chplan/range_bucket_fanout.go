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
	//
	// Ignored for anchor-grid purposes whenever OuterRange > 0 — see that
	// field's own doc. Start still doubles as the (optional) scan-prune
	// lower bound in that mode: set once a caller widens the plan spine
	// onto a query_range grid (promql.widenSubquerySpine's own
	// *RangeBucketFanout arm), left zero for the single-eval-anchor /
	// pinned subquery shape (no scan bound pushed then, mirroring
	// RangeWindow's identical gating).
	Start time.Time
	End   time.Time
	Step  time.Duration

	// OuterRange enables the SAME independent-subquery-grid mode
	// [RangeWindow.OuterRange] already provides: when non-zero, the anchor
	// grid is derived from (End, OuterRange, Step) — [End-OuterRange, End]
	// spaced by Step, end-inclusive — instead of the (Start, End, Step)
	// grid above. Set by the doubly-nested subquery lowering
	// (`<fn>(<inner-sub>)[<outer-range>:<step>]`, cerberus issue #2726)
	// for a histogram/mixed-shaped `wideInner`, mirroring the plain-float
	// sibling's OuterRange-mode RangeWindow at the identical composition
	// point (internal/promql's lowerSubqueryOverCallSubquery).
	//
	// Zero (the default) preserves today's (Start, End, Step) grid, used
	// by every existing bare-selector / request-grid-anchored caller.
	OuterRange time.Duration

	// StepAlign requests that the OuterRange anchor grid be snapped to
	// absolute-epoch multiples of Step, mirroring [RangeWindow.StepAlign]
	// exactly — PromQL subquery inner-sample-grid semantics. Meaningless
	// (and ignored) when OuterRange is zero.
	StepAlign bool

	// Lookback is the staleness horizon for the per-anchor window
	// `(anchor - Offset - Lookback, anchor - Offset]` — instantLookback
	// (5m) for the bare/value-fn paths, the rate `[range]` for the
	// aggregated paths.
	Lookback time.Duration

	// Offset is the PromQL `offset` modifier folded onto the membership
	// window (shifts the window back by Offset; does NOT move the emitted
	// anchor timestamp). `offset` is a relative shift against each step's
	// eval time, so it rides the fan-out; an absolute `@` pin is the other
	// case entirely and never reaches here — it fixes one window for the
	// whole query, which the histogram lowerings serve by evaluating the
	// instant tree once and broadcasting it across the step grid.
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

	// MinSamples is the number of distinct sample timestamps an anchor's
	// window must hold before the anchor emits a row. It carries the
	// per-function "no sample emitted" rule the collapsed range-vector
	// function owns: a bare selector resolves to at most one sample so one
	// is enough, while the `rate` / `increase` idiom needs two points to
	// span a delta and reference PromQL emits NOTHING at an anchor whose
	// window holds fewer. Values <= 1 impose no filter — an anchor with no
	// sample already produces no fanned row, hence no GROUP BY row.
	MinSamples int

	// AnchorAlias is the output column name for the grid anchor
	// (always "anchor_ts" today).
	AnchorAlias string

	// TimestampCol is the per-sample timestamp column on Input — the
	// argMax tie-break argument and the fan-out distance reference.
	TimestampCol string

	// PeakIndependentOfGrid marks a fan-out whose PEAK intermediate working
	// set does not shrink when the anchor grid is sliced — see
	// AnchorGridDivides, which reports its negation to the solver.
	//
	// It is a per-CONSTRUCTION-SITE flag rather than a property of the node
	// type because the two lowerings that build this node have opposite
	// memory profiles under the same shape. The classic bucket-ladder fold
	// (histogram_quantile over `<name>_bucket`) fits route A comfortably —
	// 2.84 GB measured on an APM-style panel — so slicing it is 23x pure
	// waste. The exponential/native merge does NOT: it is the shape behind 19
	// observed MEMORY_LIMIT_EXCEEDED failures (#2385, cause still
	// unexplained), where slicing is what bounds the memory at all.
	//
	// The zero value is therefore the SAFE one: false means "assume slicing
	// helps", which preserves every existing lowering's behaviour. Only a
	// site that has MEASURED route A to fit sets it true.
	PeakIndependentOfGrid bool
}

func (*RangeBucketFanout) planNode() {}

func (r *RangeBucketFanout) Children() []Node { return []Node{r.Input} }

// NumAnchors is the number of grid anchor points this fan-out materialises:
// one row per Step across [Start, End] (end-inclusive), i.e.
// (End-Start)/Step + 1 — mirrors the chsql emitter's own `span/stepNS + 1`
// (internal/chsql/range_bucket_fanout.go) so the budget gate and the emit
// agree on the count, and mirrors [RangeWindow.NumAnchors]'s role for the
// identical resource axis on this node's own grid.
//
// Zero when Start/End are both unset (the now64() fixture shape, where the
// emitter substitutes a single anchor) or Step <= 0 — there is no
// materialised grid to charge in either case.
//
// This is the per-series intermediate row count [requireSubquerySampleBudget]
// exists to bound (docs/resource-bound-audit): when this node lowers a
// histogram range function whose Start/End/Step come from a PromQL subquery
// grid (`histogram_quantile(...)[OuterRange:Step]` nested inside an outer
// `<reducer>_over_time(...)`), NOTHING else in the plan carries that grid's
// width — the histogram lowering (internal/promql) absorbs the subquery's
// own materialisation into THIS node directly rather than wrapping it in an
// OuterRange-bearing [RangeWindow], unlike a bare-selector subquery. Before
// this method existed, [requireSubquerySampleBudget]'s
// *chplan.RangeWindow-only type switch could not see this axis at all, so a
// histogram-quantile-as-subquery-inner query with a divergent [range:step]
// escaped the same protection its scalar sibling already had.
func (r *RangeBucketFanout) NumAnchors() int64 {
	if r.OuterRange > 0 {
		// Mirrors RangeWindow.NumAnchors' own OuterRange formula exactly,
		// including its choice to ignore StepAlign: the inclusive count is a
		// safe (equal-or-over) upper bound for the budget gate either way,
		// and RangeWindow's own NumAnchors already sets that precedent.
		if r.Step <= 0 {
			return 0
		}
		return r.OuterRange.Nanoseconds()/r.Step.Nanoseconds() + 1
	}
	return numAnchorsFromGrid(r.Start, r.End, r.Step)
}

// InputWindow returns the [start, end) bound this fan-out's OWN Input
// spine must be widened to so every anchor it evaluates across [start, end]
// finds every sample its window needs — the RangeBucketFanout twin of
// [RangeWindow.InputWindow], sharing that method's role as the single
// owner of this arithmetic (promql.widenSubquerySpine and
// chplan.ReanchorRange both call this instead of re-deriving it).
//
// Each anchor reduces the samples in `(anchor-Offset-Lookback,
// anchor-Offset]`, so the union across every anchor in [start, end] needs
// input covering [start-Offset-Lookback, end]. Offset enters with its
// sign, mirroring RangeWindow.InputWindow and RangeLWR's identical
// Offset+Lookback widening.
func (r *RangeBucketFanout) InputWindow(start, end time.Time) (time.Time, time.Time) {
	return start.Add(-r.Offset - r.Lookback), end
}

// numAnchorsFromGrid is the shared end-inclusive anchor-count formula every
// (Start, End, Step) grid node's own NumAnchors delegates to:
// (End-Start)/Step + 1, or 0 when either bound is unset or Step <= 0 (no
// materialised grid to charge). [RangeBucketFanout.NumAnchors],
// [RangeBucketGridNative.NumAnchors], and [RangeLWR.NumAnchors] all shared
// this identical body verbatim before this extraction — see
// RangeBucketFanout.NumAnchors' own doc for why this axis needs its own
// charge in [requireSubquerySampleBudget] rather than relying on an
// ancestor [RangeWindow].
func numAnchorsFromGrid(start, end time.Time, step time.Duration) int64 {
	if start.IsZero() || end.IsZero() || step <= 0 {
		return 0
	}
	return end.Sub(start).Nanoseconds()/step.Nanoseconds() + 1
}

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
	if r.OuterRange != o.OuterRange || r.StepAlign != o.StepAlign {
		return false
	}
	if r.MinSamples != o.MinSamples {
		return false
	}
	if r.AnchorAlias != o.AnchorAlias || r.TimestampCol != o.TimestampCol {
		return false
	}
	// Part of plan identity: it changes the solver's routing verdict (see
	// AnchorGridDivides), so two otherwise-identical fan-outs that disagree
	// here are genuinely different plans.
	if r.PeakIndependentOfGrid != o.PeakIndependentOfGrid {
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
