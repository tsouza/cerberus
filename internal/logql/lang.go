package logql

import (
	"context"
	"net/http"
	"strings"
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
//
// Step carries the request's `step` for /loki/api/v1/query_range metric
// queries. When > 0, the range-aggregation lowering switches to the
// matrix RangeWindow shape (one row per anchor across [Start, End]
// spaced by Step) so the emitter fans across the step grid instead of
// anchoring at `now64(9)`. Without this, a metric query whose seeded
// data lies outside the last 5 minutes of wall-clock returns an empty
// matrix (the compat-harness 0/55 regression that surfaced when the
// seed window drifted away from wall-clock).
type Lang struct {
	Schema schema.Logs
	Start  time.Time
	End    time.Time
	Step   time.Duration
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
	plan, err := LowerAtRange(ctx, expr, l.Schema, l.Start, l.End, l.Step)
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
		// TimeUnix source:
		//   - Matrix-shape RangeWindow (OuterRange > 0): the inner SELECT
		//     exposes the per-row anchor under the literal column
		//     `anchor_ts`. Forward it so toMatrixStepGrid sees one row per
		//     anchor across the request's step grid. Without this, the
		//     outer synthesised `now64(9) - 5s` collapses every per-step
		//     row into a single point — the matrix pivot then sees one
		//     sample per series instead of one per step.
		//   - Vector aggregate over matrix RangeWindow: the inner
		//     `wrapVectorAggregateForSample` already projected TimeUnix
		//     from the per-anchor bucket alias. Forward the existing
		//     `TimeUnix` column rather than overwrite it.
		//   - Instant shape with a known request window: anchor the
		//     synthesised timestamp at the request End. The previous
		//     `now64(9) - 5s` synthesis was load-sensitive — CH
		//     evaluates now64 at query-execution time, so any >5s gap
		//     between the client building its [start, end] window and
		//     CH executing the query (admission-control queueing under
		//     a saturated sweep) pushed the only row past `end`, and
		//     toMatrixStepGrid's boundary clip dropped it (`vector(1)`
		//     flaking between 1 series and empty). End is inside the
		//     window by definition, so the row always survives.
		//   - Instant shape without a window (Lang constructed bare):
		//     keep the `now64(9) - 5s` synthesis.
		var tsExpr chplan.Expr
		switch {
		case isVectorAggregateSampleShape(plan):
			tsExpr = &chplan.ColumnRef{Name: "TimeUnix"}
		case isMatrixRangeWindow(plan):
			tsExpr = &chplan.ColumnRef{Name: "anchor_ts"}
		case !l.End.IsZero():
			tsExpr = timeLiteralExpr(l.End)
		default:
			tsExpr = chplan.NowNanoMinusStaleness()
		}
		return &chplan.Project{
			Input: plan,
			Projections: []chplan.Projection{
				{Expr: &chplan.LitString{V: ""}, Alias: "MetricName"},
				{Expr: &chplan.ColumnRef{Name: attrsCol}, Alias: "Attributes"},
				{Expr: tsExpr, Alias: "TimeUnix"},
				{Expr: &chplan.ColumnRef{Name: rangeAggSynthValueColumn}, Alias: "Value"},
			},
		}
	}
	// Log-stream query: chclient.Sample is (MetricName, Attributes, Timestamp,
	// Value) where Value is float64. The log line `Body` is a String, so it
	// can't ride in Value — instead we put it in MetricName (also a String)
	// and write a 0.0 placeholder into Value. toStreamsWithTransform reads
	// back from Sample.MetricName as the line content.
	//
	// The Attributes column is wrapped in [withDetectedLevel] so the
	// emitted stream identity carries the synthesized severity label
	// whenever the row's `SeverityText` is non-empty. Reference Loki
	// surfaces `detected_level` on every log query that returns rows
	// whose severity can be detected from stream / structured-metadata
	// labels, parser-stage extraction, or content scan (see
	// `queryShouldSurfaceDetectedLevel`'s doc comment for the upstream
	// detection sources cerberus mirrors). The wrap's inner mapFilter
	// drops the `detected_level` entry on rows without severity, so a
	// query that lands on severity-free data sees no change in its
	// stream label set.
	//
	// The earlier restrictive gate (only when the user explicitly
	// named `detected_level` / `level` in a matcher / filter /
	// grouping) caused the `fast/basic-selectors.yaml` regressions —
	// reference Loki splits bare selector queries into one Stream per
	// detected_level value, but cerberus collapsed them into a single
	// Stream because the gate never fired. The broadened predicate
	// restores stream-identity parity for bare selectors, line
	// filters, label filters on unrelated keys, and parser-stage
	// queries (`| logfmt`, `| json`, `| regexp ...`).
	//
	// When the parsed pipeline carries a SQL-side parser stage
	// (`| logfmt`, `| json`, `| regexp ...`, typed-variants), the
	// projection swaps the bare `ResourceAttributes` column for the
	// pipeline's final labels-map expression (see [PipelineLabelsExpr])
	// so each row carries the union of stream-selector labels and
	// parser-extracted keys. [toStreamsWithTransform] groups by the
	// projected label set, so unique (resource-label, extracted-key)
	// tuples land in distinct Streams — matching reference Loki's
	// stream-identity contract. Without this swap the projection only
	// surfaces ResourceAttributes and a query like
	// `{cluster="c"} | logfmt` collapses hundreds of upstream-Loki
	// streams into a single one, regressing the loki-compat diff.
	expr, _ := meta.Extra["expr"].(syntax.Expr)
	attrsExpr := chplan.Expr(&chplan.ColumnRef{Name: s.ResourceAttributesColumn})
	if HasLabelMutatingStage(expr) {
		if parsed, err := PipelineLabelsExpr(expr, s); err == nil && parsed != nil {
			attrsExpr = parsed
		}
	}
	if queryShouldSurfaceDetectedLevel(expr) {
		attrsExpr = withDetectedLevel(s, attrsExpr)
	}
	return &chplan.Project{
		Input: plan,
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: s.BodyColumn}, Alias: "MetricName"},
			{Expr: attrsExpr, Alias: "Attributes"},
			{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}, Alias: "TimeUnix"},
			// LitFloat is wrapped centrally in toFloat64(?) by
			// internal/chsql/Builder.Expr; without that pin CH would
			// narrow the bare `0` placeholder to UInt8 and clickhouse-go's
			// Sample Scan would reject UInt8 → *float64.
			{Expr: &chplan.LitFloat{V: 0}, Alias: "Value"},
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
		*syntax.LiteralExpr, *syntax.BinOpExpr, *syntax.LabelReplaceExpr,
		*syntax.VectorExpr:
		// VectorExpr (`vector(1)`) produces a numeric sample, same as
		// LiteralExpr. Omitting it routed the query down the
		// log-stream wrap, which references Body / Timestamp — columns
		// the synthetic one-row plan doesn't carry — and surfaced as
		// 502 `Unknown expression identifier 'Body'`.
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
	// Rewrite OTel-dotted stream-selector keys (`service.name` →
	// `service_name`) before the parser sees them. Without this, a
	// Grafana query landing on cerberus from the Loki datasource hits
	// `parse error … unexpected '.'`. The OTel-CH schema stores both
	// forms on each row, so the underscored matcher targets the same
	// data the dotted form would.
	expr, err := ParseExprPermissive(normalizeLokiDottedLabels(query))
	if err != nil {
		span.RecordError(err)
		return nil, err
	}
	return expr, nil
}

