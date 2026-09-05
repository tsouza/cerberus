package tempo

import (
	"context"
	"errors"
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/telemetry"
	traceql_lower "github.com/tsouza/cerberus/internal/traceql"
	traceql "github.com/tsouza/cerberus/internal/traceql/ast"
)

// errParseStage / errLowerStage are sentinel markers the Lang adapter
// uses so the handler-side error classifier can distinguish parser
// failures (HTTP 400) from lowering failures (HTTP 422). The engine
// wraps Lang.Parse errors with `engine: parse:` which collapses both
// into a single bucket; carrying the stage in the wrapped error chain
// preserves the per-stage HTTP-status mapping.
//
// ErrParseStage / ErrLowerStage are the exported aliases callers outside
// this package chain via errors.Is. Both the HTTP handlers and the sibling
// gRPC handler (internal/api/tempo/grpc) reach them through the shared
// ClassifyErr ladder in errclass.go rather than matching directly.
var (
	errParseStage = errors.New("traceql parse stage")
	errLowerStage = errors.New("traceql lower stage")

	// ErrParseStage / ErrLowerStage re-export the parse / lower stage
	// markers so external callers (e.g. the gRPC StreamingQuerier
	// surface) can errors.Is against them without depending on the
	// unexported sentinels.
	ErrParseStage = errParseStage
	ErrLowerStage = errLowerStage
)

// traceqlLang adapts the TraceQL head to engine.Lang. Parse runs the
// Tempo parser + lowering (each wrapped in its pipeline-stage span +
// stopwatch so the trace shape matches what the inlined handler emits
// today); ProjectSamples delegates to wrapWithSampleProjection so the
// canonical chclient.Sample row shape is materialised before the
// optimizer pass.
//
// Tempo's /traces/{id} short-circuit bypasses Parse entirely — the
// handler constructs the lookup plan via lowerTraceByID and calls
// engine.QueryPlan with Meta.IsTraceByID = true; that path still goes
// through ProjectSamples (which keeps the same wrap-projection rule).
//
// All trace responses are span-summary shaped (no metric matrix), so
// Meta.IsMetric stays false. ResponseShape is "tempo-trace" — purely
// informational since the handler picks its envelope by route, not by
// the meta flag.
type traceqlLang struct {
	schema schema.Traces

	// AttrStrategies resolves how the traces attribute-map columns
	// (schema.AttributesColumn / ResourceAttributesColumn /
	// ScopeAttributesColumn) are physically stored, per
	// internal/preflight's boot probe (cerberus issue #2777,
	// Result.TracesAttrStrategies) — nil (the zero value) means every
	// column is a genuine ClickHouse Map, rendering byte-identical to
	// before this field existed. Wired via Handler.SetAttrStrategies,
	// which cmd/cerberus calls once at construction (mirroring
	// internal/api/loki's Handler.AttrStrategies + h.Lang.AttrStrategies
	// copy) — see SetAttrStrategies's doc for why tempo needs a setter
	// where loki needs only a field assignment. EmitAttrStrategies
	// (below) is the duck-typed hook engine.emitForHead reads to thread
	// it onto the emit-time context, exactly like *logql.Lang's field of
	// the same name (cerberus issue #3062).
	AttrStrategies chsql.AttrStrategies
}

func (l *traceqlLang) Name() string { return telemetry.QLTraceQL }

// SpansTable exposes the spans table so the engine threads it onto the emit
// context (chsql.WithSpansTable), letting RequireSpansScansBounded verify every
// otel_traces scan in the search / structural / nested-set / trace-by-id plans
// is resource-bounded.
func (l *traceqlLang) SpansTable() string { return l.schema.SpansTable }

// EmitAttrStrategies returns the AttrStrategies this Lang's queries should
// render attribute-map accesses against — see the AttrStrategies field's
// doc. engine.emitForHead duck-types on this (the attrStrategier
// interface) to thread it onto the emit context via
// chsql.WithAttrStrategies, exactly as it already does for LogQL — the
// duck-typed hook is signal-agnostic, so this method is the entire
// remaining wiring needed on the Lang side of cerberus issue #3062.
func (l *traceqlLang) EmitAttrStrategies() chsql.AttrStrategies { return l.AttrStrategies }

func (l *traceqlLang) Parse(ctx context.Context, query string) (chplan.Node, engine.Meta, error) {
	// Parse pipeline-stage stopwatch — mirrors the inlined handler so
	// cerberus.queries.parse_duration_ms keeps its per-head label.
	parseT := telemetry.ObserveStage(telemetry.StageParse, l.Name())
	expr, err := parseExpr(ctx, query)
	parseT.Done(ctx)
	if err != nil {
		return nil, engine.Meta{}, fmt.Errorf("%w: %w", errParseStage, err)
	}

	// Lower pipeline-stage stopwatch. traceql.Lower opens its own
	// cerbtrace.SpanLower span internally.
	lowerT := telemetry.ObserveStage(telemetry.StageLower, l.Name())
	plan, err := traceql_lower.Lower(ctx, expr, l.schema)
	lowerT.Done(ctx)
	if err != nil {
		return nil, engine.Meta{}, fmt.Errorf("%w: %w", errLowerStage, err)
	}

	return plan, engine.Meta{
		IsMetric:      false,
		IsTraceByID:   false,
		ResponseShape: "tempo-trace",
		// Carry the /api/search trace limit so ProjectSamples can cap a
		// spanset-aggregation search to the newest N traces server-side
		// (the parity counterpart to plain search's SearchTraceLimit node).
		Extra: map[string]any{
			metaKeySearchTraceLimit: traceql_lower.SearchTraceLimit(ctx),
			// Carry whether the query named the `name` intrinsic, so the
			// response shaper can populate spanSets[].spans[].name for
			// exactly the queries reference Tempo populates it for. Must
			// be computed here: this is the only place the parsed AST is
			// still in hand.
			metaKeyRefsNameIntrinsic: expr.ReferencesIntrinsic(traceql.IntrinsicName),
		},
	}, nil
}

func (l *traceqlLang) ProjectSamples(plan chplan.Node, meta engine.Meta) chplan.Node {
	// Tempo's wrap-projection inspects the inner plan shape
	// (Scan / StructuralJoin / Aggregate / Project) and materialises
	// the canonical (MetricName, Attributes, TimeUnix, Value) tuple.
	// IsTraceByID is threaded through so the Filter(Scan) branch can
	// enrich the Attributes map with the span-detail fields Grafana's
	// trace-view UI consumes (TraceId / SpanId / ParentSpanId /
	// SpanKind / StatusCode + SpanAttributes); the search-path
	// branches use the leaner canonical projection unchanged.
	return wrapWithSampleProjection(plan, l.schema, meta)
}
