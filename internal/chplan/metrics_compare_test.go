package chplan_test

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// newCompareFixture builds a fully-populated MetricsCompare for the
// Equal-invariant tests; each negative case mutates one field.
func newCompareFixture() *chplan.MetricsCompare {
	return &chplan.MetricsCompare{
		Selection: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: "StatusCode"},
			Right: &chplan.LitString{V: "Error"},
		},
		TopN:    10,
		StartNs: 100,
		EndNs:   200,
		Pairs:   &chplan.FuncCall{Name: "array"},
		RootLookup: &chplan.Aggregate{
			Input:    &chplan.Scan{Table: "otel_traces"},
			GroupBy:  []chplan.Expr{&chplan.ColumnRef{Name: "TraceId"}},
			AggFuncs: []chplan.AggFunc{{Name: "any", Args: []chplan.Expr{&chplan.ColumnRef{Name: "SpanName"}}, Alias: "__root_name"}},
		},
		TraceIDColumn:    "TraceId",
		RootNameAlias:    "__root_name",
		RootServiceAlias: "__root_service_name",
		SelAlias:         "is_selection",
		AttrAlias:        "attr",
		ValAlias:         "val",
		ValueAlias:       "Value",
		Inner:            &chplan.Scan{Table: "otel_traces"},
	}
}

func TestMetricsCompare_Equal_Positive(t *testing.T) {
	t.Parallel()
	if !newCompareFixture().Equal(newCompareFixture()) {
		t.Fatal("identical MetricsCompare trees should be Equal")
	}
}

func TestMetricsCompare_Equal_Negative_Fields(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(m *chplan.MetricsCompare)
	}{
		{"topN", func(m *chplan.MetricsCompare) { m.TopN = 5 }},
		{"startNs", func(m *chplan.MetricsCompare) { m.StartNs = 0 }},
		{"endNs", func(m *chplan.MetricsCompare) { m.EndNs = 999 }},
		{"selection", func(m *chplan.MetricsCompare) {
			m.Selection = &chplan.LitBool{V: true}
		}},
		{"selectionNil", func(m *chplan.MetricsCompare) { m.Selection = nil }},
		{"pairs", func(m *chplan.MetricsCompare) {
			m.Pairs = &chplan.FuncCall{Name: "arrayConcat"}
		}},
		{"pairsNil", func(m *chplan.MetricsCompare) { m.Pairs = nil }},
		{"rootLookupNil", func(m *chplan.MetricsCompare) { m.RootLookup = nil }},
		// Both RootLookups non-nil but with different content. Kills the
		// CONDITIONALS_NEGATION at metrics_compare.go (the `!m.RootLookup.Equal`
		// guard): dropping the `!` would make divergent-but-present RootLookups
		// fall through to "equal". rootLookupNil only exercises the nil-mismatch
		// guard a line above; this exercises the content comparison.
		{"rootLookupContent", func(m *chplan.MetricsCompare) {
			m.RootLookup = &chplan.Scan{Table: "different_root_lookup"}
		}},
		{"traceIDColumn", func(m *chplan.MetricsCompare) { m.TraceIDColumn = "Other" }},
		{"rootNameAlias", func(m *chplan.MetricsCompare) { m.RootNameAlias = "x" }},
		{"rootServiceAlias", func(m *chplan.MetricsCompare) { m.RootServiceAlias = "x" }},
		{"selAlias", func(m *chplan.MetricsCompare) { m.SelAlias = "x" }},
		{"attrAlias", func(m *chplan.MetricsCompare) { m.AttrAlias = "x" }},
		{"valAlias", func(m *chplan.MetricsCompare) { m.ValAlias = "x" }},
		{"valueAlias", func(m *chplan.MetricsCompare) { m.ValueAlias = "x" }},
		{"inner", func(m *chplan.MetricsCompare) { m.Inner = &chplan.Scan{Table: "other"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a, b := newCompareFixture(), newCompareFixture()
			tc.mutate(b)
			if a.Equal(b) || b.Equal(a) {
				t.Errorf("MetricsCompare.Equal must detect %s divergence", tc.name)
			}
		})
	}
}

func TestMetricsCompare_Equal_OtherNodeType(t *testing.T) {
	t.Parallel()
	if newCompareFixture().Equal(&chplan.Scan{Table: "otel_traces"}) {
		t.Fatal("MetricsCompare.Equal(<other node type>) must be false")
	}
}

// TestChildren_MetricsCompare — Children() exposes Inner (always) and
// RootLookup (when set) so generic walkers descend into both relations.
func TestChildren_MetricsCompare(t *testing.T) {
	t.Parallel()

	m := newCompareFixture()
	kids := m.Children()
	if len(kids) != 2 || kids[0] != m.Inner || kids[1] != m.RootLookup {
		t.Errorf("Children() with RootLookup should return [Inner, RootLookup], got %v", kids)
	}

	m.RootLookup = nil
	kids = m.Children()
	if len(kids) != 1 || kids[0] != m.Inner {
		t.Errorf("Children() without RootLookup should return [Inner], got %v", kids)
	}
}
