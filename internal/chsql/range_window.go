package chsql

import (
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// emitMetricsAggregate renders a chplan.MetricsAggregate as a single-row
// aggregate when it sits at the root of the plan tree (no wrapping
// RangeWindow). The SQL shape mirrors chplan.Aggregate so the TraceQL
// instant-metric fixtures stay byte-identical across the IR change
// from bare Aggregate → MetricsAggregate.
//
// When a RangeWindow wraps a MetricsAggregate the matrix path is taken
// instead (see emitRangeWindowMetrics).
//
// Multi-phi quantile_over_time (len(m.Quantiles) > 1) is the only shape
// that needs a wrapping outer SELECT: the inner aggregator returns a
// single Array(Float64) column, and the outer SELECT arrayJoins it
// against a parallel phi-string array to fan out one row per phi value
// tagged with the synthetic `__phi__` label.
func (e *emitter) emitMetricsAggregate(m *chplan.MetricsAggregate) error {
	name, params, args, err := metricsAggregateCH(m)
	if err != nil {
		return err
	}
	// Pre-flight expressions so chplan errors surface synchronously
	// (mirrors emitAggregate's pre-flight loop).
	for _, g := range m.GroupBy {
		if err := (&Builder{}).Expr(g); err != nil {
			return err
		}
	}
	for _, p := range params {
		if err := (&Builder{}).Expr(p); err != nil {
			return err
		}
	}
	for _, a := range args {
		if err := (&Builder{}).Expr(a); err != nil {
			return err
		}
	}
	if m.Inner == nil {
		return fmt.Errorf("%w: MetricsAggregate.Inner is nil", ErrUnsupported)
	}

	sub, err := e.subqueryFrag(m.Inner)
	if err != nil {
		return err
	}

	multi := m.Op == chplan.MetricsOpQuantileOverTime && len(m.Quantiles) > 1

	sb := NewQuery().From(sub)
	// For the multi-phi path the outer SELECTs need to reference each
	// group column by a stable alias. Use outerGroupAliases (which
	// falls back to "g0", "g1", ... for un-aliased groups) so the
	// outer SELECT-list can pluck the values regardless of whether
	// the source GroupByAliases was set.
	multiGroupAliases := outerGroupAliases(m.GroupBy, m.GroupByAliases)
	for i, g := range m.GroupBy {
		expr := g
		var alias string
		if multi {
			alias = multiGroupAliases[i]
		} else if i < len(m.GroupByAliases) {
			alias = m.GroupByAliases[i]
		}
		sb.SelectAs(func(b *Builder) { _ = b.Expr(expr) }, alias)
	}
	if multi {
		// Inner SELECT: project the `quantiles(p1, p2, ...)(Attr)`
		// Array(Float64) under an internal alias; the outer SELECT
		// fans it out via arrayJoin + tupleElement.
		af := chplan.AggFunc{Name: name, Params: params, Args: args, Alias: ""}
		sb.Select(RawAs(aggFuncFrag(af), "qs_array"))
	} else {
		af := chplan.AggFunc{Name: name, Params: params, Args: args, Alias: m.ValueAlias}
		sb.Select(aggFuncFrag(af))
	}
	// An empty GroupBy appends no keys (GroupBy is a no-op on an empty
	// slice), so no length guard is needed.
	groupFrags := make([]Frag, 0, len(m.GroupBy))
	for _, g := range m.GroupBy {
		expr := g
		groupFrags = append(groupFrags, func(b *Builder) { _ = b.Expr(expr) })
	}
	sb.GroupBy(groupFrags...)

	if !multi {
		e.emitSelect(sb)
		return nil
	}

	// Multi-phi wrap layer 1: arrayJoin the per-(group) quantiles
	// array into one row per phi, tagged with a (phi, value) tuple
	// under the `phi_val` alias.
	outer := NewQuery().From(sb.Frag())
	for _, alias := range multiGroupAliases {
		a := alias
		outer.Select(func(b *Builder) { b.Ident(a) })
	}
	outer.Select(RawAs(metricsMultiQuantileFanoutFrag(m.Quantiles, "qs_array"), "phi_val"))

	// Multi-phi wrap layer 2: split phi_val into the synthetic
	// `__phi__` label + the Float64 `Value` column.
	final := NewQuery().From(outer.Frag())
	for _, alias := range multiGroupAliases {
		a := alias
		final.Select(func(b *Builder) { b.Ident(a) })
	}
	final.Select(As(Call("tupleElement", BareIdent("phi_val"), InlineLit(int64(1))), metricsMultiQuantilePhiLabel))
	final.Select(As(Call("tupleElement", BareIdent("phi_val"), InlineLit(int64(2))), m.ValueAlias))
	e.emitSelect(final)
	return nil
}

// metricsAggregateCH maps a MetricsAggregate.Op to the CH aggregate
// function name + parameter list + argument list. Centralises the
// per-Op switch so both the bare-emission path and the matrix path
// agree on the per-bucket reducer.
//
// rate / count_over_time → `count(1)` (the rate-specific per-bucket
// division by seconds lives on the matrix-path emitter, not here).
// *_over_time(attr) → the matching CH aggregate over Attr.
// quantile_over_time(attr, q) → `quantile(q)(Attr)` for the single-phi
// case; `quantiles(q1, q2, ...)(Attr)` (returns Array(Float64)) for the
// multi-phi case — the per-phi fanout into individual output series
// happens in the wrapping emitter (emitMetricsAggregate /
// emitRangeWindowMetrics), not here.
func metricsAggregateCH(m *chplan.MetricsAggregate) (
	name string,
	params []chplan.Expr,
	args []chplan.Expr,
	err error,
) {
	switch m.Op {
	case chplan.MetricsOpRate, chplan.MetricsOpCountOverTime:
		return "count", nil, []chplan.Expr{&chplan.LitInt{V: 1}}, nil
	case chplan.MetricsOpSumOverTime:
		if m.Attr == nil {
			return "", nil, nil, fmt.Errorf("%w: %s requires Attr", ErrUnsupported, m.Op)
		}
		return "sum", nil, []chplan.Expr{m.Attr}, nil
	case chplan.MetricsOpAvgOverTime:
		if m.Attr == nil {
			return "", nil, nil, fmt.Errorf("%w: %s requires Attr", ErrUnsupported, m.Op)
		}
		return "avg", nil, []chplan.Expr{m.Attr}, nil
	case chplan.MetricsOpMinOverTime:
		if m.Attr == nil {
			return "", nil, nil, fmt.Errorf("%w: %s requires Attr", ErrUnsupported, m.Op)
		}
		return "min", nil, []chplan.Expr{m.Attr}, nil
	case chplan.MetricsOpMaxOverTime:
		if m.Attr == nil {
			return "", nil, nil, fmt.Errorf("%w: %s requires Attr", ErrUnsupported, m.Op)
		}
		return "max", nil, []chplan.Expr{m.Attr}, nil
	case chplan.MetricsOpQuantileOverTime:
		if m.Attr == nil {
			return "", nil, nil, fmt.Errorf("%w: %s requires Attr", ErrUnsupported, m.Op)
		}
		if len(m.Quantiles) == 0 {
			return "", nil, nil, fmt.Errorf("%w: MetricsAggregate quantile requires at least 1 quantile", ErrUnsupported)
		}
		if len(m.Quantiles) == 1 {
			return "quantile", []chplan.Expr{&chplan.LitFloat{V: m.Quantiles[0]}}, []chplan.Expr{m.Attr}, nil
		}
		// Multi-phi: emit `quantiles(p1, p2, ...)(Attr)` which returns
		// an Array(Float64) of size N. The wrapping emitter (bare or
		// matrix) is responsible for the per-phi fanout into N output
		// rows tagged with the synthetic `__phi__` label.
		ps := make([]chplan.Expr, len(m.Quantiles))
		for i, q := range m.Quantiles {
			ps[i] = &chplan.LitFloat{V: q}
		}
		return "quantiles", ps, []chplan.Expr{m.Attr}, nil
	}
	return "", nil, nil, fmt.Errorf("%w: MetricsAggregate op %s", ErrUnsupported, m.Op)
}

// metricsMultiQuantilePhiLabel is the SELECT-list alias for the
// synthetic per-phi label projected by the multi-quantile fanout.
// Each output series of `quantile_over_time(attr, p1, p2, ...)` carries
// one row per phi value tagged with this label, with the value formatted
// as a decimal string (no trailing zeros) — "0.5" reads "0.5" (not
// "0.500000"); aligned with Tempo upstream's per-quantile label
// production in pkg/traceql/engine_metrics.go.
const metricsMultiQuantilePhiLabel = "__phi__"

// RangeWindowAnchorAlias is the SELECT-list alias the matrix-shape
// RangeWindow emitters give the per-step anchor timestamp column
// ("anchor_ts" in every emitted matrix subquery above). Exported so the
// Tempo API layer can stamp chplan.MetricsSecondStage.PartitionBy with
// the exact column the inner SQL exposes — `LIMIT K BY anchor_ts` is
// what turns a global topk into Tempo's per-anchor topk semantics.
const RangeWindowAnchorAlias = "anchor_ts"

// metricsMultiQuantileFanoutFrag returns a Frag rendering the per-(group,
// anchor) fanout from a `quantiles(p1, p2, ...)(...) AS qs_array` column
// into N (phi, value) tuples via arrayJoin + arrayMap over a parallel
// `[p1, p2, ...]` string-literal array. The result is intended to be
// aliased (typically as `phi_val`) and then split via tupleElement(...)
// in an outer SELECT:
//
//	arrayJoin(arrayMap((phi, q) -> (phi, toFloat64(q)),
//	                   ['p1', 'p2', ...],
//	                   <qs_col>))
//
// The phi values are emitted as inline string literals (formatted via
// formatFloat → strconv.FormatFloat('f', -1, 64)) so the resulting
// `__phi__` label reads "0.5" / "0.95" / "0.99" — query-shape constants,
// not user data; emitting inline keeps both the SQL stream and the
// per-phi label string stable regardless of driver float formatting.
func metricsMultiQuantileFanoutFrag(qs []float64, qsCol string) Frag {
	phiElems := make([]Frag, len(qs))
	for i, q := range qs {
		phiStr := formatFloat(q)
		phiElems[i] = InlineLit(phiStr)
	}
	body := Tuple(BareIdent("phi"), Call("toFloat64", BareIdent("q")))
	return Call(
		"arrayJoin",
		Call(
			"arrayMap",
			Lambda2("phi", "q", body),
			Array(phiElems...),
			BareIdent(qsCol),
		),
	)
}

// emitRangeWindow lowers a chplan.RangeWindow to ClickHouse SQL using the
// windowed-array idiom inspired by promshim-clickhouse:
//
//  1. GROUP BY the series-identity columns; build (ts, value) tuples per
//     series via groupArray + arraySort.
//  2. arrayFilter to the window [end-range, end].
//  3. Apply the function-specific aggregation on the windowed values:
//     - rate / increase: arrayPopBack + arrayPopFront pair-up with
//     `if(c < p, c, c - p)` to repair counter resets, arraySum to total.
//     - *_over_time: straight array aggregation (arrayAvg, arraySum, ...).
//
// The emitter substitutes literal timestamps for r.End inline. Zero
// values fall back to ClickHouse's `now64(9)` so fixtures stay
// deterministic and runtime queries still resolve to the current eval
// time.
//
// When r.OuterRange > 0 emission switches to the matrix shape: each
// input row arrayJoins across only the anchors (on the
// [End-OuterRange, End] grid spaced by Step, end-inclusive) whose
// window contains its timestamp, a GROUP BY (series, anchor) rebuilds
// the per-window array, and the outer SELECT projects the anchor
// timestamp alongside the per-anchor value (see
// sampleAnchorFanoutFrag). Used by PromQL query_range + subqueries.
//
// When r.Identity is true, Func is ignored and the per-window value is
// the last sample in the window — used by bare-vector subqueries like
// `up[5m:1m]`.
func (e *emitter) emitRangeWindow(r *chplan.RangeWindow) error {
	if m, ok := r.Input.(*chplan.MetricsAggregate); ok {
		return e.emitRangeWindowMetrics(r, m)
	}
	if h, ok := r.Input.(*chplan.MetricsHistogramOverTime); ok {
		return e.emitRangeWindowHistogram(r, h)
	}
	if c, ok := r.Input.(*chplan.MetricsCompare); ok {
		return e.emitRangeWindowCompare(r, c)
	}
	if r.Identity {
		return e.emitRangeWindowIdentity(r)
	}
	switch r.Func {
	case "rate":
		return e.emitRangeWindowRate(r)
	case "irate":
		return e.emitRangeWindowIRate(r)
	case "increase":
		return e.emitRangeWindowIncrease(r)
	case "delta":
		return e.emitRangeWindowDelta(r)
	case "idelta":
		return e.emitRangeWindowIDelta(r)
	case "sum_over_time", "avg_over_time", "min_over_time", "max_over_time", "count_over_time", "first_over_time", "last_over_time", "stddev_over_time", "stdvar_over_time", "present_over_time", "mad_over_time":
		return e.emitRangeWindowOverTime(r)
	case "ts_of_first_over_time", "ts_of_last_over_time", "ts_of_max_over_time", "ts_of_min_over_time":
		return e.emitRangeWindowTsOfOverTime(r)
	case "quantile_over_time":
		return e.emitRangeWindowQuantileOverTime(r)
	case "log_rate":
		return e.emitRangeWindowLogRate(r)
	case "predict_linear":
		return e.emitRangeWindowPredictLinear(r)
	case "holt_winters":
		return e.emitRangeWindowHoltWinters(r)
	case "deriv":
		return e.emitRangeWindowDeriv(r)
	case "resets":
		return e.emitRangeWindowResets(r)
	case "changes":
		return e.emitRangeWindowChanges(r)
	default:
		return fmt.Errorf("%w: range function %q", ErrUnsupported, r.Func)
	}
}

// emitRangeWindowPredictLinear emits SQL for `predict_linear(v[range], t)`.
//
// The samples in the window become two parallel arrays: `xs` (the
// per-sample offset from the anchor, in seconds; numbers grow more
// negative as you go further back in time) and `ys` (the values).
// ClickHouse's `simpleLinearRegression(x, y)` returns the
// `(slope, intercept)` tuple of the least-squares fit. The predicted
// value at horizon `t` seconds past the anchor is therefore
// `intercept + slope * t`.
//
// PromQL semantics: < 2 samples in the window → drop the series (Prom
// emits NaN; we mirror that with `nan`).
//
// The `t` scalar binds as a placeholder argument; range_seconds is
// only used for the x-axis scale.
func (e *emitter) emitRangeWindowPredictLinear(r *chplan.RangeWindow) error {
	// The horizon t arrives either as a literal (Scalars) or as a
	// computed expression (ScalarExprs — `predict_linear(v[r],
	// scalar(x))`; a scalar subquery CH folds to a constant). NaN t
	// propagates NaN through `intercept + slope * t`, matching Prom.
	var writeT Frag
	switch {
	case len(r.ScalarExprs) == 1:
		tExpr := r.ScalarExprs[0]
		if err := (&Builder{}).Expr(tExpr); err != nil {
			return err
		}
		writeT = func(b *Builder) { _ = b.Expr(tExpr) }
	case len(r.Scalars) == 1:
		t := r.Scalars[0]
		writeT = func(b *Builder) { b.Arg(t) }
	default:
		return fmt.Errorf("%w: predict_linear requires 1 scalar (t), got %d literals + %d exprs",
			ErrUnsupported, len(r.Scalars), len(r.ScalarExprs))
	}
	// In matrix mode each row carries its own anchor_ts; the anchor
	// Frag the factory receives below renders the per-row anchor so the
	// per-sample x-offset (dateDiff('second', anchor, sample_ts)) is
	// computed against the anchor of THIS row, not r.End.
	writer := func(anchor Frag) Frag {
		// arrayMap to derive xs (seconds from anchor) and ys (values).
		// window_pairs is Array(Tuple(DateTime64(9), Float64)).
		//
		// CH's `simpleLinearRegression(x, y)` is an aggregate — it
		// rejects raw arrays at the call site (ILLEGAL_TYPE_OF_ARGUMENT).
		// `arrayReduce('simpleLinearRegression', xs, ys)` is the idiom
		// for applying an aggregate to parallel array columns
		// row-by-row, matching the per-series shape the window-array
		// path produces. Mirrors the stddev_over_time / quantile_over_time
		// emit paths in this file.
		slr := windowPairsSLRFrag(anchor)
		intercept := Call("tupleElement", slr, InlineLit(int64(2)))
		slope := Call("tupleElement", slr, InlineLit(int64(1)))
		// intercept + slope * t
		predict := Add(intercept, Mul(slope, writeT))
		return Call(
			"if",
			Gt(Call("length", BareIdent("window_pairs")), InlineLit(int64(1))),
			predict,
			BareIdent("nan"),
		)
	}
	return e.emitWindowedArrayPairsAnchored(r, writer, 2)
}

// emitRangeWindowHoltWinters emits SQL for `holt_winters(v[range], sf, tf)`.
//
// Holt-Winters double-exponential smoothing applies the recurrence:
//
//	s[0] = y[0]
//	b[0] = y[1] - y[0]
//	s[i] = sf*y[i] + (1-sf)*(s[i-1] + b[i-1])
//	b[i] = tf*(s[i] - s[i-1]) + (1-tf)*b[i-1]
//	result = s[n-1]
//
// We encode this as an arrayFold over the window. CH's
// `arrayFold(lambda(acc, x), arr, initial_acc)` carries a Tuple(s, b)
// accumulator from the first element to the last; the first two
// samples seed the accumulator and the third onward applies the
// recurrence.
//
// PromQL behaviour: < 2 samples → NaN.
func (e *emitter) emitRangeWindowHoltWinters(r *chplan.RangeWindow) error {
	if len(r.Scalars) != 2 {
		return fmt.Errorf("%w: holt_winters requires 2 scalars (sf, tf), got %d", ErrUnsupported, len(r.Scalars))
	}
	sf := r.Scalars[0]
	tf := r.Scalars[1]
	return e.emitWindowedArray(r, holtWintersValueFrag(sf, tf), 2)
}

// holtWintersValueExpr renders the per-window Holt-Winters value
// expression. Operates on `window_vals` (Array(Float64)) and uses
// `arrayFold` to accumulate the (s, b) tuple. The fold's lambda treats
// the first two iterations specially via an `index`-tracking trick:
// the initial accumulator carries `s = ys[1]` and `b = ys[1] - ys[0]`,
// matching Prometheus's seeding; subsequent iterations apply the
// recurrence.
//
// The expression returns NaN when the window has < 2 samples (Prom
// emits NaN there).
func holtWintersValueFrag(sf, tf float64) Frag {
	// We seed with the first two samples, then fold over the slice
	// `window_vals[3:]` applying the recurrence. CH's arrayFold takes
	// (lambda, array, initialAcc) and the lambda is (acc, elem).
	//
	// The smoothing constants ride InlineLit so they stay inline
	// query-shape literals (no `?` binding) and format identically to the
	// pinned goldens.
	sfL := InlineLit(sf)
	oneMinusSf := InlineLit(1 - sf)
	tfL := InlineLit(tf)
	oneMinusTf := InlineLit(1 - tf)
	accS := Call("tupleElement", BareIdent("acc"), InlineLit(int64(1)))
	accB := Call("tupleElement", BareIdent("acc"), InlineLit(int64(2)))
	// new_s = sf*x + (1-sf)*(acc.s + acc.b). A factory because it appears
	// both as a tuple element and inside new_b.
	newS := func() Frag {
		return Add(
			Mul(sfL, BareIdent("x")),
			Mul(oneMinusSf, Paren(Add(accS, accB))),
		)
	}
	// new_b = tf*(new_s - acc.s) + (1-tf)*acc.b
	newB := Add(
		Mul(tfL, Paren(Sub(newS(), accS))),
		Mul(oneMinusTf, accB),
	)
	fold := Call(
		"arrayFold",
		Lambda2("acc", "x", Tuple(newS(), newB)),
		Call("arraySlice", BareIdent("window_vals"), InlineLit(int64(3))),
		Tuple(
			Subscript(BareIdent("window_vals"), InlineLit(int64(2))),
			Sub(Subscript(BareIdent("window_vals"), InlineLit(int64(2))),
				Subscript(BareIdent("window_vals"), InlineLit(int64(1)))),
		),
	)
	return Call(
		"if",
		Gt(Call("length", BareIdent("window_vals")), InlineLit(int64(1))),
		Call("tupleElement", fold, InlineLit(int64(1))),
		BareIdent("nan"),
	)
}

// emitWindowedArrayPairs is a variant of emitWindowedArray for callers
// that need `window_pairs` (Array(Tuple(ts, value))) rather than just
// `window_vals` — e.g. `predict_linear` needs the per-sample timestamps
// to compute x-axis offsets, not just the values.
//
// Generates the same Inner/Inner-middle layers as emitWindowedArray
// but skips the middle layer that derives `window_vals` /
// `counter_delta`; the outer SELECT can directly reference
// `window_pairs`.
//
// minWindowSize controls the PromQL "drop empty windows" semantics:
// when > 0, the outer SELECT adds `WHERE length(window_pairs) >= N`
// so series whose window holds fewer than N samples are dropped from
// the result (matches Prom's funcRate / funcIrate / funcPredictLinear,
// which return no sample for those windows). 0 disables the filter
// (used by LogQL log_rate, which emits 0 for empty windows).
//
// When r.OuterRange > 0, emission switches to the matrix path: each
// series emits one row per anchor (across [End-OuterRange, End] spaced
// by Step, end-inclusive) whose window holds enough samples, built via
// the sample-side fanout + regroup (see emitWindowedArrayPairsMatrix).
// The outer SELECT additionally projects the anchor timestamp as
// `anchor_ts`. The value-writer is invoked with the matrix anchor
// (`anchor_ts`) so anchor-relative expressions (deriv /
// predict_linear) compute per-anchor results rather than re-anchoring
// every row at r.End.
func (e *emitter) emitWindowedArrayPairs(r *chplan.RangeWindow, valueWriter Frag, minWindowSize int) error {
	// Anchor-free callers pass a verbatim Frag — the factory ignores
	// its anchor argument and returns it unchanged. The factory form
	// below threads the anchor into anchor-aware writers (deriv /
	// predict_linear) without duplicating the dispatch.
	return e.emitWindowedArrayPairsAnchored(r, func(_ Frag) Frag { return valueWriter }, minWindowSize)
}

// emitWindowedArrayPairsAnchored is the anchor-aware variant of
// emitWindowedArrayPairs. The valueWriter is built lazily from the
// current anchor Frag — `r.End` in instant mode, `anchor_ts` (from the
// arrayJoin fanout) in matrix mode — so emitters whose per-window value
// depends on the eval anchor (deriv, predict_linear) emit one row per
// anchor with the correct per-anchor anchor, not a single row pinned
// to r.End.
//
// Callers whose value expression doesn't reference the anchor (irate)
// route through emitWindowedArrayPairs and the factory returns the
// pre-built writer unchanged.
func (e *emitter) emitWindowedArrayPairsAnchored(r *chplan.RangeWindow, valueWriterFor func(anchor Frag) Frag, minWindowSize int) error {
	if r.TimestampColumn == "" {
		return fmt.Errorf("%w: RangeWindow.TimestampColumn unset", ErrUnsupported)
	}
	if r.ValueColumn == "" {
		return fmt.Errorf("%w: RangeWindow.ValueColumn unset", ErrUnsupported)
	}
	if r.OuterRange > 0 {
		if r.Step <= 0 {
			return fmt.Errorf("%w: RangeWindow.OuterRange > 0 requires Step > 0", ErrUnsupported)
		}
		return e.emitWindowedArrayPairsMatrix(r, valueWriterFor, minWindowSize)
	}

	end := endExprFrag(r)
	rangeNS := r.Range.Nanoseconds()
	groupFrags, err := e.collectGroupByFrags(r.GroupBy)
	if err != nil {
		return err
	}

	// Innermost SELECT — groupArray of (ts, value), sorted.
	innermost := NewQuery()
	for _, g := range groupFrags {
		innermost.Select(g)
	}
	innermost.Select(RawAs(groupArrayPairFrag(r.TimestampColumn, r.ValueColumn), "series_array"))
	innerSub, err := e.subqueryFrag(r.Input)
	if err != nil {
		return err
	}
	innermost.From(innerSub)
	// GroupBy is a no-op on an empty slice, so no length guard is needed.
	innermost.GroupBy(groupFrags...)
	// Bound the innermost groupArray to the single eval window so CH prunes
	// granules instead of groupArray-ing the full per-series retention (the
	// arrayFilter below stays as the precise post-groupArray gate). The
	// bound is rendered byte-identically by instantWindowScanBoundsFrags;
	// pushInstantScanBound fail-closes if the IR scan-time bound
	// (RangeWindow.InstantScanBounded, established by
	// chplan.AttachInstantScanTimeBounds) was never set, so a future
	// windowed-array shape cannot silently regress to an unbounded scan.
	if err := pushInstantScanBound(innermost, r, end, rangeNS); err != nil {
		return err
	}

	// Inner SELECT — arrayFilter to the [end-range, end] window.
	innerSb := NewQuery().From(innermost.Frag())
	for _, g := range groupFrags {
		innerSb.Select(g)
	}
	innerSb.Select(RawAs(windowFilterPairsFrag(end, rangeNS), "window_pairs"))

	// Outer SELECT — final value per series.
	outerSb := NewQuery().From(innerSb.Frag())
	for _, g := range groupFrags {
		outerSb.Select(g)
	}
	outerSb.Select(RawAs(valueWriterFor(end), r.ValueColumn))
	if minWindowSize > 0 {
		outerSb.Where(windowLenAtLeastFrag("window_pairs", minWindowSize))
	}

	e.emitSelect(outerSb)
	return nil
}

// emitWindowedArrayPairsMatrix is the OuterRange > 0 variant of
// emitWindowedArrayPairs: each series emits one row per anchor (across
// [End-OuterRange, End] spaced by Step, end-inclusive) whose window
// holds at least minWindowSize samples. Mirrors emitWindowedArrayMatrix
// — sample-side fanout + per-(series, anchor) regroup — but exposes
// `window_pairs` directly without the `window_vals` / `counter_delta`
// middle layer the values-only shape needs.
//
// SQL skeleton (with N = OuterRange/Step + 1):
//
//	SELECT series_key, anchor_ts, <valueFrag> AS value FROM (
//	  SELECT series_key, anchor_ts, arraySort(groupArray((TimeUnix, Value))) AS window_pairs
//	  FROM (
//	    SELECT series_key, TimeUnix, Value,
//	      arrayJoin(arrayMap(i -> <end> - toIntervalNanosecond(i * <step_ns>),
//	                range(<covered-anchor index bounds>))) AS anchor_ts
//	    FROM (<input>)
//	  ) GROUP BY series_key, anchor_ts
//	)
//
// See emitWindowedArrayMatrix for the memory-shape rationale (the
// previous full-grid fanout re-filtered the whole series array per
// anchor — O(anchors × window_samples) peak) and the empty-window
// contract (no group materialises for sample-less anchors; all matrix
// callers pass minWindowSize >= 2 here, so the emitted rows match the
// old shape exactly).
//
// The value-writer is built from the per-row anchor `anchor_ts` (not
// r.End) so anchor-relative shapes (deriv, predict_linear) render the
// correct per-anchor expression at every row.
func (e *emitter) emitWindowedArrayPairsMatrix(r *chplan.RangeWindow, valueWriterFor func(anchor Frag) Frag, minWindowSize int) error {
	end := endExprFrag(r)
	rangeNS := r.Range.Nanoseconds()
	stepNS := r.Step.Nanoseconds()
	// End-inclusive anchor count. Truncating division matches Prom.
	numAnchors := r.OuterRange.Nanoseconds()/stepNS + 1
	end, numAnchors = stepAlignGrid(r, end, stepNS, numAnchors)
	groupFrags, err := e.collectGroupByFrags(r.GroupBy)
	if err != nil {
		return err
	}

	innerSub, err := e.subqueryFrag(r.Input)
	if err != nil {
		return err
	}
	innerSub, srcTs := fanoutTsSource(innerSub, r.TimestampColumn)

	// Sample-fanout SELECT — one row per (sample, covered anchor).
	fanout := NewQuery().From(innerSub)
	for _, g := range groupFrags {
		fanout.Select(g)
	}
	fanout.Select(Col(srcTs))
	fanout.Select(Col(r.ValueColumn))
	fanout.Select(RawAs(
		sampleAnchorFanoutFrag(end, Col(srcTs), stepNS, rangeNS, numAnchors),
		"anchor_ts",
	))
	// Restrict the input scan to the offset-shifted
	// (Start - Offset - range, End - Offset] window the anchor grid
	// covers — same pushdown as emitWindowedArrayMatrix, against srcTs
	// (the timestamp column present in the fanout's FROM). See
	// maybePushInnerScanTimeBounds.
	maybePushInnerScanTimeBounds(fanout, r, srcTs, rangeNS)

	// Regroup SELECT — rebuild the per-(series, anchor) window array.
	regroup := NewQuery().From(fanout.Frag())
	for _, g := range groupFrags {
		regroup.Select(g)
	}
	regroup.Select(Col("anchor_ts"))
	regroup.Select(RawAs(groupArrayPairFrag(srcTs, r.ValueColumn), "window_pairs"))
	regroupKeys := make([]Frag, 0, len(groupFrags)+1)
	regroupKeys = append(regroupKeys, groupFrags...)
	regroupKeys = append(regroupKeys, Col("anchor_ts"))
	regroup.GroupBy(regroupKeys...)

	// Outer SELECT — per-(series, anchor) row.
	outer := NewQuery().From(regroup.Frag())
	for _, g := range groupFrags {
		outer.Select(g)
	}
	outer.Select(Col("anchor_ts"))
	// See emitWindowedArrayExtrapolatedMatrix for the rationale: surface
	// anchor_ts under the schema timestamp column so a wrapping
	// Aggregate's per-step GROUP BY (ColumnRef{TimestampColumn}) resolves.
	if r.TimestampColumn != "" && r.TimestampColumn != "anchor_ts" {
		outer.Select(As(verbatim("anchor_ts"), r.TimestampColumn))
	}
	outer.Select(RawAs(valueWriterFor(verbatim("anchor_ts")), r.ValueColumn))
	if minWindowSize > 0 {
		outer.Where(windowLenAtLeastFrag("window_pairs", minWindowSize))
	}

	e.emitSelect(outer)
	return nil
}

// endExprFrag returns a Frag rendering `<End> [- toIntervalNanosecond(<offset>)]`.
// Shared by every windowed-array emitter; centralises the Offset
// branch. `r.Offset != 0` so a negative offset (Prom's forward-shift
// form, `rate(metric[range] offset -5m)`) still emits the subtract —
// CH interval arithmetic renders `End - toIntervalNanosecond(-N)` as
// `End + N` so the window shifts forward into the future correctly.
func endExprFrag(r *chplan.RangeWindow) Frag {
	return offsetShiftedBaseFrag(timeOrNowFrag(r.End), r.Offset)
}

// windowPairsSLRFrag renders the per-row CH simple-linear-regression
// over a window of (timestamp, value) pairs:
//
//	arrayReduce('simpleLinearRegression',
//	            arrayMap(p -> dateDiff('second', <anchor>, tupleElement(p, 1)), window_pairs),
//	            arrayMap(p -> tupleElement(p, 2), window_pairs))
//
// The xs are seconds elapsed from `anchor` to each pair's timestamp
// (tuple element 1); the ys are the pair values (tuple element 2).
// `simpleLinearRegression` is a CH aggregate, so it's applied to the two
// parallel arrays via arrayReduce. The result is a Tuple(slope,
// intercept) — element 1 is the slope (k), element 2 the intercept (b).
// Shared by predict_linear (intercept + slope*t) and deriv (slope).
func windowPairsSLRFrag(anchor Frag) Frag {
	xs := Call(
		"arrayMap",
		Lambda1("p", Call("dateDiff", InlineLit("second"), anchor,
			Call("tupleElement", BareIdent("p"), InlineLit(int64(1))))),
		BareIdent("window_pairs"),
	)
	ys := Call(
		"arrayMap",
		Lambda1("p", Call("tupleElement", BareIdent("p"), InlineLit(int64(2)))),
		BareIdent("window_pairs"),
	)
	return Call("arrayReduce", InlineLit("simpleLinearRegression"), xs, ys)
}

// fanoutTsSource resolves the per-sample timestamp column the matrix
// fanout layers (the arrayJoin'd anchor expression + the regroup's
// groupArray pairs) must reference, returning the possibly-wrapped
// input Frag and the column name to use.
//
// The subtle case is a NESTED matrix shape — `irate(up[5m:1m])`,
// `max_over_time(irate(m[5m:1m])[10m:1m])` — where the input relation
// already exposes its per-sample timestamps under the fanout's own
// `anchor_ts` output alias (the inner matrix emits anchor_ts and the
// wrapping lowering sets TimestampColumn to it). Selecting the source
// column bare alongside `arrayJoin(...) AS anchor_ts` makes the name
// ambiguous one subquery up: CH resolves the regroup layer's
// `groupArray((anchor_ts, Value))` against the FANNED alias, so every
// pair in a window carries its group's anchor instead of the sample's
// own timestamp. Order-insensitive reducers (max/avg/…) survive that
// shadowing — pairwise ones don't (irate's `dateDiff(prev, last)`
// becomes 0 → NaN).
//
// The rename happens on an interposed `SELECT *, anchor_ts AS _src_ts`
// projection layer (NOT a lateral alias in the fanout SELECT itself —
// CH's analyzer rejects referencing a same-SELECT alias from inside
// the arrayJoin argument). The non-nested path returns the input
// untouched (byte-stable fixtures).
func fanoutTsSource(innerSub Frag, tsCol string) (Frag, string) {
	if tsCol != "anchor_ts" {
		return innerSub, tsCol
	}
	wrap := NewQuery().From(innerSub).Select(
		Star(),
		RawAs(Col("anchor_ts"), "_src_ts"),
	)
	return wrap.Frag(), "_src_ts"
}

// timeOrNowFrag returns a Frag rendering an explicit DateTime64(9)
// literal for a non-zero time or `now64(9)` for the zero value.
func timeOrNowFrag(t time.Time) Frag {
	return func(b *Builder) {
		if t.IsZero() {
			b.Now64()
			return
		}
		b.DateTime64Lit(t)
	}
}

// formatFloat renders a float64 in a CH-friendly form (no `e`-notation
// unless the value needs it; trailing zeros stripped). Mirrors what
// strconv.FormatFloat(v, 'f', -1, 64) does.
func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// emitRangeWindowMetrics renders a RangeWindow wrapping a
// MetricsAggregate — the TraceQL `/api/metrics/query_range` shape.
// Each per-span row out of m.Inner is fanned across only the anchors
// whose window contains its timestamp (sample-side fanout, ≤
// range/step + 1 anchors per row — see sampleAnchorFanoutFrag; the
// previous shape fanned every row across the full N-anchor grid and
// re-filtered per (row, anchor)); the outer SELECT applies the
// Op-specific CH aggregate per (group-by, anchor) bucket.
//
// SQL skeleton, observed-only ops (N = (End-Start)/Step + 1 or
// OuterRange/Step + 1):
//
//	SELECT [<group cols>,] anchor_ts, <reducer> AS value
//	FROM (
//	  SELECT [<group cols>,] [<Attr> AS metric_arg,]
//	         arrayJoin(arrayMap(i -> <anchor_base> - toIntervalNanosecond(i * <step_ns>),
//	                   range(<covered-anchor index bounds>))) AS anchor_ts
//	  FROM (<Inner>)
//	)
//	GROUP BY [<group cols>,] anchor_ts
//
// Zero-fill ops (count_over_time / rate) add a lightweight generator
// arm via UNION ALL — one (group, anchor, in_window=0) row per
// (distinct group, grid anchor) — and reduce with
// `toFloat64(sum(in_window))` so anchors with no samples still emit 0
// (Tempo StepAggregator / CountOverTimeAggregator semantics). See
// metricsZeroFillGridArm / metricsSumWeightReducerFrag.
//
// The bucket is left-open / right-closed — `(anchor_ts - range, anchor_ts]`
// — matching Tempo upstream's `IntervalMapperQueryRange` semantics
// (`(start, start+step], (start+step, start+2*step], …`; see
// `pkg/traceql/engine_metrics.go`'s `IntervalMapperQueryRange.interval`).
// A sample at exactly `anchor_ts` belongs to *this* anchor (right edge
// included); a sample at exactly `anchor_ts - range` belongs to the
// *previous* anchor (left edge excluded) — the sample-side index bounds
// encode exactly this open/closed pairing (strict floor+1 lower bound,
// inclusive floor upper bound; see sampleAnchorFanoutFrag).
//
// `<reducer>` depends on m.Op:
//
//   - Rate: `toFloat64(sum(in_window)) / <range_seconds>`
//   - CountOverTime: `toFloat64(sum(in_window))`
//   - Sum/Min/Max/AvgOverTime: `sum/min/max/avg(metric_arg)`
//   - QuantileOverTime: routed to the bucket shape (see
//     emitRangeWindowMetricsQuantileBuckets).
//
// `<anchor_base>` is r.End (or now64(9) for the zero-time fixture).
// Range defaults to Step when r.Range is zero (matches Tempo's TraceQL
// metrics semantics: each bucket covers exactly its Step width).
func (e *emitter) emitRangeWindowMetrics(r *chplan.RangeWindow, m *chplan.MetricsAggregate) error {
	if r.TimestampColumn == "" {
		return fmt.Errorf("%w: RangeWindow.TimestampColumn unset (required for MetricsAggregate input)", ErrUnsupported)
	}
	if r.Step <= 0 {
		return fmt.Errorf("%w: RangeWindow wrapping MetricsAggregate requires Step > 0", ErrUnsupported)
	}
	// Fail closed if the inner is a spans scan with no request window: the
	// shared maybePushInnerScanTimeBounds below silently no-ops on a zero
	// window, which over otel_traces is a full-retention scan. Fires only for
	// the Tempo spans inner (PromQL's inner is a metrics table). This covers
	// both the regular and the quantile-bucket routes below.
	if err := requireInnerSpansScanBound(r, m.Inner, e.spansTable); err != nil {
		return err
	}
	// quantile_over_time takes the bucket-shape route: emit one row per
	// (group, anchor, bucket) with `count(1)` and let the Tempo handler
	// (internal/api/tempo/metrics_query_range.go) post-process via
	// pkg/traceql.Log2QuantileWithBucket so the wire matches Tempo's
	// HistogramAggregator. Native CH `quantile` / `quantiles` aggregates
	// would diverge from Tempo's power-of-two bucket-interpolation
	// algorithm; routing through the bucket shape keeps the upstream
	// algorithm authoritative.
	if m.Op == chplan.MetricsOpQuantileOverTime {
		return e.emitRangeWindowMetricsQuantileBuckets(r, m)
	}
	chName, params, args, err := metricsAggregateCH(m)
	if err != nil {
		return err
	}

	// Pre-flight all expressions so chplan errors surface synchronously.
	for _, g := range m.GroupBy {
		if err := (&Builder{}).Expr(g); err != nil {
			return err
		}
	}
	for _, p := range params {
		if err := (&Builder{}).Expr(p); err != nil {
			return err
		}
	}
	for _, a := range args {
		if err := (&Builder{}).Expr(a); err != nil {
			return err
		}
	}

	end := endExprFrag(r)
	stepNS := r.Step.Nanoseconds()
	// Range defaults to Step (per-bucket = per-step). When r.Range is
	// set explicitly (e.g. a future rate-over-Range(Step)), use it
	// verbatim.
	rangeDur := r.Range
	if rangeDur == 0 {
		rangeDur = r.Step
	}
	rangeNS := rangeDur.Nanoseconds()
	rangeSeconds := rangeDur.Seconds()

	// Anchor count: end-inclusive grid across [End-Span, End] spaced by
	// Step. When OuterRange > 0 use it (PromQL subquery semantics);
	// otherwise derive from [Start, End].
	var numAnchors int64
	switch {
	case r.OuterRange > 0:
		numAnchors = r.OuterRange.Nanoseconds()/stepNS + 1
	case !r.Start.IsZero() && !r.End.IsZero():
		span := r.End.Sub(r.Start).Nanoseconds()
		if span < 0 {
			return fmt.Errorf("%w: RangeWindow.Start > End", ErrUnsupported)
		}
		numAnchors = span/stepNS + 1
	default:
		// Instant fallback — single anchor at End.
		numAnchors = 1
	}

	inner, err := e.subqueryFrag(m.Inner)
	if err != nil {
		return err
	}

	// Sample-fanout SELECT: fan each Inner row across only the anchors
	// whose `(anchor_ts - range, anchor_ts]` window contains its
	// timestamp (sample-side fanout — ≤ range/step + 1 anchors per row,
	// not the full N-anchor grid; see sampleAnchorFanoutFrag), projecting
	// group-by cols, [the metric operand as metric_arg,] and anchor_ts.
	// Group-by columns are aliased so the outer SELECT / GROUP BY can
	// reference them by a stable name regardless of whether the source
	// expression was a bare ColumnRef or a Map lookup. The fanout
	// predicate IS the window predicate, so no per-row `(anchor_ts -
	// range, anchor_ts]` re-check survives downstream.
	groupAliases := outerGroupAliases(m.GroupBy, m.GroupByAliases)
	tsCol := r.TimestampColumn
	tsIdent := func(b *Builder) { b.Ident(tsCol) }
	fanout := NewQuery().From(inner)
	for i, g := range m.GroupBy {
		expr := g
		alias := groupAliases[i]
		fanout.SelectAs(func(b *Builder) { _ = b.Expr(expr) }, alias)
	}
	zeroFill := metricsOpZeroFillsEmptyBuckets(m.Op)
	if !zeroFill && m.Attr != nil {
		attr := m.Attr
		fanout.SelectAs(func(b *Builder) { _ = b.Expr(attr) }, "metric_arg")
	}
	fanout.SelectAs(
		sampleAnchorFanoutFrag(end, tsIdent, stepNS, rangeNS, numAnchors),
		"anchor_ts",
	)
	if zeroFill {
		// Sample rows carry weight 1; the zero-fill generator rows
		// (below) carry 0 so `sum(in_window)` counts only real samples.
		fanout.SelectAs(InlineLit(int64(1)), "in_window")
	}
	// Push (Start - range, End] onto the wrapping SELECT over m.Inner
	// for CH partition / granule pruning. Gated so subquery-internal
	// shapes (no Start/End grid) stay byte-stable. See
	// maybePushInnerScanTimeBounds.
	maybePushInnerScanTimeBounds(fanout, r, tsCol, rangeNS)

	// Zero-fill semantics live in the SQL for the two ops whose Tempo
	// aggregators emit 0 for empty buckets — count_over_time and rate.
	// With the sample-side fanout an anchor with no samples produces NO
	// row out of the GROUP BY, so the zero rows come from a lightweight
	// generator arm UNION ALL'd alongside the samples: one row per
	// (distinct group, grid anchor) with in_window = 0 — the full-grid
	// anchorFanoutFrag applied to a per-GROUP discovery subquery (O(groups
	// × N) tiny rows, no payload arrays). The reducer is
	// `toFloat64(sum(in_window))` [/ range_seconds for rate], so empty
	// anchors emit 0 and observed anchors emit the real sample count —
	// exactly the rows the previous full-grid countIf(<window pred>)
	// shape produced, at a fraction of the row blowup. The group set is
	// identical by construction: the discovery arm GROUPs the same
	// bounded Inner scan the sample arm reads, so a group appears in the
	// generator iff it had at least one Inner row — the same condition
	// that materialised its (group, anchor) grid in the old fanout.
	//
	// sum / avg / min / max over_time stay observed-only: Tempo
	// initialises their aggregators to NaN and skips empty buckets at
	// SeriesSet.ToProto, so the response shape includes only observed
	// (group, anchor) rows — which is exactly what the sample-side
	// GROUP BY emits with no generator arm.
	var source Frag
	if zeroFill {
		grid := e.metricsZeroFillGridArm(
			inner, r, m, groupAliases, end, stepNS, rangeNS, numAnchors, nil,
		)
		source = Paren(UnionAll(fanout.Frag(), grid))
	} else {
		source = fanout.Frag()
	}

	// Outer SELECT: GROUP BY group cols + anchor_ts; apply the
	// per-bucket reducer.
	outerSb := NewQuery().From(source)

	// Group-by columns in the outer SELECT-list are referenced by the
	// stable inner-SELECT aliases (set above).
	for _, alias := range groupAliases {
		a := alias
		outerSb.Select(func(b *Builder) { b.Ident(a) })
	}
	outerSb.Select(Col("anchor_ts"))

	if zeroFill {
		outerSb.Select(As(metricsSumWeightReducerFrag(m.Op, rangeSeconds), m.ValueAlias))
	} else {
		outerSb.Select(As(metricsReducerFrag(m.Op, chName, params, args, rangeSeconds), m.ValueAlias))
	}

	// GROUP BY group aliases + anchor_ts.
	groupFrags := make([]Frag, 0, len(groupAliases)+1)
	for _, alias := range groupAliases {
		a := alias
		groupFrags = append(groupFrags, func(b *Builder) { b.Ident(a) })
	}
	groupFrags = append(groupFrags, Col("anchor_ts"))
	outerSb.GroupBy(groupFrags...)

	e.emitSelect(outerSb)
	return nil
}

// zeroFillExtraCol is an extra (frag, alias) SELECT item the zero-fill
// generator arm projects ahead of anchor_ts so its SELECT-list aligns
// positionally with the sample arm's — CH UNION ALL unifies columns by
// position. Used by the quantile bucket path to pin a `0 AS metric_arg`
// placeholder against the sample arm's real operand column.
type zeroFillExtraCol struct {
	frag  Frag
	alias string
}

// metricsZeroFillGridArm builds the generator arm of the zero-fill
// UNION ALL: one row per (distinct group, grid anchor) carrying
// `0 AS in_window` (and, when extraCols is non-empty, the listed
// (frag, alias) pairs ahead of anchor_ts so the arm's SELECT-list
// aligns positionally with the sample arm's).
//
// Group discovery replays the same Inner subquery (and the same
// Start/End scan-bound pushdown) the sample arm reads, GROUPed by the
// group aliases so the arm emits exactly one row per group that has at
// least one Inner row in the bounded scan. With no group-by columns the
// discovery degenerates to `SELECT 1 FROM (<Inner>) LIMIT 1` — one row
// iff the scan is non-empty, zero rows otherwise — so a fully-empty
// input still produces an empty result (matching the old full-grid
// fanout, which had no rows to fan).
func (e *emitter) metricsZeroFillGridArm(
	inner Frag,
	r *chplan.RangeWindow,
	m *chplan.MetricsAggregate,
	groupAliases []string,
	end Frag,
	stepNS, rangeNS, numAnchors int64,
	extraCols []zeroFillExtraCol,
) Frag {
	tsCol := r.TimestampColumn
	var disc *QueryBuilder
	if len(groupAliases) > 0 {
		disc = NewQuery().From(inner)
		for i, g := range m.GroupBy {
			expr := g
			alias := groupAliases[i]
			disc.SelectAs(func(b *Builder) { _ = b.Expr(expr) }, alias)
		}
		discKeys := make([]Frag, 0, len(groupAliases))
		for _, alias := range groupAliases {
			a := alias
			discKeys = append(discKeys, func(b *Builder) { b.Ident(a) })
		}
		disc.GroupBy(discKeys...)
		// Group discovery replays the sample arm's Start/End scan-bound
		// pushdown so it reads the same bounded window.
		maybePushInnerScanTimeBounds(disc, r, tsCol, rangeNS)
	} else {
		// No group-by: the zero grid needs exactly one driver row. Read it from
		// the synthetic 1-row `system.one` table so the probe carries NO spans
		// scan — the old `SELECT 1 FROM (<otel_traces>) LIMIT 1` planted a
		// windowless fact-table scan that prunes nothing (it has no Timestamp
		// predicate) purely to test non-emptiness. system.one is always exactly
		// one row, so the unlabelled zero grid is now emitted unconditionally.
		disc = NewQuery().Select(InlineLit(int64(1))).From(Qual("system", "one"))
	}

	grid := NewQuery().From(disc.Frag())
	for _, alias := range groupAliases {
		a := alias
		grid.Select(func(b *Builder) { b.Ident(a) })
	}
	for _, c := range extraCols {
		grid.SelectAs(c.frag, c.alias)
	}
	grid.SelectAs(anchorFanoutFrag(end, stepNS, numAnchors), "anchor_ts")
	grid.SelectAs(InlineLit(int64(0)), "in_window")
	return grid.Frag()
}

// metricsSumWeightReducerFrag returns the per-(group, anchor) reducer
// for the zero-fill matrix path: `toFloat64(sum(in_window))` for
// count_over_time, divided through the range duration in seconds for
// rate. Sample-arm rows carry in_window = 1 (and each already belongs
// to its anchor's window by fanout construction), generator-arm rows
// carry 0 — so the sum equals the old countIf(<window pred>) sample
// count per (group, anchor), with empty anchors pinned at 0 by the
// generator arm. The toFloat64 wrap keeps the Value column at the
// uniform Float64 wire type chclient.Sample.Value expects (see
// TestRangeWindowMetricsReducerIsFloat64).
func metricsSumWeightReducerFrag(op chplan.MetricsOp, rangeSeconds float64) Frag {
	sum := Call("toFloat64", Call("sum", BareIdent("in_window")))
	switch op {
	case chplan.MetricsOpRate:
		return Div(sum, InlineLit(rangeSeconds))
	default:
		return sum
	}
}

// metricsOpZeroFillsEmptyBuckets reports whether the given
// MetricsAggregate.Op surfaces 0-valued samples for empty buckets on
// Tempo's wire (rather than NaN-skipping them). The two upstream code
// paths that produce zeros are StepAggregator + CountOverTimeAggregator
// (for count_over_time and rate — the underlying counter aggregator
// starts at zero) and HistogramAggregator.Results (for
// quantile_over_time — explicitly sets ts.Values[i] = 0.0 when the
// bucket has no histogram entries). All other operators reach the wire
// via OverTimeAggregator's NaN-init path, so the cerberus emitter's
// observed-only emission (sample-side fanout, no generator arm)
// already matches Tempo's output and needs no fill.
//
// emitRangeWindowMetrics (count / rate) branches on this predicate to
// pick the UNION ALL zero-fill-generator + sum(in_window) shape vs the
// plain observed-only aggregate; emitRangeWindowMetricsQuantileBuckets
// uses the generator arm unconditionally because the
// quantile_over_time op is always on the zero-fill path.
func metricsOpZeroFillsEmptyBuckets(op chplan.MetricsOp) bool {
	switch op {
	case chplan.MetricsOpCountOverTime,
		chplan.MetricsOpRate,
		chplan.MetricsOpQuantileOverTime:
		return true
	}
	return false
}

// emitRangeWindowMetricsQuantileBuckets renders quantile_over_time in
// the matrix path as a `(group, anchor, bucket, count)` row stream. The
// Tempo handler (internal/api/tempo/metrics_query_range.go) groups those
// rows by (group, anchor) and calls upstream
// `pkg/traceql.Log2QuantileWithBucket(phi, buckets)` per phi to compute
// the per-anchor quantile value — mirroring Tempo's HistogramAggregator
// (engine_metrics.go) so the wire reading aligns with the reference
// engine's power-of-two bucket interpolation.
//
// SQL skeleton:
//
//	SELECT [<group cols>,] anchor_ts,
//	       pow(2, ceil(log2(toFloat64(metric_arg) [* 1e9]))) [/ 1e9] AS `__bucket`,
//	       toFloat64(count(1)) AS Value
//	FROM (
//	  SELECT [<group cols>,] <TimestampColumn> AS ts, <Attr> AS metric_arg,
//	         arrayJoin(arrayMap(i -> <anchor_base> - toIntervalNanosecond(i * <step_ns>), range(0, <N>))) AS anchor_ts
//	  FROM (<Inner>)
//	)
//	WHERE ts >  anchor_ts - toIntervalNanosecond(<range_ns>)
//	  AND ts <= anchor_ts
//	  AND <metric_arg-in-nanos>  >= 2     -- Tempo's bucketize* drops <2
//	GROUP BY [<group cols>,] anchor_ts, `__bucket`
//
// The `<metric_arg-in-nanos> >= 2` filter mirrors Tempo's
// bucketizeDuration / bucketizeAttribute "if d < 2 return nil" guard. For
// duration operands `metric_arg` is already `Duration / 1e9` (the
// cerberus-side seconds rebase), so the filter expands to
// `metric_arg * 1e9 >= 2` — i.e. raw `Duration >= 2` nanoseconds. For
// non-duration numeric operands the cerberus lowering passes the value
// unchanged, so the filter is `metric_arg >= 2`.
//
// The bucket alias is the wire-format-stable `__bucket` literal — same
// alias `MetricsHistogramOverTime` uses (`histogramBucketAlias` /
// `internalLabelBucket` in Tempo) so downstream readers can pick the
// bucket out of the row stream by a stable name.
func (e *emitter) emitRangeWindowMetricsQuantileBuckets(r *chplan.RangeWindow, m *chplan.MetricsAggregate) error {
	if m.Attr == nil {
		return fmt.Errorf("%w: quantile_over_time matrix path requires MetricsAggregate.Attr", ErrUnsupported)
	}
	if len(m.Quantiles) == 0 {
		return fmt.Errorf("%w: quantile_over_time matrix path requires at least one phi", ErrUnsupported)
	}
	if m.Inner == nil {
		return fmt.Errorf("%w: quantile_over_time matrix path requires MetricsAggregate.Inner", ErrUnsupported)
	}
	// Fail closed on a zero-window spans inner (see emitRangeWindowMetrics).
	// Redundant when reached via emitRangeWindowMetrics, but keeps this entry
	// point safe if ever called directly.
	if err := requireInnerSpansScanBound(r, m.Inner, e.spansTable); err != nil {
		return err
	}

	// Pre-flight expressions so chplan errors surface synchronously.
	if err := (&Builder{}).Expr(m.Attr); err != nil {
		return err
	}
	for _, g := range m.GroupBy {
		if err := (&Builder{}).Expr(g); err != nil {
			return err
		}
	}

	end := endExprFrag(r)
	stepNS := r.Step.Nanoseconds()
	rangeDur := r.Range
	if rangeDur == 0 {
		rangeDur = r.Step
	}
	rangeNS := rangeDur.Nanoseconds()

	var numAnchors int64
	switch {
	case r.OuterRange > 0:
		numAnchors = r.OuterRange.Nanoseconds()/stepNS + 1
	case !r.Start.IsZero() && !r.End.IsZero():
		span := r.End.Sub(r.Start).Nanoseconds()
		if span < 0 {
			return fmt.Errorf("%w: RangeWindow.Start > End", ErrUnsupported)
		}
		numAnchors = span/stepNS + 1
	default:
		numAnchors = 1
	}

	inner, err := e.subqueryFrag(m.Inner)
	if err != nil {
		return err
	}

	groupAliases := outerGroupAliases(m.GroupBy, m.GroupByAliases)
	tsCol := r.TimestampColumn
	tsIdent := func(b *Builder) { b.Ident(tsCol) }

	// Sample arm — sample-side fanout (≤ range/step + 1 anchors per
	// row; see sampleAnchorFanoutFrag), so every fanned row already
	// belongs to its anchor's `(anchor_ts - range, anchor_ts]` window.
	fanout := NewQuery().From(inner)
	for i, g := range m.GroupBy {
		expr := g
		alias := groupAliases[i]
		fanout.SelectAs(func(b *Builder) { _ = b.Expr(expr) }, alias)
	}
	attr := m.Attr
	fanout.SelectAs(func(b *Builder) { _ = b.Expr(attr) }, "metric_arg")
	fanout.SelectAs(
		sampleAnchorFanoutFrag(end, tsIdent, stepNS, rangeNS, numAnchors),
		"anchor_ts",
	)
	fanout.SelectAs(InlineLit(int64(1)), "in_window")
	// Same Start/End pushdown as emitRangeWindowMetrics — see
	// maybePushInnerScanTimeBounds.
	maybePushInnerScanTimeBounds(fanout, r, tsCol, rangeNS)

	// Generator arm — quantile_over_time is always on Tempo's zero-fill
	// path (HistogramAggregator.Results sets ts.Values[i] = 0.0 for
	// anchors with no histogram entries), so every observed group needs
	// one (group, anchor, __bucket=0) row per grid anchor even when no
	// sample lands in the window. The lightweight per-GROUP grid arm
	// pins those rows with in_window = 0 and a 0 metric_arg placeholder
	// (which can never satisfy the `>= 2` bucketize guard); see
	// metricsZeroFillGridArm.
	grid := e.metricsZeroFillGridArm(
		inner, r, m, groupAliases, end, stepNS, rangeNS, numAnchors,
		[]zeroFillExtraCol{{frag: InlineLit(int64(0)), alias: "metric_arg"}},
	)

	outerSb := NewQuery().From(Paren(UnionAll(fanout.Frag(), grid)))
	for _, alias := range groupAliases {
		a := alias
		outerSb.Select(func(b *Builder) { b.Ident(a) })
	}
	outerSb.Select(Col("anchor_ts"))
	// Bucket projection is conditional on the sample-arm marker + the
	// raw-value >= 2 guard: rows that don't satisfy both fall into a
	// phantom 0-bucket group (matching no real bucket because the
	// minimum power-of-two bucket is >= 2 after the `metric_arg >= 2`
	// guard). The conditional projection — rather than a WHERE-clause
	// filter — guarantees one (group, anchor, __bucket=0) row per
	// (group, grid anchor) tuple even when zero samples land in
	// the window, so the handler's post-processor sees the empty
	// (group, anchor) and Log2QuantileWithBucket returns 0 there
	// (matching Tempo HistogramAggregator.Results's
	// `ts.Values[i] = 0.0` for empty buckets).
	outerSb.SelectAs(quantileBucketIfFrag(m.IsDuration), metricsQuantileBucketAlias)
	// Value is countIf over the same conjunction so phantom rows count
	// 0 and real-bucket rows count their observed sample count.
	outerSb.Select(As(quantileCountIfFrag(m.IsDuration), m.ValueAlias))

	// GROUP BY group aliases + anchor_ts + bucket.
	groupFrags := make([]Frag, 0, len(groupAliases)+2)
	for _, alias := range groupAliases {
		a := alias
		groupFrags = append(groupFrags, func(b *Builder) { b.Ident(a) })
	}
	groupFrags = append(groupFrags, Col("anchor_ts"), Col(metricsQuantileBucketAlias))
	outerSb.GroupBy(groupFrags...)

	e.emitSelect(outerSb)
	return nil
}

// quantileBucketIfFrag renders the conditional `__bucket` projection
// for the zero-fill quantile path: real-bucket value when the row came
// from the sample arm (in_window = 1; every sample-arm row already
// sits in its anchor's window by sample-side fanout construction) AND
// the raw metric_arg meets Tempo's bucketize* `>= 2` guard, else 0
// (the phantom sentinel — distinct from every real bucket because the
// minimum power-of-two bucket-edge is 2 nanoseconds, which renders as
// 2 for the non-duration branch and 2e-9 for the seconds-rebased
// duration branch).
//
// Pairs with quantileCountIfFrag so the per-(group, anchor) GROUP BY
// emits at least one row (the phantom 0-bucket / 0-count row) per
// (group, grid anchor) tuple — SQL-side zero-fill matching Tempo's
// HistogramAggregator.Results emission of `ts.Values[i] = 0.0` for
// anchors with no histogram entries. The generator arm
// (metricsZeroFillGridArm) guarantees the phantom row exists for every
// grid anchor; in the pre-sample-side shape that came from fanning
// every row across the full anchor grid instead.
func quantileBucketIfFrag(isDuration bool) Frag {
	bucket := quantileBucketFrag(isDuration)
	return Call("if", quantileSamplePredicateFrag(isDuration), bucket, InlineLit(int64(0)))
}

// quantileCountIfFrag renders the conditional `Value` projection for
// the zero-fill quantile path: `toFloat64(countIf(in_window = 1 AND
// <metric_arg >= 2>))`. Phantom rows (which fall in the
// __bucket=0 group via quantileBucketIfFrag) count 0; real-bucket rows
// count their observed sample count. See quantileBucketIfFrag for the
// per-(group, anchor) zero-fill rationale.
func quantileCountIfFrag(isDuration bool) Frag {
	return Call("toFloat64", Call("countIf", quantileSamplePredicateFrag(isDuration)))
}

// quantileSamplePredicateFrag renders the conjunction shared by the
// quantile zero-fill `if(...)` / `countIf(...)` calls:
//
//	in_window = 1 AND <metric_arg-min-pred>
//
// `in_window = 1` marks sample-arm rows — each already inside its
// anchor's `(anchor_ts - range, anchor_ts]` window by sample-side
// fanout construction, so the explicit window re-check the legacy
// full-grid shape needed here collapses to the arm marker. The
// `metric_arg >= 2` clause is Tempo's bucketize* guard
// (pkg/traceql/ast_metrics.go, bucketizeDuration / bucketizeAttribute);
// for duration operands metric_arg carries seconds (the cerberus-side
// `Duration / 1e9` rebase), so the guard re-scales to raw nanoseconds.
func quantileSamplePredicateFrag(isDuration bool) Frag {
	inWindow := Eq(BareIdent("in_window"), InlineLit(int64(1)))
	if isDuration {
		return And(inWindow,
			Gte(Mul(BareIdent("metric_arg"), InlineLit(chplan.NanoToSecondDivisor)), InlineLit(int64(2))))
	}
	return And(inWindow, Gte(BareIdent("metric_arg"), InlineLit(int64(2))))
}

// metricsQuantileBucketAlias is the SELECT-list alias the matrix-path
// quantile_over_time emitter assigns to its per-row bucket column.
// Mirrors Tempo's internal `__bucket` label name (engine_metrics.go,
// `internalLabelBucket`) so the Tempo handler's
// `postProcessQuantileBuckets` can pick the bucket value out of the
// row stream by a stable name. The Tempo handler holds its own
// matching constant (`tempoQuantileBucketLabel` in
// internal/api/tempo/metrics_query_range.go); both must agree on the
// literal "__bucket".
const metricsQuantileBucketAlias = "__bucket"

// quantileBucketFrag renders the per-row bucket key. Mirrors Tempo's
// `Log2Bucketize(v) [/ time.Second]` (pkg/traceql/engine_metrics.go).
//
//   - duration operands: cerberus's lowering already projects
//     `metric_arg = Duration / 1e9`. To recover the raw nanosecond value
//     for the `Log2Bucketize(d) = 2^ceil(log2(d))` formula, multiply by
//     1e9 inside the log2; divide the resulting power-of-two back by 1e9
//     to keep the bucket label in fractional seconds, matching
//     bucketizeDuration's wire shape.
//   - non-duration operands: `metric_arg` carries the raw numeric value;
//     emit `pow(2, ceil(log2(toFloat64(metric_arg))))` verbatim.
//
// The 1e9 divisor renders as the literal `1000000000` so the emitted
// SQL has no bound argument for the unit-conversion constant (it's
// query-shape, not user data) — same idiom used by
// `histogramBucketFrag` in internal/chsql/histogram_over_time.go.
func quantileBucketFrag(isDuration bool) Frag {
	metricArg := Call("toFloat64", BareIdent("metric_arg"))
	if isDuration {
		// log2 over the raw nanosecond value (metric_arg rebased back up by
		// 1e9), bucket divided back to fractional seconds.
		bucket := Call("pow", InlineLit(int64(2)),
			Call("ceil", Call("log2", Mul(metricArg, InlineLit(chplan.NanoToSecondDivisor)))))
		return Div(bucket, InlineLit(chplan.NanoToSecondDivisor))
	}
	return Call("pow", InlineLit(int64(2)), Call("ceil", Call("log2", metricArg)))
}

// anchorFanoutFrag returns a Frag rendering
// `arrayJoin(arrayMap(i -> <end> - toIntervalNanosecond(i * <stepNS>), range(0, <N>)))`.
// The FULL-GRID fanout: every input row fans across all N anchors.
//
// Since the sample-side fanout landed (see sampleAnchorFanoutFrag) this
// shape survives only as the lightweight zero-fill generator: the
// zero-fill matrix emitters (count_over_time / rate /
// quantile_over_time on the Tempo metrics path) UNION a per-GROUP grid
// of (group, anchor, in_window=0) rows produced by this Frag so
// anchors with no contributing samples still emit a zero row. The
// generator's input is one row per distinct group (not one row per
// sample), so the fanout is O(groups × N) tiny rows — never the
// O(rows × N) blowup the sample-side fanout replaced.
//
// end is rendered via the Frag callback (the CH expression for the
// eval-grid anchor base — typically a DateTime64 literal or
// `now64(9)`); stepNS and N are inline integer literals.
func anchorFanoutFrag(end Frag, stepNS, numAnchors int64) Frag {
	return Call(
		"arrayJoin",
		Call(
			"arrayMap",
			Lambda1("i", anchorBaseAtIdxFrag(end, stepNS)),
			Call("range", InlineLit(int64(0)), InlineLit(numAnchors)),
		),
	)
}

// anchorBaseAtIdxFrag renders the arrayMap body shared by every anchor
// fan-out: `<end> - toIntervalNanosecond(i * <stepNS>)` — the i-th grid
// anchor walking back from the eval-grid base. `i` is the enclosing
// Lambda1 param (BareIdent("i")).
func anchorBaseAtIdxFrag(end Frag, stepNS int64) Frag {
	return Sub(end, Call("toIntervalNanosecond", Mul(BareIdent("i"), InlineLit(stepNS))))
}

// sampleAnchorFanoutFrag returns a Frag rendering the SAMPLE-SIDE
// anchor fanout — the bounded replacement for pairing the full-grid
// anchorFanoutFrag with a per-(row, anchor) window re-check:
//
//	arrayJoin(arrayMap(i -> <end> - toIntervalNanosecond(i * <stepNS>),
//	          range(greatest(0, <floorDiv(dist - rangeNS, stepNS) + 1>),
//	                least(<N>, <floorDiv(dist, stepNS) + 1>))))
//
// where `dist = dateDiff('nanosecond', <ts>, <end>)` is the row's
// distance behind the newest anchor. A sample at timestamp ts belongs
// to exactly the anchors a_i = end - i*step whose left-open /
// right-closed window `(a_i - range, a_i]` contains ts:
//
//	ts <= a_i          ⇔  i*step <= dist          ⇔  i <= floor(dist / step)
//	ts >  a_i - range  ⇔  i*step >  dist - range  ⇔  i >= floor((dist - range) / step) + 1
//
// — at most range/step + 1 indices per row (e.g. rate[5m] at step=15s
// fans each sample to ≤ 21 anchors), versus the previous full-grid
// shape where every row was re-checked against ALL N anchors (5,760
// for a 24h window at 15s). The window predicate is exact by
// construction, so downstream layers need no `(anchor_ts - range,
// anchor_ts]` re-filter: every fanned row already belongs to its
// anchor's window.
//
// Rows that cover no anchor on the [0, N) grid produce an empty index
// array; `arrayJoin([])` drops the row (verified against CH 24.8:
// `range(lo, hi)` returns `[]` whenever `hi <= lo`, including negative
// Int64 bounds). The greatest/least clamps map both raw bounds through
// the same monotone clamp into [0, N], so `lo <= hi` is preserved and
// out-of-grid rows degenerate to the empty range.
//
// CH's intDiv truncates toward zero (intDiv(-1, 3) = 0), not toward
// negative infinity, so the floor division is spelled
// `intDiv(x, step) - (modulo(x, step) < 0)` — CH's modulo carries the
// dividend's sign, making the correction term exactly the "truncation
// rounded the wrong way" indicator. See writeAnchorGridFloorIdx.
func sampleAnchorFanoutFrag(end, ts Frag, stepNS, rangeNS, numAnchors int64) Frag {
	dist := distBehindAnchorFrag(ts, end)
	return Call(
		"arrayJoin",
		Call(
			"arrayMap",
			Lambda1("i", anchorBaseAtIdxFrag(end, stepNS)),
			Call(
				"range",
				Call("greatest", InlineLit(int64(0)), anchorGridFloorIdxFrag(dist, -rangeNS, stepNS)),
				Call("least", InlineLit(numAnchors), anchorGridFloorIdxFrag(dist, 0, stepNS)),
			),
		),
	)
}

// epochAlignedEndFrag snaps the anchor-grid base `end` down to the nearest
// absolute-epoch multiple of stepNS (phase 0), spelled
//
//	fromUnixTimestamp64Nano(intDiv(toUnixTimestamp64Nano(<end>), <stepNS>) * <stepNS>)
//
// PromQL evaluates a subquery's inner samples at timestamps that are exact
// epoch-multiples of the subquery step (`interval * ((endTs - offset) /
// interval)` in reference engine.go's evalSubquery), independent of any
// offset or the outer request grid. The anchor grid walks back from this
// snapped base by i*stepNS, so every fanned anchor lands on phase 0.
//
// `end` here is the post-offset base (endExprFrag already subtracted the
// offset). CH's intDiv truncates toward zero; epoch-nanos are positive so
// the truncation is a floor — the snapped base is the largest phase-0
// timestamp <= end. Snapping by a multiple of step preserves phase 0
// regardless of offset, so this one wrap handles literal End, now64(9),
// and the offset-shifted form uniformly.
// stepAlignGrid centralises the StepAlign branch shared by the three
// matrix emitters (emitWindowedArrayPairsMatrix / emitWindowedArrayMatrix
// / emitWindowedArrayExtrapolatedMatrix). When r.StepAlign is set, it
// snaps the anchor-grid base to a phase-0 epoch multiple of stepNS (see
// epochAlignedEndFrag); numAnchors is left unchanged.
//
// The anchor count needs no over-provisioning: snapping shifts the base
// down by δ ∈ [0, step), which maps every sample to an anchor index ≤ its
// pre-snap index (dist shrinks by δ < step), so no sample crosses the
// `least(numAnchors, …)` clamp that wasn't already inside it. The oldest
// epoch-multiple any outer window needs sits at index
// (snappedEnd − anchor)/step ≤ (OuterRange − δ)/step ≤ floor(OuterRange/
// step) = numAnchors − 1 < numAnchors — already covered. (A speculative
// +1 here regressed the subquery_over_increase cardinality ratchet
// peak_intermediate 6→7 for zero coverage gain.)
//
// When StepAlign is false the inputs pass through unchanged (byte-stable
// goldens for the outer query_range grid and the Tempo metrics path).
func stepAlignGrid(r *chplan.RangeWindow, end Frag, stepNS, numAnchors int64) (Frag, int64) {
	if !r.StepAlign {
		return end, numAnchors
	}
	return epochAlignedEndFrag(end, stepNS), numAnchors
}

func epochAlignedEndFrag(end Frag, stepNS int64) Frag {
	step := InlineLit(stepNS)
	// fromUnixTimestamp64Nano(intDiv(toUnixTimestamp64Nano(end), step) * step)
	return Call(
		"fromUnixTimestamp64Nano",
		Mul(
			Call("intDiv", Call("toUnixTimestamp64Nano", end), step),
			step,
		),
	)
}

// distBehindAnchorFrag renders `dateDiff('nanosecond', <ts>, <base>)` —
// the row's distance (ns) behind the newest anchor base, the shared
// `dist` term every bounded fan-out feeds into anchorGridFloorIdxFrag.
func distBehindAnchorFrag(ts, base Frag) Frag {
	return Call("dateDiff", InlineLit("nanosecond"), ts, base)
}

// anchorGridFloorIdxFrag renders the floor-division grid index
// `floorDiv(<dist> + <addNS>, <stepNS>) + 1` with CH's
// truncate-toward-zero intDiv corrected into a true floor for negative
// numerators:
//
//	intDiv(<dist>[ ± addNS], toInt64(<stepNS>)) - (modulo(<dist>[ ± addNS], toInt64(<stepNS>)) < 0) + 1
//
// CH's modulo carries the dividend's sign (modulo(-1, 3) = -1), so
// `modulo(x, step) < 0` is 1 exactly when x is negative AND not an
// exact multiple of step — the only case where truncation lands one
// above the floor. addNS = 0 omits the additive term; a negative addNS
// renders as `- |addNS|` (the windowed lower bound's `dist - range`).
//
// The toInt64 wrap on the divisor is load-bearing: a bare step literal
// above the Int32 range parses as UInt64, and CH's modulo(Int64,
// UInt64) reinterprets the negative dividend as unsigned
// (modulo(-59000000000, 60000000000) = 34709551616 on CH 24.8) —
// silently breaking the negative-floor correction and dropping the
// newest anchors. With the signed divisor the dividend's sign survives
// (modulo(-59000000000, toInt64(60000000000)) = -59000000000).
//
// Shared by sampleAnchorFanoutFrag (backward grid, anchors walk back
// from End) and absentOverTimeCoveredAnchorFrag (forward grid, anchors
// walk forward from Start) — the two differ only in how `dist` is
// oriented and which addNS shifts encode their open/closed window
// edges.
func anchorGridFloorIdxFrag(dist Frag, addNS, stepNS int64) Frag {
	// Numerator: `dist`, `dist + addNS`, or `dist - |addNS|`. binOp adds
	// the surrounding single spaces, byte-identical to the old hand-rolled
	// " + " / " - " glue; addNS == 0 leaves the bare dist.
	num := dist
	switch {
	case addNS > 0:
		num = Add(dist, InlineLit(addNS))
	case addNS < 0:
		num = Sub(dist, InlineLit(-addNS))
	}
	step := Call("toInt64", InlineLit(stepNS))
	// intDiv(num, toInt64(step)) - (modulo(num, toInt64(step)) < 0) + 1
	return Add(
		Sub(
			Call("intDiv", num, step),
			Paren(Lt(Call("modulo", num, step), InlineLit(int64(0)))),
		),
		InlineLit(int64(1)),
	)
}

// maybePushInnerScanTimeBounds pushes the (Start - range, End] time
// bound onto `innerSb` (the wrapping SELECT over the MetricsAggregate
// Inner subquery) so ClickHouse can prune partitions / granules by the
// otel_traces Timestamp key — without the bounds the fan-out
// `arrayJoin(range(0, N))` shape would force a full-table scan
// (~31× row blowup per anchor) which routinely outlasts Grafana's
// request timeout on the TraceQL /api/metrics/query_range path.
//
// The pushdown is gated on BOTH Start and End being set — the PromQL
// subquery-internal RangeWindow shapes (Range / Step / OuterRange only,
// no explicit grid) rely on the bounds being absent to stay byte-stable
// against pinned snapshots. The `&&` gate is load-bearing: with either
// bound zero the WHERE clause is suppressed entirely. Shared by
// emitRangeWindowMetrics, emitRangeWindowMetricsQuantileBuckets,
// emitMetricsExemplars, and the PromQL matrix emitters
// (emitWindowedArrayMatrix / emitWindowedArrayExtrapolatedMatrix /
// emitWindowedArrayPairsMatrix) so every matrix-shape emitter keeps a
// single pushdown contract.
//
// Offset (rw.Offset) enters with its sign: the matrix anchors are
// `a_i = (End - Offset) - i·step` (see endExprFrag), so windows live in
// the half-open interval `(Start - Offset - Range, End - Offset]`. The
// scan bound mirrors that interval. Offset == 0 (the common case, and
// every current Tempo caller) reduces to `tsCol > Start - range AND
// tsCol <= End`, keeping those fixtures byte-stable.
func maybePushInnerScanTimeBounds(innerSb *QueryBuilder, rw *chplan.RangeWindow, tsCol string, rangeNS int64) {
	if rw.Start.IsZero() || rw.End.IsZero() {
		return
	}
	lo, hi := innerScanTsBoundsFrags(tsCol, rw.Start, rw.End, rw.Offset.Nanoseconds(), rangeNS)
	innerSb.Where(lo, hi)
}

// innerScanTsBoundsFrags returns the two Frags that pin the input scan
// to the offset-shifted (Start - Offset - range, End - Offset] window:
//
//	<tsCol> >  (<Start> - toIntervalNanosecond(<offsetNS>)) - toIntervalNanosecond(<rangeNS>)
//	<tsCol> <= (<End>   - toIntervalNanosecond(<offsetNS>))
//
// Strict lower / inclusive upper matches the per-anchor `(anchor_ts -
// range, anchor_ts]` window the outer SELECT later applies over the
// offset-shifted anchor grid `a_i = (End - Offset) - i·step`: any row
// that could land in any anchor satisfies
// `tsCol > (Start - Offset) - range AND tsCol <= (End - Offset)`.
//
// offsetNS enters with its sign — a negative offset (Prom's forward-shift
// form, `rate(metric[range] offset -5m)`) renders
// `End - toIntervalNanosecond(-N)` which CH evaluates as `End + N`,
// widening the upper bound to the RIGHT past End exactly as the anchor
// base does; a positive offset shifts the whole interval left. offsetNS
// == 0 omits the shift entirely so the bound collapses to
// `tsCol > Start - range AND tsCol <= End` (byte-stable for the Tempo
// callers, which always pass Offset == 0). See
// maybePushInnerScanTimeBounds for the gating contract callers go
// through.
func innerScanTsBoundsFrags(tsCol string, start, end time.Time, offsetNS, rangeNS int64) (Frag, Frag) {
	startFrag := offsetShiftedTimeFrag(start, offsetNS)
	endFrag := offsetShiftedTimeFrag(end, offsetNS)
	lower := Gt(
		Col(tsCol),
		Sub(startFrag, Call("toIntervalNanosecond", InlineLit(rangeNS))),
	)
	upper := Lte(Col(tsCol), endFrag)
	return lower, upper
}

// instantWindowScanBoundsFrags returns the two scan-prune predicates that
// bound the innermost MergeTree read of an instant (OuterRange == 0)
// windowed-array emitter to the single eval window (end-range, end]:
//
//	<tsCol> >  <end> - toIntervalNanosecond(<rangeNS>)
//	<tsCol> <= <end>
//
// Unlike innerScanTsBoundsFrags (matrix path, anchored on the Start/End grid
// and gated on rw.Start being set), the instant shape is lowered with Start
// ZERO, so the bound is anchored entirely off the single `end` Frag
// (endExprFrag, which already carries r.Offset) and the window `rangeNS`. The
// lower bound is byte-identical to the arrayFilter window lower bound
// (RangeWindowFilter / windowFilterPairsFrag both use the same
// `end - toIntervalNanosecond(rangeNS)`), so the scan reads exactly the rows
// the subsequent arrayFilter would keep — the arrayFilter stays as the precise
// post-groupArray gate, this WHERE just shrinks what the groupArray sees so CH
// can prune granules.
//
// No extrapolation margin is added (margin == 0): Prom's extrapolatedRate
// consults only IN-window samples — durationToStart measures from rangeStart to
// the first in-window sample, and counter-reset detection runs over the
// in-window value array — so a sample at or before the window start never
// participates in the result. Widening the scan past rangeStart would read rows
// the arrayFilter immediately discards, changing nothing but the bytes read.
func instantWindowScanBoundsFrags(tsCol string, end Frag, rangeNS int64) (Frag, Frag) {
	return Gt(Col(tsCol), rangeStartFrag(end, rangeNS)), Lte(Col(tsCol), end)
}

// pushInstantScanBound bounds the innermost groupArray of an instant
// (OuterRange == 0) windowed-array emitter to the single eval window, and
// fail-closes when the RangeWindow has not had its scan-time bound established
// in the IR (RangeWindow.InstantScanBounded). The flag is established once, in
// the IR, by chplan.AttachInstantScanTimeBounds (run at the top of Emit and by
// the optimizer's NormalizeScanTimeBound analyzer rule). The predicate text is
// rendered here via instantWindowScanBoundsFrags — byte-identical to #1098 —
// the flag is only the contract gate.
//
// This guard is the emit-time complement to the optimizer's fail-closed
// RequireScanTimeBound analyzer: it guarantees that a future instant
// windowed-array shape that reaches this emitter without an established bound
// surfaces as a loud error rather than silently regressing to an unbounded
// full-retention groupArray (the #1027 / #1048 / #1056 / #1059 / #1080 /
// #1088 / #1089 / #1098 bug class).
func pushInstantScanBound(innermost *QueryBuilder, r *chplan.RangeWindow, end Frag, rangeNS int64) error {
	if err := requireInstantScanBound(r); err != nil {
		return err
	}
	scanLo, scanHi := instantWindowScanBoundsFrags(r.TimestampColumn, end, rangeNS)
	innermost.Where(scanLo, scanHi)
	return nil
}

// requireInstantScanBound fail-closes when an instant windowed-array leaf
// RangeWindow reaches an emitter without its IR scan-time bound established. It
// is the shared gate for every instant-leaf emit path — the windowed-array
// emitters (via pushInstantScanBound) and the OverTimeDirect instant path —
// so no instant-leaf emitter can render an unbounded innermost scan.
func requireInstantScanBound(r *chplan.RangeWindow) error {
	if !r.InstantScanBounded {
		return fmt.Errorf(
			"%w: instant windowed-array RangeWindow (Func=%q) reached emit without an established scan time bound; "+
				"chplan.AttachInstantScanTimeBounds (or the optimizer's NormalizeScanTimeBound rule) must establish it before emit",
			ErrUnsupported, r.Func,
		)
	}
	return nil
}

// offsetShiftedTimeFrag renders `<t>` shifted left by Offset:
// `(<t> - toIntervalNanosecond(<offsetNS>))` when offsetNS != 0, else
// the bare `<t>` literal. Mirrors endExprFrag's Offset branch so the
// scan bound's End-side anchor base lines up byte-for-byte with the
// anchor grid the fanout walks (both subtract the same
// `toIntervalNanosecond(Offset)` term). A negative offsetNS renders the
// subtraction of a negative interval — CH folds `t - toIntervalNanosecond(-N)`
// to `t + N`, the forward-shift the matrix anchors take.
func offsetShiftedTimeFrag(t time.Time, offsetNS int64) Frag {
	base := timeOrNowFrag(t)
	if offsetNS == 0 {
		return base
	}
	return Paren(Sub(base, Call("toIntervalNanosecond", InlineLit(offsetNS))))
}

// groupArrayPairFrag returns a Frag rendering
// `arraySort(groupArray((<ts>, <val>)))`. The CH idiom that turns a
// per-row scan of a metrics table into a per-series (ts, value) array,
// sorted ascending by ts so subsequent counter-reset arithmetic
// operates in chronological order.
func groupArrayPairFrag(tsCol, valCol string) Frag {
	return Call("arraySort",
		Call("groupArray", Tuple(Col(tsCol), Col(valCol))))
}

// dedupWindowPairsByTsFrag collapses a `arraySort`-ordered
// `Array(Tuple(ts, value))` down to one tuple per distinct timestamp,
// keeping the LAST tuple of each equal-ts run. Because the input is
// sorted ascending by (ts, value), that last-of-run tuple is the
// max-valued sample at the timestamp — byte-for-byte the choice the
// ClickHouse-native `timeSeries*ToGrid` aggregates make (verified
// insertion-order-independent against `timeSeriesRateToGrid`), and the
// single-sample-per-timestamp invariant Prometheus assumes.
//
// OTel/ClickHouse ingestion can write two rows with the same
// (Attributes, TimeUnix); without this collapse `length(window_vals)`
// over-counts samples, shrinking the count-derived average sampling
// interval that feeds Prom's extrapolation cap (extend to the window
// boundary only when the gap < 1.1 × average interval). The deflated
// interval trips the cap when it should not, corrupting the
// extrapolated rate / increase / delta. Deduplicating here makes every
// downstream quantity (length, counter_delta, first/last_ts, first_val)
// count distinct timestamps.
//
// Collapse strategy: `arrayCompact(p -> ts(p), …)` drops every element
// equal (by ts) to its predecessor, keeping the FIRST of each run in a
// single linear pass that never captures `arr` itself — so, unlike an
// `arrayFilter` whose lambda reads `arr[i + 1]`, ClickHouse does not
// replicate the whole window array once per element (that O(n²) blow-up
// per window OOM'd `rate(…[5m])` query_range past the per-query memory
// cap). To keep the LAST (max-valued) tuple of each run rather than the
// first while staying linear, the array is reversed into ts-descending
// order before the compact and reversed back after: arrayCompact then
// keeps the max-valued tuple of each run, and the outer reverse restores
// the ts-ascending order the downstream layers assume — byte-identical
// membership and ordering to the prior arrayFilter form.
func dedupWindowPairsByTsFrag(arr Frag) Frag {
	tsOf := func(t Frag) Frag { return Call("tupleElement", t, InlineLit(int64(1))) }
	return Call(
		"arrayReverse",
		Call(
			"arrayCompact",
			Lambda1("p", tsOf(BareIdent("p"))),
			Call("arrayReverse", arr),
		),
	)
}

// dedupWindowPairsLayer interposes a projection that replaces the
// upstream `window_pairs` column with its timestamp-deduplicated form
// (see dedupWindowPairsByTsFrag), re-projecting the group columns (and
// the per-anchor `anchor_ts` column in the matrix shape) unchanged so
// the downstream mid / extrap / outer layers consume a window array
// that holds one sample per distinct timestamp.
func dedupWindowPairsLayer(upstream Frag, groupFrags []Frag, withAnchor bool) Frag {
	q := NewQuery().From(upstream)
	for _, g := range groupFrags {
		q.Select(g)
	}
	if withAnchor {
		q.Select(Col("anchor_ts"))
	}
	q.Select(As(dedupWindowPairsByTsFrag(BareIdent("window_pairs")), "window_pairs"))
	return q.Frag()
}

// windowFilterPairsFrag returns a Frag rendering the per-series
// arrayFilter clamp to the (end-range, end] window over the
// `series_array` alias. The interval is left-open / right-closed to
// match PromQL range vector selector semantics. Thin wrapper over
// the chsql.RangeWindowFilter typed compound-idiom helper; kept as a
// local helper so the rangeNS-arithmetic stays a single inline literal
// rather than repeated `Sub(end, Call("toIntervalNanosecond", …))`
// boilerplate at every callsite.
//
// end may render arbitrary CH expressions (DateTime64 literal,
// `now64(9)`, or `anchor_ts` in the matrix path); the rangeNS bound
// is inline.
func windowFilterPairsFrag(end Frag, rangeNS int64) Frag {
	start := Sub(end, Call("toIntervalNanosecond", InlineLit(rangeNS)))
	return RangeWindowFilter(start, end, BareIdent("series_array"))
}

// windowLenAtLeastFrag returns a Frag rendering
// `length(<arrCol>) >= <n>` — the predicate the outer SELECT uses to
// drop empty-window rows so PromQL "no sample emitted" semantics
// survive the lowering. arrCol is the array alias projected up from
// the FROM (typically `window_vals` or `window_pairs`); n is the
// per-function minimum sample count (1 for *_over_time, 2 for
// rate / increase / delta / idelta / irate / predict_linear /
// holt_winters).
func windowLenAtLeastFrag(arrCol string, n int) Frag {
	return Gte(Call("length", Col(arrCol)), InlineLit(int64(n)))
}

// counterDeltaFrag returns a Frag rendering
//
//	arraySum(arrayMap((p, c) -> if(c < p, c, c - p),
//	                  arrayPopBack(arrayMap(x -> tupleElement(x, 2), window_pairs)),
//	                  arrayPopFront(arrayMap(x -> tupleElement(x, 2), window_pairs))))
//
// — the counter-reset-aware delta over the window's values. Used by
// the rate / increase value expressions.
//
// The inner 5-function sandwich (the two arrayPopBack/Front layers
// over the value-projection arrayMap, paired by the outer arrayMap)
// is delegated to chsql.CounterDelta — a typed compound-idiom helper.
// The outer arraySum stays here because rate / increase reduce the
// per-pair delta array to a scalar; emitters that wanted the raw delta
// array could call CounterDelta directly.
func counterDeltaFrag() Frag {
	return Call("arraySum", CounterDelta(BareIdent("window_pairs")))
}

// windowValsFrag returns a Frag rendering
// `arrayMap(p -> tupleElement(p, 2), window_pairs)` — the per-window
// values array (the values projected out of the (ts, value) tuples).
func windowValsFrag() Frag {
	return Call(
		"arrayMap",
		Lambda1("p", Call("tupleElement", BareIdent("p"), InlineLit(int64(2)))),
		BareIdent("window_pairs"),
	)
}

// metricsReducerFrag returns the per-bucket reducer Frag for the matrix
// emission path. rate normalises `count(1)` by dividing through the
// range duration in seconds (rendered as a literal — duration constants
// are query-shape, not user-data).
//
// The reducer is always wrapped in `toFloat64(...)` so the projected
// `Value` column has a uniform Float64 wire type — `chclient.Sample.Value`
// is `float64`, and the CH Go driver refuses to coerce UInt64 (the
// natural type of `count()`) or Int64 (the natural type of
// `sum/min/max(Duration)`) into `*float64` at Scan time. Without the
// cast, `| count_over_time() by (...)` against a real ClickHouse
// surfaces as `engine: execute: chclient: scan: (Value) converting
// UInt64 to *float64 is unsupported`. The rate case keeps the cast as
// well even though `count() / N` already promotes to Float64 in CH —
// the uniform wrap is cheaper to reason about than a per-op exception.
func metricsReducerFrag(op chplan.MetricsOp, chName string, params, args []chplan.Expr, rangeSeconds float64) Frag {
	// Translate Attr operand to a metric_arg reference (the alias the
	// inner SELECT projects under) for *_over_time cases.
	argFrags := make([]func(b *Builder), 0, len(args))
	for range args {
		argFrags = append(argFrags, func(b *Builder) { b.Ident("metric_arg") })
	}
	if op == chplan.MetricsOpRate || op == chplan.MetricsOpCountOverTime {
		// args is [LitInt{1}] — pass through verbatim so we emit count(?).
		argFrags = argFrags[:0]
		for _, a := range args {
			expr := a
			argFrags = append(argFrags, func(b *Builder) { _ = b.Expr(expr) })
		}
	}
	paramFrags := make([]func(b *Builder), 0, len(params))
	for _, p := range params {
		expr := p
		paramFrags = append(paramFrags, func(b *Builder) { _ = b.Expr(expr) })
	}

	agg := Call("toFloat64", func(b *Builder) { b.ParamAgg(chName, paramFrags, argFrags) })
	switch op {
	case chplan.MetricsOpRate:
		return Div(agg, InlineLit(rangeSeconds))
	}
	return agg
}

// outerGroupAliases returns the SELECT-list aliases used to refer to
// group-by columns in the outer matrix SELECT. Falls back to a
// "g0", "g1", ... synthetic alias when the source GroupByAliases is
// empty (chplan permits unaliased groups; the matrix shape needs a
// stable handle to thread between subquery and GROUP BY).
func outerGroupAliases(groupBy []chplan.Expr, aliases []string) []string {
	if len(groupBy) == 0 {
		return nil
	}
	out := make([]string, 0, len(groupBy))
	for i := range groupBy {
		if i < len(aliases) && aliases[i] != "" {
			out = append(out, aliases[i])
			continue
		}
		out = append(out, "g"+strconv.Itoa(i))
	}
	return out
}

// emitRangeWindowIdentity emits the "last value in window" shape used
// by bare-vector subqueries (`up[5m:1m]`). Functionally equivalent to
// last_over_time but lowered from a SubqueryExpr rather than a Call.
// Drops anchors whose window is empty (1+ samples required to have a
// "last").
func (e *emitter) emitRangeWindowIdentity(r *chplan.RangeWindow) error {
	return e.emitWindowedArray(r, lastWindowValOrNaNFrag(), 1)
}

// emitRangeWindowLogRate emits SQL for LogQL-style `rate({...}[range])`
// (and `bytes_rate`, after the lowering layer projects `length(Body)`
// as Value): `arraySum(window_vals) / range_seconds`. Distinct from
// PromQL's counter `rate`, which uses counter-reset-aware deltas.
//
// range_seconds binds as a parameter via the value-writer callback so
// the emitter stays free of new Sprintf-on-SQL instances. The
// empty-window guard is delegated to chsql.IfNonZero — defensive
// belt-and-suspenders with the minWindowSize filter below, so a row
// that did sneak through with an empty array would still render `0.0`
// rather than dividing by zero.
//
// Empty windows are dropped from the result (matrix and instant
// alike): Loki's batchRangeVectorIterator only keeps series whose
// window holds 1+ samples (range_vector.go::popBack deletes series
// once their window goes empty; At() then emits one Sample per
// remaining series). The outer step grid therefore exposes one row
// per (series, anchor) tuple where the inner window has at least one
// matching log line — anchors whose window was empty contribute no
// row to the matrix at all. Mirroring that requires
// minWindowSize = 1 so the WHERE clause drops the empty-window rows
// rather than zero-filling every step grid anchor. Without this an
// outer `sum(rate({...}[5m]))` over a sparse stream produces one row
// per step grid anchor (e.g. 1441 anchors for a 24h/1m grid) instead
// of one row per anchor with contributing samples — see the
// matrix-length drift the loki-compat suite flagged
// (`matrix[0] series length: expected=1382 actual=1441`).
func (e *emitter) emitRangeWindowLogRate(r *chplan.RangeWindow) error {
	rangeSeconds := r.Range.Seconds()
	return e.emitWindowedArray(r, IfNonZero(
		Call("arraySum", BareIdent("window_vals")),
		Lit(rangeSeconds),
	), 1)
}

// emitRangeWindowRate emits SQL for `rate(metric[range])`.
//
// Routes through emitWindowedArrayExtrapolated so the per-window value
// applies Prom's `extrapolatedRate` boundary correction (mirrors
// prometheus/promql/functions.go::extrapolatedRate) on top of the
// counter-reset-aware delta.
//
// PromQL rate drops series whose window holds fewer than 2 samples
// (matches Prom's funcRate / extrapolatedRate). The outer SELECT gets
// `WHERE length(window_vals) >= 2`.
func (e *emitter) emitRangeWindowRate(r *chplan.RangeWindow) error {
	return e.emitWindowedArrayExtrapolated(r, extrapolationKindRate)
}

// emitRangeWindowIncrease emits SQL for `increase(metric[range])`. Same
// counter-reset arithmetic + extrapolation as rate but without dividing
// by range_seconds (matches Prom's funcIncrease).
//
// PromQL increase drops series whose window holds fewer than 2 samples.
func (e *emitter) emitRangeWindowIncrease(r *chplan.RangeWindow) error {
	return e.emitWindowedArrayExtrapolated(r, extrapolationKindIncrease)
}

// emitRangeWindowOverTime emits SQL for the `*_over_time` family:
// sum_over_time, avg_over_time, min_over_time, max_over_time,
// count_over_time, first_over_time, last_over_time, stddev_over_time,
// stdvar_over_time, mad_over_time.
// These don't need counter-reset handling — they're straight array
// aggregations over the window's values.
//
// mad_over_time is the experimental median-absolute-deviation reducer
// (median(|x - median(x)|)); it reuses the linear-interpolation median
// helper (medianOverArrayFrag) over window_vals. The timestamp-returning
// siblings (ts_of_*_over_time) need the per-sample timestamps and so route
// through emitRangeWindowTsOfOverTime, not this values-only path.
//
// stddev_over_time / stdvar_over_time use a manual two-pass population
// variance expression — `arraySum((x - μ)²) / N` with `μ = arrayAvg(vals)`
// — instead of `arrayReduce('stddevPop' | 'varPop', ...)`. The CH
// aggregate uses the textbook one-pass `E[X²] − (E[X])²` formula which
// suffers catastrophic cancellation at value scale 2^31 (the
// `demo_memory_usage_bytes` regime), causing ULP-level drift vs
// Prometheus's `Engine.evalAggrOverTime → varianceOverTime` Welford
// estimator (see issue #400 bucket 1). The two-pass form recovers
// precision by first computing the mean exactly and then summing
// squared deviations against it — `arrayWithConstant(N, μ)` broadcasts
// μ across the window so `arrayMap` evaluates the centred sample once
// per element. Divisor N matches Prom (population variance).
func (e *emitter) emitRangeWindowOverTime(r *chplan.RangeWindow) error {
	// Fast path: the incrementally-reducible *_over_time funcs whose
	// result is reduction-order-independent (min/max/count/present) need
	// no per-window sample array at all — a direct CH group aggregate over
	// window membership produces byte-identical results. Dropping the
	// groupArray + arraySort + window_vals materialisation removes the
	// per-(series, anchor) array fan-out for these funcs (recorded as a
	// fan_factor drop in the cardinality baseline). sum/avg_over_time stay
	// on the array path to preserve the exact arraySum/arrayAvg reduction
	// order (float addition is non-associative — see overTimeDirectAggFrag);
	// quantile_over_time (arraySort interpolation) and stddev/stdvar_over_time
	// (two-pass moments over the array) genuinely need the materialised
	// values; first/last_over_time keep the array's deterministic
	// duplicate-ts tie-break (window_vals[1] / window_vals[N] over the
	// arraySort-by-(ts, value) order) that argMin/argMax don't replicate.
	if agg, ok := overTimeDirectAggFrag(r.Func, r.ValueColumn); ok {
		return e.emitRangeWindowOverTimeDirect(r, agg)
	}
	// Two-pass population variance: μ = arrayAvg(vals); Σ(x - μ)² / N.
	// arrayWithConstant materialises the broadcast mean exactly once
	// per row so the lambda doesn't re-evaluate arrayAvg per element.
	varPopTwoPass := varPopTwoPassFrag()
	var inner Frag
	switch r.Func {
	case "sum_over_time":
		inner = Call("arraySum", BareIdent("window_vals"))
	case "avg_over_time":
		inner = nonEmptyWindowOrNaNFrag(Call("arrayAvg", BareIdent("window_vals")))
	case "min_over_time":
		inner = nonEmptyWindowOrNaNFrag(Call("arrayMin", BareIdent("window_vals")))
	case "max_over_time":
		inner = nonEmptyWindowOrNaNFrag(Call("arrayMax", BareIdent("window_vals")))
	case "count_over_time":
		inner = Call("toFloat64", Call("length", BareIdent("window_vals")))
	case "present_over_time":
		// PromQL present_over_time(v[range]) emits 1 for every series
		// with ≥1 sample in the window (prometheus/promql/functions.go::
		// funcPresentOverTime — it appends a single `1` per series with a
		// non-empty window). The shared `WHERE length(window_vals) >= 1`
		// outer filter below already drops empty-window series, so the
		// per-window value is the constant 1.
		inner = Call("toFloat64", InlineLit(int64(1)))
	case "first_over_time":
		// LogQL `first_over_time(... | unwrap v [r])` — the value of the
		// time-EARLIEST sample in the window (Loki's FirstOverTime
		// streaming aggregator / `first` batch fn, pkg/logql/
		// range_vector.go). window_vals is time-sorted (arraySort over
		// (ts, value) tuples upstream), so element 1 is the earliest.
		inner = nonEmptyWindowOrNaNFrag(Subscript(BareIdent("window_vals"), InlineLit(int64(1))))
	case "last_over_time":
		inner = lastWindowValOrNaNFrag()
	case "mad_over_time":
		// PromQL mad_over_time(v[range]) = median(|x - median(x)|)
		// (prometheus/promql/functions.go::funcMadOverTime). Prometheus's
		// median is the linear-interpolation quantile(0.5) over the SORTED
		// values, so we mirror it with the same lower/upper blend the
		// computed-phi quantile_over_time path uses (phi=0.5), applied
		// twice: once over window_vals for the inner median, once over the
		// absolute deviations. Empty windows are dropped by the shared
		// outer WHERE length(window_vals) >= 1 below.
		med := medianOverArrayFrag(BareIdent("window_vals"))
		devs := Call(
			"arrayMap",
			Lambda1("x", Call("abs", Sub(BareIdent("x"), med))),
			BareIdent("window_vals"),
		)
		inner = nonEmptyWindowOrNaNFrag(medianOverArrayFrag(devs))
	case "stddev_over_time":
		// Empty window → drop the series (Prom returns no sample).
		// We mirror with NaN; the engine layer treats NaN as "drop"
		// when projecting samples. Single-sample windows render the
		// population stddev which is 0 (Σ (x − μ)² with N=1 and
		// μ=x gives 0) — matches Prom's Welford estimator.
		//
		// Two-pass variance under a sqrt: see the package comment
		// above for the precision rationale (CH varPop one-pass loses
		// precision at value scale ≥ 2^31; #400 bucket 1).
		inner = nonEmptyWindowOrNaNFrag(Call("sqrt", varPopTwoPass))
	case "stdvar_over_time":
		// Population variance (divides by N, not N-1) to match
		// Prometheus's funcStdvarOverTime / varianceOverTime. Same
		// empty-window contract as stddev_over_time: drop the series
		// (we emit NaN; the engine treats NaN as "drop"). Single-sample
		// window renders 0 (Σ (x − μ)² with μ=x is 0).
		//
		// Two-pass variance: see the package comment above for the
		// precision rationale (CH varPop one-pass loses precision at
		// value scale ≥ 2^31; #400 bucket 1).
		inner = nonEmptyWindowOrNaNFrag(varPopTwoPass)
	default:
		return fmt.Errorf("%w: over-time function %q", ErrUnsupported, r.Func)
	}
	// Every *_over_time variant drops empty-window rows per Prom
	// semantics (Prom's funcSumOverTime / funcCountOverTime / etc. all
	// short-circuit on zero samples). The outer SELECT gets
	// `WHERE length(window_vals) >= 1`.
	return e.emitWindowedArray(r, inner, 1)
}

// nonEmptyWindowOrNaNFrag wraps a per-window value in the PromQL
// drop-empty-window guard `if(length(window_vals) > 0, <val>, nan)` —
// the shared shape the *_over_time family uses so an empty window
// surfaces NaN (which the engine layer treats as "drop the series").
func nonEmptyWindowOrNaNFrag(val Frag) Frag {
	return Call(
		"if",
		Gt(Call("length", BareIdent("window_vals")), InlineLit(int64(0))),
		val,
		BareIdent("nan"),
	)
}

// lastWindowValOrNaNFrag renders `if(length(window_vals) > 0,
// window_vals[length(window_vals)], nan)` — the time-LATEST sample in
// the window (last_over_time / bare-subquery identity), or NaN for an
// empty window. window_vals is arraySort-ordered so the final element
// is the newest sample.
func lastWindowValOrNaNFrag() Frag {
	return nonEmptyWindowOrNaNFrag(
		Subscript(BareIdent("window_vals"), Call("length", BareIdent("window_vals"))),
	)
}

// varPopTwoPassFrag renders the two-pass population variance
// `arraySum(arrayMap((x, m) -> (x - m) * (x - m), window_vals,
// arrayWithConstant(length(window_vals), arrayAvg(window_vals)))) /
// length(window_vals)`. The broadcast mean is materialised once via
// arrayWithConstant so the lambda doesn't re-evaluate arrayAvg per
// element. Shared by stddev_over_time (under sqrt) and stdvar_over_time.
func varPopTwoPassFrag() Frag {
	diff := Paren(Sub(BareIdent("x"), BareIdent("m")))
	body := Mul(diff, diff)
	means := Call("arrayWithConstant",
		Call("length", BareIdent("window_vals")),
		Call("arrayAvg", BareIdent("window_vals")))
	sumSq := Call("arraySum",
		Call("arrayMap", Lambda2("x", "m", body), BareIdent("window_vals"), means))
	return Div(sumSq, Call("length", BareIdent("window_vals")))
}

// medianOverArrayFrag renders Prometheus's `quantile(0.5, values)`
// (prometheus/promql/quantile.go) — a linear-interpolation median over
// the SORTED array `arr`:
//
//	rank   = 0.5 * (N - 1)
//	lower  = sorted[floor(rank) + 1]            (1-based CH index)
//	upper  = sorted[least(N-1, floor(rank)+1) + 1]
//	median = lower*(1-weight) + upper*weight,   weight = rank - floor(rank)
//
// Mirrors the computed-phi quantile_over_time interpolation
// (emitRangeWindowQuantileOverTime) specialised to phi=0.5, so
// mad_over_time's inner/outer medians match Prom bit-for-bit on finite
// windows. `arr` is any Array(Float64) expression; it is sorted here, so
// callers pass the raw values (the absolute-deviation array for the
// outer median) without pre-sorting.
func medianOverArrayFrag(arr Frag) Frag {
	const medianPhi = 0.5
	sorted := Call("arraySort", arr)
	nMinus := Call("toFloat64", Sub(Call("length", arr), InlineLit(int64(1))))
	rank := func() Frag { return Paren(Mul(InlineLit(medianPhi), nMinus)) }
	floorRank := func() Frag { return Call("floor", rank()) }
	lowerIdx := Add(Call("toUInt32", floorRank()), InlineLit(int64(1)))
	upperIdx := Add(
		Call("toUInt32", Call("least", nMinus, Add(floorRank(), InlineLit(int64(1))))),
		InlineLit(int64(1)),
	)
	weight := func() Frag { return Paren(Sub(rank(), floorRank())) }
	lowerTerm := Mul(Subscript(sorted, lowerIdx), Paren(Sub(InlineLit(int64(1)), weight())))
	upperTerm := Mul(Subscript(sorted, upperIdx), weight())
	return Add(lowerTerm, upperTerm)
}

// emitRangeWindowTsOfOverTime emits SQL for the experimental timestamp
// `ts_of_*_over_time` family:
//
//   - ts_of_first_over_time(v[r]) → epoch-seconds timestamp of the
//     time-EARLIEST sample in the window.
//   - ts_of_last_over_time(v[r])  → epoch-seconds timestamp of the
//     time-LATEST sample in the window.
//   - ts_of_max_over_time(v[r])   → epoch-seconds timestamp of the
//     MAXIMUM-valued sample (argmax; ties resolve to the LAST equal-max
//     sample in time order — Prom's `cur >= maxVal` forward scan).
//   - ts_of_min_over_time(v[r])   → epoch-seconds timestamp of the
//     MINIMUM-valued sample (argmin; ties → last equal-min, Prom's
//     `cur <= minVal`).
//
// Reference: prometheus/promql/functions.go::funcTsOf{First,Last}OverTime
// + compareOverTime(returnTimestamp=true). Prometheus returns the
// millisecond sample timestamp divided by 1000 (epoch seconds, ms
// precision); we mirror with `toFloat64(toUnixTimestamp64Milli(ts)) / 1000`
// so a whole-second seed renders an exact integer and sub-second scrapes
// keep ms precision. These reducers need the per-sample timestamps, so
// they route through the window_pairs path (Array(Tuple(ts, value)))
// rather than the values-only window_vals path. Empty windows are
// dropped (minWindowSize = 1) — Prom's matrix selector excludes
// sample-less series before the function runs.
func (e *emitter) emitRangeWindowTsOfOverTime(r *chplan.RangeWindow) error {
	var tsExpr Frag
	switch r.Func {
	case "ts_of_first_over_time":
		// window_pairs is arraySort-ordered by (ts, value), so element 1
		// is the earliest sample; tuple element 1 is its timestamp.
		tsExpr = tupleTsFrag(Subscript(BareIdent("window_pairs"), InlineLit(int64(1))))
	case "ts_of_last_over_time":
		tsExpr = tupleTsFrag(
			Subscript(BareIdent("window_pairs"), Call("length", BareIdent("window_pairs"))),
		)
	case "ts_of_max_over_time":
		// Timestamp of the max-valued sample; on a value tie the LATEST
		// sample wins (Prom's forward `cur >= maxVal`). See tsOfExtremeFrag
		// for why a composite (value, tsNanos) key is needed (plain CH
		// argMax keeps the FIRST tie, diverging from Prom).
		tsExpr = tsOfExtremeFrag(true)
	case "ts_of_min_over_time":
		// Timestamp of the min-valued sample; latest wins on a value tie
		// (Prom's `cur <= minVal`).
		tsExpr = tsOfExtremeFrag(false)
	default:
		return fmt.Errorf("%w: ts-of-over-time function %q", ErrUnsupported, r.Func)
	}
	return e.emitWindowedArrayPairs(r, tsExpr, 1)
}

// tupleTsFrag renders the epoch-seconds (ms-precision) value of a
// window-pair tuple's timestamp element (tuple element 1, a
// DateTime64(9)): `toFloat64(toUnixTimestamp64Milli(tupleElement(p, 1))) / 1000`.
// Matches Prometheus's `float64(sample.T) / 1000` (T is the ms
// timestamp). Shared by ts_of_first / ts_of_last.
func tupleTsFrag(pair Frag) Frag {
	ts := Call("tupleElement", pair, InlineLit(int64(1)))
	return tsEpochSecondsFrag(ts)
}

// tsEpochSecondsFrag renders `toFloat64(toUnixTimestamp64Milli(<ts>)) / 1000`
// — the DateTime64(9) → epoch-seconds (ms-precision) conversion the
// ts_of_*_over_time family returns. toUnixTimestamp64Milli truncates to
// milliseconds first so the result equals Prometheus's millisecond
// `sample.T / 1000` exactly (no DateTime64 nanosecond tail leaking a
// sub-millisecond fraction Prom never sees).
func tsEpochSecondsFrag(ts Frag) Frag {
	const millisPerSecond = 1000
	return Div(
		Call("toFloat64", Call("toUnixTimestamp64Milli", ts)),
		InlineLit(int64(millisPerSecond)),
	)
}

// tsOfExtremeFrag renders the epoch-seconds timestamp of the
// value-extreme sample in the window via CH's argMax over a COMPOSITE
// (value, timestamp) comparison key, matching Prometheus's tie-break:
//
//	toFloat64(toUnixTimestamp64Milli(
//	  arrayReduce('argMax',
//	              arrayMap(p -> tupleElement(p, 1), window_pairs),       -- the ts to return
//	              arrayMap(p -> (<keyVal>, toUnixTimestamp64Nano(tupleElement(p, 1))), window_pairs)
//	  ))) / 1000
//
// Prometheus's compareOverTime scans the window in time order and
// updates on `cur >= maxVal` (ts_of_max) / `cur <= minVal` (ts_of_min),
// so on a VALUE tie the LATEST sample wins. CH's plain
// `argMax(ts, value)` instead returns the FIRST tie (verified on chDB:
// argMax([10,20,30],[7,9,9]) = 20, not 30), which would diverge from
// Prom. To recover Prom's last-equal tie-break we maximise a composite
// key `(keyVal, tsNanos)`: the timestamp component breaks value ties in
// favour of the larger timestamp, i.e. the time-latest equal-extreme
// sample. ts_of_max uses keyVal = value (maximise value); ts_of_min
// uses keyVal = -value (maximise -value = minimise value), keeping the
// SAME `(…, +tsNanos)` tie component so the latest equal-min wins —
// exactly Prom's `cur <= minVal` forward scan.
func tsOfExtremeFrag(wantMax bool) Frag {
	val := func() Frag { return Call("tupleElement", BareIdent("p"), InlineLit(int64(2))) }
	keyVal := val()
	if !wantMax {
		keyVal = Neg(val())
	}
	tsNanos := Call("toUnixTimestamp64Nano", Call("tupleElement", BareIdent("p"), InlineLit(int64(1))))
	tsArr := Call(
		"arrayMap",
		Lambda1("p", Call("tupleElement", BareIdent("p"), InlineLit(int64(1)))),
		BareIdent("window_pairs"),
	)
	keyArr := Call(
		"arrayMap",
		Lambda1("p", Tuple(keyVal, tsNanos)),
		BareIdent("window_pairs"),
	)
	extremeTs := Call("arrayReduce", InlineLit("argMax"), tsArr, keyArr)
	return tsEpochSecondsFrag(extremeTs)
}

// overTimeDirectAggFrag returns the direct CH group-aggregate Frag for an
// incrementally-reducible *_over_time func that the direct path can render
// BYTE-IDENTICALLY, and whether the func is on the direct-aggregate fast
// path at all.
//
// The aggregate runs over the per-(series[, anchor]) GROUP BY of the
// window-membership rows, so it produces exactly the value the array
// reduce produced over `window_vals` — but with no array materialised:
//
//   - min_over_time   → min(Value)        (arrayMin — a SELECTION, no
//     summation, so the result is the same element regardless of
//     reduction order)
//   - max_over_time   → max(Value)        (arrayMax — likewise a selection)
//   - count_over_time → toFloat64(count()) (toFloat64(length(...)) — an
//     exact integer cardinality)
//   - present_over_time → toFloat64(1)    (constant per non-empty group)
//
// count/present are wrapped in toFloat64 so the Value column keeps the
// uniform Float64 wire type chclient.Sample.Value expects (count() is
// UInt64; the CH Go driver refuses to coerce it into *float64 at Scan
// time — same rationale as metricsReducerFrag).
//
// sum_over_time / avg_over_time are deliberately NOT on the direct path
// even though they are incrementally reducible: CH's streaming sum()/avg()
// accumulate floats in scan/merge order, whereas the array path's
// arraySum()/arrayAvg() reduce over the arraySort-by-(ts, value) order.
// Float addition is non-associative, so the two differ in the final ULP on
// non-integer inputs (observed on avg_over_time(rate(...)) where the rate
// values are fractional — `0.14814814814814814` vs `...817`). Keeping them
// on the array path preserves byte-identical results; this optimization is
// pure-performance and must not perturb a single output bit.
//
// quantile/stddev/stdvar/first/last_over_time also return ok=false and
// stay on the array path (see emitRangeWindowOverTime for why).
func overTimeDirectAggFrag(fn, valueCol string) (Frag, bool) {
	switch fn {
	case "min_over_time":
		return Call("min", Col(valueCol)), true
	case "max_over_time":
		return Call("max", Col(valueCol)), true
	case "count_over_time":
		return Call("toFloat64", Call("count")), true
	case "present_over_time":
		return Call("toFloat64", InlineLit(int64(1))), true
	}
	return nil, false
}

// emitRangeWindowOverTimeDirect renders an incrementally-reducible
// *_over_time func (see overTimeDirectAggFrag) as a direct CH group
// aggregate over the window-membership rows — no per-window sample array.
//
// Instant mode (OuterRange == 0): the window predicate
// `(end - range, end]` is pushed into a WHERE over the input scan, then a
// single `GROUP BY <series>` applies the direct aggregate. A series with
// no rows in the window produces no group, which matches the array path's
// `WHERE length(window_vals) >= 1` empty-window drop. Compare the old
// shape (groupArray((ts,val)) → arrayFilter → window_vals → arraySum):
//
//	SELECT <series>, <agg(Value)> AS Value
//	FROM (<input>)
//	WHERE <ts> > end - range AND <ts> <= end
//	GROUP BY <series>
//
// Matrix mode (OuterRange > 0) is delegated to the matrix variant.
func (e *emitter) emitRangeWindowOverTimeDirect(r *chplan.RangeWindow, agg Frag) error {
	if r.TimestampColumn == "" {
		return fmt.Errorf("%w: RangeWindow.TimestampColumn unset", ErrUnsupported)
	}
	if r.ValueColumn == "" {
		return fmt.Errorf("%w: RangeWindow.ValueColumn unset", ErrUnsupported)
	}
	if r.OuterRange > 0 {
		if r.Step <= 0 {
			return fmt.Errorf("%w: RangeWindow.OuterRange > 0 requires Step > 0", ErrUnsupported)
		}
		return e.emitRangeWindowOverTimeDirectMatrix(r, agg)
	}

	// Memory-bounded fused path for instant PromQL subqueries
	// `<reducer>(rate|increase|delta(m[range])[outer:step])`: collapse the
	// inner matrix regroup + outer reducer regroup into a single
	// `GROUP BY <series>` over one per-series samples array (see
	// range_window_fused.go). Non-fusible shapes fall through unchanged.
	if handled, err := e.tryEmitFusedInstantSubquery(r); handled || err != nil {
		return err
	}

	end := endExprFrag(r)
	rangeNS := r.Range.Nanoseconds()
	groupFrags, err := e.collectGroupByFrags(r.GroupBy)
	if err != nil {
		return err
	}

	inner, err := e.subqueryFrag(r.Input)
	if err != nil {
		return err
	}

	sb := NewQuery().From(inner)
	for _, g := range groupFrags {
		sb.Select(g)
	}
	sb.Select(As(agg, r.ValueColumn))
	// The (end - range, end] window predicate the array path applied via
	// arrayFilter over the (ts, value) tuples becomes a row-level WHERE:
	// left-open / right-closed, identical bounds. This direct path is an
	// instant windowed-array leaf too (IsInstantWindowedLeaf), so it shares
	// the fail-closed contract — refuse to emit an unbounded scan unless the
	// IR scan-time bound was established.
	if err := requireInstantScanBound(r); err != nil {
		return err
	}
	winStart := Sub(end, Call("toIntervalNanosecond", InlineLit(rangeNS)))
	sb.Where(
		Gt(Col(r.TimestampColumn), winStart),
		Lte(Col(r.TimestampColumn), end),
	)
	sb.GroupBy(groupFrags...)

	e.emitSelect(sb)
	return nil
}

// emitRangeWindowOverTimeDirectMatrix is the OuterRange > 0 variant of
// emitRangeWindowOverTimeDirect: each series emits one row per anchor
// (across [End-OuterRange, End] spaced by Step, end-inclusive) whose
// window holds at least one sample. The sample-side arrayJoin fanout (the
// membership mechanism — each input row fans across only the anchors
// whose `(anchor - range, anchor]` window contains its timestamp; see
// sampleAnchorFanoutFrag) is kept; the per-(series, anchor) regroup
// applies the direct aggregate INSTEAD of rebuilding the window array via
// groupArray + arraySort + window_vals. Anchors whose window is empty
// produce no group (matching the array path's empty-window drop).
//
// SQL skeleton (with N = OuterRange/Step + 1):
//
//	SELECT <series>, anchor_ts, anchor_ts AS <ts>, <agg(Value)> AS Value
//	FROM (
//	  SELECT <series>, <ts>, Value,
//	    arrayJoin(arrayMap(i -> <end> - toIntervalNanosecond(i * <step_ns>),
//	              range(<covered-anchor index bounds>))) AS anchor_ts
//	  FROM (<input>)
//	)
//	GROUP BY <series>, anchor_ts
func (e *emitter) emitRangeWindowOverTimeDirectMatrix(r *chplan.RangeWindow, agg Frag) error {
	end := endExprFrag(r)
	rangeNS := r.Range.Nanoseconds()
	stepNS := r.Step.Nanoseconds()
	numAnchors := r.OuterRange.Nanoseconds()/stepNS + 1
	groupFrags, err := e.collectGroupByFrags(r.GroupBy)
	if err != nil {
		return err
	}

	innerSub, err := e.subqueryFrag(r.Input)
	if err != nil {
		return err
	}
	innerSub, srcTs := fanoutTsSource(innerSub, r.TimestampColumn)

	// Sample-fanout SELECT — one row per (sample, covered anchor).
	fanout := NewQuery().From(innerSub)
	for _, g := range groupFrags {
		fanout.Select(g)
	}
	fanout.Select(Col(srcTs))
	fanout.Select(Col(r.ValueColumn))
	fanout.Select(As(
		sampleAnchorFanoutFrag(end, Col(srcTs), stepNS, rangeNS, numAnchors),
		"anchor_ts",
	))
	maybePushInnerScanTimeBounds(fanout, r, srcTs, rangeNS)

	// Regroup SELECT — direct aggregate per (series, anchor). No array.
	regroup := NewQuery().From(fanout.Frag())
	for _, g := range groupFrags {
		regroup.Select(g)
	}
	regroup.Select(Col("anchor_ts"))
	// Surface anchor_ts under the schema timestamp column so a wrapping
	// Aggregate's per-step GROUP BY (ColumnRef{TimestampColumn}) resolves
	// — mirrors emitWindowedArrayMatrix's outer projection.
	if r.TimestampColumn != "" && r.TimestampColumn != "anchor_ts" {
		regroup.Select(As(verbatim("anchor_ts"), r.TimestampColumn))
	}
	// The aggregate references srcTs/ValueColumn; in the nested-matrix
	// rename case (fanoutTsSource) it operates over Value only, so the
	// rename of the timestamp column doesn't affect these order-free
	// reducers.
	regroup.Select(As(agg, r.ValueColumn))
	regroupKeys := make([]Frag, 0, len(groupFrags)+1)
	regroupKeys = append(regroupKeys, groupFrags...)
	regroupKeys = append(regroupKeys, Col("anchor_ts"))
	regroup.GroupBy(regroupKeys...)

	e.emitSelect(regroup)
	return nil
}

// emitRangeWindowDeriv emits SQL for `deriv(v[range])`.
//
// PromQL: returns the per-second slope of a least-squares linear fit
// over the samples in the window. The fit's x-axis is the per-sample
// seconds-from-anchor (negative-going as you walk backwards in time);
// the y-axis is the sample value. We piggy-back on the same
// `simpleLinearRegression` aggregate used by predict_linear and pull
// out tuple element 2 (the slope; element 1 is the intercept).
//
// PromQL behaviour: < 2 samples in the window → drop the series (Prom
// emits no sample). We emit NaN there; the engine layer treats NaN as
// "drop". Additionally, the outer SELECT gets `WHERE length(window_pairs) >= 2`
// so single-sample series don't reach the projection.
//
// Mirrors emitRangeWindowPredictLinear's structure — same xs/ys
// arrayMap construction, same `simpleLinearRegression` arrayReduce
// idiom — but pulls slope only (no `+ slope * t` horizon arithmetic
// and no t scalar).
func (e *emitter) emitRangeWindowDeriv(r *chplan.RangeWindow) error {
	// In matrix mode each row's anchor is `anchor_ts` from the arrayJoin
	// fanout; in instant mode it's r.End (or now64(9) for zero-time).
	// Use the factory form so the per-sample seconds-from-anchor
	// computation references the correct per-row anchor.
	writer := func(anchor Frag) Frag {
		slope := Call("tupleElement", windowPairsSLRFrag(anchor), InlineLit(int64(1)))
		return Call(
			"if",
			Gt(Call("length", BareIdent("window_pairs")), InlineLit(int64(1))),
			slope,
			BareIdent("nan"),
		)
	}
	return e.emitWindowedArrayPairsAnchored(r, writer, 2)
}

// emitRangeWindowResets emits SQL for `resets(v[range])`.
//
// PromQL: returns the count of counter-reset events in the window — a
// reset is any adjacent pair (prev, curr) where `curr < prev`. The
// result is rendered as a Float64 to match the wire type the engine
// projects.
//
// Empty window → drop the series (Prom emits no sample). Single-sample
// windows render 0 (no adjacent pairs to compare). The outer SELECT
// drops empty-window rows via `WHERE length(window_vals) >= 1`.
//
// Implementation: the standard arrayPopBack/arrayPopFront sandwich
// gives parallel `prev` / `curr` lists; an arrayMap with a per-pair
// `if(curr < prev, 1, 0)` indicator + arraySum reduces to the count.
func (e *emitter) emitRangeWindowResets(r *chplan.RangeWindow) error {
	value := Cast(
		Call(
			"arraySum",
			Call(
				"arrayMap",
				Lambda2("p", "c", If(
					Lt(BareIdent("c"), BareIdent("p")),
					InlineLit(int64(1)),
					InlineLit(int64(0)),
				)),
				Call("arrayPopBack", BareIdent("window_vals")),
				Call("arrayPopFront", BareIdent("window_vals")),
			),
		),
		"Float64",
	)
	return e.emitWindowedArray(r, value, 1)
}

// emitRangeWindowChanges emits SQL for `changes(v[range])`.
//
// PromQL: returns the count of value changes in the window — any
// adjacent pair (prev, curr) where `curr != prev`. Like resets, the
// result is a Float64 count.
//
// Empty window → drop the series. Single-sample windows render 0
// (no adjacent pairs). The outer SELECT drops empty-window rows via
// `WHERE length(window_vals) >= 1`.
//
// Implementation mirrors emitRangeWindowResets but with `c != p` as
// the per-pair indicator. Prom's funcChanges has an additional
// `!(NaN(curr) && NaN(prev))` carve-out so a NaN-on-both-sides pair
// is not counted as a change; we accept the divergence on float-NaN
// streams (rare in practice, and the goldens cover only finite values).
func (e *emitter) emitRangeWindowChanges(r *chplan.RangeWindow) error {
	value := Cast(
		Call(
			"arraySum",
			Call(
				"arrayMap",
				Lambda2("p", "c", If(
					Neq(BareIdent("c"), BareIdent("p")),
					InlineLit(int64(1)),
					InlineLit(int64(0)),
				)),
				Call("arrayPopBack", BareIdent("window_vals")),
				Call("arrayPopFront", BareIdent("window_vals")),
			),
		),
		"Float64",
	)
	return e.emitWindowedArray(r, value, 1)
}

// emitRangeWindowDelta emits SQL for `delta(v[range])`: the
// extrapolation-corrected difference between the LAST and FIRST samples
// in the window. Unlike `increase`, delta is meant for gauges (no
// counter-reset arithmetic and no clamp-to-zero), but it still receives
// the same boundary-extrapolation correction Prom applies via its
// shared `extrapolatedRate(isCounter=false, isRate=false)` helper.
//
// PromQL `delta` returns NaN when the window holds fewer than 2
// samples — same as Prom's `funcDelta`.
func (e *emitter) emitRangeWindowDelta(r *chplan.RangeWindow) error {
	return e.emitWindowedArrayExtrapolated(r, extrapolationKindDelta)
}

// emitRangeWindowIDelta emits SQL for `idelta(v[range])`: the
// difference between the LAST TWO samples in the window. Like
// `delta`, no counter-reset arithmetic.
//
// `idelta` returns NaN when the window holds fewer than 2 samples
// (matches Prom's `funcIdelta`).
func (e *emitter) emitRangeWindowIDelta(r *chplan.RangeWindow) error {
	n := Call("length", BareIdent("window_vals"))
	last := Subscript(BareIdent("window_vals"), n)
	prev := Subscript(BareIdent("window_vals"), Sub(n, InlineLit(int64(1))))
	expr := Call(
		"if",
		Gt(n, InlineLit(int64(1))),
		Sub(last, prev),
		BareIdent("nan"),
	)
	// PromQL idelta drops series whose window holds fewer than 2
	// samples (matches Prom's funcIdelta).
	return e.emitWindowedArray(r, expr, 2)
}

// emitRangeWindowIRate emits SQL for `irate(v[range])`: per-second
// instantaneous rate using ONLY the last two samples in the window.
//
//	irate = if(c >= p, c - p, c) / (last_ts - prev_ts)
//
// The numerator is counter-reset aware (`if(c < p, c, c - p)`) and
// the denominator is the time between the two samples in seconds.
// PromQL's `funcIrate` returns NaN if there are fewer than 2 samples
// in the window.
func (e *emitter) emitRangeWindowIRate(r *chplan.RangeWindow) error {
	// We need both the last two values and the last two timestamps,
	// so reach for `window_pairs` (Array(Tuple(ts, value))) via
	// emitWindowedArrayPairs rather than the values-only
	// emitWindowedArray path. PromQL irate drops series whose window
	// holds fewer than 2 samples (matches Prom's funcIrate).
	return e.emitWindowedArrayPairs(r, irateValueFrag(), 2)
}

// irateValueExpr renders the irate per-window value expression. Operates
// on `window_pairs` (Array(Tuple(DateTime64(9), Float64))). The two
// most recent samples are at positions length(window_pairs) - 1 and
// length(window_pairs); the rate is the counter-reset-aware delta
// divided by the per-sample interval in seconds.
//
// CH note: dateDiff('second', earlier, later) returns an Int32 that
// loses sub-second precision. For sub-second sample intervals (rare
// in PromQL but possible with high-resolution scrapes), the
// dateDiff('nanosecond', earlier, later) flavour returns the gap in
// nanoseconds; divide by 1e9 to get fractional seconds. We use the
// nanosecond flavour so the result agrees with Prometheus's
// nanosecond-precision arithmetic.
func irateValueFrag() Frag {
	wp := BareIdent("window_pairs")
	n := Call("length", wp)
	lastPair := func() Frag { return Subscript(wp, n) }
	prevPair := func() Frag { return Subscript(wp, Sub(n, InlineLit(int64(1)))) }
	lastVal := func() Frag { return Call("tupleElement", lastPair(), InlineLit(int64(2))) }
	prevVal := func() Frag { return Call("tupleElement", prevPair(), InlineLit(int64(2))) }
	lastTs := Call("tupleElement", lastPair(), InlineLit(int64(1)))
	prevTs := Call("tupleElement", prevPair(), InlineLit(int64(1)))
	// dateDiff('nanosecond', earlier, later) returns Int64.
	dt := func() Frag { return Call("dateDiff", InlineLit("nanosecond"), prevTs, lastTs) }
	// Counter-reset-aware delta: if the value dropped, treat it as a reset
	// and take the raw last value; otherwise the difference.
	delta := Call("if", Lt(lastVal(), prevVal()), lastVal(), Sub(lastVal(), prevVal()))
	// Guard against zero-second interval (two samples at the same
	// nanosecond) — return NaN rather than divide-by-zero.
	return Call(
		"if",
		And(Gt(n, InlineLit(int64(1))), Gt(dt(), InlineLit(int64(0)))),
		Div(Paren(delta), Paren(Div(Paren(dt()), BareIdent("1e9")))),
		BareIdent("nan"),
	)
}

// emitRangeWindowQuantileOverTime emits SQL for
// `quantile_over_time(phi, v[range])`.
//
// Literal phi (RangeWindow.Scalars[0]) feeds CH's parameterised
// `quantile(<phi>)(<arg>)` aggregate via `arrayReduce` — the only
// way to apply a parameterised aggregate to an array literal inside
// a SELECT expression without re-introducing an outer GROUP BY. Phi
// is rendered inline as a CH literal (query shape, not user data).
//
// Computed phi (RangeWindow.ScalarExprs[0] — `quantile_over_time(
// scalar(x), v[r])`) cannot ride arrayReduce's string-typed aggregate
// name, so the emitter renders Prometheus's quantile() interpolation
// (prometheus/promql/quantile.go) directly over the sorted window:
//
//	rank  = phi * (N - 1)
//	lower = sorted[floor(rank)]        (0-based)
//	upper = sorted[min(N-1, floor(rank)+1)]
//	value = lower*(1-weight) + upper*weight, weight = rank-floor(rank)
//
// with the runtime domain rules Prom applies before interpolating:
// NaN phi → NaN, phi < 0 → -Inf, phi > 1 → +Inf. (The literal path
// resolves the same rules at lowering time via outOfRangePhiInf.)
//
// PromQL drops series when the window is empty (matches Prom's
// funcQuantileOverTime).
func (e *emitter) emitRangeWindowQuantileOverTime(r *chplan.RangeWindow) error {
	if len(r.ScalarExprs) == 1 {
		phiE := r.ScalarExprs[0]
		// Pre-flight so chplan errors surface synchronously.
		if err := (&Builder{}).Expr(phiE); err != nil {
			return err
		}
		// Linear-interpolation quantile over the sorted window, computed-phi
		// path. Mirrors Prom's quantile() helper: rank = phi*(n-1), the
		// value is the lower / upper sorted samples blended by the
		// fractional part of rank, with out-of-range phi mapping to
		// ±Inf / NaN (matches the literal-phi outOfRangePhiInf rules).
		phi := func(b *Builder) { _ = b.Expr(phiE) }
		sorted := Call("arraySort", BareIdent("window_vals"))
		nMinus := Call("toFloat64", Sub(Call("length", BareIdent("window_vals")), InlineLit(int64(1))))
		// CH evaluates each Frag fresh on every render, so rank / floorRank
		// are factories: rank = (phi * (n-1)); floorRank = floor(rank).
		rank := func() Frag { return Paren(Mul(phi, nMinus)) }
		floorRank := func() Frag { return Call("floor", rank()) }
		// 1-based CH array indices: lower = toUInt32(floor(rank)) + 1;
		// upper = toUInt32(least(n-1, floor(rank) + 1)) + 1.
		lowerIdx := Add(Call("toUInt32", floorRank()), InlineLit(int64(1)))
		upperIdx := Add(
			Call("toUInt32", Call("least", nMinus, Add(floorRank(), InlineLit(int64(1))))),
			InlineLit(int64(1)),
		)
		// weight = rank - floor(rank); blend = lower*(1-weight) + upper*weight.
		weight := func() Frag { return Paren(Sub(rank(), floorRank())) }
		lowerTerm := Mul(Subscript(sorted, lowerIdx), Paren(Sub(InlineLit(int64(1)), weight())))
		upperTerm := Mul(Subscript(sorted, upperIdx), weight())
		interp := Add(lowerTerm, upperTerm)
		frag := Call(
			"multiIf",
			Call("isNaN", phi), BareIdent("nan"),
			Lt(phi, InlineLit(int64(0))), InlineLit(math.Inf(-1)),
			Gt(phi, InlineLit(int64(1))), InlineLit(math.Inf(+1)),
			interp,
		)
		return e.emitWindowedArray(r, frag, 1)
	}
	if len(r.Scalars) != 1 {
		return fmt.Errorf("%w: quantile_over_time requires 1 scalar (phi), got %d", ErrUnsupported, len(r.Scalars))
	}
	phi := r.Scalars[0]
	// arrayReduce('quantile(<phi>)', window_vals) — the literal-phi path;
	// the aggregate name carries phi inline (formatFloat keeps it stable
	// across driver float formatting), so it rides InlineLit as a single
	// quoted string. Empty window drops the series (nan).
	frag := Call(
		"if",
		Gt(Call("length", BareIdent("window_vals")), InlineLit(int64(0))),
		Call("arrayReduce", InlineLit("quantile("+formatFloat(phi)+")"), BareIdent("window_vals")),
		BareIdent("nan"),
	)
	return e.emitWindowedArray(r, frag, 1)
}

// extrapolationKind selects the per-function flavour of Prom's shared
// `extrapolatedRate` helper. rate / increase / delta all funnel
// through the same boundary-extrapolation arithmetic but differ in
//
//  1. whether the raw window value is `counter_delta` (counter-reset
//     aware: rate / increase) or `last_val - first_val` (gauge: delta),
//  2. whether the counter clamp-to-zero shortcut runs (rate / increase
//     only — Prom's isCounter branch), and
//  3. whether the factor is per-second (rate only — Prom's isRate
//     branch).
//
// Mirrors prometheus/promql/functions.go::extrapolatedRate, lines
// 188-314 (the helper funcDelta / funcRate / funcIncrease share).
type extrapolationKind int

const (
	// extrapolationKindRate matches `funcRate(...) = extrapolatedRate(isCounter=true, isRate=true)`.
	extrapolationKindRate extrapolationKind = iota
	// extrapolationKindIncrease matches `funcIncrease(...) = extrapolatedRate(isCounter=true, isRate=false)`.
	extrapolationKindIncrease
	// extrapolationKindDelta matches `funcDelta(...) = extrapolatedRate(isCounter=false, isRate=false)`.
	extrapolationKindDelta
)

// isCounter reports whether the raw window value runs through Prom's
// counter-reset-aware delta + clamp-to-zero shortcut (rate / increase)
// or stays as a straight gauge delta (delta).
func (k extrapolationKind) isCounter() bool {
	return k == extrapolationKindRate || k == extrapolationKindIncrease
}

// isRate reports whether the extrapolation factor is per-second
// (only `rate` divides through `r.Range.Seconds()` at the end).
func (k extrapolationKind) isRate() bool { return k == extrapolationKindRate }

// emitWindowedArrayExtrapolated emits SQL for rate / increase / delta
// with Prom's `extrapolatedRate` boundary correction applied to the
// per-window value. Compared to emitWindowedArray, this path projects
// three extra columns at the mid layer — first_ts, last_ts, first_val —
// then adds an extrap layer that materialises Prom's
// `durationToStart`, `durationToEnd`, and `sampled_interval` quantities
// so the outer SELECT can express the per-window value as a single
// multiplication.
//
// SQL skeleton (instant eval, omitting matrix anchor_ts):
//
//	SELECT series_key,
//	       <raw_result> * <factor> [/ <range_seconds>] AS Value
//	FROM (
//	  SELECT series_key, window_vals, counter_delta,
//	         first_ts, last_ts, first_val,
//	         sampled_interval, duration_to_start, duration_to_end
//	  FROM (
//	    SELECT series_key, window_vals, counter_delta,
//	           first_ts, last_ts, first_val
//	    FROM (...standard window_pairs scaffolding...)
//	  )
//	)
//
// The `<factor>` is `(sampled_interval + duration_to_start + duration_to_end) / sampled_interval`,
// matching Prom's `factor := (sampledInterval + durationToStart + durationToEnd) / sampledInterval`
// at functions.go:304.
//
// `<raw_result>` is `counter_delta` for rate / increase (Prom's
// counter-reset-aware accumulator) and `(last_val - first_val)` for
// delta (Prom's gauge `samples[N-1].F - samples[0].F`).
//
// When `r.OuterRange > 0`, the matrix variant is selected instead — same
// per-window arithmetic, but the window range is `(anchor_ts - range, anchor_ts]`
// and each series fans out one row per anchor.
func (e *emitter) emitWindowedArrayExtrapolated(r *chplan.RangeWindow, kind extrapolationKind) error {
	if r.TimestampColumn == "" {
		return fmt.Errorf("%w: RangeWindow.TimestampColumn unset", ErrUnsupported)
	}
	if r.ValueColumn == "" {
		return fmt.Errorf("%w: RangeWindow.ValueColumn unset", ErrUnsupported)
	}
	if r.OuterRange > 0 {
		if r.Step <= 0 {
			return fmt.Errorf("%w: RangeWindow.OuterRange > 0 requires Step > 0", ErrUnsupported)
		}
		return e.emitWindowedArrayExtrapolatedMatrix(r, kind)
	}

	end := endExprFrag(r)
	rangeNS := r.Range.Nanoseconds()
	rangeSeconds := r.Range.Seconds()
	rangeStart := rangeStartFrag(end, rangeNS)
	groupFrags, err := e.collectGroupByFrags(r.GroupBy)
	if err != nil {
		return err
	}

	// Innermost SELECT — groupArray of (ts, value), sorted.
	innermost := NewQuery()
	for _, g := range groupFrags {
		innermost.Select(g)
	}
	innermost.Select(As(groupArrayPairFrag(r.TimestampColumn, r.ValueColumn), "series_array"))
	innerSub, err := e.subqueryFrag(r.Input)
	if err != nil {
		return err
	}
	innermost.From(innerSub)
	// GroupBy is a no-op on an empty slice, so no length guard is needed.
	innermost.GroupBy(groupFrags...)
	// Bound the innermost groupArray to the single eval window so CH prunes
	// granules instead of groupArray-ing the full per-series retention (the
	// arrayFilter below stays as the precise post-groupArray gate). The
	// bound is rendered byte-identically by instantWindowScanBoundsFrags;
	// pushInstantScanBound fail-closes if the IR scan-time bound
	// (RangeWindow.InstantScanBounded, established by
	// chplan.AttachInstantScanTimeBounds) was never set, so a future
	// windowed-array shape cannot silently regress to an unbounded scan.
	if err := pushInstantScanBound(innermost, r, end, rangeNS); err != nil {
		return err
	}

	// Inner-middle SELECT — arrayFilter to the (end-range, end] window.
	innerMid := NewQuery().From(innermost.Frag())
	for _, g := range groupFrags {
		innerMid.Select(g)
	}
	innerMid.Select(As(windowFilterPairsFrag(end, rangeNS), "window_pairs"))

	// Mid SELECT — derives the per-window scalars the extrap layer
	// consumes. window_vals + counter_delta cover the standard shape;
	// first_ts / last_ts / first_val are the extra columns Prom's
	// extrapolatedRate needs to compute the boundary correction. The
	// window_pairs feeding it is timestamp-deduplicated first so a
	// duplicate (series, ts) sample does not inflate the sample count and
	// corrupt the extrapolation (see dedupWindowPairsByTsFrag).
	mid := NewQuery().From(dedupWindowPairsLayer(innerMid.Frag(), groupFrags, false))
	for _, g := range groupFrags {
		mid.Select(g)
	}
	mid.Select(As(windowValsFrag(), "window_vals"))
	mid.Select(As(counterDeltaFrag(), "counter_delta"))
	mid.Select(As(firstTsFrag(), "first_ts"))
	mid.Select(As(lastTsFrag(), "last_ts"))
	mid.Select(As(firstValFrag(), "first_val"))

	// Extrap SELECT — materialises the Prom-side scalars derived from
	// the mid columns. Splitting them off keeps the outer expression a
	// single multiplication rather than a nested CASE-with-shared-
	// subexpression soup. Each scalar is plain dependent arithmetic;
	// CH evaluates the chain row-by-row.
	extrap := NewQuery().From(mid.Frag())
	for _, g := range groupFrags {
		extrap.Select(g)
	}
	extrap.Select(Col("window_vals"))
	extrap.Select(Col("counter_delta"))
	extrap.Select(Col("first_val"))
	extrap.Select(As(sampledIntervalFrag(), "sampled_interval"))
	extrap.Select(As(durationToStartFrag(rangeStart), "duration_to_start"))
	extrap.Select(As(durationToEndFrag(end), "duration_to_end"))

	// Outer SELECT — final value per series.
	outer := NewQuery().From(extrap.Frag())
	for _, g := range groupFrags {
		outer.Select(g)
	}
	outer.Select(As(extrapolatedValueFrag(kind, rangeSeconds), r.ValueColumn))
	outer.Where(windowLenAtLeastFrag("window_vals", 2))

	e.emitSelect(outer)
	return nil
}

// emitWindowedArrayExtrapolatedMatrix is the OuterRange > 0 variant of
// emitWindowedArrayExtrapolated. Each series emits one row per anchor
// (across [End-OuterRange, End] spaced by Step, end-inclusive) whose
// `(anchor_ts - range, anchor_ts]` window holds 2+ samples; the
// per-row window bounds drive the extrapolation arithmetic.
//
// The window arrays are built via the same sample-side fanout +
// per-(series, anchor) regroup as emitWindowedArrayMatrix (see there
// for the memory-shape rationale — this emitter is the one behind the
// run-27277793810 `rate(...)` 2.12 GiB OOM). The regrouped
// `window_pairs` is element-for-element identical to the old
// arrayFilter output — same membership, same arraySort order — so the
// extrapolation quantities (first_ts / last_ts / first_val /
// sampled_interval / duration_to_start / duration_to_end) see exactly
// the per-anchor sample sets Prom's extrapolatedRate contract pins.
func (e *emitter) emitWindowedArrayExtrapolatedMatrix(r *chplan.RangeWindow, kind extrapolationKind) error {
	end := endExprFrag(r)
	rangeNS := r.Range.Nanoseconds()
	stepNS := r.Step.Nanoseconds()
	rangeSeconds := r.Range.Seconds()
	// End-inclusive anchor count. Truncating division matches Prom.
	numAnchors := r.OuterRange.Nanoseconds()/stepNS + 1
	end, numAnchors = stepAlignGrid(r, end, stepNS, numAnchors)
	anchor := verbatim("anchor_ts")
	rangeStart := rangeStartFrag(anchor, rangeNS)
	groupFrags, err := e.collectGroupByFrags(r.GroupBy)
	if err != nil {
		return err
	}

	innerSub, err := e.subqueryFrag(r.Input)
	if err != nil {
		return err
	}
	innerSub, srcTs := fanoutTsSource(innerSub, r.TimestampColumn)

	// Sample-fanout SELECT — one row per (sample, covered anchor).
	fanout := NewQuery().From(innerSub)
	for _, g := range groupFrags {
		fanout.Select(g)
	}
	fanout.Select(Col(srcTs))
	fanout.Select(Col(r.ValueColumn))
	fanout.Select(As(
		sampleAnchorFanoutFrag(end, Col(srcTs), stepNS, rangeNS, numAnchors),
		"anchor_ts",
	))
	// Restrict the input scan to the offset-shifted
	// (Start - Offset - range, End - Offset] window the anchor grid
	// covers — same pushdown as emitWindowedArrayMatrix, against srcTs
	// (the timestamp column present in the fanout's FROM). See
	// maybePushInnerScanTimeBounds.
	maybePushInnerScanTimeBounds(fanout, r, srcTs, rangeNS)

	// Regroup SELECT — rebuild the per-(series, anchor) window array.
	regroup := NewQuery().From(fanout.Frag())
	for _, g := range groupFrags {
		regroup.Select(g)
	}
	regroup.Select(Col("anchor_ts"))
	regroup.Select(As(groupArrayPairFrag(srcTs, r.ValueColumn), "window_pairs"))
	regroupKeys := make([]Frag, 0, len(groupFrags)+1)
	regroupKeys = append(regroupKeys, groupFrags...)
	regroupKeys = append(regroupKeys, Col("anchor_ts"))
	regroup.GroupBy(regroupKeys...)

	// Mid SELECT — window_vals + counter_delta + first/last_ts + first_val.
	// window_pairs is timestamp-deduplicated first so a duplicate
	// (series, ts) sample does not inflate the per-anchor sample count and
	// corrupt the extrapolation (see dedupWindowPairsByTsFrag).
	mid := NewQuery().From(dedupWindowPairsLayer(regroup.Frag(), groupFrags, true))
	for _, g := range groupFrags {
		mid.Select(g)
	}
	mid.Select(Col("anchor_ts"))
	mid.Select(As(windowValsFrag(), "window_vals"))
	mid.Select(As(counterDeltaFrag(), "counter_delta"))
	mid.Select(As(firstTsFrag(), "first_ts"))
	mid.Select(As(lastTsFrag(), "last_ts"))
	mid.Select(As(firstValFrag(), "first_val"))

	// Extrap SELECT — Prom-side scalars derived per-(series, anchor).
	extrap := NewQuery().From(mid.Frag())
	for _, g := range groupFrags {
		extrap.Select(g)
	}
	extrap.Select(Col("anchor_ts"))
	extrap.Select(Col("window_vals"))
	extrap.Select(Col("counter_delta"))
	extrap.Select(Col("first_val"))
	extrap.Select(As(sampledIntervalFrag(), "sampled_interval"))
	extrap.Select(As(durationToStartFrag(rangeStart), "duration_to_start"))
	extrap.Select(As(durationToEndFrag(anchor), "duration_to_end"))

	// Outer SELECT — per-(series, anchor) row.
	outer := NewQuery().From(extrap.Frag())
	for _, g := range groupFrags {
		outer.Select(g)
	}
	outer.Select(Col("anchor_ts"))
	// Also surface anchor_ts under the schema timestamp column name so an
	// outer Aggregate that injected `ColumnRef{TimestampColumn}` into its
	// per-step GROUP BY (see internal/promql/lower.go `bucket_ts` branch)
	// resolves the reference. Without this alias, `sum by (X) (rate(m[5m]))`
	// in range mode fails at CH with `Unknown expression identifier
	// 'bucket_ts'` — the inner Aggregate's `TimeUnix AS bucket_ts`
	// projection has no `TimeUnix` to read from. Keeping the bare
	// `anchor_ts` column intact so downstream `wrapWithSampleProjection`
	// (api/prom/handler.go) and the histogram/instant_fn callers that
	// still read `anchor_ts` directly continue to work.
	if r.TimestampColumn != "" && r.TimestampColumn != "anchor_ts" {
		outer.Select(As(verbatim("anchor_ts"), r.TimestampColumn))
	}
	outer.Select(As(extrapolatedValueFrag(kind, rangeSeconds), r.ValueColumn))
	outer.Where(windowLenAtLeastFrag("window_vals", 2))

	e.emitSelect(outer)
	return nil
}

// firstTsFrag renders `tupleElement(window_pairs[1], 1)` — the first
// sample's timestamp (DateTime64(9)) extracted from the per-window
// pair array. Mirrors Prom's `samples.Floats[0].T`.
func firstTsFrag() Frag {
	return tupleElemFrag(Subscript(BareIdent("window_pairs"), InlineLit(int64(1))), 1)
}

// lastTsFrag renders `tupleElement(window_pairs[length(window_pairs)], 1)`
// — the last sample's timestamp. Mirrors Prom's
// `samples.Floats[numSamplesMinusOne].T`.
func lastTsFrag() Frag {
	return tupleElemFrag(
		Subscript(BareIdent("window_pairs"), Call("length", BareIdent("window_pairs"))), 1,
	)
}

// firstValFrag renders `tupleElement(window_pairs[1], 2)` — the first
// sample's value, needed by the counter clamp-to-zero shortcut so the
// extrapolated rate doesn't dip below zero when the counter started
// inside the window.
func firstValFrag() Frag {
	return tupleElemFrag(Subscript(BareIdent("window_pairs"), InlineLit(int64(1))), 2)
}

// rangeStartFrag renders `<end> - toIntervalNanosecond(<rangeNS>)` —
// Prom's `rangeStart = enh.Ts - durationMilliseconds(ms.Range+vs.Offset)`
// (functions.go:197). end may render arbitrary CH expressions; the
// rangeNS bound is inline.
func rangeStartFrag(end Frag, rangeNS int64) Frag {
	return Sub(end, Call("toIntervalNanosecond", InlineLit(rangeNS)))
}

// secondsBetweenFrag renders `toFloat64(dateDiff('nanosecond', <from>, <to>)) / 1e9`
// — a nanosecond span in seconds (Float64). The single source for the
// sampled-interval and both duration-to-edge raw computations, shared by the
// materialized path (operands = aliases / window-edge Frags) and the fused path
// (operands = inline slice-derived ts). The `1e9` divisor stays in scientific
// (BareIdent) form to be byte-stable against the pinned goldens.
func secondsBetweenFrag(from, to Frag) Frag {
	return Div(
		Call("toFloat64", Call("dateDiff", InlineLit("nanosecond"), from, to)),
		BareIdent("1e9"),
	)
}

// sampledIntervalFrag renders the per-window sampled interval in
// seconds (Float64): `dateDiff('nanosecond', first_ts, last_ts) / 1e9`.
// Mirrors Prom's `sampledInterval := float64(lastT-firstT) / 1000`
// (functions.go:258), substituting nanosecond precision for the
// millisecond timebase Prom carries.
func sampledIntervalFrag() Frag {
	return secondsBetweenFrag(BareIdent("first_ts"), BareIdent("last_ts"))
}

// durationToStartFrag renders the per-window distance from the left
// window edge to the first sample, applying Prom's extrapolation
// threshold (functions.go lines 273-276):
//
//	durationToStart := float64(firstT-rangeStart) / 1000
//	averageDurationBetweenSamples := sampledInterval / float64(numSamplesMinusOne)
//	extrapolationThreshold := averageDurationBetweenSamples * 1.1
//	if durationToStart >= extrapolationThreshold {
//	    durationToStart = averageDurationBetweenSamples / 2
//	}
//
// The counter clamp-to-zero shortcut (Prom lines 277-298) is applied
// downstream inside [extrapolatedValueFrag] because it has to share the
// counter_delta / first_val handles with the result accumulator —
// keeping the two halves co-located keeps the dependent-arithmetic
// chain readable in the emitted SQL.
//
// `sampled_interval` is a mid-layer alias the extrap layer carries
// through; `numSamplesMinusOne` is computed inline as
// `length(window_vals) - 1` (the outer WHERE clause's `length >= 2`
// gate guarantees the divisor is non-zero). rangeStart is supplied as
// a Frag so the instant + matrix paths can pin the appropriate
// window-left expression (instant: `end - range`; matrix:
// `anchor_ts - range`).
func durationToStartFrag(rangeStart Frag) Frag {
	raw := durationToStartRawFrag(rangeStart)
	return extrapThresholdClampFrag(raw)
}

// numSamplesMinusOneFrag renders `(length(window_vals) - 1)` — the
// sample-interval count the extrapolation arithmetic divides by; the
// outer WHERE `length >= 2` gate keeps it non-zero.
func numSamplesMinusOneFrag(arr Frag) Frag {
	return Paren(Sub(Call("length", arr), InlineLit(int64(1))))
}

// extrapolationThresholdFactor is Prom's `extrapolationThreshold =
// averageDurationBetweenSamples * 1.1` cutoff (functions.go): when the gap to a
// window edge exceeds it, the gap is replaced with half the average interval.
const extrapolationThresholdFactor = 1.1

// extrapThresholdClampExpr renders Prom's extrapolation-threshold clamp over
// operand Frags:
//
//	if(<raw> >= 1.1 * <sampledInterval> / <nm1>, <sampledInterval> / <nm1> / 2, <raw>)
//
// Shared by the materialized path (operands = mid-layer column aliases, single
// tokens) and the fused path (operands = inline slice-derived exprs); the
// arithmetic shape is identical, the caller supplies aliased-or-inline operands,
// so the rendered SQL is byte-identical for each path.
func extrapThresholdClampExpr(raw, sampledInterval, nm1 Frag) Frag {
	threshold := Div(Mul(InlineLit(extrapolationThresholdFactor), sampledInterval), nm1)
	halfAvg := Div(Div(sampledInterval, nm1), InlineLit(int64(2)))
	return Call("if", Gte(raw, threshold), halfAvg, raw)
}

// extrapThresholdClampFrag is the materialized-path adapter: it clamps `raw`
// against the `sampled_interval` mid-layer alias and the inline
// `length(window_vals) - 1`.
func extrapThresholdClampFrag(raw Frag) Frag {
	return extrapThresholdClampExpr(raw, BareIdent("sampled_interval"), numSamplesMinusOneFrag(BareIdent("window_vals")))
}

// durationToStartRawFrag renders the un-clamped duration-to-start in
// seconds: `toFloat64(dateDiff('nanosecond', rangeStart, first_ts)) / 1e9`.
// Mirrors Prom's `float64(firstT-rangeStart) / 1000`. The `1e9` divisor
// is an emitter-controlled query-shape constant kept in scientific form
// (BareIdent) to stay byte-stable against the pinned goldens.
func durationToStartRawFrag(rangeStart Frag) Frag {
	return secondsBetweenFrag(rangeStart, BareIdent("first_ts"))
}

// durationToEndFrag renders the per-window distance from the last
// sample to the right window edge, applying Prom's extrapolation
// threshold (no counter branch — Prom only clamps the LEFT edge to the
// counter's zero point).
//
// Mirrors functions.go lines 300-302:
//
//	durationToEnd := float64(rangeEnd-lastT) / 1000
//	if durationToEnd >= extrapolationThreshold {
//	    durationToEnd = averageDurationBetweenSamples / 2
//	}
//
// rangeEnd is supplied as a Frag so the instant + matrix paths can pin
// the appropriate window-right expression (instant: `end`; matrix:
// `anchor_ts`).
func durationToEndFrag(rangeEnd Frag) Frag {
	return extrapThresholdClampFrag(durationToEndRawFrag(rangeEnd))
}

// durationToEndRawFrag renders the un-clamped duration-to-end in
// seconds: `toFloat64(dateDiff('nanosecond', last_ts, rangeEnd)) / 1e9`.
// Mirrors Prom's `float64(rangeEnd-lastT) / 1000`.
func durationToEndRawFrag(rangeEnd Frag) Frag {
	return secondsBetweenFrag(BareIdent("last_ts"), rangeEnd)
}

// extrapolatedValueFrag renders the per-window final value:
//
//	if(sampled_interval > 0,
//	   <raw_result> * (sampled_interval + duration_to_start + duration_to_end) / sampled_interval [/ <range_seconds>],
//	   nan)
//
// The `sampled_interval > 0` guard maps Prom's `len(samples.Floats) > 1`
// + non-collapsed-timestamp case to a NaN sample (the engine layer
// treats NaN as drop). It also dodges the divide-by-zero CH would
// otherwise hit when two samples landed at the same nanosecond.
//
// `<raw_result>` is `counter_delta` for rate / increase (counter-reset
// aware) and `(last_val - first_val)` for delta (gauge). For delta we
// reference `window_vals[length(window_vals)] - first_val` to avoid
// projecting a separate `last_val` alias.
//
// The optional `/ <range_seconds>` only applies to rate (Prom's
// `isRate` branch at functions.go:305-307).
// extrapolatedValueExpr renders Prom's per-window extrapolated value over
// operand Frags. Shared by the materialized path (operands = mid-layer column
// aliases) and the fused path (operands = inline slice-derived exprs) — the
// arithmetic shape (counter-reset raw delta, the counter zero-crossing clamp,
// the `(sampled_interval + durStart + durEnd) / sampled_interval` factor, the
// rate `/ range_seconds`, the `sampled_interval > 0 ? … : nan` guard) is
// identical; the caller supplies aliased-or-inline operands so each path's SQL
// is byte-identical. `lastVal` is consumed only by the delta raw result.
func extrapolatedValueExpr(
	kind extrapolationKind, rangeSeconds float64,
	counterDelta, sampledInterval, firstVal, lastVal, durToStart, durToEnd Frag,
) Frag {
	// raw result: counter_delta for rate/increase, (last - first) for delta.
	rawResult := counterDelta
	if kind == extrapolationKindDelta {
		rawResult = Paren(Sub(lastVal, firstVal))
	}

	// duration_to_start, optionally clamped to the counter zero-crossing.
	// For counters Prom shortens it to the implied zero of the linear
	// extrapolation (functions.go) so the rate stays non-negative:
	//   if(counter_delta > 0 AND first_val >= 0,
	//      least(duration_to_start, sampled_interval * first_val / counter_delta),
	//      duration_to_start)
	durStart := durToStart
	if kind.isCounter() {
		durStart = Call(
			"if",
			And(
				Gt(counterDelta, InlineLit(int64(0))),
				Gte(firstVal, InlineLit(int64(0))),
			),
			Call("least", durToStart,
				Div(Mul(sampledInterval, firstVal), counterDelta)),
			durToStart,
		)
	}

	// factor numerator: sampled_interval + <durStart> + duration_to_end
	factorNum := Add(Add(sampledInterval, durStart), durToEnd)
	// <rawResult> * (sampled_interval + … + duration_to_end) / sampled_interval
	value := Div(Mul(rawResult, Paren(factorNum)), sampledInterval)
	if kind.isRate() {
		value = Div(value, InlineLit(rangeSeconds))
	}

	return Call("if", Gt(sampledInterval, InlineLit(int64(0))), value, BareIdent("nan"))
}

// extrapolatedValueFrag is the materialized-path adapter: it binds the operands
// to the mid/extrap-layer column aliases. delta references
// `window_vals[length(window_vals)]` for last_val (no separate alias projected).
func extrapolatedValueFrag(kind extrapolationKind, rangeSeconds float64) Frag {
	lastVal := Subscript(BareIdent("window_vals"), Call("length", BareIdent("window_vals")))
	return extrapolatedValueExpr(kind, rangeSeconds,
		BareIdent("counter_delta"), BareIdent("sampled_interval"),
		BareIdent("first_val"), lastVal,
		BareIdent("duration_to_start"), BareIdent("duration_to_end"))
}

// emitWindowedArray writes the windowed-array SQL skeleton with the
// value Frag substituted in the outer SELECT position. The Frag can
// reference `window_vals` (Array(Float64)) and `counter_delta`
// (Float64); args bound inside it land at the outer SELECT position so
// positional `?` ordering follows the SQL stream.
//
// When r.OuterRange > 0 emission switches to the matrix path: each
// series emits one row per anchor (across [End-OuterRange, End] spaced
// by Step, end-inclusive) whose window holds at least minWindowSize
// samples, built via the sample-side fanout + regroup (see
// emitWindowedArrayMatrix). The outer SELECT additionally projects the
// anchor timestamp as `anchor_ts`.
//
// minWindowSize controls the PromQL "drop empty windows" semantics:
// when > 0, the outer SELECT adds `WHERE length(window_vals) >= N`
// so series (or (series, anchor) rows in the matrix shape) whose
// window holds fewer than N samples are dropped from the result —
// matching Prom's behaviour for rate / increase / delta / *_over_time,
// which all return no sample for those windows. Every matrix caller
// passes >= 1: the sample-side matrix shape produces no row at all for
// empty windows by construction (and PromQL/LogQL want exactly that).
func (e *emitter) emitWindowedArray(r *chplan.RangeWindow, value Frag, minWindowSize int) error {
	if r.TimestampColumn == "" {
		return fmt.Errorf("%w: RangeWindow.TimestampColumn unset", ErrUnsupported)
	}
	if r.ValueColumn == "" {
		return fmt.Errorf("%w: RangeWindow.ValueColumn unset", ErrUnsupported)
	}
	if r.OuterRange > 0 {
		if r.Step <= 0 {
			return fmt.Errorf("%w: RangeWindow.OuterRange > 0 requires Step > 0", ErrUnsupported)
		}
		return e.emitWindowedArrayMatrix(r, value, minWindowSize)
	}

	end := endExprFrag(r)
	rangeNS := r.Range.Nanoseconds()
	groupFrags, err := e.collectGroupByFrags(r.GroupBy)
	if err != nil {
		return err
	}

	// Innermost SELECT — groupArray of (ts, value), sorted.
	innermost := NewQuery()
	for _, g := range groupFrags {
		innermost.Select(g)
	}
	innermost.Select(As(groupArrayPairFrag(r.TimestampColumn, r.ValueColumn), "series_array"))
	innerSub, err := e.subqueryFrag(r.Input)
	if err != nil {
		return err
	}
	innermost.From(innerSub)
	// GroupBy is a no-op on an empty slice, so no length guard is needed.
	innermost.GroupBy(groupFrags...)
	// Bound the innermost groupArray to the single eval window so CH prunes
	// granules instead of groupArray-ing the full per-series retention (the
	// arrayFilter below stays as the precise post-groupArray gate). The
	// bound is rendered byte-identically by instantWindowScanBoundsFrags;
	// pushInstantScanBound fail-closes if the IR scan-time bound
	// (RangeWindow.InstantScanBounded, established by
	// chplan.AttachInstantScanTimeBounds) was never set, so a future
	// windowed-array shape cannot silently regress to an unbounded scan.
	if err := pushInstantScanBound(innermost, r, end, rangeNS); err != nil {
		return err
	}

	// Inner-middle SELECT — arrayFilter to the [end-range, end] window.
	innerMid := NewQuery().From(innermost.Frag())
	for _, g := range groupFrags {
		innerMid.Select(g)
	}
	innerMid.Select(As(windowFilterPairsFrag(end, rangeNS), "window_pairs"))

	// Middle SELECT — derives window_vals + counter_delta from window_pairs.
	mid := NewQuery().From(innerMid.Frag())
	for _, g := range groupFrags {
		mid.Select(g)
	}
	mid.Select(As(windowValsFrag(), "window_vals"))
	mid.Select(As(counterDeltaFrag(), "counter_delta"))

	// Outer SELECT — final value per series.
	outer := NewQuery().From(mid.Frag())
	for _, g := range groupFrags {
		outer.Select(g)
	}
	outer.Select(As(value, r.ValueColumn))
	if minWindowSize > 0 {
		outer.Where(windowLenAtLeastFrag("window_vals", minWindowSize))
	}

	e.emitSelect(outer)
	return nil
}

// emitWindowedArrayMatrix is the OuterRange > 0 variant: each series
// emits one row per anchor (across [End-OuterRange, End] spaced by
// Step, end-inclusive) whose window holds at least minWindowSize
// samples. The fanout is SAMPLE-SIDE: each input row arrayJoins across
// only the ≤ range/step + 1 anchors whose `(anchor - range, anchor]`
// window contains its timestamp (see sampleAnchorFanoutFrag), and a
// GROUP BY (series, anchor) rebuilds the per-window (ts, value) array.
// Subsequent layers operate on the per-(series, anchor) tuple exactly
// as before.
//
// SQL skeleton (with N = OuterRange/Step + 1):
//
//	SELECT series_key, anchor_ts, <valueFrag> AS value FROM (
//	  SELECT series_key, anchor_ts, <window_vals + counter_delta> FROM (
//	    SELECT series_key, anchor_ts, arraySort(groupArray((TimeUnix, Value))) AS window_pairs FROM (
//	      SELECT series_key, TimeUnix, Value,
//	        arrayJoin(arrayMap(i -> <end> - toIntervalNanosecond(i * <step_ns>),
//	                  range(<covered-anchor index bounds>))) AS anchor_ts
//	      FROM (<input>)
//	    ) GROUP BY series_key, anchor_ts
//	  )
//	)
//
// Memory shape: the previous emission grouped every series into one
// `series_array`, fanned THAT row across all N anchors, and re-filtered
// the full array per (series, anchor) — O(anchors × window_samples)
// peak (run 27277793810: CH hit its 2.12 GiB cap on a 24h/15s grid).
// The sample-side fanout materialises each sample only in the windows
// it belongs to — O(samples × range/step) total — while producing
// byte-identical window arrays per (series, anchor): same membership
// (the fanout predicate IS the window predicate), same arraySort
// ordering, same duplicate handling.
//
// Anchors whose window is empty produce no group here, whereas the old
// full-grid fanout materialised them and dropped them via the
// minWindowSize WHERE. Every matrix-mode caller passes
// minWindowSize >= 1 (PromQL/LogQL drop empty windows), so the two
// shapes agree row-for-row.
func (e *emitter) emitWindowedArrayMatrix(r *chplan.RangeWindow, value Frag, minWindowSize int) error {
	end := endExprFrag(r)
	rangeNS := r.Range.Nanoseconds()
	stepNS := r.Step.Nanoseconds()
	// End-inclusive anchor count. e.g. [5m:2m] = 5m/2m + 1 = 3 anchors
	// at end, end-2m, end-4m. Truncating division matches Prom semantics.
	numAnchors := r.OuterRange.Nanoseconds()/stepNS + 1
	end, numAnchors = stepAlignGrid(r, end, stepNS, numAnchors)
	groupFrags, err := e.collectGroupByFrags(r.GroupBy)
	if err != nil {
		return err
	}

	innerSub, err := e.subqueryFrag(r.Input)
	if err != nil {
		return err
	}
	innerSub, srcTs := fanoutTsSource(innerSub, r.TimestampColumn)

	// Sample-fanout SELECT — one row per (sample, covered anchor).
	fanout := NewQuery().From(innerSub)
	for _, g := range groupFrags {
		fanout.Select(g)
	}
	fanout.Select(Col(srcTs))
	fanout.Select(Col(r.ValueColumn))
	fanout.Select(As(
		sampleAnchorFanoutFrag(end, Col(srcTs), stepNS, rangeNS, numAnchors),
		"anchor_ts",
	))
	// Restrict the input scan to the offset-shifted
	// (Start - Offset - range, End - Offset] window the anchor grid
	// covers, so CH prunes partitions / granules instead of fanning the
	// whole series history through sampleAnchorFanoutFrag. The bound
	// references srcTs (the timestamp column actually present in the
	// fanout's FROM — the same column the fanout / regroup read), and
	// ANDs with any predicate already on the input. Gated on Start/End
	// being set, so subquery-internal shapes stay byte-stable. See
	// maybePushInnerScanTimeBounds.
	maybePushInnerScanTimeBounds(fanout, r, srcTs, rangeNS)

	// Regroup SELECT — rebuild the per-(series, anchor) window array.
	regroup := NewQuery().From(fanout.Frag())
	for _, g := range groupFrags {
		regroup.Select(g)
	}
	regroup.Select(Col("anchor_ts"))
	regroup.Select(As(groupArrayPairFrag(srcTs, r.ValueColumn), "window_pairs"))
	regroupKeys := make([]Frag, 0, len(groupFrags)+1)
	regroupKeys = append(regroupKeys, groupFrags...)
	regroupKeys = append(regroupKeys, Col("anchor_ts"))
	regroup.GroupBy(regroupKeys...)

	// Middle SELECT — window_vals + counter_delta per (series, anchor).
	mid := NewQuery().From(regroup.Frag())
	for _, g := range groupFrags {
		mid.Select(g)
	}
	mid.Select(Col("anchor_ts"))
	mid.Select(As(windowValsFrag(), "window_vals"))
	mid.Select(As(counterDeltaFrag(), "counter_delta"))

	// Outer SELECT — per-(series, anchor) row.
	outer := NewQuery().From(mid.Frag())
	for _, g := range groupFrags {
		outer.Select(g)
	}
	outer.Select(Col("anchor_ts"))
	// See emitWindowedArrayExtrapolatedMatrix for the rationale: surface
	// anchor_ts under the schema timestamp column so a wrapping
	// Aggregate's per-step GROUP BY (ColumnRef{TimestampColumn}) resolves.
	if r.TimestampColumn != "" && r.TimestampColumn != "anchor_ts" {
		outer.Select(As(verbatim("anchor_ts"), r.TimestampColumn))
	}
	outer.Select(As(value, r.ValueColumn))
	if minWindowSize > 0 {
		outer.Where(windowLenAtLeastFrag("window_vals", minWindowSize))
	}

	e.emitSelect(outer)
	return nil
}

// collectGroupByFrags renders each GroupBy expression to an isolated
// captured SQL+args once, then returns a []Frag that replays only the
// SQL (no args) into the receiving Builder. Args captured during
// pre-render are appended to e.args at call time so they land in the
// final args slice at the position the first occurrence (the outermost
// SELECT-list) writes — matching the legacy collectGroupBy
// semantics.
//
// The pre-render-once + splice-only-string shape means group-by
// expressions can appear in both the SELECT-list and the GROUP BY
// clause without binding their args twice.
func (e *emitter) collectGroupByFrags(group []chplan.Expr) ([]Frag, error) {
	out := make([]Frag, 0, len(group))
	for _, g := range group {
		// Render to a separate buffer so we can reuse the string.
		sub := &Builder{}
		if err := sub.Expr(g); err != nil {
			return nil, err
		}
		sql, args := sub.Build()
		// Append captured args to the emitter so they land at the
		// position the outer SELECT-list will reference them. Since
		// every supported group-by expression is currently arg-free
		// (bare ColumnRef), `args` is empty in practice; the append
		// is harmless when it is non-empty for future expressions.
		e.args = append(e.args, args...)
		out = append(out, verbatim(sql))
	}
	return out, nil
}
