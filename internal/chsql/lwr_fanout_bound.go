package chsql

import "context"

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
// the array-domain groupArray path at all
// (internal/promql/histogram_quantile.go:histogramAggShapeLowerable); a
// by-clause omitting `le` legitimately
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
//
// Issue #2667: both maxRangeBucketFanoutRows and maxRangeLWRFanoutRows below
// are now operator-overridable — CERBERUS_CH_RANGE_BUCKET_FANOUT_MAX_ROWS /
// CERBERUS_CH_RANGE_LWR_FANOUT_MAX_ROWS — rather than fixed at their shipped
// calibration forever. #2429/#2470's own hardcoded ceilings each already
// caused a real production incident (a legitimate query rejected, or a
// dangerous one admitted) that could only be corrected with a full
// code-change-plus-release cycle, because there was no operator-facing knob
// to reach for in between. chsql itself may not import internal/config
// (.go-arch-lint.yml: chsql: mayDependOn: [chplan, spansscan]), so the
// resolved override travels in as an explicit ctx value — WithRangeBucketFanoutMaxRows
// / WithRangeLWRFanoutMaxRows below, mirroring WithDeltaPrefixLookback's own
// context-threading shape (range_window.go) exactly — parsed upstream in
// internal/engine (which already owns this same seam for
// chsql.WithDeltaPrefixLookback / chsql.WithDeltaPrefixReadEnabled, both
// route A's emitForHead and route B's routeBExecCtx) and read once per Emit
// call via rangeBucketFanoutMaxRowsFromCtx / rangeLWRFanoutMaxRowsFromCtx.
// The named Go constants below remain the shipped, calibrated DEFAULTS — an
// operator override changes what a deployment runs with, never what a fresh
// install or an unwired test (the spec/golden lane, property tests) gets.
const (
	// maxRangeBucketFanoutRows caps the total number of pre-GROUP-BY fanout
	// rows (one per (sample, covered anchor)) that may enter
	// RangeBucketFanout's collapse GROUP BY in one query. Calibrated against
	// a real testcontainers ClickHouse dataset shaped after the #2396
	// production route-A grid (Step=15s, Lookback=5m, classic-histogram
	// BucketCounts/ExplicitBounds payload — see the file doc comment for the
	// full sweep): the real OOM boundary sits between 800 series (still
	// completes, 85.6-88.9% of the 1 GiB cap) and 900-950 series (99.0%,
	// then a genuine ClickHouse code-241 MEMORY_LIMIT_EXCEEDED abort).
	// Post-fix, 700-800 series still complete unaffected while 1,000+ series
	// now get a clean, cheap 422 instead of the OOM. Recalibrate by binary
	// search against a real ClickHouse if this drifts — see the file doc
	// comment for the harness pitfall (unique query_id per run) and design
	// rationale to preserve when doing so.
	//
	// This bound is specific to RangeBucketFanout's classicBucketWindowAggs
	// collapse (the groupArray-trio accumulator that grows with in-window
	// row count — see the file doc comment). RangeLWR's own collapse
	// (argMax, a fixed-size accumulator) does NOT share this bound — see
	// maxRangeLWRFanoutRows — because sharing one number picked for the
	// dangerous path against the cheap one is exactly the miscalibration
	// issue #2470 fixed: a real nightly sentinel (a plain gauge selector
	// queried as a range vector, no rate()/increase() wrapper, 7,024 real
	// series over a 4h20m/1m-step window) fans out to 7,762,072 rows —
	// comfortably real and comfortably safe for RangeLWR's own accumulator
	// (measured: 70.3% of cap, see maxRangeLWRFanoutRows), but well past
	// this constant, and got rejected by it before #2470.
	maxRangeBucketFanoutRows = 4_000_000

	// maxRangeLWRFanoutRows is RangeLWR's own bound, separate from
	// maxRangeBucketFanoutRows (issue #2470) because RangeLWR's argMax-based
	// per-series collapse is a FIXED-size accumulator — total memory tracks
	// GROUP cardinality (series x anchors), not raw fanned-in row count —
	// unlike RangeBucketFanout's groupArray trio, which accumulates over
	// every row in a group. Real testcontainers ClickHouse 25.8-alpine
	// calibration (1 GiB cap), using this repo's own real nightly gauge
	// sample (test/perf/nightly/testdata/samples/kube_pod_status_reason.
	// parquet — 1,676,160 rows, 7,024 real series) against the EXACT query
	// shape that surfaced this gap (`sum by (reason) (kube_pod_status_
	// reason)`, a bare gauge selector over a query_range with no rate()/
	// increase() wrapper, Step=1m, Lookback=5m [Prometheus default], real
	// captured window 2026-08-18 09:05:00-13:25:00 UTC — sentinels.go's own
	// pod_status_reason_gauge):
	//
	//	fanned rows (real)   peak memory (post-#2470 fix)
	//	     7,762,072         754 MB (70.3% of cap)   — the real sample, unmodified
	//	    31,048,288         693 MB (64.6% of cap)   — same sample, ~4x row-duplicated
	//	    62,096,576         REJECTED (guard fired)  — same sample, ~8x row-duplicated
	//
	// (fanned row count read via a raw probe mirroring lwrAnchorFanoutFrag's
	// own arrayJoin formula; peak memory via system.query_log against the
	// real HTTP handler; row-duplication holds series count, window and
	// step — and therefore GROUP cardinality — constant while scaling only
	// the raw sample density, to isolate row-count sensitivity from group-
	// cardinality sensitivity). The 31M point using LESS memory than the
	// 7.8M point, not more, is the real confirmation of the fixed-size-
	// accumulator theory above: this query shape's memory cost does not
	// scale with fanned row count in the range measured. 40,000,000 sits
	// comfortably above every point actually measured passing (the 8x/62M
	// point never got a real memory reading — the guard rejected it before
	// the query ran, which is its job) with real, not assumed, headroom;
	// recalibrate by binary search against a real ClickHouse if this drifts
	// — same harness-pitfall caveat (unique query_id per run) as
	// maxRangeBucketFanoutRows. The calibration test itself
	// (TestCalibrateRangeLWRThreshold) was deleted before this fix merged,
	// per rate_window_fanout_bound.go's own established precedent — this
	// doc comment is the retained record.
	maxRangeLWRFanoutRows = 40_000_000

	// rangeBucketFanoutFoldCostUnitsPerGiB bounds a SECOND, independent axis
	// from maxRangeBucketFanoutRows above: not the raw pre-collapse
	// sample-side fanout (rows x (Lookback/Step + 1), constant in the anchor
	// grid width per this file's header doc), but what the COLLAPSE's whole
	// OUTPUT costs the stage above it — which for a groupArray-accumulating
	// collapse (classicBucketWindowAggs, [histogram_quantile_window.go], and
	// its native/exponential-histogram sibling expHistogramWindowAggs,
	// [histogram_quantile_native_window.go]) is a SEPARATE, downstream,
	// per-group cost this file's own fanout bound never sees: the
	// array-valued fold (bucket-ladder merge for classic, the window fold
	// for native — exp_histogram_window_sample_bound.go) that consumes every
	// one of those groups. That guard bounds each GROUP's own cost
	// individually; it does not bound what all of them together cost, and
	// ClickHouse's vectorized execution does not process one group, release
	// its memory, then move to the next — it batches many groups' worth of
	// that array-fold work together, so a query whose PER-GROUP cost
	// individually passes that guard can still exhaust
	// CERBERUS_CH_QUERY_MAX_MEMORY once the group count itself grows with a
	// wide range at a fine step, even though the underlying raw-row scan and
	// the sample-side fanout above both stay far under their own ceilings
	// (cerberus issue #3468: the k3d/compose self-observability dashboard's
	// own P95-by-language panel — a bare `histogram_quantile(0.95, sum by
	// (cerberus_ql) (rate(cerberus_queries_duration_exp_hist[5m])))` — OOMs
	// ClickHouse directly, with NEITHER this file's own fanout guard NOR
	// exp_histogram_window_sample_bound.go's per-group guard ever firing,
	// once the query's anchor grid (range/step) grows while the underlying
	// data stays the same).
	//
	// # Why this counts a COST and not the groups
	//
	// #3468's first fix bounded the collapse's OUTPUT ROW COUNT — the plain
	// number of (series, anchor) groups — at 800. A flat group count is not
	// a usable proxy for that fold's memory, and cerberus issue #3514 is
	// what it cost: 26 ordinary compat-corpus queries (1h window, 10s step,
	// three demo series over `demo_shifting_latency_exp_hist`) were rejected
	// at 1,070-1,444 groups while peaking at 7-78 MB — under 8% of the 1 GiB
	// cap. The measurement below says why: at a FIXED group count, peak
	// memory moves by more than an order of magnitude with the bucket-ladder
	// WIDTH the flat count cannot see, and again with the group's in-window
	// SAMPLE count. A group-count ceiling high enough to admit those 26
	// queries with any margin sits above the count at which a wide-ladder
	// metric genuinely OOMs, so no value of it is both safe and usable.
	//
	// The cost this counts instead, per group, is
	//
	//	E + W^2      E = the group's accumulated payload in bucket-ladder
	//	             elements (byteSize of its accumulators / 8)
	//	             W = E / S, the ladder width, recovered from E and the
	//	             group's in-window sample count S
	//
	// and the bound is that cost SUMMED over every group the collapse
	// produced (rangeBucketFanoutFoldCostProbe, range_bucket_fanout.go).
	// Both terms are real: E = S x W is the payload the fold reads, and the
	// W^2 term is the dense per-group reshape the fold builds from it, which
	// the measurement shows is charged once per group INDEPENDENTLY of S.
	//
	// # Calibration
	//
	// Real docker-compose ClickHouse 26.5, 1 GiB cap (the
	// CERBERUS_CH_QUERY_MAX_MEMORY default), against
	// otel_metrics_exponential_histogram: `histogram_quantile(0.95, sum by
	// (route) (rate(<metric>[<range>])))` over a query_range grid at
	// step=15s, 18 series at varied Scale/Offset seeded at a 15s cadence.
	// The metric is seeded twice, at two bucket-ladder widths, so the width
	// axis a group count cannot see is measured rather than assumed. Peak
	// memory read from system.query_log; cost units computed from the same
	// probe SQL this bound emits:
	//
	//	W    S     groups   cost units   peak memory
	//	150   21    1,080    2.81e7        494 MB (46%)
	//	150   21    2,160    5.61e7        979 MB (91%)
	//	150   21    2,520    6.55e7      1,048 MB (97.6%)
	//	150   21    2,880    7.48e7      REJECTED: real ClickHouse code 241
	//	                                 MEMORY_LIMIT_EXCEEDED
	//	150   81    1,080    3.98e7        494 MB (46%)
	//	 40   21    2,160    6.70e6         83 MB (8%)
	//	 40  321    1,080    1.89e7        566 MB (53%)
	//
	// Across those points the cost tracks peak memory at 12.4-30.0 bytes per
	// unit — a 2.4x spread, and MONOTONE: every safe point sits below the
	// one that aborted. The same query_log-derived count over the 26 issue
	// #3514 queries tops out at 5.0e5 units, two orders of magnitude below.
	// A flat group count separates the same points by 34x in the wrong
	// direction (1,080 groups is 27 MB on the compat corpus and 494 MB at
	// W=150), which is the whole reason for the change.
	//
	// 15,000,000 units per GiB of cap is the ceiling that leaves the WORST
	// measured rate (30.0 bytes/unit, the wide-S point) landing at ~450 MB
	// per GiB — under half the cap — while clearing every #3514 query by
	// 30x or more at the 1 GiB default. It admits ~578 groups of the W=150
	// shape (~264 MB), ~859 of the S=321 shape (~450 MB) and ~4,800 of the
	// cheap W=40 shape (~185 MB): the number of groups now moves with what a
	// group actually costs, which is the point. Recalibrate by binary search
	// against a real ClickHouse (docker compose up --wait from the repo
	// root; see CLAUDE.md invariant 5) if this drifts, sweeping BOTH width
	// and samples-per-group — a sweep that moves only the group count is
	// what produced the bound this replaces.
	//
	// # Why a rate per GiB and not a fixed number
	//
	// The units count a proxy for BYTES (the 12.4-30.0 bytes/unit band
	// above), and the byte ceiling is the operator's own
	// CERBERUS_CH_QUERY_MAX_MEMORY. A ceiling fixed at the 1 GiB
	// calibration is wrong at every other cap: at 512 MiB it lands the
	// worst measured rate at 88% of the cap, and any cap below that is
	// defeated outright — the query the guard exists to refuse dies on
	// ClickHouse's own code 241 instead. RangeBucketFanoutFoldCostUnitsForMemory
	// scales this rate by the live cap, the same derivation the
	// RangeBucketGridNative density bound (internal/config's
	// rbgnDensityUnitsForMemory) and the exponential-histogram window bound
	// (internal/promql's ExpHistogramWindowCostUnitsForMemory) already use,
	// and internal/engine threads the derived value on every emit. This
	// constant is the rate; the value a deployment runs under is
	// rate x cap.
	//
	// Issue #3468: operator-overridable via
	// CERBERUS_CH_RANGE_BUCKET_FANOUT_GROUP_MAX_COST_UNITS, mirroring
	// maxRangeBucketFanoutRows / maxRangeLWRFanoutRows exactly (see their
	// own "Operator override" reasoning above) — this calibration is newer
	// and narrower than theirs (one measured shape family, not a
	// multi-metric production sweep), so the escape hatch matters more
	// here, not less. A positive override pins the ceiling and opts out of
	// the derivation.
	rangeBucketFanoutFoldCostUnitsPerGiB = 15_000_000
)

