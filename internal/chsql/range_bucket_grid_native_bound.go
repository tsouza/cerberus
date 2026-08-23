package chsql

// range_bucket_grid_native_bound.go closes issue #2486: before this file
// existed, RangeBucketGridNative's per-(series, `le` rung) native aggregate
// (Level 1 of emitRangeBucketGridNative's own doc comment —
// timeSeriesRateToGrid / timeSeriesResetsToGrid, each producing one
// anchor-wide array per GROUP BY group) fed straight into ClickHouse with no
// plan-time bound at all, protected only by ClickHouse's own spill +
// max_memory_usage. At real production scale (test/perf/nightly's
// classic_histogram_quantile_by_route sentinel: 3,741 series, 12
// rungs/series, a ~4h20m/1m-step window = 261 anchors) that genuinely
// reaches a ClickHouse code-241 MEMORY_LIMIT_EXCEEDED abort — a materially
// worse failure mode than RangeBucketFanout's own guard
// (lwr_fanout_bound.go), which short-circuits in ~2s at 16-22% of cap via a
// LIMIT-based probe BEFORE the expensive computation runs.
//
// Cost axis: series cardinality x anchor grid width, NOT lwr_fanout_bound.go's
// raw-fan-out-row-count axis. This node has no per-SAMPLE arrayJoin fan-out
// at all — Level 0/0b reduce every stored row down to its own (series, `le`
// rung) rows before any aggregate runs, and Level 1's GROUP BY then
// collapses those down to one row per (series, rung) GROUP, each holding
// THREE arrays of length NumAnchors (rate, presence, anchor-timestamp) —
// unconditional on how many raw samples fed that group. So the real cost
// driver is `groups x anchors`, where `groups` is series cardinality times
// the number of distinct `le` rungs a series' own layout carries (12 for
// the real production classic-histogram sample this was calibrated
// against). This is exactly the axis issue #2486 itself names.
//
// Why the guard sits BEFORE Level 1, not after it (the mistake an earlier
// version of this file made). The obvious reuse of lwrFanoutBoundedSourceFrag
// / lwrFanoutGuardFrag (internal/chsql/lwr_fanout_bound.go) —
// wrapping Level 2's ArrayJoin explode output the way
// range_bucket_window_slide_bound.go wraps its own UNION ALL source — LOOKS
// right (Level 2's own ArrayJoin is not itself blocking), but real
// testcontainers calibration caught why it is wrong FOR THIS NODE: unlike
// window-slide's UNION ALL arms, Level 1's GROUP BY sits strictly upstream
// of the ArrayJoin, and lwrFanoutBoundedSourceFrag's own two-independent-reads
// shape means an oversized query pays for that GROUP BY's own anchor-wide
// per-group array state TWICE (once building the LIMIT-bounded read, once
// building the independent truncation probe) before the throwIf guard ever
// gets a chance to fire. Measured directly: wrapping Level 2 let a
// 5,835,960-row query (130 anchors, real 3,741-series/12-rung sample) run
// all the way to a genuine ClickHouse code-241 MEMORY_LIMIT_EXCEEDED abort
// at 99.9% of cap — inside Level 1's own per-row cumulative-reading
// expression, not the guard's own probe — even though 5,835,960 is
// comfortably past the 4,000,000 threshold the guard was supposed to
// reject it at. The guard fired correctly at a SMALLER oversized query
// (90 anchors, 4,040,280 rows, cleanly rejected at 76.6% of cap in 4.6s)
// only because that scale did not yet double-pay past the cap — a narrow,
// unreliable protection band that would NOT have caught the real
// production incident (261 anchors, 11,716,812 rows) this issue exists to
// fix.
//
// The fix: probe a CHEAP proxy computed BEFORE Level 1's expensive
// aggregate ever runs, not the expensive aggregate's own (bounded) output.
// bucketGridGroupCountBoundedSourceFrag wraps Level 0b's rungs source (the
// per-(series, rung) row population, no aggregate function at all) with a
// plain (no-aggregate) GROUP BY that counts DISTINCT (series, rung) groups —
// call it `groups`, a query whose own cost is proportional to raw scanned
// row count, NOT to NumAnchors — and multiplies that count by NumAnchors, a
// Go-computed constant requiring no query at all (r.NumAnchors(), known at
// plan time from Start/End/Step alone). `groups x NumAnchors > maxRows`
// throwIf-rejects BEFORE Level 1's GROUP BY consumes the anchor-wide array
// state the real risk lives in — true short-circuiting, unlike the
// Level-2-wrap mistake above and unlike range_bucket_window_slide_bound.go's
// own accepted (and documented) "does not shrink the work upstream of
// itself" trade-off: there is no comparably cheap upstream probe available
// for window-slide's shape (its own blocking operator IS the sort whose
// input the UNION ALL arms already fully are), but this node's own
// arithmetic — GROUP CARDINALITY times a plan-time CONSTANT — has one.
//
// Calibration (real testcontainers ClickHouse 25.9-alpine, 1 GiB cap — the
// CERBERUS_CH_QUERY_MAX_MEMORY default — a synthetic classic-histogram
// dataset shaped after the real production sample
// test/perf/nightly/sentinels.go's classic_histogram_quantile_by_route
// exercises: 3,741 series, 11 ascending finite ExplicitBounds [0.005s..10s]
// + Inf overflow (12 rungs/series, so `groups` = 3,741 x 12 = 44,892),
// ~350 samples/series at a real ~46s cadence over a ~4.5h captured span —
// matching the real sample's row-count/series-count ratio
// (1,317,183 rows / 3,741 series). Step=1m, Range=5m, sweeping NumAnchors
// (a query_range window) with a unique query_id per run, per
// lwr_fanout_bound.go's own harness-pitfall lesson — first WITHOUT this
// guard (single-pass peak memory, establishing the real safe/unsafe
// boundary), then WITH it (confirming the throwIf fires cleanly, cheaply,
// and BEFORE Level 1's own expensive state, at every oversized point
// including the real production incident's own 261-anchor full window):
//
//	anchors   groups x anchors   peak memory, NO guard   result WITH this guard
//	    5          224,460        6.5% of cap             (guard not needed; passes)
//	   10          448,920       10.5% of cap             (guard not needed; passes)
//	   20          897,840       18.3% of cap             (guard not needed; passes)
//	   30        1,346,760       27.3% of cap             (guard not needed; passes)
//	   40        1,795,680       34.7% of cap             (guard not needed; passes)
//	   50        2,244,600       46.8% of cap             (guard not needed; passes)
//	   60        2,693,520       53.6% of cap             (guard not needed; passes)
//	   90        4,040,280       (not measured, over budget)   REJECTED: 8.6% of cap, <1s
//	  130        5,835,960       (not measured, over budget)   REJECTED: 12.2% of cap, <1s
//	  180        8,080,560       (not measured, over budget)   REJECTED: 15.5% of cap, 1.4s
//	  261 (full production window)  11,716,812  (not measured, over budget)   REJECTED: 23.3%
//	                                             of cap, 1.5s — the exact real-production
//	                                             incident this issue exists to fix
//	  350       15,712,200       (not measured, over budget)   REJECTED: 24.5% of cap, 1.5s
//	  450       20,201,400       (not measured, over budget)   REJECTED: 23.1% of cap, 1.4s
//
// (an EARLIER, wrong version of this guard — the Level-2-wrap mistake this
// file's own design-history section above documents — was ALSO measured at
// 90/130 anchors before being discarded: it rejected 90 cleanly at 76.6% of
// cap in 4.6s, but 130 anchors ran past it into a genuine ClickHouse
// code-241 MEMORY_LIMIT_EXCEEDED abort at 99.9% of cap — the guard-selection
// evidence for switching designs, not this guard's own behaviour.)
//
// 4,000,000 sits comfortably above the last measured-safe point
// (2,693,520 rows / 53.6% of cap at 60 anchors, ~2x real margin) while
// every point past it — including the full production window — now gets a
// clean, cheap rejection in under 1.5s at 8-25% of cap, independent of how
// far over budget the query is (the probe's own cost is O(raw scanned
// rows), not O(NumAnchors), so a 20,201,400-groups-x-anchors query at 450
// anchors rejects just as fast and just as cheaply as one barely over
// budget at 90). Recalibrate by binary search against a real testcontainers
// ClickHouse if this drifts — see lwr_fanout_bound.go's own
// harness-pitfall note (unique query_id per run) and design rationale to
// preserve when doing so. The calibration test itself
// (TestCalibrateRangeBucketGridNativeBound) was deleted before this fix
// merged, per lwr_fanout_bound.go's own established precedent — this doc
// comment is the retained record.
const maxRangeBucketGridNativeRows = 4_000_000

