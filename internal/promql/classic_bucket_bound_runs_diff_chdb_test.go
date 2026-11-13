//go:build chdb

// Old-versus-new differential for the classic-histogram layout merge.
//
// classic_bucket_bound_runs.go builds both merge stages — the per-series
// window reduction and the across-series aggregation merge — and the
// monotonic repair in sort-and-split form, linear in the bucket entries a
// group carries. The construction it replaced rescanned every row once per
// union bound through a has-filter fold, whose captured arrays ClickHouse
// replicates once per union bound: quadratic, but the reading Prometheus's
// per-`le` semantics was proven against. This file keeps that construction,
// transcribed unchanged under a `ref` prefix, as the oracle: both are
// executed in chDB over generated groups — disjoint, shared and partially
// overlapping layouts, unordered and repeated bounds, non-finite and signed
// zero bounds, +Inf-only and single-bucket rows, equal-length and overflow
// counts, NaN and negative counts, multi-sample windows with layout changes
// — under every rung fold the lowering builds, and must agree exactly.
//
// The reference builders are a copy, not a shared helper: a shared builder
// would move in lockstep with the code under test and prove nothing.
package promql

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "github.com/chdb-io/chdb-go/chdb/driver"
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/testsql"
)

// Lambda parameter names of the reference construction.
const (
	refParamUnionBound  = "u"
	refParamRowCum      = "v"
	refParamRowLayout   = "pbs"
	refParamLadderIdx   = "i"
	refParamFiniteBound = "fb"
	refParamLadderPos   = "j"
)

func refClassicBucketUnionBoundsExpr() chplan.Expr {
	finiteRowBounds := &chplan.FuncCall{Fn: chplan.FnArrayFilter, Args: []chplan.Expr{
		&chplan.Lambda{
			Params: []string{refParamFiniteBound},
			Body:   classicBucketFiniteExpr(&chplan.BareIdent{Name: refParamFiniteBound}),
		},
		&chplan.BareIdent{Name: paramRowBounds},
	}}
	return &chplan.FuncCall{Fn: chplan.FnArraySort, Args: []chplan.Expr{
		&chplan.FuncCall{Fn: chplan.FnArrayDistinct, Args: []chplan.Expr{
			&chplan.FuncCall{Fn: chplan.FnArrayFlatten, Args: []chplan.Expr{
				&chplan.FuncCall{Fn: chplan.FnArrayMap, Args: []chplan.Expr{
					&chplan.Lambda{Params: []string{paramRowBounds}, Body: finiteRowBounds},
					&chplan.ColumnRef{Name: hqAggBoundsListAlias},
				}},
			}},
		}},
	}}
}

func refClassicBucketRowCumulativeExpr() chplan.Expr {
	return &chplan.FuncCall{Fn: chplan.FnArraySum, Args: []chplan.Expr{
		&chplan.FuncCall{Fn: chplan.FnArrayMap, Args: []chplan.Expr{
			&chplan.Lambda{
				Params: []string{paramBucketBound, paramBucketCount},
				Body: &chplan.FuncCall{Fn: chplan.FnIf, Args: []chplan.Expr{
					&chplan.Binary{
						Op:   chplan.OpAnd,
						Left: classicBucketFiniteExpr(&chplan.BareIdent{Name: paramBucketBound}),
						Right: &chplan.Binary{
							Op:    chplan.OpLe,
							Left:  &chplan.BareIdent{Name: paramBucketBound},
							Right: &chplan.BareIdent{Name: refParamUnionBound},
						},
					},
					// The ladder is a FLOAT domain throughout. Stored
					// counts are unsigned, and the per-series stage
					// differences its ladder into per-bucket counts, so
					// leaving them unsigned makes a legitimate negative
					// difference underflow — and makes CH infer a
					// Variant(Int64, UInt64) that no array aggregate
					// accepts. Prometheus's classic buckets are float
					// samples anyway.
					&chplan.FuncCall{
						Fn:   chplan.FnToFloat64,
						Args: []chplan.Expr{&chplan.BareIdent{Name: paramBucketCount}},
					},
					&chplan.LitFloat{V: 0},
				}},
			},
			&chplan.BareIdent{Name: paramRowBounds},
			&chplan.FuncCall{Fn: chplan.FnArraySlice, Args: []chplan.Expr{
				&chplan.BareIdent{Name: paramRowCounts},
				&chplan.LitInt{V: 1},
				&chplan.FuncCall{Fn: chplan.FnLength, Args: []chplan.Expr{&chplan.BareIdent{Name: paramRowBounds}}},
			}},
		}},
	}}
}

