package promql

import (
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/schema"
)

// sortOverMixedExpHistogramSetOp recognizes the direct union before the
// float-only operand policy invokes checked shadow resolution.
func sortOverMixedExpHistogramSetOp(c *parser.Call, s schema.Metrics, ctx lowerCtx) (*parser.BinaryExpr, bool) {
	if len(c.Args) != 1 {
		return nil, false
	}
	return mixedExpHistogramSetOp(c.Args[0], s, ctx)
}
