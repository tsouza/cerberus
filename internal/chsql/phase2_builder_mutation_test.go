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

// TestMutation_CountPhysicalScans_AccumulatesLevelTableReads kills the
// REMOVE_SELF_ASSIGNMENTS mutant on builder.go:countPhysicalScans:
// `b.levelTableReads += n` rewritten to `b.levelTableReads = n`. The
// per-SELECT read count is what the native timeSeries aggregate guard checks,
// so a second counted scan at the same level must add to it rather than
// replace it.
func TestMutation_CountPhysicalScans_AccumulatesLevelTableReads(t *testing.T) {
	t.Parallel()

	b := &Builder{levelTableReads: 1}
	countPhysicalScans(2, Col("merged"))(b)
	if b.levelTableReads != 3 {
		t.Fatalf("levelTableReads = %d, want 3 (1 prior + 2 counted)", b.levelTableReads)
	}
}

// TestMutation_ASCIILower_FoldsRangeEndpoints kills both CONDITIONALS_BOUNDARY
// mutants on builder.go:asciiLower:`c >= 'A' && c <= 'Z'`. `>` would leave 'A'
// unfolded and `<` would leave 'Z' unfolded; the neighbours on either side of
// the range must stay untouched.
func TestMutation_ASCIILower_FoldsRangeEndpoints(t *testing.T) {
	t.Parallel()

	if got, want := asciiLower("@AZ["), "@az["; got != want {
		t.Fatalf("asciiLower(%q) = %q, want %q", "@AZ[", got, want)
	}
}

// TestMutation_ASCIIFoldSafe_RejectsFirstNonASCIIRune kills the
// CONDITIONALS_BOUNDARY mutant on
// builder.go:asciiFoldSafe:`r >= utf8.RuneSelf` (`>=` -> `>`). U+0080 is
// exactly utf8.RuneSelf, the first rune `lower()` does not fold, so it is
// not fold-safe; the mutant would accept it.
func TestMutation_ASCIIFoldSafe_RejectsFirstNonASCIIRune(t *testing.T) {
	t.Parallel()

	if asciiFoldSafe("a\u0080") {
		t.Fatalf("asciiFoldSafe(%q) = true, want false: U+0080 is utf8.RuneSelf", "a\u0080")
	}
	if !asciiFoldSafe("a\u007f") {
		t.Fatalf("asciiFoldSafe(%q) = false, want true: U+007F is the last ASCII rune", "a\u007f")
	}
}
