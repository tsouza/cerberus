package chsql_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// TestEmitTopK_GuardClausesRejectEachInvalidCombination pins the four
// mutually-exclusive-shape guards emitTopK checks before it renders
// anything: a topk/bottomk with no SortExpr, a K that resolves to
// non-positive with no computed-K subtree, and a node that carries BOTH
// a literal K and a computed KExpr. Each case isolates exactly one guard —
// every other guard's condition is left false — so a mutation that widens
// or narrows any single guard's boundary flips exactly one of these cases
// from rejected to accepted (or vice versa) and the test catches it.
func TestEmitTopK_GuardClausesRejectEachInvalidCombination(t *testing.T) {
	t.Parallel()

	input := &chplan.Scan{Table: "samples"}
	sortExpr := &chplan.ColumnRef{Name: "Value"}
	kExpr := &chplan.Scan{Table: "scalar_input", Roles: []chplan.Column{{Name: "k", Role: chplan.RoleValue}}}

	cases := []struct {
		name      string
		plan      *chplan.TopK
		wantError string
	}{
		{
			// Ordered (Unordered=false) with no SortExpr: there is nothing
			// to rank by, so this must be rejected rather than silently
			// falling through to limitk's "arbitrary K rows" behaviour.
			name:      "ordered with nil SortExpr",
			plan:      &chplan.TopK{Input: input, K: 1, SortExpr: nil, Unordered: false},
			wantError: "TopK with nil SortExpr",
		},
		{
			// K == 0 is the exact boundary between "non-positive" (rejected)
			// and "positive" (accepted): <= 0 must still catch it, and != 0
			// negation must still catch it.
			name:      "zero K with no KExpr",
			plan:      &chplan.TopK{Input: input, K: 0, SortExpr: sortExpr, Unordered: false},
			wantError: "TopK with non-positive K=0",
		},
		{
			// A negative K must be rejected the same way as zero — pins the
			// same guard from the other side of the boundary.
			name:      "negative K with no KExpr",
			plan:      &chplan.TopK{Input: input, K: -3, SortExpr: sortExpr, Unordered: false},
			wantError: "TopK with non-positive K=-3",
		},
		{
			// A literal, positive K together with a computed-K subtree is
			// ambiguous about which one actually bounds the result.
			name:      "positive K with KExpr set",
			plan:      &chplan.TopK{Input: input, K: 1, KExpr: kExpr, SortExpr: sortExpr, Unordered: false},
			wantError: "TopK with both literal K=1 and KExpr set",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			sql, _, err := chsql.Emit(context.Background(), tc.plan)
			if err == nil {
				t.Fatalf("Emit(%+v) returned no error; it emitted:\n%s", tc.plan, sql)
			}
			if !errors.Is(err, chsql.ErrUnsupported) {
				t.Errorf("error %v does not wrap chsql.ErrUnsupported", err)
			}
			if !strings.Contains(err.Error(), tc.wantError) {
				t.Errorf("error = %q, want it to mention %q", err.Error(), tc.wantError)
			}
		})
	}
}
