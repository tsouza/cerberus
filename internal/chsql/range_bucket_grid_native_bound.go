package chsql

import "time"

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
// Issue #2522 recalibration (real ClickHouse 25.9-alpine via `docker run`,
// same 1 GiB cap): the nightly e2e dashboard's OWN self-monitoring panel —
// `histogram_quantile(0.95, sum by (le, cerberus_ql) (rate(
// cerberus_queries_duration_seconds_bucket[5m])))` at a 24h/15s window
// (5,760 anchors) — started hitting this exact throwIf in run 32688649627,
// even though `cerberus_queries_duration_seconds` is a LOW-cardinality
// self-telemetry histogram (one series per distinct (cerberus.ql,
// http.Pattern route, ok/error result) combo — internal/telemetry/
// metrics.go's QueryTimer.Done — a few dozen to a couple hundred series
// even across all three heads, nowhere near the 3,741-series production
// sample above) with a WIDE anchor grid the original calibration table
// never exercised (it only varied anchors from 5 to 450 at a FIXED HIGH
// groups count of 44,892; #2522's own shape is the opposite corner: LOW
// groups, HIGH anchors).
//
// Direct real-ClickHouse measurement of that unexercised corner (16 rungs —
// QueryDurationBoundaries' 15 finite bounds + Inf — 5,760 anchors, a
// realistic ~120 rows/series density matching the e2e stack's 10s OTLP
// PeriodicReader default over the run's own ~20-minute cluster uptime):
//
//	series   groups x anchors   peak memory (realistic density)
//	   44          4,055,040      9.6% of cap   — REJECTED by the old 4,000,000 bound
//	  100          9,216,000      8.1% of cap   — REJECTED by the old bound; this is #2522's real shape
//	  200         18,432,000     14.5% of cap   — comfortably safe
//	  500         46,080,000     21.9% of cap   — comfortably safe
//	1,000         92,160,000     42.7% of cap   — comfortably safe
//
// So the old 4,000,000 threshold was a genuine false positive for this
// shape: #2522's real query used well under 10% of the memory cap yet was
// rejected. A floor-density (2 rows/series — the adversarial case for
// THIS axis; floor density measured HIGHER percent-of-cap than realistic
// density at the same groups x anchors, 37.6% vs 8.1% at 9,216,000) sweep
// at the same 16-rung/5,760-anchor shape found the real safe/unsafe
// boundary for the groups x anchors axis alone:
//
//	series   groups x anchors   peak memory (floor density)
//	  163         15,022,080     37.6% of cap  — safe
//	  217         19,998,720     75.1% of cap  — safe
//	  271         24,975,360     75.1% of cap  — safe (plateau, not rising)
//	  326         30,044,160     75.1% of cap  — safe (still the same plateau)
//	  651         59,996,160     REJECTED: real ClickHouse code-241
//	                             MEMORY_LIMIT_EXCEEDED (57.1% reported before
//	                             the abort)
//
// 20,000,000 sits below the floor-density-confirmed-safe plateau (safe
// through at least 30,044,160, with the real OOM cliff only starting
// somewhere before 59,996,160 — a >2x margin below the nearest confirmed
// failure) AND comfortably passes every realistic-density point measured
// up to 4.6x this value (92,160,000 at 42.7% of cap) — while giving
// #2522's own real query (~9-20M formula units, depending on the actual
// route/ql/result cardinality live on a given nightly run) several-fold
// headroom rather than sitting right at its own edge.
//
// A genuinely separate finding surfaced by this same investigation, NOT
// fixed here (fixed by issue #2523, see this file's own "Density guard"
// section below): `groups x anchors` alone is not a complete cost model —
// a HIGH-density, comparatively LOW-groups-x-anchors query can
// independently OOM. Reproduced directly: 3,741 series x 12 rungs x 60
// anchors (2,693,520 groups x anchors — under BOTH the old 4,000,000 and
// this file's new 20,000,000 threshold) genuinely hit a real ClickHouse
// code-241 MEMORY_LIMIT_EXCEEDED at ~350 samples/series density (matching
// the ORIGINAL #2486 production sample's own row/series ratio), while the
// identical shape at floor density (2 rows/series) used only ~11% of cap.
// That means today's single-axis (groups x anchors) guard — both before
// and after this recalibration — does not actually protect the real
// #2486 production incident's own density profile at realistic scale; it
// only ever exercised the axis this file bounds, never the density axis.
//
// Recalibrate by binary search against a real ClickHouse if this drifts —
// see lwr_fanout_bound.go's own harness-pitfall note (unique query_id per
// run) and design rationale to preserve when doing so. The calibration
// test itself (a throwaway `docker run clickhouse/clickhouse-server`
// harness, not TestCalibrateRangeBucketGridNativeBound — that one was
// already deleted before #2504 merged) was deleted before this fix merged
// too, per lwr_fanout_bound.go's own established precedent — this doc
// comment is the retained record.
const maxRangeBucketGridNativeRows = 20_000_000

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

