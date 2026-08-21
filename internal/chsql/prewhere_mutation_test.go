package chsql

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// This file pins the exact control-flow / boundary behaviour of the
// PREWHERE-promotion helpers in prewhere.go so that the gremlins mutation
// lane (phase2 scope ./internal/chsql) cannot flip an operator or a
// loop-control token without a test going red. Each test names the
// prewhere.go line it defends.

// TestOrderedConjunctsSortContinue defends prewhere.go:203 (the `continue`
// inside the stable insertion sort over the sort-prefix bucket).
//
// Mutation INVERT_LOOPCTRL turns that `continue` into `break`. The original
// keeps bubbling the inserted element left until it reaches its rank slot;
// `break` truncates the bubble after a single swap, so an element that must
// move two positions left is left mis-ordered. We feed three conjuncts whose
// sort ranks are [1, 2, 0] in input order: the rank-0 predicate (idx 2) must
// bubble two slots to the front, which only fully sorts when the loop keeps
// going (`continue`).
func TestOrderedConjunctsSortContinue(t *testing.T) {
	t.Parallel()
	shape := TableShape{
		SortColumns: []string{"ServiceName", "SeverityText", "Timestamp"}, // ranks 0,1,2
	}
	// Input order: rank 1, rank 2, rank 0.
	rank1 := &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "SeverityText"}, Right: &chplan.LitString{V: "ERROR"}}
	rank2 := &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "Timestamp"}, Right: &chplan.LitInt{V: 1}}
	rank0 := &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "ServiceName"}, Right: &chplan.LitString{V: "api"}}

	got := orderedConjuncts([]chplan.Expr{rank1, rank2, rank0}, shape)
	// Original (full bubble): ascending rank → [rank0, rank1, rank2].
	// Mutant (continue→break): rank-2 swaps once with rank-0 then stops →
	// [rank1, rank0, rank2], so got[0]/got[1] differ.
	want := []chplan.Expr{rank0, rank1, rank2}
	if len(got) != 3 {
		t.Fatalf("orderedConjuncts: len=%d want 3", len(got))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("orderedConjuncts pos %d mismatch: got %p want %p (full order got=%v)", i, got[i], want[i], got)
		}
	}
}

// TestIsLeadingSortKeyEqualityLogical defends prewhere.go:269 (the
// `!ok || bin.Op != chplan.OpEq` short-circuit).
//
// Mutation INVERT_LOGICAL turns `||` into `&&`. With `&&`, a binary whose
// operator is NOT `=` no longer returns early as "not a leading-sort-key
// equality": the function falls through and reports true for any predicate
// on the leading sort column regardless of operator. The original must
// reject a `<` predicate. (The `&&` form also dereferences a nil *Binary
// for non-binary inputs, which the second assertion catches via panic.)
func TestIsLeadingSortKeyEqualityLogical(t *testing.T) {
	t.Parallel()
	shape := TableShape{SortColumns: []string{"MetricName", "Attributes"}}

	// `<` on the leading sort column: original false; mutant (||→&&) → true.
	ltOnLeadKey := &chplan.Binary{Op: chplan.OpLt, Left: &chplan.ColumnRef{Name: "MetricName"}, Right: &chplan.LitInt{V: 5}}
	if isLeadingSortKeyEquality(ltOnLeadKey, shape) {
		t.Errorf("isLeadingSortKeyEquality(MetricName < 5) = true, want false (|| guard rejects non-Eq op)")
	}

	// Non-binary input: original short-circuits on !ok and returns false;
	// the &&-mutant evaluates bin.Op on a nil *Binary and panics.
	bare := &chplan.ColumnRef{Name: "MetricName"}
	if isLeadingSortKeyEquality(bare, shape) {
		t.Errorf("isLeadingSortKeyEquality(bare ColumnRef) = true, want false")
	}

	// Sanity: the genuine leading-sort-key equality is still true.
	eqOnLeadKey := &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "MetricName"}, Right: &chplan.LitString{V: "up"}}
	if !isLeadingSortKeyEquality(eqOnLeadKey, shape) {
		t.Errorf("isLeadingSortKeyEquality(MetricName = up) = false, want true")
	}
}

// TestOrderedConjunctsSingleFastPath defends prewhere.go:167
// (`if len(conjuncts) <= 1 { return conjuncts }`).
//
// Mutation CONDITIONALS_BOUNDARY turns `<=` into `<`, so a single conjunct
// no longer takes the fast path: it is rebuilt through the bucket/sort
// machinery into a fresh slice. The emitted content is identical (one
// element can't reorder), so the observable contract is the fast path's
// identity guarantee: for len<=1 the *same* backing slice is returned.
func TestOrderedConjunctsSingleFastPath(t *testing.T) {
	t.Parallel()
	shape := TableShape{SortColumns: []string{"ServiceName"}}
	c := &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "ServiceName"}, Right: &chplan.LitString{V: "api"}}
	in := []chplan.Expr{c}
	got := orderedConjuncts(in, shape)
	if len(got) != 1 || got[0] != c {
		t.Fatalf("orderedConjuncts(single): got %v want [c]", got)
	}
	// Original returns the input slice itself; the `<` mutant allocates a
	// fresh slice via make(), so the backing array address differs.
	if &got[0] != &in[0] {
		t.Errorf("orderedConjuncts(single) returned a fresh slice; fast path (len<=1) must return the input slice unchanged")
	}
}

