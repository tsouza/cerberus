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
// This bound wraps the emitter's own UNION ALL source via the EXISTING
// lwrFanoutBoundedSourceFrag / lwrFanoutGuardFrag helpers
// (internal/chsql/lwr_fanout_bound.go) directly — they already take the
// threshold and message as parameters, so this file supplies only its own
// constants below rather than a second copy of the two-independent-reads
// LIMIT+throwIf shape (a code review caught the earlier duplicate). See
// that file's doc comment for the fuller design history (four designs
// tried, why a LIMIT alone doesn't suffice — a window function needs its
// whole PARTITION materialised before it emits even one row, so a LIMIT
// downstream of the sort does not bound the sort's own peak — and why the
// truncation probe must be an INDEPENDENT unwindowed count() on a second
// read rather than a window function annotating the bounded result
// directly).
//
// NOT short-circuiting, unlike the pure-arrayJoin fan-out this shape is
// modelled on. lwrFanoutBoundedSourceFrag's own rationale rests on "no
// blocking operator sits between this LIMIT and the underlying
// scan/arrayJoin, so ClickHouse stops pulling upstream data once
// maxRows+1 rows are produced" — that holds for RangeBucketFanout's
// arrayJoin source, but NOT here: this bound wraps a UNION ALL whose real
// arm is itself an INNER JOIN against a per-series GROUP BY aggregate
// (emitRangeBucketWindowSlide's canonBySeries), and whose sentinel arm is
// generated FROM that same GROUP BY. Both are blocking operators the
// LIMIT sits downstream of, so an oversized query still pays for the full
// canon-by-series aggregate (and its own input scan) before the guard has
// a chance to fire. The guard still correctly REJECTS an oversized query
// before the window sort runs — it just does not shrink the work upstream
// of itself the way the arrayJoin case does. Recovering that property is a
// different bound entirely (on the scan feeding canonBySeries, not this
// LIMIT), not a variant of the shape here.
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
// real measurement. This axis is now the actual dominant cost driver for
// the sort: an earlier version of the canon-by-series computation used a
// per-row whole-partition window aggregate (O(rows^2) per series — a real
// quadratic blow-up a code review caught before this shipped), which this
// row-count bound could not have caught at any realistic per-series row
// count; canonBySeries is now an ordinary (linear) GROUP BY, so the total
// union row count is the right axis again. Recalibrate by binary search
// against a real ClickHouse the same way lwr_fanout_bound.go's own
// constants were calibrated (unique query_id per run — see that file's
// harness-pitfall note) once this path carries real traffic.
const maxRangeBucketWindowSlideRows = 4_000_000

// RangeBucketWindowSlideBudgetMessage is the throwIf message
// lwrFanoutGuardFrag raises when this node's own bound fires. Distinct
// text from RangeBucketFanoutBudgetMessage / RangeLWRFanoutBudgetMessage so
// a rejection's error message alone says which bound fired — see those
// constants' own doc for why that matters for classifyThrowIfGuardError
// and for a human reading a query_log entry.
const RangeBucketWindowSlideBudgetMessage = "classic-histogram window-slide anchor injection exceeds the " +
	"series-times-anchors resource bound"
