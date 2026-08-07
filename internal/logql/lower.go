// Package logql lowers Loki LogQL queries into the shared cerberus chplan
// IR. Covers stream selectors with `=`/`!=`/`=~`/`!~` label matchers,
// the line-filter family (`|=`, `!=`, `|~`, `!~`), label filters
// (`| label="v"`), parsers (`| json`, `| logfmt`), the metric form
// (rate, count_over_time, ...), and aggregations.
package logql

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	syntax "github.com/tsouza/cerberus/internal/logql/lsyntax"

	"github.com/tsouza/cerberus/internal/api/format"
	"github.com/tsouza/cerberus/internal/cerbtrace"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/qlcommon"
	"github.com/tsouza/cerberus/internal/schema"
)

// tracer emits the `lower` pipeline-stage span for LogQL lowering.
var tracer = otel.Tracer("github.com/tsouza/cerberus/internal/logql")

// lowerCtx threads query-time information needed by lowering. Zero
// Start / End mean "no time window threaded" — the lowering emits a plan
// without a Timestamp BETWEEN predicate. Callers reaching LogQL through
// the API handler pass the request's [start, end] so each Scan(otel_logs)
// is filtered down to the requested wire-format window at the SQL layer
// (the previous behaviour returned every matching log row regardless of
// the requested window — a Loki wire-format contract violation).
//
// Step carries the request's `step` for /loki/api/v1/query_range metric
// queries. When > 0 (and the [Start, End] window is non-zero), the
// range-aggregation lowering sets RangeWindow.{Start,End,Step,OuterRange}
// so the emitter fans across the request's step grid via the matrix
// path (one row per anchor in [Start, End], spaced by Step) instead of
// the instant-eval shape that anchors at `now64(9)`. Without this, a
// metric query whose seeded data lies outside the last 5 minutes of
// wall-clock returns an empty matrix because the windowed-array filter
// `arrayFilter(p -> tupleElement(p,1) > now64(9) - <range>, ...)` drops
// every sample. Mirrors the PromQL LowerAtRange / lowerCtx.step shape
// in internal/promql/lower.go.
type lowerCtx struct {
	Start time.Time
	End   time.Time
	Step  time.Duration

	// OuterByLabels carries the by-clause labels of an enclosing
	// vector aggregation (`sum by (SeverityText, ServiceName) (rate(...))`)
	// down into the inner range aggregation's identity Project. When
	// one of these labels names a top-level OTel-CH scalar column
	// (SeverityText, ServiceName, SeverityNumber, ...) that lives
	// outside ResourceAttributes, the inner range Project surfaces it
	// into the augmented identity map so the outer Aggregate's
	// `ResourceAttributes[<label>]` lookup resolves to the column's
	// per-row value rather than the empty string. Without this plumb,
	// `sum by (SeverityText) (rate({}[5m]))` collapsed every row into
	// one series `{SeverityText:""}` because `SeverityText` is a
	// top-level otel_logs column, not a key inside the
	// ResourceAttributes Map. See [withDetectedLevel] for the wrap
	// that consumes this list and [topLevelLogColumnFor] for the
	// label→column resolution.
	OuterByLabels []string
}

// withOuterByLabels returns a copy of c with OuterByLabels set to
// labels. Used by [lowerVectorAggregation] before recursing into the
// inner range aggregation so the inner identity can surface any
// top-level OTel-CH columns the outer by-clause references.
func (c lowerCtx) withOuterByLabels(labels []string) lowerCtx {
	out := c
	out.OuterByLabels = labels
	return out
}

// hasTimeWindow reports whether the context carries a non-degenerate
// [Start, End] pair to inject as a BETWEEN predicate.
func (c lowerCtx) hasTimeWindow() bool {
	return !c.Start.IsZero() && !c.End.IsZero()
}

// rangeMode reports whether the context carries a request step grid
// (a non-zero Step on top of a non-zero [Start, End] pair). The
// range-aggregation lowering switches to the matrix RangeWindow shape
// only when this is true.
func (c lowerCtx) rangeMode() bool {
	return c.Step > 0 && c.hasTimeWindow()
}

// withMatcherWindowExtension returns a copy of c with Start moved back
// by `extension`. Range-aggregation lowerings call this before threading
// the context into the inner LogSelectorExpr lowering so the pre-scan
// `Timestamp >= start AND Timestamp <= end` clamp (see andFoldTimeWindow)
// keeps the per-anchor `(anchor_ts - range, anchor_ts]` windows complete
// at the left edge of the matrix.
//
// Without the extension, the leftmost anchors of a /query_range matrix
// (anchor = Start, Start + Step, …, up to Start + range) evaluate against
// truncated windows — only the [Start, anchor] portion survives the
// outer clamp. Reference Loki / Prom evaluators read across the full
// (anchor - range, anchor] window because they have no equivalent
// pre-scan clamp. The fix mirrors that behaviour by extending the clamp
// back to `Start - max(range + offset)` whenever a range aggregation
// lowering descends into its inner selector.
//
// Both call sites pass `Interval + Offset`: an offset shifts the window
// further into the past, so it *adds* to how far back the clamp has to
// reach. A non-positive extension is a no-op, so a zero-interval
// selector needs no guard at the call site. A context with no time
// window at all is likewise a no-op — the [Lower] entry point, or a
// [LowerAt] caller that passed zero bounds. Either way no clamp was
// injected, so there is nothing for the extension to move.
func (c lowerCtx) withMatcherWindowExtension(extension time.Duration) lowerCtx {
	if extension <= 0 || !c.hasTimeWindow() {
		return c
	}
	out := c
	out.Start = c.Start.Add(-extension)
	return out
}

// Lower turns a parsed LogQL expression into a chplan tree. No time
// window is injected — callers that know the request's [start, end]
// should use [LowerAt] instead.
func Lower(ctx context.Context, expr syntax.Expr, s schema.Logs) (chplan.Node, error) {
	return lowerWithCtx(ctx, expr, s, lowerCtx{})
}

// LowerAt is the time-aware variant of [Lower]: it AND-folds a
// `<TimestampColumn> >= start AND <TimestampColumn> <= end` predicate
// above every Scan(LogsTable) the lowering produces, so the emitted
// SQL honours the request's window. For an instant query the caller
// passes start == end == ts (or [time-step, time] per Loki convention).
func LowerAt(ctx context.Context, expr syntax.Expr, s schema.Logs, start, end time.Time) (chplan.Node, error) {
	return lowerWithCtx(ctx, expr, s, lowerCtx{Start: start, End: end})
}

// LowerAtRange is the range-mode variant of [LowerAt]: it threads a
// step duration alongside [start, end] so range-aggregation lowerings
// can emit the matrix RangeWindow shape (one row per anchor across
// [start, end] spaced by step). Mirrors PromQL's LowerAtRange. Step ≤ 0
// falls back to the instant shape (same as LowerAt).
func LowerAtRange(ctx context.Context, expr syntax.Expr, s schema.Logs, start, end time.Time, step time.Duration) (chplan.Node, error) {
	return lowerWithCtx(ctx, expr, s, lowerCtx{Start: start, End: end, Step: step})
}

func lowerWithCtx(ctx context.Context, expr syntax.Expr, s schema.Logs, lc lowerCtx) (chplan.Node, error) {
	_, span := tracer.Start(ctx, cerbtrace.SpanLower, trace.WithAttributes(cerbtrace.AttrQL.String("logql")))
	defer span.End()
	plan, err := lower(expr, s, lc)
	if err != nil {
		span.RecordError(err)
		return nil, err
	}
	span.SetAttributes(cerbtrace.AttrPlanNodeCount.Int(cerbtrace.CountNodes(plan)))
	return plan, nil
}

func lower(expr syntax.Expr, s schema.Logs, lc lowerCtx) (chplan.Node, error) {
	switch e := expr.(type) {
	case *syntax.MatchersExpr:
		return lowerMatchers(e, s, lc), nil
	case *syntax.PipelineExpr:
		return lowerPipeline(e, s, lc)
	case *syntax.RangeAggregationExpr:
		return lowerRangeAggregation(e, s, lc)
	case *syntax.VectorAggregationExpr:
		return lowerVectorAggregation(e, s, lc)
	case *syntax.LiteralExpr:
		return lowerLiteral(e, s)
	case *syntax.VectorExpr:
		return lowerVector(e, s)
	case *syntax.BinOpExpr:
		return lowerBinary(e, s, lc)
	case *syntax.LabelReplaceExpr:
		return lowerLabelReplace(e, s, lc)
	case *syntax.MultiVariantExpr:
		return lowerMultiVariant(e, s, lc)
	default:
		return nil, fmt.Errorf("logql: unsupported expression %T", expr)
	}
}

// lowerMatchers turns `{job="api", env=~"prod|stg"}` into Scan + Filter.
// Stream-selector label matchers go against the ResourceAttributes map
// since OTel-CH stores stream-identity labels there. When the context
// carries a [start, end] window, a `TimestampColumn BETWEEN start AND end`
// predicate is AND-folded above the Scan so the emitted SQL honours
// the request's wire-format window.
func lowerMatchers(e *syntax.MatchersExpr, s schema.Logs, lc lowerCtx) chplan.Node {
	scan := &chplan.Scan{Table: s.LogsTable}
	pred := buildMatchersPredicate(e.Mts, s)
	pred = andFoldTimeWindow(pred, s, lc)
	if pred == nil {
		return scan
	}
	return &chplan.Filter{Input: scan, Predicate: pred}
}

// lowerPipeline handles a stream selector followed by line / label
// filters and parser stages.
//
// labelsExpr threads the "current labels map" through stage iteration:
// initially it's the schema's ResourceAttributes column; after a parser
// stage (`| logfmt`, `| json`, `| regexp`) it becomes a mapConcat that
// folds the parsed key/value pairs onto the prior labels map. The exact
// inner expression varies by parser:
//
//   - `| logfmt`  → extractKeyValuePairs(Body, '=', ' ', '"')
//   - `| json`    → CAST(JSONExtractKeysAndValues(Body, 'String') AS Map(...))
//   - `| regexp`  → map(<name>, extractAllGroupsHorizontal(Body, <pat>)[i][1], ...)
//
// Downstream label filters resolve against this composite labels map.
// Loki's documented contract is "parsed labels appended; on conflict
// the stream label wins and parsed gets `_extracted` suffix". Cerberus
// enforces that on the SQL side for every parser family that lowers to
// a labels merge — bare and typed `| logfmt`, bare and typed `| json`,
// and `| regexp`. Both merge shapes route through a single pair of
// constructors so the policy cannot be applied to one family and
// skipped on another: [LogfmtParsedLabels] wraps a query-time-unknown key
// set in a `mapApply` rename, and [mergeParsedFields] wraps each
// statically-known destination identifier in a per-key `if(...)`.
func lowerPipeline(e *syntax.PipelineExpr, s schema.Logs, lc lowerCtx) (chplan.Node, error) {
	node, _, err := lowerPipelineWithLabels(e, s, lc)
	return node, err
}

// lowerPipelineWithLabels is the underlying pipeline lowering. It returns
// the final "labels map" expression alongside the Node so range-aggregation
// callers (range_aggregation.go) can plumb `| unwrap` post-filters against
// the same labels map the pipeline produced for ordinary label filters.
//
// The returned labelsExpr is the schema's ResourceAttributes column when
// no parser stage ran; otherwise it carries a `mapConcat(...)` wrapper
// that adds parsed keys (see [logfmtMergeLabels]).
func lowerPipelineWithLabels(e *syntax.PipelineExpr, s schema.Logs, lc lowerCtx) (chplan.Node, chplan.Expr, error) {
	inner := lowerMatchers(e.Left, s, lc)
	pred := chplan.Expr(nil)
	if f, ok := inner.(*chplan.Filter); ok {
		pred = f.Predicate
		inner = f.Input
	}
	labelsExpr := chplan.Expr(&chplan.ColumnRef{Name: s.ResourceAttributesColumn})
	// dynamicLabels becomes true once a `| unpack` / `| pattern` stage
	// runs — both extract labels in Go after the rows return (see
	// unpackStep / newPatternStep in internal/api/loki/post_process.go)
	// rather than folding into labelsExpr the way `| json` / `| logfmt`
	// / `| regexp` do. A downstream *syntax.LabelFilterExpr still gets
	// SQL lowering — for an ordinary label name that's a deliberate,
	// pinned fallback (structuredOrStreamLookup: check the structured-
	// metadata / stream-label columns as a best-effort pre-filter,
	// since a query-time JSON payload's fields aren't knowable at
	// lowering time). But the fallback is actively WRONG for the
	// `__error__` / `__error_details__` family specifically: those
	// keys only ever exist as unpack's own error markers (see
	// unpackStep) — they're never legitimately present in
	// LogAttributes/ResourceAttributes — so the fallback's SQL
	// predicate degenerates to a silent no-op: `__error__=""` matches
	// EVERY row (the key is simply absent from both columns) and
	// `__error__="JSONParserErr"` matches NONE, incorrectly excluding
	// rows before postProcessExtract's Go-side unpackStep ever runs
	// (see #1611's compat corpus, which caught this via a real
	// differential run). Skip SQL lowering for just that family;
	// [newLabelFilterStep] applies the same LabelFilterer in Go once
	// the dynamic stage's transform has actually computed the row's
	// true `__error__` / `__error_details__` labels.
	dynamicLabels := false
	for _, stage := range e.MultiStages {
		if lf, ok := stage.(*syntax.LabelFilterExpr); ok && dynamicLabels && FiltersErrorLabel(lf.LabelFilterer) {
			continue
		}
		next, newLabels, err := lowerStage(stage, s, labelsExpr)
		if err != nil {
			return nil, nil, err
		}
		if newLabels != nil {
			labelsExpr = newLabels
		}
		if isDynamicLabelStage(stage) {
			dynamicLabels = true
		}
		// Post-fetch stages (`| line_format`, `| decolorize`) return a
		// nil predicate — they're applied in Go after the rows return,
		// not in SQL. Skip them so we don't fold a nil into the AND.
		if next == nil {
			continue
		}
		if pred == nil {
			pred = next
		} else {
			pred = &chplan.Binary{Op: chplan.OpAnd, Left: pred, Right: next}
		}
	}
	if pred == nil {
		return inner, labelsExpr, nil
	}
	return &chplan.Filter{Input: inner, Predicate: pred}, labelsExpr, nil
}

