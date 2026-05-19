// Package logql lowers Loki LogQL queries into the shared cerberus chplan
// IR. The seed (M3.1) covers stream selectors with `=`/`!=`/`=~`/`!~`
// label matchers and the line-filter family (`|=`, `!=`, `|~`, `!~`).
//
// Subsequent milestones add label filters (`| label="v"`), parsers
// (`| json`, `| logfmt`), the metric form (rate, count_over_time, ...),
// and aggregations.
package logql

import (
	"context"
	"fmt"
	"regexp"
	"time"

	loglib "github.com/grafana/loki/v3/pkg/logql/log"
	"github.com/grafana/loki/v3/pkg/logql/log/jsonexpr"
	"github.com/grafana/loki/v3/pkg/logql/syntax"
	"github.com/prometheus/prometheus/model/labels"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"github.com/tsouza/cerberus/internal/cerbtrace"
	"github.com/tsouza/cerberus/internal/chplan"
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
// A non-positive extension is a no-op so callers can pass `Interval -
// Offset` without checking for zero. An extension stays clamped to the
// query's actual range — instant queries (Step == 0) and bare matcher
// queries still emit the unmodified clamp.
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
// the stream label wins and parsed gets `_extracted` suffix". For
// `| logfmt` cerberus enforces that on the SQL side: the merge wraps
// extracted keys in a `mapApply` (or, for typed `| logfmt foo="..."`,
// a per-identifier `if(...)`) that suffixes the destination name when
// the stream column already carries it. See [logfmtMergeLabels] and
// [logfmtExpressionMergeLabels] for the exact lowering shape. Strict
// conflict semantics for the other parser families (`| json`,
// `| regexp`) remain a known approximation — extracted keys win on
// conflict there — and are tracked separately.
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
	for _, stage := range e.MultiStages {
		next, newLabels, err := lowerStage(stage, s, labelsExpr)
		if err != nil {
			return nil, nil, err
		}
		if newLabels != nil {
			labelsExpr = newLabels
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
		case syntax.OpParserTypeUnpack, syntax.OpParserTypePattern:
			return nil, nil
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
	}
	return nil, nil
}

// HasParserStage reports whether the parsed LogQL expression contains a
// parser stage (`| logfmt`, `| json`, `| regexp ...`, typed-variants)
// that the SQL lowering folds into the labels map. Used by
// [Lang.ProjectSamples] to gate the parser-extracted labels surface —
// when true, the projection uses [PipelineLabelsExpr]'s output for the
// Attributes column so per-row labels include extracted keys.
//
// `| unpack` and `| pattern` return false: those parsers extract their
// labels in Go after the rows return (see post_process.go), not in SQL,
// so the SQL projection has nothing to surface for them — the
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
			case syntax.OpParserTypeJSON, syntax.OpParserTypeRegexp:
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
		p, err := lowerLabelFilter(st, s, labelsExpr)
		return p, nil, err
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
		case syntax.OpParserTypeUnpack, syntax.OpParserTypePattern:
			return nil, nil, nil
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
		// Strict / KeepEmpty flags are intentionally ignored for now:
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
	return &chplan.FuncCall{
		Name: "mapConcat",
		Args: []chplan.Expr{
			prev,
			renameExtractedOnCollision(s, extractKVPairs(s)),
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
func logfmtExpressionMergeLabels(prev chplan.Expr, s schema.Logs, exprs []loglib.LabelExtractionExpr) (chplan.Expr, error) {
	if len(exprs) == 0 {
		// Defensive: a parser-emitted empty list is shaped like the
		// bare `| logfmt` form. Treat it as such so we don't drop the
		// stage entirely.
		return logfmtMergeLabels(prev, s), nil
	}
	kvBase := extractKVPairs(s)
	args := make([]chplan.Expr, 0, len(exprs)*2)
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
		args = append(
			args,
			renameIdentifierOnCollision(s, ext.Identifier),
			&chplan.MapAccess{
				Map: kvBase,
				Key: &chplan.LitString{V: key},
			},
		)
	}
	return &chplan.FuncCall{
		Name: "mapConcat",
		Args: []chplan.Expr{
			prev,
			&chplan.FuncCall{Name: "map", Args: args},
		},
	}, nil
}

// renameExtractedOnCollision wraps a Map(String,String) expression with
// a `mapApply` that renames any key that already exists in the stream's
// label set. The CH shape is
//
//	mapApply(
//	    (k, v) -> (if(mapContains(<stream>, k), concat(k, '_extracted'), k), v),
//	    <extracted>)
//
// Used by the bare `| logfmt` form where the extracted-key set is
// unknown at SQL-emit time, so the rename has to happen per-key inside
// the lambda.
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
// contain `<id>`) or `<id>_extracted` (when it does). Used by typed
// `| logfmt foo="..."` lowering where each destination identifier is
// known statically — the rename is a per-key `if(mapContains(<stream>,
// '<id>'), '<id>_extracted', '<id>')` evaluated once per row instead
// of via mapApply.
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
	return &chplan.FuncCall{
		Name: "extractKeyValuePairs",
		Args: []chplan.Expr{
			&chplan.ColumnRef{Name: s.BodyColumn},
			&chplan.LitString{V: "="},
			&chplan.LitString{V: " "},
			&chplan.LitString{V: "\""},
		},
	}
}

// jsonBareMergeLabels wraps the current labelsExpr with a
// `mapConcat(<prev>, CAST(JSONExtractKeysAndValues(Body, 'String') AS
// Map(String,String)))` so subsequent label filters see the union of
// stream-selector labels and JSON-parsed top-level key/value pairs.
//
// JSONExtractKeysAndValues(json, 'String') returns
// `Array(Tuple(String, String))` for the top-level object keys with each
// value cast to String. CAST to Map(String, String) gives the same shape
// the rest of the pipeline expects (mirrors the `| logfmt` lowering).
// Nested objects stringify to their JSON form rather than flattening to
// `parent_child` keys — that's an approximation of Loki's bare `| json`
// semantics; the common flat-object case is exact.
func jsonBareMergeLabels(prev chplan.Expr, s schema.Logs) chplan.Expr {
	return &chplan.FuncCall{
		Name: "mapConcat",
		Args: []chplan.Expr{
			prev,
			&chplan.FuncCall{
				Name: "CAST",
				Args: []chplan.Expr{
					&chplan.FuncCall{
						Name: "JSONExtractKeysAndValues",
						Args: []chplan.Expr{
							&chplan.ColumnRef{Name: s.BodyColumn},
							&chplan.LitString{V: "String"},
						},
					},
					&chplan.LitString{V: "Map(String,String)"},
				},
			},
		},
	}
}

// jsonExpressionMergeLabels wraps labelsExpr with a `mapConcat` that
// stitches in only the named JSON extractions (identifier => extracted
// value). Each expression is `<identifier>="<json-path>"`. The lowering
// parses each JSON path via Loki's own jsonexpr parser (matching Loki's
// supported syntax: dot-notation, `[index]` bracket, quoted keys) and
// renders `JSONExtractString(Body, <segment...>)` with one variadic
// argument per path segment — CH treats string segments as object keys
// and integer segments as array indexes, the same shape Loki's runtime
// expects.
func jsonExpressionMergeLabels(prev chplan.Expr, s schema.Logs, exprs []loglib.LabelExtractionExpr) (chplan.Expr, error) {
	if len(exprs) == 0 {
		// Defensive: a parser-emitted empty list is shaped like the
		// bare `| json` form. Treat it as such so we don't drop the
		// stage entirely.
		return jsonBareMergeLabels(prev, s), nil
	}
	args := make([]chplan.Expr, 0, len(exprs)*2)
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
		args = append(
			args,
			&chplan.LitString{V: ext.Identifier},
			extract,
		)
	}
	return &chplan.FuncCall{
		Name: "mapConcat",
		Args: []chplan.Expr{
			prev,
			&chplan.FuncCall{Name: "map", Args: args},
		},
	}, nil
}

