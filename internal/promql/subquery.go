package promql

import (
	"fmt"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// defaultSubqueryStep is the step cerberus substitutes when a subquery
// omits an explicit `step` (`expr[5m:]`). Prom defines empty-step
// semantics as "use the engine's eval step"; cerberus doesn't thread
// that through lowering yet (M2.1 territory) so we hardcode 1m, which
// matches Prom's default eval step.
const defaultSubqueryStep = time.Minute

// lowerSubquery handles `<expr>[<range>:<step>]`. P0 4.5 scope: the
// inner is a `*parser.VectorSelector` (`up[5m:1m]`). Inner ranges over
// other call shapes (`rate(m[5m])[1h:5m]`) land in P0 4.6; outer
// range-vector functions over a subquery (`max_over_time(...)[1h:5m]`)
// land in P0 4.7.
//
// The lowered shape is a matrix-mode RangeWindow with Identity=true
// (the "last value in window" emission). Each anchor across
// `[End-OuterRange, End]` evaluates the inner selector by picking the
// last sample whose timestamp falls within `[anchor-Step, anchor]`.
func lowerSubquery(e *parser.SubqueryExpr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if e.Range <= 0 {
		return nil, fmt.Errorf("promql: subquery range must be positive, got %s", e.Range)
	}
	step := e.Step
	if step == 0 {
		step = defaultSubqueryStep
	}
	if step < 0 {
		return nil, fmt.Errorf("promql: subquery step must be positive, got %s", e.Step)
	}

	switch inner := e.Expr.(type) {
	case *parser.VectorSelector:
		return lowerSubqueryOverVectorSelector(e, inner, step, s, ctx)
	case *parser.Call:
		return lowerSubqueryOverCall(e, inner, step, s, ctx)
	case *parser.ParenExpr:
		// `(<expr>)[5m:1m]` — unwrap and retry with the same subquery.
		// Build a synthetic SubqueryExpr around the inner expr so the
		// modifiers + range/step are preserved.
		inner2 := *e
		inner2.Expr = inner.Expr
		return lowerSubquery(&inner2, s, ctx)
	case *parser.AggregateExpr:
		return nil, fmt.Errorf("promql: subquery over aggregated expression is not yet supported (deferred from P0 4 — `max_over_time(sum by(...) (rate(...))[1h:5m])` needs chained RangeWindow over Aggregate output, landing in RC3)")
	case *parser.SubqueryExpr:
		return nil, fmt.Errorf("promql: nested subqueries are not yet supported (deferred to RC3)")
	}
	return nil, fmt.Errorf("promql: subquery over %T is not yet supported", e.Expr)
}

// lowerSubqueryOverCall — `<range-vector-fn>(<inner>[<inner_range>])[<outer_range>:<step>]`.
// The most common shape is `rate(m[5m])[1h:5m]`. Lowers to a single
// matrix-shape RangeWindow where:
//
//   - Func    = the inner range function (rate / increase / *_over_time)
//   - Range   = the inner matrix selector's range (the 5m in `rate(m[5m])`)
//   - OuterRange / Step come from the subquery
//
// I.e. the same RangeWindow IR that lowerRangeVectorCall produces for
// instant rate, but with OuterRange + Step populated to fan out N anchors.
func lowerSubqueryOverCall(
	sub *parser.SubqueryExpr,
	call *parser.Call,
	step time.Duration,
	s schema.Metrics,
	ctx lowerCtx,
) (chplan.Node, error) {
	if len(call.Args) != 1 {
		return nil, fmt.Errorf("promql: subquery inner %s expects exactly 1 argument, got %d",
			call.Func.Name, len(call.Args))
	}
	ms, ok := call.Args[0].(*parser.MatrixSelector)
	if !ok {
		return nil, fmt.Errorf("promql: subquery inner %s must wrap a MatrixSelector, got %T",
			call.Func.Name, call.Args[0])
	}
	vs, ok := ms.VectorSelector.(*parser.VectorSelector)
	if !ok {
		return nil, fmt.Errorf("promql: subquery inner matrix selector must wrap a VectorSelector, got %T",
			ms.VectorSelector)
	}

	// Strip the inner VS modifier — the subquery's own modifier shadows it.
	// inRangeVector also suppresses the bare-selector LWR wrap so every
	// in-window sample reaches the surrounding RangeWindow.
	vsNoModifier := *vs
	vsNoModifier.Timestamp = nil
	vsNoModifier.OriginalOffset = 0
	vsNoModifier.Offset = 0
	vsNoModifier.StartOrEnd = 0
	rangeCtx := ctx
	rangeCtx.inRangeVector = true
	inner, err := lowerVectorSelector(&vsNoModifier, s, rangeCtx)
	if err != nil {
		return nil, err
	}

	anchor, err := subqueryAnchor(sub, ctx)
	if err != nil {
		return nil, err
	}

	return &chplan.RangeWindow{
		Input:           inner,
		Func:            call.Func.Name,
		Range:           ms.Range,
		OuterRange:      sub.Range,
		Step:            step,
		End:             anchor.End,
		Offset:          anchor.Offset,
		TimestampColumn: s.TimestampColumn,
		ValueColumn:     s.ValueColumn,
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}},
	}, nil
}

