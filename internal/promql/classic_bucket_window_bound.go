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
// the series it is folding, so one long series can exhaust the cap on its
// own while the rest of the query is small. For a series of many samples the
// ceiling refuses only where one series of that many entries runs out of
// memory on every run without the check, on both builds and at every thread
// count; a one-sample series millions of bounds wide is the one measured
// exception (# The per-series ceiling).
//
// No check bounds the stage as a whole. A query whose stage exceeds the
// ClickHouse per-query memory cap across many series fails when ClickHouse
// reaches the cap (code 241); internal/engine recognises that the failing
// statement carries a classic-histogram quantile by its
// chplan.HistogramQuantile node, and the Prometheus head answers it with its
// own HTTP 422 message.
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
//	58,000 × 112 (shared or disjoint;      331.3M   OOM    OOM
//	  rate, increase, sum_over_time, bare)
//	1,000,000 × 1, disjoint, sum_over_time 51.0M    OOM    OOM
//	20,000 × 62                            63.2M    740    554
//	2,000 × 620                            63.2M    749    574
//	200 × 3,100                            31.6M    586    567
//	200 × 6,200                            63.2M    OOM    661
//	  (single-read statement, answered 5 of 5 runs on each build:
//	   871-1,004 MiB on 26.8, 601-690 MiB on 26.6)
//	60 × 9,800                             30.0M    597    574
//	120 × 9,800                            60.0M    952    575
//
// Many short series under the shipped constants, measured at max_threads 2
// (the thread count of the 2-CPU containers) with every sample of a series
// inside the [5m] range: the peak range over the runs in parentheses, OOM
// where every run ended in code 241, or band where some runs answered and the
// others ended in code 241 at the cap.
//
//	series × samples   E        26.8                 26.6
//	58,000 × 28        82.8M    683-822 (4 runs)     638-642 (4 runs)
//	58,000 × 30        88.7M    684-798 (4 runs)     638-643 (4 runs)
//	58,000 × 56        165.6M   765-836 (6 runs)     band (24 runs)
//	  (26.6: answered 22 of 24 runs at 709-986 MiB, and 2 ended in code 241
//	   at the cap; 22 of these runs and 4 of the 26.8 ones in the 8 GiB
//	   containers of test/chserver. The peak is in the window fold after
//	   the GROUP BY spills; #4340)
//	58,000 × 72        213.0M   961-1,011 (2 runs)   839-853 (2 runs)
//	58,000 × 84        248.5M   OOM (2 runs)         922-952 (2 runs)
//	58,000 × 96        284.0M   OOM (8 runs)         OOM (8 runs)
//
// Many series fold on several threads at once, so their peak grows with the
// thread count: 145 × 5,900, which peaks at 784 MiB on two threads, runs out
// of memory at max_threads 8 on 26.8. No ceiling bounds them; the cap does.
//
// # The per-series ceiling
//
// One series folds on one thread, so its peak does not depend on the thread
// count. One 50-bucket series (51 entries per sample) under `rate`, without
// the per-series check, at max_threads 2 (the 2-CPU containers' auto(2)) and
// 16 (a users.d default profile): answered runs over all runs, and the peak
// range of the answered runs in MiB. Each shape gives the same outcome at
// both thread counts, and its peaks at the two agree to within 1%. In one
// data part:
//
//	samples  entries    26.8                 26.6
//	30,000   1,530,000  10/10 (651-653)      10/10 (536-555)
//	45,000   2,295,000  0/20                 20/20 (936-976)
//	60,000   3,060,000  0/18                 1/19 (1,004)
//	74,900   3,819,900  0/6                  0/6
//	75,000   3,825,000  0/16                 0/16
//
// Spread over 16 data parts, with merges stopped, so ClickHouse reads the
// series on several threads (4 at auto(2), 7 at max_threads 16), 3 runs at
// each thread count:
//
//	samples  entries    26.8                 26.6
//	30,000   1,530,000  6/6 (623-646)        6/6 (504-523)
//	45,000   2,295,000  0/6                  6/6 (871-878)
//	60,000   3,060,000  0/6                  6/6 (1,002-1,009)
//	74,900   3,819,900  0/6                  0/6
//
// The largest series measured to answer at all is 60,000 samples, on 26.6
// (once in one part, on every run in sixteen); 26.8 answers none from
// 45,000. The ceiling is set so that 75,000
// samples, which runs out of memory on every run on both builds at both
// thread counts, is the first series refused: 74,924 samples is the largest
// admitted, 0.1% below 75,000 and 25% above 60,000. An admitted series that
// does not fit (74,900 samples runs out of memory on every run as well)
// reaches the server, which fails it at the cap with code 241.
//
// The ceiling counts entries, measured at 51 per sample. One series of a
// single sample with millions of bounds folds in less memory per entry on
// 26.6: under `sum_over_time`, 3,000,000 and 3,825,000 entries answer there
// at 862 and 969 MiB and 4,500,000 runs out of memory (2 runs each), while
// all three run out of memory on 26.8. On 26.6 the ceiling therefore refuses
// a one-row series of about 3.82M to under 4.5M entries that would answer;
// the bucket-merge ceiling it replaces refused every one of them as well.

// classicBucketWindowSeriesCapBytesPerEntry is the per-query cap, in bytes,
// per bucket entry one series' window may collect: the cap over the entries
// of the series set to be the first refused (75,000 samples x 51 entries =
// 3,825,000 at 1 GiB is 280.7 bytes per entry), rounded up so that series is
// refused.
const classicBucketWindowSeriesCapBytesPerEntry = 281

// classicBucketRowEntryCountExpr is one classic-histogram row's bucket entries:
// its finite bounds plus the +Inf rung its trailing count carries. The
// per-series window guard counts in this unit.
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
// ceiling from a ClickHouse per-query memory cap: the cap over
// classicBucketWindowSeriesCapBytesPerEntry, rounded down, so 3,821,145
// entries at CERBERUS_CH_QUERY_MAX_MEMORY's 1 GiB default, linear in the cap.
// One series folds on one thread, so the ceiling does not depend on the
// server's thread count. A non-positive cap means cerberus stamps no
// `max_memory_usage`, and the ceiling is the one the 1 GiB default derives,
// as [ExpHistogramWindowCostUnitsForMemory] does for its own ceiling.
func ClassicBucketWindowSeriesEntriesForMemory(chQueryMaxMemory int64) int64 {
	if chQueryMaxMemory <= 0 {
		chQueryMaxMemory = bytesPerGiB
	}
	return chQueryMaxMemory / classicBucketWindowSeriesCapBytesPerEntry
}
