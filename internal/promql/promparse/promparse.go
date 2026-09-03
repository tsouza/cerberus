// Package promparse owns the one PromQL parser configuration cerberus parses
// with.
//
// Every non-test construction of a Prometheus parser in this repository goes
// through [New]. The ten production sites that used to spell the options inline
// — the HTTP handler, both offline explain langs, the migration rule graph and
// the six bench-report harnesses — held one policy in ten statements with
// nothing keeping them in agreement, and one of those options is load-bearing
// (see [New]). test/regression's
// TestPromQLParserOptionsHaveASingleSource fails on any non-test file that
// constructs a parser directly, so an eleventh site cannot quietly choose
// different options.
package promparse

import "github.com/prometheus/prometheus/promql/parser"

// New returns the PromQL parser cerberus parses every query with.
//
// EnableExperimentalFunctions is ON. The upstream grammar gates a set of
// functions behind it, and cerberus lowers members of that set, so parsing
// without it would reject queries the engine can answer.
//
// The other three [parser.Options] fields are OFF, and that is a decision with
// a consumer rather than a default nobody revisited.
//
// ExperimentalDurationExpr is the load-bearing one. The grammar populates
// MatrixSelector.Range from a *NumberLiteral only; a duration EXPRESSION
// (`[1m+1m]`, `[2m-2m]`, `[step()]`) parses to a ZERO Range with the value
// moved into MatrixSelector.RangeExpr instead. With the option unset the
// grammar records "experimental duration expression is not enabled" and
// ParseExpr returns an error, so a zero Range never reaches the engine.
//
// The histogram-quantile lowerings rest on exactly that. Cerberus binds the
// window from internal/promql/histogram_quantile.go:`windowRange: ms.Range`,
// and both lowerings then derive the window's left bound and its staleness
// lower bound from it with NO positivity guard —
// internal/promql/histogram_quantile.go:lowerHistogramQuantileAgg:`rangeStart := windowLeftBoundExpr(anchor, shape.windowRange)`
// and its native sibling lowerHistogramQuantileNativeAgg.
// Those guards were deleted in #2970 on the proof that the field is always
// positive, and this option being off is what carries the proof. Enabling it
// re-opens a degenerate window: a zero-width `TimeUnix` range on the classic
// path, and a scan with no lower time bound at all once the staleness
// predicate is dropped. internal/promql's
// TestPromQLGrammarRefusesNonPositiveMatrixRange parses through THIS function
// and turns red the moment the option flips.
//
// EnableExtendedRangeSelectors and EnableBinopFillModifiers are off for a
// weaker but still concrete reason: the lowering has no arm for the syntax
// they admit. `VectorSelector.Anchored` is read nowhere in internal/promql —
// only internal/api/prom/parse_query_ast.go:`"anchored":   vs.Anchored,`
// echoes it back on the AST-rendering endpoint — so a query carrying one of
// those modifiers would parse and then lower as though the modifier were
// absent. Enabling either is a lowering change, not a parser flag.
func New() parser.Parser {
	return parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
}
