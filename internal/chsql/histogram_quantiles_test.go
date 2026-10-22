package chsql_test

import (
	"context"
	"errors"
	"math"
	"strconv"
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
// classicQuantilesPlan. Its input carries the group keys as columns, as the
// lowering's folded-histogram input does.
func nativeQuantilesPlan(levels ...chplan.HistogramQuantileLevel) *chplan.HistogramQuantilesNative {
	input := nativeQuantileTestInput(true)
	input.Columns = append([]string{"Attributes", "MetricName", "TimeUnix"}, input.Columns...)
	input.Roles = append([]chplan.Column{
		{Name: "Attributes", Role: chplan.RoleAttributes},
		{Name: "MetricName", Role: chplan.RoleMetricName},
		{Name: "TimeUnix", Role: chplan.RoleTimestamp},
	}, input.Roles...)
	return &chplan.HistogramQuantilesNative{
		Histogram: &chplan.HistogramQuantileNative{
			Input:            input,
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
// the key past the end of the alias list under its own name, the level label
// stamped into the Attributes carrier only, and the level's quantile as
// Value — one row per level over (label, value) pairs. The classic emitter,
// and the native one when a key is not an input column, expand the pairs with
// arrayJoin in a derived query; the native one otherwise expands them with
// the ARRAY JOIN of the SELECT that reads the input boundary's tuple.
func TestEmitHistogramQuantiles_OneRowPerLevelWithLabelledAttributes(t *testing.T) {
	t.Parallel()

	levels := []chplan.HistogramQuantileLevel{{Phi: 0.25, Label: "0.25"}, {Phi: 0.75, Label: "0.75"}}
	const staged = "SELECT mapConcat(`Attributes`, map('quantile', `_cerb_hq_level`.1)) AS `Attributes`, " +
		"`MetricName` AS `Metric`, `TimeUnix`, `_cerb_hq_level`.2 AS `Value` " +
		"FROM (SELECT *, arrayJoin(arrayZip(['0.25', '0.75'], `_cerb_hq_levels`)) AS `_cerb_hq_level` FROM ("
	const inline = "SELECT mapConcat(`_cerb_histogram_input`.1, map('quantile', `_cerb_hq_level`.1)) AS `Attributes`, " +
		"`_cerb_histogram_input`.2 AS `Metric`, `_cerb_histogram_input`.3 AS `TimeUnix`, `_cerb_hq_level`.2 AS `Value` " +
		"FROM (SELECT arrayJoin([(`Attributes`, `MetricName`, `TimeUnix`, "
	computedKey := nativeQuantilesPlan(levels...)
	computedKey.Histogram.GroupBy = append([]chplan.Expr{
		&chplan.FuncCall{Fn: chplan.FnMapSort, Args: []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}}},
	}, computedKey.Histogram.GroupBy[1:]...)
	const computedStaged = "SELECT mapConcat(mapSort(`Attributes`), map('quantile', `_cerb_hq_level`.1)) AS `Attributes`, " +
		"`MetricName` AS `Metric`, `TimeUnix`, `_cerb_hq_level`.2 AS `Value` " +
		"FROM (SELECT *, arrayJoin(arrayZip(['0.25', '0.75'], `_cerb_hq_levels`)) AS `_cerb_hq_level` FROM ("
	for name, tc := range map[string]struct {
		plan       chplan.Node
		wantPrefix string
		wantJoin   string
	}{
		"classic":             {plan: classicQuantilesPlan(levels...), wantPrefix: staged},
		"native":              {plan: nativeQuantilesPlan(levels...), wantPrefix: inline, wantJoin: ") ARRAY JOIN arrayZip(['0.25', '0.75'], "},
		"native/computed key": {plan: computedKey, wantPrefix: computedStaged},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sql := emitQuantilesSQL(t, tc.plan)
			if !strings.HasPrefix(sql, tc.wantPrefix) {
				t.Errorf("level rows:\n got: %s\nwant prefix: %s", sql, tc.wantPrefix)
			}
			if tc.wantJoin != "" && !strings.Contains(sql, tc.wantJoin) {
				t.Errorf("level rows do not expand through %q\n--- sql ---\n%s", tc.wantJoin, sql)
			}
		})
	}
}

