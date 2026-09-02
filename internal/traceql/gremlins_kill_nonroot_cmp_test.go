package traceql

import (
	"fmt"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// Tests in this file kill the four CONDITIONALS_BOUNDARY survivors gremlins
// reported on [nonRootCmpConstant] from the phase4-traceql-lower leg
// (cerberus issue #2949):
//
//	lower.go:nonRootCmpConstant:`case chplan.OpLt:`  the `v <= 1` guard in that arm
//	lower.go:nonRootCmpConstant:`case chplan.OpLe:`  the `v < 1` guard in that arm
//	lower.go:nonRootCmpConstant:`case chplan.OpGt:`  the `v < 1` guard in that arm
//	lower.go:nonRootCmpConstant:`case chplan.OpGe:`  the `v <= 1` guard in that arm
//
// Each citation names the arm's `case` label rather than the guard itself: the
// four guards are spelled with only two distinct texts between them (`v <= 1`
// twice, `v < 1` four times counting the OpEq and OpNe arms), so no substring
// of a guard singles one out. The `case` label immediately above it does, and
// the guard is the single `if` inside that arm.
//
// Every one of them is a boundary AT v == 1, and the existing tests never
// asked the function about v == 1 — only about values where the two readings
// agree. Each `<`/`<=` pair therefore looked interchangeable.
//
// The expectations below are not read off the implementation. They are
// derived from the one domain fact the function documents — a non-root
// nested-set parent position p satisfies p >= 1 — by asking, for each
// operator and each v, whether `p op v` has the same answer for EVERY such p:
//
//	p = v      constant-false iff no p equals v, i.e. v < 1
//	p != v     constant-true  iff every p differs from v, i.e. v < 1
//	p < v      constant-false iff no p is below v; the smallest p is 1, so v <= 1
//	p <= v     constant-false iff no p is at or below v, i.e. v < 1
//	p > v      constant-true  iff every p exceeds v, i.e. v < 1
//	p >= v     constant-true  iff every p is at or above v; the smallest is 1, so v <= 1
//
// v == 1 is exactly where the two `<=` operators part company with the two
// `<` ones, which is why it is the row that kills all four mutants:
// `p < 1` and `p >= 1` are decided by the domain's floor, while `p <= 1` and
// `p > 1` are genuinely open — both are answered one way at p == 1 and the
// other way at p == 2.
func TestNonRootCmpConstant_BoundaryAtTheDomainFloor(t *testing.T) {
	t.Parallel()

	type want struct {
		value    bool
		constant bool
	}
	cases := []struct {
		op   chplan.BinaryOp
		v    int64
		want want
		why  string
	}{
		// v == 0: below the floor, so every operator is decided.
		{chplan.OpEq, 0, want{false, true}, "no non-root position equals 0"},
		{chplan.OpNe, 0, want{true, true}, "every non-root position differs from 0"},
		{chplan.OpLt, 0, want{false, true}, "no non-root position is below 0"},
		{chplan.OpLe, 0, want{false, true}, "no non-root position is at or below 0"},
		{chplan.OpGt, 0, want{true, true}, "every non-root position exceeds 0"},
		{chplan.OpGe, 0, want{true, true}, "every non-root position is at or above 0"},

		// v == 1: the floor itself, and the row every mutant turns on.
		{chplan.OpEq, 1, want{false, false}, "p == 1 holds at the floor and fails above it"},
		{chplan.OpNe, 1, want{false, false}, "p != 1 fails at the floor and holds above it"},
		{chplan.OpLt, 1, want{false, true}, "nothing is below the floor, so p < 1 is never true"},
		{chplan.OpLe, 1, want{false, false}, "p <= 1 holds exactly at the floor"},
		{chplan.OpGt, 1, want{false, false}, "p > 1 fails exactly at the floor"},
		{chplan.OpGe, 1, want{true, true}, "everything is at or above the floor, so p >= 1 always holds"},

		// v == 2: above the floor, so nothing is decided either way.
		{chplan.OpEq, 2, want{false, false}, "p == 2 holds only at 2"},
		{chplan.OpNe, 2, want{false, false}, "p != 2 fails only at 2"},
		{chplan.OpLt, 2, want{false, false}, "p < 2 holds at the floor and fails at 2"},
		{chplan.OpLe, 2, want{false, false}, "p <= 2 holds up to 2 and fails above"},
		{chplan.OpGt, 2, want{false, false}, "p > 2 fails up to 2 and holds above"},
		{chplan.OpGe, 2, want{false, false}, "p >= 2 fails at the floor and holds at 2"},
	}

	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s_%d", tc.op, tc.v), func(t *testing.T) {
			t.Parallel()

			value, constant := nonRootCmpConstant(tc.op, tc.v)
			if constant != tc.want.constant {
				t.Fatalf("nonRootCmpConstant(%s, %d) constant = %v, want %v — %s",
					tc.op, tc.v, constant, tc.want.constant, tc.why)
			}
			// `value` is only meaningful where the comparison IS constant;
			// where it is not, the caller never reads it.
			if constant && value != tc.want.value {
				t.Fatalf("nonRootCmpConstant(%s, %d) value = %v, want %v — %s",
					tc.op, tc.v, value, tc.want.value, tc.why)
			}
		})
	}
}
