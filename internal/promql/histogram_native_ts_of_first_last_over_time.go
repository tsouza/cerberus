package promql

import (
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_ts_of_first_last_over_time.go lowers
// `ts_of_first_over_time(<exp-histogram selector>[range])` /
// `ts_of_last_over_time(<exp-histogram selector>[range])` (cerberus issue
// #2482, the ts_of_first/ts_of_last half — see
// range_fns.go's rangeVectorFloatOnlyDropFuncs for the ts_of_max/ts_of_min
// half, which joins the existing DROP class instead of needing a file of
// its own).
//
// Reference (tsouza/prometheus@cerberus-parser promql/functions.go,
// funcTsOfFirstOverTime / funcTsOfLastOverTime — cited in full in cerberus
// issue #2482):
//
//	var tf int64 = math.MaxInt64   // funcTsOfFirstOverTime; = 0 for Last
//	if len(el.Floats) > 0 { tf = el.Floats[0].T }        // [len-1].T for Last
//	var th int64 = math.MaxInt64   // = 0 for Last
//	if len(el.Histograms) > 0 { th = el.Histograms[0].T }  // [len-1].T for Last
//	return Sample{Metric: el.Metric, F: float64(min(tf, th)) / 1000}  // max(...) for Last
//
// i.e. the EARLIER (first) / LATER (last) of the window's own float and
// histogram timestamps, reported as an epoch-SECONDS float. Over a window
// holding EXCLUSIVELY histogram samples (the one shape a bare exp-histogram
// selector can ever produce — cerberus never mixes a metric's samples
// across the float and exp-histogram tables), tf's sentinel default drops
// out of the min/max entirely: `min(tf, th) == th` for ts_of_first
// (real millisecond timestamps are always < math.MaxInt64), and
// `max(tf, th) == th` for ts_of_last (real timestamps are always > the
// zero sentinel), so both reduce to just the histogram's own EARLIEST
// (first) / LATEST (last) in-window sample timestamp — the exact same
// argMin/argMax-by-TimeUnix selection direction
// histogram_native_last_first_over_time.go's last_over_time/first_over_time
// already apply, just reporting the picked TIMESTAMP itself as the output
// VALUE instead of preserving the picked sample.
//
// `__name__` is dropped, NOT preserved, despite `Sample{Metric: el.Metric,
// ...}` appearing to keep it: reference Prometheus's engine strips
// `__name__` from EVERY range-vector function's output except literally
// `last_over_time` / `first_over_time` (tsouza/prometheus's
// promql/engine.go — `dropName := (e.Func.Name != "last_over_time" &&
// e.Func.Name != "first_over_time")` — this cerberus package's own
// [rangeFnPreservesName] already encodes that exact exception list for the
// classic float path), so `ts_of_first_over_time` / `ts_of_last_over_time`
// hit dropName=true regardless of what the function body itself assigns to
// Sample.Metric. Verified directly against cerberus's own EXISTING classic
// (plain-float) lowering for these two functions
// (test/spec/promql/ts_of_first_over_time.txtar's `-- expected_rows --`
// carries no metric name, and its `-- chplan --` / `-- sql --` project only
// (Attributes, Value) — no MetricName column at all) rather than trusting
// the "PRESERVED" claim in cerberus issue #2482's own filing text, which
// read only the function body and missed the engine-level dropName
// override; this file's exp-histogram lowering matches that VERIFIED
// existing behaviour instead.
//
// The plan shape mirrors [lowerTimestampOverExpHistogramBareSelector]
// (histogram_native_timestamp.go) rung for rung — same three range-grid
// branches, same instant-vs-range projection split, same ms-truncated
// epoch-seconds value expression — differing only in: the window bound is
// `ms.Range` (matching last_over_time/first_over_time's own windowed
// selection) rather than the fixed staleness lookback `timestamp()` uses,
// and the picked-sample aggregate is directional (MIN for first, MAX for
// last) rather than always the newest sample.
const (
	tsOfFirstOverTimeExpHistFn = "ts_of_first_over_time"
	tsOfLastOverTimeExpHistFn  = "ts_of_last_over_time"
)

// tsOfFirstLastOverExpHistogram recognises `ts_of_first_over_time(<selector>[r])`
// / `ts_of_last_over_time(<selector>[r])` where the selector names an
// exp-histogram metric — the one shape this file answers. Mirrors
// [lastFirstOverExpHistogram]'s own recognizer shape rung for rung,
// differing only in which two function names it admits.
func tsOfFirstLastOverExpHistogram(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (fn string, ms *parser.MatrixSelector, vs *parser.VectorSelector, ok bool) {
	if s.ExpHistogramTable == "" || ctx.metadataFullRange {
		return "", nil, nil, false
	}
	call, ok := peelWrappers(expr).(*parser.Call)
	if !ok || (call.Func.Name != tsOfFirstOverTimeExpHistFn && call.Func.Name != tsOfLastOverTimeExpHistFn) || len(call.Args) != 1 {
		return "", nil, nil, false
	}
	ms, ok = peelWrappers(call.Args[0]).(*parser.MatrixSelector)
	if !ok || ms.Range <= 0 {
		return "", nil, nil, false
	}
	vs, ok = peelWrappers(ms.VectorSelector).(*parser.VectorSelector)
	if !ok || !s.IsExpHistogramMetric(metricNameFromMatchers(vs.LabelMatchers)) {
		return "", nil, nil, false
	}
	return call.Func.Name, ms, vs, true
}

// tsOfSampleTimestampAlias names the one column
// [tsOfSampleTimestampAgg] adds: the raw, direction-selected TimeUnix of
// the picked sample. Deliberately distinct from both s.TimestampColumn
// (the RANGE-mode branches use it for the STEP ANCHOR — see
// [tsOfRangeProjection]) and [stepGridAnchorColumn], mirroring
// [expHistogramSampleTimestampAlias]'s own reasoning.
const tsOfSampleTimestampAlias = "ts_of_exp_hist_sample_ts"

// tsOfSampleTimestampAgg is the aggregate ts_of_first_over_time /
// ts_of_last_over_time need from a bare exp-histogram selector: just the
// direction-selected raw TimeUnix — MIN (earliest) for
// ts_of_first_over_time, MAX (latest) for ts_of_last_over_time. Unlike
// [nativeExpHistBareAggsDirectional] this never reads a single field of
// the histogram itself (bucket ladder, count, sum): the reported VALUE is
// the timestamp, not the histogram, so a plain MIN/MAX suffices — no
// argMin/argMax-by-TimeUnix column correlation is needed.
func tsOfSampleTimestampAgg(fn string, s schema.Metrics) []chplan.AggFunc {
	aggFn := chplan.FnMax
	if fn == tsOfFirstOverTimeExpHistFn {
		aggFn = chplan.FnMin
	}
	return []chplan.AggFunc{
		{
			Fn:    aggFn,
			Args:  []chplan.Expr{&chplan.ColumnRef{Name: s.TimestampColumn}},
			Alias: tsOfSampleTimestampAlias,
		},
	}
}

// lowerTsOfFirstLastOverExpHistogram lowers the recognised shape across
// the three evaluation grids [rangeGridShapeFor] distinguishes — see
// [lowerExpHistogramBare] for what each means.
func lowerTsOfFirstLastOverExpHistogram(fn string, ms *parser.MatrixSelector, vs *parser.VectorSelector, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	switch rangeGridShapeFor(vs, ctx) {
	case gridFanout:
		scan := &chplan.Scan{Table: s.ExpHistogramTable}
		pred := buildPredicate(vs.LabelMatchers, s)
		fanout := buildHistogramBucketFanout(
			scan, pred, nil, windowFor(vs, ms.Range),
			[]chplan.Expr{histogramIdentityExpr(s)}, []string{s.AttributesColumn},
			tsOfSampleTimestampAgg(fn, s), s, ctx,
		)
		return tsOfRangeProjection(fanout, s), nil
	case gridBroadcast:
		// Same split as [lowerTimestampOverExpHistogramBareSelector]'s own
		// gridBroadcast arm: the pinned window is resolved ONCE via the
		// instant-mode aggregate, then fanned across the step grid — every
		// step reports the SAME picked sample's timestamp value, at its
		// own step position.
		selected, err := tsOfSampleTimestampSelected(fn, ms, vs, s, ctx)
		if err != nil {
			return nil, err
		}
		grid := &chplan.StepGrid{Start: ctx.start.UTC(), End: ctx.end.UTC(), Step: ctx.step}
		return tsOfRangeProjection(&chplan.CrossJoin{Left: grid, Right: selected}, s), nil
	default:
		selected, err := tsOfSampleTimestampSelected(fn, ms, vs, s, ctx)
		if err != nil {
			return nil, err
		}
		return tsOfInstantProjection(selected, s), nil
	}
}

// tsOfSampleTimestampSelected builds the instant-mode subtree beneath
// [lowerTsOfFirstLastOverExpHistogram]'s single-anchor and gridBroadcast
// branches: the filtered exp-histogram scan, windowed to `(anchor -
// ms.Range, anchor]` (the same window [expHistogramLastFirstWindowed]
// applies), collapsed to one row per series via [tsOfSampleTimestampAgg]'s
// direction-selected MIN/MAX.
func tsOfSampleTimestampSelected(fn string, ms *parser.MatrixSelector, vs *parser.VectorSelector, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	anchor, err := selectorAnchor(vs, ctx)
	if err != nil {
		return nil, err
	}
	pred := buildPredicate(vs.LabelMatchers, s)
	pred = andExpr(pred, timeBoundExpr(s.TimestampColumn, anchor))
	pred = andExpr(pred, stalenessLowerBoundExpr(s.TimestampColumn, anchor, ms.Range))
	var input chplan.Node = &chplan.Scan{Table: s.ExpHistogramTable}
	if pred != nil {
		input = &chplan.Filter{Input: input, Predicate: pred}
	}
	return latestSampleAgg(input, tsOfSampleTimestampAgg(fn, s), s), nil
}

// tsOfInstantProjection projects the INSTANT-mode picked-timestamp
// aggregate into the canonical float Sample shape ts_of_first_over_time /
// ts_of_last_over_time report: `__name__` drops (see this file's doc for
// why, despite reference's Sample literal appearing to preserve it), the
// reported TimeUnix is the evaluation instant (matching the classic
// plain-float path's own reported timestamp for these two functions —
// neither is in [rangeFnPreservesName], so neither gets a raw-sample
// timestamp column of its own), and the Value is the picked sample's own
// timestamp, converted to ms-precision epoch seconds via
// [tsOfEpochSecondsExpr].
func tsOfInstantProjection(inner chplan.Node, s schema.Metrics) chplan.Node {
	return tsOfSelectProjection(inner, chplan.NowNano(), s)
}

// tsOfRangeProjection projects a RANGE-mode plan — either
// [buildHistogramBucketFanout]'s per-(series,anchor) fan-out or the
// gridBroadcast CrossJoin — into the canonical float Sample shape. The
// reported TimeUnix is the STEP ANCHOR (every range-mode row is positioned
// along the matrix by its anchor, matching every other range-mode
// exp-histogram consumer); the Value stays the picked sample's own
// timestamp via [tsOfSampleTimestampAlias].
func tsOfRangeProjection(inner chplan.Node, s schema.Metrics) chplan.Node {
	return tsOfSelectProjection(inner, &chplan.ColumnRef{Name: stepGridAnchorColumn}, s)
}

// tsOfSelectProjection is the shared cap [tsOfInstantProjection] /
// [tsOfRangeProjection] apply, factored out so
// histogram_native_subquery_select.go's subquery sibling
// ([capSelectFnOverSubquery]) can reuse the identical float-quartet shape
// with its own tsExpr (the subquery-anchor-conditional expression
// [lowerSelectFnOverExpHistogramSubquery] derives) instead of either of
// this pair's two hardcoded choices.
func tsOfSelectProjection(inner chplan.Node, tsExpr chplan.Expr, s schema.Metrics) chplan.Node {
	rawTs := &chplan.ColumnRef{Name: tsOfSampleTimestampAlias}
	return &chplan.Project{
		Input: inner,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: tsExpr, Alias: s.TimestampColumn},
			{Expr: tsOfEpochSecondsExpr(rawTs), Alias: s.ValueColumn},
		},
	}
}

