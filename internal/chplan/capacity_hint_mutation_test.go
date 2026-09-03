package chplan

import (
	"fmt"
	"testing"
	"time"

	"github.com/tsouza/cerberus/test/capmutant"
)

// TestRecollapseIdentityKeyAliases_CapHintMutantKilled kills the ARITHMETIC_BASE
// mutant gremlins reports on
// canonical_series_keys.go:recollapseIdentityKeyAliases:`len(pass)+len(r.Recollapse)`.
//
// The mutant sizes a slice, and a capacity hint changes only allocator
// behaviour — which is exactly why this class was written off across the
// matrix. Observability is not confined to the emitted SQL, though: the slice
// this hint sizes IS the value [recollapseIdentityKeyAliases] returns, which
// [seriesIdentityKeyAliases] hands back to every caller. `cap` is readable on
// a return value from any test in this package, the appends do not grow past
// the pre-allocation, and so the finished slice still reports the hint's
// arithmetic. See docs/test-strategy.md's "When a capacity mutant is
// equivalent".
//
// [capmutant.AssertKilled] asserts that capacity against the hint AND replays
// every operator substitution to prove the assertion discriminates, so the
// claim this test makes is re-checked by the test itself rather than asserted
// in prose here.
func TestRecollapseIdentityKeyAliases_CapHintMutantKilled(t *testing.T) {
	t.Parallel()

	// The operand sizes are chosen so that no mutated hint's growth schedule
	// lands back on the true capacity; capmutant.AssertKilled reports by name
	// any substitution that would.
	const (
		passKeys   = 3
		recollapse = 2
	)

	capmutant.AssertKilled(t, capmutant.Hint{
		Construct: "canonical_series_keys.go:recollapseIdentityKeyAliases:`len(pass)+len(r.Recollapse)`",
		Positions: []capmutant.Position{{Name: "the `+`", Op: "+"}},
		Eval: func(t testing.TB, ops []string) (int, bool) {
			return capmutant.Eval(t, []int{passKeys, recollapse}, ops)
		},
		Observe: func(t *testing.T) (int, int) {
			n := recollapseNodeForCapHint(passKeys, recollapse)
			// Guard the fixture rather than the arithmetic: a node whose
			// partition differs from the one this test thinks it built would
			// adjudicate a hint with the wrong operands.
			if pass, _ := n.PartitionRecollapseGroupBy(mustGroupByColumns(t, n)); len(pass) != passKeys {
				t.Fatalf("fixture partitions %d pass-through keys, want %d", len(pass), passKeys)
			}
			aliases := seriesIdentityKeyAliases(n)
			return len(aliases), cap(aliases)
		},
		Build: func(hint int) (int, int) {
			// recollapseIdentityKeyAliases appends the pass-through aliases one
			// at a time, then the Recollapse aliases in a single variadic
			// append. The element type and the grouping both steer append's
			// growth schedule, so the replay mirrors them.
			aliases := make([]string, 0, hint)
			for i := 0; i < passKeys; i++ {
				aliases = append(aliases, "")
			}
			aliases = append(aliases, make([]string, recollapse)...)
			return len(aliases), cap(aliases)
		},
	})
}

// mustGroupByColumns answers the node's GroupBy column names, failing the test
// if any key is computed — the partition the hint's operands come from is
// undefined for such a node.
func mustGroupByColumns(t *testing.T, n *RangeWindowGridNative) []string {
	t.Helper()

	cols, computed := n.GroupByColumns()
	if computed != nil {
		t.Fatalf("fixture GroupBy carries a computed key %#v; the deferred-shaping "+
			"shape requires plain column references", computed)
	}
	return cols
}

// recollapseNodeForCapHint builds a deferred-shaping [RangeWindowGridNative]
// with exactly `pass` pass-through GroupBy keys and `recollapse` shaped ones.
// Each shaped Projection reads its OWN GroupBy key, which is what moves that
// key out of the pass-through set — see
// [RangeWindowGridNative.PartitionRecollapseGroupBy].
func recollapseNodeForCapHint(pass, recollapse int) *RangeWindowGridNative {
	groupBy := make([]Expr, 0, pass+recollapse)
	for i := 0; i < pass; i++ {
		groupBy = append(groupBy, &ColumnRef{Name: fmt.Sprintf("pass_%d", i)})
	}
	shaped := make([]Projection, 0, recollapse)
	for i := 0; i < recollapse; i++ {
		src := fmt.Sprintf("shaped_src_%d", i)
		groupBy = append(groupBy, &ColumnRef{Name: src})
		shaped = append(shaped, Projection{
			Expr:  &FuncCall{Fn: FnMapSort, Args: []Expr{&ColumnRef{Name: src}}},
			Alias: fmt.Sprintf("shaped_%d", i),
		})
	}
	return &RangeWindowGridNative{
		Input:           &Scan{Table: "otel_metrics_sum", Columns: []string{"Attributes", "Value"}},
		Func:            "rate",
		Range:           5 * time.Minute,
		Step:            time.Minute,
		Start:           time.Unix(1000, 0).UTC(),
		End:             time.Unix(4600, 0).UTC(),
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         groupBy,
		Recollapse:      shaped,
	}
}
