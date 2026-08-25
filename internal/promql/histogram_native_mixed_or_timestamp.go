package promql

import (
	"fmt"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_mixed_or_timestamp.go lowers `timestamp(...)` wrapping
// a mixed float/histogram `or` (histogram_native_mixed_or.go's
// #2330/#2335 shape) — cerberus issue #2611, the timestamp-specific
// sibling `sort`/`sort_desc` (histogram_native_mixed_or_sort.go, cerberus
// issue #2605) and the eight value-reading date/time functions (cerberus
// issue #2609) already have.
//
// Reference Prometheus's funcTimestamp (promql/functions.go) reads only
// each sample's own `Point.T` — never `Point.F`/`Point.H` — so it answers
// identically for a float-shaped row and a histogram-shaped row alike:
// BOTH arms of the `or` survive into timestamp()'s output, unlike the
// float-only "drop the histogram rows" rule sort()/the date-component
// family apply.
//
// What `T` actually IS depends on the ARGUMENT's own parser shape, not
// on any row's runtime value type — histogram_native_timestamp.go's own
// package doc already establishes this split for the non-mixed case: a
// bare VectorSelector reports the SELECTED SAMPLE's own raw timestamp;
// every other shape reports the EVALUATION instant instead. A mixed `or`
// (`<hist> or <float>`) is a BinaryExpr, never a bare VectorSelector, so
// it always falls in the "evaluation instant" bucket — every row, float-
// or histogram-shaped alike, reports the SAME evalInstant value. That
// collapses this composition to exactly
// [projectExpHistogramEvalInstant]'s existing non-selector projection,
// fed the WHOLE combined Mixed node
// ([lowerMixedExpHistogramSetOp]'s [chplan.VectorSetOp] with Mixed set)
// instead of a pure HistogramProjection: that projection only ever reads
// Attributes (present, and meaning-identical, on both a float-shaped and
// a histogram-shaped row) and never Value, so it needs no
// MixedRowShape-aware branching of its own — the
// [assertValueShapedInput] panic never applies here because this
// projection is hand-built directly (mirroring
// [timestampInstantProjection]/[timestampRangeProjection]'s own raw
// `&chplan.Project{}` construction), never routed through the generic
// [projectValueOverInner] forwarder that guard actually protects.
func timestampOverMixedExpHistogramSetOp(arg parser.Expr, s schema.Metrics, ctx lowerCtx) (*parser.BinaryExpr, bool) {
	return mixedExpHistogramSetOp(arg, s, ctx)
}

// lowerTimestampOverMixedExpHistogramSetOp lowers the shape
// [timestampOverMixedExpHistogramSetOp] recognised: the combined Mixed
// node, fed through the SAME eval-instant projection
// [lowerTimestampOverExpHistogram] already builds for any other
// non-selector histogram-valued shape — see this file's header for why
// no shadow resolution beyond what [lowerMixedExpHistogramSetOp] itself
// already applies is needed.
func lowerTimestampOverMixedExpHistogramSetOp(b *parser.BinaryExpr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if b.ReturnBool {
		return nil, fmt.Errorf("promql: 'bool' modifier is only allowed on comparison binary ops")
	}
	mixed, err := lowerMixedExpHistogramSetOp(b, s, ctx)
	if err != nil {
		return nil, err
	}
	return projectExpHistogramEvalInstant(mixed, s, ctx), nil
}
