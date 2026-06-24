package traceql

import (
	"fmt"

	"github.com/grafana/tempo/pkg/traceql"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// This file lowers TraceQL's `| compare({<selection>}, topN[, start, end])`
// metrics first-stage operator (Grafana Traces Drilldown's "Comparison"
// tab) into a chplan.MetricsCompare node.
//
// Reference semantics — grafana/tempo pkg/traceql/engine_metrics_compare.go
// (consumed via the tsouza/tempo cerberus-accessors fork):
//
//   - Every span the pipeline produces is assigned to one of two
//     cohorts: "selection" (the span matches the compare() filter and,
//     when the optional start/end unix-nanosecond window is set, its
//     start time falls in `(start, end]`) or "baseline" (everything
//     else). See MetricsCompare.isSelection upstream.
//   - For each span, every attribute the storage layer surfaces under
//     `SecondPassSelectAll` is counted into per-(cohort, attribute,
//     value) time-interval buckets, plus per-(cohort, attribute)
//     totals. Duration-typed values and the span-start-time / trace-id
//     / parent-id / nested-set intrinsics are excluded
//     (MetricsCompare.processAttribute).
//   - The wire result is the top-N values per (cohort, attribute) by
//     total count, labelled `{__meta_type="baseline|selection", <attr>=<val>}`,
//     plus `{__meta_type="baseline_total|selection_total", <attr>="nil"}`
//     totals series (BaselineAggregator.Results).
//
// The SQL this node emits produces the raw per-(cohort, attr, value
// [, anchor]) counts; the Tempo HTTP handler mirrors the
// BaselineAggregator (top-N, totals, zero-fill, label scheme). See
// internal/api/tempo/metrics_query_range_compare.go.

// Output-column aliases for the compare SQL shape. The handler's
// Sample projection references them; keeping them as constants pins
// the lowering and the HTTP layer to the same names.
const (
	compareSelAlias   = "is_selection"
	compareAttrAlias  = "attr"
	compareValAlias   = "val"
	rootNameAlias     = "__root_name"
	rootServiceAlias  = "__root_service_name"
	compareNilLiteral = "nil"
)

// arrayZip produces 1-based (key, value) tuples; tupleElement indexes them.
const (
	tupleKeyIdx   = 1
	tupleValueIdx = 2
)

// wellKnownResourceAttrs are the resource-scoped attributes vparquet4
// stores in dedicated columns (tempodb/encoding/vparquet4/block_traceql.go,
// `WellKnownColumnLookups`). Under select-all, reference Tempo fetches
// these columns for EVERY batch; a null cell (the resource lacks the
// attribute) surfaces as the literal string "nil"
// (batchCollector.KeepGroup's IsNull branch), producing a `"nil"`
// value bucket in compare() output. Cerberus mirrors that with an
// `if(mapContains(...), map[k], 'nil')` projection per key.
var wellKnownResourceAttrs = []string{
	"service.name",
	"cluster",
	"namespace",
	"pod",
	"container",
	"k8s.cluster.name",
	"k8s.namespace.name",
	"k8s.pod.name",
	"k8s.container.name",
}

// wellKnownSpanAttrs is the span-scoped slice of vparquet's
// WellKnownColumnLookups; same "nil"-fallback contract as
// wellKnownResourceAttrs (spanCollector.KeepGroup's IsNull branch).
//
// The first three are the long-standing vparquet4 columns; the last
// five are the OTel-semconv additions current Tempo main ships
// (verified live against grafana/tempo:main-2f74ea8 by the
// compatibility/tempo `metrics_compare_status_error` corpus case —
// reference compare() output surfaces a "nil" bucket for each of
// them on every span that lacks the attribute).
var wellKnownSpanAttrs = []string{
	"http.status_code",
	"http.method",
	"http.url",
	"http.request.method",
	"http.route",
	"server.address",
	"url.path",
	"url.route",
}

// lowerMetricsCompare lowers `| compare({...}, topN[, start, end])`.
//
// `prev` is the lowered spanset prefix (the query's `{...}` pipeline),
// same contract as lowerMetricsAggregate.
func lowerMetricsCompare(prev chplan.Node, mc *traceql.MetricsCompare, s schema.Traces) (chplan.Node, error) {
	f := mc.Filter()
	if f == nil {
		return nil, fmt.Errorf("traceql: compare() requires a selection spanset filter")
	}
	sel, err := lowerFieldExpr(f.Expression, s)
	if err != nil {
		return nil, err
	}
	if sel == nil {
		return nil, fmt.Errorf("traceql: compare() selection filter lowered to no predicate")
	}

	startNs := int64(mc.Start())
	endNs := int64(mc.End())
	if startNs > 0 && endNs > 0 {
		// Tempo's isSelection only evaluates the filter for spans whose
		// start time falls in (start, end]; everything else is baseline.
		// AND the window into the selection predicate so the SQL cohort
		// flag reproduces that assignment exactly.
		tsNs := &chplan.FuncCall{
			Name: "toUnixTimestamp64Nano",
			Args: []chplan.Expr{&chplan.ColumnRef{Name: s.TimestampColumn}},
		}
		window := &chplan.Binary{
			Op:    chplan.OpAnd,
			Left:  &chplan.Binary{Op: chplan.OpGt, Left: tsNs, Right: &chplan.LitInt{V: startNs}},
			Right: &chplan.Binary{Op: chplan.OpLe, Left: tsNs, Right: &chplan.LitInt{V: endNs}},
		}
		sel = &chplan.Binary{Op: chplan.OpAnd, Left: window, Right: sel}
	}

	node := &chplan.MetricsCompare{
		Selection:  sel,
		TopN:       mc.TopN(),
		StartNs:    startNs,
		EndNs:      endNs,
		Pairs:      compareAttrPairsExpr(s),
		SelAlias:   compareSelAlias,
		AttrAlias:  compareAttrAlias,
		ValAlias:   compareValAlias,
		ValueAlias: metricsValueAlias,
		Inner:      prev,
	}

	// rootName / rootServiceName are trace-level columns in vparquet;
	// OTel-CH derives them by joining each span's trace to its root span
	// (empty ParentSpanId). Requires the parent-span-id, trace-id and
	// service-name columns; schemas that blank any of them skip the two
	// trace-scoped attributes rather than failing the whole query.
	if s.ParentSpanIDColumn != "" && s.TraceIDColumn != "" && s.SpanNameColumn != "" && s.ServiceNameColumn != "" {
		node.RootLookup = compareRootLookup(s)
		node.TraceIDColumn = s.TraceIDColumn
		node.RootNameAlias = rootNameAlias
		node.RootServiceAlias = rootServiceAlias
	}

	return node, nil
}

// compareRootLookup builds the per-trace root-span relation:
//
//	SELECT <TraceId>, any(<SpanName>) AS __root_name,
//	       any(<ServiceName>) AS __root_service_name
//	FROM <spans> WHERE <ParentSpanId> = '' GROUP BY <TraceId>
//
// One row per trace; the compare emitter LEFT JOINs it on TraceId so
// every span row carries its trace's rootName / rootServiceName —
// mirroring vparquet's trace-level RootSpanName / RootServiceName
// columns. Orphan traces (no root span in the scanned window) fall out
// of the LEFT JOIN as empty strings.
func compareRootLookup(s schema.Traces) chplan.Node {
	return &chplan.Aggregate{
		Input: &chplan.Filter{
			Input: &chplan.Scan{Table: s.SpansTable},
			Predicate: &chplan.Binary{
				Op:    chplan.OpEq,
				Left:  &chplan.ColumnRef{Name: s.ParentSpanIDColumn},
				Right: &chplan.LitString{V: ""},
			},
		},
		GroupBy: []chplan.Expr{&chplan.ColumnRef{Name: s.TraceIDColumn}},
		AggFuncs: []chplan.AggFunc{
			{Name: "any", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.SpanNameColumn}}, Alias: rootNameAlias},
			{Name: "any", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.ServiceNameColumn}}, Alias: rootServiceAlias},
		},
	}
}

