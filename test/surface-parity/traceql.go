package surfaceparity

// Domain-aware TraceQL operands from the showcase seed
// (test/e2e/seed/cmd/seed/showcase_traceql.go): real resource/span
// attributes that match seeded spans.
const (
	tqlServiceName = `resource.service.name = "gateway"`
)

// tqlProbe is one TraceQL grammar symbol + its canonical probe.
type tqlProbe struct {
	symbol string
	kind   string
	probe  string
}

// tqlIntrinsicProbes enumerates the TraceQL intrinsic surface. Each
// probe is a spanset filter that references the intrinsic in a
// comparison whose RHS is type-appropriate. The scoped string form
// (e.g. "span:duration", "trace:id") is the parser's canonical token.
// Intrinsics the upstream parser marks structural-only or
// not-yet-implemented are still probed — the reference oracle
// (Parse+Validate) is what decides whether they're reference-accepted,
// so a structural-only intrinsic naturally lands parity-reject rather
// than being hand-excluded.
var tqlIntrinsicProbes = []tqlProbe{
	{"intrinsic:duration", kindIntrinsic, `{ duration > 1ms }`},
	{"intrinsic:name", kindIntrinsic, `{ name = "charge" }`},
	{"intrinsic:status", kindIntrinsic, `{ status = error }`},
	{"intrinsic:statusMessage", kindIntrinsic, `{ statusMessage = "card declined" }`},
	{"intrinsic:kind", kindIntrinsic, `{ kind = server }`},
	{"intrinsic:span:childCount", kindIntrinsic, `{ span:childCount > 0 }`},
	{"intrinsic:rootServiceName", kindIntrinsic, `{ rootServiceName = "gateway" }`},
	{"intrinsic:rootName", kindIntrinsic, `{ rootName = "request" }`},
	{"intrinsic:traceDuration", kindIntrinsic, `{ traceDuration > 1ms }`},
	{"intrinsic:nestedSetLeft", kindIntrinsic, `{ nestedSetLeft > 0 }`},
	{"intrinsic:nestedSetRight", kindIntrinsic, `{ nestedSetRight > 0 }`},
	{"intrinsic:nestedSetParent", kindIntrinsic, `{ nestedSetParent > 0 }`},
	{"intrinsic:event:name", kindIntrinsic, `{ event:name = "exception" }`},
	{"intrinsic:event:timeSinceStart", kindIntrinsic, `{ event:timeSinceStart > 1ms }`},
	{"intrinsic:link:spanID", kindIntrinsic, `{ link:spanID != "" }`},
	{"intrinsic:link:traceID", kindIntrinsic, `{ link:traceID != "" }`},
	{"intrinsic:instrumentation:name", kindIntrinsic, `{ instrumentation:name = "showcase-instrumentation" }`},
	{"intrinsic:instrumentation:version", kindIntrinsic, `{ instrumentation:version = "1.2.3" }`},
	{"intrinsic:trace:id", kindIntrinsic, `{ trace:id != "" }`},
	{"intrinsic:span:id", kindIntrinsic, `{ span:id != "" }`},
	{"intrinsic:span:parentID", kindIntrinsic, `{ span:parentID != "" }`},
	{"intrinsic:span:status", kindIntrinsic, `{ span:status = error }`},
	{"intrinsic:span:statusMessage", kindIntrinsic, `{ span:statusMessage = "card declined" }`},
	{"intrinsic:span:duration", kindIntrinsic, `{ span:duration > 1ms }`},
	{"intrinsic:span:name", kindIntrinsic, `{ span:name = "charge" }`},
	{"intrinsic:span:kind", kindIntrinsic, `{ span:kind = server }`},
	{"intrinsic:trace:rootName", kindIntrinsic, `{ trace:rootName = "request" }`},
	{"intrinsic:trace:rootService", kindIntrinsic, `{ trace:rootService = "gateway" }`},
	{"intrinsic:trace:duration", kindIntrinsic, `{ trace:duration > 1ms }`},
}

