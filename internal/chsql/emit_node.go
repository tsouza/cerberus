package chsql

import (
	"fmt"
	"strings"

	"github.com/tsouza/cerberus/internal/chplan"
)

// subqueryFrag returns a Frag that renders n as a parenthesised
// subquery into the receiving Builder. Used to plug a child plan into
// QueryBuilder.From without flattening to a string: the args bound
// by the recursive emit walk land in the receiving Builder's args
// slice at the position the Frag is written.
//
// Internally it swaps e.b / e.args with a fresh strings.Builder + nil
// args slice, runs the recursive emit, then splices the rendered SQL
// + args into the destination Builder. The error path is captured via
// the closure variable below; emitSubqueryFrag is the wrapper that
// surfaces it.
func (e *emitter) subqueryFrag(n chplan.Node) (Frag, error) {
	// Pre-render the subquery into an isolated emitter so any chplan
	// error surfaces synchronously (before the Frag is ever spliced
	// into the outer QueryBuilder). The rendered string + args are
	// then captured for cheap replay on each Frag invocation.
	saveB, saveArgs := e.b, e.args
	e.b = strings.Builder{}
	e.args = nil
	if err := e.emitSubquery(n); err != nil {
		e.b = saveB
		e.args = saveArgs
		return nil, err
	}
	sql := e.b.String()
	args := e.args
	e.b = saveB
	e.args = saveArgs
	return func(b *Builder) {
		b.sb.WriteString(sql)
		b.args = append(b.args, args...)
	}, nil
}

// emitSelect runs the assembled QueryBuilder and splices its rendered
// SQL + args into the emitter's output. Centralises the splice
// boilerplate so the per-node emitters stay focused on slot assembly.
func (e *emitter) emitSelect(sb *QueryBuilder) {
	sql, args := sb.Build()
	e.b.WriteString(sql)
	e.args = append(e.args, args...)
}

// splice drains b's accumulated SQL + args into the emitter. Retained
// for the grandfathered emitters in vector_join.go / structural_join.go
// that still compose SQL fragments through a free-standing *Builder
// before flushing to the shared emitter state. R6.6 collapses those
// onto QueryBuilder and removes this helper.
func (e *emitter) splice(b *Builder) {
	sql, args := b.Build()
	e.b.WriteString(sql)
	e.args = append(e.args, args...)
}

func (e *emitter) emitScan(s *chplan.Scan) error {
	sb := NewQuery().From(Col(s.Table))
	if len(s.Columns) > 0 {
		cols := make([]Frag, 0, len(s.Columns))
		for _, c := range s.Columns {
			cols = append(cols, Col(c))
		}
		sb.Select(cols...)
	}
	// (Empty Select list renders as `SELECT *` — matches the
	// pre-builder emitter's behaviour for a column-less Scan.)
	e.emitSelect(sb)
	return nil
}

func (e *emitter) emitFilter(f *chplan.Filter) error {
	sub, err := e.subqueryFrag(f.Input)
	if err != nil {
		return err
	}
	// Pre-flight the predicate so a chplan error surfaces here, not
	// inside the Where-render callback (where the error has no path
	// to the caller without re-introducing splice plumbing).
	if err := (&Builder{}).Expr(f.Predicate); err != nil {
		return err
	}
	pred := func(b *Builder) { _ = b.Expr(f.Predicate) }
	e.emitSelect(NewQuery().From(sub).Where(pred))
	return nil
}

func (e *emitter) emitProject(p *chplan.Project) error {
	sub, err := e.subqueryFrag(p.Input)
	if err != nil {
		return err
	}
	sb := NewQuery().From(sub)
	if len(p.Projections) > 0 {
		// Pre-flight every projection expression so a chplan error
		// surfaces synchronously rather than from inside the Frag
		// render.
		for _, pr := range p.Projections {
			if err := (&Builder{}).Expr(pr.Expr); err != nil {
				return err
			}
		}
		for _, pr := range p.Projections {
			expr := pr.Expr
			sb.SelectAs(func(b *Builder) { _ = b.Expr(expr) }, pr.Alias)
		}
	}
	e.emitSelect(sb)
	return nil
}

