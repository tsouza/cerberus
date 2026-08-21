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
// Three prior designs were tried and each was empirically disproven against
// the real query before landing on this one — all three guarded the WRONG
// stage, downstream of the GROUP BY described above rather than upstream of
// it, so by the time each guard's own count could see the data, the
// GROUP BY had already paid the memory cost the guard existed to prevent:
//
//  1. throwIf gated on a global, unpartitioned `count() OVER ()` placed
//     after the regrouped window pass. Aborted a small toy case, but let
//     the real query OOM — a global window can't emit until it has seen
//     the whole (already-materialised) input.
//  2. throwIf gated on an ORDERED RUNNING `count() OVER (ORDER BY
//     anchor_ts ROWS UNBOUNDED PRECEDING)` in the same downstream position,
//     on the theory that a monotonic running count could fire earlier.
//     Still let the real query OOM, and made an intermediate case worse.
//  3. A genuine `LIMIT` placed after the regrouped window pass (same
//     downstream position as 1 and 2, just with a real pipeline
//     short-circuit instead of a throwIf race). Verified against an
//     isolated 20M-row/100,000-partition synthetic dataset with NO spill
//     settings (which OOMs unguarded) — worked there. Still let the real
//     query OOM: the synthetic test had no blocking aggregation upstream
//     of the LIMIT, so the LIMIT could short-circuit the scan feeding it;
//     the real query's LIMIT sat downstream of the fanout→regroup GROUP BY
//     described above, which is itself blocking and had already
//     materialised every group's array before the LIMIT ever ran.
//
// The fix that actually targets the expensive stage: place the `LIMIT`
// directly on `fanoutSource` — the sample-side fanout, upstream of the
// GROUP BY, where nothing blocking sits between it and the underlying
// scan — so the LIMIT is a genuine pipeline short-circuit exactly as it was
// in the synthetic test. A cheap post-LIMIT `count() OVER ()` (cheap
// because the LIMIT has already bounded its input) then detects truncation
// — count landing on maxRateWindowFanoutRows+1 can only happen if the true
// fanned-out row count was at least that large — and a `throwIf` on that
// count aborts the query BEFORE the expensive GROUP BY runs, rather than
// after.
const (
	// maxRateWindowFanoutRows caps the total number of pre-GROUP-BY fanout
	// rows (one per (sample, covered anchor)) that may enter
	// emitWindowedArrayExtrapolatedMatrix's regroup GROUP BY in one query.
	// Calibrated by binary search against the same real, corrected
	// (unique-query-id) testcontainers dataset described above: the
	// genuinely safe 30-anchor case's true fanout row count sits between
	// 1,000,000 and 1,250,000 (its own natural, unguarded cost is 63.5% of
	// the 1 GiB cap — already tight, since this metric's cardinality alone
	// leaves little headroom); the OOMing 60-anchor case's fanout count is
	// well above 2,000,000. 1,600,000 sits above the 30-anchor point (so
	// that sentinel, and any query of similar shape, is never touched) with
	// margin for natural per-query variance, while every larger case tried
	// (60/120/180/260 anchors) is rejected with peak memory bounded to
	// 68-72% of cap regardless of how much larger the true, unguarded fanout
	// would have been — proof the LIMIT itself, not just the throwIf, is
	// what bounds worst-case memory here.
	maxRateWindowFanoutRows = 1_600_000

	// rateWindowFanoutCountAlias names the `count() OVER ()` column the
	// guard reads. A plain SELECT-list expression nothing downstream
	// references gets pruned by ClickHouse's analyzer before it can have
	// any effect — the exact trap vector_join.go's matchCheckGuardFrag
	// already documents for an identical throwIf shape — so this alias
	// exists purely to be read back by a WHERE clause in the very next
	// query layer, guaranteeing it survives.
	rateWindowFanoutCountAlias = "rate_window_fanout_total_rows"
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
// time regroup needs it (this function's first version dropped
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

	// Stage 1: the real short-circuit. No blocking operator sits between
	// this LIMIT and the underlying scan/arrayJoin, so ClickHouse stops
	// pulling upstream data once maxRateWindowFanoutRows+1 rows are
	// produced.
	bounded := NewQuery().From(fanoutSource)
	selectFanoutCols(bounded)
	bounded.Limit(maxRateWindowFanoutRows + 1)

	// Stage 2: cheap now that the input is LIMIT-bounded — this can never
	// see more than maxRateWindowFanoutRows+1 rows.
	counted := NewQuery().From(bounded.Frag())
	selectFanoutCols(counted)
	counted.Select(As(Window(Call("count"), nil, nil), rateWindowFanoutCountAlias))

	// Stage 3: the guard, evaluated before regroup's GROUP BY ever runs.
	guarded := NewQuery().From(counted.Frag())
	selectFanoutCols(guarded)
	guarded.Where(rateWindowFanoutGuardFrag(rateWindowFanoutCountAlias))

	return guarded.Frag()
}

// rateWindowFanoutGuardFrag renders the WHERE predicate that aborts the
// query when rateWindowFanoutCountAlias shows the LIMIT truncated the
// input — i.e. the count landed exactly on maxRateWindowFanoutRows+1,
// which can only happen if the true fanned-out row count was at least that
// large:
//
//	throwIf(<countCol> > maxRateWindowFanoutRows, RateWindowFanoutBudgetMessage) = 0
//
// `throwIf` returns 0 when it does not fire, so `= 0` keeps every row once
// the guard has passed.
func rateWindowFanoutGuardFrag(countCol string) Frag {
	return Eq(
		Call(
			"throwIf",
			Gt(Col(countCol), InlineLit(int64(maxRateWindowFanoutRows))),
			InlineLit(RateWindowFanoutBudgetMessage),
		),
		InlineLit(int64(0)),
	)
}

// IsRateWindowFanoutBudgetError reports whether err is the deliberate query
// abort rateWindowFanoutGuardFrag plants, rather than a backend failure —
// the sibling of IsHistogramMergeBudgetError / IsManyToManyMatchError /
// IsInfoConflictingLabelError / IsDuplicateLabelsetError, and exists for
// the same reason: the HTTP layer must classify the abort as a deliberate,
// query-shape-fault rejection (422 execution error), not the 502 bucket
// every other ClickHouse execute failure falls into.
func IsRateWindowFanoutBudgetError(err error) bool {
	return isThrowIfError(err, RateWindowFanoutBudgetMessage)
}
