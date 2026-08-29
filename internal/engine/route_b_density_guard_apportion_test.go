package engine

import (
	"testing"

	"github.com/tsouza/cerberus/internal/solver"
)

// TestDecisionK pins the K-vs-kEff choice: routeBExecCtx apportions by the
// structural shard count on the Decision, which is available at ctx-build
// time, never by kEff (min(K, pEff, gate/2)), which the executor only
// derives after the emit loop this ctx feeds. A nil or sub-1 K (a wiring
// bug — see decisionK's own doc) must floor to 1, never divide by zero.
func TestDecisionK(t *testing.T) {
	for _, tc := range []struct {
		name     string
		decision *solver.Decision
		want     int64
	}{
		{name: "nil decision floors to 1", decision: nil, want: 1},
		{name: "zero K floors to 1", decision: &solver.Decision{K: 0}, want: 1},
		{name: "negative K floors to 1", decision: &solver.Decision{K: -1}, want: 1},
		{name: "normal K passes through", decision: &solver.Decision{K: 4}, want: 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := decisionK(tc.decision); got != tc.want {
				t.Errorf("decisionK(%+v) = %d, want %d", tc.decision, got, tc.want)
			}
		})
	}
}

// TestApportionRangeBucketGridNativeBounds pins the resolve-then-divide
// contract: a zero/negative override must resolve to the compiled default
// BEFORE dividing by k (dividing the raw override would read a caller's "no
// override" as an apportioned near-zero bound instead of the un-apportioned
// default), and each axis floors at 1 independently so a pathological
// k > resolved configuration still stamps a real, if tiny, bound rather than
// 0 — which chsql's own ctx lookup treats as "absent" (see
// apportionRangeBucketGridNativeBounds's own doc).
func TestApportionRangeBucketGridNativeBounds(t *testing.T) {
	for _, tc := range []struct {
		name             string
		rows, du, k      int64
		wantRows, wantDU int64
	}{
		{
			name: "positive overrides divide evenly by k",
			rows: 100, du: 200, k: 4,
			wantRows: 25, wantDU: 50,
		},
		{
			name: "zero overrides resolve to the compiled default before dividing",
			rows: 0, du: 0, k: 5,
			wantRows: 25_000_000 / 5, wantDU: 400_000_000 / 5,
		},
		{
			name: "negative overrides resolve to the compiled default before dividing",
			rows: -1, du: -1, k: 5,
			wantRows: 25_000_000 / 5, wantDU: 400_000_000 / 5,
		},
		{
			name: "k larger than the resolved bound floors each axis at 1",
			rows: 2, du: 3, k: 10,
			wantRows: 1, wantDU: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotRows, gotDU := apportionRangeBucketGridNativeBounds(tc.rows, tc.du, tc.k)
			if gotRows != tc.wantRows {
				t.Errorf("rows = %d, want %d", gotRows, tc.wantRows)
			}
			if gotDU != tc.wantDU {
				t.Errorf("densityUnits = %d, want %d", gotDU, tc.wantDU)
			}
		})
	}
}
