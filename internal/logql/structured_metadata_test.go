package logql

import (
	"context"
	"strings"
	"testing"

	"github.com/grafana/loki/v3/pkg/logql/syntax"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// findCoalescingLookup reports whether the structured-over-stream
// coalescing resolution for `key` is present in `root`: an `if(...)`
// FuncCall somewhere in the tree whose subtree carries a
// `LogAttributes[<key>]` MapAccess (accepting the dotted OTel candidate
// for underscore-bearing keys, e.g. `query.kind` for `query_kind`).
func findCoalescingLookup(root chplan.Expr, key string) bool {
	want := map[string]bool{key: true}
	for _, c := range otelCandidatesForTest(key) {
		want[c] = true
	}
	sawIf := false
	sawLALookup := false
	walkExprTree(root, func(e chplan.Expr) {
		if fc, ok := e.(*chplan.FuncCall); ok && fc.Name == "if" {
			sawIf = true
		}
		ma, ok := e.(*chplan.MapAccess)
		if !ok {
			return
		}
		col, ok := ma.Map.(*chplan.ColumnRef)
		if !ok || col.Name != "LogAttributes" {
			return
		}
		kl, ok := ma.Key.(*chplan.LitString)
		if !ok || !want[kl.V] {
			return
		}
		sawLALookup = true
	})
	return sawIf && sawLALookup
}

// otelCandidatesForTest returns the dotted OTel candidate(s) for an
// underscore-bearing key so the coalescing matcher accepts either the
// underscored or dotted form (e.g. `query.kind` for `query_kind`).
func otelCandidatesForTest(key string) []string {
	switch key {
	case "query_kind":
		return []string{"query.kind"}
	default:
		return nil
	}
}

// TestStructuredOrStreamLookup_BarePrecedence pins the coalescing shape
// for a non-dotted key against the bare ResourceAttributes column:
// structured metadata (LogAttributes) preferred, stream label
// (ResourceAttributes) as fallback.
func TestStructuredOrStreamLookup_BarePrecedence(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelLogs()

	got := structuredOrStreamLookup(s, "env")
	fc, ok := got.(*chplan.FuncCall)
	if !ok || fc.Name != "if" || len(fc.Args) != 3 {
		t.Fatalf("want if(...) coalescing call, got %#v", got)
	}
	// guard: mapContains(LogAttributes, "env")
	guard, ok := fc.Args[0].(*chplan.FuncCall)
	if !ok || guard.Name != "mapContains" {
		t.Fatalf("want mapContains guard, got %#v", fc.Args[0])
	}
	// then: LogAttributes["env"]
	then, ok := fc.Args[1].(*chplan.MapAccess)
	if !ok {
		t.Fatalf("then-branch is %T, want MapAccess", fc.Args[1])
	}
	if col, _ := then.Map.(*chplan.ColumnRef); col == nil || col.Name != s.AttributesColumn {
		t.Errorf("then-branch reads %v, want %s", then.Map, s.AttributesColumn)
	}
	// else: ResourceAttributes["env"]
	els, ok := fc.Args[2].(*chplan.MapAccess)
	if !ok {
		t.Fatalf("else-branch is %T, want MapAccess", fc.Args[2])
	}
	if col, _ := els.Map.(*chplan.ColumnRef); col == nil || col.Name != s.ResourceAttributesColumn {
		t.Errorf("else-branch reads %v, want %s", els.Map, s.ResourceAttributesColumn)
	}
}

// TestStructuredOrStreamLookup_NoStructuredColumn verifies a custom
// schema with no structured-metadata column falls back to the plain
// stream-label lookup (regression-safe, no spurious LogAttributes ref).
func TestStructuredOrStreamLookup_NoStructuredColumn(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelLogs()
	s.AttributesColumn = ""

	got := structuredOrStreamLookup(s, "env")
	ma, ok := got.(*chplan.MapAccess)
	if !ok {
		t.Fatalf("want bare MapAccess (no coalesce), got %T", got)
	}
	if col, _ := ma.Map.(*chplan.ColumnRef); col == nil || col.Name != s.ResourceAttributesColumn {
		t.Errorf("reads %v, want %s", ma.Map, s.ResourceAttributesColumn)
	}
}

// TestStructuredOrStreamLookupOnMap_ParserMerged verifies the
// parser-merged branch keeps the parsed/stream map as the dominant
// source (parsed > structured > stream) and only falls back to
// structured metadata when the live map lacks the key.
func TestStructuredOrStreamLookupOnMap_ParserMerged(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelLogs()
	// A mapConcat carrier stands in for a `| logfmt` merged labels map.
	merged := &chplan.FuncCall{
		Name: "mapConcat",
		Args: []chplan.Expr{
			&chplan.ColumnRef{Name: s.ResourceAttributesColumn},
			&chplan.ColumnRef{Name: "extracted"},
		},
	}
	got := structuredOrStreamLookupOnMap(s, merged, "status")
	fc, ok := got.(*chplan.FuncCall)
	if !ok || fc.Name != "if" || len(fc.Args) != 3 {
		t.Fatalf("want if(...) coalescing call, got %#v", got)
	}
	// guard must be mapContains on the LIVE merged map (parsed+stream).
	guard, ok := fc.Args[0].(*chplan.FuncCall)
	if !ok || guard.Name != "mapContains" {
		t.Fatalf("want mapContains guard, got %#v", fc.Args[0])
	}
	if !guard.Args[0].Equal(merged) {
		t.Errorf("guard map is %v, want the live merged map", guard.Args[0])
	}
	// else-branch falls back to LogAttributes["status"].
	els, ok := fc.Args[2].(*chplan.MapAccess)
	if !ok {
		t.Fatalf("else-branch is %T, want MapAccess", fc.Args[2])
	}
	if col, _ := els.Map.(*chplan.ColumnRef); col == nil || col.Name != s.AttributesColumn {
		t.Errorf("else-branch reads %v, want %s (structured metadata)", els.Map, s.AttributesColumn)
	}
}

// TestStructuredMetadata_PipelineFilterResolvesLogAttributes checks the
// end-to-end lowering: a bare `| query_kind="Select"` filter resolves
// `query_kind` via the coalescing LogAttributes-preferred lookup.
func TestStructuredMetadata_PipelineFilterResolvesLogAttributes(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelLogs()
	expr, err := syntax.ParseExpr(`{service_name="clickhouse"} | query_kind="Select"`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	plan, err := lower(expr, s, lowerCtx{})
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	filt, ok := plan.(*chplan.Filter)
	if !ok {
		t.Fatalf("top is %T, want *chplan.Filter", plan)
	}
	if !findCoalescingLookup(filt.Predicate, "query_kind") {
		t.Errorf("expected a LogAttributes-preferred coalescing lookup for query_kind; got %v", filt.Predicate)
	}
}

// TestStructuredMetadata_SelectorUnchanged is the parity guard: a stream
// SELECTOR `{tier="gold"}` must resolve against ResourceAttributes ONLY
// (the index) — never the LogAttributes structured-metadata map.
func TestStructuredMetadata_SelectorUnchanged(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelLogs()
	expr, err := syntax.ParseExpr(`{tier="gold"}`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	plan, err := lower(expr, s, lowerCtx{})
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	// No LogAttributes reference may appear anywhere in the selector
	// lowering — the selector matches the stream index only.
	sawLogAttrs := false
	walkChplanExpr(plan, func(e chplan.Expr) {
		if col, ok := e.(*chplan.ColumnRef); ok && col.Name == s.AttributesColumn {
			sawLogAttrs = true
		}
	})
	if sawLogAttrs {
		t.Errorf("stream selector lowered with a LogAttributes reference; selectors must be index-only")
	}
}

// TestStructuredMetadata_GroupByInflatesIdentity verifies an outer
// `sum by (query_kind)` inflates the structured-metadata key into the
// inner range-aggregation identity map (so the post-RangeWindow outer
// aggregate can read it back) with the coalescing precedence.
func TestStructuredMetadata_GroupByInflatesIdentity(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelLogs()
	expr, err := syntax.ParseExpr(`sum by (query_kind) (count_over_time({service_name="clickhouse"}[5m]))`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	plan, err := lower(expr, s, lowerCtx{})
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	// Emit SQL and assert the inner range-aggregation identity map
	// inflates `query_kind` via the coalescing lookup. The emitted SQL
	// is the unambiguous source of truth for the whole plan (the test
	// node-walk helpers don't descend through RangeWindow/Aggregate).
	// chsql.Emit parameterises string literals as `?`, so assert on the
	// structural coalescing shape: an `if(mapContains(\`LogAttributes\`,
	// ?), ...)` reading the structured-metadata column.
	sqlStr, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sqlStr, "if(mapContains(`LogAttributes`,") {
		t.Errorf("emitted SQL has no LogAttributes coalescing inflation for the group key:\n%s", sqlStr)
	}
	if !strings.Contains(sqlStr, "`LogAttributes`[?]") {
		t.Errorf("emitted SQL never reads a LogAttributes value:\n%s", sqlStr)
	}
}

// TestStructuredOuterByKeys_FiltersTopLevelAndDetectedLevel pins the
// outer-by key classifier: top-level columns and the detected_level
// family are excluded (handled elsewhere); structured/stream keys pass
// through, deduplicated and order-preserved.
func TestStructuredOuterByKeys_FiltersTopLevelAndDetectedLevel(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelLogs()
	in := []string{"query_kind", "ServiceName", "level", "detected_level", "query_kind", "region"}
	got := structuredOuterByKeys(in, s)
	want := []string{"query_kind", "region"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
