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

// classicQuantilesPlan is a plural classic plan over classicQuantileTestInput
// that projects three group keys: the Attributes carrier, a renamed key, and
// a key past the end of the alias list.
func classicQuantilesPlan(levels ...chplan.HistogramQuantileLevel) *chplan.HistogramQuantiles {
	return &chplan.HistogramQuantiles{
		Histogram: &chplan.HistogramQuantile{
			Input:                      classicQuantileTestInput(),
			GroupBy:                    quantilesGroupBy(),
			GroupByAliases:             quantilesGroupByAliases(),
			AttributesColumn:           "Attributes",
			UseNativeQuantileAggregate: true,
		},
		LabelName: "quantile",
		Levels:    levels,
	}
}

// nativeQuantilesPlan is the exponential-histogram sibling of
// classicQuantilesPlan.
func nativeQuantilesPlan(levels ...chplan.HistogramQuantileLevel) *chplan.HistogramQuantilesNative {
	return &chplan.HistogramQuantilesNative{
		Histogram: &chplan.HistogramQuantileNative{
			Input:            nativeQuantileTestInput(true),
			GroupBy:          quantilesGroupBy(),
			GroupByAliases:   quantilesGroupByAliases(),
			AttributesColumn: "Attributes",
		},
		LabelName: "quantile",
		Levels:    levels,
	}
}

func quantilesGroupBy() []chplan.Expr {
	return []chplan.Expr{
		&chplan.ColumnRef{Name: "Attributes"},
		&chplan.ColumnRef{Name: "MetricName"},
		&chplan.ColumnRef{Name: "TimeUnix"},
	}
}

func quantilesGroupByAliases() []string { return []string{"Attributes", "Metric"} }

func emitQuantilesSQL(t *testing.T, plan chplan.Node) string {
	t.Helper()
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	return sql
}

// TestEmitHistogramQuantiles_OneRowPerLevelWithLabelledAttributes pins the
// output rows both plural emitters share: every group key under its alias,
// the key past the end of the alias list unaliased, the level label stamped
// into the Attributes carrier only, and the level's quantile as Value — one
// row per level via arrayJoin over (label, value) pairs.
func TestEmitHistogramQuantiles_OneRowPerLevelWithLabelledAttributes(t *testing.T) {
	t.Parallel()

	levels := []chplan.HistogramQuantileLevel{{Phi: 0.25, Label: "0.25"}, {Phi: 0.75, Label: "0.75"}}
	const wantPrefix = "SELECT mapConcat(`Attributes`, map('quantile', `_cerb_hq_level`.1)) AS `Attributes`, " +
		"`MetricName` AS `Metric`, `TimeUnix`, `_cerb_hq_level`.2 AS `Value` " +
		"FROM (SELECT *, arrayJoin(arrayZip(['0.25', '0.75'], `_cerb_hq_levels`)) AS `_cerb_hq_level` FROM ("
	for name, plan := range map[string]chplan.Node{
		"classic": classicQuantilesPlan(levels...),
		"native":  nativeQuantilesPlan(levels...),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if sql := emitQuantilesSQL(t, plan); !strings.HasPrefix(sql, wantPrefix) {
				t.Errorf("level rows:\n got: %s\nwant prefix: %s", sql, wantPrefix)
			}
		})
	}
}

// TestEmitHistogramQuantiles_SharedAggregateCarriesEveryLevel pins the plural
// ClickHouse aggregate's parameter list and how each level reads it back. The
// parameters list every level in order, comma-separated, with an
// out-of-domain phi clamped into [0, 1] (NaN to 0) so the aggregate stays
// valid; the level's value then ignores that clamped slot and answers
// NaN / -Inf / +Inf the way Prometheus does, while an in-domain level —
// including the closed bounds 0 and 1 — reads its own 1-based slot of the
// aggregate's result array.
func TestEmitHistogramQuantiles_SharedAggregateCarriesEveryLevel(t *testing.T) {
	t.Parallel()

	sql := emitQuantilesSQL(t, classicQuantilesPlan(
		chplan.HistogramQuantileLevel{Phi: 0.5, Label: "0.5"},
		chplan.HistogramQuantileLevel{Phi: math.NaN(), Label: "NaN"},
		chplan.HistogramQuantileLevel{Phi: -0.5, Label: "-0.5"},
		chplan.HistogramQuantileLevel{Phi: 1.5, Label: "1.5"},
		chplan.HistogramQuantileLevel{Phi: 0.9, Label: "0.9"},
		chplan.HistogramQuantileLevel{Phi: 0, Label: "0"},
		chplan.HistogramQuantileLevel{Phi: 1, Label: "1"},
	))

	const wantAggregate = "arrayReduce(concat('quantilesPrometheusHistogram(', toString(0.5), ',', toString(0), ',', " +
		"toString(0), ',', toString(1), ',', toString(0.9), ',', toString(0), ',', toString(1), ')'), "
	if !strings.Contains(sql, wantAggregate) {
		t.Errorf("aggregate parameters: want %s\nSQL: %s", wantAggregate, sql)
	}

	level := func(value string) string {
		return "if(length(`BucketCounts`) = 0, nan, if(`_cerb_hqc_observations` = 0, nan, " + value + "))"
	}
	wantValues := "[" + strings.Join([]string{
		level("`_cerb_hq_levels_raw`[1]"),
		level("nan"),
		level("-inf"),
		level("inf"),
		level("`_cerb_hq_levels_raw`[5]"),
		level("`_cerb_hq_levels_raw`[6]"),
		level("`_cerb_hq_levels_raw`[7]"),
	}, ", ") + "] AS `_cerb_hq_levels`"
	if !strings.Contains(sql, wantValues) {
		t.Errorf("level values: want %s\nSQL: %s", wantValues, sql)
	}
}

