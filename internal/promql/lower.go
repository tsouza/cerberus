package promql

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"github.com/tsouza/cerberus/internal/cerbtrace"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// tracer emits the `lower` pipeline-stage span for PromQL lowering.
var tracer = otel.Tracer("github.com/tsouza/cerberus/internal/promql")

// Lower turns a parsed PromQL expression into a chplan tree, using s for
// table and column name conventions.
//
// Supports: VectorSelector, MatrixSelector (only as a Call argument),
// range-vector Call (`rate` / `increase` / `delta` / `*_over_time`),
// instant-vector Call (`abs`, `sqrt`, `ln`, ...), AggregateExpr with
// `by (...)`, ParenExpr, BinaryExpr with scalar/vector arithmetic,
// SubqueryExpr (P0 4.5–4.7: bare-vector, over range-vector calls,
// outer reducer over subquery).
//
// Deferred to RC3 / later milestones: nested subqueries, subquery
// over AggregateExpr, subquery `@ start()`/`@ end()`, native-histogram
// `histogram_quantile` (PR H, otel_metrics_exp_histogram), exemplars.
// Classic-histogram `histogram_quantile(phi, <selector>)` is supported
// via lowerHistogramQuantile against the OTel-CH classic histogram
// table (BucketCounts × ExplicitBounds arrays).
func Lower(ctx context.Context, expr parser.Expr, s schema.Metrics) (chplan.Node, error) {
	_, span := tracer.Start(ctx, cerbtrace.SpanLower, trace.WithAttributes(cerbtrace.AttrQL.String("promql")))
	defer span.End()
	plan, err := lower(expr, s, lowerCtx{})
	if err != nil {
		span.RecordError(err)
		return nil, err
	}
	span.SetAttributes(cerbtrace.AttrPlanNodeCount.Int(cerbtrace.CountNodes(plan)))
	return plan, nil
}

// LowerAt is the time-aware variant of [Lower] used by handlers that
// know the query's evaluation range (start / end). It threads those
// times through to the `@ start()` / `@ end()` modifier resolution so
// `metric @ start()` lowers against the request's start time instead
// of erroring out.
//
// For an instant query the API layer passes start == end == ts; for a
// query_range it passes the request's start / end.
//
// LowerAt is the instant-mode entry point — it leaves step at zero, so
// the synthetic-vector lowerings continue to emit a single `OneRow`
// row (the instant query produces a single sample at the eval ts).
// Range-mode callers use [LowerAtRange] to thread a step duration in
// so the same synthetic shapes materialise as a StepGrid fanned across
// the eval window.
func LowerAt(ctx context.Context, expr parser.Expr, s schema.Metrics, start, end time.Time) (chplan.Node, error) {
	return LowerAtRange(ctx, expr, s, start, end, 0)
}

// LowerAtRange is the range-mode variant of [LowerAt]: it threads the
// query_range step duration through to the lowering context so the
// no-driving-vector synthetic shapes (`time()`, `vector(N)`, zero-arg
// date fns, `absent(...)`) emit one row per step in `[start, end]`
// instead of a single row at the eval anchor.
//
// step == 0 is equivalent to [LowerAt] (instant mode); the lowering
// keeps the OneRow source so existing per-fixture SQL stays
// byte-stable. Callers that pass step > 0 MUST also pass non-zero
// start / end (the StepGrid emitter renders them as inline DateTime64
// literals).
func LowerAtRange(ctx context.Context, expr parser.Expr, s schema.Metrics, start, end time.Time, step time.Duration) (chplan.Node, error) {
	_, span := tracer.Start(ctx, cerbtrace.SpanLower, trace.WithAttributes(cerbtrace.AttrQL.String("promql")))
	defer span.End()
	plan, err := lower(expr, s, lowerCtx{start: start, end: end, step: step})
	if err != nil {
		span.RecordError(err)
		return nil, err
	}
	span.SetAttributes(cerbtrace.AttrPlanNodeCount.Int(cerbtrace.CountNodes(plan)))
	return plan, nil
}

func lower(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	switch e := expr.(type) {
	case *parser.VectorSelector:
		return lowerVectorSelector(e, s, ctx)
	case *parser.Call:
		return lowerCall(e, s, ctx)
	case *parser.AggregateExpr:
		return lowerAggregate(e, s, ctx)
	case *parser.ParenExpr:
		return lower(e.Expr, s, ctx)
	case *parser.BinaryExpr:
		return lowerBinary(e, s, ctx)
	case *parser.SubqueryExpr:
		return lowerSubquery(e, s, ctx)
	case *parser.UnaryExpr:
		return lowerUnary(e, s, ctx)
	default:
		return nil, fmt.Errorf("promql: unsupported expression %T", expr)
	}
}

// lowerVectorSelector turns `metric{label="val"}` into Scan + Filter.
// `@` and `offset` modifiers add a `Timestamp <= anchor` predicate so the
// instant evaluation reflects the requested shifted time.
//
// When ctx.inRangeVector is false (the default — top-level selector,
// under aggregations, or inside instant arithmetic) cerberus also
// applies PromQL's Latest-With-Respect-to-T (LWR) rule: filter the
// scan to samples with `Timestamp <= anchor` AND
// `anchor - Timestamp < 5m` (Prom's default staleness window), then
// collapse to one row per series via `argMax(Value, TimeUnix)` /
// `max(TimeUnix)` grouped by `(MetricName, Attributes)`. That's the
// per-series-latest-within-lookback contract any downstream aggregation
// must aggregate over. Range-vector consumers (rate / *_over_time /
// subqueries) bypass the LWR wrap by setting `inRangeVector` before
// recursing — the RangeWindow node owns the in-window aggregation
// itself.
func lowerVectorSelector(v *parser.VectorSelector, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	metricName := metricNameFromMatchers(v.LabelMatchers)
	table := s.GaugeTable
	if metricName != "" {
		table = s.TableFor(metricName)
	}

	scan := &chplan.Scan{Table: table}

	pred := buildPredicate(v.LabelMatchers, s)

	// Resolve the effective evaluation anchor for this selector.
	// `@`/offset modifiers shadow the surrounding ctx; absent a
	// modifier we pick up ctx.end (the query's eval timestamp) so
	// the LWR predicate below has something to compare against.
	anchor, err := selectorAnchor(v, ctx)
	if err != nil {
		return nil, err
	}

	if ctx.inRangeVector {
		// Inside a range vector / subquery the surrounding node owns
		// the per-window aggregation. We still apply the modifier's
		// `Timestamp <= anchor` bound when present (matching the pre-
		// LWR behaviour) so the range-vector pipeline only sees
		// samples up to the requested instant.
		if hasModifier(v) {
			timeBound := timeBoundExpr(s.TimestampColumn, anchor)
			if pred == nil {
				pred = timeBound
			} else {
				pred = &chplan.Binary{Op: chplan.OpAnd, Left: pred, Right: timeBound}
			}
		}
		if pred == nil {
			return scan, nil
		}
		return &chplan.Filter{Input: scan, Predicate: pred}, nil
	}
	// Range mode (ctx.step > 0): build the per-step LWR by cross-joining
	// the raw scan with a StepGrid and collapsing latest-per-(series,
	// anchor). Anchor modifiers (`offset`) are honoured by shifting the
	// predicate against `anchor_ts` rather than a single end_ts.
	if ctx.step > 0 && !ctx.start.IsZero() && !ctx.end.IsZero() {
		// `@<absolute>` / `@ start()` / `@ end()` pin a single anchor
		// across all steps — every step evaluates the same fixed-time
		// LWR. Collapse the StepGrid fan-out: run the LWR once at the
		// pinned anchor (yielding one row per series, same shape as
		// instant mode) and broadcast across the step grid via
		// CrossJoin so the matrix pivot still receives one row per
		// (series, step).
		if hasAbsoluteAt(v) {
			return wrapRangeAbsoluteAtBroadcast(scan, pred, anchor, ctx, s), nil
		}
		return wrapRangeLatestPerSeries(scan, pred, anchor, ctx, s), nil
	}
	// Instant-vector context: the LWR wrapper applies both the
	// `Timestamp <= anchor` upper bound and the staleness lower
	// bound, so we DON'T pre-add the modifier's timeBoundExpr here —
	// that would duplicate the upper-bound predicate.
	return wrapInstantLatestPerSeries(scan, pred, anchor, s), nil
}