func (e *emitter) emitAggregate(a *chplan.Aggregate) error {
	// Pre-flight all expressions so chplan errors surface synchronously.
	for _, g := range a.GroupBy {
		if err := (&Builder{}).Expr(g); err != nil {
			return err
		}
	}
	for _, af := range a.AggFuncs {
		for _, p := range af.Params {
			if err := (&Builder{}).Expr(p); err != nil {
				return err
			}
		}
		for _, ar := range af.Args {
			if err := (&Builder{}).Expr(ar); err != nil {
				return err
			}
		}
	}
	if len(a.GroupBy) == 0 && len(a.AggFuncs) == 0 {
		return fmt.Errorf("%w: Aggregate with no GroupBy keys and no AggFuncs", ErrUnsupported)
	}

	sub, err := e.subqueryFrag(a.Input)
	if err != nil {
		return err
	}

	sb := NewQuery().From(sub)
	for i, g := range a.GroupBy {
		expr := g
		alias := ""
		if i < len(a.GroupByAliases) {
			alias = a.GroupByAliases[i]
		}
		sb.SelectAs(func(b *Builder) { _ = b.Expr(expr) }, alias)
	}
	for _, af := range a.AggFuncs {
		af := af
		sb.Select(aggFuncFrag(af))
	}

	// GROUP BY mirrors the SELECT-list group-by expressions (without
	// aliases — CH groups by the underlying expression, not the alias).
	if len(a.GroupBy) > 0 {
		groupFrags := make([]Frag, 0, len(a.GroupBy))
		for _, g := range a.GroupBy {
			expr := g
			groupFrags = append(groupFrags, func(b *Builder) { _ = b.Expr(expr) })
		}
		sb.GroupBy(groupFrags...)
	}
	e.emitSelect(sb)
	return nil
}

// aggFuncFrag returns a Frag rendering `<name>[(<params>)](<args>) [AS <alias>]`
// via Builder.ParamAgg + As. The expression-render errors surface from
// the pre-flight loop in emitAggregate before the Frag ever runs, so the
// rendering path here is infallible.
func aggFuncFrag(af chplan.AggFunc) Frag {
	mkExpr := func(x chplan.Expr) func(b *Builder) {
		return func(b *Builder) { _ = b.Expr(x) }
	}
	params := make([]func(b *Builder), 0, len(af.Params))
	for _, p := range af.Params {
		params = append(params, mkExpr(p))
	}
	args := make([]func(b *Builder), 0, len(af.Args))
	for _, a := range af.Args {
		args = append(args, mkExpr(a))
	}
	body := func(b *Builder) { b.ParamAgg(af.Name, params, args) }
	return As(body, af.Alias)
}

// emitRangeWindow lives in range_window.go — full windowed-array idiom.

func (e *emitter) emitLimit(l *chplan.Limit) error {
	sub, err := e.subqueryFrag(l.Input)
	if err != nil {
		return err
	}
	sb := NewQuery().From(sub)
	if l.Count > 0 {
		sb.Limit(l.Count)
	}
	e.emitSelect(sb)
	return nil
}

// emitOrderBy renders `SELECT * FROM (<input>) ORDER BY <k1> [DESC], …`
// via QueryBuilder.OrderBy. Empty Keys is a programmer error — emit an
// error so the plan tree doesn't silently lose its sort intent.
func (e *emitter) emitOrderBy(o *chplan.OrderBy) error {
	if len(o.Keys) == 0 {
		return fmt.Errorf("%w: OrderBy with no keys", ErrUnsupported)
	}
	// Pre-flight every key expression so chplan errors surface
	// synchronously rather than from inside the Frag render.
	for _, k := range o.Keys {
		if err := (&Builder{}).Expr(k.Expr); err != nil {
			return err
		}
	}
	sub, err := e.subqueryFrag(o.Input)
	if err != nil {
		return err
	}
	sb := NewQuery().From(sub)
	for _, k := range o.Keys {
		expr := k.Expr
		sb.OrderBy(func(b *Builder) { _ = b.Expr(expr) }, k.Desc)
	}
	e.emitSelect(sb)
	return nil
}
