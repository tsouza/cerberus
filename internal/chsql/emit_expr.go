package chsql

import (
	"github.com/tsouza/cerberus/internal/chplan"
)

// emit_expr.go is the legacy shim layer for the expression tree.
//
// As of RC6 R6.4, every emit method below routes through the public
// `chsql.Builder.Expr` family rather than writing SQL keywords or
// operator tokens directly into `e.b`. The shim exists because the
// grandfathered emitters in range_window.go / emit_node.go::emitOrderBy
// still call `e.emitExpr` / `e.bindArg`; both will collapse onto
// `Builder.Expr` directly when R6.5 ports those files.
//
// Each method here renders the expression into a fresh Builder via
// Builder.Expr, then splices the rendered SQL + args back into the
// emitter's accumulator. Builder.Expr is the canonical implementation;
// the shims preserve identical wire output for the grandfathered
// callers without re-implementing the expression tree twice.

// emitExpr renders x as a ClickHouse expression into e.b / e.args.
// Mirrors Builder.Expr; the two paths produce byte-identical SQL.
func (e *emitter) emitExpr(x chplan.Expr) error {
	b := &Builder{}
	if err := b.Expr(x); err != nil {
		return err
	}
	e.splice(b)
	return nil
}

// bindArg appends a `?` placeholder and records v in the emitter's args
// slice. Delegates to Builder.Arg; retained as an emitter method for the
// grandfathered range_window.go callsites which bind duration / timestamp
// literals around manually-rendered SQL fragments. R6.5 collapses those
// onto Builder.Arg directly.
func (e *emitter) bindArg(v any) error {
	b := &Builder{}
	b.Arg(v)
	e.splice(b)
	return nil
}
