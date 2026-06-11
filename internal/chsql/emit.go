// Package chsql emits ClickHouse SQL from a chplan.Node tree.
//
// Emit produces a parameterized SQL string with `?` placeholders plus a
// matching args slice; the caller (api/chclient) passes both to the
// ClickHouse driver. Every node type is emitted as a self-contained SELECT
// statement and children are inlined as subqueries; PR6's optimizer is
// expected to collapse trivial single-row subqueries.
package chsql

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.opentelemetry.io/otel"

	"github.com/tsouza/cerberus/internal/cerbtrace"
	"github.com/tsouza/cerberus/internal/chplan"
)

// tracer emits the `emit` pipeline-stage span.
var tracer = otel.Tracer("github.com/tsouza/cerberus/internal/chsql")

// ErrUnsupported is returned when the emitter encounters a node or
// expression it doesn't know how to render. Test fixtures cover every
// supported case; bumping coverage means extending the switch in
// emitNode/emitExpr.
var ErrUnsupported = errors.New("chsql: unsupported")

// Emit serializes a chplan tree as a ClickHouse SQL statement plus the
// positional argument list to bind. The SQL uses `?` placeholders.
//
// The ctx parameter carries the parent OpenTelemetry span (typically
// the otelhttp request span). Emit wraps the rendering in an `emit`
// pipeline-stage span so a query's flame graph shows how long SQL
// serialization took. The emitted SQL byte length is surfaced as
// `cerberus.sql_length` on the span.
func Emit(ctx context.Context, n chplan.Node) (string, []any, error) {
	_, span := tracer.Start(ctx, cerbtrace.SpanEmit)
	defer span.End()
	if n == nil {
		err := fmt.Errorf("%w: nil node", ErrUnsupported)
		span.RecordError(err)
		return "", nil, err
	}
	e := &emitter{}
	if err := e.emitNode(n); err != nil {
		span.RecordError(err)
		return "", nil, err
	}
	sql := e.b.String()
	span.SetAttributes(cerbtrace.AttrSQLLength.Int(len(sql)))
	return sql, e.args, nil
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
	case *chplan.OneRow:
		return e.emitOneRow(v)
	case *chplan.StepGrid:
		return e.emitStepGrid(v)
	case *chplan.Filter:
		return e.emitFilter(v)
	case *chplan.Project:
		return e.emitProject(v)
	case *chplan.Aggregate:
		return e.emitAggregate(v)
	case *chplan.MetricsAggregate:
		return e.emitMetricsAggregate(v)
	case *chplan.MetricsSecondStage:
		return e.emitMetricsSecondStage(v)
	case *chplan.MetricsHistogramOverTime:
		return e.emitMetricsHistogramOverTime(v)
	case *chplan.MetricsCompare:
		return e.emitMetricsCompare(v)
	case *chplan.RangeWindow:
		return e.emitRangeWindow(v)
	case *chplan.AbsentOverTime:
		return e.emitAbsentOverTime(v)
	case *chplan.HistogramQuantile:
		return e.emitHistogramQuantile(v)
	case *chplan.HistogramQuantileNative:
		return e.emitHistogramQuantileNative(v)
	case *chplan.Limit:
		return e.emitLimit(v)
	case *chplan.OrderBy:
		return e.emitOrderBy(v)
	case *chplan.TopK:
		return e.emitTopK(v)
	case *chplan.VectorJoin:
		return e.emitVectorJoin(v)
	case *chplan.VectorSetOp:
		return e.emitVectorSetOp(v)
	case *chplan.StructuralJoin:
		return e.emitStructuralJoin(v)
	case *chplan.NestedSetAnnotate:
		return e.emitNestedSetAnnotate(v)
	case *chplan.CrossJoin:
		return e.emitCrossJoin(v)
	case *chplan.SetOperation:
		return e.emitSetOperation(v)
	case *chplan.UnionAll:
		return e.emitUnionAll(v)
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
