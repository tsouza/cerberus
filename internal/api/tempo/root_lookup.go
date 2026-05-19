package tempo

import (
	"context"
	"fmt"
	"strings"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// rootMetadata carries the root-span identification a follow-up CH
// lookup recovers for a single trace. Empty fields mean the lookup
// found no row matching (TraceId, ParentSpanId IN (empty or zero hex)) for
// that TraceId — a true truncation where the trace's root is not in
// otel_traces, vs. a structural-join / status-filter where the root
// is present but absent from the original result set.
type rootMetadata struct {
	ServiceName string
	SpanName    string
}

// resolveTraceRoots issues a follow-up CH query that fetches each
// missing trace's root-span identity (SpanName + service.name). The
// /api/search wrap projection projects only the spans matched by the
// user's TraceQL predicate, so structural-join queries (`>`, `<`,
// `>>`, `<<`, set ops) and filter queries that exclude the root span
// (`{ status = error }` on a fixture where only children carry that
// status) deliver result sets without a root row. The shaper anchors
// on the earliest matched span as a fallback, but Tempo's wire spec
// pins rootTraceName / rootServiceName to the **actual** root span at
// the top of the trace tree. This helper recovers that.
//
// Implementation: a chplan tree built by hand (no parser, no
// optimizer pass needed) lowering to:
//
//	SELECT
//	  argMin(SpanName, Timestamp) AS RootSpanName,
//	  map(
//	    '__cerberus_traceID', stripLeadingHexZeros(TraceId),
//	    'service.name',       argMin(ResourceAttributes['service.name'], Timestamp),
//	  ) AS Attributes,
//	  now64(9) AS TimeUnix,
//	  0 AS Value
//	FROM otel_traces
//	WHERE (ParentSpanId = '' OR ParentSpanId = '0000000000000000')
//	  AND TraceId IN ('<padded-id-1>', '<padded-id-2>', ...)
//	GROUP BY TraceId
//
// Result is keyed by the stripped-zero TraceID (the form the original
// /api/search shaper used). Returns an empty map when traceIDs is
// empty; never returns nil on success.
func (h *Handler) resolveTraceRoots(ctx context.Context, traceIDs []string) (map[string]rootMetadata, error) {
	if len(traceIDs) == 0 {
		return map[string]rootMetadata{}, nil
	}

	plan := buildRootLookupPlan(h.Schema, traceIDs)
	sql, args, err := chsql.Emit(ctx, plan)
	if err != nil {
		return nil, fmt.Errorf("root lookup: emit: %w", err)
	}

	samples, err := h.Client.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("root lookup: execute: %w", err)
	}

	out := make(map[string]rootMetadata, len(samples))
	for _, s := range samples {
		// The aggregate's Attributes map carries the stripped TraceID
		// under searchKeyTraceID (`__cerberus_traceID`) and the root
		// span's service.name under the canonical `service.name` key.
		tid := s.Labels[searchKeyTraceID]
		if tid == "" {
			continue
		}
		out[tid] = rootMetadata{
			ServiceName: s.Labels["service.name"],
			SpanName:    s.MetricName,
		}
	}
	return out, nil
}

