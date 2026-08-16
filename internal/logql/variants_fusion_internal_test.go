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
		&chplan.FuncCall{Fn: chplan.FnLength, Args: []chplan.Expr{&chplan.ColumnRef{Name: "Body"}}},
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
		if v.ValueColumn != variantValueSlotColumn(i) {
			t.Errorf("arm %d value column = %q, want %q", i, v.ValueColumn, variantValueSlotColumn(i))
		}
	}
}

// TestFuseVariantArms_FusesSharedValues pins the many-to-one value layout:
// arms that differ only in their reducer share one projected value slot, and
// a third distinct expression gets the next slot without disturbing arm
// labels or order.
func TestFuseVariantArms_FusesSharedValues(t *testing.T) {
	t.Parallel()
	sharedValue := &chplan.ColumnRef{Name: "latency"}
	arms := []chplan.Node{
		variantArm("max_over_time", sharedValue, "otel_logs"),
		variantArm("min_over_time", sharedValue, "otel_logs"),
		bytesArm(),
	}
	fused, ok := fuseVariantArms(arms)
	if !ok {
		t.Fatal("shared-value arms were not fused")
	}
	proj := fused.Input.(*chplan.Project)
	if len(proj.Projections) != 4 {
		t.Fatalf("shared Project has %d projections, want 4 (2 shared + 2 distinct values)",
			len(proj.Projections))
	}
	wantColumns := []string{"Value_0", "Value_0", "Value_1"}
	for i, want := range wantColumns {
		if got := fused.Variants[i].ValueColumn; got != want {
			t.Errorf("arm %d value column = %q, want %q", i, got, want)
		}
		if got := fused.Variants[i].Label; got != variantLabelFor(i) {
			t.Errorf("arm %d label = %q, want %q", i, got, variantLabelFor(i))
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

// groupedArm wraps a range-aggregation arm in the vector-aggregation pipeline
// `sum by (app) (…)` lowers to: an Aggregate over the window, then the
// re-shaping Project into the Sample contract.
func groupedArm(rw *chplan.RangeWindow, groupKey string) chplan.Node {
	return &chplan.Project{
		Input: &chplan.Aggregate{
			Input: rw,
			GroupBy: []chplan.Expr{
				&chplan.MapAccess{
					Map: &chplan.ColumnRef{Name: "ResourceAttributes"},
					Key: &chplan.LitString{V: groupKey},
				},
			},
			GroupByAliases: []string{"gkey_0"},
			AggFuncs: []chplan.AggFunc{{
				Name:  "sum",
				Args:  []chplan.Expr{&chplan.ColumnRef{Name: "Value"}},
				Alias: "Value",
			}},
		},
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: "MetricName"},
			{Expr: &chplan.FuncCall{Fn: chplan.FnMap, Args: []chplan.Expr{
				&chplan.LitString{V: groupKey}, &chplan.ColumnRef{Name: "gkey_0"},
			}}, Alias: "Attributes"},
			{Expr: chplan.NowNano(), Alias: "TimeUnix"},
			{Expr: &chplan.ColumnRef{Name: "Value"}, Alias: "Value"},
		},
	}
}

// TestFuseVariants_FusesGroupedArms pins the shape Grafana's Logs Drilldown
// breakdown panels generate — `sum by (…)` over each arm. The per-arm
// pipelines are identical, so they collapse to ONE pipeline over the fused
// window, with the variant column threaded through every grouping above it so
// the arms stay separated.
func TestFuseVariants_FusesGroupedArms(t *testing.T) {
	t.Parallel()
	arms := []chplan.Node{
		groupedArm(countArm(), "app"),
		groupedArm(bytesArm(), "app"),
	}
	top, ok := fuseVariants(arms)
	if !ok {
		t.Fatal("identically-wrapped arms were not fused")
	}
	rw, fused := fusedVariantWindow(top)
	if !fused {
		t.Fatal("fused plan carries no fused window")
	}
	if len(rw.Variants) != 2 {
		t.Fatalf("fused window carries %d arms, want 2", len(rw.Variants))
	}

	// The pipeline must be applied ONCE.
	proj, ok := top.(*chplan.Project)
	if !ok {
		t.Fatalf("fused top is %T, want *chplan.Project", top)
	}
	agg, ok := proj.Input.(*chplan.Aggregate)
	if !ok {
		t.Fatalf("fused pipeline holds %T where the Aggregate belongs", proj.Input)
	}
	if agg.Input != chplan.Node(rw) {
		t.Error("the Aggregate does not sit directly on the fused window")
	}

	// The variant column must be threaded through both levels, or the
	// Aggregate would merge the arms' rows into one.
	if len(agg.GroupBy) != 2 {
		t.Fatalf("Aggregate groups by %d keys, want 2 (user key + variant)", len(agg.GroupBy))
	}
	if got := agg.GroupByAliases; len(got) != 2 || got[1] != variantLabel {
		t.Errorf("Aggregate GroupByAliases = %v, want the variant column appended", got)
	}
	var carried bool
	for _, p := range proj.Projections {
		if p.Alias == variantLabel {
			carried = true
		}
	}
	if !carried {
		t.Error("the re-shaping Project drops the variant column")
	}
}

// TestFuseVariants_RefusesDivergentWraps pins that arms whose per-arm
// pipelines differ keep the per-arm shape: one pipeline cannot serve two
// different ones, and applying either would report the wrong answer for the
// other arm.
func TestFuseVariants_RefusesDivergentWraps(t *testing.T) {
	t.Parallel()
	cases := map[string][]chplan.Node{
		// `variants(sum by (svc) (count_over_time(…)), count_over_time(…))`
		// — one arm aggregates, the other does not.
		"wrapped and bare": {
			groupedArm(countArm(), "app"),
			bytesArm(),
		},
		// Different grouping keys are different pipelines.
		"different group keys": {
			groupedArm(countArm(), "app"),
			groupedArm(bytesArm(), "svc"),
		},
		// A Having clause would need rewriting per arm.
		"aggregate with having": func() []chplan.Node {
			a := groupedArm(countArm(), "app")
			a.(*chplan.Project).Input.(*chplan.Aggregate).Having = &chplan.ColumnRef{Name: "keep"}
			return []chplan.Node{a, groupedArm(bytesArm(), "app")}
		}(),
		// A node the variant column cannot be threaded through.
		"unthreadable pipeline node": {
			&chplan.Limit{Input: countArm(), Count: 5},
			&chplan.Limit{Input: bytesArm(), Count: 5},
		},
	}
	for name, arms := range cases {
		if _, ok := fuseVariants(arms); ok {
			t.Errorf("%s: fused, want refusal", name)
		}
	}
}

// TestFuseVariants_RefusesPreexistingVariantColumn pins that a pipeline
// already carrying a `__variant__` column is left alone: threading a second
// one would leave the arms' identity depending on which column a consumer
// read.
func TestFuseVariants_RefusesPreexistingVariantColumn(t *testing.T) {
	t.Parallel()
	withCol := func(rw *chplan.RangeWindow) chplan.Node {
		p := groupedArm(rw, "app").(*chplan.Project)
		p.Projections = append(p.Projections, chplan.Projection{
			Expr: &chplan.LitString{V: "x"}, Alias: variantLabel,
		})
		return p
	}
	if _, ok := fuseVariants([]chplan.Node{withCol(countArm()), withCol(bytesArm())}); ok {
		t.Error("fused a pipeline that already carries the variant column, want refusal")
	}
}

// ungroupedArm wraps a range-aggregation arm in the pipeline `sum(...)` with
// NO `by` clause lowers to: an Aggregate with an EMPTY GroupBy (carrying
// DropEmptyOnNoGroup) and therefore empty GroupByAliases.
func ungroupedArm(rw *chplan.RangeWindow) chplan.Node {
	return &chplan.Project{
		Input: &chplan.Aggregate{
			Input: rw,
			AggFuncs: []chplan.AggFunc{{
				Name:  "sum",
				Args:  []chplan.Expr{&chplan.ColumnRef{Name: "Value"}},
				Alias: "Value",
			}},
			DropEmptyOnNoGroup: true,
		},
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: "MetricName"},
			{Expr: emptyAttrsMap(), Alias: "Attributes"},
			{Expr: chplan.NowNano(), Alias: "TimeUnix"},
			{Expr: &chplan.ColumnRef{Name: "Value"}, Alias: "Value"},
		},
	}
}

