package tempo

import (
	"context"
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

	// Empty parent-span-id literal must appear so the OR-chain matches
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
	// 16-char zero form must appear so the OR-chain also matches
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
