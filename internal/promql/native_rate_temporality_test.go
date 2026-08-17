package promql_test

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

func TestNativeRateLowererSplitsTemporality(t *testing.T) {
	t.Parallel()

	plan := lowerNativeTemporalityRate(t, "rate(cerberus_queries_total[5m])")
	derived, union := temporalityRateUnion(t, plan)
	if len(union.Inputs) != 2 {
		t.Fatalf("temporality-bearing native rate has %d arms, want 2", len(union.Inputs))
	}

	native, ok := union.Inputs[0].(*chplan.RangeWindowGridNative)
	if !ok {
		t.Fatalf("cumulative union arm = %T, want *chplan.RangeWindowGridNative", union.Inputs[0])
	}
	fanout, ok := union.Inputs[1].(*chplan.RangeWindow)
	if !ok {
		t.Fatalf("delta union arm = %T, want *chplan.RangeWindow", union.Inputs[1])
	}
	if fanout.TemporalityColumn != schema.DefaultOTelMetrics().AggregationTemporalityColumn {
		t.Errorf("fan-out TemporalityColumn = %q, want schema temporality column", fanout.TemporalityColumn)
	}
	assertTemporalityFilter(t, native.Input, chplan.OpNe)
	assertTemporalityFilter(t, fanout.Input, chplan.OpEq)
	assertDerivedRateProjection(t, derived)
}

