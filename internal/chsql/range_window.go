package chsql

import (
	"fmt"
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

	sb := NewQuery().From(sub)
	for i, g := range m.GroupBy {
		expr := g
		alias := ""
		if i < len(m.GroupByAliases) {
			alias = m.GroupByAliases[i]
		}
		sb.SelectAs(func(b *Builder) { _ = b.Expr(expr) }, alias)
	}
	af := chplan.AggFunc{Name: name, Params: params, Args: args, Alias: m.ValueAlias}
	sb.Select(aggFuncFrag(af))
	if len(m.GroupBy) > 0 {
		groupFrags := make([]Frag, 0, len(m.GroupBy))
		for _, g := range m.GroupBy {
			expr := g
			groupFrags = append(groupFrags, func(b *Builder) { _ = b.Expr(expr) })
		}
		sb.GroupBy(groupFrags...)
	}
	e.emitSelect(sb)
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
// quantile_over_time(attr, q) → `quantile(q)(Attr)`.
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
		if len(m.Quantiles) != 1 {
			return "", nil, nil, fmt.Errorf("%w: MetricsAggregate quantile expects exactly 1 quantile, got %d", ErrUnsupported, len(m.Quantiles))
		}
		return "quantile", []chplan.Expr{&chplan.LitFloat{V: m.Quantiles[0]}}, []chplan.Expr{m.Attr}, nil
	}
	return "", nil, nil, fmt.Errorf("%w: MetricsAggregate op %s", ErrUnsupported, m.Op)
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
// When r.OuterRange > 0 emission switches to the matrix shape: an
// arrayJoin fans out one row per anchor across [End-OuterRange, End]
// spaced by Step (end-inclusive), and the outer SELECT projects the
// anchor timestamp alongside the per-anchor value. Used by PromQL
// subqueries (P0 #4).
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
	case "sum_over_time", "avg_over_time", "min_over_time", "max_over_time", "count_over_time", "last_over_time", "stddev_over_time", "stdvar_over_time":
		return e.emitRangeWindowOverTime(r)
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
		return fmt.Errorf("%w: range function %q (lands in M1.1 follow-ups)", ErrUnsupported, r.Func)
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
	if len(r.Scalars) != 1 {
		return fmt.Errorf("%w: predict_linear requires 1 scalar (t), got %d", ErrUnsupported, len(r.Scalars))
	}
	t := r.Scalars[0]
	// In matrix mode each row carries its own anchor_ts; the anchor
	// Frag the factory receives below renders the per-row anchor so the
	// per-sample x-offset (dateDiff('second', anchor, sample_ts)) is
	// computed against the anchor of THIS row, not r.End.
	writer := func(anchor Frag) Frag {
		return func(b *Builder) {
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
			b.sb.WriteString("if(length(window_pairs) > 1, ")
			b.sb.WriteString("tupleElement(arrayReduce('simpleLinearRegression', ")
			b.sb.WriteString("arrayMap(p -> dateDiff('second', ")
			anchor(b)
			b.sb.WriteString(", tupleElement(p, 1)), window_pairs), ")
			b.sb.WriteString("arrayMap(p -> tupleElement(p, 2), window_pairs)")
			b.sb.WriteString("), 2) + tupleElement(arrayReduce('simpleLinearRegression', ")
			b.sb.WriteString("arrayMap(p -> dateDiff('second', ")
			anchor(b)
			b.sb.WriteString(", tupleElement(p, 1)), window_pairs), ")
			b.sb.WriteString("arrayMap(p -> tupleElement(p, 2), window_pairs)")
			b.sb.WriteString("), 1) * ")
			b.Arg(t)
			b.sb.WriteString(", nan)")
		}
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
	return e.emitWindowedArray(r, verbatim(holtWintersValueExpr(sf, tf)), 2)
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
func holtWintersValueExpr(sf, tf float64) string {
	// We seed with the first two samples, then fold over the slice
	// `window_vals[3:]` applying the recurrence. CH's arrayFold takes
	// (lambda, array, initialAcc) and the lambda is (acc, elem).
	//
	// Numbers are formatted with FormatFloat to keep the SQL stable and
	// avoid Sprintf-on-SQL. Bound floats inline (no `?`); these are
	// query-shape parameters, not user data.
	sfStr := formatFloat(sf)
	oneMinusSf := formatFloat(1 - sf)
	tfStr := formatFloat(tf)
	oneMinusTf := formatFloat(1 - tf)
	// Lambda body computes new (s, b) given prior (acc.s, acc.b, x).
	// new_s = sf*x + (1-sf)*(acc.s + acc.b)
	// new_b = tf*(new_s - acc.s) + (1-tf)*acc.b
	// We expose them via let bindings inline.
	return "if(length(window_vals) > 1, " +
		"tupleElement(arrayFold(" +
		"(acc, x) -> (" +
		sfStr + " * x + " + oneMinusSf + " * (tupleElement(acc, 1) + tupleElement(acc, 2)), " +
		tfStr + " * (" + sfStr + " * x + " + oneMinusSf + " * (tupleElement(acc, 1) + tupleElement(acc, 2)) - tupleElement(acc, 1)) + " + oneMinusTf + " * tupleElement(acc, 2)" +
		"), arraySlice(window_vals, 3), (window_vals[2], window_vals[2] - window_vals[1])), 1), nan)"
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
// series emits one row per anchor across [End-OuterRange, End] spaced
// by Step (end-inclusive). The outer SELECT additionally projects the
// anchor timestamp as `anchor_ts`. The value-writer is invoked with
// the matrix anchor (`anchor_ts`) so anchor-relative expressions
// (deriv / predict_linear) compute per-anchor results rather than
// re-anchoring every row at r.End.
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
	innermost.Select(rawAs(groupArrayPairFrag(r.TimestampColumn, r.ValueColumn), "series_array"))
	innerSub, err := e.subqueryFrag(r.Input)
	if err != nil {
		return err
	}
	innermost.From(innerSub)
	if len(groupFrags) > 0 {
		innermost.GroupBy(groupFrags...)
	}

	// Inner SELECT — arrayFilter to the [end-range, end] window.
	innerSb := NewQuery().From(innermost.Frag())
	for _, g := range groupFrags {
		innerSb.Select(g)
	}
	innerSb.Select(rawAs(windowFilterPairsFrag(end, rangeNS), "window_pairs"))

	// Outer SELECT — final value per series.
	outerSb := NewQuery().From(innerSb.Frag())
	for _, g := range groupFrags {
		outerSb.Select(g)
	}
	outerSb.Select(rawAs(valueWriterFor(end), r.ValueColumn))
	if minWindowSize > 0 {
		outerSb.Where(windowLenAtLeastFrag("window_pairs", minWindowSize))
	}

	e.emitSelect(outerSb)
	return nil
}

// emitWindowedArrayPairsMatrix is the OuterRange > 0 variant of
// emitWindowedArrayPairs: each series emits N rows, one per anchor
// across [End-OuterRange, End] spaced by Step (end-inclusive). Mirrors
// emitWindowedArrayMatrix but exposes `window_pairs` directly without
// the `window_vals` / `counter_delta` middle layer the values-only
// shape needs.
//
// SQL skeleton (with N = OuterRange/Step + 1):
//
//	SELECT series_key, anchor_ts, <valueFrag> AS value FROM (
//	  SELECT series_key, anchor_ts,
//	         arrayFilter(p -> p.1 in [anchor_ts - range, anchor_ts], series_array) AS window_pairs
//	  FROM (
//	    SELECT series_key, series_array,
//	      arrayJoin(arrayMap(i -> <end> - toIntervalNanosecond(i * <step_ns>), range(0, N))) AS anchor_ts
//	    FROM (
//	      SELECT series_key, arraySort(groupArray((TimeUnix, Value))) AS series_array
//	      FROM (<input>) GROUP BY series_key
//	    )
//	  )
//	)
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
	groupFrags, err := e.collectGroupByFrags(r.GroupBy)
	if err != nil {
		return err
	}

	// Innermost SELECT — groupArray of (ts, value), sorted.
	innermost := NewQuery()
	for _, g := range groupFrags {
		innermost.Select(g)
	}
	innermost.Select(rawAs(groupArrayPairFrag(r.TimestampColumn, r.ValueColumn), "series_array"))
	innerSub, err := e.subqueryFrag(r.Input)
	if err != nil {
		return err
	}
	innermost.From(innerSub)
	if len(groupFrags) > 0 {
		innermost.GroupBy(groupFrags...)
	}

	// Anchor-fanout SELECT — arrayJoin produces one row per anchor.
	fanout := NewQuery().From(innermost.Frag())
	for _, g := range groupFrags {
		fanout.Select(g)
	}
	fanout.Select(Col("series_array"))
	fanout.Select(rawAs(anchorFanoutFrag(end, stepNS, numAnchors), "anchor_ts"))

	// Window-clamp SELECT — arrayFilter to [anchor_ts - range, anchor_ts].
	innerMid := NewQuery().From(fanout.Frag())
	for _, g := range groupFrags {
		innerMid.Select(g)
	}
	innerMid.Select(Col("anchor_ts"))
	innerMid.Select(rawAs(windowFilterPairsFrag(verbatim("anchor_ts"), rangeNS), "window_pairs"))

	// Outer SELECT — per-(series, anchor) row.
	outer := NewQuery().From(innerMid.Frag())
	for _, g := range groupFrags {
		outer.Select(g)
	}
	outer.Select(Col("anchor_ts"))
	outer.Select(rawAs(valueWriterFor(verbatim("anchor_ts")), r.ValueColumn))
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
	return func(b *Builder) {
		base := timeOrNowFrag(r.End)
		if r.Offset != 0 {
			b.sb.WriteByte('(')
			base(b)
			b.sb.WriteString(" - toIntervalNanosecond(")
			b.sb.WriteString(strconv.FormatInt(r.Offset.Nanoseconds(), 10))
			b.sb.WriteString("))")
			return
		}
		base(b)
	}
}