// bytesPerGiB is the divisor [RangeBucketFanoutFoldCostUnitsForMemory]
// reads the cap in. chsql may not import internal/config
// (.go-arch-lint.yml), so the constant is restated here rather than
// shared — the same restatement internal/promql carries.
const bytesPerGiB int64 = 1 << 30

// RangeBucketFanoutFoldCostUnitsForMemory derives the fold-cost ceiling
// from the ClickHouse per-query memory cap it defends:
// rangeBucketFanoutFoldCostUnitsPerGiB scaled by the cap, with the
// sub-GiB remainder credited proportionally so a 1.5 GiB cap is not
// rounded down to a 1 GiB one, and floored at 1 so an absurd cap can never
// yield a 0 that the ctx readers below would treat as "absent, use the
// default" — the opposite of a tiny ceiling.
//
// A non-positive cap means CERBERUS_CH_QUERY_MAX_MEMORY is unset — cerberus
// stamps no max_memory_usage at all then, so ClickHouse's own server limit
// is the only ceiling and there is no number to scale. The per-GiB rate
// applied once is the honest answer there: it is the bound a 1 GiB
// deployment gets, which is the shipped product default, and it is what
// every caller that threads no cap at all (the spec/golden lane, a direct
// chsql.Emit in a test) resolves to.
func RangeBucketFanoutFoldCostUnitsForMemory(chQueryMaxMemory int64) int64 {
	if chQueryMaxMemory <= 0 {
		return rangeBucketFanoutFoldCostUnitsPerGiB
	}
	units := (chQueryMaxMemory / bytesPerGiB) * rangeBucketFanoutFoldCostUnitsPerGiB
	rem := chQueryMaxMemory % bytesPerGiB
	units += (rem * rangeBucketFanoutFoldCostUnitsPerGiB) / bytesPerGiB
	return max(units, 1)
}

