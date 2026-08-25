package promql

import (
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_timestamp.go lowers `timestamp(<exp-histogram-valued
// argument>)` (cerberus issue #2474). It is the sibling `sort`/`sort_desc`
// (#2456) and `clamp`/`absent` (#2345/#2443) already have: `lowerDateFn`
// (date_fns.go) calls the generic `lower()` on its argument unconditionally,
// which for a bare exp-histogram selector falls through to
// `expHistogramSelectorRouting`'s catch-all rejection instead of ever
// reaching `timestamp`'s own logic — and for any OTHER histogram-valued
// shape (`sum(m_exp_hist)`, `rate(m_exp_hist[5m])`, …) the recursive
// `lower()` call bypasses [lowerRoot]'s histogram-aware dispatch entirely
// (that dispatch only runs at the ROOT of the whole expression tree), so
// even a shape like `sum(m_exp_hist)` — which lowers successfully on its
// own — hits the exact same rejection once wrapped in `timestamp(...)`.
//
// Reference semantics (tsouza/prometheus@cerberus-parser promql/engine.go,
// funcTimestamp / rangeEvalTimestampFunctionOverVectorSelector — see
// date_fns.go's [timestampResultExpr] for the full citation): `timestamp(v)`
// reads only the selected sample's `Point.T`, never `Point.H`/`Point.F`, so
// it answers identically whether v is float- or histogram-valued. The
// PARSER SHAPE of the argument — not its runtime type — picks WHICH
// timestamp:
//
//   - a bare (optionally parenthesised / `@`-pinned) VectorSelector reports
//     the SELECTED SAMPLE's own raw timestamp;
//   - every other shape reports the EVALUATION instant instead.
//
// This file reproduces that same split for a histogram-valued argument:
//
//   - [lowerTimestampOverExpHistogramBareSelector] handles the selector
//     case. It needs a raw timestamp none of the existing histogram
//     lowerings expose: [nativeHistogramProjection] (histogram_native_bare.go)
//     — the projection every OTHER bare-selector-rooted histogram
//     consumer shares — collapses to the newest sample via
//     `argMax(<col>, TimeUnix)` and then stamps its OWN TimeUnix output
//     with the eval instant (`now64(9)` / the grid anchor), discarding the
//     selected sample's actual time. That eval-instant stamp is correct
//     for every other histogram-valued consumer (their VALUE math never
//     depends on the sample's own time — see [lowerHistogramValueFnInstant]),
//     but wrong for `timestamp()`, so this sibling mirrors
//     [lowerExpHistogramBare]'s three range-grid branches with a much
//     lighter aggregate — just `max(TimeUnix)`, via
//     [histogramSampleTimestampAgg] — since `timestamp()` never touches a
//     single bucket/count/sum column.
//   - every other histogram-valued shape reuses
//     [lowerExpHistogramValuedShape] directly (the same recognizer
//     `sort`/`instant_fns.go` already gate on) purely for its ROW
//     IDENTITY (Attributes) and PLAN SHAPE; [projectExpHistogramEvalInstant]
//     discards that plan's own TimeUnix column (which may be wall-clock
//     `now64(9)`, not the request's eval anchor — see
//     [nativeHistogramProjection]'s doc) and replaces it with
//     [evalInstantExpr], the SAME expression the float, non-selector arm
//     of [timestampResultExpr] already uses.
func lowerTimestampOverExpHistogram(arg parser.Expr, s schema.Metrics, ctx lowerCtx) (chplan.Node, bool, error) {
	if vs, ok := bareExpHistogramSelector(arg, s, ctx); ok {
		node, err := lowerTimestampOverExpHistogramBareSelector(vs, s, ctx)
		return node, true, err
	}
	// A mixed float/histogram `or` argument (cerberus issue #2330),
	// reached from ANY nesting depth since `timestamp` always routes
	// through [lowerDateFn] regardless of whether it sits at the query
	// root or nested under another wrapper (cerberus issue #2611). See
	// histogram_native_mixed_or_timestamp.go's own doc comment for why
	// this is never the bare-selector case above (a mixed `or` is a
	// BinaryExpr, never a VectorSelector) and always reports the
	// evaluation instant for every row.
	if b, ok := timestampOverMixedExpHistogramSetOp(arg, s, ctx); ok {
		node, err := lowerTimestampOverMixedExpHistogramSetOp(b, s, ctx)
		return node, true, err
	}
	if hist, ok, err := lowerExpHistogramValuedShape(arg, s, ctx); ok {
		if err != nil {
			return nil, true, err
		}
		return projectExpHistogramEvalInstant(hist, s, ctx), true, nil
	}
	// A nested drop-family argument (cerberus issue #2528) — e.g.
	// `timestamp(demo_latency_exp_hist + 0)`. The dropped result is
	// already an empty float vector, so no row ever surfaces a value for
	// [dateFnExpr]'s "timestamp" rewrite to apply to; the canonical
	// empty shape [lowerExpHistogramDroppingShape] already built is the
	// answer as-is.
	if dropped, ok, err := lowerExpHistogramDroppingShape(arg, s, ctx); ok {
		return dropped, true, err
	}
	return nil, false, nil
}

// expHistogramSampleTimestampAlias names the one column
// [histogramSampleTimestampAgg] adds to an otherwise-ordinary histogram
// bare-selector aggregate: the raw `max(TimeUnix)` of the selected sample.
// It is deliberately distinct from both s.TimestampColumn (which the
// range-mode branches use for the STEP ANCHOR — see [timestampRangeProjection])
// and [stepGridAnchorColumn], so a single row can carry both without either
// value shadowing the other.
const expHistogramSampleTimestampAlias = "exp_hist_sample_ts"

// histogramSampleTimestampAgg is the aggregate function `timestamp()` needs
// from a bare exp-histogram selector: just the SELECTED SAMPLE's own raw
// TimeUnix. Unlike [nativeExpHistBareAggs] it never reads a single field of
// the histogram itself — `timestamp(v)` answers `Point.T` and never touches
// `Point.H`/`Point.F` — so the bucket ladder never needs to leave the
// aggregate.
func histogramSampleTimestampAgg(s schema.Metrics) []chplan.AggFunc {
	return []chplan.AggFunc{
		{
			Fn:    chplan.FnMax,
			Args:  []chplan.Expr{&chplan.ColumnRef{Name: s.TimestampColumn}},
			Alias: expHistogramSampleTimestampAlias,
		},
	}
}

// lowerTimestampOverExpHistogramBareSelector lowers `timestamp(<bare
// exp-histogram selector>)` across the same three range-grid shapes
// [lowerExpHistogramBare] distinguishes — see this file's doc for why it
// cannot reuse that sibling's own projection.
func lowerTimestampOverExpHistogramBareSelector(vs *parser.VectorSelector, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	switch rangeGridShapeFor(vs, ctx) {
	case gridFanout:
		scan := &chplan.Scan{Table: s.ExpHistogramTable}
		pred := buildPredicate(vs.LabelMatchers, s)
		fanout := buildHistogramBucketFanout(
			scan, pred, nil, windowFor(vs, instantLookback),
			[]chplan.Expr{histogramIdentityExpr(s)}, []string{s.AttributesColumn},
			histogramSampleTimestampAgg(s), s, ctx,
		)
		return timestampRangeProjection(fanout, s), nil
	case gridBroadcast:
		// Same split as [lowerExpHistogramBare]'s own gridBroadcast arm:
		// the pinned window is resolved ONCE via the instant-mode
		// aggregate, then fanned across the step grid — every step
		// reports the SAME selected sample's timestamp, at its own
		// step position.
		latest, err := expHistogramSampleTimestampLatest(vs, s, ctx)
		if err != nil {
			return nil, err
		}
		grid := &chplan.StepGrid{Start: ctx.start.UTC(), End: ctx.end.UTC(), Step: ctx.step}
		return timestampRangeProjection(&chplan.CrossJoin{Left: grid, Right: latest}, s), nil
	default:
		latest, err := expHistogramSampleTimestampLatest(vs, s, ctx)
		if err != nil {
			return nil, err
		}
		return timestampInstantProjection(latest, s), nil
	}
}

// expHistogramSampleTimestampLatest builds the instant-mode subtree beneath
// [lowerTimestampOverExpHistogramBareSelector]'s single-anchor and
// gridBroadcast branches: the filtered exp-histogram scan collapsed to the
// newest in-window sample per series, exposing only
// [expHistogramSampleTimestampAlias]. Window/predicate logic is identical
// to [expHistogramBareLatest] — same matchers, same instant-window bound —
// only the aggregate list differs.
func expHistogramSampleTimestampLatest(vs *parser.VectorSelector, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	scan := &chplan.Scan{Table: s.ExpHistogramTable}
	pred := buildPredicate(vs.LabelMatchers, s)
	pred, err := andInstantWindow(pred, vs, s.TimestampColumn, ctx)
	if err != nil {
		return nil, err
	}
	var input chplan.Node = scan
	if pred != nil {
		input = &chplan.Filter{Input: scan, Predicate: pred}
	}
	return latestSampleAgg(input, histogramSampleTimestampAgg(s), s), nil
}

// timestampInstantProjection projects the INSTANT-mode latest-sample
// aggregate into the canonical float Sample shape `timestamp()` reports:
// `__name__` drops (a derived sample, like every other histogram-valued
// function's output — see [bareExpHistogramNameExpr]'s doc for the
// bare-selector exception that does NOT apply here), and BOTH the reported
// TimeUnix and the Value are the raw sample time — mirroring the plain
// float instant seam, where `timestamp(<selector>)` reads the very column
// the LWR aggregate already reports as TimeUnix (see [timestampResultExpr]'s
// doc: "the instant seam already emits `max(TimeUnix) AS lwr_ts`").
func timestampInstantProjection(inner chplan.Node, s schema.Metrics) chplan.Node {
	ts := &chplan.ColumnRef{Name: expHistogramSampleTimestampAlias}
	return &chplan.Project{
		Input: inner,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: ts, Alias: s.TimestampColumn},
			{Expr: asFloat64(dateFnExpr("timestamp", nil, ts)), Alias: s.ValueColumn},
		},
	}
}

