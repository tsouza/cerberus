package tempo

import (
	"context"
	"fmt"
	"time"

	traceql "github.com/tsouza/cerberus/internal/traceql/ast"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/schema"
	traceql_lower "github.com/tsouza/cerberus/internal/traceql"
)

// NewExplainLang builds an engine.Lang for cerberus's offline TraceQL explain —
// the migration preview's read-only "what SQL would cerberus run for this
// TraceQL query" path. It wraps the same unexported traceqlLang the /api/search
// handler drives, so a plain SEARCH query lowers through the identical parse +
// lower + wrap-projection pipeline; a METRICS-pipeline query (`{...} | rate()`,
// `{...} | count_over_time()`, …) additionally gets the range-window matrix wrap
// the /api/metrics/query_range handler applies, so it previews in the same
// evaluation mode the server would run it in.
//
// Both modes lower to a BOUNDED otel_traces scan: the search window + trace
// limit ride in on the context (thread them with ExplainContext before calling
// engine.DryRunSQL), and the metrics wrap reads that same window to build its
// RangeWindow. Without the bound, lowering would emit an unbounded spans scan
// that chsql.Emit's RequireSpansScansBounded chokepoint rejects — surfacing as a
// bogus UNSUPPORTED rather than a real preview. `step` sizes the metrics matrix
// buckets; it must be > 0.
//
// Trace-by-id (/traces/{id}) is the third TraceQL mode; it has no TraceQL
// expression to explain (the handler hand-rolls a lookup plan from a raw id), so
// the offline explainer covers SEARCH + metrics-range only.
func NewExplainLang(s schema.Traces, step time.Duration) engine.Lang {
	return &explainLang{traceqlLang: traceqlLang{schema: s}, step: step}
}

// ExplainContext pre-threads the offline explain window + trace limit onto ctx
// so a TraceQL query lowers to a BOUNDED spans scan under engine.DryRunSQL. A
// windowless request (both bounds zero) is clamped to the most recent
// DefaultSearchLookback ending at `end` — the same recent-data default
// /api/search applies — so the preview never emits an unbounded otel_traces
// scan; a one-sided window has its missing start clamped the same way. A
// non-positive limit falls back to DefaultSearchLimit.
func ExplainContext(ctx context.Context, start, end time.Time, limit int) context.Context {
	if limit <= 0 {
		limit = DefaultSearchLimit
	}
	// Clamp a windowless / one-sided request to a bounded lookback so the search
	// stamps and the metrics RangeWindow both have a real [start, end] to prune
	// on. When end is also absent there is nothing deterministic to anchor to, so
	// leave the (zero) window untouched and let the caller's explicit bound win.
	if start.IsZero() && !end.IsZero() {
		start = end.Add(-DefaultSearchLookback)
	}
	ctx = traceql_lower.WithSearchTraceLimit(ctx, limit)
	ctx = traceql_lower.WithSearchWindow(ctx, start, end)
	return ctx
}

// explainLang adapts the TraceQL head to engine.Lang for offline explain. It
// embeds traceqlLang so Name / ProjectSamples (search path) / SpansTable are
// inherited unchanged, and overrides Parse to route a metrics-pipeline query
// through the range-window matrix wrap.
type explainLang struct {
	traceqlLang
	step time.Duration
}

// Parse routes on the parsed query shape: a metrics-pipeline query builds the
// range matrix plan, everything else falls through to the shared search
// lowering (which reads the window + trace limit already threaded onto ctx).
func (l *explainLang) Parse(ctx context.Context, query string) (chplan.Node, engine.Meta, error) {
	expr, err := parseExpr(ctx, query)
	if err != nil {
		return nil, engine.Meta{}, fmt.Errorf("%w: %w", errParseStage, err)
	}
	if expr.MetricsPipeline != nil || expr.MetricsSecondStage != nil {
		return l.parseMetrics(ctx, expr)
	}
	return l.traceqlLang.Parse(ctx, query)
}

// parseMetrics mirrors handleMetricsQueryRange's plan construction (minus the
// HTTP / exemplar / execution machinery): lower the metrics pipeline, wrap the
// aggregate with a step-sized chplan.RangeWindow bounded to the explain window,
// then project into the Sample matrix shape. The RangeWindow is what bounds the
// metrics inner spans scan (the top-level RequireSpansScansBounded chokepoint
// skips the metrics-emitter subtree, deferring to the emitter's own per-site
// bound), so a windowless preview would otherwise emit an unbounded scan.
func (l *explainLang) parseMetrics(ctx context.Context, expr *traceql.RootExpr) (chplan.Node, engine.Meta, error) {
	if l.step <= 0 {
		return nil, engine.Meta{}, fmt.Errorf("%w: metrics explain needs a positive step", errLowerStage)
	}
	start, end := traceql_lower.SearchWindow(ctx)
	start, end = alignMetricsWindow(start, end, l.step)
	// Thread the aligned window so the universal recursive-arm stamp bounds a
	// metrics-over-structural source (`{ } >> { } | rate()`) the RangeWindow wrap
	// cannot reach below.
	ctx = traceql_lower.WithSearchWindow(ctx, start, end)
	plan, err := traceql_lower.Lower(ctx, expr, l.schema)
	if err != nil {
		return nil, engine.Meta{}, fmt.Errorf("%w: %w", errLowerStage, err)
	}
	stages, inner := peelMetricsSecondStages(plan)
	metrics, ok := unwrapMetricsAggregate(inner)
	if !ok {
		// histogram_over_time / compare() lower to their own matrix node shapes
		// (the handler's serveMetricsQueryRangeNonScalar path). The offline
		// preview covers the scalar-aggregate metrics families; name the gap
		// honestly rather than mis-wrapping a non-scalar plan.
		return nil, engine.Meta{}, fmt.Errorf(
			"%w: offline metrics preview covers scalar aggregates (rate / count_over_time / *_over_time / quantile_over_time); %q previews a non-scalar metrics shape", errLowerStage, expr.String(),
		)
	}
	rw := &chplan.RangeWindow{
		Input:           inner,
		Range:           l.step,
		Step:            l.step,
		Start:           start,
		End:             end,
		TimestampColumn: l.schema.TimestampColumn,
	}
	wrapped := wrapMetricsForSample(
		applyMetricsSecondStages(rw, stages, []string{chsql.RangeWindowAnchorAlias}),
		metrics,
	)
	return wrapped, engine.Meta{IsMetric: true, ResponseShape: "tempo-metrics-matrix"}, nil
}

// ProjectSamples keeps the matrix wrap parseMetrics already applied (a metrics
// plan is Sample-shaped on emit) and defers to the embedded search projection
// for plain search plans.
func (l *explainLang) ProjectSamples(plan chplan.Node, meta engine.Meta) chplan.Node {
	if meta.IsMetric {
		return plan
	}
	return l.traceqlLang.ProjectSamples(plan, meta)
}
