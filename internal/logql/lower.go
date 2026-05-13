// Package logql lowers Loki LogQL queries into the shared cerberus chplan
// IR. The seed (M3.1) covers stream selectors with `=`/`!=`/`=~`/`!~`
// label matchers and the line-filter family (`|=`, `!=`, `|~`, `!~`).
//
// Subsequent milestones add label filters (`| label="v"`), parsers
// (`| json`, `| logfmt`), the metric form (rate, count_over_time, ...),
// and aggregations.
package logql

import (
	"fmt"

	loglib "github.com/grafana/loki/v3/pkg/logql/log"
	"github.com/grafana/loki/v3/pkg/logql/syntax"
	"github.com/prometheus/prometheus/model/labels"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// Lower turns a parsed LogQL expression into a chplan tree.
func Lower(expr syntax.Expr, s schema.Logs) (chplan.Node, error) {
	return lower(expr, s)
}

func lower(expr syntax.Expr, s schema.Logs) (chplan.Node, error) {
	switch e := expr.(type) {
	case *syntax.MatchersExpr:
		return lowerMatchers(e, s), nil
	case *syntax.PipelineExpr:
		return lowerPipeline(e, s)
	case *syntax.RangeAggregationExpr:
		return lowerRangeAggregation(e, s)
	case *syntax.VectorAggregationExpr:
		return lowerVectorAggregation(e, s)
	default:
		return nil, fmt.Errorf("logql: unsupported expression %T", expr)
	}
}

// lowerMatchers turns `{job="api", env=~"prod|stg"}` into Scan + Filter.
// Stream-selector label matchers go against the ResourceAttributes map
// since OTel-CH stores stream-identity labels there.
func lowerMatchers(e *syntax.MatchersExpr, s schema.Logs) chplan.Node {
	scan := &chplan.Scan{Table: s.LogsTable}
	pred := buildMatchersPredicate(e.Mts, s)
	if pred == nil {
		return scan
	}
	return &chplan.Filter{Input: scan, Predicate: pred}
}

// lowerPipeline handles a stream selector followed by line filters
// (and, in later milestones, label filters and parsers).
func lowerPipeline(e *syntax.PipelineExpr, s schema.Logs) (chplan.Node, error) {
	inner := lowerMatchers(e.Left, s)
	pred := chplan.Expr(nil)
	if f, ok := inner.(*chplan.Filter); ok {
		pred = f.Predicate
		inner = f.Input
	}
	for _, stage := range e.MultiStages {
		next, err := lowerStage(stage, s)
		if err != nil {
			return nil, err
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

func lowerStage(stage syntax.StageExpr, s schema.Logs) (chplan.Expr, error) {
	switch st := stage.(type) {
	case *syntax.LineFilterExpr:
		return lowerLineFilter(st, s)
	case *syntax.LabelFilterExpr:
		return lowerLabelFilter(st, s)
	case *syntax.LineFmtExpr:
		// `| line_format "{{.x}}"` is a post-fetch transform —
		// applied in the API handler over the streams response, not
		// in SQL. Return no predicate so the lowering doesn't error
		// on it but the handler still sees the LineFmtExpr in the
		// original parsed expression.
		_ = st
		return nil, nil
	case *syntax.DecolorizeExpr:
		// Same post-fetch shape: strip ANSI codes from each line
		// after the rows return. No SQL impact.
		return nil, nil
	case *syntax.LineParserExpr:
		return nil, fmt.Errorf("logql: parser stage `| %s` is not yet supported (json/logfmt/regexp/pattern parsers deferred from M3.2; revisit in RC3 alongside chsql JSONExtract/extractKeyValuePairs helpers)", st.Op)
	case *syntax.LogfmtParserExpr:
		return nil, fmt.Errorf("logql: `| logfmt` parser is not yet supported (deferred from M3.2; revisit in RC3)")
	case *syntax.JSONExpressionParserExpr:
		return nil, fmt.Errorf("logql: `| json field=\"...\"` parser is not yet supported (deferred from M3.2; revisit in RC3)")
	case *syntax.LogfmtExpressionParserExpr:
		return nil, fmt.Errorf("logql: `| logfmt field=\"...\"` parser is not yet supported (deferred from M3.2; revisit in RC3)")
	default:
		return nil, fmt.Errorf("logql: pipeline stage %T is not yet supported", stage)
	}
}

// lowerLabelFilter handles `| label="val"` / `| label=~"regex"` and the
// boolean conjunctions Loki packs into BinaryLabelFilter. The named
// label is resolved against ResourceAttributes (parser-extracted labels
// defer until parser stages are wired up).
func lowerLabelFilter(f *syntax.LabelFilterExpr, s schema.Logs) (chplan.Expr, error) {
	return labelFiltererToExpr(f.LabelFilterer, s)
}

func labelFiltererToExpr(lf loglib.LabelFilterer, s schema.Logs) (chplan.Expr, error) {
	switch v := lf.(type) {
	case *loglib.StringLabelFilter:
		return labelMatcherToExpr(v.Matcher, s), nil
	case *loglib.LineFilterLabelFilter:
		// Loki may wrap a string label filter in this when a line-filter
		// short-circuit is also possible. Both embed *labels.Matcher and
		// behave identically for our query-rewrite purposes.
		return labelMatcherToExpr(v.Matcher, s), nil
	case *loglib.BinaryLabelFilter:
		left, err := labelFiltererToExpr(v.Left, s)
		if err != nil {
			return nil, err
		}
		right, err := labelFiltererToExpr(v.Right, s)
		if err != nil {
			return nil, err
		}
		op := chplan.OpAnd
		if !v.And {
			op = chplan.OpOr
		}
		return &chplan.Binary{Op: op, Left: left, Right: right}, nil
	case *loglib.NumericLabelFilter:
		return nil, fmt.Errorf("logql: numeric label filters are not yet supported (parser-extracted numbers depend on `| json` / `| logfmt` parser stages; both deferred to RC3)")
	case *loglib.DurationLabelFilter, *loglib.BytesLabelFilter:
		return nil, fmt.Errorf("logql: %T label filter is not yet supported (parser-extracted typed fields depend on `| json` / `| logfmt` stages; deferred to RC3)", lf)
	}
	return nil, fmt.Errorf("logql: unsupported label filterer %T", lf)
}

// labelMatcherToExpr renders a Prometheus-style label Matcher against
// ResourceAttributes. Shared between StringLabelFilter and the
// short-circuit-friendly LineFilterLabelFilter — both embed the same
// *labels.Matcher.
func labelMatcherToExpr(m *labels.Matcher, s schema.Logs) chplan.Expr {
	return &chplan.Binary{
		Op: matchOp(m.Type),
		Left: &chplan.MapAccess{
			Map: &chplan.ColumnRef{Name: s.ResourceAttributesColumn},
			Key: &chplan.LitString{V: m.Name},
		},
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
	lhs := &chplan.MapAccess{
		Map: &chplan.ColumnRef{Name: s.ResourceAttributesColumn},
		Key: &chplan.LitString{V: m.Name},
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
