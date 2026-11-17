package promql

import (
	"slices"
	"strings"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
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
	// the sample's label set minus `le`, and the instant it stands at —
	// its step anchor in range mode, the evaluation instant otherwise.
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
//	        Aggregate groupBy=[Attributes minus `le`, evaluation instant]
//	                  funcs=[groupArray(le), groupArray(Value)]
//	          Filter <sample carries a parseable `le`>
//	            <the argument, lowered as an ordinary float expression>
//
// The fan-out is the reason this route is not the first choice. It
// multiplies a histogram row into one row per bucket, and the classic
// `sum by(le)` idiom — the hottest cost centre in the product — must keep
// reaching the array-domain fold that reads the bucket arrays in place.
// Only an argument the array-domain idioms cannot express gets here: an
// aggregation with no fold, and — through
// [lowerHistogramQuantileClassicFloatOverPlan] from lowerHistogramQuantile's
// unrecognised-shape fallback — every other float expression whose samples
// may carry `le` (range functions the idioms do not accept, instant
// functions and arithmetic over a bucket selector, …).
//
// Range mode needs no separate lowering. The argument's own lowering
// already fans across the step grid, stamping each row with its anchor, so
// grouping on TimeUnix alongside the label set yields one quantile per
// (series, anchor) — the same shape the array-domain range paths build
// explicitly. An instant argument has no such column to group on: an
// instant window exposes the label set and the value only, and an
// instant selector stamps each row with its own newest sample's time,
// which differs between series one output group can draw from. Every row
// of an instant vector stands at the evaluation instant, so the instant
// group key is that instant. [evalInstantExpr] owns that distinction —
// the timestamp column in range mode, the evaluation instant otherwise.
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
	return lowerHistogramQuantileClassicFloatOverPlan(inner, phi, s, ctx), nil
}

// lowerHistogramQuantileClassicFloatOverPlan reassembles the ladder over an
// argument already lowered as an ordinary float expression; see
// [lowerHistogramQuantileClassicFloat].
func lowerHistogramQuantileClassicFloatOverPlan(inner chplan.Node, phi phiArg, s schema.Metrics, ctx lowerCtx) chplan.Node {
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
			Fn:   chplan.FnIsNotNull,
			Args: []chplan.Expr{classicBucketLeValueExpr(attrs)},
		},
	}

	// The group is the output series: every label except `le`, at one
	// timestamp. `assumeNotNull` is sound because the Filter above is the
	// only way a row reaches this expression.
	group := &chplan.Aggregate{
		Roles: metricRoles(s),
		Input: bucketed,
		GroupBy: []chplan.Expr{
			&chplan.MapWithoutKeys{Map: attrs, Keys: []string{bucketBoundLabel}},
			evalInstantExpr(s, ctx),
		},
		GroupByAliases: []string{hqFloatGroupKeyAlias, hqFloatTimeAlias},
		AggFuncs: []chplan.AggFunc{
			{
				Fn: chplan.FnGroupArray,
				Args: []chplan.Expr{&chplan.FuncCall{
					Fn:   chplan.FnAssumeNotNull,
					Args: []chplan.Expr{classicBucketLeValueExpr(attrs)},
				}},
				Alias: hqFloatBoundListAlias,
			},
			{
				Fn:    chplan.FnGroupArray,
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
		Roles: metricRoles(s),
		Input: group,
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: hqFloatGroupKeyAlias}, Alias: hqFloatGroupKeyAlias},
			{Expr: &chplan.ColumnRef{Name: hqFloatTimeAlias}, Alias: hqFloatTimeAlias},
			{
				Expr: &chplan.FuncCall{Fn: chplan.FnArraySort, Args: []chplan.Expr{
					&chplan.ColumnRef{Name: hqFloatBoundListAlias},
				}},
				Alias: hqFloatSortedBoundAlias,
			},
			{
				Expr: &chplan.FuncCall{Fn: chplan.FnArraySort, Args: []chplan.Expr{
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
		Roles: metricScanRoles(s, s.HistogramTable),
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
		BucketCountsSigned:     true,
		GroupBy: []chplan.Expr{
			&chplan.ColumnRef{Name: s.AttributesColumn},
			&chplan.ColumnRef{Name: s.TimestampColumn},
		},
		GroupByAliases:   []string{s.AttributesColumn, s.TimestampColumn},
		MetricNameColumn: s.MetricNameColumn,
		AttributesColumn: s.AttributesColumn,
		TimestampColumn:  s.TimestampColumn,
	}
	hq = ctx.lowerers.QuantileRankWalk.LowerQuantileRankWalk(hq)

	// Sample-row wrapping. The timestamp is the group's own time key
	// rather than a fresh evaluation anchor: the grouping above already
	// partitioned by it, so re-stamping would collapse a range query's
	// anchors onto a single instant.
	return &chplan.Project{
		Roles: metricRoles(s),
		Input: hq,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}, Alias: s.TimestampColumn},
			{Expr: &chplan.ColumnRef{Name: s.ValueColumn}, Alias: s.ValueColumn},
		},
	}
}

