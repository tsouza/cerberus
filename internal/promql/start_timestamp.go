package promql

import (
	"fmt"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// fnStartTimestamp names the experimental `start_timestamp(v)` function.
const fnStartTimestamp = "start_timestamp"

// lowerStartTimestamp lowers `start_timestamp(v)` to the answer the
// reference gives without its `use-start-timestamps` feature flag, which
// is how the compat lane runs it: an empty vector, for instant and range
// queries alike. Prometheus's funcStartTimestamp emits a sample only for
// a start timestamp the engine loaded alongside it, and the engine loads
// none unless that flag is on.
//
// The argument is still lowered and planned, so an argument cerberus
// cannot answer fails exactly as it would anywhere else, and ClickHouse
// produces the empty result by filtering that plan to zero rows. A
// histogram-valued argument, which the reference also answers empty,
// reaches the same filter through the canonical-float route the clamp
// family uses. A range-vector argument never gets here: the parser
// rejects it, as the reference does.
func lowerStartTimestamp(c *parser.Call, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if len(c.Args) != 1 {
		return nil, fmt.Errorf("promql: %s expects 1 argument, got %d", fnStartTimestamp, len(c.Args))
	}
	inner, ok, err := lowerExpHistogramArgAsCanonicalFloat(c.Args[0], s, ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		if inner, err = lowerMathOperand(c.Args[0], s, ctx); err != nil {
			return nil, err
		}
	}
	return &chplan.Filter{Input: inner, Predicate: &chplan.LitBool{V: false}}, nil
}