// rawAs wraps a Frag in "<expr> AS <alias>" with the alias emitted
// VERBATIM (no backticks). The windowed-array idiom relies on internal
// aliases like `series_array`, `window_pairs`, `window_vals`,
// `counter_delta`, `anchor_ts`, `value` that are never user-supplied
// and must stay un-backticked to keep the byte-level golden fixtures
// stable. The typed `As` helper backticks every alias; this is the
// matching variant for the legacy windowed-array shape.
//
// Empty alias renders the expression bare (no AS clause).
func rawAs(expr Frag, alias string) Frag {
	if alias == "" {
		return expr
	}
	return func(b *Builder) {
		expr(b)
		b.sb.WriteString(" AS ")
		b.sb.WriteString(alias)
	}
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
// Each per-span row out of m.Inner is fanned across the N evaluation
// anchors via arrayJoin(range(0, N)); the outer SELECT applies the
// Op-specific CH aggregate per (group-by, anchor) bucket.
//
// SQL skeleton (N = (End-Start)/Step + 1 or OuterRange/Step + 1):
//
//	SELECT [<group cols>,] anchor_ts, <reducer> AS value
//	FROM (
//	  SELECT [<group cols>,] <TimestampColumn> AS ts, [<Attr> AS metric_arg,]
//	         arrayJoin(arrayMap(i -> <anchor_base> - toIntervalNanosecond(i * <step_ns>), range(0, <N>))) AS anchor_ts
//	  FROM (<Inner>)
//	)
//	WHERE ts >= anchor_ts - toIntervalNanosecond(<range_ns>)
//	  AND ts <= anchor_ts
//	GROUP BY [<group cols>,] anchor_ts
//
// `<reducer>` depends on m.Op:
//
//   - Rate: `count(1) / <range_seconds>`
//   - CountOverTime: `count(1)`
//   - Sum/Min/Max/AvgOverTime: `sum/min/max/avg(metric_arg)`
//   - QuantileOverTime: `quantile(q)(metric_arg)`
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

	// Inner SELECT: fan each Inner row across N anchors, projecting
	// group-by cols, the timestamp as `ts`, [the metric operand as
	// metric_arg,] and the anchor_ts. Group-by columns are aliased
	// so the outer SELECT / WHERE / GROUP BY can reference them by
	// a stable name regardless of whether the source expression was
	// a bare ColumnRef or a Map lookup.
	groupAliases := outerGroupAliases(m.GroupBy, m.GroupByAliases)
	innerSb := NewQuery().From(inner)
	for i, g := range m.GroupBy {
		expr := g
		alias := groupAliases[i]
		innerSb.SelectAs(func(b *Builder) { _ = b.Expr(expr) }, alias)
	}
	tsCol := r.TimestampColumn
	innerSb.SelectAs(func(b *Builder) { b.Ident(tsCol) }, "ts")
	if m.Op != chplan.MetricsOpRate && m.Op != chplan.MetricsOpCountOverTime && m.Attr != nil {
		attr := m.Attr
		innerSb.SelectAs(func(b *Builder) { _ = b.Expr(attr) }, "metric_arg")
	}
	innerSb.SelectAs(
		anchorFanoutFrag(end, stepNS, numAnchors),
		"anchor_ts",
	)

	// Outer SELECT: GROUP BY group cols + anchor_ts; apply the
	// per-bucket reducer.
	outerSb := NewQuery().From(innerSb.Frag())

	// Group-by columns in the outer SELECT-list are referenced by the
	// stable inner-SELECT aliases (set above).
	for _, alias := range groupAliases {
		a := alias
		outerSb.Select(func(b *Builder) { b.Ident(a) })
	}
	outerSb.Select(Col("anchor_ts"))
	reducerFrag := metricsReducerFrag(m.Op, chName, params, args, rangeSeconds)
	outerSb.Select(As(reducerFrag, m.ValueAlias))

	// WHERE: ts ∈ [anchor_ts - range, anchor_ts].
	outerSb.Where(
		windowTsLowerBoundFrag(rangeNS),
		verbatim("ts <= anchor_ts"),
	)

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

// anchorFanoutFrag returns a Frag rendering
// `arrayJoin(arrayMap(i -> <end> - toIntervalNanosecond(i * <stepNS>), range(0, <N>)))`.
// Used by the matrix-shape RangeWindow emitter to fan each Inner row
// across N anchors in a single CH pass.
//
// end is rendered via the Frag callback (the CH expression for the
// eval-grid anchor base — typically a DateTime64 literal or
// `now64(9)`); stepNS and N are inline integer literals.
func anchorFanoutFrag(end Frag, stepNS, numAnchors int64) Frag {
	return func(b *Builder) {
		b.sb.WriteString("arrayJoin(arrayMap(i -> ")
		end(b)
		b.sb.WriteString(" - toIntervalNanosecond(i * ")
		b.sb.WriteString(strconv.FormatInt(stepNS, 10))
		b.sb.WriteString("), range(0, ")
		b.sb.WriteString(strconv.FormatInt(numAnchors, 10))
		b.sb.WriteString(")))")
	}
}

// windowTsLowerBoundFrag returns a Frag rendering
// `ts >= anchor_ts - toIntervalNanosecond(<rangeNS>)`. The companion
// upper bound is the literal `ts <= anchor_ts` (no parameters); both
// are spliced into the outer SELECT's WHERE clause.
func windowTsLowerBoundFrag(rangeNS int64) Frag {
	return func(b *Builder) {
		b.sb.WriteString("ts >= anchor_ts - toIntervalNanosecond(")
		b.sb.WriteString(strconv.FormatInt(rangeNS, 10))
		b.sb.WriteByte(')')
	}
}

// groupArrayPairFrag returns a Frag rendering
// `arraySort(groupArray((<ts>, <val>)))`. The CH idiom that turns a
// per-row scan of a metrics table into a per-series (ts, value) array,
// sorted ascending by ts so subsequent counter-reset arithmetic
// operates in chronological order.
func groupArrayPairFrag(tsCol, valCol string) Frag {
	return func(b *Builder) {
		b.sb.WriteString("arraySort(groupArray((")
		b.Ident(tsCol)
		b.sb.WriteString(", ")
		b.Ident(valCol)
		b.sb.WriteString(")))")
	}
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
	return func(b *Builder) {
		b.sb.WriteString("length(")
		b.Ident(arrCol)
		b.sb.WriteString(") >= ")
		b.sb.WriteString(strconv.Itoa(n))
	}
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
	return verbatim("arrayMap(p -> tupleElement(p, 2), window_pairs)")
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

	switch op {
	case chplan.MetricsOpRate:
		return func(b *Builder) {
			b.sb.WriteString("toFloat64(")
			b.ParamAgg(chName, paramFrags, argFrags)
			b.sb.WriteString(") / ")
			b.sb.WriteString(strconv.FormatFloat(rangeSeconds, 'f', -1, 64))
		}
	}
	return func(b *Builder) {
		b.sb.WriteString("toFloat64(")
		b.ParamAgg(chName, paramFrags, argFrags)
		b.sb.WriteByte(')')
	}
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
// last_over_time but lowered from a SubqueryExpr (P0 #4.5) rather than
// a Call. Drops anchors whose window is empty (1+ samples required to
// have a "last").
func (e *emitter) emitRangeWindowIdentity(r *chplan.RangeWindow) error {
	return e.emitWindowedArray(r, verbatim("if(length(window_vals) > 0, window_vals[length(window_vals)], nan)"), 1)
}

// emitRangeWindowLogRate emits SQL for LogQL-style `rate({...}[range])`
// (and `bytes_rate`, after the lowering layer projects `length(Body)`
// as Value): `arraySum(window_vals) / range_seconds`. Distinct from
// PromQL's counter `rate`, which uses counter-reset-aware deltas.
//
// range_seconds binds as a parameter via the value-writer callback so
// the emitter stays free of new Sprintf-on-SQL instances. The
// empty-window guard is delegated to chsql.IfNonZero.
//
// LogQL semantics emit `0` for an empty window (it's a sum-based
// metric, not counter-reset arithmetic), so the empty-window-drop
// filter on the outer SELECT is OFF (minWindowSize = 0).
func (e *emitter) emitRangeWindowLogRate(r *chplan.RangeWindow) error {
	rangeSeconds := r.Range.Seconds()
	return e.emitWindowedArray(r, IfNonZero(
		Call("arraySum", BareIdent("window_vals")),
		Lit(rangeSeconds),
	), 0)
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
// count_over_time, last_over_time, stddev_over_time, stdvar_over_time.
// These don't need counter-reset handling — they're straight array
// aggregations over the window's values.
//
// stddev_over_time / stdvar_over_time use CH's
// `arrayReduce('stddevPop' | 'varPop', ...)` to match Prometheus's
// `Engine.evalAggrOverTime → varianceOverTime` which builds a Welford
// running estimator that divides squared deviations by N (population
// variance / stddev), not by N-1.
func (e *emitter) emitRangeWindowOverTime(r *chplan.RangeWindow) error {
	var inner string
	switch r.Func {
	case "sum_over_time":
		inner = "arraySum(window_vals)"
	case "avg_over_time":
		inner = "if(length(window_vals) > 0, arrayAvg(window_vals), nan)"
	case "min_over_time":
		inner = "if(length(window_vals) > 0, arrayMin(window_vals), nan)"
	case "max_over_time":
		inner = "if(length(window_vals) > 0, arrayMax(window_vals), nan)"
	case "count_over_time":
		inner = "toFloat64(length(window_vals))"
	case "last_over_time":
		inner = "if(length(window_vals) > 0, window_vals[length(window_vals)], nan)"
	case "stddev_over_time":
		// Empty window → drop the series (Prom returns no sample).
		// We mirror with NaN; the engine layer treats NaN as "drop"
		// when projecting samples. Single-sample windows render the
		// population stddev which is 0 — matches Prom's Welford
		// estimator (sum of squared deviations / 1 = 0 when there's
		// only one sample equal to the running mean).
		inner = "if(length(window_vals) > 0, arrayReduce('stddevPop', window_vals), nan)"
	case "stdvar_over_time":
		// Population variance (divides by N, not N-1) to match
		// Prometheus's funcStdvarOverTime / varianceOverTime which
		// builds a Welford running estimator with divisor N. Same
		// empty-window contract as stddev_over_time: drop the series
		// (we emit NaN; the engine treats NaN as "drop"). Single-sample
		// window renders 0 (sum of squared deviations / 1 = 0).
		inner = "if(length(window_vals) > 0, arrayReduce('varPop', window_vals), nan)"
	default:
		return fmt.Errorf("%w: over-time function %q", ErrUnsupported, r.Func)
	}
	// Every *_over_time variant drops empty-window rows per Prom
	// semantics (Prom's funcSumOverTime / funcCountOverTime / etc. all
	// short-circuit on zero samples). The outer SELECT gets
	// `WHERE length(window_vals) >= 1`.
	return e.emitWindowedArray(r, verbatim(inner), 1)
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
		return func(b *Builder) {
			b.sb.WriteString("if(length(window_pairs) > 1, tupleElement(arrayReduce('simpleLinearRegression', ")
			b.sb.WriteString("arrayMap(p -> dateDiff('second', ")
			anchor(b)
			b.sb.WriteString(", tupleElement(p, 1)), window_pairs), ")
			b.sb.WriteString("arrayMap(p -> tupleElement(p, 2), window_pairs)")
			b.sb.WriteString("), 1), nan)")
		}
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
		Call("arraySum",
			Call("arrayMap",
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
		Call("arraySum",
			Call("arrayMap",
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
	const expr = "if(length(window_vals) > 1, window_vals[length(window_vals)] - window_vals[length(window_vals) - 1], nan)"
	// PromQL idelta drops series whose window holds fewer than 2
	// samples (matches Prom's funcIdelta).
	return e.emitWindowedArray(r, verbatim(expr), 2)
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
	return e.emitWindowedArrayPairs(r, verbatim(irateValueExpr()), 2)
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
func irateValueExpr() string {
	const lastPair = "window_pairs[length(window_pairs)]"
	const prevPair = "window_pairs[length(window_pairs) - 1]"
	const lastVal = "tupleElement(" + lastPair + ", 2)"
	const prevVal = "tupleElement(" + prevPair + ", 2)"
	const lastTs = "tupleElement(" + lastPair + ", 1)"
	const prevTs = "tupleElement(" + prevPair + ", 1)"
	// dateDiff('nanosecond', earlier, later) returns Int64.
	dt := "dateDiff('nanosecond', " + prevTs + ", " + lastTs + ")"
	delta := "if(" + lastVal + " < " + prevVal + ", " + lastVal + ", " + lastVal + " - " + prevVal + ")"
	// Guard against zero-second interval (two samples at the same
	// nanosecond) — return NaN rather than divide-by-zero.
	return "if(length(window_pairs) > 1 AND " + dt + " > 0, (" + delta + ") / ((" + dt + ") / 1e9), nan)"
}

// emitRangeWindowQuantileOverTime emits SQL for
// `quantile_over_time(phi, v[range])`. Phi rides on
// RangeWindow.Scalars[0] and feeds CH's parameterised
// `quantile(<phi>)(<arg>)` aggregate via `arrayReduce` — the only
// way to apply a parameterised aggregate to an array literal inside
// a SELECT expression without re-introducing an outer GROUP BY.
//
// PromQL drops series when the window is empty (matches Prom's
// funcQuantileOverTime). Phi is rendered inline as a CH literal
// (query shape, not user data).
func (e *emitter) emitRangeWindowQuantileOverTime(r *chplan.RangeWindow) error {
	if len(r.Scalars) != 1 {
		return fmt.Errorf("%w: quantile_over_time requires 1 scalar (phi), got %d", ErrUnsupported, len(r.Scalars))
	}
	phi := r.Scalars[0]
	expr := "if(length(window_vals) > 0, arrayReduce('quantile(" + formatFloat(phi) + ")', window_vals), nan)"
	return e.emitWindowedArray(r, verbatim(expr), 1)
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
	if len(groupFrags) > 0 {
		innermost.GroupBy(groupFrags...)
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
	// extrapolatedRate needs to compute the boundary correction.
	mid := NewQuery().From(innerMid.Frag())
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
	extrap.Select(As(durationToStartFrag(rangeStart, kind.isCounter()), "duration_to_start"))
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
// emitWindowedArrayExtrapolated. Each series emits N rows, one per
// anchor across [End-OuterRange, End] spaced by Step (end-inclusive);
// the per-row window is `(anchor_ts - range, anchor_ts]` and the
// per-row range bounds drive the extrapolation arithmetic.
func (e *emitter) emitWindowedArrayExtrapolatedMatrix(r *chplan.RangeWindow, kind extrapolationKind) error {
	end := endExprFrag(r)
	rangeNS := r.Range.Nanoseconds()
	stepNS := r.Step.Nanoseconds()
	rangeSeconds := r.Range.Seconds()
	// End-inclusive anchor count. Truncating division matches Prom.
	numAnchors := r.OuterRange.Nanoseconds()/stepNS + 1
	anchor := verbatim("anchor_ts")
	rangeStart := rangeStartFrag(anchor, rangeNS)
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
	if len(groupFrags) > 0 {
		innermost.GroupBy(groupFrags...)
	}

	// Anchor-fanout SELECT — arrayJoin produces one row per anchor.
	fanout := NewQuery().From(innermost.Frag())
	for _, g := range groupFrags {
		fanout.Select(g)
	}
	fanout.Select(Col("series_array"))
	fanout.Select(As(anchorFanoutFrag(end, stepNS, numAnchors), "anchor_ts"))

	// Inner-middle SELECT — arrayFilter to [anchor_ts - range, anchor_ts].
	innerMid := NewQuery().From(fanout.Frag())
	for _, g := range groupFrags {
		innerMid.Select(g)
	}
	innerMid.Select(Col("anchor_ts"))
	innerMid.Select(As(windowFilterPairsFrag(anchor, rangeNS), "window_pairs"))

	// Mid SELECT — window_vals + counter_delta + first/last_ts + first_val.
	mid := NewQuery().From(innerMid.Frag())
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
	extrap.Select(As(durationToStartFrag(rangeStart, kind.isCounter()), "duration_to_start"))
	extrap.Select(As(durationToEndFrag(anchor), "duration_to_end"))

	// Outer SELECT — per-(series, anchor) row.
	outer := NewQuery().From(extrap.Frag())
	for _, g := range groupFrags {
		outer.Select(g)
	}
	outer.Select(Col("anchor_ts"))
	outer.Select(As(extrapolatedValueFrag(kind, rangeSeconds), r.ValueColumn))
	outer.Where(windowLenAtLeastFrag("window_vals", 2))

	e.emitSelect(outer)
	return nil
}

// firstTsFrag renders `tupleElement(window_pairs[1], 1)` — the first
// sample's timestamp (DateTime64(9)) extracted from the per-window
// pair array. Mirrors Prom's `samples.Floats[0].T`.
func firstTsFrag() Frag {
	return verbatim("tupleElement(window_pairs[1], 1)")
}

// lastTsFrag renders `tupleElement(window_pairs[length(window_pairs)], 1)`
// — the last sample's timestamp. Mirrors Prom's
// `samples.Floats[numSamplesMinusOne].T`.
func lastTsFrag() Frag {
	return verbatim("tupleElement(window_pairs[length(window_pairs)], 1)")
}

// firstValFrag renders `tupleElement(window_pairs[1], 2)` — the first
// sample's value, needed by the counter clamp-to-zero shortcut so the
// extrapolated rate doesn't dip below zero when the counter started
// inside the window.
func firstValFrag() Frag {
	return verbatim("tupleElement(window_pairs[1], 2)")
}

// rangeStartFrag renders `<end> - toIntervalNanosecond(<rangeNS>)` —
// Prom's `rangeStart = enh.Ts - durationMilliseconds(ms.Range+vs.Offset)`
// (functions.go:197). end may render arbitrary CH expressions; the
// rangeNS bound is inline.
func rangeStartFrag(end Frag, rangeNS int64) Frag {
	return Sub(end, Call("toIntervalNanosecond", InlineLit(rangeNS)))
}

// sampledIntervalFrag renders the per-window sampled interval in
// seconds (Float64): `dateDiff('nanosecond', first_ts, last_ts) / 1e9`.
// Mirrors Prom's `sampledInterval := float64(lastT-firstT) / 1000`
// (functions.go:258), substituting nanosecond precision for the
// millisecond timebase Prom carries.
func sampledIntervalFrag() Frag {
	return verbatim("toFloat64(dateDiff('nanosecond', first_ts, last_ts)) / 1e9")
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
// `anchor_ts - range`). The `isCounter` flag is kept for parity with
// the durationToEndFrag signature even though it has no effect at the
// duration_to_start layer — leaving the parameter in place keeps the
// emit call sites symmetric.
func durationToStartFrag(rangeStart Frag, _ bool) Frag {
	return func(b *Builder) {
		b.sb.WriteString("if(")
		writeDurationToStartRaw(b, rangeStart)
		b.sb.WriteString(" >= 1.1 * sampled_interval / (length(window_vals) - 1), ")
		b.sb.WriteString("sampled_interval / (length(window_vals) - 1) / 2, ")
		writeDurationToStartRaw(b, rangeStart)
		b.sb.WriteByte(')')
	}
}

// writeDurationToStartRaw renders the un-clamped duration-to-start in
// seconds: `toFloat64(dateDiff('nanosecond', rangeStart, first_ts)) / 1e9`.
// Mirrors Prom's `float64(firstT-rangeStart) / 1000`.
func writeDurationToStartRaw(b *Builder, rangeStart Frag) {
	b.sb.WriteString("toFloat64(dateDiff('nanosecond', ")
	rangeStart(b)
	b.sb.WriteString(", first_ts)) / 1e9")
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
	return func(b *Builder) {
		b.sb.WriteString("if(")
		writeDurationToEndRaw(b, rangeEnd)
		b.sb.WriteString(" >= 1.1 * sampled_interval / (length(window_vals) - 1), ")
		b.sb.WriteString("sampled_interval / (length(window_vals) - 1) / 2, ")
		writeDurationToEndRaw(b, rangeEnd)
		b.sb.WriteByte(')')
	}
}

// writeDurationToEndRaw renders the un-clamped duration-to-end in
// seconds: `toFloat64(dateDiff('nanosecond', last_ts, rangeEnd)) / 1e9`.
// Mirrors Prom's `float64(rangeEnd-lastT) / 1000`.
func writeDurationToEndRaw(b *Builder, rangeEnd Frag) {
	b.sb.WriteString("toFloat64(dateDiff('nanosecond', last_ts, ")
	rangeEnd(b)
	b.sb.WriteString(")) / 1e9")
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
func extrapolatedValueFrag(kind extrapolationKind, rangeSeconds float64) Frag {
	return func(b *Builder) {
		b.sb.WriteString("if(sampled_interval > 0, ")
		// raw result
		switch kind {
		case extrapolationKindDelta:
			b.sb.WriteString("(window_vals[length(window_vals)] - first_val)")
		default:
			b.sb.WriteString("counter_delta")
		}
		// counter clamp-to-zero rewrite of durationToStart: when the
		// counter started inside the window, Prom shortens the
		// durationToStart to the implied zero-crossing of the linear
		// extrapolation so the rate stays non-negative. Encoded inline
		// at the `factor` level via:
		//
		//   factor = (sampled_interval +
		//             least(duration_to_start, duration_to_zero) +
		//             duration_to_end) / sampled_interval
		//
		// duration_to_zero = sampled_interval * (first_val / counter_delta)
		// only when `counter_delta > 0 && first_val >= 0`; otherwise we
		// keep the un-clamped duration_to_start.
		b.sb.WriteString(" * (sampled_interval + ")
		if kind.isCounter() {
			// least() with the counter zero-crossing guard.
			b.sb.WriteString("if(counter_delta > 0 AND first_val >= 0, ")
			b.sb.WriteString("least(duration_to_start, sampled_interval * first_val / counter_delta), ")
			b.sb.WriteString("duration_to_start)")
		} else {
			b.sb.WriteString("duration_to_start")
		}
		b.sb.WriteString(" + duration_to_end) / sampled_interval")
		if kind.isRate() {
			b.sb.WriteString(" / ")
			b.sb.WriteString(formatFloat(rangeSeconds))
		}
		b.sb.WriteString(", nan)")
	}
}

// emitWindowedArray writes the windowed-array SQL skeleton with the
// value Frag substituted in the outer SELECT position. The Frag can
// reference `window_vals` (Array(Float64)) and `counter_delta`
// (Float64); args bound inside it land at the outer SELECT position so
// positional `?` ordering follows the SQL stream.
//
// When r.OuterRange > 0 emission switches to the matrix path: each
// series emits N rows, one per anchor across [End-OuterRange, End]
// spaced by Step (end-inclusive). The outer SELECT additionally
// projects the anchor timestamp as `anchor_ts`.
//
// minWindowSize controls the PromQL "drop empty windows" semantics:
// when > 0, the outer SELECT adds `WHERE length(window_vals) >= N`
// so series (or (series, anchor) rows in the matrix shape) whose
// window holds fewer than N samples are dropped from the result —
// matching Prom's behaviour for rate / increase / delta / *_over_time,
// which all return no sample for those windows. 0 disables the filter
// (LogQL log_rate emits 0 for empty windows).
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
	if len(groupFrags) > 0 {
		innermost.GroupBy(groupFrags...)
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
// emits N rows, one per anchor across [End-OuterRange, End] spaced by
// Step (end-inclusive). The innermost SELECT computes the per-series
// (TimeUnix, Value) array once via groupArray + arraySort, then an
// arrayJoin in the next layer fans out one row per anchor. Subsequent
// layers operate on the per-(series, anchor) tuple.
//
// SQL skeleton (with N = OuterRange/Step + 1):
//
//	SELECT series_key, anchor_ts, <valueFrag> AS value FROM (
//	  SELECT series_key, anchor_ts, <window_vals + counter_delta> FROM (
//	    SELECT series_key, anchor_ts, arrayFilter(p -> p.1 in [anchor_ts - range, anchor_ts], series_array) AS window_pairs FROM (
//	      SELECT series_key, series_array,
//	        arrayJoin(arrayMap(i -> <end> - toIntervalNanosecond(i * <step_ns>), range(0, N))) AS anchor_ts
//	      FROM (
//	        SELECT series_key, arraySort(groupArray((TimeUnix, Value))) AS series_array
//	        FROM (<input>) GROUP BY series_key
//	      )
//	    )
//	  )
//	)
func (e *emitter) emitWindowedArrayMatrix(r *chplan.RangeWindow, value Frag, minWindowSize int) error {
	end := endExprFrag(r)
	rangeNS := r.Range.Nanoseconds()
	stepNS := r.Step.Nanoseconds()
	// End-inclusive anchor count. e.g. [5m:2m] = 5m/2m + 1 = 3 anchors
	// at end, end-2m, end-4m. Truncating division matches Prom semantics.
	numAnchors := r.OuterRange.Nanoseconds()/stepNS + 1
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
	if len(groupFrags) > 0 {
		innermost.GroupBy(groupFrags...)
	}

	// Anchor-fanout SELECT — arrayJoin produces one row per anchor.
	fanout := NewQuery().From(innermost.Frag())
	for _, g := range groupFrags {
		fanout.Select(g)
	}
	fanout.Select(Col("series_array"))
	fanout.Select(As(anchorFanoutFrag(end, stepNS, numAnchors), "anchor_ts"))

	// Inner-middle SELECT — arrayFilter to [anchor_ts - range, anchor_ts].
	innerMid := NewQuery().From(fanout.Frag())
	for _, g := range groupFrags {
		innerMid.Select(g)
	}
	innerMid.Select(Col("anchor_ts"))
	innerMid.Select(As(windowFilterPairsFrag(verbatim("anchor_ts"), rangeNS), "window_pairs"))

	// Middle SELECT — window_vals + counter_delta per (series, anchor).
	mid := NewQuery().From(innerMid.Frag())
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