// ResolveRangeBucketFanoutMaxRows / ResolveRangeLWRFanoutMaxRows /
// ResolveRangeBucketFanoutFoldCostMaxUnits answer the SAME
// "override-or-default" question the *FromCtx readers below answer off a
// context, without needing one — for a caller (internal/engine's
// routeBExecCtx) that must resolve the effective whole-query bound BEFORE
// apportioning it to a shard's memory share, which a ctx-keyed lookup
// cannot do: stamping override/divisor through With… when override is 0
// would divide down to 0, and 0 reads as "unset" through the emitter's own
// accessors (rangeBucketFanoutRowBound and siblings), the OPPOSITE of an
// apportioned bound. The same contract ResolveRangeBucketGridNativeMaxRows
// established for the RangeBucketGridNative pair. override <= 0 answers the
// calibrated default; override > 0 is returned unchanged.
func ResolveRangeBucketFanoutMaxRows(override int64) int64 {
	if override > 0 {
		return override
	}
	return maxRangeBucketFanoutRows
}

func ResolveRangeLWRFanoutMaxRows(override int64) int64 {
	if override > 0 {
		return override
	}
	return maxRangeLWRFanoutRows
}

// ResolveRangeBucketFanoutFoldCostMaxUnits is the fold-cost member of the
// Resolve… family above, with one more input: its default is not a
// constant but [RangeBucketFanoutFoldCostUnitsForMemory] of the
// deployment's cap, so an unset override resolves to the cap-derived
// ceiling rather than to the 1 GiB calibration.
func ResolveRangeBucketFanoutFoldCostMaxUnits(override, chQueryMaxMemory int64) int64 {
	if override > 0 {
		return override
	}
	return RangeBucketFanoutFoldCostUnitsForMemory(chQueryMaxMemory)
}