// TestSortRankForMinimum defends sortRankFor's running-minimum update
// (prewhere.go:148). It pins that sortRankFor returns the LOWEST matching
// rank across a predicate's columns regardless of the order the columns are
// discovered — i.e. a later, larger rank never overwrites an earlier rank-0.
//
// Note: with deduplicated column refs no two columns share a rank, so the
// `r < best` boundary itself is never exercised at equality and that exact
// mutation is behaviourally equivalent. This test instead nails the broader
// min-tracking contract (and would catch a `best < 0`→`best <= 0` flip,
// which lets a larger rank clobber an existing rank-0 minimum).
func TestSortRankForMinimum(t *testing.T) {
	t.Parallel()
	shape := TableShape{SortColumns: []string{"ServiceName", "SeverityText", "Timestamp"}} // ranks 0,1,2
	// rank-0 column discovered first, then a larger rank: min must stay 0.
	if got := sortRankFor([]string{"ServiceName", "Timestamp"}, shape); got != 0 {
		t.Errorf("sortRankFor([rank0, rank2]) = %d, want 0", got)
	}
	// larger rank first, then rank-0: min must still resolve to 0.
	if got := sortRankFor([]string{"Timestamp", "ServiceName"}, shape); got != 0 {
		t.Errorf("sortRankFor([rank2, rank0]) = %d, want 0", got)
	}
	// no matching column: -1.
	if got := sortRankFor([]string{"Body"}, shape); got != -1 {
		t.Errorf("sortRankFor([non-sort]) = %d, want -1", got)
	}
}

// TestIsNarrowIntegerDiscriminatorFinalReturnLogical defends
// isNarrowIntegerDiscriminator's final return (prewhere.go:287): the chain
// `columnOK && literalOK && column.Qualifier == "" && shape.IsInteger...
// && sortRankFor(...) < 0`.
//
// Mutation INVERT_LOGICAL turns the FIRST `&&` (between columnOK and
// literalOK) into `||`. Go's `&&` binds tighter than `||`, so the mutant
// parses as `columnOK || (literalOK && qualifier=="" && isDiscriminator &&
// rank<0)`: any predicate whose left/right operand resolves to a bare
// ColumnRef — columnOK true — would report true regardless of whether the
// OTHER operand is even an integer literal. We feed a column-to-column
// equality (`ServiceName = OtherCol`): columnOK becomes true (one side is a
// ColumnRef) but literalOK is false (neither side is a LitInt), so the
// correct answer is false. The `||` mutant would return true.
func TestIsNarrowIntegerDiscriminatorFinalReturnLogical(t *testing.T) {
	t.Parallel()
	shape := TableShape{
		WideColumns:                 []string{"Body"},
		IntegerDiscriminatorColumns: []string{"ServiceName"},
	}
	colEqCol := &chplan.Binary{
		Op:    chplan.OpEq,
		Left:  &chplan.ColumnRef{Name: "ServiceName"},
		Right: &chplan.ColumnRef{Name: "OtherCol"},
	}
	if isNarrowIntegerDiscriminator(colEqCol, shape) {
		t.Errorf("isNarrowIntegerDiscriminator(ServiceName = OtherCol) = true, want false (neither operand is a LitInt)")
	}

	// Sanity: the genuine narrow-integer-discriminator shape is still true.
	colEqInt := &chplan.Binary{
		Op:    chplan.OpEq,
		Left:  &chplan.ColumnRef{Name: "ServiceName"},
		Right: &chplan.LitInt{V: 3},
	}
	if !isNarrowIntegerDiscriminator(colEqInt, shape) {
		t.Errorf("isNarrowIntegerDiscriminator(ServiceName = 3) = false, want true")
	}
}

// NOT KILLABLE — documented, not defended by a test.
//
// prewhere.go:131:4, :187:5 and :207:4 (INVERT_LOOPCTRL) each swap a
// terminal `break` for `continue` guarding a "found it, stop" boolean latch
// (touchesWide / hitsSkip) or the stable-insertion-sort early exit. In
// every case the flag is set exactly once and never reset, or (for the
// sort) the insertion-sort invariant guarantees every comparison below the
// break point is already in order, so scanning further with `continue`
// instead of `break` can never change the final return value — only the
// iteration count. No observable-behaviour test can distinguish them.
//
// prewhere.go:283:15 (INVERT_LOGICAL, `||`→`&&` in
// isNarrowIntegerDiscriminator's swap guard) is equivalent for the same
// structural reason as the min-tracking boundary above: a chplan.Expr value
// has exactly one concrete type, so whichever of columnOK/literalOK is
// already true from the direct (Left, Right) orientation is provably
// unreachable from the swapped (Right, Left) orientation, and vice versa —
// the branch this condition guards can only ever produce the SAME final
// (columnOK, literalOK) pair whether or not the swap runs.
