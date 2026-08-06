package promql

import (
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// Aliases for the float-domain ladder assembly. The `_hq_` prefix keeps
// them out of the user-label namespace, matching the array-domain merge
// aliases in histogram_quantile.go.
const (
	// hqFloatBoundListAlias / hqFloatCountListAlias hold the group's
	// groupArray of each contributing sample's `le` bound and cumulative
	// count, in whatever order the group produced them.
	hqFloatBoundListAlias = "_hq_le_list"
	hqFloatCountListAlias = "_hq_cum_list"
	// hqFloatSortedBoundAlias / hqFloatSortedCountAlias hold the same two
	// arrays re-ordered by ascending bound. They get their own Project
	// layer because the guards and the monotonic repair below read them
	// several times each, and inlining the sorts would multiply them.
	hqFloatSortedBoundAlias = "_hq_le_sorted"
	hqFloatSortedCountAlias = "_hq_cum_sorted"
	// hqFloatGroupKeyAlias / hqFloatTimeAlias name the two group keys:
	// the sample's label set minus `le`, and its timestamp.
	hqFloatGroupKeyAlias = "gkey_0"
	hqFloatTimeAlias     = "_hq_ts"
)

// hqFloatMinRungs is Prometheus's `len(buckets) < 2` guard from
// bucketQuantile (promql/quantile.go): a ladder with a single rung has no
// interval to interpolate across, and upstream answers NaN rather than
// inventing one. Counted over the whole ladder, so it means "the +Inf rung
// plus at least one finite bound".
const hqFloatMinRungs = 2

// lowerHistogramQuantileClassicFloat lowers
// `histogram_quantile(phi, <shaping-aggregation>(...))` over classic
// buckets by evaluating the aggregation in the FLOAT domain — as
// Prometheus itself does — instead of folding a per-`le` rung.
//
// It is the route for the aggregations that have no entry in
// classicBucketLadderFold, and having no entry there is not an oversight:
// those operators do not reduce a group to one value per rung at all.
// `topk` / `bottomk` (and their experimental `limitk` / `limit_ratio`
// siblings) SELECT a subset of the input series and keep their full label
// sets, so a given series can survive at some bounds and not others and
// the surviving ladder is genuinely partial and per-series.
// `count_values` MINTS a label whose values are sample values, changing
// output series identity outright. A fold entry for any of them could only
// produce a plausible wrong number.
//
// So the argument is lowered through the ORDINARY pipeline. That pipeline
// already fans a `<base>_bucket` selector into one Sample row per bucket
// with a synthesised `le` label (wrapHistogramBucketFanout), which is
// exactly Prometheus's own representation, and every aggregation already
// has a float lowering over it. This function only has to put the ladder
// back together afterwards:
//
//	Project [Sample-row contract]
//	  HistogramQuantile phi=phi, cumulative, groupBy=[Attributes, TimeUnix]
//	    Project [Attributes, TimeUnix, finite bounds, guarded monotone ladder]
//	      Project [gkey_0, _hq_ts, bounds sorted ascending, counts in that order]
//	        Aggregate groupBy=[Attributes minus `le`, TimeUnix]
//	                  funcs=[groupArray(le), groupArray(Value)]
//	          Filter <sample carries a parseable `le`>
//	            <the argument, lowered as an ordinary float expression>
//
// The fan-out is the reason this route is GUARDED rather than universal.
// It multiplies a histogram row into one row per bucket, and the classic
// `sum by(le)` idiom — the hottest cost centre in the product — must keep
// reaching the array-domain fold that reads the bucket arrays in place.
// Only an aggregation with no fold gets here.
//
// Range mode needs no separate lowering. The argument's own lowering
// already fans across the step grid, stamping each row with its anchor, so
// grouping on TimeUnix alongside the label set yields one quantile per
// (series, anchor) — the same shape the array-domain range paths build
// explicitly.
func lowerHistogramQuantileClassicFloat(
	arg parser.Expr,
	phi phiArg,
	s schema.Metrics,
	ctx lowerCtx,
) (chplan.Node, error) {
	inner, err := lower(arg, s, ctx)
	if err != nil {
		return nil, err
	}

	attrs := chplan.Expr(&chplan.ColumnRef{Name: s.AttributesColumn})

	// Prometheus reads each float sample's `le` and skips the sample when
	// it does not parse — which is also how it skips samples that carry no
	// `le` at all, since the absent label reads as the empty string. One
	// NULL test covers both, so a non-bucket argument
	// (`histogram_quantile(phi, topk(2, rate(up[5m])))`) drops every row
	// here and the query answers with the reference's empty vector rather
	// than interpolating over labels that are not bounds.
	bucketed := &chplan.Filter{
		Input: inner,
		Predicate: &chplan.FuncCall{
			Name: "isNotNull",
			Args: []chplan.Expr{classicBucketLeValueExpr(attrs)},
		},
	}

	// The group is the output series: every label except `le`, at one
	// timestamp. `assumeNotNull` is sound because the Filter above is the
	// only way a row reaches this expression.
	group := &chplan.Aggregate{
		Input: bucketed,
		GroupBy: []chplan.Expr{
			&chplan.MapWithoutKeys{Map: attrs, Keys: []string{bucketBoundLabel}},
			&chplan.ColumnRef{Name: s.TimestampColumn},
		},
		GroupByAliases: []string{hqFloatGroupKeyAlias, hqFloatTimeAlias},
		AggFuncs: []chplan.AggFunc{
			{
				Name: "groupArray",
				Args: []chplan.Expr{&chplan.FuncCall{
					Name: "assumeNotNull",
					Args: []chplan.Expr{classicBucketLeValueExpr(attrs)},
				}},
				Alias: hqFloatBoundListAlias,
			},
			{
				Name:  "groupArray",
				Args:  []chplan.Expr{&chplan.ColumnRef{Name: s.ValueColumn}},
				Alias: hqFloatCountListAlias,
			},
		},
	}

	// Layer 1: order the ladder by ascending bound. Both arrays are sorted
	// on the SAME key, so rung k of one still pairs with rung k of the
	// other. The +Inf rung lands last because +Inf is the largest float,
	// which is what makes the overflow guard below a check on the tail.
	sorted := &chplan.Project{
		Input: group,
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: hqFloatGroupKeyAlias}, Alias: hqFloatGroupKeyAlias},
			{Expr: &chplan.ColumnRef{Name: hqFloatTimeAlias}, Alias: hqFloatTimeAlias},
			{
				Expr: &chplan.FuncCall{Name: "arraySort", Args: []chplan.Expr{
					&chplan.ColumnRef{Name: hqFloatBoundListAlias},
				}},
				Alias: hqFloatSortedBoundAlias,
			},
			{
				Expr: &chplan.FuncCall{Name: "arraySort", Args: []chplan.Expr{
					&chplan.Lambda{
						Params: []string{paramBucketCount, paramBucketBound},
						Body:   &chplan.BareIdent{Name: paramBucketBound},
					},
					&chplan.ColumnRef{Name: hqFloatCountListAlias},
					&chplan.ColumnRef{Name: hqFloatBoundListAlias},
				}},
				Alias: hqFloatSortedCountAlias,
			},
		},
	}

	// Layer 2: the histogram-row contract HistogramQuantile consumes.
	reshaped := &chplan.Project{
		Input: sorted,
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: hqFloatGroupKeyAlias}, Alias: s.AttributesColumn},
			{Expr: &chplan.ColumnRef{Name: hqFloatTimeAlias}, Alias: s.TimestampColumn},
			{Expr: hqFloatFiniteBoundsExpr(), Alias: s.ExplicitBoundsColumn},
			{Expr: hqFloatGuardedLadderExpr(), Alias: s.BucketCountsColumn},
		},
	}

	hq := &chplan.HistogramQuantile{
		Input:                  reshaped,
		Phi:                    phi.lit,
		PhiExpr:                phi.expr,
		BucketCountsColumn:     s.BucketCountsColumn,
		ExplicitBoundsColumn:   s.ExplicitBoundsColumn,
		BucketCountsCumulative: true,
		GroupBy: []chplan.Expr{
			&chplan.ColumnRef{Name: s.AttributesColumn},
			&chplan.ColumnRef{Name: s.TimestampColumn},
		},
		GroupByAliases:   []string{s.AttributesColumn, s.TimestampColumn},
		MetricNameColumn: s.MetricNameColumn,
		AttributesColumn: s.AttributesColumn,
		TimestampColumn:  s.TimestampColumn,
	}

	// Sample-row wrapping. The timestamp is the one the argument's own
	// rows carried rather than a fresh evaluation anchor: the grouping
	// above already partitioned by it, so re-stamping would collapse a
	// range query's anchors onto a single instant.
	return &chplan.Project{
		Input: hq,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}, Alias: s.TimestampColumn},
			{Expr: &chplan.ColumnRef{Name: s.ValueColumn}, Alias: s.ValueColumn},
		},
	}, nil
}

