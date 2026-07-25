package migrateverify

import (
	"sort"

	"github.com/tsouza/cerberus/internal/logql/lsyntax"
	"github.com/tsouza/cerberus/internal/migrate"
	"github.com/tsouza/cerberus/internal/traceql/ast"
)

// discoverySourceTag is the Source every tag/label discovery probe carries.
// These probes are corpus-anchored (see discoveryProbesForCorpus) rather than
// harvested from any single rule or dashboard, so there is no per-entry
// provenance to name — "derived:discovery" says plainly where they came from
// without inventing one.
const discoverySourceTag = "derived:discovery"

// discoveryProbesForCorpus builds the tag/label discovery probes a corpus's
// own queries anchor: one unfiltered tag/label-NAME enumeration per surface
// for every head the corpus routed at least one replayable query to, plus one
// per-key tag/label-VALUE probe for every distinct label/attribute key that
// head's queries reference.
//
// Discovery is corpus-anchored rather than unconditional so a corpus with no
// Loki or Tempo entries adds no Loki or Tempo probes at all — a PromQL-only
// invocation stays byte-identical to a build that never had a discovery lane.
func discoveryProbesForCorpus(queries []Query) []Query {
	lokiKeys := map[string]struct{}{}
	tempoKeys := map[string]struct{}{}
	var hasLoki, hasTempo bool
	for _, q := range queries {
		switch q.Head {
		case HeadLoki:
			hasLoki = true
			for _, k := range logqlMatcherKeys(q.Expr) {
				lokiKeys[k] = struct{}{}
			}
		case HeadTempo:
			hasTempo = true
			for _, k := range traceqlAttributeKeys(q.Expr) {
				tempoKeys[k] = struct{}{}
			}
		}
	}

	var out []Query
	if hasLoki {
		out = append(out, Query{
			Head: HeadLoki, Lang: migrate.LangLogQL, Kind: KindTagDiscovery,
			Surface: SurfaceLokiLabels, Source: discoverySourceTag,
		})
		for _, k := range sortedStringSet(lokiKeys) {
			out = append(out, Query{
				Head: HeadLoki, Lang: migrate.LangLogQL, Kind: KindTagDiscovery,
				Surface: SurfaceLokiLabelValues, TagName: k, Source: discoverySourceTag,
			})
		}
	}
	if hasTempo {
		out = append(
			out,
			Query{Head: HeadTempo, Lang: migrate.LangTraceQL, Kind: KindTagDiscovery, Surface: SurfaceTempoTagsV1, Source: discoverySourceTag},
			Query{Head: HeadTempo, Lang: migrate.LangTraceQL, Kind: KindTagDiscovery, Surface: SurfaceTempoTagsV2, Source: discoverySourceTag},
		)
		for _, k := range sortedStringSet(tempoKeys) {
			out = append(
				out,
				Query{Head: HeadTempo, Lang: migrate.LangTraceQL, Kind: KindTagDiscovery, Surface: SurfaceTempoTagValuesV1, TagName: k, Source: discoverySourceTag},
				Query{Head: HeadTempo, Lang: migrate.LangTraceQL, Kind: KindTagDiscovery, Surface: SurfaceTempoTagValuesV2, TagName: k, Source: discoverySourceTag},
			)
		}
	}
	return out
}

// sortedStringSet renders a key set as a sorted slice, so the discovery
// probes a corpus anchors are always emitted in the same, deterministic order.
func sortedStringSet(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// logqlMatcherKeys extracts the distinct label names a LogQL selector's
// matchers reference, offline, using the same in-house parser RouteQuery
// already uses. A query the parser rejects contributes no keys — it is
// already reported out of scope by RouteQuery, so this never silently widens
// or narrows what LaneRouting decided.
func logqlMatcherKeys(expr string) []string {
	e, err := lsyntax.ParseExprWithoutValidation(lsyntax.NormalizeDottedLabels(expr))
	if err != nil {
		return nil
	}
	sel, ok := e.(lsyntax.LogSelectorExpr)
	if !ok {
		return nil
	}
	seen := make(map[string]struct{})
	for _, m := range sel.Matchers() {
		seen[m.Name] = struct{}{}
	}
	return sortedStringSet(seen)
}

// traceqlAttributeKeys extracts the distinct attribute names a TraceQL
// query's spanset filters reference, offline. It walks every SpansetFilter
// reachable from the pipeline (including nested spanset operations) and
// collects every Attribute node inside each filter's field expression,
// dropping intrinsics (name/kind/status/… are not dynamic keys a tag-values
// probe can enumerate).
//
// A node shape this walk does not cover contributes no keys, which never
// weakens the gate: the unfiltered per-scope tag-NAME diff is exact and
// complete on its own (see compareTagDiscovery); the corpus-anchored
// tag-VALUE probes only add depth on the keys the corpus's own queries happen
// to reference.
func traceqlAttributeKeys(expr string) []string {
	root, err := ast.Parse(expr)
	if err != nil || root == nil {
		return nil
	}
	seen := make(map[string]struct{})
	walkTraceQLPipeline(root.Pipeline, seen)
	return sortedStringSet(seen)
}

func walkTraceQLPipeline(p ast.Pipeline, seen map[string]struct{}) {
	for _, elem := range p.Elements {
		switch e := elem.(type) {
		case *ast.SpansetFilter:
			walkTraceQLFieldExpr(e.Expression, seen)
		case ast.SpansetOperation:
			walkTraceQLSpansetExpr(e.LHS, seen)
			walkTraceQLSpansetExpr(e.RHS, seen)
		case ast.Pipeline:
			walkTraceQLPipeline(e, seen)
		}
	}
}

func walkTraceQLSpansetExpr(se ast.SpansetExpression, seen map[string]struct{}) {
	switch s := se.(type) {
	case *ast.SpansetFilter:
		walkTraceQLFieldExpr(s.Expression, seen)
	case ast.SpansetOperation:
		walkTraceQLSpansetExpr(s.LHS, seen)
		walkTraceQLSpansetExpr(s.RHS, seen)
	case ast.Pipeline:
		walkTraceQLPipeline(s, seen)
	}
}

// walkTraceQLFieldExpr descends a field expression collecting every
// non-intrinsic Attribute name it references. BinaryOperation and
// UnaryOperation are the only two composite FieldExpression shapes (see
// internal/traceql/ast/expr.go); Attribute is the leaf this walk exists to
// find; Static (a literal) contributes nothing.
func walkTraceQLFieldExpr(fe ast.FieldExpression, seen map[string]struct{}) {
	switch e := fe.(type) {
	case *ast.BinaryOperation:
		walkTraceQLFieldExpr(e.LHS, seen)
		walkTraceQLFieldExpr(e.RHS, seen)
	case ast.UnaryOperation:
		walkTraceQLFieldExpr(e.Expression, seen)
	case ast.Attribute:
		if e.Intrinsic == ast.IntrinsicNone && e.Name != "" {
			seen[e.Name] = struct{}{}
		}
	}
}