// isDynamicLabelStage reports whether stage is a `| unpack` / `|
// pattern` parser stage — see [lowerPipelineWithLabels]'s dynamicLabels
// gate.
func isDynamicLabelStage(stage syntax.StageExpr) bool {
	lp, ok := stage.(*syntax.LineParserExpr)
	if !ok {
		return false
	}
	return lp.Op == syntax.OpParserTypePattern
}

// FiltersErrorLabel reports whether lf tests the `__error__` /
// `__error_details__` label anywhere in its tree (walking
// BinaryLabelFilter's and/or composition) — see
// [lowerPipelineWithLabels]'s dynamicLabels gate, which only skips SQL
// lowering for this family, not for arbitrary label names. Exported so
// internal/api/loki's postProcessExtract can apply the exact same gate
// when deciding which post-`| unpack` / `| pattern` label filters need
// a Go-side re-evaluation (see post_process.go's newLabelFilterStep).
func FiltersErrorLabel(lf syntax.LabelFilterer) bool {
	switch f := lf.(type) {
	case *syntax.StringLabelFilter:
		return f.Name == syntax.ErrorLabel || f.Name == syntax.ErrorDetailsLabel
	case *syntax.BinaryLabelFilter:
		return FiltersErrorLabel(f.Left) || FiltersErrorLabel(f.Right)
	case *syntax.NumericLabelFilter:
		return f.Name == syntax.ErrorLabel || f.Name == syntax.ErrorDetailsLabel
	case *syntax.DurationLabelFilter:
		return f.Name == syntax.ErrorLabel || f.Name == syntax.ErrorDetailsLabel
	case *syntax.BytesLabelFilter:
		return f.Name == syntax.ErrorLabel || f.Name == syntax.ErrorDetailsLabel
	case *syntax.IPLabelFilter:
		return f.Label == syntax.ErrorLabel || f.Label == syntax.ErrorDetailsLabel
	}
	return false
}

// PipelineLabelsExpr re-walks the parsed LogQL expression and returns the
// final labels-map expression a log-stream query would project as its
// per-row Attributes column. The returned shape mirrors the live
// labelsExpr that [lowerPipelineWithLabels] threads through pipeline
// stages — the schema's ResourceAttributes column when no parser stage
// fired, or a `mapConcat(...)` wrapper folding parser-extracted keys
// onto the prior labels map (see [logfmtMergeLabels] / [jsonBareMergeLabels]
// / [regexpMergeLabels] / [logfmtExpressionMergeLabels] /
// [jsonExpressionMergeLabels]).
//
// Returns nil when expr is nil, when expr is a non-log shape (metric
// queries hit a different ProjectSamples branch), or when the pipeline
// has no parser stage (the caller can fall back to ResourceAttributes
// directly).
//
// Used by [Lang.ProjectSamples]'s log-stream branch to surface
// parser-extracted keys (`| logfmt`, `| json`, `| regexp ...`) as
// per-row Attributes so [toStreamsWithTransform] groups one Stream per
// unique (resource-label, extracted-key) tuple — matching reference
// Loki's stream-identity contract (PR #570). Without this hook the
// projection would only carry the raw ResourceAttributes column and a
// query like `{cluster="c"} | logfmt` would collapse hundreds of
// reference-Loki streams into a handful, regressing the loki-compat
// differential.
//
// The implementation re-walks rather than re-using the lowering's
// labelsExpr because Parse → ProjectSamples threads through engine.Meta,
// not through the Lower call stack, and storing a chplan.Expr in
// Meta.Extra would tie the engine type to chplan. The walk is cheap
// (linear in stage count) and the lowering itself is the source-of-truth
// for the per-stage merge shape — the helpers below dispatch to the
// same constructors.
func PipelineLabelsExpr(expr syntax.Expr, s schema.Logs) (chplan.Expr, error) {
	pipe, ok := expr.(*syntax.PipelineExpr)
	if !ok {
		return nil, nil
	}
	labelsExpr := chplan.Expr(&chplan.ColumnRef{Name: s.ResourceAttributesColumn})
	for _, stage := range pipe.MultiStages {
		merged, err := pipelineStageLabels(stage, s, labelsExpr)
		if err != nil {
			return nil, err
		}
		if merged != nil {
			labelsExpr = merged
		}
	}
	return labelsExpr, nil
}

// pipelineStageLabels returns the post-stage labels-map expression for a
// single pipeline stage, or nil if the stage doesn't alter the visible
// label set. Mirrors the `newLabels` branch of [lowerStage] but isolates
// the labels-only walk from the predicate-side concerns so callers that
// only need the final labels expression don't pay for predicate
// construction.
func pipelineStageLabels(stage syntax.StageExpr, s schema.Logs, labelsExpr chplan.Expr) (chplan.Expr, error) {
	switch st := stage.(type) {
	case *syntax.LineParserExpr:
		switch st.Op {
		case syntax.OpParserTypePattern:
			return nil, nil
		case syntax.OpParserTypeUnpack:
			return unpackMergeLabels(labelsExpr, s), nil
		case syntax.OpParserTypeJSON:
			return jsonBareMergeLabels(labelsExpr, s), nil
		case syntax.OpParserTypeRegexp:
			return regexpMergeLabels(labelsExpr, s, st.Param)
		}
		return nil, nil
	case *syntax.LogfmtParserExpr:
		return logfmtMergeLabels(labelsExpr, s), nil
	case *syntax.JSONExpressionParserExpr:
		return jsonExpressionMergeLabels(labelsExpr, s, st.Expressions)
	case *syntax.LogfmtExpressionParserExpr:
		return logfmtExpressionMergeLabels(labelsExpr, s, st.Expressions)
	case *syntax.LabelFilterExpr:
		// Duration label filters conditionally stamp `__error__` /
		// `__error_details__` onto kept rows (see [lowerStage]); the
		// projection-side walk must mirror that so the per-row
		// Attributes column carries the same label set the lowering's
		// labelsExpr does.
		_, marks, err := labelFiltererLower(st.LabelFilterer, s, labelsExpr)
		if err != nil {
			return nil, err
		}
		if len(marks) == 0 {
			return nil, nil
		}
		return wrapLabelsWithMarks(labelsExpr, marks), nil
	}
	return nil, nil
}

// PipelineLineExpr returns the expression a log-stream query should
// project as its line column, or nil when no stage rewrites the line in
// SQL and the caller can project the body column directly.
//
// Only `| unpack` rewrites the line in SQL today: it replaces the packed
// payload with its `_entry` member. The rewrite has to happen here
// rather than after the rows return, because the labels the same stage
// extracts are already computed in SQL — splitting one stage's line
// across two evaluation sites is how the two answers drift apart.
//
// `| line_format` and `| decolorize` still rewrite the line in Go, which
// leaves one ordering the projection cannot honour: a `| line_format`
// BEFORE an `| unpack` feeds the reformatted line to unpack upstream,
// while here unpack reads the stored body. That ordering is already the
// shape every SQL-side parser stage has — `| line_format | json` reads
// the stored body too — so unpack now shares it rather than introducing
// it.
func PipelineLineExpr(expr syntax.Expr, s schema.Logs) (chplan.Expr, error) {
	pipe, ok := expr.(*syntax.PipelineExpr)
	if !ok {
		return nil, nil
	}
	var lineExpr chplan.Expr
	for _, stage := range pipe.MultiStages {
		lp, ok := stage.(*syntax.LineParserExpr)
		if !ok || lp.Op != syntax.OpParserTypeUnpack {
			continue
		}
		prev := lineExpr
		if prev == nil {
			prev = &chplan.ColumnRef{Name: s.BodyColumn}
		}
		lineExpr = unpackLineExpr(prev, s)
	}
	return lineExpr, nil
}

// HasParserStage reports whether the parsed LogQL expression contains a
// parser stage (`| logfmt`, `| json`, `| unpack`, `| regexp ...`,
// typed-variants) that the SQL lowering folds into the labels map. Used
// by [Lang.ProjectSamples] to gate the parser-extracted labels surface —
// when true, the projection uses [PipelineLabelsExpr]'s output for the
// Attributes column so per-row labels include extracted keys.
//
// `| pattern` returns false: it is the one parser whose labels are still
// extracted in Go after the rows return (see post_process.go) rather
// than in SQL, so the SQL projection has nothing to surface for it — the
// post-process step mutates the labels map per-row instead.
func HasParserStage(expr syntax.Expr) bool {
	pipe, ok := expr.(*syntax.PipelineExpr)
	if !ok {
		return false
	}
	for _, stage := range pipe.MultiStages {
		switch st := stage.(type) {
		case *syntax.LineParserExpr:
			switch st.Op {
			case syntax.OpParserTypeJSON, syntax.OpParserTypeRegexp, syntax.OpParserTypeUnpack:
				return true
			}
		case *syntax.LogfmtParserExpr,
			*syntax.JSONExpressionParserExpr,
			*syntax.LogfmtExpressionParserExpr:
			return true
		}
	}
	return false
}

// HasLabelMutatingStage reports whether the parsed LogQL expression
// contains any pipeline stage that alters the SQL-side labels map —
// either a parser stage (see [HasParserStage]) or a duration label
// filter, which conditionally stamps `__error__` / `__error_details__`
// onto rows whose value Go's time.ParseDuration rejects (reference
// Loki keeps such rows and marks them; see [durationLabelFilterExpr]).
// [Lang.ProjectSamples] gates the Attributes-projection swap on this
// so `{app="x"} | duration > 5s` surfaces the error labels even
// without a parser stage in the pipeline.
func HasLabelMutatingStage(expr syntax.Expr) bool {
	if HasParserStage(expr) {
		return true
	}
	pipe, ok := expr.(*syntax.PipelineExpr)
	if !ok {
		return false
	}
	for _, stage := range pipe.MultiStages {
		if st, ok := stage.(*syntax.LabelFilterExpr); ok && labelFiltererHasDuration(st.LabelFilterer) {
			return true
		}
	}
	return false
}

// labelFiltererHasDuration walks a label-filterer tree for duration
// filters — the only filter kind whose lowering currently produces
// `__error__` marks.
func labelFiltererHasDuration(lf syntax.LabelFilterer) bool {
	switch v := lf.(type) {
	case *syntax.DurationLabelFilter:
		return true
	case *syntax.BinaryLabelFilter:
		return labelFiltererHasDuration(v.Left) || labelFiltererHasDuration(v.Right)
	}
	return false
}