func refClassicBucketMergedLadderExpr(fold classicBucketRungFold) chplan.Expr {
	boundsList := chplan.Expr(&chplan.ColumnRef{Name: hqAggBoundsListAlias})
	countsList := chplan.Expr(&chplan.ColumnRef{Name: hqAggCountsListAlias})

	rowCumulativeAtBound := refClassicBucketRowCumulativeExpr()

	contributingRungs := &chplan.FuncCall{Fn: chplan.FnArrayFilter, Args: []chplan.Expr{
		&chplan.Lambda{
			Params: []string{refParamRowCum, refParamRowLayout},
			Body: &chplan.FuncCall{Fn: chplan.FnArrayHas, Args: []chplan.Expr{
				&chplan.BareIdent{Name: refParamRowLayout},
				&chplan.BareIdent{Name: refParamUnionBound},
			}},
		},
		&chplan.FuncCall{Fn: chplan.FnArrayMap, Args: []chplan.Expr{
			&chplan.Lambda{
				Params: []string{paramRowBounds, paramRowCounts},
				Body:   rowCumulativeAtBound,
			},
			boundsList,
			countsList,
		}},
		boundsList,
	}}

	infRungs := &chplan.FuncCall{Fn: chplan.FnArrayMap, Args: []chplan.Expr{
		&chplan.Lambda{
			Params: []string{paramRowCounts},
			Body:   classicBucketRowTotalExpr(),
		},
		countsList,
	}}

	return &chplan.FuncCall{Fn: chplan.FnArrayConcat, Args: []chplan.Expr{
		&chplan.FuncCall{Fn: chplan.FnArrayMap, Args: []chplan.Expr{
			&chplan.Lambda{Params: []string{refParamUnionBound}, Body: fold(contributingRungs)},
			refClassicBucketUnionBoundsExpr(),
		}},
		&chplan.FuncCall{Fn: chplan.FnArray, Args: []chplan.Expr{fold(infRungs)}},
	}}
}

func refClassicBucketMonotonicExpr(ladder chplan.Expr) chplan.Expr {
	return &chplan.FuncCall{Fn: chplan.FnArrayMap, Args: []chplan.Expr{
		&chplan.Lambda{
			Params: []string{refParamLadderIdx},
			Body: &chplan.FuncCall{Fn: chplan.FnArrayMax, Args: []chplan.Expr{
				&chplan.FuncCall{Fn: chplan.FnArraySlice, Args: []chplan.Expr{
					ladder,
					&chplan.LitInt{V: 1},
					&chplan.BareIdent{Name: refParamLadderIdx},
				}},
			}},
		},
		&chplan.FuncCall{Fn: chplan.FnArrayEnumerate, Args: []chplan.Expr{ladder}},
	}}
}

func refClassicBucketWindowLadderExpr(fold histogramWindowTimeFold) chplan.Expr {
	boundsList := chplan.Expr(&chplan.ColumnRef{Name: hqAggBoundsListAlias})
	countsList := chplan.Expr(&chplan.ColumnRef{Name: hqAggCountsListAlias})
	tsList := chplan.Expr(&chplan.ColumnRef{Name: hqWindowTsListAlias})

	// The rows whose layout carries this bound, and their timestamps.
	// Both filters share one predicate so the two arrays stay aligned —
	// a fold that sorts values by order would otherwise pair a count
	// with another row's timestamp.
	contributing := func(vals chplan.Expr, param string) chplan.Expr {
		return &chplan.FuncCall{Fn: chplan.FnArrayFilter, Args: []chplan.Expr{
			&chplan.Lambda{
				Params: []string{param, refParamRowLayout},
				Body: &chplan.FuncCall{Fn: chplan.FnArrayHas, Args: []chplan.Expr{
					&chplan.BareIdent{Name: refParamRowLayout},
					&chplan.BareIdent{Name: refParamUnionBound},
				}},
			},
			vals,
			boundsList,
		}}
	}

	rowCums := &chplan.FuncCall{Fn: chplan.FnArrayMap, Args: []chplan.Expr{
		&chplan.Lambda{
			Params: []string{paramRowBounds, paramRowCounts},
			Body:   refClassicBucketRowCumulativeExpr(),
		},
		boundsList,
		countsList,
	}}

	// The +Inf rung exists in every layout — every row reports
	// `{le="+Inf"}` — so it folds over the whole window unfiltered.
	infCums := &chplan.FuncCall{Fn: chplan.FnArrayMap, Args: []chplan.Expr{
		&chplan.Lambda{
			Params: []string{paramRowCounts},
			Body:   classicBucketRowTotalExpr(),
		},
		countsList,
	}}

	return &chplan.FuncCall{Fn: chplan.FnArrayConcat, Args: []chplan.Expr{
		&chplan.FuncCall{Fn: chplan.FnArrayMap, Args: []chplan.Expr{
			&chplan.Lambda{
				Params: []string{refParamUnionBound},
				Body: fold(
					contributing(rowCums, refParamRowCum),
					contributing(tsList, paramRowTime),
				),
			},
			refClassicBucketUnionBoundsExpr(),
		}},
		&chplan.FuncCall{Fn: chplan.FnArray, Args: []chplan.Expr{fold(infCums, tsList)}},
	}}
}

