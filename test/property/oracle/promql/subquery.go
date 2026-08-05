package promql

import (
	"fmt"
	"sort"
	"time"

	"github.com/prometheus/prometheus/promql/parser"
)

// defaultSubqueryStep is the resolution substituted when a subquery
// omits an explicit `:resolution` (`expr[5m:]`). Matches Prometheus's
// default and cerberus's own lowering (internal/promql/subquery.go's
// defaultSubqueryStep).
const defaultSubqueryStep = time.Minute

// evalSubqueryExpr evaluates a subquery (`<expr>[range:resolution]`)
// into a []RangePoints: one entry per distinct output series, each
// carrying the samples produced by evaluating expr as an independent
// INSTANT query at every point of an epoch-aligned anchor grid inside
// (end-range, end].
//
// This mirrors reference Prometheus's engine.go eval()'s
// `case *parser.SubqueryExpr:` exactly (durationMilliseconds +
// noStepSubqueryIntervalFn):
//
//	newEv.interval = step (or the default when step == 0)
//	newEv.startTimestamp = interval * ((end - interval) / interval)   // floor to a multiple of interval
//	if newEv.startTimestamp <= (end - rangeMs) {
//	    newEv.startTimestamp += interval                              // left-exclusive lower bound
//	}
//	// grid runs newEv.startTimestamp, +interval, ... up to and including end
//
// end already has the subquery's own @-modifier + offset applied,
// mirroring effectiveEvalTs/OriginalOffset for a plain VectorSelector.
// Each anchor evaluates sub.Expr as a fully independent instant query at
// that timestamp — offsets/​@ modifiers inside sub.Expr resolve relative
// to that anchor, and any nested @start()/@end() still resolves against
// the OUTER query's startMs/endMs (PromQL's @start()/@end() always name
// the top-level query's own bounds, never a subquery's synthetic grid),
// which falls out naturally since e.startMs/e.endMs are untouched here.
//
// Nested subquery-of-subquery (`expr[r1:s1][r2:s2]`) is out of scope — a
// genuinely rare shape — and returns a clear error rather than silently
// mishandling it.
func (e *Evaluator) evalSubqueryExpr(sub *parser.SubqueryExpr, evalTsMs int64) ([]RangePoints, error) {
	if _, nested := sub.Expr.(*parser.SubqueryExpr); nested {
		return nil, fmt.Errorf("oracle: nested subquery-of-subquery not supported")
	}

	end := subqueryEffectiveTs(sub, evalTsMs, e.startMs, e.endMs)
	end -= sub.OriginalOffset.Milliseconds()

	stepMs := sub.Step.Milliseconds()
	if stepMs == 0 {
		stepMs = defaultSubqueryStep.Milliseconds()
	}
	rangeMs := sub.Range.Milliseconds()

	lowerBound := end - rangeMs
	start := stepMs * (lowerBound / stepMs)
	if start <= lowerBound {
		start += stepMs
	}

	series := make(map[string]*RangePoints)
	order := make([]string, 0)
	for t := start; t <= end; t += stepMs {
		v, err := e.evalAny(sub.Expr, t)
		if err != nil {
			return nil, err
		}
		if v.Kind != kindVec {
			return nil, fmt.Errorf("oracle: subquery inner expression must evaluate to an instant vector, got kind=%d", v.Kind)
		}
		for _, row := range v.Vec {
			key := labelKey(row.Labels)
			rp, ok := series[key]
			if !ok {
				rp = &RangePoints{Labels: CopyLabels(row.Labels)}
				series[key] = rp
				order = append(order, key)
			}
			rp.Samples = append(rp.Samples, Sample{T: t, V: row.V})
		}
	}

	sort.Strings(order)
	out := make([]RangePoints, 0, len(order))
	for _, k := range order {
		out = append(out, *series[k])
	}
	return out, nil
}

// subqueryEffectiveTs is effectiveEvalTs's counterpart for
// *parser.SubqueryExpr, which carries its own Timestamp/StartOrEnd
// fields for @-modifier support (independent of the inner expression's
// own @ modifier, if any).
func subqueryEffectiveTs(v *parser.SubqueryExpr, evalTsMs, startMs, endMs int64) int64 {
	if v.Timestamp != nil {
		return *v.Timestamp
	}
	switch v.StartOrEnd {
	case parser.START:
		return startMs
	case parser.END:
		return endMs
	}
	return evalTsMs
}

// evalRangeArg evaluates a range-vector function's matrix argument — a
// bare matrix selector (`m[5m]`) or a subquery (`<expr>[5m:1m]`, added
// for #1694, e.g. `max_over_time(rate(m[5m])[10m:1m])`) — into its
// per-series window samples, the window's duration, and the "effective"
// end timestamp rate/increase/delta's extrapolation anchors to (the
// (T-range, T] mathematical interval after @-modifier + offset shifts —
// not necessarily the first/last actual sample timestamp).
//
// Note: reference Prometheus's own extrapolatedRate hard type-asserts
// its arg as *parser.MatrixSelector (promql/functions.go), so
// rate/increase/delta over a bare subquery argument panics upstream —
// not a shape this oracle needs to reproduce faithfully, since no
// exotic-matrix case exercises it. Every other range function
// (*_over_time, deriv, predict_linear, quantile_over_time,
// double_exponential_smoothing) only touches the resulting Matrix
// samples, so they support a subquery argument fine both upstream and
// here.
func (e *Evaluator) evalRangeArg(arg parser.Expr, evalTsMs int64) (ranges []RangePoints, rangeMs, effectiveTs int64, err error) {
	switch v := arg.(type) {
	case *parser.MatrixSelector:
		vs, _ := v.VectorSelector.(*parser.VectorSelector)
		effectiveTs = evalTsMs
		if vs != nil {
			effectiveTs = effectiveEvalTs(vs, evalTsMs, e.startMs, e.endMs)
			effectiveTs -= vs.OriginalOffset.Milliseconds()
		}
		return e.evalMatrixSelector(v, evalTsMs), v.Range.Milliseconds(), effectiveTs, nil
	case *parser.SubqueryExpr:
		ranges, err = e.evalSubqueryExpr(v, evalTsMs)
		if err != nil {
			return nil, 0, 0, err
		}
		effectiveTs = subqueryEffectiveTs(v, evalTsMs, e.startMs, e.endMs) - v.OriginalOffset.Milliseconds()
		return ranges, v.Range.Milliseconds(), effectiveTs, nil
	}
	return nil, 0, 0, fmt.Errorf("oracle: range-vector argument must be a matrix selector or subquery, got %T", arg)
}
