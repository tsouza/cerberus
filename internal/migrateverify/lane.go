package migrateverify

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/logql/lsyntax"
	"github.com/tsouza/cerberus/internal/migrate"
	"github.com/tsouza/cerberus/internal/traceql/ast"
)

// Query-shape kinds. These name the SHAPE of a query — what it returns — and are
// distinct from migrate.CorpusQuery.Kind, which names its PROVENANCE
// (record / alert / panel).
const (
	// KindLogStream: a LogQL query that returns log lines, not a metric matrix.
	KindLogStream = "log-stream"
	// KindTraceSearch: a TraceQL query that returns trace summaries, not a metric matrix.
	KindTraceSearch = "trace-search"
	// KindMetricsCompare: a TraceQL compare() query, whose baseline-vs-selection
	// attribute split is not a comparable metric matrix.
	KindMetricsCompare = "metrics-compare"
	// KindUnparseable: the head's own parser rejected the expression, so its
	// shape cannot be classified at all.
	KindUnparseable = "unparseable"
	// KindUnknownLang: the corpus entry names a query language this build has no
	// metric lane for.
	KindUnknownLang = "unknown-lang"
)

// LaneRouting is where one corpus entry belongs. Exactly one of Replayable (the
// entry is replayed on Head's lane) or Kind+Reason (the entry is reported out of
// scope with a specific, operator-readable cause) is populated. There is no third
// outcome: an entry is never dropped.
type LaneRouting struct {
	Head       string
	Replayable bool
	Kind       string
	Reason     string
}

// RouteQuery decides an entry's lane offline, with no network call, using the
// same in-house parsers cerberus's own heads use to pivot their response shape.
// A query this function cannot classify is reported with the parser's verbatim
// error, never guessed into a lane.
//
// An empty lang routes to the PromQL lane: that is the pre-three-headed corpus
// default and mirrors how `migrate explain` resolves an untagged entry.
func RouteQuery(lang, expr string) LaneRouting {
	switch lang {
	case "", migrate.LangPromQL:
		return LaneRouting{Head: HeadProm, Replayable: true}
	case migrate.LangLogQL:
		return routeLogQL(expr)
	case migrate.LangTraceQL:
		return routeTraceQL(expr)
	default:
		return LaneRouting{
			Kind:   KindUnknownLang,
			Reason: fmt.Sprintf("lang=%q: this build has no metric lane for that query language", lang),
		}
	}
}

// routeLogQL splits LogQL into the metric lane (anything that produces a sample
// series) and the out-of-scope log-stream shape.
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
func routeLogQL(expr string) LaneRouting {
	e, err := lsyntax.ParseExprWithoutValidation(lsyntax.NormalizeDottedLabels(expr))
	if err != nil {
		return LaneRouting{
			Head: HeadLoki,
			Kind: KindUnparseable,
			Reason: fmt.Sprintf("lang=logql kind=unparseable: cerberus's LogQL parser rejected this expression (%v), "+
				"so its shape cannot be classified and no metric-lane baseline is definable", err),
		}
	}
	if _, ok := e.(lsyntax.SampleExpr); ok {
		return LaneRouting{Head: HeadLoki, Replayable: true}
	}
	return LaneRouting{
		Head: HeadLoki,
		Kind: KindLogStream,
		Reason: "lang=logql kind=log-stream: this query returns log lines, not a metric matrix; " +
			"log-stream parity is not part of the metric-lane gate",
	}
}

// routeTraceQL splits TraceQL into the metrics lane (the scalar/bucket
// first-stage aggregates, whose results are genuine labelled matrices) and the
// two out-of-scope shapes: a bare spanset search, and compare().
func routeTraceQL(expr string) LaneRouting {
	root, err := ast.Parse(expr)
	if err != nil {
		return LaneRouting{
			Head: HeadTempo,
			Kind: KindUnparseable,
			Reason: fmt.Sprintf("lang=traceql kind=unparseable: cerberus's TraceQL parser rejected this expression (%v), "+
				"so its shape cannot be classified and no metric-lane baseline is definable", err),
		}
	}
	if root.MetricsPipeline == nil && root.MetricsSecondStage == nil {
		return LaneRouting{
			Head: HeadTempo,
			Kind: KindTraceSearch,
			Reason: "lang=traceql kind=trace-search: this query returns trace summaries, not a metric matrix; " +
				"trace-search parity is not part of the metric-lane gate",
		}
	}
	if _, ok := root.MetricsPipeline.(*ast.MetricsCompare); ok {
		return LaneRouting{
			Head: HeadTempo,
			Kind: KindMetricsCompare,
			Reason: "lang=traceql kind=metrics-compare: compare() emits a baseline-vs-selection attribute split " +
				"keyed by __meta_type whose series inventory depends on topN, not a comparable metric matrix",
		}
	}
	return LaneRouting{Head: HeadTempo, Replayable: true}
}