func refClassicBucketWindowCountsExpr(ladder chplan.Expr) chplan.Expr {
	pos := chplan.Expr(&chplan.BareIdent{Name: refParamLadderPos})
	return &chplan.FuncCall{Fn: chplan.FnArrayMap, Args: []chplan.Expr{
		&chplan.Lambda{
			Params: []string{refParamLadderPos},
			Body: &chplan.FuncCall{Fn: chplan.FnIf, Args: []chplan.Expr{
				&chplan.Binary{Op: chplan.OpEq, Left: pos, Right: &chplan.LitInt{V: 1}},
				&chplan.Subscript{Container: ladder, Key: pos},
				&chplan.Binary{
					Op:   chplan.OpSub,
					Left: &chplan.Subscript{Container: ladder, Key: pos},
					Right: &chplan.Subscript{
						Container: ladder,
						Key:       &chplan.Binary{Op: chplan.OpSub, Left: pos, Right: &chplan.LitInt{V: 1}},
					},
				},
			}},
		},
		&chplan.FuncCall{Fn: chplan.FnArrayEnumerate, Args: []chplan.Expr{ladder}},
	}}
}

// cbmDiffSeed is the generator's fixed seed: the groups are pseudo-random but
// identical on every run, so a disagreement reproduces.
const cbmDiffSeed = 4032

// cbmDiffGroups is how many groups each table holds. Every group draws its
// rows' layouts, counts and timestamps independently, so the count is the
// number of independent merges each fold is compared over.
const cbmDiffGroups = 400

// cbmDiffMaxRows bounds the rows one group collects (series for the merge,
// samples for the window stage).
const cbmDiffMaxRows = 7

// cbmDiffMaxCount bounds one generated bucket count.
const cbmDiffMaxCount = 40

// cbmDiffMergeTable / cbmDiffWindowTable are this file's own tables — the
// process-shared chDB session makes every fixture's tables visible to every
// other test in the binary (fixture_chdb_test.go's header).
const (
	cbmDiffIntegerMergeTable    = "classic_bucket_runs_diff_merge_int"
	cbmDiffFractionalMergeTable = "classic_bucket_runs_diff_merge_frac"
	cbmDiffWindowTable          = "classic_bucket_runs_diff_window"
)

// cbmDiffMaxWidth bounds one generated layout's finite bounds; a layout
// may carry one signed zero on top (see cbmDiffLayout).
const cbmDiffMaxWidth = 6

// cbmDiffFractionDenominator divides the fractional merge table's counts,
// so they are not exactly representable and their sums round.
const cbmDiffFractionDenominator = 7

// cbmDiffReassociationTolerance is the absolute difference two summations
// of the same addends in different orders may show, for the fractional
// merge table: each order's rounding error is at most
// (n - 1) * eps * sum|x| (the standard bound for recursive summation), so
// two orders differ by at most twice that. n is a row's most addends — its
// widest counts array — and sum|x| that many of the largest count; the
// reference sums a row's buckets in index order through arraySum, the
// sort-and-split construction per bound and then by arrayCumSum, so the two
// agree on every addend and differ only in association. Every fold the
// fractional table is compared under (sum, min, max, avg, count, group,
// quantile) and the monotonic repair move a value by at most what moved
// their inputs, so the bound carries through them.
const cbmDiffReassociationTolerance = 2 * (cbmDiffMaxWidth + 2) * 0x1p-52 *
	(cbmDiffMaxWidth + 2) * cbmDiffMaxCount / cbmDiffFractionDenominator

// Column names the window table carries besides the aggregate aliases the
// expressions read.
const (
	cbmDiffGroupColumn      = "grp"
	cbmDiffRangeStartColumn = "rs"
	cbmDiffRangeEndColumn   = "re"
)

// cbmDiffWindowEnd is the right edge of every generated window; samples sit
// in the cbmDiffWindowSeconds before it.
var cbmDiffWindowEnd = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// cbmDiffWindowSeconds is the generated window's width.
const cbmDiffWindowSeconds = 300

// Values the bound generator draws from besides ordinary finite bounds.
var cbmDiffSpecialBounds = []float64{math.Inf(1), math.Inf(-1), math.NaN(), math.Copysign(0, -1), 0}

