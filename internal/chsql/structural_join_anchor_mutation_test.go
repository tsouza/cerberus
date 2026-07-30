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
