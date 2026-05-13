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
// instead (see emitWindowedArrayMatrixMetrics).
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

	sb := NewSelect().From(sub)
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
	if r.Identity {
		return e.emitRangeWindowIdentity(r)
	}
	switch r.Func {
	case "rate":
		return e.emitRangeWindowRate(r)
	case "increase":
		return e.emitRangeWindowIncrease(r)
	case "sum_over_time", "avg_over_time", "min_over_time", "max_over_time", "count_over_time", "last_over_time":
		return e.emitRangeWindowOverTime(r)
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
	return e.emitWindowedArrayPairs(r, func() error {
		// arrayMap to derive xs (seconds from anchor) and ys (values).
		// window_pairs is Array(Tuple(DateTime64(9), Float64)).
		e.b.WriteString("if(length(window_pairs) > 1, ")
		e.b.WriteString("tupleElement(simpleLinearRegression(")
		e.b.WriteString("arrayMap(p -> dateDiff('second', ")
		e.b.WriteString(anchorExpr(r))
		e.b.WriteString(", tupleElement(p, 1)), window_pairs), ")
		e.b.WriteString("arrayMap(p -> tupleElement(p, 2), window_pairs)")
		e.b.WriteString("), 2) + tupleElement(simpleLinearRegression(")
		e.b.WriteString("arrayMap(p -> dateDiff('second', ")
		e.b.WriteString(anchorExpr(r))
		e.b.WriteString(", tupleElement(p, 1)), window_pairs), ")
		e.b.WriteString("arrayMap(p -> tupleElement(p, 2), window_pairs)")
		e.b.WriteString("), 1) * ")
		if err := e.bindArg(t); err != nil {
			return err
		}
		e.b.WriteString(", nan)")
		return nil
	})
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
	return e.emitWindowedArray(r, holtWintersValueExpr(sf, tf))
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
func (e *emitter) emitWindowedArrayPairs(r *chplan.RangeWindow, valueWriter func() error) error {
	if r.TimestampColumn == "" {
		return fmt.Errorf("%w: RangeWindow.TimestampColumn unset", ErrUnsupported)
	}
	if r.ValueColumn == "" {
		return fmt.Errorf("%w: RangeWindow.ValueColumn unset", ErrUnsupported)
	}
	if r.OuterRange > 0 {
		return fmt.Errorf("%w: predict_linear over subquery not yet supported", ErrUnsupported)
	}
	endExpr := timeOrNow(r.End)
	if r.Offset > 0 {
		endExpr = "(" + endExpr + " - toIntervalNanosecond(" + strconv.FormatInt(r.Offset.Nanoseconds(), 10) + "))"
	}
	rangeNS := r.Range.Nanoseconds()
	groupKeys, err := e.collectGroupBy(r.GroupBy)
	if err != nil {
		return err
	}

	// Outer SELECT — final value per series.
	e.b.WriteString("SELECT ")
	e.writeGroupSelectList(groupKeys)
	e.b.WriteString(", ")
	if err := valueWriter(); err != nil {
		return err
	}
	e.b.WriteString(" AS value FROM (")

	// Inner SELECT — arrayFilter to the [end-range, end] window.
	e.b.WriteString("SELECT ")
	e.writeGroupSelectList(groupKeys)
	fmt.Fprintf(&e.b, ", arrayFilter(p -> tupleElement(p, 1) >= %s - toIntervalNanosecond(%d) AND tupleElement(p, 1) <= %s, series_array) AS window_pairs FROM (",
		endExpr, rangeNS, endExpr)

	// Innermost SELECT — groupArray of (ts, value), sorted.
	e.b.WriteString("SELECT ")
	e.writeGroupSelectList(groupKeys)
	fmt.Fprintf(&e.b, ", arraySort(groupArray((%s, %s))) AS series_array FROM ",
		quoteIdent(r.TimestampColumn), quoteIdent(r.ValueColumn))
	if err := e.emitSubquery(r.Input); err != nil {
		return err
	}
	if len(groupKeys) > 0 {
		e.b.WriteString(" GROUP BY ")
		e.writeGroupSelectList(groupKeys)
	}
	e.b.WriteByte(')')

	e.b.WriteByte(')')
	return nil
}

// anchorExpr returns the SQL expression for the RangeWindow's window
// anchor (End - Offset, or now64(9) - Offset for the zero-End case).
// Used by predict_linear to compute per-sample seconds-from-anchor.
func anchorExpr(r *chplan.RangeWindow) string {
	base := timeOrNow(r.End)
	if r.Offset > 0 {
		base = "(" + base + " - toIntervalNanosecond(" + strconv.FormatInt(r.Offset.Nanoseconds(), 10) + "))"
	}
	return base
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

	endExpr := timeOrNow(r.End)
	if r.Offset > 0 {
		endExpr = "(" + endExpr + " - toIntervalNanosecond(" + strconv.FormatInt(r.Offset.Nanoseconds(), 10) + "))"
	}
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

	// Build the per-span anchor-fanout subquery via SelectBuilder so
	// args bound by GroupBy expressions land in the right position.
	// We then wrap it in the outer SELECT (the bucket reducer) by
	// hand because the WHERE clause references `anchor_ts` (the
	// arrayJoined column from the subquery) — a structure SelectBuilder
	// handles naturally via Frag callbacks.

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
	innerSb := NewSelect().From(inner)
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
		anchorFanoutFrag(endExpr, stepNS, numAnchors),
		"anchor_ts",
	)

	// Outer SELECT: GROUP BY group cols + anchor_ts; apply the
	// per-bucket reducer.
	outerSb := NewSelect().From(innerSb.Frag())

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
	tsRangeNS := rangeNS
	outerSb.Where(
		func(b *Builder) {
			b.sb.WriteString("ts >= anchor_ts - toIntervalNanosecond(")
			b.sb.WriteString(strconv.FormatInt(tsRangeNS, 10))
			b.sb.WriteByte(')')
		},
		func(b *Builder) {
			b.sb.WriteString("ts <= anchor_ts")
		},
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
// `arrayJoin(arrayMap(i -> <endExpr> - toIntervalNanosecond(i * <stepNS>), range(0, <N>)))`.
// Used by the matrix-shape RangeWindow emitter to fan each Inner row
// across N anchors in a single CH pass.
//
// endExpr is rendered verbatim (the CH expression for the eval-grid
// anchor base — typically a DateTime64 literal or `now64(9)`); stepNS
// and N are inline integer literals.
func anchorFanoutFrag(endExpr string, stepNS, numAnchors int64) Frag {
	return func(b *Builder) {
		b.sb.WriteString("arrayJoin(arrayMap(i -> ")
		b.sb.WriteString(endExpr)
		b.sb.WriteString(" - toIntervalNanosecond(i * ")
		b.sb.WriteString(strconv.FormatInt(stepNS, 10))
		b.sb.WriteString("), range(0, ")
		b.sb.WriteString(strconv.FormatInt(numAnchors, 10))
		b.sb.WriteString(")))")
	}
}

// metricsReducerFrag returns the per-bucket reducer Frag for the matrix
// emission path. rate normalises `count(1)` by dividing through the
// range duration in seconds (rendered as a literal — duration constants
// are query-shape, not user-data).
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
			b.ParamAgg(chName, paramFrags, argFrags)
			b.sb.WriteString(" / ")
			b.sb.WriteString(strconv.FormatFloat(rangeSeconds, 'f', -1, 64))
		}
	}
	return func(b *Builder) {
		b.ParamAgg(chName, paramFrags, argFrags)
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
// a Call.
func (e *emitter) emitRangeWindowIdentity(r *chplan.RangeWindow) error {
	return e.emitWindowedArray(r, "if(length(window_vals) > 0, window_vals[length(window_vals)], nan)")
}

// emitRangeWindowLogRate emits SQL for LogQL-style `rate({...}[range])`
// (and `bytes_rate`, after the lowering layer projects `length(Body)`
// as Value): `arraySum(window_vals) / range_seconds`. Distinct from
// PromQL's counter `rate`, which uses counter-reset-aware deltas.
//
// range_seconds binds as a parameter via the value-writer callback so
// the emitter stays free of new Sprintf-on-SQL instances (RC6 rule).
func (e *emitter) emitRangeWindowLogRate(r *chplan.RangeWindow) error {
	return e.emitWindowedArrayCb(r, func() error {
		e.b.WriteString("if(length(window_vals) > 0, arraySum(window_vals) / ")
		if err := e.bindArg(r.Range.Seconds()); err != nil {
			return err
		}
		e.b.WriteString(", 0.0)")
		return nil
	})
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
	return e.emitWindowedArray(r, rateValueExpr(r.Range.Seconds()))
}

// emitRangeWindowIncrease emits SQL for `increase(metric[range])`. Same
// as rate but without dividing by range_seconds.
func (e *emitter) emitRangeWindowIncrease(r *chplan.RangeWindow) error {
	return e.emitWindowedArray(r, "if(length(window_vals) > 1, counter_delta, 0.0)")
}

// emitRangeWindowOverTime emits SQL for the `*_over_time` family:
// sum_over_time, avg_over_time, min_over_time, max_over_time,
// count_over_time, last_over_time. These don't need counter-reset
// handling — they're straight array aggregations over the window's
// values.
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
	default:
		return fmt.Errorf("%w: over-time function %q", ErrUnsupported, r.Func)
	}
	return e.emitWindowedArray(r, inner)
}

