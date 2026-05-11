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
	default:
		return nil, fmt.Errorf("logql: pipeline stage %T is not yet supported (label filters and parsers land in M3.2)", stage)
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