// # Density guard (issue #2523)
//
// bucketGridGroupCountBoundedSourceFrag's own `groups x anchors` axis above
// is NOT a complete cost model on its own — see this file's header doc's
// own "A genuinely separate finding" paragraph. Real ClickHouse 25.9-alpine
// measurement (a `docker run` container, 1 GiB `max_memory_usage` cap —
// matching CERBERUS_CH_QUERY_MAX_MEMORY's default — sweeping BOTH the
// `groups x anchors` axis AND raw scanned-row density together, NOT
// trusting issue #2523's own prose alone) found the real interaction is
// genuinely two-part and ADDITIVE, not a single product of the two axes:
//
//  1. Level 0's bucketGridRungsFrag computes, for every stored row, a
//     per-rung cumulative reading via an O(width) filter-sum evaluated
//     ONCE PER RUNG the row itself carries — an O(width^2) cost PER ROW,
//     paid entirely upstream of Level 1's GROUP BY and of
//     bucketGridGroupCountBoundedSourceFrag's own probe. This term is
//     `rawRows x width^2` and is essentially INDEPENDENT of `groups x
//     anchors` (it never touches an anchor at all).
//  2. Level 1's own native aggregate (timeSeriesRateToGrid /
//     timeSeriesResetsToGrid) turns out NOT to be "unconditional on how
//     many raw samples fed that group" the way this file's original #2486
//     design doc assumed — real measurement directly refutes that: at a
//     `groups x anchors` value already close to
//     maxRangeBucketGridNativeRows (17,625,600 — the exact #2522 pinned
//     regression shape, TestRangeBucketGridNativeBound_PassesLowCardinalityWideAnchorShape),
//     adding as few as 30,000 raw rows (500 samples/series, nowhere near
//     the ~350-samples/series #2486 reference density) was ALREADY enough
//     to hit a real ClickHouse code-241 MEMORY_LIMIT_EXCEEDED inside
//     AggregatingTransform — the `groups x anchors` skeleton alone already
//     consumes most of the 1 GiB budget near that ceiling, leaving very
//     little headroom for ANY additional raw-row cost.
//
// # Calibration
//
// 16 real ClickHouse 25.9-alpine points (docker run, 1 GiB cap), combining
// `costUnits = (groups x anchors) + (rawRows x width^2)` where rawRows is
// the COUNT of raw stored rows the scan actually reads (bucketGridDensityBoundedSourceFrag's
// own probe over the SAME Input source Level 0 unnests, under the SAME
// scan time bound) and width is the real per-row `length(ExplicitBounds)`
// (max over the scanned rows, not assumed uniform — see "Generalising to
// heterogeneous widths" below):
//
//	shape (groups x anchors)   raw rows    width   costUnits      real result
//	2,693,520 (3,741s x 12r x 60a)  7,482      11    3,598,842      safe (d2, floor)
//	2,693,520                      74,820      11   11,746,740      safe (d20)
//	2,693,520                     187,050      11   25,326,570      safe (d50)
//	2,693,520                     336,690      11   43,433,010      safe (d90)
//	2,693,520                     654,675      11   81,909,195      safe (d175)
//	2,693,520                     785,610      11   97,752,330      OOM  (d210)
//	2,693,520                     935,250      11  115,858,770      OOM  (d250)
//	2,693,520                   1,309,350      11  161,124,870      OOM  (d350 -- the
//	                                                                  real #2486/#2408
//	                                                                  reference density)
//	17,625,600 (60s x 51r x 5,760a)   120      50   17,925,600      safe (floor -- the
//	                                                                  #2522 pinned
//	                                                                  regression itself)
//	17,625,600                     30,000      50   92,625,600      OOM  (d500)
//	17,625,600                    300,000      50  767,625,600      OOM  (d5000)
//	17,625,600                    900,000      50  2,282,625,600    OOM  (d15000)
//	9,216,000 (100s x 16r x 5,760a) 12,000      15   11,916,000      safe (d120, the
//	                                                                  #2522 REALISTIC-
//	                                                                  density shape)
//	72,000 (60s x 12r x 100a)       60,000      11    7,332,000      safe (isolation
//	                                                                  sweep)
//
// Every safe point's costUnits sits below 81,909,195 (the tightest: 3,741
// series x 12 rungs x 60 anchors at density 175). Every OOM point's
// costUnits sits above 92,625,600 (the tightest: the #2522 pinned-regression
// SHAPE at density 500 — 17,625,600 groups x anchors leaves so little
// headroom that even this modest density trips it). [maxRangeBucketGridNativeDensityUnits]
// is set at 85,000,000 — inside that gap, ~4% above the tightest confirmed-safe
// point and ~8% below the tightest confirmed-OOM point — rather than at either
// edge, and correctly classifies all 16 points above including BOTH of this
// issue's own required checks: the real #2486/#2408 reference density (d350,
// 161,124,870, comfortably rejected) and the #2522 pinned regression itself
// (17,925,600, comfortably passed, ~4.75x headroom below the threshold).
//
// A pure single-axis model on EITHER quantity alone was tried first and
// discarded: raw-rows-alone, width^2-alone, `(groups x anchors) x rawRows`,
// and `anchors x rawRows` were each checked against the same 16 points and
// each produced either a false positive against the #2522 pinned regression
// or a false negative against one of the two OOM shapes above (`anchors x
// rawRows` in particular wrongly flags the #2522 REALISTIC-density shape:
// 5,760 x 12,000 = 69,120,000, close to where that model would need to
// reject the real #2486 density too) — the additive two-term form is the
// only one of the tried candidates consistent with every measured point.
//
// # Generalising to heterogeneous per-row widths
//
// Real ExplicitBounds length can vary row-to-row (a `by(route)` group can
// mix producers with different bucket configurations) — bucketGridDensityBoundedSourceFrag's
// own probe reads `max(length(ExplicitBounds))` over the ACTUAL scanned
// rows, the same "widest row governs" generalisation classic_bucket_merge_bound.go's
// widestRowBucketWidthExpr already established for the cross-series merge
// stage: `rawRows x maxWidth^2 >= rawRows x avgWidth^2` for any real
// distribution of per-row widths, so a heterogeneous scan is judged AT
// LEAST as costly as its average-width equivalent, never less.
//
// # Overflow safety
//
// [maxRangeBucketGridNativeDensityClampedRows] / [maxRangeBucketGridNativeDensityClampedWidth]
// clamp rawRows and width BEFORE the square + multiply — width^2 alone
// could otherwise overflow a pathological/malformed ExplicitBounds length
// long before any real query reaches it. 100,000,000 x 100,000^2 =
// 1 x 10^18, comfortably under Int64's ~9.2x10^18 ceiling, while both caps
// sit orders of magnitude above any width or raw-row count this
// calibration or any legitimate production workload reaches.
//
// # Placement
//
// Wired via bucketGridDensityBoundedSourceFrag, layered on top of (NOT
// replacing) bucketGridGroupCountBoundedSourceFrag's own axis1 guard —
// see emitRangeBucketGridNative's own call site. Both guards fire from
// independent, cheap probes upstream of Level 1's expensive native
// aggregate; axis1 alone already rejects anything with `groups x anchors`
// past maxRangeBucketGridNativeRows regardless of density, so this second
// guard's only NEW effect is catching density-driven danger axis1 alone
// passes through — exactly this issue's own gap.
//
// Recalibrate by binary search against a real ClickHouse (not chDB) if
// this drifts — same harness-pitfall caveat (unique query_id per run) as
// this file's own axis1 calibration above. The calibration harness itself
// (a throwaway `docker run clickhouse/clickhouse-server` test, not
// TestCalibrate2523) was deleted before this fix merged, per this file's
// own established precedent — this doc comment is the retained record.
const (
	// maxRangeBucketGridNativeDensityUnits bounds `(groups x anchors) +
	// (rawRows x width^2)` — see this file's own "Density guard" doc above
	// for the real ClickHouse calibration this was picked against.
	maxRangeBucketGridNativeDensityUnits = 85_000_000

	// maxRangeBucketGridNativeDensityClampedRows / …ClampedWidth are NOT
	// behavioral thresholds — see this file's "Overflow safety" doc above.
	// They exist purely so `rawRows x width^2` can never overflow Int64
	// arithmetic; a query anywhere near these scales is already rejected
	// by maxRangeBucketGridNativeDensityUnits (or by
	// maxRangeBucketGridNativeRows) long before either clamp matters.
	maxRangeBucketGridNativeDensityClampedRows  = 100_000_000
	maxRangeBucketGridNativeDensityClampedWidth = 100_000
)

