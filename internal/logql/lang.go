package logql

import (
	"context"
	"net/http"
	"time"

	"github.com/grafana/loki/v3/pkg/logql/syntax"
	"go.opentelemetry.io/otel/trace"

	"github.com/tsouza/cerberus/internal/api/httperr"
	"github.com/tsouza/cerberus/internal/cerbtrace"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/telemetry"
)

// Lang is the LogQL adapter for engine.Engine. It owns the upstream
// parser type (syntax.Expr), the lowering call (Lower), and the
// per-query metric/log decision that drives the wrap-projection.
//
// Lang.Parse runs the LogQL parser, classifies the expression as a
// metric or log query, lowers it to chplan, and returns the plan plus
// engine.Meta{IsMetric}. Lang.ProjectSamples then wraps the plan with
// the canonical chclient.Sample shape — synthesising MetricName /
// TimeUnix / Value for metric queries and pulling Body / Timestamp out
// of the logs table for log-stream queries.
//
// The parser-stage spans (cerbtrace.SpanParse + cerbtrace.SpanLower)
// open inside Parse so cerberus's trace shape stays identical to the
// pre-engine handler.
//
// Start / End carry the request's wire-format [start, end] window so
// the lowering can AND-fold a Timestamp BETWEEN predicate above every
// Scan(LogsTable). Zero values disable the window injection (matches
// the previous behaviour for callers that only care about the parse
// + lower contract without an HTTP window). The handler constructs a
// fresh *Lang per request; the long-lived bits (Schema) come from the
// Handler so per-request allocation is cheap.
type Lang struct {
	Schema schema.Logs
	Start  time.Time
	End    time.Time
}

// errorTypes mirrors the Loki errorType vocabulary the handler emits.
// Duplicated here (not re-imported from internal/api/loki) so the LogQL
// adapter doesn't depend on the HTTP handler package — engine adapters
// sit underneath the handlers in the dependency graph.
const (
	errBadData   = "bad_data"
	errExecution = "execution"
)

// Name returns "logql" — the stable per-language label engine threads
// onto progress-context keys and trace attributes.
func (l *Lang) Name() string { return "logql" }

// Parse runs the LogQL parser, lowers the AST, and returns the plan
// plus engine.Meta. Parser failures map to 400 bad_data; lowering
// failures (e.g. unsupported `| json` stage in the M3 window) map to
// 422 execution — both wire-format-identical to the handler's
// pre-engine error contract.
func (l *Lang) Parse(ctx context.Context, query string) (chplan.Node, engine.Meta, error) {
	parseT := telemetry.ObserveStage(telemetry.StageParse)
	expr, err := parseExprTraced(ctx, query)
	parseT.Done(ctx)
	if err != nil {
		return nil, engine.Meta{}, &httperr.Error{
			Kind:   errBadData,
			Err:    err,
			Status: http.StatusBadRequest,
		}
	}

	lowerT := telemetry.ObserveStage(telemetry.StageLower)
	plan, err := LowerAt(ctx, expr, l.Schema, l.Start, l.End)
	lowerT.Done(ctx)
	if err != nil {
		return nil, engine.Meta{}, &httperr.Error{
			Kind:   errExecution,
			Err:    err,
			Status: http.StatusUnprocessableEntity,
		}
	}

	meta := engine.Meta{
		IsMetric: IsMetricQuery(expr),
		Extra:    map[string]any{"expr": expr},
	}
	if meta.IsMetric {
		meta.ResponseShape = "loki-matrix"
	} else {
		meta.ResponseShape = "loki-streams"
	}
	return plan, meta, nil
}