// RangeBucketGridNativeBudgetMessage is the throwIf message
// bucketGridGroupCountGuardFrag raises when this node's own bound fires.
// Distinct text from RangeBucketFanoutBudgetMessage / RangeLWRFanoutBudgetMessage
// / RangeBucketWindowSlideBudgetMessage so a rejection's error message alone
// says which bound fired — see those constants' own doc for why that
// matters for classifyThrowIfGuardError and for a human reading a
// query_log entry.
const RangeBucketGridNativeBudgetMessage = "classic-histogram native rate grid exceeds the " +
	"series-times-rungs-times-anchors resource bound"

// bucketGridGroupCountBoundedSourceFrag wraps rungsSource — Level 0b's own
// per-(series, `le` rung) row population (emitRangeBucketGridNative's own
// "rungs" QueryBuilder), one row per (stored row, rung it reports), upstream
// of Level 1's blocking native-aggregate GROUP BY — with a throwIf guard
// keyed on a CHEAP group-count probe times the plan-time-known NumAnchors,
// and returns the guarded, otherwise-unchanged result as the new source for
// that GROUP BY.
//
// keyCols is the series-identity key Frag list (emitRangeBucketGridNative's
// own keyCols — bare Col(alias) references, safe to reuse: see this file's
// own doc for why rungsSource itself must ALSO be render-safe on a second
// splice, which bucketGridGroupKeyFrag's fresh-closure-per-call design
// already guarantees for the group keys threaded through Level 0).
// numAnchors is r.NumAnchors() — a plain Go int64, not a query. maxRows /
// message are this node's own maxRangeBucketGridNativeRows /
// RangeBucketGridNativeBudgetMessage.
//
// Unlike lwrFanoutBoundedSourceFrag, this helper does NOT also emit a
// LIMIT-bounded copy of rungsSource: rungsSource itself is cheap (no
// aggregate function, no anchor-width array), so there is nothing to
// truncate — only the probe's own DISTINCT-group count needs guarding
// against, and that count is compared against maxRows/numAnchors directly
// rather than materialising an oversized intermediate the way the
// arrayJoin-fan-out family's LIMIT does.
func bucketGridGroupCountBoundedSourceFrag(rungsSource Frag, keyCols []Frag, numAnchors, maxRows int64, message string) Frag {
	probeGroups := NewQuery().From(rungsSource)
	probeGroups.Select(keyCols...)
	probeGroups.Select(Col(bucketGridLeAlias))
	probeGroups.GroupBy(append(append([]Frag{}, keyCols...), Col(bucketGridLeAlias))...)
	probeCount := NewQuery().From(probeGroups.Frag())
	probeCount.Select(As(Call("count"), "n"))

	guarded := NewQuery().From(rungsSource)
	guarded.Select(Star())
	guarded.Where(bucketGridGroupCountGuardFrag(probeCount, numAnchors, maxRows, message))
	return guarded.Frag()
}

// bucketGridGroupCountGuardFrag renders the WHERE predicate that aborts the
// query when probeCount — a scalar subquery counting the DISTINCT (series,
// `le` rung) groups Level 1's own GROUP BY will collapse rungsSource into —
// times numAnchors (every one of those groups' own per-anchor array width,
// identical for every group by construction) would exceed maxRows:
//
//	throwIf((<probeCount>) * numAnchors > maxRows, message) = 0
//
// `throwIf` returns 0 when it does not fire, so `= 0` keeps every row once
// the guard has passed. Mirrors lwrFanoutGuardFrag's own shape (internal/chsql/
// lwr_fanout_bound.go) — same throwIf-in-a-WHERE idiom — with a scaled
// comparison in place of a bare count, since this node's risk axis is a
// PRODUCT of a queried quantity and a plan-time constant, not a queried
// quantity alone.
func bucketGridGroupCountGuardFrag(probeCount *QueryBuilder, numAnchors, maxRows int64, message string) Frag {
	return Eq(
		Call(
			"throwIf",
			Gt(Mul(Subquery(probeCount), InlineLit(numAnchors)), InlineLit(maxRows)),
			InlineLit(message),
		),
		InlineLit(int64(0)),
	)
}
