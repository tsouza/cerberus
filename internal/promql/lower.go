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

	"github.com/tsouza/cerberus/internal/api/format"
	"github.com/tsouza/cerberus/internal/cerbtrace"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/qlcommon"
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
	plan, err := lower(expr, s, lowerCtx{lowerers: RangeLowerers{}.withDefaults()})
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
	// Lowerers is the BOOT-WIRED polymorphic dispatch table for the
	// ClickHouse-native timeSeries*ToGrid family. cmd/cerberus builds it ONCE
	// at boot from the resolved chopt.EnabledSet (per-function: native rate and
	// native staleness are independent), wiring each field to a CONCRETE
	// strategy (the native impl with an embedded fan-out fallback, or the
	// fan-out impl directly), and threads it through the prom handler -> lang
	// adapter into here. The zero value (nil strategy fields) is the
	// "no caller opted in" sentinel, resolved to the all-fan-out table at the
	// lowering-entry seam, so a caller that does not opt in lowers
	// byte-identically to the pre-seam path. The per-query lowering dispatches
	// through this table as a plain interface method call — NO feature-flag /
	// version conditional AND NO nil/presence check; the only per-query
	// decisions are AST node-type and query-SHAPE eligibility, which live inside
	// each strategy. See [RangeLowerers].
	Lowerers RangeLowerers

	// Guards is the sink for the scalar-parameter domain checks lowering
	// cannot settle on its own — see [ScalarGuard]. A caller that can
	// execute a second statement before the main one (the query
	// handlers, through the engine) passes a non-nil pointer and gets
	// reference Prometheus's evaluation-time rejections; a caller that
	// only wants the plan (the spec harness, [Lower] and friends) leaves
	// it nil and no guard plan is built.
	Guards *[]ScalarGuard
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
		start:    start,
		end:      end,
		step:     step,
		lowerers: opts.Lowerers.withDefaults(),
		guards:   opts.Guards,
	})
	if err != nil {
		span.RecordError(err)
		return nil, err
	}
	span.SetAttributes(cerbtrace.AttrPlanNodeCount.Int(cerbtrace.CountNodes(plan)))
	return plan, nil
}

