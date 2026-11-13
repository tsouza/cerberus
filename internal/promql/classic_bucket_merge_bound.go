package promql

import (
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// classic_bucket_merge_bound.go bounds the classic-histogram cross-series
// bucket merge — classicBucketShaping.reshape over the Aggregate that
// classicBucketMergeShaping or classicBucketMergeShapingSumMap builds — by
// the number of bucket entries one merge group carries.
//
// # What the merge costs
//
// Both merge constructions, classic_bucket_bound_runs.go's sort-and-split
// fold and classic_bucket_merge_summap.go's sumMap, are linear in a group's
// bucket entries: every collected row's finite bounds plus its +Inf rung.
// The guard reads exactly that count, sum(length(ExplicitBounds) + 1),
// collected by the merge Aggregate itself, so it costs one comparison per
// group and needs no model of the layout: disjoint, shared and overlapping
// layouts cost the same per entry.
//
// # Ordering
//
// The guard is a data dependency of the merge, not a Filter beneath it:
// every array the merge reads off the Aggregate passes through
// classicBucketMergeGateExpr first. See that function for why a Filter is
// not evaluated before the Projects above it.
//
// # Calibration (real ClickHouse 26.6.8.7 and 26.8.36.3)
//
// `histogram_quantile(0.5, sum by (le) (sum_over_time(<bucket>[5m])))` was
// served through the production Prometheus handler (lowerer, emitter,
// engine settings) against clickhouse/clickhouse-server containers capped
// at 8 GiB, with the guard lifted and max_memory_usage at 6 GiB, so each
// peak is the query's real need rather than the point an abort stopped at.
// Peak memory_usage was read from system.query_log after SYSTEM FLUSH LOGS
// and a settle delay. Series i carries one sample with W bounds
// `[i*W+1 .. i*W+W]` (disjoint) or the same W bounds as every other series
// (shared):
//
//	layout    series x W     entries     peak 26.8    peak 26.6
//	disjoint  25,000 x  50   1,275,000    448.4 MiB    379.8 MiB
//	disjoint  50,000 x  50   2,550,000    624.4 MiB    517.5 MiB
//	disjoint  75,000 x  50   3,825,000    733.3 MiB    603.0 MiB
//	disjoint 100,000 x  50   5,100,000   1246.9 MiB   1028.8 MiB
//	disjoint 150,000 x  50   7,650,000   1463.1 MiB   1201.0 MiB
//	shared    50,000 x  50   2,550,000    624.5 MiB    515.8 MiB
//	shared   100,000 x  50   5,100,000   1246.9 MiB   1028.7 MiB
//	shared   200,000 x  50  10,200,000   2620.9 MiB   2178.0 MiB
//	disjoint  10,000 x 250   2,510,000    648.8 MiB    539.7 MiB
//	disjoint  20,000 x 250   5,020,000   1291.6 MiB   1076.1 MiB
//
// Peak memory follows the entry count alone — the disjoint and shared
// layouts at 50,000 and 100,000 series match to the tenth of a MiB, and
// width 250 matches width 50 at equal entries — in steps (buffer growth
// doubles), whose upper envelope is classicBucketMergeBytesPerEntry: the
// largest ratio measured, 26.8 at 200,000 x 50 and 20,000 x 250. 26.6 sits
// below 26.8 at every point.
//
// The same seed at the shipped ceiling, under the 1 GiB cap with the
// default guard: 58,000 x 50 (2,958,000 entries) completes at 659.2 MiB
// (26.8) and 544.4 MiB (26.6); 60,000 x 50 (3,060,000 entries) is refused
// with the guard's message at 154.4 / 155.6 MiB, before the merge ran.
//
// The per-series window stage that feeds the merge (classicBucketWindowStage)
// is the same sort-and-split construction, run once per series over that
// series' in-window samples, so its cost is linear in the samples the query
// scans rather than in the merge's entries, and this guard does not bound
// it. Measured the same way under the 1 GiB cap: `rate` over 20,000 series
// with one shared 50-bucket layout and 14 samples each completes at
// 492.8 MiB (26.8) and 500.9 MiB (26.6).
const (
	// classicBucketMergeBytesPerEntry is the merge's measured peak cost per
	// bucket entry: the upper envelope of the calibration in this file's
	// header (26.8, 200,000 x 50 shared and 20,000 x 250 disjoint, both
	// about 270 bytes per entry).
	classicBucketMergeBytesPerEntry = 270

	// classicBucketMergeQueryMemoryBytes is the per-query memory cap the
	// ceiling is sized against: CERBERUS_CH_QUERY_MAX_MEMORY's 1 GiB
	// default.
	classicBucketMergeQueryMemoryBytes = 1 << 30

	// classicBucketMergeCapSharePercent is the share of the query cap the
	// merge may use at the ceiling. The rest covers the scan and the
	// per-series window stage that run in the same query, and the step the
	// peak takes between calibration points (75,000 x 50 peaks at 733 MiB,
	// 100,000 x 50 at 1247 MiB on 26.8).
	classicBucketMergeCapSharePercent = 75

	// percentDenominator turns classicBucketMergeCapSharePercent into a
	// fraction.
	percentDenominator = 100

	// maxClassicBucketMergeEntries bounds the bucket entries one group of
	// the classic-histogram cross-series merge may carry: the sum, over
	// every row the merge Aggregate collects, of that row's finite bound
	// count plus its +Inf rung. It is classicBucketMergeCapSharePercent of
	// the 1 GiB cap at classicBucketMergeBytesPerEntry: 2,982,616 entries.
	//
	// It is the compile-time default of
	// [ResourceBounds.ClassicBucketMergeMaxCostUnits], which an operator
	// overrides through [EnvClassicBucketMergeMaxCostUnits]
	// (CERBERUS_PROMQL_CLASSIC_BUCKET_MERGE_MAX_COST_UNITS) — see
	// resource_bounds_env.go. The guard checks the resolved value.
	maxClassicBucketMergeEntries = classicBucketMergeQueryMemoryBytes * classicBucketMergeCapSharePercent /
		percentDenominator / classicBucketMergeBytesPerEntry
)

// hqAggMergeEntriesAlias holds a merge group's bucket-entry count — the
// quantity [maxClassicBucketMergeEntries] bounds. Both merge shapings
// (classicBucketMergeShaping and classicBucketMergeShapingSumMap) collect
// it, so the guard reads one scalar column whichever shaping built the
// Aggregate.
const hqAggMergeEntriesAlias = "_hq_merge_entries"

// classicBucketMergeEntriesAgg is the aggregate the guard reads: per group,
// `sum(length(ExplicitBounds) + 1)` — every row's finite bounds plus the
// +Inf rung its trailing count carries.
func classicBucketMergeEntriesAgg(s schema.Metrics) chplan.AggFunc {
	return chplan.AggFunc{
		Fn: chplan.FnSum,
		Args: []chplan.Expr{addExpr(
			&chplan.FuncCall{Fn: chplan.FnLength, Args: []chplan.Expr{&chplan.ColumnRef{Name: s.ExplicitBoundsColumn}}},
			&chplan.LitInt{V: 1},
		)},
		Alias: hqAggMergeEntriesAlias,
	}
}

// classicBucketMergeGateExpr returns list — an array the merge reads off
// the merge Aggregate — unchanged, but computed through the guard:
//
//	arraySlice(list, 1, length(list) + throwIf(entries > maxEntries, message))
//
// throwIf answers 0 when the group is within the ceiling, so the slice is
// the whole array; past the ceiling it aborts the query with the guard's
// message. The merge cannot read list without evaluating the throwIf first,
// and that data dependency is the point: a guard placed as a Filter beneath
// the merge's Projects is not ordered before them, because ClickHouse folds
// the Filter and the Projects above it into one expression step and
// computes the projected merge alongside the filter column.
//
// maxEntries is the caller-resolved ceiling
// ([ResourceBounds.ClassicBucketMergeMaxCostUnits]); it is threaded in
// rather than read from the environment here because this package may not
// depend on internal/config (.go-arch-lint.yml).
func classicBucketMergeGateExpr(list chplan.Expr, maxEntries int64) chplan.Expr {
	guard := &chplan.FuncCall{
		Fn: chplan.FnThrowIf,
		Args: []chplan.Expr{
			gtLit(&chplan.ColumnRef{Name: hqAggMergeEntriesAlias}, maxEntries),
			&chplan.InlineString{V: chplan.ClassicBucketMergeBudgetMessage},
		},
	}
	return &chplan.FuncCall{Fn: chplan.FnArraySlice, Args: []chplan.Expr{
		list,
		&chplan.LitInt{V: firstPosition},
		addExpr(&chplan.FuncCall{Fn: chplan.FnLength, Args: []chplan.Expr{list}}, guard),
	}}
}