// jsonExtractStringExpr renders `JSONExtractString(Body, segment1,
// segment2, ...)` for a Loki JSON path string. Segments come from the
// jsonexpr parser as `[]interface{}` — strings for object keys, ints
// for array indexes. CH's JSONExtractString accepts that exact variadic
// shape natively.
func jsonExtractStringExpr(s schema.Logs, path string) (chplan.Expr, error) {
	segments, err := jsonexpr.Parse(path, false)
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
// records only the first match per group on each line.
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
	mapArgs := make([]chplan.Expr, 0, len(named)*2)
	for _, g := range named {
		mapArgs = append(
			mapArgs,
			&chplan.LitString{V: g.name},
			// extractAllGroupsHorizontal(...)[<group>][1] — group i,
			// first match. CH 1-indexes both dimensions. Allocate a
			// fresh FuncCall per named capture so the chplan tree
			// stays free of shared sub-pointers an optimizer rule
			// might rewrite in place.
			&chplan.MapAccess{
				Map: &chplan.MapAccess{
					Map: groupsCall(),
					Key: &chplan.LitInt{V: int64(g.index)},
				},
				Key: &chplan.LitInt{V: 1},
			},
		)
	}
	return &chplan.FuncCall{
		Name: "mapConcat",
		Args: []chplan.Expr{
			prev,
			&chplan.FuncCall{Name: "map", Args: mapArgs},
		},
	}, nil
}

