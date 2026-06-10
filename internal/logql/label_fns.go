package logql

import (
	"fmt"

	"github.com/grafana/loki/v3/pkg/logql/syntax"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/qlcommon"
	"github.com/tsouza/cerberus/internal/schema"
)

// lowerLabelReplace handles LogQL's
//
//	label_replace(<sample_expr>, "dst", "replacement", "src", "regex")
//
// Mirrors internal/promql/label_fns.go::lowerLabelReplace. The chplan
// node `chplan.LabelReplace` already exists and does the work; the
// only LogQL-specific bit is that the input column is
// `ResourceAttributes` (LogQL's stream-identity map) rather than
// PromQL's `Attributes`.
//
// Loki's parser fields the call as `*syntax.LabelReplaceExpr` with the
// four string arguments already extracted (`Dst`, `Replacement`, `Src`,
// `Regex`) — no parser-level string-literal re-extraction needed,
// unlike PromQL where the args ride as `parser.StringLiteral`. The
// parser also pre-compiles `Re` and stashes any invalid-regex error
// inside an unexported `err` field; ParseExpr surfaces it before
// lowering reaches us.
//
// The Replacement string is run through qlcommon.ReplacementToCH so
// Go-regexp `$N` / `${N}` backrefs become CH `replaceRegexpOne` `\N`
// backrefs. LogQL's reference replacement engine is Go's
// `regexp.Regexp.ExpandString` (identical to PromQL); without the
// translation a replacement like `"svc-$1"` is emitted to CH verbatim
// and treated as the literal string `svc-$1` instead of a capture
// substitution. See internal/qlcommon/label_replace.go for the rule
// table.
func lowerLabelReplace(e *syntax.LabelReplaceExpr, s schema.Logs, lc lowerCtx) (chplan.Node, error) {
	if e.Left == nil {
		return nil, fmt.Errorf("logql: label_replace has nil inner")
	}

	inner, err := lower(e.Left, s, lc)
	if err != nil {
		return nil, err
	}

	// Resolve which column carries the inner plan's stream identity —
	// a raw RangeWindow exposes `ResourceAttributes`, a vector
	// aggregation (post-wrapVectorAggregateForSample) exposes
	// `Attributes` — and rewrite THAT map. Reading ResourceAttributes
	// unconditionally surfaced as 502 `Unknown expression identifier
	// 'ResourceAttributes'` for `label_replace(sum by (...) (...), …)`.
	cols := logSampleColumns(inner, s)
	attrs := &chplan.LabelReplace{
		Map:              &chplan.ColumnRef{Name: cols.attrsCol},
		Dst:              e.Dst,
		Replacement:      qlcommon.ReplacementToCH(e.Replacement, e.Regex),
		Src:              e.Src,
		Regex:            e.Regex,
		EmptyReplacement: qlcommon.EmptyCapturesReplacement(e.Replacement),
	}
	// Emit the full canonical Sample shape (mirrors
	// projectValueOverLogInner): forwarding the inner's per-anchor
	// timestamp keeps range-mode step grids alive through the wrap,
	// and the Attributes alias makes the output recognisably
	// Sample-shaped for Lang.ProjectSamples / enclosing binops.
	return &chplan.Project{
		Input: inner,
		Projections: []chplan.Projection{
			{Expr: cols.metricName, Alias: "MetricName"},
			{Expr: attrs, Alias: "Attributes"},
			{Expr: cols.timeExpr, Alias: "TimeUnix"},
			{Expr: &chplan.ColumnRef{Name: rangeAggSynthValueColumn}, Alias: rangeAggSynthValueColumn},
		},
	}, nil
}
