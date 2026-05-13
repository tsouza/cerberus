// Package chsql emits ClickHouse SQL from a chplan.Node tree.
//
// Emit produces a parameterized SQL string with `?` placeholders plus a
// matching args slice; the caller (api/chclient) passes both to the
// ClickHouse driver. Every node type is emitted as a self-contained SELECT
// statement and children are inlined as subqueries; PR6's optimizer is
// expected to collapse trivial single-row subqueries.
package chsql

import (
	"errors"
	"fmt"
	"strings"

	"github.com/tsouza/cerberus/internal/chplan"
)

// ErrUnsupported is returned when the emitter encounters a node or
// expression it doesn't know how to render. Test fixtures cover every
// supported case; bumping coverage means extending the switch in
// emitNode/emitExpr.
var ErrUnsupported = errors.New("chsql: unsupported")

// Emit serializes a chplan tree as a ClickHouse SQL statement plus the
// positional argument list to bind. The SQL uses `?` placeholders.
func Emit(n chplan.Node) (string, []any, error) {
	if n == nil {
		return "", nil, fmt.Errorf("%w: nil node", ErrUnsupported)
	}
	e := &emitter{}
	if err := e.emitNode(n); err != nil {
		return "", nil, err
	}
	return e.b.String(), e.args, nil
}

type emitter struct {
	b    strings.Builder
	args []any
}

// emitNode writes a `SELECT ...` statement for n into e.b.
func (e *emitter) emitNode(n chplan.Node) error {
	switch v := n.(type) {
	case *chplan.Scan:
		return e.emitScan(v)
	case *chplan.Filter:
		return e.emitFilter(v)
	case *chplan.Project:
		return e.emitProject(v)
	case *chplan.Aggregate:
		return e.emitAggregate(v)
	case *chplan.MetricsAggregate:
		return e.emitMetricsAggregate(v)
	case *chplan.RangeWindow:
		return e.emitRangeWindow(v)
	case *chplan.HistogramQuantile:
		return e.emitHistogramQuantile(v)
	case *chplan.HistogramQuantileNative:
		return e.emitHistogramQuantileNative(v)
	case *chplan.Limit:
		return e.emitLimit(v)
	case *chplan.OrderBy:
		return e.emitOrderBy(v)
	case *chplan.VectorJoin:
		return e.emitVectorJoin(v)
	case *chplan.StructuralJoin:
		return e.emitStructuralJoin(v)
	case *chplan.SetOperation:
		return e.emitSetOperation(v)
	default:
		return fmt.Errorf("%w: node %T", ErrUnsupported, n)
	}
}

// emitSubquery wraps emitNode(n) in parentheses, used wherever a node feeds
// a parent SELECT's FROM clause.
func (e *emitter) emitSubquery(n chplan.Node) error {
	e.b.WriteByte('(')
	if err := e.emitNode(n); err != nil {
		return err
	}
	e.b.WriteByte(')')
	return nil
}
