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
	// The L subquery's `?` arg appears exactly once — at the seed position
	// (FROM (<L>) AS _seed). The recursive step no longer re-embeds the
	// seed-trace-id IN subquery (the step JOIN ON `t.TraceId = c.TraceId`
	// already confines `t` to the seed's traces), so the arg is not
	// duplicated. The bare `{}` right side is not a cheap selective leaf, so
	// no candidate-prefilter subquery re-renders L either. Nothing may be
	// swallowed.
	if len(args) != 1 || args[0] != "GET /home" {
		t.Errorf("args = %v, want [GET /home]", args)
	}
}

// TestEmitStructuralJoin_NestedJoinKeyProjection pins the qualifier-
// stripping projection shape that the emitter uses so nested structural
// joins compose without CH 25.8's analyzer rejecting `L.TraceId`
// against an inner subquery whose only matching identifier is
// `R.TraceId`. The projection list MUST include explicit `R.<key> AS
// <key>` aliases for each of (TraceId, SpanId, ParentSpanId). Re-
// emitted naïvely as `SELECT R.*`, the inner subquery output carries
// the `R.` qualifier on every column (verified by the chDB roundtrip
// in test/spec/traceql/multi_hop_chain.txtar; see task #57 for the
// full failing trace).
//
// When ExtraProjectionColumns is empty the projection falls back to
// `R.* EXCEPT (TraceId, SpanId, ParentSpanId)` for the non-key tail.
func TestEmitStructuralJoin_NestedJoinKeyProjection(t *testing.T) {
	t.Parallel()

	// 2-hop direct chain: `(a > b) > c`. The inner StructuralJoin
	// becomes the LEFT side of the outer; the outer references
	// L.TraceId / L.SpanId against that subquery.
	plan := &chplan.StructuralJoin{
		Left: &chplan.StructuralJoin{
			Left:               &chplan.Scan{Table: "otel_traces"},
			Right:              &chplan.Scan{Table: "otel_traces"},
			Op:                 chplan.StructuralChild,
			TraceIDColumn:      "TraceId",
			SpanIDColumn:       "SpanId",
			ParentSpanIDColumn: "ParentSpanId",
		},
		Right:              &chplan.Scan{Table: "otel_traces"},
		Op:                 chplan.StructuralChild,
		TraceIDColumn:      "TraceId",
		SpanIDColumn:       "SpanId",
		ParentSpanIDColumn: "ParentSpanId",
	}
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}

	// Each side projection must expose bare-name join keys so the
	// surrounding subquery wrap doesn't bake the `R.` qualifier into
	// the output column names.
	for _, key := range []string{"TraceId", "SpanId", "ParentSpanId"} {
		want := "R.`" + key + "` AS `" + key + "`"
		if !strings.Contains(sql, want) {
			t.Errorf("nested-structural projection missing bare-alias for %s.\n  want substring: %s\n  got: %s",
				key, want, sql)
		}
	}

	// With ExtraProjectionColumns empty (chplan-direct construction),
	// the `R.* EXCEPT (...)` tail keeps all non-key columns flowing
	// through without duplicating the keys already projected with
	// explicit aliases.
	wantExcept := "R.* EXCEPT (`TraceId`, `SpanId`, `ParentSpanId`)"
	if !strings.Contains(sql, wantExcept) {
		t.Errorf("projection missing `R.* EXCEPT (...)` tail.\n  want substring: %s\n  got: %s",
			wantExcept, sql)
	}

	// Bare `SELECT R.*` (with NO accompanying alias projections) is
	// the precise shape that broke the multi-hop chain — guard against
	// regression to that exact form. We allow `R.* EXCEPT (...)` (which
	// is what the legacy emitter produces) but reject a standalone
	// `R.*` projection that the parent subquery would expand
	// qualifier-first.
	if strings.Contains(sql, "SELECT R.* FROM") {
		t.Errorf("regression: emitter reverted to bare `SELECT R.* FROM ...` (qualifier survives wrap).\n  got: %s",
			sql)
	}
}

// TestEmitStructuralJoin_ExplicitProjection pins the explicit-column
// projection shape the emitter renders when StructuralJoin.
// ExtraProjectionColumns is populated. CH 25.8 drops `R.*`-introduced
// columns from outer-scope resolution when the JOIN sides share column
// names (otel_traces self-join), so the explicit list is what lets the
// Tempo API-layer wrap projection reference (SpanName, Duration,
// Timestamp, ResourceAttributes) without `Unknown identifier` errors.
// See tempo compat report 26098988786 for the failing repro.
func TestEmitStructuralJoin_ExplicitProjection(t *testing.T) {
	t.Parallel()

	extras := []string{"SpanName", "Duration", "Timestamp", "ResourceAttributes"}
	plan := &chplan.StructuralJoin{
		Left:                   &chplan.Scan{Table: "otel_traces"},
		Right:                  &chplan.Scan{Table: "otel_traces"},
		Op:                     chplan.StructuralChild,
		TraceIDColumn:          "TraceId",
		SpanIDColumn:           "SpanId",
		ParentSpanIDColumn:     "ParentSpanId",
		ExtraProjectionColumns: extras,
	}
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}

	// Every extra column must appear as an explicit `R.<col> AS <col>`
	// alias so the wrap subquery exposes it bare-name to the outer
	// scope.
	for _, col := range extras {
		want := "R.`" + col + "` AS `" + col + "`"
		if !strings.Contains(sql, want) {
			t.Errorf("explicit projection missing bare-alias for %s.\n  want substring: %s\n  got: %s",
				col, want, sql)
		}
	}

	// The `R.* EXCEPT (...)` fallback must NOT be present — the
	// explicit list replaces it entirely.
	if strings.Contains(sql, "R.* EXCEPT") {
		t.Errorf("explicit projection should not emit `R.* EXCEPT` tail.\n  got: %s", sql)
	}
}
