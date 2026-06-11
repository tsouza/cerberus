package tempo

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestBuildRootLookupPlan_SQLShape pins the SQL shape emitted by the
// follow-up root-lookup the /api/search shaper issues when the
// original result set lacks a root row. The query must:
//
//  1. Group by TraceId and emit argMin(SpanName, Timestamp) AS
//     RootSpanName so the earliest root span (smallest Timestamp)
//     wins under broken-trace shapes with multiple roots.
//  2. Filter on the OTel-CH on-disk root markers (empty / 16-char-zero
//     ParentSpanId) AND on the affected TraceIDs so only true root
//     rows are aggregated and the scan is bounded.
//  3. Project the canonical Sample shape (MetricName, Attributes,
//     TimeUnix, Value) so chclient.Sample decodes the rows positionally
//     — the resolveTraceRoots caller reads SpanName off MetricName and
//     service.name + __cerberus_traceID off the Attributes map.
func TestBuildRootLookupPlan_SQLShape(t *testing.T) {
	t.Parallel()
	plan := buildRootLookupPlan(schema.DefaultOTelTraces(), []string{
		"17",                               // short stripped form
		"abc",                              // shorter, will be padded
		"0123456789abcdef0123456789abcdef", // already 32 chars
	})

	sql, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}

	// Must aggregate via argMin(SpanName, Timestamp) so the earliest
	// root span wins for broken traces.
	if !strings.Contains(sql, "argMin") {
		t.Errorf("SQL must use argMin for per-trace root selection; got %s", sql)
	}
	// Must alias one aggregate as RootSpanName so the outer Project
	// can project it as MetricName.
	if !strings.Contains(sql, "RootSpanName") {
		t.Errorf("SQL must alias the SpanName argMin as RootSpanName; got %s", sql)
	}
	// Must alias the service-name aggregate as RootSvc.
	if !strings.Contains(sql, "RootSvc") {
		t.Errorf("SQL must alias the service.name argMin as RootSvc; got %s", sql)
	}
	// Must filter on ParentSpanId (the root-identifier column).
	if !strings.Contains(sql, "ParentSpanId") {
		t.Errorf("SQL must filter on ParentSpanId; got %s", sql)
	}
	// Must filter on TraceId (the per-trace scoping column).
	if !strings.Contains(sql, "TraceId") {
		t.Errorf("SQL must filter on TraceId; got %s", sql)
	}
	// Must group by TraceId so the aggregate yields one row per trace.
	if !strings.Contains(sql, "GROUP BY") {
		t.Errorf("SQL must group by TraceId; got %s", sql)
	}
	// Canonical Sample envelope columns.
	for _, alias := range []string{"MetricName", "Attributes", "TimeUnix", "Value"} {
		if !strings.Contains(sql, alias) {
			t.Errorf("SQL must project canonical Sample alias %q; got %s", alias, sql)
		}
	}

	// Trace-duration aggregates: the inner Aggregate must surface
	// TraceStartNs (min of toUnixTimestamp64Nano(Timestamp)) and
	// TraceEndNs (max of toUnixTimestamp64Nano(Timestamp) +
	// toInt64(Duration)) so the outer Project can derive
	// `TraceEndNs - TraceStartNs` as the trace-wide wall-clock span
	// in nanoseconds, threaded through the canonical Value slot for
	// the shaper's applyRootMetadata to pick up.
	for _, alias := range []string{"TraceStartNs", "TraceEndNs"} {
		if !strings.Contains(sql, alias) {
			t.Errorf("SQL must project trace-duration aggregate %q; got %s", alias, sql)
		}
	}
	if !strings.Contains(sql, "toUnixTimestamp64Nano") {
		t.Errorf("SQL must cast Timestamp to nanoseconds via toUnixTimestamp64Nano; got %s", sql)
	}
	if !strings.Contains(sql, "toInt64") {
		t.Errorf("SQL must cast Duration to Int64 so the sum stays signed; got %s", sql)
	}
	// The root-span identifying aggregates must use argMinIf so the
	// (ParentSpanId IN ('', '0000000000000000')) condition carries
	// inside the same GROUP BY group as the unconditional trace
	// start / end aggregates. The previous shape filtered with WHERE
	// ParentSpanId IN (...) AND ..., which would have scoped the
	// trace-duration aggregates to root spans only and under-
	// reported by definition.
	if !strings.Contains(sql, "argMinIf") {
		t.Errorf("SQL must use argMinIf so root-span identity scopes ParentSpanId without restricting the trace-duration aggregates; got %s", sql)
	}

	// Args must include each TraceID padded to 32-char lowercase hex
	// so the equality match hits the otel_traces.TraceId column.
	// padTraceIDs pads to 32 chars: "17" → 30 zeros + "17", "abc" → 29
	// zeros + "abc", already-32-char input round-trips byte-for-byte.
	wantPadded := map[string]bool{
		"00000000000000000000000000000017": false,
		"00000000000000000000000000000abc": false,
		"0123456789abcdef0123456789abcdef": false,
	}
	for _, a := range args {
		s, ok := a.(string)
		if !ok {
			continue
		}
		if _, present := wantPadded[s]; present {
			wantPadded[s] = true
		}
	}
	for id, found := range wantPadded {
		if !found {
			t.Errorf("padded TraceID %q missing from args %v", id, args)
		}
	}

	// Reserved key strings must be parameterised — searchKeyTraceID
	// + "service.name" both appear as args so the map() call binds
	// positionally.
	for _, key := range []string{searchKeyTraceID, "service.name"} {
		var sawKey bool
		for _, a := range args {
			if s, ok := a.(string); ok && s == key {
				sawKey = true
				break
			}
		}
		if !sawKey {
			t.Errorf("expected reserved key %q in args; got %v", key, args)
		}
	}

	// Empty parent-span-id literal must appear so the IN list matches
	// the OTel-CH on-disk empty form (hex.EncodeToString(nil) → "").
	var sawEmptyParent bool
	for _, a := range args {
		if s, ok := a.(string); ok && s == "" {
			sawEmptyParent = true
			break
		}
	}
	if !sawEmptyParent {
		t.Errorf("expected empty-string ParentSpanId literal in args; got %v", args)
	}
	// 16-char zero form must appear so the IN list also matches
	// producers that store the canonical hex-encoded all-zero shape.
	var sawZeroParent bool
	for _, a := range args {
		if s, ok := a.(string); ok && s == "0000000000000000" {
			sawZeroParent = true
			break
		}
	}
	if !sawZeroParent {
		t.Errorf("expected 16-char-zero ParentSpanId literal in args; got %v", args)
	}
}