// lowerLabelFilter handles `| label="val"` / `| label=~"regex"` and the
// boolean conjunctions Loki packs into BinaryLabelFilter. The named
// label is resolved against labelsExpr — initially the schema's
// ResourceAttributes column, but after a `| logfmt` parser stage a
// `mapConcat(ResourceAttributes, extractKeyValuePairs(Body, ...))`
// wrapper so parsed keys are also visible. The schema is threaded so
// the synthesized `detected_level` label can short-circuit the
// MapAccess resolution and emit a SeverityText normalisation instead.
func lowerLabelFilter(f *syntax.LabelFilterExpr, s schema.Logs, labelsExpr chplan.Expr) (chplan.Expr, error) {
	return labelFiltererToExpr(f.LabelFilterer, s, labelsExpr)
}

func labelFiltererToExpr(lf loglib.LabelFilterer, s schema.Logs, labelsExpr chplan.Expr) (chplan.Expr, error) {
	switch v := lf.(type) {
	case *loglib.StringLabelFilter:
		return labelMatcherToExpr(v.Matcher, s, labelsExpr), nil
	case *loglib.LineFilterLabelFilter:
		// Loki may wrap a string label filter in this when a line-filter
		// short-circuit is also possible. Both embed *labels.Matcher and
		// behave identically for our query-rewrite purposes.
		return labelMatcherToExpr(v.Matcher, s, labelsExpr), nil
	case *loglib.BinaryLabelFilter:
		left, err := labelFiltererToExpr(v.Left, s, labelsExpr)
		if err != nil {
			return nil, err
		}
		right, err := labelFiltererToExpr(v.Right, s, labelsExpr)
		if err != nil {
			return nil, err
		}
		op := chplan.OpAnd
		if !v.And {
			op = chplan.OpOr
		}
		return &chplan.Binary{Op: op, Left: left, Right: right}, nil
	case *loglib.NumericLabelFilter:
		return numericLabelFilterExpr(v, labelsExpr), nil
	case *loglib.DurationLabelFilter:
		return durationLabelFilterExpr(v, labelsExpr), nil
	case *loglib.BytesLabelFilter:
		return bytesLabelFilterExpr(v, labelsExpr), nil
	}
	return nil, fmt.Errorf("logql: unsupported label filterer %T", lf)
}

// numericLabelFilterExpr lowers `| field > 5` / `>= 5` / `< 5` / `<= 5`
// / `= 5` / `!= 5` to a numeric comparison on the parsed Float64 value
// of the named label. The label value is interpreted via
// `toFloat64OrZero(labelsExpr['<name>'])` — matching Loki's runtime
// `strconv.ParseFloat` shape — and compared with the literal value at
// CH evaluation time so a non-parseable value falls through as 0.
func numericLabelFilterExpr(f *loglib.NumericLabelFilter, labelsExpr chplan.Expr) chplan.Expr {
	lhs := &chplan.FuncCall{
		Name: "toFloat64OrZero",
		Args: []chplan.Expr{&chplan.MapAccess{
			Map: labelsExpr,
			Key: &chplan.LitString{V: f.Name},
		}},
	}
	return &chplan.Binary{
		Op:    labelFilterOp(f.Type),
		Left:  lhs,
		Right: &chplan.LitFloat{V: f.Value},
	}
}