// ParseExprPermissive parses a LogQL expression with cerberus's
// permissive contract: shapes upstream Loki rejects as
// "empty-compatible" matchers (e.g. `{label=~".*"}` — Loki's
// SplitFiltersAndMatchers drops the matcher entirely as a no-op, then
// `validateMatchers` rejects the now-empty matcher set with "queries
// require at least one regexp or equality matcher that does not have
// an empty-compatible value") are accepted instead.
//
// Rationale: cerberus is a query gateway, not an index-scoped store.
// Upstream Loki's rejection exists because its chunk index is keyed by
// label set and a match-all matcher would force a full-store fan-out;
// cerberus translates to a ClickHouse WHERE predicate that the CH
// optimiser already prunes (PREWHERE / MV substitution / sparse-index
// skip), so a `{service_name=~".*"}` query is well-defined and lowers
// to `match(ResourceAttributes['service_name'], '.*')` — equivalent to
// `service_name!=""` on rows where the label is present, plus the
// rows where it's absent (RA[missing] = ” in CH; `match(”, '.*')`
// returns 1). The Grafana "Logs Drilldown" UX hits this shape every
// time the user clears all label filters but keeps the data source
// selected, so the rejection surfaces as a confusing 400 instead of
// the expected "all streams" result.
//
// Implementation: try the strict ParseExpr first (the common case —
// every well-formed Loki query passes). On the specific
// empty-compatible rejection, retry with ParseExprWithoutValidation —
// the parser-stage errors (`e.err` fields on BinOpExpr / LiteralExpr /
// VectorExpr / VectorAggregationExpr / LabelReplaceExpr) are populated
// during parsing itself, so the permissive path still surfaces them
// downstream when cerberus's lowering walks the AST.
//
// Detection is by error-message substring because the upstream
// errAtleastOneEqualityMatcherRequired constant is unexported.
// `empty-compatible` is unique to that single rejection path in
// vendored Loki v3.0.0-cerberus-parser, so the substring is stable
// across the corpus the cerberus-forks-monitor rebases.
//
// Exported so handlers outside the logql package
// (internal/api/loki/index_stats.go's selectorMatchers,
// internal/api/loki/tail.go) can share the same permissive contract
// without re-importing the upstream syntax package directly.
func ParseExprPermissive(query string) (syntax.Expr, error) {
	expr, err := syntax.ParseExpr(query)
	if err == nil {
		return expr, nil
	}
	if !strings.Contains(err.Error(), "empty-compatible") {
		return nil, err
	}
	return syntax.ParseExprWithoutValidation(query)
}
