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

// TestNativeIrateLowererSplitsTemporality pins cerberus issue #2803's fix:
// irate() over a `_total`-suffixed counter (routes to exactly one Sum table,
// so rangeVectorCounterTemporalityColumn sets TemporalityColumn) now splits
// into complementary CUMULATIVE-native / DELTA-fan-out arms instead of
// falling back to the fan-out unconditionally — mirrors
// TestNativeRateLowererSplitsTemporality exactly, modulo the derivedIrateArm
// wrapper and the native arm's own Func="irate".
func TestNativeIrateLowererSplitsTemporality(t *testing.T) {
	t.Parallel()

	plan := lowerNativeTemporalityIrate(t, "irate(cerberus_queries_total[5m])")
	derived, union := temporalityIrateUnion(t, plan)
	if len(union.Inputs) != 2 {
		t.Fatalf("temporality-bearing native irate has %d arms, want 2", len(union.Inputs))
	}

	native, ok := union.Inputs[0].(*chplan.RangeWindowGridNative)
	if !ok {
		t.Fatalf("cumulative union arm = %T, want *chplan.RangeWindowGridNative", union.Inputs[0])
	}
	if native.Func != "irate" {
		t.Errorf("cumulative union arm Func = %q, want %q", native.Func, "irate")
	}
	fanout, ok := union.Inputs[1].(*chplan.RangeWindow)
	if !ok {
		t.Fatalf("delta union arm = %T, want *chplan.RangeWindow", union.Inputs[1])
	}
	if fanout.TemporalityColumn != schema.DefaultOTelMetrics().AggregationTemporalityColumn {
		t.Errorf("fan-out TemporalityColumn = %q, want schema temporality column", fanout.TemporalityColumn)
	}
	assertIrateTemporalityFilter(t, native.Input, chplan.OpNe)
	assertIrateTemporalityFilter(t, fanout.Input, chplan.OpEq)
	assertDerivedIrateProjection(t, derived)
}

// TestNativeIrateLowererTemporalitySplitPreservesOffsetGrid mirrors
// TestNativeRateLowererTemporalitySplitPreservesOffsetGrid for irate: an
// `offset` modifier must thread identically onto both split arms.
func TestNativeIrateLowererTemporalitySplitPreservesOffsetGrid(t *testing.T) {
	t.Parallel()

	const queryOffset = time.Minute
	plan := lowerNativeTemporalityIrate(t, "irate(cerberus_queries_total[5m] offset 1m)")
	derived, union := temporalityIrateUnion(t, plan)
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
	assertDerivedIrateProjection(t, derived)
	// UNSPECIFIED is deliberately admitted by the native arm: only a positive
	// DELTA reading takes the fan-out branch.
	if schema.AggregationTemporalityUnspecified == schema.AggregationTemporalityDelta {
		t.Fatal("UNSPECIFIED must remain distinct from DELTA for the native predicate")
	}
	assertIrateTemporalityFilter(t, native.Input, chplan.OpNe)
}

// TestNativeIrateLowererUnsupportedShapeFallsBack mirrors
// TestNativeRateLowererUnsupportedShapeFallsBack: an INSTANT (Step == 0)
// irate() shape is outside nativeTSGridMatrixNode's eligibility and must
// still fall back to the plain fan-out RangeWindow, temporality split or
// not — irate's instant arm carries no union split (unlike its matrix arm),
// matching NativeRateLowerer's own Instant-arm scope note.
func TestNativeIrateLowererUnsupportedShapeFallsBack(t *testing.T) {
	t.Parallel()

	plan := lowerNativeTemporalityIrateAt(t, "irate(cerberus_queries_total[5m])")
	if _, ok := plan.(*chplan.RangeWindow); !ok {
		t.Errorf("instant native irate shape = %T, want unchanged fan-out RangeWindow", plan)
	}
}

