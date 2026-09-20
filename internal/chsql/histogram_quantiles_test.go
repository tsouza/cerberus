package chsql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

func TestEmitHistogramQuantiles_UsesOnePluralClickHouseState(t *testing.T) {
	t.Parallel()

	plan := &chplan.HistogramQuantiles{
		Histogram: &chplan.HistogramQuantile{
			Input:                      classicQuantileTestInput(),
			BucketCountsColumn:         "BucketCounts",
			ExplicitBoundsColumn:       "ExplicitBounds",
			GroupBy:                    []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
			GroupByAliases:             []string{"Attributes"},
			AttributesColumn:           "Attributes",
			UseNativeQuantileAggregate: true,
		},
		LabelName: "quantile",
		Levels: []chplan.HistogramQuantileLevel{
			{Phi: 0.5, Label: "0.5"},
			{Phi: 0.95, Label: "0.95"},
			{Phi: 0.99, Label: "0.99"},
		},
	}

	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	for token, want := range map[string]int{
		"quantilesPrometheusHistogram(": 1,
		"arrayReduce(":                  1,
		"arrayZip(":                     1,
	} {
		if got := strings.Count(sql, token); got != want {
			t.Errorf("%s occurs %d time(s), want %d\nSQL: %s", token, got, want, sql)
		}
	}
	if strings.Contains(sql, "quantilePrometheusHistogram(") {
		t.Errorf("plural plan emitted a singular ClickHouse state\nSQL: %s", sql)
	}
}
