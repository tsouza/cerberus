package prom

import (
	"strconv"

	"github.com/prometheus/prometheus/model/labels"
	promparser "github.com/prometheus/prometheus/promql/parser"
)

// translatePromQLAST ports upstream Prometheus's `translateAST` (the
// function backing its own `/api/v1/parse_query`, unexported in
// `web/api/v1/translate_ast.go`) into cerberus verbatim — same field
// reads, same JSON keys, same node-type coverage. Cerberus parses with
// the identical `promql/parser` package (via the `tsouza/prometheus`
// Dependabot-boundary fork, zero patches — see go.mod's replace and
// docs/upstream-forks.md), so every field this function reads
// (VectorSelector.Anchored/Smoothed/OriginalOffsetExpr/StartOrEnd,
// MatrixSelector.RangeExpr, SubqueryExpr.StepExpr, DurationExpr,
// BinaryExpr.VectorMatching, Call.Func) exists on cerberus's parsed AST
// exactly as it does on upstream's. Reimplementing (rather than vendoring
// the unexported function) is Apache-2.0-clean: it is upstream's own
// Apache-licensed logic, transcribed rather than imported because the
// symbol is unexported.
//
// Grafana's PromQL query-builder / tree view (and any other client
// depending on Prometheus's documented `/api/v1/parse_query` response
// shape) parses this exact recursive node structure — a `{type, node}`
// stub broke that integration even though it "parsed OK".
func translatePromQLAST(node promparser.Expr) any {
	if node == nil {
		return nil
	}

	switch n := node.(type) {
	case *promparser.AggregateExpr:
		return map[string]any{
			"type":     "aggregation",
			"op":       n.Op.String(),
			"expr":     translatePromQLAST(n.Expr),
			"param":    translatePromQLAST(n.Param),
			"grouping": sanitizeStringList(n.Grouping),
			"without":  n.Without,
		}
	case *promparser.BinaryExpr:
		var matching any
		if m := n.VectorMatching; m != nil {
			matching = map[string]any{
				"card":    m.Card.String(),
				"labels":  sanitizeStringList(m.MatchingLabels),
				"on":      m.On,
				"include": sanitizeStringList(m.Include),
				"fillValues": map[string]*float64{
					"lhs": m.FillValues.LHS,
					"rhs": m.FillValues.RHS,
				},
			}
		}

		return map[string]any{
			"type":     "binaryExpr",
			"op":       n.Op.String(),
			"lhs":      translatePromQLAST(n.LHS),
			"rhs":      translatePromQLAST(n.RHS),
			"matching": matching,
			"bool":     n.ReturnBool,
		}
	case *promparser.Call:
		args := []any{}
		for _, arg := range n.Args {
			args = append(args, translatePromQLAST(arg))
		}

		return map[string]any{
			"type": "call",
			"func": map[string]any{
				"name":       n.Func.Name,
				"argTypes":   n.Func.ArgTypes,
				"variadic":   n.Func.Variadic,
				"returnType": n.Func.ReturnType,
			},
			"args": args,
		}
	case *promparser.MatrixSelector:
		vs := n.VectorSelector.(*promparser.VectorSelector)
		return map[string]any{
			"type":       "matrixSelector",
			"name":       vs.Name,
			"range":      n.Range.Milliseconds(),
			"rangeExpr":  translateDurationExpr(n.RangeExpr),
			"offset":     vs.OriginalOffset.Milliseconds(),
			"offsetExpr": translateDurationExpr(vs.OriginalOffsetExpr),
			"matchers":   translatePromQLMatchers(vs.LabelMatchers),
			"timestamp":  vs.Timestamp,
			"startOrEnd": promQLStartOrEnd(vs.StartOrEnd),
			"anchored":   vs.Anchored,
			"smoothed":   vs.Smoothed,
		}
	case *promparser.SubqueryExpr:
		return map[string]any{
			"type":       "subquery",
			"expr":       translatePromQLAST(n.Expr),
			"range":      n.Range.Milliseconds(),
			"rangeExpr":  translateDurationExpr(n.RangeExpr),
			"offset":     n.OriginalOffset.Milliseconds(),
			"offsetExpr": translateDurationExpr(n.OriginalOffsetExpr),
			"step":       n.Step.Milliseconds(),
			"stepExpr":   translateDurationExpr(n.StepExpr),
			"timestamp":  n.Timestamp,
			"startOrEnd": promQLStartOrEnd(n.StartOrEnd),
		}
	case *promparser.DurationExpr:
		return translateDurationExpr(n)
	case *promparser.NumberLiteral:
		return map[string]string{
			"type": "numberLiteral",
			"val":  strconv.FormatFloat(n.Val, 'f', -1, 64),
		}
	case *promparser.ParenExpr:
		return map[string]any{
			"type": "parenExpr",
			"expr": translatePromQLAST(n.Expr),
		}
	case *promparser.StringLiteral:
		return map[string]any{
			"type": "stringLiteral",
			"val":  n.Val,
		}
	case *promparser.UnaryExpr:
		return map[string]any{
			"type": "unaryExpr",
			"op":   n.Op.String(),
			"expr": translatePromQLAST(n.Expr),
		}
	case *promparser.VectorSelector:
		return map[string]any{
			"type":       "vectorSelector",
			"name":       n.Name,
			"offset":     n.OriginalOffset.Milliseconds(),
			"offsetExpr": translateDurationExpr(n.OriginalOffsetExpr),
			"matchers":   translatePromQLMatchers(n.LabelMatchers),
			"timestamp":  n.Timestamp,
			"startOrEnd": promQLStartOrEnd(n.StartOrEnd),
			"anchored":   n.Anchored,
			"smoothed":   n.Smoothed,
		}
	}
	panic("cerberus: unsupported PromQL AST node type in translatePromQLAST")
}

