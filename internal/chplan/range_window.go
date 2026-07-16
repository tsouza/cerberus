package chplan

import "time"

// RangeWindowAnchorColumn is the column name the matrix-shape RangeWindow
// emitters give the per-step anchor timestamp ("anchor_ts"). It is the IR-level
// contract between the emitter (which produces the column) and the chplan
// Projects / API result adapters that reference it — re-projecting it into the
// TimeUnix slot, offset-relabeling it, or grouping by it. chsql's
// RangeWindowAnchorAlias aliases this so every layer names the column once.
const RangeWindowAnchorColumn = "anchor_ts"

// RangeWindow is a PromQL-style range-vector aggregation: for each step
// across [Start, End] (inclusive), compute Func over the rows whose
// timestamp lies within [step-Range, step]. Used to lower expressions like
// `rate(metric[5m])` and `sum_over_time(metric[1h])`.
//
// Input shapes (the emitter discriminates at render time):
//
//   - Row-shape relation (PromQL / LogQL): every row carries the
//     per-sample (TimestampColumn, ValueColumn) pair plus the GroupBy
//     series identity. The emitter (internal/chsql/range_window.go)
//     produces ClickHouse SQL using the windowed-array idiom: GROUP BY
//     series, build a sorted (ts, value) array via groupArray +
//     arraySort, arrayFilter to the per-step window, then apply the
//     function-specific aggregation. Func names the PromQL operator
//     (`rate`, `*_over_time`, …); TimestampColumn / ValueColumn are
//     required.
//
//   - MetricsAggregate input (TraceQL): the underlying relation is a
//     chplan.MetricsAggregate whose Inner is a per-span Scan/Filter
//     tree. Func is ignored — MetricsAggregate.Op carries the
//     per-bucket reducer. The emitter renders a time-bucketed matrix
//     via arrayJoin(range(...)) over [Start, End] spaced by Step,
//     applying the Op-specific CH aggregate per bucket. TimestampColumn
//     is required (it names the per-span Timestamp on Inner);
//     ValueColumn is unused (the metric value is the reduce of Attr).
//     Step must be > 0 in this mode.
type RangeWindow struct {
	Input Node

	// Func is the PromQL range function: "rate", "increase", "delta",
	// "avg_over_time", "sum_over_time", "min_over_time", "max_over_time",
	// "count_over_time", "last_over_time", ...
	Func string

	// Range is the [duration] window from the PromQL source.
	Range time.Duration

	// Step is the evaluation step (the resolution of the produced series).
	// Zero means instant query (a single step at End).
	Step time.Duration

	// OuterRange enables PromQL subquery emission: when non-zero the
	// emitter produces one row per anchor across [End - OuterRange, End]
	// spaced by Step (end-inclusive), rather than the instant single-anchor
	// shape. Set by the subquery lowering for `<expr>[<OuterRange>:<Step>]`.
	// Zero (the default) preserves today's instant semantics.
	//
	// Step must be > 0 whenever OuterRange > 0 — number of anchors is
	// OuterRange/Step + 1.
	OuterRange time.Duration

	// Identity reports whether the range function is the no-op
	// "evaluate the last sample in window" path used by bare-vector
	// subqueries (`up[5m:1m]`). When true, Func is ignored and the
	// emitter renders `if(length(window_vals) > 0,
	// window_vals[length(window_vals)], nan)`. Cleaner than overloading
	// Func with an "identity" sentinel.
	Identity bool

	// Start / End define the eval grid the function is evaluated at.
	// Both zero means the emitter substitutes ClickHouse `now64()` for the
	// query-time anchor, which keeps test fixtures deterministic (the SQL
	// text is the same regardless of wall-clock).
	Start time.Time
	End   time.Time

	// Offset is the PromQL `offset` modifier shifted onto the inner
	// VectorSelector. Subtracted from End at emit time so the window
	// becomes [End - Offset - Range, End - Offset]. Zero means no offset.
	Offset time.Duration

	// StepAlign requests that the matrix anchor grid be snapped so anchors
	// land on absolute-epoch multiples of Step — PromQL subquery
	// inner-sample-grid semantics. Reference Prometheus evaluates a
	// subquery's inner samples at timestamps `interval * ((endTs - offset)
	// / interval)`, i.e. epoch-aligned (phase 0), independent of the outer
	// request's start/step. When true the emitter floors the anchor-grid
	// base to `fromUnixTimestamp64Nano(intDiv(toUnixTimestamp64Nano(End),
	// Step) * Step)` (after offset) so the fan-out lands on phase-0
	// timestamps. The outer query_range eval grid leaves this false — it
	// uses the user-supplied start + k*Step grid (not epoch-aligned).
	StepAlign bool

	// TimestampColumn names the column carrying the per-sample timestamp
	// on Input (typically "TimeUnix" for OTel-CH).
	TimestampColumn string

	// ValueColumn names the column carrying the per-sample float value
	// on Input (typically "Value" for OTel-CH).
	ValueColumn string

	// GroupBy lists the expressions that identify a series for grouping
	// (typically `[ColumnRef("Attributes")]` for OTel-CH, since the map
	// column carries all the labels). May be nil/empty, in which case
	// the emitter does not group — all rows are treated as one series.
	GroupBy []Expr

	// Scalars carries the scalar arguments threaded onto the range
	// function by the lowering layer. Used by `predict_linear(v, t)`
	// (single scalar — predict horizon in seconds) and
	// `holt_winters(v, sf, tf)` (smoothing factor + trend factor).
	// Empty for the simpler range functions (rate / increase /
	// *_over_time / log_rate) that take no extra parameters.
	Scalars []float64

	// ScalarExprs is the computed-scalar sibling of Scalars: when
	// non-empty it carries one Expr per scalar argument (typically a
	// ScalarSubquery built from a `scalar(<vector>)` argument, possibly
	// composed with literals through Binary nodes) and takes precedence
	// over Scalars at emit time. Set by the PromQL lowering for
	// `predict_linear(v[r], scalar(x))` and
	// `quantile_over_time(scalar(x), v[r])` — the shapes whose scalar
	// parameter the reference engine computes per evaluation. Mutually
	// exclusive with Scalars: a lowering populates one or the other.
	ScalarExprs []Expr

	// InstantScanBounded records, as an IR-level property, that this
	// INSTANT (OuterRange == 0) windowed-array leaf RangeWindow has had
	// its scan-prune bound established: the innermost groupArray read is
	// constrained to `TimestampColumn > End - range AND
	// TimestampColumn <= End` (offset-shifted, End-or-now anchored). The
	// predicate text itself is rendered at emit time by the windowed-array
	// emitters (instantWindowScanBoundsFrags / the OverTimeDirect WHERE);
	// this flag is the contract object the optimizer and emit guard verify.
	//
	// Without the bound ClickHouse cannot prune granules: the innermost
	// groupArray materialises the full per-series retention before the
	// post-groupArray arrayFilter discards out-of-window samples.
	//
	// The flag lives on the RangeWindow (not the Scan) because the bound is
	// pushed at the innermost groupArray level — over whatever the windowed
	// Input renders to (a bare Scan, a Filter(Scan), a UnionAll of
	// per-table scans, or another window's per-anchor output) — so it is
	// Input-shape-independent.
	//
	// Established once by AttachInstantScanTimeBounds (and the optimizer's
	// NormalizeScanTimeBound analyzer rule) for the instant windowed-array
	// LEAF shape only (Input is not a MetricsAggregate /
	// MetricsHistogramOverTime / MetricsCompare — those carry their own
	// emit-time bound; the matrix OuterRange>0 paths bound via
	// maybePushInnerScanTimeBounds). false means "not an instant
	// windowed-array leaf, or not yet established". The fail-closed
	// RequireScanTimeBound analyzer rejects an instant windowed-array leaf
	// whose flag is still false, and the emitters fail closed too, turning
	// the recurring unbounded-scan bug class
	// (#1027 / #1048 / #1056 / #1059 / #1080 / #1088 / #1089 / #1098) into
	// an enforced plan-build invariant rather than a per-emitter memory.
	InstantScanBounded bool
}

