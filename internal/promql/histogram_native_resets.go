package promql

import (
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_resets.go lowers `resets(<selector>[range])` and
// `changes(<selector>[range])` over an OTel exponential (native)
// histogram — `<base>_exp_hist` — into a FLOAT-valued result.
//
// It is the first exp-histogram range-vector lowering in this package
// whose answer is a plain sample rather than a distribution, and that is
// the whole of what makes it a separate file from histogram_native_rate.go
// rather than another branch inside it. Reference Prometheus's funcResets
// and funcChanges both `return append(enh.Out, Sample{F: float64(n)})` —
// they COUNT sample pairs and publish the count, so nothing about the
// merged bucket ladder survives into the answer. Concretely, that flips
// three things relative to `rate()`:
//
//   - The cap is an ordinary four-column [chplan.Project] (the sample
//     quartet), not [aggregatedHistogramProjection]'s thirteen.
//   - There is no window FOLD at all. `rate()` telescopes every component
//     across time and stretches the result; these two reduce the window to
//     a single integer and never touch a bucket count as a VALUE.
//   - The per-series sample floor is one, not two. Reference emits a
//     sample for a single-sample window — the comparison loop simply never
//     runs and the count is 0 — where `rate()` emits nothing at all. That
//     falls out of [histogramWindowMinSamples] returning
//     stalenessMinSamples for every function that is not rate/increase,
//     so this file states the floor by not overriding it.
//
// # What each function counts, against reference
//
// Both walk consecutive sample pairs in timestamp order and count the
// pairs a predicate condemns. In cerberus's OTel-CH model an exp-histogram
// selector yields histogram samples ONLY — floats and histograms live in
// disjoint tables — so of the three branches each reference function
// switches over, exactly one is reachable here: the histogram/histogram
// pair. The float/float branch needs a float sample this table cannot
// produce, and the float<->histogram TRANSITION branch needs one of each
// in a single series, which no single table can produce either. That is
// why this file transcribes one predicate per function rather than three.
//
//   - `resets()` → `curSample.H.DetectReset(prevSample.H)`, which is
//     exactly the per-pair verdict histogram_native_reset.go already
//     renders for `rate()`'s counter-reset correction. This file reuses
//     [expHistogramResetMaskExpr] rather than restating it, so the two
//     shapes cannot drift apart about what a reset IS: a query that
//     reports N resets and a `rate()` that corrects for N restarts are
//     reading one expression.
//   - `changes()` → `!curSample.H.Equals(prevSample.H)`, which is a
//     DIFFERENT and much stricter predicate, not the reset kernel with a
//     comparison flipped. `Equals` is structural whole-row equality;
//     `DetectReset` is a directional regression test. A histogram that
//     merely GROWS — the normal case for a counter — is a `changes()` hit
//     and a `resets()` miss, so reusing the reset mask here would
//     under-count by nearly the whole window. See
//     [expHistogramChangeMaskExpr].
//
// Plan shape (instant mode; the range shapes differ only in the anchor
// column threaded through the reduction):
//
//	Project [MetricName='', Attributes, now64(9), Value=<pair count>]
//	  Filter uniqExact(TimeUnix) >= 1
//	    Aggregate groupBy=[series identity] funcs=<expHistogramPairCountAggs>
//	      Filter <matchers> AND <window bounds>
//	        Scan(otel_metrics_exponential_histogram)
//
// Like every other lowering in the exp-histogram family this one is asked
// only of the ROOT of a query (see [lowerRoot]). Unlike the
// histogram-VALUED siblings, that restriction is NOT about the wire being
// the only consumer of the answer — a float sample is exactly what every
// PromQL consumer expects, so `resets(m_exp_hist[5m]) * 2` is meaningful
// in a way `rate(m_exp_hist[5m]) * 2` is not. It is about the ARGUMENT:
// the shapes that could wrap this one reach lowerVectorSelector through
// the ordinary descent, where the selector is still an exp-histogram
// selector with no Value column, and are still rejected there — the same
// posture every other exp-histogram lowering in this package holds, and
// for the same reason.

// resetsWindowFn / changesWindowFn are the two range-vector functions this
// file answers, spelled once so the matcher and the per-function verdict
// cannot disagree about which is which.
const (
	resetsWindowFn  = "resets"
	changesWindowFn = "changes"
)

// Lambda parameter names for the `changes()` mask. They shadow nothing
// from histogram_native_reset.go's set on purpose: the two masks are
// rendered by disjoint queries, but naming them apart keeps a reader from
// assuming a shared binding that does not exist.
const (
	paramChangeOrderedRows = "wcp"
	paramChangePrevRow     = "ca"
	paramChangeCurrRow     = "cb"
	paramChangeRowPos      = "cp"
	paramChangeRowTime     = "ct"
)

// resetsOrChangesOverExpHistogram reports whether expr is
// `resets(<bare exp-histogram selector>[range])` or the `changes` twin —
// the two shapes this file answers — returning a [histogramAggShape]
// carrying the matched selector, window and function name.
//
// It builds the shape directly instead of going through
// [matchHistogramAggIdiom], because that walker's vocabulary is the
// classic-histogram idiom set (rate / increase / sum_over_time, optionally
// under sum / avg) and neither of these functions belongs to it. Teaching
// it two more names would widen every classic-histogram path that consults
// it, to no purpose: a `[le]`-bucket ladder under `resets()` is an
// ordinary float series that the existing emitter already answers.
//
// A non-positive `[range]` is refused rather than answered, matching
// [rateOverExpHistogram]: falling through leaves the shape on the explicit
// rejection path instead of reducing an empty window.
func resetsOrChangesOverExpHistogram(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (histogramAggShape, bool) {
	if s.ExpHistogramTable == "" || ctx.metadataFullRange {
		return histogramAggShape{}, false
	}
	call, ok := unwrapCall(expr)
	if !ok || len(call.Args) != 1 {
		return histogramAggShape{}, false
	}
	fn := call.Func.Name
	if fn != resetsWindowFn && fn != changesWindowFn {
		return histogramAggShape{}, false
	}
	ms, ok := unwrapParens(call.Args[0]).(*parser.MatrixSelector)
	if !ok || ms.Range <= 0 {
		return histogramAggShape{}, false
	}
	vs, ok := unwrapVectorSelector(ms.VectorSelector)
	if !ok {
		return histogramAggShape{}, false
	}
	if !s.IsExpHistogramMetric(metricNameFromMatchers(vs.LabelMatchers)) {
		return histogramAggShape{}, false
	}
	return histogramAggShape{selector: vs, windowRange: ms.Range, windowFn: fn}, true
}

// unwrapCall peels the transparent wrappers the parser can put between the
// root and a function call — `(resets(m[5m]))` and the step-invariance
// marker — and reports the call underneath. Mirrors
// [unwrapAggregateExpr] and [unwrapVectorSelector], which do the same for
// their own node types.
func unwrapCall(e parser.Expr) (*parser.Call, bool) {
	for {
		switch v := e.(type) {
		case *parser.Call:
			return v, true
		case *parser.ParenExpr:
			e = v.Expr
		case *parser.StepInvariantExpr:
			e = v.Expr
		default:
			return nil, false
		}
	}
}

// lowerExpHistogramResetsOrChanges lowers the `resets` / `changes` shape
// across the three evaluation shapes [rangeGridShapeFor] distinguishes,
// the same three its histogram-valued siblings handle — see
// [lowerExpHistogramBare] for what each one means.
func lowerExpHistogramResetsOrChanges(shape histogramAggShape, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if grid := rangeGridShapeFor(shape.selector, ctx); grid == gridFanout {
		return lowerExpHistogramResetsRange(shape, s, ctx), nil
	} else if grid == gridBroadcast {
		windowed, err := expHistogramResetsWindowed(shape, s, ctx)
		if err != nil {
			return nil, err
		}
		// Same broadcast placement as every sibling path's: the cross
		// join goes UNDER the sample projection, so the pinned window is
		// reduced ONCE and every step reports that one count at its own
		// timestamp.
		grid := &chplan.StepGrid{Start: ctx.start.UTC(), End: ctx.end.UTC(), Step: ctx.step}
		return expHistogramPairCountProjection(
			&chplan.CrossJoin{Left: grid, Right: windowed},
			&chplan.ColumnRef{Name: stepGridAnchorColumn},
			s,
		), nil
	}
	windowed, err := expHistogramResetsWindowed(shape, s, ctx)
	if err != nil {
		return nil, err
	}
	return expHistogramPairCountProjection(windowed, chplan.NowNano(), s), nil
}

// expHistogramResetsWindowed builds the instant-mode subtree beneath the
// sample projection: the filtered scan reduced, per series, to that
// series' pair count. Its prologue is [expHistogramRateWindowed]'s rung
// for rung — same anchor resolution and the same `(anchor - range,
// anchor]` predicate — because both answer a range-vector function over
// the same table and must select the same rows for the same `[range]`.
//
// It takes no window-bound expressions, which is the one rung it drops:
// those exist so `rate()`'s boundary extrapolation can read the same edges
// the Filter selects against, and neither of these functions extrapolates.
// Reference counts the pairs it was handed and stretches nothing.
func expHistogramResetsWindowed(shape histogramAggShape, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	vs := shape.selector
	scan := &chplan.Scan{Table: s.ExpHistogramTable}
	pred := buildPredicate(vs.LabelMatchers, s)

	anchor, err := anchorFromSelector(vs, ctx)
	if err != nil {
		return nil, err
	}
	if anchor.End.IsZero() && !ctx.end.IsZero() {
		anchor.End = ctx.end.UTC()
	}
	pred = andExpr(pred, timeBoundExpr(s.TimestampColumn, anchor))
	pred = andExpr(pred, stalenessLowerBoundExpr(s.TimestampColumn, anchor, shape.windowRange))

	var input chplan.Node = scan
	if pred != nil {
		input = &chplan.Filter{Input: scan, Predicate: pred}
	}

	group := &chplan.Aggregate{
		Input:              input,
		GroupBy:            []chplan.Expr{histogramIdentityExpr(s)},
		GroupByAliases:     []string{s.AttributesColumn},
		AggFuncs:           append(expHistogramPairCountAggs(shape.windowFn, s), windowSampleCountAgg(s)),
		DropEmptyOnNoGroup: true,
	}
	return expHistogramPairCountStage(
		minSamplesFilter(group, shape.minSamples()),
		shape.windowFn,
		[]string{s.AttributesColumn},
		s,
	), nil
}

// lowerExpHistogramResetsRange is the query_range shape: the per-anchor
// [chplan.RangeBucketFanout] the sibling range paths build — one
// `[range]` window per step anchor, keyed on SERIES identity — with the
// pair count on top and the sample projection as the cap.
//
// The fan-out owns the per-series sample floor natively through
// [chplan.RangeBucketFanout.MinSamples] (emitted as a HAVING), which is
// why this path does not repeat the instant stage's minSamplesFilter.
func lowerExpHistogramResetsRange(shape histogramAggShape, s schema.Metrics, ctx lowerCtx) chplan.Node {
	scan := &chplan.Scan{Table: s.ExpHistogramTable}
	pred := buildPredicate(shape.selector.LabelMatchers, s)
	anchorRef := &chplan.ColumnRef{Name: stepGridAnchorColumn}

	perSeries := expHistogramPairCountStage(
		buildHistogramBucketFanout(
			scan, pred, nil, aggWindowFor(shape),
			[]chplan.Expr{histogramIdentityExpr(s)}, []string{s.AttributesColumn},
			expHistogramPairCountAggs(shape.windowFn, s), s, ctx,
		),
		shape.windowFn,
		[]string{stepGridAnchorColumn, s.AttributesColumn},
		s,
	)
	return expHistogramPairCountProjection(perSeries, anchorRef, s)
}

// expHistogramPairCountAggs is the groupArray set the per-pair verdict
// reads, which is not the same set for the two functions.
//
// `resets()` reads exactly what [expHistogramResetMaskExpr] reads, which
// is [expHistogramWindowAggs] unchanged. `changes()` needs Sum on top of
// it, because `FloatHistogram.Equals` compares Sum and `DetectReset`
// deliberately does not ("in any bucket, in the zero count, or in the
// count of observations, but NOT the sum of observations"). Collecting Sum
// for `resets()` too would put a column in every emitted SELECT that
// nothing downstream reads; branching states which function reads what.
func expHistogramPairCountAggs(windowFn string, s schema.Metrics) []chplan.AggFunc {
	if windowFn == changesWindowFn {
		return expHistogramValuedWindowAggs(s)
	}
	return expHistogramWindowAggs(s)
}

// expHistogramPairCountStage projects the per-series pair count alongside
// the grouping's own key columns, which is everything the sample
// projection above it reads.
//
// The mask is inlined into the Value projection rather than given its own
// layer, which is the opposite of what [expHistogramResetMaskStage] does
// for `rate()` — and for a reason that does not apply here. There, the
// reshape above the mask calls the fold five times with two invocations
// inside each, so a mask expression would render ten times per query and
// twice per TARGET BUCKET. Here it is read exactly once, by the single
// arraySum below, so a separate layer would add a projection without
// removing a repetition.
func expHistogramPairCountStage(input chplan.Node, windowFn string, keyAliases []string, s schema.Metrics) chplan.Node {
	projs := make([]chplan.Projection, 0, len(keyAliases)+1)
	for _, name := range keyAliases {
		projs = append(projs, chplan.Projection{Expr: &chplan.ColumnRef{Name: name}, Alias: name})
	}
	return &chplan.Project{
		Input: input,
		Projections: append(projs, chplan.Projection{
			Expr:  expHistogramPairCountExpr(windowFn, s),
			Alias: s.ValueColumn,
		}),
	}
}

// expHistogramPairCountExpr reduces the per-function pair mask to the
// single float reference publishes: the number of condemned pairs.
//
// A boolean CH array sums to the count of its true elements, so `arraySum`
// IS the count and no separate indicator map is needed. The cast to
// Float64 is what makes the result a PromQL sample value rather than a
// UInt64 — the classic float emitters (emitRangeWindowResets /
// emitRangeWindowChanges in internal/chsql) cast for the same reason, and
// prod ClickHouse is strict where chDB would coerce.
func expHistogramPairCountExpr(windowFn string, s schema.Metrics) chplan.Expr {
	mask := expHistogramResetMaskExpr()
	if windowFn == changesWindowFn {
		mask = expHistogramChangeMaskExpr(s)
	}
	return toFloat64Expr(&chplan.FuncCall{Name: "arraySum", Args: []chplan.Expr{mask}})
}

// expHistogramPairCountProjection caps the reduced subtree with the
// ordinary four-column sample quartet, reporting an EMPTY `__name__`.
//
// The empty name is PromQL's rule, not a shortcut: reference drops
// `__name__` from every range-vector function result, exactly as it does
// from every aggregation result (see [aggregatedHistogramProjection] for
// the histogram-valued twin of this same rule).
func expHistogramPairCountProjection(input chplan.Node, tsExpr chplan.Expr, s schema.Metrics) chplan.Node {
	return &chplan.Project{
		Input: input,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: tsExpr, Alias: s.TimestampColumn},
			{Expr: &chplan.ColumnRef{Name: s.ValueColumn}, Alias: s.ValueColumn},
		},
	}
}

