package chsql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// TestEmitStructuralRecursive_DescendantOrientation pins the recursive
// step join direction for `>>`: child spans (otel_traces.ParentSpanId)
// join the closure's SpanId (one level down per iteration).
func TestEmitStructuralRecursive_DescendantOrientation(t *testing.T) {
	t.Parallel()

	plan := &chplan.StructuralJoin{
		Left:               &chplan.Scan{Table: "otel_traces"},
		Right:              &chplan.Scan{Table: "otel_traces"},
		Op:                 chplan.StructuralDescendant,
		TraceIDColumn:      "TraceId",
		SpanIDColumn:       "SpanId",
		ParentSpanIDColumn: "ParentSpanId",
	}
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	want := "t.`ParentSpanId` = c.`SpanId`"
	if !strings.Contains(sql, want) {
		t.Errorf("descendant join key missing.\n  want substring: %s\n  got: %s", want, sql)
	}
}

// TestEmitStructuralRecursive_AncestorOrientation pins the mirror case
// for `<<`: parent spans (otel_traces.SpanId) join the closure's
// ParentSpanId (one level up per iteration).
func TestEmitStructuralRecursive_AncestorOrientation(t *testing.T) {
	t.Parallel()

	plan := &chplan.StructuralJoin{
		Left:               &chplan.Scan{Table: "otel_traces"},
		Right:              &chplan.Scan{Table: "otel_traces"},
		Op:                 chplan.StructuralAncestor,
		TraceIDColumn:      "TraceId",
		SpanIDColumn:       "SpanId",
		ParentSpanIDColumn: "ParentSpanId",
	}
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	want := "t.`SpanId` = c.`ParentSpanId`"
	if !strings.Contains(sql, want) {
		t.Errorf("ancestor join key missing.\n  want substring: %s\n  got: %s", want, sql)
	}
}

// TestEmitStructuralRecursive_AnchorExcluded confirms the final
// SELECT filters `_depth > 0` so the anchor row (L itself) isn't
// returned as a descendant/ancestor of L. TraceQL semantics require
// R to be strictly downstream / upstream of L.
func TestEmitStructuralRecursive_AnchorExcluded(t *testing.T) {
	t.Parallel()

	plan := &chplan.StructuralJoin{
		Left:               &chplan.Scan{Table: "otel_traces"},
		Right:              &chplan.Scan{Table: "otel_traces"},
		Op:                 chplan.StructuralDescendant,
		TraceIDColumn:      "TraceId",
		SpanIDColumn:       "SpanId",
		ParentSpanIDColumn: "ParentSpanId",
	}
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, "WHERE _depth > 0") {
		t.Errorf("anchor not excluded; emitted SQL missing 'WHERE _depth > 0':\n  %s", sql)
	}
}

// TestEmitStructuralRecursive_PreservesLeftArgs confirms the recursive
// emitter still threads the L subquery's positional `?` args at the
// seed position (rather than swallowing them).
func TestEmitStructuralRecursive_PreservesLeftArgs(t *testing.T) {
	t.Parallel()

	plan := &chplan.StructuralJoin{
		Left: &chplan.Filter{
			Input: &chplan.Scan{Table: "otel_traces"},
			Predicate: &chplan.Binary{
				Op:    chplan.OpEq,
				Left:  &chplan.ColumnRef{Name: "SpanName"},
				Right: &chplan.LitString{V: "GET /home"},
			},
		},
		Right:              &chplan.Scan{Table: "otel_traces"},
		Op:                 chplan.StructuralDescendant,
		TraceIDColumn:      "TraceId",
		SpanIDColumn:       "SpanId",
		ParentSpanIDColumn: "ParentSpanId",
	}
	_, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if len(args) != 1 || args[0] != "GET /home" {
		t.Errorf("args = %v, want [GET /home]", args)
	}
}