// rateValueExpr returns the outer SELECT value expression for rate(),
// dividing the counter delta by range_seconds. Length check avoids
// dividing on a single-point window (rate is undefined there).
func rateValueExpr(rangeSeconds float64) string {
	return fmt.Sprintf("if(length(window_vals) > 1, counter_delta / %s, 0.0)",
		strconv.FormatFloat(rangeSeconds, 'f', -1, 64))
}

// emitWindowedArray writes the windowed-array SQL skeleton with valueExpr
// substituted in the outer SELECT position. valueExpr can reference
// `window_vals` (Array(Float64)) and `counter_delta` (Float64).
func (e *emitter) emitWindowedArray(r *chplan.RangeWindow, valueExpr string) error {
	return e.emitWindowedArrayCb(r, func() error {
		e.b.WriteString(valueExpr)
		return nil
	})
}

// emitWindowedArrayCb is the callback variant of emitWindowedArray. The
// valueWriter callback runs at the exact SQL position where the value
// expression lands; callers may bind args inside it (via e.bindArg) so
// `?` placeholders are emitted in lock-step with the args slice.
//
// When r.OuterRange > 0 emission switches to the matrix path: each
// series emits N rows, one per anchor across [End-OuterRange, End]
// spaced by Step (end-inclusive). The outer SELECT additionally
// projects the anchor timestamp as `anchor_ts`.
func (e *emitter) emitWindowedArrayCb(r *chplan.RangeWindow, valueWriter func() error) error {
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
		return e.emitWindowedArrayMatrix(r, valueWriter)
	}

	endExpr := timeOrNow(r.End)
	if r.Offset > 0 {
		endExpr = "(" + endExpr + " - toIntervalNanosecond(" + strconv.FormatInt(r.Offset.Nanoseconds(), 10) + "))"
	}
	rangeNS := r.Range.Nanoseconds()
	groupKeys, err := e.collectGroupBy(r.GroupBy)
	if err != nil {
		return err
	}

	// Outer SELECT — final value per series.
	e.b.WriteString("SELECT ")
	e.writeGroupSelectList(groupKeys)
	e.b.WriteString(", ")
	if err := valueWriter(); err != nil {
		return err
	}
	e.b.WriteString(" AS value FROM (")

	// Middle SELECT — derives window_vals + counter_delta from window_pairs.
	e.b.WriteString("SELECT ")
	e.writeGroupSelectList(groupKeys)
	e.b.WriteString(", arrayMap(p -> tupleElement(p, 2), window_pairs) AS window_vals")
	e.b.WriteString(", arraySum(arrayMap((p, c) -> if(c < p, c, c - p), ")
	e.b.WriteString("arrayPopBack(arrayMap(x -> tupleElement(x, 2), window_pairs)), ")
	e.b.WriteString("arrayPopFront(arrayMap(x -> tupleElement(x, 2), window_pairs))")
	e.b.WriteString(")) AS counter_delta FROM (")

	// Inner-middle SELECT — arrayFilter to the [end-range, end] window.
	e.b.WriteString("SELECT ")
	e.writeGroupSelectList(groupKeys)
	fmt.Fprintf(&e.b, ", arrayFilter(p -> tupleElement(p, 1) >= %s - toIntervalNanosecond(%d) AND tupleElement(p, 1) <= %s, series_array) AS window_pairs FROM (",
		endExpr, rangeNS, endExpr)

	// Innermost SELECT — groupArray of (ts, value), sorted.
	e.b.WriteString("SELECT ")
	e.writeGroupSelectList(groupKeys)
	fmt.Fprintf(&e.b, ", arraySort(groupArray((%s, %s))) AS series_array FROM ",
		quoteIdent(r.TimestampColumn), quoteIdent(r.ValueColumn))
	if err := e.emitSubquery(r.Input); err != nil {
		return err
	}
	if len(groupKeys) > 0 {
		e.b.WriteString(" GROUP BY ")
		e.writeGroupSelectList(groupKeys)
	}
	e.b.WriteByte(')')

	e.b.WriteByte(')')
	e.b.WriteByte(')')
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
//	SELECT series_key, anchor_ts, <valueExpr> AS value FROM (
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
func (e *emitter) emitWindowedArrayMatrix(r *chplan.RangeWindow, valueWriter func() error) error {
	endExpr := timeOrNow(r.End)
	if r.Offset > 0 {
		endExpr = "(" + endExpr + " - toIntervalNanosecond(" + strconv.FormatInt(r.Offset.Nanoseconds(), 10) + "))"
	}
	rangeNS := r.Range.Nanoseconds()
	stepNS := r.Step.Nanoseconds()
	// End-inclusive anchor count. e.g. [5m:2m] = 5m/2m + 1 = 3 anchors
	// at end, end-2m, end-4m. Truncating division matches Prom semantics.
	numAnchors := r.OuterRange.Nanoseconds()/stepNS + 1
	groupKeys, err := e.collectGroupBy(r.GroupBy)
	if err != nil {
		return err
	}

	// Outer SELECT — per-(series, anchor) row.
	e.b.WriteString("SELECT ")
	e.writeGroupSelectList(groupKeys)
	if len(groupKeys) > 0 {
		e.b.WriteString(", ")
	}
	e.b.WriteString("anchor_ts, ")
	if err := valueWriter(); err != nil {
		return err
	}
	e.b.WriteString(" AS value FROM (")

	// Middle SELECT — window_vals + counter_delta per (series, anchor).
	e.b.WriteString("SELECT ")
	e.writeGroupSelectList(groupKeys)
	if len(groupKeys) > 0 {
		e.b.WriteString(", ")
	}
	e.b.WriteString("anchor_ts, arrayMap(p -> tupleElement(p, 2), window_pairs) AS window_vals")
	e.b.WriteString(", arraySum(arrayMap((p, c) -> if(c < p, c, c - p), ")
	e.b.WriteString("arrayPopBack(arrayMap(x -> tupleElement(x, 2), window_pairs)), ")
	e.b.WriteString("arrayPopFront(arrayMap(x -> tupleElement(x, 2), window_pairs))")
	e.b.WriteString(")) AS counter_delta FROM (")

	// Inner-middle SELECT — arrayFilter to [anchor_ts - range, anchor_ts].
	e.b.WriteString("SELECT ")
	e.writeGroupSelectList(groupKeys)
	if len(groupKeys) > 0 {
		e.b.WriteString(", ")
	}
	fmt.Fprintf(&e.b, "anchor_ts, arrayFilter(p -> tupleElement(p, 1) >= anchor_ts - toIntervalNanosecond(%d) AND tupleElement(p, 1) <= anchor_ts, series_array) AS window_pairs FROM (",
		rangeNS)

	// Anchor-fanout SELECT — arrayJoin produces one row per anchor.
	e.b.WriteString("SELECT ")
	e.writeGroupSelectList(groupKeys)
	if len(groupKeys) > 0 {
		e.b.WriteString(", ")
	}
	fmt.Fprintf(&e.b, "series_array, arrayJoin(arrayMap(i -> %s - toIntervalNanosecond(i * %d), range(0, %d))) AS anchor_ts FROM (",
		endExpr, stepNS, numAnchors)

	// Innermost SELECT — groupArray of (ts, value), sorted.
	e.b.WriteString("SELECT ")
	e.writeGroupSelectList(groupKeys)
	if len(groupKeys) > 0 {
		e.b.WriteString(", ")
	}
	fmt.Fprintf(&e.b, "arraySort(groupArray((%s, %s))) AS series_array FROM ",
		quoteIdent(r.TimestampColumn), quoteIdent(r.ValueColumn))
	if err := e.emitSubquery(r.Input); err != nil {
		return err
	}
	if len(groupKeys) > 0 {
		e.b.WriteString(" GROUP BY ")
		e.writeGroupSelectList(groupKeys)
	}
	e.b.WriteByte(')')

	e.b.WriteByte(')')
	e.b.WriteByte(')')
	e.b.WriteByte(')')
	return nil
}

