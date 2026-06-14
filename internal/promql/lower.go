package promql

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"github.com/tsouza/cerberus/internal/api/format"
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
// SubqueryExpr (bare-vector, over range-vector calls, outer reducer
// over subquery). Nested subqueries reachable through the parser (e.g.
// `max_over_time(rate(m[1m])[5m:30s])[1h:5m]`,
// `sum_over_time(max_over_time(rate(m[5m])[10m:1m])[1h:5m])`) lower
// via the Call / ParenExpr / AggregateExpr intermediaries the parser
// requires between two `SubqueryExpr` nodes; direct
// `SubqueryExpr.Expr = *SubqueryExpr` is parser-impossible but
// `lowerSubqueryOverSubquery` handles it defensively.
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
	return LowerAtRangeOpts(ctx, expr, s, start, end, step, LowerOpts{})
}

// LowerOpts carries optional, per-request lowering knobs that are
// off-by-default. A zero LowerOpts reproduces [LowerAtRange]'s behaviour
// byte-for-byte, so every caller that doesn't opt in stays on the
// established lowering paths.
type LowerOpts struct {
	// ExperimentalTSGridRange opts eligible `rate(<counter>[<range>])`
	// query_range expressions into the ClickHouse-native
	// timeSeriesRateToGrid lowering (a chplan.RangeWindowNative node)
	// instead of the default arrayJoin fan-out. Threaded from
	// Config.ExperimentalTSGridRange. Default false.
	ExperimentalTSGridRange bool
}

// LowerAtRangeOpts is the options-carrying variant of [LowerAtRange].
// The query_range handler adapters pass a populated LowerOpts so the
// experimental native-rate path can be enabled per deployment; every
// other caller uses [Lower] / [LowerAt] / [LowerAtRange] and gets the
// zero-options (default) behaviour.
func LowerAtRangeOpts(ctx context.Context, expr parser.Expr, s schema.Metrics, start, end time.Time, step time.Duration, opts LowerOpts) (chplan.Node, error) {
	_, span := tracer.Start(ctx, cerbtrace.SpanLower, trace.WithAttributes(cerbtrace.AttrQL.String("promql")))
	defer span.End()
	plan, err := lower(expr, s, lowerCtx{
		start:                   start,
		end:                     end,
		step:                    step,
		experimentalTSGridRange: opts.ExperimentalTSGridRange,
	})
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
	case *parser.MatrixSelector:
		return lowerMatrixSelector(e, s, ctx)
	case *parser.UnaryExpr:
		return lowerUnary(e, s, ctx)
	default:
		return nil, fmt.Errorf("promql: unsupported expression %T", expr)
	}
}

// lowerMatrixSelector handles a TOP-LEVEL range-vector selector —
// `up[5m]` sent to /api/v1/query. Reference Prometheus answers these
// with resultType "matrix": every RAW sample in `(eval − range, eval]`
// per series, original timestamps preserved (no per-step alignment, no
// staleness lookback). The lowering is therefore the bare selector
// path with the LWR collapse suppressed plus the window bound — the
// canonical 4-column row shape carries the per-sample timestamps the
// handler's instant-matrix pivot groups on.
//
// MatrixSelector in ARGUMENT position (`rate(up[5m])`) never reaches
// here — lowerCall routes it into the range-vector machinery first.
// On /api/v1/query_range the handler rejects matrix-typed expressions
// before lowering (mirroring upstream's "invalid expression type"
// guard), so this path is instant-only by construction.
func lowerMatrixSelector(ms *parser.MatrixSelector, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	vs, ok := ms.VectorSelector.(*parser.VectorSelector)
	if !ok {
		return nil, fmt.Errorf("promql: matrix selector's inner must be a VectorSelector, got %T", ms.VectorSelector)
	}
	anchor, err := anchorFromSelector(vs, ctx)
	if err != nil {
		return nil, err
	}
	if anchor.End.IsZero() && !ctx.end.IsZero() {
		anchor.End = ctx.end.UTC()
	}

	// Strip the modifier — the window bound below carries the anchor;
	// inRangeVector suppresses the LWR wrap so every in-window sample
	// survives.
	vsNoMod := *vs
	vsNoMod.Timestamp = nil
	vsNoMod.OriginalOffset = 0
	vsNoMod.Offset = 0
	vsNoMod.StartOrEnd = 0
	rangeCtx := ctx
	rangeCtx.inRangeVector = true
	inner, err := lowerVectorSelector(&vsNoMod, s, rangeCtx)
	if err != nil {
		return nil, err
	}

	// (anchor − range, anchor] window — left-open / right-closed, the
	// PromQL range-selector contract.
	pred := &chplan.Binary{
		Op:    chplan.OpAnd,
		Left:  timeBoundExpr(s.TimestampColumn, anchor),
		Right: stalenessLowerBoundExpr(s.TimestampColumn, anchor, ms.Range),
	}
	// Project the canonical 4-column Sample shape explicitly (the bare
	// Filter-over-Scan would emit `SELECT *`, dragging every physical
	// table column onto the wire). Matrix selectors PRESERVE
	// `__name__` — the samples are raw, not derived.
	return &chplan.Project{
		Input: &chplan.Filter{Input: inner, Predicate: pred},
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: s.MetricNameColumn}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}, Alias: s.TimestampColumn},
			{Expr: &chplan.ColumnRef{Name: s.ValueColumn}, Alias: s.ValueColumn},
		},
	}, nil
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
	// Resolve the candidate physical tables for this matcher.
	//
	// `TablesFor` admits the OTel-emitter reality that cerberus's
	// original suffix heuristic (`TableFor`) missed: hostmetrics /
	// sqlquery / prometheus-self ship cumulative sums under bare names
	// (`system_cpu_time`, `clickhouse_event`, `otelcol_process_uptime`)
	// that the Prom convention reserves for gauges. Returning the
	// (Gauge, Sum) pair for unsuffixed names lets the scan resolve
	// against either physical layout — the MetricName PREWHERE makes
	// the empty arm cost-free.
	//
	// Suffixed names (`_total` / `_count` / `_sum` / `_bucket`) still
	// route to a single table via TableFor; histogram-companion +
	// bucket selectors below override to the histogram table without
	// touching the union path. Existing fixtures stay byte-stable.
	//
	// When the selector carries no MatchEqual `__name__` (a regex name
	// matcher, a negated matcher, or no name matcher at all) the scan
	// fans across the same (Gauge, Sum) pair the unsuffixed arm uses —
	// see schema.Metrics.TablesForUnknownName. A gauge-only fallback
	// here made `{__name__=~".*cerberus_query_inflight.*"}` (the exact
	// shape Grafana's Metrics Drilldown breakdown tab sends) return
	// empty for every sum-stored metric.
	tables := s.TablesForUnknownName()
	if metricName != "" {
		tables = s.TablesFor(metricName)
	}

	// Classic-histogram companion routing: `<base>_count` / `<base>_sum`
	// are Prom-convention companion names whose data lives, in the OTel-CH
	// layout, as `Count` / `Sum` columns on rows written under the bare
	// `<base>` name in the histogram table. Reroute the scan + strip the
	// suffix off the `__name__` matcher + alias the column as `Value` so
	// the downstream Sample-row contract holds. Mirrors stripBucketSuffix
	// (PR #637) for the `_bucket` companion, and the exemplars handler's
	// routing in internal/api/prom/exemplars.go::exemplarsTableFor.
	//
	// `<base>_bucket` takes a parallel-but-distinct path: the OTel-CH
	// histogram row stores per-bucket counts as the `BucketCounts` array
	// with `ExplicitBounds` carrying the bucket edges. Prom exposes the
	// same data as N+1 separate series under `<base>_bucket{le=<bound>}`,
	// so the bare-selector lowering fans the array into N+1 Sample-shape
	// rows via arrayJoin. See wrapHistogramBucketFanout for the plan
	// shape. The bucket suffix is detected via isClassicBucketSelector;
	// the matcher-strip + `le` matcher split happens in splitBucketMatchers.
	matchers := v.LabelMatchers
	var companionValueColumn string
	var bucketSuffixed string
	var bucketLeMatchers []*labels.Matcher
	var companionSuffixed string
	var companionBare string
	if bareBucket, ok := isClassicBucketSelector(metricName, s); ok {
		tables = []string{s.HistogramTable}
		bucketSuffixed = metricName
		var scanMatchers []*labels.Matcher
		scanMatchers, bucketLeMatchers = splitBucketMatchers(matchers, bareBucket)
		matchers = scanMatchers
	} else if bare, col, ok := s.HistogramCompanionColumn(metricName); ok && s.HistogramTable != "" {
		// `<base>_count` / `<base>_sum` — a classic-histogram companion
		// suffix. Two physical layouts may carry the matching rows:
		//
		//   1. The OTel-CH histogram exporter writes Count/Sum as
		//      columns on a single row keyed by the BARE `<base>` name
		//      in the histogram table.
		//   2. The OTel-hostmetrics / sqlquery emitters write the
		//      suffixed name (`system_cpu_logical_count`,
		//      `system_processes_count`, `system_filesystem_inodes_count`,
		//      `system_processes_created_count`, …) as a cumulative Sum
		//      under the suffixed name in the sum table.
		//
		// When Sum is configured and distinct from Histogram, fan the
		// scan across both layouts via a UnionAll of per-arm Projects.
		// Each arm bakes its own MetricName filter so the union arms
		// hold disjoint row sets by construction. The non-MetricName
		// matchers (attribute / service equality / regex matchers) are
		// applied inside each arm so the optimizer's PREWHERE promotion
		// path still sees a Filter-over-Scan shape per arm.
		//
		// `companionValueColumn` / `companionSuffixed` / `companionBare`
		// drive the per-arm Project-shape decision below. When the union
		// path doesn't apply (no Sum table configured, or Sum equals
		// Histogram by config), fall back to the single-arm histogram
		// projection — same shape as before this multi-table fan-out.
		tables = []string{s.HistogramTable}
		companionValueColumn = col
		companionSuffixed = metricName
		companionBare = bare
		// The single-arm fallback rewrites matchers in-place to the bare
		// name so the legacy histogram-companion-only emit shape stays
		// byte-stable for deployments without a separate Sum table.
		if s.SumTable == "" || s.SumTable == s.HistogramTable {
			matchers = rewriteMetricName(matchers, bare)
		}
	}

	// Multi-arm companion union: when both histogram + sum tables are
	// in play for a `_count` / `_sum` selector, hand off to the
	// dedicated builder which assembles the per-arm Projects, stitches
	// them with chplan.UnionAll, and wraps the union with the right
	// LWR / range-vector shape for ctx.
	if needCompanionUnion(s, companionValueColumn, companionSuffixed, companionBare) {
		return lowerCompanionUnion(
			v, s, ctx, matchers,
			companionBare, companionSuffixed, companionValueColumn,
		)
	}

	scan := scanFromTables(tables)

	pred := buildPredicate(matchers, s)
	// Build the input subtree the LWR / range-vector pipeline consumes.
	// For the classic-histogram companion path we project the source
	// column (Count / Sum) as `Value` so downstream nodes still see the
	// canonical (MetricName, Attributes, TimeUnix, Value) shape.
	//
	// For the `_bucket` companion path the fan-out is more involved —
	// arrayJoin over BucketCounts × ExplicitBounds produces N+1 rows per
	// source row with the synthetic `le` label baked into Attributes.
	// Any user-supplied `le` matcher applies AFTER the fan-out as an
	// outer Filter on `Attributes['le']` (the column doesn't exist on
	// the raw scan row).
	var selectorInput chplan.Node = scan
	switch {
	case bucketSuffixed != "":
		// The scan-side filter (non-le matchers) feeds into the
		// fan-out Project, then the post-fanout filter applies any
		// `le=<bound>` matcher the user wrote against the synthesized
		// `Attributes['le']` key.
		var fanInput chplan.Node = scan
		if pred != nil {
			fanInput = &chplan.Filter{Input: scan, Predicate: pred}
		}
		selectorInput = wrapHistogramBucketFanout(fanInput, bucketSuffixed, s)
		if lePred := buildPredicate(bucketLeMatchers, s); lePred != nil {
			selectorInput = &chplan.Filter{Input: selectorInput, Predicate: lePred}
		}
		// `pred` is already baked into the fan-out's input Filter (or
		// nil when there were no scan-side matchers), so the LWR /
		// range-vector wrapper below must NOT re-apply it.
		pred = nil
	case companionValueColumn != "":
		selectorInput = wrapHistogramCompanionProject(scan, companionValueColumn, s)
	}

	// Resolve the effective evaluation anchor for this selector.
	// `@`/offset modifiers shadow the surrounding ctx; absent a
	// modifier we pick up ctx.end (the query's eval timestamp) so
	// the LWR predicate below has something to compare against.
	anchor, err := selectorAnchor(v, ctx)
	if err != nil {
		return nil, err
	}

	// When an enclosing vector aggregation's by-clause references a
	// label that routes to a dedicated top-level OTel-CH column
	// (currently only `service.name` / `service_name` → ServiceName),
	// inflate Attributes with one synthesised key per such column so
	// the downstream LWR / RangeWindow groups partition over the
	// effective series identity. Without this, rows with distinct
	// ServiceName collapse into a single Attributes bucket — the
	// `sum by (service_name) (rate({__name__=~".+"}[5m]))` task-#232
	// bug. The Project lands between the Scan/Filter and the per-mode
	// wraps so PREWHERE-eligible matchers stay on the raw Scan
	// (the optimizer's promotion path is untouched).
	//
	// Order-of-operations for `pred`: when the augmenting Project
	// kicks in it preserves only the canonical Sample quadruple
	// (MetricName, Attributes, TimeUnix, Value) on its output side —
	// any raw scan column the matcher predicate references
	// (`ServiceName` in particular, via the `service_name` coalesce
	// chain from PR #679 / task #232) goes out of scope above the
	// Project. The downstream LWR / range-vector wrappers would then
	// apply the matcher Filter ON TOP of the augmented Project and CH
	// rejects the query with `Unknown expression or function
	// identifier 'ServiceName'` (HTTP 502, error 47 — caught by PR
	// #681's Phase-3 filter-drill sweep on
	// `topk(10, sum by (service_name) (rate({__name__=~".+",service_name="api"}[5m])))`).
	// The fix sinks `pred` to a Filter immediately above the raw
	// scan-side input BEFORE augmenting — at that layer every raw
	// column (including ServiceName) is still in scope and the
	// optimizer's PREWHERE promotion path still sees the matcher
	// Filter directly above the Scan. The downstream wrappers then
	// receive `pred=nil` and only attach the LWR / staleness time
	// bounds (which reference TimeUnix, preserved by the augment).
	//
	// The bucket-suffix case at L188-204 bakes pred into the
	// fan-out's inner Filter and zeroes pred before this point, so
	// the branch below is a no-op for that path.
	if pred != nil && augmentAttributesForOuterBy(s, ctx.outerByLabels) != nil {
		selectorInput = &chplan.Filter{Input: selectorInput, Predicate: pred}
		pred = nil
	}
	selectorInput = augmentSelectorAttributes(selectorInput, ctx, s)

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
			return selectorInput, nil
		}
		return &chplan.Filter{Input: selectorInput, Predicate: pred}, nil
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
			return wrapRangeAbsoluteAtBroadcast(selectorInput, pred, anchor, ctx, s), nil
		}
		return wrapRangeLatestPerSeries(selectorInput, pred, anchor, ctx, s), nil
	}
	// Instant-vector context: the LWR wrapper applies both the
	// `Timestamp <= anchor` upper bound and the staleness lower
	// bound, so we DON'T pre-add the modifier's timeBoundExpr here —
	// that would duplicate the upper-bound predicate.
	return wrapInstantLatestPerSeries(selectorInput, pred, anchor, s), nil
}