// lowerStage handles one pipeline stage. Returns up to two values:
//
//   - pred: a predicate expression to AND into the pipeline filter
//     (nil for post-fetch / no-op-in-SQL stages).
//   - newLabels: a replacement labels-map expression to thread into
//     subsequent label filters (nil for stages that don't change the
//     visible label set).
//
// labelsExpr is the current "labels map" expression — the base
// `ResourceAttributes` column, or a `mapConcat(...)` wrapped form after
// a `| logfmt` stage. Label filters MapAccess against it so they see
// both stream-selector labels and parser-extracted keys.
func lowerStage(stage syntax.StageExpr, s schema.Logs, labelsExpr chplan.Expr) (chplan.Expr, chplan.Expr, error) {
	switch st := stage.(type) {
	case *syntax.LineFilterExpr:
		p, err := lowerLineFilter(st, s)
		return p, nil, err
	case *syntax.LabelFilterExpr:
		pred, marks, err := labelFiltererLower(st.LabelFilterer, s, labelsExpr)
		if err != nil {
			return nil, nil, err
		}
		// Duration filters stamp `__error__` / `__error_details__` on
		// rows whose label value Go's time.ParseDuration rejects (the
		// row is KEPT — see durationLabelFilterExpr). Thread the
		// conditionally-stamped labels map forward so later stages
		// (`| __error__ = ""`, groupings, the stream projection) see
		// the same labels the reference engine's LabelsBuilder carries.
		// No marks → nil keeps the existing labels expression (and the
		// existing fixture surface) untouched.
		if len(marks) == 0 {
			return pred, nil, nil
		}
		return pred, wrapLabelsWithMarks(labelsExpr, marks), nil
	case *syntax.LineFmtExpr:
		// `| line_format "{{.x}}"` is a post-fetch transform —
		// applied in the API handler over the streams response, not
		// in SQL. Return no predicate so the lowering doesn't error
		// on it but the handler still sees the LineFmtExpr in the
		// original parsed expression.
		_ = st
		return nil, nil, nil
	case *syntax.DecolorizeExpr:
		// Same post-fetch shape: strip ANSI codes from each line
		// after the rows return. No SQL impact.
		return nil, nil, nil
	case *syntax.LabelFmtExpr:
		// `| label_format new=old, lvl="{{.severity}}"` mutates the
		// row's label set in Go after the rows return — rename or
		// template-set per Loki's contract. No SQL impact; the
		// post-process pipeline pulls the LabelFmtExpr from the
		// parsed expression on the handler side.
		return nil, nil, nil
	case *syntax.LineParserExpr:
		// `| unpack` and `| pattern` are parser stages that extract
		// labels from the line in Go after the rows return — they have
		// no SQL impact (lowering returns no predicate). The API handler
		// pulls them out of the parsed expression via postProcessExtract
		// and applies them per row.
		//
		// `| json` and `| regexp` lower to a labels-map merge so
		// subsequent label filters resolve against the parsed keys —
		// mirroring how `| logfmt` is handled below.
		switch st.Op {
		case syntax.OpParserTypePattern:
			return nil, nil, nil
		case syntax.OpParserTypeUnpack:
			return nil, unpackMergeLabels(labelsExpr, s), nil
		case syntax.OpParserTypeJSON:
			return nil, jsonBareMergeLabels(labelsExpr, s), nil
		case syntax.OpParserTypeRegexp:
			merged, err := regexpMergeLabels(labelsExpr, s, st.Param)
			if err != nil {
				return nil, nil, err
			}
			return nil, merged, nil
		}
		return nil, nil, fmt.Errorf("logql: parser stage `| %s` is not yet supported", st.Op)
	case *syntax.LogfmtParserExpr:
		// Bare `| logfmt` — extracts all `key=value` pairs from the
		// line. Subsequent label filters resolve against
		// mapConcat(<prev labels>, extractKeyValuePairs(Body, ...)).
		// Strict / KeepEmpty flags are intentionally ignored:
		// CH's extractKeyValuePairs is lenient (no Strict equivalent)
		// and always drops bare keys (no KeepEmpty equivalent), which
		// matches Loki's non-strict default semantics for the common
		// case.
		_ = st
		return nil, logfmtMergeLabels(labelsExpr, s), nil
	case *syntax.JSONExpressionParserExpr:
		// Typed `| json foo="response.code", bar="status"` — maps
		// caller-chosen local names to specific JSON paths. Resulting
		// labels expose only the named fields (no implicit merge of all
		// top-level keys).
		merged, err := jsonExpressionMergeLabels(labelsExpr, s, st.Expressions)
		if err != nil {
			return nil, nil, err
		}
		return nil, merged, nil
	case *syntax.LogfmtExpressionParserExpr:
		// Typed `| logfmt foo="bar", baz="qux"` — maps caller-chosen
		// local names to specific logfmt keys. Resulting labels expose
		// only the named fields (no implicit merge of all pairs).
		merged, err := logfmtExpressionMergeLabels(labelsExpr, s, st.Expressions)
		if err != nil {
			return nil, nil, err
		}
		return nil, merged, nil
	case *syntax.DropLabelsExpr:
		// `| drop foo, bar` removes named keys from the output label set
		// in Go after the rows return. The matching `*labels.Matcher`
		// variant (`| drop foo="v"`) drops only when the value matches.
		// Either way there's no SQL impact — the stream selector +
		// label filters already constrain which rows are returned; drop
		// only narrows the label map carried back to the caller. The
		// API handler pulls the stage out via postProcessExtract.
		_ = st
		return nil, nil, nil
	case *syntax.KeepLabelsExpr:
		// `| keep foo, bar` is the inverse projection: only the named
		// labels survive on the output row. Same post-fetch shape as
		// `| drop` — no SQL impact, applied in Go.
		_ = st
		return nil, nil, nil
	default:
		return nil, nil, fmt.Errorf("logql: pipeline stage %T is not yet supported", stage)
	}
}

// duplicateSuffix mirrors `loglib.duplicateSuffix` (unexported in
// upstream Loki). When a parser-extracted key would shadow a
// stream-selector label, Loki appends this suffix to the extracted
// key so the stream label wins on collision and the extracted value
// is still reachable under the suffixed name. Cerberus mirrors that
// disambiguation contract on the SQL side (see [logfmtMergeLabels]
// + [logfmtExpressionMergeLabels]) and on the Go-side
// post-processing path (`internal/api/loki/post_process.go`).
const duplicateSuffix = "_extracted"

// logfmtMergeLabels wraps the current labelsExpr with a
// `mapConcat(<prev>, <renamed extracted>)` so subsequent label filters
// see the union of stream-selector labels and logfmt-parsed key/value
// pairs — with stream-selector labels winning on key collisions.
//
// The renamed-extracted form is
//
//	mapApply(
//	    (k, v) -> (if(mapContains(<stream>, k), concat(k, '_extracted'), k), v),
//	    extractKeyValuePairs(Body, '=', ' ', '"'))
//
// where `<stream>` is the schema's ResourceAttributes column (NOT
// `prev`, which may itself include parser-extracted keys from an
// earlier parser stage in the same pipeline). Loki's reference
// implementation (`LabelsBuilder.Add` → `BaseHas`) only suffixes when
// the parsed name collides with a stream label, not when it collides
// with another parser stage's output — cerberus matches that.
// extractKeyValuePairs is the CH built-in that lifts arbitrary
// `key=value` text into a `Map(String, String)`; the separator /
// pair-delimiter / quote arguments mirror Loki's logfmt parser
// defaults.
func logfmtMergeLabels(prev chplan.Expr, s schema.Logs) chplan.Expr {
	return concatParsedLabels(prev, LogfmtParsedLabels(s))
}

// LogfmtParsedLabels returns the `Map(String, String)` of labels a bare
// `| logfmt` stage contributes to the label set — the CH-side extraction
// with Loki's stream-label collision rename already applied, but WITHOUT
// the merge onto the running labels map.
//
// It is exported because the Loki metadata surface has to answer "which
// fields can a `| logfmt` query actually read?" and the only answer that
// cannot drift is the expression the query path itself runs.
// `/loki/api/v1/detected_fields` projects this very expression into its
// peek SQL rather than re-deriving the field set with a second, Go-side
// extractor: a second derivation is what advertised logfmt keys the
// query path could never produce (issue #1888). Cerberus's `| logfmt`
// lowers to ClickHouse's `extractKeyValuePairs`, whose key grammar skips
// characters Loki's own decoder would rewrite to `_`, so the two
// extractors genuinely disagree on the KEY NAMES — `(method='GET')`
// yields `method` here and `_method` under a Loki-shaped decoder.
func LogfmtParsedLabels(s schema.Logs) chplan.Expr {
	return renameExtractedOnCollision(s, extractKVPairs(s))
}

// JSONParsedLabels is the `| json` counterpart of [LogfmtParsedLabels]:
// the top-level key/value pairs a bare `| json` stage contributes, with
// the collision rename applied. Same single-source-of-truth contract —
// `/detected_fields` projects it instead of re-parsing bodies in Go.
func JSONParsedLabels(s schema.Logs) chplan.Expr {
	return renameExtractedOnCollision(s, jsonFlattenedMap(s))
}

// concatParsedLabels merges an already-renamed parsed-label map onto the
// running labels map. Stream-selector labels win on collision because
// the renamed map has had its colliding keys suffixed already.
//
// Together with [mergeParsedFields] this is one of the only two ways to
// build a parser-stage labels merge, and its argument comes from
// [LogfmtParsedLabels] or [JSONParsedLabels] — the two constructors that
// apply the collision policy. No parser family can therefore merge
// extracted keys without the rename: the divergence that let `| json` /
// `| regexp` extracted keys silently overwrite stream labels is closed
// by construction rather than by repeating the rename at each call site.
func concatParsedLabels(prev, renamedParsed chplan.Expr) chplan.Expr {
	return &chplan.FuncCall{
		Name: "mapConcat",
		Args: []chplan.Expr{prev, renamedParsed},
	}
}

// parsedField pairs a parser stage's statically-known destination label
// name with the expression that yields the label's value.
type parsedField struct {
	name  string
	value chplan.Expr
}

// mergeParsedFields folds a parser stage's statically-known destination
// identifiers onto the running labels map, applying Loki's collision
// policy to each name. Used by the parser forms whose destination
// identifier set is fixed at SQL-emit time — typed `| logfmt foo="..."`,
// typed `| json foo="..."`, and `| regexp` (named captures) — where the
// rename is a per-key `if(mapContains(<stream>, '<id>'), '<id>_extracted',
// '<id>')` evaluated once per row instead of via `mapApply`.
//
// Callers hand over the raw identifier, never a pre-built key expression:
// the rename is this constructor's job, so a new parser family cannot
// forget it. See [LogfmtParsedLabels] / [JSONParsedLabels] for the
// dynamic-key counterparts.
func mergeParsedFields(prev chplan.Expr, s schema.Logs, fields []parsedField) chplan.Expr {
	args := make([]chplan.Expr, 0, len(fields)*2)
	for _, f := range fields {
		args = append(args, renameIdentifierOnCollision(s, f.name), f.value)
	}
	return &chplan.FuncCall{
		Name: "mapConcat",
		Args: []chplan.Expr{
			prev,
			&chplan.FuncCall{Name: "map", Args: args},
		},
	}
}

// logfmtExpressionMergeLabels wraps labelsExpr with a `mapConcat` that
// stitches in only the named extractions (identifier => extracted
// value). Each expression is `<identifier>="<key-path>"` — for logfmt
// the path is a top-level key, so the lowering is
// `extractKeyValuePairs(Body, ...)[<key-path>]`. The result is a
// `map(<rename(id1)>, <val1>, <rename(id2)>, <val2>, …)` where
// `<rename(id)>` is `if(mapContains(<stream>, '<id>'), '<id>_extracted',
// '<id>')` — same conflict-resolution contract as [logfmtMergeLabels],
// applied at SQL-emit time for each user-chosen identifier since the
// identifier set is known statically.
func logfmtExpressionMergeLabels(prev chplan.Expr, s schema.Logs, exprs []syntax.LabelExtractionExpr) (chplan.Expr, error) {
	if len(exprs) == 0 {
		// Defensive: a parser-emitted empty list is shaped like the
		// bare `| logfmt` form. Treat it as such so we don't drop the
		// stage entirely.
		return logfmtMergeLabels(prev, s), nil
	}
	kvBase := extractKVPairs(s)
	fields := make([]parsedField, 0, len(exprs))
	for _, ext := range exprs {
		if ext.Identifier == "" {
			return nil, fmt.Errorf("logql: `| logfmt` expression has empty identifier")
		}
		// `Expression` is the source key in the logfmt-parsed map.
		// When the user writes `| logfmt foo` (no `="..."`), Loki's
		// parser fills `Expression == Identifier` so both forms
		// resolve identically.
		key := ext.Expression
		if key == "" {
			key = ext.Identifier
		}
		fields = append(fields, parsedField{
			name: ext.Identifier,
			value: &chplan.MapAccess{
				Map: kvBase,
				Key: &chplan.LitString{V: key},
			},
		})
	}
	return mergeParsedFields(prev, s, fields), nil
}

// renameExtractedOnCollision wraps a Map(String,String) expression with
// a `mapApply` that renames any key that already exists in the stream's
// label set. The CH shape is
//
//	mapApply(
//	    (k, v) -> (if(mapContains(<stream>, k), concat(k, '_extracted'), k), v),
//	    <extracted>)
//
// Applied by [LogfmtParsedLabels] and [JSONParsedLabels] for the bare
// `| logfmt` and bare `| json`
// forms, where the extracted-key set is unknown at SQL-emit time so the
// rename has to happen per-key inside the lambda.
func renameExtractedOnCollision(s schema.Logs, extracted chplan.Expr) chplan.Expr {
	streamCol := &chplan.ColumnRef{Name: s.ResourceAttributesColumn}
	return &chplan.FuncCall{
		Name: "mapApply",
		Args: []chplan.Expr{
			&chplan.Lambda{
				Params: []string{"k", "v"},
				Body: &chplan.FuncCall{
					Name: "tuple",
					Args: []chplan.Expr{
						&chplan.FuncCall{
							Name: "if",
							Args: []chplan.Expr{
								&chplan.FuncCall{
									Name: "mapContains",
									Args: []chplan.Expr{
										streamCol,
										&chplan.BareIdent{Name: "k"},
									},
								},
								&chplan.FuncCall{
									Name: "concat",
									Args: []chplan.Expr{
										&chplan.BareIdent{Name: "k"},
										&chplan.LitString{V: duplicateSuffix},
									},
								},
								&chplan.BareIdent{Name: "k"},
							},
						},
						&chplan.BareIdent{Name: "v"},
					},
				},
			},
			extracted,
		},
	}
}

// renameIdentifierOnCollision returns a chplan.Expr that resolves at
// query time to either `<id>` (when the stream's label set does not
// contain `<id>`) or `<id>_extracted` (when it does). Applied by
// [mergeParsedFields] for the typed `| logfmt foo="..."` / typed
// `| json foo="..."` / `| regexp` lowerings, where each destination
// identifier is known statically — the rename is a per-key
// `if(mapContains(<stream>, '<id>'), '<id>_extracted', '<id>')`
// evaluated once per row instead of via mapApply.
func renameIdentifierOnCollision(s schema.Logs, id string) chplan.Expr {
	return &chplan.FuncCall{
		Name: "if",
		Args: []chplan.Expr{
			&chplan.FuncCall{
				Name: "mapContains",
				Args: []chplan.Expr{
					&chplan.ColumnRef{Name: s.ResourceAttributesColumn},
					&chplan.LitString{V: id},
				},
			},
			&chplan.LitString{V: id + duplicateSuffix},
			&chplan.LitString{V: id},
		},
	}
}

