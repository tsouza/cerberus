package chsql

import (
	"context"
	"fmt"
	"strconv"

	"github.com/tsouza/cerberus/internal/cerbtrace"
	"github.com/tsouza/cerberus/internal/chplan"
)

// EmitMetricsExemplars renders the trace-anchored exemplars SQL the
// Tempo /api/metrics/query_range handler runs alongside the matrix
// metrics SQL. For each (series, bucket) anchor it picks one
// representative span — the latest by `ts` within the bucket window —
// and projects (MetricName, Attributes, TimeUnix, Value) so the result
// set decodes through chclient.Cursor identically to the matrix-shape
// rows. The `Attributes` map carries the by(...) group-by labels plus
// the reserved `trace:id` / `span:id` keys attachExemplars surfaces as
// Exemplar.TraceID / SpanID on the wire.
//
// maxPerSeries caps the number of exemplars emitted per (series,
// bucket) tuple via `LIMIT N BY <group-aliases>, anchor_ts`. A value
// of 0 disables the cap (every span in every bucket window flows
// through, useful only for tests / dev backends).
//
// Returns ErrUnsupported when the call is structurally invalid — nil
// RangeWindow, MetricsAggregate, missing TimestampColumn, missing
// trace/span ID column names, Step ≤ 0, or Start > End on the range.
// The handler treats any non-nil error as "best-effort exemplars
// off" and serves the metric series with an empty Exemplars array;
// see internal/api/tempo/metrics_query_range.go for the exact
// fan-out / merge contract.
func EmitMetricsExemplars(
	ctx context.Context,
	rw *chplan.RangeWindow,
	m *chplan.MetricsAggregate,
	traceIDCol, spanIDCol string,
	maxPerSeries int64,
) (string, []any, error) {
	_, span := tracer.Start(ctx, cerbtrace.SpanEmit)
	defer span.End()

	if rw == nil {
		err := fmt.Errorf("%w: RangeWindow is nil", ErrUnsupported)
		span.RecordError(err)
		return "", nil, err
	}
	if m == nil {
		err := fmt.Errorf("%w: MetricsAggregate is nil", ErrUnsupported)
		span.RecordError(err)
		return "", nil, err
	}
	if rw.TimestampColumn == "" {
		err := fmt.Errorf("%w: RangeWindow.TimestampColumn unset (required for exemplar emission)", ErrUnsupported)
		span.RecordError(err)
		return "", nil, err
	}
	if rw.Step <= 0 {
		err := fmt.Errorf("%w: RangeWindow wrapping exemplars requires Step > 0", ErrUnsupported)
		span.RecordError(err)
		return "", nil, err
	}
	if m.Inner == nil {
		err := fmt.Errorf("%w: MetricsAggregate.Inner is nil", ErrUnsupported)
		span.RecordError(err)
		return "", nil, err
	}
	if traceIDCol == "" {
		err := fmt.Errorf("%w: traceIDCol is empty", ErrUnsupported)
		span.RecordError(err)
		return "", nil, err
	}
	if spanIDCol == "" {
		err := fmt.Errorf("%w: spanIDCol is empty", ErrUnsupported)
		span.RecordError(err)
		return "", nil, err
	}

	e := &emitter{}
	if err := e.emitMetricsExemplars(rw, m, traceIDCol, spanIDCol, maxPerSeries); err != nil {
		span.RecordError(err)
		return "", nil, err
	}
	sql := e.b.String()
	span.SetAttributes(cerbtrace.AttrSQLLength.Int(len(sql)))
	return sql, e.args, nil
}

