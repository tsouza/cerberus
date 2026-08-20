package promql

import (
	"time"

	"github.com/prometheus/prometheus/model/labels"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// The boot-wired strategy seam for the ONE range-mode classic-histogram
// stage whose fan-out cost is dominated by a captured-column replica: the
// per-series `rate` window fold under
// `histogram_quantile(phi, <agg> by(le) (rate(<bucket>[range])))`.
//
// It follows the same contract as the timeSeries*ToGrid seam in
// lower_strategy.go — the native/fan-out choice is made ONCE at boot from
// the resolved chopt.EnabledSet and injected as a strategy; the per-query
// path calls the interface method with no feature-flag or version read, and
// the native impl DELEGATES to its embedded fan-out fallback for any shape
// it cannot express, so the method always returns a valid lowering.

// classicHistogramWindowInput carries everything the per-series window stage
// needs, already resolved by the caller: the matchers-filtered scan, the
// matched aggregation shape, the fan-out window, and the schema / grid
// context. It is the intrinsic SHAPE description — no feature or version
// state rides here.
type classicHistogramWindowInput struct {
	// scan is the raw histogram-table Scan; pred is the label-matcher
	// predicate over it; leMatchers is the `le` restriction (#1478) both
	// arms apply beneath their own collapse.
	scan       *chplan.Scan
	pred       chplan.Expr
	leMatchers []*labels.Matcher

	// shape is the matched `<agg> by(le) (<fn>(<bucket>[range]))` idiom;
	// win is the fan-out window it implies (lookback / offset / minSamples).
	shape histogramAggShape
	win   histogramWindow

	s   schema.Metrics
	ctx lowerCtx
}

// ClassicHistogramWindowLowerer lowers the per-(series, anchor) classic-
// histogram window stage to a chplan node publishing the classic-histogram
// row contract `(anchor_ts, Attributes, BucketCounts, ExplicitBounds)`. It
// ALWAYS returns a valid lowering: the native impl emits the native ladder
// aggregate for a shape it handles and delegates to its embedded fan-out
// fallback otherwise; the fan-out impl builds the RangeBucketFanout +
// reshape pair directly.
type ClassicHistogramWindowLowerer interface {
	// LowerClassicHistogramWindow returns the per-series stage node for in.
	// It never returns nil.
	LowerClassicHistogramWindow(in classicHistogramWindowInput) chplan.Node
}

// FanoutClassicHistogramWindowLowerer is the concrete DEFAULT
// ClassicHistogramWindowLowerer: the bounded sample-side RangeBucketFanout
// wearing the array-expression ladder fold. It is the fallback the native
// impl embeds AND the strategy a fan-out-only deployment wires directly, so
// the dispatch site never needs a nil check.
type FanoutClassicHistogramWindowLowerer struct{}

// LowerClassicHistogramWindow builds the RangeBucketFanout + two-Project
// reshape the classic-histogram range path has always emitted.
func (FanoutClassicHistogramWindowLowerer) LowerClassicHistogramWindow(in classicHistogramWindowInput) chplan.Node {
	fanout := buildHistogramBucketFanout(
		in.scan, in.pred, in.leMatchers, in.win,
		[]chplan.Expr{histogramIdentityExpr(in.s)}, []string{in.s.AttributesColumn},
		classicBucketWindowAggs(in.s, in.shape.windowFn), in.s, in.ctx,
	)
	// The classic bucket-ladder fold's peak does not shrink when the anchor
	// grid is sliced, so ModeAuto must not predictively shard it — see
	// chplan.RangeBucketFanout.PeakIndependentOfGrid. Set HERE, at the one
	// lowering whose route-A cost was actually measured (2.84 GB peak on a
	// APM-style panel), and deliberately NOT on the exponential/native
	// siblings, whose route A is where #2385's OOMs happen.
	if f, ok := fanout.(*chplan.RangeBucketFanout); ok {
		f.PeakIndependentOfGrid = true
	}
	anchorRef := &chplan.ColumnRef{Name: stepGridAnchorColumn}
	rangeStart, rangeEnd := fanoutWindowBoundsExpr(anchorRef, in.win)
	return classicBucketWindowReshape(
		fanout,
		// countValues=nil and resets=nil — see classicBucketWindowStage's
		// identical call for why the classic caller reads durationToZero
		// off each rung's own values rather than a separate count series,
		// and detects each rung's reset on that rung alone.
		histogramWindowFold(in.shape.windowFn, histogramWindowInputs{
			rangeStart:  rangeStart,
			rangeEnd:    rangeEnd,
			temporality: classicBucketWindowTemporalityExpr(in.s, in.shape.windowFn),
		}),
		[]chplan.Projection{
			{Expr: anchorRef, Alias: stepGridAnchorColumn},
			{Expr: &chplan.ColumnRef{Name: in.s.AttributesColumn}, Alias: in.s.AttributesColumn},
		},
		in.s,
	)
}

// NativeClassicHistogramWindowLowerer is the boot-wired
// ClassicHistogramWindowLowerer that emits the ClickHouse-native ladder
// aggregate (a chplan.RangeBucketGridNative) for shape-eligible windows.
// cmd/cerberus wires it ONLY when chopt resolved the ts_grid_histogram
// feature at boot. It embeds a concrete Fallback: a shape it cannot handle
// delegates rather than returning nil.
type NativeClassicHistogramWindowLowerer struct {
	// Fallback is the concrete lowerer for shapes the native path cannot
	// handle. Boot wires it to FanoutClassicHistogramWindowLowerer{}.
	Fallback ClassicHistogramWindowLowerer
}

// LowerClassicHistogramWindow returns the native ladder aggregate for an
// eligible window, or delegates to the embedded Fallback otherwise.
//
// A schema that declares an AggregationTemporality column splits into
// COMPLEMENTARY arms, exactly as [NativeRateLowerer] splits the scalar
// counter path: the native aggregate reads a cumulative counter and has no
// DELTA semantics, so DELTA-temporality rows keep the fan-out's own
// delta-sum branch (counterIncreaseFold) while every other row rides the
// native ladder. Both arms publish the identical
// `(anchor_ts, Attributes, BucketCounts, ExplicitBounds)` column list in the
// identical order, so the positional UnionAll between them is sound and the
// across-series merge above is unaffected.
func (n NativeClassicHistogramWindowLowerer) LowerClassicHistogramWindow(in classicHistogramWindowInput) chplan.Node {
	if !nativeClassicHistogramEligible(in) {
		return n.Fallback.LowerClassicHistogramWindow(in)
	}
	if in.s.AggregationTemporalityColumn == "" {
		return nativeClassicHistogramNode(in, in.pred)
	}
	cumulative := nativeClassicHistogramNode(in,
		andPredicate(in.pred, temporalityPredicate(in.s.AggregationTemporalityColumn, chplan.OpNe)))
	delta := in
	delta.pred = andPredicate(in.pred, temporalityPredicate(in.s.AggregationTemporalityColumn, chplan.OpEq))
	return &chplan.UnionAll{Inputs: []chplan.Node{
		cumulative,
		n.Fallback.LowerClassicHistogramWindow(delta),
	}}
}

// nativeClassicHistogramEligible is the intrinsic SHAPE check. Each clause
// names a semantic the native aggregate cannot reproduce, never a feature
// flag:
//
//   - windowFn must be `rate`. timeSeriesRateToGrid divides the window's
//     extrapolated increase by the window seconds, which is what `rate`
//     means; `increase` / `sum_over_time` / `delta` / `irate` / `idelta` are
//     different reductions with no member of the family behind them.
//   - the grid must be MATERIALISED (Step > 0 with both bounds pinned): the
//     aggregate takes (start, end, step) as constant parameters, so there is
//     nothing to hand it in instant mode.
//   - a subquery inner's epoch-aligned grid (ctx.stepAligned) is left on the
//     fan-out: chplan.RangeBucketGridNative carries no StepAlign field, so a
//     re-anchoring consumer would re-derive the wrong grid for it.
//   - the grid step, the window and the offset must each be a WHOLE number of
//     seconds. timeSeriesRateToGrid takes grid_step and window as whole-second
//     UInt32 parameters and its start/end as whole-second DateTime bounds, so a
//     sub-second `[500ms]` window would silently truncate to 0 (and a
//     sub-second step to a grid the aggregate cannot express). The array fold
//     evaluates those durations at nanosecond precision, so a sub-second shape
//     stays on it rather than being answered at the wrong resolution.
func nativeClassicHistogramEligible(in classicHistogramWindowInput) bool {
	if in.shape.windowFn != rateWindowFn {
		return false
	}
	if in.ctx.step <= 0 || in.ctx.start.IsZero() || in.ctx.end.IsZero() {
		return false
	}
	if in.ctx.stepAligned {
		return false
	}
	if in.win.lookback <= 0 {
		return false
	}
	return wholeSeconds(in.ctx.step) && wholeSeconds(in.win.lookback) && wholeSeconds(in.win.offset)
}

// wholeSeconds reports whether d is an exact multiple of one second — the
// resolution every parameter of the timeSeries*ToGrid family is expressed in.
func wholeSeconds(d time.Duration) bool {
	return d%time.Second == 0
}

// nativeClassicHistogramNode builds the native ladder aggregate over the
// matchers-filtered, `le`-restricted scan. pred is the predicate for THIS
// arm (the caller folds the temporality split into it), so the two arms of a
// split read complementary row sets from the same table.
func nativeClassicHistogramNode(in classicHistogramWindowInput, pred chplan.Expr) chplan.Node {
	var rawSide chplan.Node = in.scan
	if pred != nil {
		rawSide = &chplan.Filter{Input: in.scan, Predicate: pred}
	}
	// `le` matcher restriction (#1478) — narrows BucketCounts /
	// ExplicitBounds to the retained rungs BEFORE the unnest reads them, so
	// the native ladder is built over the restricted layout exactly as the
	// fan-out's aggregation is. No-op when leMatchers is empty.
	rawSide = classicBucketLeRestriction(rawSide, in.leMatchers, in.s)

	return &chplan.RangeBucketGridNative{
		Input: rawSide,
		Start: in.ctx.start.UTC(),
		End:   in.ctx.end.UTC(),
		Step:  in.ctx.step,
		Range: in.win.lookback,
		// The fan-out's window is `(anchor - Offset - Range, anchor - Offset]`
		// and the aggregate's is the same span, so Offset threads through
		// unchanged — see chplan.RangeBucketGridNative.Offset.
		Offset: in.win.offset,
		// The grouping keys read the raw table columns, so this is a
		// series-identity BINDING site — see [canonicalGroupKeyExpr].
		GroupBy:           canonicalGroupKeyExprs([]chplan.Expr{histogramIdentityExpr(in.s)}, in.s),
		GroupByAliases:    []string{in.s.AttributesColumn},
		AnchorAlias:       stepGridAnchorColumn,
		TimestampCol:      in.s.TimestampColumn,
		BucketCountsCol:   in.s.BucketCountsColumn,
		ExplicitBoundsCol: in.s.ExplicitBoundsColumn,
	}
}

// temporalityPredicate renders `<col> <op> DELTA`. OpNe admits CUMULATIVE
// and the legacy UNSPECIFIED reading, which is the row set the native
// cumulative-counter aggregate is defined on; OpEq selects the complement.
func temporalityPredicate(column string, op chplan.BinaryOp) chplan.Expr {
	return &chplan.Binary{
		Op:    op,
		Left:  &chplan.ColumnRef{Name: column},
		Right: &chplan.LitInt{V: schema.AggregationTemporalityDelta},
	}
}

// andPredicate conjoins two selector predicates, tolerating a nil left side
// (a selector with no label matchers at all).
func andPredicate(left, right chplan.Expr) chplan.Expr {
	if left == nil {
		return right
	}
	return &chplan.Binary{Op: chplan.OpAnd, Left: left, Right: right}
}