// extractKVPairs renders the CH built-in
// `extractKeyValuePairs(<Body>, '=', ' ', '"')` — the Map(String,String)
// that the `| logfmt` parser stage exposes to downstream label filters.
// The three delimiter arguments are Loki's logfmt defaults: `=` between
// key and value, space between pairs, double-quote as the quoting
// character.
func extractKVPairs(s schema.Logs) chplan.Expr {
	pairs := &chplan.FuncCall{
		Name: "extractKeyValuePairs",
		Args: []chplan.Expr{
			&chplan.ColumnRef{Name: s.BodyColumn},
			&chplan.LitString{V: "="},
			&chplan.LitString{V: " "},
			&chplan.LitString{V: "\""},
		},
	}
	// `dup=1 dup=2` yields a Map carrying BOTH entries, and a CH Map
	// lookup resolves to the first — upstream's decoder loop assigns each
	// pair in turn, so the last write stands. Reduce before the map is
	// built so every consumer of a logfmt label set, bare or by
	// expression, reads the same value.
	return castToLabelMap(lastKeyWins(castToLabelPairArray(pairs)))
}

// jsonNestedKeySpacer joins the segments of a nested JSON key into one
// flat label name — `{"user":{"id":…}}` becomes `user_id`. It doubles as
// the replacement byte for every character Loki's key sanitisation
// rejects, because upstream uses `_` for both (`jsonSpacer` and
// `sanitizeLabelKey` in pkg/logql/log/parser.go).
const jsonNestedKeySpacer = "_"

// jsonKeyInvalidCharClass is the RE2 class of bytes Loki's
// sanitizeLabelKey rewrites to [jsonNestedKeySpacer]: everything outside
// the Prometheus label-name alphabet.
const jsonKeyInvalidCharClass = `[^A-Za-z0-9_]`

// jsonKeyLeadingDigit / jsonKeyLeadingDigitFix prepend `_` to a key that
// starts with a digit, so the result is a legal label name. Loki applies
// the rule only where the accumulated prefix is still empty
// (`appendSanitized`), which is why the flattening applies it to the
// JOINED key: once a non-empty prefix is in front, the joined key starts
// with the prefix's own already-fixed first byte and the rewrite is a
// no-op. `{"1a":{"2b":…}}` therefore yields `_1a_2b`, not `_1a__2b`.
//
// Spelling this as a capturing rewrite rather than an `if(match(…))`
// keeps the key expression referenced once instead of three times, which
// matters: this expression is inlined at every use of the labels map.
const (
	jsonKeyLeadingDigit    = `^([0-9])`
	jsonKeyLeadingDigitFix = `_\1`
)

// jsonScalarLeafPrefix matches the first byte of every raw JSON value
// Loki's parseObject extracts — a string, `true`/`false`, or a number.
// Objects (`{`), arrays (`[`) and `null` are the complement, and are
// exactly the values upstream drops. One anchored match replaces three
// separate prefix tests over the same value.
const jsonScalarLeafPrefix = `^["tf0-9-]`

// jsonKeyTrimWhitespace strips the leading and trailing whitespace Loki's
// key sanitisation trims before the character rewrite. Trimming first is
// load-bearing: without it the surrounding spaces would survive as
// `_` bytes in the label name.
const jsonKeyTrimWhitespace = `^[\t\n\v\f\r ]+|[\t\n\v\f\r ]+$`

// The first byte of a raw JSON value discriminates its type, which is
// all the flattening needs: objects recurse, arrays and nulls are
// dropped, strings are unescaped, and everything else (numbers,
// booleans) is already in its final textual form.
const (
	jsonObjectPrefix = "{"
	jsonStringPrefix = `"`
)

// jsonBareMergeLabels wraps the current labelsExpr with a
// `mapConcat(<prev>, mapApply(<rename>, CAST(JSONExtractKeysAndValues(
// Body, 'String') AS Map(String,String))))` so subsequent label filters
// see the union of stream-selector labels and JSON-parsed top-level
// key/value pairs — with stream-selector labels winning on key
// collisions and the shadowed JSON value reachable under the
// `_extracted`-suffixed name (see [LogfmtParsedLabels]).
//
// JSONExtractKeysAndValues(json, 'String') returns
// `Array(Tuple(String, String))` for the top-level object keys with each
// value cast to String. CAST to Map(String, String) gives the same shape
// the rest of the pipeline expects (mirrors the `| logfmt` lowering).
// Nested objects stringify to their JSON form rather than flattening to
// `parent_child` keys — that's an approximation of Loki's bare `| json`
// semantics; the common flat-object case is exact.
func jsonBareMergeLabels(prev chplan.Expr, s schema.Logs) chplan.Expr {
	return concatParsedLabels(prev, JSONParsedLabels(s))
}

// jsonFlattenedMap renders the `Map(String, String)` of every scalar leaf
// in the row's JSON body, keyed by its `parent_child` flattened name.
//
// The shape is a fold over a work list of `(flattened key, raw value)`
// pairs. `JSONExtractKeysAndValuesRaw` splits one object into its
// immediate members without interpreting their values, so each fold step
// replaces every member that is itself an object by that object's own
// members — carrying the parent's already-sanitised key down as a
// prefix — and leaves every scalar untouched. After enough steps no
// object remains and the fold is a fixpoint.
//
// "Enough steps" is the document's own nesting depth, which is bounded
// by its count of `{`: one step consumes one level, and a document
// cannot nest deeper than the number of objects it contains. Deriving
// the bound from the row rather than pinning a constant is what keeps
// this exact at any depth — a fixed cap would silently truncate deep
// documents, which is the class of approximation this expression exists
// to remove. Steps past the fixpoint are no-ops, so over-counting `{`
// (a brace inside a string literal, say) costs iterations, never
// correctness.
func jsonFlattenedMap(s schema.Logs) chplan.Expr {
	body := &chplan.ColumnRef{Name: s.BodyColumn}

	// Seed: the body's top-level members, keyed by the sanitised
	// top-level key. Loki applies its leading-digit rule here because the
	// accumulated prefix is still empty.
	seed := &chplan.FuncCall{
		Name: "arrayMap",
		Args: []chplan.Expr{
			&chplan.Lambda{
				Params: []string{jsonMemberParam},
				Body: jsonPair(
					jsonJoinKey(&chplan.LitString{V: ""}, jsonMemberKey()),
					jsonMemberValue(),
				),
			},
			jsonMembersOf(body),
		},
	}

	folded := &chplan.FuncCall{
		Name: "arrayFold",
		Args: []chplan.Expr{
			&chplan.Lambda{
				Params: []string{jsonAccParam, jsonStepParam},
				Body:   jsonExpandObjects(),
			},
			&chplan.FuncCall{
				Name: "range",
				Args: []chplan.Expr{
					&chplan.FuncCall{
						Name: "countSubstrings",
						Args: []chplan.Expr{body, &chplan.LitString{V: jsonObjectPrefix}},
					},
				},
			},
			seed,
		},
	}

	return castToLabelMap(lastKeyWins(jsonReadLeaves(folded)))
}

// Lambda parameter names for the flattening fold. They are emitted
// verbatim into the SQL, so they are named to be unmistakable in a
// golden and impossible to confuse with a column.
const (
	jsonAccParam       = "__json_acc"
	jsonStepParam      = "__json_step"
	jsonEntryParam     = "__json_entry"
	jsonMemberParam    = "__json_member"
	jsonPartParam      = "__json_part"
	jsonSeenParam      = "__json_seen"
	jsonSeenEntryParam = "__json_seen_entry"
)

// jsonMembersOf renders `JSONExtractKeysAndValuesRaw(<json>)` — the
// immediate members of one JSON object as `Array(Tuple(String, String))`
// with values left as raw JSON text. A non-object (or unparseable) input
// yields an empty array rather than an error, which is what lets a
// malformed body flow through the fold as "no extracted labels".
func jsonMembersOf(json chplan.Expr) chplan.Expr {
	return &chplan.FuncCall{Name: "JSONExtractKeysAndValuesRaw", Args: []chplan.Expr{json}}
}

func jsonPair(key, value chplan.Expr) chplan.Expr {
	return &chplan.FuncCall{Name: "tuple", Args: []chplan.Expr{key, value}}
}

func jsonTupleField(tuple chplan.Expr, index int64) chplan.Expr {
	return &chplan.FuncCall{
		Name: "tupleElement",
		Args: []chplan.Expr{tuple, &chplan.LitInt{V: index}},
	}
}

// The work-list pairs are (key, raw value); these name the two fields at
// the point of use so the callers below read as prose.
const (
	jsonPairKeyField   = 1
	jsonPairValueField = 2
)

func jsonMemberKey() chplan.Expr {
	return jsonTupleField(&chplan.BareIdent{Name: jsonMemberParam}, jsonPairKeyField)
}

func jsonMemberValue() chplan.Expr {
	return jsonTupleField(&chplan.BareIdent{Name: jsonMemberParam}, jsonPairValueField)
}

func jsonEntryKey() chplan.Expr {
	return jsonTupleField(&chplan.BareIdent{Name: jsonEntryParam}, jsonPairKeyField)
}

func jsonEntryValue() chplan.Expr {
	return jsonTupleField(&chplan.BareIdent{Name: jsonEntryParam}, jsonPairValueField)
}

// jsonExpandObjects is one fold step: every work-list entry whose raw
// value is an object is replaced by that object's members (their keys
// prefixed with the entry's key), and every other entry is passed
// through unchanged. `arrayFlatten` splices the two cases back into a
// single work list.
func jsonExpandObjects() chplan.Expr {
	expanded := &chplan.FuncCall{
		Name: "arrayMap",
		Args: []chplan.Expr{
			&chplan.Lambda{
				Params: []string{jsonMemberParam},
				Body: jsonPair(
					jsonJoinKey(jsonEntryKey(), jsonMemberKey()),
					jsonMemberValue(),
				),
			},
			jsonMembersOf(jsonEntryValue()),
		},
	}
	return &chplan.FuncCall{
		Name: "arrayFlatten",
		Args: []chplan.Expr{
			&chplan.FuncCall{
				Name: "arrayMap",
				Args: []chplan.Expr{
					&chplan.Lambda{
						Params: []string{jsonEntryParam},
						Body: &chplan.FuncCall{
							Name: "if",
							Args: []chplan.Expr{
								jsonStartsWith(jsonEntryValue(), jsonObjectPrefix),
								expanded,
								&chplan.FuncCall{
									Name: "array",
									Args: []chplan.Expr{&chplan.BareIdent{Name: jsonEntryParam}},
								},
							},
						},
					},
					&chplan.BareIdent{Name: jsonAccParam},
				},
			},
		},
	}
}

// jsonJoinKey appends one nested segment to an accumulated flattened
// key, mirroring upstream's buildSanitizedPrefixFromBuffer: parts that
// are empty after sanitisation are skipped rather than contributing a
// separator, and the surviving parts are joined with
// [jsonNestedKeySpacer]. Filtering an `[prefix, segment]` array states
// that skip rule directly and keeps both operands referenced once.
//
// The leading-digit rule is applied to the joined result — see
// [jsonKeyLeadingDigit] for why that is equivalent to upstream applying
// it only to the first surviving part.
func jsonJoinKey(prefix, rawSegment chplan.Expr) chplan.Expr {
	parts := &chplan.FuncCall{
		Name: "array",
		Args: []chplan.Expr{prefix, jsonSanitizeKey(rawSegment)},
	}
	nonEmpty := &chplan.FuncCall{
		Name: "arrayFilter",
		Args: []chplan.Expr{
			&chplan.Lambda{
				Params: []string{jsonPartParam},
				Body: &chplan.Binary{
					Op:    chplan.OpNe,
					Left:  &chplan.BareIdent{Name: jsonPartParam},
					Right: &chplan.LitString{V: ""},
				},
			},
			parts,
		},
	}
	return jsonApplyLeadingDigitRule(&chplan.FuncCall{
		Name: "arrayStringConcat",
		Args: []chplan.Expr{nonEmpty, &chplan.LitString{V: jsonNestedKeySpacer}},
	})
}

// jsonSanitizeKey renders Loki's sanitizeLabelKey for one raw key
// segment: trim surrounding whitespace, then rewrite every byte outside
// the label alphabet. The trim has to precede the rewrite, or the
// surrounding whitespace would survive as `_` bytes in the label name.
func jsonSanitizeKey(rawKey chplan.Expr) chplan.Expr {
	trimmed := &chplan.FuncCall{
		Name: "replaceRegexpAll",
		Args: []chplan.Expr{
			rawKey,
			&chplan.LitString{V: jsonKeyTrimWhitespace},
			&chplan.LitString{V: ""},
		},
	}
	return &chplan.FuncCall{
		Name: "replaceRegexpAll",
		Args: []chplan.Expr{
			trimmed,
			&chplan.LitString{V: jsonKeyInvalidCharClass},
			&chplan.LitString{V: jsonNestedKeySpacer},
		},
	}
}

