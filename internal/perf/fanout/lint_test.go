package fanout_test

import (
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/perf/fanout"
)

// rawScan is a bare Filter-over-Scan — an UN-collapsed relation (the raw
// row count flows out unchanged).
func rawScan() chplan.Node {
	return &chplan.Filter{
		Input:     &chplan.Scan{Table: "otel_metrics_gauge"},
		Predicate: &chplan.LitBool{V: true},
	}
}

// collapsed wraps an input in a no-GROUP-BY Aggregate (exactly one row).
func collapsed(in chplan.Node) chplan.Node {
	return &chplan.Aggregate{
		Input:    in,
		AggFuncs: []chplan.AggFunc{{Name: "count", Alias: "n"}},
	}
}

func stepGrid() chplan.Node {
	return &chplan.StepGrid{
		Start: time.Unix(0, 0).UTC(),
		End:   time.Unix(300, 0).UTC(),
		Step:  30 * time.Second,
	}
}

func hasRule(vs []fanout.Violation, r fanout.Rule) bool {
	for _, v := range vs {
		if v.Rule == r {
			return true
		}
	}
	return false
}

// TestRule1_UnboundedCrossJoin_Trips proves the pre-#804 step-grid blowup
// shape (CrossJoin of an N-anchor grid against a RAW scan) is flagged.
func TestRule1_UnboundedCrossJoin_Trips(t *testing.T) {
	t.Parallel()
	plan := &chplan.CrossJoin{Left: stepGrid(), Right: rawScan()}
	vs := fanout.Lint(plan, "")
	if !hasRule(vs, fanout.RuleUnboundedCrossJoin) {
		t.Fatalf("expected unbounded-cross-join violation, got %v", vs)
	}
}

// TestRule1_BroadcastCrossJoin_OK proves the #804 broadcast shape
// (CrossJoin of a grid against an already-collapsed Aggregate side) is
// NOT flagged — neither is the absent(...) no-group-by count broadcast.
func TestRule1_BroadcastCrossJoin_OK(t *testing.T) {
	t.Parallel()
	// Right side collapsed by an Aggregate, wrapped in the reshape
	// Project the lowering emits.
	right := &chplan.Project{
		Input: &chplan.Aggregate{
			Input:   rawScan(),
			GroupBy: []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
			AggFuncs: []chplan.AggFunc{{
				Name:  "argMax",
				Args:  []chplan.Expr{&chplan.ColumnRef{Name: "Value"}, &chplan.ColumnRef{Name: "TimeUnix"}},
				Alias: "lwr_value",
			}},
		},
		Projections: []chplan.Projection{{Expr: &chplan.ColumnRef{Name: "lwr_value"}, Alias: "lwr_value"}},
	}
	plan := &chplan.CrossJoin{Left: stepGrid(), Right: right}
	if vs := fanout.Lint(plan, ""); hasRule(vs, fanout.RuleUnboundedCrossJoin) {
		t.Fatalf("broadcast CROSS JOIN must NOT be flagged, got %v", vs)
	}
}

// TestRule2_FanoutFeedingJoin_Trips proves a raw StepGrid feeding a JOIN
// side without an intervening collapse is flagged.
func TestRule2_FanoutFeedingJoin_Trips(t *testing.T) {
	t.Parallel()
	plan := &chplan.VectorJoin{
		Left:  stepGrid(), // raw N-anchor grid feeds the join un-collapsed
		Right: collapsed(rawScan()),
	}
	vs := fanout.Lint(plan, "")
	if !hasRule(vs, fanout.RuleFanoutFeedingJoin) {
		t.Fatalf("expected fanout-feeding-join violation, got %v", vs)
	}
}

// TestRule2_CollapsedBeforeJoin_OK proves a fan-out collapsed by an
// Aggregate before the JOIN is NOT flagged.
func TestRule2_CollapsedBeforeJoin_OK(t *testing.T) {
	t.Parallel()
	plan := &chplan.VectorJoin{
		Left:  collapsed(stepGrid()),
		Right: collapsed(rawScan()),
	}
	if vs := fanout.Lint(plan, ""); hasRule(vs, fanout.RuleFanoutFeedingJoin) {
		t.Fatalf("collapsed-before-join must NOT be flagged, got %v", vs)
	}
}