// wrapHistogramCompanionProject wraps a histogram-table Scan in a
// Project that synthesises the canonical Sample-row shape:
// `(MetricName, Attributes, TimeUnix, toFloat64(<col>) AS Value)`. The
// LWR / RangeWindow / Aggregate nodes downstream reference
// `s.ValueColumn` ("Value") generically — projecting the histogram-row
// `Count` / `Sum` column under that alias keeps the rest of the
// lowering pipeline schema-agnostic about which companion suffix it's
// servicing.
//
// `toFloat64` is required because OTel-CH's histogram `Count` is
// `UInt64` while the canonical PromQL `Value` is `Float64`. CH would
// otherwise silently up-cast inside arithmetic, but emitting the cast
// here keeps the downstream rate / arithmetic expressions consistent
// with the gauge / sum-table path (where `Value` is already
// `Float64`).
func wrapHistogramCompanionProject(scan *chplan.Scan, sourceColumn string, s schema.Metrics) chplan.Node {
	return &chplan.Project{
		Input: scan,
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: s.MetricNameColumn}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}, Alias: s.TimestampColumn},
			{
				Expr: &chplan.FuncCall{
					Name: "toFloat64",
					Args: []chplan.Expr{&chplan.ColumnRef{Name: sourceColumn}},
				},
				Alias: s.ValueColumn,
			},
		},
	}
}

// needCompanionUnion reports whether the classic-histogram-companion
// multi-arm UnionAll lowering applies. All five guards must hold:
// (1) the lowering identified a companion-suffix metric (non-empty
// `companionValueColumn`); (2) the suffixed user-visible name is
// non-empty (the histogram-arm Project synthesises it as a literal);
// (3) the bare base name is non-empty (the histogram-arm filter
// targets it); (4) a Sum table is configured; (5) the Sum table is
// physically distinct from the Histogram table so the two arms read
// from different physical layouts. Any miss falls through to the
// single-arm histogram emit path that PR #710 already covers.
func needCompanionUnion(s schema.Metrics, companionValueColumn, companionSuffixed, companionBare string) bool {
	if companionValueColumn == "" || companionSuffixed == "" || companionBare == "" {
		return false
	}
	if s.SumTable == "" || s.SumTable == s.HistogramTable {
		return false
	}
	return true
}

// lowerCompanionUnion builds the chplan subtree for a
// `<base>_count` / `<base>_sum` selector that resolves against both
// the histogram + sum tables. The output mirrors the surrounding
// lowering's wrap shape (LWR for instant queries, range-mode pivot
// for query_range, identity passthrough for nested range-vector
// callers) so the union plugs into the broader pipeline transparently.
//
// MetricName + non-MetricName matchers are baked into each per-arm
// Filter — the outer pred passed to wrapRange* / wrapInstant* is nil
// because the arm-level Filters already narrowed every relevant row.
func lowerCompanionUnion(
	v *parser.VectorSelector, s schema.Metrics, ctx lowerCtx,
	matchers []*labels.Matcher,
	bareName, suffixedName, sourceColumn string,
) (chplan.Node, error) {
	histArm := buildHistogramCompanionArm(s, matchers, bareName, suffixedName, sourceColumn)
	sumArm := buildSumCompanionArm(s, matchers, suffixedName)
	selectorInput := chplan.Node(&chplan.UnionAll{Inputs: []chplan.Node{histArm, sumArm}})
	anchor, err := selectorAnchor(v, ctx)
	if err != nil {
		return nil, err
	}
	selectorInput = augmentSelectorAttributes(selectorInput, ctx, s)
	if ctx.inRangeVector {
		// Nested range-vector consumer (rate / *_over_time / subquery):
		// the surrounding RangeWindow owns the per-window aggregation.
		// The `@`/offset modifier still pins a per-step time bound — we
		// add it as a thin Filter on top of the canonical Sample shape
		// the union produces. Absent a modifier the union flows through
		// unchanged.
		if hasModifier(v) {
			timeBound := timeBoundExpr(s.TimestampColumn, anchor)
			return &chplan.Filter{Input: selectorInput, Predicate: timeBound}, nil
		}
		return selectorInput, nil
	}
	if ctx.step > 0 && !ctx.start.IsZero() && !ctx.end.IsZero() {
		if hasAbsoluteAt(v) {
			return wrapRangeAbsoluteAtBroadcast(selectorInput, nil, anchor, ctx, s), nil
		}
		return wrapRangeLatestPerSeries(selectorInput, nil, anchor, ctx, s), nil
	}
	return wrapInstantLatestPerSeries(selectorInput, nil, anchor, s), nil
}