// hqFloatOverflowRungExpr reports whether the group reported the +Inf
// overflow rung — the bound classicBucketLeStringExpr renders for the
// trailing bucket, read back through classicBucketLeValueExpr as a
// positive infinity. Written as an existence test rather than a lookup of
// the sorted array's last element so the empty group answers false instead
// of subscripting an empty array.
func hqFloatOverflowRungExpr() chplan.Expr {
	return &chplan.FuncCall{Name: "arrayExists", Args: []chplan.Expr{
		&chplan.Lambda{
			Params: []string{paramBucketBound},
			Body: andExpr(
				&chplan.FuncCall{
					Name: "isInfinite",
					Args: []chplan.Expr{&chplan.BareIdent{Name: paramBucketBound}},
				},
				&chplan.Binary{
					Op:    chplan.OpGt,
					Left:  &chplan.BareIdent{Name: paramBucketBound},
					Right: &chplan.LitInt{V: 0},
				},
			),
		},
		&chplan.ColumnRef{Name: hqFloatSortedBoundAlias},
	}}
}

// hqFloatFiniteBoundsExpr renders the group's ExplicitBounds: every
// reported bound that is not the +Inf overflow. The overflow rung has no
// entry in ExplicitBounds by the schema's own invariant — BucketCounts
// runs one longer — so filtering it out is what restores that invariant
// from a wire ladder where it is just another `le`.
func hqFloatFiniteBoundsExpr() chplan.Expr {
	return &chplan.FuncCall{Name: "arrayFilter", Args: []chplan.Expr{
		&chplan.Lambda{
			Params: []string{paramBucketBound},
			Body: &chplan.FuncCall{Name: "not", Args: []chplan.Expr{
				&chplan.FuncCall{
					Name: "isInfinite",
					Args: []chplan.Expr{&chplan.BareIdent{Name: paramBucketBound}},
				},
			}},
		},
		&chplan.ColumnRef{Name: hqFloatSortedBoundAlias},
	}}
}