// expHistogramChangeMaskExpr renders `!FloatHistogram.Equals` for every
// consecutive sample pair: one boolean per pair, true where the two
// stored histograms differ in any field reference compares.
//
// Its skeleton is [expHistogramResetMaskExpr]'s — the same
// sort-positions-once permutation, read through the same popBack/popFront
// pairing — because both answer a per-pair question over the same
// groupArrays and the pair ORDER must be identical. What differs is the
// verdict, and one structural consequence of it: this mask compares each
// row's stored fields AS STORED, with no rescale to the group's merged
// scale, because `Equals` compares Schema itself. Two samples at different
// scales are unequal by that first check alone, so reconciling them onto a
// common scale before comparing would be answering a different question
// (and would make two genuinely different histograms compare equal
// whenever the downscale happened to erase the difference).
func expHistogramChangeMaskExpr(s schema.Metrics) chplan.Expr {
	tsList := chplan.Expr(&chplan.ColumnRef{Name: hqWindowTsListAlias})
	orderedRows := &chplan.FuncCall{Name: "arraySort", Args: []chplan.Expr{
		&chplan.Lambda{
			Params: []string{paramChangeRowPos, paramChangeRowTime},
			Body:   &chplan.BareIdent{Name: paramChangeRowTime},
		},
		&chplan.FuncCall{Name: "arrayEnumerate", Args: []chplan.Expr{tsList}},
		tsList,
	}}

	return hqLet(paramChangeOrderedRows, orderedRows, func(rows chplan.Expr) chplan.Expr {
		return &chplan.FuncCall{Name: "arrayMap", Args: []chplan.Expr{
			&chplan.Lambda{
				Params: []string{paramChangePrevRow, paramChangeCurrRow},
				Body:   expHistogramChangeVerdictExpr(s),
			},
			&chplan.FuncCall{Name: "arrayPopBack", Args: []chplan.Expr{rows}},
			&chplan.FuncCall{Name: "arrayPopFront", Args: []chplan.Expr{rows}},
		}}
	})
}

