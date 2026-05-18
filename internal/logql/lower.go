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
	"time"

	loglib "github.com/grafana/loki/v3/pkg/logql/log"
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
type lowerCtx struct {
	Start time.Time
	End   time.Time
}

// hasTimeWindow reports whether the context carries a non-degenerate
// [Start, End] pair to inject as a BETWEEN predicate.
func (c lowerCtx) hasTimeWindow() bool {
	return !c.Start.IsZero() && !c.End.IsZero()
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
// initially it's the schema's ResourceAttributes column; after a
// `| logfmt` parser stage it becomes
// `mapConcat(<prev>, extractKeyValuePairs(Body, '=', ' ', '"'))` so
// downstream label filters resolve against the parsed key set in
// addition to the stream-selector labels. Loki's documented contract
// is "parsed labels appended; on conflict the stream label wins and
// parsed gets `_extracted` suffix"; the CH-side mapConcat lets parsed
// keys win on conflict instead. That's an acceptable v1 approximation
// — the common case is parsed keys that don't shadow stream labels
// (`level`, `msg`, `duration`, …). Strict Loki conflict semantics
// stay open as a follow-up.
func lowerPipeline(e *syntax.PipelineExpr, s schema.Logs, lc lowerCtx) (chplan.Node, error) {
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
			return nil, err
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
		return inner, nil
	}
	return &chplan.Filter{Input: inner, Predicate: pred}, nil
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
		// `| json` / `| regexp` parser stages stay deferred (the
		// dedicated `| logfmt` syntax types are handled below).
		switch st.Op {
		case syntax.OpParserTypeUnpack, syntax.OpParserTypePattern:
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("logql: parser stage `| %s` is not yet supported (json/regexp parsers deferred from M3.2; revisit in RC3 alongside chsql JSONExtract helpers)", st.Op)
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
		return nil, nil, fmt.Errorf("logql: `| json field=\"...\"` parser is not yet supported (deferred from M3.2; revisit in RC3)")
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

// logfmtMergeLabels wraps the current labelsExpr with a
// `mapConcat(<prev>, extractKeyValuePairs(Body, '=', ' ', '"'))` so
// subsequent label filters see the union of stream-selector labels
// and logfmt-parsed key/value pairs. extractKeyValuePairs is the CH
// built-in that lifts arbitrary `key=value` text into a
// `Map(String, String)`; the separator / pair-delimiter / quote
// arguments mirror Loki's logfmt parser defaults.
func logfmtMergeLabels(prev chplan.Expr, s schema.Logs) chplan.Expr {
	return &chplan.FuncCall{
		Name: "mapConcat",
		Args: []chplan.Expr{
			prev,
			extractKVPairs(s),
		},
	}
}

// logfmtExpressionMergeLabels wraps labelsExpr with a `mapConcat` that
// stitches in only the named extractions (identifier => extracted
// value). Each expression is `<identifier>="<key-path>"` — for logfmt
// the path is a top-level key, so the lowering is
// `extractKeyValuePairs(Body, ...)[<key-path>]`. The result is a
// `map(<id1>, <val1>, <id2>, <val2>, …)` that mapConcat appends onto
// the prior label map.
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
		args = append(args,
			&chplan.LitString{V: ext.Identifier},
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
		return nil, fmt.Errorf("logql: numeric label filters are not yet supported (parser-extracted numbers require typed lifting beyond the Map(String,String) `| logfmt` exposes; deferred to a follow-up)")
	case *loglib.DurationLabelFilter, *loglib.BytesLabelFilter:
		return nil, fmt.Errorf("logql: %T label filter is not yet supported (parser-extracted typed fields require typed lifting beyond the Map(String,String) `| logfmt` exposes; deferred to a follow-up)", lf)
	}
	return nil, fmt.Errorf("logql: unsupported label filterer %T", lf)
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
