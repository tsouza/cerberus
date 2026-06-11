package chsql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// The NestedSetAnnotate emitter must render its input subquery exactly
// once (the FROM arm) and scope the recursive anchor's trace-id IN by
// a cheap plan-derived superset (traceScopeFrag) instead of a second
// full rendering. A second rendering doubles the evaluation of
// expensive inputs — the structural-join + `||` Drilldown structure
// tab query blew past the #756 1 GiB per-query memory cap on exactly
// that doubled closure walk (PR #775's documented caveat).

func nsAnnotateOver(input chplan.Node) *chplan.NestedSetAnnotate {
	return &chplan.NestedSetAnnotate{
		Input:              input,
		SpansTable:         "otel_traces",
		TraceIDColumn:      "TraceId",
		SpanIDColumn:       "SpanId",
		ParentSpanIDColumn: "ParentSpanId",
		TimestampColumn:    "Timestamp",
	}
}

func nsFilterScan(col, val string) chplan.Node {
	return &chplan.Filter{
		Input: &chplan.Scan{Table: "otel_traces"},
		Predicate: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: col},
			Right: &chplan.LitString{V: val},
		},
	}
}

func nsStructuralJoin(op chplan.StructuralOp) *chplan.StructuralJoin {
	return &chplan.StructuralJoin{
		Left:               nsFilterScan("ParentSpanId", ""),
		Right:              nsFilterScan("SpanKind", "Server"),
		Op:                 op,
		TraceIDColumn:      "TraceId",
		SpanIDColumn:       "SpanId",
		ParentSpanIDColumn: "ParentSpanId",
	}
}

// TestNestedSetAnnotate_StructuralInput_RenderedOnce pins the
// dedup contract for a bare structural-join input: the recursive
// closure CTE appears exactly once (FROM arm), and the anchor scope
// is the UNION ALL of the two arm scans.
func TestNestedSetAnnotate_StructuralInput_RenderedOnce(t *testing.T) {
	t.Parallel()
	sql, _, err := chsql.Emit(context.Background(), nsAnnotateOver(nsStructuralJoin(chplan.StructuralDescendant)))
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if got := strings.Count(sql, "WITH RECURSIVE _struct_closure"); got != 1 {
		t.Errorf("structural closure CTE must render exactly once, got %d:\n%s", got, sql)
	}
	wantScope := "IN ((SELECT `TraceId` FROM (SELECT * FROM `otel_traces` WHERE (`ParentSpanId` = ?))) UNION ALL (SELECT `TraceId` FROM (SELECT * FROM `otel_traces` WHERE (`SpanKind` = ?))))"
	if !strings.Contains(sql, wantScope) {
		t.Errorf("anchor trace scope must be the UNION ALL of the arm scans;\nwant substring: %s\ngot:\n%s", wantScope, sql)
	}
}

// TestNestedSetAnnotate_UnionOfStructural_RenderedOnce pins the exact
// Drilldown structure-tab plan shape — SetOperation(StructuralJoin,
// Project(Filter(Scan))) — and asserts both closure CTE flavours of
// the `&>>` arm render exactly once each.
func TestNestedSetAnnotate_UnionOfStructural_RenderedOnce(t *testing.T) {
	t.Parallel()
	plain := &chplan.Project{
		Input: nsFilterScan("ParentSpanId", ""),
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: "TraceId"}},
			{Expr: &chplan.ColumnRef{Name: "SpanId"}},
			{Expr: &chplan.ColumnRef{Name: "ParentSpanId"}},
		},
	}
	input := &chplan.SetOperation{
		Left:          nsStructuralJoin(chplan.StructuralUnionDescendant),
		Right:         plain,
		Op:            chplan.SetUnion,
		TraceIDColumn: "TraceId",
		SpanIDColumn:  "SpanId",
	}
	sql, _, err := chsql.Emit(context.Background(), nsAnnotateOver(input))
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	for _, cte := range []string{"WITH RECURSIVE _struct_closure ", "WITH RECURSIVE _struct_closure_inv "} {
		if got := strings.Count(sql, cte); got != 1 {
			t.Errorf("%q must render exactly once, got %d:\n%s", cte, got, sql)
		}
	}
	// The Project passes TraceId through bare, so the `||` arm's scope
	// recurses past it down to the Filter(Scan) leaf.
	wantPlainScope := "UNION ALL (SELECT `TraceId` FROM (SELECT * FROM `otel_traces` WHERE (`ParentSpanId` = ?))))"
	if !strings.Contains(sql, wantPlainScope) {
		t.Errorf("plain `||` arm scope must recurse through the bare-TraceId Project;\nwant substring: %s\ngot:\n%s", wantPlainScope, sql)
	}
}

// TestNestedSetAnnotate_ProjectWithoutBareTraceID_FallsBack pins the
// conservative fallback: a Project that renames TraceId away cannot be
// recursed through (the scope SELECT needs the column by name), so the
// scope wraps the Project node itself.
func TestNestedSetAnnotate_ProjectWithoutBareTraceID_FallsBack(t *testing.T) {
	t.Parallel()
	renamed := &chplan.Project{
		Input: nsFilterScan("SpanKind", "Server"),
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: "TraceId"}, Alias: "tid"},
			{Expr: &chplan.ColumnRef{Name: "SpanId"}},
		},
	}
	sql, _, err := chsql.Emit(context.Background(), nsAnnotateOver(renamed))
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	wantScope := "IN (SELECT `TraceId` FROM (SELECT `TraceId` AS `tid`, `SpanId` FROM"
	if !strings.Contains(sql, wantScope) {
		t.Errorf("aliased-away TraceId must fall back to wrapping the Project;\nwant substring: %s\ngot:\n%s", wantScope, sql)
	}
}

// TestNestedSetAnnotate_LimitInput_ScopeDropsLimit pins the Limit
// recursion: dropping a LIMIT only widens the trace scope (superset
// stays correct), so the scope reads the Limit's input directly.
func TestNestedSetAnnotate_LimitInput_ScopeDropsLimit(t *testing.T) {
	t.Parallel()
	limited := &chplan.Limit{Input: nsFilterScan("ParentSpanId", ""), Count: 5}
	sql, _, err := chsql.Emit(context.Background(), nsAnnotateOver(limited))
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	wantScope := "IN (SELECT `TraceId` FROM (SELECT * FROM `otel_traces` WHERE (`ParentSpanId` = ?)))"
	if !strings.Contains(sql, wantScope) {
		t.Errorf("Limit input's scope must recurse past the LIMIT;\nwant substring: %s\ngot:\n%s", wantScope, sql)
	}
	if got := strings.Count(sql, "LIMIT 5"); got != 1 {
		t.Errorf("the FROM arm must keep its LIMIT exactly once, got %d:\n%s", got, sql)
	}
}