// TestFuseVariants_EmptyGroupByAliasesStayEmpty pins the parallel-slice
// contract on chplan.Aggregate: GroupByAliases is either empty or exactly as
// long as GroupBy. An ungrouped `sum(...)` arm aggregates with no aliases, so
// threading the variant column must extend GroupBy WITHOUT starting an alias
// list — a one-alias-for-two-keys slice violates the node's own invariant.
func TestFuseVariants_EmptyGroupByAliasesStayEmpty(t *testing.T) {
	t.Parallel()
	top, ok := fuseVariants([]chplan.Node{ungroupedArm(countArm()), ungroupedArm(bytesArm())})
	if !ok {
		t.Fatal("ungrouped arms were not fused")
	}
	agg, ok := top.(*chplan.Project).Input.(*chplan.Aggregate)
	if !ok {
		t.Fatalf("fused pipeline holds %T where the Aggregate belongs", top.(*chplan.Project).Input)
	}
	if len(agg.GroupBy) != 1 {
		t.Fatalf("Aggregate groups by %d keys, want 1 (the variant column alone)", len(agg.GroupBy))
	}
	if len(agg.GroupByAliases) != 0 {
		t.Errorf("GroupByAliases = %v, want it left empty so it stays parallel to GroupBy",
			agg.GroupByAliases)
	}
}

