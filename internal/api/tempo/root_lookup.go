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
//
// TraceDurationNs carries the whole-trace wall-clock span
// (`max(span.end) - min(span.start)` across every span of the trace,
// in nanoseconds). Tempo's /api/search reports `durationMs` as the
// trace-wide wall-clock span, not the matched row's per-span
// Duration; the search result set typically contains only matched
// child spans (structural-join queries, `{ status = error }` against
// child-only fixtures), so the per-row Sample.Value fallback in
// toTraceSummaries under-reports vs. Tempo. The same follow-up CH
// query that recovers RootSpanName + RootServiceName also computes
// the trace-wide duration so applyRootMetadata can patch DurationMs
// alongside the root identity.
type rootMetadata struct {
	ServiceName     string
	SpanName        string
	TraceDurationNs int64
}

// resolveTraceRoots issues a follow-up CH query that fetches each
// missing trace's root-span identity (SpanName + service.name) and the
// whole-trace duration. The /api/search wrap projection projects only
// the spans matched by the user's TraceQL predicate, so structural-
// join queries (`>`, `<`, `>>`, `<<`, set ops) and filter queries that
// exclude the root span (`{ status = error }` on a fixture where only
// children carry that status) deliver result sets without a root row
// and with per-span (not trace-wide) durations. The shaper anchors on
// the earliest matched span as a fallback, but Tempo's wire spec pins
// rootTraceName / rootServiceName to the **actual** root span at the
// top of the trace tree and `durationMs` to the trace-wide wall-clock
// span. This helper recovers both.
//
// Implementation: a chplan tree built by hand (no parser, no
// optimizer pass needed) lowering to:
//
//	SELECT
//	  argMinIf(SpanName, Timestamp, ParentSpanId = '' OR ParentSpanId = '0000000000000000') AS RootSpanName,
//	  argMinIf(ResourceAttributes['service.name'], Timestamp, ParentSpanId = '' OR ParentSpanId = '0000000000000000') AS RootSvc,
//	  min(toUnixTimestamp64Nano(Timestamp)) AS TraceStartNs,
//	  max(toUnixTimestamp64Nano(Timestamp) + toInt64(Duration)) AS TraceEndNs
//	FROM otel_traces
//	WHERE TraceId IN ('<padded-id-1>', '<padded-id-2>', ...)
//	GROUP BY TraceId
//
// then an outer Project rewrites the Aggregate's columns into the
// canonical chclient.Sample envelope (MetricName / Attributes /
// TimeUnix / Value). Value carries `TraceEndNs - TraceStartNs` as
// float64 so applyRootMetadata can patch summary.DurationMs alongside
// the root identity.
//
// The WHERE clause is intentionally TraceId-only (no ParentSpanId
// restriction) so the trace start/end aggregates see every span,
// not just the root row. argMinIf isolates the root-span identity
// inside the same group so the query stays a single round trip.
//
// Result is keyed by the canonical 32-char lowercase-hex TraceID (the
// form the /api/search shaper surfaces on the wire post-#209; pre-fix,
// this was the leading-zero-stripped variant). Returns an empty map
// when traceIDs is empty; never returns nil on success.
func (h *Handler) resolveTraceRoots(ctx context.Context, traceIDs []string) (map[string]rootMetadata, error) {
	if len(traceIDs) == 0 {
		return map[string]rootMetadata{}, nil
	}

	plan := buildRootLookupPlan(h.Schema, traceIDs)
	// The root-lookup plan scans the spans table bounded by a literal
	// `TraceId IN (<padded ids>)` set (form-b). Thread the spans table onto the
	// emit context so chsql.Emit's RequireSpansScansBounded verifies that bound
	// is present — a regression that dropped the IN filter would fail closed
	// rather than full-scan otel_traces.
	sql, args, err := chsql.Emit(chsql.WithSpansTable(ctx, h.Schema.SpansTable), plan)
	if err != nil {
		return nil, fmt.Errorf("root lookup: emit: %w", err)
	}

	samples, err := h.Client.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("root lookup: execute: %w", err)
	}

	out := make(map[string]rootMetadata, len(samples))
	for _, s := range samples {
		// The aggregate's Attributes map carries the canonical 32-char
		// TraceID under searchKeyTraceID (`__cerberus_traceID`) and the
		// root span's service.name under the canonical `service.name`
		// key.
		tid := s.Labels[searchKeyTraceID]
		if tid == "" {
			continue
		}
		// Sample.Value carries the trace-wide duration in nanoseconds
		// (TraceEndNs - TraceStartNs), cast to float64 by the outer
		// Project so the chclient cursor decodes positionally. The
		// difference is non-negative by construction (max ≥ min); a
		// negative read indicates a corrupt fixture and is treated as
		// "no duration recovered" so the shaper's per-row fallback
		// stays in place.
		var durationNs int64
		if s.Value > 0 {
			durationNs = int64(s.Value)
		}
		out[tid] = rootMetadata{
			ServiceName:     s.Labels["service.name"],
			SpanName:        s.MetricName,
			TraceDurationNs: durationNs,
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
// carries the root SpanName. The `TimeUnix` column is a constant
// placeholder (`now64(9)`) — the follow-up lookup contributes per-
// trace metadata, not per-row timing. The `Value` column carries the
// trace-wide wall-clock duration in nanoseconds (max-end minus min-
// start across every span of the trace) cast to float64 so the
// chclient cursor decodes positionally; the shaper's
// applyRootMetadata reads it back and surfaces durationMs =
// TraceDurationNs / 1e6 alongside the recovered root identity.
//
// The filter is TraceId-only (no ParentSpanId restriction) so the
// trace start / end aggregates see every span of each affected
// trace, not just the root row. The root-span identifying
// aggregates use argMinIf with the (ParentSpanId IN (”,
// '0000000000000000')) condition so they pick the earliest root
// span within the same GROUP BY group — keeping the lookup a single
// round trip while distinguishing root vs. non-root rows.
func buildRootLookupPlan(s schema.Traces, traceIDs []string) chplan.Node {
	// Filter predicate: TraceId IN (<padded-id-1>, <padded-id-2>, ...)
	// as a single flat chplan.InList. The flatness is load-bearing: the
	// previous shape rendered a nested OR-chain of Eq predicates, whose
	// ClickHouse parser AST deepens by one level per trace ID — at
	// ~1000 IDs the query trips `max_parser_depth` (default 1000) with
	// code 306 ("Maximum parse depth exceeded"; observed in
	// compose-smoke run 27307036248 with missing=1006) and the search
	// response silently loses its root decoration. The IN tuple's
	// elements are AST siblings, so parse depth stays constant no
	// matter how many traces need root enrichment. Restricting to
	// TraceId only lets the trace start / end aggregates see every span
	// of the trace; argMinIf (below) carries the ParentSpanId
	// restriction on the root-span identifying aggregates so the same
	// group computes both.
	traceMatch := inStringList(s.TraceIDColumn, padTraceIDs(traceIDs))

	// rootCond expresses ParentSpanId IN ('', '0000000000000000') — the
	// on-disk root marker (pre-strip empty form + canonical 16-char
	// zero form). Reused inside both argMinIf aggregates so they pick
	// the root span among each TraceId group.
	rootCond := inStringList(s.ParentSpanIDColumn, []string{"", "0000000000000000"})

	// argMinIf(SpanName, Timestamp, rootCond) AS RootSpanName.
	aggSpanName := chplan.AggFunc{
		Name: "argMinIf",
		Args: []chplan.Expr{
			&chplan.ColumnRef{Name: s.SpanNameColumn},
			&chplan.ColumnRef{Name: s.TimestampColumn},
			rootCond,
		},
		Alias: "RootSpanName",
	}
	// argMinIf(ResourceAttributes['service.name'], Timestamp, rootCond) AS RootSvc.
	aggSvc := chplan.AggFunc{
		Name: "argMinIf",
		Args: []chplan.Expr{
			&chplan.MapAccess{
				Map: &chplan.ColumnRef{Name: s.ResourceAttributesColumn},
				Key: &chplan.LitString{V: "service.name"},
			},
			&chplan.ColumnRef{Name: s.TimestampColumn},
			rootCond,
		},
		Alias: "RootSvc",
	}
	// min(toUnixTimestamp64Nano(Timestamp)) AS TraceStartNs — the
	// earliest span-start across the trace, in nanoseconds since the
	// Unix epoch. Casting up-front lets the difference fall out as a
	// plain integer (DateTime64 lacks a typed interval subtraction
	// in the chplan IR).
	aggTraceStart := chplan.AggFunc{
		Name: "min",
		Args: []chplan.Expr{
			&chplan.FuncCall{
				Name: "toUnixTimestamp64Nano",
				Args: []chplan.Expr{&chplan.ColumnRef{Name: s.TimestampColumn}},
			},
		},
		Alias: "TraceStartNs",
	}
	// max(toUnixTimestamp64Nano(Timestamp) + toInt64(Duration)) AS TraceEndNs —
	// the latest span-end across the trace. Coercing Duration to Int64
	// keeps the sum Int64 so subtracting TraceStartNs (Int64) yields
	// a signed integer CH doesn't refuse to subtract.
	aggTraceEnd := chplan.AggFunc{
		Name: "max",
		Args: []chplan.Expr{
			&chplan.Binary{
				Op: chplan.OpAdd,
				Left: &chplan.FuncCall{
					Name: "toUnixTimestamp64Nano",
					Args: []chplan.Expr{&chplan.ColumnRef{Name: s.TimestampColumn}},
				},
				Right: &chplan.FuncCall{
					Name: "toInt64",
					Args: []chplan.Expr{&chplan.ColumnRef{Name: s.DurationColumn}},
				},
			},
		},
		Alias: "TraceEndNs",
	}
	agg := &chplan.Aggregate{
		Input:          &chplan.Filter{Input: &chplan.Scan{Table: s.SpansTable}, Predicate: traceMatch},
		GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: s.TraceIDColumn}},
		GroupByAliases: []string{"TraceId"},
		AggFuncs:       []chplan.AggFunc{aggSpanName, aggSvc, aggTraceStart, aggTraceEnd},
	}

	// Outer Project rewrites to canonical Sample shape so chclient
	// decodes positionally: MetricName (RootSpanName), Attributes
	// (map carrying TraceID + service.name), TimeUnix (now64), Value
	// (TraceEndNs - TraceStartNs as float64). Reading the columns
	// produced by the Aggregate by bare name works because Aggregate
	// emits `<expr> AS <alias>` + `<func> AS <alias>` in the SELECT
	// list.
	attrsMap := &chplan.FuncCall{Name: "map", Args: []chplan.Expr{
		&chplan.LitString{V: searchKeyTraceID},
		stripLeadingHexZeros("TraceId"),
		&chplan.LitString{V: "service.name"},
		&chplan.ColumnRef{Name: "RootSvc"},
	}}
	traceDurationNs := &chplan.Binary{
		Op:    chplan.OpSub,
		Left:  &chplan.ColumnRef{Name: "TraceEndNs"},
		Right: &chplan.ColumnRef{Name: "TraceStartNs"},
	}
	return &chplan.Project{
		Input: agg,
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: "RootSpanName"}, Alias: "MetricName"},
			{Expr: attrsMap, Alias: "Attributes"},
			{Expr: chplan.NowNano(), Alias: "TimeUnix"},
			{Expr: &chplan.FuncCall{Name: "toFloat64", Args: []chplan.Expr{traceDurationNs}}, Alias: "Value"},
		},
	}
}

