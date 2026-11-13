package promql

import (
	"github.com/tsouza/cerberus/internal/chplan"
)

// classic_bucket_bound_runs.go builds the classic-histogram layout merge in
// sort-and-split form: every (bound, value) entry the contributing rows
// offer is sorted by bound once, cut into one run per distinct bound, and
// each run is folded on its own. The merged layout is the run heads, and
// the merged ladder is one fold per run.
//
// # The capture rule every expression here keeps
//
// ClickHouse evaluates a lambda over a whole column at once, and a column
// the lambda body reads from OUTSIDE its own parameters is replicated once
// per element of the array being mapped. A body that reads a group-sized
// array while mapping a group-sized array therefore materialises the
// product of the two — the quadratic the per-union-bound rescan
// `arrayMap(u -> fold(arrayFilter(... has(pbs, u) ..., BL)), union)` paid.
// Every lambda in this file reads only its own parameters (or a scalar,
// such as a constant phi), and every helper that needs one value in several
// places binds it once through [hqLet], whose single-element array makes
// any capture inside it a copy of one value rather than one per element. So
// the merge costs a sort of the T contributing entries plus a constant
// number of linear passes over them.
//
// # Equivalence with the per-bound has-filter fold
//
// The rung at bound u is the fold, over the rows whose OWN layout carries u,
// of that row's cumulative count at u (every bucket whose finite bound is
// <= u). Each row first reduces itself to one (bound, cumulative) entry per
// distinct finite bound it carries ([classicBucketRowEntriesExpr]), so a run
// holds exactly one entry per row carrying its bound and none for a row that
// does not — the has-filter's membership test, expressed as grouping.
//
// Every sort keys on the pair (bound, collection position). ClickHouse's
// arraySort is not stable, so keying on the bound alone would leave the
// order inside a run — and with it the order a fold reads its values in —
// to the sort's internals; the position makes every key unique, so the sort
// has exactly one answer and a run lists its rows in the order the group
// collected them, the order the has-filter fold read them in. Each payload
// is sorted under that same unique key, so payloads stay paired entry for
// entry. The run's head bound is the first-collected spelling of its value,
// which matters for one value only: -0.0 and 0.0 compare equal, so they share
// a run (exactly the rows `has` matches for either).
//
// classic_bucket_bound_runs_diff_chdb_test.go keeps the per-bound has-filter
// construction as its reference and checks every merge fold and window
// reducer against it.

// Lambda parameter and binding names. The row, merge and window levels each
// carry their own prefix so a nested level never shadows an enclosing one.
const (
	runsPrefixRow    = "kr"
	runsPrefixMerge  = "km"
	runsPrefixWindow = "kw"

	runsSortedBoundsSuffix = "b"
	runsStartsSuffix       = "s"
	runsUnionSuffix        = "u"
	runsElemSuffix         = "x"
	runsFlagSuffix         = "f"
	runsDiffSuffix         = "d"
	runsPosSuffix          = "i"
	runsKeySuffix          = "k"
	runsRunSuffix          = "r"
	runsRunOrderSuffix     = "o"
	runsEntriesSuffix      = "e"
	runsFlatBoundsSuffix   = "eb"
	runsFlatValuesSuffix   = "ev"
	runsFlatOrderSuffix    = "et"
	runsRowCountsSuffix    = "rc"
	runsRowBoundsSuffix    = "rb"
	runsRowTimeSuffix      = "rt"

	// Prefix-maximum (monotonic repair) names.
	paramPrefixMaxLadder = "kpl"
	paramPrefixMaxRank   = "kpr"
	paramPrefixMaxRunMax = "kpm"
	paramPrefixMaxA      = "kpa"
	paramPrefixMaxB      = "kpb"
	paramPrefixMaxIdx    = "kpi"
	paramPrefixMaxVal    = "kpv"
	paramPrefixMaxKey    = "kpk"
	paramPrefixMaxPerm   = "kpp"
	paramPrefixMaxDiff   = "kpd"
)

// Tuple positions of a row's (or a group's) reduced entries — see
// [classicBucketRowEntriesExpr] and [classicBucketMergeRunsExpr].
const (
	runsTupleBounds = 1
	runsTupleValues = 2
	runsTupleOrder  = 3
)

// firstPosition is the 1-based index of an array's first element.
const firstPosition = 1

func bare(name string) chplan.Expr { return &chplan.BareIdent{Name: name} }

func call(fn chplan.Fn, args ...chplan.Expr) chplan.Expr {
	return &chplan.FuncCall{Fn: fn, Args: args}
}

func lambda(body chplan.Expr, params ...string) chplan.Expr {
	return &chplan.Lambda{Params: params, Body: body}
}

func tupleAt(t chplan.Expr, pos int64) chplan.Expr {
	return call(chplan.FnTupleElement, t, &chplan.LitInt{V: pos})
}

