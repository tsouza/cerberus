package chsql_test

import (
	"context"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/test/spec"
)

// TestChplanIRDiscriminatesPlansThatEmitDifferentSQL is the end-to-end
// statement of what the `-- chplan --` golden section is for.
//
// The section's whole value is that a reviewer can read an IR snapshot
// and know which plan produced it. That guarantee fails silently the
// moment two plans emit different ClickHouse SQL but print the same
// snapshot: the golden then passes on a change that altered the query,
// and the IR half of Layer 2a is pinning nothing. Both pairs below were
// in exactly that state — same IR, different SQL — until the printer
// learned the fields that select between the emitter's arms.
//
// Asserting the SQL differs first is what keeps the test honest. Without
// it a future emitter change could collapse a pair to one SQL shape and
// this would go on asserting a distinction that no longer means
// anything.
func TestChplanIRDiscriminatesPlansThatEmitDifferentSQL(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		// field names the single field the two plans differ in, so a
		// failure says which printer arm regressed.
		field string
		left  chplan.Node
		right chplan.Node
	}{
		{
			name:  "histogram_quantile native aggregate",
			field: "HistogramQuantile.UseNativeQuantileAggregate",
			left:  histogramQuantilePlan(false),
			right: histogramQuantilePlan(true),
		},
		{
			name:  "range window downsample tier",
			field: "RangeWindow.DownsampleTier",
			left:  downsampleTierPlan(false),
			right: downsampleTierPlan(true),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			leftSQL, _, err := chsql.Emit(ctx, tc.left)
			if err != nil {
				t.Fatalf("emit left: %v", err)
			}
			rightSQL, _, err := chsql.Emit(ctx, tc.right)
			if err != nil {
				t.Fatalf("emit right: %v", err)
			}
			if leftSQL == rightSQL {
				t.Fatalf("%s no longer selects a different emitter arm — the two plans emit "+
					"identical SQL, so this case proves nothing and needs replacing with a "+
					"field that still does", tc.field)
			}

			leftIR := spec.PrintChplan(tc.left)
			rightIR := spec.PrintChplan(tc.right)
			if leftIR == rightIR {
				t.Errorf("two plans emitting different SQL print the same `-- chplan --` "+
					"snapshot, so the golden cannot tell them apart. Differing field: %s\n"+
					"IR:\n%s\nleft SQL:\n%s\nright SQL:\n%s",
					tc.field, leftIR, leftSQL, rightSQL)
			}
		})
	}
}

func histogramQuantilePlan(native bool) chplan.Node {
	return &chplan.HistogramQuantile{
		Input:                      classicQuantileTestInput(),
		Phi:                        0.9,
		MetricNameColumn:           "MetricName",
		AttributesColumn:           "Attributes",
		TimestampColumn:            "TimeUnix",
		BucketCountsColumn:         "BucketCounts",
		ExplicitBoundsColumn:       "ExplicitBounds",
		GroupBy:                    []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		GroupByAliases:             []string{"Attributes"},
		UseNativeQuantileAggregate: native,
	}
}

// downsampleTierPlan builds the same RangeWindow twice, once reading its
// raw Input and once reading the bucketed DownsampleTierInput. The two
// answer the same query from different tables, which is as different as
// two emissions of one node get.
//
// The func is last_over_time because downsampleTierMinSamples accepts
// only irate, idelta and last_over_time.
func downsampleTierPlan(tier bool) chplan.Node {
	const (
		windowRange = 10 * time.Minute
		gridStep    = time.Minute
	)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return &chplan.RangeWindow{
		Input:               &chplan.Scan{Table: "otel_metrics_gauge"},
		DownsampleTierInput: &chplan.Scan{Table: "otel_metrics_gauge_downsampled"},
		DownsampleTier:      tier,
		Func:                "last_over_time",
		Range:               windowRange,
		Step:                gridStep,
		Start:               start,
		End:                 start.Add(windowRange),
		TimestampColumn:     "TimeUnix",
		ValueColumn:         "Value",
		TemporalityColumn:   "Temporality",
		GroupBy:             []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
}