// LowerMetadataRange lowers a bare metadata matcher (one match[] selector
// from /api/v1/series, /api/v1/labels, or /api/v1/label/<name>/values)
// over the FULL [start,end] request window.
//
// Reference Prometheus answers these metadata endpoints by enumerating
// every series/label/value with ANY sample anywhere in [start,end]. That
// is NOT the instant-query contract [LowerAt] implements: an instant
// lowering anchors a 5-minute LWR staleness window at `end` and collapses
// to the latest sample per series — which silently drops any series whose
// only sample sits earlier in the requested window (the rc.9 /series
// empty-window bug, and the same defect in /labels + /label/<name>/values
// once you account for late-arriving samples). LowerMetadataRange is the
// single chokepoint that gives all three endpoints the correct full-range
// window: the [metadataFullRange] flag routes the bare-selector path to
// [wrapMetadataFullRange], which emits a closed `Timestamp >= start AND
// Timestamp <= end` filter (a zero start/end omits that bound — whole-
// table scan, matching reference defaults) with NO staleness collapse.
func LowerMetadataRange(ctx context.Context, expr parser.Expr, s schema.Metrics, start, end time.Time) (chplan.Node, error) {
	_, span := tracer.Start(ctx, cerbtrace.SpanLower, trace.WithAttributes(cerbtrace.AttrQL.String("promql")))
	defer span.End()
	plan, err := lower(expr, s, lowerCtx{start: start, end: end, metadataFullRange: true, lowerers: RangeLowerers{}.withDefaults()})
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
		plan, err := lowerSubquery(e, s, ctx)
		if err != nil {
			return nil, err
		}
		// Instant bare subquery (no outer range-vector fn wrapping it): bound
		// the inner scan to the eval window, the same way
		// lowerOuterRangeFnOverSubquery does for `<fn>(<subquery>)`. Bare
		// subqueries route here, not through that path, so without this
		// `rate(m[5m])[90d:1s]` / `up[5m:1m]` on /api/v1/query read FULL
		// RETENTION (#1109 GAP-2 axis-1, bare shape). Gated on instant
		// (step == 0); range mode widens via widenSubquerySpine inside
		// lowerOuterRangeFnOverSubquery and never reaches a bare top-level
		// subquery (a range query over a matrix-typed expr is rejected upstream).
		if ctx.step == 0 {
			if a, aerr := subqueryAnchor(e, ctx); aerr == nil && !a.End.IsZero() {
				widenSubquerySpine(plan, a.End.Add(-e.Range), a.End)
			}
		}
		// A bare subquery is a raw range vector — no reducer, so no
		// `dropName` — and must report each series' own `__name__`.
		// Widening runs first: it walks RangeWindow spines and would not
		// see past the canonical Project wrapBareSubqueryName adds.
		return wrapBareSubqueryName(plan, s), nil
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
// lowerHistogramSelectorInput builds the selector's input subtree for the
// classic-histogram companion (`_count` / `_sum`) and `_bucket` fan-out
// paths, returning the (possibly wrapped) input node, the residual
// predicate the LWR / range-vector wrapper must still apply, and whether
// the wrap already folded the resource-attribute merge into its
// Attributes projection (`attributesPreMerged`).
//
// For the bare-selector path (neither bucketSuffixed nor
// companionValueColumn set) the input is the raw scan, pred is returned
// unchanged, and attributesPreMerged is false — the caller merges
// resource attributes at the selector seam.
//
//   - `_bucket` (bucketSuffixed != ""): the scan-side filter feeds into
//     the fan-out Project, then a post-fan-out Filter applies any
//     `le=<bound>` matcher against the synthesized `Attributes['le']` key.
//     That key lives ONLY in the (already resource-merged) Attributes map,
//     so the `le` predicate is built against a schema view with the
//     resource column cleared to keep it a bare Attributes match. `pred`
//     is baked into the fan-out's input Filter and returned nil so the
//     downstream wrapper doesn't re-apply it.
//   - `_count` / `_sum` (companionValueColumn != ""): wrap the scan in the
//     companion Project that aliases the source Count / Sum column as
//     `Value`. pred passes through to the downstream wrapper.
//
// Both companion paths set attributesPreMerged=true because their
// canonical-quadruple output drops the raw ResourceAttributes and
// dedicated (ServiceName) columns — so the merge AND the dedicated-column
// overlay must both be folded in per-arm (they are, via
// [selectorLabelProjections]) rather than stacked above the union.
func lowerHistogramSelectorInput(
	scan *chplan.Scan,
	pred chplan.Expr,
	bucketSuffixed string,
	bucketLeMatchers []*labels.Matcher,
	companionValueColumn string,
	s schema.Metrics,
	cat *metadataCatalog,
) (chplan.Node, chplan.Expr, bool) {
	switch {
	case bucketSuffixed != "":
		var fanInput chplan.Node = scan
		if pred != nil {
			fanInput = &chplan.Filter{Input: scan, Predicate: pred}
		}
		selectorInput := wrapHistogramBucketFanout(fanInput, bucketSuffixed, s, cat)
		leSchema := s
		leSchema.ResourceAttributesColumn = ""
		if lePred := buildPredicate(bucketLeMatchers, leSchema); lePred != nil {
			selectorInput = &chplan.Filter{Input: selectorInput, Predicate: lePred}
		}
		return selectorInput, nil, true
	case companionValueColumn != "":
		return wrapHistogramCompanionProject(scan, companionValueColumn, s, cat), pred, true
	}
	return scan, pred, false
}

// expHistogramSelectorRouting resolves how lowerVectorSelector should
// handle a selector that names (or companion-suffixes) an exp-histogram
// metric — split out of lowerVectorSelector's if/else chain to keep that
// function's cyclomatic complexity in check (see #1704).
//
// ok is false when metricName has nothing to do with an exp-histogram
// metric at all, and the caller's existing classic-histogram / bucket
// handling applies unchanged.
//
// When ok is true and err is nil, the `_count` / `_sum` companion suffix
// is in play: the OTel-CH exp-histogram exporter writes Count/Sum as
// columns on a single row keyed by the bare `<base>_exp_hist` name, the
// same companion convention the classic-histogram path serves — just
// reading from ExpHistogramTable instead of HistogramTable. Unlike the
// classic case there is no hostmetrics/standalone-gauge name collision to
// fan across: `_exp_hist` is cerberus's own synthetic marker suffix, not
// an upstream naming convention any other emitter could produce, so this
// is always a single-arm projection (the caller never routes it through
// the multi-table companion union).
//
// When ok is true and err is non-nil, every other shape over a pinned
// exp-histogram selector — a bare selector, or one wrapped in
// rate()/resets()/changes()/sum()/etc. — has no scalar Value column to
// read: the exp-histogram row shape (Sum/Count/Scale/PositiveCounts/
// NegativeCounts) is disjoint from the Sample contract these functions
// reduce over. histogram_quantile()/histogram_count()/histogram_sum()
// detect this shape themselves before ever reaching lowerVectorSelector
// (see their own s.IsExpHistogramMetric checks), and the companion arm
// above is the one column-backed exception — so any selector that
// reaches here is a shape none of those paths recognise. err rejects it
// explicitly rather than silently matching zero rows against the
// Gauge/Sum tables.
//
// Metadata enumeration (/series, /labels — ctx.metadataFullRange) is
// exempted: it doesn't consume a Value column, only MetricName +
// Attributes, so a hard error there would break legitimate series/label
// discovery for a real metric. It still resolves against Gauge/Sum like
// before this function existed — the exp-histogram table can't join that
// merge() fan-out (disjoint row shape, see TablesForUnknownName) — a
// separate, already-documented gap this function doesn't newly introduce.
func expHistogramSelectorRouting(metricName string, s schema.Metrics, ctx lowerCtx, matchers []*labels.Matcher) (tables []string, newMatchers []*labels.Matcher, companionValueColumn, companionBare string, ok bool, err error) {
	if bare, col, hasCompanion := s.HistogramCompanionColumn(metricName); hasCompanion && s.IsExpHistogramMetric(bare) && s.ExpHistogramTable != "" {
		return []string{s.ExpHistogramTable}, rewriteMetricName(matchers, bare), col, bare, true, nil
	}
	if s.IsExpHistogramMetric(metricName) && !ctx.metadataFullRange {
		return nil, nil, "", "", true, fmt.Errorf(
			"promql: %q is an exponential histogram metric; only histogram_quantile(), "+
				"histogram_count(), histogram_sum(), and the %q/%q companion selectors are supported",
			metricName, metricName+"_count", metricName+"_sum",
		)
	}
	return nil, nil, "", "", false, nil
}

// selectorRouting is resolveSelectorRouting's result: the physical
// table(s) + matcher rewrite lowerVectorSelector's LWR/range-vector
// pipeline should scan, plus whichever companion/bucket bookkeeping
// downstream steps (lowerHistogramSelectorInput, needCompanionUnion)
// need to pick the right Project/UnionAll shape.
type selectorRouting struct {
	tables                []string
	matchers              []*labels.Matcher
	companionValueColumn  string
	bucketSuffixed        string
	bucketLeMatchers      []*labels.Matcher
	companionSuffixed     string
	companionBare         string
	expHistogramCompanion bool
}

// resolveSelectorRouting picks which physical table(s) a vector selector
// scans and how its matchers should be rewritten, covering the
// `_bucket` classic-histogram fan-out, the exp-histogram companion /
// rejection routing (expHistogramSelectorRouting), and the classic
// `_count`/`_sum` histogram-companion suffix — in that priority order.
// Split out of lowerVectorSelector to keep that function's cyclomatic
// complexity in check (see #1704); defaultTables is the TablesFor /
// TablesForUnknownName result the caller already resolved, used as-is
// when none of the three special cases match.
func resolveSelectorRouting(metricName string, s schema.Metrics, ctx lowerCtx, defaultTables []string, matchers []*labels.Matcher) (selectorRouting, error) {
	route := selectorRouting{tables: defaultTables, matchers: matchers}

	// `<base>_bucket` takes a parallel-but-distinct path: the OTel-CH
	// histogram row stores per-bucket counts as the `BucketCounts` array
	// with `ExplicitBounds` carrying the bucket edges. Prom exposes the
	// same data as N+1 separate series under `<base>_bucket{le=<bound>}`,
	// so the bare-selector lowering fans the array into N+1 Sample-shape
	// rows via arrayJoin. See wrapHistogramBucketFanout for the plan
	// shape. The bucket suffix is detected via isClassicBucketSelector;
	// the matcher-strip + `le` matcher split happens in splitBucketMatchers.
	if bareBucket, ok := isClassicBucketSelector(metricName, s); ok {
		route.tables = []string{s.HistogramTable}
		route.bucketSuffixed = metricName
		scanMatchers, leMatchers := splitBucketMatchers(matchers, bareBucket)
		route.matchers = scanMatchers
		route.bucketLeMatchers = leMatchers
		return route, nil
	}

	if expTables, expMatchers, expCol, expBare, expOK, expErr := expHistogramSelectorRouting(metricName, s, ctx, matchers); expOK {
		if expErr != nil {
			return selectorRouting{}, expErr
		}
		route.tables = expTables
		route.matchers = expMatchers
		route.expHistogramCompanion = true
		route.companionValueColumn = expCol
		route.companionSuffixed = metricName
		route.companionBare = expBare
		return route, nil
	}

	// Classic-histogram companion routing: `<base>_count` / `<base>_sum`
	// are Prom-convention companion names whose data lives, in the OTel-CH
	// layout, as `Count` / `Sum` columns on rows written under the bare
	// `<base>` name in the histogram table. Reroute the scan + strip the
	// suffix off the `__name__` matcher + alias the column as `Value` so
	// the downstream Sample-row contract holds. Mirrors stripBucketSuffix
	// (PR #637) for the `_bucket` companion, and the per-arm row-key
	// rewrite schema.Metrics.ExemplarSources drives on the exemplars path.
	//
	// Two physical layouts may carry the matching rows:
	//
	//   1. The OTel-CH histogram exporter writes Count/Sum as columns on
	//      a single row keyed by the BARE `<base>` name in the histogram
	//      table.
	//   2. The OTel-hostmetrics / sqlquery emitters write the suffixed
	//      name (`system_cpu_logical_count`, `system_processes_count`,
	//      `system_filesystem_inodes_count`,
	//      `system_processes_created_count`, …) as a cumulative Sum
	//      under the suffixed name in the sum table.
	//
	// When Sum is configured and distinct from Histogram, the caller
	// fans the scan across both layouts via a UnionAll of per-arm
	// Projects (needCompanionUnion / lowerCompanionUnion), keyed off
	// companionValueColumn / companionSuffixed / companionBare below.
	// When the union path doesn't apply (no Sum table configured, or Sum
	// equals Histogram by config), the caller falls back to the
	// single-arm histogram projection — same shape as before this
	// multi-table fan-out.
	if bare, col, ok := s.HistogramCompanionColumn(metricName); ok && s.HistogramTable != "" {
		route.tables = []string{s.HistogramTable}
		route.companionValueColumn = col
		route.companionSuffixed = metricName
		route.companionBare = bare
		// The single-arm fallback rewrites matchers in-place to the bare
		// name so the legacy histogram-companion-only emit shape stays
		// byte-stable — but ONLY when there is no distinct Sum/Gauge value
		// table to scan under the literal suffixed name (else the union path
		// owns the rewrite per-arm).
		if len(literalCompanionValueTables(s)) == 0 {
			route.matchers = rewriteMetricName(matchers, bare)
		}
	}

	return route, nil
}

func lowerVectorSelector(v *parser.VectorSelector, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	metricName := metricNameFromMatchers(v.LabelMatchers)
	if metricName == "" && hasUnpinnedMetricNameMatcher(v.LabelMatchers) && s.HistogramTable != "" {
		return lowerRegexHistogramSelector(v, s, ctx)
	}
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

	// Resolve the `_bucket` / exp-histogram / classic-histogram-companion
	// routing (see resolveSelectorRouting's doc comment for the full
	// per-case rationale) and apply it to the scan tables + matchers
	// before building the LWR / range-vector pipeline below.
	route, err := resolveSelectorRouting(metricName, s, ctx, tables, v.LabelMatchers)
	if err != nil {
		return nil, err
	}
	tables = route.tables
	matchers := route.matchers
	companionValueColumn := route.companionValueColumn
	bucketSuffixed := route.bucketSuffixed
	bucketLeMatchers := route.bucketLeMatchers
	companionSuffixed := route.companionSuffixed
	companionBare := route.companionBare
	expHistogramCompanion := route.expHistogramCompanion

	// Multi-arm companion union: when both histogram + sum tables are
	// in play for a `_count` / `_sum` selector, hand off to the
	// dedicated builder which assembles the per-arm Projects, stitches
	// them with chplan.UnionAll, and wraps the union with the right
	// LWR / range-vector shape for ctx.
	if !expHistogramCompanion && needCompanionUnion(s, companionValueColumn, companionSuffixed, companionBare) {
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
	selectorInput, pred, attributesPreMerged := lowerHistogramSelectorInput(
		scan, pred, bucketSuffixed, bucketLeMatchers, companionValueColumn, s, ctx.catalog,
	)

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
	// ServiceName collapse into a single Attributes bucket, breaking
	// `sum by (service_name) (rate({__name__=~".+"}[5m]))`.
	// The Project lands between the Scan/Filter and the per-mode
	// wraps so PREWHERE-eligible matchers stay on the raw Scan
	// (the optimizer's promotion path is untouched).
	//
	// Order-of-operations for `pred`: when the augmenting Project
	// kicks in it preserves only the canonical Sample quadruple
	// (MetricName, Attributes, TimeUnix, Value) on its output side —
	// any raw scan column the matcher predicate references
	// (`ServiceName` in particular, via the `service_name` coalesce
	// chain) goes out of scope above the
	// Project. The downstream LWR / range-vector wrappers would then
	// apply the matcher Filter ON TOP of the augmented Project and CH
	// rejects the query with `Unknown expression or function
	// identifier 'ServiceName'` (HTTP 502, error 47) on shapes like
	// `topk(10, sum by (service_name) (rate({__name__=~".+",service_name="api"}[5m])))`.
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
	//
	// The guard now fires whenever the selector Project will be emitted —
	// not only for the outer-by overlay but also for the always-on
	// resource-attribute merge (which references `ResourceAttributes` and
	// drops the raw `ServiceName` column out of scope above the Project,
	// same as the outer-by case). `isBareAttributesRef` is the same
	// decision `augmentSelectorAttributes` uses, so the pred sink and the
	// Project wrap stay in lock-step.
	attrCtx := ctx
	if attributesPreMerged {
		attrCtx = ctx.withAttributesPreMerged()
	}
	if pred != nil && !isBareAttributesRef(selectorAttributesExpr(attrCtx, s), s) {
		selectorInput = &chplan.Filter{Input: selectorInput, Predicate: pred}
		pred = nil
	}
	selectorInput = augmentSelectorAttributes(selectorInput, attrCtx, s)

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
	if ctx.rangeMode() {
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
	// Metadata enumeration (/series, /labels, /label/<name>/values): full
	// [start,end] window, no LWR staleness collapse — see
	// [wrapMetadataFullRange]. Checked before the instant LWR wrap because
	// metadata lowerings run with step==0 and inRangeVector==false, so they
	// fall through to exactly this seam.
	if ctx.metadataFullRange {
		if ctx.catalog != nil {
			return wrapMetadataCatalog(selectorInput, pred, ctx.start, ctx.end, s, ctx.catalog), nil
		}
		return wrapMetadataFullRange(selectorInput, pred, ctx.start, ctx.end, s), nil
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
func wrapHistogramCompanionProject(scan *chplan.Scan, sourceColumn string, s schema.Metrics, cat *metadataCatalog) chplan.Node {
	projections := []chplan.Projection{
		{Expr: &chplan.ColumnRef{Name: s.MetricNameColumn}, Alias: s.MetricNameColumn},
	}
	// Merge resource attributes here (raw ResourceAttributes in scope on
	// the histogram Scan); the canonical quadruple this Project exposes
	// drops the raw column, so the selector seam above treats this path as
	// attributes-pre-merged. Catalog mode keeps the raw sources instead —
	// see [catalogAttributesProjections].
	projections = append(projections, selectorLabelProjections(cat, s)...)
	projections = append(
		projections,
		chplan.Projection{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}, Alias: s.TimestampColumn},
		chplan.Projection{
			Expr: &chplan.FuncCall{
				Name: "toFloat64",
				Args: []chplan.Expr{&chplan.ColumnRef{Name: sourceColumn}},
			},
			Alias: s.ValueColumn,
		},
	)
	return &chplan.Project{Input: scan, Projections: projections}
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
	// The union is needed when the suffixed name can live somewhere other than
	// the histogram companion's bare-name row — i.e. there is at least one
	// distinct Sum/Gauge value table to scan under the LITERAL suffixed name.
	// (Gauge is the standalone-`<x>_sum`-gauge case; Sum is hostmetrics.)
	return len(literalCompanionValueTables(s)) > 0
}

// literalCompanionValueTables returns the distinct value tables a
// `_count`/`_sum`-suffixed name may live in UNDER ITS LITERAL NAME — the Sum
// table (OTel-hostmetrics cumulative sums) and the Gauge table (standalone
// gauges literally named `<x>_sum`/`<x>_count`, e.g. yace CloudWatch statistic
// suffixes). The histogram table is excluded — it is the bare-name arm. Sum
// precedes Gauge for a stable union arm order; empties and duplicates drop.
func literalCompanionValueTables(s schema.Metrics) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, t := range []string{s.SumTable, s.GaugeTable} {
		if t == "" || t == s.HistogramTable {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
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
	inputs := []chplan.Node{
		buildHistogramCompanionArm(s, matchers, bareName, suffixedName, sourceColumn, ctx.catalog),
	}
	// One literal-suffixed-name arm per distinct value table the name may live
	// in: the Sum table (hostmetrics cumulative sums) and the Gauge table
	// (standalone `<x>_sum`/`<x>_count` gauges — the yace CloudWatch-suffix
	// case). Empty arms are cost-free under the per-arm MetricName filter.
	for _, t := range literalCompanionValueTables(s) {
		inputs = append(inputs, buildLiteralNameCompanionArm(s, matchers, suffixedName, t, ctx.catalog))
	}
	selectorInput := chplan.Node(&chplan.UnionAll{Inputs: inputs})
	anchor, err := selectorAnchor(v, ctx)
	if err != nil {
		return nil, err
	}
	// Each arm already merged resource attributes (the raw
	// ResourceAttributes column is dropped by the UnionAll's canonical
	// quadruple), so the post-union augment overlays only the outer-by
	// top-level columns on top of the already-merged Attributes.
	selectorInput = augmentSelectorAttributes(selectorInput, ctx.withAttributesPreMerged(), s)
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
	if ctx.rangeMode() {
		if hasAbsoluteAt(v) {
			return wrapRangeAbsoluteAtBroadcast(selectorInput, nil, anchor, ctx, s), nil
		}
		return wrapRangeLatestPerSeries(selectorInput, nil, anchor, ctx, s), nil
	}
	// Metadata enumeration over the (Gauge, Sum) union: full [start,end]
	// window, no LWR staleness collapse — same seam as the single-table
	// path above. pred is nil here (already applied per union arm).
	if ctx.metadataFullRange {
		if ctx.catalog != nil {
			return wrapMetadataCatalog(selectorInput, nil, ctx.start, ctx.end, s, ctx.catalog), nil
		}
		return wrapMetadataFullRange(selectorInput, nil, ctx.start, ctx.end, s), nil
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
// `__name__` rewrite is local to this arm — the literal-name arms
// (`buildLiteralNameCompanionArm`) see the suffixed name in matcher
// form.
func buildHistogramCompanionArm(
	s schema.Metrics, matchers []*labels.Matcher,
	bareName, suffixedName, sourceColumn string,
	cat *metadataCatalog,
) chplan.Node {
	armMatchers := rewriteMetricName(matchers, bareName)
	scan := &chplan.Scan{Table: s.HistogramTable}
	var armInput chplan.Node = scan
	if pred := buildPredicate(armMatchers, s); pred != nil {
		armInput = &chplan.Filter{Input: scan, Predicate: pred}
	}
	projections := []chplan.Projection{
		{Expr: &chplan.LitString{V: suffixedName}, Alias: s.MetricNameColumn},
	}
	// Merge resource attributes per-arm: the raw ResourceAttributes column
	// is in scope here (the arm scans the histogram table directly) but is
	// dropped from the canonical quadruple the UnionAll exposes, so the
	// post-union seam cannot reference it. Catalog mode passes the raw
	// sources through instead — see [catalogAttributesProjections].
	projections = append(projections, selectorLabelProjections(cat, s)...)
	projections = append(
		projections,
		chplan.Projection{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}, Alias: s.TimestampColumn},
		chplan.Projection{
			Expr: &chplan.FuncCall{
				Name: "toFloat64",
				Args: []chplan.Expr{&chplan.ColumnRef{Name: sourceColumn}},
			},
			Alias: s.ValueColumn,
		},
	)
	return &chplan.Project{Input: armInput, Projections: projections}
}

// buildLiteralNameCompanionArm assembles a literal-suffixed-name arm of the
// classic-histogram-companion UnionAll. The arm scans `table` with the
// MetricName filter kept on the SUFFIXED user-visible name
// (`system_cpu_logical_count`, `aws_applicationelb_request_count_sum`, etc.)
// and projects the canonical Sample-row quadruple directly. Used for BOTH the
// Sum table (OTel-hostmetrics cumulative sums under the suffixed name) and the
// Gauge table (a standalone gauge literally named `<x>_sum`/`<x>_count`). The
// Value column is already `Float64`, so no `toFloat64` cast is required (the
// histogram arm needs the cast because its Count column is UInt64).
func buildLiteralNameCompanionArm(
	s schema.Metrics, matchers []*labels.Matcher, suffixedName, table string,
	cat *metadataCatalog,
) chplan.Node {
	// Defensive: thread the suffixed name back through rewriteMetricName
	// so any non-Equal `__name__` matchers in the input list (regex
	// alternations etc.) flow unchanged and only the canonical
	// `__name__ = <suffixed>` literal is normalised. The lowering's
	// metricNameFromMatchers contract already pinned the suffixed name
	// as the canonical Equal matcher, so this is a no-op for the
	// production input shape but the helper stays robust against
	// alternate matcher shapes upstream callers might thread in.
	//
	// `table` is the SUFFIXED-name value table this arm scans — the Sum
	// table (OTel-hostmetrics cumulative sums under the suffixed name) or
	// the Gauge table (a STANDALONE gauge literally named `<x>_sum` /
	// `<x>_count`, e.g. yace CloudWatch statistic suffixes). Both store the
	// value in the canonical Value column (Float64), so no toFloat64 cast is
	// needed (unlike the histogram arm's UInt64 Count column).
	armMatchers := rewriteMetricName(matchers, suffixedName)
	scan := &chplan.Scan{Table: table}
	var armInput chplan.Node = scan
	if pred := buildPredicate(armMatchers, s); pred != nil {
		armInput = &chplan.Filter{Input: scan, Predicate: pred}
	}
	projections := []chplan.Projection{
		{Expr: &chplan.ColumnRef{Name: s.MetricNameColumn}, Alias: s.MetricNameColumn},
	}
	// Merge resource attributes per-arm (see buildHistogramCompanionArm):
	// raw ResourceAttributes is in scope on the sum table but dropped by
	// the canonical quadruple the UnionAll exposes. Catalog mode passes the
	// raw sources through instead.
	projections = append(projections, selectorLabelProjections(cat, s)...)
	projections = append(
		projections,
		chplan.Projection{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}, Alias: s.TimestampColumn},
		chplan.Projection{Expr: &chplan.ColumnRef{Name: s.ValueColumn}, Alias: s.ValueColumn},
	)
	return &chplan.Project{Input: armInput, Projections: projections}
}

// augmentSelectorAttributes wraps `input` with a Project that rebinds
// the Attributes column to `mapConcat(Attributes, <synthesised top-
// level columns>)` so every projected series carries the dedicated
// top-level OTel-CH columns the schema configures. The function returns
// `input` unchanged only when the rebind would be an identity map — a
// schema with neither a ResourceAttributes column nor a dedicated one.
//
// The Project's column shape preserves the canonical Sample-row
// quadruple (MetricName, Attributes, TimeUnix, Value) the downstream
// LWR / RangeWindow consumes. The dedicated top-level column
// (ServiceName) is read by `augmentAttributesForTopLevelExpr` from the
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
	attrsExpr := selectorAttributesExpr(ctx, s)
	if isBareAttributesRef(attrsExpr, s) {
		// No resource merge (schema cleared ResourceAttributesColumn) AND
		// no outer-by overlay — the Project would be a no-op identity, so
		// skip it to keep custom-schema-without-ResourceAttributes
		// fixtures byte-identical.
		return input
	}
	return &chplan.Project{
		Input: input,
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: s.MetricNameColumn}, Alias: s.MetricNameColumn},
			{Expr: attrsExpr, Alias: s.AttributesColumn},
			{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}, Alias: s.TimestampColumn},
			{Expr: &chplan.ColumnRef{Name: s.ValueColumn}, Alias: s.ValueColumn},
		},
	}
}