// rangeBucketFanoutMaxRowsKey / rangeLWRFanoutMaxRowsKey are the unexported
// context keys carrying an operator-configured override for
// maxRangeBucketFanoutRows / maxRangeLWRFanoutRows (issue #2667) — see
// WithRangeBucketFanoutMaxRows / WithRangeLWRFanoutMaxRows and the
// corresponding *FromCtx readers below.
type rangeBucketFanoutMaxRowsKey struct{}

type rangeLWRFanoutMaxRowsKey struct{}

// rangeBucketFanoutFoldCostMaxUnitsKey is the unexported context key carrying
// the resolved fold-cost ceiling — the operator's override, or the value
// derived from the deployment's memory cap (issue #3468,
// RangeBucketFanoutFoldCostUnitsForMemory) — see
// WithRangeBucketFanoutFoldCostMaxUnits /
// rangeBucketFanoutFoldCostMaxUnitsFromCtx below.
type rangeBucketFanoutFoldCostMaxUnitsKey struct{}

// WithRangeBucketFanoutMaxRows returns ctx carrying n as the operator
// override for RangeBucketFanout's own fanout-row bound (otherwise
// maxRangeBucketFanoutRows). The caller — internal/engine's emitForHead /
// routeBExecCtx — only threads this when
// CERBERUS_CH_RANGE_BUCKET_FANOUT_MAX_ROWS is actually set: unlike
// WithDeltaPrefixLookback's zero-is-meaningful explicit opt-out, a fanout
// row bound of zero is never a legitimate operator intent (it would reject
// every query outright), so "never threaded" — not "threaded as zero" — is
// what selects the compiled-in default; see
// rangeBucketFanoutMaxRowsFromCtx.
func WithRangeBucketFanoutMaxRows(ctx context.Context, n int64) context.Context {
	return context.WithValue(ctx, rangeBucketFanoutMaxRowsKey{}, n)
}

