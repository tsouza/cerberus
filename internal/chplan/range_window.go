package chplan

import "time"

// RangeWindowAnchorColumn is the column name the matrix-shape RangeWindow
// emitters give the per-step anchor timestamp ("anchor_ts"). It is the IR-level
// contract between the emitter (which produces the column) and the chplan
// Projects / API result adapters that reference it — re-projecting it into the
// TimeUnix slot, offset-relabeling it, or grouping by it. chsql's
// RangeWindowAnchorAlias aliases this so every layer names the column once.
const RangeWindowAnchorColumn = "anchor_ts"

// OffsetReanchoredAnchorExpr builds `anchor_ts + toIntervalNanosecond(offset)`
// — the IR expression that re-anchors a matrix window's offset-SHIFTED
// anchor_ts back onto the unshifted request grid. The fan-out matrix emitters
// keep anchor_ts offset-shifted (chsql gridAnchorFrag), but a reducing window's
// result is reported on the unshifted grid, so the consumers that read the bare
// anchor_ts — the Prom sample-projection adapter (wrapWithSampleProjection) and
// the last/first_over_time preserve-__name__ lowering wrapper — add the offset
// back with this. Offset enters with its sign (a negative/forward offset
// subtracts). Shared so the two call sites cannot drift.
func OffsetReanchoredAnchorExpr(offset time.Duration) Expr {
	return &Binary{
		Op:   OpAdd,
		Left: &ColumnRef{Name: RangeWindowAnchorColumn},
		Right: &FuncCall{
			Name: "toIntervalNanosecond",
			Args: []Expr{&LitInt{V: offset.Nanoseconds()}},
		},
	}
}

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

	// PredictLinearSlopeColumn asks the predict_linear emitter to project
	// the fitted slope beside its value. The pinned-range lowering uses that
	// slope after broadcasting the fixed regression across the request grid,
	// because Prometheus advances the forecast target at every eval step even
	// though an absolute @ modifier keeps the input window fixed.
	PredictLinearSlopeColumn string

	// TemporalityColumn names the column carrying the OTel
	// AggregationTemporality enum for the scanned series (typically
	// "AggregationTemporality" for an OTel-CH Sum / Histogram table).
	// Only "rate" / "increase" consult it today.
	//
	// Empty (the default) means "not applicable to this Input" — a
	// Gauge-table scan carries no such column, neither does the
	// Scan.UnionTables cross-table read (which deliberately drops the
	// Sum-only column from its projection so the union's column list
	// still matches) — and the emitter falls back to the historical,
	// pre-#1628 behaviour of applying Prometheus's counter-reset rule
	// unconditionally.
	//
	// When set, the emitter reads `any(<TemporalityColumn>)` once per
	// series-window group (a single OTel time series has ONE
	// temporality for its lifetime, so any() over the window's rows is
	// exact, not a lossy pick) and branches at RUNTIME between the
	// DELTA reading (schema.AggregationTemporalityDelta — sum the
	// window's raw, already-exclusive samples) and the CUMULATIVE
	// reading (anything else — the counter-reset-aware delta). See
	// chsql.CounterOrDeltaSum, the shared primitive both the ordinary
	// counter path and the classic-histogram bucket fold
	// (internal/promql's counterIncreaseFold) apply the same branch
	// through, and issue #1628.
	TemporalityColumn string

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

	// Variants carries the FUSED multi-arm shape: when it holds more than
	// one entry, this single window evaluates every entry's range function
	// over ONE pass of Input and unpivots the results into one row per
	// (series, anchor, variant), stamping the entry's Label into
	// VariantColumn. Func / ValueColumn describe no arm in that mode and
	// are ignored by the emitter; each arm names its own pair.
	//
	// The construct that produces it is LogQL's `variants(m0, m1, …) of
	// ({selector}[r])`, whose arms share a selector by definition — see
	// internal/logql/variants.go. Without fusion each arm lowers to its own
	// Scan/Filter subtree and ClickHouse reads the log table once per arm:
	// a two-arm query measured 10.0M read_rows / 794 MiB against the fused
	// shape's 5.0M / 510 MiB on the same 5M-row table, and a three-arm one
	// 15.0M / 1.27 GiB against the same 5.0M / 510 MiB (issue #1501). A CTE
	// cannot express the saving — ClickHouse inlines a multiply-referenced
	// CTE and re-reads per reference — so the single pass has to be a
	// single aggregation, which is what this field asks the emitter for.
	//
	// Empty (the default) is the ordinary single-arm window. A one-entry
	// Variants is rejected rather than fused: a lone arm has nothing to
	// share a pass with, so it stays on the ordinary path.
	Variants []RangeWindowVariant

	// VariantColumn names the output column carrying the per-row arm Label
	// in the fused shape ("__variant__" for LogQL). Required when Variants
	// is populated, unused otherwise.
	VariantColumn string

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

