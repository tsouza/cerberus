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
// histogram timestamps, reported as an epoch-SECONDS float. The two
// sentinels are not symmetric:
//
//   - ts_of_first's absent kind defaults to math.MaxInt64, which no real
//     millisecond timestamp reaches, so `min(tf, th)` is always the
//     window's EARLIEST sample timestamp, whichever kinds it holds.
//   - ts_of_last's absent kind defaults to 0, the Unix epoch. A window
//     holding only ONE kind of sample therefore answers
//     `max(t, 0)`: its LATEST sample timestamp, or 0 when that sample
//     lies before 1970. A window holding BOTH kinds has no sentinel left
//     in the max and answers its latest sample timestamp even before
//     1970.
//
// A bare exp-histogram selector only ever produces a histogram-only window
// (cerberus never mixes a metric's samples across the float and
// exp-histogram tables), so its ts_of_last is `max(th, 0)`. A Mixed
// relation (a subquery over a float/histogram `or`) can hold both kinds
// in one window, so its reduction also counts the kinds the window holds
// — see [tsOfSampleTimestampAgg] and [tsOfValueExpr]. The picked
// timestamp follows the exact same MIN/MAX-by-TimeUnix selection direction
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
	if !expHistogramLoweringAvailable(s, ctx) {
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

// tsOfWindowKindsAlias names the column [tsOfSampleTimestampAgg] adds for
// ts_of_last_over_time over a Mixed relation: how many distinct sample
// kinds (float, histogram) the window holds, read back by [tsOfValueExpr].
const tsOfWindowKindsAlias = "ts_of_window_kinds"

// tsOfBothSampleKinds is the [tsOfWindowKindsAlias] count of a window
// holding float and histogram samples alike.
const tsOfBothSampleKinds = 2

// tsOfSampleTimestampAgg is the aggregate ts_of_first_over_time /
// ts_of_last_over_time need from a histogram-valued or Mixed window: the
// direction-selected raw TimeUnix — MIN (earliest) for
// ts_of_first_over_time, MAX (latest) for ts_of_last_over_time. Unlike
// [nativeExpHistBareAggsDirectional] this never reads a single field of
// the histogram itself (bucket ladder, count, sum): the reported VALUE is
// the timestamp, not the histogram, so a plain MIN/MAX suffices — no
// argMin/argMax-by-TimeUnix column correlation is needed.
//
// discriminator is a Mixed input's discriminator column
// ([mixedDiscriminator]), nil for a single-kind input. Over a Mixed input
// ts_of_last_over_time also counts its distinct values as
// [tsOfWindowKindsAlias]: whether the window holds both kinds decides
// whether reference's zero default for an absent kind takes part in its
// max (see this file's top-level doc).
func tsOfSampleTimestampAgg(fn string, discriminator *chplan.ColumnRef, s schema.Metrics) []chplan.AggFunc {
	aggFn := chplan.FnMax
	if fn == tsOfFirstOverTimeExpHistFn {
		aggFn = chplan.FnMin
	}
	aggs := []chplan.AggFunc{
		{
			Fn:    aggFn,
			Args:  []chplan.Expr{&chplan.ColumnRef{Name: s.TimestampColumn}},
			Alias: tsOfSampleTimestampAlias,
		},
	}
	if fn == tsOfLastOverTimeExpHistFn && discriminator != nil {
		aggs = append(aggs, chplan.AggFunc{
			Fn:    chplan.FnUniqExact,
			Args:  []chplan.Expr{discriminator},
			Alias: tsOfWindowKindsAlias,
		})
	}
	return aggs
}

// mixedDiscriminator returns input's discriminator column when input is a
// Mixed float/histogram relation, and nil otherwise.
func mixedDiscriminator(input chplan.Node) *chplan.ColumnRef {
	row := input.RowType()
	if row.SampleKind() != chplan.SampleKindMixed {
		return nil
	}
	return requireSampleRole(row, chplan.RoleDiscriminator)
}

// lowerTsOfFirstLastOverExpHistogram lowers the recognised shape across
// the three evaluation grids [rangeGridShapeFor] distinguishes — see
// [lowerExpHistogramBare] for what each means.
func lowerTsOfFirstLastOverExpHistogram(fn string, ms *parser.MatrixSelector, vs *parser.VectorSelector, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	switch rangeGridShapeFor(vs, ctx) {
	case gridFanout:
		scan := &chplan.Scan{Roles: metricScanRoles(s, s.ExpHistogramTable), Table: s.ExpHistogramTable}
		pred := buildPredicate(vs.LabelMatchers, s)
		fanout := buildRangeHistogramBucketFanout(
			scan, pred, nil, windowFor(vs, ms.Range),
			[]chplan.Expr{histogramIdentityExpr(s)}, []string{s.AttributesColumn},
			tsOfSampleTimestampAgg(fn, nil, s), s, ctx,
		)
		return tsOfRangeProjection(fn, fanout, s, ctx), nil
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
		return tsOfRangeProjection(fn, &chplan.CrossJoin{Left: grid, Right: selected}, s, ctx), nil
	default:
		selected, err := tsOfSampleTimestampSelected(fn, ms, vs, s, ctx)
		if err != nil {
			return nil, err
		}
		return tsOfInstantProjection(fn, selected, s, ctx), nil
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
	pred = withHistogramRangeStaleDrop(pred, s)
	var input chplan.Node = &chplan.Scan{Roles: metricScanRoles(s, s.ExpHistogramTable), Table: s.ExpHistogramTable}
	if pred != nil {
		input = &chplan.Filter{Input: input, Predicate: pred}
	}
	return latestSampleAgg(input, tsOfSampleTimestampAgg(fn, nil, s), s), nil
}

// tsOfInstantProjection projects the INSTANT-mode picked-timestamp
// aggregate into the canonical float Sample shape ts_of_first_over_time /
// ts_of_last_over_time report: `__name__` drops (see this file's doc for
// why, despite reference's Sample literal appearing to preserve it), the
// reported TimeUnix is the evaluation instant (matching the classic
// plain-float path's own reported timestamp for these two functions —
// neither is in [rangeFnPreservesName], so neither gets a raw-sample
// timestamp column of its own), and the Value is the picked sample's own
// timestamp as reference reports it ([tsOfValueExpr]).
func tsOfInstantProjection(fn string, inner chplan.Node, s schema.Metrics, ctx lowerCtx) chplan.Node {
	return tsOfSelectProjection(fn, nil, inner, chplan.NowNano(), s, ctx)
}

// tsOfRangeProjection projects a RANGE-mode plan — either
// [buildHistogramBucketFanout]'s per-(series,anchor) fan-out or the
// gridBroadcast CrossJoin — into the canonical float Sample shape. The
// reported TimeUnix is the STEP ANCHOR (every range-mode row is positioned
// along the matrix by its anchor, matching every other range-mode
// exp-histogram consumer); the Value stays the picked sample's own
// timestamp via [tsOfSampleTimestampAlias].
func tsOfRangeProjection(fn string, inner chplan.Node, s schema.Metrics, ctx lowerCtx) chplan.Node {
	return tsOfSelectProjection(fn, nil, inner, &chplan.ColumnRef{Name: stepGridAnchorColumn}, s, ctx)
}

// tsOfSelectProjection is the shared cap [tsOfInstantProjection] /
// [tsOfRangeProjection] apply, factored out so
// histogram_native_subquery_select.go's subquery sibling
// ([capSelectFnOverSubquery]) can reuse the identical float-quartet shape
// with its own tsExpr (the subquery-anchor-conditional expression
// [lowerSelectFnOverExpHistogramSubquery] derives) instead of either of
// this pair's two hardcoded choices. fn and discriminator are the
// reduction's own ([tsOfSampleTimestampAgg]), which [tsOfValueExpr]
// reads back.
func tsOfSelectProjection(fn string, discriminator *chplan.ColumnRef, inner chplan.Node, tsExpr chplan.Expr, s schema.Metrics, ctx lowerCtx) chplan.Node {
	return &chplan.Project{
		Roles: metricRoles(s),
		Input: inner,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: tsExpr, Alias: s.TimestampColumn},
			{Expr: tsOfValueExpr(fn, discriminator, ctx), Alias: s.ValueColumn},
		},
	}
}