func jsonApplyLeadingDigitRule(key chplan.Expr) chplan.Expr {
	return &chplan.FuncCall{
		Name: "replaceRegexpOne",
		Args: []chplan.Expr{
			key,
			&chplan.LitString{V: jsonKeyLeadingDigit},
			&chplan.LitString{V: jsonKeyLeadingDigitFix},
		},
	}
}

// jsonReadLeaves keeps the work-list entries that are extractable scalars
// and renders each value the way upstream's readValue does. Arrays,
// nulls, and any object left unexpanded are dropped — upstream's
// parseObject only ever emits String, Number, Boolean and Object, and an
// Object is never a leaf. An entry whose key sanitised to empty is
// dropped too, matching the `if sk == ""` guard on upstream's top-level
// scalar path.
func jsonReadLeaves(workList chplan.Expr) chplan.Expr {
	isScalar := &chplan.FuncCall{
		Name: "match",
		Args: []chplan.Expr{jsonEntryValue(), &chplan.LitString{V: jsonScalarLeafPrefix}},
	}
	keyPresent := &chplan.Binary{
		Op:    chplan.OpNe,
		Left:  jsonEntryKey(),
		Right: &chplan.LitString{V: ""},
	}
	kept := &chplan.FuncCall{
		Name: "arrayFilter",
		Args: []chplan.Expr{
			&chplan.Lambda{
				Params: []string{jsonEntryParam},
				Body:   &chplan.Binary{Op: chplan.OpAnd, Left: keyPresent, Right: isScalar},
			},
			workList,
		},
	}
	return &chplan.FuncCall{
		Name: "arrayMap",
		Args: []chplan.Expr{
			&chplan.Lambda{
				Params: []string{jsonEntryParam},
				Body:   jsonPair(jsonEntryKey(), jsonReadValue()),
			},
			kept,
		},
	}
}

// jsonReadValue unescapes a raw JSON string into its textual value and
// leaves every other scalar as the text it already is — numbers and
// booleans need no rewriting. `JSONExtractString` applied to a bare JSON
// string value performs exactly the unescape upstream's readValue does.
func jsonReadValue() chplan.Expr {
	return &chplan.FuncCall{
		Name: "if",
		Args: []chplan.Expr{
			jsonStartsWith(jsonEntryValue(), jsonStringPrefix),
			&chplan.FuncCall{Name: "JSONExtractString", Args: []chplan.Expr{jsonEntryValue()}},
			jsonEntryValue(),
		},
	}
}

func jsonStartsWith(e chplan.Expr, prefix string) chplan.Expr {
	return &chplan.FuncCall{
		Name: "startsWith",
		Args: []chplan.Expr{e, &chplan.LitString{V: prefix}},
	}
}

// lastKeyWins reduces a `(key, value)` array to one entry per key,
// keeping the LAST occurrence. Every text format a parser stage reads
// can repeat a key — a JSON object may state one twice, a nested JSON
// key may flatten onto the same label name as a sibling, a logfmt line
// may carry `dup=1 dup=2` — and in each case upstream assigns the pairs
// in turn, so the last write is the one left standing.
// ClickHouse's Map, by contrast, retains duplicate entries and resolves
// a lookup to the FIRST, so without this reduction the two disagree on
// exactly the documents where it matters.
//
// The reduction walks the list back to front, keeping an entry only if
// its key has not been kept already, then restores the original
// direction. Folding rather than filtering-by-index is what keeps the
// (large) input expression referenced once.
//
// The `__json_*` lambda parameter names are the ones the JSON flattening
// fold introduced, shared rather than duplicated: they name a pair and
// an accumulator, which is what every caller passes.
func lastKeyWins(pairs chplan.Expr) chplan.Expr {
	seen := &chplan.BareIdent{Name: jsonSeenParam}
	// A distinct parameter name: this lambda is nested inside the fold's
	// own `__json_entry` scope, and reusing the name would shadow it —
	// legal SQL, but it would read as though the `has(…)` needle and the
	// haystack keys came from the same value.
	seenKeys := &chplan.FuncCall{
		Name: "arrayMap",
		Args: []chplan.Expr{
			&chplan.Lambda{
				Params: []string{jsonSeenEntryParam},
				Body: jsonTupleField(
					&chplan.BareIdent{Name: jsonSeenEntryParam},
					jsonPairKeyField,
				),
			},
			seen,
		},
	}
	keepFirstUnseen := &chplan.Lambda{
		Params: []string{jsonSeenParam, jsonEntryParam},
		Body: &chplan.FuncCall{
			Name: "if",
			Args: []chplan.Expr{
				&chplan.FuncCall{Name: "has", Args: []chplan.Expr{seenKeys, jsonEntryKey()}},
				seen,
				&chplan.FuncCall{
					Name: "arrayPushBack",
					Args: []chplan.Expr{seen, &chplan.BareIdent{Name: jsonEntryParam}},
				},
			},
		},
	}
	return &chplan.FuncCall{
		Name: "arrayReverse",
		Args: []chplan.Expr{
			&chplan.FuncCall{
				Name: "arrayFold",
				Args: []chplan.Expr{
					keepFirstUnseen,
					&chplan.FuncCall{Name: "arrayReverse", Args: []chplan.Expr{pairs}},
					castToLabelPairArray(&chplan.FuncCall{Name: "array"}),
				},
			},
		},
	}
}

// packedEntryKey is the member Promtail's `pack` stage writes the
// original log line under. `| unpack` treats it as the line rather than
// as a label, and its presence is what arms the stage: an object without
// it contributes nothing at all.
const packedEntryKey = "_entry"

// JSONParserErrValue is the `__error__` value Loki stamps for every
// failure of a JSON-family parser stage, `| unpack` included. Exported
// because the SQL side is where it is now produced, and
// internal/api/loki's post-processing recognises rows carrying it.
const JSONParserErrValue = "JSONParserErr"

// UnexpectedJSONObjectDetail reproduces Loki's `__error_details__` text
// for a payload whose first byte is not `{`. Upstream builds it as
// `fmt.Errorf("expecting json object(%d), but it is not", jsoniter.ObjectValue)`
// and json-iterator's ObjectValue ordinal is 6, so the rendered text is
// part of the wire contract rather than an internal diagnostic.
const UnexpectedJSONObjectDetail = "expecting json object(6), but it is not"

// unpackMergeLabels lowers `| unpack` to a labels-map merge, so the
// stage's extracted keys are visible to everything downstream of the
// scan — label filters, and (the reason this exists) metric-mode
// `by (...)` grouping, which never runs a Go-side pass at all.
//
// The three rules that are easy to get backwards are all upstream's:
//
//   - The extraction is armed by a top-level string `_entry` member. A
//     well-formed object without one contributes NO labels, rather than
//     contributing its other members.
//   - Only string-valued members become labels; numbers, booleans,
//     arrays and nested objects are skipped. `| unpack` is deliberately
//     shallower than `| json`, which flattens nested objects.
//   - A payload that is not a readable JSON object is an ERROR, not a
//     silent pass-through: it carries `__error__` so that `| __error__=""`
//     — the usual way callers ask for "only the lines that parsed" —
//     excludes it instead of matching everything.
//
// The extracted keys go through [mergeParsedMap], so one that shadows a
// stream label lands under `<key>_extracted` and the stream label wins.
// The error markers are concatenated OUTSIDE that merge: they are the
// stage's own diagnostics rather than payload-derived keys, so renaming
// them on collision would hide them from the very filter that looks for
// them.
func unpackMergeLabels(prev chplan.Expr, s schema.Logs) chplan.Expr {
	return &chplan.FuncCall{
		Name: "mapConcat",
		Args: []chplan.Expr{
			concatParsedLabels(prev, UnpackParsedLabels(s)),
			unpackErrorMarkers(s),
		},
	}
}

// UnpackParsedLabels is the `| unpack` counterpart of
// [LogfmtParsedLabels] and [JSONParsedLabels]: the labels a packed
// payload contributes, with the collision rename applied and without the
// merge onto the running map. Same single-source-of-truth contract, so
// the field surface and the query path read one expression.
func UnpackParsedLabels(s schema.Logs) chplan.Expr {
	return renameExtractedOnCollision(s, unpackExtractedMap(s))
}

// unpackLineExpr renders the line `| unpack` yields: the unescaped
// `_entry` member when the payload is packed, and the original body
// otherwise — including on the error paths, where upstream returns the
// line untouched.
func unpackLineExpr(prev chplan.Expr, s schema.Logs) chplan.Expr {
	return &chplan.FuncCall{
		Name: "if",
		Args: []chplan.Expr{
			unpackIsPacked(s),
			&chplan.FuncCall{
				Name: "JSONExtractString",
				Args: []chplan.Expr{
					&chplan.ColumnRef{Name: s.BodyColumn},
					&chplan.LitString{V: packedEntryKey},
				},
			},
			prev,
		},
	}
}

// unpackExtractedMap renders the label set `| unpack` contributes, or an
// empty map when the payload is not packed. Keys are sanitised with the
// same rules the `| json` lowering uses, and `_entry` is excluded because
// it becomes the line rather than a label.
func unpackExtractedMap(s schema.Logs) chplan.Expr {
	labelMembers := &chplan.FuncCall{
		Name: "arrayFilter",
		Args: []chplan.Expr{
			&chplan.Lambda{
				Params: []string{jsonMemberParam},
				Body: &chplan.Binary{
					Op:   chplan.OpAnd,
					Left: jsonStartsWith(jsonMemberValue(), jsonStringPrefix),
					Right: &chplan.Binary{
						Op:    chplan.OpNe,
						Left:  jsonMemberKey(),
						Right: &chplan.LitString{V: packedEntryKey},
					},
				},
			},
			jsonMembersOf(&chplan.ColumnRef{Name: s.BodyColumn}),
		},
	}
	pairs := &chplan.FuncCall{
		Name: "arrayMap",
		Args: []chplan.Expr{
			&chplan.Lambda{
				Params: []string{jsonMemberParam},
				Body: jsonPair(
					jsonApplyLeadingDigitRule(jsonSanitizeKey(jsonMemberKey())),
					&chplan.FuncCall{
						Name: "JSONExtractString",
						Args: []chplan.Expr{jsonMemberValue()},
					},
				),
			},
			labelMembers,
		},
	}
	return &chplan.FuncCall{
		Name: "if",
		Args: []chplan.Expr{
			unpackIsPacked(s),
			castToLabelMap(lastKeyWins(pairs)),
			emptyLabelMap(),
		},
	}
}

// unpackIsPacked reports whether the body carries a top-level string
// `_entry` member — the gate that arms the whole stage.
func unpackIsPacked(s schema.Logs) chplan.Expr {
	stringMembers := &chplan.FuncCall{
		Name: "arrayFilter",
		Args: []chplan.Expr{
			&chplan.Lambda{
				Params: []string{jsonMemberParam},
				Body:   jsonStartsWith(jsonMemberValue(), jsonStringPrefix),
			},
			jsonMembersOf(&chplan.ColumnRef{Name: s.BodyColumn}),
		},
	}
	keys := &chplan.FuncCall{
		Name: "arrayMap",
		Args: []chplan.Expr{
			&chplan.Lambda{Params: []string{jsonMemberParam}, Body: jsonMemberKey()},
			stringMembers,
		},
	}
	return &chplan.FuncCall{
		Name: "has",
		Args: []chplan.Expr{keys, &chplan.LitString{V: packedEntryKey}},
	}
}

// unpackErrorMarkers renders the `__error__` / `__error_details__` pair
// for a payload `| unpack` cannot read, and an empty map otherwise.
//
// An empty body is not an error — upstream returns it unchanged — so the
// non-empty test comes first. Everything else that is not a readable JSON
// object is: a body whose first byte is not `{` carries upstream's exact
// sentinel text, and a `{`-prefixed body that does not parse carries the
// error label without a detail, because that detail is the Go JSON
// reader's own parse-position message and no property of the input
// yields it (see [internal/api/loki.unpackParseDetailStep], which
// supplies it on the log-stream path).
func unpackErrorMarkers(s schema.Logs) chplan.Expr {
	body := &chplan.ColumnRef{Name: s.BodyColumn}
	nonEmpty := &chplan.Binary{
		Op:    chplan.OpNe,
		Left:  body,
		Right: &chplan.LitString{V: ""},
	}
	// Two independent ways to not be a readable JSON object, and neither
	// test subsumes the other: `["a"]` is valid JSON that is not an object,
	// and `{"a":` is object-shaped but does not parse.
	notObjectShaped := &chplan.FuncCall{
		Name: "not",
		Args: []chplan.Expr{jsonStartsWith(body, jsonObjectPrefix)},
	}
	doesNotParse := &chplan.FuncCall{
		Name: "not",
		Args: []chplan.Expr{
			&chplan.FuncCall{Name: "isValidJSON", Args: []chplan.Expr{body}},
		},
	}
	unreadable := &chplan.Binary{
		Op:    chplan.OpOr,
		Left:  notObjectShaped,
		Right: doesNotParse,
	}
	markers := &chplan.FuncCall{
		Name: "map",
		Args: []chplan.Expr{
			&chplan.LitString{V: syntax.ErrorLabel},
			&chplan.LitString{V: JSONParserErrValue},
			&chplan.LitString{V: syntax.ErrorDetailsLabel},
			&chplan.FuncCall{
				Name: "if",
				Args: []chplan.Expr{
					jsonStartsWith(body, jsonObjectPrefix),
					&chplan.LitString{V: ""},
					&chplan.LitString{V: UnexpectedJSONObjectDetail},
				},
			},
		},
	}
	return &chplan.FuncCall{
		Name: "if",
		Args: []chplan.Expr{
			&chplan.Binary{Op: chplan.OpAnd, Left: nonEmpty, Right: unreadable},
			markers,
			emptyLabelMap(),
		},
	}
}

