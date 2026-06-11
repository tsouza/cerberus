package surfaceparity

import (
	"context"
	"time"

	tempotraceql "github.com/grafana/tempo/pkg/traceql"
	promparser "github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/logql"
	"github.com/tsouza/cerberus/internal/optimizer"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/traceql"
)

// probeStart / probeEnd anchor the instant/range window used when
// lowering probe expressions. A fixed deterministic window keeps the
// generated SQL — and therefore the accept/reject verdict — stable
// across runs.
var (
	probeStart = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	probeEnd   = probeStart.Add(5 * time.Minute)
	probeStep  = 30 * time.Second
)

// cerberusVerdictPromQL runs one PromQL probe through the real cerberus
// path the /api/v1/query_range handler uses — parse → lower (fold
// included) → optimize → emit — and returns ACCEPT with empty error, or
// REJECT with the failure. EnableExperimentalFunctions mirrors the
// parser options the prom Lang adapter uses so the parse stage never
// gates experimental fns out before cerberus's own lowering can have an
// opinion.
func cerberusVerdictPromQL(query string) (Verdict, string) {
	p := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(query)
	if err != nil {
		// A probe that cerberus's own parser can't parse is a probe-
		// synthesis bug, surfaced as a reject with a tagged message so
		// it's distinguishable from a lowering rejection.
		return VerdictReject, "probe-parse: " + err.Error()
	}
	plan, err := promql.LowerAtRange(context.Background(), expr, schema.DefaultOTelMetrics(), probeStart, probeEnd, probeStep)
	if err != nil {
		return VerdictReject, err.Error()
	}
	return emitVerdict(optimizer.DefaultWithSchema(schema.DefaultOTelMetrics()), plan)
}

// cerberusVerdictLogQL runs one LogQL probe through the path the Loki
// range handler uses — ParseExprPermissive → lower → optimize → emit.
func cerberusVerdictLogQL(query string) (Verdict, string) {
	expr, err := logql.ParseExprPermissive(query)
	if err != nil {
		return VerdictReject, "probe-parse: " + err.Error()
	}
	plan, err := logql.LowerAtRange(context.Background(), expr, schema.DefaultOTelLogs(), probeStart, probeEnd, probeStep)
	if err != nil {
		return VerdictReject, err.Error()
	}
	return emitVerdict(optimizer.Default(), plan)
}

// cerberusVerdictTraceQL runs one TraceQL probe through the path the
// tempo handler uses — traceql.Parse → lower → optimize → emit.
func cerberusVerdictTraceQL(query string) (Verdict, string) {
	expr, err := tempotraceql.Parse(query)
	if err != nil {
		return VerdictReject, "probe-parse: " + err.Error()
	}
	plan, err := traceql.Lower(context.Background(), expr, schema.DefaultOTelTraces())
	if err != nil {
		return VerdictReject, err.Error()
	}
	return emitVerdict(optimizer.Default(), plan)
}

// emitVerdict runs the optimizer + SQL emitter over a lowered plan,
// completing the wire pipeline. A symbol that lowers but fails to emit
// is still a cerberus rejection from the caller's point of view (the
// HTTP request 500s/422s), so emit failure is a REJECT.
func emitVerdict(driver *optimizer.Driver, plan chplan.Node) (Verdict, string) {
	ctx := context.Background()
	plan = driver.Run(ctx, plan)
	if _, _, err := chsql.Emit(ctx, plan); err != nil {
		return VerdictReject, "emit: " + err.Error()
	}
	return VerdictAccept, ""
}
