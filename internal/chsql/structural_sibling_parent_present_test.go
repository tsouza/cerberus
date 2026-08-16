package chsql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// siblingPlan returns a `~`-family structural join over the spans table.
// op selects the flavour (`~` / `!~` / `&~`), all three of which read their
// L side through the same root-reachability gate.
func siblingPlan(op chplan.StructuralOp) *chplan.StructuralJoin {
	return &chplan.StructuralJoin{
		Left:               &chplan.Scan{Table: "otel_traces"},
		Right:              &chplan.Scan{Table: "otel_traces"},
		Op:                 op,
		TraceIDColumn:      "TraceId",
		SpanIDColumn:       "SpanId",
		ParentSpanIDColumn: "ParentSpanId",
	}
}

// TestEmitSibling_GateRequiresRootReachability pins the full rootedness gate
// in every flavour of the operator.
//
// Upstream answers `~` from the nested-set numbering: two spans are siblings
// when their nested-set parents agree. Tempo numbers only spans reachable from
// a real root, so a shallow "parent row exists" probe is insufficient: an
// orphaned parent and its children all remain unnumbered. The recursive anchor,
// parent-child edge, and final tuple-membership gate are each load-bearing.
func TestEmitSibling_GateRequiresRootReachability(t *testing.T) {
	t.Parallel()

	for _, op := range []chplan.StructuralOp{
		chplan.StructuralSibling,
		chplan.StructuralNotSibling,
		chplan.StructuralUnionSibling,
	} {
		t.Run(string(op), func(t *testing.T) {
			t.Parallel()

			sql, _, err := chsql.Emit(context.Background(), siblingPlan(op))
			if err != nil {
				t.Fatalf("Emit(%s): %v", op, err)
			}
			for _, want := range []string{
				"WITH RECURSIVE _struct_rooted_",
				"WHERE `ParentSpanId` = '' AND `TraceId` IN",
				"t.`TraceId` = c.`TraceId` AND t.`ParentSpanId` = c.`SpanId`",
				"WHERE (`TraceId`, `SpanId`) IN (WITH RECURSIVE _struct_rooted_",
			} {
				if !strings.Contains(sql, want) {
					t.Errorf("%s: rooted sibling gate missing %q.\nSQL: %s", op, want, sql)
				}
			}
		})
	}
}

// TestEmitSibling_RootedWalkIsTraceScoped pins the recursive walk's anchor
// bound. An unscoped anchor would traverse the window's whole span set to
// answer a question about the L side; the emitter derives a trace superset
// from that operand and carries it into the physical root scan.
func TestEmitSibling_RootedWalkIsTraceScoped(t *testing.T) {
	t.Parallel()

	sql, _, err := chsql.Emit(context.Background(), siblingPlan(chplan.StructuralSibling))
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, "FROM `otel_traces` WHERE `ParentSpanId` = '' AND `TraceId` IN (SELECT `TraceId` FROM") {
		t.Fatalf("rooted walk must be scoped to the L side's traces:\n%s", sql)
	}
}

// TestEmitSibling_RootedWalkCarriesWindowAndPhaseB pins that the recursive
// walk folds the two optional bounds the plan can carry. Both are pure pruning:
// a window-stamped node must partition-prune the walk, and a phase-B node must
// granule-prune it to the top-N traces. Either one dropped turns a bounded walk
// into a wide read that no result-set assertion would notice.
func TestEmitSibling_RootedWalkCarriesWindowAndPhaseB(t *testing.T) {
	t.Parallel()

	plan := siblingPlan(chplan.StructuralSibling)
	plan.TimestampColumn = "Timestamp"
	plan.WindowStartNano = 1_700_000_000_000_000_000
	plan.WindowEndNano = 1_700_003_600_000_000_000
	plan.TraceIDRestriction = []string{"aabb", "ccdd"}

	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	for _, want := range []string{
		"`Timestamp` >= fromUnixTimestamp64Nano(1700000000000000000)",
		"`Timestamp` <= fromUnixTimestamp64Nano(1700003600000000000)",
		"`TraceId` IN ('aabb', 'ccdd')",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("rooted walk missing bound %q.\nSQL: %s", want, sql)
		}
	}
}

// TestEmitSibling_UnwindowedWalkStaysBare is the complement: a plan carrying
// neither bound must render the walk with the trace scope alone, so the
// assertions above cannot be satisfied by an emitter that always splices a
// window in.
func TestEmitSibling_UnwindowedWalkStaysBare(t *testing.T) {
	t.Parallel()

	sql, _, err := chsql.Emit(context.Background(), siblingPlan(chplan.StructuralSibling))
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if strings.Contains(sql, "fromUnixTimestamp64Nano") {
		t.Fatalf("an unwindowed sibling plan must not invent a window bound:\n%s", sql)
	}
}