// ProjectSamples wraps plan with the projection that reshapes the
// emitted rows into chclient.Sample's positional shape. Metric queries
// synthesise MetricName + a near-now TimeUnix anchor (mirrors the
// promql side's anchor trick to keep matrix step-grid bucketing from
// dropping the only row); log queries pull the log Body into the
// MetricName slot (Sample.Value is float64, so the string body has to
// ride in a String column).
func (l *Lang) ProjectSamples(plan chplan.Node, meta engine.Meta) chplan.Node {
	s := l.Schema
	if meta.IsMetric {
		// Metric queries lower to RangeWindow / Aggregate / Filter(Aggregate),
		// whose output is (group-keys…, <metric-value>). MetricName + TimeUnix
		// don't exist in that scope — synthesise them so the chclient
		// Sample scanner has the four positional columns it expects.
		//
		// The metric-value column is the canonical PascalCase `Value` (the
		// alias the RangeWindow / Aggregate emitters project at every outer
		// SELECT site since #310 collapsed the rename Project layer); mirror
		// it here so the wire-wrap doesn't ColumnRef the pre-#310 lowercase
		// alias.
		//
		// Inner stream-identity column resolution: a bare range-aggregation
		// (`rate({...}[5m])` / `count_over_time({...}[5m])` / …) leaves the
		// raw `ResourceAttributes` column in scope, since the RangeWindow's
		// outer SELECT projects it under its own name. A vector aggregation
		// (`sum(rate(...))` / `sum by (svc) (count_over_time(...))` / …)
		// runs through [wrapVectorAggregateForSample], which has already
		// projected the row into the canonical (MetricName, Attributes,
		// TimeUnix, Value) Sample contract — at that point `ResourceAttributes`
		// is gone (the Aggregate's GROUP BY consumed it) and the stream
		// identity rides under the `Attributes` alias instead. Reading
		// `ResourceAttributes` in that scope surfaces as 502 'Unknown
		// expression identifier ResourceAttributes' from ClickHouse. Pick
		// the right column name based on the inner shape — mirrors the
		// same `isVectorAggregateSampleShape` switch the binop lowering
		// applies in [sampleShapeOverLogInner].
		attrsCol := s.ResourceAttributesColumn
		if isVectorAggregateSampleShape(plan) {
			attrsCol = "Attributes"
		}
		return &chplan.Project{
			Input: plan,
			Projections: []chplan.Projection{
				{Expr: &chplan.LitString{V: ""}, Alias: "MetricName"},
				{Expr: &chplan.ColumnRef{Name: attrsCol}, Alias: "Attributes"},
				// now64(9) - 5s buffer; see prom handler's synthesizedAnchor
				// docstring. Avoids toMatrixStepGrid dropping the only row
				// when CH-now > client-end.
				{Expr: &chplan.Binary{
					Op:    chplan.OpSub,
					Left:  &chplan.FuncCall{Name: "now64", Args: []chplan.Expr{&chplan.LitInt{V: 9}}},
					Right: &chplan.FuncCall{Name: "toIntervalNanosecond", Args: []chplan.Expr{&chplan.LitInt{V: 5_000_000_000}}},
				}, Alias: "TimeUnix"},
				{Expr: &chplan.ColumnRef{Name: rangeAggSynthValueColumn}, Alias: "Value"},
			},
		}
	}
	// Log-stream query: chclient.Sample is (MetricName, Attributes, Timestamp,
	// Value) where Value is float64. The log line `Body` is a String, so it
	// can't ride in Value — instead we put it in MetricName (also a String)
	// and write a 0.0 placeholder into Value. toStreamsWithTransform reads
	// back from Sample.MetricName as the line content.
	return &chplan.Project{
		Input: plan,
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: s.BodyColumn}, Alias: "MetricName"},
			{Expr: &chplan.ColumnRef{Name: s.ResourceAttributesColumn}, Alias: "Attributes"},
			{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}, Alias: "TimeUnix"},
			// Wrap the placeholder zero in toFloat64 so CH returns the column
			// as Float64; without the cast a bare `0` literal becomes UInt8
			// and clickhouse-go's Scan rejects UInt8 → *float64.
			{Expr: &chplan.FuncCall{Name: "toFloat64", Args: []chplan.Expr{&chplan.LitFloat{V: 0}}}, Alias: "Value"},
		},
	}
}

// IsMetricQuery reports whether the parsed LogQL expression produces a
// numeric series (rate / count_over_time / aggregations) versus a raw
// log-line stream. Exported so the api/loki handler can pivot the
// response shape (matrix/vector vs streams) without re-classifying via
// engine.Meta — the AST type switch is the single source of truth.
func IsMetricQuery(expr syntax.Expr) bool {
	switch expr.(type) {
	case *syntax.RangeAggregationExpr, *syntax.VectorAggregationExpr,
		*syntax.LiteralExpr, *syntax.BinOpExpr, *syntax.LabelReplaceExpr:
		return true
	}
	return false
}

// parseExprTraced wraps syntax.ParseExpr in a cerbtrace.SpanParse span.
// Mirrors the per-handler tracer block so the engine-driven path emits
// the same span tree as the pre-port pipeline.
func parseExprTraced(ctx context.Context, query string) (syntax.Expr, error) {
	_, span := tracer.Start(ctx, cerbtrace.SpanParse,
		trace.WithAttributes(cerbtrace.ParseAttrs("logql", query)...))
	defer span.End()
	expr, err := syntax.ParseExpr(query)
	if err != nil {
		span.RecordError(err)
		return nil, err
	}
	return expr, nil
}