// translateDurationExpr mirrors upstream's `translateDurationExpr` — a
// `*DurationExpr` recurses into its own shape, a `*NumberLiteral` (a bare
// duration constant) gets its literal shape, and any other Expr (already
// resolved, e.g. after constant folding) falls back to the general
// translator.
func translateDurationExpr(node promparser.Expr) any {
	if node == nil {
		return nil
	}

	switch n := node.(type) {
	case *promparser.DurationExpr:
		if n == nil {
			return nil
		}
		return map[string]any{
			"type":    "durationExpr",
			"op":      n.Op.String(),
			"lhs":     translateDurationExpr(n.LHS),
			"rhs":     translateDurationExpr(n.RHS),
			"wrapped": n.Wrapped,
		}
	case *promparser.NumberLiteral:
		if n == nil {
			return nil
		}
		return map[string]any{
			"type":     "numberLiteral",
			"val":      strconv.FormatFloat(n.Val, 'f', -1, 64),
			"duration": n.Duration,
		}
	default:
		return translatePromQLAST(n)
	}
}

// sanitizeStringList mirrors upstream's `sanitizeList`: a nil grouping /
// matching-labels slice serializes as `[]`, not `null` — Grafana's tree
// view iterates the field unconditionally.
func sanitizeStringList(l []string) []string {
	if l == nil {
		return []string{}
	}
	return l
}

// translatePromQLMatchers mirrors upstream's `translateMatchers`.
func translatePromQLMatchers(in []*labels.Matcher) any {
	out := []map[string]any{}
	for _, m := range in {
		out = append(out, map[string]any{
			"name":  m.Name,
			"value": m.Value,
			"type":  m.Type.String(),
		})
	}
	return out
}

// promQLStartOrEnd mirrors upstream's `getStartOrEnd`: the zero ItemType
// means no `@ start()` / `@ end()` modifier was used, serialized as
// `null` rather than the zero item's string representation.
func promQLStartOrEnd(startOrEnd promparser.ItemType) any {
	if startOrEnd == 0 {
		return nil
	}
	return startOrEnd.String()
}
