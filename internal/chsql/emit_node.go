package chsql

import (
	"fmt"
	"strings"

	"github.com/tsouza/cerberus/internal/chplan"
)

func (e *emitter) emitScan(s *chplan.Scan) error {
	e.b.WriteString("SELECT ")
	if len(s.Columns) == 0 {
		e.b.WriteByte('*')
	} else {
		for i, c := range s.Columns {
			if i > 0 {
				e.b.WriteString(", ")
			}
			writeIdent(&e.b, c)
		}
	}
	e.b.WriteString(" FROM ")
	writeIdent(&e.b, s.Table)
	return nil
}

func (e *emitter) emitFilter(f *chplan.Filter) error {
	e.b.WriteString("SELECT * FROM ")
	if err := e.emitSubquery(f.Input); err != nil {
		return err
	}
	e.b.WriteString(" WHERE ")
	return e.emitExpr(f.Predicate)
}

func (e *emitter) emitProject(p *chplan.Project) error {
	e.b.WriteString("SELECT ")
	if len(p.Projections) == 0 {
		e.b.WriteByte('*')
	} else {
		for i, pr := range p.Projections {
			if i > 0 {
				e.b.WriteString(", ")
			}
			if err := e.emitExpr(pr.Expr); err != nil {
				return err
			}
			if pr.Alias != "" {
				e.b.WriteString(" AS ")
				writeIdent(&e.b, pr.Alias)
			}
		}
	}
	e.b.WriteString(" FROM ")
	return e.emitSubquery(p.Input)
}

func (e *emitter) emitAggregate(a *chplan.Aggregate) error {
	e.b.WriteString("SELECT ")
	first := true
	for _, g := range a.GroupBy {
		if !first {
			e.b.WriteString(", ")
		}
		first = false
		if err := e.emitExpr(g); err != nil {
			return err
		}
	}
	for _, af := range a.AggFuncs {
		if !first {
			e.b.WriteString(", ")
		}
		first = false
		if err := e.emitAggFunc(af); err != nil {
			return err
		}
	}
	if first {
		return fmt.Errorf("%w: Aggregate with no GroupBy keys and no AggFuncs", ErrUnsupported)
	}
	e.b.WriteString(" FROM ")
	if err := e.emitSubquery(a.Input); err != nil {
		return err
	}
	if len(a.GroupBy) > 0 {
		e.b.WriteString(" GROUP BY ")
		for i, g := range a.GroupBy {
			if i > 0 {
				e.b.WriteString(", ")
			}
			if err := e.emitExpr(g); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *emitter) emitAggFunc(af chplan.AggFunc) error {
	e.b.WriteString(af.Name)
	e.b.WriteByte('(')
	for i, a := range af.Args {
		if i > 0 {
			e.b.WriteString(", ")
		}
		if err := e.emitExpr(a); err != nil {
			return err
		}
	}
	e.b.WriteByte(')')
	if af.Alias != "" {
		e.b.WriteString(" AS ")
		writeIdent(&e.b, af.Alias)
	}
	return nil
}

// emitRangeWindow emits a placeholder form for range-vector lowering. The
// real shape lives in PR5 (PromQL lowering) and PR6 (optimizer); the seed's
// goal is just to produce a syntactically obvious comment that explains
// what the plan node represents, so spec tests can assert end-to-end on
// instant queries today while range queries land later.
func (e *emitter) emitRangeWindow(r *chplan.RangeWindow) error {
	fmt.Fprintf(&e.b, "/* range_window: func=%s range=%s step=%s */ SELECT * FROM ",
		r.Func, r.Range, r.Step)
	return e.emitSubquery(r.Input)
}

func (e *emitter) emitLimit(l *chplan.Limit) error {
	e.b.WriteString("SELECT * FROM ")
	if err := e.emitSubquery(l.Input); err != nil {
		return err
	}
	if l.Count > 0 {
		fmt.Fprintf(&e.b, " LIMIT %d", l.Count)
	}
	return nil
}

// writeIdent writes a ClickHouse identifier with backtick quoting, escaping
// embedded backticks. ClickHouse accepts backtick-quoted identifiers in all
// positions where an identifier is expected.
func writeIdent(b *strings.Builder, name string) {
	b.WriteByte('`')
	b.WriteString(strings.ReplaceAll(name, "`", "``"))
	b.WriteByte('`')
}
