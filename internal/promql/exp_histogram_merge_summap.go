package promql

import (
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// exp_histogram_merge_summap.go implements cerberus issue #2757's two-pass
// sumMap-keyed alternative to expHistogramGroupMergeFanout's groupArray +
// per-target-bucket arrayReduce/arraySlice picker fold — the same
// cross-series bucket-merge fan-out histogram_merge_bound.go's own audited
// cost finding identifies (`rows x width^2`), applied to the sibling
// exponential-histogram path issue #2756 / PR #2826 fixed for classic
// buckets.
//
// # Scope
//
// Every INSTANT shape is eligible: `sum(<native-histogram selector>)` or
// its `avg()` twin (cerberus issue #2866 widened the original SUM-only v1,
// #2757, to include AVG), with or without a `by(...)`/`without(...)`
// clause (cerberus issue #2865 widened v1's original single-group-only
// restriction — see "Multi-group mergedScale" below). Range/query_range
// mode still stays on the existing fold unconditionally; see
// NativeExpHistogramMergeLowerer for why — the identical per-anchor
// mergedScale problem multi-group solves for by()/without() applies to
// range mode too, and cerberus issue #2865's own follow-up
// (filed at that issue's completion) tracks wiring it onto this SAME
// mechanism.
//
// # The two-pass restructure (issue #2757's own "known hard part",
// # verified here)
//
// sumMap needs each row's OWN bucket-count array keyed by ITS DOWNSCALED
// absolute bucket index (bitShiftRight(offset + j - 1, rowScale -
// mergedScale)) as a per-row ARGUMENT. mergedScale is min(Scale) over the
// WHOLE group, only known once every row has been visited — and a
// ClickHouse aggregate's arguments are per-row expressions evaluated
// DURING the same GROUP BY pass that produces every aggregate's own
// result; one aggregate's finished output is not visible to a sibling
// aggregate's row-level arguments within that same pass. Feeding THIS
// Aggregate's own min(Scale) into sumMap's row args in the SAME Aggregate
// is therefore not expressible — mergedScale must come from an
// independent, EARLIER pass.
//
// # Single-group mergedScale (v1, cerberus issue #2757)
//
// For the single-group shape — `sum(<selector>)`, no by/without, every
// matching series collapsing into ONE output group
// ([histogramAggGroupBy] returns an empty GroupBy for exactly this shape)
// — expHistogramGroupMergeSumMap gets mergedScale via a
// [chplan.ScalarSubquery]: the same idiom info_fn.go's two-arm base
// already uses for "compute one group-collapsing scalar in an independent
// pass, feed it into a sibling expression". Pass 1 is a plain, no-GROUP-BY
// min(Scale) Aggregate over a CLONE of perSeries ([chplan.CloneNode] —
// required because chplan rewrites plans in place, so two arms must never
// alias the same node; see info_fn.go's own comment on its identical
// clone). Pass 2 is the real merge Aggregate, keying sumMap by each row's
// own downscaled index using that already-resolved scalar.
// [chplan.ScalarSubquery]'s contract requires its wrapped subtree to yield
// EXACTLY one row — true here, false the moment a `by(route)`-style
// grouping is present (see below) — so this mechanism stays scoped to the
// single-group shape rather than migrating onto the multi-group mechanism
// below, whose `OVER ()` degenerate case would reproduce it byte-for-byte
// anyway: keeping v1's already-shipped SQL untouched avoids churning its
// existing goldens for a change with no behavioural difference.
//
// # Multi-group mergedScale (cerberus issue #2865)
//
// A `sum by(route)(<selector>)` query needs a DIFFERENT mergedScale PER
// OUTPUT GROUP — [chplan.ScalarSubquery] cannot express that (it always
// resolves to a single scalar). Issue #2834 originally scoped this as
// needing a real per-group JOIN back onto perSeries — no PromQL lowering
// in this codebase built that shape (internal/promql has no chplan.Join
// usage at all; the existing VectorJoin / HistogramVectorJoin family
// models PromQL BINARY-OPERATOR vector matching, not a generic
// aggregate-then-rejoin-by-group idiom). Issue #2865's own scoping session
// found a materially simpler alternative that avoids a JOIN entirely: a
// [chplan.WindowExpr] `min(Scale) OVER (PARTITION BY <group-key>)`
// evaluated in an EARLIER, non-aggregating Project stage over perSeries
// (expHistogramMergeScaleWindowProject) annotates every row with ITS OWN
// group's mergedScale, without collapsing any rows and without ever
// emitting a JOIN — the outer, grouped Aggregate then reads that windowed
// column as a plain per-row value inside sumMap's row args, exactly like
// ScalarSubquery's resolved scalar does for the single-group case. An
// empty PARTITION BY list degenerates to ScalarSubquery's own single-scalar
// behavior, verified against real ClickHouse 26.6 in that scoping session
// (issue #2865's own comment thread has the SQL and the hand-verified
// numeric result) — which is why this file does not migrate the
// single-group path onto it (see above): the two mechanisms already agree
// on that shape, so there is nothing to gain from unifying them beyond
// unnecessary golden churn.
//
// # Cost model, and the rows-independent budget guard (cerberus issue #2834)
//
// The old picker-based fold costs `rows x (posWidth^2 + negWidth^2)`
// (histogram_merge_bound.go's calibration). This design's dominant cost is
// `rows x width` — sumMap's own keyed pass is genuinely linear — plus a
// width-only reconstruction term (expHistogramSumMapLadderExpr, below)
// that does NOT shrink as rows grow, and in the worst case (every merged
// bucket populated) is itself quadratic in width alone; this design also
// keeps collecting every row's raw groupArrays (needed by the guard, see
// exp_histogram_merge_summap_bound.go), so it never beats the fold by more
// than that fixed per-row overhead saves. Real ClickHouse 26.6 measurements
// taken for issue #2757, against the ACTUAL emitted SQL (not a
// hand-written approximation), at realistic OTel-SDK-default width (~160
// buckets): roughly PARITY at a single series (3.4 MiB fold vs 3.7 MiB
// this design), rising to a real 13x win at 100 series, 34x at 1,000, and
// 43x at 3,741 (issue #2490's own repro). At a SINGLE series with an
// unusually wide individual layout (width 1,280 and up) this design costs
// MORE memory than the fold — 42.7 MiB vs 31.7 MiB at width 1,280,
// 2,604 MiB vs 1,828 MiB at width 10,240 — a real, measured regression for
// that narrow shape, though still FASTER in wall-clock time even there
// (708ms vs 994ms at width 10,240). See cerberus issue #2757 for the full
// measured table.
//
// [wrapExpHistogramMergeSumMapBudgetGuard]
// (exp_histogram_merge_summap_bound.go) bounds this design with its OWN,
// rows-independent guard, calibrated against real ClickHouse 26.6 for
// cerberus issue #2834: it rejects whenever
// `sumMapMergeCostMultiplier x (posWidth^2 + negWidth^2) > maxCostUnits`
// (no `rows x` multiplier) OR the row count exceeds
// [maxHistogramMergeRowCountOverflowGuard] — see that file's header doc
// for the full calibration table and the row-count backstop's dual role.
// Issue #2490's own 3,741-series repro at realistic OTel-default width,
// guard-rejected under the OLD `rows x (posWidth^2 + negWidth^2)` model at
// ~1.79 GiB, is comfortably ADMITTED under this guard (real measured cost
// ~42 MiB).
//
// Because [expHistogramMergeAggs] — reused verbatim below for the
// groupArray columns the guard reads — still collects every row's raw
// Scale/Offset/BucketCounts arrays, the guard's own cost computation is
// unaffected by this file: it reads the SAME columns it always has,
// still populated the SAME way, regardless of which merge shape produced
// them.
//
// # The multi-group budget guard (cerberus issue #2865)
//
// The guard above bounds each OUTPUT GROUP independently — it is a Filter
// predicate evaluated per row of the merge Aggregate's own output, one row
// per group. That is not enough once a query can produce MANY groups: a
// huge group and many small groups can carry the identical PER-GROUP cost
// (each individually admitted) while costing very different amounts of
// TOTAL work, because the WindowExpr pre-pass above must materialise
// EVERY row of perSeries — across every group at once — before the
// per-group Aggregate (and its guard) ever runs. Real ClickHouse 26.6
// measurements taken for this issue confirm the gap: at a fixed width
// (160 buckets) and a fixed TOTAL row count (10,000), 1,000 groups of 10
// rows each peaked at 87.3 MiB while 10,000 groups of 1 row each peaked at
// 312.9 MiB — a 3.6x difference the per-group guard alone cannot see,
// since every one of those groups individually costs far less than the
// per-group ceiling. [wrapExpHistogramMergeSumMapBudgetGuard]'s multiGroup
// branch (exp_histogram_merge_summap_bound.go) closes that gap with two
// ADDITIONAL, real-ClickHouse-calibrated axes — total row count and total
// distinct group count, both summed ACROSS every group via a second pair
// of WindowExprs in the SAME pre-pass Project (empty PARTITION BY, so
// every row sees the query's own total) — ORed into the existing per-group
// check. See that file's header doc for the full calibration table.
const (
	// hqAggPosSumMapAlias / hqAggNegSumMapAlias hold the group's
	// sumMap(downscaled-absolute-index, count) result for the positive /
	// negative ladder — a Tuple(Array(Int64) keys sorted ascending,
	// Array(Float64) values), with any zero-summed bucket dropped (the
	// same sumMap key-drop quirk classic_bucket_merge_summap.go's header
	// documents; expHistogramSumMapLadderExpr's indexOf-based
	// reconstruction below defaults a miss to zero the identical way
	// classicBucketSumMapLookupExpr does).
	hqAggPosSumMapAlias = "_hq_pos_summap"
	hqAggNegSumMapAlias = "_hq_neg_summap"

	// hqWinMergedScaleAlias names [expHistogramMergeScaleWindowProject]'s
	// pre-pass WindowExpr column: each perSeries row annotated with ITS OWN
	// group's mergedScale (min(Scale) OVER (PARTITION BY <group-key>)) —
	// the multi-group counterpart of expHistogramMergeScaleScalarSubquery's
	// single scalar (cerberus issue #2865).
	hqWinMergedScaleAlias = "_hq_win_merged_scale"

	// hqWinTotalRowCountAlias / hqWinTotalGroupCountAlias name the SAME
	// pre-pass Project's two multi-group-guard columns — count() OVER () /
	// uniqExact(<group-key>) OVER (), both partitioned over the WHOLE
	// perSeries input (empty PARTITION BY) so every row sees the query's
	// TOTAL row count / distinct group count, not its own group's. See
	// exp_histogram_merge_summap_bound.go for how the guard reads them.
	hqWinTotalRowCountAlias   = "_hq_win_total_row_count"
	hqWinTotalGroupCountAlias = "_hq_win_total_group_count"

	// hqAggMultiGroupTotalRowCountAlias / hqAggMultiGroupTotalGroupCountAlias
	// name pass 2's own collection of the two window columns above — max()
	// over a value that is already constant across the whole input, the
	// simplest way to carry a WindowExpr-computed whole-query scalar
	// through a GROUP BY untouched (mirrors this file's own ZeroThreshold
	// max() fold in [expHistogramMergeAggs] for the identical "pick the one
	// value every row already agrees on" shape).
	hqAggMultiGroupTotalRowCountAlias   = "_hq_agg_total_row_count"
	hqAggMultiGroupTotalGroupCountAlias = "_hq_agg_total_group_count"
)

// expHistogramMergeScaleScalarSubquery renders pass 1: a no-GROUP-BY
// min(Scale) Aggregate over a CLONE of perSeries, wrapped as a
// [chplan.ScalarSubquery]. A plain (non-GROUP-BY) ClickHouse aggregate
// always returns exactly one row — even over zero input rows (min()
// answers NULL) — satisfying ScalarSubquery's "exactly one row" contract
// without needing chplan.Aggregate.DropEmptyOnNoGroup, which exists for
// the opposite (GROUPED, zero-groups-should-mean-zero-rows) case.
//
// CloneNode is required — not optional — because chplan rewrites plans in
// place: reusing the SAME perSeries pointer both here and as pass 2's own
// Aggregate.Input would let a rewrite of one arm silently alias the
// other, exactly the hazard info_fn.go's own two-arm base clone guards
// against.
func expHistogramMergeScaleScalarSubquery(perSeries chplan.Node, s schema.Metrics) chplan.Expr {
	const scalarAlias = "_hq_pass1_merged_scale"
	agg := &chplan.Aggregate{
		Input: chplan.CloneNode(perSeries),
		AggFuncs: []chplan.AggFunc{
			{Fn: chplan.FnMin, Args: []chplan.Expr{&chplan.ColumnRef{Name: s.ScaleColumn}}, Alias: scalarAlias},
		},
	}
	return &chplan.ScalarSubquery{
		Input: &chplan.Project{
			Input:       agg,
			Projections: []chplan.Projection{{Expr: &chplan.ColumnRef{Name: scalarAlias}, Alias: scalarAlias}},
		},
	}
}

// expHistogramMergeScaleWindowPartitionBy returns the PARTITION BY
// expression list a [chplan.WindowExpr] mergedScale pre-pass needs for a
// given grouping: the group-key expressions [histogramAggGroupBy] resolves
// for by()/without(), optionally prefixed by a range-mode step anchor.
// anchor is nil at every call site wired today (instant mode only — see
// this file's header); range mode (cerberus issue #2865's tracked
// follow-up) reuses this SAME function with its own anchor column, so the
// partition-key shape is derived exactly once regardless of how many call
// sites eventually need it.
func expHistogramMergeScaleWindowPartitionBy(anchor *chplan.ColumnRef, groupBy []chplan.Expr) []chplan.Expr {
	if anchor == nil {
		return groupBy
	}
	partitionBy := make([]chplan.Expr, 0, len(groupBy)+1)
	partitionBy = append(partitionBy, anchor)
	return append(partitionBy, groupBy...)
}

// expHistogramMergeScaleWindowCols names the three per-row scalars
// [expHistogramMergeScaleWindowProject]'s pre-pass Project adds to
// perSeries: MergedScale (this row's own group's min(Scale), cerberus
// issue #2865's core mechanism — see this file's header) and, for the
// multi-group budget guard (exp_histogram_merge_summap_bound.go),
// TotalRowCount / TotalGroupCount — the SAME count() OVER () / uniqExact()
// OVER () idiom, but partitioned over the WHOLE perSeries input instead of
// per group, so every row (and therefore every pass-2 output group, once
// collected via max()) sees the query's total scale, an axis no per-group
// guard can see on its own.
type expHistogramMergeScaleWindowCols struct {
	MergedScale, TotalRowCount, TotalGroupCount chplan.Expr
}

// expHistogramMergeScaleWindowProject wraps perSeries in a
// non-aggregating Project stage that annotates every row with the three
// [expHistogramMergeScaleWindowCols] scalars, keyed by partitionBy (see
// [expHistogramMergeScaleWindowPartitionBy]). This is the WindowExpr
// pre-pass cerberus issue #2865 verified against real ClickHouse 26.6 —
// see this file's header for the mechanism and the guard's own header doc
// (exp_histogram_merge_summap_bound.go) for TotalRowCount/TotalGroupCount's
// calibration.
//
// The Project lists perSeries' columns EXPLICITLY rather than relying on
// chplan.Project's bare-`*` pass-through: chplan.Project has no "wildcard
// plus one new column" shape (only a full explicit list, or `*` with named
// REPLACEMENTS), and an explicit list is this package's own established
// idiom for exactly this kind of reshape (see e.g.
// expHistogramGroupMergeProjectionsSumMap). The list is fixed by
// [nativeExpHistValuedLatestAggs]' own output contract
// (histogram_quantile_range.go) — every column
// [expHistogramGroupMergeAggsSumMap]'s pass-2 aggregates read — so a
// column added there without being added here would silently vanish
// before pass 2 ever sees it.
func expHistogramMergeScaleWindowProject(perSeries chplan.Node, partitionBy []chplan.Expr, s schema.Metrics) (chplan.Node, expHistogramMergeScaleWindowCols) {
	passthrough := func(col string) chplan.Projection {
		return chplan.Projection{Expr: &chplan.ColumnRef{Name: col}, Alias: col}
	}
	projs := []chplan.Projection{
		passthrough(s.AttributesColumn),
		passthrough(s.CountColumn),
		passthrough(s.SumColumn),
		passthrough(s.ScaleColumn),
		passthrough(s.ZeroCountColumn),
		passthrough(s.PositiveOffsetColumn),
		passthrough(s.PositiveBucketCountsColumn),
		passthrough(s.NegativeOffsetColumn),
		passthrough(s.NegativeBucketCountsColumn),
	}
	if s.ZeroThresholdColumn != "" {
		projs = append(projs, passthrough(s.ZeroThresholdColumn))
	}
	projs = append(
		projs,
		chplan.Projection{
			Expr: &chplan.WindowExpr{
				Fn:          chplan.FnMin,
				Args:        []chplan.Expr{&chplan.ColumnRef{Name: s.ScaleColumn}},
				PartitionBy: partitionBy,
			},
			Alias: hqWinMergedScaleAlias,
		},
		chplan.Projection{
			Expr:  &chplan.WindowExpr{Fn: chplan.FnCount, Args: []chplan.Expr{}},
			Alias: hqWinTotalRowCountAlias,
		},
		chplan.Projection{
			Expr:  &chplan.WindowExpr{Fn: chplan.FnUniqExact, Args: partitionBy},
			Alias: hqWinTotalGroupCountAlias,
		},
	)
	return &chplan.Project{Input: perSeries, Projections: projs}, expHistogramMergeScaleWindowCols{
		MergedScale:     &chplan.ColumnRef{Name: hqWinMergedScaleAlias},
		TotalRowCount:   &chplan.ColumnRef{Name: hqWinTotalRowCountAlias},
		TotalGroupCount: &chplan.ColumnRef{Name: hqWinTotalGroupCountAlias},
	}
}

// expHistogramSumMapRowIndexExpr renders one row's downscaled ABSOLUTE
// bucket index array for one signed ladder — bitShiftRight(offset + j - 1,
// Scale - mergedScale) over arrayEnumerate(buckets) — the sumMap "keys"
// argument. mergedScale MUST be pass 1's already-resolved scalar (see this
// file's header for why it cannot be this same Aggregate's own min(Scale)
// alias). Mirrors expHistogramMergeOffsetExpr's identical
// bitShiftRight(off, s - mergedScale) shift with no extra integer casts —
// ClickHouse's own type promotion across Int32 offset / UInt64
// arrayEnumerate position is already exercised by that existing,
// production code path.
func expHistogramSumMapRowIndexExpr(offsetCol, bucketsCol string, mergedScale chplan.Expr, s schema.Metrics) chplan.Expr {
	const paramJ = "j"
	off := &chplan.ColumnRef{Name: offsetCol}
	buckets := &chplan.ColumnRef{Name: bucketsCol}
	scale := &chplan.ColumnRef{Name: s.ScaleColumn}
	return &chplan.FuncCall{Fn: chplan.FnArrayMap, Args: []chplan.Expr{
		&chplan.Lambda{
			Params: []string{paramJ},
			Body: &chplan.FuncCall{Fn: chplan.FnBitShiftRight, Args: []chplan.Expr{
				subExpr(addExpr(off, &chplan.BareIdent{Name: paramJ}), &chplan.LitInt{V: 1}),
				subExpr(scale, mergedScale),
			}},
		},
		&chplan.FuncCall{Fn: chplan.FnArrayEnumerate, Args: []chplan.Expr{buckets}},
	}}
}

// expHistogramSumMapRowCountsExpr casts one row's raw UInt64 bucket-count
// array to Float64 — the ladder is float-domain throughout, mirroring
// classicBucketSumMapRowArgs' identical counts cast and
// expHistogramMergeBucketsRowsSumExpr's existing toFloat64 practice for
// this exact column family.
func expHistogramSumMapRowCountsExpr(bucketsCol string) chplan.Expr {
	const paramX = "x"
	return &chplan.FuncCall{Fn: chplan.FnArrayMap, Args: []chplan.Expr{
		&chplan.Lambda{Params: []string{paramX}, Body: toFloat64Expr(&chplan.BareIdent{Name: paramX})},
		&chplan.ColumnRef{Name: bucketsCol},
	}}
}

// expHistogramGroupMergeAggsSumMap is pass 2's AggFuncs: [expHistogramMergeAggs]
// verbatim (still needed — the reused budget guard reads its groupArray
// columns, see this file's header) plus the two sumMap aggregates keyed by
// mergedScale (pass 1's resolved scalar).
func expHistogramGroupMergeAggsSumMap(mergedScale chplan.Expr, s schema.Metrics) []chplan.AggFunc {
	aggs := expHistogramMergeAggs(s)
	return append(
		aggs,
		chplan.AggFunc{
			Fn: chplan.FnSumMap,
			Args: []chplan.Expr{
				expHistogramSumMapRowIndexExpr(s.PositiveOffsetColumn, s.PositiveBucketCountsColumn, mergedScale, s),
				expHistogramSumMapRowCountsExpr(s.PositiveBucketCountsColumn),
			},
			Alias: hqAggPosSumMapAlias,
		},
		chplan.AggFunc{
			Fn: chplan.FnSumMap,
			Args: []chplan.Expr{
				expHistogramSumMapRowIndexExpr(s.NegativeOffsetColumn, s.NegativeBucketCountsColumn, mergedScale, s),
				expHistogramSumMapRowCountsExpr(s.NegativeBucketCountsColumn),
			},
			Alias: hqAggNegSumMapAlias,
		},
	)
}

// expHistogramSumMapLadderExpr reconstructs one signed ladder's merged
// (offset, bucket-counts) pair from its sumMap aggregate result: sm.1
// (ascending Int64 keys) / sm.2 (Float64 values, any zero-summed key
// dropped — see this file's header). An EMPTY sumMap result (every
// contributing row's own array on this side was empty — routine: most
// latency histograms carry no negative buckets at all) reconstructs to
// offset 0 / an empty bucket array, rather than calling arrayMin/arrayMax
// on an empty array (a ClickHouse error).
//
// The non-empty reconstruction mirrors classicBucketSumMapLookupExpr's
// indexOf-based lookup — arrayConcat([0.], sm.2)[indexOf(sm.1, u) + 1],
// defaulting a miss to 0 via the leading zero pad — walked over
// range(mergedLength) at ABSOLUTE indices [arrayMin(sm.1), arrayMax(sm.1)]
// rather than over a separately-constructed union-bounds array: unlike
// the classic ladder's `le` bounds (which need a union because different
// rows can report different ExplicitBounds), an exp-histogram bucket
// index is already a canonical absolute integer once downscaled to
// mergedScale, so sumMap's own compact key range IS the merged bucket
// range — no separate union construction is needed here at all.
func expHistogramSumMapLadderExpr(sumMapAlias string) (offset, buckets chplan.Expr) {
	const paramT = "t"
	sm := chplan.Expr(&chplan.ColumnRef{Name: sumMapAlias})
	keys := &chplan.FuncCall{Fn: chplan.FnTupleElement, Args: []chplan.Expr{sm, &chplan.LitInt{V: 1}}}
	vals := &chplan.FuncCall{Fn: chplan.FnTupleElement, Args: []chplan.Expr{sm, &chplan.LitInt{V: 2}}}
	isEmpty := &chplan.Binary{Op: chplan.OpEq, Left: &chplan.FuncCall{Fn: chplan.FnLength, Args: []chplan.Expr{keys}}, Right: &chplan.LitInt{V: 0}}

	minKey := &chplan.FuncCall{Fn: chplan.FnArrayMin, Args: []chplan.Expr{keys}}
	maxKey := &chplan.FuncCall{Fn: chplan.FnArrayMax, Args: []chplan.Expr{keys}}
	paddedVals := &chplan.FuncCall{Fn: chplan.FnArrayConcat, Args: []chplan.Expr{
		&chplan.FuncCall{Fn: chplan.FnArray, Args: []chplan.Expr{&chplan.LitFloat{V: 0}}},
		vals,
	}}
	lookupAt := func(target chplan.Expr) chplan.Expr {
		pos := addExpr(&chplan.FuncCall{Fn: chplan.FnIndexOf, Args: []chplan.Expr{keys, target}}, &chplan.LitInt{V: 1})
		return &chplan.Subscript{Container: paddedVals, Key: pos}
	}
	mergedLength := addExpr(subExpr(maxKey, minKey), &chplan.LitInt{V: 1})
	dense := &chplan.FuncCall{Fn: chplan.FnArrayMap, Args: []chplan.Expr{
		&chplan.Lambda{Params: []string{paramT}, Body: lookupAt(addExpr(minKey, &chplan.BareIdent{Name: paramT}))},
		&chplan.FuncCall{Fn: chplan.FnRange, Args: []chplan.Expr{&chplan.FuncCall{Fn: chplan.FnToUInt64, Args: []chplan.Expr{mergedLength}}}},
	}}

	offset = &chplan.FuncCall{Fn: chplan.FnIf, Args: []chplan.Expr{isEmpty, &chplan.LitInt{V: 0}, minKey}}
	buckets = &chplan.FuncCall{Fn: chplan.FnIf, Args: []chplan.Expr{isEmpty, &chplan.FuncCall{Fn: chplan.FnEmptyArrayFloat64}, dense}}
	return offset, buckets
}

// expHistogramGroupMergeProjectionsSumMap is [expHistogramMergeProjections]
// with the positive/negative offset+bucket fields replaced by
// [expHistogramSumMapLadderExpr]'s reconstruction — Scale, ZeroCount and
// ZeroThreshold are UNCHANGED (still the existing Kahan/max fold over the
// SAME groupArrays [expHistogramGroupMergeAggsSumMap] still collects for
// the reused budget guard).
func expHistogramGroupMergeProjectionsSumMap(s schema.Metrics) []chplan.Projection {
	projs := []chplan.Projection{
		{Expr: &chplan.ColumnRef{Name: hqAggMergedScaleAlias}, Alias: s.ScaleColumn},
		{Expr: promHistogramKahanSum(&chplan.ColumnRef{Name: hqMergeZeroCountsArrayAlias}), Alias: s.ZeroCountColumn},
	}
	if s.ZeroThresholdColumn != "" {
		projs = append(projs, chplan.Projection{
			Expr:  &chplan.ColumnRef{Name: s.ZeroThresholdColumn},
			Alias: s.ZeroThresholdColumn,
		})
	}
	posOffset, posBuckets := expHistogramSumMapLadderExpr(hqAggPosSumMapAlias)
	negOffset, negBuckets := expHistogramSumMapLadderExpr(hqAggNegSumMapAlias)
	return append(projs, []chplan.Projection{
		{Expr: posOffset, Alias: s.PositiveOffsetColumn},
		{Expr: posBuckets, Alias: s.PositiveBucketCountsColumn},
		{Expr: negOffset, Alias: s.NegativeOffsetColumn},
		{Expr: negBuckets, Alias: s.NegativeBucketCountsColumn},
	}...)
}

// expHistogramGroupMergeSumMap builds the full two-pass merge node for
// every eligible instant shape — SUM or AVG fold, with or without a
// by()/without() clause (cerberus issue #2865 widened the original
// single-group-only v1; see this file's header for both mergedScale
// mechanisms): pass 1 resolves mergedScale (ScalarSubquery for the
// single-group shape, the WindowExpr pre-pass for multi-group), pass 2 is
// the Aggregate (wrapped in this design's OWN budget guard,
// [wrapExpHistogramMergeSumMapBudgetGuard] — see this file's header and
// exp_histogram_merge_summap_bound.go for the calibration behind it), and
// the reshape Project. No [expHistogramMergeSortStage] call: that stage
// exists only to give the OLD Kahan-compensated bucket fold a
// deterministic per-series summation order (cerberus issue #2254);
// sumMap's own per-key grouping is order-invariant, so this path has
// nothing to resort.
//
// Count / Sum stay on the existing groupArray + Kahan fold, byte-for-byte
// — this file touches only the bucket-ladder scale/offset/counts fields,
// mirroring how PR #2826's classic-bucket sumMap change left every
// non-bucket field alone.
//
// The Attributes field always projects [histogramAggGroupBy]'s own
// attrsRebuild rather than a hardcoded [emptyAttrsMap] call: for the
// single-group shape that function ALREADY returns emptyAttrsMap() (an
// empty by/without list collapses to one group with no labels), so this is
// not a behavior change for v1 — just one fewer special case to keep in
// sync with the grouping logic below.
//
// avg() reuses this SAME merge — its own division is a projection rewrite
// on top, mirroring how [expHistogramGroupMergeFanout] applies
// [expHistogramAvgScaleProjections] to the fold path's output (cerberus
// issue #2866): when agg is avg, this function also collects the group's
// series count ([expHistogramGroupSeriesCountAgg], the divisor) and
// divides the five count-bearing fields of projs by it before capping the
// Project. Both steps are conditional on expHistogramGroupIsAvg(agg)
// exactly like the fold path's own, so sum() emits byte-identical SQL to
// before this file learned about avg.
func expHistogramGroupMergeSumMap(perSeries chplan.Node, agg *parser.AggregateExpr, s schema.Metrics, maxCostUnits int64) chplan.Node {
	groupBy, groupByAliases, attrsRebuild := histogramAggGroupBy(agg, &chplan.ColumnRef{Name: s.AttributesColumn}, s)
	multiGroup := len(groupBy) > 0

	mergeInput := perSeries
	var mergedScale chplan.Expr
	var totalRowCount, totalGroupCount chplan.Expr
	if multiGroup {
		partitionBy := expHistogramMergeScaleWindowPartitionBy(nil, groupBy)
		var winCols expHistogramMergeScaleWindowCols
		mergeInput, winCols = expHistogramMergeScaleWindowProject(perSeries, partitionBy, s)
		mergedScale, totalRowCount, totalGroupCount = winCols.MergedScale, winCols.TotalRowCount, winCols.TotalGroupCount
	} else {
		mergedScale = expHistogramMergeScaleScalarSubquery(perSeries, s)
	}

	aggFuncs := append(
		[]chplan.AggFunc{
			{Fn: chplan.FnGroupArray, Args: []chplan.Expr{&chplan.ColumnRef{Name: s.CountColumn}}, Alias: hqMergeCountsArrayAlias},
			{Fn: chplan.FnGroupArray, Args: []chplan.Expr{&chplan.ColumnRef{Name: s.SumColumn}}, Alias: hqMergeSumsArrayAlias},
		},
		expHistogramGroupMergeAggsSumMap(mergedScale, s)...,
	)
	if multiGroup {
		aggFuncs = append(
			aggFuncs,
			chplan.AggFunc{Fn: chplan.FnMax, Args: []chplan.Expr{totalRowCount}, Alias: hqAggMultiGroupTotalRowCountAlias},
			chplan.AggFunc{Fn: chplan.FnMax, Args: []chplan.Expr{totalGroupCount}, Alias: hqAggMultiGroupTotalGroupCountAlias},
		)
	}
	isAvg := expHistogramGroupIsAvg(agg)
	if isAvg {
		aggFuncs = append(aggFuncs, expHistogramGroupSeriesCountAgg())
	}
	merged := &chplan.Aggregate{
		Input:              mergeInput,
		GroupBy:            groupBy,
		GroupByAliases:     groupByAliases,
		AggFuncs:           aggFuncs,
		DropEmptyOnNoGroup: true,
	}
	guarded := wrapExpHistogramMergeSumMapBudgetGuard(merged, maxCostUnits, multiGroup)
	projs := append(
		[]chplan.Projection{
			{Expr: attrsRebuild, Alias: s.AttributesColumn},
			{Expr: promHistogramKahanSum(&chplan.ColumnRef{Name: hqMergeCountsArrayAlias}), Alias: s.CountColumn},
			{Expr: promHistogramKahanSum(&chplan.ColumnRef{Name: hqMergeSumsArrayAlias}), Alias: s.SumColumn},
		},
		expHistogramGroupMergeProjectionsSumMap(s)...,
	)
	if isAvg {
		projs = expHistogramAvgScaleProjections(projs, s)
	}
	return &chplan.Project{Input: guarded, Projections: projs}
}

// ExpHistogramMergeLowerer decides how the across-series exponential-
// histogram merge stage (expHistogramGroupMerge's two call sites —
// histogram_native_sum.go's expHistogramGroupMergedInstant and
// lowerExpHistogramSumOrAvgRange) collects and merges every contributing
// row's distribution: the existing groupArray + arrayReduce/arraySlice
// picker fold (every shape), or this file's two-pass sumMap reshape
// (instant mode, with or without by()/without(), SUM or AVG fold —
// chopt.FeatureExpHistogramMergeSumMap). It never returns nil.
type ExpHistogramMergeLowerer interface {
	// LowerExpHistogramMerge returns the merged chplan.Node. anchor is nil
	// in instant mode (eligible for the sumMap path) and non-nil in range
	// mode (never eligible yet — see this file's header, cerberus issue
	// #2865's tracked follow-up).
	LowerExpHistogramMerge(perSeries chplan.Node, anchor *chplan.ColumnRef, agg *parser.AggregateExpr, s schema.Metrics, maxCostUnits int64) chplan.Node
}

// FanoutExpHistogramMergeLowerer is the concrete DEFAULT
// ExpHistogramMergeLowerer: always the existing groupArray-fold merge
// (expHistogramGroupMergeFanout), regardless of shape.
type FanoutExpHistogramMergeLowerer struct{}

// LowerExpHistogramMerge returns expHistogramGroupMergeFanout(...).
func (FanoutExpHistogramMergeLowerer) LowerExpHistogramMerge(
	perSeries chplan.Node, anchor *chplan.ColumnRef, agg *parser.AggregateExpr, s schema.Metrics, maxCostUnits int64,
) chplan.Node {
	return expHistogramGroupMergeFanout(perSeries, anchor, agg, s, maxCostUnits)
}

// NativeExpHistogramMergeLowerer is the boot-wired ExpHistogramMergeLowerer
// that routes every eligible INSTANT shape (anchor == nil — any grouping,
// including by()/without(), cerberus issue #2865; SUM or AVG fold,
// cerberus issue #2866) onto expHistogramGroupMergeSumMap. cmd/cerberus
// wires it ONLY when chopt resolved exp_histogram_merge_summap at boot.
// Range mode (anchor != nil) delegates to the embedded Fallback
// unconditionally — see this file's header for why, and issue #2865's
// tracked follow-up for wiring it onto the same mechanism.
type NativeExpHistogramMergeLowerer struct {
	// Fallback is the concrete lowerer for every ineligible shape. Boot
	// wires it to FanoutExpHistogramMergeLowerer{}.
	Fallback ExpHistogramMergeLowerer
}

// LowerExpHistogramMerge returns expHistogramGroupMergeSumMap(...) for
// every instant shape (anchor == nil), or delegates to n.Fallback for
// range mode.
func (n NativeExpHistogramMergeLowerer) LowerExpHistogramMerge(
	perSeries chplan.Node, anchor *chplan.ColumnRef, agg *parser.AggregateExpr, s schema.Metrics, maxCostUnits int64,
) chplan.Node {
	if anchor == nil {
		return expHistogramGroupMergeSumMap(perSeries, agg, s, maxCostUnits)
	}
	return n.Fallback.LowerExpHistogramMerge(perSeries, anchor, agg, s, maxCostUnits)
}