// wrapInstantLatestPerSeries adds the LWR + staleness predicates on
// top of (scan, pred) and collapses to one row per `(MetricName,
// Attributes)` series via `argMax(Value, TimeUnix)`. The output
// preserves the canonical Sample-row schema — MetricName, Attributes,
// TimeUnix, Value — so the surrounding plan tree (Aggregate, Project,
// Filter, ...) keeps consuming the same column shape it did before
// the LWR wrap landed.
//
// Schema-preservation is what lets `wrapWithSampleProjection` upstream
// keep its non-derived-shape path: the root after this wrap is a
// chplan.Project whose output columns match the table's canonical
// names, so `isDerivedShape` returns false and the handler-side
// projection is a pass-through.
//
// Aliasing detail: the inner Aggregate projects the per-series TimeUnix
// + Value pair through temporary aliases (`lwr_ts`, `lwr_value`) so
// `argMax(Value, TimeUnix)` is unambiguous. CH otherwise rejects the
// query with ILLEGAL_AGGREGATION on the (TimeUnix-the-alias /
// TimeUnix-the-column) shadow inside the same SELECT projection list.
// The outer Project re-aliases back to the canonical names so the
// surrounding plan tree continues to see the same `MetricName /
// Attributes / TimeUnix / Value` shape.
func wrapInstantLatestPerSeries(scan *chplan.Scan, pred chplan.Expr, anchor evalAnchor, s schema.Metrics) chplan.Node {
	lwr := timeBoundExpr(s.TimestampColumn, anchor)
	staleness := stalenessLowerBoundExpr(s.TimestampColumn, anchor, instantLookback)
	combined := pred
	for _, p := range []chplan.Expr{lwr, staleness} {
		if combined == nil {
			combined = p
			continue
		}
		combined = &chplan.Binary{Op: chplan.OpAnd, Left: combined, Right: p}
	}
	filtered := &chplan.Filter{Input: scan, Predicate: combined}

	const (
		lwrTsAlias    = "lwr_ts"
		lwrValueAlias = "lwr_value"
	)

	agg := &chplan.Aggregate{
		Input: filtered,
		GroupBy: []chplan.Expr{
			&chplan.ColumnRef{Name: s.MetricNameColumn},
			&chplan.ColumnRef{Name: s.AttributesColumn},
		},
		GroupByAliases: []string{s.MetricNameColumn, s.AttributesColumn},
		AggFuncs: []chplan.AggFunc{
			{
				Name:  "max",
				Args:  []chplan.Expr{&chplan.ColumnRef{Name: s.TimestampColumn}},
				Alias: lwrTsAlias,
			},
			{
				Name: "argMax",
				Args: []chplan.Expr{
					&chplan.ColumnRef{Name: s.ValueColumn},
					&chplan.ColumnRef{Name: s.TimestampColumn},
				},
				Alias: lwrValueAlias,
			},
		},
	}

	return &chplan.Project{
		Input: agg,
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: s.MetricNameColumn}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: &chplan.ColumnRef{Name: lwrTsAlias}, Alias: s.TimestampColumn},
			{Expr: &chplan.ColumnRef{Name: lwrValueAlias}, Alias: s.ValueColumn},
		},
	}
}

