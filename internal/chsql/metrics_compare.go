package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

// This file emits chplan.MetricsCompare — TraceQL's
// `| compare({...}, topN[, start, end])` first-stage metrics operator.
//
// The SQL produces the RAW per-(cohort, attribute, value[, anchor])
// counts; the Tempo HTTP handler
// (internal/api/tempo/metrics_query_range_compare.go) mirrors upstream
// Tempo's BaselineAggregator on top of the row stream — top-N per
// (cohort, attribute), per-attribute totals, zero-filled anchor grids,
// and the `__meta_type` label scheme. Top-N is deliberately NOT pushed
// into the SQL: the totals series count every occurrence (not just the
// top-N survivors), so the full value inventory must reach the handler
// anyway.
//
// Bare shape (no RangeWindow wrapper — TXTAR / chdb layer):
//
//	SELECT `is_selection`, tupleElement(kv, ?) AS `attr`,
//	       tupleElement(kv, ?) AS `val`, toFloat64(count(?)) AS `Value`
//	FROM (
//	  SELECT <Selection> AS `is_selection`, arrayJoin(<Pairs>) AS `kv`
//	  FROM (<Inner>) [AS s LEFT JOIN (<RootLookup>) AS r ON s.<TraceID> = r.<TraceID>]
//	)
//	GROUP BY `is_selection`, `attr`, `val`
//	ORDER BY `is_selection`, `attr`, `val`
//
// The ORDER BY pins a deterministic row order for the bare shape only
// (GROUP BY output order is otherwise unspecified); the matrix shape
// leaves ordering to the handler, matching the other metrics emitters.
//
// Matrix shape (RangeWindow wrapper — /api/metrics/query_range):
//
//	SELECT `is_selection`, `attr`, `val`, anchor_ts, toFloat64(count(?)) AS `Value`
//	FROM (
//	  SELECT `is_selection`, tupleElement(kv, ?) AS `attr`,
//	         tupleElement(kv, ?) AS `val`, <sample-side anchor fanout> AS anchor_ts
//	  FROM (
//	    SELECT <Selection> AS `is_selection`, arrayJoin(<Pairs>) AS `kv`, <tsCol>
//	    FROM (<Inner>) [LEFT JOIN <RootLookup>] [WHERE <(Start-range, End] bounds>]
//	  )
//	)
//	GROUP BY `is_selection`, `attr`, `val`, anchor_ts
//
// Anchors with no rows emit nothing — the handler zero-fills the grid
// (upstream's counts arrays are zero-initialised across all intervals).

// compareJoinLeftAlias / compareJoinRightAlias are the bare table
// aliases the LEFT JOIN uses. Following vector_join's `L`/`R`
// convention; `s` (spans) / `r` (roots) read better in EXPLAIN output.
const (
	compareJoinLeftAlias  = "s"
	compareJoinRightAlias = "r"
)

// compareBaseQuery builds the innermost SELECT — cohort flag +
// arrayJoin'd attribute pairs (+ extraCols, used by the matrix path to
// carry the timestamp column up to the fanout level) over Inner,
// LEFT JOINed against RootLookup when present.
func (e *emitter) compareBaseQuery(m *chplan.MetricsCompare, extraCols ...string) (*QueryBuilder, error) {
	if m.Selection == nil {
		return nil, fmt.Errorf("%w: MetricsCompare.Selection is nil", ErrUnsupported)
	}
	if m.Pairs == nil {
		return nil, fmt.Errorf("%w: MetricsCompare.Pairs is nil", ErrUnsupported)
	}
	if m.Inner == nil {
		return nil, fmt.Errorf("%w: MetricsCompare.Inner is nil", ErrUnsupported)
	}
	// Pre-flight chplan expressions so errors surface synchronously
	// (mirrors emitMetricsAggregate's pre-flight loop).
	if err := (&Builder{}).Expr(m.Selection); err != nil {
		return nil, err
	}
	if err := (&Builder{}).Expr(m.Pairs); err != nil {
		return nil, err
	}

	inner, err := e.subqueryFrag(m.Inner)
	if err != nil {
		return nil, err
	}

	qb := NewQuery()
	if m.RootLookup != nil {
		if m.TraceIDColumn == "" {
			return nil, fmt.Errorf("%w: MetricsCompare.RootLookup set but TraceIDColumn empty", ErrUnsupported)
		}
		root, rerr := e.subqueryFrag(m.RootLookup)
		if rerr != nil {
			return nil, rerr
		}
		qb.From(aliasedFrag(inner, compareJoinLeftAlias)).
			Join(
				LeftJoin,
				aliasedFrag(root, compareJoinRightAlias),
				Eq(
					qualColFrag(compareJoinLeftAlias, m.TraceIDColumn),
					qualColFrag(compareJoinRightAlias, m.TraceIDColumn),
				),
			)
	} else {
		qb.From(inner)
	}

	sel := m.Selection
	qb.SelectAs(func(b *Builder) { _ = b.Expr(sel) }, compareSelOut(m))
	pairs := m.Pairs
	qb.SelectAs(Call("arrayJoin", func(b *Builder) { _ = b.Expr(pairs) }), "kv")
	for _, c := range extraCols {
		col := c
		qb.Select(func(b *Builder) { b.Ident(col) })
	}
	return qb, nil
}

