package tempo

import (
	"context"
	"errors"
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

// This file owns the ONE answer to the question every Tempo metrics
// entrypoint asks of a lowered TraceQL plan: "what metrics pipeline is at
// this plan root, if any?"
//
// The Tempo head has five request entrypoints that ask it — the HTTP
// handlers in metrics_query_range.go and metrics_query_instant.go, the two
// StreamingQuerier exports in grpc_exports.go, and the offline explain
// adapter in explain.go. Each used to answer it by hand, with its own
// ladder of unwrap calls and its own copy of the accepted-forms sentence,
// so a plan kind one entrypoint knew about was silently dropped by the
// others: `| histogram_over_time(...)` rendered over
// /api/metrics/query_range and was rejected as "not a TraceQL
// metrics-pipeline expression" by /api/metrics/query, the gRPC
// MetricsQueryInstant RPC and the explain preview (#1484). Four earlier
// instances of the same class (#1435, #1487, #1593, #1626) were each closed
// one site at a time.
//
// classifyMetricsPipeline is now the only place the question is answered,
// and metricsPipelineRouter is the exhaustive per-kind contract every
// entrypoint implements. The router is an INTERFACE on purpose: a future
// plan kind adds a method to it, and Go then refuses to compile every
// entrypoint that has not grown the new branch. An enum-plus-switch would
// have compiled fine and degraded silently, which is exactly the failure
// this file exists to make unrepresentable.

// errNotMetricsPipeline is the sentinel behind "this query is well-formed
// TraceQL, but it is not the KIND of query a metrics endpoint evaluates".
// It is classified separately from errLowerStage (see ClassifyErr) because
// the two are different facts: a lowering rejection is a query cerberus
// cannot evaluate anywhere, while this one is a query sent to the wrong
// endpoint — `{ span.http.status_code = 500 }` is a perfectly good search.
var errNotMetricsPipeline = errors.New("not a TraceQL metrics-pipeline expression")

// metricsSurface names the entrypoint a rejection message is written for.
// It is the ONLY part of that message an entrypoint chooses; the
// accepted-forms sentence is shared, so the two cannot drift apart the way
// the five hand-written copies did.
type metricsSurface string

const (
	surfaceMetricsRangeHTTP   metricsSurface = "/api/metrics/query_range"
	surfaceMetricsInstantHTTP metricsSurface = "/api/metrics/query"
	surfaceMetricsRangeGRPC   metricsSurface = "MetricsQueryRange"
	surfaceMetricsInstantGRPC metricsSurface = "MetricsQueryInstant"
	surfaceMetricsExplain     metricsSurface = "the offline metrics preview"
)

// acceptedMetricsPipelineForms is the single spelling of "what a metrics
// endpoint accepts", quoted verbatim into every entrypoint's rejection. It
// must list exactly the pipeline forms metricsPipelineRouter has a method
// for — adding a router method without extending this sentence tells the
// caller a form is unsupported while the code happily evaluates it.
const acceptedMetricsPipelineForms = "`| rate()`, `| count_over_time()`, `| *_over_time(...)`, " +
	"`| quantile_over_time(...)`, `| histogram_over_time(...)` or `| compare({...}, N)`"

// metricsPipelineKind identifies which plan kind classifyMetricsPipeline
// found. It exists so metricsPipeline.Route can dispatch without
// re-deriving the answer from nil-pointer checks; the compile-time
// exhaustiveness guarantee comes from metricsPipelineRouter, not from this
// enum.
type metricsPipelineKind int

const (
	// metricsPipelineUnclassified is the zero value: a metricsPipeline that
	// never came out of classifyMetricsPipeline. Routing one is a caller
	// control-flow bug, not a client error.
	metricsPipelineUnclassified metricsPipelineKind = iota
	metricsPipelineScalar
	metricsPipelineHistogram
	metricsPipelineCompare
)

// metricsPipelineRouter is the exhaustive per-kind contract every Tempo
// metrics entrypoint implements. One method per metrics-pipeline plan kind
// cerberus can evaluate; metricsPipeline.Route calls exactly one of them.
//
// Adding a plan kind means adding a method here, which breaks compilation
// at EVERY implementation — the four transports plus the offline explain
// adapter — instead of letting four of them keep answering "not a metrics
// pipeline" for a shape the fifth already renders.
type metricsPipelineRouter interface {
	// Scalar evaluates the scalar-valued aggregates: rate,
	// count_over_time, the *_over_time family and quantile_over_time.
	Scalar(context.Context, *chplan.MetricsAggregate) error
	// Histogram evaluates `| histogram_over_time(<attr>)`, whose per-anchor
	// value is a distribution rather than a scalar (one series per
	// (group, __bucket)).
	Histogram(context.Context, *chplan.MetricsHistogramOverTime) error
	// Compare evaluates `| compare({...}, topN)`, whose output is the
	// baseline-vs-selection attribute split under the __meta_type scheme.
	Compare(context.Context, *chplan.MetricsCompare) error
}

// metricsPipeline is the classified form of a lowered TraceQL metrics
// query: which pipeline kind sits at the plan root, the plan node that
// carries it, and the second-stage chain wrapped around it.
//
// Construct one only through classifyMetricsPipeline. The kind-specific
// nodes are unexported so no caller can reach past Route and re-derive the
// per-kind branch by hand, which is how the five copies grew in the first
// place.
type metricsPipeline struct {
	// Inner is the lowered plan with the MetricsSecondStage chain peeled
	// off — the node the range-window wrap must sit directly on top of, so
	// the per-step reducer sees the fanout grid.
	Inner chplan.Node
	// Stages is that peeled chain, outermost first (the order the lowering
	// wrapped them, i.e. reverse source order).
	Stages []*chplan.MetricsSecondStage

	kind      metricsPipelineKind
	scalar    *chplan.MetricsAggregate
	histogram *chplan.MetricsHistogramOverTime
	compare   *chplan.MetricsCompare
}

// classifyMetricsPipeline peels the second-stage chain off a lowered plan
// and identifies the metrics pipeline underneath it. surface and query only
// shape the rejection: they name the endpoint the caller used and echo the
// query text back, exactly as the five hand-written rejections did.
//
// The returned error wraps errNotMetricsPipeline, so every transport
// encodes it through the one classification in errclass.go rather than
// picking a status per site.
func classifyMetricsPipeline(plan chplan.Node, surface metricsSurface, query string) (metricsPipeline, error) {
	stages, inner := peelMetricsSecondStages(plan)
	p := metricsPipeline{Inner: inner, Stages: stages}
	if m, ok := unwrapMetricsAggregate(inner); ok {
		p.kind, p.scalar = metricsPipelineScalar, m
		return p, nil
	}
	if m, ok := unwrapMetricsHistogram(inner); ok {
		p.kind, p.histogram = metricsPipelineHistogram, m
		return p, nil
	}
	if m, ok := unwrapMetricsCompare(inner); ok {
		p.kind, p.compare = metricsPipelineCompare, m
		return p, nil
	}
	return metricsPipeline{}, fmt.Errorf("query %q is %w — %s requires %s",
		query, errNotMetricsPipeline, surface, acceptedMetricsPipelineForms)
}

// Route validates the second-stage chain against the pipeline kind and
// then hands the pipeline to exactly one router method.
//
// The validation lives here rather than in the entrypoints for the same
// reason the classification does: `| topk(N)` over histogram_over_time is
// unsupported for a reason that belongs to the plan kind (there is no
// scalar Value to rank), not to the transport that received it, and four
// entrypoints each carried their own copy of the check.
func (p metricsPipeline) Route(ctx context.Context, r metricsPipelineRouter) error {
	switch p.kind {
	case metricsPipelineScalar:
		if len(p.Stages) > 0 && p.scalar.Op == chplan.MetricsOpQuantileOverTime {
			// The quantile matrix rows are (group, anchor, bucket, count)
			// tuples that only become per-series scalars after the Go-side
			// Log2QuantileWithBucket fold — an SQL-side LIMIT BY / WHERE on
			// Value would rank bucket counts, not quantiles.
			return p.rejectSecondStage("quantile_over_time",
				"quantiles are computed from bucket rows after SQL execution")
		}
		return r.Scalar(ctx, p.scalar)
	case metricsPipelineHistogram:
		if len(p.Stages) > 0 {
			return p.rejectSecondStage("histogram_over_time",
				"the per-bucket distribution rows have no scalar Value to rank or threshold")
		}
		return r.Histogram(ctx, p.histogram)
	case metricsPipelineCompare:
		if len(p.Stages) > 0 {
			return p.rejectSecondStage("compare()",
				"compare() series carry the __meta_type split, not a scalar Value to rank or threshold")
		}
		return r.Compare(ctx, p.compare)
	case metricsPipelineUnclassified:
	}
	return fmt.Errorf("%w: metrics pipeline routed before classification", errLowerStage)
}

// rejectSecondStage renders the "second-stage X over Y is unsupported"
// rejection, naming the outermost stage the caller wrote. It wraps
// errLowerStage: the query is well-formed and IS a metrics pipeline, it
// just combines two things cerberus cannot evaluate together.
func (p metricsPipeline) rejectSecondStage(form, reason string) error {
	return fmt.Errorf("%w: traceql: second-stage %s over %s is unsupported — %s",
		errLowerStage, p.Stages[0].Op, form, reason)
}
