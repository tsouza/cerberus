package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

// emitMetricsHistogramOverTime renders a chplan.MetricsHistogramOverTime
// as a bare histogram aggregation (no wrapping RangeWindow). The shape
// mirrors emitMetricsAggregate but adds a synthetic bucket column to
// the SELECT-list and the GROUP BY:
//
//	SELECT [<group cols>,]
//	       pow(2, ceil(log2(toFloat64(<attr>)))) [/ 1e9] AS `__bucket`,
//	       count(1) AS `Value`
//	FROM (<Inner>)
//	WHERE <attr> >= 2
//	GROUP BY [<group cols>,] `__bucket`
//
// Bucketing follows Tempo's `bucketizeFnFor` (pkg/traceql/ast_metrics.go):
// each row contributes 1 to a bucket keyed by `Log2Bucketize(<attr>)` —
// the value rounded UP to the next power of two (engine_metrics.go's
// `Log2Bucketize`, equivalent to `2^ceil(log2(v))` for v >= 2). When
// IsDuration is true the attribute is in nanoseconds and the bucket
// key is divided by 1e9 so the label reads in seconds — matching
// `bucketizeDuration`'s `Log2Bucketize(d) / float64(time.Second)`.
//
// Spans with <attr> < 2 are dropped (bucketizeDuration /
// bucketizeAttribute return NewStaticNil() for that range); the WHERE
// filter mirrors that contract.
//
// When a RangeWindow wraps a MetricsHistogramOverTime the matrix path
// in emitRangeWindowHistogram fans each row across N anchors and the
// outer GROUP BY adds `anchor_ts` alongside the bucket column.
func (e *emitter) emitMetricsHistogramOverTime(m *chplan.MetricsHistogramOverTime) error {
	if m.Attr == nil {
		return fmt.Errorf("%w: MetricsHistogramOverTime.Attr is nil", ErrUnsupported)
	}
	if m.Inner == nil {
		return fmt.Errorf("%w: MetricsHistogramOverTime.Inner is nil", ErrUnsupported)
	}
	// Pre-flight every chplan expression so errors surface synchronously
	// (mirrors emitMetricsAggregate's pre-flight loop).
	if err := (&Builder{}).Expr(m.Attr); err != nil {
		return err
	}
	for _, g := range m.GroupBy {
		if err := (&Builder{}).Expr(g); err != nil {
			return err
		}
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

	bucketAlias := m.BucketAlias
	if bucketAlias == "" {
		bucketAlias = "__bucket"
	}
	sb.SelectAs(histogramBucketFrag(m.Attr, m.IsDuration), bucketAlias)

	// count(1) AS <ValueAlias>.
	valueAlias := m.ValueAlias
	if valueAlias == "" {
		valueAlias = "Value"
	}
	countFunc := chplan.AggFunc{
		Name:  "count",
		Args:  []chplan.Expr{&chplan.LitInt{V: 1}},
		Alias: valueAlias,
	}
	sb.Select(aggFuncFrag(countFunc))

	// WHERE <attr> >= 2 — Tempo's runtime drops rows with attr < 2.
	attrExpr := m.Attr
	sb.Where(func(b *Builder) {
		_ = b.Expr(attrExpr)
		b.sb.WriteString(" >= 2")
	})

	// GROUP BY <group cols>, bucket.
	groupFrags := make([]Frag, 0, len(m.GroupBy)+1)
	for _, g := range m.GroupBy {
		expr := g
		groupFrags = append(groupFrags, func(b *Builder) { _ = b.Expr(expr) })
	}
	groupFrags = append(groupFrags, Col(bucketAlias))
	sb.GroupBy(groupFrags...)

	e.emitSelect(sb)
	return nil
}

// histogramBucketFrag renders the
// `pow(2, ceil(log2(toFloat64(<attr>)))) [/ 1e9]` bucket-key
// expression — the value rounded UP to the next power of two, exactly
// Tempo's `Log2Bucketize` (pkg/traceql/engine_metrics.go: for v >= 2,
// `1 << (64 - bits.LeadingZeros64(v-1))` == `2^ceil(log2(v))`; exact
// powers of two map to themselves under both forms because IEEE log2
// of an exact power of two is exact). The `/ 1e9` divisor applies only
// when isDuration is true (the attribute carries nanoseconds and the
// bucket label must read in seconds, matching bucketizeDuration's
// `Log2Bucketize(d) / float64(time.Second)`).
//
// The pre-fix shape emitted the bare exponent (`log2(<attr>)`, and for
// durations `log2(<attr>) / 1e9` — the exponent itself divided by 1e9)
// instead of the power-of-two value, so a 1.024µs span surfaced bucket
// ~1e-08 where reference Tempo reports 1.024e-06 seconds. Mirrors
// quantileBucketFrag (internal/chsql/range_window.go), which always
// carried the correct pow/ceil form; the operand differs (raw column
// expression here vs the seconds-rebased `metric_arg` alias there) so
// no `* 1e9` recovery multiply appears in this frag.
//
// The divisor is rendered as the literal `1000000000` rather than a
// bound argument: it's part of the query shape, not user data, and the
// per-attribute bucket arithmetic is otherwise parameter-free.
func histogramBucketFrag(attr chplan.Expr, isDuration bool) Frag {
	attrFrag := func(b *Builder) { _ = b.Expr(attr) }
	return func(b *Builder) {
		b.sb.WriteString("pow(2, ceil(log2(toFloat64(")
		attrFrag(b)
		b.sb.WriteString("))))")
		if isDuration {
			// "/ 1e9" — inline divisor (query-shape constant, not user
			// data); appended via direct write because the surrounding
			// emitter expects a writer callback, not a Frag.
			b.sb.WriteString(" / 1000000000")
		}
	}
}

// emitRangeWindowHistogram renders a RangeWindow wrapping a
// MetricsHistogramOverTime — the TraceQL `/api/metrics/query_range`
// matrix shape for histogram series.
//
// Each per-span row is fanned across only the anchors whose window
// contains its timestamp (sample-side fanout, ≤ range/step + 1 anchors
// per row — see sampleAnchorFanoutFrag); a UNION ALL generator arm
// pins one zero row per (observed series, grid anchor); the outer
// SELECT groups by (<user group-by>, bucket, anchor_ts) and reduces
// `sum(in_window)` per bucket. SQL skeleton (N = (End-Start)/Step + 1
// or OuterRange/Step + 1):
//
//	SELECT [<group cols>,] `__bucket`, anchor_ts, toFloat64(sum(in_window)) AS `Value`
//	FROM (
//	  (SELECT [<group cols>,]
//	          pow(2, ceil(log2(toFloat64(<Attr>)))) [/ 1e9] AS `__bucket`,
//	          arrayJoin(arrayMap(i -> <anchor_base> - toIntervalNanosecond(i * <step_ns>),
//	                    range(<covered-anchor index bounds>))) AS anchor_ts,
//	          1 AS in_window
//	   FROM (<Inner>)
//	   WHERE <Attr> >= 2)
//	  UNION ALL
//	  (SELECT [<group cols>,] `__bucket`,
//	          arrayJoin(arrayMap(i -> ..., range(0, <N>))) AS anchor_ts,
//	          0 AS in_window
//	   FROM (SELECT [<group cols>,] <bucket expr> AS `__bucket`
//	         FROM (<Inner>) WHERE <Attr> >= 2
//	         GROUP BY [<group cols>,] `__bucket`))
//	)
//	GROUP BY [<group cols>,] `__bucket`, anchor_ts
//
// The bucket column is computed in the inner SELECTs (alongside the
// arrayJoined anchors) so the outer GROUP BY can reference it
// by alias without recomputing the bucketize expression.
//
// The per-anchor window is left-open / right-closed
// (`(anchor_ts - range, anchor_ts]`) — encoded by the sample-side
// index bounds (strict floor+1 lower / inclusive floor upper; see
// sampleAnchorFanoutFrag) — same bucket semantics as the
// non-histogram metrics emitter (emitRangeWindowMetrics) and Tempo
// upstream's `IntervalMapperQueryRange`.
//
// Zero-fill: upstream histogram_over_time runs on
// NewCountOverTimeAggregator (ast_metrics.go init switch), whose
// per-interval counts start at zero, and SeriesSet.ToProto skips only
// NaN values — so every (group, __bucket) series Tempo observes
// anywhere in the window reaches the wire DENSE, carrying a 0 sample
// at every grid anchor with no in-window spans. Same wire posture as
// count_over_time / rate (see metricsOpZeroFillsEmptyBuckets). The
// generator arm discovers the distinct (group, __bucket) series from
// the same bounded scan the sample arm reads and fans each across the
// full anchor grid with `0 AS in_window`, so the outer
// `sum(in_window)` pins empty anchors at 0 instead of dropping the
// row. A fully-empty scan yields no discovered series and therefore
// no output rows — matching Tempo, which emits no series at all in
// that case.
func (e *emitter) emitRangeWindowHistogram(r *chplan.RangeWindow, m *chplan.MetricsHistogramOverTime) error {
	if r.TimestampColumn == "" {
		return fmt.Errorf("%w: RangeWindow.TimestampColumn unset (required for MetricsHistogramOverTime input)", ErrUnsupported)
	}
	if r.Step <= 0 {
		return fmt.Errorf("%w: RangeWindow wrapping MetricsHistogramOverTime requires Step > 0", ErrUnsupported)
	}
	if m.Attr == nil {
		return fmt.Errorf("%w: MetricsHistogramOverTime.Attr is nil", ErrUnsupported)
	}
	if m.Inner == nil {
		return fmt.Errorf("%w: MetricsHistogramOverTime.Inner is nil", ErrUnsupported)
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

	bucketAlias := m.BucketAlias
	if bucketAlias == "" {
		bucketAlias = "__bucket"
	}
	valueAlias := m.ValueAlias
	if valueAlias == "" {
		valueAlias = "Value"
	}

	// Sample arm: group-by cols, bucket, attr filter, sample-side
	// anchor fanout, `1 AS in_window` marker.
	groupAliases := outerGroupAliases(m.GroupBy, m.GroupByAliases)
	innerSb := NewQuery().From(inner)
	for i, g := range m.GroupBy {
		expr := g
		alias := groupAliases[i]
		innerSb.SelectAs(func(b *Builder) { _ = b.Expr(expr) }, alias)
	}
	tsCol := r.TimestampColumn
	innerSb.SelectAs(histogramBucketFrag(m.Attr, m.IsDuration), bucketAlias)
	innerSb.SelectAs(
		sampleAnchorFanoutFrag(end, func(b *Builder) { b.Ident(tsCol) }, stepNS, rangeNS, numAnchors),
		"anchor_ts",
	)
	innerSb.SelectAs(InlineLit(int64(1)), "in_window")
	// Push the <attr> >= 2 filter into the inner SELECT so the anchor
	// fanout doesn't multiply work for rows that drop anyway.
	attrExpr := m.Attr
	attrGuard := func(b *Builder) {
		_ = b.Expr(attrExpr)
		b.sb.WriteString(" >= 2")
	}
	innerSb.Where(attrGuard)
	// Same Start/End scan-bound pushdown as the non-histogram metrics
	// emitter — see maybePushInnerScanTimeBounds.
	maybePushInnerScanTimeBounds(innerSb, r, tsCol, rangeNS)

	// Generator arm: one `0 AS in_window` row per (observed (group,
	// __bucket) series, grid anchor). Series discovery replays the
	// same bounded scan + bucketize the sample arm reads, GROUPed by
	// (group aliases…, __bucket) so each observed series fans across
	// the full grid exactly once.
	disc := NewQuery().From(inner)
	for i, g := range m.GroupBy {
		expr := g
		alias := groupAliases[i]
		disc.SelectAs(func(b *Builder) { _ = b.Expr(expr) }, alias)
	}
	disc.SelectAs(histogramBucketFrag(m.Attr, m.IsDuration), bucketAlias)
	disc.Where(attrGuard)
	maybePushInnerScanTimeBounds(disc, r, tsCol, rangeNS)
	discKeys := make([]Frag, 0, len(groupAliases)+1)
	for _, alias := range groupAliases {
		a := alias
		discKeys = append(discKeys, func(b *Builder) { b.Ident(a) })
	}
	discKeys = append(discKeys, Col(bucketAlias))
	disc.GroupBy(discKeys...)

	grid := NewQuery().From(disc.Frag())
	for _, alias := range groupAliases {
		a := alias
		grid.Select(func(b *Builder) { b.Ident(a) })
	}
	grid.Select(Col(bucketAlias))
	grid.SelectAs(anchorFanoutFrag(end, stepNS, numAnchors), "anchor_ts")
	grid.SelectAs(InlineLit(int64(0)), "in_window")

	// Outer SELECT: group-by + bucket + anchor_ts; sum(in_window) per
	// (series, anchor) — sample-arm rows contribute 1 each, generator
	// rows pin empty anchors at 0.
	outerSb := NewQuery().From(Paren(UnionAll(innerSb.Frag(), grid.Frag())))
	for _, alias := range groupAliases {
		a := alias
		outerSb.Select(func(b *Builder) { b.Ident(a) })
	}
	outerSb.Select(Col(bucketAlias))
	outerSb.Select(Col("anchor_ts"))
	outerSb.SelectAs(func(b *Builder) {
		b.sb.WriteString("toFloat64(sum(in_window))")
	}, valueAlias)

	// GROUP BY group aliases + bucket + anchor_ts.
	groupFrags := make([]Frag, 0, len(groupAliases)+2)
	for _, alias := range groupAliases {
		a := alias
		groupFrags = append(groupFrags, func(b *Builder) { b.Ident(a) })
	}
	groupFrags = append(groupFrags, Col(bucketAlias), Col("anchor_ts"))
	outerSb.GroupBy(groupFrags...)

	e.emitSelect(outerSb)
	return nil
}