// wrapRangeLatestPerSeries builds the per-step LWR for a vector
// selector evaluated over a query_range window. The shape is:
//
//	Project [MetricName, Attributes, anchor_ts AS TimeUnix, lwr_value AS Value]
//	  Aggregate by (MetricName, Attributes, anchor_ts) funcs=[argMax(Value, TimeUnix) AS lwr_value]
//	    Filter (anchor_ts - <offset> >= TimeUnix AND TimeUnix > anchor_ts - <offset> - 5m)
//	      CrossJoin(StepGrid(start, end, step), Scan + matchers_filter)
//
// At each step `t = start, start+step, ..., end` the StepGrid emits one
// `anchor_ts = t` row; the CrossJoin pairs that with every raw sample row,
// the Filter trims to the per-step LWR window `(t-offset-5m, t-offset]`,
// the Aggregate collapses to one row per (series, anchor) carrying the
// latest-in-window Value, and the outer Project exposes the canonical
// (MetricName, Attributes, TimeUnix, Value) shape with TimeUnix = anchor_ts.
//
// Output schema preservation lets the surrounding plan tree (aggregations,
// arithmetic, instant fns) keep consuming the same column shape it did
// before — the difference vs the instant LWR wrap is that each (series)
// produces N rows (one per step inside `[start, end]` that had data) rather
// than a single row at `end_ts`.
func wrapRangeLatestPerSeries(scan *chplan.Scan, pred chplan.Expr, anchor evalAnchor, ctx lowerCtx, s schema.Metrics) chplan.Node {
	const (
		anchorCol     = "anchor_ts"
		lwrValueAlias = "lwr_value"
	)
	anchorRef := &chplan.ColumnRef{Name: anchorCol}
	// `@`/offset modifiers shift the LWR window against the per-step
	// anchor: `up offset 5m` over a step at `t` evaluates the latest
	// sample in `(t-5m-5m, t-5m]`. The shift is `(anchor_ts - offset)`
	// for the upper bound and `(anchor_ts - offset - lookback)` for the
	// strict lower bound.
	//
	// When the modifier is `@<absolute>` (no offset, ctx.end ignored)
	// the per-step semantics still apply — Prom queries with `@ start()`
	// over a range query keep a single anchor across all steps. That
	// shape is rare enough that the wrap below falls back to a per-step
	// anchor (i.e., the user's intent is preserved when they don't pin
	// the absolute @ modifier).
	var upperBound chplan.Expr = anchorRef
	// Negative offsets shift the lookback window FORWARD in time relative
	// to each step's anchor (Prom evaluates `metric offset -5m` at
	// `t - (-5m) = t + 5m`). The subtraction below handles the sign
	// correctly — `anchor_ts - toIntervalNanosecond(-300_000_000_000)`
	// renders as `anchor_ts + 5m` per CH interval arithmetic. The
	// guard checks `!= 0` rather than `> 0` so the negative case isn't
	// silently zeroed.
	if anchor.Offset != 0 {
		upperBound = &chplan.Binary{
			Op:   chplan.OpSub,
			Left: anchorRef,
			Right: &chplan.FuncCall{
				Name: "toIntervalNanosecond",
				Args: []chplan.Expr{&chplan.LitInt{V: anchor.Offset.Nanoseconds()}},
			},
		}
	}
	// `TimeUnix <= anchor_ts - offset` (non-strict upper)
	lwrUpper := &chplan.Binary{
		Op:    chplan.OpLe,
		Left:  &chplan.ColumnRef{Name: s.TimestampColumn},
		Right: upperBound,
	}
	// `TimeUnix > anchor_ts - offset - lookback` (strict lower)
	lookbackNs := instantLookback.Nanoseconds() + anchor.Offset.Nanoseconds()
	lowerBound := &chplan.Binary{
		Op:   chplan.OpSub,
		Left: anchorRef,
		Right: &chplan.FuncCall{
			Name: "toIntervalNanosecond",
			Args: []chplan.Expr{&chplan.LitInt{V: lookbackNs}},
		},
	}
	lwrLower := &chplan.Binary{
		Op:    chplan.OpGt,
		Left:  &chplan.ColumnRef{Name: s.TimestampColumn},
		Right: lowerBound,
	}

	// Inner Scan/Filter — apply matchers via PREWHERE-eligible filter
	// the optimizer already promotes. The `(scan, pred)` split keeps the
	// downstream PREWHERE path unchanged; when pred is nil (no matchers)
	// the scan flows directly into the CrossJoin.
	var rawSide chplan.Node = scan
	if pred != nil {
		rawSide = &chplan.Filter{Input: scan, Predicate: pred}
	}

	stepGrid := &chplan.StepGrid{
		Start: ctx.start.UTC(),
		End:   ctx.end.UTC(),
		Step:  ctx.step,
	}
	joined := &chplan.CrossJoin{
		Left:  stepGrid,
		Right: rawSide,
	}

	// Filter to the per-step LWR window.
	filterPred := chplan.Expr(&chplan.Binary{
		Op: chplan.OpAnd, Left: lwrUpper, Right: lwrLower,
	})
	filtered := &chplan.Filter{Input: joined, Predicate: filterPred}

	// Aggregate per (MetricName, Attributes, anchor_ts) collapsing to
	// the latest sample in the per-step window. The argMax over
	// TimeUnix gives the LWR-canonical "newest sample in window".
	agg := &chplan.Aggregate{
		Input: filtered,
		GroupBy: []chplan.Expr{
			&chplan.ColumnRef{Name: s.MetricNameColumn},
			&chplan.ColumnRef{Name: s.AttributesColumn},
			anchorRef,
		},
		GroupByAliases: []string{s.MetricNameColumn, s.AttributesColumn, anchorCol},
		AggFuncs: []chplan.AggFunc{
			{
				Name: "argMax",
				Args: []chplan.Expr{
					&chplan.ColumnRef{Name: s.ValueColumn},
					&chplan.ColumnRef{Name: s.TimestampColumn},
				},
				Alias: lwrValueAlias,
			},
		},
	}

	// Outer Project re-aliases anchor_ts → TimeUnix so the canonical
	// 4-column Sample contract holds for downstream consumers
	// (aggregations, arithmetic, instant fns, the handler-side pivot).
	return &chplan.Project{
		Input: agg,
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: s.MetricNameColumn}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: anchorRef, Alias: s.TimestampColumn},
			{Expr: &chplan.ColumnRef{Name: lwrValueAlias}, Alias: s.ValueColumn},
		},
	}
}

// wrapRangeAbsoluteAtBroadcast is the range-mode lowering for a bare
// vector selector pinned by an ABSOLUTE `@` modifier (`@<unix-ts>`,
// `@ start()`, `@ end()`). The pinned anchor is fixed across every step
// in `[start, end]`, so every step evaluates the SAME LWR window and
// yields the same per-series value. Rather than emit the N-anchor
// StepGrid fan-out that the bare-selector path uses, this wrap:
//
//  1. Evaluates the LWR ONCE against the pinned anchor — produces 1 row
//     per series with the canonical `[MetricName, Attributes, lwr_value]`
//     shape (TimeUnix is dropped so it doesn't collide with the StepGrid's
//     anchor_ts in the outer scope).
//  2. CrossJoins with a StepGrid spanning the request window — yields
//     N (series, step) rows total.
//  3. Projects the StepGrid's anchor_ts as TimeUnix and the inner
//     lwr_value as Value — restoring the canonical 4-column Sample
//     contract for downstream consumers.
//
// Plan shape:
//
//	Project [MetricName, Attributes, anchor_ts AS TimeUnix, lwr_value AS Value]
//	  CrossJoin
//	    StepGrid(start, end, step)
//	    Project [MetricName, Attributes, lwr_value]
//	      Aggregate by(MetricName, Attributes) argMax(Value, TimeUnix) AS lwr_value
//	        Filter (matchers AND TimeUnix <= @ts AND TimeUnix > @ts - 5m)
//	          Scan(<table>)
//
// Response shape is unchanged: matrixFromCursor still receives N rows
// per series (one per step, all carrying the same Value at distinct
// step timestamps), so the JSON payload preserves Prom's expected
// N-sample matrix for a fixed-anchor query.
//
// The win is SQL complexity: the bucket-aggregate fan-out collapses to a
// single-pass LWR over the raw scan + a trivial broadcast — CH evaluates
// the staleness window once instead of N times, and the PREWHERE-eligible
// matchers stay on the bare scan (the optimizer promotes them as usual).
//
// Closes follow-up #2 from Pool-AK's PR #347.
func wrapRangeAbsoluteAtBroadcast(scan *chplan.Scan, pred chplan.Expr, anchor evalAnchor, ctx lowerCtx, s schema.Metrics) chplan.Node {
	// Inner: LWR collapsed once at the pinned anchor. The filter is the
	// same shape wrapInstantLatestPerSeries uses — Timestamp <= anchor
	// AND Timestamp > anchor - lookback — with offset (if any) folded
	// in via timeBoundExpr / stalenessLowerBoundExpr. Honoring offset
	// here lets `metric @ 1700000000 offset 5m` slide the LWR window
	// back by 5m and still produce a stable per-series result.
	lwr := timeBoundExpr(s.TimestampColumn, anchor)
	staleness := stalenessLowerBoundExpr(s.TimestampColumn, anchor, instantLookback)
	combined := pred
	for _, p := range []chplan.Expr{lwr, staleness} {
		if combined == nil {
			combined = p
			continue
		}
		combined = &chplan.Binary{Op: chplan.OpAnd, Left: combined, Right: p}
	}
	filtered := &chplan.Filter{Input: scan, Predicate: combined}

	const lwrValueAlias = "lwr_value"

	innerAgg := &chplan.Aggregate{
		Input: filtered,
		GroupBy: []chplan.Expr{
			&chplan.ColumnRef{Name: s.MetricNameColumn},
			&chplan.ColumnRef{Name: s.AttributesColumn},
		},
		GroupByAliases: []string{s.MetricNameColumn, s.AttributesColumn},
		AggFuncs: []chplan.AggFunc{{
			Name: "argMax",
			Args: []chplan.Expr{
				&chplan.ColumnRef{Name: s.ValueColumn},
				&chplan.ColumnRef{Name: s.TimestampColumn},
			},
			Alias: lwrValueAlias,
		}},
	}
	// Drop TimeUnix from the inner output so it doesn't collide with
	// the StepGrid's anchor_ts column once the two sides CrossJoin —
	// the outer Project re-projects anchor_ts into the TimeUnix slot.
	innerProject := &chplan.Project{
		Input: innerAgg,
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: s.MetricNameColumn}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: &chplan.ColumnRef{Name: lwrValueAlias}, Alias: lwrValueAlias},
		},
	}

	joined := &chplan.CrossJoin{
		Left:  &chplan.StepGrid{Start: ctx.start.UTC(), End: ctx.end.UTC(), Step: ctx.step},
		Right: innerProject,
	}

	// Re-shape the joined output into the canonical Sample 4-column
	// contract with TimeUnix sourced from the step grid's anchor_ts.
	return &chplan.Project{
		Input: joined,
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: s.MetricNameColumn}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: &chplan.ColumnRef{Name: "anchor_ts"}, Alias: s.TimestampColumn},
			{Expr: &chplan.ColumnRef{Name: lwrValueAlias}, Alias: s.ValueColumn},
		},
	}
}

