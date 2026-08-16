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
	union, ok := plan.(*chplan.UnionAll)
	if !ok {
		t.Fatalf("temporality-bearing native rate = %T, want a two-arm UnionAll", plan)
	}
	if len(union.Inputs) != 2 {
		t.Fatalf("temporality-bearing native rate has %d arms, want 2", len(union.Inputs))
	}

	nativeArm, ok := union.Inputs[0].(*chplan.Project)
	if !ok {
		t.Fatalf("cumulative union arm = %T, want *chplan.Project", union.Inputs[0])
	}
	native, ok := nativeArm.Input.(*chplan.RangeWindowGridNative)
	if !ok {
		t.Fatalf("cumulative arm input = %T, want *chplan.RangeWindowGridNative", nativeArm.Input)
	}
	fanoutArm, ok := union.Inputs[1].(*chplan.Project)
	if !ok {
		t.Fatalf("delta union arm = %T, want *chplan.Project", union.Inputs[1])
	}
	fanout, ok := fanoutArm.Input.(*chplan.RangeWindow)
	if !ok {
		t.Fatalf("delta arm input = %T, want *chplan.RangeWindow", fanoutArm.Input)
	}
	if fanout.TemporalityColumn != schema.DefaultOTelMetrics().AggregationTemporalityColumn {
		t.Errorf("fan-out TemporalityColumn = %q, want schema temporality column", fanout.TemporalityColumn)
	}
	assertTemporalityFilter(t, native.Input, chplan.OpNe)
	assertTemporalityFilter(t, fanout.Input, chplan.OpEq)
}

func TestFanoutRateLowererSplitsTemporality(t *testing.T) {
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
	union, ok := plan.(*chplan.UnionAll)
	if !ok {
		t.Fatalf("temporality-bearing fan-out rate = %T, want a two-arm UnionAll", plan)
	}
	if len(union.Inputs) != 2 {
		t.Fatalf("temporality-bearing fan-out rate has %d arms, want 2", len(union.Inputs))
	}
	cumulativeArm := union.Inputs[0].(*chplan.Project)
	deltaArm := union.Inputs[1].(*chplan.Project)
	cumulative := cumulativeArm.Input.(*chplan.RangeWindow)
	delta := deltaArm.Input.(*chplan.RangeWindow)
	if cumulative.TemporalityColumn != "" {
		t.Errorf("cumulative TemporalityColumn = %q, want empty cheap-path marker", cumulative.TemporalityColumn)
	}
	if delta.TemporalityColumn != schema.DefaultOTelMetrics().AggregationTemporalityColumn {
		t.Errorf("delta TemporalityColumn = %q, want schema temporality column", delta.TemporalityColumn)
	}
	assertTemporalityFilter(t, cumulative.Input, chplan.OpNe)
	assertTemporalityFilter(t, delta.Input, chplan.OpEq)
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
	union, ok := plan.(*chplan.UnionAll)
	if !ok {
		t.Fatalf("offset temporality split = %T, want a two-arm UnionAll", plan)
	}
	if len(union.Inputs) != 2 {
		t.Fatalf("offset temporality split has %d arms, want 2", len(union.Inputs))
	}
	nativeArm := union.Inputs[0].(*chplan.Project)
	fanoutArm := union.Inputs[1].(*chplan.Project)
	native := nativeArm.Input.(*chplan.RangeWindowGridNative)
	fanout := fanoutArm.Input.(*chplan.RangeWindow)

	if native.Offset != queryOffset || fanout.Offset != queryOffset {
		t.Errorf("arm offsets = native %s, fan-out %s, want %s", native.Offset, fanout.Offset, queryOffset)
	}
	if !native.Start.Equal(fanout.Start) || !native.End.Equal(fanout.End) || native.Step != fanout.Step {
		t.Errorf("arm grids diverge: native [%v,%v]/%s, fan-out [%v,%v]/%s",
			native.Start, native.End, native.Step, fanout.Start, fanout.End, fanout.Step)
	}
	for _, arm := range []*chplan.Project{nativeArm, fanoutArm} {
		if !arm.Projections[2].Expr.Equal(&chplan.ColumnRef{Name: chplan.RangeWindowAnchorColumn}) ||
			arm.Projections[2].Alias != chplan.RangeWindowAnchorColumn ||
			arm.Projections[3].Alias != schema.DefaultOTelMetrics().TimestampColumn {
			t.Errorf("arm projection does not preserve the anchor and output timestamp contract: %#v", arm.Projections)
		}
	}
	// UNSPECIFIED is deliberately admitted by the native arm: only a positive
	// DELTA reading takes the fan-out branch.
	if schema.AggregationTemporalityUnspecified == schema.AggregationTemporalityDelta {
		t.Fatal("UNSPECIFIED must remain distinct from DELTA for the native predicate")
	}
	assertTemporalityFilter(t, native.Input, chplan.OpNe)
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
	if predicate.Op != want {
		t.Errorf("filter op = %q, want %q", predicate.Op, want)
	}
	if !predicate.Left.Equal(&chplan.ColumnRef{Name: schema.DefaultOTelMetrics().AggregationTemporalityColumn}) ||
		!predicate.Right.Equal(&chplan.LitInt{V: schema.AggregationTemporalityDelta}) {
		t.Errorf("filter predicate is not the aggregation-temporality DELTA partition")
	}
}