// timestampRangeProjection projects a RANGE-mode plan — either
// [buildHistogramBucketFanout]'s per-(series,anchor) fan-out or the
// gridBroadcast CrossJoin — into the canonical float Sample shape. Unlike
// the instant projection, the reported TimeUnix and the Value diverge: the
// former is the STEP ANCHOR (every range-mode row is positioned along the
// matrix by its anchor, exactly like [lowerHistogramValueFnRange] and
// [nativeHistogramProjection]'s own range branches), while the latter stays
// the selected sample's raw time, via [expHistogramSampleTimestampAlias].
func timestampRangeProjection(inner chplan.Node, s schema.Metrics) chplan.Node {
	anchor := &chplan.ColumnRef{Name: stepGridAnchorColumn}
	rawTs := &chplan.ColumnRef{Name: expHistogramSampleTimestampAlias}
	return &chplan.Project{
		Input: inner,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: anchor, Alias: s.TimestampColumn},
			{Expr: asFloat64(dateFnExpr("timestamp", nil, rawTs)), Alias: s.ValueColumn},
		},
	}
}

// projectExpHistogramEvalInstant projects any OTHER exp-histogram-valued
// shape (`sum(m)`, `rate(m[5m])`, a histogram/histogram binop, …) — i.e.
// every shape [lowerExpHistogramValuedShape] recognises OTHER than a bare
// selector, which [lowerTimestampOverExpHistogram] routes to
// [lowerTimestampOverExpHistogramBareSelector] instead — into the float
// Sample shape `timestamp()` reports for a non-selector argument: the
// EVALUATION instant, via [evalInstantExpr], the same expression the
// float, non-selector arm of [timestampResultExpr] already uses.
//
// hist's own TimeUnix column is deliberately never read here: every
// histogram-valued lowering stamps it with SOME eval-instant-shaped value
// of its own choosing (`now64(9)` in instant mode — see
// [nativeHistogramProjection]'s doc — or the grid anchor in range mode),
// which for the RANGE case happens to already equal [evalInstantExpr]'s
// answer but for the INSTANT case does not: `now64(9)` is the wall-clock
// time the query executes, not the request's own eval anchor
// ([evalInstantExpr] reads the anchor the request pinned, falling back to
// `now64(9)` only when none was threaded). Overriding it here rather than
// forwarding it keeps this projection correct for both modes without a
// mode branch of its own — [evalInstantExpr] already contains that branch.
func projectExpHistogramEvalInstant(hist chplan.Node, s schema.Metrics, ctx lowerCtx) chplan.Node {
	ts := evalInstantExpr(s, ctx)
	return &chplan.Project{
		Input: hist,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: ts, Alias: s.TimestampColumn},
			{Expr: asFloat64(dateFnExpr("timestamp", nil, ts)), Alias: s.ValueColumn},
		},
	}
}
