package chsql

// lwr_fanout_bound.go closes issue #2447.
//
// RangeBucketFanout (histogram_quantile range-vector lowerings,
// range_bucket_fanout.go) and RangeLWR (bare-selector range-vector
// lowerings, range_lwr.go) share lwrAnchorFanoutFrag's sample-side fan-out:
// one row per (raw sample, anchor whose staleness window covers it), via
// `arrayJoin` over each sample's ≤ Lookback/Step + 1 covered anchors,
// followed by a `GROUP BY <series>, anchor_ts` regroup. That GROUP BY is
// the same blocking operator #2429 (rate_window_fanout_bound.go) bounded
// for emitWindowedArrayExtrapolatedMatrix's identical arrayJoin-then-
// blocking-GROUP-BY shape: ClickHouse cannot emit a single output group
// until it has consumed the ENTIRE fanned-out input, and the intermediate
// row count is "rows x (Lookback/Step + 1)" — constant in the anchor grid
// width N (already gated elsewhere, see requireSubquerySampleBudget /
// format.MaxResolutionPoints), but "rows" itself is the raw, data-
// determined, plan-IR-unbounded sample count a high-cardinality selector
// matches.
//
// This axis was classified boundRuntimeNet (spill + max_memory_usage only)
// in internal/engine/resource_bound_classification_test.go — a considered
// choice, not an oversight, because unlike #2429's rate() case the
// per-GROUP accumulator most RangeBucketFanout/RangeLWR collapse AggFuncs
// use (argMax, sumForEach) is FIXED-SIZE, so total memory tracks GROUP
// cardinality rather than raw row count — the same already-accepted risk
// class as any ordinary Aggregate. But one real call site breaks that
// assumption: classicBucketWindowAggs (histogram_quantile_window.go), the
// `sum by(le)(rate(<bucket>[range]))` aggregated-classic-histogram path,
// collapses each (series, anchor) group with THREE groupArray aggregates
// (ExplicitBounds, BucketCounts, TimeUnix) — an accumulator that grows
// with in-window row count, not a fixed size, on top of an already
// array-valued (BucketCounts/ExplicitBounds) per-row payload. That is the
// #2429 mechanism exactly, and chplan.RangeBucketFanout.PeakIndependentOfGrid's
// own doc comment already carries the real, measured evidence: a route-A
// production query (Step=15s, Lookback=5m, OuterRange=1h) at just 93,608
// raw matched rows peaked at 4.01 GB (internal/optimizer's #2396 commit
// history) — no plan-time guard at all before this fix, protected only by
// ClickHouse's own spill + max_memory_usage.
//
// Calibration (real testcontainers ClickHouse 25.8-alpine, 1 GiB cap — the
// CERBERUS_CH_QUERY_MAX_MEMORY default — a synthetic classic-histogram
// dataset shaped after the #2396 route-A production grid: Step=15s,
// Lookback=5m [261 samples/series over a 1h05m seed window], 1h query
// window, `histogram_quantile(0.95, sum by (le, route) (increase(<bucket>
// [5m])))` — `by(le, ...)`, not a bare `by(route)`, is REQUIRED to reach
// the array-domain groupArray path at all (histogramAggShapeLowerable,
// histogram_quantile.go:1058); a by-clause omitting `le` legitimately
// resolves to the empty ordinary-float-bucket fallback instead. A unique
// query_id per run, per #2429's own harness-pitfall lesson):
//
//	series   seeded rows   peak memory (BEFORE this fix)
//	   700       182,700     740 MB (68.9%)
//	   750       195,750     703-773 MB (65.5-72.0%)
//	   800       208,800     919-955 MB (85.6-88.9%)
//	   900       234,900   1,063 MB (99.0%, right at the edge)
//	   950       247,950   REJECTED: real ClickHouse code 241
//	                       MEMORY_LIMIT_EXCEEDED (99.7% before abort)
//
// AFTER this fix, at the SAME 1,000/1,500-series scale that used to OOM
// outright: a clean 422 in ~2.3s at 16-22% of cap (the LIMIT+probe
// short-circuits almost immediately once truncated) instead of a ~6s
// ClickHouse-side abort. 700-800 series (the last genuinely safe range)
// still complete unaffected — 4,000,000 sits below where real risk starts
// (800 series) while staying comfortably above every legitimate query this
// repo's own fixtures exercise.
//
// A companion sweep confirmed point 4 of the issue this file closes
// (#2447): RangeLWR (bare-selector range queries, whose collapse uses
// argMax — a FIXED-size accumulator, unlike RangeBucketFanout's
// aggregated-path groupArray trio) is far safer at the identical raw row
// count — at 950 series (RangeBucketFanout's own OOM-adjacent point) RangeLWR
// used only 18% of cap, and stayed under cap through 10,000 series (95%,
// the first sign of real pressure) — so the SAME 4,000,000-row threshold,
// shared via lwrAnchorFanoutFrag, is a defense-in-depth belt for RangeLWR
// rather than a bound it is realistically close to tripping in practice.
//
// (see the PR that closed #2447 for the exact numbers this file's constant
// was calibrated against, gathered via a throwaway test/perf/nightly
// harness modelled on realch_perfnightly_integration_test.go, deleted
// before merge — this doc comment IS the retained record, per
// rate_window_fanout_bound.go's own precedent).
//
// The bound is applied at the FAN-OUT SOURCE itself — inside
// rangeLWRFanoutFrag, upstream of BOTH of its consumers
// (rangeLWRCollapseFrag's argMax collapse and
// emitAggregateRangeLWRFusedDistinctCount's uniqExact fast path,
// aggregate_range_lwr_fusion.go) — and inside emitRangeBucketFanout,
// upstream of its own collapse GROUP BY regardless of which AggFuncs a
// given lowering hands it. Protecting the shared source once, rather than
// each collapse call site separately, means a construction site cannot
// forget to opt in (the same reasoning rateWindowFanoutBoundedSourceFrag's
// own single call site already established for its one consumer).
//
// Design mirrors rateWindowFanoutBoundedSourceFrag exactly — see that
// file's doc comment for the fuller design history (four designs tried,
// why a LIMIT alone doesn't suffice, why the truncation probe must be an
// INDEPENDENT unwindowed count() on a second read rather than a window
// function annotating the bounded result directly).
const (
	// maxLWRAnchorFanoutRows caps the total number of pre-GROUP-BY fanout
	// rows (one per (sample, covered anchor)) that may enter
	// RangeBucketFanout's or RangeLWR's collapse GROUP BY in one query.
	// Calibrated against a real testcontainers ClickHouse dataset shaped
	// after the #2396 production route-A grid (Step=15s, Lookback=5m,
	// classic-histogram BucketCounts/ExplicitBounds payload — see the file
	// doc comment for the full sweep): the real OOM boundary sits between
	// 800 series (still completes, 85.6-88.9% of the 1 GiB cap) and 900-950
	// series (99.0%, then a genuine ClickHouse code-241
	// MEMORY_LIMIT_EXCEEDED abort). Post-fix, 700-800 series still complete
	// unaffected while 1,000+ series now get a clean, cheap 422 instead of
	// the OOM. Recalibrate by binary search against a real ClickHouse if
	// this drifts — see the file doc comment for the harness pitfall
	// (unique query_id per run) and design rationale to preserve when
	// doing so.
	maxLWRAnchorFanoutRows = 4_000_000
)