// expHistogramChangeVerdictExpr renders `!Equals` for ONE consecutive
// pair, with the pair's two row positions bound to paramChangePrevRow /
// paramChangeCurrRow by the caller's lambda.
//
// `Equals` is a conjunction of per-field equalities, so its negation is
// the disjunction of the per-field INequalities — which is why this shares
// [orAllExpr] with the reset verdict despite testing the opposite
// direction. The fields are the eight the OTel-CH exp-histogram row
// stores, matching reference's list one for one: Schema (Scale here),
// Count, Sum, ZeroCount, and both signed ladders' offset and bucket
// counts. Reference also compares spans; cerberus's contiguous
// offset-plus-counts encoding carries the same information in the two
// fields per sign that are compared here, so there is no third field to
// test.
//
// ZeroThreshold is absent for the reason histogram_native_reset.go gives
// about the same field: upstream OTel-CH persists no zero-threshold
// column, and a schema that does declare one aggregates it as a single
// `max` per group rather than a groupArray — one value for the whole
// window, which cannot differ between two rows OF that window.
func expHistogramChangeVerdictExpr(s schema.Metrics) chplan.Expr {
	prev := chplan.Expr(&chplan.BareIdent{Name: paramChangePrevRow})
	curr := chplan.Expr(&chplan.BareIdent{Name: paramChangeCurrRow})

	at := func(list, pos chplan.Expr) chplan.Expr {
		return &chplan.Subscript{Container: list, Key: pos}
	}
	differs := func(alias string) chplan.Expr {
		list := chplan.Expr(&chplan.ColumnRef{Name: alias})
		return &chplan.Binary{Op: chplan.OpNe, Left: at(list, curr), Right: at(list, prev)}
	}

	return orAllExpr(
		differs(hqAggScalesArrayAlias),
		differs(hqWindowCountArrayAlias),
		expHistogramSumDiffersExpr(),
		differs(hqWindowZeroCountsArrayAlias),
		differs(hqAggPosOffsetsArrayAlias),
		differs(hqAggPosBucketsArrayAlias),
		differs(hqAggNegOffsetsArrayAlias),
		differs(hqAggNegBucketsArrayAlias),
	)
}

