package promql_test

import (
	"context"
	"math"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_SubqueryInnerQuantileOverTime_OutOfRangePhi_KeepsDuplicateLabelsetGuard
// pins the one shape where the subquery-inner scalar binder and the
// name-drop collision guard meet:
//
//	quantile_over_time(2, {__name__=~"a|b"}[5m])[10m:1m]
//
// Both apply at once and neither may cancel the other. `quantile_over_time`
// drops `__name__` (it is not last_over_time / first_over_time), so a
// regex-name selector hands the window two series that differ only by name —
// the case reference Prometheus refuses with "vector cannot contain metrics
// with the same labelset". Its phi is also outside [0, 1], which PromQL
// defines as ±Inf per series rather than an error, so the window ran on the
// in-domain sentinel and the spec constant must replace the Value column on
// the way out.
//
// The guard therefore has to survive the phi fold AND the phi fold has to
// survive the guard. The fold rides inside the guard's own projection: the
// guard returns a Project, so stacking projectValueOverInner on top would
// take that helper's non-RangeWindow branch and drop the `anchor_ts`
// passthrough a matrix-shaped window owes its enclosing reducer.
func TestLower_SubqueryInnerQuantileOverTime_OutOfRangePhi_KeepsDuplicateLabelsetGuard(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})

	expr, err := p.ParseExpr(`quantile_over_time(2, {__name__=~"cpu_temp|gpu_temp"}[5m])[10m:1m]`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	plan, err := promql.Lower(context.Background(), expr, s)
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}

	proj, ok := plan.(*chplan.Project)
	if !ok {
		t.Fatalf("expected the guard's *chplan.Project, got %T", plan)
	}
	agg, ok := proj.Input.(*chplan.Aggregate)
	if !ok {
		t.Fatalf("expected *chplan.Aggregate under the guard projection, got %T", proj.Input)
	}
	if agg.Having == nil {
		t.Fatal("duplicate-labelset guard: Aggregate.Having is nil — the phi fold disarmed the abort")
	}

	// The window under the guard kept the widened grouping key; without it
	// ClickHouse's GROUP BY Attributes merges the two names inside the window
	// and the HAVING has nothing left to count.
	rw, ok := agg.Input.(*chplan.RangeWindow)
	if !ok {
		t.Fatalf("expected *chplan.RangeWindow under the guard aggregate, got %T", agg.Input)
	}
	if len(rw.GroupBy) != 2 {
		t.Fatalf("window grouping key: got %d keys, want 2 (%s + %s)",
			len(rw.GroupBy), s.AttributesColumn, s.MetricNameColumn)
	}

	// The window still ran on the in-domain sentinel phi — the out-of-range
	// literal never reaches ClickHouse's quantile aggregate, which rejects it.
	if len(rw.Scalars) != 1 {
		t.Fatalf("window Scalars = %v, want exactly one sentinel phi", rw.Scalars)
	}
	if rw.Scalars[0] < 0 || rw.Scalars[0] > 1 {
		t.Fatalf("window phi = %v, want the in-domain sentinel", rw.Scalars[0])
	}

	// …and the guard's own projection carries the PromQL-spec +Inf constant
	// as Value, not the sentinel quantile the window computed.
	var valueExpr chplan.Expr
	for _, pr := range proj.Projections {
		if pr.Alias == s.ValueColumn {
			valueExpr = pr.Expr
		}
	}
	if valueExpr == nil {
		t.Fatalf("guard projection exposes no %s output: %#v", s.ValueColumn, proj.Projections)
	}
	lit, ok := valueExpr.(*chplan.LitFloat)
	if !ok {
		t.Fatalf("guard %s projection = %#v, want the folded ±Inf literal", s.ValueColumn, valueExpr)
	}
	if !math.IsInf(lit.V, 1) {
		t.Fatalf("guard %s literal = %v, want +Inf (phi > 1)", s.ValueColumn, lit.V)
	}
}

// TestLower_SubqueryInnerQuantileOverTime_SingleName_NoGuard is the negative
// half: a selector pinned to ONE metric name cannot collide on the name drop,
// so the guard must stay off and the phi fold takes the plain
// projectValueOverInner path over the window itself.
func TestLower_SubqueryInnerQuantileOverTime_SingleName_NoGuard(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})

	expr, err := p.ParseExpr(`quantile_over_time(2, cpu_temp[5m])[10m:1m]`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	plan, err := promql.Lower(context.Background(), expr, s)
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}

	proj, ok := plan.(*chplan.Project)
	if !ok {
		t.Fatalf("expected the phi-fold *chplan.Project, got %T", plan)
	}
	if _, guarded := proj.Input.(*chplan.Aggregate); guarded {
		t.Fatal("duplicate-labelset guard fired on a single-name selector — nothing can collide there")
	}
	if _, ok := proj.Input.(*chplan.RangeWindow); !ok {
		t.Fatalf("expected the phi fold directly over the window, got %T", proj.Input)
	}
}
