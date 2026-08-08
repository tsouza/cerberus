package promql

import (
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// The classic-histogram aggregated idiom
// `histogram_quantile(phi, <agg> by(le) (<fn>(<bucket>[range])))` is TWO
// reductions, and reference Prometheus applies them in a fixed order:
//
//  1. PER SERIES, the range-vector function `<fn>` reduces that series'
//     in-window samples to one value per `le`.
//  2. ACROSS SERIES, `<agg>` folds those per-series values, rung by rung.
//
// Collapsing both into a single grouping — folding every in-window ROW of
// every series at once — is not the same computation. It conflates the
// TIME axis with the SERIES axis, and the two only agree when the fold is
// `sum` AND the window reduction is also a sum. Three defects followed
// from that one conflation:
//
//   - #1533: for a cumulative counter, `rate` / `increase` span a DELTA
//     between the window's endpoints; summing every sample in the window
//     instead inflates each rung by the number of scrapes. It survives
//     unnoticed while the bucket distribution holds its shape, because
//     `histogram_quantile` reads only the RATIOS between rungs — and a
//     constant-shape window scales every rung alike. A window whose shape
//     MOVES does not.
//   - #1535 / #1584: reference PromQL's `rate` / `increase` emit nothing
//     for a series with fewer than two samples in the window. That floor
//     is per SERIES, so there is nowhere to apply it while series are
//     folded together — and under `sum by(le)`, which drops `le` and
//     leaves no grouping key at all, there is not even a key to count
//     distinct timestamps within.
//
// This file builds the missing first stage: one row per series, carrying
// that series' window-reduced bucket layout. The existing merge then runs
// as stage 2 over those rows unchanged — its "rows" become series, which
// is exactly the input Prometheus's aggregation sees.
//
// Aliases for the per-series stage. `_hq_` prefixed like the merge
// aliases, so they cannot collide with user labels.
const (
	// hqWindowTsListAlias holds the group's groupArray of each row's
	// timestamp, parallel to the bounds / counts lists. The window fold
	// needs it to put the samples in TIME order — groupArray's own order
	// is unspecified, and a delta between endpoints is meaningless
	// without it.
	hqWindowTsListAlias = "_hq_ts_list"
	// hqWindowSampleCountAlias holds the number of DISTINCT sample
	// timestamps the series has in the window, which the min-samples
	// floor filters on. Distinct rather than raw row count: a series may
	// be stored more than once at one instant, and the rule is about how
	// many scrapes the window spans.
	hqWindowSampleCountAlias = "_hq_samples"
	// hqWindowLadderAlias holds the per-series cumulative ladder before
	// it is differenced back into per-bucket counts. It gets its own
	// Project layer because the differencing reads it twice.
	hqWindowLadderAlias = "_hq_window_ladder"
)

// Lambda parameter names for the per-series window fold. `p` / `c` are a
// consecutive (previous, current) pair of one bucket's cumulative counts;
// `t` is a row's timestamp; `j` indexes the ladder during differencing.
const (
	paramPrevCum   = "p"
	paramCurrCum   = "c"
	paramRowTime   = "t"
	paramLadderPos = "j"
)

// histogramWindowTimeFold reduces ONE bucket's per-row counts down to
// that series' single value for the bucket. `values` holds the
// contributing rows' counts at that bucket; `order` holds the same
// rows' timestamps, positionally aligned, so a fold that cares about
// sample order can sort by it.
//
// Both histogram representations reduce along this axis with the same
// folds: for a classic histogram the bucket is one `le` rung of the
// cumulative ladder, for an exponential histogram it is one
// scale-aligned bucket index. "Sum the consecutive deltas of a counter"
// does not care which — only that `values` are one bucket's readings
// and `order` puts them in time order — so both paths share this type
// and the folds below rather than keeping parallel copies that drift.
//
// Distinct from classicBucketRungFold, which folds across SERIES for the
// user's aggregation operator: this one folds across TIME for the
// range-vector function. Different axis, different input, different
// resolution table — keeping them separate types is what stops a fold
// meant for one axis being reached from the other.
type histogramWindowTimeFold func(values, order chplan.Expr) chplan.Expr

// histogramWindowFold maps a matched range-vector function to the
// reduction it performs on one series' in-window samples.
//
// `rate` / `increase` read a COUNTER: the window's value is the total
// increase across it, which is the sum of consecutive sample-to-sample
// deltas with Prometheus's counter-reset rule applied (a drop means the
// counter restarted, so the current value IS the increase). This is the
// same rule the ordinary counter path applies — see chsql's CounterDelta,
// which this transcribes into the bucket-array domain. Prometheus divides
// that total by the range for `rate`, but `histogram_quantile` reads only
// the ratios between rungs and a per-series constant cancels out of every
// ratio, so THAT division is left out rather than emitted and undone.
//
// Prometheus's `extrapolatedRate` applies a SECOND correction first,
// though, and that one does NOT cancel: the boundary-extrapolation factor
// (sampledInterval + durationToStart + durationToEnd) / sampledInterval,
// which stretches the observed increase to cover the requested window
// when the series' own samples don't quite reach its edges. It reads
// EACH series' own in-window sample timestamps against the shared window
// bounds, so it is a per-series constant, not a per-query one — two
// series scraped on the same cadence and phase compute the identical
// factor (which is why omitting it entirely, as this fold did before
// #1958's investigation, went unnoticed: every classic-histogram family
// in the corpus shares one scrape cadence), but two series on different
// cadences do not, and reference Prometheus weighs each series' rate by
// its OWN factor before summing them together. rangeStart / rangeEnd
// carry the window bounds that correction needs — see
// histogramExtrapolationFactorExpr.
//
// `sum_over_time` reads the samples as VALUES and sums them, whatever
// they represent — the canonical shape for delta-temporality histograms,
// where each sample is already a per-window increment. It has no
// extrapolation counterpart in Prometheus (only rate/increase/delta
// extrapolate), so rangeStart / rangeEnd go unused on that branch.
//
// The engine reads `rate` / `increase` as cumulative everywhere, not just
// here: the ordinary counter lowering applies the identical reset rule
// without consulting the schema's AggregationTemporality column either.
// Making delta-temporality counters a first-class case is a cross-cutting
// change across every range-vector lowering, tracked in #1628.
//
// The empty string is the #1692 sentinel: `histogram_quantile(phi,
// sum by(le)(<bucket-selector>))` with no range-vector wrapper at all.
// There's no window to reduce — the shape means "ordinary instant-vector
// selector", which resolves to at most the single newest sample per
// series in the staleness lookback — so it gets its own fold rather than
// falling into the sum_over_time default.
//
// countValues is the series-wide "total observation count" time series
// histogramExtrapolationFactorExpr's durationToZero clamp reads (see that
// function's doc) — nil for the classic-histogram caller, which folds a
// SINGLE `le` rung and so reads durationToZero off that very rung's own
// (values, order), exactly as reference Prometheus's float-series
// extrapolatedRate does; non-nil for the exponential-histogram caller,
// which folds a DIFFERENT bucket array on every call but needs the SAME
// whole-histogram Count series regardless of which bucket is being
// folded, exactly as reference's histogram-typed extrapolatedRate reads
// resultHistogram.Count / samples.Histograms[0].H.Count rather than any
// one bucket's own values.
func histogramWindowFold(fn string, rangeStart, rangeEnd, countValues chplan.Expr) histogramWindowTimeFold {
	switch fn {
	case "rate", "increase":
		return func(values, order chplan.Expr) chplan.Expr {
			cv := values
			if countValues != nil {
				cv = countValues
			}
			return &chplan.Binary{
				Op:    chplan.OpMul,
				Left:  counterIncreaseFold(values, order),
				Right: histogramExtrapolationFactorExpr(order, rangeStart, rangeEnd, cv),
			}
		}
	case "":
		return latestSampleFold
	default:
		return func(values, _ chplan.Expr) chplan.Expr {
			return &chplan.FuncCall{Name: "arraySum", Args: []chplan.Expr{values}}
		}
	}
}

// histogramExtrapolationThresholdFactor is Prom's `extrapolationThreshold
// = averageDurationBetweenSamples * 1.1` cutoff (promql/functions.go): a
// gap to a window edge past this multiple of the average inter-sample gap
// is deemed "the series starts or ends inside the window" rather than
// "the series just didn't happen to land a sample on the edge", and gets
// clamped to half the average gap instead of extrapolated the full
// distance. Named apart from chsql's identical `extrapolationThresholdFactor`
// (internal/chsql/range_window.go) because that one is unexported and this
// fold builds chplan IR rather than chsql Frags — see that constant's doc
// for the shared derivation.
const histogramExtrapolationThresholdFactor = 1.1

// histogramExtrapolationFactorExpr renders Prometheus's rate/increase
// boundary-extrapolation correction (functions.go's `extrapolatedRate`;
// internal/chsql's ordinary-counter twin lives at
// range_window.go's durationToStartFrag / durationToEndFrag /
// extrapolatedValueFrag) as a single scalar factor over one fold call's
// own `order` argument — the in-window sample timestamps that same call
// is about to fold `values` over:
//
//	factor = (sampledInterval + durationToStart + durationToEnd) / sampledInterval
//
// sampledInterval is the gap between the EARLIEST and LATEST timestamp in
// `order`; durationToStart / durationToEnd are the gaps from `order`'s
// own first/last timestamp out to the shared window edges rangeStart /
// rangeEnd, each clamped to half the average inter-sample gap once the
// raw gap grows past histogramExtrapolationThresholdFactor times that
// average (see that constant's doc), and durationToStart is clamped a
// SECOND time by durationToZero below.
//
// Reusing whatever `order` the caller already folds over — rather than a
// single series-wide timestamp list computed once — matters for the
// classic-histogram ladder: reference Prometheus treats every `le` rung
// as its OWN float time series, so `rate(hist_bucket{le="1"}[5m])` and
// `rate(hist_bucket{le="2"}[5m])` extrapolate independently. In the
// common case (a series' bucket layout is stable across the window) every
// rung sees the same sample set and the factor is the same for all of
// them; classicBucketWindowLadderExpr's per-rung `order` (filtered to the
// rows that actually reported that bound) only diverges from the
// series-wide list on the rarer layout-change case, and diverging THERE
// is exactly what matches reference rather than what would flatten it.
// The exponential-histogram fold passes the full per-series timestamp
// list for every bucket instead, matching reference's own native-
// histogram `histogramRate`, which computes ONE factor for the whole
// histogram (every bucket scaled alike) rather than per bucket.
//
// durationToZero is Prom's counter zero-crossing clamp (functions.go's
// `extrapolatedRate`, the `if isCounter` block): a counter cannot have
// been negative, so if `countValues` rose at all across the window, the
// duration back to where the counter would have crossed zero —
// `sampledInterval * (firstCount / resultCount)` — caps how far
// durationToStart may extrapolate, on top of the threshold clamp above.
// It is NOT a reset-only correction: it engages for a counter that
// genuinely started recently relative to the window, which
// demo_shifting_latency_exp_hist's compat corpus exercises directly —
// every series in that fixture starts at the seed's own time anchor, so
// the corpus's EARLIEST evaluated timestamps see a `firstCount` still
// small next to the window's `resultCount`, exactly the shape that
// clamps durationToStart. An earlier revision of this function judged the
// omission safe because it believed the clamp only bound near a genuine
// mid-window reset; the corpus proved that judgment wrong at a bucket-
// ratio-visible margin (a prior version of this comment cited a #1628
// follow-up for this — that citation was itself part of the mistake).
func histogramExtrapolationFactorExpr(order, rangeStart, rangeEnd, countValues chplan.Expr) chplan.Expr {
	sortedOrder := &chplan.FuncCall{Name: "arraySort", Args: []chplan.Expr{order}}
	firstTs := &chplan.Subscript{Container: sortedOrder, Key: &chplan.LitInt{V: 1}}
	lastTs := &chplan.Subscript{
		Container: sortedOrder,
		Key:       &chplan.FuncCall{Name: "length", Args: []chplan.Expr{sortedOrder}},
	}
	nMinusOne := &chplan.Binary{
		Op:    chplan.OpSub,
		Left:  &chplan.FuncCall{Name: "length", Args: []chplan.Expr{order}},
		Right: &chplan.LitInt{V: 1},
	}

	sampledInterval := secondsBetweenTsExpr(firstTs, lastTs)
	avgGap := &chplan.Binary{Op: chplan.OpDiv, Left: sampledInterval, Right: nMinusOne}
	threshold := &chplan.Binary{
		Op: chplan.OpMul, Left: &chplan.LitFloat{V: histogramExtrapolationThresholdFactor}, Right: avgGap,
	}
	halfAvgGap := &chplan.Binary{Op: chplan.OpDiv, Left: avgGap, Right: &chplan.LitFloat{V: 2}}

	clamp := func(raw chplan.Expr) chplan.Expr {
		return &chplan.FuncCall{Name: "if", Args: []chplan.Expr{
			&chplan.Binary{Op: chplan.OpGe, Left: raw, Right: threshold},
			halfAvgGap, raw,
		}}
	}
	thresholdClampedStart := clamp(secondsBetweenTsExpr(rangeStart, firstTs))
	durationToEnd := clamp(secondsBetweenTsExpr(lastTs, rangeEnd))

	// durationToZero: a counter that rose across the window
	// (resultCount > 0) could not have been negative firstCount seconds
	// ago at its own average rate, so the zero-crossing distance caps
	// durationToStart on top of the threshold clamp. `resultCount <= 0`
	// (flat or reset-dominated window) leaves durationToStart at its
	// threshold-clamped value — reference's own fallback
	// (`durationToZero := durationToStart`) — via the `if` below never
	// finding a SMALLER value than thresholdClampedStart itself.
	// firstCount is never negative here (a stored UInt64 total-observation
	// count), so reference's extra `Floats[0].F >= 0` / `H.Count >= 0`
	// guard is always true and is not encoded separately.
	resultCount := counterIncreaseFold(countValues, order)
	firstCount := firstInTimeExpr(countValues, order)
	durationToZero := &chplan.FuncCall{Name: "if", Args: []chplan.Expr{
		&chplan.Binary{Op: chplan.OpGt, Left: resultCount, Right: &chplan.LitInt{V: 0}},
		&chplan.Binary{
			Op:    chplan.OpMul,
			Left:  sampledInterval,
			Right: &chplan.Binary{Op: chplan.OpDiv, Left: firstCount, Right: resultCount},
		},
		thresholdClampedStart,
	}}
	durationToStart := &chplan.FuncCall{Name: "if", Args: []chplan.Expr{
		&chplan.Binary{Op: chplan.OpLt, Left: durationToZero, Right: thresholdClampedStart},
		durationToZero,
		thresholdClampedStart,
	}}

	factor := &chplan.Binary{
		Op: chplan.OpDiv,
		Left: &chplan.Binary{
			Op:    chplan.OpAdd,
			Left:  &chplan.Binary{Op: chplan.OpAdd, Left: sampledInterval, Right: durationToStart},
			Right: durationToEnd,
		},
		Right: sampledInterval,
	}
	// A degenerate `order` (fewer than two DISTINCT timestamps — e.g. a
	// classic-histogram rung a mid-window layout change left under-
	// reported) collapses sampledInterval to 0, which the division above
	// leaves undefined. counterIncreaseFold already answers 0 for that
	// shape (arraySum of an empty consecutive-diff array), so multiplying
	// by 1 rather than by the undefined factor preserves that fallback
	// instead of turning it into a NaN that poisons every downstream sum.
	return &chplan.FuncCall{Name: "if", Args: []chplan.Expr{
		&chplan.Binary{Op: chplan.OpGt, Left: sampledInterval, Right: &chplan.LitInt{V: 0}},
		factor,
		&chplan.LitFloat{V: 1},
	}}
}

// secondsBetweenTsExpr renders the signed gap `to - from` between two
// DateTime64(9) expressions as a Float64 number of seconds:
// `toFloat64(dateDiff('nanosecond', from, to)) / 1e9`. Mirrors chsql's
// `secondsBetweenFrag` (range_window.go) at nanosecond precision, kept as
// a chplan-IR twin because this fold builds plan expressions rather than
// chsql Frags directly.
func secondsBetweenTsExpr(from, to chplan.Expr) chplan.Expr {
	return &chplan.Binary{
		Op: chplan.OpDiv,
		Left: &chplan.FuncCall{Name: "toFloat64", Args: []chplan.Expr{
			&chplan.FuncCall{Name: "dateDiff", Args: []chplan.Expr{
				&chplan.LitString{V: "nanosecond"}, from, to,
			}},
		}},
		Right: &chplan.LitFloat{V: 1e9},
	}
}

// latestSampleFold reduces one `le` rung's per-row cumulative counts to
// the value from the row with the LATEST timestamp — the per-rung
// analogue of an ordinary instant-vector selector's "most recent sample
// in the staleness lookback" rule (#1692). A classic-histogram row's
// BucketCounts / ExplicitBounds are captured together at one TimeUnix, so
// every rung of the row with the latest timestamp shares that same
// timestamp: this fold therefore recovers exactly that one row's ladder,
// not a cross-time blend, whenever a series' bucket layout is stable
// across the window — the only realistic case for OTel-CH classic
// histograms (a series' ExplicitBounds essentially never changes between
// scrapes). The rarer case of a genuine mid-window layout change is
// handled the same best-effort way sum_over_time / rate already handle
// it: classicBucketWindowLadderExpr's union-of-bounds construction, not
// this fold.
func latestSampleFold(values, order chplan.Expr) chplan.Expr {
	sorted := &chplan.FuncCall{Name: "arraySort", Args: []chplan.Expr{
		&chplan.Lambda{
			Params: []string{paramCurrCum, paramRowTime},
			Body:   &chplan.BareIdent{Name: paramRowTime},
		},
		values,
		order,
	}}
	return &chplan.Subscript{
		Container: sorted,
		Key:       &chplan.FuncCall{Name: "length", Args: []chplan.Expr{sorted}},
	}
}

// firstInTimeExpr is latestSampleFold's mirror image: the value from the
// row with the EARLIEST timestamp rather than the latest. Prometheus's
// `extrapolatedRate` reads exactly this — `samples.Floats[0].F` /
// `samples.Histograms[0].H.Count`, the first-in-window sample's own
// value — as the durationToZero clamp's numerator (see
// histogramExtrapolationFactorExpr); it is not itself a fold over the
// whole window (unlike counterIncreaseFold), so it is a plain helper
// rather than a histogramWindowTimeFold.
func firstInTimeExpr(values, order chplan.Expr) chplan.Expr {
	sorted := &chplan.FuncCall{Name: "arraySort", Args: []chplan.Expr{
		&chplan.Lambda{
			Params: []string{paramCurrCum, paramRowTime},
			Body:   &chplan.BareIdent{Name: paramRowTime},
		},
		values,
		order,
	}}
	return &chplan.Subscript{Container: sorted, Key: &chplan.LitInt{V: 1}}
}

// counterIncreaseFold sums consecutive deltas of a counter's in-window
// values under Prometheus's reset rule: `if(c < p, c, c - p)` over each
// (previous, current) pair, so a counter that restarts contributes its
// post-reset value rather than a negative delta.
//
// With no reset this telescopes to `last - first`, which is exactly the
// numerator reference PromQL computes for `rate` / `increase`.
func counterIncreaseFold(values, order chplan.Expr) chplan.Expr {
	sorted := chplan.Expr(&chplan.FuncCall{Name: "arraySort", Args: []chplan.Expr{
		&chplan.Lambda{
			Params: []string{paramCurrCum, paramRowTime},
			Body:   &chplan.BareIdent{Name: paramRowTime},
		},
		values,
		order,
	}})
	return &chplan.FuncCall{Name: "arraySum", Args: []chplan.Expr{
		&chplan.FuncCall{Name: "arrayMap", Args: []chplan.Expr{
			&chplan.Lambda{
				Params: []string{paramPrevCum, paramCurrCum},
				Body: &chplan.FuncCall{Name: "if", Args: []chplan.Expr{
					&chplan.Binary{
						Op:    chplan.OpLt,
						Left:  &chplan.BareIdent{Name: paramCurrCum},
						Right: &chplan.BareIdent{Name: paramPrevCum},
					},
					&chplan.BareIdent{Name: paramCurrCum},
					&chplan.Binary{
						Op:    chplan.OpSub,
						Left:  &chplan.BareIdent{Name: paramCurrCum},
						Right: &chplan.BareIdent{Name: paramPrevCum},
					},
				}},
			},
			&chplan.FuncCall{Name: "arrayPopBack", Args: []chplan.Expr{sorted}},
			&chplan.FuncCall{Name: "arrayPopFront", Args: []chplan.Expr{sorted}},
		}},
	}}
}

// classicBucketWindowAggs are the per-series stage's aggregates: every
// in-window row's layout, counts and timestamp.
func classicBucketWindowAggs(s schema.Metrics) []chplan.AggFunc {
	return []chplan.AggFunc{
		{
			Name:  "groupArray",
			Args:  []chplan.Expr{&chplan.ColumnRef{Name: s.ExplicitBoundsColumn}},
			Alias: hqAggBoundsListAlias,
		},
		{
			Name:  "groupArray",
			Args:  []chplan.Expr{&chplan.ColumnRef{Name: s.BucketCountsColumn}},
			Alias: hqAggCountsListAlias,
		},
		{
			Name:  "groupArray",
			Args:  []chplan.Expr{&chplan.ColumnRef{Name: s.TimestampColumn}},
			Alias: hqWindowTsListAlias,
		},
	}
}

// windowSampleCountAgg counts the DISTINCT sample timestamps a series has
// in the window, which minSamplesFilter then filters on. Only the instant
// path needs it as a column: the range path's fan-out node owns the same
// rule natively through chplan.RangeBucketFanout.MinSamples, which emits
// it as a HAVING rather than a wrapping Filter.
func windowSampleCountAgg(s schema.Metrics) chplan.AggFunc {
	return chplan.AggFunc{
		Name:  "uniqExact",
		Args:  []chplan.Expr{&chplan.ColumnRef{Name: s.TimestampColumn}},
		Alias: hqWindowSampleCountAlias,
	}
}

// classicBucketWindowStage builds the instant-mode per-series stage: one
// row per series carrying that series' window-reduced bucket layout, with
// series holding too few samples dropped.
//
// The grouping key is the full series identity, which is also a
// series-identity BINDING site — it reads the raw table columns, so it
// goes through the same canonicalisation every other histogram grouping
// uses (see latestSampleAgg). The Attributes column it aliases out is
// already canonical, which is why the aggregation stage above binds its
// keys from that column rather than re-deriving them from the table.
//
// rangeStart / rangeEnd are the window's own edges — the same ones the
// caller already used to build the Filter this stage's `input` reads
// through (timeBoundExpr / stalenessLowerBoundExpr) — threaded down so
// the `rate` / `increase` fold can apply Prometheus's boundary-
// extrapolation correction (see histogramWindowFold).
func classicBucketWindowStage(input chplan.Node, shape histogramAggShape, rangeStart, rangeEnd chplan.Expr, s schema.Metrics) chplan.Node {
	group := &chplan.Aggregate{
		Input:              input,
		GroupBy:            []chplan.Expr{histogramIdentityExpr(s)},
		GroupByAliases:     []string{s.AttributesColumn},
		AggFuncs:           append(classicBucketWindowAggs(s), windowSampleCountAgg(s)),
		DropEmptyOnNoGroup: true,
	}
	return classicBucketWindowReshape(
		minSamplesFilter(group, shape.minSamples()),
		// countValues=nil: classicBucketWindowLadderExpr folds one `le`
		// rung's own cumulative values per call, so histogramWindowFold's
		// durationToZero clamp reads THAT rung's own (values, order) —
		// matching reference Prometheus's per-rung float-series
		// extrapolatedRate exactly (see histogramWindowFold's doc).
		histogramWindowFold(shape.windowFn, rangeStart, rangeEnd, nil),
		[]chplan.Projection{{
			Expr:  &chplan.ColumnRef{Name: s.AttributesColumn},
			Alias: s.AttributesColumn,
		}},
		s,
	)
}

// classicBucketWindowLadderExpr renders one SERIES' cumulative per-`le`
// ladder over that series' own union of bucket layouts: one rung per
// distinct bound the series reported in the window, plus the trailing
// +Inf rung, each reduced across the series' in-window rows by `fold`.
//
// The construction mirrors classicBucketMergedLadderExpr — the merge is
// in the cumulative domain for the same reason (a per-bucket count is
// meaningless without the bounds array indexing it, a cumulative count at
// a bound is self-contained) — and differs only in what it folds over:
// the rows of ONE series across TIME, carrying their timestamps so the
// fold can order them.
func classicBucketWindowLadderExpr(fold histogramWindowTimeFold) chplan.Expr {
	boundsList := chplan.Expr(&chplan.ColumnRef{Name: hqAggBoundsListAlias})
	countsList := chplan.Expr(&chplan.ColumnRef{Name: hqAggCountsListAlias})
	tsList := chplan.Expr(&chplan.ColumnRef{Name: hqWindowTsListAlias})

	// The rows whose layout carries this bound, and their timestamps.
	// Both filters share one predicate so the two arrays stay aligned —
	// a fold that sorts values by order would otherwise pair a count
	// with another row's timestamp.
	contributing := func(vals chplan.Expr, param string) chplan.Expr {
		return &chplan.FuncCall{Name: "arrayFilter", Args: []chplan.Expr{
			&chplan.Lambda{
				Params: []string{param, paramRowLayout},
				Body: &chplan.FuncCall{Name: "has", Args: []chplan.Expr{
					&chplan.BareIdent{Name: paramRowLayout},
					&chplan.BareIdent{Name: paramUnionBound},
				}},
			},
			vals,
			boundsList,
		}}
	}

	rowCums := &chplan.FuncCall{Name: "arrayMap", Args: []chplan.Expr{
		&chplan.Lambda{
			Params: []string{paramRowBounds, paramRowCounts},
			Body:   classicBucketRowCumulativeExpr(),
		},
		boundsList,
		countsList,
	}}

	// The +Inf rung exists in every layout — every row reports
	// `{le="+Inf"}` — so it folds over the whole window unfiltered.
	infCums := &chplan.FuncCall{Name: "arrayMap", Args: []chplan.Expr{
		&chplan.Lambda{
			Params: []string{paramRowCounts},
			Body:   classicBucketRowTotalExpr(),
		},
		countsList,
	}}

	return &chplan.FuncCall{Name: "arrayConcat", Args: []chplan.Expr{
		&chplan.FuncCall{Name: "arrayMap", Args: []chplan.Expr{
			&chplan.Lambda{
				Params: []string{paramUnionBound},
				Body: fold(
					contributing(rowCums, paramRowCum),
					contributing(tsList, paramRowTime),
				),
			},
			classicBucketUnionBoundsExpr(),
		}},
		&chplan.FuncCall{Name: "array", Args: []chplan.Expr{fold(infCums, tsList)}},
	}}
}

// classicBucketWindowCountsExpr differences the per-series ladder back
// into the per-bucket counts every downstream consumer of a classic
// histogram row expects: `counts[i] = ladder[i] - ladder[i-1]`, with the
// first rung passing through unchanged.
//
// The round trip through the cumulative domain is exact — the stage that
// consumes these counts re-accumulates them, and a sum of differences
// telescopes back to the ladder it came from. That also makes a rung
// that dips below its predecessor harmless here: it differences to a
// negative count, re-accumulates to the same dipped ladder, and is
// repaired once, at the end, by the merge's monotonic envelope.
func classicBucketWindowCountsExpr() chplan.Expr {
	ladder := chplan.Expr(&chplan.ColumnRef{Name: hqWindowLadderAlias})
	pos := chplan.Expr(&chplan.BareIdent{Name: paramLadderPos})
	return &chplan.FuncCall{Name: "arrayMap", Args: []chplan.Expr{
		&chplan.Lambda{
			Params: []string{paramLadderPos},
			Body: &chplan.FuncCall{Name: "if", Args: []chplan.Expr{
				&chplan.Binary{Op: chplan.OpEq, Left: pos, Right: &chplan.LitInt{V: 1}},
				&chplan.Subscript{Container: ladder, Key: pos},
				&chplan.Binary{
					Op:   chplan.OpSub,
					Left: &chplan.Subscript{Container: ladder, Key: pos},
					Right: &chplan.Subscript{
						Container: ladder,
						Key:       &chplan.Binary{Op: chplan.OpSub, Left: pos, Right: &chplan.LitInt{V: 1}},
					},
				},
			}},
		},
		&chplan.FuncCall{Name: "arrayEnumerate", Args: []chplan.Expr{ladder}},
	}}
}

// classicBucketWindowReshape wraps the per-series grouping in the two
// Projects that turn it back into the classic-histogram row contract
// (Attributes + ExplicitBounds + BucketCounts), so the aggregation stage
// above consumes it exactly as it consumes raw table rows.
func classicBucketWindowReshape(
	group chplan.Node,
	fold histogramWindowTimeFold,
	passthrough []chplan.Projection,
	s schema.Metrics,
) chplan.Node {
	ladder := make([]chplan.Projection, 0, len(passthrough)+2)
	ladder = append(ladder, passthrough...)
	ladder = append(
		ladder,
		chplan.Projection{Expr: classicBucketWindowLadderExpr(fold), Alias: hqWindowLadderAlias},
		chplan.Projection{Expr: classicBucketUnionBoundsExpr(), Alias: s.ExplicitBoundsColumn},
	)

	counts := make([]chplan.Projection, 0, len(passthrough)+2)
	for _, p := range passthrough {
		counts = append(counts, chplan.Projection{
			Expr:  &chplan.ColumnRef{Name: p.Alias},
			Alias: p.Alias,
		})
	}
	counts = append(
		counts,
		chplan.Projection{Expr: classicBucketWindowCountsExpr(), Alias: s.BucketCountsColumn},
		chplan.Projection{
			Expr:  &chplan.ColumnRef{Name: s.ExplicitBoundsColumn},
			Alias: s.ExplicitBoundsColumn,
		},
	)

	return &chplan.Project{
		Input:       &chplan.Project{Input: group, Projections: ladder},
		Projections: counts,
	}
}

// minSamplesFilter applies the range-vector function's "no sample
// emitted" floor to the per-series stage. A series whose window holds
// fewer than `minSamples` distinct sample timestamps must contribute
// NOTHING — reference PromQL's rate / increase need two points to span a
// delta — so it is dropped before the aggregation stage ever sees it,
// which is the only place the rule can be applied per series.
//
// A floor of one needs no filter: a series with no samples in the window
// forms no group, so "at least one" holds by construction. This mirrors
// chsql's fanoutNoMinSampleFilter on the range-mode node.
func minSamplesFilter(input chplan.Node, minSamples int) chplan.Node {
	if minSamples <= noMinSampleFilter {
		return input
	}
	return &chplan.Filter{
		Input: input,
		Predicate: &chplan.Binary{
			Op:    chplan.OpGe,
			Left:  &chplan.ColumnRef{Name: hqWindowSampleCountAlias},
			Right: &chplan.LitInt{V: int64(minSamples)},
		},
	}
}

// noMinSampleFilter is the largest sample floor that needs no filter at
// all — see minSamplesFilter.
const noMinSampleFilter = 1