// RangeBucketGridNativeDensityBudgetMessage is the throwIf message
// bucketGridDensityGuardFrag raises when the density guard fires. Distinct
// text from RangeBucketGridNativeBudgetMessage (axis1's own message) so a
// rejection's error message alone says which of the TWO independent
// RangeBucketGridNative bounds fired.
const RangeBucketGridNativeDensityBudgetMessage = "classic-histogram native rate grid exceeds the " +
	"density-weighted raw-row resource bound"

// bucketGridDensityBoundedSourceFrag wraps source — the axis1-guarded
// rungs-consuming source bucketGridGroupCountBoundedSourceFrag already
// returned — with a SECOND throwIf guard combining a fresh groups probe
// (mirrors bucketGridGroupCountBoundedSourceFrag's own probeGroups/probeCount
// shape, over groupsSource / keyCols) with a raw-row-density probe read
// directly off inputSource (the RangeBucketGridNative.Input subplan Level 0
// itself unnests, under the identical scan time bound
// maybePushRangeScanTimeBound already pushes onto Level 0 — tsCol / start /
// end / offsetNS / rangeNS mirror that call exactly). numAnchors / maxUnits
// / message parallel bucketGridGroupCountBoundedSourceFrag's own numAnchors
// / maxRows / message.
//
// Both probes are cheap: the groups probe is the SAME no-aggregate
// DISTINCT-group-count shape axis1 already uses (re-derived fresh rather
// than reusing axis1's own probe subquery, matching this file's own
// established "render fresh, don't share a Frag across an unrelated second
// use" caution), and the density probe is a plain `count()` + `max(length(...))`
// over inputSource with no arrayJoin, no GROUP BY, and no native aggregate
// at all — strictly cheaper than either axis1's probe or Level 0's own
// unnest.
func bucketGridDensityBoundedSourceFrag(
	source, groupsSource Frag, keyCols []Frag,
	inputSource Frag, tsCol, boundsCol string, start, end time.Time, offsetNS, rangeNS int64,
	numAnchors, maxUnits int64, message string,
) Frag {
	probeGroups := NewQuery().From(groupsSource)
	probeGroups.Select(keyCols...)
	probeGroups.Select(Col(bucketGridLeAlias))
	probeGroups.GroupBy(append(append([]Frag{}, keyCols...), Col(bucketGridLeAlias))...)
	probeGroupsCount := NewQuery().From(probeGroups.Frag())
	probeGroupsCount.Select(As(Call("count"), "n"))

	rawRowsProbe := NewQuery().From(inputSource)
	rawRowsProbe.Select(As(Call("count"), "n"))
	maybePushRangeScanTimeBound(rawRowsProbe, tsCol, start, end, offsetNS, rangeNS)

	maxWidthProbe := NewQuery().From(inputSource)
	maxWidthProbe.Select(As(Call("max", Call("length", Col(boundsCol))), "w"))
	maybePushRangeScanTimeBound(maxWidthProbe, tsCol, start, end, offsetNS, rangeNS)

	guarded := NewQuery().From(source)
	guarded.Select(Star())
	guarded.Where(bucketGridDensityGuardFrag(probeGroupsCount, rawRowsProbe, maxWidthProbe, numAnchors, maxUnits, message))
	return guarded.Frag()
}