// labelMapType is the ClickHouse type every parser stage's extracted
// label set is cast to before it is merged onto the running labels map.
const labelMapType = "Map(String,String)"

// labelPairArrayType is the same label set in its ordered form. A
// stage's extracted pairs pass through it whenever their order carries
// meaning — which is whenever a key can repeat, because [lastKeyWins]
// resolves the repeat by position and a Map has already lost it.
const labelPairArrayType = "Array(Tuple(String,String))"

func castToLabelMap(pairs chplan.Expr) chplan.Expr {
	return &chplan.FuncCall{
		Name: "CAST",
		Args: []chplan.Expr{pairs, &chplan.LitString{V: labelMapType}},
	}
}

func castToLabelPairArray(pairs chplan.Expr) chplan.Expr {
	return &chplan.FuncCall{
		Name: "CAST",
		Args: []chplan.Expr{pairs, &chplan.LitString{V: labelPairArrayType}},
	}
}

// emptyLabelMap renders an empty Map(String,String). A bare `map()`
// would leave CH to infer the key and value types from nothing, which
// makes the surrounding `if` branches disagree on type.
func emptyLabelMap() chplan.Expr {
	return castToLabelMap(&chplan.FuncCall{Name: "array"})
}

// jsonExpressionMergeLabels wraps labelsExpr with a `mapConcat` that
// stitches in only the named JSON extractions (identifier => extracted
// value). Each expression is `<identifier>="<json-path>"`. The lowering
// parses each JSON path via the in-house jsonPathParse (matching Loki's
// supported syntax: dot-notation, `[index]` bracket, quoted keys) and
// renders `JSONExtractString(Body, <segment...>)` with one variadic
// argument per path segment — CH treats string segments as object keys
// and integer segments as array indexes, the same shape Loki's runtime
// expects. Each destination identifier goes through [mergeParsedFields],
// so one that collides with a stream label lands under
// `<id>_extracted` and the stream label wins.
func jsonExpressionMergeLabels(prev chplan.Expr, s schema.Logs, exprs []syntax.LabelExtractionExpr) (chplan.Expr, error) {
	if len(exprs) == 0 {
		// Defensive: a parser-emitted empty list is shaped like the
		// bare `| json` form. Treat it as such so we don't drop the
		// stage entirely.
		return jsonBareMergeLabels(prev, s), nil
	}
	fields := make([]parsedField, 0, len(exprs))
	for _, ext := range exprs {
		if ext.Identifier == "" {
			return nil, fmt.Errorf("logql: `| json` expression has empty identifier")
		}
		path := ext.Expression
		if path == "" {
			// Loki fills Expression == Identifier when the user writes
			// the bare-identifier form `| json foo`.
			path = ext.Identifier
		}
		extract, err := jsonExtractStringExpr(s, path)
		if err != nil {
			return nil, err
		}
		fields = append(fields, parsedField{name: ext.Identifier, value: extract})
	}
	return mergeParsedFields(prev, s, fields), nil
}

// jsonExtractStringExpr renders `JSONExtractString(Body, segment1,
// segment2, ...)` for a Loki JSON path string. Segments come from the
// jsonPathParse as `[]any` — strings for object keys, ints
// for array indexes. CH's JSONExtractString accepts that exact variadic
// shape natively.
func jsonExtractStringExpr(s schema.Logs, path string) (chplan.Expr, error) {
	segments, err := jsonPathParse(path)
	if err != nil {
		return nil, fmt.Errorf("logql: invalid `| json` path %q: %w", path, err)
	}
	args := make([]chplan.Expr, 0, len(segments)+1)
	args = append(args, &chplan.ColumnRef{Name: s.BodyColumn})
	for _, seg := range segments {
		switch v := seg.(type) {
		case string:
			args = append(args, &chplan.LitString{V: v})
		case int:
			args = append(args, &chplan.LitInt{V: int64(v)})
		default:
			return nil, fmt.Errorf("logql: unsupported JSON path segment type %T in %q", seg, path)
		}
	}
	return &chplan.FuncCall{Name: "JSONExtractString", Args: args}, nil
}

// regexpMergeLabels lowers a `| regexp "<pattern>"` stage to a label-map
// merge. The pattern is compiled in Go so we can discover the
// named-capture positions (Go's regexp/syntax matches RE2 — the same
// engine CH uses for extractAllGroupsHorizontal). Each named capture
// becomes a key in a `map(<name>, extractAllGroupsHorizontal(Body,
// <pattern>)[<i>][1], ...)` literal that gets mapConcat'd onto the
// running labels expression. The `[i][1]` indexing reaches into group
// `i`'s array of matches and picks the first — Loki's regexp parser
// records only the first match per group on each line. Each capture name
// goes through [mergeParsedFields], so one that collides with a stream
// label lands under `<name>_extracted` and the stream label wins.
func regexpMergeLabels(prev chplan.Expr, s schema.Logs, pattern string) (chplan.Expr, error) {
	if pattern == "" {
		return nil, fmt.Errorf("logql: `| regexp` requires a non-empty pattern")
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("logql: invalid `| regexp` pattern %q: %w", pattern, err)
	}
	type namedGroup struct {
		index int
		name  string
	}
	var named []namedGroup
	seen := map[string]struct{}{}
	for i, n := range re.SubexpNames() {
		if i == 0 || n == "" {
			continue
		}
		if _, dup := seen[n]; dup {
			return nil, fmt.Errorf("logql: `| regexp` pattern has duplicate named capture %q", n)
		}
		seen[n] = struct{}{}
		named = append(named, namedGroup{index: i, name: n})
	}
	if len(named) == 0 {
		return nil, fmt.Errorf("logql: `| regexp` pattern %q has no named captures", pattern)
	}
	groupsCall := func() *chplan.FuncCall {
		return &chplan.FuncCall{
			Name: "extractAllGroupsHorizontal",
			Args: []chplan.Expr{
				&chplan.ColumnRef{Name: s.BodyColumn},
				&chplan.LitString{V: pattern},
			},
		}
	}
	fields := make([]parsedField, 0, len(named))
	for _, g := range named {
		fields = append(fields, parsedField{
			name: g.name,
			// extractAllGroupsHorizontal(...)[<group>][1] — group i,
			// first match. CH 1-indexes both dimensions. Allocate a
			// fresh FuncCall per named capture so the chplan tree
			// stays free of shared sub-pointers an optimizer rule
			// might rewrite in place.
			value: &chplan.MapAccess{
				Map: &chplan.MapAccess{
					Map: groupsCall(),
					Key: &chplan.LitInt{V: int64(g.index)},
				},
				Key: &chplan.LitInt{V: 1},
			},
		})
	}
	return mergeParsedFields(prev, s, fields), nil
}

// labelFiltererLower handles `| label="val"` / `| label=~"regex"` and
// the boolean conjunctions Loki packs into BinaryLabelFilter, lowering
// one label-filterer tree to (predicate, `__error__` marks). The named
// label is resolved against labelsExpr — initially the schema's
// ResourceAttributes column, but after a `| logfmt` parser stage a
// `mapConcat(ResourceAttributes, extractKeyValuePairs(Body, ...))`
// wrapper so parsed keys are also visible. The schema is threaded so
// the synthesized `detected_level` label can short-circuit the
// MapAccess resolution and emit a SeverityText normalisation instead.
//
// The marks mirror reference Loki's per-row error
// stamping (pkg/logql/log/label_filter.go) including the engine's
// short-circuit reachability:
//
//   - `a or b` — BinaryLabelFilter.Process returns as soon as the left
//     side passes (which includes "left errored": an unparseable value
//     KEEPS the row), so the right side's marks only fire on rows where
//     the left predicate is false.
//   - `a , b` (and) — both sides always Process, so both sides' marks
//     are reachable; "don't overwrite" ordering (left first) is
//     preserved by mark order, which [wrapLabelsWithMarks] folds into a
//     first-match-wins multiIf.
//
// Duration, numeric, and bytes filters all produce marks: each parses
// its label value with a conversion that reference Loki keeps-and-marks
// on rejection (time.ParseDuration / strconv.ParseFloat /
// humanize.ParseBytes — see durationLabelFilterExpr /
// numericLabelFilterExpr / bytesLabelFilterExpr). String filters never
// error in reference Loki, so they contribute no mark.
func labelFiltererLower(lf syntax.LabelFilterer, s schema.Logs, labelsExpr chplan.Expr) (chplan.Expr, []labelFilterMark, error) {
	switch v := lf.(type) {
	case *syntax.StringLabelFilter:
		return labelMatcherToExpr(v.Matcher, s, labelsExpr), nil, nil
	case *syntax.BinaryLabelFilter:
		left, leftMarks, err := labelFiltererLower(v.Left, s, labelsExpr)
		if err != nil {
			return nil, nil, err
		}
		right, rightMarks, err := labelFiltererLower(v.Right, s, labelsExpr)
		if err != nil {
			return nil, nil, err
		}
		op := chplan.OpAnd
		if !v.And {
			op = chplan.OpOr
			// OR short-circuits in the reference engine: the right
			// side only runs (and only marks) when the left predicate
			// is false.
			gated := make([]labelFilterMark, 0, len(rightMarks))
			for _, m := range rightMarks {
				gated = append(gated, gateMark(m, notExpr(left)))
			}
			rightMarks = gated
		}
		return &chplan.Binary{Op: op, Left: left, Right: right},
			append(leftMarks, rightMarks...), nil
	case *syntax.NumericLabelFilter:
		pred, mark := numericLabelFilterExpr(v, s, labelsExpr)
		return pred, []labelFilterMark{mark}, nil
	case *syntax.DurationLabelFilter:
		pred, mark := durationLabelFilterExpr(v, s, labelsExpr)
		return pred, []labelFilterMark{mark}, nil
	case *syntax.BytesLabelFilter:
		pred, mark := bytesLabelFilterExpr(v, s, labelsExpr)
		return pred, []labelFilterMark{mark}, nil
	case *syntax.IPLabelFilter:
		// `| addr = ip("...")` / `!= ip("...")` — no marks: reference
		// Loki's IPLabelFilter never stamps `__error__` itself (its
		// only failure mode is an invalid pattern, rejected at
		// lowering). See internal/logql/ip.go.
		pred, err := ipLabelFilterExpr(v, labelsExpr)
		return pred, nil, err
	}
	return nil, nil, fmt.Errorf("logql: unsupported label filterer %T", lf)
}

// numericLabelFilterExpr lowers `| field > 5` / `>= 5` / `< 5` / `<= 5`
// / `= 5` / `!= 5` to a numeric comparison on the parsed Float64 value
// of the named label.
//
// Reference per-row semantics
// (pkg/logql/log/label_filter.go::(*NumericLabelFilter).Process, which
// calls strconv.ParseFloat) mirror the duration filter exactly:
//
//   - label absent → the row is DROPPED (predicate false), no error.
//   - label present but strconv.ParseFloat rejects the value → the row
//     is KEPT (predicate true) and `__error__="LabelFilterErr"` +
//     `__error_details__` are stamped — the returned mark carries that
//     stamping event for [wrapLabelsWithMarks].
//   - otherwise → compare the parsed value against the literal.
//
// The parse is the regex-gated [newNumericParse] shape so an
// unparseable value keeps-and-marks the row instead of silently
// falling through as 0 (the prior `toFloat64OrZero` behaviour diverged
// from reference, which stamps LabelFilterErr).
func numericLabelFilterExpr(f *syntax.NumericLabelFilter, s schema.Logs, labelsExpr chplan.Expr) (chplan.Expr, labelFilterMark) {
	access := structuredOrStreamLookupOnMap(s, labelsExpr, f.Name)
	parse := newNumericParse(access)
	exists := labelPresenceOnMap(s, labelsExpr, f.Name)
	pred := &chplan.FuncCall{
		Name: "multiIf",
		Args: []chplan.Expr{
			notExpr(exists), &chplan.LitBool{V: false},
			notExpr(parse.valid), &chplan.LitBool{V: true},
			&chplan.Binary{
				Op:    labelFilterOp(f.Type),
				Left:  parse.value,
				Right: &chplan.LitFloat{V: f.Value},
			},
		},
	}
	mark := labelFilterMark{
		cond: &chplan.Binary{
			Op:    chplan.OpAnd,
			Left:  exists,
			Right: notExpr(parse.valid),
		},
		kind:    errLabelFilterKind,
		details: parse.details,
	}
	return pred, mark
}