// RangeWindowVariant is one arm of a fused multi-arm RangeWindow: the range
// function to evaluate, the per-sample value column it reads, and the label
// stamped into RangeWindow.VariantColumn on the rows it produces.
//
// Every arm reduces the SAME window membership — the arms differ only in
// which per-row value column they read and which reducer they apply — so the
// emitter builds one sorted (timestamp, value₀, …, valueₙ₋₁) array per
// (series, anchor) and reduces it once per arm.
type RangeWindowVariant struct {
	// Func is the range function, from the same vocabulary as
	// RangeWindow.Func.
	Func string

	// ValueColumn names this arm's per-sample value column on Input.
	// Distinct per arm — that is the whole point of the fused shape.
	ValueColumn string

	// Label is the value stamped into RangeWindow.VariantColumn for the
	// rows this arm produces (LogQL uses the arm's zero-based index
	// rendered as a decimal string).
	Label string
}

// Equal reports structural equality of two fused-window arms.
func (v RangeWindowVariant) Equal(o RangeWindowVariant) bool {
	return v.Func == o.Func && v.ValueColumn == o.ValueColumn && v.Label == o.Label
}

// FusibleWindowFunc reports whether fn is a range function the FUSED
// multi-arm window can reduce out of the shared per-window sample array.
//
// It is the contract between the two sides of the fused shape: a lowering
// consults it BEFORE building a RangeWindow.Variants, so an arm the emitter
// could not render never reaches the emitter as an error — the query stays on
// the correct-but-slower per-arm path instead. The set is exactly the
// *_over_time family whose value is a reduction over the window's values
// array; the functions with their own bespoke emission (rate / increase /
// delta and the pairwise / extrapolating forms, quantile_over_time's sorted
// interpolation, the ts_of_* timestamp pickers) are not members, because they
// read the (timestamp, value) PAIRS rather than a values array.
//
// TestFusibleWindowFuncMatchesEmitter pins this list against the emitter's
// own dispatch in both directions, so neither side can grow a function the
// other does not know about.
func FusibleWindowFunc(fn string) bool {
	switch fn {
	case "sum_over_time",
		"avg_over_time",
		"min_over_time",
		"max_over_time",
		"count_over_time",
		"present_over_time",
		"first_over_time",
		"last_over_time",
		"mad_over_time",
		"stddev_over_time",
		"stdvar_over_time":
		return true
	}
	return false
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

// InputWindow returns the [start, end) bound r's OWN Input spine must be
// widened to so every anchor r evaluates across [start, end] finds every
// sample its window needs. This is the SINGLE owner of the "how far back
// does a RangeWindow's input need to reach" arithmetic: every re-anchoring /
// widening / grid-prediction pass that walks below a RangeWindow (chplan's
// own ReanchorRange, promql's widenSubquerySpine, and the solver planner's
// grid-prediction walk) calls this instead of re-deriving the formula, so
// the three stay consistent by construction — see issue #1464, where the
// RangeWindow arm of ReanchorRange re-derived `start.Add(-r.Range)` locally
// and silently dropped the Offset term the RangeLWR arm got right.
//
// A matrix-shape window (Step > 0) reduces the samples in
// `(anchor-Offset-Range, anchor-Offset]` at each anchor (the package doc
// above documents the scanned span as [End-Offset-Range, End-Offset]), so
// the union across every anchor in [start, end] needs input covering
// [start-Offset-Range, end]. Offset enters with its sign, mirroring the
// emitter's own maybePushInnerScanTimeBounds (internal/chsql/range_window.go)
// and RangeLWR's Offset+Lookback widening. An instant-shape window (Step <=
// 0) resolves a single anchor itself and is never walked below by these
// passes; InputWindow returns (start, end) unchanged for it so a caller that
// invokes it unconditionally is still safe.
func (r *RangeWindow) InputWindow(start, end time.Time) (time.Time, time.Time) {
	if r.Step <= 0 {
		return start, end
	}
	return start.Add(-r.Offset - r.Range), end
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
	if r.TimestampColumn != o.TimestampColumn || r.ValueColumn != o.ValueColumn ||
		r.PredictLinearSlopeColumn != o.PredictLinearSlopeColumn {
		return false
	}
	if r.TemporalityColumn != o.TemporalityColumn {
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
	if r.VariantColumn != o.VariantColumn || len(r.Variants) != len(o.Variants) {
		return false
	}
	for i := range r.Variants {
		if !r.Variants[i].Equal(o.Variants[i]) {
			return false
		}
	}
	return r.Input.Equal(o.Input)
}
