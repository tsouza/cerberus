package chsql

import (
	"context"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// rangeLWRFusionTestLWR builds the minimal well-formed RangeLWR
// matchRangeLWRFusion's tests match against.
func rangeLWRFusionTestLWR() *chplan.RangeLWR {
	return &chplan.RangeLWR{
		Input:         &chplan.Scan{Table: "otel_metrics_gauge"},
		Step:          1, // Duration in ns; only Step > 0 matters for these tests
		MetricNameCol: "MetricName",
		AttributesCol: "Attributes",
		TimestampCol:  "TimeUnix",
		ValueCol:      "Value",
	}
}

// rangeLWRFusionTestAggregate builds `sum(Value)`/`count(Value)` grouped by
// (label, bucket) over lwr — the shape lowerAggregate emits for a PromQL
// `<fn> by (label) (<bare selector>)` range query.
func rangeLWRFusionTestAggregate(fn chplan.Fn, lwr *chplan.RangeLWR) *chplan.Aggregate {
	return &chplan.Aggregate{
		Input: lwr,
		GroupBy: []chplan.Expr{
			&chplan.MapAccess{Map: &chplan.ColumnRef{Name: lwr.AttributesCol}, Key: &chplan.LitString{V: "reason"}},
			&chplan.ColumnRef{Name: lwr.TimestampCol},
		},
		GroupByAliases: []string{"gkey_0", "bucket_ts"},
		AggFuncs: []chplan.AggFunc{{
			Fn:    fn,
			Args:  []chplan.Expr{&chplan.ColumnRef{Name: lwr.ValueCol}},
			Alias: "Value",
		}},
		DropEmptyOnNoGroup: true,
	}
}

func TestMatchRangeLWRFusion_FiresForSum(t *testing.T) {
	lwr := rangeLWRFusionTestLWR()
	a := rangeLWRFusionTestAggregate(chplan.FnSum, lwr)
	gotLWR, gotKind := matchRangeLWRFusion(a)
	if gotKind != rangeLWRFusionSum || gotLWR != lwr {
		t.Fatalf("matchRangeLWRFusion(sum) = (%v, %v), want (lwr, rangeLWRFusionSum)", gotLWR, gotKind)
	}
}

func TestMatchRangeLWRFusion_FiresForCount(t *testing.T) {
	lwr := rangeLWRFusionTestLWR()
	a := rangeLWRFusionTestAggregate(chplan.FnCount, lwr)
	gotLWR, gotKind := matchRangeLWRFusion(a)
	if gotKind != rangeLWRFusionCount || gotLWR != lwr {
		t.Fatalf("matchRangeLWRFusion(count) = (%v, %v), want (lwr, rangeLWRFusionCount)", gotLWR, gotKind)
	}
}

// TestMatchRangeLWRFusion_Declines pins every precondition
// matchRangeLWRFusion enforces before it lets a shape ride the fast path —
// each subtest mutates exactly ONE thing away from the eligible baseline and
// asserts the match reverts to rangeLWRFusionNone, so a future change that
// accidentally loosens one of these checks fails here first, before it can
// emit incorrect SQL.
func TestMatchRangeLWRFusion_Declines(t *testing.T) {
	cases := []struct {
		name string
		make func() *chplan.Aggregate
	}{
		{
			name: "input is not RangeLWR",
			make: func() *chplan.Aggregate {
				lwr := rangeLWRFusionTestLWR()
				a := rangeLWRFusionTestAggregate(chplan.FnSum, lwr)
				a.Input = &chplan.Project{Input: lwr}
				return a
			},
		},
		{
			name: "avg is not a fusable aggregate",
			make: func() *chplan.Aggregate {
				return rangeLWRFusionTestAggregate(chplan.FnAvg, rangeLWRFusionTestLWR())
			},
		},
		{
			name: "quantile is not a fusable aggregate",
			make: func() *chplan.Aggregate {
				return rangeLWRFusionTestAggregate(chplan.FnQuantile, rangeLWRFusionTestLWR())
			},
		},
		{
			name: "Having is set",
			make: func() *chplan.Aggregate {
				a := rangeLWRFusionTestAggregate(chplan.FnSum, rangeLWRFusionTestLWR())
				a.Having = &chplan.Binary{Op: chplan.OpGt, Left: &chplan.ColumnRef{Name: "Value"}, Right: &chplan.LitInt{V: 0}}
				return a
			},
		},
		{
			name: "RangeLWR.SampleTimestamp is set",
			make: func() *chplan.Aggregate {
				lwr := rangeLWRFusionTestLWR()
				lwr.SampleTimestamp = true
				return rangeLWRFusionTestAggregate(chplan.FnSum, lwr)
			},
		},
		{
			name: "GroupBy key references the Value column",
			make: func() *chplan.Aggregate {
				lwr := rangeLWRFusionTestLWR()
				a := rangeLWRFusionTestAggregate(chplan.FnSum, lwr)
				a.GroupBy[0] = &chplan.ColumnRef{Name: lwr.ValueCol}
				return a
			},
		},
		{
			name: "GroupBy key has a qualifier",
			make: func() *chplan.Aggregate {
				lwr := rangeLWRFusionTestLWR()
				a := rangeLWRFusionTestAggregate(chplan.FnSum, lwr)
				a.GroupBy[0] = &chplan.ColumnRef{Name: lwr.AttributesCol, Qualifier: "L"}
				return a
			},
		},
		{
			name: "a GroupByAlias is empty",
			make: func() *chplan.Aggregate {
				a := rangeLWRFusionTestAggregate(chplan.FnSum, rangeLWRFusionTestLWR())
				a.GroupByAliases[0] = ""
				return a
			},
		},
		{
			name: "GroupBy is empty",
			make: func() *chplan.Aggregate {
				a := rangeLWRFusionTestAggregate(chplan.FnSum, rangeLWRFusionTestLWR())
				a.GroupBy = nil
				a.GroupByAliases = nil
				return a
			},
		},
		{
			name: "sum() over a computed expression, not the bare Value column",
			make: func() *chplan.Aggregate {
				lwr := rangeLWRFusionTestLWR()
				a := rangeLWRFusionTestAggregate(chplan.FnSum, lwr)
				a.AggFuncs[0].Args = []chplan.Expr{&chplan.Binary{
					Op: chplan.OpMul, Left: &chplan.ColumnRef{Name: lwr.ValueCol}, Right: &chplan.LitInt{V: 2},
				}}
				return a
			},
		},
		{
			name: "two AggFuncs",
			make: func() *chplan.Aggregate {
				a := rangeLWRFusionTestAggregate(chplan.FnSum, rangeLWRFusionTestLWR())
				a.AggFuncs = append(a.AggFuncs, a.AggFuncs[0])
				return a
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, gotKind := matchRangeLWRFusion(tc.make())
			if gotKind != rangeLWRFusionNone {
				t.Fatalf("matchRangeLWRFusion() kind = %v, want rangeLWRFusionNone (declined)", gotKind)
			}
		})
	}
}

// TestEmitAggregateRangeLWRFused_SumKeepsArgMaxCollapse asserts the sum()
// fusion still renders the per-series argMax collapse (correctness: sum
// must still pick each series' OWN latest sample before summing) while
// dropping RangeLWR's separate rename-only outer SELECT — the fused SQL
// carries exactly one `anchor_ts AS` re-alias to the bucket alias, not the
// two-stage rename (anchor_ts -> TimeUnix -> bucket_ts) the unfused path
// would render.
func TestEmitAggregateRangeLWRFused_SumKeepsArgMaxCollapse(t *testing.T) {
	a := rangeLWRFusionTestAggregate(chplan.FnSum, rangeLWRFusionTestLWR())
	sql, _, err := Emit(context.Background(), a)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if !strings.Contains(sql, "argMax(") {
		t.Errorf("fused sum() SQL dropped the per-series argMax collapse:\n%s", sql)
	}
	if strings.Contains(sql, "uniqExact(") {
		t.Errorf("fused sum() SQL unexpectedly used uniqExact (that's the count() fusion's shape):\n%s", sql)
	}
}

// TestEmitAggregateRangeLWRFused_CountSkipsArgMaxCollapse asserts the
// count() fusion renders uniqExact directly on the fan-out — no argMax
// collapse at all, which is the whole point of this fusion (see
// emitAggregateRangeLWRFusedDistinctCount's doc comment).
func TestEmitAggregateRangeLWRFused_CountSkipsArgMaxCollapse(t *testing.T) {
	a := rangeLWRFusionTestAggregate(chplan.FnCount, rangeLWRFusionTestLWR())
	sql, _, err := Emit(context.Background(), a)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if !strings.Contains(sql, "uniqExact(") {
		t.Errorf("fused count() SQL did not use uniqExact:\n%s", sql)
	}
	if strings.Contains(sql, "argMax(") {
		t.Errorf("fused count() SQL unexpectedly still ran the per-series argMax collapse:\n%s", sql)
	}
	if !strings.Contains(sql, "toFloat64(") {
		t.Errorf("fused count() SQL did not wrap uniqExact's UInt64 result in toFloat64:\n%s", sql)
	}
}

// TestEmitAggregateRangeLWRFused_DeclinedShapeStillEmits pins that a
// declined shape (avg, here) still renders through the ordinary
// opaque-subquery Aggregate path rather than erroring — matchRangeLWRFusion
// declining must never make the query un-emittable.
func TestEmitAggregateRangeLWRFused_DeclinedShapeStillEmits(t *testing.T) {
	a := rangeLWRFusionTestAggregate(chplan.FnAvg, rangeLWRFusionTestLWR())
	sql, _, err := Emit(context.Background(), a)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if !strings.Contains(sql, "avg(") {
		t.Errorf("declined avg() shape did not render avg():\n%s", sql)
	}
}
