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

// TestNativeRateLowererSplitsTemporalityInstant pins cerberus issue #2843's
// fix: an INSTANT (Step == 0) rate() over a temporality-bearing counter now
// splits into complementary CUMULATIVE-native / DELTA-fan-out arms — mirrors
// TestNativeRateLowererSplitsTemporality (the MATRIX case) modulo two
// deliberate differences: the native arm's own type
// (*chplan.RangeWindowGridNativeInstant) and the ABSENCE of a
// derivedRateArm-style wrapping Project. Unlike the matrix union, the two
// instant arms already publish byte-identical (Attributes, Value) columns —
// see NativeRateLowerer.LowerRate's own doc for why wrapping would be both
// unnecessary and actively wrong (a live ClickHouse code 47 for a further
// wrapper like `abs(rate(...))`, since chplan.RowShapeOf's *Project case
// cannot see through to the wrapped column list).
func TestNativeRateLowererSplitsTemporalityInstant(t *testing.T) {
	t.Parallel()

	plan := lowerNativeTemporalityRateInstant(t, "rate(cerberus_queries_total[5m])")
	union := temporalityRateInstantUnion(t, plan)
	if len(union.Inputs) != 2 {
		t.Fatalf("temporality-bearing native instant rate has %d arms, want 2", len(union.Inputs))
	}

	native, ok := union.Inputs[0].(*chplan.RangeWindowGridNativeInstant)
	if !ok {
		t.Fatalf("cumulative union arm = %T, want *chplan.RangeWindowGridNativeInstant", union.Inputs[0])
	}
	if native.Func != "rate" {
		t.Errorf("cumulative union arm Func = %q, want %q", native.Func, "rate")
	}
	fanout, ok := union.Inputs[1].(*chplan.RangeWindow)
	if !ok {
		t.Fatalf("delta union arm = %T, want *chplan.RangeWindow", union.Inputs[1])
	}
	if fanout.OuterRange > 0 || fanout.Step > 0 {
		t.Errorf("delta union arm is matrix-shaped (OuterRange=%s, Step=%s), want the instant shape", fanout.OuterRange, fanout.Step)
	}
	if fanout.TemporalityColumn != schema.DefaultOTelMetrics().AggregationTemporalityColumn {
		t.Errorf("fan-out TemporalityColumn = %q, want schema temporality column", fanout.TemporalityColumn)
	}
	assertTemporalityFilter(t, native.Input, chplan.OpNe)
	assertTemporalityFilter(t, fanout.Input, chplan.OpEq)

	// Activation pin for chplan.RowShapeOf's *UnionAll case (row_shape.go):
	// the raw union must classify as the reduced shape, matching what
	// nativeTSGridInstantNode's own un-unioned answer already does, rather
	// than silently defaulting to SampleRowShape.
	if got := chplan.RowShapeOf(union); got != chplan.ReducedWindowRowShape {
		t.Errorf("RowShapeOf(instant temporality union) = %s, want %s", got, chplan.ReducedWindowRowShape)
	}
	s := schema.DefaultOTelMetrics()
	if got := chplan.IsDerivedShape(union, chplan.SampleColumns{
		MetricName: s.MetricNameColumn, Attributes: s.AttributesColumn,
		Timestamp: s.TimestampColumn, Value: s.ValueColumn,
	}); !got {
		t.Errorf("IsDerivedShape(instant temporality union) = %v, want true", got)
	}
}

// TestNativeRateLowererInstantOffDeclinesTemporalitySplit pins the scope
// boundary the Instant field controls: WITHOUT Instant set, a
// temporality-bearing instant window must stay on the unchanged fan-out
// exactly like before this issue's fix — the union split for the instant
// shape is reachable ONLY when the boot-wired strategy also opted into
// ts_grid_instant, mirroring how the matrix split is unconditional on
// nothing but ts_grid_range.
func TestNativeRateLowererInstantOffDeclinesTemporalitySplit(t *testing.T) {
	t.Parallel()

	plan := lowerNativeTemporalityRateAt(t, "rate(cerberus_queries_total[5m])")
	if _, ok := plan.(*chplan.RangeWindow); !ok {
		t.Errorf("instant native rate shape (Instant unset) = %T, want unchanged fan-out RangeWindow", plan)
	}
}

// TestNativeRateLowererInstantTemporalitySplitPreservesOffsetGrid mirrors
// TestNativeRateLowererTemporalitySplitPreservesOffsetGrid for the instant
// shape: an `offset` modifier must thread identically onto both split arms.
func TestNativeRateLowererInstantTemporalitySplitPreservesOffsetGrid(t *testing.T) {
	t.Parallel()

	const queryOffset = time.Minute
	plan := lowerNativeTemporalityRateInstant(t, "rate(cerberus_queries_total[5m] offset 1m)")
	union := temporalityRateInstantUnion(t, plan)
	if len(union.Inputs) != 2 {
		t.Fatalf("offset instant temporality split has %d arms, want 2", len(union.Inputs))
	}
	native := union.Inputs[0].(*chplan.RangeWindowGridNativeInstant)
	fanout := union.Inputs[1].(*chplan.RangeWindow)

	if native.Offset != queryOffset || fanout.Offset != queryOffset {
		t.Errorf("arm offsets = native %s, fan-out %s, want %s", native.Offset, fanout.Offset, queryOffset)
	}
	if !native.Anchor.Equal(fanout.End) {
		t.Errorf("arm anchors diverge: native %v, fan-out end %v", native.Anchor, fanout.End)
	}
	// UNSPECIFIED is deliberately admitted by the native arm: only a positive
	// DELTA reading takes the fan-out branch.
	if schema.AggregationTemporalityUnspecified == schema.AggregationTemporalityDelta {
		t.Fatal("UNSPECIFIED must remain distinct from DELTA for the native predicate")
	}
	assertTemporalityFilter(t, native.Input, chplan.OpNe)
}

func temporalityRateInstantUnion(t *testing.T, plan chplan.Node) *chplan.UnionAll {
	t.Helper()
	union, ok := plan.(*chplan.UnionAll)
	if !ok {
		t.Fatalf("temporality-bearing instant rate = %T, want a raw two-arm UnionAll (no wrapping Project)", plan)
	}
	return union
}

// lowerNativeTemporalityRateInstant lowers query as a genuinely PINNED
// instant evaluation (step == 0 forces rangeGridShapeFor's gridSingleAnchor
// branch regardless of start/end; a non-zero end additionally back-fills
// the window anchor via anchorFromSelector, mirroring an /api/v1/query
// request at time=T rather than a context-free Lower() with no eval
// instant at all — the latter leaves rw.End zero, which
// nativeTSGridInstantNode declines unconditionally).
func lowerNativeTemporalityRateInstant(t *testing.T, query string) chplan.Node {
	t.Helper()
	expr, err := parser.NewParser(parser.Options{}).ParseExpr(query)
	if err != nil {
		t.Fatalf("parse %q: %v", query, err)
	}
	instant := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)
	plan, err := promql.LowerAtRangeOpts(context.Background(), expr, schema.DefaultOTelMetrics(), instant, instant,
		time.Duration(0), promql.LowerOpts{Lowerers: promql.RangeLowerers{Rate: promql.NativeRateLowerer{
			Fallback: promql.FanoutRateLowerer{},
			Instant:  true,
		}}})
	if err != nil {
		t.Fatalf("lower %q: %v", query, err)
	}
	return plan
}
