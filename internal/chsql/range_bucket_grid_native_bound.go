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
// fixed here: `groups x anchors` alone is not a complete cost model — a
// HIGH-density, comparatively LOW-groups-x-anchors query can independently
// OOM. Reproduced directly: 3,741 series x 12 rungs x 60 anchors
// (2,693,520 groups x anchors — under BOTH the old 4,000,000 and this
// file's new 20,000,000 threshold) genuinely hit a real ClickHouse
// code-241 MEMORY_LIMIT_EXCEEDED at ~350 samples/series density (matching
// the ORIGINAL #2486 production sample's own row/series ratio), while the
// identical shape at floor density (2 rows/series) used only ~11% of cap.
// That means today's single-axis (groups x anchors) guard — both before
// and after this recalibration — does not actually protect the real
// #2486 production incident's own density profile at realistic scale; it
// only ever exercised the axis this file bounds, never the density axis.
// Tracked separately (see the issue this recalibration's own PR files)
// rather than folded into this fix, because closing it properly needs a
// density-aware (or raw-row-volume-aware) second guard with its own
// dedicated real-ClickHouse calibration sweep, not a threshold number on
// this same single axis.
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