// durationLabelFilterExpr lowers `| field > 5s` and friends. Loki's
// parser has already converted the right-hand-side spec to a
// time.Duration; we compare the parsed-from-string seconds against the
// duration converted to seconds. The label string is parsed with CH's
// `parseTimeDelta` which understands Loki's Go-duration shape ("1.5s",
// "200ms", "1m30s", "1h"). `parseTimeDelta` returns Float64 seconds —
// matching the units of the duration literal we emit.
func durationLabelFilterExpr(f *loglib.DurationLabelFilter, labelsExpr chplan.Expr) chplan.Expr {
	lhs := &chplan.FuncCall{
		Name: "parseTimeDelta",
		Args: []chplan.Expr{&chplan.MapAccess{
			Map: labelsExpr,
			Key: &chplan.LitString{V: f.Name},
		}},
	}
	return &chplan.Binary{
		Op:    labelFilterOp(f.Type),
		Left:  lhs,
		Right: &chplan.LitFloat{V: f.Value.Seconds()},
	}
}

// bytesLabelFilterExpr lowers `| field > 1KB` and friends. Loki's
// parser has already converted the right-hand-side spec to a `uint64`
// byte count; we compare the parsed-from-string byte count against the
// literal. The label string is parsed with CH's `parseReadableSize`
// which understands "1KB", "1MiB", "1.5G", etc. — covering the
// `humanize.ParseBytes` shape Loki's runtime uses. `parseReadableSize`
// returns Float64 bytes; we cast the right-hand-side accordingly.
func bytesLabelFilterExpr(f *loglib.BytesLabelFilter, labelsExpr chplan.Expr) chplan.Expr {
	lhs := &chplan.FuncCall{
		Name: "parseReadableSize",
		Args: []chplan.Expr{&chplan.MapAccess{
			Map: labelsExpr,
			Key: &chplan.LitString{V: f.Name},
		}},
	}
	return &chplan.Binary{
		Op:    labelFilterOp(f.Type),
		Left:  lhs,
		Right: &chplan.LitFloat{V: float64(f.Value)},
	}
}

// labelFilterOp maps a LogQL LabelFilterType (the value-comparison enum
// for numeric / duration / bytes filters) onto a chplan BinaryOp.
func labelFilterOp(t loglib.LabelFilterType) chplan.BinaryOp {
	switch t {
	case loglib.LabelFilterEqual:
		return chplan.OpEq
	case loglib.LabelFilterNotEqual:
		return chplan.OpNe
	case loglib.LabelFilterGreaterThan:
		return chplan.OpGt
	case loglib.LabelFilterGreaterThanOrEqual:
		return chplan.OpGe
	case loglib.LabelFilterLesserThan:
		return chplan.OpLt
	case loglib.LabelFilterLesserThanOrEqual:
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
		lhs = &chplan.MapAccess{
			Map: labelsExpr,
			Key: &chplan.LitString{V: m.Name},
		}
	}
	return &chplan.Binary{
		Op:    matchOp(m.Type),
		Left:  lhs,
		Right: &chplan.LitString{V: m.Value},
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

func lineFilterOp(t loglib.LineMatchType) (isRegex, negated bool, err error) {
	switch t {
	case loglib.LineMatchEqual:
		return false, false, nil
	case loglib.LineMatchNotEqual:
		return false, true, nil
	case loglib.LineMatchRegexp:
		return true, false, nil
	case loglib.LineMatchNotRegexp:
		return true, true, nil
	}
	return false, false, fmt.Errorf("logql: line-filter op %s is not yet supported (`|>` pattern filters land in M3.2)", t)
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
	var lhs chplan.Expr
	if isDetectedLevelLabel(m.Name) {
		lhs = detectedLevelExpr(s)
	} else {
		lhs = &chplan.MapAccess{
			Map: &chplan.ColumnRef{Name: s.ResourceAttributesColumn},
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
			&chplan.LitInt{V: 9},
		},
	}
}