// tsOfValueExpr renders the value reference Prometheus reports for the
// window [tsOfSampleTimestampAgg] reduced, over its picked timestamp's
// Unix milliseconds:
//
//   - ts_of_first_over_time: the milliseconds themselves.
//   - ts_of_last_over_time over a single-kind window: `greatest(<ms>, 0)`,
//     reference's zero default for the absent kind.
//   - ts_of_last_over_time over a Mixed relation (discriminator non-nil):
//     `if(<kinds> = 2, <ms>, greatest(<ms>, 0))` — the zero default takes
//     part only where the window lacks one of the two kinds.
//
// The milliseconds are [lowerCtx.sampleMillis]'s, so a shifted frame's
// shift is taken back before the floor. Each is then reported as
// [millisSecondsValue]'s `toFloat64(<ms>) / 1000`,
// Prometheus's `float64(t) / 1000`.
func tsOfValueExpr(fn string, discriminator *chplan.ColumnRef, ctx lowerCtx) chplan.Expr {
	millis := ctx.sampleMillis(&chplan.ColumnRef{Name: tsOfSampleTimestampAlias})
	if fn != tsOfLastOverTimeExpHistFn {
		return millisSecondsValue(millis)
	}
	floored := &chplan.FuncCall{Fn: chplan.FnGreatest, Args: []chplan.Expr{millis, &chplan.LitInt{V: 0}}}
	if discriminator == nil {
		return millisSecondsValue(floored)
	}
	return millisSecondsValue(&chplan.FuncCall{Fn: chplan.FnIf, Args: []chplan.Expr{
		&chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: tsOfWindowKindsAlias},
			Right: &chplan.LitInt{V: tsOfBothSampleKinds},
		},
		millis,
		floored,
	}})
}
