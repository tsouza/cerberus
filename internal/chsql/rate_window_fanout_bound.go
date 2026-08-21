package chsql

// rate_window_fanout_bound.go closes issue #2429.
//
// emitWindowedArrayExtrapolatedMatrix (range_window.go) builds each series'
// per-anchor sample window in two stages: a sample-side fanout — one row
// per (raw sample, anchor it falls inside), via `arrayJoin` over the
// anchors each sample covers — followed by a `GROUP BY <series>, anchor_ts`
// that regroups the fanned-out rows back into a per-(series, anchor)
// `window_pairs` array via `groupArrayIf`. That GROUP BY is a blocking
// operator: ClickHouse cannot emit a single output group until it has
// consumed the ENTIRE fanned-out input, because until every row has been
// seen, no group's array is known to be complete. For a high-cardinality
// series set over a wide anchor grid, the fanout multiplies row count by
// the number of anchors each sample covers (up to range/step), and the
// GROUP BY then has to hold every series×anchor group's accumulating array
// in memory simultaneously — this is the documented mechanism behind the
// run-27277793810 `rate(...)` 2.12 GiB OOM the emitter's own doc comment
// already names, and the same mechanism the #2429 nightly-harness sentinel
// (`sum by(method)(rate(<counter>[5m]))` over 3,740 real series) rediscovered.
//
// The first calibration pass against a real testcontainers ClickHouse
// 25.8-alpine loaded with #2411's real single-day sample (3,740 series)
// under a 1 GiB cap reported the OOM boundary sitting way out at a
// 260-anchor grid (91.5% of cap), with 180 anchors reading as comfortably
// safe (56.8%). Those specific numbers were wrong — not the mechanism, the
// measurement. optcorpus.CHQueryLogSource.FinishedByQueryID folds every
// system.query_log row sharing a query_id within its lookback window
// together via max(memory_usage)/max(exception_code); the ad hoc
// calibration harness reused a fixed, non-unique query_id across repeated
// manual runs, so every reading was silently the MAX across every prior
// run in the past hour, including runs from earlier, different candidate
// designs. Once the harness was fixed to mint a unique query_id per run,
// the real boundary turned out to sit far tighter: the SAME 3,740-series
// dataset OOMs already at 60 anchors (100% of cap), with only 30 anchors
// reading as genuinely safe (63.5% of cap, unguarded). Anyone recalibrating
// this bound should mint a unique query_id per run for exactly this reason.
//
// Four designs were tried, in order, before landing on this one:
//
//  1. throwIf gated on a global, unpartitioned `count() OVER ()` placed
//     AFTER the regrouped window pass. Aborted a small toy case, but let
//     the real query OOM — a global window can't emit until it has seen
//     the whole (already-materialised) input.
//
//  2. throwIf gated on an ORDERED RUNNING `count() OVER (ORDER BY
//     anchor_ts ROWS UNBOUNDED PRECEDING)`, same downstream position, on
//     the theory that a monotonic running count could fire earlier. Still
//     let the real query OOM, and made an intermediate case worse.
//
//  3. A genuine `LIMIT` placed BEFORE the regroup GROUP BY (upstream of it,
//     unlike 1 and 2), immediately followed by a `count() OVER ()` reading
//     that LIMIT-bounded input to detect truncation. The LIMIT itself
//     correctly bounded the real query's OOM — but the `count() OVER ()`
//     right after it reintroduced design 1's exact problem one layer down:
//     a global window function still has to fully materialise its ENTIRE
//     input (all maxRows+1 of it) before it can annotate a single row,
//     regardless of how that input arrived. Caught not by the OOM case but
//     by a REGRESSION on a previously-safe query: test/perf/smoke's
//     spill_high_cardinality_groupby sentinel (10,000 series, 241 anchors,
//     small per-row payload) never came close to the LIMIT, yet its own
//     peak memory rose from ~61% to ~87% of cap purely from this design's
//     presence — the buffering cost of the window function, not the guard
//     firing, was the regression.
//
//  4. keep design 3's LIMIT (still the correct, genuine short-circuit),
//     but move the truncation-detecting count() OFF the LIMIT-bounded
//     result and onto an independent SECOND read: a plain, unwindowed
//     `count()` over its own separate LIMIT-bounded copy of fanoutSource,
//     rather than a window function annotating `bounded` itself. A window
//     function — global OR ordered-running, tried both — has to fully
//     materialise its entire input before it can annotate a single row,
//     and that alone (independent of whether the guard ever fires)
//     defeats the bound at the scale this file needs to admit: caught not
//     by the OOM case but by a regression on a previously-safe query,
//     test/perf/smoke's spill_high_cardinality_groupby sentinel (10,000
//     series, small per-row payload, true fanout ~2.8M rows, never close
//     to truncating) — its own peak memory rose from ~61% to ~87-91% of
//     cap purely from the window function's buffering cost. A plain
//     count() is a streaming running total with no such requirement, so
//     this second read costs at most one more ordinary pass over the same
//     already time-bounded window
//     (TestExtrapolatedMatrixDeltaPrefixUsesOneScanAndWindowedLevels was
//     updated from one scan to two for exactly this reason — see its own
//     comment for why this is not the retention-wide rescan that test
//     originally guarded against).
//
//     Re-measuring the real production query under this design turned up
//     a second, welcome effect: the mere PRESENCE of a LIMIT clause on the
//     fanout — independent of whether it ever truncates — measurably
//     improves ClickHouse's own memory behaviour for this plan shape (the
//     real 60-anchor case, which OOM'd outright with no guard at all, now
//     completes safely at ~39% of cap once wrapped in a LIMIT comfortably
//     above its own true row count). Plausibly a planner/streaming side
//     effect of a LIMIT being present at all, not specifically caused by
//     hitting it — not depended upon for correctness (the throwIf is what
//     actually bounds the unsafe cases), but it is what makes a single
//     threshold work for both the real production shape and the smoke
//     tier's synthetic shape at once; see maxRateWindowFanoutRows's own
//     comment for the measured numbers.
const (
	// maxRateWindowFanoutRows caps the total number of pre-GROUP-BY fanout
	// rows (one per (sample, covered anchor)) that may enter
	// emitWindowedArrayExtrapolatedMatrix's regroup GROUP BY in one query.
	// Calibrated against two real testcontainers datasets: #2411's real
	// single-day production sample (3,740 series, wide merged Attributes),
	// and test/perf/smoke's own spill_high_cardinality_groupby fixture
	// (10,000 series, 241-anchor grid, narrow single-key Attributes — its
	// own true fanout row count sits close to 2.8M). At 2,800,000: the real
	// production dataset is safe through 60 anchors (417MB / 38.8% of the
	// 1 GiB cap) and cleanly, cheaply rejected from 120 anchors on (peak
	// memory bounded to 1-12% of cap, since the LIMIT-then-scalar-count
	// probe short-circuits almost immediately once truncated); the smoke
	// fixture's own legitimate query passes. Recalibrate by binary search
	// against both datasets if either shape's own numbers drift — see the
	// file doc comment for the harness pitfall (unique query_id per run)
	// and design rationale (why a plain scalar count(), not count() OVER
	// ()) to avoid when doing so.
	maxRateWindowFanoutRows = 2_800_000
)