// rangeBucketFanoutMaxRowsFromCtx recovers the bound
// WithRangeBucketFanoutMaxRows set, or maxRangeBucketFanoutRows (the
// compiled-in, calibrated default — see this file's own doc comment) when
// the caller never threaded one.
func rangeBucketFanoutMaxRowsFromCtx(ctx context.Context) int64 {
	if n, ok := ctx.Value(rangeBucketFanoutMaxRowsKey{}).(int64); ok {
		return n
	}
	return maxRangeBucketFanoutRows
}

// WithRangeLWRFanoutMaxRows / rangeLWRFanoutMaxRowsFromCtx mirror
// WithRangeBucketFanoutMaxRows / rangeBucketFanoutMaxRowsFromCtx exactly,
// for RangeLWR's own fanout-row bound (otherwise maxRangeLWRFanoutRows;
// CERBERUS_CH_RANGE_LWR_FANOUT_MAX_ROWS).
func WithRangeLWRFanoutMaxRows(ctx context.Context, n int64) context.Context {
	return context.WithValue(ctx, rangeLWRFanoutMaxRowsKey{}, n)
}

func rangeLWRFanoutMaxRowsFromCtx(ctx context.Context) int64 {
	if n, ok := ctx.Value(rangeLWRFanoutMaxRowsKey{}).(int64); ok {
		return n
	}
	return maxRangeLWRFanoutRows
}