// tsOfEpochSecondsExpr renders `toFloat64(toUnixTimestamp64Milli(<ts>)) /
// 1000` — the DateTime64(9) → epoch-seconds (ms-precision) conversion
// ts_of_first_over_time / ts_of_last_over_time report, chplan-Expr-level
// mirror of chsql's [chsql.tsEpochSecondsFrag] (the classic plain-float
// path's own identical conversion — see that function's doc for why
// TRUNCATING to milliseconds first, rather than dividing a nanosecond
// count by 1e9, is the correct match for Prometheus's own
// millisecond-resolution Point.T: a plain nanosecond division would leak
// sub-millisecond precision from the exp-histogram table's DateTime64(9)
// column that Prometheus's own int64-millisecond Sample.T never carries,
// diverging from reference on data with genuine sub-ms timestamps even
// though the classic float path (fed the identical seed data) would not).
func tsOfEpochSecondsExpr(ts chplan.Expr) chplan.Expr {
	const millisPerSecond = 1000
	return &chplan.Binary{
		Op: chplan.OpDiv,
		Left: &chplan.FuncCall{
			Fn:   chplan.FnToFloat64,
			Args: []chplan.Expr{&chplan.FuncCall{Fn: chplan.FnToUnixMillis, Args: []chplan.Expr{ts}}},
		},
		Right: &chplan.LitFloat{V: float64(millisPerSecond)},
	}
}
