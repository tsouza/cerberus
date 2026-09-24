package chsql_test

import (
	"context"
	"errors"
	"math"
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

// classicQuantilesPlan is a classic histogram_quantiles plan over the
// default classic columns with the given group keys, aliases and levels.
func classicQuantilesPlan(groupBy []chplan.Expr, aliases []string, levels ...chplan.HistogramQuantileLevel) *chplan.HistogramQuantiles {
	return &chplan.HistogramQuantiles{
		Histogram: &chplan.HistogramQuantile{
			Input:                      classicQuantileTestInput(),
			BucketCountsColumn:         "BucketCounts",
			ExplicitBoundsColumn:       "ExplicitBounds",
			GroupBy:                    groupBy,
			GroupByAliases:             aliases,
			AttributesColumn:           "Attributes",
			UseNativeQuantileAggregate: true,
		},
		LabelName: "quantile",
		Levels:    levels,
	}
}

// TestEmitHistogramQuantiles_ClampsAggregateLevels pins the level list handed
// to ClickHouse's quantilesPrometheusHistogram: every level lies in [0, 1] (a
// NaN or out-of-domain phi is answered per level instead), levels are comma
// separated, and each level's value reads its own position of the aggregate's
// result array.
func TestEmitHistogramQuantiles_ClampsAggregateLevels(t *testing.T) {
	t.Parallel()

	plan := classicQuantilesPlan(
		[]chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}}, []string{"Attributes"},
		chplan.HistogramQuantileLevel{Phi: math.NaN(), Label: "nan"},
		chplan.HistogramQuantileLevel{Phi: -0.5, Label: "low"},
		chplan.HistogramQuantileLevel{Phi: 0.25, Label: "mid"},
		chplan.HistogramQuantileLevel{Phi: 1, Label: "one"},
		chplan.HistogramQuantileLevel{Phi: 1.5, Label: "high"},
	)
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	for _, want := range []string{
		"concat('quantilesPrometheusHistogram(', toString(0), ',', toString(0), ',', toString(0.25), ',', toString(1), ',', toString(1), ')')",
		"if(`_cerb_hqc_observations` = 0, nan, nan))",
		"if(`_cerb_hqc_observations` = 0, nan, -inf))",
		"if(`_cerb_hqc_observations` = 0, nan, `_cerb_hq_levels_raw`[3]))",
		"if(`_cerb_hqc_observations` = 0, nan, `_cerb_hq_levels_raw`[4]))",
		"if(`_cerb_hqc_observations` = 0, nan, inf))",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("SQL missing %q\nSQL: %s", want, sql)
		}
	}
}

// TestEmitHistogramQuantiles_LabelsOnlyTheAttributesKey pins the output keys:
// the key aliased to the Attributes column gains the quantile label, any other
// aliased key renders under its alias unchanged, and a key beyond the alias
// list renders bare.
func TestEmitHistogramQuantiles_LabelsOnlyTheAttributesKey(t *testing.T) {
	t.Parallel()

	plan := classicQuantilesPlan(
		[]chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}, &chplan.ColumnRef{Name: "svc"}, &chplan.ColumnRef{Name: "job"}},
		[]string{"Attributes", "service"},
		chplan.HistogramQuantileLevel{Phi: 0.5, Label: "0.5"},
	)
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	const want = "SELECT mapConcat(`Attributes`, map('quantile', `_cerb_hq_level`.1)) AS `Attributes`, `svc` AS `service`, `job`, `_cerb_hq_level`.2 AS `Value` FROM "
	if !strings.HasPrefix(sql, want) {
		t.Errorf("SQL does not start with %q\nSQL: %s", want, sql)
	}
}

// TestEmitHistogramQuantiles_RejectsIncompletePlans pins the guards of both
// plural emitters: a missing histogram, a histogram without input, and an
// empty level list are each unsupported on their own.
func TestEmitHistogramQuantiles_RejectsIncompletePlans(t *testing.T) {
	t.Parallel()

	level := []chplan.HistogramQuantileLevel{{Phi: 0.5, Label: "0.5"}}
	classic := func() *chplan.HistogramQuantile {
		return classicQuantilesPlan(nil, nil).Histogram
	}
	native := func() *chplan.HistogramQuantileNative {
		return hqNativePlan(0.5, nil)
	}
	noInputClassic, noInputNative := classic(), native()
	noInputClassic.Input, noInputNative.Input = nil, nil
	for name, plan := range map[string]chplan.Node{
		"classic without histogram": &chplan.HistogramQuantiles{Levels: level},
		"classic without input":     &chplan.HistogramQuantiles{Histogram: noInputClassic, Levels: level},
		"classic without levels":    &chplan.HistogramQuantiles{Histogram: classic()},
		"native without histogram":  &chplan.HistogramQuantilesNative{Levels: level},
		"native without input":      &chplan.HistogramQuantilesNative{Histogram: noInputNative, Levels: level},
		"native without levels":     &chplan.HistogramQuantilesNative{Histogram: native()},
	} {
		if _, _, err := chsql.Emit(context.Background(), plan); !errors.Is(err, chsql.ErrUnsupported) || !strings.Contains(err.Error(), "requires a histogram input and levels") {
			t.Errorf("%s: Emit error = %v, want the unsupported histogram-input-and-levels error", name, err)
		}
	}
}