// classicBucketBoundRunsExpr sorts the entries keyed by bounds, cuts them
// into one run per distinct bound, and hands body the distinct bounds
// (ascending) and, for each payload, the payload's runs — payloads[k]'s
// runs[k][j] holds the payload values of the entries whose bound is
// union[j], in collection order.
//
// bounds and every payload must be cheap references (a column or a lambda
// parameter) of the same length: each is rendered more than once.
func classicBucketBoundRunsExpr(prefix string, bounds chplan.Expr, payloads []chplan.Expr, body func(union chplan.Expr, runs []chplan.Expr) chplan.Expr) chplan.Expr {
	elem, flag := prefix+runsElemSuffix, prefix+runsFlagSuffix
	key, pos0 := prefix+runsKeySuffix, prefix+runsPosSuffix
	// positionKey renders the unique sort key (bound, collection position)
	// — see this file's header for why the position is part of it.
	positionKey := func(payload chplan.Expr) chplan.Expr {
		return call(
			chplan.FnArraySort,
			lambda(call(chplan.FnTuple, bare(key), bare(pos0)), elem, key, pos0),
			payload, bounds, call(chplan.FnArrayEnumerate, bounds),
		)
	}
	return hqLet(prefix+runsSortedBoundsSuffix, positionKey(bounds), func(sorted chplan.Expr) chplan.Expr {
		// A run starts at the first entry and wherever the bound differs from
		// the one before it. arrayDifference is zero exactly for an equal
		// pair of finite bounds (the only kind the row reduction keeps), so
		// this is the `b != previous b` test without reading a neighbour
		// through a capture.
		diff, pos := prefix+runsDiffSuffix, prefix+runsPosSuffix
		starts := call(
			chplan.FnArrayMap,
			lambda(&chplan.Binary{
				Op:    chplan.OpOr,
				Left:  &chplan.Binary{Op: chplan.OpEq, Left: bare(pos), Right: &chplan.LitInt{V: firstPosition}},
				Right: &chplan.Binary{Op: chplan.OpNe, Left: bare(diff), Right: &chplan.LitInt{V: 0}},
			}, diff, pos),
			call(chplan.FnArrayDifference, sorted),
			call(chplan.FnArrayEnumerate, sorted),
		)
		return hqLet(prefix+runsStartsSuffix, starts, func(st chplan.Expr) chplan.Expr {
			union := call(chplan.FnArrayFilter, lambda(bare(flag), elem, flag), sorted, st)
			return hqLet(prefix+runsUnionSuffix, union, func(u chplan.Expr) chplan.Expr {
				runs := make([]chplan.Expr, len(payloads))
				for k, p := range payloads {
					runs[k] = call(chplan.FnArraySplit, lambda(bare(flag), elem, flag), positionKey(p), st)
				}
				return body(u, runs)
			})
		})
	})
}

// classicBucketRowEntriesExpr reduces ONE row — its ExplicitBounds bs and
// BucketCounts cs — to tuple(bounds, cumulative): one entry per distinct
// finite bound the row carries, ascending, paired with the row's count of
// observations at or below it. A non-finite bound and its paired count are
// dropped first (#2495: a -Inf bound would otherwise fold into every rung),
// and the trailing overflow count BucketCounts carries past the paired range
// belongs to no finite bound, so it is left out here and read by the +Inf
// rung instead.
//
// A row that repeats a bound contributes ONE entry for it, holding every
// bucket at or below it — what the has-filter fold reads for that row, and
// why the row reduces itself before the group merge sees it.
//
// extra, when set, appends a third tuple element built from the row's own
// entry count (the window stage uses it to stamp each entry with the row's
// timestamp).
func classicBucketRowEntriesExpr(bs, cs chplan.Expr, extra func(n chplan.Expr) chplan.Expr) chplan.Expr {
	finite := prefixed(runsPrefixRow, runsElemSuffix)
	keep := classicBucketFiniteExpr(bare(finite))
	countParam, boundParam := prefixed(runsPrefixRow, runsRowCountsSuffix), prefixed(runsPrefixRow, runsRowBoundsSuffix)
	finiteBounds := call(chplan.FnArrayFilter, lambda(keep, finite), bs)
	// The ladder is a FLOAT domain throughout — see
	// classicBucketRowTotalExpr.
	floatCounts := call(
		chplan.FnArrayMap,
		lambda(toFloat64Expr(bare(countParam)), countParam),
		call(chplan.FnArraySlice, cs, &chplan.LitInt{V: firstPosition}, call(chplan.FnLength, bs)),
	)
	finiteCounts := call(
		chplan.FnArrayFilter,
		lambda(classicBucketFiniteExpr(bare(boundParam)), countParam, boundParam),
		floatCounts, bs,
	)
	return hqLet(prefixed(runsPrefixRow, runsFlatBoundsSuffix), finiteBounds, func(fb chplan.Expr) chplan.Expr {
		return hqLet(prefixed(runsPrefixRow, runsFlatValuesSuffix), finiteCounts, func(fc chplan.Expr) chplan.Expr {
			return classicBucketBoundRunsExpr(runsPrefixRow, fb, []chplan.Expr{fc}, func(union chplan.Expr, runs []chplan.Expr) chplan.Expr {
				run := prefixed(runsPrefixRow, runsRunSuffix)
				perBound := call(chplan.FnArrayMap, lambda(call(chplan.FnArraySum, bare(run)), run), runs[0])
				elems := []chplan.Expr{union, call(chplan.FnArrayCumSum, perBound)}
				if extra != nil {
					elems = append(elems, extra(call(chplan.FnLength, union)))
				}
				return call(chplan.FnTuple, elems...)
			})
		})
	})
}

