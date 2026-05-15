package promql

import (
	"fmt"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/qlcommon"
	"github.com/tsouza/cerberus/internal/schema"
)

// lowerLabelReplace lowers
//
//	label_replace(v, dst, replacement, src, regex)
//
// into a Project that rewrites the Attributes Map(String, String) via a
// chplan.LabelReplace expression. PromQL's parser already validates the
// arg shape (5 args; arg 0 is an instant-vector; args 1..4 are
// StringLiterals), but the parser's static check lives behind
// `parser.Check` which `parser.ParseExpr` does not run on lone Calls in
// every code path — so we re-assert the StringLiteral shape here with
// clear errors instead of panicking on a bad cast.
func lowerLabelReplace(c *parser.Call, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if len(c.Args) != 5 {
		return nil, fmt.Errorf("promql: label_replace expects 5 arguments, got %d", len(c.Args))
	}
	dst, err := stringArg(c.Args[1], "label_replace", "dst_label")
	if err != nil {
		return nil, err
	}
	replacement, err := stringArg(c.Args[2], "label_replace", "replacement")
	if err != nil {
		return nil, err
	}
	src, err := stringArg(c.Args[3], "label_replace", "src_label")
	if err != nil {
		return nil, err
	}
	regex, err := stringArg(c.Args[4], "label_replace", "regex")
	if err != nil {
		return nil, err
	}

	inner, err := lower(c.Args[0], s, ctx)
	if err != nil {
		return nil, err
	}
	attrs := &chplan.LabelReplace{
		Map:         &chplan.ColumnRef{Name: s.AttributesColumn},
		Dst:         dst,
		Replacement: qlcommon.ReplacementToCH(replacement, regex),
		Src:         src,
		Regex:       regex,
	}
	return projectAttributesOverInner(inner, s, attrs), nil
}

// lowerLabelJoin lowers
//
//	label_join(v, dst, separator, src1, src2, ...)
//
// into a Project that rewrites Attributes via a chplan.LabelJoin
// expression. PromQL allows zero src labels (the parser flags this as
// `Variadic: -1` on top of the 4 fixed args); the spec then says the
// joined value is the empty string, which our emit path drops via the
// outer mapFilter — leaving the dst label absent on the wire.
func lowerLabelJoin(c *parser.Call, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if len(c.Args) < 3 {
		return nil, fmt.Errorf("promql: label_join expects at least 3 arguments (v, dst, separator[, src…]), got %d", len(c.Args))
	}
	dst, err := stringArg(c.Args[1], "label_join", "dst_label")
	if err != nil {
		return nil, err
	}
	separator, err := stringArg(c.Args[2], "label_join", "separator")
	if err != nil {
		return nil, err
	}
	srcs := make([]string, 0, len(c.Args)-3)
	for i := 3; i < len(c.Args); i++ {
		src, err := stringArg(c.Args[i], "label_join", fmt.Sprintf("src_label_%d", i-2))
		if err != nil {
			return nil, err
		}
		srcs = append(srcs, src)
	}

	inner, err := lower(c.Args[0], s, ctx)
	if err != nil {
		return nil, err
	}
	attrs := &chplan.LabelJoin{
		Map:       &chplan.ColumnRef{Name: s.AttributesColumn},
		Dst:       dst,
		Separator: separator,
		Srcs:      srcs,
	}
	return projectAttributesOverInner(inner, s, attrs), nil
}

// stringArg extracts a static string literal from a Call argument,
// returning a clear error pointing at the call and parameter when the
// argument isn't a parser.StringLiteral. PromQL's parser only accepts
// static strings for these positions, so this guards against an
// upstream parser shape change without panicking.
func stringArg(e parser.Expr, fnName, paramName string) (string, error) {
	sl, ok := e.(*parser.StringLiteral)
	if !ok {
		return "", fmt.Errorf("promql: %s(%s) requires a string literal, got %T", fnName, paramName, e)
	}
	return sl.Val, nil
}

// projectAttributesOverInner wraps inner with a Project that keeps every
// other column and replaces only Attributes with the new attrs
// expression. Mirrors projectValueOverInner (instant_fns.go) but
// targets the Attributes column instead of Value.
//
// When inner is a RangeWindow, MetricName / TimeUnix don't survive the
// windowed groupArray and the projection lists only Attributes + Value.
// Every other inner shape (Scan / Filter / Project / Aggregate / LWR)
// keeps the full Sample-row schema, so we forward all four canonical
// columns.
func projectAttributesOverInner(inner chplan.Node, s schema.Metrics, attrs chplan.Expr) chplan.Node {
	if _, ok := inner.(*chplan.RangeWindow); ok {
		return &chplan.Project{
			Input: inner,
			Projections: []chplan.Projection{
				{Expr: attrs, Alias: s.AttributesColumn},
				{Expr: &chplan.ColumnRef{Name: s.ValueColumn}},
			},
		}
	}
	return &chplan.Project{
		Input: inner,
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: s.MetricNameColumn}},
			{Expr: attrs, Alias: s.AttributesColumn},
			{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}},
			{Expr: &chplan.ColumnRef{Name: s.ValueColumn}},
		},
	}
}