// selectorAnchor resolves the effective evaluation anchor for a vector
// selector, threading through `@` / offset / start() / end() modifiers
// and falling back to the surrounding query's end timestamp. The zero
// anchor means "use `now64(9)` at the SQL level" — picked up by
// `timeBoundExpr` callers.
//
// `@<ts>` and `@ start()/@ end()` set the absolute anchor directly;
// `offset` shifts the anchor by a fixed delta and keeps whatever base
// anchor the rest of the resolution produced. So `up offset 5m`
// against a query with eval_ts = T anchors at `(T, offset=5m)` —
// `timeBoundExpr` then renders `Timestamp <= T - 5m` and the
// staleness predicate renders `Timestamp > T - 5m - lookback`.
func selectorAnchor(vs *parser.VectorSelector, ctx lowerCtx) (evalAnchor, error) {
	if hasModifier(vs) {
		a, err := anchorFromSelector(vs, ctx)
		if err != nil {
			return evalAnchor{}, err
		}
		// `up offset 5m` (no `@`) leaves anchorFromSelector with
		// `End == zero` because the selector itself doesn't pin an
		// absolute time. Without threading ctx.end through, the SQL
		// renders `now64(9)` and the LWR window would skew off the
		// real eval timestamp — bug-shaped for instant queries that
		// resolve eval_ts in the API layer. So back-fill End from
		// the surrounding query whenever an offset would otherwise
		// land on a zero anchor.
		if a.End.IsZero() && !ctx.end.IsZero() {
			a.End = ctx.end.UTC()
		}
		return a, nil
	}
	// No modifier — anchor the LWR window to the surrounding query's
	// end time when threaded through LowerAt. Otherwise leave the
	// anchor zero so the SQL renders `now64(9)`.
	if !ctx.end.IsZero() {
		return evalAnchor{End: ctx.end.UTC()}, nil
	}
	return evalAnchor{}, nil
}

// stalenessLowerBoundExpr renders the strict-lower-bound half of the
// LWR window:  `<col> > (<anchor> - <lookback>)`. Combined with the
// non-strict upper bound `<col> <= <anchor>` (from timeBoundExpr), the
// pair matches Prometheus's `Timestamp <= T AND T - Timestamp <
// lookback` rule.
func stalenessLowerBoundExpr(col string, a evalAnchor, lookback time.Duration) chplan.Expr {
	anchor := anchorBaseExpr(a)
	offsetNs := lookback.Nanoseconds() + a.Offset.Nanoseconds()
	right := &chplan.Binary{
		Op:   chplan.OpSub,
		Left: anchor,
		Right: &chplan.FuncCall{
			Name: "toIntervalNanosecond",
			Args: []chplan.Expr{&chplan.LitInt{V: offsetNs}},
		},
	}
	return &chplan.Binary{
		Op:    chplan.OpGt,
		Left:  &chplan.ColumnRef{Name: col},
		Right: right,
	}
}

// metricNameFromMatchers returns the value of the __name__ matcher (if any
// exists with MatchType == Equal); empty string otherwise. Used to pick the
// CH table for VectorSelectors that name a specific metric.
func metricNameFromMatchers(ms []*labels.Matcher) string {
	for _, m := range ms {
		if m.Name == model.MetricNameLabel && m.Type == labels.MatchEqual {
			return m.Value
		}
	}
	return ""
}

// buildPredicate AND-folds the label matchers into a single chplan.Expr.
// __name__ goes against the MetricName column; everything else goes against
// `Attributes[<label>]` via MapAccess.
func buildPredicate(matchers []*labels.Matcher, s schema.Metrics) chplan.Expr {
	var out chplan.Expr
	for _, m := range matchers {
		cond := matcherToExpr(m, s)
		if out == nil {
			out = cond
			continue
		}
		out = &chplan.Binary{Op: chplan.OpAnd, Left: out, Right: cond}
	}
	return out
}

func matcherToExpr(m *labels.Matcher, s schema.Metrics) chplan.Expr {
	var lhs chplan.Expr
	if m.Name == model.MetricNameLabel {
		lhs = &chplan.ColumnRef{Name: s.MetricNameColumn}
	} else {
		lhs = &chplan.MapAccess{
			Map: &chplan.ColumnRef{Name: s.AttributesColumn},
			Key: &chplan.LitString{V: m.Name},
		}
	}
	return &chplan.Binary{
		Op:    matchOp(m.Type),
		Left:  lhs,
		Right: &chplan.LitString{V: m.Value},
	}
}

func matchOp(t labels.MatchType) chplan.BinaryOp {
	switch t {
	case labels.MatchEqual:
		return chplan.OpEq
	case labels.MatchNotEqual:
		return chplan.OpNe
	case labels.MatchRegexp:
		return chplan.OpMatch
	case labels.MatchNotRegexp:
		return chplan.OpNotMatch
	}
	// Any new labels.MatchType added upstream would land here as Equal —
	// safer than panicking, and we'd notice via the spec tests.
	return chplan.OpEq
}

