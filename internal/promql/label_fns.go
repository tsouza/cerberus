package promql

import (
	"fmt"
	"strings"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
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
		Replacement: promReplacementToCH(replacement),
		Src:         src,
		Regex:       regex,
	}
	return projectAttributesOverInner(inner, s, attrs), nil
}

// promReplacementToCH translates a PromQL `label_replace` replacement
// template (Go-regexp `$1` / `${1}` / `$$` syntax) into the equivalent
// ClickHouse `replaceRegexpOne` replacement (`\1` / `\\` syntax).
//
// Reference Prometheus runs the replacement through Go's
// `regexp.Regexp.ExpandString`, which treats:
//
//   - `$$`            → literal `$`
//   - `$N` / `${N}`   → numbered capture group N
//   - `$name` / `${name}` → named capture group
//
// ClickHouse's `replaceRegexpOne` uses backslash escapes instead:
//
//   - `\\`            → literal backslash
//   - `\0` … `\9`     → numbered capture group (`\0` = whole match)
//
// Without translation, a PromQL replacement like `"svc-$1"` is passed
// to CH verbatim and emitted as the literal string `svc-$1` — the
// capture group is never substituted.
//
// Translation rules implemented here (single-digit captures only — the
// upstream PromQL `funcLabelReplace` doesn't constrain the index but
// CH's backref syntax tops out at `\9`; multi-digit / named captures
// are not used by any test or compatibility fixture and would need a
// separate emit path):
//
//   - Pre-escape every existing `\` in the input to `\\`, so any
//     literal backslash in the PromQL template survives as a literal
//     backslash in CH (and is not re-interpreted as a CH backref by
//     the digits we're about to introduce).
//   - `$$` → `$` (literal dollar).
//   - `$N` for a single ASCII digit (0-9) → `\N`.
//   - `${N}` for a single ASCII digit (0-9) → `\N`.
//   - Any other `$<x>` (including bare `$` at end-of-string, `$<letter>`,
//     `${name}`, `$10` etc.) is preserved verbatim so we don't silently
//     mistranslate a shape we don't fully support.
func promReplacementToCH(repl string) string {
	// First pass: double every literal backslash so CH sees them as
	// "literal backslash" (`\\`) rather than the start of its own
	// backref escape sequence after we splice `\N` in below.
	escaped := strings.ReplaceAll(repl, `\`, `\\`)

	var b strings.Builder
	b.Grow(len(escaped))
	for i := 0; i < len(escaped); i++ {
		c := escaped[i]
		if c != '$' {
			b.WriteByte(c)
			continue
		}
		// Lone `$` at end of string — preserve.
		if i+1 >= len(escaped) {
			b.WriteByte('$')
			continue
		}
		next := escaped[i+1]
		switch {
		case next == '$':
			// `$$` → literal `$`.
			b.WriteByte('$')
			i++
		case next >= '0' && next <= '9':
			// `$N` → `\N` (single digit only — `$10` is preserved
			// verbatim per upstream Go regexp semantics, but CH
			// has no `\10`, so we'd mistranslate either way; preserving
			// keeps the failure visible rather than silently wrong).
			if i+2 < len(escaped) && escaped[i+2] >= '0' && escaped[i+2] <= '9' {
				b.WriteByte('$')
				continue
			}
			b.WriteByte('\\')
			b.WriteByte(next)
			i++
		case next == '{':
			// `${N}` (single digit) → `\N`. Anything else (named
			// captures, multi-digit indices) is preserved verbatim.
			if i+3 < len(escaped) && escaped[i+2] >= '0' && escaped[i+2] <= '9' && escaped[i+3] == '}' {
				b.WriteByte('\\')
				b.WriteByte(escaped[i+2])
				i += 3
				continue
			}
			b.WriteByte('$')
		default:
			// `$<letter>` etc. — preserve verbatim.
			b.WriteByte('$')
		}
	}
	return b.String()
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
