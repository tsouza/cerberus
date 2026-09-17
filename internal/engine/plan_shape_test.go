package engine

import (
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// shapeOf inspects plan against the default OTel traces schema's trace-id
// column — what every production caller resolves from its own
// SettingsRules.Traces.
func shapeOf(plan chplan.Node) planShapeFacts {
	return inspectPlanShape(plan, schema.DefaultOTelTraces().TraceIDColumn)
}

// TestInspectPlanShape_SpineFactsStopAtExprSlots pins the one place the
// merged inspection deliberately keeps two reaches. The aggregation-in-order
// and condition-cache checks reason about the row-flow spine (chplan.Walk),
// so an Aggregate / Scan / Filter that only exists inside a ScalarSubquery's
// embedded plan must NOT count toward them — while every deep fact (the
// memory bounds, lazy materialisation, the trace-id predicate, now()) MUST
// see the same embedded subtree.
func TestInspectPlanShape_SpineFactsStopAtExprSlots(t *testing.T) {
	t.Parallel()

	embedded := &chplan.Limit{
		Count: 7,
		Input: &chplan.OrderBy{
			Input: &chplan.Filter{
				Input: aggOverScan("otel_metrics_sum", "MetricName"),
				Predicate: &chplan.Binary{
					Op:    chplan.OpEq,
					Left:  &chplan.ColumnRef{Name: schema.DefaultOTelTraces().TraceIDColumn},
					Right: &chplan.FuncCall{Fn: chplan.FnNow},
				},
			},
		},
	}
	plan := &chplan.Project{
		Input: &chplan.Scan{Table: "otel_metrics_gauge"},
		Projections: []chplan.Projection{{
			Expr:  &chplan.ScalarSubquery{Input: embedded},
			Alias: "Value",
		}},
	}

	f := shapeOf(plan)

	// Spine: the outer Scan only.
	if _, ok := f.singleAggregate(); ok {
		t.Error("an Aggregate inside a ScalarSubquery counted as the spine's single Aggregate")
	}
	if table, ok := f.singleScanTable(); !ok || table != "otel_metrics_gauge" {
		t.Errorf("singleScanTable = %q, %v; want the outer Scan only", table, ok)
	}
	if f.spineHasFilter {
		t.Error("a Filter inside a ScalarSubquery counted as a spine Filter")
	}

	// Deep: everything inside the subquery.
	if limit, ok := f.lazyMaterializationLimit(); !ok || limit != 7 {
		t.Errorf("lazyMaterializationLimit = %d, %v; want the embedded Limit(OrderBy) to be found", limit, ok)
	}
	if !f.traceIDPredicate {
		t.Error("the embedded TraceId equality predicate was not found")
	}
	if !f.hasNowExpr {
		t.Error("the embedded now() call was not found")
	}
}

// TestInspectPlanShape_DeepFactsReachExprSlots pins that every memory-bound
// fact is deep: the same shape found at the top level is found inside a
// ScalarSubquery, because a bound a walk fails to see is a bound that never
// fires.
func TestInspectPlanShape_DeepFactsReachExprSlots(t *testing.T) {
	t.Parallel()

	nest := func(inner chplan.Node) chplan.Node {
		return &chplan.Project{
			Input:       &chplan.Scan{Table: "otel_metrics_gauge"},
			Projections: []chplan.Projection{{Expr: &chplan.ScalarSubquery{Input: inner}, Alias: "Value"}},
		}
	}
	cases := []struct {
		name  string
		inner chplan.Node
		fact  func(planShapeFacts) bool
	}{
		{"compare", compareOverScan(), func(f planShapeFacts) bool { return f.hasCompare }},
		{"join", &chplan.VectorJoin{}, func(f planShapeFacts) bool { return f.hasJoin }},
		{"sorted slab", &chplan.RangeWindow{Input: &chplan.Scan{Table: "otel_metrics_sum"}, SortedSlabOverTime: true}, func(f planShapeFacts) bool { return f.sortedSlabOverTime }},
		{"native histogram", expHistogramWindowPlan(), func(f planShapeFacts) bool { return f.nativeHistogramAnalyzerHazard }},
		{"exp-histogram window", expHistogramWindowPlan(), func(f planShapeFacts) bool { return f.expHistogramWindowGrouping }},
		{"ts grid", &chplan.RangeWindowGridNative{Input: &chplan.Scan{Table: "otel_metrics_sum"}}, func(f planShapeFacts) bool { return f.tsGridNative }},
		{"structural join", &chplan.StructuralJoin{}, func(f planShapeFacts) bool { return f.hasStructuralJoin }},
		{"range carrier", &chplan.RangeWindow{Input: &chplan.Scan{Table: "otel_metrics_sum"}, Start: fixedNow.Add(-time.Hour), End: fixedNow, Step: time.Minute}, func(f planShapeFacts) bool { return f.rangeCarriers == 1 }},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if !tc.fact(shapeOf(tc.inner)) {
				t.Fatalf("%s: the fact is not set on the shape at top level; the nested case below would be vacuous", tc.name)
			}
			if !tc.fact(shapeOf(nest(tc.inner))) {
				t.Errorf("%s: the fact is lost once the shape sits inside a ScalarSubquery", tc.name)
			}
		})
	}
}

// TestInspectPlanShape_UnionScanIsNeverASingleTable pins that a union or
// table-less Scan on the spine makes singleScanTable fail closed regardless
// of how many plain Scans sit beside it: a union has no single sort key to
// be a prefix of, and a second plain Scan cannot cancel that out.
func TestInspectPlanShape_UnionScanIsNeverASingleTable(t *testing.T) {
	t.Parallel()

	plan := &chplan.VectorJoin{
		Left:  &chplan.Scan{Table: "otel_metrics_sum", UnionTables: []string{"otel_metrics_sum", "otel_metrics_gauge"}},
		Right: &chplan.CrossJoin{Left: &chplan.Scan{Table: "otel_metrics_gauge"}, Right: &chplan.Scan{Table: "otel_metrics_gauge"}},
	}
	if table, ok := shapeOf(plan).singleScanTable(); ok {
		t.Errorf("singleScanTable = %q, ok; want fail-closed on a union Scan", table)
	}
}