// buildHistogramCompanionArm assembles the histogram-table arm of the
// classic-histogram-companion UnionAll. The arm scans the histogram
// table with the MetricName filter rewritten to the BARE base name
// (the OTel-CH histogram row keyed by `<base>`), projects the
// companion column (Count or Sum) as the canonical `Value`, and
// synthesises `MetricName` as the SUFFIXED user-visible name so the
// downstream pipeline (LWR / range-vector / matrix pivot) sees a
// uniform `MetricName = '<base>_count'` / `'<base>_sum'` label across
// both arms of the union.
//
// Non-MetricName matchers (attribute / service / regex matchers the
// user wrote alongside `__name__`) flow through unchanged so the arm's
// scan-side Filter still narrows on every other matcher. The bare
// `__name__` rewrite is local to this arm — the sum arm
// (`buildSumCompanionArm`) sees the suffixed name in matcher form.
func buildHistogramCompanionArm(
	s schema.Metrics, matchers []*labels.Matcher,
	bareName, suffixedName, sourceColumn string,
) chplan.Node {
	armMatchers := rewriteMetricName(matchers, bareName)
	scan := &chplan.Scan{Table: s.HistogramTable}
	var armInput chplan.Node = scan
	if pred := buildPredicate(armMatchers, s); pred != nil {
		armInput = &chplan.Filter{Input: scan, Predicate: pred}
	}
	return &chplan.Project{
		Input: armInput,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: suffixedName}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}, Alias: s.TimestampColumn},
			{
				Expr: &chplan.FuncCall{
					Name: "toFloat64",
					Args: []chplan.Expr{&chplan.ColumnRef{Name: sourceColumn}},
				},
				Alias: s.ValueColumn,
			},
		},
	}
}

// buildSumCompanionArm assembles the sum-table arm of the
// classic-histogram-companion UnionAll. The arm scans the sum table
// with the MetricName filter kept on the SUFFIXED user-visible name
// (`system_cpu_logical_count`, `system_processes_count`, etc. — the
// shape OTel-hostmetrics emits for these counters) and projects the
// canonical Sample-row quadruple directly. The Value column is
// already `Float64` on the sum table, so no `toFloat64` cast is
// required (the histogram arm needs the cast because its Count column
// is UInt64).
func buildSumCompanionArm(
	s schema.Metrics, matchers []*labels.Matcher, suffixedName string,
) chplan.Node {
	// Defensive: thread the suffixed name back through rewriteMetricName
	// so any non-Equal `__name__` matchers in the input list (regex
	// alternations etc.) flow unchanged and only the canonical
	// `__name__ = <suffixed>` literal is normalised. The lowering's
	// metricNameFromMatchers contract already pinned the suffixed name
	// as the canonical Equal matcher, so this is a no-op for the
	// production input shape but the helper stays robust against
	// alternate matcher shapes upstream callers might thread in.
	armMatchers := rewriteMetricName(matchers, suffixedName)
	scan := &chplan.Scan{Table: s.SumTable}
	var armInput chplan.Node = scan
	if pred := buildPredicate(armMatchers, s); pred != nil {
		armInput = &chplan.Filter{Input: scan, Predicate: pred}
	}
	return &chplan.Project{
		Input: armInput,
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: s.MetricNameColumn}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}, Alias: s.TimestampColumn},
			{Expr: &chplan.ColumnRef{Name: s.ValueColumn}, Alias: s.ValueColumn},
		},
	}
}

// augmentSelectorAttributes wraps `input` with a Project that rebinds
// the Attributes column to `mapConcat(Attributes, <synthesised top-
// level columns>)` when the enclosing aggregation's by-clause
// (threaded via [lowerCtx.outerByLabels]) references a label that
// routes to a dedicated top-level OTel-CH column. When the by-clause
// references no such label — the common case — the function returns
// `input` unchanged so existing fixture SQL stays byte-identical.
//
// The Project's column shape preserves the canonical Sample-row
// quadruple (MetricName, Attributes, TimeUnix, Value) the downstream
// LWR / RangeWindow consumes. The dedicated top-level column
// (ServiceName) is read by `augmentAttributesForOuterBy` from the
// row's input scope — when `input` is a Scan / Filter the column is
// directly addressable; when `input` is a `wrapHistogramCompanion-
// Project` the column flows through unchanged because the histogram
// companion Project preserves every original Scan column the next
// SELECT references (CH resolves `ServiceName` against the inner
// subquery's underlying table).
//
// Mirrors the LogQL augmenting wrap in
// [internal/logql.withDetectedLevelAndColumns] (PR #666 / task #218)
// at a different layer: LogQL inflates the post-RangeWindow identity
// map; PromQL inflates the pre-RangeWindow per-row Attributes so the
// RangeWindow's `GROUP BY Attributes` already partitions over the
// distinct ServiceName values.
func augmentSelectorAttributes(input chplan.Node, ctx lowerCtx, s schema.Metrics) chplan.Node {
	augmented := augmentAttributesForOuterBy(s, ctx.outerByLabels)
	if augmented == nil {
		return input
	}
	return &chplan.Project{
		Input: input,
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: s.MetricNameColumn}, Alias: s.MetricNameColumn},
			{Expr: augmented, Alias: s.AttributesColumn},
			{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}, Alias: s.TimestampColumn},
			{Expr: &chplan.ColumnRef{Name: s.ValueColumn}, Alias: s.ValueColumn},
		},
	}
}

// rewriteMetricName returns a copy of matchers where any
// `__name__=<X>` (MatchEqual) matcher carries the supplied bare name in
// place of `<X>`. Used by the classic-histogram companion rewrite to
// strip the `_count` / `_sum` suffix from the `__name__` matcher so the
// emitted filter resolves against the bare metric name OTel-CH writes.
//
// Non-`__name__` matchers and non-Equal `__name__` matchers (e.g.
// `__name__=~"foo|bar"`) flow through unchanged: the bare-name strip
// only applies to a single equality matcher, which is the only shape
// `metricNameFromMatchers` recognises in the first place.
//
// Copy-on-write semantics mirror stripBucketSuffix: a fresh slice +
// fresh matcher are allocated, the input is never mutated. The parser
// can reuse the matcher slice across lowering passes and a mutation
// here would silently bleed back into later passes.
func rewriteMetricName(matchers []*labels.Matcher, bareName string) []*labels.Matcher {
	out := make([]*labels.Matcher, len(matchers))
	for i, m := range matchers {
		if m.Name == model.MetricNameLabel && m.Type == labels.MatchEqual && m.Value != bareName {
			copied, err := labels.NewMatcher(m.Type, m.Name, bareName)
			if err != nil {
				out[i] = m
				continue
			}
			out[i] = copied
			continue
		}
		out[i] = m
	}
	return out
}