// hqFloatGuardedLadderExpr renders the group's cumulative ladder with the
// two NaN guards bucketQuantile applies before it will interpolate at all:
// the highest rung must be the +Inf overflow, and at least
// hqFloatMinRungs rungs must exist. Both fail the same way the array-domain
// `le` restriction fails them — by collapsing BucketCounts to an empty
// array, which the emitter's own `length(BucketCounts) = 0 -> nan` branch
// turns into Prometheus's NaN. That keeps the guard in the shape of the
// plan and out of the emitter.
//
// Passing the guards, the ladder gets Prometheus's
// ensureMonotonicAndIgnoreSmallDeltas repair, which the cumulative input
// mode of chplan.HistogramQuantile requires of its producer. It is not
// theoretical here: each rung is an independently evaluated float — a
// separate series through a separate `rate` — so a higher bound can carry
// a smaller count than a lower one, and the interpolation would read a
// negative-width interval.
func hqFloatGuardedLadderExpr() chplan.Expr {
	ladder := chplan.Expr(&chplan.ColumnRef{Name: hqFloatSortedCountAlias})
	interpolable := andExpr(
		hqFloatOverflowRungExpr(),
		&chplan.Binary{
			Op:    chplan.OpGe,
			Left:  &chplan.FuncCall{Name: "length", Args: []chplan.Expr{ladder}},
			Right: &chplan.LitInt{V: hqFloatMinRungs},
		},
	)
	return &chplan.FuncCall{Name: "if", Args: []chplan.Expr{
		interpolable,
		classicBucketMonotonicExpr(ladder),
		// arraySlice(_, 1, 0) empties the array while keeping its element
		// type, so the branches need no type reconciliation — the same
		// device classicBucketLeRestriction uses for the same guards.
		&chplan.FuncCall{Name: "arraySlice", Args: []chplan.Expr{
			ladder, &chplan.LitInt{V: 1}, &chplan.LitInt{V: 0},
		}},
	}}
}