func prefixed(prefix, suffix string) string { return prefix + suffix }

// classicBucketRowsEntriesFlattened binds rows — an array of
// classicBucketRowEntriesExpr tuples — once, flattens each tuple position
// across every row, and hands body the flat bounds, values and (when
// withOrder) order arrays, all entry-aligned.
func classicBucketRowsEntriesFlattened(prefix string, rows chplan.Expr, withOrder bool, body func(bounds, values, order chplan.Expr) chplan.Expr) chplan.Expr {
	elem := prefix + runsElemSuffix
	flatten := func(entries chplan.Expr, pos int64) chplan.Expr {
		return call(chplan.FnArrayFlatten, call(chplan.FnArrayMap, lambda(tupleAt(bare(elem), pos), elem), entries))
	}
	return hqLet(prefix+runsEntriesSuffix, rows, func(entries chplan.Expr) chplan.Expr {
		return hqLet(prefix+runsFlatBoundsSuffix, flatten(entries, runsTupleBounds), func(eb chplan.Expr) chplan.Expr {
			return hqLet(prefix+runsFlatValuesSuffix, flatten(entries, runsTupleValues), func(ev chplan.Expr) chplan.Expr {
				if !withOrder {
					return body(eb, ev, nil)
				}
				return hqLet(prefix+runsFlatOrderSuffix, flatten(entries, runsTupleOrder), func(et chplan.Expr) chplan.Expr {
					return body(eb, ev, et)
				})
			})
		})
	})
}

// classicBucketMergeRunsExpr renders the group's merged layout and its
// not-yet-repaired cumulative ladder as tuple(union, ladder): one rung per
// union bound, folded by fold over the rows carrying it, plus the trailing
// +Inf rung folded over every row (the overflow bucket exists in every
// layout). boundsList and countsList are the group's groupArray-collected
// ExplicitBounds and BucketCounts lists, read through the merge guard's gate
// (see classicBucketMergeGateExpr) so ClickHouse cannot start the merge
// before the guard has run.
func classicBucketMergeRunsExpr(fold classicBucketRungFold, boundsList, countsList chplan.Expr) chplan.Expr {
	rows := call(
		chplan.FnArrayMap,
		lambda(classicBucketRowEntriesExpr(bare(paramRowBounds), bare(paramRowCounts), nil), paramRowBounds, paramRowCounts),
		boundsList, countsList,
	)
	infRungs := call(chplan.FnArrayMap, lambda(classicBucketRowTotalExpr(), paramRowCounts), countsList)
	return classicBucketRowsEntriesFlattened(runsPrefixMerge, rows, false, func(eb, ev, _ chplan.Expr) chplan.Expr {
		return classicBucketBoundRunsExpr(runsPrefixMerge, eb, []chplan.Expr{ev}, func(union chplan.Expr, runs []chplan.Expr) chplan.Expr {
			run := runsPrefixMerge + runsRunSuffix
			rungs := call(chplan.FnArrayMap, lambda(fold(bare(run)), run), runs[0])
			ladder := call(chplan.FnArrayConcat, rungs, call(chplan.FnArray, fold(infRungs)))
			return call(chplan.FnTuple, union, ladder)
		})
	})
}

