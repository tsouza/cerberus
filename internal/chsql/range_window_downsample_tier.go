package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// downsampleTierMergedAlias / downsampleTierSortedAlias / downsampleTierTemporalityAlias
// name the synthetic intermediate columns emitRangeWindowDownsampleTier
// builds on top of the tier table's own row shape — see that function's
// doc for the SQL skeleton.
const (
	downsampleTierMergedAlias      = "downsample_tier_merged"
	downsampleTierSortedAlias      = "downsample_tier_sorted"
	downsampleTierTemporalityAlias = "downsample_tier_temporality"
)

// downsampleTierMinSamples reports the minimum sample count
// emitRangeWindowDownsampleTier requires before it reports a value for a
// (series, bucket): 1 for last_over_time (a single sample is enough to
// answer "the last value"), 2 for irate / idelta (both are defined over a
// TRAILING PAIR — PromQL drops the series entirely when a window holds
// fewer than 2 samples, the same "insufficient samples" rule the fan-out's
// own `WHERE length(window_vals) >= 2` applies).
func downsampleTierMinSamples(fn string) (int64, error) {
	switch fn {
	case "irate", "idelta":
		return 2, nil
	case "last_over_time":
		return 1, nil
	}
	return 0, fmt.Errorf("%w: downsample-tier func %q (supported: irate, idelta, last_over_time)", ErrUnsupported, fn)
}

// downsampleTierElemFrag renders `<sorted>[<idx>]` — CH's array indexing
// (1-based positive, or negative counting from the end) into the
// (timestamp, value) pair array emitRangeWindowDownsampleTier's "sorted"
// level builds. idx is a small, self-evident positional constant
// (invariant 13's carve-out), never a computed value.
func downsampleTierElemFrag(idx int64) Frag {
	return Subscript(Col(downsampleTierSortedAlias), InlineLit(idx))
}

// downsampleTierValFrag / downsampleTierTsFrag project the value / timestamp
// half of the pair tuple downsampleTierElemFrag(idx) selects.
func downsampleTierValFrag(idx int64) Frag { return TupleIndex(downsampleTierElemFrag(idx), 2) }
func downsampleTierTsFrag(idx int64) Frag  { return TupleIndex(downsampleTierElemFrag(idx), 1) }

// downsampleTierIntervalSecondsFrag renders the whole-second (float)
// interval between the two most recent retained samples — irate's
// denominator — via `toUnixTimestamp64Nano`, mirroring the codebase's own
// nanosecond-precision interval idiom elsewhere (see e.g.
// endExprFrag / rangeStartFrag's toIntervalNanosecond arithmetic).
//
// Paren-wrapped TWICE, both load-bearing: chsql's binOp (Div/Sub/...)
// renders NO parentheses of its own (the caller is responsible — see
// builder.go's binOp). Sub(a, b) used directly as Div's numerator renders
// as bare `a - b / <divisor>` text with NO grouping around the
// subtraction at all — SQL's own operator precedence then parses that as
// `a - (b / <divisor>)`, not `(a - b) / <divisor>`, so the inner
// Paren(Sub(...)) is required on its own. The outer Paren additionally
// protects this Frag's result at ITS OWN call site (Div(numerator, this)):
// without it, this being the DIVISOR of a further outer Div would repeat
// the identical bug one level up. Caught empirically by this package's
// downsample-tier chDB test (the result read back at a near-epoch-nanosecond
// scale instead of an interval in seconds — twice, once per missing Paren).
func downsampleTierIntervalSecondsFrag() Frag {
	nanosAt := func(idx int64) Frag { return Call("toUnixTimestamp64Nano", downsampleTierTsFrag(idx)) }
	return Paren(Div(Paren(Sub(nanosAt(-1), nanosAt(-2))), InlineLit(float64(1e9))))
}

