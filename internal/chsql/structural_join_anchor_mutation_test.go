package chsql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// structuralClosurePlan returns the canonical descendant closure, which the
// recursive emitter renders as a WITH RECURSIVE anchor + step.
func structuralClosurePlan() *chplan.StructuralJoin {
	return &chplan.StructuralJoin{
		Left:               &chplan.Scan{Table: "otel_traces"},
		Right:              &chplan.Scan{Table: "otel_traces"},
		Op:                 chplan.StructuralDescendant,
		TraceIDColumn:      "TraceId",
		SpanIDColumn:       "SpanId",
		ParentSpanIDColumn: "ParentSpanId",
	}
}

// TestEmitStructuralRecursive_AnchorCarriesCandidatePrefilter pins that the
// candidate prefilter reaches the closure's ANCHOR SEED. The prefilter is what
// makes a both-sides-selective closure affordable: without it the anchor seeds
// the recursion with every trace the left side matches, and the step then walks
// each of them even though the right side can never join. A prefilter computed
// and then dropped is invisible in the result set and shows up only as the
// runaway scan it was added to prevent.
//
// Kills structural_join.go's `len(seedWhere) > 0` CONDITIONALS_NEGATION
// (`<= 0`), which applies the seed WHERE only when it is EMPTY — so exactly the
// plans that asked for pruning get none.
func TestEmitStructuralRecursive_AnchorCarriesCandidatePrefilter(t *testing.T) {
	t.Parallel()

	plan := structuralClosurePlan()
	plan.CandidatePrefilter = true

	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, "AS _seed WHERE `TraceId` IN (SELECT DISTINCT `TraceId`") {
		t.Fatalf("candidate prefilter subquery missing from the anchor seed:\n%s", sql)
	}
}

// TestEmitStructuralRecursive_AnchorCarriesTraceIDRestriction pins the other
// producer of the same seed WHERE: the two-phase phase-B restriction to the
// top-N trace ids. Dropping it turns a bounded phase-B into a full closure over
// every seeded trace.
//
// Kills the same `len(seedWhere) > 0` CONDITIONALS_NEGATION via the second
// conjunct source, so the assertion does not rest on the prefilter alone.
func TestEmitStructuralRecursive_AnchorCarriesTraceIDRestriction(t *testing.T) {
	t.Parallel()

	plan := structuralClosurePlan()
	plan.TraceIDRestriction = []string{"aabb", "ccdd"}

	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, "AS _seed WHERE `TraceId` IN ('aabb', 'ccdd')") {
		t.Fatalf("trace-id restriction missing from the anchor seed:\n%s", sql)
	}
}

// TestEmitStructuralRecursive_UnrestrictedAnchorHasNoSeedWhere pins the
// complement: a plan that opts into neither pruning bound must render the
// anchor with no seed WHERE at all, so the two assertions above cannot be
// satisfied by an emitter that always splices one in.
func TestEmitStructuralRecursive_UnrestrictedAnchorHasNoSeedWhere(t *testing.T) {
	t.Parallel()

	sql, _, err := chsql.Emit(context.Background(), structuralClosurePlan())
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if strings.Contains(sql, "AS _seed WHERE") {
		t.Fatalf("plan set no pruning bound, yet the anchor seed carries a WHERE:\n%s", sql)
	}
}

// structuralUnionClosurePlan returns a union recursive (`&>>`) StructuralJoin
// — the shape whose L-projection arm renders through
// buildStructuralInverseClosure's inverse closure, alongside the canonical
// one the R-projection arm uses.
func structuralUnionClosurePlan() *chplan.StructuralJoin {
	plan := structuralClosurePlan()
	plan.Op = chplan.StructuralUnionDescendant
	return plan
}

// TestEmitStructuralRecursive_InverseAnchorCarriesTraceIDRestriction pins the
// #2104 fix: the INVERSE closure's anchor seed (buildStructuralInverseClosure,
// `_struct_closure_inv_*`) must fold the two-phase phase-B trace-id
// restriction, exactly as the canonical closure's anchor does
// (TestEmitStructuralRecursive_AnchorCarriesTraceIDRestriction). Before the
// fix the inverse anchor carried only the request-window bound, so its seed
// scan read every windowed trace on the R side instead of granule-pruning to
// the top-N literals via idx_trace_id — a correct-but-amplified read, not a
// wrong result (the closure only ever projects `_depth > 0` rows, which the
// step arm's own restriction already confines to the top-N set).
func TestEmitStructuralRecursive_InverseAnchorCarriesTraceIDRestriction(t *testing.T) {
	t.Parallel()

	plan := structuralUnionClosurePlan()
	plan.TraceIDRestriction = []string{"aabb", "ccdd"}

	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, "_struct_closure_inv_") {
		t.Fatalf("union recursive op did not render an inverse closure CTE:\n%s", sql)
	}
	if !strings.Contains(sql, "AS _seed WHERE `TraceId` IN ('aabb', 'ccdd')") {
		t.Fatalf("trace-id restriction missing from the inverse closure's anchor seed:\n%s", sql)
	}
}

// TestEmitStructuralRecursive_InverseAnchorUnrestrictedHasNoSeedWhere mirrors
// TestEmitStructuralRecursive_UnrestrictedAnchorHasNoSeedWhere for the inverse
// closure: a plan that sets no phase-B restriction (the single-query path,
// not phase B of the two-phase search) must render the inverse anchor with no
// seed WHERE, so the assertion above cannot be satisfied by an emitter that
// always splices one in.
func TestEmitStructuralRecursive_InverseAnchorUnrestrictedHasNoSeedWhere(t *testing.T) {
	t.Parallel()

	sql, _, err := chsql.Emit(context.Background(), structuralUnionClosurePlan())
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, "_struct_closure_inv_") {
		t.Fatalf("union recursive op did not render an inverse closure CTE:\n%s", sql)
	}
	if strings.Contains(sql, "AS _seed WHERE") {
		t.Fatalf("plan set no pruning bound, yet the inverse anchor seed carries a WHERE:\n%s", sql)
	}
}

// NOT KILLABLE — documented, not defended by a test.
//
// structural_join.go:`len(seedWhere) > 0` (CONDITIONALS_BOUNDARY,
// `if seedWhere := structuralAnchorWhere(j, rightSub); len(seedWhere) > 0`
// -> `>= 0`). The
// forms differ only when seedWhere is empty, where the mutant runs
// `anchor = anchor.Where()`. QueryBuilder.Where is variadic and its body is
// `s.where = append(s.where, conds...)`, so a zero-argument call appends
// nothing and returns the same builder — the WHERE slot stays empty and the
// renderer emits no clause. Byte-identical SQL. (The CONDITIONALS_NEGATION
// mutant on this same comparison is a real defect, and the three tests
// above kill it.)
//
// structural_join.go:`len(cols) > 0` (CONDITIONALS_BOUNDARY,
// `if cols := structuralUnionOutputCols(j); len(cols) > 0` -> `>= 0` in
// emitStructuralSpanUnion). structuralUnionOutputCols returns either nil or
// the three join keys plus the extra projection columns, so len(cols) is
// either 0 or at least 3 and the forms can differ only at 0. There the
// original keeps `proj = []Frag{verbatim("*")}` while the mutant rebuilds
// proj as an EMPTY slice and passes it to Select — and QueryBuilder's
// renderer writes a bare `*` for an empty SELECT list. Both spellings
// render `SELECT *`, so the statement is byte-identical.