// compareSelOut / compareAttrOut / compareValOut / compareValueOut
// resolve the output aliases with the same defaults the lowering pins
// (internal/traceql/metrics_compare.go).
func compareSelOut(m *chplan.MetricsCompare) string {
	if m.SelAlias != "" {
		return m.SelAlias
	}
	return "is_selection"
}

func compareAttrOut(m *chplan.MetricsCompare) string {
	if m.AttrAlias != "" {
		return m.AttrAlias
	}
	return "attr"
}

func compareValOut(m *chplan.MetricsCompare) string {
	if m.ValAlias != "" {
		return m.ValAlias
	}
	return "val"
}

func compareValueOut(m *chplan.MetricsCompare) string {
	if m.ValueAlias != "" {
		return m.ValueAlias
	}
	return "Value"
}

// compareTupleElementFrag renders `tupleElement(kv, <idx>)` — the
// attr (idx 1) / val (idx 2) projection out of the arrayJoin'd pair.
// The index binds as a positional arg (clickhouse-go interpolates it
// client-side, so CH sees the constant literal tupleElement requires).
func compareTupleElementFrag(idx int64) Frag {
	return func(b *Builder) {
		b.sb.WriteString("tupleElement(kv, ")
		b.Arg(idx)
		b.sb.WriteByte(')')
	}
}

// compareCountValueFrag renders `toFloat64(count(?))` with the bound
// literal 1 — same reducer shape as the other metrics emitters, pinned
// to Float64 so chclient.Sample.Value scans cleanly.
func compareCountValueFrag() Frag {
	return func(b *Builder) {
		b.sb.WriteString("toFloat64(")
		_ = b.Expr(&chplan.FuncCall{Name: "count", Args: []chplan.Expr{&chplan.LitInt{V: 1}}})
		b.sb.WriteByte(')')
	}
}

// emitMetricsCompare renders the bare (time-collapsed) shape.
func (e *emitter) emitMetricsCompare(m *chplan.MetricsCompare) error {
	base, err := e.compareBaseQuery(m)
	if err != nil {
		return err
	}

	selA, attrA, valA := compareSelOut(m), compareAttrOut(m), compareValOut(m)
	outer := NewQuery().From(base.Frag())
	outer.Select(Col(selA))
	outer.SelectAs(compareTupleElementFrag(1), attrA)
	outer.SelectAs(compareTupleElementFrag(2), valA)
	outer.SelectAs(compareCountValueFrag(), compareValueOut(m))
	outer.GroupBy(Col(selA), Col(attrA), Col(valA))
	outer.OrderBy(Col(selA), false).OrderBy(Col(attrA), false).OrderBy(Col(valA), false)

	e.emitSelect(outer)
	return nil
}

// emitRangeWindowCompare renders the matrix shape — one row per
// (cohort, attr, val, anchor) with the per-anchor count. Mirrors
// emitRangeWindowHistogram's anchor machinery (sample-side fanout,
// scan-bound pushdown); see the package comment at the top of this
// file for the SQL skeleton.
func (e *emitter) emitRangeWindowCompare(r *chplan.RangeWindow, m *chplan.MetricsCompare) error {
	if r.TimestampColumn == "" {
		return fmt.Errorf("%w: RangeWindow.TimestampColumn unset (required for MetricsCompare input)", ErrUnsupported)
	}
	if r.Step <= 0 {
		return fmt.Errorf("%w: RangeWindow wrapping MetricsCompare requires Step > 0", ErrUnsupported)
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

	tsCol := r.TimestampColumn
	base, err := e.compareBaseQuery(m, tsCol)
	if err != nil {
		return err
	}
	maybePushInnerScanTimeBounds(base, r, tsCol, rangeNS)

	selA, attrA, valA := compareSelOut(m), compareAttrOut(m), compareValOut(m)

	fanout := NewQuery().From(base.Frag())
	fanout.Select(Col(selA))
	fanout.SelectAs(compareTupleElementFrag(1), attrA)
	fanout.SelectAs(compareTupleElementFrag(2), valA)
	fanout.SelectAs(
		sampleAnchorFanoutFrag(end, func(b *Builder) { b.Ident(tsCol) }, stepNS, rangeNS, numAnchors),
		RangeWindowAnchorAlias,
	)

	outer := NewQuery().From(fanout.Frag())
	outer.Select(Col(selA), Col(attrA), Col(valA), Col(RangeWindowAnchorAlias))
	outer.SelectAs(compareCountValueFrag(), compareValueOut(m))
	outer.GroupBy(Col(selA), Col(attrA), Col(valA), Col(RangeWindowAnchorAlias))

	e.emitSelect(outer)
	return nil
}