// durationLabelFilterExpr lowers `| field > 5s` and friends. Loki's
// parser has already converted the right-hand-side spec to a
// time.Duration; we compare the parsed-from-string seconds against the
// duration converted to seconds.
//
// Reference per-row semantics
// (pkg/logql/log/label_filter.go::(*DurationLabelFilter).Process):
//
//   - label absent → the row is DROPPED (predicate false), no error.
//   - label present but time.ParseDuration rejects the value → the row
//     is KEPT (predicate true) and `__error__="LabelFilterErr"` +
//     `__error_details__` are stamped — the returned mark carries that
//     stamping event for [wrapLabelsWithMarks].
//   - otherwise → compare parsed seconds against the literal.
//
// The parse itself is the regex-gated [newDurationParse] shape, NOT a
// bare `parseTimeDelta(...)`: CH's parseTimeDelta throws (code 36) on
// the first row it can't parse — including Go-valid shapes like
// `291.792µs` on CH 24.8 — aborting the whole query where reference
// Loki degrades per-row.
func durationLabelFilterExpr(f *syntax.DurationLabelFilter, s schema.Logs, labelsExpr chplan.Expr) (chplan.Expr, labelFilterMark) {
	access := structuredOrStreamLookupOnMap(s, labelsExpr, f.Name)
	parse := newDurationParse(access)
	exists := labelPresenceOnMap(s, labelsExpr, f.Name)
	pred := &chplan.FuncCall{
		Name: "multiIf",
		Args: []chplan.Expr{
			notExpr(exists), &chplan.LitBool{V: false},
			notExpr(parse.valid), &chplan.LitBool{V: true},
			&chplan.Binary{
				Op:    labelFilterOp(f.Type),
				Left:  parse.seconds,
				Right: &chplan.LitFloat{V: f.Value.Seconds()},
			},
		},
	}
	mark := labelFilterMark{
		cond: &chplan.Binary{
			Op:    chplan.OpAnd,
			Left:  exists,
			Right: notExpr(parse.valid),
		},
		kind:    errLabelFilterKind,
		details: parse.details,
	}
	return pred, mark
}

// bytesLabelFilterExpr lowers `| field > 1KB` and friends. Loki's
// parser has already converted the right-hand-side spec to a `uint64`
// byte count; we compare the parsed-from-string byte count against the
// literal.
//
// Reference per-row semantics
// (pkg/logql/log/label_filter.go::(*BytesLabelFilter).Process, which
// calls humanize.ParseBytes) mirror the duration filter exactly:
//
//   - label absent → the row is DROPPED (predicate false), no error.
//   - label present but humanize.ParseBytes rejects the value → the row
//     is KEPT (predicate true) and `__error__="LabelFilterErr"` +
//     `__error_details__` are stamped — the returned mark carries that
//     stamping event for [wrapLabelsWithMarks].
//   - otherwise → compare the parsed byte count against the literal.
//
// The parse is the regex-gated [newBytesParse] shape (replicating
// humanize.ParseBytes's number/unit split) so an unparseable value
// keeps-and-marks the row instead of silently falling through as 0 (the
// prior bare `parseReadableSize` behaviour diverged from reference,
// which stamps LabelFilterErr). The value branch still reads through
// `parseReadableSize`, which understands "1KB", "1MiB", "1.5G", etc.
func bytesLabelFilterExpr(f *syntax.BytesLabelFilter, s schema.Logs, labelsExpr chplan.Expr) (chplan.Expr, labelFilterMark) {
	access := structuredOrStreamLookupOnMap(s, labelsExpr, f.Name)
	parse := newBytesParse(access)
	exists := labelPresenceOnMap(s, labelsExpr, f.Name)
	pred := &chplan.FuncCall{
		Name: "multiIf",
		Args: []chplan.Expr{
			notExpr(exists), &chplan.LitBool{V: false},
			notExpr(parse.valid), &chplan.LitBool{V: true},
			&chplan.Binary{
				Op:    labelFilterOp(f.Type),
				Left:  parse.value,
				Right: &chplan.LitFloat{V: float64(f.Value)},
			},
		},
	}
	mark := labelFilterMark{
		cond: &chplan.Binary{
			Op:    chplan.OpAnd,
			Left:  exists,
			Right: notExpr(parse.valid),
		},
		kind:    errLabelFilterKind,
		details: parse.details,
	}
	return pred, mark
}

// labelFilterOp maps a LogQL LabelFilterType (the value-comparison enum
// for numeric / duration / bytes filters) onto a chplan BinaryOp.
func labelFilterOp(t syntax.LabelFilterType) chplan.BinaryOp {
	switch t {
	case syntax.LabelFilterEqual:
		return chplan.OpEq
	case syntax.LabelFilterNotEqual:
		return chplan.OpNe
	case syntax.LabelFilterGreaterThan:
		return chplan.OpGt
	case syntax.LabelFilterGreaterThanOrEqual:
		return chplan.OpGe
	case syntax.LabelFilterLesserThan:
		return chplan.OpLt
	case syntax.LabelFilterLesserThanOrEqual:
		return chplan.OpLe
	}
	return chplan.OpEq
}

// labelMatcherToExpr renders a Prometheus-style label Matcher against
// labelsExpr — the live "labels map" for the current point in the
// pipeline. Shared between StringLabelFilter and the short-circuit-
// friendly LineFilterLabelFilter (both embed the same *labels.Matcher).
//
// The synthesized `detected_level` label short-circuits the standard
// MapAccess resolution: instead of reading `<labels>[detected_level]`,
// the LHS becomes a `multiIf(...)` normalisation of SeverityText that
// matches upstream Loki's `normalizeLogLevel` mapping.
func labelMatcherToExpr(m *labels.Matcher, s schema.Logs, labelsExpr chplan.Expr) chplan.Expr {
	var lhs chplan.Expr
	if isDetectedLevelLabel(m.Name) {
		lhs = detectedLevelExpr(s)
	} else {
		lhs = structuredOrStreamLookupOnMap(s, labelsExpr, m.Name)
	}
	return &chplan.Binary{
		Op:    matchOp(m.Type),
		Left:  lhs,
		Right: &chplan.LitString{V: m.Value},
	}
}

// attributeLookupExpr returns the chplan.Expr that resolves an
// underscored Loki label `key` against the live `labelsMap` (the
// schema's ResourceAttributes column or a `mapConcat(...)` wrapping it
// after a parser stage). For names with no rewritable underscore (e.g.
// `job`, `__error__`) it returns a plain MapAccess — the byte-stable
// emit shape the fixtures match.
//
// For names with at least one rewritable underscore (e.g.
// `cerberus_ql`) it emits a left-associative `if(mapContains(m, k1),
// m[k1], m[k2])` chain over every candidate from
// [format.PromLabelToOTelCandidates]. CH's `Attributes['missing']`
// returns the value-type default (empty string) rather than NULL, so
// `coalesce` would short-circuit on the first lookup even when the
// row's actual key is the dotted form; `mapContains` cleanly
// distinguishes "present with empty value" from "absent".
//
// Mirrors `internal/promql/lower.go::attributeLookup`. The two heads
// share the [format.PromLabelToOTelCandidates] heuristic so a Grafana
// dashboard mixing Prom + Loki panels by `{cerberus_ql=...}` /
// `cerberus_ql{...}` gets symmetric resolution.
func attributeLookupExpr(labelsMap chplan.Expr, key string) chplan.Expr {
	if !format.PromLabelNeedsDottedFallback(key) {
		return &chplan.MapAccess{Map: labelsMap, Key: &chplan.LitString{V: key}}
	}
	// Dotted-fallback is meaningful only when `labelsMap` points at the
	// OTel-shaped ResourceAttributes column directly — that's where
	// keys can be stored in dotted form. After a parser stage (`|
	// logfmt`, `| json`) the labels map is a mapConcat(...) /
	// mapApply(...) wrapper whose extracted keys come from the log
	// payload, NOT from OTel semantic conventions, so dotted-fallback
	// would just duplicate the complex sub-expression three times for
	// no semantic gain. Restrict the fallback to bare ColumnRef
	// carriers; everything else takes the single-MapAccess fast path.
	if _, isBareCol := labelsMap.(*chplan.ColumnRef); !isBareCol {
		return &chplan.MapAccess{Map: labelsMap, Key: &chplan.LitString{V: key}}
	}
	candidates := format.PromLabelToOTelCandidates(key)
	if len(candidates) <= 1 {
		return &chplan.MapAccess{Map: labelsMap, Key: &chplan.LitString{V: key}}
	}
	return qlcommon.OTelDottedFallbackChain(labelsMap, candidates)
}

// attributeLookupColumn is the column-name convenience wrapper for
// attributeLookupExpr — re-uses the same dotted-fallback chain against
// a bare ColumnRef. Used by stream-selector matchers and the metric-
// form aggregation group-by where the labels map is always the
// schema's ResourceAttributes column.
func attributeLookupColumn(col, key string) chplan.Expr {
	return attributeLookupExpr(&chplan.ColumnRef{Name: col}, key)
}

// structuredOrStreamLookup resolves a bare LogQL label `key` for the
// OTel-CH logs mapping with reference-Loki LabelsBuilder precedence,
// restricted to the categories cerberus surfaces at the points it is
// called from (pipeline label-filters + by/without groupings — NOT
// stream-selector matchers).
//
// Reference Loki resolves a bare label across categories in the order
// parsed > structured-metadata > stream (pkg/logql/log/labels.go::
// LabelsBuilder.Get). cerberus stores those categories as:
//
//	stream labels        → ResourceAttributes map  (the index — what a
//	                       `{k="v"}` selector matches)
//	structured metadata  → LogAttributes map       (per-log-record
//	                       attributes)
//	top-level scalars    → dedicated columns (ServiceName, SeverityText,
//	                       ScopeName, TraceId, ...) — see
//	                       [topLevelLogColumnFor].
//
// There are no parsed labels at the call sites that route through this
// helper (parser stages thread their own `mapConcat(...)` labels map),
// so the effective precedence collapses to:
//
//	top-level scalar column  >  structured metadata (LogAttributes)
//	                         >  stream label (ResourceAttributes)
//
// Top-level columns are handled by the callers BEFORE this helper (they
// consult [topLevelLogColumnFor] first and short-circuit), so this
// helper only resolves the structured-metadata-over-stream tail. The
// emitted shape is
//
//	if(mapContains(LogAttributes, k),
//	   LogAttributes[k],
//	   <ResourceAttributes dotted-fallback chain>)
//
// i.e. structured metadata SHADOWS the stream label on a key conflict
// (matching Loki, where a structured-metadata value overrides a stream
// value of the same name), while a key present only in
// ResourceAttributes still resolves via the fallback (so existing
// stream-label / dashboard behaviour is preserved — regression-safe).
// A key present only in LogAttributes now resolves (the fix for task
// #59: OTel structured-metadata attributes like `query_duration_ms` /
// `query_kind` were previously invisible to `| k op v` filters and
// `by (k)` groupings).
//
// The `mapContains` guard (rather than a bare coalesce on the value)
// is deliberate: CH's `LogAttributes['missing']` returns the value-type
// default (empty string), so a plain `coalesce` could not distinguish
// "present with empty value" from "absent" — `mapContains` cleanly
// resolves the precedence even when the structured-metadata value is
// the empty string.
//
// Dotted-fallback ([attributeLookupExpr]) is applied to BOTH the
// LogAttributes side and the ResourceAttributes side, so a label like
// `cerberus_ql` whose OTel key is the dotted `cerberus.ql` resolves in
// either map.
//
// IMPORTANT — this does NOT touch stream-selector matchers. A selector
// `{k="v"}` matches the index (stream labels) only; reference Loki does
// NOT consult structured metadata in the selector. So [matcherToExpr] /
// [buildMatchersPredicate] keep resolving against ResourceAttributes
// alone — only pipeline label-filters and groupings coalesce.
func structuredOrStreamLookup(s schema.Logs, key string) chplan.Expr {
	streamSide := attributeLookupColumn(s.ResourceAttributesColumn, key)
	if s.AttributesColumn == "" {
		// Custom schema with no structured-metadata column: nothing to
		// coalesce, fall back to the stream-label lookup unchanged.
		return streamSide
	}
	structuredSide := attributeLookupColumn(s.AttributesColumn, key)
	return &chplan.FuncCall{
		Name: "if",
		Args: []chplan.Expr{
			structuredMetadataPresence(s, key),
			structuredSide,
			streamSide,
		},
	}
}

// structuredMetadataPresence returns the boolean "is `key` present in the
// structured-metadata (LogAttributes) map" guard for
// [structuredOrStreamLookup]. It mirrors [attributeLookupExpr]'s
// dotted-fallback candidate set: a label such as `cerberus_ql` is
// considered present when EITHER the underscored form OR any dotted OTel
// candidate (`cerberus.ql`) is a key in the map, so the value-side
// lookup and the presence guard agree on which candidate they read.
func structuredMetadataPresence(s schema.Logs, key string) chplan.Expr {
	col := &chplan.ColumnRef{Name: s.AttributesColumn}
	candidates := []string{key}
	if format.PromLabelNeedsDottedFallback(key) {
		candidates = format.PromLabelToOTelCandidates(key)
	}
	var out chplan.Expr
	for _, c := range candidates {
		has := &chplan.FuncCall{
			Name: "mapContains",
			Args: []chplan.Expr{col, &chplan.LitString{V: c}},
		}
		if out == nil {
			out = has
			continue
		}
		out = &chplan.Binary{Op: chplan.OpOr, Left: out, Right: has}
	}
	return out
}

