package promql

import (
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_bare.go lowers a BARE selector over an OTel
// exponential (native) histogram — `<base>_exp_hist`, with no wrapping
// function — into a histogram-VALUED result.
//
// Reference Prometheus answers such a selector with a native-histogram
// sample: `Sample{H: *histogram.FloatHistogram}`, no float value. Every
// other cerberus lowering reduces a histogram row to a scalar before it
// reaches the wire (histogram_quantile / histogram_count /
// histogram_sum), which is why [expHistogramSelectorRouting] rejects the
// bare shape wherever it is NOT the whole query: a consumer that reads a
// `Value` column the histogram row shape does not carry would silently
// reduce the placeholder value. Such shapes keep their explicit
// rejection until each grows its own histogram-AWARE lowering — the one
// that exists so far is `sum()`, in histogram_native_sum.go, which
// consumes the bucket ladders rather than a Value (issue #1967).
//
// The plan shape mirrors the native-quantile siblings in
// histogram_quantile.go / histogram_quantile_range.go rung for rung —
// same Scan, same matcher predicate, same per-series `argMax(<col>,
// TimeUnix)` collapse, same [chplan.RangeBucketFanout] in range mode —
// and differs in exactly one place: the cap is a
// [chplan.HistogramProjection], which publishes the nine raw structural
// columns as final output instead of folding them into a quantile.
//
// Output-column contract. The projection's GroupBy/GroupByAliases carry
// the canonical sample quartet `(MetricName, Attributes, Timestamp,
// Value)` FIRST, so the emitted SELECT is those four followed by the
// nine `chplan.Histogram*Column` aliases — precisely the thirteen
// destinations internal/chclient's row cursor binds when it probes the
// final column for `HistogramNegativeBucketCounts`. That is also why
// api/prom's wrapWithSampleProjection leaves a histogram-shaped plan
// root alone: re-projecting the quartet on top would drop the nine
// columns the cursor is probing for.

// histogramSampleValuePlaceholder is the `Value` a histogram-valued row
// carries. A native-histogram sample has no float value — reference
// Prometheus's Sample keeps `H` set and leaves `F` unread, and
// api/prom's wire marshalling makes `value` and `histogram` mutually
// exclusive — but the canonical four-column sample contract (and the
// chclient scan built on it) always binds a Value destination, so the
// projection supplies an unread constant rather than leaving a hole.
const histogramSampleValuePlaceholder = 0.0

// bareExpHistogramSelector reports whether expr is a bare selector over
// an exp-histogram metric, i.e. the one shape this file answers.
//
// It is deliberately asked only of the ROOT of a query (see [lowerRoot]).
// A selector nested under anything else — a reducer, a range-vector
// function, arithmetic, `label_replace` — reaches lowerVectorSelector
// instead and is rejected there, because its consumer reads a `Value`
// column a histogram row does not publish.
//
// Metadata enumeration (/series, /labels) is excluded: it consumes only
// MetricName + Attributes and has its own full-range lowering, which
// expHistogramSelectorRouting already exempts from rejection.
func bareExpHistogramSelector(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (*parser.VectorSelector, bool) {
	if s.ExpHistogramTable == "" || ctx.metadataFullRange {
		return nil, false
	}
	vs, ok := unwrapVectorSelector(expr)
	if !ok {
		return nil, false
	}
	if !s.IsExpHistogramMetric(metricNameFromMatchers(vs.LabelMatchers)) {
		return nil, false
	}
	return vs, true
}

// lowerExpHistogramBare lowers a bare exp-histogram selector across the
// three evaluation shapes [rangeGridShapeFor] distinguishes, the same
// three the native-quantile bare path handles:
//
//   - gridFanout — a query_range with no `@` pin: one row per (series,
//     anchor), each anchor resolving its own staleness lookback.
//   - gridBroadcast — a query_range whose selector carries an absolute
//     `@`: the pinned window is resolved ONCE and fanned across the step
//     grid, so every step reports the same histogram at its own
//     timestamp.
//   - gridSingleAnchor — an instant query: one row per series at
//     `now64(9)`.
func lowerExpHistogramBare(vs *parser.VectorSelector, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if shape := rangeGridShapeFor(vs, ctx); shape == gridFanout {
		return lowerExpHistogramBareRange(vs, s, ctx), nil
	} else if shape == gridBroadcast {
		latest, err := expHistogramBareLatest(vs, s, ctx)
		if err != nil {
			return nil, err
		}
		// Same broadcast shape as broadcastHistogramAtPin, built one
		// rung LOWER: the cross join goes UNDER the histogram
		// projection rather than over it, so the plan root stays a
		// HistogramProjection and keeps publishing the thirteen-column
		// contract. Capping with a Project instead would re-project the
		// quartet alone and delete the nine histogram columns.
		grid := &chplan.StepGrid{Start: ctx.start.UTC(), End: ctx.end.UTC(), Step: ctx.step}
		return nativeHistogramProjection(
			&chplan.CrossJoin{Left: grid, Right: latest},
			bareExpHistogramNameExpr(s),
			&chplan.ColumnRef{Name: stepGridAnchorColumn},
			s,
		), nil
	}
	latest, err := expHistogramBareLatest(vs, s, ctx)
	if err != nil {
		return nil, err
	}
	return nativeHistogramProjection(latest, bareExpHistogramNameExpr(s), chplan.NowNano(), s), nil
}

// bareExpHistogramNameExpr is the `__name__` a BARE selector reports: the
// series' own metric name, carried up from [nativeExpHistBareAggs]'s
// argMax. Only DERIVED samples drop the name — which is why the `sum()`
// sibling in histogram_native_sum.go passes an empty literal here
// instead.
func bareExpHistogramNameExpr(s schema.Metrics) chplan.Expr {
	return &chplan.ColumnRef{Name: s.MetricNameColumn}
}

// expHistogramBareLatest builds the instant-mode subtree beneath the
// histogram projection: the filtered exp-histogram scan collapsed to the
// newest in-window sample per series. Identical to
// lowerHistogramQuantileNative's own input rung apart from the wider
// aggregate list — see [nativeExpHistBareAggs].
func expHistogramBareLatest(vs *parser.VectorSelector, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	scan := &chplan.Scan{Table: s.ExpHistogramTable}
	pred := buildPredicate(vs.LabelMatchers, s)
	pred, err := andInstantWindow(pred, vs, s.TimestampColumn, ctx)
	if err != nil {
		return nil, err
	}
	var input chplan.Node = scan
	if pred != nil {
		input = &chplan.Filter{Input: scan, Predicate: pred}
	}
	return latestSampleAgg(input, nativeExpHistBareAggs(s), s), nil
}

// lowerExpHistogramBareRange is the query_range shape: the same
// per-anchor [chplan.RangeBucketFanout] lowerHistogramQuantileNativeBareRange
// builds — one staleness window per step anchor, keyed on series
// identity — capped with the histogram projection instead of the
// quantile node. The timestamp comes from the fan-out's anchor column,
// so each step reports at its own grid time.
func lowerExpHistogramBareRange(vs *parser.VectorSelector, s schema.Metrics, ctx lowerCtx) chplan.Node {
	scan := &chplan.Scan{Table: s.ExpHistogramTable}
	pred := buildPredicate(vs.LabelMatchers, s)
	fanout := buildHistogramBucketFanout(
		scan, pred, nil, windowFor(vs, instantLookback),
		[]chplan.Expr{histogramIdentityExpr(s)}, []string{s.AttributesColumn},
		nativeExpHistBareAggs(s), s, ctx,
	)
	return nativeHistogramProjection(fanout, bareExpHistogramNameExpr(s), &chplan.ColumnRef{Name: stepGridAnchorColumn}, s)
}

// nativeExpHistBareAggs is [nativeExpHistLatestAggs] widened by the three
// fields a histogram-VALUED answer needs that a quantile does not:
//
//   - MetricName, because PromQL keeps `__name__` on a bare selector
//     (only DERIVED samples drop it). Reading it as `argMax(MetricName,
//     TimeUnix)` rather than adding it to the group keys is deliberate:
//     the metric-name predicate fans out across the OTel dot/slash/
//     underscore name variants, and grouping on the column would split
//     one logical series into one group per stored spelling.
//   - Count and Sum, which the quantile kernel never reads because it
//     works off the bucket ladder, but which are part of the native
//     histogram's wire shape.
func nativeExpHistBareAggs(s schema.Metrics) []chplan.AggFunc {
	return append(
		[]chplan.AggFunc{latestArgMax(s.MetricNameColumn, s)},
		nativeExpHistValuedLatestAggs(s)...,
	)
}

// nativeExpHistValuedLatestAggs is [nativeExpHistLatestAggs] widened by
// Count and Sum — the two whole-histogram scalars the quantile kernel
// never reads (it works off the bucket ladder) but which are part of the
// native histogram's wire shape, so any histogram-VALUED answer owes
// them.
//
// It stops short of MetricName because that field's correct value is not
// a property of the row: a bare selector keeps `__name__` and an
// aggregation drops it, so each caller supplies its own — see
// [bareExpHistogramNameExpr].
func nativeExpHistValuedLatestAggs(s schema.Metrics) []chplan.AggFunc {
	return append(
		[]chplan.AggFunc{
			latestArgMax(s.CountColumn, s),
			latestArgMax(s.SumColumn, s),
		},
		nativeExpHistLatestAggs(s)...,
	)
}

// nativeHistogramProjection caps input with the
// [chplan.HistogramProjection] that publishes the thirteen-column
// histogram row: the canonical sample quartet first — with nameExpr
// supplying `__name__` (the series' own name for a bare selector, an
// empty literal for a derived sample) and tsExpr the timestamp, which is
// `now64(9)` for an instant query and the grid anchor column for a range
// one — then the nine raw structural columns under their fixed
// chplan.Histogram*Column aliases.
//
// Shared with the `sum()` lowering in histogram_native_sum.go, which
// differs from the bare path in exactly those two expressions and in the
// subtree it caps.
func nativeHistogramProjection(input chplan.Node, nameExpr, tsExpr chplan.Expr, s schema.Metrics) *chplan.HistogramProjection {
	return &chplan.HistogramProjection{
		Input:                      input,
		CountColumn:                s.CountColumn,
		SumColumn:                  s.SumColumn,
		ScaleColumn:                s.ScaleColumn,
		ZeroCountColumn:            s.ZeroCountColumn,
		ZeroThresholdColumn:        s.ZeroThresholdColumn,
		PositiveOffsetColumn:       s.PositiveOffsetColumn,
		PositiveBucketCountsColumn: s.PositiveBucketCountsColumn,
		NegativeOffsetColumn:       s.NegativeOffsetColumn,
		NegativeBucketCountsColumn: s.NegativeBucketCountsColumn,
		GroupBy: []chplan.Expr{
			nameExpr,
			&chplan.ColumnRef{Name: s.AttributesColumn},
			tsExpr,
			&chplan.LitFloat{V: histogramSampleValuePlaceholder},
		},
		GroupByAliases: []string{
			s.MetricNameColumn,
			s.AttributesColumn,
			s.TimestampColumn,
			s.ValueColumn,
		},
		MetricNameColumn: s.MetricNameColumn,
		AttributesColumn: s.AttributesColumn,
		TimestampColumn:  s.TimestampColumn,
	}
}
