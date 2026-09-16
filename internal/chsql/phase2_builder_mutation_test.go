package chsql

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestMutation_CountPhysicalScans_AccumulatesOntoNonzeroCount kills the
// REMOVE_SELF_ASSIGNMENTS mutant on builder.go:countPhysicalScans:
// `b.physicalScans += n` rewritten to `b.physicalScans = n`. Priming the
// Builder with a nonzero count is what makes the two forms distinguishable:
// the original accumulates onto the prior value, while the mutant overwrites
// it.
func TestMutation_CountPhysicalScans_AccumulatesOntoNonzeroCount(t *testing.T) {
	t.Parallel()

	b := &Builder{physicalScans: 1}
	countPhysicalScans(2, Col("merged"))(b)
	if got := b.PhysicalScans(); got != 3 {
		t.Fatalf("PhysicalScans = %d, want 3 (1 prior + 2 counted)", got)
	}
}

// TestMutation_ExprScalarSubquery_AccumulatesSubScanCount kills both
// mutations gremlins emits on builder.go:exprScalarSubquery's
// `b.physicalScans += e.physicalScans`:
//
//   - INVERT_ASSIGNMENTS (`+=` -> `-=`) would subtract the subquery's scan
//     count from the outer Builder;
//   - REMOVE_SELF_ASSIGNMENTS (`+=` -> `=`) would overwrite the outer
//     Builder's prior count with only the subquery's.
//
// Priming the outer Builder to 1 makes each mutant leave either 0 or 1
// instead of the correct 2.
func TestMutation_ExprScalarSubquery_AccumulatesSubScanCount(t *testing.T) {
	t.Parallel()

	b := &Builder{physicalScans: 1}
	err := b.exprScalarSubquery(&chplan.ScalarSubquery{
		Input: &chplan.Scan{Table: "otel_metrics_sum"},
	})
	if err != nil {
		t.Fatalf("exprScalarSubquery: %v", err)
	}
	if got := b.PhysicalScans(); got != 2 {
		t.Fatalf("PhysicalScans = %d, want 2 (1 prior + 1 subquery scan)", got)
	}
}

// TestMutation_Spliced_AccumulatesOntoNonzeroCount kills the
// REMOVE_SELF_ASSIGNMENTS mutant on builder.go:Spliced:
// `b.physicalScans += s.physicalScans()` rewritten to `b.physicalScans =
// s.physicalScans()`. The primed outer count is the observable distinction.
func TestMutation_Spliced_AccumulatesOntoNonzeroCount(t *testing.T) {
	t.Parallel()

	b := &Builder{physicalScans: 1}
	Spliced(PreRenderedSQL{SQL: "SELECT 1", PhysicalScans: 1})(b)
	if got := b.PhysicalScans(); got != 2 {
		t.Fatalf("PhysicalScans = %d, want 2 (1 prior + 1 spliced)", got)
	}
}

// TestMutation_Subquery_AccumulatesOntoNonzeroCount kills the
// REMOVE_SELF_ASSIGNMENTS mutant on builder.go:Subquery:
// `b.physicalScans += s.physicalScans()` rewritten to `b.physicalScans =
// s.physicalScans()`. The primed outer count is the observable distinction,
// matching Spliced's sibling guard below.
func TestMutation_Subquery_AccumulatesOntoNonzeroCount(t *testing.T) {
	t.Parallel()

	b := &Builder{physicalScans: 1}
	Subquery(PreRenderedSQL{SQL: "SELECT 1", PhysicalScans: 1})(b)
	if got := b.PhysicalScans(); got != 2 {
		t.Fatalf("PhysicalScans = %d, want 2 (1 prior + 1 subquery)", got)
	}
}