// downsampleTierValueExprFrag renders the per-Func output Value expression
// over the sorted trailing pair — verified empirically against a real
// ClickHouse instance (this package's downsample-tier chDB test) to match
// Prometheus's own funcIrate / funcIdelta / funcLastOverTime exactly,
// including the counter-reset case landing ON the trailing pair itself:
//
//   - "last_over_time": the single most recent value, sorted[-1].2.
//   - "idelta": sorted[-1].2 - sorted[-2].2 — a PLAIN difference, NEVER
//     counter-reset-corrected regardless of temporality, matching funcIdelta
//     AND matching this codebase's own emitRangeWindowIDelta, which never
//     branches on temporality either — see
//     schema.DownsampleTierTemporalityColumn's own doc.
//   - "irate": chsql.CounterOrDeltaPairDelta over the trailing pair, the
//     SAME primitive the raw fan-out's irateValueFrag uses: the reset-aware
//     `if(curr<prev,curr,curr-prev)` for a CUMULATIVE (or temporality-less)
//     counter, but the RAW current sample alone for a DELTA-temporality one
//     (each DELTA sample already IS the increment since the prior export,
//     so a difference would double-count) — divided by the whole-second
//     interval between the two retained timestamps.
func downsampleTierValueExprFrag(fn string) Frag {
	if fn == "last_over_time" {
		return downsampleTierValFrag(-1)
	}
	if fn == "idelta" {
		return Sub(downsampleTierValFrag(-1), downsampleTierValFrag(-2))
	}
	prev := func() Frag { return downsampleTierValFrag(-2) }
	curr := func() Frag { return downsampleTierValFrag(-1) }
	numerator := CounterOrDeltaPairDelta(prev, curr, Col(downsampleTierTemporalityAlias))
	return Div(numerator, downsampleTierIntervalSecondsFrag())
}

