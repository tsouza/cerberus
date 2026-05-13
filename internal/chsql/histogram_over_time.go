package chsql

import (
	"fmt"
	"strconv"

	"github.com/tsouza/cerberus/internal/chplan"
)

// emitMetricsHistogramOverTime renders a chplan.MetricsHistogramOverTime
// as a bare histogram aggregation (no wrapping RangeWindow). The shape
// mirrors emitMetricsAggregate but adds a synthetic bucket column to
// the SELECT-list and the GROUP BY:
//
//	SELECT [<group cols>,]
//	       log2(<attr>) [/ 1e9] AS `__bucket`,
//	       count(1) AS `Value`
//	FROM (<Inner>)
//	WHERE <attr> >= 2
//	GROUP BY [<group cols>,] `__bucket`
//
// Bucketing follows Tempo's `bucketizeFnFor` (pkg/traceql/ast_metrics.go):
// each row contributes 1 to a bucket keyed by `log2(ceil(<attr>))`.
// When IsDuration is true the attribute is in nanoseconds and the
// bucket key is divided by 1e9 so the label reads in seconds — matching
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

// histogramBucketFrag renders the `log2(<attr>) [/ 1e9]` bucket-key
// expression. The `/ 1e9` divisor applies only when isDuration is true
// (the attribute carries nanoseconds and the bucket label should read
// in seconds, matching Tempo's bucketizeDuration).
//
// The divisor is rendered as the literal `1000000000` rather than a
// bound argument: it's part of the query shape, not user data, and the
// per-attribute bucket arithmetic is otherwise parameter-free.
func histogramBucketFrag(attr chplan.Expr, isDuration bool) Frag {
	return func(b *Builder) {
		b.sb.WriteString("log2(")
		_ = b.Expr(attr)
		b.sb.WriteByte(')')
		if isDuration {
			b.sb.WriteString(" / 1000000000")
		}
	}
}

// emitRangeWindowHistogram renders a RangeWindow wrapping a
// MetricsHistogramOverTime — the TraceQL `/api/metrics/query_range`
// matrix shape for histogram series.
//
// Each per-span row is fanned across N evaluation anchors via
// arrayJoin(range(0, N)); the outer SELECT groups by
// (<user group-by>, bucket, anchor_ts) and applies `count(1)` per
// bucket. SQL skeleton (N = (End-Start)/Step + 1 or OuterRange/Step + 1):
//
//	SELECT [<group cols>,] `__bucket`, anchor_ts, count(1) AS `Value`
//	FROM (
//	  SELECT [<group cols>,] <TimestampColumn> AS ts,
//	         log2(<Attr>) [/ 1e9] AS `__bucket`,
//	         arrayJoin(arrayMap(i -> <anchor_base> - toIntervalNanosecond(i * <step_ns>), range(0, <N>))) AS anchor_ts
//	  FROM (<Inner>)
//	  WHERE <Attr> >= 2
//	)
//	WHERE ts >= anchor_ts - toIntervalNanosecond(<range_ns>)
//	  AND ts <= anchor_ts
//	GROUP BY [<group cols>,] `__bucket`, anchor_ts
//
// The bucket column is computed in the inner SELECT (alongside the
// arrayJoined anchors) so the outer WHERE / GROUP BY can reference it
// by alias without recomputing the log2.
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

	// Inner SELECT: group-by cols, ts, bucket, attr filter, anchor fanout.
	groupAliases := outerGroupAliases(m.GroupBy, m.GroupByAliases)
	innerSb := NewQuery().From(inner)
	for i, g := range m.GroupBy {
		expr := g
		alias := groupAliases[i]
		innerSb.SelectAs(func(b *Builder) { _ = b.Expr(expr) }, alias)
	}
	tsCol := r.TimestampColumn
	innerSb.SelectAs(func(b *Builder) { b.Ident(tsCol) }, "ts")
	innerSb.SelectAs(histogramBucketFrag(m.Attr, m.IsDuration), bucketAlias)
	innerSb.SelectAs(
		anchorFanoutFrag(end, stepNS, numAnchors),
		"anchor_ts",
	)
	// Push the <attr> >= 2 filter into the inner SELECT so the anchor
	// fanout doesn't multiply work for rows that drop anyway.
	attrExpr := m.Attr
	innerSb.Where(func(b *Builder) {
		_ = b.Expr(attrExpr)
		b.sb.WriteString(" >= 2")
	})

	// Outer SELECT: group-by + bucket + anchor_ts; count(1) per bucket.
	outerSb := NewQuery().From(innerSb.Frag())
	for _, alias := range groupAliases {
		a := alias
		outerSb.Select(func(b *Builder) { b.Ident(a) })
	}
	outerSb.Select(Col(bucketAlias))
	outerSb.Select(Col("anchor_ts"))
	countFunc := chplan.AggFunc{
		Name:  "count",
		Args:  []chplan.Expr{&chplan.LitInt{V: 1}},
		Alias: valueAlias,
	}
	outerSb.Select(aggFuncFrag(countFunc))

	// WHERE: ts ∈ [anchor_ts - range, anchor_ts].
	outerSb.Where(
		func(b *Builder) {
			b.sb.WriteString("ts >= anchor_ts - toIntervalNanosecond(")
			b.sb.WriteString(strconv.FormatInt(rangeNS, 10))
			b.sb.WriteByte(')')
		},
		func(b *Builder) {
			b.sb.WriteString("ts <= anchor_ts")
		},
	)

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
