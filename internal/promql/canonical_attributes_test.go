package promql

import (
	"context"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// canonicalAttributesExpr establishes an invariant the whole engine leans
// on, so the tests below assert it over a whole lowered plan rather than
// at the one call site. Everything downstream — the LWR staleness
// collapse, a RangeWindow's partition, `without(...)`, `topk`'s
// PARTITION BY, the vector-join arms — keys series identity on the
// Attributes Map, and a CH Map compares positionally over its (keys,
// values) arrays. One un-canonicalised binding anywhere on the read path
// is enough to split a logical series in two.

// TestCanonicalAttributesExpr_WrapsInMapSort pins the helper's shape: the
// emitters and the `mapSort` name are what make the invariant real, so a
// rename or an accidental pass-through must fail here first.
func TestCanonicalAttributesExpr_WrapsInMapSort(t *testing.T) {
	t.Parallel()
	inner := &chplan.ColumnRef{Name: "Attributes"}
	got, ok := canonicalAttributesExpr(inner).(*chplan.FuncCall)
	if !ok {
		t.Fatalf("got %T, want *chplan.FuncCall", canonicalAttributesExpr(inner))
	}
	if got.Name != "mapSort" {
		t.Fatalf("func name: got %q, want %q", got.Name, "mapSort")
	}
	if len(got.Args) != 1 || got.Args[0] != chplan.Expr(inner) {
		t.Fatalf("args: got %#v, want exactly the input expr", got.Args)
	}
}

// TestSelectorAttributesExpr_AlwaysCanonicalOrBare pins the two lowering
// entry points that bind the Attributes column. Either they canonicalise,
// or they return the bare ColumnRef — the catalog / pre-merged cases,
// where the value was already canonicalised upstream. What must never
// happen is a merged-but-unsorted map, which reads as a valid label set
// while carrying whatever key order the store handed back.
func TestSelectorAttributesExpr_AlwaysCanonicalOrBare(t *testing.T) {
	t.Parallel()

	custom := schema.DefaultOTelMetrics()
	custom.ResourceAttributesColumn = ""
	custom.ServiceNameColumn = ""

	cases := []struct {
		name   string
		s      schema.Metrics
		ctx    lowerCtx
		isBare bool
	}{
		{name: "default_schema", s: schema.DefaultOTelMetrics()},
		{name: "resource_merge_opted_out", s: custom},
		{
			name:   "catalog_mode",
			s:      schema.DefaultOTelMetrics(),
			ctx:    lowerCtx{catalog: &metadataCatalog{}},
			isBare: true,
		},
		{
			name:   "pre_merged_by_arms",
			s:      schema.DefaultOTelMetrics(),
			ctx:    lowerCtx{attributesPreMerged: true},
			isBare: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := selectorAttributesExpr(tc.ctx, tc.s)
			if tc.isBare {
				if !isBareAttributesRef(got, tc.s) {
					t.Fatalf("got %#v, want the bare %s ColumnRef", got, tc.s.AttributesColumn)
				}
				return
			}
			call, ok := got.(*chplan.FuncCall)
			if !ok || call.Name != "mapSort" {
				t.Fatalf("got %#v, want a mapSort(...) wrap", got)
			}
		})
	}
}

// TestLoweredPlans_AttributesBindingsAreCanonical walks the plan for a
// corpus spanning every shape that keys on the Attributes Map and asserts
// that no projection binds the Attributes alias to an expression that
// reads the raw column without sorting it.
//
// The corpus is the point: the fix lives at one projection precisely so
// that adding a new operator cannot reintroduce the split, and a
// whole-plan walk is what makes that claim checkable.
func TestLoweredPlans_AttributesBindingsAreCanonical(t *testing.T) {
	t.Parallel()

	queries := []string{
		"mko",
		"count(mko)",
		"sum(mko)",
		"sum by (job, dc) (mko)",
		"sum without (dc) (mko)",
		"count by (job) (mko)",
		"topk(3, mko)",
		"increase(mko[5m])",
		"rate(mko[5m])",
		"max_over_time(mko[5m])",
		"avg_over_time(mko[5m])",
		"absent_over_time(mko[5m])",
		`label_replace(mko, "dc", "eu", "", "")`,
		"mko / ignoring(dc) mko_b",
		"mko unless mko_b",
		"histogram_quantile(0.9, sum by (le) (rate(mko[5m])))",
	}

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	for _, q := range queries {
		t.Run(q, func(t *testing.T) {
			expr, err := p.ParseExpr(q)
			if err != nil {
				t.Fatalf("parse %q: %v", q, err)
			}
			plan, err := Lower(context.Background(), expr, s)
			if err != nil {
				t.Fatalf("lower %q: %v", q, err)
			}
			assertAttributesBindingsCanonical(t, plan, s)
		})
	}
}

// assertAttributesBindingsCanonical walks every node of plan and fails on
// any projection aliased to the Attributes column whose expression reads
// that column without a `mapSort` on the path from the binding's root down
// to the read.
func assertAttributesBindingsCanonical(t *testing.T, plan chplan.Node, s schema.Metrics) {
	t.Helper()
	chplan.Walk(plan, func(n chplan.Node) bool {
		proj, ok := n.(*chplan.Project)
		if !ok {
			return true
		}
		for _, p := range proj.Projections {
			if p.Alias != s.AttributesColumn {
				continue
			}
			// A bare pass-through of an already-canonical upstream
			// binding reads the column without sorting it again, which
			// is correct and must not be flagged.
			if ref, isRef := p.Expr.(*chplan.ColumnRef); isRef && ref.Name == s.AttributesColumn {
				continue
			}
			if !readsAttributesUnderMapSort(p.Expr, s.AttributesColumn, false) {
				t.Errorf("projection %q binds an un-canonicalised map: %#v", p.Alias, p.Expr)
			}
		}
		return true
	})
}

// readsAttributesUnderMapSort reports whether every read of column inside
// expr sits below a `mapSort`. sorted carries whether an enclosing
// mapSort has already been seen on the current path.
func readsAttributesUnderMapSort(expr chplan.Expr, column string, sorted bool) bool {
	switch v := expr.(type) {
	case *chplan.ColumnRef:
		return sorted || v.Name != column
	case *chplan.FuncCall:
		if v.Name == "mapSort" {
			sorted = true
		}
		for _, a := range v.Args {
			if !readsAttributesUnderMapSort(a, column, sorted) {
				return false
			}
		}
		return true
	default:
		// Anything else cannot itself be the Attributes read; the
		// canonicalisation always roots the binding, so a non-call root
		// that is not a ColumnRef carries no raw map to sort.
		return true
	}
}
