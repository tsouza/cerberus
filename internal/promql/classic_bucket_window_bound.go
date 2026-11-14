package promql

import (
	"slices"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// classic_bucket_window_bound.go bounds the instant classic-histogram
// per-series window stage (classicBucketWindowStage) — the Aggregate that
// collects each series' in-window samples and the fold that reduces them to
// one bucket ladder per series — before that stage reads a row.
//
// # What the stage costs
//
// The stage's memory has three parts, and the guard estimates each from a
// pre-count over the filtered scan:
//
//   - every series it groups: the group key and aggregate states, the row
//     the stage emits for it, and that row held by the bucket-merge guard
//     (classicBucketMergeInputGate), which reads every per-series row before
//     it can count them;
//   - every entry of those per-series ladders: one per distinct finite
//     bound of the series plus its +Inf rung, held in the same places;
//   - every further bucket entry the scan feeds the stage: the samples past
//     the first in each series, which the stage collects and folds and then
//     discards;
//   - the fold's working set: proportional to the entries of the series it
//     is folding, on each thread folding one at once.
//
// The pre-count reads the scan once more, through a scalar subquery over a
// copy of the filtered scan: sum(length(ExplicitBounds) + 1) — the scanned
// entries E, read from the ExplicitBounds.size0 subcolumn — count() — the
// scanned rows R — and an approximate count of the distinct series N, read
// from the columns the stage's group key is built from. The per-series
// ladder entries are estimated as N × E / R: the series count times the
// mean entries a scanned row carries, which is exact when every series
// keeps one layout across the window. The fold is charged at the mean series
// size E / N; a series far larger than the mean is held to the per-series
// ceiling instead, which the stage checks on each series' own count before
// folding it (classicBucketWindowSeriesGuard) and which is sized so that
// one such series on every folding thread fits in the share of the cap the
// query-wide ceiling leaves free.
//
// # Ordering
//
// The query-wide check is a conjunct of the filter the stage reads through,
// and ClickHouse evaluates its scalar subquery before that filter passes a
// block, so a refused query aborts with the check's message before the stage
// collects a sample. The per-series check reads each series' count from the
// stage's Aggregate and refuses before the fold runs on that series.
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
// Without the window checks:
//
//	series × samples                       E        26.8   26.6
//	58,000 × 14                            41.4M    689    638
//	58,000 × 28                            82.8M    775    638
//	58,000 × 56                            165.6M   840    OOM
//	58,000 × 112 (shared or disjoint;      331.4M   OOM    OOM
//	  rate, increase, sum_over_time, bare)
//	1,000,000 × 1, disjoint, sum_over_time 51.0M    OOM    OOM
//	20,000 × 62                            63.2M    740    554
//	2,000 × 620                            63.2M    749    574
//	200 × 3,100                            31.6M    586    567
//	200 × 6,200                            63.2M    OOM    661
//	120 × 9,800                            60.0M    952    575
//	1 × 7,500                              0.38M    158    -
//	1 × 30,000                             1.53M    624    -
//	1 × 60,000                             3.06M    OOM    1004
//
// The per-unit costs are fitted on 26.8, which peaks higher than 26.6 at
// every point but 58,000 × 56:
//
//   - classicBucketWindowSeriesBytes and classicBucketWindowLadderEntryBytes
//     come from single-sample disjoint seeds of 200,000 to 700,000 series at
//     10 to 200 buckets, where every scanned entry is a ladder entry and the
//     stage plus the bucket-merge guard's buffer peak at about 157 B per
//     series and 39 B per entry.
//   - classicBucketWindowScannedEntryBytes is the slope of the stage's
//     collection at few, long series, where the external GROUP BY sheds the
//     least: 60 × 9,800 to 120 × 9,800 adds 30.0M entries and 355 MiB on
//     26.8, 12.4 B per entry. Many short series cost less per entry
//     (58,000 × 14 to 58,000 × 28 adds 41.4M entries and 86 MiB).
//   - classicBucketWindowFoldEntryBytes is the fold of one long series:
//     1 × 7,500 peaks at 433 B per entry of the series, 1 × 30,000 at 428 B,
//     rounded up to 440. Two series fold at once on two threads: 2 × 6,200
//     peaks at 276 MiB, and 200 × 6,200 runs out of memory where 2,000 × 620
//     of the same E peaks at 749 MiB.
//
// With the shipped constants (estimate ceiling 805,306,368, per-series
// ceiling 305,040 entries at the 1 GiB cap), peaks on 26.8 / 26.6:
//
//	58,000 × 112, shared and disjoint, rate, increase,  refused, 18-31 MiB
//	  sum_over_time and bare (estimate 4.1G)
//	1,000,000 × 1, disjoint (estimate 2.2G)             refused, 26 / 26
//	120 × 9,800 (estimate 1.2G)                         refused, 24 / 26
//	1 × 6,000 (306,000 entries in one series)           refused, 30 / 26
//	1 × 60,000 (3,060,000 entries in one series)        refused, 169 / 166
//	58,000 × 20 (estimate 802.9M)                       answers, 757 / 639
//	350 × 3,100 (estimate 803.7M)                       answers, 649 / 569
//	145 × 5,900 (estimate 788.6M)                       answers, 812 / 582
//	20,000 × 62 and 2 × 5,900 in one query (800.7M)     answers, 676 / 557
//	2 × 5,900 (300,900 entries per series)              answers, 270 / 228
//
// 26.6.8.7, the upgrade floor, answers and refuses the same way at the same
// shapes (58,000 × 20 at 640 MiB, 145 × 5,900 at 584 MiB, every refusal
// below 28 MiB). The fold runs one series per thread, so these figures hold
// for two threads; 145 × 5,900 at max_threads 8 runs out of memory on 26.8
// (https://github.com/tsouza/cerberus/issues/4128).
const (
	// classicBucketWindowSeriesBytes is the measured cost of one series the
	// stage groups, independent of its width.
	classicBucketWindowSeriesBytes = 160

	// classicBucketWindowLadderEntryBytes is the measured cost of one entry
	// of a per-series ladder.
	classicBucketWindowLadderEntryBytes = 40

	// classicBucketWindowScannedEntryBytes is the cost charged per scanned
	// bucket entry past each series' ladder.
	classicBucketWindowScannedEntryBytes = 12

	// classicBucketWindowFoldEntryBytes is the measured working set of the
	// fold per bucket entry of the series it is folding.
	classicBucketWindowFoldEntryBytes = 440

	// classicBucketWindowFoldConcurrency is the number of series the
	// calibration server folds at once: one per ClickHouse thread of the
	// two-CPU calibration containers.
	classicBucketWindowFoldConcurrency = 2

	// classicBucketWindowCapSharePercent is the share of the query cap the
	// estimated stage may use. The rest is the headroom the per-series
	// ceiling is sized to.
	classicBucketWindowCapSharePercent = 75
)

// ClassicBucketWindowCostUnitsForMemory derives the window stage's ceiling, in
// estimated bytes, from a ClickHouse per-query memory cap:
// classicBucketWindowCapSharePercent of the cap. A non-positive cap means
// CERBERUS_CH_QUERY_MAX_MEMORY is unset and cerberus stamps no
// `max_memory_usage`; the ceiling is then the one a one-GiB cap derives, the
// shipped default, as [ClassicBucketMergeEntriesForMemory] does.
func ClassicBucketWindowCostUnitsForMemory(chQueryMaxMemory int64) int64 {
	if chQueryMaxMemory <= 0 {
		chQueryMaxMemory = bytesPerGiB
	}
	return (chQueryMaxMemory/percentDenominator)*classicBucketWindowCapSharePercent +
		(chQueryMaxMemory%percentDenominator)*classicBucketWindowCapSharePercent/percentDenominator
}

// classicBucketRowEntryCountExpr is one classic-histogram row's bucket entries:
// its finite bounds plus the +Inf rung its trailing count carries. Both the
// window guard and the bucket-merge guard count in this unit.
func classicBucketRowEntryCountExpr(s schema.Metrics) chplan.Expr {
	return addExpr(
		&chplan.FuncCall{Fn: chplan.FnLength, Args: []chplan.Expr{&chplan.ColumnRef{Name: s.ExplicitBoundsColumn}}},
		&chplan.LitInt{V: 1},
	)
}

// classicBucketWindowIdentityColumns are the columns the window stage's
// group key (histogramIdentityExpr) reads, sorted. Two rows that agree on
// all of them land in one group, so their distinct count bounds the stage's
// group count from above.
func classicBucketWindowIdentityColumns(s schema.Metrics) []chplan.Expr {
	var names []string
	chplan.InspectExpr(histogramIdentityExpr(s), func(e chplan.Expr) bool {
		if c, ok := e.(*chplan.ColumnRef); ok && !slices.Contains(names, c.Name) {
			names = append(names, c.Name)
		}
		return true
	})
	slices.Sort(names)
	cols := make([]chplan.Expr, len(names))
	for i, n := range names {
		cols[i] = &chplan.ColumnRef{Name: n}
	}
	return cols
}

// Aliases of the pre-count's aggregates and of its one projected column.
const (
	classicBucketWindowEntriesAlias = "_hq_window_scan_entries"
	classicBucketWindowRowsAlias    = "_hq_window_scan_rows"
	classicBucketWindowSeriesAlias  = "_hq_window_scan_series"
	classicBucketWindowCostAlias    = "_hq_window_scan_cost"
)

// classicBucketWindowCostExpr is the pre-count: a scalar subquery over a
// copy of input — the filtered scan the window stage reads — answering the
// stage's estimated bytes:
//
//	seriesBytes × N + ladderEntryBytes × M + scannedEntryBytes × max(E − M, 0)
//	  + foldEntryBytes × foldConcurrency × E / max(N, 1)
//	M = N × E / max(R, 1)
//
// A no-GROUP-BY aggregate answers exactly one row, an empty scan included
// (E = R = N = 0, so the estimate is 0), which is the one-row contract
// [chplan.ScalarSubquery] requires. CloneNode keeps the copy from aliasing
// the stage's own input under an in-place plan rewrite.
func classicBucketWindowCostExpr(input chplan.Node, s schema.Metrics) chplan.Expr {
	agg := &chplan.Aggregate{
		Roles: metricRoles(s),
		Input: chplan.CloneNode(input),
		AggFuncs: []chplan.AggFunc{
			{Fn: chplan.FnSum, Args: []chplan.Expr{classicBucketRowEntryCountExpr(s)}, Alias: classicBucketWindowEntriesAlias},
			{Fn: chplan.FnCount, Alias: classicBucketWindowRowsAlias},
			{Fn: chplan.FnUniqCombined64, Args: classicBucketWindowIdentityColumns(s), Alias: classicBucketWindowSeriesAlias},
		},
	}
	entries := &chplan.ColumnRef{Name: classicBucketWindowEntriesAlias}
	series := &chplan.ColumnRef{Name: classicBucketWindowSeriesAlias}
	rows := &chplan.FuncCall{Fn: chplan.FnGreatest, Args: []chplan.Expr{
		&chplan.ColumnRef{Name: classicBucketWindowRowsAlias}, &chplan.LitInt{V: 1},
	}}
	ladder := &chplan.Binary{Op: chplan.OpDiv, Left: &chplan.Binary{Op: chplan.OpMul, Left: series, Right: entries}, Right: rows}
	beyondLadder := &chplan.FuncCall{Fn: chplan.FnGreatest, Args: []chplan.Expr{subExpr(entries, ladder), &chplan.LitInt{V: 0}}}
	meanSeriesEntries := &chplan.Binary{Op: chplan.OpDiv, Left: entries, Right: &chplan.FuncCall{Fn: chplan.FnGreatest, Args: []chplan.Expr{
		series, &chplan.LitInt{V: 1},
	}}}
	cost := addExpr(
		addExpr(
			&chplan.Binary{Op: chplan.OpMul, Left: &chplan.LitInt{V: classicBucketWindowSeriesBytes}, Right: series},
			&chplan.Binary{Op: chplan.OpMul, Left: &chplan.LitInt{V: classicBucketWindowLadderEntryBytes}, Right: ladder},
		),
		addExpr(
			&chplan.Binary{Op: chplan.OpMul, Left: &chplan.LitInt{V: classicBucketWindowScannedEntryBytes}, Right: beyondLadder},
			&chplan.Binary{Op: chplan.OpMul, Left: &chplan.LitInt{
				V: classicBucketWindowFoldEntryBytes * classicBucketWindowFoldConcurrency,
			}, Right: meanSeriesEntries},
		),
	)
	return &chplan.ScalarSubquery{Input: &chplan.Project{
		Roles:       metricRoles(s),
		Input:       agg,
		Projections: []chplan.Projection{{Expr: cost, Alias: classicBucketWindowCostAlias}},
	}}
}

// classicBucketWindowInputGuard returns input under a filter that refuses
// the query when the pre-count's estimate exceeds maxCost:
//
//	throwIf(<classicBucketWindowCostExpr> > maxCost, message) = 0
//
// maxCost is the caller-resolved ceiling
// ([ResourceBounds.ClassicBucketWindowMaxCostUnits]); like every other
// [ResourceBounds] field a zero ceiling refuses rather than admits.
func classicBucketWindowInputGuard(input chplan.Node, s schema.Metrics, maxCost int64) chplan.Node {
	return &chplan.Filter{
		Input: input,
		Predicate: &chplan.Binary{
			Op: chplan.OpEq,
			Left: &chplan.FuncCall{
				Fn: chplan.FnThrowIf,
				Args: []chplan.Expr{
					gtLit(classicBucketWindowCostExpr(input, s), maxCost),
					&chplan.InlineString{V: chplan.ClassicBucketWindowBudgetMessage},
				},
			},
			Right: &chplan.LitInt{V: 0},
		},
	}
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
// fold, on every concurrently folding thread at once, fits in the headroom
// [ClassicBucketWindowCostUnitsForMemory] leaves below the cap. The query-wide
// estimate charges the fold at the mean series size; this ceiling is what
// keeps a few series far larger than the mean inside that headroom. A
// non-positive cap derives the one-GiB ceiling.
func ClassicBucketWindowSeriesEntriesForMemory(chQueryMaxMemory int64) int64 {
	if chQueryMaxMemory <= 0 {
		chQueryMaxMemory = bytesPerGiB
	}
	headroom := chQueryMaxMemory - ClassicBucketWindowCostUnitsForMemory(chQueryMaxMemory)
	return headroom / (classicBucketWindowFoldEntryBytes * classicBucketWindowFoldConcurrency)
}
