package chsql

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/tsouza/cerberus/internal/chplan"
)

// splice drains b's accumulated SQL + args into the emitter. Used by the
// RC6 R6.2 ported emitters (emitScan / emitFilter / emitProject /
// emitLimit) so they can build SQL fragments through the public
// *Builder API and re-thread them into the shared emitter state at the
// point the unported subquery / expression emitters expect to find
// them. R6.3+ retires this helper as each remaining emit function gets
// ported and the emitter struct converts to a *Builder directly.
func (e *emitter) splice(b *Builder) {
	sql, args := b.Build()
	e.b.WriteString(sql)
	e.args = append(e.args, args...)
}

func (e *emitter) emitScan(s *chplan.Scan) error {
	b := NewBuilder()
	b.WriteSQL("SELECT ")
	if len(s.Columns) == 0 {
		b.WriteSQL("*")
	} else {
		for i, c := range s.Columns {
			if i > 0 {
				b.WriteSQL(", ")
			}
			b.Ident(c)
		}
	}
	b.WriteSQL(" FROM ")
	b.Ident(s.Table)
	e.splice(b)
	return nil
}

func (e *emitter) emitFilter(f *chplan.Filter) error {
	// Prefix: SELECT * FROM
	prefix := NewBuilder()
	prefix.WriteSQL("SELECT * FROM ")
	e.splice(prefix)
	// Subquery is still rendered through the legacy emitter — its
	// args land in e.args at this textual position, before the WHERE
	// clause emitted next.
	if err := e.emitSubquery(f.Input); err != nil {
		return err
	}
	// Suffix: WHERE <predicate>
	suffix := NewBuilder()
	suffix.WriteSQL(" WHERE ")
	if err := suffix.Expr(f.Predicate); err != nil {
		return err
	}
	e.splice(suffix)
	return nil
}

func (e *emitter) emitProject(p *chplan.Project) error {
	// Prefix: SELECT <projections>
	prefix := NewBuilder()
	prefix.WriteSQL("SELECT ")
	if len(p.Projections) == 0 {
		prefix.WriteSQL("*")
	} else {
		for i, pr := range p.Projections {
			if i > 0 {
				prefix.WriteSQL(", ")
			}
			if err := prefix.Expr(pr.Expr); err != nil {
				return err
			}
			if pr.Alias != "" {
				prefix.WriteSQL(" AS ")
				prefix.Ident(pr.Alias)
			}
		}
	}
	prefix.WriteSQL(" FROM ")
	e.splice(prefix)
	return e.emitSubquery(p.Input)
}

func (e *emitter) emitAggregate(a *chplan.Aggregate) error {
	// Prefix: SELECT <group-by keys + aliases>, <agg funcs>
	prefix := NewBuilder()
	prefix.WriteSQL("SELECT ")
	first := true
	for i, g := range a.GroupBy {
		if !first {
			prefix.WriteSQL(", ")
		}
		first = false
		if err := prefix.Expr(g); err != nil {
			return err
		}
		if i < len(a.GroupByAliases) && a.GroupByAliases[i] != "" {
			prefix.WriteSQL(" AS ")
			prefix.Ident(a.GroupByAliases[i])
		}
	}
	for _, af := range a.AggFuncs {
		if !first {
			prefix.WriteSQL(", ")
		}
		first = false
		if err := writeAggFunc(prefix, af); err != nil {
			return err
		}
	}
	if first {
		return fmt.Errorf("%w: Aggregate with no GroupBy keys and no AggFuncs", ErrUnsupported)
	}
	prefix.WriteSQL(" FROM ")
	e.splice(prefix)
	// Subquery still flows through the legacy emitter — its args land in
	// e.args at this textual position, between the SELECT list and the
	// optional GROUP BY suffix.
	if err := e.emitSubquery(a.Input); err != nil {
		return err
	}
	if len(a.GroupBy) > 0 {
		suffix := NewBuilder()
		suffix.WriteSQL(" GROUP BY ")
		for i, g := range a.GroupBy {
			if i > 0 {
				suffix.WriteSQL(", ")
			}
			if err := suffix.Expr(g); err != nil {
				return err
			}
		}
		e.splice(suffix)
	}
	return nil
}

// emitAggFunc renders an AggFunc into a fresh Builder and splices the
// result into the emitter. The legacy method signature is preserved so
// the (currently unported) RangeWindow / other callers continue to
// work; R6.4+ converts the remaining call sites to writeAggFunc.
func (e *emitter) emitAggFunc(af chplan.AggFunc) error {
	b := NewBuilder()
	if err := writeAggFunc(b, af); err != nil {
		return err
	}
	e.splice(b)
	return nil
}

// writeAggFunc renders `<name>[(<params>)](<args>) [AS <alias>]` into b
// using the public Builder helpers. The parameterised-aggregate shape
// (`quantile(0.95)(value)`, `quantiles(0.5, 0.9)(value)`, …) goes
// through Builder.ParamAgg.
func writeAggFunc(b *Builder, af chplan.AggFunc) error {
	// Capture the first expression-render error encountered while the
	// ParamAgg callbacks run; we surface it after ParamAgg returns so
	// the SQL stays consistent with the legacy emitter's positional
	// argument ordering on the happy path.
	var firstErr error
	mkExpr := func(x chplan.Expr) func(b *Builder) {
		return func(b *Builder) {
			if err := b.Expr(x); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	params := make([]func(b *Builder), 0, len(af.Params))
	for _, p := range af.Params {
		params = append(params, mkExpr(p))
	}
	args := make([]func(b *Builder), 0, len(af.Args))
	for _, a := range af.Args {
		args = append(args, mkExpr(a))
	}
	b.ParamAgg(af.Name, params, args)
	if firstErr != nil {
		return firstErr
	}
	if af.Alias != "" {
		b.WriteSQL(" AS ")
		b.Ident(af.Alias)
	}
	return nil
}

// emitRangeWindow lives in range_window.go — full windowed-array idiom.

func (e *emitter) emitLimit(l *chplan.Limit) error {
	prefix := NewBuilder()
	prefix.WriteSQL("SELECT * FROM ")
	e.splice(prefix)
	if err := e.emitSubquery(l.Input); err != nil {
		return err
	}
	if l.Count > 0 {
		// LIMIT count is part of the query *shape*, not user data —
		// rendered as a literal integer, matching SelectBuilder.Limit.
		suffix := NewBuilder()
		suffix.WriteSQL(" LIMIT ")
		suffix.WriteSQL(strconv.FormatInt(l.Count, 10))
		e.splice(suffix)
	}
	return nil
}

// emitOrderBy renders `SELECT * FROM (<input>) ORDER BY <k1> [DESC], …`.
// Empty Keys is a programmer error — emit an error so the plan tree
// doesn't silently lose its sort intent.
func (e *emitter) emitOrderBy(o *chplan.OrderBy) error {
	if len(o.Keys) == 0 {
		return fmt.Errorf("%w: OrderBy with no keys", ErrUnsupported)
	}
	e.b.WriteString("SELECT * FROM ")
	if err := e.emitSubquery(o.Input); err != nil {
		return err
	}
	e.b.WriteString(" ORDER BY ")
	for i, k := range o.Keys {
		if i > 0 {
			e.b.WriteString(", ")
		}
		if err := e.emitExpr(k.Expr); err != nil {
			return err
		}
		if k.Desc {
			e.b.WriteString(" DESC")
		}
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