// hqFloatOverflowRungExpr reports whether the group reported the +Inf
// overflow rung — the bound classicBucketLeStringExpr renders for the
// trailing bucket, read back through classicBucketLeValueExpr as a
// positive infinity. Written as an existence test rather than a lookup of
// the sorted array's last element so the empty group answers false instead
// of subscripting an empty array.
func hqFloatOverflowRungExpr() chplan.Expr {
	return &chplan.FuncCall{Fn: chplan.FnArrayExists, Args: []chplan.Expr{
		&chplan.Lambda{
			Params: []string{paramBucketBound},
			Body: andExpr(
				&chplan.FuncCall{
					Fn:   chplan.FnIsInfinite,
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

// hqFloatBoundsButLastExpr / hqFloatCountsButLastExpr render the sorted
// bound/count arrays with their trailing element dropped — the position
// [hqFloatOverflowRungExpr] identifies as the group's own +Inf overflow
// rung (sorting places it last: +Inf is the largest representable float,
// so ANY reported +Inf sample sorts to the tail alongside it). Both are
// length N-1 for a length-N sorted pair, and — critically — index i of
// one still pairs with index i of the other, the same invariant the full
// sorted arrays hold.
//
// [hqFloatFiniteBoundsExpr] and [hqFloatGuardedLadderExpr] both start
// from these rather than the raw sorted arrays so that whichever INTERIOR
// positions the finite filter drops, both arrays drop the SAME positions
// — see hqFloatGuardedLadderExpr's own doc for why a real OTLP row can
// carry more than one non-finite `le` and why filtering the bounds alone
// desyncs the pairing (cerberus pre-release audit finding, the
// histogram_quantile_le.go / #2495-fix sibling of this file).
func hqFloatBoundsButLastExpr() chplan.Expr {
	return arraySliceButLast(&chplan.ColumnRef{Name: hqFloatSortedBoundAlias})
}

func hqFloatCountsButLastExpr() chplan.Expr {
	return arraySliceButLast(&chplan.ColumnRef{Name: hqFloatSortedCountAlias})
}

// arraySliceButLast renders `arraySlice(arr, 1, -1)` — every element of
// arr except the last. ClickHouse's arraySlice treats a negative length
// as "trim that many elements off the end" and answers an empty array
// for an already-empty or single-element input, so this needs no
// separate zero-length guard.
func arraySliceButLast(arr chplan.Expr) chplan.Expr {
	return &chplan.FuncCall{Fn: chplan.FnArraySlice, Args: []chplan.Expr{
		arr, &chplan.LitInt{V: 1}, &chplan.LitInt{V: -1},
	}}
}

// hqFloatFiniteBoundsExpr renders the group's ExplicitBounds: every
// reported bound OTHER than the trailing +Inf overflow rung
// ([hqFloatBoundsButLastExpr]) that is not itself a non-finite value.
// The overflow rung has no entry in ExplicitBounds by the schema's own
// invariant — BucketCounts runs one longer — so dropping it is what
// restores that invariant from a wire ladder where it is just another
// `le`; the additional isInfinite filter over the REMAINING interior
// positions guards a malformed row that reports more than one
// non-finite `le` (a duplicate "+Inf", or a nonsensical "-Inf") — see
// [hqFloatGuardedLadderExpr]'s doc for why the paired count must be
// dropped in lockstep rather than left for BucketCounts to carry alone.
func hqFloatFiniteBoundsExpr() chplan.Expr {
	return &chplan.FuncCall{Fn: chplan.FnArrayFilter, Args: []chplan.Expr{
		&chplan.Lambda{
			Params: []string{paramBucketBound},
			Body: &chplan.FuncCall{Fn: chplan.FnNot, Args: []chplan.Expr{
				&chplan.FuncCall{
					Fn:   chplan.FnIsInfinite,
					Args: []chplan.Expr{&chplan.BareIdent{Name: paramBucketBound}},
				},
			}},
		},
		hqFloatBoundsButLastExpr(),
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
	// Called once per use site rather than bound to a shared local: the
	// chplan IR stays a tree, never a DAG (see lowerCtx.scalarAnchorColumn's
	// doc comment for the same discipline elsewhere), so each reference
	// below builds its OWN independent expression tree of identical shape
	// rather than three branches of the plan pointing at one shared node.
	interpolable := andExpr(
		hqFloatOverflowRungExpr(),
		&chplan.Binary{
			Op:    chplan.OpGe,
			Left:  &chplan.FuncCall{Fn: chplan.FnLength, Args: []chplan.Expr{hqFloatPairedLadderExpr()}},
			Right: &chplan.LitInt{V: hqFloatMinRungs},
		},
	)
	return &chplan.FuncCall{Fn: chplan.FnIf, Args: []chplan.Expr{
		interpolable,
		classicBucketMonotonicExpr(hqFloatPairedLadderExpr()),
		// arraySlice(_, 1, 0) empties the array while keeping its element
		// type, so the branches need no type reconciliation — the same
		// device classicBucketLeRestriction uses for the same guards.
		&chplan.FuncCall{Fn: chplan.FnArraySlice, Args: []chplan.Expr{
			hqFloatPairedLadderExpr(), &chplan.LitInt{V: 1}, &chplan.LitInt{V: 0},
		}},
	}}
}

// hqFloatPairedLadderExpr renders the group's cumulative BucketCounts
// ladder, paired index-for-index with [hqFloatFiniteBoundsExpr]'s
// ExplicitBounds output: the same interior non-finite positions
// [hqFloatFiniteBoundsExpr] drops from the bounds are dropped here from
// the counts too, so the two arrays stay paired — a real OTLP row can
// report more than one non-finite `le` (a duplicate "+Inf", or a
// nonsensical "-Inf"), and dropping only the bounds side would desync
// BucketCounts from ExplicitBounds by more than the schema's one-longer
// invariant expects, silently misaligning every downstream interpolated
// rung against the wrong bound (cerberus pre-release audit finding, the
// float-domain sibling of #2495's array-domain fix in histogram_quantile.go's
// classicBucketRowEntriesExpr).
//
// The trailing +Inf overflow's OWN count is carried through
// UNCONDITIONALLY via `arraySlice(sorted, -1)` — CH's negative-offset
// slice reads the last element as a one-item array without ever
// subscripting (and so never throwing on an empty group) — rather than
// through the interior filter, matching ExplicitBounds running exactly
// one shorter than BucketCounts by construction.
func hqFloatPairedLadderExpr() chplan.Expr {
	pairedInterior := &chplan.FuncCall{Fn: chplan.FnArrayFilter, Args: []chplan.Expr{
		&chplan.Lambda{
			Params: []string{paramBucketCount, paramBucketBound},
			Body: &chplan.FuncCall{Fn: chplan.FnNot, Args: []chplan.Expr{
				&chplan.FuncCall{
					Fn:   chplan.FnIsInfinite,
					Args: []chplan.Expr{&chplan.BareIdent{Name: paramBucketBound}},
				},
			}},
		},
		hqFloatCountsButLastExpr(),
		hqFloatBoundsButLastExpr(),
	}}
	trailingCount := &chplan.FuncCall{Fn: chplan.FnArraySlice, Args: []chplan.Expr{
		&chplan.ColumnRef{Name: hqFloatSortedCountAlias}, &chplan.LitInt{V: -1},
	}}
	return &chplan.FuncCall{Fn: chplan.FnArrayConcat, Args: []chplan.Expr{pairedInterior, trailingCount}}
}

// mayCarryBucketBound reports whether e's output samples can carry an `le`
// label, deciding it from the expression's label algebra alone. A false
// answer folds histogram_quantile over e to the empty vector without
// evaluating it. Every shape the algebra below does not settle answers
// true, which costs the reassembly reading the whole argument from
// ClickHouse only to drop every row it reads.
//
// The selector rule rests on a naming assumption rather than on the
// data: a series whose name lacks the `_bucket` suffix is taken to store
// no `le` label. A plain-named series that does store one answers empty
// here where Prometheus would interpolate it (cerberus issue #4393).
//
//   - A scalar or string expression outputs no labels.
//   - A selector carries `le` only from a classic-histogram bucket fan-out
//     (a pinned `__name__` with the `_bucket` suffix) or from a series an
//     `le` matcher names. A selector that pins a name without the suffix
//     and has no `le` matcher answers false; one whose name is unpinned
//     (regex, or omitted) answers true.
//   - A range selector, a subquery, a unary minus, parentheses and `@` read
//     their operand's label sets unchanged.
//   - A function call carries `le` as [callMayCarryBucketBound] decides.
//   - A reducing aggregation outputs exactly its grouping labels: `le`
//     survives `by` only when listed, and `without` only when not listed
//     and the operand may carry it. No grouping at all outputs no labels.
//   - `topk` / `bottomk` / `limitk` / `limit_ratio` select samples and keep
//     their label sets, so they carry `le` exactly when their operand does.
//   - `count_values` outputs its grouping labels plus the label it mints,
//     whose values are sample values: minting `le` makes them bounds.
//   - A binary operation's output labels come from its vector operands
//     (`group_left` / `group_right` copy them from the other operand), so
//     it carries `le` only when one of them may.
func mayCarryBucketBound(e parser.Expr) bool {
	if t := e.Type(); t == parser.ValueTypeScalar || t == parser.ValueTypeString {
		return false
	}
	switch n := e.(type) {
	case *parser.ParenExpr:
		return mayCarryBucketBound(n.Expr)
	case *parser.StepInvariantExpr:
		return mayCarryBucketBound(n.Expr)
	case *parser.UnaryExpr:
		return mayCarryBucketBound(n.Expr)
	case *parser.SubqueryExpr:
		return mayCarryBucketBound(n.Expr)
	case *parser.MatrixSelector:
		return mayCarryBucketBound(n.VectorSelector)
	case *parser.VectorSelector:
		return selectorMayCarryBucketBound(n.LabelMatchers)
	case *parser.Call:
		return callMayCarryBucketBound(n)
	case *parser.BinaryExpr:
		return mayCarryBucketBound(n.LHS) || mayCarryBucketBound(n.RHS)
	case *parser.AggregateExpr:
		switch n.Op {
		case parser.TOPK, parser.BOTTOMK, parser.LIMITK, parser.LIMIT_RATIO:
			return mayCarryBucketBound(n.Expr)
		case parser.COUNT_VALUES:
			if mintsBucketBound(n.Param) {
				return true
			}
			return groupingMayKeepBucketBound(n)
		case parser.SUM, parser.AVG, parser.MIN, parser.MAX, parser.COUNT, parser.GROUP,
			parser.STDDEV, parser.STDVAR, parser.QUANTILE:
			return groupingMayKeepBucketBound(n)
		}
	}
	return true
}

// selectorMayCarryBucketBound applies [mayCarryBucketBound]'s selector rule
// to a selector's matchers.
func selectorMayCarryBucketBound(matchers []*labels.Matcher) bool {
	name := metricNameFromMatchers(matchers)
	if name == "" || strings.HasSuffix(name, bucketSuffix) {
		return true
	}
	return hasBucketBoundMatcher(matchers)
}

// hasBucketBoundMatcher reports whether matchers include one on `le`.
func hasBucketBoundMatcher(matchers []*labels.Matcher) bool {
	return slices.ContainsFunc(matchers, func(m *labels.Matcher) bool {
		return m.Name == bucketBoundLabel
	})
}

// labelKeepingFunctions names the functions whose output labels are a
// subset of their arguments' labels, so they carry `le` only when an
// argument may. A function missing from this set and from
// [callMayCarryBucketBound]'s own cases answers true.
var labelKeepingFunctions = map[string]struct{}{
	"abs": {}, "acos": {}, "acosh": {}, "asin": {}, "asinh": {}, "atan": {},
	"atanh": {}, "avg_over_time": {}, "ceil": {}, "changes": {}, "clamp": {},
	"clamp_max": {}, "clamp_min": {}, "cos": {}, "cosh": {},
	"count_over_time": {}, "days_in_month": {}, "day_of_month": {},
	"day_of_week": {}, "day_of_year": {}, "deg": {}, "delta": {}, "deriv": {},
	"double_exponential_smoothing": {}, "exp": {}, "first_over_time": {},
	"floor": {}, "histogram_avg": {}, "histogram_count": {},
	"histogram_fraction": {}, "histogram_quantile": {},
	"histogram_stddev": {}, "histogram_stdvar": {}, "histogram_sum": {},
	"hour": {}, "idelta": {}, "increase": {}, "irate": {},
	"last_over_time": {}, "ln": {}, "log10": {}, "log2": {},
	"mad_over_time": {}, "max_over_time": {}, "min_over_time": {},
	"minute": {}, "month": {}, "predict_linear": {}, "present_over_time": {},
	"quantile_over_time": {}, "rad": {}, "rate": {}, "resets": {},
	"round": {}, "sgn": {}, "sin": {}, "sinh": {}, "sort": {},
	"sort_by_label": {}, "sort_by_label_desc": {}, "sort_desc": {},
	"sqrt": {}, fnStartTimestamp: {}, "stddev_over_time": {},
	"stdvar_over_time": {}, "sum_over_time": {}, "tan": {}, "tanh": {},
	timestampFunctionName: {}, "ts_of_first_over_time": {},
	"ts_of_last_over_time": {}, "ts_of_max_over_time": {},
	"ts_of_min_over_time": {}, "vector": {}, "year": {},
}

// callMayCarryBucketBound applies [mayCarryBucketBound] to a function
// call. A label-keeping function ([labelKeepingFunctions]) carries `le`
// when one of its arguments may; its scalar and string arguments carry
// none, so `vector(s)` and an argument-less `hour()` answer false.
//
//   - `label_replace` / `label_join` / `histogram_quantiles` keep their
//     vector argument's labels and write the label their second argument
//     names: writing `le` mints it.
//   - `absent` / `absent_over_time` output the labels their argument's
//     equality matchers pin, so they carry `le` when a selector in the
//     argument matches on it.
//   - `info` keeps its vector argument's labels and copies data labels
//     from the info series its second argument selects: see
//     [infoSeriesMayCarryBucketBound].
func callMayCarryBucketBound(c *parser.Call) bool {
	switch c.Func.Name {
	case fnLabelReplace, fnLabelJoin, "histogram_quantiles":
		return mintsBucketBound(c.Args[1]) || mayCarryBucketBound(c.Args[0])
	case "absent", "absent_over_time":
		return mentionsBucketBound(c.Args[0])
	case "info":
		if mayCarryBucketBound(c.Args[0]) {
			return true
		}
		return len(c.Args) > 1 && infoSeriesMayCarryBucketBound(c.Args[1])
	}
	if _, keeps := labelKeepingFunctions[c.Func.Name]; !keeps {
		return true
	}
	return slices.ContainsFunc(c.Args, mayCarryBucketBound)
}

// infoSeriesMayCarryBucketBound reports whether the info series `info`'s
// data-label selector sel picks can contribute an `le` label. Without a
// `__name__` matcher the info metric is [targetInfoMetric], so the series
// carry `le` only when sel names it; a pinned info-metric name is judged
// by [selectorMayCarryBucketBound]'s rule; a regex or negated name
// matcher may select any metric, so it answers true.
func infoSeriesMayCarryBucketBound(sel parser.Expr) bool {
	vs, ok := sel.(*parser.VectorSelector)
	if !ok || hasBucketBoundMatcher(vs.LabelMatchers) {
		return true
	}
	if !slices.ContainsFunc(vs.LabelMatchers, func(m *labels.Matcher) bool {
		return m.Name == model.MetricNameLabel
	}) {
		return false
	}
	return selectorMayCarryBucketBound(vs.LabelMatchers)
}

// mintsBucketBound reports whether a label-name argument may name `le`:
// any argument other than a string literal naming another label may.
func mintsBucketBound(arg parser.Expr) bool {
	minted, ok := arg.(*parser.StringLiteral)
	return !ok || minted.Val == bucketBoundLabel
}

// mentionsBucketBound reports whether any selector in e has an `le`
// matcher.
func mentionsBucketBound(e parser.Expr) bool {
	found := false
	parser.Inspect(e, func(node parser.Node, _ []parser.Node) error {
		if vs, ok := node.(*parser.VectorSelector); ok {
			found = found || hasBucketBoundMatcher(vs.LabelMatchers)
		}
		return nil
	})
	return found
}

// groupingMayKeepBucketBound applies a reducing aggregation's grouping to
// its operand's `le`: see [mayCarryBucketBound].
func groupingMayKeepBucketBound(agg *parser.AggregateExpr) bool {
	listed := slices.Contains(agg.Grouping, bucketBoundLabel)
	if agg.Without == listed {
		return false
	}
	return mayCarryBucketBound(agg.Expr)
}

// planProvablyEmpty reports whether inner's lowering already folded it to
// zero rows: a constant-false Filter reached through nodes that can only
// map or drop rows (a native-histogram argument under a function that
// drops histogram samples, `max_over_time(h_exp_hist[5m])`, lowers so).
// An Aggregate stops the walk, since one without grouping keys answers a
// row over an empty input.
func planProvablyEmpty(inner chplan.Node) bool {
	for {
		switch n := inner.(type) {
		case *chplan.Filter:
			if lit, ok := n.Predicate.(*chplan.LitBool); ok && !lit.V {
				return true
			}
			inner = n.Input
		case *chplan.Project:
			inner = n.Input
		case *chplan.OrderBy:
			inner = n.Input
		default:
			return false
		}
	}
}