// TestNativeIrateTemporalityUnionDeclinesVectorAggFold is the CRITICAL SCOPE
// regression guard cerberus issue #2803 exists to pin: irate's own
// temporality union (derivedIrateArm) is POSITIONALLY IDENTICAL to rate/
// increase's (derivedRateArm) — Project{UnionAll{RangeWindowGridNative,
// RangeWindow}} — so `sum by(...) (irate(...))` over that union must NOT
// silently fold into ts_grid_vector_agg's RangeWindowGridNativeVectorAgg the
// way the rate/increase union does (rateIncreaseTemporalityUnionArms,
// internal/promql/lower.go, guards on the native arm's own Func). That fold
// was never validated for irate — this test fails loudly if a future change
// widens the Func guard (or otherwise re-couples the two shapes) without
// updating this pin.
func TestNativeIrateTemporalityUnionDeclinesVectorAggFold(t *testing.T) {
	t.Parallel()

	expr, err := parser.NewParser(parser.Options{}).ParseExpr("sum by(host) (irate(cerberus_queries_total[5m]))")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	plan, err := promql.LowerAtRangeOpts(context.Background(), expr, schema.DefaultOTelMetrics(), start, start.Add(5*time.Minute),
		time.Minute, promql.LowerOpts{Lowerers: promql.RangeLowerers{
			Irate:     promql.NativeIrateLowerer{Fallback: promql.FanoutIrateLowerer{}},
			VectorAgg: true,
		}})
	if err != nil {
		t.Fatalf("lower sum(irate(...)): %v", err)
	}

	outer, ok := plan.(*chplan.Project)
	if !ok {
		t.Fatalf("sum(irate(...)) plan = %T, want the wrapping Project [wrapAggregateForSample] builds", plan)
	}
	agg, ok := outer.Input.(*chplan.Aggregate)
	if !ok {
		t.Fatalf("sum(irate(...)) aggregate input = %T, want the ORDINARY *chplan.Aggregate — "+
			"a RangeWindowGridNativeVectorAgg or a combining Aggregate here would mean irate's "+
			"temporality union started matching rateIncreaseTemporalityUnionArms", outer.Input)
	}
	derived, ok := agg.Input.(*chplan.Project)
	if !ok {
		t.Fatalf("aggregate input = %T, want irate's own derivedIrateArm Project unchanged", agg.Input)
	}
	union, ok := derived.Input.(*chplan.UnionAll)
	if !ok {
		t.Fatalf("derivedIrateArm input = %T, want the two-arm UnionAll", derived.Input)
	}
	native, ok := union.Inputs[0].(*chplan.RangeWindowGridNative)
	if !ok || native.Func != "irate" {
		t.Fatalf("union native arm = %#v, want a RangeWindowGridNative with Func=irate", union.Inputs[0])
	}
}

func temporalityIrateUnion(t *testing.T, plan chplan.Node) (*chplan.Project, *chplan.UnionAll) {
	t.Helper()
	derived, ok := plan.(*chplan.Project)
	if !ok {
		t.Fatalf("temporality-bearing irate = %T, want derived Project", plan)
	}
	union, ok := derived.Input.(*chplan.UnionAll)
	if !ok {
		t.Fatalf("derived irate input = %T, want two-arm UnionAll", derived.Input)
	}
	return derived, union
}

func assertDerivedIrateProjection(t *testing.T, project *chplan.Project) {
	t.Helper()
	if !project.Projections[2].Expr.Equal(&chplan.ColumnRef{Name: chplan.RangeWindowAnchorColumn}) ||
		project.Projections[2].Alias != chplan.RangeWindowAnchorColumn ||
		project.Projections[3].Alias != schema.DefaultOTelMetrics().TimestampColumn {
		t.Errorf("derived irate projection does not preserve the anchor and output timestamp contract: %#v", project.Projections)
	}
}

func lowerNativeTemporalityIrate(t *testing.T, query string) chplan.Node {
	t.Helper()
	expr, err := parser.NewParser(parser.Options{}).ParseExpr(query)
	if err != nil {
		t.Fatalf("parse %q: %v", query, err)
	}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	plan, err := promql.LowerAtRangeOpts(context.Background(), expr, schema.DefaultOTelMetrics(), start, start.Add(5*time.Minute),
		time.Minute, promql.LowerOpts{Lowerers: promql.RangeLowerers{Irate: promql.NativeIrateLowerer{
			Fallback: promql.FanoutIrateLowerer{},
		}}})
	if err != nil {
		t.Fatalf("lower %q: %v", query, err)
	}
	return plan
}

func lowerNativeTemporalityIrateAt(t *testing.T, query string) chplan.Node {
	t.Helper()
	expr, err := parser.NewParser(parser.Options{}).ParseExpr(query)
	if err != nil {
		t.Fatalf("parse %q: %v", query, err)
	}
	plan, err := promql.LowerAtRangeOpts(context.Background(), expr, schema.DefaultOTelMetrics(), time.Time{}, time.Time{},
		time.Duration(0), promql.LowerOpts{Lowerers: promql.RangeLowerers{Irate: promql.NativeIrateLowerer{
			Fallback: promql.FanoutIrateLowerer{},
		}}})
	if err != nil {
		t.Fatalf("lower %q: %v", query, err)
	}
	return plan
}

func assertIrateTemporalityFilter(t *testing.T, input chplan.Node, want chplan.BinaryOp) {
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