// RateWindowFanoutBudgetMessage is the throwIf message
// rateWindowFanoutGuardFrag raises. Lives in chsql (not chplan) because,
// unlike #2385's histogram merge guard, this bound is built and consumed
// entirely inside the emitter — internal/promql never constructs or needs
// to know about it, so there is no cross-package import-cycle constraint
// forcing the message into chplan.
const RateWindowFanoutBudgetMessage = "range window sample fanout exceeds the series-times-anchors resource bound"

// rateWindowFanoutBoundedSourceFrag wraps fanoutSource — the sample-side
// fanout SELECT built in emitWindowedArrayExtrapolatedMatrix, one row per
// (sample, covered anchor), upstream of its regroup GROUP BY — with a hard
// LIMIT plus a truncation-detecting guard, and returns the result as the
// new source for that GROUP BY.
//
// groupFrags, srcTs, valueColumn and (when hasTemporality)
// temporalityColumn are exactly fanoutSource's own per-row columns, plus
// the anchor_ts column the fanout itself produces; this must project the
// identical set through every layer below or a column goes missing by the
// time regroup needs it (an earlier version of this function dropped
// delta_prefix_pairs at a different call site the same way and broke every
// query hitting that path with a code 47 "unknown identifier").
func rateWindowFanoutBoundedSourceFrag(
	fanoutSource Frag, groupFrags []Frag, srcTs, valueColumn, temporalityColumn string, hasTemporality bool,
) Frag {
	selectFanoutCols := func(q *QueryBuilder) {
		q.Select(groupFrags...)
		q.Select(Col(srcTs))
		q.Select(Col(valueColumn))
		if hasTemporality {
			q.Select(Col(temporalityColumn))
		}
		q.Select(Col("anchor_ts"))
	}

	// The real short-circuit. No blocking operator sits between this LIMIT
	// and the underlying scan/arrayJoin, so ClickHouse stops pulling
	// upstream data once maxRateWindowFanoutRows+1 rows are produced.
	bounded := NewQuery().From(fanoutSource)
	selectFanoutCols(bounded)
	bounded.Limit(maxRateWindowFanoutRows + 1)

	// A second read of fanoutSource, independently bounded by the SAME
	// LIMIT and the SAME pushed-down time-window WHERE clause as `bounded`
	// — not a retention-wide rescan, the thing
	// TestExtrapolatedMatrixDeltaPrefixUsesOneScanAndWindowedLevels exists
	// to prevent (see its own history, #2240) — reduced to a single scalar
	// count(). Deliberately NOT a window function on `bounded` itself: see
	// the file doc comment's design-3/4 notes for why every window-function
	// variant tried (global, and ordered-running) forces full
	// materialisation of the whole LIMIT-bounded set before it can annotate
	// even one row, and why that alone — independent of whether the guard
	// ever fires — reintroduces enough memory pressure to defeat the bound
	// for a real, wide-payload production row at the scale this bound must
	// admit. A plain, unwindowed count() is a streaming running total with
	// no such requirement, so this second read costs at most one more
	// ordinary pass over the same already-bounded time window — the
	// cheapest mechanism that actually protects the real query rather than
	// merely appearing to.
	probe := NewQuery().From(fanoutSource)
	probe.Select(Col(srcTs))
	probe.Limit(maxRateWindowFanoutRows + 1)
	probeCount := NewQuery().From(probe.Frag())
	probeCount.Select(As(Call("count"), "n"))

	guarded := NewQuery().From(bounded.Frag())
	selectFanoutCols(guarded)
	guarded.Where(rateWindowFanoutGuardFrag(probeCount))

	return guarded.Frag()
}

// rateWindowFanoutGuardFrag renders the WHERE predicate that aborts the
// query when probeCount — a scalar subquery counting an independent
// LIMIT-bounded copy of the same fanout — shows the LIMIT truncated the
// input: the count landing on maxRateWindowFanoutRows+1 can only happen if
// the true fanned-out row count was at least that large.
//
//	throwIf((<probeCount>) > maxRateWindowFanoutRows, RateWindowFanoutBudgetMessage) = 0
//
// `throwIf` returns 0 when it does not fire, so `= 0` keeps every row once
// the guard has passed.
func rateWindowFanoutGuardFrag(probeCount *QueryBuilder) Frag {
	return Eq(
		Call(
			"throwIf",
			Gt(Subquery(probeCount), InlineLit(int64(maxRateWindowFanoutRows))),
			InlineLit(RateWindowFanoutBudgetMessage),
		),
		InlineLit(int64(0)),
	)
}
