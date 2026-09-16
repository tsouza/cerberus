package promql

import (
	"reflect"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestWrapExpHistogramMergeSumMapBudgetGuard_InstantMultiGroupUsesItsOwnGuard
// kills the INVERT_LOGICAL mutant on
// wrapExpHistogramMergeSumMapBudgetGuard's
//
//	switch {
//	case multiGroup && rangeMode:
//		predicate = expHistogramMergeSumMapRangeBudgetGuardExpr(maxCostUnits)
//	case multiGroup:
//		predicate = expHistogramMergeSumMapMultiGroupBudgetGuardExpr(maxCostUnits)
//	}
//
// with the first `&&` rewritten to `||`. No existing untagged test called
// wrapExpHistogramMergeSumMapBudgetGuard directly, so gremlins reported
// the mutant NOT COVERED.
//
// The distinguishing input is `multiGroup == true, rangeMode == false` —
// the doc comment's own "instant multi-group" case (cerberus issue
// #2865), NOT the excluded `!multiGroup && rangeMode` combination the
// doc comment separately rules out ("range mode is ALWAYS multi-group").
// Under the original `&&`, this combination is false and falls through
// to the second case, selecting the instant multi-group guard
// ([expHistogramMergeSumMapMultiGroupBudgetGuardExpr]). Under the
// mutated `||`, `true || false` is true, so the RANGE guard
// ([expHistogramMergeSumMapRangeBudgetGuardExpr]) is selected instead —
// a real, differently-calibrated predicate (this file's own "Multi-group
// calibration" vs. "Range-mode calibration" sections), for a query that
// is not in range mode at all.
func TestWrapExpHistogramMergeSumMapBudgetGuard_InstantMultiGroupUsesItsOwnGuard(t *testing.T) {
	t.Parallel()

	const maxCostUnits = 1_000_000

	guard := wrapExpHistogramMergeSumMapBudgetGuard(nil, maxCostUnits, true, false)
	filter, ok := guard.(*chplan.Filter)
	if !ok {
		t.Fatalf("wrapExpHistogramMergeSumMapBudgetGuard(multiGroup, !rangeMode) = %T, want *chplan.Filter", guard)
	}

	wantInstant := expHistogramMergeSumMapMultiGroupBudgetGuardExpr(maxCostUnits)
	if !reflect.DeepEqual(filter.Predicate, wantInstant) {
		t.Fatalf("predicate = %#v, want the instant multi-group guard %#v", filter.Predicate, wantInstant)
	}

	rangeGuard := expHistogramMergeSumMapRangeBudgetGuardExpr(maxCostUnits)
	if reflect.DeepEqual(filter.Predicate, rangeGuard) {
		t.Fatal("predicate equals the RANGE-mode guard for a non-range multi-group query")
	}
}
