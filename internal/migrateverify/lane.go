package migrateverify

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/logql/lsyntax"
	"github.com/tsouza/cerberus/internal/migrate"
	"github.com/tsouza/cerberus/internal/traceql/ast"
)

// LaneRouting is where one corpus entry belongs. Kind always names the entry's
// result SHAPE; Replayable says whether a comparator exists for that shape. A
// replayable entry carries Kind and no Reason (the shape selects its
// comparator); an entry with no comparator carries Kind and the specific,
// operator-readable Reason it was not judged. There is no third outcome: an
// entry is never dropped.
type LaneRouting struct {
	Head       string
	Replayable bool
	Kind       string
	Reason     string
}

// RouteQuery decides an entry's lane and result shape offline, with no network
// call, using the same in-house parsers cerberus's own heads use to pivot their
// response shape. A query this function cannot classify is reported with the
// parser's verbatim error, never guessed into a lane.
//
// An empty lang routes to the PromQL lane: that is the pre-three-headed corpus
// default and mirrors how `migrate explain` resolves an untagged entry.
func RouteQuery(lang, expr string) LaneRouting {
	switch lang {
	case "", migrate.LangPromQL:
		return LaneRouting{Head: HeadProm, Replayable: true, Kind: KindMetricMatrix}
	case migrate.LangLogQL:
		return routeLogQL(expr)
	case migrate.LangTraceQL:
		return routeTraceQL(expr)
	default:
		return LaneRouting{
			Kind: KindUnknownLang,
			Reason: fmt.Sprintf("lang=%q: this build has no parity lane for that query language, "+
				"so its result shape cannot be classified and no comparator can be selected for it", lang),
		}
	}
}

// routeLogQL splits LogQL into its two result shapes: the metric matrix
// (anything that produces a sample series) and the log stream (a selector, with
// or without pipeline stages, that returns log lines). Both have a comparator,
// so both are replayed.
//
// It parses with ParseExprWithoutValidation and the same dotted-label
// normalisation cerberus's Loki head applies, so classification can never reject
// an expression cerberus would happily serve: validation only enforces the
// "at least one non-empty matcher" rule, which cerberus's own permissive parse
// already relaxes, and an unnormalised `{service.name="api"}` — a routine OTel
// panel — would otherwise fail to parse and be misrouted as unparseable.
//
// The metric test is the SampleExpr assertion, which is exactly the predicate the
// Loki head uses to decide between a matrix and a streams response.
//
// No pipeline shape is excluded from the log-stream lane. line_format /
// decolorize / unpack make the compared LINE a derived string, and label_format /
// drop / keep / pattern make the compared LABEL SET post-transform; both are
// still exactly comparable, and none of them adds or removes an entry.
func routeLogQL(expr string) LaneRouting {
	e, err := lsyntax.ParseExprWithoutValidation(lsyntax.NormalizeDottedLabels(expr))
	if err != nil {
		return LaneRouting{
			Head: HeadLoki,
			Kind: KindUnparseable,
			Reason: fmt.Sprintf("lang=logql kind=unparseable: cerberus's LogQL parser rejected this expression (%v), "+
				"so its result shape cannot be classified and no comparator can be selected for it", err),
		}
	}
	if _, ok := e.(lsyntax.SampleExpr); ok {
		return LaneRouting{Head: HeadLoki, Replayable: true, Kind: KindMetricMatrix}
	}
	return LaneRouting{Head: HeadLoki, Replayable: true, Kind: KindLogStream}
}

// routeTraceQL splits TraceQL into the metrics lane (the scalar/bucket
// first-stage aggregates, whose results are genuine labelled matrices), the
// trace-search shape, and compare().
func routeTraceQL(expr string) LaneRouting {
	root, err := ast.Parse(expr)
	if err != nil {
		return LaneRouting{
			Head: HeadTempo,
			Kind: KindUnparseable,
			Reason: fmt.Sprintf("lang=traceql kind=unparseable: cerberus's TraceQL parser rejected this expression (%v), "+
				"so its result shape cannot be classified and no comparator can be selected for it", err),
		}
	}
	if root.MetricsPipeline == nil && root.MetricsSecondStage == nil {
		return LaneRouting{
			Head: HeadTempo,
			Kind: KindTraceSearch,
			Reason: "lang=traceql kind=trace-search: this query returns trace summaries, whose result order is a " +
				"relevance ranking neither backend's wire contract fixes; no comparator is registered for that shape",
		}
	}
	if _, ok := root.MetricsPipeline.(*ast.MetricsCompare); ok {
		return LaneRouting{
			Head: HeadTempo,
			Kind: KindMetricsCompare,
			Reason: "lang=traceql kind=metrics-compare: compare() emits a baseline-vs-selection attribute split " +
				"keyed by __meta_type whose series inventory is chosen by a topN ranking neither backend's wire " +
				"contract specifies. It is neither a comparable metric matrix nor a comparable set: two correct " +
				"backends can select different attributes, and the gate has no rule that would tell that apart " +
				"from a defect",
		}
	}
	return LaneRouting{Head: HeadTempo, Replayable: true, Kind: KindMetricMatrix}
}