// bucketGridDensityGuardFrag renders the WHERE predicate that aborts the
// query when the combined cost this file's "Density guard" doc calibrates
// — `(groupsProbe x numAnchors) + (clamp(rawRowsProbe) x clamp(maxWidthProbe)^2)`
// — would exceed maxUnits:
//
//	throwIf((<groupsProbe>) * numAnchors +
//	        least(<rawRowsProbe>, clampRows) * least(<maxWidthProbe>, clampWidth) ^ 2
//	        > maxUnits, message) = 0
//
// The `groups x anchors` term is intentionally NOT clamped here — it
// mirrors bucketGridGroupCountGuardFrag's own unclamped construct exactly
// (same probe shape, same lack-of-clamp precedent: numAnchors is a
// plan-time-bounded constant and probeGroups is bounded by real scanned row
// cardinality). Only the NEW rawRows/width quantities this guard
// introduces are clamped — see this file's "Overflow safety" doc.
func bucketGridDensityGuardFrag(groupsProbe, rawRowsProbe, maxWidthProbe *QueryBuilder, numAnchors, maxUnits int64, message string) Frag {
	groupsXAnchors := Mul(Subquery(groupsProbe), InlineLit(numAnchors))
	clampedRows := Call("least", Subquery(rawRowsProbe), InlineLit(int64(maxRangeBucketGridNativeDensityClampedRows)))
	clampedWidth := Call("least", Subquery(maxWidthProbe), InlineLit(int64(maxRangeBucketGridNativeDensityClampedWidth)))
	marginal := Mul(clampedRows, Mul(clampedWidth, clampedWidth))
	cost := Add(groupsXAnchors, marginal)
	return Eq(
		Call("throwIf", Gt(cost, InlineLit(maxUnits)), InlineLit(message)),
		InlineLit(int64(0)),
	)
}