// inStringList returns `<col> IN (<v1>, <v2>, ...)` as a single flat
// chplan.InList over string literals. Returns a guaranteed-false
// (`1 = 0`) predicate when vals is empty — CH rejects an empty IN
// list, and the caller (resolveTraceRoots) guards against the empty
// case anyway, so the branch only keeps the helper total over its
// input.
//
// Replaces the previous orEqLiterals OR-chain: that shape nested one
// Binary per value and blew ClickHouse's max_parser_depth (default
// 1000, error code 306) once a search returned >1000 traces needing
// root enrichment. InList parses at constant depth regardless of
// len(vals); see chplan.InList for the contract.
func inStringList(col string, vals []string) chplan.Expr {
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
	list := make([]chplan.Expr, 0, len(vals))
	for _, v := range vals {
		list = append(list, &chplan.LitString{V: v})
	}
	return &chplan.InList{
		Left: &chplan.ColumnRef{Name: col},
		List: list,
	}
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
// roots with the looked-up RootServiceName / RootTraceName /
// DurationMs. Skips summaries with no matching root entry (the
// trace's root is not in otel_traces — true truncation; the
// earliest-span fallback that toTraceSummaries already populated
// stays in place).
//
// The duration patch is the source of truth for traces whose result
// set lacked a root row: toTraceSummaries' fallback path picks the
// max per-row Sample.Value, which for structural-join / status-
// filter queries (where the result set only carries matched child
// spans) under-reports vs. Tempo's trace-wide wall-clock span. The
// follow-up CH query computes (max(Timestamp + Duration) -
// min(Timestamp)) across every span of each affected trace and
// surfaces the result via rootMetadata.TraceDurationNs; we patch it
// in here when positive so summary.DurationMs matches Tempo.
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
		// Patch DurationMs only when the lookup recovered a positive
		// trace-wide span (negative would indicate a corrupt fixture;
		// zero means a single-instant trace and is indistinguishable
		// from "no duration computed" so the per-row fallback stays).
		if root.TraceDurationNs > 0 {
			summaries[i].DurationMs = int(root.TraceDurationNs / 1_000_000)
		}
	}
}