// TestRule3_UncappedRecursion_Trips proves a WITH RECURSIVE without a
// `_depth < N` literal cap is flagged.
func TestRule3_UncappedRecursion_Trips(t *testing.T) {
	t.Parallel()
	sql := `WITH RECURSIVE c AS (SELECT TraceId, SpanId, 0 AS _depth FROM t ` +
		`UNION ALL SELECT t.TraceId, t.SpanId, c._depth + 1 FROM t JOIN c ON t.ParentSpanId = c.SpanId) ` +
		`SELECT * FROM c`
	vs := fanout.Lint(nil, sql)
	if !hasRule(vs, fanout.RuleUncappedRecursion) {
		t.Fatalf("expected uncapped-recursion violation, got %v", vs)
	}
}

// TestRule3_CappedRecursion_OK proves a WITH RECURSIVE WITH a `_depth < N`
// cap is NOT flagged, including a multi-CTE statement.
func TestRule3_CappedRecursion_OK(t *testing.T) {
	t.Parallel()
	sql := `WITH RECURSIVE c1 AS (... WHERE c._depth < 128 ...), ` +
		`c2 AS (... WHERE c._depth < 128 ...) SELECT * FROM c2`
	// Two recursive markers would need two caps; emulate that.
	two := strings.Replace(sql, "WITH RECURSIVE c1", "WITH RECURSIVE c1 WITH RECURSIVE", 1)
	if vs := fanout.Lint(nil, two); hasRule(vs, fanout.RuleUncappedRecursion) {
		t.Fatalf("capped recursion must NOT be flagged, got %v", vs)
	}
}

// TestRule3_NonLiteralCap_Trips proves a `_depth < <column>` (non-integer)
// bound does NOT count as a cap.
func TestRule3_NonLiteralCap_Trips(t *testing.T) {
	t.Parallel()
	sql := `WITH RECURSIVE c AS (... WHERE c._depth < maxDepthCol ...) SELECT * FROM c`
	vs := fanout.Lint(nil, sql)
	if !hasRule(vs, fanout.RuleUncappedRecursion) {
		t.Fatalf("non-literal depth bound must be flagged, got %v", vs)
	}
}

// TestRule4_CorrelatedSubquery_Trips proves a ScalarSubquery over a
// non-collapsed plan is flagged.
func TestRule4_CorrelatedSubquery_Trips(t *testing.T) {
	t.Parallel()
	plan := &chplan.Filter{
		Input: &chplan.Scan{Table: "otel_metrics_gauge"},
		Predicate: &chplan.Binary{
			Op:    chplan.OpGt,
			Left:  &chplan.ColumnRef{Name: "Value"},
			Right: &chplan.ScalarSubquery{Input: rawScan()}, // un-collapsed
		},
	}
	vs := fanout.Lint(plan, "")
	if !hasRule(vs, fanout.RuleCorrelatedSubquery) {
		t.Fatalf("expected correlated-subquery violation, got %v", vs)
	}
}

// TestRule4_PinnedScalar_OK proves a ScalarSubquery over a one-row
// Aggregate (the legitimate scalar() shape) is NOT flagged.
func TestRule4_PinnedScalar_OK(t *testing.T) {
	t.Parallel()
	plan := &chplan.Filter{
		Input: &chplan.Scan{Table: "otel_metrics_gauge"},
		Predicate: &chplan.Binary{
			Op:    chplan.OpGt,
			Left:  &chplan.ColumnRef{Name: "Value"},
			Right: &chplan.ScalarSubquery{Input: collapsed(rawScan())},
		},
	}
	if vs := fanout.Lint(plan, ""); hasRule(vs, fanout.RuleCorrelatedSubquery) {
		t.Fatalf("pinned scalar subquery must NOT be flagged, got %v", vs)
	}
}

// TestEmptyInputs proves Lint is a no-op on nil plan + empty SQL.
func TestEmptyInputs(t *testing.T) {
	t.Parallel()
	if vs := fanout.Lint(nil, ""); len(vs) != 0 {
		t.Fatalf("empty inputs must yield no violations, got %v", vs)
	}
}