// compareAttrPairsExpr builds the Array(Tuple(String, String))
// expression enumerating every (attribute wire name, value) pair of a
// span row — the OTel-CH analogue of what reference Tempo's
// `span.AllAttributesFunc` yields under SecondPassSelectAll, minus the
// shapes compare() excludes:
//
//   - Duration-typed values (`duration`, `traceDuration`) — excluded by
//     processAttribute's TypeDuration guard.
//   - span:id / span:parentID / span start time / trace:id /
//     nested-set positions — excluded by the select-all column set or
//     processAttribute's intrinsic skip-list.
//
// Included, mirroring vparquet4's select-all column inventory:
//
//   - intrinsics: name, status (lowercased to Tempo's Status.String()
//     forms), statusMessage, kind (lowercased to Kind.String() forms);
//   - trace-level rootName / rootServiceName (via the RootLookup join);
//   - instrumentation:name / instrumentation:version (scope columns);
//   - the well-known dedicated attributes (vparquet4
//     WellKnownColumnLookups) with the literal "nil" fallback for
//     absent values — reference Tempo surfaces null dedicated-column
//     cells as the string "nil";
//   - every remaining ResourceAttributes / SpanAttributes entry,
//     scope-prefixed (`resource.` / `span.`).
//
// Every value is projected as String — Tempo's typed AnyValue label
// values (intValue / boolValue / doubleValue) canonicalise to the same
// strings on the differ/Grafana side, and the OTel-CH maps store
// attribute values as strings already.
func compareAttrPairsExpr(s schema.Traces) chplan.Expr {
	col := func(name string) chplan.Expr { return &chplan.ColumnRef{Name: name} }
	lit := func(v string) chplan.Expr { return &chplan.LitString{V: v} }
	call := func(name string, args ...chplan.Expr) chplan.Expr {
		return &chplan.FuncCall{Name: name, Args: args}
	}
	pair := func(name string, val chplan.Expr) chplan.Expr {
		return call("tuple", lit(name), call("toString", val))
	}

	fixed := []chplan.Expr{
		pair("name", col(s.SpanNameColumn)),
		pair("status", call("lower", col(s.StatusCodeColumn))),
		pair("statusMessage", col(s.StatusMessageColumn)),
		pair("kind", call("lower", col(s.SpanKindColumn))),
	}
	if s.ParentSpanIDColumn != "" && s.TraceIDColumn != "" && s.SpanNameColumn != "" && s.ServiceNameColumn != "" {
		fixed = append(
			fixed,
			pair("rootName", &chplan.BareIdent{Name: rootNameAlias}),
			pair("rootServiceName", &chplan.BareIdent{Name: rootServiceAlias}),
		)
	}
	if s.ScopeNameColumn != "" {
		fixed = append(fixed, pair("instrumentation:name", col(s.ScopeNameColumn)))
	}
	if s.ScopeVersionColumn != "" {
		fixed = append(fixed, pair("instrumentation:version", col(s.ScopeVersionColumn)))
	}

	// Well-known dedicated attributes: present map entries surface
	// verbatim, absent ones as the literal "nil" (reference Tempo's
	// null-cell contract — see wellKnownResourceAttrs).
	wellKnown := func(mapCol, scopePrefix string, keys []string) []chplan.Expr {
		out := make([]chplan.Expr, 0, len(keys))
		for _, k := range keys {
			out = append(out, pair(
				scopePrefix+k,
				call(
					"if",
					call("mapContains", col(mapCol), lit(k)),
					&chplan.MapAccess{Map: col(mapCol), Key: lit(k)},
					lit(compareNilLiteral),
				),
			))
		}
		return out
	}
	fixed = append(fixed, wellKnown(s.ResourceAttributesColumn, "resource.", wellKnownResourceAttrs)...)
	fixed = append(fixed, wellKnown(s.AttributesColumn, "span.", wellKnownSpanAttrs)...)

	// Generic map attributes: scope-prefix every (key, value) entry that
	// is NOT carried by a well-known dedicated column above.
	generic := func(mapCol, scopePrefix string, exclude []string) chplan.Expr {
		exq := make([]chplan.Expr, 0, len(exclude))
		for _, k := range exclude {
			exq = append(exq, lit(k))
		}
		t := &chplan.BareIdent{Name: "t"}
		zipped := call("arrayZip", call("mapKeys", col(mapCol)), call("mapValues", col(mapCol)))
		filtered := call(
			"arrayFilter",
			&chplan.Lambda{Params: []string{"t"}, Body: call(
				"not",
				call("has", call("array", exq...), call("tupleElement", t, &chplan.LitInt{V: tupleKeyIdx})),
			)},
			zipped,
		)
		return call(
			"arrayMap",
			&chplan.Lambda{Params: []string{"t"}, Body: call(
				"tuple",
				call("concat", lit(scopePrefix), call("tupleElement", t, &chplan.LitInt{V: tupleKeyIdx})),
				call("toString", call("tupleElement", t, &chplan.LitInt{V: tupleValueIdx})),
			)},
			filtered,
		)
	}

	return call(
		"arrayConcat",
		call("array", fixed...),
		generic(s.ResourceAttributesColumn, "resource.", wellKnownResourceAttrs),
		generic(s.AttributesColumn, "span.", wellKnownSpanAttrs),
	)
}