// expHistogramSumDiffersExpr is the Sum half of the `changes()` verdict,
// carved out from its seven siblings because Sum is the one stored field
// that can hold a NaN and the one reference compares by BIT PATTERN.
//
// `Equals` reads `math.Float64bits(h.Sum) != math.Float64bits(h2.Sum)`,
// under which two NaNs with the same payload are EQUAL. ClickHouse's `!=`
// follows IEEE-754 instead, where NaN differs from everything including
// itself, so a series parked at a NaN Sum would report a change at every
// single sample. The carve-out suppresses exactly the both-NaN pair and
// nothing else: a NaN paired with a finite value is a change under both
// readings, since `!=` is already true there.
//
// This is the same correction the float emitter applies for the same
// reason — emitRangeWindowChanges in internal/chsql spells
// `c != p AND NOT (isNaN(c) AND isNaN(p))` after issue #1489 — which is
// what makes a float `changes()` and a histogram `changes()` agree about
// a NaN-valued window.
//
// Count and ZeroCount need no such carve-out: both are stored as unsigned
// integers, which have no NaN to compare.
func expHistogramSumDiffersExpr() chplan.Expr {
	list := chplan.Expr(&chplan.ColumnRef{Name: hqWindowSumArrayAlias})
	curr := &chplan.Subscript{Container: list, Key: &chplan.BareIdent{Name: paramChangeCurrRow}}
	prev := &chplan.Subscript{Container: list, Key: &chplan.BareIdent{Name: paramChangePrevRow}}
	isNaN := func(e chplan.Expr) chplan.Expr {
		return &chplan.FuncCall{Name: "isNaN", Args: []chplan.Expr{e}}
	}
	bothNaN := &chplan.Binary{Op: chplan.OpAnd, Left: isNaN(curr), Right: isNaN(prev)}
	return &chplan.Binary{
		Op:    chplan.OpAnd,
		Left:  &chplan.Binary{Op: chplan.OpNe, Left: curr, Right: prev},
		Right: &chplan.FuncCall{Name: "not", Args: []chplan.Expr{bothNaN}},
	}
}