// WithRangeBucketFanoutFoldCostMaxUnits /
// rangeBucketFanoutFoldCostMaxUnitsFromCtx mirror
// WithRangeBucketFanoutMaxRows / rangeBucketFanoutMaxRowsFromCtx, for
// RangeBucketFanout's COLLAPSE OUTPUT fold-cost bound
// (CERBERUS_CH_RANGE_BUCKET_FANOUT_GROUP_MAX_COST_UNITS) — a different axis
// from RangeBucketFanoutMaxRows' own pre-collapse sample fanout, see
// rangeBucketFanoutFoldCostUnitsPerGiB's own doc. One difference from that
// sibling: the value threaded is the RESOLVED ceiling, override or
// cap-derived (ResolveRangeBucketFanoutFoldCostMaxUnits), because the
// default is a function of the deployment's memory cap that only the
// caller knows; a caller that threads nothing gets the 1 GiB calibration
// (RangeBucketFanoutFoldCostUnitsForMemory(0)).
func WithRangeBucketFanoutFoldCostMaxUnits(ctx context.Context, n int64) context.Context {
	return context.WithValue(ctx, rangeBucketFanoutFoldCostMaxUnitsKey{}, n)
}

func rangeBucketFanoutFoldCostMaxUnitsFromCtx(ctx context.Context) int64 {
	if n, ok := ctx.Value(rangeBucketFanoutFoldCostMaxUnitsKey{}).(int64); ok {
		return n
	}
	return RangeBucketFanoutFoldCostUnitsForMemory(0)
}

// rangeBucketFanoutRowBound / rangeLWRFanoutRowBound return e's own resolved
// bound — e.rangeBucketFanoutMaxRows / e.rangeLWRFanoutMaxRows, seeded once
// from ctx inside chsql.Emit (emit.go) — falling back to
// maxRangeBucketFanoutRows / maxRangeLWRFanoutRows whenever the field reads
// as its Go zero value. That fallback is not merely the "no operator
// override" case Emit's own seeding already resolves (rangeBucketFanoutMaxRowsFromCtx
// returns the default for an unset ctx value, never 0) — it also covers
// every internal round-trip test in this package that constructs &emitter{}
// directly, bypassing chsql.Emit's ctx-seeding entirely (see e.g.
// range_window_fused_chdb_test.go's fusedDiffSQL). Unlike
// deltaPrefixLookbackNS, whose zero Go value IS a legitimate value ("no
// lower bound"), a maxRows of 0 read literally would reject every row of
// every such test outright, which very nearly happened. This accessor is
// what protects any FUTURE direct-&emitter{}-construction call site from
// repeating that.
func (e *emitter) rangeBucketFanoutRowBound() int64 {
	if e.rangeBucketFanoutMaxRows > 0 {
		return e.rangeBucketFanoutMaxRows
	}
	return maxRangeBucketFanoutRows
}

// rangeBucketFanoutFoldCostBound mirrors rangeBucketFanoutRowBound exactly,
// for e.rangeBucketFanoutFoldCostMaxUnits, falling back to the 1 GiB
// calibration (RangeBucketFanoutFoldCostUnitsForMemory's unset-cap answer)
// for a direct &emitter{} construction.
func (e *emitter) rangeBucketFanoutFoldCostBound() int64 {
	if e.rangeBucketFanoutFoldCostMaxUnits > 0 {
		return e.rangeBucketFanoutFoldCostMaxUnits
	}
	return RangeBucketFanoutFoldCostUnitsForMemory(0)
}

func (e *emitter) rangeLWRFanoutRowBound() int64 {
	if e.rangeLWRFanoutMaxRows > 0 {
		return e.rangeLWRFanoutMaxRows
	}
	return maxRangeLWRFanoutRows
}

// RangeBucketFanoutBudgetMessage is the throwIf message
// lwrFanoutGuardFrag raises for RangeBucketFanout's collapse.
const RangeBucketFanoutBudgetMessage = "histogram/LWR sample fanout exceeds the series-times-anchors resource bound"

// RangeLWRFanoutBudgetMessage is the throwIf message lwrFanoutGuardFrag
// raises for RangeLWR's collapse. Distinct text from
// RangeBucketFanoutBudgetMessage so a rejection's error message alone says
// which bound fired, for classifyThrowIfGuardError and for a human reading
// a query_log entry.
const RangeLWRFanoutBudgetMessage = "range LWR sample fanout exceeds the series-times-anchors resource bound"

