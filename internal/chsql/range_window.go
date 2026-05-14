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
	case "sum_over_time", "avg_over_time", "min_over_time", "max_over_time", "count_over_time", "last_over_time", "stddev_over_time":
		return e.emitRangeWindowOverTime(r)
	case "quantile_over_time":
		return e.emitRangeWindowQuantileOverTime(r)
	case "log_rate":
		return e.emitRangeWindowLogRate(r)
	case "predict_linear":
		return e.emitRangeWindowPredictLinear(r)
	case "holt_winters":
		return e.emitRangeWindowHoltWinters(r)
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
	anchor := anchorExprFrag(r)
	return e.emitWindowedArrayPairs(r, func(b *Builder) {
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
	}, 2)
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
func (e *emitter) emitWindowedArrayPairs(r *chplan.RangeWindow, valueWriter Frag, minWindowSize int) error {
	if r.TimestampColumn == "" {
		return fmt.Errorf("%w: RangeWindow.TimestampColumn unset", ErrUnsupported)
	}
	if r.ValueColumn == "" {
		return fmt.Errorf("%w: RangeWindow.ValueColumn unset", ErrUnsupported)
	}
	if r.OuterRange > 0 {
		return fmt.Errorf("%w: predict_linear over subquery not yet supported", ErrUnsupported)
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
	outerSb.Select(rawAs(valueWriter, r.ValueColumn))
	if minWindowSize > 0 {
		outerSb.Where(windowLenAtLeastFrag("window_pairs", minWindowSize))
	}

	e.emitSelect(outerSb)
	return nil
}

// anchorExprFrag returns a Frag rendering the RangeWindow's window
// anchor (End - Offset, or now64(9) - Offset for the zero-End case).
// Used by predict_linear to compute per-sample seconds-from-anchor.
func anchorExprFrag(r *chplan.RangeWindow) Frag {
	return endExprFrag(r)
}

// endExprFrag returns a Frag rendering `<End> [- toIntervalNanosecond(<offset>)]`.
// Shared by every windowed-array emitter; centralises the Offset
// branch.
func endExprFrag(r *chplan.RangeWindow) Frag {
	return func(b *Builder) {
		base := timeOrNowFrag(r.End)
		if r.Offset > 0 {
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
// arrayFilter clamp to the [end-range, end] window over the
// `series_array` alias. Thin wrapper over the chsql.RangeWindowFilter
// typed compound-idiom helper; kept as a local helper so the
// rangeNS-arithmetic stays a single inline literal rather than repeated
// `Sub(end, Call("toIntervalNanosecond", …))` boilerplate at every
// callsite.
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
// Form (instant eval at r.End, looking back r.Range):
//
//	SELECT
//	    series_key,
//	    if(length(window_vals) > 1, counter_delta / range_seconds, 0.0) AS value
//	FROM (
//	    SELECT
//	        series_key,
//	        arrayMap(p -> tupleElement(p, 2), window_pairs) AS window_vals,
//	        arraySum(arrayMap(
//	            (p, c) -> if(c < p, c, c - p),
//	            arrayPopBack(arrayMap(x -> tupleElement(x, 2), window_pairs)),
//	            arrayPopFront(arrayMap(x -> tupleElement(x, 2), window_pairs))
//	        )) AS counter_delta,
//	        <range_seconds> AS range_seconds
//	    FROM (
//	        SELECT
//	            series_key,
//	            arrayFilter(
//	                p -> tupleElement(p, 1) >= <end> - toIntervalNanosecond(<range_ns>)
//	                  AND tupleElement(p, 1) <= <end>,
//	                series_array
//	            ) AS window_pairs
//	        FROM (
//	            SELECT
//	                <group-by-keys> AS series_key,
//	                arraySort(groupArray((`TimeUnix`, `Value`))) AS series_array
//	            FROM (<input>)
//	            GROUP BY <group-by-keys>
//	        )
//	    )
//	)
func (e *emitter) emitRangeWindowRate(r *chplan.RangeWindow) error {
	// PromQL rate drops series whose window holds fewer than 2 samples
	// (matches Prom's funcRate / extrapolatedRate). The outer SELECT
	// gets `WHERE length(window_vals) >= 2`.
	return e.emitWindowedArray(r, rateValueFrag(r.Range.Seconds()), 2)
}

// emitRangeWindowIncrease emits SQL for `increase(metric[range])`. Same
// as rate but without dividing by range_seconds.
func (e *emitter) emitRangeWindowIncrease(r *chplan.RangeWindow) error {
	// PromQL increase drops series whose window holds fewer than 2
	// samples (matches Prom's funcIncrease / extrapolatedRate).
	return e.emitWindowedArray(r, verbatim("if(length(window_vals) > 1, counter_delta, 0.0)"), 2)
}

// emitRangeWindowOverTime emits SQL for the `*_over_time` family:
// sum_over_time, avg_over_time, min_over_time, max_over_time,
// count_over_time, last_over_time, stddev_over_time. These don't need
// counter-reset handling — they're straight array aggregations over
// the window's values.
//
// stddev_over_time uses CH's `arrayReduce('stddevPop', ...)` to match
// Prometheus's `Engine.evalAggrOverTime → varianceOverTime` which
// builds a Welford running estimator that divides squared deviations
// by N (population variance), not by N-1.
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
	default:
		return fmt.Errorf("%w: over-time function %q", ErrUnsupported, r.Func)
	}
	// Every *_over_time variant drops empty-window rows per Prom
	// semantics (Prom's funcSumOverTime / funcCountOverTime / etc. all
	// short-circuit on zero samples). The outer SELECT gets
	// `WHERE length(window_vals) >= 1`.
	return e.emitWindowedArray(r, verbatim(inner), 1)
}

// emitRangeWindowDelta emits SQL for `delta(v[range])`: the
// difference between the LAST and FIRST samples in the window. Unlike
// `increase`, delta is meant for gauges (no counter-reset arithmetic),
// so the value is simply `window_vals[N] - window_vals[1]`.
//
// PromQL `delta` returns NaN when the window holds fewer than 2
// samples — same as Prom's `funcDelta`.
func (e *emitter) emitRangeWindowDelta(r *chplan.RangeWindow) error {
	const expr = "if(length(window_vals) > 1, window_vals[length(window_vals)] - window_vals[1], nan)"
	// PromQL delta drops series whose window holds fewer than 2
	// samples (matches Prom's funcDelta).
	return e.emitWindowedArray(r, verbatim(expr), 2)
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

// rateValueFrag returns the outer SELECT value Frag for rate(),
// dividing the counter delta by range_seconds. Length check avoids
// dividing on a single-point window (rate is undefined there). The
// divisor is rendered as a literal float (query shape, not user data).
func rateValueFrag(rangeSeconds float64) Frag {
	return func(b *Builder) {
		b.sb.WriteString("if(length(window_vals) > 1, counter_delta / ")
		b.sb.WriteString(formatFloat(rangeSeconds))
		b.sb.WriteString(", 0.0)")
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