// TestFanoutRateLowererPreservesTemporalityColumn pins the DEFAULT
// RateLowerer's fused shape: a temporality-bearing counter window is no
// longer split into complementary CUMULATIVE / DELTA arms at the plan level.
// chsql's single-scan matrix emitter (emitWindowedArrayExtrapolatedMatrix)
// now branches on the temporality column per row inside one query, so the
// plan-level split — and the per-arm selector-projection shaping it required
// — is unnecessary cost the lowerer no longer pays. See
// FanoutRateLowerer.LowerRate.
func TestFanoutRateLowererPreservesTemporalityColumn(t *testing.T) {
	t.Parallel()

	expr, err := parser.NewParser(parser.Options{}).ParseExpr("rate(cerberus_queries_total[5m])")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	plan, err := promql.LowerAtRange(context.Background(), expr, schema.DefaultOTelMetrics(),
		start, start.Add(5*time.Minute), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	rw, ok := plan.(*chplan.RangeWindow)
	if !ok {
		t.Fatalf("temporality-bearing fan-out rate = %T, want unchanged fan-out RangeWindow", plan)
	}
	if rw.TemporalityColumn != schema.DefaultOTelMetrics().AggregationTemporalityColumn {
		t.Errorf("TemporalityColumn = %q, want schema temporality column", rw.TemporalityColumn)
	}
	assertFilterOmitsTemporalityFusion(t, rw.Input)
}

// assertFilterOmitsTemporalityFusion locates the selector's innermost Filter
// — the same Filter-or-Project-then-Filter shape assertTemporalityFilter
// inspects for the split arms — and checks its predicate was never AND-fused
// with an aggregation-temporality condition. A bare `rw.Input.(*chplan.Filter)`
// type check is not enough here: the metric-name selector for
// "cerberus_queries_total" already lowers to a *chplan.Project wrapping a
// *chplan.Filter, so that shallow assertion is true (Input is a Project, not
// a Filter) whether or not FanoutRateLowerer still fuses a temporality
// predicate into it underneath.
func assertFilterOmitsTemporalityFusion(t *testing.T, input chplan.Node) {
	t.Helper()
	filter, ok := input.(*chplan.Filter)
	if !ok {
		project, projectOK := input.(*chplan.Project)
		if !projectOK {
			return
		}
		filter, ok = project.Input.(*chplan.Filter)
		if !ok {
			return
		}
	}
	if _, nested := filter.Input.(*chplan.Filter); nested {
		t.Error("selector Filter is nested rather than fused; want a single Filter to inspect for temporality fusion")
	}
	assertPredicateOmitsTemporalityColumn(t, filter.Predicate)
}

// assertPredicateOmitsTemporalityColumn recurses through AND-conjuncts —
// fuseTemporalityFilter's shape for combining an existing selector predicate
// with a temporality partition — and fails if any conjunct references the
// aggregation-temporality column.
func assertPredicateOmitsTemporalityColumn(t *testing.T, predicate chplan.Expr) {
	t.Helper()
	temporalityColumn := schema.DefaultOTelMetrics().AggregationTemporalityColumn
	binary, ok := predicate.(*chplan.Binary)
	if !ok {
		return
	}
	if binary.Op == chplan.OpAnd {
		assertPredicateOmitsTemporalityColumn(t, binary.Left)
		assertPredicateOmitsTemporalityColumn(t, binary.Right)
		return
	}
	if binary.Left.Equal(&chplan.ColumnRef{Name: temporalityColumn}) {
		t.Errorf("selector filter predicate is fused with the aggregation-temporality column: %#v", predicate)
	}
}

func TestNativeRateLowererUnsupportedShapeFallsBack(t *testing.T) {
	t.Parallel()

	plan := lowerNativeTemporalityRateAt(t, "rate(cerberus_queries_total[5m])")
	if _, ok := plan.(*chplan.RangeWindow); !ok {
		t.Errorf("instant native rate shape = %T, want unchanged fan-out RangeWindow", plan)
	}
}

func TestNativeRateLowererTemporalitySplitPreservesOffsetGrid(t *testing.T) {
	t.Parallel()

	const queryOffset = time.Minute
	plan := lowerNativeTemporalityRate(t, "rate(cerberus_queries_total[5m] offset 1m)")
	derived, union := temporalityRateUnion(t, plan)
	if len(union.Inputs) != 2 {
		t.Fatalf("offset temporality split has %d arms, want 2", len(union.Inputs))
	}
	native := union.Inputs[0].(*chplan.RangeWindowGridNative)
	fanout := union.Inputs[1].(*chplan.RangeWindow)

	if native.Offset != queryOffset || fanout.Offset != queryOffset {
		t.Errorf("arm offsets = native %s, fan-out %s, want %s", native.Offset, fanout.Offset, queryOffset)
	}
	if !native.Start.Equal(fanout.Start) || !native.End.Equal(fanout.End) || native.Step != fanout.Step {
		t.Errorf("arm grids diverge: native [%v,%v]/%s, fan-out [%v,%v]/%s",
			native.Start, native.End, native.Step, fanout.Start, fanout.End, fanout.Step)
	}
	assertDerivedRateProjection(t, derived)
	// UNSPECIFIED is deliberately admitted by the native arm: only a positive
	// DELTA reading takes the fan-out branch.
	if schema.AggregationTemporalityUnspecified == schema.AggregationTemporalityDelta {
		t.Fatal("UNSPECIFIED must remain distinct from DELTA for the native predicate")
	}
	assertTemporalityFilter(t, native.Input, chplan.OpNe)
}

func temporalityRateUnion(t *testing.T, plan chplan.Node) (*chplan.Project, *chplan.UnionAll) {
	t.Helper()
	derived, ok := plan.(*chplan.Project)
	if !ok {
		t.Fatalf("temporality-bearing rate = %T, want derived Project", plan)
	}
	union, ok := derived.Input.(*chplan.UnionAll)
	if !ok {
		t.Fatalf("derived rate input = %T, want two-arm UnionAll", derived.Input)
	}
	return derived, union
}

func assertDerivedRateProjection(t *testing.T, project *chplan.Project) {
	t.Helper()
	if !project.Projections[2].Expr.Equal(&chplan.ColumnRef{Name: chplan.RangeWindowAnchorColumn}) ||
		project.Projections[2].Alias != chplan.RangeWindowAnchorColumn ||
		project.Projections[3].Alias != schema.DefaultOTelMetrics().TimestampColumn {
		t.Errorf("derived rate projection does not preserve the anchor and output timestamp contract: %#v", project.Projections)
	}
}

func lowerNativeTemporalityRate(t *testing.T, query string) chplan.Node {
	t.Helper()
	expr, err := parser.NewParser(parser.Options{}).ParseExpr(query)
	if err != nil {
		t.Fatalf("parse %q: %v", query, err)
	}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	plan, err := promql.LowerAtRangeOpts(context.Background(), expr, schema.DefaultOTelMetrics(), start, start.Add(5*time.Minute),
		time.Minute, promql.LowerOpts{Lowerers: promql.RangeLowerers{Rate: promql.NativeRateLowerer{
			Fallback: promql.FanoutRateLowerer{},
		}}})
	if err != nil {
		t.Fatalf("lower %q: %v", query, err)
	}
	return plan
}

func lowerNativeTemporalityRateAt(t *testing.T, query string) chplan.Node {
	t.Helper()
	expr, err := parser.NewParser(parser.Options{}).ParseExpr(query)
	if err != nil {
		t.Fatalf("parse %q: %v", query, err)
	}
	plan, err := promql.LowerAtRangeOpts(context.Background(), expr, schema.DefaultOTelMetrics(), time.Time{}, time.Time{},
		time.Duration(0), promql.LowerOpts{Lowerers: promql.RangeLowerers{Rate: promql.NativeRateLowerer{
			Fallback: promql.FanoutRateLowerer{},
		}}})
	if err != nil {
		t.Fatalf("lower %q: %v", query, err)
	}
	return plan
}

func assertTemporalityFilter(t *testing.T, input chplan.Node, want chplan.BinaryOp) {
	t.Helper()
	filter, ok := input.(*chplan.Filter)
	if !ok {
		project, projectOK := input.(*chplan.Project)
		if !projectOK {
			t.Fatalf("arm input = %T, want a Filter or selector Project", input)
		}
		filter, ok = project.Input.(*chplan.Filter)
		if !ok {
			t.Fatalf("selector Project input = %T, want *chplan.Filter", project.Input)
		}
	}
	predicate, ok := filter.Predicate.(*chplan.Binary)
	if !ok {
		t.Fatalf("filter predicate = %T, want *chplan.Binary", filter.Predicate)
	}
	if predicate.Op == chplan.OpAnd {
		combined := predicate
		predicate, ok = combined.Right.(*chplan.Binary)
		if !ok {
			t.Fatalf("combined filter right predicate = %T, want *chplan.Binary", combined.Right)
		}
	}
	if predicate.Op != want ||
		!predicate.Left.Equal(&chplan.ColumnRef{Name: schema.DefaultOTelMetrics().AggregationTemporalityColumn}) ||
		!predicate.Right.Equal(&chplan.LitInt{V: schema.AggregationTemporalityDelta}) {
		t.Errorf("filter predicate is not the aggregation-temporality DELTA partition")
	}
	if _, nested := filter.Input.(*chplan.Filter); nested {
		t.Error("temporality partition remained a nested Filter instead of fusing with the selector predicate")
	}
}
