package promql

import (
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// Every discriminator predicate resolves the column by ROLE. A mixed
// relation whose discriminator is not literally named
// chplan.MixedDiscriminatorColumn (here: "source_kind") must still
// partition into its float and histogram sides, and the float side must
// still read as a float-narrowing proof — a predicate written against the
// canonical name would reference a column the relation does not have.
func TestMixedDiscriminatorPredicatesResolveByRole(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	live, _ := mixedConsumerTestRows(s)
	const wantName = "source_kind"

	assertDiscriminatorPredicate := func(t *testing.T, f *chplan.Filter, want int64) {
		t.Helper()
		predicate, ok := f.Predicate.(*chplan.Binary)
		if !ok || predicate.Op != chplan.OpEq {
			t.Fatalf("predicate = %#v, want <discriminator> = %d", f.Predicate, want)
		}
		column, ok := predicate.Left.(*chplan.ColumnRef)
		if !ok || column.Name != wantName {
			t.Fatalf("discriminator column = %#v, want the role-resolved %q", predicate.Left, wantName)
		}
		lit, ok := predicate.Right.(*chplan.LitInt)
		if !ok || lit.V != want {
			t.Fatalf("discriminator literal = %#v, want %d", predicate.Right, want)
		}
	}

	t.Run("float narrowing", func(t *testing.T) {
		t.Parallel()
		f, ok := mixedRowsFloatOnly(live).(*chplan.Filter)
		if !ok {
			t.Fatalf("mixedRowsFloatOnly = %T, want *chplan.Filter", mixedRowsFloatOnly(live))
		}
		assertDiscriminatorPredicate(t, f, mixedDiscriminatorFloat)
		if !chplan.IsMixedFloatNarrowing(f) {
			t.Fatal("the role-resolved float filter is not recognised as a float-narrowing proof")
		}
	})
	t.Run("partition filters", func(t *testing.T) {
		t.Parallel()
		assertDiscriminatorPredicate(t, mixedDiscriminatorFilter(live, mixedDiscriminatorFloat), mixedDiscriminatorFloat)
		assertDiscriminatorPredicate(t, mixedDiscriminatorFilter(live, mixedDiscriminatorHistogram), mixedDiscriminatorHistogram)
	})
	t.Run("sum partition", func(t *testing.T) {
		t.Parallel()
		expr, err := parser.NewParser(parser.Options{}).ParseExpr(`sum(latency_exp_hist)`)
		if err != nil {
			t.Fatal(err)
		}
		at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		ctx := lowerCtx{start: at, end: at, lowerers: RangeLowerers{}.withDefaults(), resourceBounds: DefaultResourceBounds().withDefaults()}
		plan, err := lowerSumOrAvgOverMixedPlan(expr.(*parser.AggregateExpr), live, s, ctx)
		if err != nil {
			t.Fatal(err)
		}
		found := map[int64]bool{}
		chplan.WalkDeep(plan, func(n chplan.Node) bool {
			f, ok := n.(*chplan.Filter)
			if !ok || f.Input != live {
				return true
			}
			predicate, ok := f.Predicate.(*chplan.Binary)
			if !ok {
				return true
			}
			column, ok := predicate.Left.(*chplan.ColumnRef)
			lit, litOK := predicate.Right.(*chplan.LitInt)
			if !ok || !litOK {
				return true
			}
			if column.Name != wantName {
				t.Fatalf("sum partition reads discriminator %q, want the role-resolved %q", column.Name, wantName)
			}
			found[lit.V] = true
			return true
		})
		if !found[mixedDiscriminatorFloat] || !found[mixedDiscriminatorHistogram] {
			t.Fatalf("sum partition filters found = %v, want both %d and %d over the live relation", found, mixedDiscriminatorFloat, mixedDiscriminatorHistogram)
		}
	})
}