// cbmDiffLayout draws one row's ExplicitBounds. shared is the group's
// common layout and series the row's own index, so a family can be shared
// across the group, disjoint per series, or overlapping.
func cbmDiffLayout(r *rand.Rand, shared []float64, series int) []float64 {
	const (
		disjointStride = 10
		overlapStride  = 3
	)
	width := 1 + r.IntN(cbmDiffMaxWidth)
	bounds := make([]float64, 0, width)
	switch r.IntN(9) {
	case 0, 1: // the group's shared layout
		return append(bounds, shared...)
	case 2: // disjoint per series
		for j := range width {
			bounds = append(bounds, float64(series*disjointStride+j+1))
		}
	case 3: // partially overlapping per series
		for j := range width {
			bounds = append(bounds, float64(series*overlapStride+j+1))
		}
	case 4: // unordered, with repeats
		for range width {
			bounds = append(bounds, float64(1+r.IntN(cbmDiffMaxWidth))/2)
		}
	case 5: // a special value spliced into an ordinary layout
		for j := range width {
			bounds = append(bounds, float64(j+1))
		}
		bounds[r.IntN(len(bounds))] = cbmDiffSpecialBounds[r.IntN(len(cbmDiffSpecialBounds))]
	case 6: // +Inf-only row: no finite bound at all
	case 7: // one bucket
		bounds = append(bounds, float64(1+r.IntN(cbmDiffMaxWidth)))
	default: // fractional bounds around zero, signed zeros included
		for j := range width {
			bounds = append(bounds, float64(j)-float64(width)/2)
		}
		if r.IntN(2) == 0 {
			bounds = append([]float64{math.Copysign(0, -1)}, bounds...)
		}
	}
	return bounds
}

// cbmDiffSharedLayout draws a group's common layout.
func cbmDiffSharedLayout(r *rand.Rand) []float64 {
	layouts := [][]float64{
		{0.1, 0.5, 1, 5, 10},
		{1, 2, 4, 8},
		{0, 1},
		{0.25},
	}
	return layouts[r.IntN(len(layouts))]
}

// cbmDiffCountLen is the counts length a row with bounds carries: one past
// the bounds (the OTel overflow rung) or, as the corpus also seeds, equal.
func cbmDiffCountLen(r *rand.Rand, bounds []float64) int {
	if r.IntN(4) == 0 && len(bounds) > 0 {
		return len(bounds)
	}
	return len(bounds) + 1
}

