package logql

import (
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// variantArm builds a lowered variant arm of the shape the range-aggregation
// lowering produces: RangeWindow over a Project that computes the per-sample
// value, over a shared Filter/Scan subtree.
func variantArm(fn string, value chplan.Expr, table string) *chplan.RangeWindow {
	return &chplan.RangeWindow{
		Input: &chplan.Project{
			Input: &chplan.Filter{
				Input:     &chplan.Scan{Table: table},
				Predicate: &chplan.ColumnRef{Name: "keep"},
			},
			Projections: []chplan.Projection{
				{Expr: &chplan.ColumnRef{Name: "ResourceAttributes"}, Alias: "ResourceAttributes"},
				{Expr: &chplan.ColumnRef{Name: "Timestamp"}},
				{Expr: value, Alias: rangeAggSynthValueColumn},
			},
		},
		Func:            fn,
		Range:           5 * time.Minute,
		TimestampColumn: "Timestamp",
		ValueColumn:     rangeAggSynthValueColumn,
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "ResourceAttributes"}},
	}
}

func countArm() *chplan.RangeWindow {
	return variantArm("count_over_time", &chplan.LitInt{V: 1}, "otel_logs")
}

func bytesArm() *chplan.RangeWindow {
	return variantArm("sum_over_time",
		&chplan.FuncCall{Name: "length", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Body"}}},
		"otel_logs")
}

// TestFuseVariantArms_FusesSharedStream is the happy path: two arms over one
// stream collapse into a single window carrying both, reading the shared
// subtree once.
func TestFuseVariantArms_FusesSharedStream(t *testing.T) {
	t.Parallel()
	arms := []chplan.Node{countArm(), bytesArm()}
	fused, ok := fuseVariantArms(arms)
	if !ok {
		t.Fatal("arms over one stream were not fused")
	}
	if len(fused.Variants) != 2 {
		t.Fatalf("fused window carries %d arms, want 2", len(fused.Variants))
	}
	want := []chplan.RangeWindowVariant{
		{Func: "count_over_time", ValueColumn: "Value_0", Label: "0"},
		{Func: "sum_over_time", ValueColumn: "Value_1", Label: "1"},
	}
	for i, w := range want {
		if !fused.Variants[i].Equal(w) {
			t.Errorf("arm %d = %+v, want %+v", i, fused.Variants[i], w)
		}
	}
	if fused.VariantColumn != variantLabel {
		t.Errorf("VariantColumn = %q, want %q", fused.VariantColumn, variantLabel)
	}
	if fused.Func != "" {
		t.Errorf("fused Func = %q, want empty (each arm names its own)", fused.Func)
	}
	// The shared input Project must carry BOTH arms' value expressions and
	// exactly one copy of every shared projection.
	proj, ok := fused.Input.(*chplan.Project)
	if !ok {
		t.Fatalf("fused Input is %T, want *chplan.Project", fused.Input)
	}
	if len(proj.Projections) != 4 {
		t.Fatalf("shared Project has %d projections, want 4 (2 shared + 2 values)",
			len(proj.Projections))
	}
	for i, alias := range []string{"Value_0", "Value_1"} {
		if got := proj.Projections[2+i].Alias; got != alias {
			t.Errorf("projection %d alias = %q, want %q", 2+i, got, alias)
		}
	}
	// One shared subtree, not two.
	if !proj.Input.Equal(countArm().Input.(*chplan.Project).Input) {
		t.Error("fused Project does not read the arms' shared subtree")
	}
}

// TestFuseVariantArms_RefusesDivergentArms pins the cases the fusion must
// decline. Each one would produce a wrong answer or an unrenderable plan if
// fused, so declining is the correct outcome — the query keeps the per-arm
// shape, which costs a scan per arm but answers correctly.
func TestFuseVariantArms_RefusesDivergentArms(t *testing.T) {
	t.Parallel()
	cases := map[string][]chplan.Node{
		// A lone arm has nothing to share a pass with.
		"single arm": {countArm()},
		// Different selectors really do read different streams — the
		// `of (...)` hint does not make them one.
		"different tables": {
			countArm(),
			variantArm("sum_over_time", &chplan.LitInt{V: 1}, "other_logs"),
		},
		"different filters": func() []chplan.Node {
			b := bytesArm()
			b.Input.(*chplan.Project).Input.(*chplan.Filter).Predicate = &chplan.ColumnRef{Name: "other"}
			return []chplan.Node{countArm(), b}
		}(),
		// A different window grid cannot be served by one pass.
		"different ranges": func() []chplan.Node {
			b := bytesArm()
			b.Range = time.Hour
			return []chplan.Node{countArm(), b}
		}(),
		"different grouping": func() []chplan.Node {
			b := bytesArm()
			b.GroupBy = []chplan.Expr{&chplan.ColumnRef{Name: "Other"}}
			return []chplan.Node{countArm(), b}
		}(),
		// A different IDENTITY projection means different series; folding
		// them onto one grouped pass would report one arm's grouping twice.
		"different identity projection": func() []chplan.Node {
			b := bytesArm()
			b.Input.(*chplan.Project).Projections[0].Expr = &chplan.ColumnRef{Name: "Other"}
			return []chplan.Node{countArm(), b}
		}(),
		// Two differing positions: the arms diverge somewhere the single
		// fanned-out value column cannot represent.
		"two differing projections": func() []chplan.Node {
			b := bytesArm()
			b.Input.(*chplan.Project).Projections[0].Expr = &chplan.ColumnRef{Name: "Other"}
			b.Input.(*chplan.Project).Projections[1].Expr = &chplan.ColumnRef{Name: "Other"}
			return []chplan.Node{countArm(), b}
		}(),
		// The arms compute the same value: nothing to fan out.
		"identical arms": {countArm(), countArm()},
		// A function the fused emitter does not reduce over the values
		// array keeps the whole query on the per-arm path.
		"unfusible function": func() []chplan.Node {
			b := bytesArm()
			b.Func = "rate"
			return []chplan.Node{countArm(), b}
		}(),
		"quantile arm": func() []chplan.Node {
			b := bytesArm()
			b.Func = "quantile_over_time"
			return []chplan.Node{countArm(), b}
		}(),
		// Shapes the matcher does not recognise as a bare range aggregation.
		"non-window arm": {countArm(), &chplan.Scan{Table: "otel_logs"}},
		"identity window": func() []chplan.Node {
			b := bytesArm()
			b.Identity = true
			return []chplan.Node{countArm(), b}
		}(),
		"window over non-project": func() []chplan.Node {
			b := bytesArm()
			b.Input = &chplan.Scan{Table: "otel_logs"}
			return []chplan.Node{countArm(), b}
		}(),
		"already fused arm": func() []chplan.Node {
			b := bytesArm()
			b.Variants = []chplan.RangeWindowVariant{{Func: "count_over_time", ValueColumn: "V", Label: "0"}}
			return []chplan.Node{countArm(), b}
		}(),
	}
	for name, arms := range cases {
		if fused, ok := fuseVariantArms(arms); ok {
			t.Errorf("%s: fused into %+v, want refusal", name, fused.Variants)
		}
	}
}

// TestFuseVariantArms_FusesThreeArms pins that the fusion is N-ary rather than
// a two-arm special case — the cost of the fused shape is flat in the arm
// count, so a third arm must join the same pass.
func TestFuseVariantArms_FusesThreeArms(t *testing.T) {
	t.Parallel()
	third := variantArm("max_over_time", &chplan.ColumnRef{Name: "Len"}, "otel_logs")
	fused, ok := fuseVariantArms([]chplan.Node{countArm(), bytesArm(), third})
	if !ok {
		t.Fatal("three arms over one stream were not fused")
	}
	if len(fused.Variants) != 3 {
		t.Fatalf("fused window carries %d arms, want 3", len(fused.Variants))
	}
	proj := fused.Input.(*chplan.Project)
	if len(proj.Projections) != 5 {
		t.Fatalf("shared Project has %d projections, want 5 (2 shared + 3 values)",
			len(proj.Projections))
	}
	for i, v := range fused.Variants {
		if v.Label != variantLabelFor(i) {
			t.Errorf("arm %d label = %q, want %q", i, v.Label, variantLabelFor(i))
		}
		if v.ValueColumn != variantArmValueColumn(i) {
			t.Errorf("arm %d value column = %q, want %q", i, v.ValueColumn, variantArmValueColumn(i))
		}
	}
}

// TestFuseVariantArms_DoesNotMutateInputArms pins that the fusion builds a
// fresh window rather than editing an arm in place — the lowering falls back
// to the per-arm shape on refusal, and a mutated arm would corrupt it.
func TestFuseVariantArms_DoesNotMutateInputArms(t *testing.T) {
	t.Parallel()
	a, b := countArm(), bytesArm()
	beforeA, beforeB := countArm(), bytesArm()
	if _, ok := fuseVariantArms([]chplan.Node{a, b}); !ok {
		t.Fatal("arms were not fused")
	}
	if !a.Equal(beforeA) {
		t.Error("first arm was mutated by fusion")
	}
	if !b.Equal(beforeB) {
		t.Error("second arm was mutated by fusion")
	}
}

// TestIsVariantPlan_AcceptsBothShapes pins that ProjectSamples recognises the
// fused shape as well as the per-arm union — missing it would re-wrap the plan
// in the generic metric reshape, which re-references a column the variant
// Project has already consumed.
func TestIsVariantPlan_AcceptsBothShapes(t *testing.T) {
	t.Parallel()
	sampleShaped := func(input chplan.Node) chplan.Node {
		return &chplan.Project{
			Input: input,
			Projections: []chplan.Projection{
				{Expr: &chplan.LitString{V: ""}, Alias: "MetricName"},
				{Expr: &chplan.ColumnRef{Name: "ResourceAttributes"}, Alias: "Attributes"},
				{Expr: &chplan.ColumnRef{Name: "anchor_ts"}, Alias: "TimeUnix"},
				{Expr: &chplan.ColumnRef{Name: "Value"}, Alias: "Value"},
			},
		}
	}
	fused, ok := fuseVariantArms([]chplan.Node{countArm(), bytesArm()})
	if !ok {
		t.Fatal("arms were not fused")
	}
	if !isVariantPlan(sampleShaped(fused)) {
		t.Error("fused variant plan not recognised")
	}
	union := &chplan.UnionAll{Inputs: []chplan.Node{
		sampleShaped(countArm()), sampleShaped(bytesArm()),
	}}
	if !isVariantPlan(union) {
		t.Error("per-arm variant union not recognised")
	}
	// A plain range aggregation is neither.
	if isVariantPlan(sampleShaped(countArm())) {
		t.Error("a non-variant sample plan was recognised as a variant plan")
	}
	if isVariantPlan(countArm()) {
		t.Error("a bare RangeWindow was recognised as a variant plan")
	}
}