// lowerCall dispatches PromQL function calls. The arg shape decides the
// path: a MatrixSelector means a range-vector function (rate, increase,
// *_over_time); the clamp family takes a vector + scalar bounds; everything
// else is treated as an instant-vector math function (abs, sqrt, ln, ...)
// if recognised. Other functions surface a clear "not yet supported"
// error pointing at the relevant milestone.
func lowerCall(c *parser.Call, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	// `quantile_over_time(phi, v[range])` takes a scalar first; the
	// range-vector lives at c.Args[1]. Route it before the generic
	// "is c.Args[0] a MatrixSelector?" check below.
	if c.Func.Name == "quantile_over_time" {
		return lowerQuantileOverTime(c, s, ctx)
	}
	if len(c.Args) >= 1 {
		if _, ok := c.Args[0].(*parser.MatrixSelector); ok {
			return lowerRangeVectorCall(c, s, ctx)
		}
		if sq, ok := c.Args[0].(*parser.SubqueryExpr); ok {
			// `<range-vector-fn>(<subquery>)` — the canonical Grafana
			// shape `max_over_time(rate(m[5m])[1h:5m])`. Lowers to a
			// chained RangeWindow: outer reducer over the inner matrix.
			return lowerOuterRangeFnOverSubquery(c, sq, s, ctx)
		}
	}
	switch c.Func.Name {
	case "absent":
		return lowerAbsent(c, s, ctx)
	case "clamp", "clamp_min", "clamp_max":
		return lowerClamp(c, s, ctx)
	case "histogram_quantile":
		return lowerHistogramQuantile(c, s, ctx)
	case "label_replace":
		return lowerLabelReplace(c, s, ctx)
	case "label_join":
		return lowerLabelJoin(c, s, ctx)
	case "time":
		return lowerTime(c, s, ctx)
	case "vector":
		return lowerVector(c, s, ctx)
	case "year", "month", "day_of_month", "day_of_week",
		"days_in_month", "hour", "minute", "timestamp":
		return lowerDateFn(c, s, ctx)
	}
	if chFn, ok := instantFnCH[c.Func.Name]; ok {
		return lowerInstantFn(c, s, chFn, ctx)
	}
	return nil, fmt.Errorf("promql: function %s is not yet supported", c.Func.Name)
}

// lowerRangeVectorCall handles range-vector functions: rate, increase,
// delta, and the `*_over_time` family. The single argument is a
// MatrixSelector wrapping a VectorSelector; we lower the VectorSelector
// and wrap the result in a RangeWindow capturing the function name +
// range duration.
func lowerRangeVectorCall(c *parser.Call, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	switch c.Func.Name {
	case "predict_linear":
		return lowerPredictLinear(c, s, ctx)
	case "holt_winters", "double_exponential_smoothing":
		return lowerHoltWinters(c, s, ctx)
	case "absent_over_time":
		return lowerAbsentOverTime(c, s, ctx)
	}
	if len(c.Args) != 1 {
		return nil, fmt.Errorf("promql: %s expects exactly 1 argument, got %d", c.Func.Name, len(c.Args))
	}
	ms, ok := c.Args[0].(*parser.MatrixSelector)
	if !ok {
		return nil, fmt.Errorf("promql: %s argument must be a range-vector selector, got %T",
			c.Func.Name, c.Args[0])
	}
	vs, ok := ms.VectorSelector.(*parser.VectorSelector)
	if !ok {
		return nil, fmt.Errorf("promql: matrix selector's inner must be a VectorSelector, got %T",
			ms.VectorSelector)
	}

	anchor, err := anchorFromSelector(vs, ctx)
	if err != nil {
		return nil, err
	}

	// The RangeWindow already encodes the window's eval anchor; emitting a
	// duplicate time-bound predicate on the inner Filter would double-count.
	// Build the inner Scan/Filter without the modifier-derived bound here.
	// The inRangeVector flag also suppresses the bare-selector LWR wrap so
	// every in-window sample reaches the RangeWindow node.
	vsNoModifier := *vs
	vsNoModifier.Timestamp = nil
	vsNoModifier.OriginalOffset = 0
	vsNoModifier.Offset = 0
	vsNoModifier.StartOrEnd = 0
	rangeCtx := ctx
	rangeCtx.inRangeVector = true
	inner, err := lowerVectorSelector(&vsNoModifier, s, rangeCtx)
	if err != nil {
		return nil, err
	}
	rw := &chplan.RangeWindow{
		Input:           inner,
		Func:            c.Func.Name,
		Range:           ms.Range,
		End:             anchor.End,
		Offset:          anchor.Offset,
		TimestampColumn: s.TimestampColumn,
		ValueColumn:     s.ValueColumn,
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}},
	}
	// In range mode, fan the range function across the request's step
	// grid: each anchor in [start, end] (spaced by step) emits one row
	// per series with the per-anchor function value. The emitter
	// already supports this via OuterRange + Step (the matrix path used
	// by subqueries); we just need to flip the switch when LowerAtRange
	// threaded a non-zero step. Without this, `rate(m[5m])` over
	// query_range degenerates to a single anchor at end_ts and the
	// matrix pivot only sees one sample per series — the same root
	// cause as the bare-selector range-mode bug Pool-AK is fixing.
	if ctx.step > 0 && !ctx.start.IsZero() && !ctx.end.IsZero() {
		rw.Start = ctx.start.UTC()
		rw.End = ctx.end.UTC()
		rw.Step = ctx.step
		rw.OuterRange = ctx.end.Sub(ctx.start)
	}
	return rw, nil
}