// selectorAttributesExpr returns the rebound Attributes expression for the
// selector Project: the base resource-merge
// (`mapUpdate(sanitize(ResourceAttributes), Attributes)`) with the
// dedicated top-level-column overlay (`mapConcat(<base>, mapFilter(…))`)
// composed on top. When neither applies it is the bare Attributes
// ColumnRef, and [augmentSelectorAttributes] skips the Project entirely.
//
// The overlay is unconditional because a dedicated column carries part of
// the series identity on every row, not only on rows a by-clause happens
// to group by: `service.name` is removed from the ResourceAttributes arm
// (see [dedicatedResourceKeys]) on the promise that this path re-adds it,
// so gating the overlay on the query shape dropped `service_name` from
// every bare selector's projected labels.
func selectorAttributesExpr(ctx lowerCtx, s schema.Metrics) chplan.Expr {
	base := mergeResourceAttributesExpr(s)
	if ctx.catalog != nil {
		// Catalog mode answers with ONE column resolved at the leaf, so the
		// merged map is never read: building it here would rebuild every
		// row's whole attribute map (two mapFromArrays over a per-key regex)
		// only for the catalog projection to subscript one key out of it.
		// Leaving Attributes bare also keeps the raw ResourceAttributes /
		// dedicated columns in scope for [catalogLabelValuesExpr], and keeps
		// the matcher predicate un-sunk so it lands on one Filter with the
		// metadata window bound. The dedicated-column overlay is skipped for
		// the same reason: [dedicatedLabelColumns] already resolves those
		// columns directly.
		return &chplan.ColumnRef{Name: s.AttributesColumn}
	}
	if ctx.attributesPreMerged {
		// The selector input already carries BOTH the merge and the
		// dedicated-column overlay in its Attributes column — each
		// companion-union arm folds them in via [selectorAttributesSource].
		// Re-deriving either here would reference an out-of-scope
		// ResourceAttributes / ServiceName: the arms collapse to the
		// canonical Sample quadruple, so the raw columns are gone above the
		// union.
		return &chplan.ColumnRef{Name: s.AttributesColumn}
	}
	if overlay := augmentAttributesForTopLevelExpr(s, base); overlay != nil {
		return canonicalAttributesExpr(overlay)
	}
	return canonicalAttributesExpr(base)
}

// rewriteMetricName returns a copy of matchers where the pinned
// `__name__` matcher (WireArmWireNamePinned) carries the supplied name
// in place of its original value. Used by the exp-histogram / classic
// -histogram companion routing (expHistogramSelectorRouting,
// resolveSelectorRouting, buildHistogramCompanionArm,
// buildLiteralNameCompanionArm) to redirect the scan-side `__name__`
// filter onto whichever physical table's stored name the caller has
// already resolved — the bare base name for a histogram-table arm, the
// literal suffixed name for a companion Sum/Gauge arm.
//
// This is a TABLE-ROUTING rewrite, not a wire-domain classification
// decision: unlike WireArms.ResolveName (which trims a caller-supplied
// wire suffix off a pinned matcher's existing value, or reports
// DecisionUnsatisfiable when the value doesn't carry it),
// rewriteMetricName unconditionally substitutes the caller's target
// name regardless of the matcher's original value. It only shares
// WireArms's PINNED-MATCHER CLASSIFICATION (m.Name == "__name__" &&
// m.Type == MatchEqual) — the exact predicate #1756 centralized after
// finding it reimplemented, subtly-inconsistently, at multiple call
// sites (see wireArms's doc comment). Deriving that classification from
// wireArms here, instead of re-checking m.Name/m.Type inline, is what
// #1761 migrates: this was the fourth independent reimplementation of
// that same pinned-name predicate.
//
// Non-`__name__` matchers and non-Equal `__name__` matchers (e.g.
// `__name__=~"foo|bar"`) flow through unchanged: the rewrite only
// applies to a single equality matcher, which is the only shape
// `metricNameFromMatchers` recognises in the first place.
//
// Copy-on-write semantics mirror stripBucketSuffix: a fresh slice +
// fresh matcher are allocated, the input is never mutated. The parser
// can reuse the matcher slice across lowering passes and a mutation
// here would silently bleed back into later passes.
func rewriteMetricName(matchers []*labels.Matcher, name string) []*labels.Matcher {
	out := make([]*labels.Matcher, len(matchers))
	w := wireArms(matchers)
	for i, m := range matchers {
		if w.Arms[i] == WireArmWireNamePinned && m.Value != name {
			copied, err := labels.NewMatcher(m.Type, m.Name, name)
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
// Sum — the suffix heuristic alone can't disambiguate. The caller
// always passes at least one candidate (schema.Metrics.TablesFor /
// TablesForUnknownName never return an empty slice), so a zero-length
// slice is a programmer error and fails fast rather than emitting an
// invalid empty Scan.
func scanFromTables(tables []string) *chplan.Scan {
	if len(tables) == 0 {
		panic("promql: scanFromTables called with no candidate tables")
	}
	if len(tables) == 1 {
		return &chplan.Scan{Table: tables[0]}
	}
	// Defensive copy: the caller's slice may be a return from
	// schema.Metrics.TablesFor whose backing array is shared with
	// the schema. A downstream optimizer pass that wanted to
	// in-place mutate UnionTables would corrupt the schema; the
	// copy keeps the plan-tree slice independent.
	owned := append([]string(nil), tables...)
	return &chplan.Scan{UnionTables: owned}
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

// wrapMetadataFullRange filters (scan, pred) to the closed [start,end]
// request window and collapses to one row per `(MetricName, Attributes)`
// series via `any()`. It is the metadata-endpoint analogue of
// [wrapInstantLatestPerSeries]: same canonical 4-column Sample output
// shape, but with two deliberate differences that implement metadata
// enumeration semantics instead of instant-query semantics —
//
//   - WINDOW: a closed `Timestamp >= start AND Timestamp <= end` filter
//     (each bound omitted when zero — a no-bound side scans the whole
//     table, matching reference Prometheus's min/max-retention default),
//     NOT the instant `(end - 5m, end]` LWR + staleness window. A series
//     whose only sample sits early in [start,end] must still surface.
//   - COLLAPSE: `any(TimeUnix)` / `any(Value)` per series — we only need
//     existence-of-a-series, not the latest-per-series value — so there
//     is no `argMax`/`max` LWR pick. The downstream /series dedup and the
//     /labels + /label/<name>/values DISTINCT projections fold the
//     one-row-per-series output exactly as they did the instant shape.
//
// Collapsing here (rather than streaming every in-window sample) bounds
// the row count to the number of distinct series, keeping a wide
// metadata window cheap on the wire.
func wrapMetadataFullRange(scan chplan.Node, pred chplan.Expr, start, end time.Time, s schema.Metrics) chplan.Node {
	input := scan
	if combined := metadataWindowPredicate(pred, start, end, s); combined != nil {
		input = &chplan.Filter{Input: scan, Predicate: combined}
	}

	const (
		metaTsAlias    = "meta_ts"
		metaValueAlias = "meta_value"
	)
	agg := &chplan.Aggregate{
		Input: input,
		GroupBy: []chplan.Expr{
			&chplan.ColumnRef{Name: s.MetricNameColumn},
			&chplan.ColumnRef{Name: s.AttributesColumn},
		},
		GroupByAliases: []string{s.MetricNameColumn, s.AttributesColumn},
		AggFuncs: []chplan.AggFunc{
			{Name: "any", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.TimestampColumn}}, Alias: metaTsAlias},
			{Name: "any", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.ValueColumn}}, Alias: metaValueAlias},
		},
	}
	return &chplan.Project{
		Input: agg,
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: s.MetricNameColumn}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: &chplan.ColumnRef{Name: metaTsAlias}, Alias: s.TimestampColumn},
			{Expr: &chplan.ColumnRef{Name: metaValueAlias}, Alias: s.ValueColumn},
		},
	}
}

// metadataWindowPredicate ANDs the closed `[start,end]` metadata window
// onto pred. Each bound is omitted when zero — a no-bound side scans the
// whole table, matching reference Prometheus's min/max-retention default.
// Returns nil when there is nothing to filter on (no matchers, no window),
// so callers emit no Filter at all.
//
// Shared by [wrapMetadataFullRange] and [wrapMetadataCatalog] so the two
// metadata seams can never drift on window semantics.
func metadataWindowPredicate(pred chplan.Expr, start, end time.Time, s schema.Metrics) chplan.Expr {
	combined := pred
	addBound := func(b chplan.Expr) {
		if combined == nil {
			combined = b
			return
		}
		combined = &chplan.Binary{Op: chplan.OpAnd, Left: combined, Right: b}
	}
	if !start.IsZero() {
		addBound(&chplan.Binary{
			Op:    chplan.OpGe,
			Left:  &chplan.ColumnRef{Name: s.TimestampColumn},
			Right: metadataBoundExpr(start),
		})
	}
	if !end.IsZero() {
		addBound(&chplan.Binary{
			Op:    chplan.OpLe,
			Left:  &chplan.ColumnRef{Name: s.TimestampColumn},
			Right: metadataBoundExpr(end),
		})
	}
	return combined
}