// RangeBucketFanoutGroupBudgetMessage is the throwIf message
// lwrFanoutGuardFrag raises for RangeBucketFanout's COLLAPSE OUTPUT (issue
// #3468) — distinct text from RangeBucketFanoutBudgetMessage so a
// rejection's error message alone says which of the two axes fired: the
// pre-collapse sample fanout (RangeBucketFanoutBudgetMessage) or the
// post-collapse fold cost this constant names.
const RangeBucketFanoutGroupBudgetMessage = "histogram window fold exceeds the collapsed-payload resource bound"

// lwrFanoutBoundedSourceFrag wraps fanoutSource — the sample-side
// LWR-style anchor fan-out SELECT shared by RangeBucketFanout and
// RangeLWR (lwrAnchorFanoutFrag), one row per (sample, covered anchor),
// upstream of either node's regroup GROUP BY — with a hard LIMIT plus a
// truncation-detecting guard, and returns the bounded, guarded result as
// the new source for that GROUP BY.
//
// probeSource is the fan-out the truncation probe counts: the same
// arrayJoin over the same pruned input as fanoutSource, so its row count IS
// fanoutSource's, but free to omit per-row projections the collapse needs
// and the count does not (RangeBucketFanout's hoisted group keys). Each
// source is embedded verbatim, so whatever text probeSource leaves out is
// text the statement carries once instead of twice. RangeLWR passes the
// same Frag for both.
//
// probeColumn names a column probeSource is guaranteed to carry unchanged
// (both callers pass their node's own TimestampCol) — used only for the
// truncation probe's reduced-width second read, mirroring
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
// maxRows is the caller's own bound (maxRangeBucketFanoutRows or
// maxRangeLWRFanoutRows — issue #2470) and message is the throwIf text that
// names which one fired.
func lwrFanoutBoundedSourceFrag(fanoutSource, probeSource Frag, probeColumn string, maxRows int64, message string) Frag {
	// The real short-circuit. No blocking operator sits between this LIMIT
	// and the underlying scan/arrayJoin, so ClickHouse stops pulling
	// upstream data once maxRows+1 rows are produced.
	bounded := NewQuery().From(fanoutSource)
	bounded.Select(Star())
	bounded.Limit(maxRows + 1)

	// A second, independently LIMIT-bounded read of the fan-out (probeSource,
	// the count-equivalent copy), reduced to a single scalar count() —
	// deliberately NOT a window function on `bounded` itself. See
	// rateWindowFanoutBoundedSourceFrag's doc comment (design 3/4) for why
	// every window-function variant tried forces full materialisation of
	// the whole LIMIT-bounded set before it can annotate even one row,
	// defeating the bound at the scale this file needs to admit.
	probe := NewQuery().From(probeSource)
	probe.Select(Col(probeColumn))
	probe.Limit(maxRows + 1)
	probeCount := NewQuery().From(probe.Frag())
	probeCount.Select(As(Call("count"), "n"))

	guarded := NewQuery().From(bounded.Frag())
	guarded.Select(Star())
	guarded.Where(lwrFanoutGuardFrag(probeCount, maxRows, message))

	return guarded.Frag()
}

// lwrFanoutGuardFrag renders the WHERE predicate that aborts the query
// when probeCount — a scalar subquery counting an independent
// LIMIT-bounded copy of the same fan-out — shows the LIMIT truncated the
// input: the count landing on maxRows+1 can only happen if the true
// fanned-out row count was at least that large.
//
//	throwIf((<probeCount>) > maxRows, message) = 0
//
// `throwIf` returns 0 when it does not fire, so `= 0` keeps every row once
// the guard has passed. Mirrors rateWindowFanoutGuardFrag exactly.
func lwrFanoutGuardFrag(probeCount *QueryBuilder, maxRows int64, message string) Frag {
	return Eq(
		Call(
			"throwIf",
			Gt(Subquery(probeCount), InlineLit(maxRows)),
			InlineLit(message),
		),
		InlineLit(int64(0)),
	)
}