// lowerAggregate handles `sum by (job) (...)`, `sum without (instance) (...)`,
// `count(...)`, `stddev(...)`, `stdvar(...)`, `group(...)`, and
// `quantile(0.95, ...)`. The shape-changing aggregates `topk`/`bottomk` are
// handled separately via lowerTopK — they produce K rows per partition
// rather than one, so they map to a TopK plan node (CH's `LIMIT K BY`)
// instead of the regular Aggregate. `count_values` remains deferred.
//
// The Aggregate is wrapped with a Project that re-shapes its output into
// the Sample contract (MetricName, Attributes, TimeUnix, Value) so the
// API layer can stream rows through `chclient.Sample` directly. PromQL
// aggregations drop `__name__`, so the projected MetricName is the empty
// string; the projected Attributes is built from the group-key columns.
func lowerAggregate(a *parser.AggregateExpr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if a.Op == parser.TOPK || a.Op == parser.BOTTOMK {
		return lowerTopK(a, s, ctx)
	}
	if a.Op == parser.COUNT_VALUES {
		return lowerCountValues(a, s, ctx)
	}

	input, err := lower(a.Expr, s, ctx)
	if err != nil {
		return nil, err
	}

	groupBy, err := aggregateGroupBy(a, s)
	if err != nil {
		return nil, err
	}

	aggFunc, err := buildAggFunc(a, s)
	if err != nil {
		return nil, err
	}

	aliases := groupKeyAliases(len(groupBy))
	// In range mode the input plan exposes a per-step TimeUnix
	// (anchor_ts re-aliased by wrapRangeLatestPerSeries). Aggregations
	// must group by the per-step bucket in addition to the user's
	// `by/without` keys — otherwise CH would collapse N anchors into one
	// row per series-set. Inject TimeUnix as an extra group key with a
	// stable alias (`bucket_ts`) the wrap can reference.
	const bucketAlias = "bucket_ts"
	rangeBucketed := ctx.step > 0
	if rangeBucketed {
		groupBy = append(groupBy, &chplan.ColumnRef{Name: s.TimestampColumn})
		aliases = append(aliases, bucketAlias)
	}
	agg := &chplan.Aggregate{
		Input:              input,
		GroupBy:            groupBy,
		GroupByAliases:     aliases,
		AggFuncs:           []chplan.AggFunc{aggFunc},
		DropEmptyOnNoGroup: true,
	}
	// The wrap re-projects the bucket alias onto TimeUnix so range-mode
	// aggregations expose per-step rows on the canonical column shape.
	userAliases := aliases
	if rangeBucketed {
		userAliases = aliases[:len(aliases)-1]
	}
	wrapped := wrapAggregateForSample(agg, a, s, userAliases, rangeBucketed, bucketAlias)
	// quantile(phi, V) with phi outside [0, 1] is well-defined in
	// PromQL — see prometheus/promql/quantile.go: phi<0 → -Inf,
	// phi>1 → +Inf. CH's `quantile` aggregate rejects out-of-range
	// phi at the wire layer, so buildAggFunc has already clamped
	// the emitted phi to 0.5; here we wrap the Aggregate output in
	// a Project that overrides Value with the PromQL-spec Inf
	// constant. The per-group identity (MetricName / Attributes /
	// TimeUnix) carries through unchanged from the inner Project.
	if a.Op == parser.QUANTILE {
		if phi, ok := tryScalarLiteral(a.Param); ok {
			if infValue, outOfRange := outOfRangePhiInf(phi); outOfRange {
				wrapped = projectValueOverInner(wrapped, s, &chplan.LitFloat{V: infValue})
			}
		}
	}
	return wrapped, nil
}

// lowerCountValues lowers `count_values("label", expr) [by(g)]`. The
// shape is: for each distinct value of `expr` (within each `by` group),
// emit a row whose Attributes carry the unique value as a synthetic
// label binding (`<label>=<stringified value>`) plus any `by`-grouped
// labels, and whose Value is the count of input series that hit that
// value.
//
// SQL shape (no `by`):
//
//	SELECT '' AS MetricName,
//	       CAST(map('<label>', toString(Value)), 'Map(String,String)') AS Attributes,
//	       now64(9) AS TimeUnix,
//	       count() AS Value
//	FROM (<inner>)
//	GROUP BY toString(Value)
//
// SQL shape (with `by(g)`):
//
//	SELECT '' AS MetricName,
//	       mapWithoutEmpty(map('g', gkey_0, '<label>', toString(Value))) AS Attributes,
//	       now64(9) AS TimeUnix,
//	       count() AS Value
//	FROM (<inner>)
//	GROUP BY Attributes['g'], toString(Value)
//
// `without(...)` follows the same path as the other aggregations for
// compositionality, but `count_values` semantics with `without` are
// rarely used in practice; we reject it for now to keep the surface
// small.
func lowerCountValues(a *parser.AggregateExpr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if a.Without {
		return nil, fmt.Errorf("promql: count_values without(...) is not yet supported")
	}
	label, ok := tryStringLiteral(a.Param)
	if !ok {
		return nil, fmt.Errorf("promql: count_values requires a string-literal label name as the first arg")
	}
	if label == "" {
		return nil, fmt.Errorf("promql: count_values requires a non-empty label name")
	}

	input, err := lower(a.Expr, s, ctx)
	if err != nil {
		return nil, err
	}

	// Group keys: the `by(...)` labels (as Attributes map accesses) plus
	// the value-as-label key (toString(Value)). The value-as-label key
	// gets its own alias so the wrapping Project can reference it.
	groupBy := make([]chplan.Expr, 0, len(a.Grouping)+1)
	aliases := make([]string, 0, len(a.Grouping)+1)
	for i, lbl := range a.Grouping {
		groupBy = append(groupBy, &chplan.MapAccess{
			Map: &chplan.ColumnRef{Name: s.AttributesColumn},
			Key: &chplan.LitString{V: lbl},
		})
		aliases = append(aliases, fmt.Sprintf("gkey_%d", i))
	}
	const (
		valueKeyAlias = "cv_val"
		countAlias    = "cv_count"
	)
	groupBy = append(groupBy, &chplan.FuncCall{
		Name: "toString",
		Args: []chplan.Expr{&chplan.ColumnRef{Name: s.ValueColumn}},
	})
	aliases = append(aliases, valueKeyAlias)

	agg := &chplan.Aggregate{
		Input:          input,
		GroupBy:        groupBy,
		GroupByAliases: aliases,
		// Alias count() as cv_count (not Value) so CH's name resolution
		// in the GROUP BY clause doesn't pick up the aggregate alias
		// when it sees `toString(Value)` — CH otherwise errors with
		// `Aggregate function count() AS Value is found in GROUP BY`.
		// The outer Project re-aliases cv_count back to Value.
		AggFuncs: []chplan.AggFunc{
			{Name: "count", Args: []chplan.Expr{}, Alias: countAlias},
		},
		// count_values returns one row per distinct value; empty input
		// produces no rows naturally because there's nothing to group
		// over. The count() guard isn't needed (and would be wrong —
		// it would suppress the zero-distinct-values case).
		DropEmptyOnNoGroup: false,
	}

	// Build the Attributes map for the wrapping Project. Each `by`
	// label binds its gkey_<i> column; the synthetic label binds
	// cv_val.
	mapArgs := make([]chplan.Expr, 0, (len(a.Grouping)+1)*2)
	for i, lbl := range a.Grouping {
		mapArgs = append(mapArgs,
			&chplan.LitString{V: lbl},
			&chplan.ColumnRef{Name: aliases[i]},
		)
	}
	mapArgs = append(mapArgs,
		&chplan.LitString{V: label},
		&chplan.ColumnRef{Name: valueKeyAlias},
	)
	attrs := &chplan.MapWithoutEmptyValues{
		Map: &chplan.FuncCall{Name: "map", Args: mapArgs},
	}

	return &chplan.Project{
		Input: agg,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			{Expr: attrs, Alias: s.AttributesColumn},
			{Expr: &chplan.FuncCall{Name: "now64", Args: []chplan.Expr{&chplan.LitInt{V: 9}}}, Alias: s.TimestampColumn},
			{Expr: &chplan.ColumnRef{Name: countAlias}, Alias: s.ValueColumn},
		},
	}, nil
}

