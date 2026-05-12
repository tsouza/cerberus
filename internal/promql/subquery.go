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
func lowerSubquery(e *parser.SubqueryExpr, s schema.Metrics) (chplan.Node, error) {
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
	if e.StartOrEnd != 0 {
		return nil, fmt.Errorf("promql: subquery `@ start()` / `@ end()` is not yet supported (deferred from P0 4)")
	}

	switch inner := e.Expr.(type) {
	case *parser.VectorSelector:
		return lowerSubqueryOverVectorSelector(e, inner, step, s)
	case *parser.SubqueryExpr:
		return nil, fmt.Errorf("promql: nested subqueries are not yet supported (deferred to RC3)")
	}
	return nil, fmt.Errorf("promql: subquery over %T is not yet supported (lands in P0 4.6 / 4.7)", e.Expr)
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
) (chplan.Node, error) {
	vsNoModifier := *vs
	vsNoModifier.Timestamp = nil
	vsNoModifier.OriginalOffset = 0
	vsNoModifier.Offset = 0
	inner, err := lowerVectorSelector(&vsNoModifier, s)
	if err != nil {
		return nil, err
	}

	anchor, err := subqueryAnchor(sub)
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

// subqueryAnchor reads the subquery's `@` + `offset` modifiers into an
// evalAnchor. Mirrors anchorFromSelector for SubqueryExpr's identical
// modifier fields.
func subqueryAnchor(e *parser.SubqueryExpr) (evalAnchor, error) {
	a := evalAnchor{Offset: e.OriginalOffset}
	if e.StartOrEnd != 0 {
		return evalAnchor{}, fmt.Errorf("promql: subquery `@ start()` / `@ end()` modifiers are not yet supported")
	}
	if e.Timestamp != nil {
		a.End = time.UnixMilli(*e.Timestamp).UTC()
	}
	return a, nil
}