// TestEmitHistogramQuantilesNative_SingleSelectOverBoundary pins the nesting
// depth of the plural native quantile. ClickHouse's query analysis re-walks a
// derived query's whole subtree for every level above it, so the plural
// quantile, like the single-phi one, is exactly two SELECTs over its input:
// the ARRAY JOIN materialization boundary, and the SELECT that reads the
// histogram fields off the boundary's tuple, computes every level and expands
// the levels into rows through its own ARRAY JOIN.
func TestEmitHistogramQuantilesNative_SingleSelectOverBoundary(t *testing.T) {
	t.Parallel()

	plan := nativeQuantilesPlan(
		chplan.HistogramQuantileLevel{Phi: 0.25, Label: "0.25"},
		chplan.HistogramQuantileLevel{Phi: 0.9, Label: "0.9"},
	)
	sql := emitQuantilesSQL(t, plan)
	input, _, err := chsql.Emit(context.Background(), plan.Histogram.Input)
	if err != nil {
		t.Fatalf("Emit input: %v", err)
	}
	const quantileAndBoundary = 2
	if got, want := strings.Count(sql, "SELECT "), strings.Count(input, "SELECT ")+quantileAndBoundary; got != want {
		t.Errorf("plural native quantile renders %d SELECTs over an input of %d, want %d\n--- sql ---\n%s",
			got, strings.Count(input, "SELECT "), want, sql)
	}
}

// TestEmitHistogramQuantilesNative_BindsEachLevelsStopOnce pins that each
// level binds its own rank-walk stop index, running count and value index
// exactly once, under names no other level shares, and that the rank walk
// itself is rendered once per arm a level can take. Rendered at every use
// site instead, the walk and the values read off it repeat many times per
// level, and on ClickHouse's analyzer that repetition is planning cost.
func TestEmitHistogramQuantilesNative_BindsEachLevelsStopOnce(t *testing.T) {
	t.Parallel()

	levels := []chplan.HistogramQuantileLevel{
		{Phi: 0.25, Label: "0.25"},
		{Phi: 0.75, Label: "0.75"},
		{Phi: 0.9, Label: "0.9"},
	}
	sql := emitQuantilesSQL(t, nativeQuantilesPlan(levels...))
	for i := range levels {
		for _, helper := range []string{"_cerb_hq_idx", "_cerb_hq_count", "_cerb_hq_value_idx"} {
			decl := " AS `" + helper + "_" + strconv.Itoa(i+1) + "`"
			if got := strings.Count(sql, decl); got != 1 {
				t.Errorf("level %d binds %s %d times, want 1\n--- sql ---\n%s", i+1, helper, got, sql)
			}
		}
	}
	// Every level can take the forward walk (a NaN Sum forces it); the two
	// levels at or above 0.5 can also take the backward one.
	const forwardWalks, backwardWalks = 3, 2
	if got := strings.Count(sql, "arrayFirstIndex(c -> c >= ("); got != forwardWalks {
		t.Errorf("forward rank walk rendered %d times, want %d\n--- sql ---\n%s", got, forwardWalks, sql)
	}
	if got := strings.Count(sql, "arrayLastIndex(c -> c >= ((1 - "); got != backwardWalks {
		t.Errorf("backward rank walk rendered %d times, want %d\n--- sql ---\n%s", got, backwardWalks, sql)
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
// hqNativeLet SELECT-scope alias, not a derived-query column — see
// hqNativeBindPrepared) when any level can take the backward rank walk (a
// literal phi at or above 0.5), and left out when every level resolves to
// the forward walk at emit time.
func TestEmitHistogramQuantilesNative_MaterialisesReverseWalkOnlyWhenALevelNeedsIt(t *testing.T) {
	t.Parallel()

	// The alias declaration is the one-and-only binding site; every read of
	// the bound value names it as an ordinary identifier, `_cerb_hq_revcum`.
	const declSite = " AS `_cerb_hq_revcum`"
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
	// The level's value reads the bound alias rather than re-deriving the
	// reverse walk inline; the declaration itself spells the name once.
	if uses := strings.Count(mixed, useSite) - strings.Count(mixed, declSite); uses == 0 {
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