// tryStringLiteral returns the value of a *parser.StringLiteral, peeling
// off ParenExpr wrappers. Returns ("", false) if e isn't a string
// literal.
func tryStringLiteral(e parser.Expr) (string, bool) {
	switch v := e.(type) {
	case *parser.StringLiteral:
		return v.Val, true
	case *parser.ParenExpr:
		return tryStringLiteral(v.Expr)
	}
	return "", false
}

// lowerTopK lowers `topk(K, expr) [by(g) | without(g)] (...)` and
// `bottomk(K, expr) ...` into a chplan.TopK over the lowered inner
// expression. Unlike a regular aggregation, topk/bottomk preserve
// every input label — `by(...)` only partitions; the result vector
// keeps all the original labels of the surviving series.
//
// SQL shape:
//
//	SELECT MetricName, Attributes, TimeUnix, Value FROM (<inner>)
//	  ORDER BY Value [DESC|ASC] LIMIT K [BY <partition_exprs>]
//
// K must be a non-negative integer scalar literal at lowering time.
// PromQL also accepts a `0` K (returns no series); we treat K=0 as
// an error since the SQL `LIMIT 0` shape is degenerate and the
// fixture-driven flow keeps a positive K invariant on the plan tree.
//
// `without (l1, l2, ...)` partitions by "every label except <these>".
// We emit a single `MapWithoutKeys` Expr into the `By` slot — it lowers
// to CH's `mapFilter((k, v) -> NOT (k IN (?,...)), Attributes)`, which
// is exactly the per-series partition key we want for LIMIT K BY. The
// degenerate `without ()` case (empty Grouping) means "remove nothing",
// equivalent to partitioning by the full Attributes map; emit a bare
// ColumnRef so we don't render an empty IN-list (CH rejects that).
//
// Range mode (ctx.step > 0): PromQL's topk/bottomk selects K series
// **per evaluation step**, not K across the whole time range. The inner
// plan (`wrapRangeLatestPerSeries`) re-aliases the per-step anchor onto
// `TimeUnix`, so by appending TimeUnix to the partition list the
// emitter's `LIMIT K BY (<user-partition>, TimeUnix)` selects K rows
// per anchor — matching Prom's per-step semantics. Without this thread,
// `LIMIT K BY <user-partition>` collapses every (series, step) pair
// into a single K-row global window and the matrix pivot loses every
// step beyond the K-th overall.
func lowerTopK(a *parser.AggregateExpr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	kF, ok := tryScalarLiteral(a.Param)
	if !ok {
		return nil, fmt.Errorf("promql: %s requires a scalar literal K (computed K is not yet supported)", a.Op.String())
	}
	if kF < 0 || kF != float64(int64(kF)) {
		return nil, fmt.Errorf("promql: %s K must be a non-negative integer literal, got %v", a.Op.String(), kF)
	}
	if kF == 0 {
		// PromQL semantics: topk(0, v) returns an empty result. CH's
		// LIMIT 0 is degenerate and downstream invariants assume
		// positive K; reject so callers see a clear error rather
		// than a silent empty.
		return nil, fmt.Errorf("promql: %s K must be > 0", a.Op.String())
	}

	input, err := lower(a.Expr, s, ctx)
	if err != nil {
		return nil, err
	}

	var by []chplan.Expr
	switch {
	case a.Without && len(a.Grouping) == 0:
		// `topk(K, v) without ()` — partition by the full Attributes map.
		by = []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}}
	case a.Without:
		// `topk(K, v) without (l1, l2)` — partition by `Attributes` with
		// the listed labels stripped via mapFilter. Single MapWithoutKeys
		// Expr keeps the per-series partition shape symmetric with the
		// non-shape-changing aggregation path (`aggregateGroupBy`).
		by = []chplan.Expr{&chplan.MapWithoutKeys{
			Map:  &chplan.ColumnRef{Name: s.AttributesColumn},
			Keys: append([]string(nil), a.Grouping...),
		}}
	default:
		by = make([]chplan.Expr, 0, len(a.Grouping))
		for _, label := range a.Grouping {
			by = append(by, &chplan.MapAccess{
				Map: &chplan.ColumnRef{Name: s.AttributesColumn},
				Key: &chplan.LitString{V: label},
			})
		}
	}

	// Range mode: thread the per-step anchor (TimeUnix re-aliased from
	// anchor_ts by the inner wrapRangeLatestPerSeries) into the partition
	// list so `LIMIT K BY (..., TimeUnix)` fires per anchor. The instant
	// path (ctx.step == 0) keeps the original partition shape so the
	// existing instant-mode fixtures stay byte-stable.
	if ctx.step > 0 {
		by = append(by, &chplan.ColumnRef{Name: s.TimestampColumn})
	}

	return &chplan.TopK{
		Input:    input,
		K:        int64(kF),
		By:       by,
		SortExpr: &chplan.ColumnRef{Name: s.ValueColumn},
		Desc:     a.Op == parser.TOPK,
		Columns: []string{
			s.MetricNameColumn,
			s.AttributesColumn,
			s.TimestampColumn,
			s.ValueColumn,
		},
	}, nil
}

// groupKeyAliases returns ["gkey_0", "gkey_1", ...] of length n. Empty
// slice for n=0 so unaggregated aggregates (`count(up)` with no `by/
// without`) still skip the aliasing path.
func groupKeyAliases(n int) []string {
	if n == 0 {
		return nil
	}
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("gkey_%d", i)
	}
	return out
}

