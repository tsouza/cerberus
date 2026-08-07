package promql

import "github.com/prometheus/prometheus/promql/parser"

// peelWrappers strips ParenExpr / StepInvariantExpr wrappers — the
// parser inserts them for shapes that are otherwise inert.
func peelWrappers(e parser.Expr) parser.Expr {
	for {
		switch v := e.(type) {
		case *parser.ParenExpr:
			e = v.Expr
		case *parser.StepInvariantExpr:
			e = v.Expr
		default:
			return e
		}
	}
}