// TestBuildRootLookupPlan_FlatParseDepth pins the O(1)-parse-depth
// IN-list shape of the trace-ID filter. The pre-fix shape rendered a
// nested OR-chain of equality predicates — one ClickHouse AST level
// per trace ID — and blew `max_parser_depth` (default 1000, error
// code 306) once a search returned >1000 traces needing root
// enrichment (compose-smoke run 27307036248: "Maximum parse depth
// (1000) exceeded", missing=1006), silently dropping the root
// decoration from the response.
//
// Parenthesis nesting in the emitted SQL is a faithful proxy for CH
// parse depth here (every OR-chain level emitted a paren pair), so the
// regression contract is: the maximum paren depth at N=2000 IDs must
// equal the depth at N=2 — i.e. depth must not scale with ID count.
func TestBuildRootLookupPlan_FlatParseDepth(t *testing.T) {
	t.Parallel()

	depthFor := func(n int) int {
		t.Helper()
		ids := make([]string, 0, n)
		for i := range n {
			// Distinct, full-width hex IDs (no leading-zero stripping
			// ambiguity) so each contributes one IN element.
			ids = append(ids, fmt.Sprintf("%032x", i+1))
		}
		plan := buildRootLookupPlan(schema.DefaultOTelTraces(), ids)
		sql, args, err := chsql.Emit(context.Background(), plan)
		if err != nil {
			t.Fatalf("Emit with %d IDs: %v", n, err)
		}
		if len(args) < n {
			t.Fatalf("expected >= %d bound args (one per trace ID), got %d", n, len(args))
		}
		depth, maxDepth := 0, 0
		for _, c := range sql {
			switch c {
			case '(':
				depth++
				if depth > maxDepth {
					maxDepth = depth
				}
			case ')':
				depth--
			}
		}
		return maxDepth
	}

	d2 := depthFor(2)
	d2000 := depthFor(2000)
	if d2000 != d2 {
		t.Errorf("paren depth scales with trace-ID count: depth(N=2000)=%d, depth(N=2)=%d — the trace filter must stay a flat IN list", d2000, d2)
	}

	// The trace filter must render as a single flat IN over the
	// TraceId column, not an OR-chain.
	plan := buildRootLookupPlan(schema.DefaultOTelTraces(), []string{"17", "abc"})
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, "`TraceId` IN (") {
		t.Errorf("expected flat `TraceId IN (...)` filter; got %s", sql)
	}
	if strings.Contains(sql, " OR ") {
		t.Errorf("expected no OR-chain anywhere in the lookup SQL; got %s", sql)
	}
}

// TestBuildRootLookupPlan_NoTraces verifies the helper renders a
// valid SQL even with no trace IDs — defensive guarantee against an
// empty `missing` list bypassing the caller's `len(traceIDs) == 0`
// short-circuit (callers should not reach this with empty input, but
// the helper must stay total).
func TestBuildRootLookupPlan_NoTraces(t *testing.T) {
	t.Parallel()
	plan := buildRootLookupPlan(schema.DefaultOTelTraces(), nil)
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit on empty traceIDs: %v", err)
	}
	if sql == "" {
		t.Errorf("expected non-empty SQL even for empty traceIDs; got empty string")
	}
}