// emitRangeWindowDownsampleTier renders SQL for an r with DownsampleTier set
// (cerberus issue #2751): reads r.DownsampleTierInput's bucketed
// timeSeriesLastTwoSamples state instead of windowing r.Input's raw rows.
// r.Input is UNUSED here — the boot-wired DownsampleTier*Lowerer
// (internal/promql/lower_strategy.go) only sets DownsampleTier when
// r.DownsampleTierInput answers the query completely on its own.
//
// Because that Lowerer's eligibility check (downsampleTierEligible) only
// ever fires when the query_range grid's own anchors are EXACTLY the tier's
// bucket boundaries (r.Range equals the tier's fixed bucket, r.Step a
// positive multiple of it, r.Start bucket-aligned), each grid anchor maps
// to EXACTLY ONE tier row — this reads the tier table directly, bounded to
// [r.Start, r.End] on its own bucket column, with no multi-bucket merge or
// grid materialisation (unlike the native timeSeries*ToGrid family's own
// timeSeriesRange grid axis in range_window_grid_native.go, which this
// deliberately does not need — see the v1 scope note on
// downsampleTierEligible).
//
// SQL shape (three levels, mirroring RangeWindowGridNative's own
// inner/outer split):
//
//	merged: SELECT <group...>, BucketEnd,
//	               timeSeriesLastTwoSamplesMerge(LastTwoSamples) AS merged
//	        FROM (<r.DownsampleTierInput>)
//	        WHERE BucketEnd >= r.Start AND BucketEnd <= r.End
//	        GROUP BY <group...>, BucketEnd
//
// GROUP BY + the -Merge combinator (never a bare per-row
// finalizeAggregation) is load-bearing, not a style choice:
// AggregatingMergeTree can hold MULTIPLE unmerged parts for the SAME
// (series, bucket) key until a background merge runs — the live MV fires
// once per INSERT batch, and ordinary multi-batch ingest (or late data)
// routinely produces more than one row per key before that merge happens.
// A naive per-row finalizeAggregation read would silently answer with
// whichever part it happened to see FIRST, missing samples a DIFFERENT
// part holds — verified empirically to diverge from the correct
// GROUP BY + xMerge answer against a real ClickHouse instance (see this
// package's downsample-tier chDB test).
//
//	sorted: SELECT <group...>, BucketEnd,
//	               arraySort(x -> x.1, arrayZip(merged.1, merged.2)) AS sorted
//	        FROM merged
//
// finalizeAggregation / the -Merge combinator's own element order is NOT
// documented as sorted — verified empirically to come back DESCENDING
// (newest first) — so this explicitly re-sorts ASCENDING by timestamp
// rather than trust that order, making sorted[-1] always "most recent" and
// sorted[-2] always "second most recent" regardless of the engine's
// internal representation.
//
//	outer: SELECT <group...>, BucketEnd AS anchor_ts[, r.TimestampColumn],
//	              <downsampleTierValueExprFrag> AS r.ValueColumn
//	       FROM sorted
//	       WHERE length(sorted) >= downsampleTierMinSamples(r.Func)
//
// The WHERE guard is the tier's exact analogue of the fan-out's
// `WHERE length(window_vals) >= 2` — PromQL's own "insufficient samples in
// window" drop-series rule. A bucket the MV/backfill never populated for
// this series (not yet backfilled, or the series had 0 samples that
// bucket) degrades to the SAME absent-row outcome, never a wrong value —
// see schema.DownsampleTierTable's own doc for why that safety property is
// this mechanism's central design constraint.
func (e *emitter) emitRangeWindowDownsampleTier(r *chplan.RangeWindow) error {
	if r.DownsampleTierInput == nil {
		return fmt.Errorf("%w: RangeWindow.DownsampleTier set with no DownsampleTierInput", ErrUnsupported)
	}
	if r.TimestampColumn == "" {
		return fmt.Errorf("%w: RangeWindow.TimestampColumn unset", ErrUnsupported)
	}
	if r.ValueColumn == "" {
		return fmt.Errorf("%w: RangeWindow.ValueColumn unset", ErrUnsupported)
	}
	minSamples, err := downsampleTierMinSamples(r.Func)
	if err != nil {
		return err
	}
	groupFrags, err := e.collectGroupByFrags(r.GroupBy)
	if err != nil {
		return err
	}
	tierSub, err := e.subqueryFrag(r.DownsampleTierInput)
	if err != nil {
		return err
	}

	bucketCol := Col(schema.DownsampleTierBucketColumn)
	merged := NewQuery().From(tierSub)
	merged.Select(groupFrags...)
	merged.Select(bucketCol)
	merged.Select(As(Call("timeSeriesLastTwoSamplesMerge", Col(schema.DownsampleTierSamplesColumn)), downsampleTierMergedAlias))
	// any(Temporality) merges the SimpleAggregateFunction(any, Int32) column
	// across however many unmerged parts the (series, bucket) key currently
	// has — the SAME multi-part hazard timeSeriesLastTwoSamplesMerge guards
	// against immediately above, see emitRangeWindowDownsampleTier's own
	// doc. Selected unconditionally (idelta / last_over_time never read it
	// — see downsampleTierValueExprFrag) rather than branching the query
	// shape on r.Func, keeping this function's three-level skeleton
	// identical for every eligible Func.
	merged.Select(As(Call("any", Col(schema.DownsampleTierTemporalityColumn)), downsampleTierTemporalityAlias))
	merged.Where(
		Gte(bucketCol, Lit(r.Start)),
		Lte(bucketCol, Lit(r.End)),
	)
	mergedGroupBy := append(append([]Frag{}, groupFrags...), bucketCol)
	merged.GroupBy(mergedGroupBy...)

	sorted := NewQuery().From(merged.Frag())
	sorted.Select(groupFrags...)
	sorted.Select(bucketCol)
	sorted.Select(Col(downsampleTierTemporalityAlias))
	sortedPairs := Call(
		"arrayZip",
		TupleIndex(Col(downsampleTierMergedAlias), 1),
		TupleIndex(Col(downsampleTierMergedAlias), 2),
	)
	sorted.Select(As(Call("arraySort", Lambda1("x", TupleIndex(BareIdent("x"), 1)), sortedPairs), downsampleTierSortedAlias))

	outer := NewQuery().From(sorted.Frag())
	outer.Select(groupFrags...)
	outer.Select(As(bucketCol, RangeWindowAnchorAlias))
	if r.TimestampColumn != RangeWindowAnchorAlias {
		outer.Select(As(bucketCol, r.TimestampColumn))
	}
	outer.Select(As(downsampleTierValueExprFrag(r.Func), r.ValueColumn))
	outer.Where(Gte(Call("length", Col(downsampleTierSortedAlias)), InlineLit(minSamples)))

	return e.emitSelect(outer)
}
