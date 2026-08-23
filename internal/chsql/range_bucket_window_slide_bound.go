package chsql

// range_bucket_window_slide_bound.go closes the same class of gap issue
// #2447 closed for the arrayJoin fan-out (lwr_fanout_bound.go) — before
// this file existed, RangeBucketWindowSlide's UNION ALL anchor-injection
// source (real sample rows + one synthetic sentinel row per (series,
// anchor)) fed straight into a PARTITION BY / ORDER BY window sort with no
// plan-time bound at all, protected only by ClickHouse's own spill +
// max_memory_usage. Issue #2486 — filed from the CI image-floor work, not
// this design — found exactly that gap on RangeBucketGridNative (this
// node's rate-window sibling): it shipped with no resource-bound guard and
// genuinely OOMs at real scale. That is the cautionary precedent this file
// exists to not repeat: the bound lands in the SAME PR as the emitter, not
// as a follow-up.
//
// Cost axis. Unlike lwrFanoutBoundedSourceFrag's arrayJoin fan-out — whose
// row count is `raw_rows x (Lookback/Step + 1)`, quadratic-ish in the
// window/step ratio because each sample is replicated once per anchor its
// window covers — RangeBucketWindowSlide never replicates a sample: each
// real row appears exactly once, and the anchor side contributes exactly
// `series x anchors` sentinel rows regardless of Lookback. So this bound's
// axis is `scanned_rows + series x anchors`, independent of Lookback,
// mirroring how internal/engine's subqueryAnchorLoad already charges this
// node's own NumAnchors() against the PER-SERIES anchor budget (a
// different, complementary axis: that gate bounds one series' own anchor
// count against MaxQuerySamples before the query is even planned; this one
// bounds the TOTAL unioned row count the window sort actually receives).
//
// The two-independent-reads LIMIT+throwIf shape (bounded copy + a second,
// independently LIMIT-bounded scalar count() probe) is copied verbatim from
// lwrFanoutBoundedSourceFrag/lwrFanoutGuardFrag rather than re-derived: see
// that file's doc comment for the fuller design history (four designs
// tried, why a LIMIT alone doesn't suffice — a window function needs its
// whole PARTITION materialised before it emits even one row, so a LIMIT
// downstream of the sort does not bound the sort's own peak — and why the
// truncation probe must be an INDEPENDENT unwindowed count() on a second
// read rather than a window function annotating the bounded result
// directly).
//
// Calibration. maxRangeBucketWindowSlideRows is a REASONED, not yet
// chDB/testcontainers-measured, starting bound: this node's blocking
// operator is a PARTITION BY / ORDER BY sort ahead of the window function,
// which — like RangeBucketFanout's blocking collapse GROUP BY
// (lwr_fanout_bound.go) and unlike RangeLWR's fixed-size argMax
// accumulator — must hold data proportional to total row count rather than
// GROUP cardinality, so it is calibrated against the SAME order of
// magnitude as maxRangeBucketFanoutRows rather than
// maxRangeLWRFanoutRows's much larger fixed-accumulator bound, pending a
// real measurement. Recalibrate by binary search against a real
// ClickHouse the same way lwr_fanout_bound.go's own constants were
// calibrated (unique query_id per run — see that file's harness-pitfall
// note) once this path carries real traffic.
const maxRangeBucketWindowSlideRows = 4_000_000

// RangeBucketWindowSlideBudgetMessage is the throwIf message
// windowSlideFanoutGuardFrag raises. Distinct text from
// RangeBucketFanoutBudgetMessage / RangeLWRFanoutBudgetMessage so a
// rejection's error message alone says which bound fired — see those
// constants' own doc for why that matters for classifyThrowIfGuardError
// and for a human reading a query_log entry.
const RangeBucketWindowSlideBudgetMessage = "classic-histogram window-slide anchor injection exceeds the " +
	"series-times-anchors resource bound"

// windowSlideBoundedSourceFrag wraps unionSrc — the UNION ALL of real
// sample rows and per-anchor sentinel rows RangeBucketWindowSlide's emit
// builds (rwsRawNsAlias / rwsCountsFAlias / rwsBoundsFAlias / rwsIsAnchorAlias
// / rwsAnchorNsAlias) — with a hard LIMIT plus a truncation-detecting guard,
// mirroring lwrFanoutBoundedSourceFrag's exact two-independent-reads shape.
//
// probeColumn names a column unionSrc is guaranteed to carry unchanged
// (rwsRawNsAlias, present and Int64-typed on both UNION ALL arms) — used
// only for the truncation probe's reduced-width second read.
func windowSlideBoundedSourceFrag(unionSrc Frag, probeColumn string) Frag {
	bounded := NewQuery().From(unionSrc)
	bounded.Select(Star())
	bounded.Limit(maxRangeBucketWindowSlideRows + 1)

	probe := NewQuery().From(unionSrc)
	probe.Select(Col(probeColumn))
	probe.Limit(maxRangeBucketWindowSlideRows + 1)
	probeCount := NewQuery().From(probe.Frag())
	probeCount.Select(As(Call("count"), "n"))

	guarded := NewQuery().From(bounded.Frag())
	guarded.Select(Star())
	guarded.Where(windowSlideFanoutGuardFrag(probeCount))
	return guarded.Frag()
}

// windowSlideFanoutGuardFrag renders the WHERE predicate that aborts the
// query when probeCount — a scalar subquery counting an independent
// LIMIT-bounded copy of the same UNION ALL source — shows the LIMIT
// truncated the input, mirroring lwrFanoutGuardFrag exactly:
//
//	throwIf((<probeCount>) > maxRangeBucketWindowSlideRows, RangeBucketWindowSlideBudgetMessage) = 0
func windowSlideFanoutGuardFrag(probeCount *QueryBuilder) Frag {
	return Eq(
		Call(
			"throwIf",
			Gt(Subquery(probeCount), InlineLit(int64(maxRangeBucketWindowSlideRows))),
			InlineLit(RangeBucketWindowSlideBudgetMessage),
		),
		InlineLit(int64(0)),
	)
}
