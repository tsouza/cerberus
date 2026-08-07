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
// ratio, so the division is left out rather than emitted and undone.
//
// `sum_over_time` reads the samples as VALUES and sums them, whatever
// they represent — the canonical shape for delta-temporality histograms,
// where each sample is already a per-window increment.
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
func histogramWindowFold(fn string) histogramWindowTimeFold {
	switch fn {
	case "rate", "increase":
		return counterIncreaseFold
	case "":
		return latestSampleFold
	default:
		return func(values, _ chplan.Expr) chplan.Expr {
			return &chplan.FuncCall{Name: "arraySum", Args: []chplan.Expr{values}}
		}
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
func classicBucketWindowStage(input chplan.Node, shape histogramAggShape, s schema.Metrics) chplan.Node {
	group := &chplan.Aggregate{
		Input:              input,
		GroupBy:            []chplan.Expr{histogramIdentityExpr(s)},
		GroupByAliases:     []string{s.AttributesColumn},
		AggFuncs:           append(classicBucketWindowAggs(s), windowSampleCountAgg(s)),
		DropEmptyOnNoGroup: true,
	}
	return classicBucketWindowReshape(
		minSamplesFilter(group, shape.minSamples()),
		histogramWindowFold(shape.windowFn),
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