// wrapAggregateForSample produces the Sample-shape Project on top of an
// Aggregate so downstream `chclient.Sample` decoding works for any
// PromQL aggregation.
//
//	MetricName  = ''                          (aggregations drop __name__)
//	Attributes  = map('lbl0', gkey_0, ...)    for `by (lbl0, lbl1, ...)`
//	            | gkey_0                       for `without (...)` (mapFilter output)
//	            | empty Map(String,String)     for unaggregated forms
//	TimeUnix    = now64(9)                    (instant mode — eval time)
//	            | <bucketAlias>                (range mode — per-step anchor)
//	Value       = <aggFunc alias>             (sum / avg / quantile / ...)
//
// rangeBucketed reflects whether the underlying Aggregate carries an
// extra TimeUnix group key (range mode); when true the projection's
// TimeUnix slot references the bucket alias the Aggregate exposed so
// per-step aggregation rows propagate onto the canonical column shape.
func wrapAggregateForSample(agg *chplan.Aggregate, a *parser.AggregateExpr, s schema.Metrics, aliases []string, rangeBucketed bool, bucketAlias string) chplan.Node {
	var attrs chplan.Expr
	switch {
	case len(aliases) == 0:
		// No grouping — emit an empty Map(String, String).
		attrs = emptyAttrsMap()
	case a.Without:
		// Single mapFilter-derived attribute column; the gkey IS the map.
		attrs = &chplan.ColumnRef{Name: aliases[0]}
	default:
		// `sum by (job, instance) (...)` over series whose `job` label is
		// absent produces a gkey with the CH-Map default empty string
		// (`Attributes['job']` returns `''` when the key is missing).
		// PromQL's canonical Labels representation drops empty-valued
		// labels, so wrap the map() literal with MapWithoutEmptyValues
		// to strip empty-valued entries before the wire layer renders
		// them. Series with an explicit `""` label value canonicalise
		// the same way upstream, so this is lossless for real-world
		// inputs.
		args := make([]chplan.Expr, 0, len(a.Grouping)*2)
		for i, label := range a.Grouping {
			args = append(args, &chplan.LitString{V: label}, &chplan.ColumnRef{Name: aliases[i]})
		}
		attrs = &chplan.MapWithoutEmptyValues{
			Map: &chplan.FuncCall{Name: "map", Args: args},
		}
	}

	tsExpr := chplan.Expr(&chplan.FuncCall{Name: "now64", Args: []chplan.Expr{&chplan.LitInt{V: 9}}})
	if rangeBucketed {
		tsExpr = &chplan.ColumnRef{Name: bucketAlias}
	}

	return &chplan.Project{
		Input: agg,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			{Expr: attrs, Alias: s.AttributesColumn},
			{Expr: tsExpr, Alias: s.TimestampColumn},
			{Expr: &chplan.ColumnRef{Name: s.ValueColumn}, Alias: s.ValueColumn},
		},
	}
}

// emptyAttrsMap returns a CH expression for an empty Map(String,String),
// used when an aggregation drops all labels (e.g. `count(up)` with no
// `by/without` clause).
func emptyAttrsMap() chplan.Expr {
	return &chplan.FuncCall{
		Name: "CAST",
		Args: []chplan.Expr{
			&chplan.FuncCall{Name: "map", Args: nil},
			&chplan.LitString{V: "Map(String,String)"},
		},
	}
}

// aggregateGroupBy builds the group-key list for an aggregation. For
// `by (...)` it returns one MapAccess per named label; for `without (...)`
// it returns a single MapWithoutKeys spanning the full Attributes map with
// the named labels stripped.
//
// `without ()` (empty Grouping list) is the degenerate "remove nothing"
// shape — equivalent to grouping by the full Attributes map. Emitting a
// MapWithoutKeys{Keys: []} would lower to `mapFilter((k, v) -> NOT (k
// IN ()), Attributes)`, which CH rejects as a syntax error (empty IN
// list). Short-circuit to a bare ColumnRef so the GroupBy slot
// references `Attributes` directly. Semantics match Prometheus's
// `aggregators.test` "Empty without" case: one output row per unique
// input label set, with all labels preserved (aggregation drops only
// `__name__`, which the OTel-CH Attributes map never contains).
func aggregateGroupBy(a *parser.AggregateExpr, s schema.Metrics) ([]chplan.Expr, error) {
	if a.Without {
		if len(a.Grouping) == 0 {
			return []chplan.Expr{
				&chplan.ColumnRef{Name: s.AttributesColumn},
			}, nil
		}
		return []chplan.Expr{
			&chplan.MapWithoutKeys{
				Map:  &chplan.ColumnRef{Name: s.AttributesColumn},
				Keys: append([]string(nil), a.Grouping...),
			},
		}, nil
	}
	out := make([]chplan.Expr, 0, len(a.Grouping))
	for _, label := range a.Grouping {
		out = append(out, &chplan.MapAccess{
			Map: &chplan.ColumnRef{Name: s.AttributesColumn},
			Key: &chplan.LitString{V: label},
		})
	}
	return out, nil
}

// buildAggFunc produces the single AggFunc for an aggregation. The output-
// shape-changing aggregates `topk`/`bottomk` are handled out-of-band via
// lowerTopK before this function is called. `count_values` (the remaining
// shape-changer) is still rejected here.
func buildAggFunc(a *parser.AggregateExpr, s schema.Metrics) (chplan.AggFunc, error) {
	valueArg := &chplan.ColumnRef{Name: s.ValueColumn}

	switch a.Op {
	case parser.SUM, parser.COUNT, parser.AVG, parser.MIN, parser.MAX, parser.STDDEV, parser.STDVAR:
		if a.Param != nil {
			return chplan.AggFunc{}, fmt.Errorf("promql: aggregation %s does not take a parameter", a.Op.String())
		}
		name, err := plainAggCH(a.Op)
		if err != nil {
			return chplan.AggFunc{}, err
		}
		return chplan.AggFunc{
			Name:  name,
			Args:  []chplan.Expr{valueArg},
			Alias: s.ValueColumn,
		}, nil

	case parser.GROUP:
		// PromQL `group(...)` returns 1 for every label combination; emit
		// `any(1)` which yields a constant 1 per CH group.
		if a.Param != nil {
			return chplan.AggFunc{}, fmt.Errorf("promql: group() does not take a parameter")
		}
		return chplan.AggFunc{
			Name:  "any",
			Args:  []chplan.Expr{&chplan.LitInt{V: 1}},
			Alias: s.ValueColumn,
		}, nil

	case parser.QUANTILE:
		phi, ok := tryScalarLiteral(a.Param)
		if !ok {
			return chplan.AggFunc{}, fmt.Errorf("promql: quantile(phi, ...) requires a scalar literal phi (computed phi defers to M1.7)")
		}
		// CH's `quantile(phi)` aggregate errors on phi outside
		// [0, 1]; clamp the emitted phi to a safe sentinel (0.5)
		// for those cases. lowerAggregate post-Projects the Value
		// column to ±Inf (matching Prom's funcQuantile semantics)
		// so the clamped value is never observed.
		emitPhi := phi
		if _, outOfRange := outOfRangePhiInf(phi); outOfRange {
			emitPhi = 0.5
		}
		return chplan.AggFunc{
			Name:   "quantile",
			Params: []chplan.Expr{&chplan.LitFloat{V: emitPhi}},
			Args:   []chplan.Expr{valueArg},
			Alias:  s.ValueColumn,
		}, nil

	case parser.COUNT_VALUES:
		return chplan.AggFunc{}, fmt.Errorf("promql: %s changes output shape and lands with M1.7 result shaping", a.Op.String())
	}

	return chplan.AggFunc{}, fmt.Errorf("promql: aggregation op %s is not yet supported", a.Op.String())
}

// plainAggCH maps a non-parameterised PromQL aggregator to its CH name.
func plainAggCH(op parser.ItemType) (string, error) {
	switch op {
	case parser.SUM:
		return "sum", nil
	case parser.COUNT:
		return "count", nil
	case parser.AVG:
		return "avg", nil
	case parser.MIN:
		return "min", nil
	case parser.MAX:
		return "max", nil
	case parser.STDDEV:
		return "stddevPop", nil
	case parser.STDVAR:
		return "varPop", nil
	}
	return "", fmt.Errorf("promql: aggregation op %s is not yet supported", op.String())
}