// TestFuseVariantArms_ValueProjectionFirst pins that the differing projection
// is located by its ALIAS, not by its position. A lowering that put the value
// first would otherwise be silently refused.
func TestFuseVariantArms_ValueProjectionFirst(t *testing.T) {
	t.Parallel()
	valueFirst := func(fn string, value chplan.Expr) *chplan.RangeWindow {
		rw := variantArm(fn, value, "otel_logs")
		p := rw.Input.(*chplan.Project)
		p.Projections = []chplan.Projection{
			{Expr: value, Alias: rangeAggSynthValueColumn},
			{Expr: &chplan.ColumnRef{Name: "ResourceAttributes"}, Alias: "ResourceAttributes"},
			{Expr: &chplan.ColumnRef{Name: "Timestamp"}},
		}
		return rw
	}
	arms := []chplan.Node{
		valueFirst("count_over_time", &chplan.LitInt{V: 1}),
		valueFirst("sum_over_time", &chplan.ColumnRef{Name: "Len"}),
	}
	fused, ok := fuseVariantArms(arms)
	if !ok {
		t.Fatal("arms differing at projection 0 (the value alias) were not fused")
	}
	proj := fused.Input.(*chplan.Project)
	// Both arms' values land first, then the shared projections follow.
	for i, alias := range []string{"Value_0", "Value_1", "ResourceAttributes"} {
		if got := proj.Projections[i].Alias; got != alias {
			t.Errorf("projection %d alias = %q, want %q", i, got, alias)
		}
	}
}

// TestProjectionEqual_NilExpressions pins the nil-expression guard. A
// Projection carrying no expression compares equal only to another one, and
// the mixed case must report unequal rather than dereference the nil.
func TestProjectionEqual_NilExpressions(t *testing.T) {
	t.Parallel()
	nilProj := chplan.Projection{Alias: "Value"}
	setProj := chplan.Projection{Alias: "Value", Expr: &chplan.LitInt{V: 1}}
	if !projectionEqual(nilProj, nilProj) {
		t.Error("two nil-expression projections compared unequal")
	}
	if projectionEqual(nilProj, setProj) || projectionEqual(setProj, nilProj) {
		t.Error("a nil-expression projection compared equal to one carrying an expression")
	}
}

// TestRebuildVariantWrap_RefusesPassThroughProject pins the fail-closed guard
// on the pass-through `SELECT *` Project shape (and its `* REPLACE (...)`
// modifier). Appending the variant column to an empty projection list would
// turn `SELECT *` into a one-column SELECT and drop every other column.
func TestRebuildVariantWrap_RefusesPassThroughProject(t *testing.T) {
	t.Parallel()
	passThrough := []chplan.Node{&chplan.Project{}}
	if _, ok := rebuildVariantWrap(passThrough, &chplan.OneRow{}, variantLabel); ok {
		t.Error("threaded the variant column through a pass-through Project, want refusal")
	}
	withReplacements := []chplan.Node{&chplan.Project{
		Projections:  []chplan.Projection{{Expr: &chplan.LitInt{V: 1}, Alias: "Value"}},
		Replacements: []chplan.Projection{{Expr: &chplan.LitInt{V: 2}, Alias: "Body"}},
	}}
	if _, ok := rebuildVariantWrap(withReplacements, &chplan.OneRow{}, variantLabel); ok {
		t.Error("threaded the variant column through a Project carrying Replacements, want refusal")
	}
}
