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
	// Closure CTE names carry a per-emit sequence suffix
	// (`_struct_closure_<n>` / `_struct_closure_inv_<n>`) so nested
	// structural joins don't collide; match the prefixes. The forward
	// closure name is a prefix of the inverse one, so count the inverse
	// first and subtract it from the forward count.
	invCount := strings.Count(sql, "WITH RECURSIVE _struct_closure_inv_")
	fwdCount := strings.Count(sql, "WITH RECURSIVE _struct_closure_") - invCount
	if fwdCount != 1 {
		t.Errorf("forward closure CTE must render exactly once, got %d:\n%s", fwdCount, sql)
	}
	if invCount != 1 {
		t.Errorf("inverse closure CTE must render exactly once, got %d:\n%s", invCount, sql)
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

// drilldownUnionInput builds the Drilldown structure-tab input shape:
// `({ParentSpanId=”} &>> {SpanKind=Server}) || ({ParentSpanId=”})`.
func drilldownUnionInput() *chplan.SetOperation {
	return &chplan.SetOperation{
		Left:          nsStructuralJoin(chplan.StructuralUnionDescendant),
		Right:         nsFilterScan("ParentSpanId", ""),
		Op:            chplan.SetUnion,
		TraceIDColumn: "TraceId",
		SpanIDColumn:  "SpanId",
	}
}

// TestNestedSetAnnotate_TraceLimit_BoundsAnchorScope pins the bounded
// numbering: a non-zero TraceLimit narrows the anchor's trace-id IN scope to
// the SELF-CONTAINED top-N root subquery (`SELECT TraceId FROM otel_traces
// WHERE ParentSpanId=” GROUP BY TraceId ORDER BY min(Timestamp) DESC,
// TraceId ASC LIMIT N`) — exactly the set /api/search's TruncateSummaries
// keeps (newest by start time, ties by TraceId). It is self-contained (no
// `IN <input scope>` conjunct) so the SAME frag can gate the row-source
// leaves (chplan.BoundedTraceScope) without an emit cycle; the
// numbering==gate identity is pinned in
// TestNestedSetAnnotate_TraceLimit_NumberingMatchesLeafGate.
func TestNestedSetAnnotate_TraceLimit_BoundsAnchorScope(t *testing.T) {
	t.Parallel()
	n := nsAnnotateOver(drilldownUnionInput())
	n.TraceLimit = 200
	sql, _, err := chsql.Emit(context.Background(), n)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	// The anchor scope is the self-contained newest-N root selection. The
	// ordering (DESC, TraceId ASC) and the GROUP BY min(Timestamp) match
	// sortSummariesStartDesc / toTraceSummaries' StartTimeUnixNano.
	wantBound := "WHERE `ParentSpanId` = '' AND `TraceId` IN (SELECT `TraceId` FROM `otel_traces` WHERE `ParentSpanId` = '' GROUP BY `TraceId` ORDER BY min(`Timestamp`) DESC, `TraceId` LIMIT 200)"
	if !strings.Contains(sql, wantBound) {
		t.Errorf("bounded anchor scope missing;\nwant substring: %s\ngot:\n%s", wantBound, sql)
	}
	// The bound is a single extra subquery over the root scan — the
	// recursive numbering CTE still renders exactly once.
	if got := strings.Count(sql, "WITH RECURSIVE _cerberus_ns_paths"); got != 1 {
		t.Errorf("numbering CTE must render exactly once, got %d:\n%s", got, sql)
	}
}

// TestNestedSetAnnotate_TraceLimit_NumberingMatchesLeafGate pins the
// load-bearing identity: the top-N subquery the numbering anchor scope emits
// is BYTE-IDENTICAL to the one a leaf-scan BoundedTraceScope gate emits. If
// they ever drift, the numbering would number a different trace set than the
// row source produces, stranding kept rows at the 0/0/0 LEFT-JOIN default.
func TestNestedSetAnnotate_TraceLimit_NumberingMatchesLeafGate(t *testing.T) {
	t.Parallel()
	const topN = "(SELECT `TraceId` FROM `otel_traces` WHERE `ParentSpanId` = '' GROUP BY `TraceId` ORDER BY min(`Timestamp`) DESC, `TraceId` LIMIT 200)"

	// Numbering side: a bounded NestedSetAnnotate's anchor scope.
	n := nsAnnotateOver(drilldownUnionInput())
	n.TraceLimit = 200
	nsSQL, _, err := chsql.Emit(context.Background(), n)
	if err != nil {
		t.Fatalf("Emit numbering: %v", err)
	}
	if !strings.Contains(nsSQL, "`TraceId` IN "+topN) {
		t.Errorf("numbering anchor scope does not emit the expected top-N subquery %s\ngot:\n%s", topN, nsSQL)
	}

	// Gate side: a leaf Filter carrying a BoundedTraceScope, emitted directly.
	gated := &chplan.Filter{
		Input: &chplan.Scan{Table: "otel_traces"},
		Predicate: &chplan.BoundedTraceScope{
			SpansTable:         "otel_traces",
			TraceIDColumn:      "TraceId",
			ParentSpanIDColumn: "ParentSpanId",
			TimestampColumn:    "Timestamp",
			TraceLimit:         200,
		},
	}
	gateSQL, _, err := chsql.Emit(context.Background(), gated)
	if err != nil {
		t.Fatalf("Emit gate: %v", err)
	}
	if !strings.Contains(gateSQL, "`TraceId` IN "+topN) {
		t.Errorf("leaf gate does not emit the expected top-N subquery %s\ngot:\n%s", topN, gateSQL)
	}
}

// TestNestedSetAnnotate_TraceLimit_ZeroUnbounded pins the no-churn
// invariant: TraceLimit == 0 (every non-search caller — metrics, tests,
// property harness) emits the exact unbounded SQL, with no LIMIT / GROUP BY
// / ORDER BY injected into the anchor scope.
func TestNestedSetAnnotate_TraceLimit_ZeroUnbounded(t *testing.T) {
	t.Parallel()
	n := nsAnnotateOver(drilldownUnionInput()) // TraceLimit defaults to 0
	sql, _, err := chsql.Emit(context.Background(), n)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	// The anchor scope is the bare UNION-ALL superset, no newest-N wrap.
	wantUnbounded := "WHERE `ParentSpanId` = '' AND `TraceId` IN (((SELECT `TraceId` FROM (SELECT * FROM `otel_traces` WHERE (`ParentSpanId` = ?))) UNION ALL (SELECT `TraceId` FROM (SELECT * FROM `otel_traces` WHERE (`SpanKind` = ?)))) UNION ALL (SELECT `TraceId` FROM (SELECT * FROM `otel_traces` WHERE (`ParentSpanId` = ?)))) UNION ALL"
	if !strings.Contains(sql, wantUnbounded) {
		t.Errorf("unbounded anchor scope changed;\nwant substring: %s\ngot:\n%s", wantUnbounded, sql)
	}
	if strings.Contains(sql, "ORDER BY min(`Timestamp`)") {
		t.Errorf("TraceLimit=0 must not inject the newest-N ordering:\n%s", sql)
	}
}
