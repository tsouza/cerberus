package logql

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// [sharedValueProjectionIndex] decides which projection slot the multi-variant
// range-aggregation fusion may unpivot. Getting it wrong is not a slow query:
// picking a slot the arms differ at somewhere OTHER than the value column
// folds two arms that describe DIFFERENT series onto one grouped pass, and the
// result reports one arm's grouping for both.
//
// Three of its decisions survived mutation on `phase4-logql-other-b` (cerberus
// issue #2913): the `continue` that walks past an agreeing position, the
// duplicate-alias rejection inside the fallback alias search, and the
// "found nothing" rejection after it. The corpus only ever reaches this
// function through well-formed fused fixtures — arms that agree everywhere
// except one properly-aliased value slot — so the loop only ever had one
// answer to give and none of the three rejections was ever the reason for a
// verdict.
//
// Each case below is therefore a shape the function must REJECT (or, for the
// index-zero case, accept) that no fixture produces, asserted against an
// accepted baseline of the same shape so a broken accept path cannot make a
// rejection look correct.

// sharedValueOtherAlias is a projection alias that is deliberately NOT
// [rangeAggSynthValueColumn], so a slot carrying it can never be chosen as the
// value slot.
const sharedValueOtherAlias = "Attributes"

// sharedValueProj builds one arm's Project from (alias, column-name) pairs.
// The column name is what makes two arms differ at a position: projectionEqual
// compares the alias first, then the expression.
func sharedValueProj(pairs ...[2]string) *chplan.Project {
	projections := make([]chplan.Projection, 0, len(pairs))
	for _, p := range pairs {
		projections = append(projections, chplan.Projection{
			Expr:  &chplan.ColumnRef{Name: p[1]},
			Alias: p[0],
		})
	}
	return &chplan.Project{Projections: projections}
}

func TestSharedValueProjectionIndex_PicksTheSoleDifferingValueSlot(t *testing.T) {
	t.Parallel()
	// The baseline every rejection below is a perturbation of: the arms agree
	// on their identity projection and differ only at the value slot.
	projects := []*chplan.Project{
		sharedValueProj([2]string{sharedValueOtherAlias, "labels"}, [2]string{rangeAggSynthValueColumn, "a"}),
		sharedValueProj([2]string{sharedValueOtherAlias, "labels"}, [2]string{rangeAggSynthValueColumn, "b"}),
	}
	idx, ok := sharedValueProjectionIndex(projects)
	if !ok || idx != 1 {
		t.Fatalf("sharedValueProjectionIndex = (%d, %v); want (1, true)", idx, ok)
	}
}

// TestSharedValueProjectionIndex_RejectsTwoDifferingPositions pins that the
// scan does not stop at the first position the arms AGREE at. Two differing
// positions is the "arms diverge somewhere the fused pass cannot represent"
// case, and it is only reachable by continuing past position 0.
//
// Abandoning the scan there instead would silently fall through to the
// every-position-agrees fallback, which finds the value alias by name and
// reports the arms fusable — exactly the unsound fold the function exists to
// refuse.
func TestSharedValueProjectionIndex_RejectsTwoDifferingPositions(t *testing.T) {
	t.Parallel()
	projects := []*chplan.Project{
		sharedValueProj(
			[2]string{sharedValueOtherAlias, "labels"}, // position 0: the arms agree here
			[2]string{rangeAggSynthValueColumn, "a"},   // position 1: differs
			[2]string{sharedValueOtherAlias, "x"},      // position 2: differs too
		),
		sharedValueProj(
			[2]string{sharedValueOtherAlias, "labels"},
			[2]string{rangeAggSynthValueColumn, "b"},
			[2]string{sharedValueOtherAlias, "y"},
		),
	}
	if idx, ok := sharedValueProjectionIndex(projects); ok {
		t.Errorf("sharedValueProjectionIndex = (%d, true); want ok=false — the arms differ at positions 1 AND 2, so no single slot describes the divergence", idx)
	}
}

// TestSharedValueProjectionIndex_RejectsADuplicateValueAlias covers the
// fallback path taken when every arm projects the same thing everywhere: the
// value slot is then located by ALIAS, and two slots carrying that alias make
// the choice ambiguous. Picking either one leaves the other unpivoted under
// the same output name.
//
// The duplicate is placed at positions 0 and 1 deliberately: the ambiguity is
// only detectable if the already-found index 0 counts as "already found", and
// index 0 is the one value a `>= 0` guard and a `> 0` one disagree about.
func TestSharedValueProjectionIndex_RejectsADuplicateValueAlias(t *testing.T) {
	t.Parallel()
	arm := func() *chplan.Project {
		return sharedValueProj(
			[2]string{rangeAggSynthValueColumn, "a"},
			[2]string{rangeAggSynthValueColumn, "b"},
		)
	}
	projects := []*chplan.Project{arm(), arm()}
	if idx, ok := sharedValueProjectionIndex(projects); ok {
		t.Errorf("sharedValueProjectionIndex = (%d, true); want ok=false — two slots carry %q, so the value slot is ambiguous", idx, rangeAggSynthValueColumn)
	}
}

// TestSharedValueProjectionIndex_AcceptsTheValueAliasAtPositionZero is the
// other half of the same boundary. When every arm agrees everywhere, the
// fallback alias search legitimately answers "slot 0" — and slot 0 is a real
// answer, not the "found nothing" sentinel the search starts from. Treating it
// as a miss would refuse to fuse every shared-value query whose value column
// happens to be projected first.
func TestSharedValueProjectionIndex_AcceptsTheValueAliasAtPositionZero(t *testing.T) {
	t.Parallel()
	arm := func() *chplan.Project {
		return sharedValueProj(
			[2]string{rangeAggSynthValueColumn, "a"},
			[2]string{sharedValueOtherAlias, "labels"},
		)
	}
	projects := []*chplan.Project{arm(), arm()}
	idx, ok := sharedValueProjectionIndex(projects)
	if !ok || idx != 0 {
		t.Errorf("sharedValueProjectionIndex = (%d, %v); want (0, true) — the value alias sits at slot 0, which is a found index, not a miss", idx, ok)
	}
}

// TestSharedValueProjectionIndex_RejectsWhenNoSlotCarriesTheValueAlias is the
// case the "found nothing" rejection actually exists for: arms that agree
// everywhere but project no value column at all cannot be unpivoted.
func TestSharedValueProjectionIndex_RejectsWhenNoSlotCarriesTheValueAlias(t *testing.T) {
	t.Parallel()
	arm := func() *chplan.Project {
		return sharedValueProj(
			[2]string{sharedValueOtherAlias, "labels"},
			[2]string{"TimeUnix", "ts"},
		)
	}
	projects := []*chplan.Project{arm(), arm()}
	if idx, ok := sharedValueProjectionIndex(projects); ok {
		t.Errorf("sharedValueProjectionIndex = (%d, true); want ok=false — no slot carries %q", idx, rangeAggSynthValueColumn)
	}
}
