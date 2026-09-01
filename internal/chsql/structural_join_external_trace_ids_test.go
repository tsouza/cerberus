package chsql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chsql"
)

// TestEmitStructuralRecursive_AnchorCarriesExternalTraceIDTable pins the
// external-table sibling of TestEmitStructuralRecursive_AnchorCarriesTraceIDRestriction
// (structural_join_anchor_mutation_test.go): once TraceIDExternalTable is set
// instead of TraceIDRestriction, the closure's anchor seed WHERE renders the
// subquery form `IN (SELECT ... FROM ...)` — chclient.WithExternalTraceIDs's
// counterpart on the query's context — never the literal list (issue #2783).
func TestEmitStructuralRecursive_AnchorCarriesExternalTraceIDTable(t *testing.T) {
	t.Parallel()

	plan := structuralClosurePlan()
	plan.TraceIDExternalTable = "cerberus_phase_b_trace_ids"

	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, "AS _seed WHERE `TraceId` IN (SELECT `TraceId` FROM `cerberus_phase_b_trace_ids`)") {
		t.Fatalf("external trace-id table reference missing from the anchor seed:\n%s", sql)
	}
	if strings.Contains(sql, "IN ('") {
		t.Fatalf("external-table plan must not ALSO splice a literal IN list:\n%s", sql)
	}
}

// TestEmitStructuralRecursive_TraceIDRestrictionWinsOverExternalTable pins
// traceIDRestrictionFrag's documented precedence: TraceIDRestriction, when
// non-empty, renders the literal form even if TraceIDExternalTable is ALSO
// set. restrictStructural never actually sets both (they are mutually
// exclusive by construction), but the emitter's own precedence is worth
// pinning directly rather than trusting the one caller to never violate it.
func TestEmitStructuralRecursive_TraceIDRestrictionWinsOverExternalTable(t *testing.T) {
	t.Parallel()

	plan := structuralClosurePlan()
	plan.TraceIDRestriction = []string{"aabb"}
	plan.TraceIDExternalTable = "cerberus_phase_b_trace_ids"

	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, "AS _seed WHERE `TraceId` IN ('aabb')") {
		t.Fatalf("literal restriction should win when both fields are set:\n%s", sql)
	}
	if strings.Contains(sql, "cerberus_phase_b_trace_ids") {
		t.Fatalf("external table must not ALSO be referenced when TraceIDRestriction wins:\n%s", sql)
	}
}

// TestEmitStructuralRecursive_InverseAnchorCarriesExternalTraceIDTable mirrors
// TestEmitStructuralRecursive_InverseAnchorCarriesTraceIDRestriction for the
// external-table form: the union op's INVERSE closure anchor
// (buildStructuralInverseClosure) must fold TraceIDExternalTable exactly as
// it folds TraceIDRestriction, so a wide union search gets the same
// SQL-text-size relief on both arms.
func TestEmitStructuralRecursive_InverseAnchorCarriesExternalTraceIDTable(t *testing.T) {
	t.Parallel()

	plan := structuralUnionClosurePlan()
	plan.TraceIDExternalTable = "cerberus_phase_b_trace_ids"

	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, "_struct_closure_inv_") {
		t.Fatalf("union recursive op did not render an inverse closure CTE:\n%s", sql)
	}
	if !strings.Contains(sql, "AS _seed WHERE `TraceId` IN (SELECT `TraceId` FROM `cerberus_phase_b_trace_ids`)") {
		t.Fatalf("external trace-id table reference missing from the inverse closure's anchor seed:\n%s", sql)
	}
}
