package chsql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// TestEmitStructuralUnion_IdentityDedup pins every `&`-prefixed structural
// op to identity dedup — `SELECT … FROM (<R arm> UNION ALL <L arm>) LIMIT 1
// BY (TraceId, SpanId)` — and forbids the full-row `UNION DISTINCT` it
// replaced.
//
// A ClickHouse Map compares POSITIONALLY over its (keys, values) arrays, so
// map('job','api','dc','eu') and map('dc','eu','job','api') are unequal
// despite carrying the same attribute set. Both union arms project
// ResourceAttributes and SpanAttributes, so a full-row UNION DISTINCT let two
// physical rows of ONE span — the same span redelivered under a different
// OTLP attribute wire order — both survive: the span came back twice in the
// spanset. Identity dedup never looks at the Map columns, so it is immune by
// construction.
//
// The TXTAR round-trips (test/spec/traceql/structural_union_*_dup_key_order)
// pin the user-visible row set; this pins the SQL SHAPE that produces it, so
// a regression to UNION DISTINCT cannot be regenerated away by a golden
// refresh. Sibling of TestEmitNode_SetOperation_Union, which pins the same
// shape for `||`.
func TestEmitStructuralUnion_IdentityDedup(t *testing.T) {
	t.Parallel()

	for _, op := range []chplan.StructuralOp{
		chplan.StructuralUnionChild,
		chplan.StructuralUnionParent,
		chplan.StructuralUnionDescendant,
		chplan.StructuralUnionAncestor,
		chplan.StructuralUnionSibling,
	} {
		t.Run(string(op), func(t *testing.T) {
			t.Parallel()

			plan := &chplan.StructuralJoin{
				Left:               &chplan.Scan{Table: "otel_traces"},
				Right:              &chplan.Scan{Table: "otel_traces"},
				Op:                 op,
				TraceIDColumn:      "TraceId",
				SpanIDColumn:       "SpanId",
				ParentSpanIDColumn: "ParentSpanId",
				// The wide projection the Tempo head always lowers with — it is
				// what puts the two Map columns into the dedup tuple.
				ExtraProjectionColumns: []string{"SpanName", "ResourceAttributes", "SpanAttributes"},
			}
			sql, _, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
			if strings.Contains(sql, "UNION DISTINCT") {
				t.Errorf("%s still glues its arms with the full-row UNION DISTINCT, "+
					"which double-returns a span redelivered under a different Map key order: %s", op, sql)
			}
			if !strings.Contains(sql, "UNION ALL") {
				t.Errorf("%s did not glue its arms with UNION ALL: %s", op, sql)
			}
			if !strings.Contains(sql, "LIMIT 1 BY `TraceId`, `SpanId`") {
				t.Errorf("%s missing identity dedup 'LIMIT 1 BY `TraceId`, `SpanId`': %s", op, sql)
			}
			// The outer projection enumerates the arms' bare aliases rather than
			// `*`, so the union's column names stay resolvable to a wrapping
			// subquery (the CH 25.8 analyzer constraint structuralProjectionFrags
			// documents).
			for _, col := range []string{
				"SELECT `TraceId`, `SpanId`, `ParentSpanId`, `SpanName`, `ResourceAttributes`, `SpanAttributes` FROM (",
			} {
				if !strings.Contains(sql, col) {
					t.Errorf("%s outer projection is not the arms' bare alias list (want prefix %q): %s", op, col, sql)
				}
			}
		})
	}
}

// TestEmitStructuralUnion_NonUnionOpsUndeduped confirms the identity dedup is
// scoped to the `&` forms only: a plain `>` / `>>` / `~` structural join has a
// single arm and emits no union wrapper at all, so adding `LIMIT 1 BY` there
// would be a silent row-dropping behaviour change rather than a dedup.
func TestEmitStructuralUnion_NonUnionOpsUndeduped(t *testing.T) {
	t.Parallel()

	for _, op := range []chplan.StructuralOp{
		chplan.StructuralChild,
		chplan.StructuralParent,
		chplan.StructuralDescendant,
		chplan.StructuralAncestor,
		chplan.StructuralSibling,
	} {
		t.Run(string(op), func(t *testing.T) {
			t.Parallel()

			plan := &chplan.StructuralJoin{
				Left:               &chplan.Scan{Table: "otel_traces"},
				Right:              &chplan.Scan{Table: "otel_traces"},
				Op:                 op,
				TraceIDColumn:      "TraceId",
				SpanIDColumn:       "SpanId",
				ParentSpanIDColumn: "ParentSpanId",
			}
			sql, _, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
			if strings.Contains(sql, "LIMIT 1 BY") {
				t.Errorf("%s is a single-arm relation and must not carry the union's identity dedup: %s", op, sql)
			}
		})
	}
}