func (e *emitter) emitMetricsExemplars(
	rw *chplan.RangeWindow,
	m *chplan.MetricsAggregate,
	traceIDCol, spanIDCol string,
	maxPerSeries int64,
) error {
	for _, g := range m.GroupBy {
		if err := (&Builder{}).Expr(g); err != nil {
			return err
		}
	}

	end := endExprFrag(rw)
	stepNS := rw.Step.Nanoseconds()
	rangeDur := rw.Range
	if rangeDur == 0 {
		rangeDur = rw.Step
	}
	rangeNS := rangeDur.Nanoseconds()

	var numAnchors int64
	switch {
	case rw.OuterRange > 0:
		numAnchors = rw.OuterRange.Nanoseconds()/stepNS + 1
	case !rw.Start.IsZero() && !rw.End.IsZero():
		span := rw.End.Sub(rw.Start).Nanoseconds()
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
	// Attributes-map keys: prefer the lowering's display names (the
	// Tempo-canonical scope-prefixed form, e.g. `resource.service.name`)
	// so the exemplar response uses the same wire labels as the matrix
	// shape. Falls back to the SQL alias when the lowering didn't
	// populate display names (PromQL paths leave them empty; no
	// behaviour change for those). The KEYS of the resulting Attributes
	// map are the wire labels; the VALUES still reference the SQL-side
	// alias via Col(alias).
	groupDisplayNames := make([]string, len(groupAliases))
	for i, alias := range groupAliases {
		if i < len(m.GroupByDisplayNames) && m.GroupByDisplayNames[i] != "" {
			groupDisplayNames[i] = m.GroupByDisplayNames[i]
			continue
		}
		groupDisplayNames[i] = alias
	}

	innerSb := NewQuery().From(inner)
	for i, g := range m.GroupBy {
		expr := g
		alias := groupAliases[i]
		innerSb.SelectAs(func(b *Builder) { _ = b.Expr(expr) }, alias)
	}
	tsCol := rw.TimestampColumn
	innerSb.SelectAs(func(b *Builder) { b.Ident(tsCol) }, "ts")
	if m.Op != chplan.MetricsOpRate && m.Op != chplan.MetricsOpCountOverTime && m.Attr != nil {
		attr := m.Attr
		innerSb.SelectAs(func(b *Builder) { _ = b.Expr(attr) }, "metric_arg")
	}
	innerSb.SelectAs(func(b *Builder) { b.Ident(traceIDCol) }, "exemplar_trace_id")
	innerSb.SelectAs(func(b *Builder) { b.Ident(spanIDCol) }, "exemplar_span_id")
	// Sample-side fanout: each row lands only on the anchors whose
	// `(anchor_ts - range, anchor_ts]` window contains its timestamp
	// (≤ range/step + 1 anchors per row — see sampleAnchorFanoutFrag),
	// so the outer SELECT needs no per-(row, anchor) window re-check.
	// Anchors with no spans produce no row — same observed-only contract
	// as the previous full-grid + WHERE shape.
	innerSb.SelectAs(
		sampleAnchorFanoutFrag(end, func(b *Builder) { b.Ident(tsCol) }, stepNS, rangeNS, numAnchors),
		"anchor_ts",
	)
	// Same Start/End pushdown as emitRangeWindowMetrics — see
	// maybePushInnerScanTimeBounds.
	maybePushInnerScanTimeBounds(innerSb, rw, tsCol, rangeNS)

	// The outer SELECT MUST project exactly four columns in the order
	// (MetricName, Attributes, TimeUnix, Value) — chclient.Cursor binds
	// the result-set rows positionally to those four fields of
	// chclient.Sample. Group-by attributes ride inside the Attributes
	// map (keyed by the by(...) alias); they are NOT separate columns.
	outerSb := NewQuery().From(innerSb.Frag())

	outerSb.Select(As(Lit(""), "MetricName"))

	// Attributes carries the group-by labels plus the trace:id / span:id
	// keys attachExemplars reads. Group aliases reference the inner
	// SELECT (grouped + scalar), so toString(<alias>) is a valid SELECT
	// item under the outer GROUP BY (alias, anchor_ts). The KEY side of
	// the map uses the Tempo-canonical display name (e.g.
	// `resource.service.name`) so the exemplar wire shape matches the
	// matrix-shape labels produced by
	// internal/api/tempo/metrics_query_range.go::wrapMetricsForSample;
	// attachExemplars matches each exemplar to its parent series by
	// canonical label-set hash, so the two label-key projections must
	// agree.
	attrMapFrags := make([]Frag, 0, len(groupAliases)*2+6)
	for i, alias := range groupAliases {
		a := alias
		display := groupDisplayNames[i]
		attrMapFrags = append(
			attrMapFrags,
			Lit(display),
			Call("toString", Col(a)),
		)
	}
	// quantile_over_time series are keyed by a `p="<phi>"` label
	// (mirroring Tempo's HistogramAggregator; see
	// internal/api/tempo/metrics_query_range.go::wrapMetricsForSample).
	// Other ungrouped ops are keyed by `__name__="<op>"` (Tempo's
	// UngroupedAggregator wire shape). The exemplar response keys
	// series by the same canonical label-set hash via attachExemplars,
	// so the exemplar Attributes must carry the same per-op entry —
	// otherwise the exemplar's canonical key drifts and no series in
	// the matrix shape matches.
	switch {
	case m.Op == chplan.MetricsOpQuantileOverTime && len(m.Quantiles) == 1:
		attrMapFrags = append(
			attrMapFrags,
			Lit("p"),
			Lit(strconv.FormatFloat(m.Quantiles[0], 'f', -1, 64)),
		)
	case len(groupAliases) == 0 && m.Op != chplan.MetricsOpQuantileOverTime:
		attrMapFrags = append(
			attrMapFrags,
			Lit("__name__"),
			Lit(m.Op.String()),
		)
	}
	attrMapFrags = append(
		attrMapFrags,
		Lit("trace:id"),
		Call("argMax", Col("exemplar_trace_id"), Col("ts")),
		Lit("span:id"),
		Call("argMax", Col("exemplar_span_id"), Col("ts")),
	)
	outerSb.Select(As(
		Call("map", attrMapFrags...),
		"Attributes",
	))
	outerSb.SelectAs(Col("anchor_ts"), "TimeUnix")

	var valueFrag Frag
	if m.Op == chplan.MetricsOpRate || m.Op == chplan.MetricsOpCountOverTime {
		valueFrag = Call("toFloat64", Call("argMax", InlineLit(int64(1)), Col("ts")))
	} else if m.Attr != nil {
		valueFrag = Call("toFloat64", Call("argMax", Col("metric_arg"), Col("ts")))
	} else {
		valueFrag = Call("toFloat64", Call("argMax", InlineLit(int64(1)), Col("ts")))
	}
	outerSb.Select(As(valueFrag, "Value"))

	groupFrags := make([]Frag, 0, len(groupAliases)+1)
	for _, alias := range groupAliases {
		a := alias
		groupFrags = append(groupFrags, func(b *Builder) { b.Ident(a) })
	}
	groupFrags = append(groupFrags, Col("anchor_ts"))
	outerSb.GroupBy(groupFrags...)

	if maxPerSeries > 0 {
		limitByFrags := make([]Frag, 0, len(groupAliases))
		for _, alias := range groupAliases {
			a := alias
			limitByFrags = append(limitByFrags, func(b *Builder) { b.Ident(a) })
		}
		limitByFrags = append(limitByFrags, Col("anchor_ts"))
		outerSb.Limit(maxPerSeries)
		outerSb.LimitBy(limitByFrags...)
	}

	e.emitSelect(outerSb)
	return nil
}