func (*RangeWindow) planNode() {}

func (r *RangeWindow) Children() []Node { return []Node{r.Input} }

// NumAnchors is the number of subquery anchor points this RangeWindow
// materialises: one row per Step across (End - OuterRange, End], i.e.
// OuterRange/Step + 1. It is the per-series intermediate row count of a
// PromQL subquery grid (the dominant resource axis for a fine-step/long-range
// subquery like `[90d:1s]` = 7.78M). Zero for a non-subquery instant leaf
// (OuterRange == 0) or an unstepped window (Step <= 0). Mirrors the emitter's
// own `OuterRange.Nanoseconds()/stepNS + 1` so the budget gate and the emit
// agree on the count.
func (r *RangeWindow) NumAnchors() int64 {
	if r.OuterRange <= 0 || r.Step <= 0 {
		return 0
	}
	return r.OuterRange.Nanoseconds()/r.Step.Nanoseconds() + 1
}

func (r *RangeWindow) Equal(other Node) bool {
	o, ok := other.(*RangeWindow)
	if !ok {
		return false
	}
	if r.Func != o.Func || r.Range != o.Range || r.Step != o.Step || r.Offset != o.Offset {
		return false
	}
	if r.OuterRange != o.OuterRange || r.Identity != o.Identity {
		return false
	}
	if r.StepAlign != o.StepAlign {
		return false
	}
	if !r.Start.Equal(o.Start) || !r.End.Equal(o.End) {
		return false
	}
	if r.TimestampColumn != o.TimestampColumn || r.ValueColumn != o.ValueColumn {
		return false
	}
	if len(r.GroupBy) != len(o.GroupBy) {
		return false
	}
	for i := range r.GroupBy {
		if !r.GroupBy[i].Equal(o.GroupBy[i]) {
			return false
		}
	}
	if len(r.Scalars) != len(o.Scalars) {
		return false
	}
	for i := range r.Scalars {
		if r.Scalars[i] != o.Scalars[i] {
			return false
		}
	}
	if len(r.ScalarExprs) != len(o.ScalarExprs) {
		return false
	}
	for i := range r.ScalarExprs {
		if !r.ScalarExprs[i].Equal(o.ScalarExprs[i]) {
			return false
		}
	}
	if r.InstantScanBounded != o.InstantScanBounded {
		return false
	}
	return r.Input.Equal(o.Input)
}