// TestEmitHistogramQuantilesNative_MaterialisesReverseWalkOnlyWhenALevelNeedsIt
// pins the shared reverse cumulative walk: it is bound once (as a
// hqNativeLet lambda parameter, not a derived-query column — see
// hqNativeBindPrepared) when any level can take the backward rank walk (a
// literal phi at or above 0.5), and left out when every level resolves to
// the forward walk at emit time.
func TestEmitHistogramQuantilesNative_MaterialisesReverseWalkOnlyWhenALevelNeedsIt(t *testing.T) {
	t.Parallel()

	// hqNativeBindPrepared always binds _cerb_hq_revcum last among the
	// lambda's parameters when it binds it at all, so its bare (unquoted)
	// spelling immediately before the lambda arrow is the one-and-only
	// binding site; every read of the bound value inside the lambda body
	// quotes it as an ordinary identifier, `_cerb_hq_revcum`.
	const declSite = ", _cerb_hq_revcum) ->"
	const useSite = "`_cerb_hq_revcum`"
	forward := emitQuantilesSQL(t, nativeQuantilesPlan(
		chplan.HistogramQuantileLevel{Phi: 0.1, Label: "0.1"},
		chplan.HistogramQuantileLevel{Phi: 0.25, Label: "0.25"},
	))
	if strings.Contains(forward, declSite) || strings.Contains(forward, useSite) {
		t.Errorf("forward-only levels materialised the reverse walk\nSQL: %s", forward)
	}
	mixed := emitQuantilesSQL(t, nativeQuantilesPlan(
		chplan.HistogramQuantileLevel{Phi: 0.1, Label: "0.1"},
		chplan.HistogramQuantileLevel{Phi: 0.9, Label: "0.9"},
	))
	if got := strings.Count(mixed, declSite); got != 1 {
		t.Errorf("a level at phi 0.9 needs the reverse walk bound once, got %d\nSQL: %s", got, mixed)
	}
	// The level's value reads the bound parameter rather than re-deriving
	// the reverse walk inline.
	if uses := strings.Count(mixed, useSite); uses == 0 {
		t.Errorf("the phi 0.9 level never reads the bound reverse walk\nSQL: %s", mixed)
	}
}

// TestEmitHistogramQuantiles_RejectsIncompletePlans pins that each plural
// emitter rejects a plan with no histogram, with no input under the
// histogram, or with no levels, as an unsupported plan.
func TestEmitHistogramQuantiles_RejectsIncompletePlans(t *testing.T) {
	t.Parallel()

	level := chplan.HistogramQuantileLevel{Phi: 0.5, Label: "0.5"}
	classicNoInput := classicQuantilesPlan(level)
	classicNoInput.Histogram.Input = nil
	nativeNoInput := nativeQuantilesPlan(level)
	nativeNoInput.Histogram.Input = nil
	for name, plan := range map[string]chplan.Node{
		"classic/no histogram": &chplan.HistogramQuantiles{LabelName: "quantile", Levels: []chplan.HistogramQuantileLevel{level}},
		"classic/no input":     classicNoInput,
		"classic/no levels":    classicQuantilesPlan(),
		"native/no histogram":  &chplan.HistogramQuantilesNative{LabelName: "quantile", Levels: []chplan.HistogramQuantileLevel{level}},
		"native/no input":      nativeNoInput,
		"native/no levels":     nativeQuantilesPlan(),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sql, _, err := chsql.Emit(context.Background(), plan)
			if !errors.Is(err, chsql.ErrUnsupported) {
				t.Fatalf("Emit error = %v, want ErrUnsupported\nSQL: %s", err, sql)
			}
		})
	}
}