// metadataBoundExpr renders a metadata window bound as a
// `toDateTime64('YYYY-MM-DD HH:MM:SS.fffffffff', 9)` literal — the same
// literal shape [anchorBaseExpr] emits for an absolute eval anchor, so
// the metadata window bounds read identically to the instant-query
// bounds in emitted SQL.
func metadataBoundExpr(t time.Time) chplan.Expr {
	return &chplan.FuncCall{
		Name: "toDateTime64",
		Args: []chplan.Expr{
			&chplan.LitString{V: t.UTC().Format("2006-01-02 15:04:05.000000000")},
			&chplan.LitInt{V: chplan.NanoScale},
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

	// BOOT-WIRED native dispatch (PURE polymorphic — no branching here): hand
	// the resolved staleness input to the boot-wired staleness strategy. The
	// decision of WHETHER the native timeSeriesResampleToGridWithStaleness path
	// is active was made ONCE at boot (the ts_grid_resample feature) and is
	// encoded in the injected ctx.lowerers.Staleness impl — there is NO
	// feature-flag / version read AND NO nil/presence check here. The strategy
	// ALWAYS returns a valid lowering: the native impl emits the resample node,
	// the concrete fan-out impl emits the RangeLWR. Both produce the IDENTICAL
	// canonical 4-column Sample row shape (proven on the chDB substrate by the
	// resample dual-emit parity test), so the surrounding plan tree is
	// unaffected by which strategy is wired.
	return ctx.lowerers.Staleness.LowerStaleness(stalenessLowerInput{
		input:         rawSide,
		start:         ctx.start.UTC(),
		end:           ctx.end.UTC(),
		step:          ctx.step,
		lookback:      instantLookback,
		offset:        anchor.Offset,
		stepAligned:   ctx.stepAligned,
		metricNameCol: s.MetricNameColumn,
		attributesCol: s.AttributesColumn,
		timestampCol:  s.TimestampColumn,
		valueCol:      s.ValueColumn,
	})
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
			{Expr: &chplan.ColumnRef{Name: chplan.RangeWindowAnchorColumn}, Alias: s.TimestampColumn},
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

// RewriteMetricName is the exported wrapper around [rewriteMetricName] for
// callers outside the promql package — the `/api/v1/query_exemplars`
// handler in internal/api/prom, which retargets a companion selector at
// the bare-named histogram row exactly the way the sample lowering does.
//
// Returns a copy: the input matcher slice is never mutated, and matchers
// other than the pinned `__name__` equality pass through untouched.
func RewriteMetricName(matchers []*labels.Matcher, name string) []*labels.Matcher {
	return rewriteMetricName(matchers, name)
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
//  3. A non-service, non-`__name__` label when the schema names a
//     ResourceAttributes column (and the label is allowlisted, if an
//     allowlist is configured) — resolves against BOTH the metric
//     Attributes map AND the ResourceAttributes map, Attributes winning
//     on collision. See [resourceMatcherFallback] / the branch-3 comment
//     below for the coalesce-over-nullIf precedence + negative-matcher
//     emptiness floor.
//
//  4. Anything else — falls through to the Attributes-map lookup
//     (with the dot/underscore candidate expansion documented on
//     [attributeLookup]).
func matcherToExpr(m *labels.Matcher, s schema.Metrics) chplan.Expr {
	if m.Name == model.MetricNameLabel {
		return metricNamePredicate(m, s)
	}
	return &chplan.Binary{
		Op:    matchOp(m.Type),
		Left:  rawLabelValueExpr(s, m.Name),
		Right: &chplan.LitString{V: m.Value},
	}
}

// rawLabelValueExpr resolves the Prom label promLabel to the expression
// that yields ITS VALUE on a RAW metric row — branches 2 to 4 of
// [matcherToExpr]'s routing, factored out so the matcher predicate and the
// label-values catalog projection ([catalogLabelValuesExpr]) resolve a
// label the SAME way. Any divergence between the two would let the catalog
// advertise values no selector can match, or hide values a selector does
// match.
//
// The row is raw: the caller must not have replaced Attributes with the
// merged/renamed read-path projection, because this expression reads the
// dedicated column and the ResourceAttributes map directly.
//
// `__name__` is NOT handled here — it resolves against the MetricName
// column and, in matcher position, carries its own dotted-candidate
// fan-out ([metricNamePredicate]).
//
// The merged-row counterpart is [mergedLabelValueExpr].
func rawLabelValueExpr(s schema.Metrics, promLabel string) chplan.Expr {
	var lhs chplan.Expr
	mapLookup := attributeLookup(s.AttributesColumn, promLabel)
	if col := schemaTopLevelColumn(s, promLabel); col != "" {
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
	} else if resArm := resourceMatcherFallback(s, promLabel); resArm != nil {
		// BRANCH 3 — Attributes ∪ ResourceAttributes, Attributes-win.
		//
		//   coalesce(nullIf(Attributes[cands], ''),
		//            nullIf(ResourceAttributes[cands], ''),
		//            '')
		//
		// The metric-level Attributes map is arg 0, so a non-empty
		// Attributes value shadows the resource value — byte-for-byte the
		// same precedence the read-path projection's
		// `mapUpdate(sanitize(ResourceAttributes), Attributes)` produces.
		//
		// Each map side is `nullIf(<lookup>, '')` because CH's
		// `Map(String,String)['missing']` returns the empty-string default
		// (not NULL); without nullIf, coalesce would always stop at the
		// Attributes arm and never consult ResourceAttributes. The trailing
		// `''` re-floors the LHS so "absent in BOTH maps" yields the empty
		// string (not NULL): a negative matcher `{env!="prod"}` must KEEP a
		// row that has no `env` at all (Prom "absent label → empty string"),
		// and CH three-valued logic would otherwise drop the NULL row.
		args := []chplan.Expr{
			&chplan.FuncCall{
				Name: "nullIf",
				Args: []chplan.Expr{mapLookup, &chplan.LitString{V: ""}},
			},
			resArm,
		}
		// The dot/underscore candidate chain inside resArm cannot reach a
		// configured resource key whose Prom spelling differs from it by any
		// OTHER separator (`a-b`, `a b`) — those keys are only discoverable
		// by inverting the sanitisation over the configured allowlist, which
		// [configuredResourceKeysFor] does exactly. Each extra key becomes
		// one more coalesce arm AFTER the candidate chain, so an allowlist
		// entry never displaces a spelling the chain already resolves.
		for _, k := range configuredResourceKeysFor(s, promLabel) {
			args = append(args, &chplan.FuncCall{
				Name: "nullIf",
				Args: []chplan.Expr{
					&chplan.MapAccess{
						Map: &chplan.ColumnRef{Name: s.ResourceAttributesColumn},
						Key: &chplan.LitString{V: k},
					},
					&chplan.LitString{V: ""},
				},
			})
		}
		lhs = &chplan.FuncCall{Name: "coalesce", Args: append(args, &chplan.LitString{V: ""})}
	} else {
		lhs = mapLookup
	}
	return lhs
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
	return metricNamePredicateOn(m, s, func() chplan.Expr {
		return &chplan.ColumnRef{Name: s.MetricNameColumn}
	})
}

// metricNamePredicateOn is [metricNamePredicate] with the name-bearing
// expression injected, for callers whose rows expose the metric name
// somewhere other than the raw MetricName column — notably the
// classic-histogram quantile paths, whose Prometheus wire name is the
// synthetic `concat(MetricName, '_bucket')` ladder rather than the bare
// stored name (see [histogramQuantileMatcherPredicate]).
//
// nameExpr MUST mint a fresh node per call: the regex branch places the
// name in two independent plan positions (raw + Prom-normalised) and
// chplan trees are walked and rewritten in place, so a shared pointer
// would alias them.
func metricNamePredicateOn(m *labels.Matcher, s schema.Metrics, nameExpr func() chplan.Expr) chplan.Expr {
	single := &chplan.Binary{
		Op:    matchOp(m.Type),
		Left:  nameExpr(),
		Right: &chplan.LitString{V: m.Value},
	}
	if m.Type == labels.MatchRegexp || m.Type == labels.MatchNotRegexp {
		normalized := &chplan.Binary{
			Op: matchOp(m.Type),
			Left: &chplan.FuncCall{
				Name: "replaceRegexpAll",
				Args: []chplan.Expr{
					nameExpr(),
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
		Left:    nameExpr(),
		List:    list,
		Negated: m.Type == labels.MatchNotEqual,
	}
}

// attributeLookup returns the chplan.Expr that resolves a Prom matcher
// name `key` against the CH Map column `col`. For names with no
// rewritable underscore (e.g. `job`, `__name__`) it returns a plain
// MapAccess — the byte-stable emit shape the fixtures match.
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
// attributeLookupKeys returns the storage keys [attributeLookup] probes
// for the Prom label `key`, in probe order. Callers that need to know
// which spellings the chain already covers (see
// [configuredResourceKeysFor]) read it instead of re-deriving the rule.
func attributeLookupKeys(key string) []string {
	if !format.PromLabelNeedsDottedFallback(key) {
		return []string{key}
	}
	candidates := format.PromLabelToOTelCandidates(key)
	if len(candidates) <= 1 {
		return []string{key}
	}
	return candidates
}

func attributeLookup(col, key string) chplan.Expr {
	return attributeLookupExpr(&chplan.ColumnRef{Name: col}, key)
}

// attributeLookupExpr is attributeLookup over an already-shaped label map.
// Histogram paths group directly over their scans, so they use this form to
// resolve labels from the ResourceAttributes-merged series identity.
func attributeLookupExpr(m chplan.Expr, key string) chplan.Expr {
	if !format.PromLabelNeedsDottedFallback(key) {
		return &chplan.MapAccess{
			Map: m,
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
			Map: m,
			Key: &chplan.LitString{V: key},
		}
	}
	return qlcommon.OTelDottedFallbackChain(m, candidates)
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
	// The switch covers all four labels.MatchType values; a new one added
	// upstream surfaces here as a parser divergence rather than silently
	// degrading to Equal.
	panic(fmt.Sprintf("promql: unexpected match type %v", t))
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
	// "is c.Args[0] a MatrixSelector?" check below — including the
	// subquery form `quantile_over_time(phi, rate(m[5m])[1h:5m])`, whose
	// range-vector arg is a SubqueryExpr rather than a MatrixSelector and
	// so needs its own outer-reducer dispatch (#1456).
	if c.Func.Name == "quantile_over_time" {
		if len(c.Args) == 2 {
			if sq, ok := c.Args[1].(*parser.SubqueryExpr); ok {
				return lowerOuterRangeFnOverSubquery(c, sq, s, ctx)
			}
		}
		return lowerQuantileOverTime(c, s, ctx)
	}
	if len(c.Args) >= 1 {
		// Parentheses are transparent grouping in PromQL — peel them (and
		// any step-invariant wrapper) before deciding the argument's
		// shape, so `rate((m[5m]))` dispatches identically to `rate(m[5m])`.
		arg0 := peelWrappers(c.Args[0])
		if _, ok := arg0.(*parser.MatrixSelector); ok {
			return lowerRangeVectorCall(c, s, ctx)
		}
		if sq, ok := arg0.(*parser.SubqueryExpr); ok {
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
	case "histogram_quantiles":
		return lowerHistogramQuantiles(c, s, ctx)
	case "histogram_count", "histogram_sum", "histogram_avg",
		"histogram_stddev", "histogram_stdvar", "histogram_fraction":
		return lowerHistogramValueFn(c, s, ctx)
	case "label_replace":
		return lowerLabelReplace(c, s, ctx)
	case "label_join":
		return lowerLabelJoin(c, s, ctx)
	case "info":
		return lowerInfo(c, s, ctx)
	case "time":
		return lowerTime(c, s, ctx)
	case "vector":
		return lowerVector(c, s, ctx)
	case "year", "month", "day_of_month", "day_of_week", "day_of_year",
		"days_in_month", "hour", "minute", "timestamp":
		return lowerDateFn(c, s, ctx)
	case "sort", "sort_desc":
		return lowerSort(c, s, ctx)
	case "sort_by_label", "sort_by_label_desc":
		return lowerSortByLabel(c, s, ctx)
	case "scalar":
		return lowerScalarTopLevel(c, s, ctx)
	case "range", "step":
		// Query-context functions: their value is constant for a given
		// query execution (it depends only on the eval range, not on
		// series data). The reference engine constant-folds these into
		// NumberLiterals before evaluation; cerberus folds them at
		// lowering into a synthetic scalar vector, mirroring `time()` /
		// `vector(N)`. See [lowerQueryContextFold].
		//
		// Only `range()` and `step()` are folded as top-level scalar
		// calls. `start()` / `end()` are NOT callable functions in this
		// position — upstream's parser only admits them inside an `@`
		// modifier (`up @ start()`), which lowers through the at-modifier
		// path, not here. Lowering bare `start()` / `end()` as scalar
		// calls would accept a shape stock reference Prometheus rejects.
		return lowerQueryContextFold(c, s, ctx)
	case "start", "end":
		// `start()` / `end()` are query-context time anchors, not
		// standalone callable functions. Upstream's parser admits them
		// only inside an `@` modifier (`up @ start()`, `up @ end()` —
		// both supported, lowered via the at-modifier path); the bare
		// top-level call is invalid PromQL that stock reference
		// Prometheus rejects. Cerberus rejects it identically — a
		// permanent parity decision, not a coverage gap.
		return nil, fmt.Errorf("promql: function %s() is only valid inside an @ modifier (e.g. up @ %s()), not as a standalone call", c.Func.Name, c.Func.Name)
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
	return nil, fmt.Errorf("promql: function %s is not a recognized lowering target", c.Func.Name)
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
		// `double_exponential_smoothing` (and its legacy `holt_winters`
		// alias) applies Holt-Winters double-exponential smoothing over
		// the lookback window. The chsql emitter renders the sequential
		// recurrence as an `arrayFold` over the windowed value array and
		// is verified reference-exact against
		// prometheus/promql/functions.go::funcDoubleExponentialSmoothing
		// (incl. the calcTrendValue seeding). `lowerHoltWinters`
		// enforces the (0,1) smoothing/trend-factor guards and emits the
		// canonical "holt_winters" IR Func regardless of which spelling
		// the query used.
		return lowerHoltWinters(c, s, ctx)
	case "absent_over_time":
		return lowerAbsentOverTime(c, s, ctx)
	}
	// `first_over_time(v[range])` — the time-EARLIEST sample value in the
	// window, mirroring last_over_time exactly. It flows through the
	// generic RangeWindow over-time path below: the shared chsql over-time
	// emitter's `first_over_time` reducer returns `window_vals[1]` (the
	// arrayBy (ts, value)-sorted earliest element), reference-exact against
	// prometheus/promql/functions.go::funcFirstOverTime. Like
	// last_over_time it preserves `__name__` via the
	// wrapRangeWindowPreserveName special-case below.
	if len(c.Args) != 1 {
		return nil, fmt.Errorf("promql: %s expects exactly 1 argument, got %d", c.Func.Name, len(c.Args))
	}
	arg := peelWrappers(c.Args[0])
	ms, ok := arg.(*parser.MatrixSelector)
	if !ok {
		return nil, fmt.Errorf("promql: %s argument must be a range-vector selector, got %T",
			c.Func.Name, arg)
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
	// Name-drop collision guard. When the function drops `__name__` and the
	// selector spans several metrics, two source series that differ only by
	// name land on one label set — the case reference Prometheus refuses
	// (engine.go:2295) and cerberus used to answer with a silently merged
	// series. Widening the window's grouping key with MetricName is what
	// makes the collision observable at all; the wrap below re-collapses the
	// widened rows and aborts on any group that held more than one name.
	// This runs BEFORE the shape switch so the fan-out, native and
	// `@`-broadcast lowerings all inherit the widened key.
	guardNameCollision := rangeFnCollidesOnNameDrop(c.Func.Name, vs.LabelMatchers, inner, s)
	if guardNameCollision {
		appendNameGroupKey(rw, s)
	}
	shape := rangeGridShapeFor(vs, ctx)
	// node is the lowering that flows to the name-preservation seam below.
	// Outside range mode it is the fan-out RangeWindow rw; in plain range mode
	// the boot-wired rate strategy re-derives it (native or fan-out). The
	// name-preservation wrap keys off c.Func.Name (PromQL semantics), never off
	// the dispatch, so the two concerns compose without a dispatch-site branch.
	node := chplan.Node(rw)
	switch shape {
	case gridBroadcast:
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
		var nameExpr chplan.Expr
		if rangeFnPreservesName(c.Func.Name) {
			nameExpr = preservedNameExpr(rw, vs.LabelMatchers, s)
		} else if guardNameCollision {
			// The broadcast Project has to carry the widened key out of the
			// pinned window, otherwise the guard below has no name column to
			// count. It is blanked again by the guard's own projection, so the
			// wire shape is unchanged.
			nameExpr = &chplan.ColumnRef{Name: s.MetricNameColumn}
		}
		broadcast := wrapRangeWindowAtBroadcast(rw, ctx, s, nameExpr, &chplan.ColumnRef{Name: s.ValueColumn})
		if guardNameCollision {
			// The broadcast relation is matrix-shaped whatever rw is: the
			// CrossJoin fans the single pinned value across the step grid and
			// projects `anchor_ts` from the grid side, already unshifted.
			return wrapDropNameCollisionGuard(broadcast, s,
				&chplan.ColumnRef{Name: chplan.RangeWindowAnchorColumn},
				&chplan.ColumnRef{Name: s.ValueColumn}), nil
		}
		return broadcast, nil
	case gridFanout:
		// In range mode, fan the range function across the request's step
		// grid: each anchor in [start, end] (spaced by step) emits one row
		// per series with the per-anchor function value. The emitter
		// already supports this via OuterRange + Step (the matrix path used
		// by subqueries); we just need to flip the switch when LowerAtRange
		// threaded a non-zero step. Without this, `rate(m[5m])` over
		// query_range degenerates to a single anchor at end_ts and the
		// matrix pivot only sees one sample per series — the same root
		// cause as the bare-selector range-mode bug Pool-AK is fixing.
		applyStepGridFanout(rw, ctx)

		// BOOT-WIRED native dispatch (PURE polymorphic — no branching here):
		// hand the fan-out RangeWindow to the boot-wired rate strategy. The
		// decision of WHETHER the native path is active was made ONCE at boot
		// (the ts_grid_range feature) and is encoded in the injected
		// ctx.lowerers.Rate impl — there is NO feature-flag / version read AND
		// NO nil/presence check here. The strategy ALWAYS returns a valid
		// lowering: the native impl emits timeSeriesRateToGrid for a
		// shape-eligible rate window (rate func, materialised grid, plain
		// Scan/Filter input) and delegates to its embedded fan-out fallback for
		// any other shape; the fan-out impl returns this RangeWindow unchanged.
		// All intrinsic SHAPE / AST-node dispatch lives INSIDE the impl. The
		// native node carries the same Func/Range/Step/Start/End/Offset/columns/
		// GroupBy as the fan-out RangeWindow — only the emitter differs — and
		// produces the identical per-(series, anchor) row shape (proven
		// byte-identical on the chDB substrate; see
		// test/spec/promql/native_rate_range_step.txtar and the dual-emit
		// parity test).
		//
		// For a non-rate window (increase / delta / *_over_time) the strategy
		// returns rw unchanged, so node stays rw and the last/first_over_time
		// name-preservation wrap below applies exactly as before. For rate the
		// returned node IS the lowering (native or fan-out RangeWindow); rate
		// drops `__name__`, so it never matches the name-preservation wrap and
		// flows through as-is.
		//
		// The function family is selected by c.Func.Name — pure AST/func
		// dispatch, NOT a feature/version branch (that decision is baked into
		// WHICH concrete strategy boot wired into each field). Each strategy is
		// always non-nil (withDefaults), always returns a valid lowering, and
		// keeps its own intrinsic shape-eligibility inside the impl. rate /
		// changes / resets each route to their own boot-wired strategy; every
		// other range fn (increase / delta / *_over_time / ...) keeps the fan-out
		// rw via the rate strategy's pass-through (those funcs have no native
		// timeSeries*ToGrid aggregate proven equivalent yet).
		switch c.Func.Name {
		case "changes":
			node = ctx.lowerers.Changes.LowerChanges(rw, s)
		case "resets":
			node = ctx.lowerers.Resets.LowerResets(rw, s)
		case "deriv":
			node = ctx.lowerers.Deriv.LowerDeriv(rw, s)
		default:
			node = ctx.lowerers.Rate.LowerRate(rw, s)
		}
	case gridSingleAnchor:
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
	// A single `__name__="x"` equality matcher pins one name for every
	// row, so the projection can use a literal. A regex matcher
	// (`__name__=~"foo|bar"`) spans several metrics whose names differ
	// per series, so `preservedNameExpr` threads MetricName through the
	// window's grouping key instead.
	if rangeFnPreservesName(c.Func.Name) {
		return wrapRangeWindowPreserveName(rw, s, preservedNameExpr(rw, vs.LabelMatchers, s)), nil
	}
	if guardNameCollision {
		return wrapDropNameCollisionGuard(node, s, dropNameGuardAnchor(node, rw),
			&chplan.ColumnRef{Name: s.ValueColumn}), nil
	}
	return node, nil
}

// nativeTSGridRateNode returns a chplan.RangeWindowNative when rw is a
// SHAPE-eligible `rate(<counter>[<range>])` query_range RangeWindow; otherwise
// nil (the NativeRateLowerer then delegates to its embedded fan-out fallback).
// This is the intrinsic query-shape eligibility predicate ONLY — it reads NO
// feature flag or server version. The boot decision of whether the native path
// is active lives in WHICH strategy cmd/cerberus wired (NativeRateLowerer vs
// FanoutRateLowerer); this function is a pure shape classifier called only from
// inside NativeRateLowerer.LowerRate, so it stays on the per-query path under
// the no-feature-branch rule. The predicate is intentionally narrow — every clause
// that fails sends the query down the unchanged fan-out path:
//
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
//     row-shape relation timeSeriesRateToGrid consumes — optionally
//     wrapped in the canonical selector-attributes Project (the always-on
//     resource-attribute merge / outer-by overlay). That Project re-exposes
//     exactly the `(MetricName, Attributes, TimeUnix, Value)` quadruple the
//     native emitter reads from its `FROM (<input>)` subquery, so it is
//     transparent for eligibility. Inputs that route through
//     MetricsAggregate / MetricsHistogramOverTime / MetricsCompare keep
//     their own emit branches.
//
// The OuterRange field is intentionally NOT copied: it is a fan-out-only
// emit knob (the matrix anchor span) that the native grid encodes
// directly via Start/End/Step.
//
// recollapse carries the boot-resolved ts_grid_recollapse decision (see
// NativeRateLowerer.Recollapse): when set, an eligible input additionally has
// its label-shaping Project deferred past the aggregate. It is threaded rather
// than read here for the same reason the native/fan-out choice is — the
// per-query path reads no feature flag.
func nativeTSGridRateNode(rw *chplan.RangeWindow, s schema.Metrics, recollapse bool) *chplan.RangeWindowNative {
	return nativeTSGridMatrixNode(rw, "rate", s, recollapse)
}

// nativeTSGridMatrixNode returns a chplan.RangeWindowNative when rw is a
// SHAPE-eligible query_range matrix-function RangeWindow whose Func matches
// wantFunc; otherwise nil (the calling Native*Lowerer then delegates to its
// embedded fan-out fallback). It is the generalisation of the rate-only
// predicate to the whole timeSeries*ToGrid matrix family — rate, changes,
// resets — which share ONE node shape (RangeWindowNative) and ONE eligibility
// contract: the only per-func difference is the aggregate-name triple the
// emitter selects from chsql.nativeTSGridFn off RangeWindowNative.Func (the
// plain ToGrid name plus the -State/-Merge pair the deferred-shaping shape
// needs).
//
// Like the rate predicate it reads NO feature flag or server version — the
// boot decision of whether the native path is active lives in WHICH strategy
// cmd/cerberus wired (Native*Lowerer vs Fanout*Lowerer); this is a pure shape
// classifier called only from inside a Native*Lowerer. The clauses, every one
// of which sends the query down the unchanged fan-out path on failure:
//
//   - rw.Func must equal wantFunc. The caller passes its own family token
//     ("rate" / "changes" / "resets"), so a rate strategy never claims a
//     changes window and vice versa. The 4th aggregate param binds to the
//     PromQL matrix [range] (rw.Range) for ALL three (NOT a 5m staleness
//     lookback — that is the bare-selector resample shape, a different node).
//   - The window must be the materialised range grid: Step > 0 and both Start
//     and End pinned.
//   - rw.Identity must be false (the bare-vector subquery no-op path) and
//     rw.Input must be a plain Scan / Filter, optionally wrapped in the
//     canonical selector-attributes Project — the row-shape relation the
//     native emitter consumes.
//
// The OuterRange field is intentionally NOT copied: it is a fan-out-only emit
// knob (the matrix anchor span) that the native grid encodes directly via
// Start/End/Step.
//
// recollapse is the boot-resolved ts_grid_recollapse decision. This function is
// the ONLY construction site for RangeWindowNative, which makes it the single
// eligibility funnel for the deferred label-shaping shape: with recollapse set
// AND the input carrying a hoistable shaping Project, the node additionally
// gets Recollapse + a widened raw GroupBy (see [hoistShaping]). Every miss
// keeps the unchanged two-level node — the hoist is purely additive, and never
// falls back to "no native node at all".
func nativeTSGridMatrixNode(rw *chplan.RangeWindow, wantFunc string, s schema.Metrics, recollapse bool) *chplan.RangeWindowNative {
	if rw.Func != wantFunc {
		return nil
	}
	if rw.Identity || rw.Step <= 0 || rw.Start.IsZero() || rw.End.IsZero() {
		return nil
	}
	if !isNativeRateInput(rw.Input, s) {
		return nil
	}
	input, groupBy := rw.Input, rw.GroupBy
	var recollapseProjections []chplan.Projection
	if recollapse {
		if hoisted, projections, rawGroupBy, ok := hoistShaping(rw, s); ok {
			input, recollapseProjections, groupBy = hoisted, projections, rawGroupBy
		}
	}
	return &chplan.RangeWindowNative{
		Input:           input,
		Func:            rw.Func,
		Range:           rw.Range,
		Step:            rw.Step,
		Start:           rw.Start,
		End:             rw.End,
		Offset:          rw.Offset,
		TimestampColumn: rw.TimestampColumn,
		ValueColumn:     rw.ValueColumn,
		GroupBy:         groupBy,
		// Recollapse is empty unless the shaping hoist fired, and an empty
		// Recollapse emits byte-for-byte the pre-hoist two-level shape.
		Recollapse: recollapseProjections,
		// Scalars carries predict_linear's literal horizon t (empty for
		// rate/changes/resets/deriv). The native emitter threads it into
		// timeSeriesPredictLinearToGrid's 5th parametric arg; the caller
		// (NativePredictLinearLowerer) gates eligibility to a whole-second
		// literal before reaching here, so any element present is native-safe.
		Scalars: rw.Scalars,
	}
}

// isNativeRateInput reports whether n is an input the native
// timeSeriesRateToGrid emitter can consume directly. That is a plain
// Scan / Filter chain ([isPlainScanFilter]) OPTIONALLY wrapped in the
// canonical selector-attributes Project ([isCanonicalSampleProject]) —
// the always-on resource-attribute merge / outer-by overlay
// [augmentSelectorAttributes] inserts above the Filter. The native
// emitter reads its input as `FROM (<input>)` selecting the canonical
// `(MetricName, Attributes, TimeUnix, Value)` aliases, so that Project is
// transparent: peeling it for the eligibility check keeps the native path
// firing once the resource arm is active, instead of silently regressing
// the query to the heavier arrayJoin fan-out (the experimental flag's
// whole point is the lighter native aggregate).
func isNativeRateInput(n chplan.Node, s schema.Metrics) bool {
	if p, ok := n.(*chplan.Project); ok && isCanonicalSampleProject(p, s) {
		n = p.Input
	}
	return isPlainScanFilter(n)
}

// isCanonicalSampleProject reports whether p is the canonical
// selector-attributes Project: exactly four projections aliased to the
// schema's MetricName / Attributes / Timestamp / Value columns (the
// quadruple [augmentSelectorAttributes] and the histogram-companion
// projects emit). Only the alias SET is checked — not the Attributes
// expression — because the native emitter consumes the column by NAME
// from its subquery regardless of how the map was rebound (bare column,
// resource merge, or outer-by overlay).
func isCanonicalSampleProject(p *chplan.Project, s schema.Metrics) bool {
	if len(p.Projections) != 4 {
		return false
	}
	want := map[string]struct{}{
		s.MetricNameColumn: {},
		s.AttributesColumn: {},
		s.TimestampColumn:  {},
		s.ValueColumn:      {},
	}
	for _, proj := range p.Projections {
		if _, ok := want[proj.Alias]; !ok {
			return false
		}
		delete(want, proj.Alias)
	}
	return len(want) == 0
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

// noRecollapse spells out the [nativeTSGridMatrixNode] recollapse argument for
// the range functions with no -State/-Merge pair proven exact under merged
// partial states (see chsql.nativeTSGridFn): only rate is re-collapse-eligible
// today, so every other family passes this rather than a bare false.
const noRecollapse = false

// hoistShaping attempts the deferred-label-shaping rewrite on rw: it peels the
// label-shaping Project off rw.Input so the shaping tower can be re-evaluated
// once per raw SERIES above the aggregate instead of once per raw ROW beneath
// it, returning the peeled input, the deferred projections, and the widened raw
// GroupBy the three-level emit needs.
//
// The peel is re-validated with [isNativeRateInput] because peeling changes
// what the input chain IS: that predicate accepts a Scan / Filter chain wrapped
// in AT MOST ONE canonical Project, so a chain carrying a second relation under
// the shaping Project (an intervening Limit, or a nested Project) reads as
// eligible before the peel and ineligible after it, and handing the emitter a
// relation it cannot read is a production 502.
//
// Today the caller's own [isNativeRateInput] check IMPLIES this one: the only
// Project it tolerates is the canonical one, which is the same Project the
// classifier peels. The re-validation is the coupling guard for that
// implication, which nothing else enforces — the two predicates accept
// deliberately different sets (the classifier does not require the canonical
// four-alias set, only an identity timestamp/value pass-through), so widening
// either one would otherwise silently break it.
func hoistShaping(rw *chplan.RangeWindow, s schema.Metrics) (input chplan.Node, recollapse []chplan.Projection, rawGroupBy []chplan.Expr, ok bool) {
	hoisted, projections, raw, ok := hoistableShapingProject(rw.Input, rw.GroupBy, rw.TimestampColumn, rw.ValueColumn)
	if !ok {
		return nil, nil, nil, false
	}
	if !isNativeRateInput(hoisted, s) {
		return nil, nil, nil, false
	}
	return hoisted, projections, raw, true
}

// hoistableShapingProject classifies in as a label-shaping Project whose
// per-row work can be deferred past a native grid aggregate, returning the
// relation BELOW it, the shaping expressions to re-evaluate above the aggregate
// (each aliased to the output column name it replaces), and the RAW grouping
// keys the aggregate must group on instead of the shaped ones.
//
// The rewrite is only value-preserving under a narrow set of conditions, and
// every one of them is a REFUSAL rather than a repair — the caller keeps the
// unchanged two-level shape, which is correct at the old cost:
//
//   - in is a Project with projections and NO Replacements. Replacements
//     rebind columns in place on the PASS-THROUGH row shape and are only
//     meaningful when Projections is empty (see chplan.Project), so a node
//     carrying both is malformed — refuse rather than guess which slot names
//     the row shape.
//   - Every groupBy entry is a plain ColumnRef naming one of the Project's
//     aliases, and no key repeats. A computed key, or a key the Project does
//     not define, cannot be split into a shaped half and a raw half.
//   - timestampCol and valueCol pass through as EXACT identity ColumnRefs.
//     This is the load-bearing discriminator against the histogram-companion
//     Project ([wrapHistogramCompanionProject]), which emits the same four
//     canonical aliases over the same shaping tower but derives Value as
//     `toFloat64(<Count>)`: hoisting that one would strip both the cast and
//     the rename, feeding the aggregate a column that is not there.
//   - At least one group key is genuinely shaped (its Project expression is
//     not an identity ColumnRef) and the shaping reads at least one column.
//     There is nothing to defer otherwise, and a constant tower would
//     re-collapse every series onto a single key.
//   - No pass-through key is read by the shaping. A group key is classified as
//     a shaping INPUT the moment some deferred expression reads it
//     (chplan.RangeWindowNative.PartitionRecollapseGroupBy), and shaping inputs
//     never surface above the merge level, so such a key would silently drop out
//     of the output series identity.
//
// The returned rawGroupBy lists the pass-through keys first (in groupBy order),
// then the raw columns the shaping reads in first-seen order. The output column
// NAME set is therefore unchanged by the hoist; only the internal key ORDER can
// differ from the pre-hoist one.
func hoistableShapingProject(in chplan.Node, groupBy []chplan.Expr, timestampCol, valueCol string) (input chplan.Node, recollapse []chplan.Projection, rawGroupBy []chplan.Expr, ok bool) {
	proj, isProject := in.(*chplan.Project)
	if !isProject || len(proj.Projections) == 0 || len(proj.Replacements) > 0 {
		return nil, nil, nil, false
	}
	byAlias := make(map[string]chplan.Expr, len(proj.Projections))
	for _, p := range proj.Projections {
		if p.Alias == "" {
			return nil, nil, nil, false
		}
		if _, dup := byAlias[p.Alias]; dup {
			return nil, nil, nil, false
		}
		byAlias[p.Alias] = p.Expr
	}
	// An absent alias fails the identity check as well: byAlias yields a nil
	// Expr, which is not a *chplan.ColumnRef.
	if !isIdentityColumnRef(byAlias[timestampCol], timestampCol) || !isIdentityColumnRef(byAlias[valueCol], valueCol) {
		return nil, nil, nil, false
	}

	var passThrough []string
	seenKey := make(map[string]struct{}, len(groupBy))
	for _, g := range groupBy {
		ref, isRef := g.(*chplan.ColumnRef)
		if !isRef {
			return nil, nil, nil, false
		}
		if _, dup := seenKey[ref.Name]; dup {
			return nil, nil, nil, false
		}
		seenKey[ref.Name] = struct{}{}
		shaped, defined := byAlias[ref.Name]
		if !defined {
			return nil, nil, nil, false
		}
		if isIdentityColumnRef(shaped, ref.Name) {
			passThrough = append(passThrough, ref.Name)
			continue
		}
		recollapse = append(recollapse, chplan.Projection{Expr: shaped, Alias: ref.Name})
	}
	if len(recollapse) == 0 {
		return nil, nil, nil, false
	}

	var rawRefs []string
	rawSeen := make(map[string]struct{}, len(recollapse))
	for _, p := range recollapse {
		chplan.InspectExpr(p.Expr, func(x chplan.Expr) bool {
			ref, isRef := x.(*chplan.ColumnRef)
			if !isRef {
				return true
			}
			if _, dup := rawSeen[ref.Name]; !dup {
				rawSeen[ref.Name] = struct{}{}
				rawRefs = append(rawRefs, ref.Name)
			}
			return true
		})
	}
	if len(rawRefs) == 0 {
		return nil, nil, nil, false
	}
	for _, name := range passThrough {
		if _, shapingInput := rawSeen[name]; shapingInput {
			return nil, nil, nil, false
		}
	}

	rawGroupBy = make([]chplan.Expr, 0, len(passThrough)+len(rawRefs))
	for _, name := range passThrough {
		rawGroupBy = append(rawGroupBy, &chplan.ColumnRef{Name: name})
	}
	for _, name := range rawRefs {
		rawGroupBy = append(rawGroupBy, &chplan.ColumnRef{Name: name})
	}
	return proj.Input, recollapse, rawGroupBy, true
}

// isIdentityColumnRef reports whether x is exactly `ColumnRef(name)` — the
// shape a projection takes when it carries a column through untouched.
func isIdentityColumnRef(x chplan.Expr, name string) bool {
	ref, ok := x.(*chplan.ColumnRef)
	return ok && ref.Name == name
}

// rangeFnPreservesName reports whether a PromQL range function keeps
// `__name__` on its output. Mirrors upstream:
//
//	prometheus/prometheus@cerberus-parser/promql/engine.go:2114
//	`dropName := (e.Func.Name != "last_over_time" && e.Func.Name != "first_over_time")`
func rangeFnPreservesName(fn string) bool {
	return fn == "last_over_time" || fn == "first_over_time"
}

// preservedNameExpr picks the expression a preserve-name wrapper should
// project as the MetricName column, for the LEAF shape — a range function
// applied directly to a matrix selector, where the matchers are in scope.
//
// A single `__name__="x"` equality matcher pins the same name onto every
// row, so a literal is enough and the window's grouping key is left
// alone. Anything else — `__name__=~"a|b"`, or a selector carrying no
// name matcher — spans several metrics whose names differ per series. A
// literal cannot express that: `last_over_time({__name__=~"a|b"}[5m])`
// would answer with rows whose `__name__` is absent, and, worse, two
// metrics sharing an attribute set would collapse into a single series
// because MetricName is not part of the grouping key. So the name rides
// through the window on the group key and is projected per row.
//
// The SPINE sibling of this function is [subqueryPreservedNameExpr] in
// subquery.go: an outer reducer over a subquery has no matcher context at
// all (its input is another lowered plan, not a selector), so it always
// takes the per-series column route — widening every RangeWindow between
// the reducer and the name-bearing Project. Same design, applied one
// level down.
func preservedNameExpr(rw *chplan.RangeWindow, ms []*labels.Matcher, s schema.Metrics) chplan.Expr {
	if name := metricNameFromMatchers(ms); name != "" {
		return &chplan.LitString{V: name}
	}
	if s.MetricNameColumn == "" {
		// A schema with no metric-name column has no per-series name to
		// carry; the literal keeps the canonical 4-column shape intact.
		return &chplan.LitString{V: ""}
	}
	return appendNameGroupKey(rw, s)
}

// appendNameGroupKey adds `MetricName` to a RangeWindow's grouping key so
// the windowed-array emitter projects it per row (the emitter projects
// exactly the group keys, so a column absent from GroupBy is absent from
// the window's output relation). Idempotent: a window that already groups
// on the name keeps a single key. Returns the ColumnRef a caller projects.
func appendNameGroupKey(rw *chplan.RangeWindow, s schema.Metrics) *chplan.ColumnRef {
	nameRef := &chplan.ColumnRef{Name: s.MetricNameColumn}
	for _, g := range rw.GroupBy {
		if isIdentityColumnRef(g, s.MetricNameColumn) {
			return nameRef
		}
	}
	rw.GroupBy = append(append(make([]chplan.Expr, 0, len(rw.GroupBy)+1), rw.GroupBy...), nameRef)
	return nameRef
}

// wrapRangeWindowPreserveName wraps a RangeWindow with a canonical
// 4-column Project that projects `name` as the MetricName column, so the
// HTTP-layer `wrapWithSampleProjection` recognises the canonical shape
// and preserves `__name__` on the wire. Used by `last_over_time` /
// `first_over_time` — at the leaf (lowerRangeVectorCall) and over a
// subquery spine (lowerOuterRangeFnOverSubquery) — to mirror Prom's
// `dropName=false` for those two fns. `name` is either a literal (a
// pinned `__name__=` matcher) or a ColumnRef the window now carries on
// its grouping key.
//
// The matrix-shape RangeWindow (OuterRange > 0) carries per-row anchors
// in the `anchor_ts` column; the instant shape doesn't expose a real
// TimeUnix at all (the SQL emits only Attributes + Value), so the
// projection synthesises one via the same `now64() - 5s` expression
// the handler uses for derived-shape Projects. The outer
// `wrapWithSampleProjection` canonical branch reads back the
// `s.TimestampColumn` alias verbatim either way.
func wrapRangeWindowPreserveName(rw *chplan.RangeWindow, s schema.Metrics, name chplan.Expr) chplan.Node {
	var tsExpr chplan.Expr
	if rw.OuterRange > 0 {
		// The matrix RangeWindow keeps anchor_ts offset-SHIFTED for the
		// window/reduce math; PromQL reports a reducing window's result on the
		// UNSHIFTED grid, so add Offset back (matching the emitter's
		// gridAnchorFrag and the handler's matrixWindowOffset). A raw range
		// vector / subquery (Identity) reports its actual shifted time; a zero
		// offset needs no relabel. Without this, last_over_time / first_over_time
		// (the only *_over_time fns that keep __name__ and so route through this
		// preserve-name wrapper) re-shifted their output past this Project.
		tsExpr = &chplan.ColumnRef{Name: chplan.RangeWindowAnchorColumn}
		if rw.Offset != 0 && !rw.Identity {
			tsExpr = chplan.OffsetReanchoredAnchorExpr(rw.Offset)
		}
	} else {
		// Mirror `synthesizedAnchor()` in internal/api/prom/handler.go:
		// the instant-shape RangeWindow doesn't expose a real per-row
		// anchor, so we stamp `now64(9) - toIntervalNanosecond(5e9)`.
		tsExpr = chplan.NowNanoMinusStaleness()
	}
	return &chplan.Project{
		Input: rw,
		Projections: []chplan.Projection{
			{Expr: name, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: tsExpr, Alias: s.TimestampColumn},
			{Expr: &chplan.ColumnRef{Name: s.ValueColumn}, Alias: s.ValueColumn},
		},
	}
}

// rangeFnCollidesOnNameDrop reports whether a range-function call can
// collapse two distinct series onto one label set by dropping `__name__`,
// which is the condition reference Prometheus refuses to answer:
//
//	prometheus/prometheus@cerberus-parser/promql/engine.go:2295
//	`if !ev.enableDelayedNameRemoval && mat.ContainsSameLabelset() {
//	     ev.errorf("vector cannot contain metrics with the same labelset") }`
//
// Three conditions have to hold together, and each one that fails leaves
// the query on the unchanged, unguarded path:
//
//   - The function drops the name. `last_over_time` / `first_over_time`
//     keep it (rangeFnPreservesName), so their output series stay
//     distinguishable and nothing can collide.
//   - The selector is not pinned to ONE metric name. `rate(cpu[5m])`
//     reads a single name, so after the drop every output row still comes
//     from a distinct label set — there is nothing to collide with. Only a
//     regex name matcher (`{__name__=~"a|b"}`) or a selector carrying no
//     name matcher at all (`{job="x"}`) spans several names.
//   - The window's input still exposes a real per-series name to compare
//     (nodeCarriesMetricName). An inner that already dropped the name has
//     no identity left for the guard to read, and inventing one would be
//     worse than leaving the shape alone.
func rangeFnCollidesOnNameDrop(fn string, ms []*labels.Matcher, inner chplan.Node, s schema.Metrics) bool {
	return !rangeFnPreservesName(fn) &&
		s.MetricNameColumn != "" &&
		metricNameFromMatchers(ms) == "" &&
		nodeCarriesMetricName(inner, s)
}

// duplicateLabelsetGuardExpr is the HAVING gate that aborts the query when
// one output group was fed by more than one metric name.
//
// It rides as a real `chplan.Aggregate.Having` rather than an extra
// SELECT-list column for the reason spelled out on that field: ClickHouse's
// analyzer prunes a SELECT expression nothing downstream reads, `throwIf`
// side effect included, so a column-shaped guard silently never fires.
// `throwIf` returns 0 on success, so `= 0` is the gate — the same idiom
// collapseInfoSeriesBySignature uses for the info() tie guard.
func duplicateLabelsetGuardExpr(s schema.Metrics) chplan.Expr {
	return &chplan.Binary{
		Op: chplan.OpEq,
		Left: &chplan.FuncCall{
			Name: "throwIf",
			Args: []chplan.Expr{
				&chplan.Binary{
					Op:    chplan.OpGt,
					Left:  &chplan.FuncCall{Name: "uniqExact", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.MetricNameColumn}}},
					Right: &chplan.LitInt{V: 1},
				},
				&chplan.InlineString{V: chplan.DuplicateLabelsetMessage},
			},
		},
		Right: &chplan.LitInt{V: 0},
	}
}

// wrapDropNameCollisionGuard re-collapses a name-dropping range function's
// per-(label set, name) rows back onto the per-label-set rows PromQL
// reports, aborting instead when a group was fed by more than one name.
//
// The caller has already widened the window's grouping key with MetricName
// (appendNameGroupKey), which is what makes the collision observable at
// all: without it ClickHouse's `GROUP BY Attributes` merges the two source
// series inside the window and there is nothing left to detect — and the
// merged value is wrong on its own terms, since it reduces two metrics'
// interleaved samples as if they were one series.
//
// `anchorTS` is the expression this wrap reports as the sample timestamp,
// or nil for the instant shape that exposes no per-row anchor (mirroring
// wrapRangeWindowPreserveName's two branches). When it is non-nil the
// window is matrix-shaped and `anchor_ts` joins the guard's grouping key,
// so the check is per (label set, anchor) exactly like upstream's
// per-timestamp Matrix scan.
//
// The output carries an EMPTY MetricName — the name really is dropped,
// per `dropName` — over the same columns the unguarded window exposed:
// the canonical Sample quadruple, plus the raw `anchor_ts` passthrough a
// matrix window owes an enclosing reducer (an outer `*_over_time` over a
// subquery reads its per-row anchor from that column, not from TimeUnix).
// Naming all four canonical outputs is what lets the HTTP layer's
// `projectionExposesCanonical` read the result through unchanged.
//
// `value` is the expression projected as the Value column. Callers that
// forward the surviving row's own value pass `&chplan.ColumnRef{Name:
// s.ValueColumn}`; a subquery-inner `quantile_over_time` with an
// out-of-range literal phi passes the PromQL-spec ±Inf / NaN constant
// instead, folding in the substitution `projectValueOverInner` performs on
// the unguarded paths. Stacking that helper on top of this wrap instead
// would take its non-RangeWindow branch and drop the `anchor_ts`
// passthrough — the same reason wrapRangeWindowAtBroadcast takes a `value`.
func wrapDropNameCollisionGuard(
	node chplan.Node, s schema.Metrics, anchorTS, value chplan.Expr,
) chplan.Node {
	groupBy := []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}}
	aliases := []string{s.AttributesColumn}
	projections := []chplan.Projection{
		{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
		{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
	}
	tsExpr := anchorTS
	if anchorTS == nil {
		// Mirror wrapRangeWindowPreserveName's instant branch (and the
		// handler's synthesizedAnchor()): no per-row anchor exists.
		tsExpr = chplan.NowNanoMinusStaleness()
	} else {
		groupBy = append(groupBy, &chplan.ColumnRef{Name: chplan.RangeWindowAnchorColumn})
		aliases = append(aliases, chplan.RangeWindowAnchorColumn)
		projections = append(projections, chplan.Projection{
			Expr:  &chplan.ColumnRef{Name: chplan.RangeWindowAnchorColumn},
			Alias: chplan.RangeWindowAnchorColumn,
		})
	}
	agg := &chplan.Aggregate{
		Input:          node,
		GroupBy:        groupBy,
		GroupByAliases: aliases,
		// `any` is exact here, not a pick: the HAVING guard has already
		// aborted every group that held more than one name, so a surviving
		// group holds exactly one row.
		AggFuncs: []chplan.AggFunc{{
			Name:  "any",
			Args:  []chplan.Expr{&chplan.ColumnRef{Name: s.ValueColumn}},
			Alias: s.ValueColumn,
		}},
		Having: duplicateLabelsetGuardExpr(s),
	}
	return &chplan.Project{
		Input: agg,
		Projections: append(
			projections,
			chplan.Projection{Expr: tsExpr, Alias: s.TimestampColumn},
			chplan.Projection{Expr: value, Alias: s.ValueColumn},
		),
	}
}

// dropNameGuardAnchor picks the timestamp expression
// [wrapDropNameCollisionGuard] reports for the fan-out / single-anchor
// shapes, or nil when the window is instant-shaped and exposes no anchor.
//
// It mirrors wrapRangeWindowPreserveName exactly, including the offset
// re-anchor: the fan-out matrix window keeps `anchor_ts` offset-SHIFTED for
// the window math while PromQL reports a reducing window on the UNSHIFTED
// grid. The native (timeSeries*ToGrid) node is the documented exception —
// its anchor axis is already the unshifted request grid, so re-shifting it
// here would double-shift `rate(m[r] offset o)`; this is the same carve-out
// the handler's matrixWindowOffset makes.
func dropNameGuardAnchor(node chplan.Node, rw *chplan.RangeWindow) chplan.Expr {
	if rw.OuterRange <= 0 {
		return nil
	}
	anchor := chplan.Expr(&chplan.ColumnRef{Name: chplan.RangeWindowAnchorColumn})
	if _, native := node.(*chplan.RangeWindowNative); native {
		return anchor
	}
	if rw.Offset != 0 && !rw.Identity {
		return chplan.OffsetReanchoredAnchorExpr(rw.Offset)
	}
	return anchor
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
// `name` is the expression projected as the MetricName column, or nil
// when the range function drops `__name__`. `last_over_time` /
// `first_over_time` preserve it (dropName=false in Prom); for them the
// caller supplies `preservedNameExpr`'s literal-or-column choice and the
// projection exposes the canonical 4-column Sample contract
// `(MetricName, Attributes, anchor_ts AS TimeUnix, Value)`, mirroring
// wrapRangeWindowPreserveName's matrix branch. Every other range fn
// passes nil, so MetricName is omitted.
//
// `value` is the expression projected as the Value column. Callers that
// forward the window's own value pass `&chplan.ColumnRef{Name:
// s.ValueColumn}`; `quantile_over_time` with an out-of-range literal phi
// passes the PromQL-spec ±Inf / NaN constant instead, folding the
// substitution that projectValueOverInner does on the non-pinned paths
// into this projection. Reusing projectValueOverInner here would restamp
// every broadcast row: its generic branch projects the inner's own
// TimeUnix and drops `anchor_ts`, which is precisely the column the
// broadcast exists to carry.
func wrapRangeWindowAtBroadcast(
	rw *chplan.RangeWindow, ctx lowerCtx, s schema.Metrics, name, value chplan.Expr,
) chplan.Node {
	grid := &chplan.StepGrid{Start: ctx.start.UTC(), End: ctx.end.UTC(), Step: ctx.step}
	joined := &chplan.CrossJoin{Left: grid, Right: rw}

	projections := make([]chplan.Projection, 0, 4)
	if name != nil {
		projections = append(projections,
			chplan.Projection{Expr: name, Alias: s.MetricNameColumn})
	}
	projections = append(
		projections,
		chplan.Projection{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
		chplan.Projection{Expr: &chplan.ColumnRef{Name: chplan.RangeWindowAnchorColumn}, Alias: chplan.RangeWindowAnchorColumn},
		chplan.Projection{Expr: &chplan.ColumnRef{Name: chplan.RangeWindowAnchorColumn}, Alias: s.TimestampColumn},
		chplan.Projection{Expr: value, Alias: s.ValueColumn},
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
	if a.Op == parser.LIMITK {
		return lowerLimitK(a, s, ctx)
	}
	if a.Op == parser.COUNT_VALUES {
		return lowerCountValues(a, s, ctx)
	}
	if a.Op == parser.LIMIT_RATIO {
		return lowerLimitRatio(a, s, ctx)
	}

	input, err := lower(a.Expr, s, ctx)
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
	// Pinned, because the aggregate this guards reads phi through
	// [sanitizedPhiParamExpr], which is pinned by ClickHouse's
	// constant-parameter rule. The two halves must read the SAME phi: a
	// per-step guard beside a statement-wide aggregate would let an
	// in-range step pass the 0.5 sentinel the aggregate computed with
	// straight through as if it were the real quantile.
	phiE, err := lowerScalarArg(a.Param, s, ctx.withPinnedScalars())
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

// topKDomain resolves a LITERAL topk/bottomk/limitk K against reference
// Prometheus's K rules and, when the query survives them, returns the
// int64 K the plan selects with.
//
// The rules themselves are not here: they live in
// [aggregationParamDomain], which the computed-K path runs too (via a
// [ScalarGuard]) on the value series ClickHouse produces. A literal K is
// simply a one-value series, so passing it through the same function is
// what keeps the two paths from ever disagreeing about the domain.
//
// What is left here is the literal path's own remainder: reference
// truncates the surviving K toward zero (`int64(fParam)`), so
// `topk(1.5, v)` selects the top 1 series.
//
// Returns (k, false, nil) for the regular path, (0, true, nil) for the
// empty-result short-circuit, and a non-nil error for the shapes
// reference Prometheus itself rejects.
func topKDomain(kF float64) (k int64, empty bool, err error) {
	empty, err = aggregationParamDomain([]float64{kF})
	if err != nil || empty {
		return 0, empty, err
	}
	return int64(kF), false, nil
}

// lowerLimitRatio lowers `limit_ratio(r, expr) [by(g) | without(g)]`,
// PromQL's experimental ratio-sampling aggregator. Like topk/bottomk it
// SELECTS a subset of input series and preserves every label of the
// survivors; unlike topk it picks the subset deterministically by a hash
// of each series' label set rather than by Value rank, so the `by/
// without` clause never affects which series survive — it only governs
// the (unused) partitioning, exactly as in reference Prometheus where
// aggregationK runs the ratio sampler per series independent of the
// group key.
//
// Reference semantics (prometheus/promql/engine.go, HashRatioSampler):
//
//	offset(series) = float64(labels.Hash()) / float64(math.MaxUint64)
//	keep when r >= 0: offset < r
//	keep when r <  0: offset >= 1 + r        (the complement of |r|)
//	r == 0           : empty result
//	r  > 1 / r < -1  : warning only; the raw comparison still applies,
//	                   so |r| >= 1 keeps every series.
//	r is NaN         : query error.
//
// labels.Hash() (default `stringlabels` build) is
// `xxhash.Sum64(<encoded label set>)`, where the encoding is the labels
// sorted by name, each emitted as len-prefix(name)+name+len-prefix(value)
// +value with a single length byte per field (see [lenPrefixExpr] for
// the >=255-byte escape that real label sets never trigger). ClickHouse's
// `xxHash64` is byte-for-byte XXH64(seed=0), matching cespare/xxhash/v2,
// so reconstructing that exact byte string from MetricName (the
// `__name__` label) + the Attributes map and hashing it reproduces the
// reference offset bit-for-bit. See [ratioOffsetExpr].
//
// SQL shape (r >= 0):
//
//	SELECT MetricName, Attributes, TimeUnix, Value FROM (<inner>)
//	WHERE <offset> < r
//
// No partition/anchor threading is needed: the predicate is per-row and
// stable across evaluation steps, so the same series survive at every
// anchor — matching Prom's per-step-but-deterministic behaviour.
func lowerLimitRatio(a *parser.AggregateExpr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	input, err := lower(a.Expr, s, ctx)
	if err != nil {
		return nil, err
	}
	// Instant context: the input relation is the canonical Sample shape,
	// so `__name__` reads straight off the MetricName column.
	offset := func() chplan.Expr {
		return ratioOffsetExpr(s, &chplan.ColumnRef{Name: s.MetricNameColumn})
	}
	pred, err := limitRatioPredicate(a.Param, offset, s, ctx)
	if err != nil {
		return nil, err
	}
	return &chplan.Filter{Input: input, Predicate: pred}, nil
}

// limitRatioPredicate builds the row predicate implementing
// HashRatioSampler's keep rule for ratio expression `param`, given a
// factory that mints a fresh per-series offset expression.
//
// `offset` is a factory rather than a value because the computed-ratio
// arm needs the offset in two sibling positions and chplan Expr trees
// must stay trees (clone / walk invariants assume distinct nodes).
//
// Shared by the instant lowering (`lowerLimitRatio`) and the
// subquery-matrix lowering (`lowerSubqueryOverLimitRatio`); the two
// differ only in where the series' `__name__` comes from, which is
// exactly what the factory closes over.
func limitRatioPredicate(
	param parser.Expr,
	offset func() chplan.Expr,
	s schema.Metrics,
	ctx lowerCtx,
) (chplan.Expr, error) {
	// Literal-ratio fast path (the common case: `limit_ratio(0.5, v)`,
	// any scalar tree TryFoldScalar reduces). The ratio's sign is known
	// at plan time, so a single comparison suffices and `r == 0` folds
	// to a constant-false predicate (Prometheus returns early on all-zero
	// r), mirroring topk's K<1 degenerate arm.
	if r, ok := tryScalarLiteral(param); ok {
		empty, err := limitRatioParamDomain([]float64{r})
		if err != nil {
			return nil, err
		}
		switch {
		case empty:
			return &chplan.LitBool{V: false}, nil
		case r > 0:
			// keep offset < r
			return &chplan.Binary{Op: chplan.OpLt, Left: offset(), Right: &chplan.LitFloat{V: r}}, nil
		default:
			// keep offset >= 1 + r (r < 0 here, so 1+r in [0,1))
			return &chplan.Binary{Op: chplan.OpGe, Left: offset(), Right: &chplan.LitFloat{V: 1.0 + r}}, nil
		}
	}

	// Computed ratio (`limit_ratio(scalar(x), v)`, `limit_ratio(time()
	// % 2, v)`, …): the sign isn't known at plan time, so emit the full
	// runtime predicate exactly as HashRatioSampler.AddRatioSampleWithOffset
	// spells it —
	//
	//	(r >= 0 AND offset < r) OR (r < 0 AND offset >= 1 + r)
	//
	// which also yields the empty result for r == 0 (neither arm fires).
	// The ratio Expr is bound per step by lowerScalarArg, so the predicate
	// sees the same ratio series reference's newFParams builds.
	//
	// A NaN ratio is the one value the predicate cannot express: both arms
	// compare false, so the query would answer empty where reference raises
	// "Ratio value is NaN". Register the guard that evaluates the ratio on
	// its own and applies the same domain the literal arm above just used.
	if err := registerScalarGuard(ctx, "limit_ratio ratio", param, s, func(values []float64) error {
		_, err := limitRatioParamDomain(values)
		return err
	}); err != nil {
		return nil, err
	}
	zero := &chplan.LitFloat{V: 0}
	one := &chplan.LitFloat{V: 1}
	// Lower the ratio arg once per predicate slot so no chplan Expr
	// pointer is shared between sibling Binary nodes (keeps the IR a
	// tree, not a DAG — clone / walk invariants assume distinct nodes).
	// lowerScalarArg is deterministic, so the four ratio sub-expressions
	// are structurally identical. A single error site covers all four.
	rExprs := make([]chplan.Expr, 4)
	for i := range rExprs {
		e, err := lowerScalarArg(param, s, ctx)
		if err != nil {
			return nil, fmt.Errorf("promql: limit_ratio ratio: %w", err)
		}
		rExprs[i] = e
	}
	posArm := &chplan.Binary{
		Op:    chplan.OpAnd,
		Left:  &chplan.Binary{Op: chplan.OpGe, Left: rExprs[0], Right: zero},
		Right: &chplan.Binary{Op: chplan.OpLt, Left: offset(), Right: rExprs[1]},
	}
	negArm := &chplan.Binary{
		Op:    chplan.OpAnd,
		Left:  &chplan.Binary{Op: chplan.OpLt, Left: rExprs[2], Right: zero},
		Right: &chplan.Binary{Op: chplan.OpGe, Left: offset(), Right: &chplan.Binary{Op: chplan.OpAdd, Left: one, Right: rExprs[3]}},
	}
	return &chplan.Binary{Op: chplan.OpOr, Left: posArm, Right: negArm}, nil
}

// ratioOffsetExpr builds the CH expression reproducing Prometheus's
// HashRatioSampler.SampleOffset for a series: a deterministic value in
// [0, 1) derived from `xxhash.Sum64` over the canonical length-prefixed
// encoding of the series label set, divided by float64(math.MaxUint64).
//
// The label set is reconstructed from the Attributes map with the
// `__name__` label restored from `nameExpr` (later-key-wins via
// mapConcat so `nameExpr` is authoritative). Keys are sorted ascending
// to match Prometheus's sorted-labels invariant, then each (name, value)
// pair is emitted as lenPrefix(name)+name+lenPrefix(value)+value and the
// whole run concatenated before hashing.
//
// `nameExpr` nil means the input relation carries no per-series metric
// name — the label set IS Attributes, with no `__name__` member at all,
// and the overlay is skipped entirely. That is not a fallback but the
// correct encoding: an input whose name was dropped upstream (`rate(…)`,
// `sum by (…) (…)`) has a genuinely name-less label set in the reference
// engine too, and overlaying `__name__=”` would hash a label set no
// Prometheus series ever has.
func ratioOffsetExpr(s schema.Metrics, nameExpr chplan.Expr) chplan.Expr {
	const (
		mapKeysAlias = "k"
		// float64(math.MaxUint64); the divisor Prometheus uses to map a
		// 64-bit hash into [0, 1).
		maxUint64AsFloat = 18446744073709551615.0
	)

	attrs := chplan.Expr(&chplan.ColumnRef{Name: s.AttributesColumn})

	// full = mapConcat(Attributes, map('__name__', <nameExpr>))
	//
	// The `__name__` label is restored from `nameExpr` and overlaid onto
	// the Attributes map. mapConcat is later-key-wins, so `nameExpr` is
	// authoritative if Attributes somehow already carried a `__name__`
	// key (it never does in the OTel-CH schema).
	//
	// The overlay is unconditional *within this branch*: whether a name
	// exists at all is a plan-time question the caller has already
	// answered by passing nil or not, so no CH-side conditional is
	// needed. That matters — every CH spelling of a typed empty-map
	// fallback (`CAST(map(), ?)` binds the Map type as a `?`
	// placeholder; `mapFilter(...)` over Attributes left the branch type
	// indeterminate) poisons ClickHouse's `concat`-vs-`arrayConcat`
	// overload resolution downstream (Code 43) once the expression flows
	// up through the inner aggregate's argMax/GROUP-BY aliasing.
	full := attrs
	if nameExpr != nil {
		full = &chplan.FuncCall{
			Name: "mapConcat",
			Args: []chplan.Expr{
				attrs,
				&chplan.FuncCall{
					Name: "map",
					// `__name__` MUST be an inline literal, not a `?`-bound
					// LitString: with the key bound as a placeholder CH can't
					// resolve the map literal's key type at analysis time and
					// mis-dispatches the downstream concat to arrayConcat
					// (Code 43). See chplan.InlineString.
					Args: []chplan.Expr{&chplan.InlineString{V: "__name__"}, nameExpr},
				},
			},
		}
	}

	// Sorted key list of the full map.
	sortedKeys := &chplan.FuncCall{
		Name: "arraySort",
		Args: []chplan.Expr{&chplan.FuncCall{Name: "mapKeys", Args: []chplan.Expr{full}}},
	}

	// Per-key encoding lambda: lenPrefix(k)+k+lenPrefix(full[k])+full[k].
	//
	// Each piece is wrapped in toString(): ClickHouse overloads `concat`
	// to dispatch to `arrayConcat` when it infers any argument as an
	// Array, and the conditional Map subscript
	// `if(..., mapConcat(...), Attributes)[k]` flowing up through the
	// argMax/GROUP-BY aliasing of the inner aggregate trips that
	// inference (Code 43, "Argument 0 for function arrayConcat must be
	// an array but it has type String"). Forcing every operand to String
	// pins the string-concat overload and keeps the byte output
	// identical.
	keyIdent := &chplan.BareIdent{Name: mapKeysAlias}
	valExpr := &chplan.Subscript{Container: full, Key: keyIdent}
	toStr := func(e chplan.Expr) chplan.Expr {
		return &chplan.FuncCall{Name: "toString", Args: []chplan.Expr{e}}
	}
	perKey := &chplan.FuncCall{
		Name: "concat",
		Args: []chplan.Expr{
			toStr(lenPrefixExpr(keyIdent)),
			toStr(keyIdent),
			toStr(lenPrefixExpr(valExpr)),
			toStr(valExpr),
		},
	}

	encoded := &chplan.FuncCall{
		Name: "arrayStringConcat",
		Args: []chplan.Expr{
			&chplan.FuncCall{
				Name: "arrayMap",
				Args: []chplan.Expr{
					&chplan.Lambda{Params: []string{mapKeysAlias}, Body: perKey},
					sortedKeys,
				},
			},
		},
	}

	hash := &chplan.FuncCall{Name: "xxHash64", Args: []chplan.Expr{encoded}}

	// offset = toFloat64(hash) / float64(math.MaxUint64)
	return &chplan.Binary{
		Op:    chplan.OpDiv,
		Left:  &chplan.FuncCall{Name: "toFloat64", Args: []chplan.Expr{hash}},
		Right: &chplan.LitFloat{V: maxUint64AsFloat},
	}
}

// lenPrefixExpr renders Prometheus's stringlabels length prefix for a
// string `s` as the single byte `char(length(s))`.
//
// Prometheus's encoding uses a single length byte for sizes 0..254 and a
// `0xFF` escape + 3-byte little-endian length for sizes >= 255. Cerberus
// emits only the single-byte form: OTel-CH label names and values are
// short identifiers (metric names, instance/job/service tags) that never
// approach 255 bytes — Prometheus's own metric/label-name grammar and
// the OTel attribute model keep them well under that — so the escape
// branch can never fire for real series. Restricting to the single-byte
// form keeps the per-series hash expression compact and byte-for-byte
// identical to `labels.Hash()` for every label set that can actually
// reach this path.
func lenPrefixExpr(sExpr chplan.Expr) chplan.Expr {
	return &chplan.FuncCall{
		Name: "char",
		Args: []chplan.Expr{&chplan.FuncCall{Name: "length", Args: []chplan.Expr{sExpr}}},
	}
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
	k, empty, err := topKDomain(kF)
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
		Columns:  topKOutputColumns(input, s),
	}, nil
}

// lowerLimitK lowers PromQL's experimental `limitk(K, expr) [by(g) |
// without(g)] (...)` aggregator. Unlike topk/bottomk, limitk does NOT
// rank: per aggregation group it returns up to K *arbitrary* series,
// with their samples unchanged. The result vector keeps every original
// label of the surviving series — `by(...)` only partitions, it does
// not drop labels (same shape contract as topk).
//
// SQL shape:
//
//	SELECT MetricName, Attributes, TimeUnix, Value FROM (<inner>)
//	  LIMIT K [BY <partition_exprs>]
//
// There is no ORDER BY — CH's `LIMIT K BY <group>` returns the first K
// rows it encounters per partition, which is exactly limitk's "any K
// series per group" contract. The partition shape and the K-domain
// rules are shared verbatim with topk/bottomk (topKPartition /
// topKDomain): K < 1 short-circuits to an empty result, fractional
// K >= 1 truncates toward zero, NaN / int64-overflow K are rejected.
//
// Computed K (`limitk(scalar(<vector>), v)`) falls through to
// lowerTopKComputed, which swaps CH's constant-only LIMIT for a
// `row_number() OVER (PARTITION BY <by>) <= K` rank filter. The window
// carries no ORDER BY there, so the K survivors are again whichever rows
// the window numbers first — the same arbitrary-K-per-group contract
// `LIMIT K BY` gives on the literal path.
//
// Range mode (ctx.step > 0): like topk, limitk selects K series per
// evaluation step. topKPartition appends the per-step TimeUnix anchor
// so `LIMIT K BY (<user-partition>, TimeUnix)` fires per anchor.
func lowerLimitK(a *parser.AggregateExpr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	kF, ok := tryScalarLiteral(a.Param)
	if !ok {
		return lowerTopKComputed(a, s, ctx)
	}
	k, empty, err := topKDomain(kF)
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
		// lowerTopK's K-too-small fold.
		return &chplan.Filter{
			Input:     input,
			Predicate: &chplan.LitBool{V: false},
		}, nil
	}

	by := topKPartition(a, s, ctx)

	return &chplan.TopK{
		Input:     input,
		K:         k,
		By:        by,
		Unordered: true,
		Columns:   topKOutputColumns(input, s),
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

// canonicalSampleColumns adapts the schema's configurable column naming
// into the [chplan.SampleColumns] shape every chplan classifier takes.
// The names are configuration — a schema override can rename any of the
// four — so the classifiers are handed the live naming rather than the
// OTel-CH defaults.
func canonicalSampleColumns(s schema.Metrics) chplan.SampleColumns {
	return chplan.SampleColumns{
		MetricName: s.MetricNameColumn,
		Attributes: s.AttributesColumn,
		Timestamp:  s.TimestampColumn,
		Value:      s.ValueColumn,
	}
}

// topKOutputColumns derives the outer SELECT list a [chplan.TopK] projects
// over `input` from what `input` ACTUALLY exposes, rather than declaring
// the canonical quadruple and hoping the input agrees.
//
// topk / bottomk / limitk re-project their input's rows without re-keying
// them, so their output column set IS their input's column set. A
// canonical input (a selector, a binop, a name-preserving Project) carries
// (MetricName, Attributes, Timestamp, Value), and naming them pins a
// fixed-arity list for the chDB round-trip runner and the handler
// projection. A DERIVED input — an instant-mode [chplan.RangeWindow], the
// shape `topk(2, sum_over_time(m[5m]))` lowers to — exposes only
// (group keys…, Value): no MetricName and no timestamp exist in that
// scope, so naming them emits a reference ClickHouse rejects with code 47,
// `Unknown expression identifier 'MetricName'`. The empty list renders
// `SELECT *`, which forwards whatever the input produces verbatim — the
// derivation, not a second declaration of it.
//
// [chplan.IsDerivedShape] is the one classifier for that question, shared
// with the HTTP layer's canonical-sample projection and the emitter's
// VectorSetOp arm canonicalisation, so the three cannot disagree about one
// node's column set.
func topKOutputColumns(input chplan.Node, s schema.Metrics) []string {
	if chplan.IsDerivedShape(input, canonicalSampleColumns(s)) {
		return nil
	}
	return []string{
		s.MetricNameColumn,
		s.AttributesColumn,
		s.TimestampColumn,
		s.ValueColumn,
	}
}

// lowerTopKComputed lowers `topk`/`bottomk`/`limitk` with a K that is
// any scalar-valued PromQL expression rather than a foldable literal —
// `topk(scalar(<vector>), v)`, `topk(scalar(x) * 2, v)`,
// `topk(ceil(scalar(x) / 3) + 1, v)`, `topk(time() % 5, v)`. CH's LIMIT
// clause requires a constant, so the lowering routes K through
// chplan.TopK's KExpr slot and the emitter renders a `row_number()
// OVER (...) <= K` rank filter (see emitTopKComputed).
//
// K binds through [lowerScalarArg], the single owner of "scalar-typed
// PromQL expression → chplan.Expr": the parser's type checker closes
// that space over literals, arithmetic, `scalar(<vector>)`, `time()` and
// `pi()`, so every shape the grammar admits in K position is covered by
// construction — there is no residual shape for a guard to reject. The
// resulting Expr is materialised as a one-row relation because KExpr is
// a Node slot (the emitter reads its `Value` column).
//
// [topKDomainExpr] applies the runtime half of the K-domain rules the
// literal path resolves in [topKDomain].
func lowerTopKComputed(a *parser.AggregateExpr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	// The K domain is reference Prometheus's, and reference applies it to
	// the parameter's whole per-step value series before it aggregates
	// anything. A computed K has no value until the query runs, and the
	// rejection messages quote the offending value, so the check rides
	// beside the plan as its own statement — see [ScalarGuard].
	if err := registerScalarGuard(ctx, a.Op.String()+" K", a.Param, s, func(values []float64) error {
		_, err := aggregationParamDomain(values)
		return err
	}); err != nil {
		return nil, err
	}

	// Lower the K argument in instant context (step=0). chplan.TopK's
	// KExpr is a one-row relation the emitter reads with an UNCORRELATED
	// `(SELECT … LIMIT 1)`, so there is no per-row anchor in scope for a
	// per-step K to key off and K binds once per statement. Reusing the
	// surrounding ctx (with step > 0) would also drag a StepGrid CROSS
	// JOIN into the K subtree — the result vector's shape comes from
	// `a.Expr`, not the K subtree.
	kCtx := ctx.withPinnedScalars()
	kCtx.step = 0
	kValue, err := lowerScalarArg(a.Param, s, kCtx)
	if err != nil {
		return nil, err
	}
	kExpr := &chplan.Project{
		Input: &chplan.OneRow{},
		Projections: []chplan.Projection{
			{Expr: topKDomainExpr(kValue), Alias: s.ValueColumn},
		},
	}

	input, err := lower(a.Expr, s, ctx)
	if err != nil {
		return nil, err
	}

	t := &chplan.TopK{
		Input:   input,
		KExpr:   kExpr,
		By:      topKPartition(a, s, ctx),
		Columns: topKOutputColumns(input, s),
	}
	if a.Op == parser.LIMITK {
		// limitk keeps K arbitrary series per group — no ranking, so the
		// rank filter's window carries a bare PARTITION BY and the
		// surviving rows are whichever K the window numbers first.
		t.Unordered = true
	} else {
		t.SortExpr = &chplan.ColumnRef{Name: s.ValueColumn}
		t.Desc = a.Op == parser.TOPK
	}
	return t, nil
}

// topKDomainExpr is the runtime sibling of [topKDomain]: it folds a
// computed K into the rank threshold `row_number() <= K` compares
// against, so the two paths agree on the K domain.
//
// Ranks start at 1, so a threshold below 1 selects nothing and a
// fractional threshold truncates for free (`_rn <= 2.7` ⇔ `_rn <= 2`) —
// which is reference's `params.Max() < 1 → empty result` and its
// `int64(fParam)` truncation, with no cast needed. The one shape that
// does need folding is NaN: an unfolded NaN threshold would compare
// false and read as "empty" only by accident, and CH's integer casts
// turn it into 0 or a saturated maximum depending on version. Fold it to
// [topKEmptyThreshold] explicitly so the empty result is the plan's
// intent rather than a coincidence of comparison semantics.
func topKDomainExpr(k chplan.Expr) chplan.Expr {
	return &chplan.FuncCall{
		Name: "if",
		Args: []chplan.Expr{
			isNaNExpr(k),
			&chplan.LitFloat{V: topKEmptyThreshold},
			k,
		},
	}
}

// topKEmptyThreshold is the rank threshold that selects no series:
// row_number() is 1-based, so any value below 1 is empty and 0 is the
// canonical spelling.
const topKEmptyThreshold = 0

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
		// ClickHouse requires a parameterised aggregate's parameter to be a
		// CONSTANT expression, so the phi in `quantile(<phi>)(Value)` cannot
		// reference a per-row anchor and binds once per statement here.
		phiE, err := lowerScalarArg(a.Param, s, ctx.withPinnedScalars())
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
		// Unreachable from the wire: count_values changes the output shape
		// (a synthetic label per distinct value), so lowerAggregate and
		// lowerSubqueryOverAggregate intercept it before buildAggFunc is
		// consulted. Arriving here is a routing bug, not a missing feature.
		return chplan.AggFunc{}, fmt.Errorf("promql: %s must be lowered by lowerCountValues, not the generic aggregate path", a.Op.String())
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