// lowerSubqueryOverVectorSelector — `metric[range:step]` lowering.
//
// The subquery's own modifiers (offset, @) shadow any modifiers on the
// inner VectorSelector — Prom evaluates `up[5m:1m] offset 10m` as the
// subquery anchored at `now - 10m`, NOT at the inner VS's modifier
// (which is illegal on a subquery's inner anyway). We strip the
// inner's modifier before lowering and apply the subquery's own.
func lowerSubqueryOverVectorSelector(
	sub *parser.SubqueryExpr,
	vs *parser.VectorSelector,
	step time.Duration,
	s schema.Metrics,
	ctx lowerCtx,
) (chplan.Node, error) {
	vsNoModifier := *vs
	vsNoModifier.Timestamp = nil
	vsNoModifier.OriginalOffset = 0
	vsNoModifier.Offset = 0
	vsNoModifier.StartOrEnd = 0
	rangeCtx := ctx
	rangeCtx.inRangeVector = true
	inner, err := lowerVectorSelector(&vsNoModifier, s, rangeCtx)
	if err != nil {
		return nil, err
	}

	anchor, err := subqueryAnchor(sub, ctx)
	if err != nil {
		return nil, err
	}

	return &chplan.RangeWindow{
		Input:           inner,
		Identity:        true,
		Range:           step, // per-anchor lookback = subquery step
		OuterRange:      sub.Range,
		Step:            step,
		End:             anchor.End,
		Offset:          anchor.Offset,
		TimestampColumn: s.TimestampColumn,
		ValueColumn:     s.ValueColumn,
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}},
	}, nil
}

// lowerOuterRangeFnOverSubquery — `max_over_time(rate(m[5m])[1h:5m])`,
// the canonical Grafana subquery shape. The outer call is a
// range-vector function reducing over the inner matrix output.
//
// IR is a chained RangeWindow:
//
//	RangeWindow{
//	  Func:       <outer fn name>,      // "max_over_time", "sum_over_time", …
//	  Range:      <subquery range>,     // the full inner matrix lookback
//	  Step:       0,                    // instant — single value per series
//	  Input:      RangeWindow{          // matrix from lowerSubquery
//	    Func:       <inner fn name>,
//	    Range:      <inner matrix range>,
//	    OuterRange: <subquery range>,
//	    Step:       <subquery step>,
//	    ...,
//	  },
//	  TimestampColumn: "anchor_ts",     // inner matrix's per-row anchor
//	  ValueColumn:     "value",         // inner matrix's emitted value
//	}
//
// The outer's TimestampColumn / ValueColumn point at the inner matrix
// output columns rather than the underlying table's TimeUnix/Value.
func lowerOuterRangeFnOverSubquery(
	outer *parser.Call,
	sub *parser.SubqueryExpr,
	s schema.Metrics,
	ctx lowerCtx,
) (chplan.Node, error) {
	if _, ok := rangeVectorFn[outer.Func.Name]; !ok {
		return nil, fmt.Errorf("promql: %s does not accept a subquery argument", outer.Func.Name)
	}

	inner, err := lowerSubquery(sub, s, ctx)
	if err != nil {
		return nil, err
	}

	return &chplan.RangeWindow{
		Input:           inner,
		Func:            outer.Func.Name,
		Range:           sub.Range,
		TimestampColumn: "anchor_ts",
		ValueColumn:     "value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}},
	}, nil
}

// rangeVectorFn is the set of PromQL functions cerberus's emitter
// handles as range-vector reducers. Subquery-argument lowering only
// fires for these.
var rangeVectorFn = map[string]struct{}{
	"rate":            {},
	"increase":        {},
	"delta":           {},
	"sum_over_time":   {},
	"avg_over_time":   {},
	"min_over_time":   {},
	"max_over_time":   {},
	"count_over_time": {},
	"last_over_time":  {},
}

// subqueryAnchor reads the subquery's `@` + `offset` modifiers into an
// evalAnchor. Mirrors anchorFromSelector for SubqueryExpr's identical
// modifier fields.
func subqueryAnchor(e *parser.SubqueryExpr, ctx lowerCtx) (evalAnchor, error) {
	a := evalAnchor{Offset: e.OriginalOffset}
	switch e.StartOrEnd {
	case parser.START:
		if ctx.start.IsZero() {
			return evalAnchor{}, fmt.Errorf("promql: subquery `@ start()` modifier requires query range context (use LowerAt)")
		}
		a.End = ctx.start.UTC()
	case parser.END:
		if ctx.end.IsZero() {
			return evalAnchor{}, fmt.Errorf("promql: subquery `@ end()` modifier requires query range context (use LowerAt)")
		}
		a.End = ctx.end.UTC()
	case 0:
		// no start/end modifier
	default:
		return evalAnchor{}, fmt.Errorf("promql: unexpected subquery StartOrEnd token %v", e.StartOrEnd)
	}
	if e.Timestamp != nil {
		a.End = time.UnixMilli(*e.Timestamp).UTC()
	}
	return a, nil
}
