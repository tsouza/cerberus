package promql

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/optimizer"
	"github.com/tsouza/cerberus/internal/schema"
)

func TestRowTypeSyntheticSampleRoles(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	s.MetricNameColumn = "custom_name"
	s.AttributesColumn = "custom_labels"
	s.TimestampColumn = "custom_time"
	s.ValueColumn = "custom_value"
	for _, query := range []string{"time()", "vector(3)", "abs(vector(3))", "vector(scalar(up))", "sum(up)"} {
		t.Run(query, func(t *testing.T) {
			expr, err := parser.NewParser(parser.Options{EnableExperimentalFunctions: true}).ParseExpr(query)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := Lower(context.Background(), expr, s)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range metricRoles(s) {
				got, ok := plan.RowType().ByName(want.Name)
				if !ok || got.Role != want.Role {
					t.Errorf("query %s role %s = %#v, %v; want %#v", query, want.Name, got, ok, want)
				}
			}
		})
	}
}

func TestRowTypeHistogramStorageRoles(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	s.CountColumn = "custom_count"
	s.SumColumn = "custom_sum"
	s.ScaleColumn = "custom_scale"
	s.ZeroThresholdColumn = "custom_zero_threshold"
	s.ZeroCountColumn = "custom_zero_count"
	s.PositiveOffsetColumn = "custom_positive_offset"
	s.PositiveBucketCountsColumn = "custom_positive_buckets"
	s.NegativeOffsetColumn = "custom_negative_offset"
	s.NegativeBucketCountsColumn = "custom_negative_buckets"
	s.BucketCountsColumn = "custom_classic_buckets"
	s.ExplicitBoundsColumn = "custom_explicit_bounds"

	exponential := map[chplan.HistogramField]string{
		chplan.HistogramFieldCount:                s.CountColumn,
		chplan.HistogramFieldSum:                  s.SumColumn,
		chplan.HistogramFieldScale:                s.ScaleColumn,
		chplan.HistogramFieldZeroThreshold:        s.ZeroThresholdColumn,
		chplan.HistogramFieldZeroCount:            s.ZeroCountColumn,
		chplan.HistogramFieldPositiveOffset:       s.PositiveOffsetColumn,
		chplan.HistogramFieldPositiveBucketCounts: s.PositiveBucketCountsColumn,
		chplan.HistogramFieldNegativeOffset:       s.NegativeOffsetColumn,
		chplan.HistogramFieldNegativeBucketCounts: s.NegativeBucketCountsColumn,
	}
	classic := map[chplan.HistogramField]string{
		chplan.HistogramFieldCount:          s.CountColumn,
		chplan.HistogramFieldSum:            s.SumColumn,
		chplan.HistogramFieldBucketCounts:   s.BucketCountsColumn,
		chplan.HistogramFieldExplicitBounds: s.ExplicitBoundsColumn,
	}
	for _, table := range []string{s.HistogramTable, s.ExpHistogramTable} {
		roles := chplan.Schema{Columns: metricScanRoles(s, table)}
		if roles.Has(chplan.RoleValue) {
			t.Errorf("histogram %s falsely declares float Value", table)
		}
		want := classic
		if table == s.ExpHistogramTable {
			want = exponential
		}
		for identity, name := range want {
			column, ok := roles.FindHistogramField(identity)
			if !ok || column.Name != name {
				t.Errorf("histogram %s identity %d = %#v/%v, want %q", table, identity, column, ok, name)
			}
		}
	}
}

func TestRowTypeOptimizerPreservesDeclarations(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	plan := &chplan.Project{Input: &chplan.OneRow{}, Roles: metricRoles(s), Projections: []chplan.Projection{{
		Expr: &chplan.Binary{Op: chplan.OpAdd, Left: &chplan.LitInt{V: 1}, Right: &chplan.LitInt{V: 2}}, Alias: s.ValueColumn,
	}}}
	want := plan.RowType()
	optimized := optimizer.Default().Run(context.Background(), plan)
	if got := optimized.RowType(); !got.Equal(want) {
		t.Fatalf("optimizer lost output declaration: got %#v, want %#v", got, want)
	}
}

func TestRowTypeExpHistogramRateCarriesPhysicalPayload(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	expr, err := parser.NewParser(parser.Options{EnableExperimentalFunctions: true}).ParseExpr(
		"rate(dense_exp_hist[5m])",
	)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	plan, err := LowerAt(context.Background(), expr, s, at, at)
	if err != nil {
		t.Fatal(err)
	}
	histogram, ok := plan.(*chplan.HistogramProjection)
	if !ok {
		t.Fatalf("plan = %T, want *chplan.HistogramProjection", plan)
	}
	want := []chplan.HistogramField{
		chplan.HistogramFieldCount,
		chplan.HistogramFieldSum,
		chplan.HistogramFieldScale,
		chplan.HistogramFieldZeroCount,
		chplan.HistogramFieldPositiveOffset,
		chplan.HistogramFieldPositiveBucketCounts,
		chplan.HistogramFieldNegativeOffset,
		chplan.HistogramFieldNegativeBucketCounts,
	}
	childSchema := histogram.Input.RowType()
	if childSchema.Open {
		t.Fatal("HistogramProjection child schema is open")
	}
	for _, field := range want {
		if _, ok := childSchema.FindHistogramField(field); !ok {
			t.Errorf("HistogramProjection child schema is missing field %d: %#v", field, childSchema)
		}
	}
}

func TestLowerExpHistogramRangeClosesFanoutInputSchema(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	expr, err := parser.NewParser(parser.Options{EnableExperimentalFunctions: true}).ParseExpr(
		"rate(dense_exp_hist[5m])",
	)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	plan, err := LowerAtRange(context.Background(), expr, s, start, start.Add(time.Minute), 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	chplan.Walk(plan, func(node chplan.Node) bool {
		fanout, ok := node.(*chplan.RangeBucketFanout)
		if !ok {
			return true
		}
		found = true
		childSchema := fanout.Input.RowType()
		if childSchema.Open {
			t.Error("RangeBucketFanout child schema is open")
		}
		if _, ok := childSchema.Find(chplan.RoleTimestamp); !ok {
			t.Errorf("RangeBucketFanout child schema has no timestamp role: %#v", childSchema)
		}
		return true
	})
	if !found {
		t.Fatal("lowered range plan has no RangeBucketFanout")
	}
}
