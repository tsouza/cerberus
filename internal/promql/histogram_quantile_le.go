package promql

import (
	"github.com/prometheus/prometheus/model/labels"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// bucketBoundInfLabel is the wire spelling of the trailing overflow
// rung's bound. Prometheus writes it exactly this way, and its reader —
// strconv.ParseFloat on the `le` label — turns it back into +Inf;
// ClickHouse's toFloat64OrNull agrees, which is what lets
// classicBucketLeValueExpr invert the synthesis without a special case.
const bucketBoundInfLabel = "+Inf"

// classicBucketLeStringExpr renders the wire `le` value of the 1-based
// bucket position `idx` on a classic-histogram row whose upper bounds are
// `bounds`: the formatted bound for a position inside the bounds array,
// and the +Inf spelling for the trailing overflow position (BucketCounts
// always runs one element longer than ExplicitBounds).
//
// This is the single definition of the array-position -> `le` mapping, and
// every producer of the label goes through it: wrapHistogramBucketFanout,
// which materialises `le` onto exploded Sample rows, and
// classicBucketLeRestriction, which matches `le` selectors against the
// same positions without exploding anything. Two copies of the rendering
// would let the array domain and the float domain disagree about which
// bucket a given `le` names, and no single query exercises both halves —
// the divergence would only surface once a query mixed them.
func classicBucketLeStringExpr(idx, bounds chplan.Expr) chplan.Expr {
	return &chplan.FuncCall{
		Name: "if",
		Args: []chplan.Expr{
			&chplan.Binary{
				Op:    chplan.OpGt,
				Left:  idx,
				Right: &chplan.FuncCall{Name: "length", Args: []chplan.Expr{bounds}},
			},
			&chplan.LitString{V: bucketBoundInfLabel},
			&chplan.FuncCall{Name: "toString", Args: []chplan.Expr{
				&chplan.Subscript{Container: bounds, Key: idx},
			}},
		},
	}
}

// classicBucketLeValueExpr reads the `le` label back off a Sample row's
// label map as a Float64 — the inverse of classicBucketLeStringExpr, and
// the only place the float-domain evaluator interprets a bucket bound.
//
// NULL means "this sample carries no usable bucket bound". Both ways of
// getting there collapse into that one value: a Map lookup of an absent
// key yields the empty string, and neither the empty string nor an
// unparseable one converts. Reference Prometheus makes the same single
// decision — strconv.ParseFloat on the label, skip the sample on error —
// so a sample with no `le` and a sample with a nonsense `le` are treated
// identically on both sides.
func classicBucketLeValueExpr(attrs chplan.Expr) chplan.Expr {
	return &chplan.FuncCall{Name: "toFloat64OrNull", Args: []chplan.Expr{
		&chplan.MapAccess{Map: attrs, Key: &chplan.LitString{V: bucketBoundLabel}},
	}}
}

// classicBucketLeRestriction narrows a classic-histogram row's
// BucketCounts x ExplicitBounds arrays down to just the buckets an `le`
// matcher set selects (#1478), re-deriving per-bucket counts from the
// retained cumulative ladder so every downstream stage — latestSampleAgg,
// classicBucketWindowStage, classicBucketMergeShaping — keeps operating
// on an ordinary per-bucket histogram row, just a narrower one. No other
// lowering stage needs to know `le` was ever a matcher: the restriction
// rides the same BucketCountsColumn / ExplicitBoundsColumn aliases via
// chplan.Project.Replacements (`SELECT * REPLACE (...)`), so every other
// column — Attributes, TimeUnix, MetricName — passes through untouched.
//
// Returns input unchanged when no `le` matcher is present (the common
// case), so unaffected fixtures don't churn.
//
// The `le` string synthesised at each bucket position mirrors
// wrapHistogramBucketFanout's convention exactly: ExplicitBounds[i]
// formatted with toString for i within ExplicitBounds' range, "+Inf" for
// the trailing overflow position (BucketCounts always runs one longer
// than ExplicitBounds — the schema invariant both this and
// wrapHistogramBucketFanout rely on). Multiple `le` matchers AND
// together, matching PromQL's own selector semantics for repeated
// matchers on one label name.
//
// Prometheus's own bucketQuantile (promql/quantile.go) refuses to
// interpolate a ladder whose top (highest-index) bucket isn't the +Inf
// overflow bucket, and separately refuses one with fewer than two
// buckets — both return NaN. This mirrors both guards: whenever either
// fails, BucketCounts collapses to an empty array so the existing
// `length(BucketCounts) = 0 -> nan` branch in the chsql emitter
// (histogramQuantileValueFrag) fires. No emitter change is needed —
// the guard lives entirely in the shape this Project constructs.
func classicBucketLeRestriction(input chplan.Node, leMatchers []*labels.Matcher, s schema.Metrics) chplan.Node {
	if len(leMatchers) == 0 {
		return input
	}

	bc := &chplan.ColumnRef{Name: s.BucketCountsColumn}
	eb := &chplan.ColumnRef{Name: s.ExplicitBoundsColumn}

	n := &chplan.FuncCall{Name: "length", Args: []chplan.Expr{bc}}

	// cumFloat[i] = running total through bucket i, 1-based, length n.
	// BucketCounts is Array(UInt64) in the OTel-CH schema; cast to
	// Float64 before summing so it composes with the Float64
	// ExplicitBounds arithmetic downstream (mirrors histogramQuantileValueFrag's
	// own bcFloat cast).
	cumFloat := &chplan.FuncCall{
		Name: "arrayCumSum",
		Args: []chplan.Expr{
			&chplan.FuncCall{
				Name: "arrayMap",
				Args: []chplan.Expr{
					&chplan.Lambda{
						Params: []string{"x"},
						Body:   &chplan.FuncCall{Name: "toFloat64", Args: []chplan.Expr{&chplan.BareIdent{Name: "x"}}},
					},
					bc,
				},
			},
		},
	}

	// lePred(i) ANDs every `le` matcher against the position-`i` bucket's
	// synthesised le string — matchOp covers all four labels.MatchType
	// values (=, !=, =~, !~), same routing matcherToExpr uses for every
	// other label. Regex values are passed through as-is: the chsql
	// emitter's anchoredRegexPattern (builder.go, #1741) wraps every
	// chplan.OpMatch/OpNotMatch pattern with `^(?:...)$` at the one SQL
	// emission site both render paths share, so an unanchored "1" here
	// would still only match bucket "1", never "0.1" or "21" — wrapping
	// again here would double-anchor to `^(?:^(?:1)$)$`.
	var lePred chplan.Expr
	for _, m := range leMatchers {
		cmp := &chplan.Binary{
			Op:    matchOp(m.Type),
			Left:  classicBucketLeStringExpr(&chplan.BareIdent{Name: "i"}, eb),
			Right: &chplan.LitString{V: m.Value},
		}
		lePred = andExpr(lePred, cmp)
	}

	// keptIdx = the 1-based bucket positions the `le` matcher set selects,
	// in ascending order (arrayFilter preserves source order).
	keptIdx := &chplan.FuncCall{
		Name: "arrayFilter",
		Args: []chplan.Expr{
			&chplan.Lambda{Params: []string{"i"}, Body: lePred},
			&chplan.FuncCall{
				Name: "range",
				Args: []chplan.Expr{&chplan.LitInt{V: 1}, addExpr(n, &chplan.LitInt{V: 1})},
			},
		},
	}
	lenKept := &chplan.FuncCall{Name: "length", Args: []chplan.Expr{keptIdx}}

	// keptIdxAllButLast() = keptIdx with its last element dropped —
	// shared by both the previous-cumulative lookup (every retained rung
	// except the last contributes a "previous" point to the NEXT rung)
	// and the retained ExplicitBounds (the top retained rung has no
	// bound of its own: it is either the +Inf overflow position or,
	// under the NaN guards below, discarded along with BucketCounts).
	// Re-invoked at each use site (mirrors histogramQuantileValueFrag's
	// idx() closure) rather than shared by pointer — ClickHouse's CSE
	// folds the repeated subexpression.
	keptIdxAllButLast := func() chplan.Expr {
		return &chplan.FuncCall{
			Name: "arraySlice",
			Args: []chplan.Expr{
				keptIdx,
				&chplan.LitInt{V: 1},
				&chplan.FuncCall{Name: "greatest", Args: []chplan.Expr{subExpr(lenKept, &chplan.LitInt{V: 1}), &chplan.LitInt{V: 0}}},
			},
		}
	}

	// keptCumArr[k] = cum[keptIdx[k]] — the cumulative count AT each
	// retained rung.
	keptCumArr := &chplan.FuncCall{
		Name: "arrayMap",
		Args: []chplan.Expr{
			&chplan.Lambda{Params: []string{"i"}, Body: &chplan.Subscript{Container: cumFloat, Key: &chplan.BareIdent{Name: "i"}}},
			keptIdx,
		},
	}

	// prevCumArr[k] = cum[keptIdx[k-1]] for k>1, else 0.0 — the
	// cumulative count just BEFORE each retained rung, so the element-wise
	// difference against keptCumArr below reconstructs a per-bucket
	// (non-cumulative) delta for the retained rung alone. Telescoping
	// means re-summing those deltas downstream (every consumer of
	// BucketCounts assumes a per-bucket array and re-cumsums it) recovers
	// keptCumArr exactly — the restriction is a pure re-indexing, not an
	// approximation.
	prevCumArr := &chplan.FuncCall{
		Name: "if",
		Args: []chplan.Expr{
			&chplan.Binary{Op: chplan.OpEq, Left: lenKept, Right: &chplan.LitInt{V: 0}},
			&chplan.FuncCall{Name: "emptyArrayFloat64"},
			&chplan.FuncCall{
				Name: "arrayConcat",
				Args: []chplan.Expr{
					&chplan.FuncCall{Name: "array", Args: []chplan.Expr{&chplan.LitFloat{V: 0}}},
					&chplan.FuncCall{
						Name: "arrayMap",
						Args: []chplan.Expr{
							&chplan.Lambda{Params: []string{"i"}, Body: &chplan.Subscript{Container: cumFloat, Key: &chplan.BareIdent{Name: "i"}}},
							keptIdxAllButLast(),
						},
					},
				},
			},
		},
	}

	restrictedBC := &chplan.FuncCall{
		Name: "arrayMap",
		Args: []chplan.Expr{
			&chplan.Lambda{
				Params: []string{"cur", "prev"},
				Body:   subExpr(&chplan.BareIdent{Name: "cur"}, &chplan.BareIdent{Name: "prev"}),
			},
			keptCumArr,
			prevCumArr,
		},
	}

	// The two NaN guards Prometheus's bucketQuantile applies before ever
	// interpolating: the retained ladder's top rung must be the +Inf
	// overflow bucket (checked via array membership of position n, the
	// only position classicBucketLeStringExpr ever renders as the +Inf
	// bound), and at least two
	// rungs must remain.
	overflowRetained := &chplan.FuncCall{Name: "has", Args: []chplan.Expr{keptIdx, n}}
	atLeastTwoRungs := &chplan.Binary{Op: chplan.OpGe, Left: lenKept, Right: &chplan.LitInt{V: 2}}
	nanTrigger := &chplan.FuncCall{
		Name: "not",
		Args: []chplan.Expr{andExpr(overflowRetained, atLeastTwoRungs)},
	}

	// Force BucketCounts to length 0 whenever either guard fails —
	// arraySlice(_, 1, 0) keeps the exact element type (Float64) rather
	// than introducing a second array-literal branch `if` would have to
	// reconcile types with.
	finalBC := &chplan.FuncCall{
		Name: "if",
		Args: []chplan.Expr{
			nanTrigger,
			&chplan.FuncCall{Name: "arraySlice", Args: []chplan.Expr{restrictedBC, &chplan.LitInt{V: 1}, &chplan.LitInt{V: 0}}},
			restrictedBC,
		},
	}

	restrictedEB := &chplan.FuncCall{
		Name: "arrayMap",
		Args: []chplan.Expr{
			&chplan.Lambda{Params: []string{"i"}, Body: &chplan.Subscript{Container: eb, Key: &chplan.BareIdent{Name: "i"}}},
			keptIdxAllButLast(),
		},
	}

	return &chplan.Project{
		Input: input,
		Replacements: []chplan.Projection{
			{Expr: finalBC, Alias: s.BucketCountsColumn},
			{Expr: restrictedEB, Alias: s.ExplicitBoundsColumn},
		},
	}
}