// buildRootLookupPlan returns the chplan tree resolveTraceRoots emits.
// Extracted so the SQL shape can be tested in isolation without going
// through the chclient stub.
//
// The shape is intentionally rendered through the canonical wrap-
// projection format (MetricName, Attributes, TimeUnix, Value) so the
// existing chclient.Sample decoding picks it up without a new path.
// The Attributes map carries the stripped-zero TraceID (the search-
// path identity key) plus the root span's service.name; MetricName
// carries the root SpanName. The `TimeUnix` and `Value` columns are
// constant placeholders — the follow-up lookup contributes metadata,
// not per-trace timing, which the original shaper already has.
func buildRootLookupPlan(s schema.Traces, traceIDs []string) chplan.Node {
	// Filter predicate: ParentSpanId IN ('', '0000000000000000') AND
	// TraceId IN (<padded-id-1>, <padded-id-2>, ...). Without an `IN`
	// chplan expression, both arms render as flat OR-chains of Eq
	// predicates — CH planner collapses these to constant-set lookups
	// at execute time.
	parentIsRoot := orEqLiterals(s.ParentSpanIDColumn, []string{"", "0000000000000000"})
	traceMatch := orEqLiterals(s.TraceIDColumn, padTraceIDs(traceIDs))
	pred := &chplan.Binary{
		Op:    chplan.OpAnd,
		Left:  parentIsRoot,
		Right: traceMatch,
	}

	// argMin(SpanName, Timestamp) AS RootSpanName.
	aggSpanName := chplan.AggFunc{
		Name:  "argMin",
		Args:  []chplan.Expr{&chplan.ColumnRef{Name: s.SpanNameColumn}, &chplan.ColumnRef{Name: s.TimestampColumn}},
		Alias: "RootSpanName",
	}
	// argMin(ResourceAttributes['service.name'], Timestamp) AS RootSvc.
	aggSvc := chplan.AggFunc{
		Name: "argMin",
		Args: []chplan.Expr{
			&chplan.MapAccess{
				Map: &chplan.ColumnRef{Name: s.ResourceAttributesColumn},
				Key: &chplan.LitString{V: "service.name"},
			},
			&chplan.ColumnRef{Name: s.TimestampColumn},
		},
		Alias: "RootSvc",
	}
	agg := &chplan.Aggregate{
		Input:          &chplan.Filter{Input: &chplan.Scan{Table: s.SpansTable}, Predicate: pred},
		GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: s.TraceIDColumn}},
		GroupByAliases: []string{"TraceId"},
		AggFuncs:       []chplan.AggFunc{aggSpanName, aggSvc},
	}

	// Outer Project rewrites to canonical Sample shape so chclient
	// decodes positionally: MetricName (RootSpanName), Attributes
	// (map carrying TraceID + service.name), TimeUnix (now64), Value
	// (0). Reading the columns produced by the Aggregate by bare name
	// works because Aggregate emits `<expr> AS <alias>` + `<func> AS
	// <alias>` in the SELECT list.
	attrsMap := &chplan.FuncCall{Name: "map", Args: []chplan.Expr{
		&chplan.LitString{V: searchKeyTraceID},
		stripLeadingHexZeros("TraceId"),
		&chplan.LitString{V: "service.name"},
		&chplan.ColumnRef{Name: "RootSvc"},
	}}
	return &chplan.Project{
		Input: agg,
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: "RootSpanName"}, Alias: "MetricName"},
			{Expr: attrsMap, Alias: "Attributes"},
			{Expr: &chplan.FuncCall{Name: "now64", Args: []chplan.Expr{&chplan.LitInt{V: 9}}}, Alias: "TimeUnix"},
			{Expr: &chplan.FuncCall{Name: "toFloat64", Args: []chplan.Expr{&chplan.LitInt{V: 0}}}, Alias: "Value"},
		},
	}
}

// orEqLiterals returns `<col> = <v1> OR <col> = <v2> OR ...` as a
// nested chplan.Binary OR-chain. Returns a single Eq when len(vals) ==
// 1, and a guaranteed-false (`<col> = <col> AND 1=0`-equivalent) when
// vals is empty — but the caller (resolveTraceRoots) guards against
// the empty case so the branch is unreachable in production.
func orEqLiterals(col string, vals []string) chplan.Expr {
	if len(vals) == 0 {
		// Guaranteed-false: 1 = 0. CH evaluates this at planning time
		// and short-circuits the filter; we should never hit this
		// branch but it keeps the helper total over its input.
		return &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.LitInt{V: 1},
			Right: &chplan.LitInt{V: 0},
		}
	}
	eq := func(v string) chplan.Expr {
		return &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: col},
			Right: &chplan.LitString{V: v},
		}
	}
	out := eq(vals[0])
	for i := 1; i < len(vals); i++ {
		out = &chplan.Binary{Op: chplan.OpOr, Left: out, Right: eq(vals[i])}
	}
	return out
}

// padTraceIDs left-pads each stripped-zero TraceID back to the
// canonical 32-char lowercase-hex on-disk form so the equality
// predicate matches otel_traces.TraceId. Mirrors normaliseTraceID
// but in bulk; sorted-stable so the generated SQL is deterministic
// across calls (helps the chsql golden tests this helper unblocks).
func padTraceIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, normaliseTraceID(id))
	}
	return out
}

// applyRootMetadata patches each summary whose TraceID appears in
// roots with the looked-up RootServiceName / RootTraceName. Skips
// summaries with no matching root entry (the trace's root is not in
// otel_traces — true truncation; the earliest-span fallback that
// toTraceSummaries already populated stays in place).
//
// Modifies the summaries slice in place; the slice header itself
// (length, capacity, ordering) is unchanged.
func applyRootMetadata(summaries []TraceSummary, roots map[string]rootMetadata) {
	if len(roots) == 0 {
		return
	}
	for i := range summaries {
		root, ok := roots[summaries[i].TraceID]
		if !ok {
			continue
		}
		// Empty root SpanName means the lookup returned a row but the
		// column was empty — treat as "no useful root data" and keep
		// the earliest-span fallback. SpanName=="" rows can happen if
		// a future producer emits a span with no name; the wire spec
		// permits but discourages it.
		if strings.TrimSpace(root.SpanName) != "" {
			summaries[i].RootTraceName = root.SpanName
		}
		if root.ServiceName != "" {
			summaries[i].RootServiceName = root.ServiceName
		}
	}
}