// classicBucketWindowRunsExpr is classicBucketMergeRunsExpr's per-series
// counterpart: the rows are ONE series' in-window samples, and each rung is
// folded across TIME by fold, which receives the run's values and their
// sample timestamps, positionally aligned.
func classicBucketWindowRunsExpr(fold histogramWindowTimeFold) chplan.Expr {
	boundsList := chplan.Expr(&chplan.ColumnRef{Name: hqAggBoundsListAlias})
	countsList := chplan.Expr(&chplan.ColumnRef{Name: hqAggCountsListAlias})
	tsList := chplan.Expr(&chplan.ColumnRef{Name: hqWindowTsListAlias})
	rowTime := prefixed(runsPrefixWindow, runsRowTimeSuffix)
	stamp := func(n chplan.Expr) chplan.Expr {
		return call(chplan.FnArrayWithConstant, n, bare(rowTime))
	}
	rows := call(
		chplan.FnArrayMap,
		lambda(classicBucketRowEntriesExpr(bare(paramRowBounds), bare(paramRowCounts), stamp), paramRowBounds, paramRowCounts, rowTime),
		boundsList, countsList, tsList,
	)
	// The +Inf rung exists in every layout — every row reports
	// `{le="+Inf"}` — so it folds over the whole window unfiltered.
	infCums := call(chplan.FnArrayMap, lambda(classicBucketRowTotalExpr(), paramRowCounts), countsList)
	return classicBucketRowsEntriesFlattened(runsPrefixWindow, rows, true, func(eb, ev, et chplan.Expr) chplan.Expr {
		return classicBucketBoundRunsExpr(runsPrefixWindow, eb, []chplan.Expr{ev, et}, func(union chplan.Expr, runs []chplan.Expr) chplan.Expr {
			run, order := runsPrefixWindow+runsRunSuffix, runsPrefixWindow+runsRunOrderSuffix
			rungs := call(chplan.FnArrayMap, lambda(fold(bare(run), bare(order)), run, order), runs[0], runs[1])
			ladder := call(chplan.FnArrayConcat, rungs, call(chplan.FnArray, fold(infCums, tsList)))
			return call(chplan.FnTuple, union, ladder)
		})
	})
}

// classicBucketMonotonicExpr is Prometheus's ensureMonotonicAndIgnoreSmallDeltas
// over an arbitrary ladder expression: a prefix maximum, so every rung is at
// least as high as the one below it, a NaN rung never raising the running
// maximum, and an equal later rung keeping the earlier one's value — exactly
// arrayMax over each prefix, without the per-rung prefix rescan that reads
// the ladder through a capture once per rung.
//
// A rung is a RECORD when it is the strict maximum of its prefix. Ranking the
// rungs (ascending value, and among equal values the EARLIER position
// ranking higher, NaN ranked 0) turns "strict maximum of the prefix" into
// "its rank equals the running maximum of the ranks"; the running maximum of
// integers is exact through the drawdown recurrence
// g[i] = max(0, g[i-1] + r[i-1] - r[i]) (arrayCumSumNonNegative over the
// negated rank differences), max = r + g. arrayFill then carries each
// record forward over the rungs that are not records. Two sorts and a few
// linear passes, all capture-free.
func classicBucketMonotonicExpr(ladder chplan.Expr) chplan.Expr {
	a, b := paramPrefixMaxA, paramPrefixMaxB
	return hqLet(paramPrefixMaxLadder, ladder, func(l chplan.Expr) chplan.Expr {
		idx, val := paramPrefixMaxIdx, paramPrefixMaxVal
		byValue := call(
			chplan.FnArraySort,
			lambda(call(chplan.FnTuple, bare(val), subExpr(&chplan.LitInt{V: 0}, bare(idx))), idx, val),
			call(chplan.FnArrayEnumerate, l), l,
		)
		key, perm := paramPrefixMaxKey, paramPrefixMaxPerm
		ranks := call(chplan.FnArraySort, lambda(bare(perm), key, perm), call(chplan.FnArrayEnumerate, l), byValue)
		nanUnranked := call(
			chplan.FnArrayMap,
			lambda(call(
				chplan.FnIf,
				call(chplan.FnIsNaN, bare(val)),
				&chplan.LitInt{V: 0},
				call(chplan.FnToInt64, bare(a)),
			), a, val),
			ranks, l,
		)
		return hqLet(paramPrefixMaxRank, nanUnranked, func(r chplan.Expr) chplan.Expr {
			diff := paramPrefixMaxDiff
			drawdown := call(chplan.FnArrayCumSumNonNegative, call(
				chplan.FnArrayMap,
				lambda(subExpr(&chplan.LitInt{V: 0}, bare(diff)), diff),
				call(chplan.FnArrayDifference, r),
			))
			runningMax := call(chplan.FnArrayMap, lambda(addExpr(bare(a), bare(b)), a, b), r, drawdown)
			return hqLet(paramPrefixMaxRunMax, runningMax, func(m chplan.Expr) chplan.Expr {
				record := call(chplan.FnArrayMap, lambda(&chplan.Binary{
					Op:    chplan.OpAnd,
					Left:  &chplan.Binary{Op: chplan.OpGt, Left: bare(a), Right: &chplan.LitInt{V: 0}},
					Right: &chplan.Binary{Op: chplan.OpEq, Left: bare(a), Right: bare(b)},
				}, a, b), r, m)
				return call(chplan.FnArrayFill, lambda(bare(key), val, key), l, record)
			})
		})
	})
}