// TestApplyRootMetadata_PatchesSummaries verifies the per-trace patch
// merges recovered roots into the summary slice without touching
// summaries whose TraceID is absent from the lookup map.
func TestApplyRootMetadata_PatchesSummaries(t *testing.T) {
	t.Parallel()
	summaries := []TraceSummary{
		{TraceID: "a", RootServiceName: "fallback-svc-a", RootTraceName: "fallback-name-a"},
		{TraceID: "b", RootServiceName: "fallback-svc-b", RootTraceName: "fallback-name-b"},
		{TraceID: "c", RootServiceName: "fallback-svc-c", RootTraceName: "fallback-name-c"},
	}
	roots := map[string]rootMetadata{
		"a": {ServiceName: "real-svc-a", SpanName: "real-name-a"},
		"c": {ServiceName: "real-svc-c", SpanName: "real-name-c"},
	}
	applyRootMetadata(summaries, roots)

	if summaries[0].RootTraceName != "real-name-a" || summaries[0].RootServiceName != "real-svc-a" {
		t.Errorf("summary a: got %+v, want recovered metadata", summaries[0])
	}
	if summaries[1].RootTraceName != "fallback-name-b" || summaries[1].RootServiceName != "fallback-svc-b" {
		t.Errorf("summary b: got %+v, want untouched fallback metadata", summaries[1])
	}
	if summaries[2].RootTraceName != "real-name-c" || summaries[2].RootServiceName != "real-svc-c" {
		t.Errorf("summary c: got %+v, want recovered metadata", summaries[2])
	}
}

// TestApplyRootMetadata_EmptyRootsNoop confirms the helper is a no-op
// when no roots were recovered (lookup returned nothing for any
// trace) — the caller's earliest-span fallback survives intact.
func TestApplyRootMetadata_EmptyRootsNoop(t *testing.T) {
	t.Parallel()
	summaries := []TraceSummary{
		{TraceID: "a", RootServiceName: "fallback", RootTraceName: "fallback"},
	}
	applyRootMetadata(summaries, nil)
	if summaries[0].RootServiceName != "fallback" || summaries[0].RootTraceName != "fallback" {
		t.Errorf("empty roots must leave summaries untouched; got %+v", summaries[0])
	}
}

// TestApplyRootMetadata_PatchesDurationMs covers the duration-patch
// arm: the follow-up lookup query surfaces the whole-trace wall-
// clock span via rootMetadata.TraceDurationNs, and the merge step
// must convert it to milliseconds and overwrite the per-row
// Sample.Value fallback toTraceSummaries computed from matched
// child spans. Mirrors the structural-join / status-filter compat
// cases that pre-fix reported durationMs from a single child span
// instead of the trace-wide span Tempo emits.
func TestApplyRootMetadata_PatchesDurationMs(t *testing.T) {
	t.Parallel()
	summaries := []TraceSummary{
		// trace a: per-row fallback computed 20ms; the follow-up
		// lookup recovers the trace-wide 150ms span.
		{TraceID: "a", DurationMs: 20},
		// trace b: lookup recovered no positive duration (e.g. a
		// single-instant trace, or a fixture with no spans seeded
		// post-search-window) — the per-row fallback (80ms) stays.
		{TraceID: "b", DurationMs: 80},
		// trace c: lookup absent (true truncation) — DurationMs
		// untouched.
		{TraceID: "c", DurationMs: 60},
	}
	roots := map[string]rootMetadata{
		"a": {ServiceName: "checkout", SpanName: "POST /api/checkout", TraceDurationNs: 150_000_000},
		"b": {ServiceName: "frontend", SpanName: "GET /healthz"},
	}
	applyRootMetadata(summaries, roots)

	if got := summaries[0].DurationMs; got != 150 {
		t.Errorf("trace a DurationMs: got %d, want 150 (lookup-recovered trace-wide span)", got)
	}
	if got := summaries[1].DurationMs; got != 80 {
		t.Errorf("trace b DurationMs: got %d, want 80 (lookup carried no duration; per-row fallback preserved)", got)
	}
	if got := summaries[2].DurationMs; got != 60 {
		t.Errorf("trace c DurationMs: got %d, want 60 (lookup absent; per-row fallback preserved)", got)
	}
}

// TestApplyRootMetadata_NegativeDurationIgnored guards against the
// (defensively-handled) corrupt-fixture path where TraceEndNs <
// TraceStartNs leaks through resolveTraceRoots. The patch must not
// rewrite DurationMs to a negative integer.
func TestApplyRootMetadata_NegativeDurationIgnored(t *testing.T) {
	t.Parallel()
	summaries := []TraceSummary{
		{TraceID: "a", DurationMs: 42},
	}
	roots := map[string]rootMetadata{
		"a": {ServiceName: "svc", SpanName: "name", TraceDurationNs: -1},
	}
	applyRootMetadata(summaries, roots)
	if summaries[0].DurationMs != 42 {
		t.Errorf("negative TraceDurationNs must be ignored; got DurationMs=%d", summaries[0].DurationMs)
	}
}