// LWRAnchorFanoutBudgetMessage is the throwIf message
// lwrFanoutGuardFrag raises.
const LWRAnchorFanoutBudgetMessage = "histogram/LWR sample fanout exceeds the series-times-anchors resource bound"

// lwrFanoutBoundedSourceFrag wraps fanoutSource — the sample-side
// LWR-style anchor fan-out SELECT shared by RangeBucketFanout and
// RangeLWR (lwrAnchorFanoutFrag), one row per (sample, covered anchor),
// upstream of either node's regroup GROUP BY — with a hard LIMIT plus a
// truncation-detecting guard, and returns the bounded, guarded result as
// the new source for that GROUP BY.
//
// probeColumn names a column fanoutSource is guaranteed to carry
// unchanged (both callers pass their node's own TimestampCol) — used only
// for the truncation probe's reduced-width second read, mirroring
// rateWindowFanoutBoundedSourceFrag's srcTs argument.
//
// Unlike rateWindowFanoutBoundedSourceFrag, which enumerates fanoutSource's
// column set explicitly (groupFrags / srcTs / valueColumn /
// temporalityColumn), this uses a bare `SELECT *` at both the bounded and
// guarded layers: fanoutSource's own column set already varies by caller
// (RangeBucketFanout projects `*` from its Input plus anchor_ts;
// rangeLWRFanoutFrag projects an explicit four/five-column list plus
// anchor_ts) and is opaque to this shared helper, so `*` passes whatever
// fanoutSource emits through unchanged — the same passthrough
// `emitRangeBucketFanout` itself already relies on for its own first fan-out
// layer.
func lwrFanoutBoundedSourceFrag(fanoutSource Frag, probeColumn string) Frag {
	// The real short-circuit. No blocking operator sits between this LIMIT
	// and the underlying scan/arrayJoin, so ClickHouse stops pulling
	// upstream data once maxLWRAnchorFanoutRows+1 rows are produced.
	bounded := NewQuery().From(fanoutSource)
	bounded.Select(Star())
	bounded.Limit(maxLWRAnchorFanoutRows + 1)

	// A second, independently LIMIT-bounded read of fanoutSource, reduced
	// to a single scalar count() — deliberately NOT a window function on
	// `bounded` itself. See rateWindowFanoutBoundedSourceFrag's doc
	// comment (design 3/4) for why every window-function variant tried
	// forces full materialisation of the whole LIMIT-bounded set before it
	// can annotate even one row, defeating the bound at the scale this
	// file needs to admit.
	probe := NewQuery().From(fanoutSource)
	probe.Select(Col(probeColumn))
	probe.Limit(maxLWRAnchorFanoutRows + 1)
	probeCount := NewQuery().From(probe.Frag())
	probeCount.Select(As(Call("count"), "n"))

	guarded := NewQuery().From(bounded.Frag())
	guarded.Select(Star())
	guarded.Where(lwrFanoutGuardFrag(probeCount))

	return guarded.Frag()
}

// lwrFanoutGuardFrag renders the WHERE predicate that aborts the query
// when probeCount — a scalar subquery counting an independent
// LIMIT-bounded copy of the same fan-out — shows the LIMIT truncated the
// input: the count landing on maxLWRAnchorFanoutRows+1 can only happen if
// the true fanned-out row count was at least that large.
//
//	throwIf((<probeCount>) > maxLWRAnchorFanoutRows, LWRAnchorFanoutBudgetMessage) = 0
//
// `throwIf` returns 0 when it does not fire, so `= 0` keeps every row once
// the guard has passed. Mirrors rateWindowFanoutGuardFrag exactly.
func lwrFanoutGuardFrag(probeCount *QueryBuilder) Frag {
	return Eq(
		Call(
			"throwIf",
			Gt(Subquery(probeCount), InlineLit(int64(maxLWRAnchorFanoutRows))),
			InlineLit(LWRAnchorFanoutBudgetMessage),
		),
		InlineLit(int64(0)),
	)
}