// cbmDiffFloat renders v as a ClickHouse Float64 literal, special values
// and the sign of zero included.
func cbmDiffFloat(v float64) string {
	switch {
	case math.IsNaN(v):
		return "nan"
	case math.IsInf(v, 1):
		return "inf"
	case math.IsInf(v, -1):
		return "-inf"
	case v == 0 && math.Signbit(v):
		return "-0."
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// cbmDiffArray renders vals as a typed ClickHouse array literal.
func cbmDiffArray(vals []string, elemType string) string {
	return "CAST([" + strings.Join(vals, ", ") + "] AS Array(" + elemType + "))"
}

// cbmDiffMergeSeed renders a merge table: per group, the groupArray'd
// bounds and counts the across-series merge reads. The counts are what the
// per-series stage hands up — Float64, occasionally negative (a differenced
// ladder that dipped) and occasionally NaN; integer-valued unless
// fractional, in which case they are divided by cbmDiffFractionDenominator.
func cbmDiffMergeSeed(r *rand.Rand, table string, fractional bool) string {
	var b strings.Builder
	b.WriteString("CREATE OR REPLACE TABLE " + table + " (" + cbmDiffGroupColumn + " UInt32, " +
		hqAggBoundsListAlias + " Array(Array(Float64)), " + hqAggCountsListAlias + " Array(Array(Float64))) ENGINE = Memory;\n")
	b.WriteString("INSERT INTO " + table + " VALUES\n")
	for g := range cbmDiffGroups {
		shared := cbmDiffSharedLayout(r)
		rows := 1 + r.IntN(cbmDiffMaxRows)
		boundsRows := make([]string, rows)
		countRows := make([]string, rows)
		for i := range rows {
			bounds := cbmDiffLayout(r, shared, i)
			bs := make([]string, len(bounds))
			for j, v := range bounds {
				bs[j] = cbmDiffFloat(v)
			}
			cs := make([]string, cbmDiffCountLen(r, bounds))
			for j := range cs {
				v := float64(r.IntN(cbmDiffMaxCount))
				if fractional {
					v /= cbmDiffFractionDenominator
				}
				switch r.IntN(20) {
				case 0:
					v = -v
				case 1:
					v = math.NaN()
				}
				cs[j] = cbmDiffFloat(v)
			}
			boundsRows[i] = cbmDiffArray(bs, "Float64")
			countRows[i] = cbmDiffArray(cs, "Float64")
		}
		sep := ","
		if g == cbmDiffGroups-1 {
			sep = ";"
		}
		fmt.Fprintf(&b, "(%d, [%s], [%s])%s\n", g, strings.Join(boundsRows, ", "), strings.Join(countRows, ", "), sep)
	}
	return b.String()
}

// cbmDiffWindowSeed renders the window table: per group, ONE series'
// in-window samples — raw UInt64 counter readings that mostly grow and
// occasionally reset, a layout that holds for a stretch and then changes,
// timestamps that may repeat — plus the window edges and the temporality
// the folds read.
func cbmDiffWindowSeed(r *rand.Rand) string {
	var b strings.Builder
	b.WriteString("CREATE OR REPLACE TABLE " + cbmDiffWindowTable + " (" + cbmDiffGroupColumn + " UInt32, " +
		hqAggBoundsListAlias + " Array(Array(Float64)), " + hqAggCountsListAlias + " Array(Array(UInt64)), " +
		hqWindowTsListAlias + " Array(DateTime64(9)), " + hqWindowTemporalityAlias + " Int32, " +
		cbmDiffRangeStartColumn + " DateTime64(9), " + cbmDiffRangeEndColumn + " DateTime64(9)) ENGINE = Memory;\n")
	b.WriteString("INSERT INTO " + cbmDiffWindowTable + " VALUES\n")
	end := cbmDiffWindowEnd
	start := end.Add(-cbmDiffWindowSeconds * time.Second)
	for g := range cbmDiffGroups {
		shared := cbmDiffSharedLayout(r)
		layout := cbmDiffLayout(r, shared, g)
		samples := 1 + r.IntN(cbmDiffMaxRows)
		counts := make([]uint64, cbmDiffCountLen(r, layout))
		boundsRows := make([]string, samples)
		countRows := make([]string, samples)
		tsRows := make([]string, samples)
		for i := range samples {
			if r.IntN(4) == 0 { // the layout changes mid-window
				layout = cbmDiffLayout(r, shared, g)
				counts = make([]uint64, cbmDiffCountLen(r, layout))
			}
			if r.IntN(8) == 0 { // a counter reset
				clear(counts)
			}
			cs := make([]string, len(counts))
			for j := range counts {
				counts[j] += uint64(r.IntN(cbmDiffMaxCount))
				cs[j] = strconv.FormatUint(counts[j], 10)
			}
			bs := make([]string, len(layout))
			for j, v := range layout {
				bs[j] = cbmDiffFloat(v)
			}
			// Sample i of samples sits at an increasing offset; a repeat of
			// the previous offset models a duplicate scrape.
			offset := (i + 1) * cbmDiffWindowSeconds / (cbmDiffMaxRows + 1)
			if i > 0 && r.IntN(6) == 0 {
				offset = i * cbmDiffWindowSeconds / (cbmDiffMaxRows + 1)
			}
			boundsRows[i] = cbmDiffArray(bs, "Float64")
			countRows[i] = cbmDiffArray(cs, "UInt64")
			tsRows[i] = fmt.Sprintf("toDateTime64('%s', 9)", start.Add(time.Duration(offset)*time.Second).Format(time.DateTime))
		}
		const deltaTemporality, cumulativeTemporality = 1, 2
		temporality := cumulativeTemporality
		if r.IntN(4) == 0 {
			temporality = deltaTemporality
		}
		sep := ","
		if g == cbmDiffGroups-1 {
			sep = ";"
		}
		fmt.Fprintf(&b, "(%d, [%s], [%s], [%s], %d, toDateTime64('%s', 9), toDateTime64('%s', 9))%s\n",
			g, strings.Join(boundsRows, ", "), strings.Join(countRows, ", "), strings.Join(tsRows, ", "),
			temporality, start.Format(time.DateTime), end.Format(time.DateTime), sep)
	}
	return b.String()
}

// cbmDiffCoalesced renders, over a (union, ladder) pair of columns, the
// layout the quantile emitter actually reads: every run of equal bounds
// coalesced to its first entry, and the ladder's finite rungs with it (the
// trailing +Inf rung untouched). The reference union keeps -0.0 and 0.0 as
// two bounds — arrayDistinct tells them apart — while has() folds both rungs
// over the same rows; the emitter's coalescing (internal/chsql's
// keptBoundMask) collapses them to one. Applied to both sides alike.
func cbmDiffCoalesced(union, ladder string) (coalescedUnion, coalescedLadder string) {
	mask := "arrayMap((cb, ci) -> ci = 1 OR cb != " + union + "[ci - 1], " + union + ", arrayEnumerate(" + union + "))"
	coalescedUnion = "arrayFilter((cb, ck) -> ck, " + union + ", " + mask + ")"
	coalescedLadder = "arrayConcat(arrayFilter((cv, ck) -> ck, arraySlice(" + ladder + ", 1, length(" + union + ")), " + mask + "), " +
		"arraySlice(" + ladder + ", length(" + union + ") + 1))"
	return coalescedUnion, coalescedLadder
}

// cbmDiffCompare emits plan over chDB and compares, per group, the
// reference and the new (union, ladder) column pairs after coalescing. The
// unions must be identical. A numeric ladder must agree rung for rung —
// equal values, NaN matching NaN — exactly, or with a non-zero tolerance
// within it. A ladder of strings (the order-revealing folds') must be
// identical.
// Numeric rungs compare by value rather than by rendering so a -0.0 count
// matches a 0.0 one; see cbmDiffOrderFold.
// It fails on every group that disagrees and returns how many it compared.
func cbmDiffCompare(t *testing.T, db *sql.DB, label string, plan chplan.Node, numeric bool, tolerance float64, refUnion, refLadder, newUnion, newLadder string) int {
	t.Helper()
	inner, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("%s: Emit: %v", label, err)
	}
	ru, rl := cbmDiffCoalesced(refUnion, refLadder)
	nu, nl := cbmDiffCoalesced(newUnion, newLadder)
	agree := "toString(" + rl + ") = toString(" + nl + ")"
	if numeric {
		agree = "length(" + rl + ") = length(" + nl + ") AND arrayAll((x, y) -> x = y OR (isNaN(x) AND isNaN(y)) OR " +
			"abs(x - y) <= " + strconv.FormatFloat(tolerance, 'g', -1, 64) + ", " + rl + ", " + nl + ")"
	}
	rows, err := db.Query("SELECT "+cbmDiffGroupColumn+", toString("+ru+"), toString("+nu+"), toString("+rl+"), toString("+nl+"), "+
		agree+" FROM ("+inner+") ORDER BY "+cbmDiffGroupColumn, args...)
	if err != nil {
		t.Fatalf("%s: query: %v", label, err)
	}
	defer func() { _ = rows.Close() }()
	compared := 0
	for rows.Next() {
		var grp uint32
		var wantUnion, gotUnion, wantLadder, gotLadder string
		var ladderAgrees bool
		if err := rows.Scan(&grp, &wantUnion, &gotUnion, &wantLadder, &gotLadder, &ladderAgrees); err != nil {
			t.Fatalf("%s: scan: %v", label, err)
		}
		compared++
		if gotUnion != wantUnion || !ladderAgrees {
			t.Errorf("%s group %d: sort-and-split answers %s %s, the has-filter reference answers %s %s",
				label, grp, gotUnion, gotLadder, wantUnion, wantLadder)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: rows.Err: %v", label, err)
	}
	return compared
}

// cbmDiffOrderFold is a rung fold that answers the rung's values exactly as
// handed over, in order. Any fold reads its rung through this array, so two
// constructions that agree under it agree under every fold — sum's float
// association included.
//
// Each value is rendered as x + 0, which turns -0.0 into 0.0 and leaves
// every other value as it is: a row whose only bucket at or below a bound
// counts -0.0 reads -0.0 through the reference's arraySum but 0.0 through
// arrayCumSum, which starts from +0.0. The two are equal under every
// comparison and every arithmetic step the quantile applies to a count, so
// the sign of a zero count is not part of the answer; see cbmDiffCompare,
// which compares numeric ladders by value for the same reason.
func cbmDiffOrderFold(rungs chplan.Expr) chplan.Expr {
	const param = "ofx"
	return call(chplan.FnToString, call(chplan.FnArrayMap, lambda(addExpr(bare(param), &chplan.LitFloat{V: 0}), param), rungs))
}

// cbmDiffWindowOrderFold is cbmDiffOrderFold's window counterpart: it
// answers each rung's (timestamp, value) pairs in the order handed over, so
// it also catches a value paired with another row's timestamp.
func cbmDiffWindowOrderFold(values, order chplan.Expr) chplan.Expr {
	return call(chplan.FnToString, call(chplan.FnArrayZip, order, values))
}

func cbmDiffOpenSeeded(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatalf("open chdb: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	r := rand.New(rand.NewPCG(cbmDiffSeed, cbmDiffSeed))
	for _, seed := range []string{
		cbmDiffMergeSeed(r, cbmDiffIntegerMergeTable, false),
		cbmDiffMergeSeed(r, cbmDiffFractionalMergeTable, true),
		cbmDiffWindowSeed(r),
	} {
		for _, stmt := range testsql.SplitStatements(seed) {
			if stmt = strings.TrimSpace(stmt); stmt == "" {
				continue
			}
			if _, err := db.Exec(stmt); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
	}
	return db
}

// TestClassicBucketRuns_MergeMatchesHasFilterReference compares the
// across-series merge — layout, raw ladder, and the monotonic repair over
// it — with the has-filter construction, under every rung fold the
// lowering builds and under the order-revealing fold.
//
// Over integer-valued counts every sum is exact, so the two must agree bit
// for bit under every fold: same rungs, same rows in each rung, same order.
// Over fractional counts a row's cumulative count is the same addends
// summed in a different association, so there the ladders must agree within
// cbmDiffReassociationTolerance, under the folds that tolerance carries
// through.
func TestClassicBucketRuns_MergeMatchesHasFilterReference(t *testing.T) {
	db := cbmDiffOpenSeeded(t)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})
	foldOf := func(q string) classicBucketRungFold {
		expr, err := p.ParseExpr(q)
		if err != nil {
			t.Fatalf("parse %q: %v", q, err)
		}
		fold, _, err := classicBucketLadderFold(expr.(*parser.AggregateExpr), s, lowerCtx{})
		if err != nil || fold == nil {
			t.Fatalf("%q: no rung fold (%v)", q, err)
		}
		return fold
	}
	// tolerant marks the folds the fractional table is compared under.
	type foldCase struct {
		fold     classicBucketRungFold
		numeric  bool
		tolerant bool
	}
	folds := map[string]foldCase{"order": {fold: cbmDiffOrderFold}}
	for _, q := range []string{
		"sum by (le) (x)", "avg by (le) (x)", "min by (le) (x)", "max by (le) (x)",
		"count by (le) (x)", "group by (le) (x)", "quantile by (le) (0.3, x)",
	} {
		folds[q] = foldCase{fold: foldOf(q), numeric: true, tolerant: true}
	}
	for _, q := range []string{"stddev by (le) (x)", "stdvar by (le) (x)"} {
		folds[q] = foldCase{fold: foldOf(q), numeric: true}
	}
	const (
		refUnion, refLadder, refRepaired = "ref_union", "ref_ladder", "ref_repaired"
		merged, newRepaired              = "new_merged", "new_repaired"
		newUnion, newLadder              = "new_union", "new_ladder"
	)
	boundsList := &chplan.ColumnRef{Name: hqAggBoundsListAlias}
	countsList := &chplan.ColumnRef{Name: hqAggCountsListAlias}
	for name, fc := range folds {
		for _, table := range []string{cbmDiffIntegerMergeTable, cbmDiffFractionalMergeTable} {
			tolerance := 0.0
			if table == cbmDiffFractionalMergeTable {
				if !fc.tolerant {
					continue
				}
				tolerance = cbmDiffReassociationTolerance
			}
			layered := &chplan.Project{
				Input: &chplan.Scan{Table: table},
				Projections: []chplan.Projection{
					{Expr: &chplan.ColumnRef{Name: cbmDiffGroupColumn}, Alias: cbmDiffGroupColumn},
					{Expr: refClassicBucketUnionBoundsExpr(), Alias: refUnion},
					{Expr: refClassicBucketMergedLadderExpr(fc.fold), Alias: refLadder},
					{Expr: classicBucketMergeRunsExpr(fc.fold, boundsList, countsList), Alias: merged},
				},
			}
			projections := []chplan.Projection{
				{Expr: &chplan.ColumnRef{Name: cbmDiffGroupColumn}, Alias: cbmDiffGroupColumn},
				{Expr: &chplan.ColumnRef{Name: refUnion}, Alias: refUnion},
				{Expr: &chplan.ColumnRef{Name: refLadder}, Alias: refLadder},
				{Expr: tupleAt(&chplan.ColumnRef{Name: merged}, runsTupleBounds), Alias: newUnion},
				{Expr: tupleAt(&chplan.ColumnRef{Name: merged}, runsTupleValues), Alias: newLadder},
			}
			if fc.numeric {
				projections = append(
					projections,
					chplan.Projection{Expr: refClassicBucketMonotonicExpr(&chplan.ColumnRef{Name: refLadder}), Alias: refRepaired},
					chplan.Projection{Expr: classicBucketMonotonicExpr(tupleAt(&chplan.ColumnRef{Name: merged}, runsTupleValues)), Alias: newRepaired},
				)
			}
			plan := &chplan.Project{Input: layered, Projections: projections}
			label := name + " over " + table
			if n := cbmDiffCompare(t, db, label+" ladder", plan, fc.numeric, tolerance, refUnion, refLadder, newUnion, newLadder); n != cbmDiffGroups {
				t.Fatalf("%s: compared %d groups, seeded %d", label, n, cbmDiffGroups)
			}
			if fc.numeric {
				cbmDiffCompare(t, db, label+" repaired", plan, true, tolerance, refUnion, refRepaired, newUnion, newRepaired)
			}
		}
	}
}

// TestClassicBucketRuns_WindowMatchesHasFilterReference compares the
// per-series window stage — layout, ladder, and the per-bucket counts
// differenced back out of it — with the has-filter construction, under
// every window fold the classic path builds and under the order-revealing
// fold.
func TestClassicBucketRuns_WindowMatchesHasFilterReference(t *testing.T) {
	db := cbmDiffOpenSeeded(t)
	in := histogramWindowInputs{
		rangeStart:  &chplan.ColumnRef{Name: cbmDiffRangeStartColumn},
		rangeEnd:    &chplan.ColumnRef{Name: cbmDiffRangeEndColumn},
		temporality: &chplan.ColumnRef{Name: hqWindowTemporalityAlias},
	}
	rateIn := in
	rateIn.perSecond = &chplan.LitFloat{V: cbmDiffWindowSeconds}
	folds := map[string]histogramWindowTimeFold{
		"order":                 cbmDiffWindowOrderFold,
		rateWindowFn:            histogramWindowFold(rateWindowFn, rateIn),
		increaseWindowFn:        histogramWindowFold(increaseWindowFn, in),
		sumOverTimeWindowFn:     histogramWindowFold(sumOverTimeWindowFn, in),
		avgOverTimeWindowFn:     histogramWindowFold(avgOverTimeWindowFn, in),
		lastOverTimeWindowFn:    histogramWindowFold(lastOverTimeWindowFn, in),
		firstOverTimeWindowFn:   histogramWindowFold(firstOverTimeWindowFn, in),
		countOverTimeWindowFn:   histogramWindowFold(countOverTimeWindowFn, in),
		presentOverTimeWindowFn: histogramWindowFold(presentOverTimeWindowFn, in),
		deltaWindowFn:           histogramWindowFold(deltaWindowFn, in),
		irateWindowFn:           histogramWindowFold(irateWindowFn, in),
		ideltaWindowFn:          histogramWindowFold(ideltaWindowFn, in),
		resetsWindowFn:          histogramWindowFold(resetsWindowFn, in),
		changesWindowFn:         histogramWindowFold(changesWindowFn, in),
	}
	const (
		refUnion, refLadder, refCounts = "ref_union", "ref_ladder", "ref_counts"
		newUnion, newLadder, newCounts = "new_union", "new_ladder", "new_counts"
	)
	passthrough := func(names ...string) []chplan.Projection {
		out := make([]chplan.Projection, 0, len(names))
		for _, n := range names {
			out = append(out, chplan.Projection{Expr: &chplan.ColumnRef{Name: n}, Alias: n})
		}
		return out
	}
	for name, fold := range folds {
		layered := &chplan.Project{
			Input: &chplan.Scan{Table: cbmDiffWindowTable},
			Projections: append(
				passthrough(cbmDiffGroupColumn),
				chplan.Projection{Expr: refClassicBucketUnionBoundsExpr(), Alias: refUnion},
				chplan.Projection{Expr: refClassicBucketWindowLadderExpr(fold), Alias: refLadder},
				chplan.Projection{Expr: classicBucketWindowRunsExpr(fold), Alias: hqWindowLadderAlias},
			),
		}
		projections := append(
			passthrough(cbmDiffGroupColumn, refUnion, refLadder),
			chplan.Projection{Expr: tupleAt(&chplan.ColumnRef{Name: hqWindowLadderAlias}, runsTupleBounds), Alias: newUnion},
			chplan.Projection{Expr: tupleAt(&chplan.ColumnRef{Name: hqWindowLadderAlias}, runsTupleValues), Alias: newLadder},
		)
		if name != "order" {
			projections = append(
				projections,
				chplan.Projection{Expr: refClassicBucketWindowCountsExpr(&chplan.ColumnRef{Name: refLadder}), Alias: refCounts},
				chplan.Projection{Expr: classicBucketWindowCountsExpr(), Alias: newCounts},
			)
		}
		plan := &chplan.Project{Input: layered, Projections: projections}
		n := cbmDiffCompare(t, db, name+" ladder", plan, name != "order", 0, refUnion, refLadder, newUnion, newLadder)
		if name != "order" {
			cbmDiffCompare(t, db, name+" counts", plan, true, 0, refUnion, refCounts, newUnion, newCounts)
		}
		if n != cbmDiffGroups {
			t.Fatalf("%s: compared %d groups, seeded %d", name, n, cbmDiffGroups)
		}
	}
}