// scanFromTables returns the chplan.Scan node for a metric-matcher
// lowering. A single-element `tables` slice routes to the legacy
// `Scan{Table: …}` shape so existing fixtures and emit paths remain
// byte-stable; a multi-element slice routes to `Scan{UnionTables: …}`
// which the chsql emitter renders as a CH `merge(currentDatabase(),
// '<regex>')` table function call (see `chsql.scanTableFrag`). The
// multi-element path supports the OTel-emitter case where a bare
// (unsuffixed) metric name could be either a Gauge or a cumulative
// Sum — the suffix heuristic alone can't disambiguate. The empty
// slice is treated as a Gauge-only fallback (the caller's default
// when the matcher carries no `__name__`).
func scanFromTables(tables []string) *chplan.Scan {
	switch len(tables) {
	case 0:
		// Unreachable from the production caller (which always passes
		// at least one candidate from schema.Metrics.TablesFor /
		// GaugeTable) — but the defensive zero-element case keeps the
		// helper total without panicking. Returning an empty Scan
		// would surface a downstream emit-time validation error
		// ("Scan has neither Table nor UnionTables set"), which is the
		// correct failure mode if a future caller passes nil/empty.
		return &chplan.Scan{}
	case 1:
		return &chplan.Scan{Table: tables[0]}
	default:
		// Defensive copy: the caller's slice may be a return from
		// schema.Metrics.TablesFor whose backing array is shared with
		// the schema. A downstream optimizer pass that wanted to
		// in-place mutate UnionTables would corrupt the schema; the
		// copy keeps the plan-tree slice independent.
		owned := append([]string(nil), tables...)
		return &chplan.Scan{UnionTables: owned}
	}
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
func wrapInstantLatestPerSeries(scan chplan.Node, pred chplan.Expr, anchor evalAnchor, s schema.Metrics) chplan.Node {
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
// selector evaluated over a query_range window. It emits a single
// chplan.RangeLWR node:
//
//	RangeLWR step=<step> lookback=5m [offset=<o>] ts=TimeUnix value=Value start=<s> end=<e>
//	  Scan + matchers_filter
//
// The RangeLWR emitter (internal/chsql.emitRangeLWR) renders the
// single-pass, bounded sample-side fan-out: each sample fans out to ONLY
// the ≤ lookback/step + 1 anchors whose staleness window
// `(anchor - offset - 5m, anchor - offset]` contains it, then a
// `GROUP BY (MetricName, Attributes, anchor_ts)` with
// `argMax(Value, TimeUnix)` collapses each (series, anchor) bucket to its
// newest in-window sample. The output is the canonical 4-column Sample
// contract `(MetricName, Attributes, TimeUnix = anchor_ts, Value)`, one
// row per (series, anchor) that had data — identical to the shape the
// prior StepGrid CROSS JOIN + per-anchor argMax produced, but at
// O(rows × lookback/step) intermediate cardinality (constant in the grid
// width N) instead of O(rows × N).
//
// The half-open window edges, the offset-shifts-the-window-not-the-anchor
// semantics, and the staleness-gap "no sample → no row" rule are all
// preserved by the RangeLWR emitter (see range_lwr.go). The
// `@<absolute>` pinned-anchor shape is routed away upstream
// (wrapRangeAbsoluteAtBroadcast), so anchor.End is zero here and only
// anchor.Offset shifts the window.
//
// Output schema preservation lets the surrounding plan tree (aggregations,
// arithmetic, instant fns) keep consuming the same column shape it did
// before — each (series) produces N rows (one per step inside
// `[start, end]` that had data) rather than a single row at `end_ts`.
func wrapRangeLatestPerSeries(scan chplan.Node, pred chplan.Expr, anchor evalAnchor, ctx lowerCtx, s schema.Metrics) chplan.Node {
	// Inner Scan/Filter — apply matchers via PREWHERE-eligible filter
	// the optimizer already promotes. The `(scan, pred)` split keeps the
	// downstream PREWHERE path unchanged; when pred is nil (no matchers)
	// the scan flows directly into the RangeLWR.
	rawSide := scan
	if pred != nil {
		rawSide = &chplan.Filter{Input: scan, Predicate: pred}
	}

	return &chplan.RangeLWR{
		Input:         rawSide,
		Start:         ctx.start.UTC(),
		End:           ctx.end.UTC(),
		Step:          ctx.step,
		Lookback:      instantLookback,
		Offset:        anchor.Offset,
		MetricNameCol: s.MetricNameColumn,
		AttributesCol: s.AttributesColumn,
		TimestampCol:  s.TimestampColumn,
		ValueCol:      s.ValueColumn,
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
func wrapRangeAbsoluteAtBroadcast(scan chplan.Node, pred chplan.Expr, anchor evalAnchor, ctx lowerCtx, s schema.Metrics) chplan.Node {
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

// BuildMatcherPredicate is the exported wrapper around [buildPredicate]
// for callers outside the promql package (notably the
// /api/v1/query_exemplars handler in internal/api/prom) that need to
// turn a VectorSelector's matcher list into the same chplan.Expr the
// PromQL `/query` and `/query_range` lowering paths produce.
//
// The two paths must share the matcher → predicate translation so the
// exemplars endpoint applies the same Attributes-map lookup, the same
// regex semantics, and the same MetricName-column / schema-aware
// top-level-column routing the rest of PromQL uses. Sharing keeps
// "what does `label=~regex` mean" defined in exactly one place;
// schema-aware matcher rewrites (e.g. pushing `service.name` to
// [schema.Metrics.ServiceNameColumn] instead of the Attributes map)
// live in [matcherToExpr] and flow to every caller automatically.
//
// Returns nil for an empty matcher list — callers fold a nil
// predicate into "no WHERE clause" rather than emitting a `WHERE true`
// equivalent.
func BuildMatcherPredicate(matchers []*labels.Matcher, s schema.Metrics) chplan.Expr {
	return buildPredicate(matchers, s)
}

// matcherToExpr resolves a single PromQL label matcher into the
// chplan predicate that lands on the inner Scan's Filter. The three
// routing branches are:
//
//  1. `__name__` — references the dedicated MetricName column.
//
//  2. A label that names a top-level OTel-CH column (currently only
//     `service.name` / `service_name` → `ServiceName`). The lookup
//     coalesces the dedicated column with the Attributes-map fallback
//     so producers that wrote either side (OTel-collector → top-level
//     column; raw inserts → Attributes-map key) both resolve.
//     `nullIf(<col>, ”)` rewrites the String-default-empty sentinel
//     back to NULL so `coalesce` selects the map fallback when the
//     dedicated column is unpopulated. Mirrors the LogQL fix from
//     PR #669 / task #217 in [internal/logql.matcherToExpr].
//
//  3. Anything else — falls through to the Attributes-map lookup
//     (with the dot/underscore candidate expansion documented on
//     [attributeLookup]).
func matcherToExpr(m *labels.Matcher, s schema.Metrics) chplan.Expr {
	if m.Name == model.MetricNameLabel {
		return metricNamePredicate(m, s)
	}
	var lhs chplan.Expr
	mapLookup := attributeLookup(s.AttributesColumn, m.Name)
	if col := schemaTopLevelColumn(s, m.Name); col != "" {
		lhs = &chplan.FuncCall{
			Name: "coalesce",
			Args: []chplan.Expr{
				&chplan.FuncCall{
					Name: "nullIf",
					Args: []chplan.Expr{
						&chplan.ColumnRef{Name: col},
						&chplan.LitString{V: ""},
					},
				},
				mapLookup,
			},
		}
	} else {
		lhs = mapLookup
	}
	return &chplan.Binary{
		Op:    matchOp(m.Type),
		Left:  lhs,
		Right: &chplan.LitString{V: m.Value},
	}
}

// metricNamePredicate resolves a `__name__` matcher against the
// dedicated MetricName column. Equality and negated-equality matchers
// whose value carries at least one rewritable underscore fan out
// across every OTel-dotted candidate from
// [format.PromLabelToOTelCandidates], because the `__name__` catalog
// surface (`/api/v1/label/__name__/values`) Prom-normalises stored
// dotted MetricNames (`k8s.node.cpu.usage` → `k8s_node_cpu_usage`)
// through `OTelToPromMetric` — so the matcher side must accept the
// underscored alias for rows whose stored name is still dotted, or
// every catalog-advertised kubeletstats / k8scluster / semconv-dotted
// metric returns an empty result the moment Grafana (or
// Drilldown-Metrics) queries the name it was just shown. This is the
// `__name__` analogue of the Attributes-map candidate chain in
// [attributeLookup] (PR #658) and the matcher-string fan-out the
// catalog endpoints already apply via
// [internal/api/prom.expandUnderscoredMetricNameMatcher].
//
// Shapes emitted:
//
//   - `__name__="<v>"`  → `MetricName IN (v, c1, …)`
//   - `__name__!="<v>"` → `MetricName NOT IN (v, c1, …)`
//     (a user excluding the advertised alias expects the dotted
//     storage rows excluded too — the candidates are one logical
//     series set, so the negation must reject every spelling).
//     The IN / NOT IN tuple is the flat, constant-depth, parameterised
//     equivalent of an OR / AND chain of (in)equalities: a span-metric
//     name fans out to a 2^6 = 64-element candidate powerset, and the
//     metadata handlers UNION-ALL up to 192 such variant arms into one
//     combined query — an inline OR-chain blew past ClickHouse's 256KB
//     `max_query_size` (code 62) on the metrics-explorer broad probe,
//     while the IN tuple renders the column once + N `?` placeholders.
//   - `__name__=~"<re>"`  → `match(MetricName, re) OR
//     match(replaceRegexpAll(MetricName, '[^a-zA-Z0-9_:]', '_'), re)`.
//     The regex cannot be re-expanded across the candidate powerset
//     (that would change its meaning), so instead the COLUMN side is
//     normalised: the second arm mirrors `format.OTelToPromMetric` in
//     SQL so an underscored pattern (`.*container_cpu_usage.*`, the
//     exact shape Grafana's Metrics Drilldown breakdown tab sends for
//     every catalog-advertised name) matches rows whose stored name is
//     still dotted (`container.cpu.usage`). The leading-digit `_`
//     prefix `OTelToPromMetric` applies is not mirrored — OTel metric
//     names never start with a digit.
//   - `__name__!~"<re>"` → `NOT match(MetricName, re) AND NOT
//     match(<normalised>, re)`: the raw and normalised spellings are
//     one logical series set, so the negation must reject both.
//
// Values with no rewritable underscore (`up`, `gen`) — and values that
// produce a single candidate — keep the legacy single-comparison
// emit, byte-stable with the pre-fan-out fixtures. The InList is
// `isCheapPredicate`-shaped (InList over ColumnRef / LitString), so
// the optimizer's PREWHERE promotion treats it exactly like the
// single equality it replaces.
// promMetricNormalizePattern is the SQL-side mirror of
// [format.OTelToPromMetric]: every byte outside the Prom metric-name
// grammar `[a-zA-Z0-9_:]` is rewritten to `_`. Used by the regex
// `__name__` arm of [metricNamePredicate] to compare the
// Prom-normalised spelling of a stored (possibly dotted) MetricName
// against the user's regex. Keep in lock-step with the Go-side
// normaliser in internal/api/format/otelname.go.
const promMetricNormalizePattern = "[^a-zA-Z0-9_:]"

func metricNamePredicate(m *labels.Matcher, s schema.Metrics) chplan.Expr {
	single := &chplan.Binary{
		Op:    matchOp(m.Type),
		Left:  &chplan.ColumnRef{Name: s.MetricNameColumn},
		Right: &chplan.LitString{V: m.Value},
	}
	if m.Type == labels.MatchRegexp || m.Type == labels.MatchNotRegexp {
		normalized := &chplan.Binary{
			Op: matchOp(m.Type),
			Left: &chplan.FuncCall{
				Name: "replaceRegexpAll",
				Args: []chplan.Expr{
					&chplan.ColumnRef{Name: s.MetricNameColumn},
					&chplan.LitString{V: promMetricNormalizePattern},
					&chplan.LitString{V: "_"},
				},
			},
			Right: &chplan.LitString{V: m.Value},
		}
		fold := chplan.OpOr
		if m.Type == labels.MatchNotRegexp {
			fold = chplan.OpAnd
		}
		return &chplan.Binary{Op: fold, Left: single, Right: normalized}
	}
	if m.Type != labels.MatchEqual && m.Type != labels.MatchNotEqual {
		return single
	}
	if !format.PromLabelNeedsDottedFallback(m.Value) {
		return single
	}
	candidates := format.PromLabelToOTelCandidates(m.Value)
	if len(candidates) <= 1 {
		return single
	}
	// Render the candidate set as a single flat, parameterised
	// `MetricName IN (?, …)` (NOT IN for the `!=` matcher) rather than a
	// left-associative OR/AND chain of equality Binary nodes. The flat IN
	// is the load-bearing shape: a heavily-underscored span-metric name
	// (e.g. `traces_service_graph_request_server_seconds_sum`) fans out to
	// the 2^6 = 64-element powerset of dotted re-expansions, and the
	// metadata handlers UNION-ALL up to 192 such variant arms into one
	// combined query. An OR-chain renders 64 inline `(MetricName = 'lit'
	// OR …)` terms *per arm*; crossed with the arm fan-out the rendered
	// SQL crossed ClickHouse's 256KB `max_query_size` at position 262124
	// (code 62, "Max query size exceeded") on the metrics-explorer broad
	// probe. An IN tuple renders the column once + N `?` placeholders —
	// compact text, constant parser depth — regardless of N. InList is
	// classified cheap + PREWHERE-promotable by the optimizer (see
	// internal/chsql/prewhere.go), so this preserves the
	// granule-prune posture the single-equality emit had.
	list := make([]chplan.Expr, len(candidates))
	for i, cand := range candidates {
		list[i] = &chplan.LitString{V: cand}
	}
	return &chplan.InList{
		Left:    &chplan.ColumnRef{Name: s.MetricNameColumn},
		List:    list,
		Negated: m.Type == labels.MatchNotEqual,
	}
}

// attributeLookup returns the chplan.Expr that resolves a Prom matcher
// name `key` against the CH Map column `col`. For names with no
// rewritable underscore (e.g. `job`, `__name__`) it returns a plain
// MapAccess — byte-stable with the pre-#658 emit shape so the existing
// fixtures keep matching.
//
// For names with at least one rewritable underscore (e.g. `cerberus_ql`)
// it emits a left-associative `if(mapContains(col, k1), col[k1],
// col[k2])` chain over every candidate from
// [format.PromLabelToOTelCandidates]. The chain returns the first
// matching value or the last candidate's empty-default — which
// matches Prometheus's "label absent → empty string" semantics for
// the matcher comparison.
//
// Why not `coalesce(col[k1], col[k2])`? CH's `Attributes['missing']`
// returns the value-type's default (empty string for `Map(String,
// String)`), not NULL, so `coalesce` would short-circuit on the very
// first lookup even when the row's actual key is the dotted form.
// `mapContains` distinguishes "key present with empty value" from
// "key absent" cleanly. The runtime cost is one extra `mapContains`
// per candidate beyond the first — CH evaluates this against the
// column's per-row map and the optimizer can hoist common
// sub-expressions, so the overhead is bounded.
//
// Fixture impact: every PromQL fixture whose matcher name contains an
// internal underscore now emits the if-chain. The chplan IR snapshot
// expands accordingly; `just update-golden` regenerates the SQL +
// chplan sections in lock-step.
func attributeLookup(col, key string) chplan.Expr {
	if !format.PromLabelNeedsDottedFallback(key) {
		return &chplan.MapAccess{
			Map: &chplan.ColumnRef{Name: col},
			Key: &chplan.LitString{V: key},
		}
	}
	candidates := format.PromLabelToOTelCandidates(key)
	if len(candidates) <= 1 {
		// Belt-and-braces — `PromLabelNeedsDottedFallback` already
		// returned true so we expect >= 2 candidates. Falling through
		// to the bare MapAccess keeps the contract sane if the helper
		// ever drifts.
		return &chplan.MapAccess{
			Map: &chplan.ColumnRef{Name: col},
			Key: &chplan.LitString{V: key},
		}
	}
	// Build the if-chain right-associatively so the leftmost candidate
	// (the underscored input) wins when present:
	//
	//   if(mapContains(col, k0), col[k0],
	//     if(mapContains(col, k1), col[k1],
	//       ... col[kN-1]))
	//
	// The terminal branch is a bare MapAccess against the last
	// candidate — when no candidate's key is present, the empty-string
	// default matches Prom's "absent label" semantics for matcher
	// comparison.
	mapRef := &chplan.ColumnRef{Name: col}
	last := candidates[len(candidates)-1]
	var chain chplan.Expr = &chplan.MapAccess{
		Map: mapRef,
		Key: &chplan.LitString{V: last},
	}
	for i := len(candidates) - 2; i >= 0; i-- {
		k := candidates[i]
		chain = &chplan.FuncCall{
			Name: "if",
			Args: []chplan.Expr{
				&chplan.FuncCall{
					Name: "mapContains",
					Args: []chplan.Expr{
						mapRef,
						&chplan.LitString{V: k},
					},
				},
				&chplan.MapAccess{
					Map: mapRef,
					Key: &chplan.LitString{V: k},
				},
				chain,
			},
		}
	}
	return chain
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
// if recognised. Unrecognised functions surface a clear "unsupported"
// error to the caller.
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
	case "histogram_count", "histogram_sum", "histogram_avg",
		"histogram_stddev", "histogram_stdvar", "histogram_fraction":
		return lowerHistogramValueFn(c, s, ctx)
	case "label_replace":
		return lowerLabelReplace(c, s, ctx)
	case "label_join":
		return lowerLabelJoin(c, s, ctx)
	case "time":
		return lowerTime(c, s, ctx)
	case "vector":
		return lowerVector(c, s, ctx)
	case "year", "month", "day_of_month", "day_of_week", "day_of_year",
		"days_in_month", "hour", "minute", "timestamp":
		return lowerDateFn(c, s, ctx)
	case "sort", "sort_desc":
		return lowerSort(c, s, ctx)
	case "scalar":
		return lowerScalarTopLevel(c, s, ctx)
	case "start", "end", "range", "step":
		// Query-context functions: their value is constant for a given
		// query execution (it depends only on the eval range, not on
		// series data). The reference engine constant-folds these into
		// NumberLiterals before evaluation; cerberus folds them at
		// lowering into a synthetic scalar vector, mirroring `time()` /
		// `vector(N)`. See [lowerQueryContextFold].
		return lowerQueryContextFold(c, s, ctx)
	case "pi":
		// Bare top-level `pi()` (or any scalar-foldable call the parser
		// admits as a top-level expression). The /api/v1/query handler
		// answers these in Go via TryFoldScalar without touching CH, but
		// the lowering path must still materialise a one-row synthetic
		// vector so query_range + the surface-parity prober (which drive
		// lower→emit directly) accept the symbol. lowerScalarArg folds
		// pi() to a LitFloat; syntheticScalarVector wraps it as the
		// canonical single-sample shape.
		return lowerScalarTopLevel(c, s, ctx)
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
		// `double_exponential_smoothing` (and its `holt_winters` alias) is
		// an experimental PromQL function: the reference backend
		// (prom/prometheus:v3.11.3, started WITHOUT
		// `--enable-feature=promql-experimental-functions` in the
		// compatibility harness) rejects it. Cerberus's parser enables
		// experimental functions for the deliberately-supported extension
		// subset (`@start()`/`@end()`, `predict_linear`), so the parser
		// accepts `double_exponential_smoothing` and lowering would
		// otherwise build a RangeWindow the chsql emitter executes —
		// silently turning a parity rejection into a wrong acceptance.
		// To keep the PromQL head at strict reference parity we reject
		// here at the lowering dispatch, mirroring the `first_over_time`
		// gate. `lowerHoltWinters` is retained so the package-internal
		// boundary-guard unit tests (gremlins_kill_test.go) can keep
		// exercising the (0,1) smoothing/trend factor checks directly.
		// The message contains "unsupported: range function" so the
		// showcase-promql parity-rejection contract substring matches.
		return nil, fmt.Errorf("unsupported: range function %q is experimental and not supported by the PromQL head", c.Func.Name)
	case "absent_over_time":
		return lowerAbsentOverTime(c, s, ctx)
	case "first_over_time":
		// `first_over_time` is an experimental PromQL function: the
		// reference backend (prom/prometheus:v3.11.3, started without
		// `--enable-feature=promql-experimental-functions` in the
		// compatibility harness) rejects it. Cerberus's parser enables
		// experimental functions for the deliberately-supported subset
		// (`@start()`/`@end()`, `double_exponential_smoothing`,
		// `predict_linear`), so the parser accepts `first_over_time`
		// and lowering would otherwise build a RangeWindow that the
		// *shared* chsql over-time emitter now executes — the LogQL
		// burndown added `first_over_time` to that emitter's reducer
		// set (range_window.go) for `first_over_time(... | unwrap v)`.
		// To keep the PromQL head at reference parity we reject here,
		// before the RangeWindow reaches the LogQL-shared emitter. The
		// LogQL head keeps its `first_over_time` support; only the
		// PromQL lowering path is gated. The message mirrors the
		// emitter's `ErrUnsupported: range function %q` wording so the
		// showcase-promql parity-rejection contract substring
		// ("unsupported: range function") still matches.
		return nil, fmt.Errorf("unsupported: range function %q is experimental and not supported by the PromQL head", c.Func.Name)
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
	rangeMode := ctx.step > 0 && !ctx.start.IsZero() && !ctx.end.IsZero()
	pinned := hasAbsoluteAt(vs)
	switch {
	case rangeMode && pinned:
		// `@`-pinned range-vector call (`rate(m[5m] @ <ts>)`,
		// `... @ start()` / `@ end()`) under query_range. Reference
		// PromQL evaluates the SAME pinned window [anchor - range,
		// anchor] at EVERY step in [start, end] — the `@` fixes the
		// anchor, only the OUTPUT timestamps vary. The bare matrix
		// fan-out below would instead re-anchor the window onto each
		// step grid point (the clobber overwrites rw.End with
		// ctx.end), so the pin is lost and `rate(m[5m] @ T)` fans the
		// rate across the grid rather than broadcasting the single
		// pinned value. Keep rw as the INSTANT shape (Step=0,
		// End=anchor.End, no OuterRange) — it produces one row per
		// series at the pinned window — then broadcast that value
		// across the step grid via a CrossJoin(StepGrid). This is the
		// range-vector sibling of wrapRangeAbsoluteAtBroadcast (the
		// bare-selector `@`-pin path).
		return wrapRangeWindowAtBroadcast(rw, ctx, s, c.Func.Name, metricNameFromMatchers(vs.LabelMatchers)), nil
	case rangeMode:
		// In range mode, fan the range function across the request's step
		// grid: each anchor in [start, end] (spaced by step) emits one row
		// per series with the per-anchor function value. The emitter
		// already supports this via OuterRange + Step (the matrix path used
		// by subqueries); we just need to flip the switch when LowerAtRange
		// threaded a non-zero step. Without this, `rate(m[5m])` over
		// query_range degenerates to a single anchor at end_ts and the
		// matrix pivot only sees one sample per series — the same root
		// cause as the bare-selector range-mode bug Pool-AK is fixing.
		rw.Start = ctx.start.UTC()
		rw.End = ctx.end.UTC()
		rw.Step = ctx.step
		rw.OuterRange = ctx.end.Sub(ctx.start)

		// Experimental opt-in: substitute the ClickHouse-native
		// timeSeriesRateToGrid lowering for the eligible rate query_range
		// shape. The native node carries the same Func/Range/Step/Start/
		// End/Offset/columns/GroupBy as the fan-out RangeWindow above —
		// only the emitter differs — and produces the identical
		// per-(series, anchor) row shape (proven byte-identical on the
		// chDB substrate; see test/spec/promql/native_rate_range_step.txtar
		// and the dual-emit parity test). `rate` drops `__name__` (it is a
		// derived sample), so the native node returns directly here,
		// bypassing the last/first_over_time name-preservation wrap below.
		if native := maybeNativeTSGridRate(rw, ctx); native != nil {
			return native, nil
		}
	}
	// `last_over_time` and `first_over_time` preserve `__name__`
	// per Prometheus semantics — they're position-shift reducers that
	// pick a single sample from the window, so the emitted sample carries
	// the source metric's name. Every other range-vector fn (rate,
	// increase, delta, sum_over_time, ...) produces a derived sample
	// and Prom drops `__name__` for them. See upstream:
	//
	//	prometheus/prometheus@cerberus-parser/promql/engine.go:2114
	//	`dropName := (e.Func.Name != "last_over_time" && e.Func.Name != "first_over_time")`
	//
	// The RangeWindow output schema is (Attributes, [anchor_ts,] Value)
	// — MetricName is dropped by the windowed-array GROUP BY Attributes.
	// To preserve `__name__` we wrap the RangeWindow with a canonical
	// 4-column Project that pins MetricName to the matcher's literal
	// name. The HTTP-layer `wrapWithSampleProjection` recognises this
	// shape (via `projectionExposesCanonical`) and skips its
	// derived-shape `LitString{""} AS MetricName` synthesis, so the
	// literal flows through and `__name__` appears on the wire.
	//
	// The matcher's `__name__` value must be a single equality matcher
	// (`metric_name{...}`) for `metricNameFromMatchers` to return a
	// non-empty string. Regex `__name__=~"foo|bar"` falls through to
	// the existing empty-MetricName behaviour — threading per-series
	// names through the windowed aggregation is a larger structural
	// change not modelled here.
	if c.Func.Name == "last_over_time" || c.Func.Name == "first_over_time" {
		return wrapRangeWindowPreserveName(rw, s, metricNameFromMatchers(vs.LabelMatchers)), nil
	}
	return rw, nil
}

// maybeNativeTSGridRate returns a chplan.RangeWindowNative when rw is an
// eligible `rate(<counter>[<range>])` query_range RangeWindow AND the
// experimental flag is set; otherwise nil (the caller keeps the fan-out
// RangeWindow). The eligibility predicate is intentionally narrow — every
// clause that fails sends the query down the unchanged fan-out path:
//
//   - ctx.experimentalTSGridRange must be true (default false).
//   - rw.Func must be "rate". increase / delta have no proven-equivalent
//     timeSeries*ToGrid aggregate yet (no timeSeriesIncreaseToGrid; the
//     timeSeriesDeltaToGrid + reset-semantics mapping is unverified), so
//     they stay on the fan-out until a dedicated differential sweep lands.
//   - The window must be the materialised range grid: Step > 0 and both
//     Start and End pinned. (The caller only reaches this with rw in
//     matrix shape, but the guard is explicit so the node's invariants
//     hold regardless of caller.)
//   - rw.Identity must be false (the bare-vector subquery no-op path is
//     not a rate) and rw.Input must be a plain Scan / Filter — the
//     row-shape relation timeSeriesRateToGrid consumes. Inputs that route
//     through MetricsAggregate / MetricsHistogramOverTime / MetricsCompare
//     keep their own emit branches.
//
// The OuterRange field is intentionally NOT copied: it is a fan-out-only
// emit knob (the matrix anchor span) that the native grid encodes
// directly via Start/End/Step.
func maybeNativeTSGridRate(rw *chplan.RangeWindow, ctx lowerCtx) *chplan.RangeWindowNative {
	if !ctx.experimentalTSGridRange {
		return nil
	}
	if rw.Func != "rate" {
		return nil
	}
	if rw.Identity || rw.Step <= 0 || rw.Start.IsZero() || rw.End.IsZero() {
		return nil
	}
	if !isPlainScanFilter(rw.Input) {
		return nil
	}
	return &chplan.RangeWindowNative{
		Input:           rw.Input,
		Func:            rw.Func,
		Range:           rw.Range,
		Step:            rw.Step,
		Start:           rw.Start,
		End:             rw.End,
		Offset:          rw.Offset,
		TimestampColumn: rw.TimestampColumn,
		ValueColumn:     rw.ValueColumn,
		GroupBy:         rw.GroupBy,
	}
}

// isPlainScanFilter reports whether n is a row-shape relation the native
// timeSeriesRateToGrid emitter can consume directly: a Scan, or a Filter
// chain bottoming out in a Scan. Anything else (the metrics_* TraceQL
// families, joins, set-ops) has its own emit branch and is ineligible.
func isPlainScanFilter(n chplan.Node) bool {
	for {
		switch v := n.(type) {
		case *chplan.Scan:
			return true
		case *chplan.Filter:
			n = v.Input
		default:
			return false
		}
	}
}

// wrapRangeWindowPreserveName wraps a RangeWindow with a canonical
// 4-column Project that pins MetricName to a literal so the HTTP-layer
// `wrapWithSampleProjection` recognises the canonical shape and
// preserves `__name__` on the wire. Used by `last_over_time` /
// `first_over_time` to mirror Prom's `dropName=false` for these fns.
//
// The matrix-shape RangeWindow (Step > 0) carries per-row anchors in
// the `anchor_ts` column; the instant shape doesn't expose a real
// TimeUnix at all (the SQL emits only Attributes + Value), so the
// projection synthesises one via the same `now64() - 5s` expression
// the handler uses for derived-shape Projects. The outer
// `wrapWithSampleProjection` canonical branch reads back the
// `s.TimestampColumn` alias verbatim either way.
func wrapRangeWindowPreserveName(rw *chplan.RangeWindow, s schema.Metrics, name string) chplan.Node {
	var tsExpr chplan.Expr
	if rw.OuterRange > 0 {
		tsExpr = &chplan.ColumnRef{Name: "anchor_ts"}
	} else {
		// Mirror `synthesizedAnchor()` in internal/api/prom/handler.go:
		// the instant-shape RangeWindow doesn't expose a real per-row
		// anchor, so we stamp `now64(9) - toIntervalNanosecond(5e9)`.
		tsExpr = chplan.NowNanoMinusStaleness()
	}
	return &chplan.Project{
		Input: rw,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: name}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: tsExpr, Alias: s.TimestampColumn},
			{Expr: &chplan.ColumnRef{Name: s.ValueColumn}, Alias: s.ValueColumn},
		},
	}
}

// wrapRangeWindowAtBroadcast broadcasts an INSTANT-shape range-vector
// RangeWindow (rate / increase / *_over_time / ...) pinned by an
// absolute `@` modifier across the request's step grid. The pinned
// window is evaluated ONCE (rw is left in its instant shape: Step=0,
// End=anchor.End), yielding one row per series with the canonical
// `(Attributes, Value)` shape the instant range emitter produces. A
// CrossJoin with a StepGrid spanning [start, end] then fans that single
// per-series value across every step timestamp, and the outer Project
// restores the matrix 4-column contract `(Attributes, anchor_ts,
// anchor_ts AS TimeUnix, Value)` the non-pinned range path emits — so
// downstream consumers (aggregations, arithmetic) see the identical
// column shape whether or not the inner carried an `@` pin.
//
// `last_over_time` / `first_over_time` preserve `__name__`
// (dropName=false in Prom); for them the projection pins MetricName to
// the matcher's literal name and exposes the canonical 4-column Sample
// contract `(MetricName, Attributes, anchor_ts AS TimeUnix, Value)`,
// mirroring wrapRangeWindowPreserveName's matrix branch. Every other
// range fn drops `__name__`, so MetricName is omitted.
func wrapRangeWindowAtBroadcast(rw *chplan.RangeWindow, ctx lowerCtx, s schema.Metrics, fn, name string) chplan.Node {
	grid := &chplan.StepGrid{Start: ctx.start.UTC(), End: ctx.end.UTC(), Step: ctx.step}
	joined := &chplan.CrossJoin{Left: grid, Right: rw}

	preserveName := fn == "last_over_time" || fn == "first_over_time"
	projections := make([]chplan.Projection, 0, 4)
	if preserveName {
		projections = append(projections,
			chplan.Projection{Expr: &chplan.LitString{V: name}, Alias: s.MetricNameColumn})
	}
	projections = append(
		projections,
		chplan.Projection{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
		chplan.Projection{Expr: &chplan.ColumnRef{Name: "anchor_ts"}, Alias: "anchor_ts"},
		chplan.Projection{Expr: &chplan.ColumnRef{Name: "anchor_ts"}, Alias: s.TimestampColumn},
		chplan.Projection{Expr: &chplan.ColumnRef{Name: s.ValueColumn}, Alias: s.ValueColumn},
	)
	return &chplan.Project{Input: joined, Projections: projections}
}

// lowerAggregate handles `sum by (job) (...)`, `sum without (instance) (...)`,
// `count(...)`, `stddev(...)`, `stdvar(...)`, `group(...)`, and
// `quantile(0.95, ...)`. The shape-changing aggregates `topk`/`bottomk` are
// handled separately via lowerTopK — they produce K rows per partition
// rather than one, so they map to a TopK plan node (CH's `LIMIT K BY`)
// instead of the regular Aggregate. `count_values` is handled separately
// via lowerCountValues (one row per (partition, distinct value) pair).
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

	// Thread the outer by-clause's labels down so the inner selector
	// path can inflate Attributes with the top-level OTel-CH columns
	// (currently `service_name` → `ServiceName`) the outer aggregate
	// needs to partition over. Only `by(...)` propagates — `without(...)`
	// exclusion semantics don't reference specific columns, so the
	// without branch keeps the lean Attributes shape. See
	// [augmentAttributesForOuterBy] for the resulting Project wrap and
	// [internal/logql.lowerCtx.OuterByLabels] for the LogQL precedent
	// (PR #666 / task #218).
	innerCtx := ctx
	if !a.Without {
		innerCtx = ctx.withOuterByLabels(a.Grouping)
	}
	input, err := lower(a.Expr, s, innerCtx)
	if err != nil {
		return nil, err
	}

	groupBy, err := aggregateGroupBy(a, s)
	if err != nil {
		return nil, err
	}

	aggFunc, err := buildAggFunc(a, s, ctx)
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
		return wrapQuantilePhiGuard(wrapped, a, s, ctx)
	}
	return wrapped, nil
}

// wrapQuantilePhiGuard applies PromQL's quantile phi-domain rules to
// the aggregate's output Value. A literal out-of-range phi folds to
// the ±Inf / NaN constant at lowering time (outOfRangePhiInf); a
// computed phi resolves the same rules at runtime — buildAggFunc bound
// a sanitised phi parameter (sentinel 0.5 when out of domain), and the
// guard projects NaN / -Inf / +Inf over the sentinel quantile per
// Prom's quantile() helper. The phi expression is re-lowered here —
// CH caches scalar subqueries, so the repeated reference costs one
// evaluation per statement.
func wrapQuantilePhiGuard(wrapped chplan.Node, a *parser.AggregateExpr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if phi, ok := tryScalarLiteral(a.Param); ok {
		if infValue, outOfRange := outOfRangePhiInf(phi); outOfRange {
			return projectValueOverInner(wrapped, s, &chplan.LitFloat{V: infValue}), nil
		}
		return wrapped, nil
	}
	phiE, err := lowerScalarArg(a.Param, s, ctx)
	if err != nil {
		return nil, err
	}
	return projectValueOverInner(wrapped, s,
		outOfRangePhiGuardExpr(phiE, &chplan.ColumnRef{Name: s.ValueColumn})), nil
}

// lowerCountValues lowers `count_values("label", expr) [by(g) | without(g)]`.
// The shape is: for each distinct value of `expr` (within each grouped
// partition), emit a row whose Attributes carry the unique value as a
// synthetic label binding (`<label>=<stringified value>`) plus the
// preserved per-partition labels, and whose Value is the count of input
// series that hit that value.
//
// SQL shape (no grouping):
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
// SQL shape (with `without(g1, g2)`):
//
//	SELECT '' AS MetricName,
//	       mapConcat(gkey_0, map('<label>', cv_val)) AS Attributes,
//	       now64(9) AS TimeUnix,
//	       count() AS Value
//	FROM (<inner>)
//	GROUP BY mapFilter((k, v) -> NOT (k IN ('g1', 'g2')), Attributes) AS gkey_0,
//	         toString(Value) AS cv_val
//
// SQL shape (with `without()` — degenerate empty without-set):
//
//	GROUP BY Attributes AS gkey_0, toString(Value) AS cv_val
//
// The without variant follows the same template as `sum without (...)`
// (see aggregateGroupBy / wrapAggregateForSample): the partition key is
// the Attributes map with the removed labels stripped via mapFilter, and
// the output Attributes is that partition map with the synthetic
// `<label>=cv_val` binding overlaid via `mapConcat`. `mapConcat` is
// later-key-wins, so when `<label>` collides with a preserved label the
// count_values binding takes precedence — matching Prometheus's
// `count_values` semantics where the synthetic label overwrites.
func lowerCountValues(a *parser.AggregateExpr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
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

	const (
		valueKeyAlias = "cv_val"
		countAlias    = "cv_count"
	)

	// Build the group-key list and the per-key Attributes-map fragment
	// for the wrapping Project. The two variants differ in how they
	// partition the input rows:
	//
	//   - by(l1, l2, ...) — one Attributes[lbl] MapAccess per named
	//     label; the wrap reconstructs the partition map by string-
	//     literal pairs (`map('l1', gkey_0, ...)`) wrapped in
	//     MapWithoutEmptyValues to drop unset-label slots.
	//
	//   - without(l1, l2, ...) — one MapWithoutKeys spanning the full
	//     Attributes map; the wrap references the single gkey_0 column
	//     directly and overlays the synthetic `<label>` binding via
	//     mapConcat.
	//
	//   - without() — degenerate "remove nothing" — equivalent to
	//     grouping by the full Attributes map; the wrap uses the same
	//     mapConcat overlay path.
	var (
		groupBy []chplan.Expr
		aliases []string
	)
	switch {
	case a.Without && len(a.Grouping) == 0:
		// `without ()` — partition by the full Attributes map.
		groupBy = []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}}
		aliases = []string{"gkey_0"}
	case a.Without:
		groupBy = []chplan.Expr{&chplan.MapWithoutKeys{
			Map:  &chplan.ColumnRef{Name: s.AttributesColumn},
			Keys: append([]string(nil), a.Grouping...),
		}}
		aliases = []string{"gkey_0"}
	default:
		groupBy = make([]chplan.Expr, 0, len(a.Grouping))
		aliases = make([]string, 0, len(a.Grouping))
		for i, lbl := range a.Grouping {
			// Mirror the matcher-side dotted-fallback so
			// `count_values(...) by (cerberus_ql)` partitions over
			// both the underscored and dotted CH key forms.
			groupBy = append(groupBy, attributeLookup(s.AttributesColumn, lbl))
			aliases = append(aliases, fmt.Sprintf("gkey_%d", i))
		}
	}

	// Range mode (ctx.step > 0): PromQL's count_values partitions
	// **per evaluation step**, not across the whole range. The inner
	// plan's range shape re-aliases the per-step anchor onto TimeUnix
	// (see wrapRangeLatestPerSeries), so grouping by TimeUnix gives
	// the per-anchor partitioning, and the wrapping Project surfaces
	// the anchor as the sample timestamp. Without this thread the
	// aggregate collapsed every anchor into one row stamped
	// `now64(9)`, which the matrix pivot then dropped — every
	// range-mode count_values returned an empty matrix (surfaced by
	// the showcase-promql count_values panel).
	const anchorAlias = "cv_anchor"
	rangeMode := ctx.step > 0
	if rangeMode {
		groupBy = append(groupBy, &chplan.ColumnRef{Name: s.TimestampColumn})
		aliases = append(aliases, anchorAlias)
	}

	// Append the value-as-label group key; the wrapping Project
	// references it by alias to bind the synthetic `<label>` column.
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

	// Build the Attributes map for the wrapping Project.
	var attrs chplan.Expr
	switch {
	case a.Without:
		// `without(...)` / `without()` — partition map already lives
		// in `gkey_0`. Overlay the synthetic `<label>=cv_val` binding
		// via mapConcat (later-arg-wins, matching Prom's "synthetic
		// label overwrites collisions" semantics).
		attrs = &chplan.FuncCall{
			Name: "mapConcat",
			Args: []chplan.Expr{
				&chplan.ColumnRef{Name: "gkey_0"},
				&chplan.FuncCall{
					Name: "map",
					Args: []chplan.Expr{
						&chplan.LitString{V: label},
						&chplan.ColumnRef{Name: valueKeyAlias},
					},
				},
			},
		}
	default:
		// `by(g)` / no grouping — reconstruct the partition map by
		// string-literal pairs and wrap with MapWithoutEmptyValues so
		// series whose grouped-by label was absent in the OTel-CH
		// Attributes Map don't surface as `{g=""}` on the wire.
		mapArgs := make([]chplan.Expr, 0, (len(a.Grouping)+1)*2)
		for i, lbl := range a.Grouping {
			mapArgs = append(
				mapArgs,
				&chplan.LitString{V: lbl},
				&chplan.ColumnRef{Name: aliases[i]},
			)
		}
		mapArgs = append(
			mapArgs,
			&chplan.LitString{V: label},
			&chplan.ColumnRef{Name: valueKeyAlias},
		)
		attrs = &chplan.MapWithoutEmptyValues{
			Map: &chplan.FuncCall{Name: "map", Args: mapArgs},
		}
	}

	// Instant mode stamps the single evaluation timestamp; range mode
	// forwards the per-step anchor captured in the group key above.
	tsExpr := chplan.NowNano()
	if rangeMode {
		tsExpr = &chplan.ColumnRef{Name: anchorAlias}
	}

	return &chplan.Project{
		Input: agg,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			{Expr: attrs, Alias: s.AttributesColumn},
			{Expr: tsExpr, Alias: s.TimestampColumn},
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

// topKDomain validates a topk/bottomk K parameter against reference
// Prometheus semantics. The pinned engine
// (tsouza/prometheus@cerberus-parser promql/engine.go, rangeEvalAgg +
// aggregationK) handles the parameter in this order:
//
//  1. `params.Max() < 1` → return early with an EMPTY result (2xx).
//     This covers K = 0, every negative K (including -Inf), and
//     fractional K below 1 — none of them are errors upstream.
//  2. NaN K → eval error ("Parameter value is NaN").
//  3. K >= maxInt64 → eval error ("Scalar value %v overflows int64").
//     (The symmetric underflow check is unreachable for a literal K:
//     any K <= minInt64 already took the empty-result branch.)
//  4. Otherwise K truncates toward zero (`int64(fParam)`), so
//     `topk(1.5, v)` selects the top 1 series.
//
// Returns (k, false, nil) for the regular path, (0, true, nil) for the
// empty-result short-circuit, and a non-nil error for the two shapes
// reference Prometheus itself rejects.
func topKDomain(op parser.ItemType, kF float64) (k int64, empty bool, err error) {
	switch {
	case kF < 1:
		// Mirrors upstream's `params.Max() < 1` early return — NaN
		// compares false here (as upstream) and falls through to the
		// NaN error below.
		return 0, true, nil
	case math.IsNaN(kF):
		return 0, false, fmt.Errorf("promql: %s K must not be NaN", op.String())
	case kF >= float64(math.MaxInt64):
		return 0, false, fmt.Errorf("promql: %s K %v overflows int64", op.String(), kF)
	}
	return int64(kF), false, nil
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
// K follows reference Prometheus's parameter domain (see topKDomain):
// K < 1 — including 0, negatives and sub-1 fractions — short-circuits
// to an empty result (a constant-false Filter over the lowered input,
// keeping the canonical column shape); fractional K >= 1 truncates
// toward zero; NaN / int64-overflow K are rejected exactly where the
// reference engine rejects them.
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
	// Literal-K fast path (the common case: `topk(5, v)`, `topk(2+3, v)`,
	// any scalar tree TryFoldScalar can reduce). Falls through to the
	// computed-K path when the K argument is a `scalar(<vector>)` call.
	kF, ok := tryScalarLiteral(a.Param)
	if !ok {
		return lowerTopKComputed(a, s, ctx)
	}
	k, empty, err := topKDomain(a.Op, kF)
	if err != nil {
		return nil, err
	}

	input, err := lower(a.Expr, s, ctx)
	if err != nil {
		return nil, err
	}
	if empty {
		// K < 1 → empty result per reference semantics (see
		// topKDomain). Filter the lowered input to zero rows so the
		// plan keeps the canonical column shape — same posture as
		// clamp's degenerate-bounds fold in instant_fns.go.
		return &chplan.Filter{
			Input:     input,
			Predicate: &chplan.LitBool{V: false},
		}, nil
	}

	by := topKPartition(a, s, ctx)

	return &chplan.TopK{
		Input:    input,
		K:        k,
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

// topKPartition derives the partition expressions for `topk`/`bottomk`
// from the aggregation's grouping shape. Shared between the literal-K
// and computed-K lowering paths because the partition semantics are
// identical — only the K binding differs.
//
// `without (...)` partitions by Attributes minus the listed labels;
// `without ()` partitions by the full Attributes map (so each series
// is its own partition); `by (l1, ...)` partitions by the listed
// label values. Range mode (ctx.step > 0) appends the TimeUnix anchor
// so the topk fires per evaluation step rather than globally — the
// PromQL semantics for `topk(K, v)` over a range.
func topKPartition(a *parser.AggregateExpr, s schema.Metrics, ctx lowerCtx) []chplan.Expr {
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
			// Dotted-fallback parity with the matcher / non-topk
			// aggregation path: `topk(K, v) by (cerberus_ql)` partitions
			// across both the underscored and dotted CH-keyed rows.
			by = append(by, attributeLookup(s.AttributesColumn, label))
		}
	}

	// Range mode: thread the per-step anchor (TimeUnix re-aliased from
	// anchor_ts by the inner wrapRangeLatestPerSeries) into the partition
	// list so the per-partition top-K fires per anchor. The instant path
	// (ctx.step == 0) keeps the original partition shape so the existing
	// instant-mode fixtures stay byte-stable.
	if ctx.step > 0 {
		by = append(by, &chplan.ColumnRef{Name: s.TimestampColumn})
	}
	return by
}

// lowerTopKComputed lowers `topk(scalar(<vector>), v)` and
// `bottomk(scalar(<vector>), v)` — the computed-K case where K is the
// value of a scalar subquery rather than a literal integer. CH's LIMIT
// clause requires a constant, so we route the lowering through
// chplan.TopK's KExpr slot; the emitter then renders a `row_number()
// OVER (...) <= K` rank filter (see emitTopKComputed).
//
// Only `scalar(<vector>)` is accepted as the K shape — mixed forms
// like `topk(2 + scalar(x), v)` would require constant-folding around
// the scalar subquery, which is a larger structural change not
// modelled here. The PromQL parser already type-checks the K arg as
// scalar-valued, so this is a narrow filter on the lowering surface.
func lowerTopKComputed(a *parser.AggregateExpr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	// Peel ParenExpr wrappers so `topk((scalar(x)), v)` still routes
	// here. The parser keeps explicit parens in the AST.
	param := a.Param
	for {
		p, ok := param.(*parser.ParenExpr)
		if !ok {
			break
		}
		param = p.Expr
	}
	call, ok := param.(*parser.Call)
	if !ok || call.Func == nil || call.Func.Name != "scalar" {
		return nil, fmt.Errorf("promql: %s K must be a scalar literal or scalar(<vector>); computed-K with other shapes is not yet supported", a.Op.String())
	}
	if len(call.Args) != 1 {
		return nil, fmt.Errorf("promql: %s K: scalar() expects 1 argument, got %d", a.Op.String(), len(call.Args))
	}

	// Lower the K argument in instant context (step=0). PromQL's
	// `scalar(v)` produces a single value per eval; range-mode would
	// fan it out into one row per step, but the emitter's K subquery
	// reads only the first row (LIMIT 1) so the matrix shape would be
	// wasted work. Reusing the surrounding ctx (with step > 0) would
	// also drag a StepGrid CROSS JOIN into the K subtree, bloating the
	// SQL for no semantic gain — the result vector's shape comes from
	// `a.Expr`, not the K subtree.
	kCtx := ctx
	kCtx.step = 0
	kExpr, err := lower(call.Args[0], s, kCtx)
	if err != nil {
		return nil, fmt.Errorf("promql: %s K: %w", a.Op.String(), err)
	}

	input, err := lower(a.Expr, s, ctx)
	if err != nil {
		return nil, err
	}

	by := topKPartition(a, s, ctx)

	return &chplan.TopK{
		Input:    input,
		KExpr:    kExpr,
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

	tsExpr := chplan.NowNano()
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
		// Re-use the matcher-side dotted-fallback helper so a
		// `sum by (cerberus_ql)` clause hits both the underscored AND
		// dotted row keys, matching the resolution `cerberus_ql{...}`
		// gets from buildPredicate. Without parity here the grouping
		// would collapse every dotted-keyed row into a single "" bucket
		// while the matcher path saw them as distinct series.
		out = append(out, attributeLookup(s.AttributesColumn, label))
	}
	return out, nil
}

// buildAggFunc produces the single AggFunc for an aggregation. The output-
// shape-changing aggregates `topk`/`bottomk` and `count_values` are handled
// out-of-band via lowerTopK / lowerCountValues before this function is
// called. Anything else that reaches the default arm here is rejected.
//
// ctx is consumed only by the computed-phi quantile path (lowerScalarArg
// needs the eval anchor for `scalar()` / `time()` shapes).
func buildAggFunc(a *parser.AggregateExpr, s schema.Metrics, ctx lowerCtx) (chplan.AggFunc, error) {
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
		// `any(toFloat64(1))` which yields a constant 1.0 per CH group.
		//
		// The `1` literal is wrapped in `toFloat64(...)` because the
		// clickhouse-go/v2 driver renders Go `int64(1)` as the SQL
		// literal `1` and CH narrows that to `UInt8`. `any(UInt8)`
		// returns UInt8, and the downstream cursor scans Value as
		// `*float64`. The driver refuses to convert UInt8 → *float64
		// at Scan time (`converting UInt8 to *float64 is unsupported`)
		// and the prom handler surfaces it as a 502. Wrapping in
		// `toFloat64(?)` forces CH to project Float64 on the wire
		// regardless of the bound literal's inferred type. Mirrors the
		// same wrap in [lowerAbsent] and [syntheticScalarVector]'s
		// callers. Cannot piggy-back on
		// `chsql/emit_node.go::intReturningAggregates` because `any`
		// over a Float64 / Array(Float64) column (e.g.
		// `any(ExplicitBounds)` in histogram_quantile) must NOT be
		// wrapped, so the fix has to be at the literal — not the
		// aggregate-name dispatch.
		if a.Param != nil {
			return chplan.AggFunc{}, fmt.Errorf("promql: group() does not take a parameter")
		}
		return chplan.AggFunc{
			Name: "any",
			Args: []chplan.Expr{
				&chplan.FuncCall{
					Name: "toFloat64",
					Args: []chplan.Expr{&chplan.LitInt{V: 1}},
				},
			},
			Alias: s.ValueColumn,
		}, nil

	case parser.QUANTILE:
		if phi, ok := tryScalarLiteral(a.Param); ok {
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
		}
		// Computed phi (`quantile(scalar(x), v)`): bind phi as a
		// scalar-subquery parameter. CH accepts a scalar subquery in
		// the aggregate-parameter position (it folds to a constant
		// during query analysis), but errors at runtime on a phi
		// outside [0, 1] — sanitizedPhiParamExpr clamps the parameter
		// to a 0.5 sentinel for the out-of-domain cases and
		// lowerAggregate post-wraps the output Value through
		// outOfRangePhiGuardExpr (NaN phi → NaN, phi<0 → -Inf,
		// phi>1 → +Inf) so the sentinel quantile is never observed —
		// the same split as the literal path, resolved at runtime.
		phiE, err := lowerScalarArg(a.Param, s, ctx)
		if err != nil {
			return chplan.AggFunc{}, err
		}
		return chplan.AggFunc{
			Name:   "quantile",
			Params: []chplan.Expr{sanitizedPhiParamExpr(phiE)},
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