// tqlMetricsProbes enumerate the TraceQL metrics second-stage operator
// surface. These are metrics-pipeline expressions (a spanset filter
// piped into a metrics aggregation), the surface /api/metrics serves.
var tqlMetricsProbes = []tqlProbe{
	{"metric:rate", kindMetricsOp, `{ ` + tqlServiceName + ` } | rate()`},
	{"metric:count_over_time", kindMetricsOp, `{ ` + tqlServiceName + ` } | count_over_time()`},
	{"metric:min_over_time", kindMetricsOp, `{ ` + tqlServiceName + ` } | min_over_time(duration)`},
	{"metric:max_over_time", kindMetricsOp, `{ ` + tqlServiceName + ` } | max_over_time(duration)`},
	{"metric:avg_over_time", kindMetricsOp, `{ ` + tqlServiceName + ` } | avg_over_time(duration)`},
	{"metric:sum_over_time", kindMetricsOp, `{ ` + tqlServiceName + ` } | sum_over_time(duration)`},
	{"metric:quantile_over_time", kindMetricsOp, `{ ` + tqlServiceName + ` } | quantile_over_time(duration, 0.9)`},
	{"metric:histogram_over_time", kindMetricsOp, `{ ` + tqlServiceName + ` } | histogram_over_time(duration)`},
	{"metric:compare", kindMetricsOp, `{ ` + tqlServiceName + ` } | compare({ status = error })`},
	{"metric:topk", kindMetricsOp, `{ ` + tqlServiceName + ` } | rate() by (name) | topk(3)`},
	{"metric:bottomk", kindMetricsOp, `{ ` + tqlServiceName + ` } | rate() by (name) | bottomk(3)`},
}

// tqlAggregateProbes enumerate the first-stage spanset aggregates
// (count/min/max/sum/avg) used in scalar-filter spanset pipelines.
var tqlAggregateProbes = []tqlProbe{
	{"aggregate:count", "aggregate", `{ ` + tqlServiceName + ` } | count() > 0`},
	{"aggregate:max", "aggregate", `{ ` + tqlServiceName + ` } | max(duration) > 1ms`},
	{"aggregate:min", "aggregate", `{ ` + tqlServiceName + ` } | min(duration) > 1ms`},
	{"aggregate:sum", "aggregate", `{ ` + tqlServiceName + ` } | sum(duration) > 1ms`},
	{"aggregate:avg", "aggregate", `{ ` + tqlServiceName + ` } | avg(duration) > 1ms`},
}

// probeTraceQL enumerates the TraceQL intrinsic + metrics-op + aggregate
// surface and classifies each symbol against cerberus + the reference
// verdict read from the pinned artifact
// (traceql-reference-verdicts.json).
//
// The reference verdict used to be a live in-process
// tempotraceql.Parse + tempotraceql.Validate call against upstream
// Tempo's AGPL traceql package. That worked (no compat container needed
// — parse+validate is data-independent) but meant this file — reached
// unconditionally by TestInventoryIsRegenerable and
// `just update-parity-ledgers`, both part of the default `check` gate —
// imported an AGPL package with no build tag. It now reads the frozen
// verdict the same live call produced, mirroring logql.go's
// logql_reference.go split. See traceql_reference.go +
// traceql_reference_agpl_test.go (the agpl_oracle-tagged
// regeneration/drift-check counterpart) and #1520.
func probeTraceQL() ([]Entry, error) {
	ref, err := loadTraceQLReferenceVerdicts()
	if err != nil {
		return nil, err
	}

	var entries []Entry
	all := make([]tqlProbe, 0, len(tqlIntrinsicProbes)+len(tqlMetricsProbes)+len(tqlAggregateProbes))
	all = append(all, tqlIntrinsicProbes...)
	all = append(all, tqlMetricsProbes...)
	all = append(all, tqlAggregateProbes...)
	for _, p := range all {
		cv, cerr := cerberusVerdictTraceQL(p.probe)
		rv, rerr := ref.verdict(p.symbol)
		if rerr != nil {
			return nil, rerr
		}
		entries = append(entries, Entry{
			Head:          "traceql",
			Symbol:        p.symbol,
			Kind:          p.kind,
			Probe:         p.probe,
			Cerberus:      cv,
			Reference:     rv,
			Class:         classify(cv, rv),
			CerberusError: cerr,
		})
	}
	return entries, nil
}
