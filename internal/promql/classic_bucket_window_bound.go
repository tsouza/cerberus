package promql

import (
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// classic_bucket_window_bound.go bounds one series of the instant
// classic-histogram per-series window stage (classicBucketWindowStage) — the
// Aggregate that collects each series' in-window samples and the fold that
// reduces them to one bucket ladder per series.
//
// # The check
//
// The per-series check (classicBucketWindowSeriesGuard) counts each series'
// collected bucket entries on the stage's Aggregate — every in-window row's
// finite bounds plus its +Inf rung — and refuses the query when one series
// holds more than its ceiling, after the stage has collected that series and
// before the fold reads it. The fold's working set grows with the entries of
// the series it is folding, on each thread folding one at once, so one long
// series can exhaust the cap on its own while the rest of the query is
// small. The ceiling is sized so the fold of the largest admitted series, on
// every folding thread at once, uses at most
// classicBucketWindowFoldHeadroomPercent of the part of the cap
// classicBucketWindowStageSharePercent leaves free.
//
// No check bounds the stage as a whole. A query whose stage exceeds the
// ClickHouse per-query memory cap across many series fails when ClickHouse
// reaches the cap (code 241); internal/engine recognises that the failing
// statement carries a classic-histogram stage by this check's and the
// bucket-merge check's throwIf messages, and the Prometheus head answers it
// with its own HTTP 422 message.
//
// # Calibration
//
// Measured on clickhouse/clickhouse-server 26.8.36.3 and 26.6.1.1193, each in
// a container of 2 CPUs and 6 GiB, through the production Prometheus handler
// under the 1 GiB CERBERUS_CH_QUERY_MAX_MEMORY default, with the settings the
// handler stamps (external GROUP BY at 512 MiB). The query is
// `histogram_quantile(0.5, sum by (le) (rate(<bucket>[5m])))` at one instant
// over series of one shared 50-bucket layout unless noted; each cell is the
// peak memory_usage system.query_log records, in MiB, or OOM where the
// server refused the query with code 241 at the cap. E is the scanned bucket
// entries.
//
// Without the per-series check:
//
//	series × samples                       E        26.8   26.6
//	58,000 × 14                            41.4M    689    638
//	58,000 × 28                            82.8M    775    638
//	58,000 × 56                            165.6M   840    OOM
//	58,000 × 112 (shared or disjoint;      331.3M   OOM    OOM
//	  rate, increase, sum_over_time, bare)
//	1,000,000 × 1, disjoint, sum_over_time 51.0M    OOM    OOM
//	20,000 × 62                            63.2M    740    554
//	2,000 × 620                            63.2M    749    574
//	200 × 3,100                            31.6M    586    567
//	200 × 6,200                            63.2M    OOM    661
//	60 × 9,800                             30.0M    597    574
//	120 × 9,800                            60.0M    952    575
//	1 × 7,500                              0.38M    158    -
//	1 × 30,000                             1.53M    624    -
//	1 × 60,000                             3.06M    OOM    1004
//
// classicBucketWindowFoldEntryBytes is the fold of one long series, fitted
// on 26.8, which peaks higher than 26.6 at these shapes: 1 × 7,500 peaks at
// 433 B per entry of the series, 1 × 30,000 at 428 B, rounded up to 440. Two
// series fold at once on two threads: 2 × 6,200 peaks at 276 MiB, and
// 200 × 6,200 runs out of memory where 2,000 × 620 of the same E peaks at
// 749 MiB.
//
// With the shipped constants (per-series ceiling 152,520 entries at the
// 1 GiB cap), peaks on 26.8 / 26.6:
//
//	1 × 3,000 (153,000 entries in one series)           refused, 20 / 16
//	1 × 60,000 (3,060,000 entries in one series)        refused, 169 / 166
//	2 × 2,950 (150,450 entries per series)              answers, 111 / 117
//
// The fold runs one series per thread, so these figures hold for two
// threads: 145 × 5,900, which peaks at 784 MiB on two threads, runs out of
// memory at max_threads 8 on 26.8
// (https://github.com/tsouza/cerberus/issues/4128).
const (
	// classicBucketWindowFoldEntryBytes is the measured working set of the
	// fold per bucket entry of the series it is folding.
	classicBucketWindowFoldEntryBytes = 440

	// classicBucketWindowFoldConcurrency is the number of series the
	// calibration server folds at once: one per ClickHouse thread of the
	// two-CPU calibration containers.
	classicBucketWindowFoldConcurrency = 2

	// classicBucketWindowStageSharePercent is the share of the query cap
	// set aside for the rest of the window stage — the series it groups and
	// the bucket entries it collects. The remainder of the cap is the
	// headroom the fold of the largest admitted series is sized against.
	classicBucketWindowStageSharePercent = 75

	// classicBucketWindowFoldHeadroomPercent is the share of that headroom
	// the fold of the largest series the per-series ceiling admits may use,
	// on every folding thread at once.
	classicBucketWindowFoldHeadroomPercent = 50
)

// classicBucketRowEntryCountExpr is one classic-histogram row's bucket entries:
// its finite bounds plus the +Inf rung its trailing count carries. Both the
// window guard and the bucket-merge guard count in this unit.
func classicBucketRowEntryCountExpr(s schema.Metrics) chplan.Expr {
	return addExpr(
		&chplan.FuncCall{Fn: chplan.FnLength, Args: []chplan.Expr{&chplan.ColumnRef{Name: s.ExplicitBoundsColumn}}},
		&chplan.LitInt{V: 1},
	)
}

// classicBucketWindowSeriesEntriesAlias names the per-series stage's count
// of the bucket entries one series' window collects.
const classicBucketWindowSeriesEntriesAlias = "_hq_window_series_entries"

// classicBucketWindowSeriesEntriesAgg counts the bucket entries one
// series' window collects — every in-window row's finite bounds plus its
// +Inf rung, the entries the fold flattens for that series.
func classicBucketWindowSeriesEntriesAgg(s schema.Metrics) chplan.AggFunc {
	return chplan.AggFunc{
		Fn:    chplan.FnSum,
		Args:  []chplan.Expr{classicBucketRowEntryCountExpr(s)},
		Alias: classicBucketWindowSeriesEntriesAlias,
	}
}

// classicBucketWindowSeriesGuard returns group — the per-series Aggregate,
// which carries [classicBucketWindowSeriesEntriesAgg] — under a filter that
// refuses the query when one series' window collects more than maxEntries
// bucket entries:
//
//	throwIf(_hq_window_series_entries > maxEntries, message) = 0
//
// It sits directly on the Aggregate and below the fold, so a series it
// refuses is never folded.
func classicBucketWindowSeriesGuard(group chplan.Node, maxEntries int64) chplan.Node {
	return &chplan.Filter{
		Input: group,
		Predicate: &chplan.Binary{
			Op: chplan.OpEq,
			Left: &chplan.FuncCall{
				Fn: chplan.FnThrowIf,
				Args: []chplan.Expr{
					gtLit(&chplan.ColumnRef{Name: classicBucketWindowSeriesEntriesAlias}, maxEntries),
					&chplan.InlineString{V: chplan.ClassicBucketWindowSeriesBudgetMessage},
				},
			},
			Right: &chplan.LitInt{V: 0},
		},
	}
}

// ClassicBucketWindowSeriesEntriesForMemory derives the per-series window
// ceiling from a ClickHouse per-query memory cap: the largest series whose
// fold, on every concurrently folding thread at once, fits in
// classicBucketWindowFoldHeadroomPercent of the cap left once
// classicBucketWindowStageSharePercent of it is set aside. A non-positive cap
// derives the one-GiB ceiling, the shipped default, as
// [ClassicBucketMergeEntriesForMemory] does.
func ClassicBucketWindowSeriesEntriesForMemory(chQueryMaxMemory int64) int64 {
	if chQueryMaxMemory <= 0 {
		chQueryMaxMemory = bytesPerGiB
	}
	headroom := chQueryMaxMemory - percentOf(chQueryMaxMemory, classicBucketWindowStageSharePercent)
	return percentOf(headroom, classicBucketWindowFoldHeadroomPercent) /
		(classicBucketWindowFoldEntryBytes * classicBucketWindowFoldConcurrency)
}