// structuredOrStreamLookupOnMap is the labels-map-carrier variant of
// [structuredOrStreamLookup] used by the pipeline label-filter sites,
// where the live labels map (`labelsExpr`) may already be a
// parser-stage `mapConcat(...)` wrapper rather than the bare
// ResourceAttributes column.
//
//   - No parser stage ran (labelsExpr is the bare ResourceAttributes
//     column): resolve with full precedence
//     structured-metadata > stream via [structuredOrStreamLookup].
//   - A parser stage ran (labelsExpr is a mapConcat wrapper whose
//     extracted keys are the parsed labels, which already SHADOW the
//     stream labels): parsed labels must win over structured metadata
//     (reference precedence parsed > structured > stream), so resolve
//     the parsed/stream side from the live map first and only fall back
//     to structured metadata when the live map does NOT carry the key.
func structuredOrStreamLookupOnMap(s schema.Logs, labelsExpr chplan.Expr, key string) chplan.Expr {
	if col, ok := labelsExpr.(*chplan.ColumnRef); ok && col.Name == s.ResourceAttributesColumn {
		return structuredOrStreamLookup(s, key)
	}
	// Parser-merged labels map: parsed (and stream, folded into the
	// merge base) keys take precedence; structured metadata fills the
	// gap for keys the parser did not extract.
	parsedOrStream := attributeLookupExpr(labelsExpr, key)
	if s.AttributesColumn == "" {
		return parsedOrStream
	}
	return &chplan.FuncCall{
		Name: "if",
		Args: []chplan.Expr{
			liveMapPresence(labelsExpr, key),
			parsedOrStream,
			attributeLookupColumn(s.AttributesColumn, key),
		},
	}
}

// liveMapPresence returns the "is `key` present in the live labels map"
// guard for [structuredOrStreamLookupOnMap]'s parser-merged branch. The
// live map already folds stream labels (the merge base) under any
// parser-extracted keys, so presence here means "the parsed-or-stream
// resolution has a value" — and only its absence defers to structured
// metadata.
func liveMapPresence(labelsExpr chplan.Expr, key string) chplan.Expr {
	return &chplan.FuncCall{
		Name: "mapContains",
		Args: []chplan.Expr{labelsExpr, &chplan.LitString{V: key}},
	}
}

// labelPresenceOnMap returns the "is `key` resolvable as a label at this
// point in the pipeline" guard, with the SAME category coverage as
// [structuredOrStreamLookupOnMap]: present in the live labels map
// (stream label or parser-extracted) OR present in the structured-
// metadata (LogAttributes) map. Used by [durationLabelFilterExpr], where
// reference Loki DROPS the row when the named label is absent — and a
// structured-metadata-only key must now count as present so the
// duration comparison runs against its value rather than the row being
// dropped.
func labelPresenceOnMap(s schema.Logs, labelsExpr chplan.Expr, key string) chplan.Expr {
	live := liveMapPresence(labelsExpr, key)
	if s.AttributesColumn == "" {
		return live
	}
	// Present in the live map (stream label, or parser-extracted when a
	// parser stage wrapped it) OR present in structured metadata.
	return &chplan.Binary{
		Op:    chplan.OpOr,
		Left:  live,
		Right: structuredMetadataPresence(s, key),
	}
}

// lowerLineFilter handles `|=`, `!=`, `|~`, `!~` against the Body column.
//
// Loki packs chained line filters in the same pipeline into one
// `LineFilterExpr`: `Left` walks the previous filter (older chained
// clauses) and `Or` walks alternates joined by `or`. We AND the Left
// chain and OR the Or chain so the final predicate matches Loki's
// evaluation order.
func lowerLineFilter(f *syntax.LineFilterExpr, s schema.Logs) (chplan.Expr, error) {
	body := &chplan.ColumnRef{Name: s.BodyColumn}
	return lowerLineFilterChain(f, body)
}

func lowerLineFilterChain(f *syntax.LineFilterExpr, body chplan.Expr) (chplan.Expr, error) {
	current, err := lineFilterPart(&f.LineFilter, body)
	if err != nil {
		return nil, err
	}
	// `or` alternates fold into a disjunction with the head clause.
	for or := f.Or; or != nil; or = or.Or {
		next, err := lineFilterPart(&or.LineFilter, body)
		if err != nil {
			return nil, err
		}
		current = &chplan.Binary{Op: chplan.OpOr, Left: current, Right: next}
	}
	// Older filters in the same pipeline live on `Left`. AND them in.
	if f.Left != nil {
		prev, err := lowerLineFilterChain(f.Left, body)
		if err != nil {
			return nil, err
		}
		current = &chplan.Binary{Op: chplan.OpAnd, Left: prev, Right: current}
	}
	return current, nil
}

func lineFilterPart(lf *syntax.LineFilter, body chplan.Expr) (chplan.Expr, error) {
	if lf.Op == syntax.OpFilterIP {
		// `|= ip("192.168.0.0/16")` matches lines containing an IP
		// inside the CIDR / range / single-IP match set — see
		// internal/logql/ip.go for the reference-semantics walk-through.
		return ipLineFilterExpr(lf, body)
	}
	// `|>` / `!>` pattern filters carry Loki's pattern syntax
	// (literals + `<_>` wildcards) — see internal/logql/
	// pattern_filter.go for the reference-semantics walk-through.
	switch lf.Ty {
	case syntax.LineMatchPattern:
		return patternLineFilterExpr(lf.Match, false, body)
	case syntax.LineMatchNotPattern:
		return patternLineFilterExpr(lf.Match, true, body)
	}
	isRegex, negated, err := lineFilterOp(lf.Ty)
	if err != nil {
		return nil, err
	}
	return &chplan.LineContent{
		Source:  body,
		Pattern: lf.Match,
		IsRegex: isRegex,
		Negated: negated,
	}, nil
}

func lineFilterOp(t syntax.LineMatchType) (isRegex, negated bool, err error) {
	switch t {
	case syntax.LineMatchEqual:
		return false, false, nil
	case syntax.LineMatchNotEqual:
		return false, true, nil
	case syntax.LineMatchRegexp:
		return true, false, nil
	case syntax.LineMatchNotRegexp:
		return true, true, nil
	}
	return false, false, fmt.Errorf("logql: unknown line-filter match type %s", t)
}

// SelectorPredicate is the exported entry point for callers that need
// just the stream-selector predicate without lowering the full
// expression — e.g. the /index/stats and /index/volume handlers, which
// only care about the matchers, not the pipeline stages.
//
// Returns nil if matchers is empty.
func SelectorPredicate(matchers []*labels.Matcher, s schema.Logs) chplan.Expr {
	return buildMatchersPredicate(matchers, s)
}

// buildMatchersPredicate AND-folds the stream-selector matchers into a
// chplan.Expr. Each matcher targets `ResourceAttributes[<label>]`.
func buildMatchersPredicate(matchers []*labels.Matcher, s schema.Logs) chplan.Expr {
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

func matcherToExpr(m *labels.Matcher, s schema.Logs) chplan.Expr {
	lhs := matcherLHS(m.Name, s)
	return &chplan.Binary{
		Op:    matchOp(m.Type),
		Left:  lhs,
		Right: &chplan.LitString{V: m.Value},
	}
}

// matcherLHS resolves the left-hand side expression a stream-selector
// matcher on `label` compares against, in reference-Loki precedence:
//
//  1. the `detected_level` family → the SeverityText-derived expression;
//  2. an exporter-MATERIALIZED k8s.* resource column → a bare ColumnRef.
//     The column's MATERIALIZED expression IS `ResourceAttributes[<key>]`,
//     so the bare reference is byte-for-byte equivalent to the map access
//     (including the empty-string default a missing key yields) while
//     avoiding the wide ResourceAttributes Map decompression. The k8s.*
//     keys are disjoint from the top-level scalar columns
//     (SeverityText / ServiceName / …), so this never collides with the
//     resourceFallbackColumn coalesce below;
//  3. a top-level scalar column (ServiceName / SeverityText / …) →
//     `coalesce(nullIf(<col>, ”), ResourceAttributes[<label>])`;
//  4. otherwise the plain `ResourceAttributes[<label>]` map access.
func matcherLHS(label string, s schema.Logs) chplan.Expr {
	if isDetectedLevelLabel(label) {
		return detectedLevelExpr(s)
	}
	if matCol, ok := materializedColumnFor(label, s); ok {
		return &chplan.ColumnRef{Name: matCol}
	}
	mapLookup := attributeLookupColumn(s.ResourceAttributesColumn, label)
	if col := resourceFallbackColumn(s, label); col != "" {
		return resourceAttributeFallbackLHS(col, mapLookup)
	}
	return mapLookup
}

// resourceFallbackColumn returns the dedicated top-level CH column name
// that mirrors a Prom/Loki resource-attribute label, or "" if the label
// has no such fallback. The OTel ClickHouse Exporter hoists a fixed set
// of OTel semantic-convention resource attributes out of the
// ResourceAttributes map into named columns (most prominently
// `service.name` → `ServiceName`, but also SeverityText, SeverityNumber,
// ScopeName, ScopeVersion, EventName, TraceId, SpanId, TraceFlags — every
// scalar top-level column the OTel-CH default schema dedicates). Rows
// ingested through that path carry the value ONLY in the top-level
// column, leaving `ResourceAttributes[<label>]` empty. A matcher
// lowering that reads from the map alone misses every such row.
//
// Task #240 widened the fallback from `service_name` only (initial #217
// fix) to the full top-level scalar set: `{SeverityText="DEBUG"}` and
// the matcher-path twins on the other 8 columns were silently returning
// zero rows because their values lived in the top-level columns while
// the matcher consulted the empty ResourceAttributes map. The
// group-by path (`levelAwareGroupKey`) had already routed all 9 columns
// via [topLevelLogColumnFor] when #218 was fixed; this helper now
// delegates to the same table so the two parse paths can't drift again.
//
// Custom-schema users who clear the corresponding schema.Logs field
// (e.g. `ServiceNameColumn=""`) opt out: the helper returns "" so the
// lowering stays map-only.
func resourceFallbackColumn(s schema.Logs, labelName string) string {
	if col, ok := topLevelLogColumnFor(labelName, s); ok {
		return col
	}
	// Prom/Loki convention spells `service.name` as the OTel-CH
	// `ServiceName` column's stream attribute as `service_name`. The
	// other 8 top-level columns surface under their literal column
	// name (no Prom/Loki underscore alias is in idiomatic use), but
	// `service_name` is the form Grafana panels + the
	// `matcher_self_service` fixture both emit, so this alias stays.
	if labelName == "service_name" {
		return s.ServiceNameColumn
	}
	return ""
}

// resourceAttributeFallbackLHS wraps a ResourceAttributes-lookup chain
// in a coalesce that prefers the dedicated top-level column when it
// carries a non-empty value. The CH idiom is
// `coalesce(nullIf(<col>, ”), <map lookup>)`: `nullIf` rewrites the
// String-default-empty sentinel back to NULL so `coalesce` selects the
// map fallback when the top-level column was unpopulated. The full
// shape lands on the lhs of every match operator (=, !=, =~, !~) so the
// matcher's logical contract holds regardless of which storage shape
// the row used — both presence and ABSENCE of `service.name=cerberus`
// resolve correctly when the producer wrote it to either side.
func resourceAttributeFallbackLHS(topCol string, mapLookup chplan.Expr) chplan.Expr {
	return &chplan.FuncCall{
		Name: "coalesce",
		Args: []chplan.Expr{
			&chplan.FuncCall{
				Name: "nullIf",
				Args: []chplan.Expr{
					&chplan.ColumnRef{Name: topCol},
					&chplan.LitString{V: ""},
				},
			},
			mapLookup,
		},
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
	return chplan.OpEq
}

// andFoldTimeWindow AND-folds a `<TimestampColumn> >= start AND
// <TimestampColumn> <= end` predicate onto pred when the lowering context
// carries a non-zero window. The bounds render as
// `toDateTime64('YYYY-MM-DD HH:MM:SS.fffffffff', 9)` so the placeholders
// land on the DateTime64(9) Timestamp column without an implicit
// conversion. Mirror of the prom-side anchor rendering in
// internal/promql/modifiers.go::anchorBaseExpr.
func andFoldTimeWindow(pred chplan.Expr, s schema.Logs, lc lowerCtx) chplan.Expr {
	if !lc.hasTimeWindow() {
		return pred
	}
	tsCol := &chplan.ColumnRef{Name: s.TimestampColumn}
	lowerBound := &chplan.Binary{
		Op:    chplan.OpGe,
		Left:  tsCol,
		Right: timeLiteralExpr(lc.Start),
	}
	upperBound := &chplan.Binary{
		Op:    chplan.OpLe,
		Left:  tsCol,
		Right: timeLiteralExpr(lc.End),
	}
	window := &chplan.Binary{Op: chplan.OpAnd, Left: lowerBound, Right: upperBound}
	if pred == nil {
		return window
	}
	return &chplan.Binary{Op: chplan.OpAnd, Left: pred, Right: window}
}

// timeLiteralExpr renders an absolute timestamp as a CH DateTime64(9)
// literal. The format string mirrors prom's anchorBaseExpr so the two
// paths emit identical placeholder shapes.
func timeLiteralExpr(t time.Time) chplan.Expr {
	return &chplan.FuncCall{
		Name: "toDateTime64",
		Args: []chplan.Expr{
			&chplan.LitString{V: t.UTC().Format("2006-01-02 15:04:05.000000000")},
			&chplan.LitInt{V: chplan.NanoScale},
		},
	}
}