// collectGroupBy renders each GroupBy expression to an isolated string so
// it can be reused in SELECT list, GROUP BY, and reused for the outer
// SELECT in the windowed-array stack. Args captured by emitExpr go to the
// shared args slice (positions still increase across renders).
//
// Returns the rendered identifier list (each entry is a complete SQL
// fragment like `\`Attributes\“).
func (e *emitter) collectGroupBy(group []chplan.Expr) ([]string, error) {
	out := make([]string, 0, len(group))
	for _, g := range group {
		// Render to a separate buffer so we can reuse the string.
		sub := &emitter{args: e.args}
		if err := sub.emitExpr(g); err != nil {
			return nil, err
		}
		// Append any args captured by the sub-emitter back onto ours.
		e.args = sub.args
		out = append(out, sub.b.String())
	}
	return out, nil
}

func (e *emitter) writeGroupSelectList(group []string) {
	for i, g := range group {
		if i > 0 {
			e.b.WriteString(", ")
		}
		e.b.WriteString(g)
	}
}

// timeOrNow renders an explicit DateTime64(9) literal for a non-zero time
// or falls back to ClickHouse's `now64(9)` for the zero value (which is
// what the lowering produces today; M2.1 will start populating Start/End
// from the HTTP API's time params).
func timeOrNow(t time.Time) string {
	if t.IsZero() {
		return "now64(9)"
	}
	return "toDateTime64('" + t.UTC().Format("2006-01-02 15:04:05.000000000") + "', 9)"
}

// quoteIdent backtick-quotes a CH identifier; the existing writeIdent
// writes to a builder, so this is a tiny wrapper that returns the string.
func quoteIdent(name string) string {
	var b []byte
	b = append(b, '`')
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c == '`' {
			b = append(b, '`', '`')
			continue
		}
		b = append(b, c)
	}
	b = append(b, '`')
	return string(b)
}
