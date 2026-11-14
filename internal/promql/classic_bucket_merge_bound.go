package promql

import (
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// classic_bucket_merge_bound.go bounds the classic-histogram cross-series
// bucket merge — the Aggregate that classicBucketMergeShaping or
// classicBucketMergeShapingSumMap builds, and classicBucketShaping.reshape
// above it — by the number of bucket entries one query's merge reads.
//
// # What the merge costs
//
// Both merge constructions, classic_bucket_bound_runs.go's sort-and-split
// fold and classic_bucket_merge_summap.go's sumMap, are linear in the bucket
// entries they read: every per-series row's finite bounds plus its +Inf
// rung. The guard counts exactly that, sum(length(ExplicitBounds) + 1), over
// every row the merge Aggregate reads — across every merge group and, on the
// range path, every step — because the merge's memory is held per query:
// the Aggregate keeps every group's collected rows until it finishes, and
// ClickHouse evaluates the merge over blocks holding many groups at once.
// Six `grp` groups of 40,000 disjoint 50-bucket series (2.04M entries each,
// 12.24M in all) run out of the 1 GiB cap inside the merge on 26.8 under a
// per-group count of the same ceiling. The count needs no model of the
// layout: disjoint, shared and overlapping layouts cost the same per entry.
//
// # Ordering
//
// The guard runs before the merge Aggregate reads a row, for both merge
// constructions; see classicBucketMergeInputGate.
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
// width 250 matches width 50 at equal entries — and grows in steps (buffer
// growth doubles). Small shapes carry a fixed overhead, so their ratio of
// peak to entries is higher (368.8 B per entry on 26.8 at 25,000 x 50,
// 312.4 B on 26.6) while their absolute peak is low. At the largest shapes
// the ratio settles near 270 B per entry on 26.8 — 269.4 B at 200,000 x 50
// and 269.8 B at 20,000 x 250 — which is classicBucketMergeBytesPerEntry.
// It is a ratio of peak to entries, not a slope: the increment between
// 100,000 x 50 and 200,000 x 50 is 282.5 B per added entry on 26.8. 26.6
// sits below 26.8 at every point.
//
// The ceiling is checked directly rather than inferred from the slope, with
// the default guard under the 1 GiB cap: 58,000 x 50 (2,958,000 entries,
// one group) completes at 703.9 MiB (26.8) and 603.8 MiB (26.6) through the
// groupArray merge and at 747.0 / 746.4 MiB through the sumMap merge; the
// same entries split over six groups complete at 283.9 / 269.9 MiB, so one
// group is the worst case. 60,000 x 50 (3,060,000 entries) is refused with
// the guard's message at 128.3 / 128.0 MiB, before the merge ran.
//
// The per-series window stage that feeds the merge (classicBucketWindowStage)
// runs before this guard and is not bounded by it. Its memory grows with
// the samples the query scans: `rate` over 58,000 series with one shared
// 50-bucket layout and 56 samples each — 2,958,000 merge entries, admitted —
// peaks at 851.1 MiB (26.8) and 767.6 MiB (26.6) under the 1 GiB cap. On the
// instant path no other bound covers that stage: CERBERUS_QUERY_MAX_SAMPLES
// bounds the result cerberus drains, not what ClickHouse scans. See
// https://github.com/tsouza/cerberus/issues/4084.
const (
	// classicBucketMergeBytesPerEntry is the merge's peak memory per bucket
	// entry at the largest calibrated shapes on 26.8: 2620.9 MiB over
	// 10,200,000 entries at 200,000 x 50 (269.4 B) and 1291.6 MiB over
	// 5,020,000 at 20,000 x 250 (269.8 B), rounded to 270. The marginal cost
	// between 100,000 x 50 and 200,000 x 50 is higher, 282.5 B per entry,
	// which is why the ceiling is checked directly at its value (this
	// file's header) rather than trusted from either figure.
	classicBucketMergeBytesPerEntry = 270

	// classicBucketMergeQueryMemoryBytes is the per-query memory cap the
	// ceiling is sized against: CERBERUS_CH_QUERY_MAX_MEMORY's 1 GiB
	// default.
	classicBucketMergeQueryMemoryBytes = 1 << 30

	// classicBucketMergeCapSharePercent is the share of the query cap the
	// merge may use at the ceiling. The rest is headroom for the step the
	// peak takes between calibration points (75,000 x 50 peaks at 733 MiB,
	// 100,000 x 50 at 1247 MiB on 26.8) and for the scan feeding the merge.
	// It does not bound the per-series window stage, which runs before the
	// guard and can by itself exceed the cap (this file's header and
	// https://github.com/tsouza/cerberus/issues/4084).
	classicBucketMergeCapSharePercent = 75

	// percentDenominator turns classicBucketMergeCapSharePercent into a
	// fraction.
	percentDenominator = 100

	// maxClassicBucketMergeEntries bounds the bucket entries one query's
	// classic-histogram cross-series merge may read: the sum, over every
	// row the merge Aggregate reads, of that row's finite bound count plus
	// its +Inf rung. It is classicBucketMergeCapSharePercent of the 1 GiB
	// cap at classicBucketMergeBytesPerEntry: 2,982,616 entries.
	//
	// It is the compile-time default of
	// [ResourceBounds.ClassicBucketMergeMaxEntries], which an operator
	// overrides through [EnvClassicBucketMergeMaxEntries]
	// (CERBERUS_PROMQL_CLASSIC_BUCKET_MERGE_MAX_ENTRIES) — see
	// resource_bounds_env.go. The guard checks the resolved value.
	maxClassicBucketMergeEntries = classicBucketMergeQueryMemoryBytes * classicBucketMergeCapSharePercent /
		percentDenominator / classicBucketMergeBytesPerEntry
)

// classicBucketMergeInputGate returns perSeries — the per-series stage the
// merge Aggregate reads — with its ExplicitBounds and BucketCounts columns
// replaced in place by copies computed through the guard:
//
//	arraySlice(col, 1, length(col) + throwIf(total > maxEntries, message))
//	total = sum(length(ExplicitBounds) + 1) OVER ()
//
// total is the query's merged-entry count: every per-series row's finite
// bounds plus the +Inf rung its trailing count carries, summed over every
// row the merge will read — across every merge group and, on the range
// path, every step. The merge's memory is held per query, not per group:
// ClickHouse evaluates the merge over blocks of groups and keeps every
// group's collected rows until the aggregation finishes, so a per-group
// bound admits a query whose groups each sit under the ceiling while their
// sum does not.
//
// throwIf answers 0 within the ceiling, so each column is unchanged; past
// it the query aborts with the guard's message. The window function cannot
// answer before it has read every row of its input, and the merge
// Aggregate cannot read a row without these columns, so the guard runs
// after the per-series stage and before the Aggregate collects or merges
// anything. This holds for both merge constructions: the groupArray merge
// in the Projects above the Aggregate and the sumMap merge inside it. The
// ordering is guaranteed by that data dependency rather than observed in a
// plan, which is why the guard is not a Filter: a Filter beneath the merge
// leaves the order of the predicate and the merge to the server.
//
// maxEntries is the caller-resolved ceiling
// ([ResourceBounds.ClassicBucketMergeMaxEntries]); it is threaded in rather
// than read from the environment here because this package may not depend
// on internal/config (.go-arch-lint.yml).
func classicBucketMergeInputGate(perSeries chplan.Node, s schema.Metrics, maxEntries int64) chplan.Node {
	total := &chplan.WindowExpr{Fn: chplan.FnSum, Args: []chplan.Expr{addExpr(
		&chplan.FuncCall{Fn: chplan.FnLength, Args: []chplan.Expr{&chplan.ColumnRef{Name: s.ExplicitBoundsColumn}}},
		&chplan.LitInt{V: 1},
	)}}
	gated := func(column string) chplan.Projection {
		col := &chplan.ColumnRef{Name: column}
		guard := &chplan.FuncCall{
			Fn: chplan.FnThrowIf,
			Args: []chplan.Expr{
				gtLit(total, maxEntries),
				&chplan.InlineString{V: chplan.ClassicBucketMergeBudgetMessage},
			},
		}
		return chplan.Projection{
			Expr: &chplan.FuncCall{Fn: chplan.FnArraySlice, Args: []chplan.Expr{
				col,
				&chplan.LitInt{V: firstPosition},
				addExpr(&chplan.FuncCall{Fn: chplan.FnLength, Args: []chplan.Expr{col}}, guard),
			}},
			Alias: column,
		}
	}
	return &chplan.Project{
		Input:        perSeries,
		Replacements: []chplan.Projection{gated(s.ExplicitBoundsColumn), gated(s.BucketCountsColumn)},
	}
}
